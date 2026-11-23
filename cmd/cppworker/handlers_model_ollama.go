// handlers_model_ollama.go — R524 (2026-11-19, R3 читаемости): ollama-поверхность
// (/api/tags, /api/ps, /api/show, copy/create/push) и её помощники вынесены из
// handlers_model.go отдельным файлом.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: функции перенесены дословно (только файл).
//
// ПОЧЕМУ ТОЛЬКО ЭТИ: сторож rpc_split_mode_wiring_r108_test.go читает
// handlers_model.go и требует в нём `defSplitMode := -1` / `defSplitMode2 := -1`
// (handleLoadModel, handleLoadWithParams). Эти две функции остаются на месте —
// перенос любой из них нарушил бы правило «без правки существующих тестов».
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

func handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	modelMap := make(map[string]map[string]interface{})

	mm := backend.ModelManager()
	if mm != nil {
		fsModels := mm.ListModels()
		for _, fm := range fsModels {
			modelName := fm.Filename
			if strings.HasSuffix(strings.ToLower(modelName), ".gguf") {
				modelName = modelName[:len(modelName)-5]
			}
			digest := fmt.Sprintf("sha256:%x", fm.SizeBytes)
			modelMap[modelName] = map[string]interface{}{
				"name": modelName, "model": modelName,
				"modified_at": fm.ModifiedAt.Format(time.RFC3339),
				"size":        fm.SizeBytes, "digest": digest,
				"details": map[string]interface{}{
					"format": "gguf", "family": fm.Architecture,
					"parameter_size": "unknown", "quantization_level": fm.FileType,
				},
			}
		}
	}

	loadedModels := backend.ListModels()
	for _, m := range loadedModels {
		sizeBytes := m.SizeBytes
		if sizeBytes == 0 {
			if fsEntry, ok := modelMap[m.Name]; ok {
				if fsSize, ok2 := fsEntry["size"].(int64); ok2 && fsSize > 0 {
					sizeBytes = uint64(fsSize)
				}
			}
		}
		// R65d (2026-09-20): digest — настоящий content-addressed хеш, а не
		// байтовый размер под видом хеша.
		//
		// Было: fmt.Sprintf("sha256:%x", sizeBytes) — то есть "sha256:" + hex
		// от ЧИСЛА байт. Две разные модели одинакового размера получали
		// ОДИНАКОВЫЙ "digest", а клиенты, использующие digest как идентификатор
		// (кеши, сравнение версий модели), путали их.
		//
		// Полный SHA256 файла не считаем: модели по 5-20 GB, а /api/tags
		// вызывается часто. Стабильный хеш от пути+имени+размера различает
		// модели и не меняется между запросами.
		digest := modelDigest(m.Path, m.Name, sizeBytes)
		modelMap[m.Name] = map[string]interface{}{
			"name": m.Name, "model": m.Name,
			"modified_at": m.LoadedAt.Format(time.RFC3339),
			"size":        sizeBytes, "digest": digest,
			"details": map[string]interface{}{
				"format": "gguf", "family": m.Architecture,
				"families":           []string{m.Architecture},
				"parameter_size":     modelParameterSize(m.NLayers, m.NEmbd),
				"quantization_level": quantizationOrUnknown(parseQuantization(m.Path)),
			},
		}
	}

	// R66d (2026-09-23): алиасы моделей (<name>.gguf.json из POST /api/create).
	// Раньше их не было в /api/tags, поэтому созданная модель не появлялась в
	// выборе модели у клиентов (Cline/OpenWebUI) — «создали и не видно».
	// Алиасы без файла-источника не показываем (загрузить их нельзя), но пишем
	// предупреждение в лог.
	for _, a := range mm.ListAliases() {
		if !a.SourceExists {
			logger.Get().Warnw("handleOllamaTags: alias source file is missing, skipping",
				"alias", a.Name, "source", a.Source, "source_path", a.SourcePath)
			continue
		}
		if _, already := modelMap[a.Name]; already {
			continue // загруженная модель с тем же именем — метаданные точнее
		}
		srcName := strings.TrimSuffix(filepath.Base(a.Source), ".gguf")
		details := map[string]interface{}{
			"format":         "gguf",
			"parent_model":   srcName,
			"families":       []string{},
			"parameter_size": "unknown",
		}
		if meta, err := mm.GetModelMeta(filepath.Base(a.Source)); err == nil && meta != nil {
			if meta.Architecture != "" {
				details["family"] = meta.Architecture
				details["families"] = []string{meta.Architecture}
			}
			if meta.FileType != "" {
				details["quantization_level"] = meta.FileType
			}
		}
		modelMap[a.Name] = map[string]interface{}{
			"name": a.Name, "model": a.Name,
			"modified_at": a.CreatedAt.Format(time.RFC3339),
			"size":        a.SourceSizeBytes,
			"digest":      modelDigest(a.SourcePath, a.Name, uint64(a.SourceSizeBytes)),
			"details":     details,
		}
	}

	ollamaModels := make([]map[string]interface{}, 0, len(modelMap))
	for _, v := range modelMap {
		ollamaModels = append(ollamaModels, v)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": ollamaModels})
}

// modelDigest — стабильный content-addressed digest модели.
//
// R65d (2026-09-20): раньше digest вычислялся как fmt.Sprintf("sha256:%x", sizeBytes),
// то есть под видом хеша отдавался РАЗМЕР В БАЙТАХ. Две разные модели
// одинакового размера получали одинаковый digest, и клиенты, использующие
// digest как идентификатор модели (кеши, сравнение версий), их путали.
//
// Полный SHA256 файла считать нельзя: модели по 5-20 GB, а /api/tags и /api/ps
// вызываются часто. Хеш от (путь + имя + размер) стабилен между запросами
// и различает модели.
func modelDigest(path, name string, sizeBytes uint64) string {
	if path == "" && name == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatUint(sizeBytes, 10)))
	return "sha256:" + fmt.Sprintf("%x", h.Sum(nil))
}

// quantizationForPSModel — квантование для записи /api/ps.
//
// LoadedModel в /api/ps может не иметь Path (модель описана только именем),
// а parseQuantization внутри вызывает filepath.Base, который на пустой строке
// даёт ".", поэтому пробуем сначала Path, затем Name.
func quantizationForPSModel(m cppbackend.ModelInfo) string {
	if q := parseQuantization(m.Path); q != "" && q != "unknown" {
		return q
	}
	if m.Name != "" {
		return parseQuantization(m.Name + ".gguf")
	}
	return "unknown"
}

// modelParameterSize — оценка числа параметров модели в формате "7.6B".
//
// Было: fmt.Sprintf("%.1fB", float64(m.NLayers*m.NEmbd)/1e9) — это произведение
// слоёв на размер эмбеддинга, а не число параметров. Для 7B-модели
// (32 слоя × 4096) формула даёт 0.0001B, то есть мусор в UI.
//
// Корректная оценка: params ≈ 12 × n_layers × n_embd² (attention + MLP SwiGLU).
func modelParameterSize(nLayers, nEmbd int) string {
	if nLayers <= 0 || nEmbd <= 0 {
		return "unknown"
	}
	const coeff = 12.0
	billions := coeff * float64(nLayers) * float64(nEmbd) * float64(nEmbd) / 1e9
	if billions < 0.05 {
		return "unknown"
	}
	return fmt.Sprintf("%.1fB", billions)
}

// estimateVRAMSizeForModel — сколько модель занимает VRAM (байты): веса на GPU + KV.
//
// ЧТО БЫЛО НЕ ТАК (R313, 2026-10-30). Прежняя версия считала `size × 3.0 ×
// (gpuLayers/nLayers)` по допущению «квантованные веса в VRAM разворачиваются в
// fp16». Допущение устарело: llama.cpp держит Q4_K_M на GPU квантованным, поэтому
// множитель завышал число втрое, а KV не учитывался вовсе. Живой факт со стенда:
// gemma-4-E4B (файл 4.2 ГБ, ctx 32768, 42/42 слоя на GPU) отдавала
// size_vram = 12.6 ГБ на 8-гиговой карте — физически невозможное значение.
//
// ЧЕМ ЭТО ВРЕДИЛО. Значение уезжает в балансер (poller → LlamaCppMetrics.
// VRAMUsage), а оттуда в autotune-бюджет `min(free + usage, total)`. Клип по
// total не давал решениям сломаться, но бюджет становился оптимистичнее правды
// (8 ГБ вместо реальных ~7.7), а оператор видел в /api/ps невозможное число.
//
// КАК СЧИТАЕМ ТЕПЕРЬ:
//   - веса: доля файла по числу слоёв на GPU (квантование не «разворачивается»);
//   - KV: той же формулой, что и остальной продукт (cppbackend.EstimateKVCacheMB),
//     тоже с долей по слоям — второй формулы KV в проекте не заводится;
//   - отрицательный gpuLayers (-1 = все слои, -2 = AUTO) по-прежнему значит «все
//     слои на GPU»: для AUTO точное число здесь недоступно, а верхняя граница
//     согласуется с CalculateOptimalGPULayers.
//
// ЧЕСТНАЯ ГРАНИЦА: это оценка, а не измерение (llama.cpp не отдаёт per-model VRAM
// через C-API). Но теперь она складывается из двух величин с физическим смыслом,
// а не из множителя «на глаз».
func estimateVRAMSizeForModel(m *cppbackend.ModelInfo, sizeBytes uint64) uint64 {
	if m == nil || sizeBytes == 0 || m.NLayers == 0 {
		return 0
	}
	gpuLayers := m.GPULayers
	if gpuLayers < 0 {
		gpuLayers = m.NLayers
	}
	if gpuLayers == 0 {
		// CPU-only — VRAM не занята.
		return 0
	}
	if gpuLayers > m.NLayers {
		gpuLayers = m.NLayers
	}
	fraction := float64(gpuLayers) / float64(m.NLayers)

	weights := uint64(float64(sizeBytes) * fraction)
	// R705 (2026-11-24): ширина головы K/V — РЕАЛЬНАЯ (из llama_hparams через
	// ModelInfo), а не n_embd/n_heads. У Qwen3-4B прежняя формула давала 80
	// против 128, и size_vram занижался по KV; число уезжает в балансер
	// (VRAMUsage → autotune-бюджет), поэтому геометрия передаётся явно.
	kvMB := cppbackend.EstimateKVCacheMB(m.NLayers, m.NHeads, m.NKvHeads, m.NEmbd,
		m.HeadDimK, m.HeadDimV, m.ContextSize, m.KVCacheType)
	kv := uint64(float64(kvMB) * 1024 * 1024 * fraction)
	return weights + kv
}

// quantizationOrUnknown — Ollama отдаёт "unknown" вместо пустой строки.
func quantizationOrUnknown(q string) string {
	if strings.TrimSpace(q) == "" {
		return "unknown"
	}
	return q
}

// handleOllamaPS — Ollama /api/ps (running models in memory).
// Round 21 hotfix (2026-08-03): раньше не было реализовано, balancer возвращал
// phantom 200 с `{"models":null}`. Теперь возвращает реальный список загруженных
// моделей в формате Ollama, с size_vram / expires_at / size, как ожидает клиент.
func handleOllamaPS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	loadedModels := backend.ListModels()
	psModels := make([]map[string]interface{}, 0, len(loadedModels))

	for _, m := range loadedModels {
		// Ollama-формат: name, model, size, size_vram, digest, expires_at, details.
		// size_vram = сколько занимает в VRAM (если есть), size = общий размер файла.
		// expires_at — через сколько модель выгрузится (idle TTL). У нас пока
		// нет per-model idle TTL, поэтому ставим дефолт 5 минут как в Ollama.
		sizeBytes := m.SizeBytes
		if sizeBytes == 0 && m.LoadingSizeBytes > 0 {
			sizeBytes = uint64(m.LoadingSizeBytes)
		}
		digest := modelDigest(m.Path, m.Name, sizeBytes)
		psModels = append(psModels, map[string]interface{}{
			"name":       m.Name,
			"model":      m.Name,
			"size":       sizeBytes,
			"size_vram":  estimateVRAMSizeForModel(&m, sizeBytes),
			"digest":     digest,
			"expires_at": time.Now().Add(5 * time.Minute).Format(time.RFC3339),
			"details": map[string]interface{}{
				"format":             "gguf",
				"family":             m.Architecture,
				"families":           []string{m.Architecture},
				"parameter_size":     modelParameterSize(m.NLayers, m.NEmbd),
				"quantization_level": quantizationOrUnknown(quantizationForPSModel(m)),
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"models": psModels})
}

// handleOllamaShow ? Ollama /api/show.
//
// Round 44 (2026-08-19) variant 4 structural fix: when the requested model is
// NOT yet loaded into memory, return metadata directly from the GGUF header
// instead of triggering a full llama.cpp model load.
//
// # Why this matters
//
// R31-R43 retained the historical "lazy-load on /api/show" behavior for
// compatibility with Ollama tooling that polls /api/show to discover
// capabilities. The hidden cost is that a 22GB model (Qwen3.6-35B) takes
// 6+ minutes to cold-load on RTX 3070 8GB VRAM, while 5GB gemma-4 takes
// 77s. Real Cline/Codegen clients with hard 5-minute HTTP timeouts
// (claude/claude#12829 reference; Cline CLI OLLAMA_DEFAULT_TIMEOUT_MS=300000)
// cancel the request mid-load. With the old code, cancellation left the
// cppworker still holding the single-flight load slot, blocking every
// subsequent /api/chat for the same backend (the R44 stuck-state symptom).
//
// New flow
//
//  1. If the model is already loaded in memory → return full info from
//     backend.GetModel (state="loaded"; n_vocab/gpu_layers present).
//  2. Otherwise, look up the on-disk path via ModelManager and read
//     metadata-only via mm.GetModelMeta (which parses the GGUF header in
//     <100ms, no model load). Return header-derived fields with
//     state="unloaded" and context_size=0 (load is required to populate
//     runtime-only fields like n_vocab and gpu_layers).
//  3. If neither loaded nor on disk → 404.
//
// Behavior preserved
//   - `/api/show` still answers in well under 1s even for the largest model.
//   - `state` field always set: "loaded" / "unloaded" / "not loaded".
//   - Ollama / OpenWebUI render correctly: they only inspect the family and
//     parameter_size fields for UI hints, both available from the header.
//   - The lazy-load path (ensureModelLoaded) is no longer triggered by
//     /api/show. Clients that actually need inference hit /api/chat or
//     /api/generate, which DO load the model (this is unchanged).
func handleOllamaShow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	if err := decodeJSONRequest(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := req.Name
	if name == "" {
		name = req.Model
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name or model is required")
		return
	}

	// Path 1: model already in memory → return full info (state=loaded).
	if info, err := backend.GetModel(name); err == nil {
		writeOllamaShowLoadedResponse(w, info)
		return
	}

	// Path 2: model NOT loaded → metadata-only via GGUF header (no load).
	// This is the R44 structural fix.
	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager not available")
		return
	}
	meta, metaErr := mm.GetModelMeta(name)
	if metaErr != nil || meta == nil {
		// Fallback: try with .gguf suffix (Ollama clients often send basename).
		if meta2, err2 := mm.GetModelMeta(name + ".gguf"); err2 == nil && meta2 != nil {
			meta = meta2
			metaErr = nil
		}
	}
	if metaErr == nil && meta != nil && meta.Path != "" {
		writeOllamaShowMetadataResponse(w, meta)
		return
	}

	// Path 3: not on disk either → 404 (not changed from previous behavior).
	path, err := mm.FindModelByPath(strings.TrimSuffix(name, ".gguf") + ".gguf")
	if err != nil {
		writeError(w, http.StatusNotFound, "model not found: "+name)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to stat model file: "+err.Error())
		return
	}
	modelName := strings.TrimSuffix(filepath.Base(path), ".gguf")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"license": "unknown", "modelfile": "", "parameters": "unknown",
		"template": "", "system": "",
		"details": map[string]interface{}{
			"parent_model": "", "format": "gguf", "family": "unknown",
			"families": []string{}, "parameter_size": "unknown",
			"quantization_level": "unknown",
		},
		"model_info": map[string]interface{}{
			"name": modelName, "filename": filepath.Base(path),
			"size_bytes": info.Size(), "modified_at": info.ModTime().Format(time.RFC3339),
			"state": "not loaded", "architecture": "unknown",
		},
	})
}

// writeOllamaShowLoadedResponse — Path 1 of /api/show: model is in memory.
// Same response shape as R43 and earlier; n_vocab and gpu_layers present,
// state=loaded.
func writeOllamaShowLoadedResponse(w http.ResponseWriter, info *cppbackend.ModelInfo) {
	// R66 (2026-09-21): use modelParameterSize (12 × n_layers × n_embd² / 1e9) instead
	// of the broken n_layers × n_embd / 1e9 formula, which gave "0.0B" for 7B models
	// (e.g. Qwen3-Instruct: 36 × 2560 = 92160 → 9.2e-5B → "0.0B").
	paramSize := modelParameterSize(info.NLayers, info.NEmbd)
	// R402 (2026-11-05): context_size — окно, доступное ОДНОМУ клиенту (на слот).
	//
	// ЖИВОЙ ДЕФЕКТ. Здесь отдавалось СУММАРНОЕ окно модели (contextSize), а слоты
	// его делят: при parallel=2 и total 131072 один клиент получает 65536.
	// Клиент (Cline/Roo) читает context_size как «свой» размер окна, заполняет
	// историю до мнимого предела, упирается в реальный и показывает ошибку
	// «увеличьте контекст вручную». Отдаём окно на слот, а суммарное и parallel
	// кладём отдельными полями — пусть клиент и оператор видят оба числа.
	perSlotNCtx := info.ContextSize
	if info.ContextSize > 0 {
		perSlotNCtx = cppbackend.ContextPerSlot(info.ContextSize, info.Parallel)
	}
	modelInfo := map[string]interface{}{
		"architecture": info.Architecture, "n_layers": info.NLayers,
		"n_heads": info.NHeads, "n_embd": info.NEmbd,
		// R705 (2026-11-24): head_dim_k/head_dim_v — реальная ширина головы из
		// llama_hparams (<arch>.attention.key_length), а не n_embd/n_heads.
		// head_dim_source говорит, факт это (gguf) или прежняя оценка (computed):
		// иначе занижённая ширина (Qwen3-4B: 80 вместо 128) выглядела измеренной.
		"n_kv_heads": info.NKvHeads, "head_dim_k": info.HeadDimK, "head_dim_v": info.HeadDimV,
		"head_dim_source": info.HeadDimSource,
		"n_vocab":         info.NVocab, "context_size": perSlotNCtx,
		"context_size_total": info.ContextSize, "parallel": info.Parallel,
		"gpu_layers": info.GPULayers, "kv_cache_type": info.KVCacheType, "state": info.State,
	}
	// H3 (аудит): на загруженной модели тоже отдаём "<arch>.context_length" —
	// training context из GGUF-заголовка, ровно как Path 2 (незагруженная ветка,
	// writeOllamaShowMetadataResponse). context_size выше остаётся runtime n_ctx.
	if info.GGUFContextLength > 0 && info.Architecture != "" {
		modelInfo[info.Architecture+".context_length"] = info.GGUFContextLength
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"license": "unknown", "modelfile": "",
		// R403: parameters — это Modelfile-параметры (как у настоящего Ollama),
		// а НЕ размер модели. Клиенты (Cline/Roo) читают отсюда num_ctx, чтобы
		// выбрать окно запроса; "3.3B" в этом поле не давало им ничего, и они
		// падали на своём дефолте. Размер модели остаётся в details.parameter_size.
		"parameters": fmt.Sprintf("num_ctx %d", perSlotNCtx),
		"template":   "", "system": "",
		"details": map[string]interface{}{
			"parent_model": "", "format": "gguf", "family": info.Architecture,
			"families":           []string{info.Architecture},
			"parameter_size":     paramSize,
			"quantization_level": "unknown",
		},
		"model_info": modelInfo,
	})
}

// writeOllamaShowMetadataResponse — Path 2 of /api/show: header-only metadata
// (model NOT in memory; no load triggered).
//
// R44 (2026-08-19) NEW: this is the variant-4 fix. The response shape is
// compatible with Ollama clients that only inspect family / parameter_size
// for UI rendering. n_vocab and gpu_layers are runtime-only fields and are
// zeroed; the state field is "unloaded" so the client knows the model is
// not yet ready for inference (it should call /api/chat or /api/generate
// to actually load and use it).
func writeOllamaShowMetadataResponse(w http.ResponseWriter, meta *cppbackend.GGUFModelMeta) {
	// GGUF header may be missing NEmbd/NLayers for non-llama.cpp-arch files
	// or when ReadGGUFHeader failed silently. Use safe zero defaults.
	nLayers := meta.NLayers
	nEmbd := meta.NEmbd
	// R66 (2026-09-21): use modelParameterSize for accurate parameter estimation.
	paramSize := modelParameterSize(nLayers, nEmbd)
	family := meta.Architecture
	if family == "" {
		family = "unknown"
	}
	// R65d (2026-09-20): отдаём РЕАЛЬНЫЙ контекст вместо нулей.
	//
	// Было: model_info.context_size = 0 и kv_cache_type = "" для незагруженной
	// модели, хотя ContextLength из GGUF-заголовка УЖЕ доступен (meta.ContextLength,
	// cppbackend/model_manager.go:45, заполняется lazy в GetModelMeta). Клиенты
	// (Cline/Roo) читают context_size / llama.context_length, чтобы подобрать
	// размер окна; ноль заставлял их недооценивать модель и урезать историю.
	//
	// Дополнительно: помимо context_size отдаём ключ "<arch>.context_length" —
	// именно его читают клиенты, ориентированные на настоящий Ollama
	// (llama.cpp пишет model_info как "<arch>.context_length").
	contextSize := meta.ContextLength
	modelInfo := map[string]interface{}{
		"architecture": family,
		"n_layers":     nLayers,
		"n_heads":      meta.NHeads,
		"n_embd":       nEmbd,
		"n_kv_heads":   meta.NKvHeads,
		// R705 (2026-11-24): ширина головы K/V ЕСТЬ в GGUF-заголовке
		// (<arch>.attention.key_length/value_length) — незачем отдавать 0, из-за
		// которого клиент/панель считали KV по n_embd/n_heads. head_dim_source
		// говорит, взято ли число из файла (gguf) или это формула (computed).
		"head_dim_k":      meta.KVHeadDim(),
		"head_dim_v":      meta.KVHeadVDim(),
		"head_dim_source": meta.KVHeadDimSource(),
		"n_vocab":         0, // runtime-only (load required)
		// context_size заполнен, когда GGUF-заголовок прочитан; 0 — только если
		// метаданные недоступны (тогда поле остаётся честным нулём).
		"context_size": contextSize,
		"gpu_layers":   0, // runtime-only (load required)
		"kv_cache_type": func() string {
			if meta.KVCacheType != "" {
				return meta.KVCacheType
			}
			return ""
		}(),
		"state": "unloaded",
	}
	if contextSize > 0 {
		// Настоящий Ollama кладёт training context под ключом "<arch>.context_length".
		modelInfo[family+".context_length"] = contextSize
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"license": "unknown", "modelfile": "",
		// R403: Modelfile-параметры вместо размера модели (см. loaded-ветку).
		// Для незагруженной модели доступен только training context из GGUF —
		// это максимальное окно, которое может получить одна сессия.
		"parameters": fmt.Sprintf("num_ctx %d", contextSize),
		"template":   "", "system": "",
		"details": map[string]interface{}{
			"parent_model": "", "format": "gguf", "family": family,
			"families":           []string{family},
			"parameter_size":     paramSize,
			"quantization_level": "unknown",
		},
		"model_info": modelInfo,
	})
}

// handleOllamaCopy ? Ollama /api/copy.
func handleOllamaCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	if err := decodeJSONRequest(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Source == "" || req.Destination == "" {
		writeError(w, http.StatusBadRequest, "source and destination are required")
		return
	}

	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager not available")
		return
	}

	srcName := req.Source
	if !strings.HasSuffix(strings.ToLower(srcName), ".gguf") {
		srcName = srcName + ".gguf"
	}
	dstName := req.Destination
	if !strings.HasSuffix(strings.ToLower(dstName), ".gguf") {
		dstName = dstName + ".gguf"
	}

	srcPath, err := mm.FindModelByPath(srcName)
	if err != nil {
		writeError(w, http.StatusNotFound, "source model not found: "+err.Error())
		return
	}
	dstPath := filepath.Join(filepath.Dir(srcPath), dstName)

	srcFile, err := os.Open(srcPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open source model: "+err.Error())
		return
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dstPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create destination model: "+err.Error())
		return
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to copy model: "+err.Error())
		return
	}

	if _, scanErr := mm.ScanModels(); scanErr != nil {
		logger.Get().Warnw("copy: model rescan failed", "error", scanErr)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "copied", "source": req.Source, "destination": req.Destination,
	})
}

// handleOllamaCreate ? Ollama /api/create.
func handleOllamaCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Name      string `json:"name"`
		ModelFile string `json:"modelfile"`
		From      string `json:"from"`
	}
	if err := decodeJSONRequest(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager not available")
		return
	}

	sourceName := req.From
	if sourceName == "" && req.ModelFile != "" {
		lines := strings.Split(req.ModelFile, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(strings.ToUpper(line), "FROM ") {
				sourceName = strings.TrimSpace(line[5:])
				break
			}
		}
	}
	if sourceName == "" {
		writeError(w, http.StatusBadRequest, "source model is required (from field or FROM line in modelfile)")
		return
	}
	if !strings.HasSuffix(strings.ToLower(sourceName), ".gguf") {
		sourceName = sourceName + ".gguf"
	}

	srcPath, err := mm.FindModelByPath(sourceName)
	if err != nil {
		writeError(w, http.StatusNotFound, "source model not found: "+err.Error())
		return
	}

	modelName := req.Name
	if strings.HasSuffix(strings.ToLower(modelName), ".gguf") {
		modelName = modelName[:len(modelName)-5]
	}
	dir := filepath.Dir(srcPath)
	aliasPath := filepath.Join(dir, modelName+".gguf.json")

	alias := map[string]interface{}{
		"name": modelName, "source": sourceName,
		"created_at": time.Now().Format(time.RFC3339), "modelfile": req.ModelFile,
	}
	aliasJSON, err := json.MarshalIndent(alias, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal alias: "+err.Error())
		return
	}
	if err := os.WriteFile(aliasPath, aliasJSON, 0644); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write alias file: "+err.Error())
		return
	}

	// R66d (2026-09-23): пересканируем каталог, иначе созданный алиас не попадёт
	// в реестр ModelManager, и клиент не увидит модель в /api/tags до рестарта
	// (та же причина, что у HF-загрузки: ListModels/ListAliases читают кэш).
	if _, scanErr := mm.ScanModels(); scanErr != nil {
		logger.Get().Warnw("create: rescan after alias creation failed",
			"alias", modelName, "error", scanErr)
	} else {
		logger.Get().Infow("create: model alias created", "alias", modelName, "source", sourceName)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "created", "name": modelName})
}

// handleOllamaPush — Ollama /api/push: не поддерживается для llama.cpp backend.
func handleOllamaPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	writeError(w, http.StatusNotImplemented, "Ollama registry push is not supported for llama.cpp backends")
}

// resolveModelPath резолвит имя модели в абсолютный путь к .gguf файлу.
//
// Стратегия поиска (от точного к приблизительному):
//  1. ModelManager.FindModelByPath — прямое совпадение имени или пути
//     1.5 Алиас модели (<name>.gguf.json, POST /api/create) — путь к источнику
//  2. Glob "modelsDir/<name>*.gguf" — wildcards (qwen3-4b → qwen3-4b-it.gguf)
//  3. ModelManager library scan — если в директории РОВНО ОДИН .gguf, берём его
//     (auto-pick the only model — пользователь загрузил одну модель с коротким именем)
//  4. Fallback: "modelsDir/<name>.gguf" (вызовет 500 на cppworker, но с осмысленной ошибкой)
//
// Round 21 hotfix (2026-08-03): добавлен шаг 3. Раньше при `name=qwen3-4b` и единственном
// файле `Qwen3-Instruct-2507-q4km.gguf` в директории — балансер не мог auto-load
// модель (имя не совпадало с файлом), и все inference-эндпоинты после idle-unload
// падали с HTTP 500 "load failed". Теперь single-model dirs auto-pick'аются.
//
// R66d (2026-09-23): добавлен шаг 1.5. POST /api/create создаёт алиас
// <name>.gguf.json, но load-путь про него не знал и пытался открыть
// models/<name>.gguf → «model load failed» (живой кейс: алиас
// gemma-4-E4B-it-Q4_K_M на скачанный gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf).
func resolveModelPath(modelName string) string {
	mm := backend.ModelManager()
	if mm != nil {
		if foundPath, err := mm.FindModelByPath(modelName); err == nil {
			return foundPath
		}
		// Шаг 1.5: алиас модели (Ollama-совместимый /api/create).
		if srcPath, ok := mm.AliasSourcePath(modelName); ok {
			logger.Get().Infow("resolveModelPath: resolved via model alias",
				"requested_name", modelName, "resolved_path", srcPath)
			return srcPath
		} else if srcPath != "" {
			// Алиас есть, но файл-источник пропал: возвращаем путь как есть —
			// cppworker ответит понятной ошибкой загрузки, а /api/models/load/progress
			// покажет state=failed с причиной (R66d), вместо «модель не найдена».
			logger.Get().Warnw("resolveModelPath: alias source file is missing",
				"requested_name", modelName, "source_path", srcPath)
			return srcPath
		}
		// Шаг 2: glob
		modelPath := filepath.Join(*modelsDir, modelName)
		if !strings.HasSuffix(modelPath, ".gguf") {
			if matches, err := filepath.Glob(modelPath + "*.gguf"); err == nil && len(matches) > 0 {
				return matches[0]
			}
			modelPath += ".gguf"
		}

		// Шаг 3 (Round 21 hotfix): auto-pick единственного .gguf в директории.
		// Типичный случай: пользователь скачал одну модель и переименовал её
		// при load (name=qwen3-4b, file=Qwen3-Instruct-2507-q4km.gguf). При
		// auto-load после idle-unload — балансер не знает правильного path, и
		// ищет models/qwen3-4b.gguf. Если в директории РОВНО ОДИН .gguf файл —
		// логично предположить, что это и есть нужная модель.
		files := mm.ListModels()
		if len(files) == 1 {
			logger.Get().Infow("resolveModelPath: auto-pick single .gguf from modelsDir",
				"requested_name", modelName, "resolved_path", files[0].Path)
			return files[0].Path
		}

		return modelPath
	}
	// Fallback без ModelManager
	modelPath := filepath.Join(*modelsDir, modelName)
	if !strings.HasSuffix(modelPath, ".gguf") {
		if matches, err := filepath.Glob(modelPath + "*.gguf"); err == nil && len(matches) > 0 {
			return matches[0]
		}
		modelPath += ".gguf"
	}
	return modelPath
}

// reloadSplitMode — режим раскладки для /api/models/reload.
//
// R179 (2026-10-19). Причина живого дефекта «Применить профиль теряет раскладку»:
// reload не передавал SplitMode, в мост уходил 0 = LLAMA_SPLIT_MODE_NONE, и
// llama.cpp оставлял из списка устройств только main_gpu (RPC-узел отбрасывался),
// хотя планировщик видел два устройства и серверы передавались. Путь
// /api/models/load-with-params режим передавал всегда — поэтому свежая загрузка
// раскладывала слои, а reload нет.
//
// Правило: явный splitMode запроса (если задан) > CPPWORKER_SPLIT_MODE >
// DefaultSplitMode конфига > -1 (дефолт llama.cpp = LAYER). Ноль как «не задано»
// здесь НЕ трактуем: 0 — это законное NONE, и подменять его нельзя.
func reloadSplitMode(requested *int) int {
	if requested != nil && *requested >= -1 && *requested <= 3 {
		return *requested
	}
	if v := strings.TrimSpace(os.Getenv("CPPWORKER_SPLIT_MODE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= -1 && n <= 3 {
			return n
		}
	}
	if currentConfig != nil && currentConfig.DefaultSplitMode >= -1 && currentConfig.DefaultSplitMode <= 3 {
		return currentConfig.DefaultSplitMode
	}
	return -1
}
