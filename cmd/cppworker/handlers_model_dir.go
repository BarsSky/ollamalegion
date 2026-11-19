// handlers_model_dir.go — R622 (2026-11-19): каталог моделей на диске
// (список файлов, метаданные, пересканирование каталога и разбор квантизации).
//
// ЗАЧЕМ ВЫНЕСЕНО. `handlers_model.go` держал 2782 строки и 13 обработчиков, из-за чего
// его приходилось читать целиком, чтобы найти одну ручку. Здесь — самостоятельная
// поверхность «что лежит на диске»: она не зависит от загрузки/выгрузки модели
// (кроме чтения `backend.ModelManager()`), поэтому вынесена дословно, без правок
// поведения.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть; тесты `cmd/cppworker` (включая
// `handlers_model_round37_test.go`, `ggufFileMeta`/`parseQuantization`-наборы)
// зелёные до и после.
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// handleListModelsDir — GET /api/models/files: список .gguf файлов в modelsDir.
// === R60.3 (2026-09-04): per-file meta ===
// До R60.3 API возвращал только {name, sizeBytes, modifiedAt} — webui рисовал
// карточку модели с пустыми полями «Размер: -» и «Квантизация: -», потому что
// gguf-renderer-detail.js:232-234 ожидает `m.size` и `m.quantization`. Теперь
// добавляем алиас `size` (равно sizeBytes, для обратной совместимости с webui)
// и `quantization` (парсится из имени файла: `q4km` → `Q4_K_M`, `Q8_0` → `Q8_0`).
// Cppworker's /api/models/files теперь содержит ВСЮ meta для UI, webui больше
// не нужен fallback на filename parsing.
func handleListModelsDir(w http.ResponseWriter, r *http.Request) {
	mm := backend.ModelManager()
	if mm != nil {
		// R93 (2026-10-09): список обязан отражать ФАКТ на диске.
		//
		// До этого здесь отдавался кэш ModelManager без проверки: файл (или весь
		// каталог) удалили на диске, а /api/models/files продолжал отдавать старые
		// записи, и WebUI показывал модели-призраки до перезапуска контейнера
		// именно этого воркера. ScanModelsIfStale() пересканирует каталог, если с
		// прошлого скана прошло больше CPPWORKER_MODELS_RESCAN_SEC (default 30).
		// ?refresh=1 — принудительная проверка вне зависимости от TTL.
		if strings.TrimSpace(r.URL.Query().Get("refresh")) != "" {
			_, _ = mm.ScanModels()
		} else {
			mm.ScanModelsIfStale()
		}
		writeJSON(w, http.StatusOK, modelsDirPayload(mm))
		return
	}
	var files []map[string]interface{}
	entries, err := os.ReadDir(*modelsDir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"files": []interface{}{}, "dir": *modelsDir, "count": 0,
			// R93: недоступный каталог — это состояние, о котором оператор обязан
			// узнать (раньше ответ был неотличим от «моделей нет»).
			"dirError": err.Error(), "scannedAt": time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, ggufFileMeta(name, info.Size(), info.ModTime()))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"files": files, "dir": *modelsDir, "count": len(files),
		"scannedAt": time.Now().UTC().Format(time.RFC3339),
	})
}

// modelsDirPayload — тело ответа /api/models/files и /api/models/refresh:
// список файлов, алиасы и СВЕЖЕСТЬ инвентаря (когда проверяли, что изменилось).
//
// R93: без этих полей «модель исчезла из WebUI» невозможно отличить от
// «проверка не проходила» — оператор видел просто пустой/старый список.
func modelsDirPayload(mm *cppbackend.ModelManager) map[string]interface{} {
	files := make([]map[string]interface{}, 0)
	for _, m := range mm.ListModels() {
		files = append(files, ggufFileMeta(m.Filename, m.SizeBytes, m.ModifiedAt))
	}
	// R66d (2026-09-23): отдаём и алиасы моделей (<name>.gguf.json из
	// POST /api/create). Балансер собирает /api/tags именно из этого ответа,
	// поэтому без поля aliases созданная модель не доезжала до клиентов.
	aliases := make([]map[string]interface{}, 0)
	for _, a := range mm.ListAliases() {
		if !a.SourceExists {
			// Незагружаемый алиас клиенту не показываем (в /api/tags его
			// тоже нет) — только в лог.
			logger.Get().Warnw("handleListModelsDir: alias source missing, skipping",
				"alias", a.Name, "source", a.Source)
			continue
		}
		aliases = append(aliases, map[string]interface{}{
			"name":            a.Name,
			"source":          a.Source,
			"sourceSizeBytes": a.SourceSizeBytes,
			"sourceExists":    a.SourceExists,
			"parentModel":     strings.TrimSuffix(filepath.Base(a.Source), ".gguf"),
			"createdAt":       a.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	st := mm.InventoryStatus()
	resp := map[string]interface{}{
		"files": files, "aliases": aliases, "dir": mm.GetModelsDir(),
		"count": len(files), "aliasCount": len(aliases),
		"scans": st.Scans, "changes": st.Changes, "rescanTtlSec": st.RescanTTLSec,
	}
	if !st.ScannedAt.IsZero() {
		resp["scannedAt"] = st.ScannedAt.UTC().Format(time.RFC3339)
	}
	if !st.ChangedAt.IsZero() {
		resp["changedAt"] = st.ChangedAt.UTC().Format(time.RFC3339)
	}
	if len(st.Added) > 0 {
		resp["added"] = st.Added
	}
	if len(st.Removed) > 0 {
		resp["removed"] = st.Removed
	}
	if len(st.Resized) > 0 {
		resp["resized"] = st.Resized
	}
	if st.DirError != "" {
		resp["dirError"] = st.DirError
		if !st.DirErrorAt.IsZero() {
			resp["dirErrorAt"] = st.DirErrorAt.UTC().Format(time.RFC3339)
		}
	}
	return resp
}

// handleRefreshModelsDir — POST /api/models/refresh (R93): принудительная
// проверка каталога моделей на диске.
//
// Нужна двум потребителям:
//   - WebUI (кнопка «Обновить» на странице моделей) — не ждать TTL;
//   - балансеру (POST /api/v1/models/catalog/refresh) — перечитать каталог
//     бэкенда сразу, а не по расписанию.
//
// Отвечает тем же форматом, что /api/models/files, поэтому вызывающий сразу
// видит и новый список, и диф (added/removed/resized).
func handleRefreshModelsDir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	mm := backend.ModelManager()
	if mm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "model manager unavailable"})
		return
	}
	before := len(mm.ListModels())
	if _, err := mm.ScanModels(); err != nil {
		logger.Get().Warnw("handleRefreshModelsDir: scan failed", "error", err)
	}
	st := mm.InventoryStatus()
	logger.Get().Infow("handleRefreshModelsDir: manual rescan",
		"dir", st.Dir, "before", before, "after", st.Count,
		"added", st.Added, "removed", st.Removed, "dirError", st.DirError)
	writeJSON(w, http.StatusOK, modelsDirPayload(mm))
}

// ggufFileMeta — собирает meta для одной записи /api/models/files.
// Используется в обоих циклах handleListModelsDir (через ModelManager и через
// прямой ReadDir). Возвращает map с алиасами: name + size (alias sizeBytes) +
// quantization (parse из имени) + modifiedAt.
func ggufFileMeta(filename string, size int64, modifiedAt time.Time) map[string]interface{} {
	return map[string]interface{}{
		"name":         filename,
		"size":         size, // alias for sizeBytes — webui reads m.size
		"sizeBytes":    size, // canonical name, kept for API stability
		"quantization": parseQuantization(filename),
		"modifiedAt":   modifiedAt.Format(time.RFC3339),
	}
}

// === R60.3: parseQuantization — extract quantization level from filename ===
// Поддерживает llama.cpp naming conventions:
//   - Q4_K_M, Q4_K_S, Q4_0, Q4_1, Q5_K_M, Q8_0, Q2_K, F16, F32, BF16
//   - compressed aliases: q4km → Q4_K_M, q4ks → Q4_K_S, q5_1, q8_0
//   - I-quants (llama.cpp IQ series): IQ1_S, IQ2_XXS, IQ3_S, IQ4_XS
//
// Returns "" if no recognizable quantization level found.
//
// Strategy: try multiple "candidates" derived from the last dash-separated
// segments of the filename. Filenames like "Qwen3-Instruct-2507-q4km" have
// `q4km` as a single trailing token. "Llama-3-8B-Q4_K_M" has `Q4_K_M` split
// into 3 trailing tokens. We try single-token, 2-token-joined, 3-token-joined
// variants, each against three regexes (standard underscore, compressed, F-quant).
func parseQuantization(filename string) string {
	base := filename
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	if idx := strings.LastIndex(base, `\`); idx >= 0 {
		base = base[idx+1:]
	}
	base = strings.TrimSuffix(base, ".gguf")
	base = strings.TrimSuffix(base, ".GGUF")
	if base == "" {
		return ""
	}
	parts := strings.Split(base, "-")
	n := len(parts)

	// Build candidates: try longest match first.
	candidates := make([]string, 0, 5)
	if n >= 3 {
		candidates = append(candidates, parts[n-3]+"_"+parts[n-2]+"_"+parts[n-1]) // "Q4_K_M"
		candidates = append(candidates, parts[n-3]+parts[n-2]+parts[n-1])         // "Q4KM"
	}
	if n >= 2 {
		candidates = append(candidates, parts[n-2]+"_"+parts[n-1]) // "Q6_K"
		candidates = append(candidates, parts[n-2]+parts[n-1])     // "Q6K"
	}
	candidates = append(candidates, parts[n-1]) // "q4km", "F16", etc.

	// Standard: Q4_K_M, Q4_0, IQ4_XS, Q4 (kind optional).
	stdRe := regexp.MustCompile(`^(I?Q)(\d+)(_(K_[SML]|K|0|1|S|M|L|XS|XXS))?$`)
	// Compressed: Q4KM, Q6K, Q4K (no underscore between bits and kind).
	cmpRe := regexp.MustCompile(`^(I?Q)(\d+)(KM|KS|KL|KM|KS|KL|0|1|S|M|L|XS|XXS|K)$`)
	// F-quant: F16, BF16, FP16.
	fRe := regexp.MustCompile(`^(F|BF|FP)(\d+)$`)

	for _, cand := range candidates {
		up := strings.ToUpper(cand)
		if m := fRe.FindStringSubmatch(up); m != nil {
			if m[1] == "FP" {
				return "F" + m[2]
			}
			return m[1] + m[2] // F16, BF16
		}
		if m := stdRe.FindStringSubmatch(up); m != nil {
			prefix := m[1] // "Q" or "IQ"
			bits := m[2]
			kind := m[3]
			if kind == "" {
				return prefix + bits
			}
			return prefix + bits + kind
		}
		if m := cmpRe.FindStringSubmatch(up); m != nil {
			prefix := m[1]
			bits := m[2]
			suffix := m[3]
			// Normalize compressed → canonical form.
			switch suffix {
			case "KM":
				return prefix + bits + "_K_M"
			case "KS":
				return prefix + bits + "_K_S"
			case "KL":
				return prefix + bits + "_K_L"
			case "K":
				return prefix + bits + "_K"
			case "XS":
				return prefix + bits + "_XS"
			case "XXS":
				return prefix + bits + "_XXS"
			default:
				return prefix + bits + "_" + suffix
			}
		}
	}
	return ""
}