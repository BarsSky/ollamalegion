// handlers_model_share.go — R89 (2026-10-08): перенос модели МЕЖДУ бэкендами.
//
// ЗАЧЕМ. В кластере несколько машин, и модель часто лежит только на одной.
// «Расшарить» её на другую до сих пор можно было лишь повторной загрузкой из
// HuggingFace: интернет, HF-токен, время и риск получить другую ревизию файла.
// Теперь источник отдаёт ровно тот файл, что лежит у него на диске, а приёмник
// сохраняет его — балансер стримит поток между ними (internal/balancer/model_share.go).
//
// КОНТРАКТ (симметричен паре в sdworker, см. cmd/sdworker/handlers_model_share.go):
//
//	GET  /api/models/export?name=<filename|alias>
//	     поток файла; заголовки Content-Length, X-Model-Filename, X-Model-SizeBytes.
//	     Поддерживается HEAD (балансер спрашивает размер перед копированием).
//	POST /api/models/import?filename=<file.gguf>[&size=<bytes>][&sha256=<hex>][&overwrite=1]
//	     тело — сырые байты; пишем во временный файл и переименовываем (атомарно).
//
// ПОЧЕМУ БЕЗ ХЕША ПО УМОЛЧАНИЮ: хеш считается потоком при импорте, но источник
// обязан его посчитать, а это второе чтение 10-30 ГБ. Поэтому sha256 — опция
// вызывающего (балансер передаёт, если источник его сообщил), а обязательной
// проверкой остаётся размер.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// exportableModel — что именно отдаём: имя файла, путь и размер.
type exportableModel struct {
	Filename string
	Path     string
	Size     int64
}

// resolveExportableModel — найти модель по имени (файл, имя без .gguf или алиас).
//
// Возвращает ошибку с человеческим текстом: балансер показывает её оператору
// в статусе задания на перенос, поэтому «model not found» мало — нужен список
// известных имён.
func resolveExportableModel(mm *cppbackend.ModelManager, name string) (exportableModel, error) {
	if mm == nil {
		return exportableModel{}, fmt.Errorf("model manager is not available")
	}
	want := strings.TrimSpace(name)
	if want == "" {
		return exportableModel{}, fmt.Errorf("name is required")
	}
	base := filepath.Base(want)
	// 1) Точное имя файла среди просканированных моделей.
	for _, m := range mm.ListModels() {
		if m.Filename == base || m.Filename == base+".gguf" ||
			strings.TrimSuffix(m.Filename, ".gguf") == strings.TrimSuffix(base, ".gguf") {
			return exportableModel{Filename: m.Filename, Path: m.Path, Size: m.SizeBytes}, nil
		}
	}
	// 2) Алиас (<name>.gguf.json) — отдаём его файл-источник.
	for _, a := range mm.ListAliases() {
		if a.Name == base || a.Name == strings.TrimSuffix(base, ".gguf") {
			if !a.SourceExists {
				return exportableModel{}, fmt.Errorf("alias %q points to a missing file %q", a.Name, a.Source)
			}
			size := a.SourceSizeBytes
			if fi, err := os.Stat(a.SourcePath); err == nil {
				size = fi.Size()
			}
			return exportableModel{Filename: filepath.Base(a.SourcePath), Path: a.SourcePath, Size: size}, nil
		}
	}
	return exportableModel{}, fmt.Errorf("model %q not found on this worker (known: %s)",
		want, strings.Join(knownModelNames(mm), ", "))
}

// knownModelNames — короткий список имён для сообщений об ошибке.
func knownModelNames(mm *cppbackend.ModelManager) []string {
	models := mm.ListModels()
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Filename)
		if len(out) >= 10 {
			break
		}
	}
	if len(out) == 0 {
		return []string{"(каталог пуст)"}
	}
	return out
}

// handleExportModel — GET/HEAD /api/models/export.
func handleExportModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "use GET or HEAD")
		return
	}
	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager is not available")
		return
	}
	exp, err := resolveExportableModel(mm, r.URL.Query().Get("name"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": err.Error(), "code": "model_not_found",
			"hint": "имя можно посмотреть в GET /api/models/files (или в панели, список моделей бэкенда)",
		})
		return
	}
	// Модель обязана лежать ВНУТРИ каталога моделей: имя приходит извне, и без
	// проверки «../../etc/passwd» уехало бы на другой узел.
	if !pathInsideDir(mm.GetModelsDir(), exp.Path) {
		writeJSON(w, http.StatusForbidden, map[string]interface{}{
			"error": "model path escapes the models directory",
			"code":  "path_outside_models_dir", "path": exp.Path,
		})
		return
	}
	f, err := os.Open(exp.Path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": "cannot open model file: " + err.Error(), "code": "model_not_found",
		})
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot stat model file: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Model-Filename", fi.Name())
	w.Header().Set("X-Model-SizeBytes", strconv.FormatInt(fi.Size(), 10))
	// ServeContent сам ставит Content-Length, понимает Range и корректно
	// обрабатывает HEAD — то, чем балансер спрашивает размер перед копированием.
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// handleImportModel — POST /api/models/import.
func handleImportModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager is not available")
		return
	}
	q := r.URL.Query()
	rawFilename := strings.TrimSpace(q.Get("filename"))
	filename := filepath.Base(rawFilename)
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "filename is required", "code": "filename_required",
		})
		return
	}
	// Балансер всегда передаёт простое имя файла. Если пришёл путь — это либо
	// ошибка клиента, либо попытка записать вне каталога моделей; в обоих случаях
	// честнее отказать, чем молча переименовать файл в basename.
	if rawFilename != filename || strings.ContainsAny(rawFilename, `/\`) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "filename must be a plain file name (no path separators)", "code": "invalid_filename",
			"hint": "передайте filename=<model>.gguf без каталогов",
		})
		return
	}
	if !strings.HasSuffix(strings.ToLower(filename), ".gguf") {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "only .gguf files can be imported", "code": "invalid_filename",
			"hint":  "передайте filename=<model>.gguf (каталоги/прочие расширения не переносятся)",
		})
		return
	}
	dir := mm.GetModelsDir()
	if dir == "" {
		writeError(w, http.StatusServiceUnavailable, "models directory is not configured")
		return
	}
	final := filepath.Join(dir, filename)
	overwrite := q.Get("overwrite") == "1" || strings.EqualFold(q.Get("overwrite"), "true")
	if _, err := os.Stat(final); err == nil && !overwrite {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error": fmt.Sprintf("model %q already exists on this worker", filename),
			"code":  "model_exists",
			"hint":  "повторите с overwrite=1, если файл нужно заменить",
		})
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create models dir: "+err.Error())
		return
	}

	// .importing-<file> вместо .part: понятное имя для оператора и не конфликтует
	// с .download HF-слоя (тот resume-able, этот — недоимпортированный).
	tmp := final + ".importing"
	out, err := os.Create(tmp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create temp file: "+err.Error())
		return
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), r.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "incomplete upload: " + copyErr.Error(), "code": "import_incomplete",
			"received": n,
		})
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		writeError(w, http.StatusInternalServerError, "cannot flush temp file: "+closeErr.Error())
		return
	}

	if want := strings.TrimSpace(q.Get("size")); want != "" {
		if expected, err := strconv.ParseInt(want, 10, 64); err == nil && expected != n {
			_ = os.Remove(tmp)
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": fmt.Sprintf("size mismatch: got %d bytes, want %d", n, expected),
				"code":  "size_mismatch", "received": n, "expected": expected,
				"hint": "копия прервалась (сеть/диск) — повторите перенос",
			})
			return
		}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if want := strings.ToLower(strings.TrimSpace(q.Get("sha256"))); want != "" && want != sum {
		_ = os.Remove(tmp)
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": "sha256 mismatch — файл побился при переносе", "code": "sha256_mismatch",
			"received": sum, "expected": want,
		})
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		writeError(w, http.StatusInternalServerError, "cannot move into place: "+err.Error())
		return
	}

	// Инвентарь моделей сканируется отдельно от файловой системы: без рескана
	// импортированный файл не появился бы в /api/models/files до рестарта.
	if _, err := mm.ScanModels(); err != nil {
		logger.Get().Warnw("model import: rescan failed (file is on disk)",
			"file", filename, "error", err)
	}
	logger.Get().Infow("model imported from another backend",
		"file", filename, "size_bytes", n, "sha256", sum)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"filename":  filename,
		"path":      final,
		"sizeBytes": n,
		"sha256":    sum,
	})
}

// pathInsideDir — true, если path лежит внутри dir (защита от «..» в имени).
func pathInsideDir(dir, path string) bool {
	if dir == "" || path == "" {
		return false
	}
	absDir, err1 := filepath.Abs(dir)
	absPath, err2 := filepath.Abs(path)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
