package main

// ============================================================
// Dynamic / async model loading (Round 24, 2026-08-04)
//
// Проблема: gemma-4 (5GB) и большие модели загружаются 60-180+ секунд.
// Раньше handleLoadModel блокировал HTTP-запрос на всё время загрузки —
// клиент (curl, OpenWebUI, Cline) рвал соединение по своему таймауту
// (60-180s), и UI показывал "load failed" хотя на cppworker модель
// продолжала грузиться в фоне (CGo-вызов bridge.LoadModel не
// отменяется при разрыве HTTP).
//
// Решение: динамический async load.
//   - По умолчанию handleLoadModel возвращает 202 Accepted СРАЗУ
//     с Location: /api/models/load/progress?model=<name> и
//     estimatedLoadTimeMs (compute from file size + context size).
//   - Реальная загрузка идёт в фоне (goroutine, не привязана к r.Context()).
//   - Клиент (WebUI GgufLoadProgress) полит progress endpoint —
//     состояния loading/loaded/error — и показывает спиннер.
//   - Для legacy Ollama clients (?wait=true&waitTimeoutSec=N) —
//     старый sync-режим: блокируем до готовности, но не дольше
//     waitTimeoutSec. При таймауте — 202 с текущим state.
//
// Dynamic load time estimation (estimateLoadTimeMs):
//   base = sizeBytes / loadSpeedBytesPerSec (~100 MB/s для NVMe SSD)
//   ctxFactor = contextSize / 8192 (KV-cache init, ~0.5s per 4K tokens)
//   overhead = 2s (mmap, metadata parse, locks)
//   estimate = base + ctxFactor + overhead
//
// Для gemma-4-E4B-it-Q4_K_M.gguf (5GB) с 32K ctx:
//   50s + 2s + 2s = ~54s (реально 60-90s, conservative +20%)
//
// Клиент видит эту оценку и расширяет свой таймаут, либо полит progress.
// ============================================================

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// loadSpeedBytesPerSec — оценочная скорость чтения модели с диска для расчёта
// времени загрузки. NVMe SSD типично 1-3 GB/s, но мы используем 100 MB/s
// (запас для медленных дисков, mmap, KV-cache init, GPU upload).
const loadSpeedBytesPerSec = 100 * 1024 * 1024 // 100 MB/s

// loadOverheadSec — фиксированный оверхед на mmapping, парсинг GGUF
// header, инициализацию llama.cpp context, lock'и.
const loadOverheadSec = 2.0

// loadBaseMinMs — минимальная оценка (1 секунда) чтобы клиент не подумал
// что load мгновенный.
const loadBaseMinMs = 1000

// loadCtxInitMsPer4K — миллисекунды на инициализацию KV-cache для 4K
// токенов контекста. Эмпирически ~0.5s per 4K tokens (q4_0 KV).
const loadCtxInitMsPer4K = 500

// parseLoadWaitParams извлекает параметры синхронности из query string.
//
// Поддерживает:
//
//	?wait=true                      → блокирующий режим (legacy Ollama clients)
//	?wait=false                     → async (default)
//	?waitTimeoutSec=N (default 60)  → макс. время ожидания в sync-режиме
//
// Default (без query params) — async, потому что:
//   - WebUI GgufLoadProgress умеет поллить /api/models/load/progress
//   - Не блокируем HTTP worker на 60+ секунд
//   - Клиент получает 202 + estimatedLoadTimeMs и сам решает как ждать
func parseLoadWaitParams(r *http.Request) (wait bool, waitTimeoutMs int) {
	wait = false
	waitTimeoutMs = 60000 // 60s default for sync mode

	q := r.URL.Query()
	if v := q.Get("wait"); v != "" {
		// parseBool понимает "1", "true", "True", "TRUE" как true.
		if parsed, err := strconv.ParseBool(v); err == nil {
			wait = parsed
		}
	}
	if v := q.Get("waitTimeoutSec"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 && sec <= 3600 {
			waitTimeoutMs = sec * 1000
		}
	}
	return wait, waitTimeoutMs
}

// estimateLoadTimeMs вычисляет примерное время загрузки модели в миллисекундах
// на основе размера файла и размера контекста.
//
// Round 25 (2026-08-04): использует Backend.EstimatedBytesPerSec (измеренная
// скорость на основе последних N=20 load'ов) если доступна, иначе fallback
// на хардкод 100 MB/s. Для gemma-4 (5GB) реальная скорость ~55-83 MB/s
// (60-90s load), а 100 MB/s хардкод занижает оценку → клиент думает "скоро"
// → timeout. Measured cache даёт реалистичную оценку.
//
// Возвращает 0 если файл недоступен (например, hf: downloader ещё не скачал).
// В этом случае клиент должен поллить progress и не полагаться на оценку.
//
// Формула: max(loadBaseMinMs, size/speed + ctx/4K*initMs + overhead*1000)
// где speed = backend.EstimatedBytesPerSec() ?? 100MB/s (если нет данных).
func estimateLoadTimeMs(modelPath string, ctxSize int) int64 {
	if modelPath == "" {
		return 0
	}
	fi, err := os.Stat(modelPath)
	if err != nil {
		// File not accessible (hf: downloader hasn't downloaded yet, or path typo).
		// Caller should poll progress without relying on estimate.
		return 0
	}
	sizeBytes := fi.Size()
	if sizeBytes <= 0 {
		return 0
	}

	// Round 25: используем measured speed если есть история.
	// Хардкод 100MB/s — fallback для первого load'а (когда история пуста).
	speedBytesPerSec := int64(loadSpeedBytesPerSec)
	if backend != nil {
		if measured := backend.EstimatedBytesPerSec(); measured > 0 {
			speedBytesPerSec = measured
			logger.Get().Debugw("estimateLoadTimeMs: using measured speed",
				"measured_bps", measured,
				"hardcoded_bps", loadSpeedBytesPerSec)
		}
	}

	// Base: disk read time at speed (measured или hardcoded).
	baseMs := int64(float64(sizeBytes) / float64(speedBytesPerSec) * 1000.0)

	// Ctx factor: KV-cache initialization for contextSize tokens.
	ctxMs := int64(0)
	if ctxSize > 0 {
		blocksOf4K := (ctxSize + 4095) / 4096
		ctxMs = int64(blocksOf4K) * loadCtxInitMsPer4K
	}

	overheadMs := int64(loadOverheadSec * 1000.0)

	total := baseMs + ctxMs + overheadMs
	if total < loadBaseMinMs {
		total = loadBaseMinMs
	}
	return total
}

// writeLoadAccepted отвечает HTTP 202 Accepted с Location header и JSON body
// {status: "loading", name, loadingSizeBytes, estimatedLoadTimeMs,
//
//	progressUrl, model: <ModelInfo with state=loading>}.
//
// Используется для async load — клиент полит progressUrl пока state не
// станет "loaded" или "error".
func writeLoadAccepted(w http.ResponseWriter, r *http.Request,
	modelName string, modelPath string, modelSize int64,
	estimatedMs int64, info interface{},
) {
	progressURL := fmt.Sprintf("/api/models/load/progress?model=%s",
		url.QueryEscape(modelName))
	// Build absolute URL for Location header (Location requires absolute or root-relative).
	location := progressURL
	if r.Host != "" {
		// Use r.Host + r.TLS check to build full URL.
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		location = fmt.Sprintf("%s://%s%s", scheme, r.Host, progressURL)
	}
	w.Header().Set("Location", location)
	w.Header().Set("Content-Type", "application/json")

	body := map[string]interface{}{
		"status":              "loading",
		"name":                modelName,
		"path":                modelPath,
		"loadingSizeBytes":    modelSize,
		"estimatedLoadTimeMs": estimatedMs,
		"progressUrl":         progressURL,
		"model":               info,
		"message":             "Model load started in background. Poll progressUrl for state transitions (loading → loaded / error).",
	}
	writeJSON(w, http.StatusAccepted, body)
}

// writeLoadWaitTimeout отвечает HTTP 202 Accepted когда sync-режим (?wait=true)
// исчерпал waitTimeoutMs. По сути то же что writeLoadAccepted, но с другим
// status-полем, чтобы клиент понял: "load ещё идёт, дождись сам".
func writeLoadWaitTimeout(w http.ResponseWriter, r *http.Request,
	modelName string, modelPath string, modelSize int64,
	waitedMs int64, estimatedMs int64, info interface{},
) {
	progressURL := fmt.Sprintf("/api/models/load/progress?model=%s",
		url.QueryEscape(modelName))
	location := progressURL
	if r.Host != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		location = fmt.Sprintf("%s://%s%s", scheme, r.Host, progressURL)
	}
	w.Header().Set("Location", location)
	w.Header().Set("Content-Type", "application/json")

	body := map[string]interface{}{
		"status":              "loading_after_timeout",
		"name":                modelName,
		"path":                modelPath,
		"loadingSizeBytes":    modelSize,
		"waitedMs":            waitedMs,
		"estimatedLoadTimeMs": estimatedMs,
		"progressUrl":         progressURL,
		"model":               info,
		"message":             fmt.Sprintf("Load not finished after %dms wait. Poll progressUrl.", waitedMs),
	}
	writeJSON(w, http.StatusAccepted, body)
}

// runAsyncLoad запускает реальную загрузку в background goroutine.
//
// По завершении вызывает backend.UnlockLoad(modelName) чтобы concurrent
// load-запросы могли получить info о загруженной модели.
//
// ВАЖНО: горутина НЕ привязана к r.Context() — клиент может отвалиться
// по таймауту, но load продолжится. CGo-вызов bridge.LoadModel не
// отменяется через context, поэтому единственный способ "отменить"
// загрузку — это дождаться её завершения или убить процесс.
func runAsyncLoad(modelName, modelPath string, opts cppbackend.LoadModelOpts,
	balancerReg balancerRegNotifier) {
	start := time.Now()
	logger.Get().Infow("runAsyncLoad: starting background load",
		"name", modelName, "path", modelPath,
		"ctxSize", opts.ContextSize,
		"gpuLayers", opts.GPULayers)

	loadErr := backend.LoadModelWithOpts(modelName, modelPath, opts)
	duration := time.Since(start)

	if loadErr != nil {
		logger.Get().Errorw("runAsyncLoad: background load failed",
			"name", modelName, "error", loadErr, "duration_ms", duration.Milliseconds())
		// Backend already cleaned up b.models[name] in LoadModelWithOpts error path
		// (line ~646 in backend.go). The instance is removed.
	} else {
		logger.Get().Infow("runAsyncLoad: background load complete",
			"name", modelName, "duration_ms", duration.Milliseconds())
		// Notify balancer so the model becomes "ready" for routing.
		if balancerReg != nil {
			if info, err := backend.GetModel(modelName); err == nil {
				// Round 34 (2026-08-12): передаём runtime params (kvCacheType,
				// flashAttnType, useMmap) для profile mismatch detection в
				// balancer preflight_nctx (Phase 2).
				balancerReg.notifyModelLoaded(modelName, info.Path, info.SizeBytes,
					info.ContextSize, info.GPULayers,
					info.KVCacheType, info.FlashAttnType, info.UseMmap)
			}
		}
	}
	// Always unlock so other load requests can proceed.
	backend.UnlockLoad(modelName)
}

// currentModelInfo — локальный snapshot типа *cppbackend.ModelInfo, чтобы
// runAsyncReload мог знать старые параметры для rollback при ошибке.
type currentModelInfo struct {
	Name          string
	Path          string
	GPULayers     int
	BatchSize     int
	FlashAttnType int
	NUMA          bool
	UseMmap       bool
	TensorSplit   []float32
	ContextSize   int
}

// runAsyncReload — фоновая перезагрузка модели (unload + load) с новыми
// параметрами. Используется в handleReloadModel при async-режиме
// (?wait=false, default).
//
// Round 24 (2026-08-04): Bug #1 fix — раньше WebUI settings UI зависал
// на 30-60+ секунд при reload'е reasoning-модели (gemma-4 5GB).
// Теперь HTTP-запрос завершается за миллисекунды с 202 + Location,
// реальный reload идёт в фоне. Клиент polls /api/models/load/progress.
//
// При ошибке load — пытаемся rollback к старым параметрам.
// В любом случае UnlockLoad в конце.
func runAsyncReload(modelName, modelPath string, opts cppbackend.LoadModelOpts,
	current *cppbackend.ModelInfo, balancerReg balancerRegNotifier) {
	start := time.Now()
	logger.Get().Infow("runAsyncReload: starting background reload",
		"name", modelName, "path", modelPath,
		"new_ctx", opts.ContextSize, "new_gpu_layers", opts.GPULayers)

	// Wait for in-flight requests to drain (graceful unload).
	if inflight := backend.InFlight(); inflight != nil {
		if n := inflight.Get(modelName); n > 0 {
			logger.Get().Infow("runAsyncReload: waiting for in-flight to drain",
				"name", modelName, "in_flight", n)
		}
		inflight.WaitZero(modelName, 0)
	}

	// Unload old model.
	if err := backend.UnloadModel(modelName); err != nil {
		logger.Get().Errorw("runAsyncReload: unload failed",
			"name", modelName, "error", err)
		backend.UnlockLoad(modelName)
		return
	}

	// Load with new opts.
	loadErr := backend.LoadModelWithOpts(modelName, modelPath, opts)
	duration := time.Since(start)

	if loadErr != nil {
		logger.Get().Errorw("runAsyncReload: load with new params failed",
			"name", modelName, "error", loadErr)
		// Rollback: try to reload with old params.
		oldOpts := cppbackend.LoadModelOpts{
			GPULayers:     current.GPULayers,
			ContextSize:   current.ContextSize,
			BatchSize:     current.BatchSize,
			FlashAttnType: current.FlashAttnType,
			NUMA:          current.NUMA,
			UseMmap:       current.UseMmap,
			TensorSplit:   current.TensorSplit,
		}
		if rollbackErr := backend.LoadModelWithOpts(modelName, modelPath, oldOpts); rollbackErr != nil {
			logger.Get().Errorw("runAsyncReload: rollback failed (model is no longer loaded!)",
				"name", modelName, "rollback_error", rollbackErr)
		}
		backend.UnlockLoad(modelName)
		return
	}

	logger.Get().Infow("runAsyncReload: background reload complete",
		"name", modelName, "duration_ms", duration.Milliseconds())

	if balancerReg != nil {
		if info, err := backend.GetModel(modelName); err == nil {
			// Round 34 (2026-08-12): передаём runtime params для profile
			// mismatch detection.
			balancerReg.notifyModelLoaded(modelName, info.Path, info.SizeBytes,
				info.ContextSize, info.GPULayers,
				info.KVCacheType, info.FlashAttnType, info.UseMmap)
		}
	}
	backend.UnlockLoad(modelName)
}

// balancerRegNotifier — локальный интерфейс чтобы не зависеть от точного типа
// balancerReg (он определён в balancer_register.go). Это позволяет нам передать
// либо настоящий регистратор, либо nil в тестах.
//
// Сигнатура соответствует (*balancerRegistration).notifyModelLoaded в
// balancer_register.go: (string, string, uint64, int, int, string, int, bool).
// Round 34 (2026-08-12): добавлены runtime params (kvCacheType, flashAttnType,
// useMmap) для profile mismatch detection в balancer preflight (Phase 2).
// R60.4 (2026-09-04): добавлен modelPath для передачи в webui через notify
// (path нужен webui для отображения имени файла, size, quantization).
// Добавлен notifyModelUnloaded (Phase 3) для сброса stale state в coordinator.
type balancerRegNotifier interface {
	notifyModelLoaded(name string, modelPath string, sizeBytes uint64, ctxSize, gpuLayers int, kvCacheType string, flashAttnType int, useMmap bool)
	notifyModelUnloaded(name string)
}

// derefIntPtr — маленький хелпер чтобы не писать if x != nil { *x } else 0.
func derefIntPtr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// derefBoolPtr — аналог derefIntPtr для bool.
func derefBoolPtr(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}
