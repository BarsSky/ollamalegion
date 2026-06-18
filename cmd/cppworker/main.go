// CppBackend Worker — HTTP сервер для llama.cpp GGUF моделей
//
// Предоставляет REST API для загрузки/выгрузки GGUF моделей,
// инференса (синхронного и стриминг), управления multi-GPU,
// совместимый с API форматом Ollama.
//
// Usage:
//
//	cppworker --port 18091 --models-dir ./models
//	cppworker --port 18091 --gpu-layers -1 --flash-attn
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"

	"go.uber.org/zap"
)

// ============================================================
// Флаги командной строки
// ============================================================

var (
	port          = flag.Int("port", 18092, "HTTP server port (default 18092; 18091 is legacy)")
	modelsDir     = flag.String("models-dir", "./models", "Directory with GGUF model files")
	ctxSize       = flag.Int("ctx-size", 4096, "Default context size")
	batchSize     = flag.Int("batch-size", 512, "Default batch size")
	gpuLayers     = flag.Int("gpu-layers", -1, "GPU layers (-1=all, 0=CPU)")
	flashAttn     = flag.Int("flash-attn", -1, "Flash Attention type: -1=auto, 0=disabled, 1=enabled")
	numa          = flag.Bool("numa", false, "Enable NUMA optimization")
	noMmap        = flag.Bool("no-mmap", false, "Disable mmap")
	ramFallbackNCtx       = flag.Bool("ram-fallback-n-ctx", false, "Auto-reload model with requested n_ctx using RAM when VRAM is insufficient")
	ramFallbackGpuLayers  = flag.Int("ram-fallback-gpu-layers", -1, "GPU layers to use during RAM fallback (-1=keep current, 0=CPU-only)")
	ramFallbackMaxNCtx    = flag.Int("ram-fallback-max-n-ctx", 32768, "Max n_ctx allowed for RAM fallback")
	verbose       = flag.Bool("verbose", false, "Verbose logging")
	allowedOrigin = flag.String("cors-origin", "*", "CORS allowed origin")
	envFile       = flag.String("env", "", "Path to .env configuration file (optional)")
	preloadModels = flag.Bool("preload-models", false, "Preload all .gguf models at startup (disabled by default — use with care, may exhaust VRAM)")
	healthCheck   = flag.Bool("healthcheck", false, "Run a one-shot health probe against /health and exit")
)

// ============================================================
// Sentinels
// ============================================================

// errModelIsLoading возвращается ensureModelLoaded, когда модель ещё
// загружается (другая горутина вызвала LoadModelWithOpts). Хендлеры
// перехватывают её и отвечают 503 Service Unavailable с JSON
// {"error":"model is loading", "loading":true, "elapsedMs": N, "retryAfterMs": 3000}.
// Клиент (Ollama/OpenWebUI/наш WebUI) интерпретирует это как «подожди и повтори»
// и не разрывает соединение.
var errModelIsLoading = fmt.Errorf("model is loading")

// isModelLoadingError — true, если ошибка возникла из-за того, что модель
// сейчас в процессе загрузки (другая горутина держит CGo bridge.LoadModel).
func isModelLoadingError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "model is loading")
}

// loadingInfoFor — хелпер: возвращает loading-метаданные для UI из ошибки
// ErrModelIsLoading. В текущей реализации elapsed/retry фиксированные (3 сек);
// в будущем можно парсить из ошибки.
func loadingInfoFor(_ error) (elapsedMs int64, retryAfterMs int) {
	return 0, 3000
}

// ============================================================
// Global state
// ====================================

// balancerReg — глобальная ссылка на auto-registration, чтобы хендлеры
// (handleLoadModel, ensureModelLoaded) могли уведомлять балансировщик
// о событиях загрузки/выгрузки моделей. nil, если auto-registration
// отключён (CPPWORKER_BALANCER_URL не задан).
var balancerReg *balancerRegistration

var backend *cppbackend.Backend
var uptimeStart = time.Now()

// packageLogger — единая точка доступа к zap-логгеру для goroutine'ов,
// которые не получают *zap.SugaredLogger параметром. Используется в
// balancerRegistration.notifyModelLoaded (fire-and-forget callback).
// Возвращает SugaredLogger (sugar), API совместим с logger.Get().
func packageLogger() *zap.SugaredLogger {
	return logger.Get()
}

// ============================================================
// Middleware
// ============================================================

// authMiddleware проверяет API_TOKEN для защищённых эндпоинтов (PUT/POST/DELETE)
func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := os.Getenv("API_TOKEN")
		if token == "" {
			next(w, r)
			return
		}
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") || strings.TrimPrefix(authHeader, "Bearer ") != token {
			writeError(w, http.StatusUnauthorized, "invalid or missing API token")
			return
		}
		next(w, r)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", *allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-HF-Token")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		flusher, _ := w.(http.Flusher)
		lw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK, flusher: flusher}
		next.ServeHTTP(lw, r)
		duration := time.Since(start)
		remoteIP := r.RemoteAddr
		if idx := strings.LastIndex(r.RemoteAddr, ":"); idx > 0 {
			remoteIP = r.RemoteAddr[:idx]
		}
		logger.Get().Infow("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lw.statusCode,
			"duration", duration.String(),
			"remote", remoteIP)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	flusher    http.Flusher
}

func (lw *loggingResponseWriter) WriteHeader(code int) {
	lw.statusCode = code
	lw.ResponseWriter.WriteHeader(code)
}

func (lw *loggingResponseWriter) Flush() {
	if lw.flusher != nil {
		lw.flusher.Flush()
	}
}

// ============================================================
// JSON утилиты
// ============================================================

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logger.Get().Errorw("failed to write JSON response", "error", err)
	}
}

// writeCppWorkerErrorWithBridgeInfo — структурированный JSON-ответ об ошибке
// с прокинутым кодом C-bridge и BridgeErrorInfo.
//
// Зачем: balancer (internal/balancer) различает типы ошибок через
// errors.Is(err, bridge.ErrNCtxNeedsReload). Чтобы он мог это сделать
// при HTTP 4xx/5xx-ответе от cppworker, мы добавляем в JSON-тело
// код из bridge.GetLastErrorInfo().Code (= BRIDGE_ERR_N_CTX_NEEDS_RELOAD=2
// или BRIDGE_ERR_PROMPT_TOO_LONG=3 или generic=1), а также полный
// bridge_info с current_n_ctx / required_n_ctx / max_vram_n_ctx.
//
// Формат:
//
//	{
//	  "error": "stream inference failed with code 2: ...",
//	  "code":  2,
//	  "bridge_info": {
//	    "code": 2, "current_n_ctx": 4096, "required_n_ctx": 16384,
//	    "actual_tokens": 8000, "n_predict": 4096, "n_ctx_override": 16384,
//	    "max_vram_n_ctx": 77000, "message": "..."
//	  }
//	}
//
// Вызывающий код использует http.StatusBadRequest (400) для ошибочных
// запросов клиента (override > loaded n_ctx, prompt too long) и
// http.StatusInternalServerError (500) для внутренних.
func writeCppWorkerErrorWithBridgeInfo(w http.ResponseWriter, status int, prefix string, err error) {
	info := bridge.GetLastErrorInfo()
	body := map[string]interface{}{
		"error": prefix + ": " + err.Error(),
		"code":  info.Code,
		"bridge_info": map[string]interface{}{
			"code":           info.Code,
			"current_n_ctx":  info.CurrentNCtx,
			"required_n_ctx": info.RequiredNCtx,
			"actual_tokens":  info.ActualTokens,
			"n_predict":      info.NPredict,
			"n_ctx_override": info.NCtxOverride,
			"max_vram_n_ctx": info.MaxVRAMNCtx,
			"message":        info.Message,
		},
	}
	writeJSON(w, status, body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ============================================================
// Health & Info handlers
// ============================================================

func handleHealth(w http.ResponseWriter, r *http.Request) {
	version := "initializing"
	if backend != nil {
		version = backend.Version()
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": version,
	})
}

func handleInfo(w http.ResponseWriter, r *http.Request) {
	status := backend.Status()
	writeJSON(w, http.StatusOK, status)
}

func handleGPUInfo(w http.ResponseWriter, r *http.Request) {
	metrics := backend.GetGPUMetrics()
	devices := backend.GetGPUDevices()
	result := make([]map[string]interface{}, len(devices))
	for i, dev := range devices {
		result[i] = map[string]interface{}{
			"index":       dev.Index,
			"name":        dev.Name,
			"vramTotalMB": dev.VRAMTotalMB,
			"vramFreeMB":  dev.VRAMFreeMB,
		}
		if i < len(metrics) {
			for k, v := range metrics[i] {
				result[i][k] = v
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"gpuCount": backend.GetGPUCount(),
		"devices":  result,
	})
}

// ============================================================
// Model management handlers
// ============================================================

type loadModelRequest struct {
	Name          string    `json:"name"`
	Path          string    `json:"path,omitempty"`
	GPULayers     *int      `json:"gpuLayers,omitempty"`
	ContextSize   *int      `json:"contextSize,omitempty"`
	BatchSize     *int      `json:"batchSize,omitempty"`
	TensorSplit   []float32 `json:"tensorSplit,omitempty"`
	FlashAttnType *int      `json:"flashAttn,omitempty"`
	NUMA          *bool     `json:"numa,omitempty"`
	UseMmap       *bool     `json:"useMmap,omitempty"`
}

func defaultBoolPtr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func defaultIntPtr(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// isMemorySlotError — определяет, является ли ошибка llama.cpp "memory slot" leak.
// После такой ошибки контекст модели остаётся в неконсистентном состоянии
// и последующие decode-вызовы также будут падать. Единственный надёжный путь —
// перезапустить процесс, чтобы docker-compose поднял контейнер заново с чистым VRAM.
func isMemorySlotError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "failed to find a memory slot") ||
		strings.Contains(msg, "memory slot") ||
		strings.Contains(msg, "no slot")
}

// maybeRestartOnMemorySlotError — если err связан с memory slot leak,
// логируем критическую ситуацию и завершаем процесс. Docker restart policy
// (например, `restart: unless-stopped` или `restart: on-failure`) перезапустит
// контейнер с чистым состоянием. Это устраняет долгосрочный context-slot leak
// в llama.cpp, который не освобождается при decode-исключениях.
func maybeRestartOnMemorySlotError(err error, modelName string) {
	if !isMemorySlotError(err) {
		return
	}
	logger.Get().Errorw("CRITICAL: memory slot leak detected in llama.cpp; restarting process to recover",
		"model", modelName, "error", err.Error())
	// Даём немного времени, чтобы HTTP-ответ клиенту завершился (если стрим уже частично отправлен).
	// Но в случае streaming — клиент уже получил done:error и disconnect'нулся,
	// так что задержка не критична.
	time.Sleep(200 * time.Millisecond)
	os.Exit(1)
}

func handleLoadModel(w http.ResponseWriter, r *http.Request) {
	var req loadModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	modelPath := req.Path
	modelName := req.Name

	// Резолвим путь для HF-скачанных моделей (префикс hf:)
	if strings.HasPrefix(modelName, "hf:") && backend.HFDownloader() != nil {
		// hf:repo/filename → ищем локальный путь
		parts := strings.TrimPrefix(modelName, "hf:")
		localPath, err := backend.HFDownloader().GetLocalPath(parts)
		if err == nil && localPath != "" {
			modelPath = localPath
		}
	}

	// Если путь не задан явно, ищем через ModelManager
	if modelPath == "" {
		mm := backend.ModelManager()
		if mm != nil {
			// Сначала пробуем найти через FindModelByPath (имя/частичное совпадение)
			foundPath, err := mm.FindModelByPath(modelName)
			if err == nil {
				modelPath = foundPath
			} else {
				// Fallback: конструируем путь из modelsDir
				modelPath = filepath.Join(*modelsDir, modelName)
				if !strings.HasSuffix(modelPath, ".gguf") {
					matches, err := filepath.Glob(modelPath + "*.gguf")
					if err == nil && len(matches) > 0 {
						modelPath = matches[0]
					} else {
						modelPath += ".gguf"
					}
				}
			}
		} else {
			modelPath = filepath.Join(*modelsDir, modelName)
			if !strings.HasSuffix(modelPath, ".gguf") {
				matches, err := filepath.Glob(modelPath + "*.gguf")
				if err == nil && len(matches) > 0 {
					modelPath = matches[0]
				} else {
					modelPath += ".gguf"
				}
			}
		}
	}

	// Собираем опции загрузки
	opts := cppbackend.LoadModelOpts{
		GPULayers:     defaultIntPtr(req.GPULayers, -1),
		ContextSize:   defaultIntPtr(req.ContextSize, *ctxSize),
		BatchSize:     defaultIntPtr(req.BatchSize, *batchSize),
		FlashAttnType: defaultIntPtr(req.FlashAttnType, *flashAttn),
		NUMA:          defaultBoolPtr(req.NUMA, *numa),
		UseMmap:       defaultBoolPtr(req.UseMmap, !*noMmap),
		TensorSplit:   req.TensorSplit,
	}

	logger.Get().Infow("loading model",
		"name", modelName,
		"path", modelPath,
		"gpuLayers", opts.GPULayers,
		"ctxSize", opts.ContextSize,
		"batchSize", opts.BatchSize,
		"flashAttnType", opts.FlashAttnType,
		"numa", opts.NUMA,
		"tensorSplit", opts.TensorSplit)

	// === Per-model blocking load ===
	// Пытаемся зарезервировать эксклюзивное право на загрузку этой модели.
	// Если другая горутина уже грузит её — ждём завершения (до 5 минут).
	// Это устраняет race condition между handleLoadModel, ensureModelLoaded
	// и handleReloadModel — scenario «два клиента одновременно грузят одну модель».
	lockOk, lockErr := backend.TryLockLoad(modelName)
	if lockErr != nil {
		// Модель уже загружена — проверяем, совпадают ли параметры.
		if info, getErr := backend.GetModel(modelName); getErr == nil {
			if info.Path == modelPath && sameLoadOptions(*info, opts) {
				logger.Get().Infow("model already loaded with same parameters", "name", modelName)
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status": "already_loaded",
					"model":  info,
				})
				return
			}
			// Параметры отличаются — выгружаем и перезагружаем (reload-in-place).
			logger.Get().Infow("reloading model because parameters changed", "name", modelName,
				"oldPath", info.Path, "newPath", modelPath)
			if unloadErr := backend.UnloadModel(modelName); unloadErr != nil {
				logger.Get().Errorw("failed to unload model before reload", "name", modelName, "error", unloadErr)
				writeError(w, http.StatusInternalServerError, "unload before reload failed: "+unloadErr.Error())
				return
			}
			// Пере-захватываем блокировку после выгрузки для перезагрузки.
			lockOk, lockErr = backend.TryLockLoad(modelName)
			if lockErr != nil {
				logger.Get().Errorw("race: model appeared after unload", "name", modelName)
				writeError(w, http.StatusInternalServerError, "concurrent load race after unload")
				return
			}
		} else {
			// Модель числится загруженной по TryLockLoad, но GetModel не находит —
			// редкая гонка. Продолжаем как новую загрузку.
			lockOk = true
		}
	}

	if !lockOk {
		// Другая горутина уже грузит эту модель. Ждём завершения и отвечаем
		// 503 с loading-статусом (клиент может polling'ом проверить /api/models/load/progress).
		logger.Get().Infow("model is already being loaded by another request; waiting",
			"name", modelName)
		writeLoadingResponse(w, modelName, errModelIsLoading)
		return
	}

	loadStart := time.Now()
	loadErr := backend.LoadModelWithOpts(modelName, modelPath, opts)
	backend.UnlockLoad(modelName) // освобождаем блокировку — другие горутины могут грузить ту же модель
	if loadErr != nil {
		logger.Get().Errorw("failed to load model", "name", modelName, "error", loadErr)
		writeError(w, http.StatusInternalServerError, "load failed: "+loadErr.Error())
		return
	}
	loadDuration := time.Since(loadStart)

	model, err := backend.GetModel(modelName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Уведомляем балансировщик (если auto-registration активен), что модель
	// загружена. Это позволяет балансировщику обновить кэш loadedModels
	// немедленно, не дожидаясь 30-секундного /api/models poll.
	if balancerReg != nil {
		balancerReg.notifyModelLoaded(modelName, model.SizeBytes, model.ContextSize, model.GPULayers)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "loaded",
		"model":          model,
		"loadDurationMs": loadDuration.Milliseconds(),
	})
}

// handleLoadProgress — GET /api/models/load/progress?model=<name>
// Возвращает JSON со статусом загрузки указанной модели (или всех loading-моделей,
// если параметр не передан). Используется UI для polling после 503 loading-ответа.
//
// Ответы:
//   - 200 + {model, state, loadingStartedAt, loadingSizeBytes, elapsedMs}
//   - 200 + {models: [...]} если model=* или не задан
//   - 404 если указанная модель не в loading
func handleLoadProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("model"))
	if name == "" || name == "*" {
		// Вернуть все loading-модели.
		all := backend.GetLoadingModels()
		out := make([]map[string]interface{}, 0, len(all))
		now := time.Now()
		for _, m := range all {
			elapsed := int64(0)
			if !m.LoadingStartedAt.IsZero() {
				elapsed = now.Sub(m.LoadingStartedAt).Milliseconds()
			}
			out = append(out, map[string]interface{}{
				"name":             m.Name,
				"state":            m.State,
				"loadingStartedAt": m.LoadingStartedAt.UTC().Format(time.RFC3339Nano),
				"loadingSizeBytes": m.LoadingSizeBytes,
				"elapsedMs":        elapsed,
				"error":            m.LoadingError,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"models": out,
			"count":  len(out),
		})
		return
	}

	m, err := backend.GetModel(name)
	if err != nil {
		// Возможно, она в loading — попробуем достать из GetLoadingModels().
		for _, lm := range backend.GetLoadingModels() {
			if lm.Name == name {
				elapsed := int64(0)
				if !lm.LoadingStartedAt.IsZero() {
					elapsed = time.Since(lm.LoadingStartedAt).Milliseconds()
				}
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"name":             lm.Name,
					"state":            lm.State,
					"loadingStartedAt": lm.LoadingStartedAt.UTC().Format(time.RFC3339Nano),
					"loadingSizeBytes": lm.LoadingSizeBytes,
					"elapsedMs":        elapsed,
					"error":            lm.LoadingError,
				})
				return
			}
		}
		writeError(w, http.StatusNotFound, "model not found and not loading: "+name)
		return
	}

	elapsed := int64(0)
	if !m.LoadedAt.IsZero() {
		elapsed = time.Since(m.LoadedAt).Milliseconds()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":        m.Name,
		"state":       m.State,
		"loadedAt":    m.LoadedAt.UTC().Format(time.RFC3339Nano),
		"elapsedMs":   elapsed,
		"sizeBytes":   m.SizeBytes,
		"contextSize": m.ContextSize,
	})
}

func handleUnloadModel(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}
	logger.Get().Infow("unloading model", "name", name)
	if err := backend.UnloadModel(name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "unloaded",
		"name":   name,
	})
}

func handleListModels(w http.ResponseWriter, r *http.Request) {
	models := backend.ListModels()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"models": models,
		"count":  len(models),
	})
}

func handleGetModel(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}
	model, err := backend.GetModel(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, model)
}

// ============================================================
// Inference handlers
// ============================================================

// generateOptions — Ollama-style nested options (options.*).
// Полный набор опций, маппится на bridge.GenerationParams.
type generateOptions struct {
	Temperature    float64     `json:"temperature"`
	TopP           float64     `json:"top_p"`
	TopK           int         `json:"top_k"`
	MinP           float64     `json:"min_p"`
	TypicalP       float64     `json:"typical_p"`
	TfsZ           float64     `json:"tfs_z"`
	NumPredict     int         `json:"num_predict"`
	NumKeep        int         `json:"num_keep"`
	RepeatPenalty  float64     `json:"repeat_penalty"`
	FrequencyPenalty float64  `json:"frequency_penalty"`
	PresencePenalty float64   `json:"presence_penalty"`
	RepeatLastN    int         `json:"repeat_last_n"`
	Mirostat       int         `json:"mirostat"`
	MirostatTau    float64     `json:"mirostat_tau"`
	MirostatEta    float64     `json:"mirostat_eta"`
	Seed           int         `json:"seed"`
	NumCtx         int         `json:"num_ctx"`
	Stop           interface{} `json:"stop"` // string или []string
}

type generateRequest struct {
	Model            string          `json:"model"`
	Prompt           string          `json:"prompt"`
	System           string          `json:"system,omitempty"`
	Template         string          `json:"template,omitempty"`
	Raw              bool            `json:"raw"`
	Format           string          `json:"format,omitempty"`
	KeepAlive        string          `json:"keep_alive,omitempty"`
	Context          []int           `json:"context,omitempty"`
	Images           []string        `json:"images,omitempty"` // not supported yet, accepted for compatibility
	Options          generateOptions `json:"options"`
	Temperature      float64         `json:"temperature,omitempty"`
	TopP             float64         `json:"topP,omitempty"`
	TopK             int             `json:"topK,omitempty"`
	MinP             float64         `json:"minP,omitempty"`
	TypicalP         float64         `json:"typicalP,omitempty"`
	TfsZ             float64         `json:"tfsZ,omitempty"`
	MaxTokens        int             `json:"maxTokens,omitempty"`
	RepeatPenalty    float64         `json:"repeatPenalty,omitempty"`
	FrequencyPenalty float64         `json:"frequencyPenalty,omitempty"`
	PresencePenalty  float64         `json:"presencePenalty,omitempty"`
	Seed             int             `json:"seed,omitempty"`
	// NumCtx — per-request переопределение n_ctx (effective context size).
	// 0 = использовать n_ctx, с которым модель фактически загружена в VRAM.
	// > 0 и <= effective n_ctx → ёмкость считается от NumCtx (C-bridge).
	// > effective n_ctx → C-bridge вернёт informative ошибку с предложением
	// перезагрузить модель через /api/models/reload с большим n_ctx.
	NumCtx int  `json:"numCtx,omitempty"`
	Stream bool `json:"stream"`
}

type generateResponse struct {
	Model            string  `json:"model"`
	Response         string  `json:"response"`
	Done             bool    `json:"done"`
	Tokens           int     `json:"tokens"`
	DurationMs       int64   `json:"durationMs"`
	TokensPerSec     float64 `json:"tokensPerSec,omitempty"`
	TotalDuration    int64   `json:"total_duration,omitempty"`
	LoadDuration     int64   `json:"load_duration,omitempty"`
	PromptEvalCount  int     `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64 `json:"prompt_eval_duration,omitempty"`
	EvalCount        int     `json:"eval_count,omitempty"`
	EvalDuration     int64   `json:"eval_duration,omitempty"`
}

type streamChunk struct {
	Model    string `json:"model"`
	Token    string `json:"token"`
	Response string `json:"response,omitempty"`
	Done     bool   `json:"done"`
}

// normalizeGenerateRequest приводит Ollama-формат (options.*, system, raw)
// к единому виду, понятному buildGenerationParams.
func normalizeGenerateRequest(req *generateRequest) {
	// Ollama nested options have priority when top-level fields are unset.
	if req.Options.Temperature > 0 && req.Temperature == 0 {
		req.Temperature = req.Options.Temperature
	}
	if req.Options.TopP > 0 && req.TopP == 0 {
		req.TopP = req.Options.TopP
	}
	if req.Options.TopK > 0 && req.TopK == 0 {
		req.TopK = req.Options.TopK
	}
	if req.Options.MinP > 0 && req.MinP == 0 {
		req.MinP = req.Options.MinP
	}
	if req.Options.TypicalP > 0 && req.TypicalP == 0 {
		req.TypicalP = req.Options.TypicalP
	}
	if req.Options.TfsZ > 0 && req.TfsZ == 0 {
		req.TfsZ = req.Options.TfsZ
	}
	if req.Options.NumPredict > 0 && req.MaxTokens == 0 {
		req.MaxTokens = req.Options.NumPredict
	}
	if req.Options.NumKeep > 0 && req.NumCtx == 0 {
		// num_keep не используется напрямую, передаётся через NKeep
	}
	if req.Options.RepeatPenalty > 0 && req.RepeatPenalty == 0 {
		req.RepeatPenalty = req.Options.RepeatPenalty
	}
	if req.Options.FrequencyPenalty != 0 && req.FrequencyPenalty == 0 {
		req.FrequencyPenalty = req.Options.FrequencyPenalty
	}
	if req.Options.PresencePenalty != 0 && req.PresencePenalty == 0 {
		req.PresencePenalty = req.Options.PresencePenalty
	}
	if req.Options.RepeatLastN > 0 {
		// передаётся в params напрямую
	}
	if req.Options.Mirostat > 0 {
		// передаётся в params напрямую
	}
	if req.Options.Seed != 0 && req.Seed == 0 {
		req.Seed = req.Options.Seed
	}
	if req.Options.NumCtx > 0 && req.NumCtx == 0 {
		req.NumCtx = req.Options.NumCtx
	}

	// Ollama /api/generate passes system prompt separately; cppworker has no
	// Modelfile template engine, so prepend it to the raw prompt unless the
	// caller asked for raw completion.
	if !req.Raw && req.System != "" {
		req.Prompt = req.System + "\n" + req.Prompt
	}

	// Если keep_alive задан в формате "5m" — игнорируем, у нас нет unload-таймера.
	// Просто принимаем параметр для совместимости.
	_ = req.KeepAlive
}

func buildGenerationParams(req generateRequest) bridge.GenerationParams {
	params := bridge.DefaultGenerationParams()
	if req.Temperature > 0 {
		params.Temperature = float32(req.Temperature)
	}
	if req.TopP > 0 {
		params.TopP = float32(req.TopP)
	}
	if req.TopK > 0 {
		params.TopK = float32(req.TopK)
	}
	if req.MinP > 0 {
		params.MinP = float32(req.MinP)
	}
	if req.TypicalP > 0 {
		params.TypicalP = float32(req.TypicalP)
	}
	if req.TfsZ > 0 {
		params.TfsZ = float32(req.TfsZ)
	}
	if req.MaxTokens > 0 {
		params.NPredict = req.MaxTokens
	}
	if req.RepeatPenalty > 0 {
		params.RepeatPenalty = float32(req.RepeatPenalty)
	}
	if req.FrequencyPenalty != 0 {
		params.FrequencyPenalty = float32(req.FrequencyPenalty)
	}
	if req.PresencePenalty != 0 {
		params.PresencePenalty = float32(req.PresencePenalty)
	}
	if req.Options.RepeatLastN > 0 {
		params.RepeatLastN = req.Options.RepeatLastN
	}
	if req.Options.Mirostat > 0 {
		params.Mirostat = req.Options.Mirostat
	}
	if req.Options.MirostatTau > 0 {
		params.MirostatTau = float32(req.Options.MirostatTau)
	}
	if req.Options.MirostatEta > 0 {
		params.MirostatEta = float32(req.Options.MirostatEta)
	}
	if req.Seed != 0 || req.Options.Seed != 0 {
		params.Seed = req.Seed
		if params.Seed == 0 {
			params.Seed = req.Options.Seed
		}
	}
	// Per-request n_ctx override (Шаг 1.5). 0 = использовать n_ctx модели.
	// Header X-Cpp-Ctx применяется ПОСЛЕ этого и только если здесь остался 0.
	if req.NumCtx > 0 {
		params.NCtxOverride = req.NumCtx
	}
	// Stop-последовательности: Ollama options.stop — string или []string.
	params.Antiprompts = append(params.Antiprompts, parseStopSequences(req.Options.Stop)...)
	params.StopSequences = params.Antiprompts
	return params
}

// parseStopSequences нормализует Ollama options.stop (string, []string или []interface{})
// в плоский []string для Antiprompts.
func parseStopSequences(stop interface{}) []string {
	if stop == nil {
		return nil
	}
	switch v := stop.(type) {
	case string:
		if v != "" {
			return []string{v}
		}
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// runGenerateCore — общая часть handleGenerate/handleOllamaGenerate:
// валидация, нормализация Ollama-формата, lazy-load, построение params.
// Возвращает (params, prompt, ok).
func runGenerateCore(w http.ResponseWriter, r *http.Request, req generateRequest) (bridge.GenerationParams, string, bool) {
	normalizeGenerateRequest(&req)

	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return bridge.GenerationParams{}, "", false
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return bridge.GenerationParams{}, "", false
	}

	// Ленивая загрузка модели если она ещё не в памяти
	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return bridge.GenerationParams{}, "", false
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return bridge.GenerationParams{}, "", false
	}

	params := buildGenerationParams(req)
	// Политика балансировщика (X-Cpp-Ctx header) — только если body не задал num_ctx.
	applyCppCtxHeader(r, &params)
	return params, req.Prompt, true
}

// isEmptyInferenceResult — true, если бэкенд вернул успех, но без текста.
// Пустой ответ на /api/generate выглядит как `{response:""}`, что клиенты
// (Ollama-js, OpenWebUI) интерпретируют как успешный пустой ответ.
// Вместо этого возвращаем ошибку.
func isEmptyInferenceResult(result *bridge.InferenceResult) bool {
	return result != nil && result.Status == 0 && strings.TrimSpace(result.Output) == ""
}

func handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req generateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	params, prompt, ok := runGenerateCore(w, r, req)
	if !ok {
		return
	}
	modelName := req.Model

	if req.Stream {
		writeStreamResponse(w, r, modelName, prompt, params)
		return
	}
	// cppworker side Stage 3.2.a: прокидываем структурированную информацию
	// об ошибке в JSON-теле. balancer различает типы ошибок через
	// bridge.ErrNCtxNeedsReload / ErrPromptTooLong; HTTP-статус 400
	// соответствует BRIDGE_ERR_N_CTX_NEEDS_RELOAD и BRIDGE_ERR_PROMPT_TOO_LONG,
	// 500 — для generic.
	start := time.Now()
	result, err := generateWithRamFallback(modelName, prompt, params)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "generate failed", err)
		return
	}
	if isEmptyInferenceResult(result) {
		logger.Get().Warnw("generate returned empty output", "model", modelName)
		writeError(w, http.StatusInternalServerError, "model produced an empty response")
		return
	}
	duration := time.Since(start)
	tokens := countTokens(modelName, result.Output)
	resp := generateResponse{
		Model:      modelName,
		Response:   result.Output,
		Done:       true,
		Tokens:     tokens,
		DurationMs: duration.Milliseconds(),
	}
	if duration.Seconds() > 0 {
		resp.TokensPerSec = float64(tokens) / duration.Seconds()
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeStreamResponse — streaming ответ в формате NDJSON (application/x-ndjson),
// как реальная Ollama для /api/generate и /api/chat.
// Каждая строка — валидный JSON объект с полями model, response, done.
func writeStreamResponse(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Немедленный flush заголовков: гарантирует, что клиент получит заголовки
	// ДО того, как произойдёт ошибка инференса. Без этого flusher-а, если
	// generateStreamWithRamFallback мгновенно вернёт ошибку (например,
	// "prompt too long"), клиент (OpenWebUI reasoning) получит пустое тело
	// и упадёт с json.JSONDecodeError.
	flusher.Flush()
	ctx := r.Context()
	tokens := 0
	start := time.Now()
	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		chunk := map[string]interface{}{
			"model":    modelName,
			"response": token,
			"done":     false,
		}
		jsonData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", jsonData)
		flusher.Flush()
		tokens++
		return true
	}
	if err := generateStreamWithRamFallback(modelName, prompt, params, callback); err != nil {
		// ВАЖНО: при memory slot leak контекст модели в llama.cpp не освобождается
		// и все последующие decode будут падать. Перезапускаем процесс — docker-compose
		// поднимет контейнер заново с чистым VRAM (это безопаснее, чем пытаться
		// очистить KV-кэш вручную в bridge.c).
		maybeRestartOnMemorySlotError(err, modelName)
		errChunk := map[string]interface{}{
			"model": modelName,
			"done":  true,
			"error": err.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}
	duration := time.Since(start)
	tps := float64(0)
	if duration.Seconds() > 0 {
		tps = float64(tokens) / duration.Seconds()
	}
	// Финальный done-чанк с полной статистикой (как в Ollama)
	doneChunk := map[string]interface{}{
		"model":             modelName,
		"done":              true,
		"total_duration":    duration.Microseconds() * 1000,
		"eval_count":        tokens,
		"eval_duration":     duration.Microseconds() * 1000,
		"tokens_per_second": tps,
	}
	doneJSON, _ := json.Marshal(doneChunk)
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}

// ============================================================
// Embeddings handler
// ============================================================

type embeddingsRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

func handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req embeddingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" || req.Input == "" {
		writeError(w, http.StatusBadRequest, "model and input are required")
		return
	}
	embeddings, err := backend.GetEmbeddings(req.Model, req.Input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "embeddings failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":      req.Model,
		"embeddings": embeddings,
	})
}

// ============================================================
// Ollama-compatible API handlers
// ============================================================

func handleOllamaGenerate(w http.ResponseWriter, r *http.Request) {
	var req generateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	params, prompt, ok := runGenerateCore(w, r, req)
	if !ok {
		return
	}
	modelName := req.Model

	if req.Stream {
		writeOllamaStream(w, r, modelName, prompt, params)
		return
	}
	start := time.Now()
	result, err := generateWithRamFallback(modelName, prompt, params)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "generate failed", err)
		return
	}
	if isEmptyInferenceResult(result) {
		logger.Get().Warnw("ollama generate returned empty output", "model", modelName)
		writeError(w, http.StatusInternalServerError, "model produced an empty response")
		return
	}
	promptEvalCount := countTokens(modelName, prompt)
	loadDuration := int64(0)
	if info, err := backend.GetModel(modelName); err == nil && !info.LoadedAt.IsZero() {
		loadDuration = time.Since(info.LoadedAt).Milliseconds()
	}
	duration := time.Since(start)
	tokens := countTokens(modelName, result.Output)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":                modelName,
		"response":             result.Output,
		"done":                 true,
		"context":              []int{},
		"total_duration":       duration.Microseconds() * 1000,
		"load_duration":        loadDuration * 1000,
		"prompt_eval_count":    promptEvalCount,
		"prompt_eval_duration": duration.Microseconds() * 1000,
		"eval_count":           tokens,
		"eval_duration":        duration.Microseconds() * 1000,
	})
}

func writeOllamaStream(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	start := time.Now()
	callback := func(token string) bool {
		select {
		case <-r.Context().Done():
			return false
		default:
		}
		chunk := map[string]interface{}{"model": modelName, "response": token, "done": false}
		jsonData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", jsonData)
		flusher.Flush()
		return true
	}
	if err := generateStreamWithRamFallback(modelName, prompt, params, callback); err != nil {
		maybeRestartOnMemorySlotError(err, modelName)
		errJSON, _ := json.Marshal(map[string]interface{}{"model": modelName, "done": true, "error": err.Error()})
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}
	duration := time.Since(start)
	doneJSON, _ := json.Marshal(map[string]interface{}{
		"model": modelName, "done": true,
		"total_duration": duration.Microseconds() * 1000,
		"eval_count":     0, "eval_duration": duration.Microseconds() * 1000,
	})
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}

func handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	// Используем map для дедупликации (загруженные модели имеют приоритет)
	modelMap := make(map[string]map[string]interface{})

	// 1. Сканируем файловую систему — все .gguf файлы (как Ollama)
	mm := backend.ModelManager()
	if mm != nil {
		fsModels := mm.ListModels()
		for _, fm := range fsModels {
			// Извлекаем имя модели из имени файла (без .gguf и пути)
			modelName := fm.Filename
			if strings.HasSuffix(strings.ToLower(modelName), ".gguf") {
				modelName = modelName[:len(modelName)-5] // убираем .gguf
			}
			digest := fmt.Sprintf("sha256:%x", fm.SizeBytes)
			modelMap[modelName] = map[string]interface{}{
				"name":        modelName,
				"model":       modelName, // обязательно для OpenWebUI
				"modified_at": fm.ModifiedAt.Format(time.RFC3339),
				"size":        fm.SizeBytes,
				"digest":      digest,
				"details": map[string]interface{}{
					"format":             "gguf",
					"family":             fm.Architecture,
					"parameter_size":     "unknown",
					"quantization_level": fm.FileType,
				},
			}
		}
	}

	// 2. Добавляем/перезаписываем загруженные модели (имеют более точные метаданные)
	loadedModels := backend.ListModels()
	for _, m := range loadedModels {
		sizeBytes := m.SizeBytes
		if sizeBytes == 0 {
			// Fallback: используем размер из файловой системы
			if fsEntry, ok := modelMap[m.Name]; ok {
				if fsSize, ok2 := fsEntry["size"].(int64); ok2 && fsSize > 0 {
					sizeBytes = uint64(fsSize)
				}
			}
		}
		digest := fmt.Sprintf("sha256:%x", sizeBytes)
		modelMap[m.Name] = map[string]interface{}{
			"name":        m.Name,
			"model":       m.Name, // обязательно для OpenWebUI
			"modified_at": m.LoadedAt.Format(time.RFC3339),
			"size":        sizeBytes,
			"digest":      digest,
			"details": map[string]interface{}{
				"format":             "gguf",
				"family":             m.Architecture,
				"parameter_size":     fmt.Sprintf("%.1fB", float64(m.NLayers*m.NEmbd)/1e9),
				"quantization_level": "unknown",
			},
		}
	}

	// 3. Собираем результирующий список
	ollamaModels := make([]map[string]interface{}, 0, len(modelMap))
	for _, v := range modelMap {
		ollamaModels = append(ollamaModels, v)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"models": ollamaModels})
}

// ============================================================
// Ollama-compatible model info & management handlers
// ============================================================

// handleOllamaShow — Ollama /api/show: возвращает информацию о модели.
// Если модель загружена — возвращаем богатые метаданные из backend.GetModel.
// Иначе ищем файл в modelsDir и возвращаем базовые метаданные (как в /api/tags).
func handleOllamaShow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	// Ленивая загрузка модели, если ещё не загружена, чтобы /api/show
	// работал напрямую без предварительного /api/models/load.
	if err := ensureModelLoaded(name); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, name, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	// Сначала пытаемся получить загруженную модель
	if info, err := backend.GetModel(name); err == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"license":     "unknown",
			"modelfile":   "",
			"parameters":  fmt.Sprintf("%.1fB", float64(info.NLayers*info.NEmbd)/1e9),
			"template":    "",
			"system":      "",
			"details": map[string]interface{}{
				"parent_model":       "",
				"format":             "gguf",
				"family":             info.Architecture,
				"families":           []string{info.Architecture},
				"parameter_size":     fmt.Sprintf("%.1fB", float64(info.NLayers*info.NEmbd)/1e9),
				"quantization_level": "unknown",
			},
			"model_info": map[string]interface{}{
				"architecture": info.Architecture,
				"n_layers":     info.NLayers,
				"n_heads":      info.NHeads,
				"n_embd":       info.NEmbd,
				"n_vocab":      info.NVocab,
				"context_size": info.ContextSize,
				"gpu_layers":   info.GPULayers,
				"state":        info.State,
			},
		})
		return
	}

	// Модель не загружена — ищем файл и возвращаем базовые метаданные
	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager not available")
		return
	}
	filename := name
	if !strings.HasSuffix(strings.ToLower(filename), ".gguf") {
		filename = filename + ".gguf"
	}
	path, err := mm.FindModelByPath(filename)
	if err != nil {
		writeError(w, http.StatusNotFound, "model not found: "+err.Error())
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to stat model file: "+err.Error())
		return
	}
	modelName := strings.TrimSuffix(filepath.Base(path), ".gguf")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"license":     "unknown",
		"modelfile":   "",
		"parameters":  "unknown",
		"template":    "",
		"system":      "",
		"details": map[string]interface{}{
			"parent_model":       "",
			"format":             "gguf",
			"family":             "unknown",
			"families":           []string{},
			"parameter_size":     "unknown",
			"quantization_level": "unknown",
		},
		"model_info": map[string]interface{}{
			"name":         modelName,
			"filename":     filepath.Base(path),
			"size_bytes":   info.Size(),
			"modified_at":  info.ModTime().Format(time.RFC3339),
			"state":        "not loaded",
			"architecture": "unknown",
		},
	})
}

// handleOllamaCopy — Ollama /api/copy: копирует .gguf файл в modelsDir.
func handleOllamaCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	// Rescan models
	if _, scanErr := mm.ScanModels(); scanErr != nil {
		logger.Get().Warnw("copy: model rescan failed", "error", scanErr)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "copied",
		"source":      req.Source,
		"destination": req.Destination,
	})
}

// handleOllamaCreate — Ollama /api/create: для llama.cpp backend создаёт
// alias-файл <name>.gguf.json рядом с исходной моделью, хранящий параметры.
// Если from-файл не найден — возвращает 400.
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	// Пытаемся определить исходную модель: из FROM или из первой строки Modelfile
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
	_ = modelName // used only in alias payload
	dir := filepath.Dir(srcPath)
	aliasPath := filepath.Join(dir, modelName+".gguf.json")

	alias := map[string]interface{}{
		"name":       modelName,
		"source":     sourceName,
		"created_at": time.Now().Format(time.RFC3339),
		"modelfile":  req.ModelFile,
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

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "created",
		"name":   modelName,
	})
}

// handleOllamaPush — Ollama /api/push: для llama.cpp backend не поддерживается,
// т.к. нет Ollama-реестра. Возвращаем 501 с понятным сообщением.
func handleOllamaPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	writeError(w, http.StatusNotImplemented, "Ollama registry push is not supported for llama.cpp backends")
}

// ============================================================
// File system & Utility handlers
// ============================================================

func handleListModelsDir(w http.ResponseWriter, r *http.Request) {

	mm := backend.ModelManager()
	if mm != nil {
		models := mm.ListModels()
		files := make([]map[string]interface{}, 0, len(models))
		for _, m := range models {
			files = append(files, map[string]interface{}{
				"name":       m.Filename,
				"sizeBytes":  m.SizeBytes,
				"modifiedAt": m.ModifiedAt.Format(time.RFC3339),
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"files": files, "dir": mm.GetModelsDir(), "count": len(files),
		})
		return
	}
	var files []map[string]interface{}
	entries, err := os.ReadDir(*modelsDir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"files": []interface{}{}, "dir": *modelsDir,
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
		files = append(files, map[string]interface{}{
			"name":       name,
			"sizeBytes":  info.Size(),
			"modifiedAt": info.ModTime().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"files": files, "dir": *modelsDir, "count": len(files),
	})
}

// handleDeleteModel — удаление .gguf файла с диска (DELETE /api/models/delete и /api/delete).
// Принимает JSON {"name": "<file.gguf>"} либо query param ?name=<file.gguf>.
// Если модель сейчас загружена в памяти — сначала выгружает её, потом удаляет файл.
func handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "use POST or DELETE")
		return
	}

	// Поддерживаем оба формата передачи имени:
	// 1. JSON body {"name": "..."} (Ollama-style /api/delete)
	// 2. Query param ?name=... (альтернативный вариант)
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			name = strings.TrimSpace(req.Name)
		}
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required (JSON body or ?name= query param)")
		return
	}

	// Поддерживаем как "model.gguf", так и просто "model" (без расширения)
	// Если расширения нет — добавляем .gguf
	filename := name
	if !strings.HasSuffix(strings.ToLower(filename), ".gguf") {
		filename = filename + ".gguf"
	}

	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager not available")
		return
	}

	// Получаем полный путь к файлу
	modelPath, err := mm.FindModelByPath(filename)
	if err != nil {
		writeError(w, http.StatusNotFound, "model not found: "+err.Error())
		return
	}

	// 1. Если модель сейчас загружена в память — сначала выгружаем её
	modelName := strings.TrimSuffix(filepath.Base(modelPath), ".gguf")
	if _, getErr := backend.GetModel(modelName); getErr == nil {
		logger.Get().Infow("delete: unloading model from memory before file removal", "name", modelName)
		if unloadErr := backend.UnloadModel(modelName); unloadErr != nil {
			logger.Get().Warnw("delete: unload before delete failed", "name", modelName, "error", unloadErr)
		}
	}

	// 2. Удаляем файл
	logger.Get().Infow("deleting model file", "name", modelName, "path", modelPath)
	if err := os.Remove(modelPath); err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "model file does not exist: "+modelPath)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete model: "+err.Error())
		return
	}

	// 3. Re-scan моделей чтобы обновить кэш
	if _, scanErr := mm.ScanModels(); scanErr != nil {
		logger.Get().Warnw("delete: model rescan failed", "error", scanErr)
	}

	logger.Get().Infow("model deleted", "name", modelName, "path", modelPath)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "deleted",
		"name":     modelName,
		"filename": filepath.Base(modelPath),
	})
}

// ============================================================
// HuggingFace Handlers
// ============================================================

func handleHFSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, "query parameter is required")
		return
	}
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := parseInt(l, 20); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	// Read HF token from X-HF-Token header (set by GgufApi in WebUI)
	hfToken := r.Header.Get("X-HF-Token")
	if hfToken == "" {
		hfToken = r.Header.Get("Authorization")
		if strings.HasPrefix(hfToken, "Bearer ") {
			hfToken = strings.TrimPrefix(hfToken, "Bearer ")
		}
	}
	// Увеличенный таймаут: SearchModels теперь параллельно подгружает файлы
	// для каждого результата (4 параллельных запроса × 10s = до 30-60s в худшем случае).
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if hfToken != "" {
		backend.HFDownloader().SetToken(hfToken)
	}
	results, err := backend.HFDownloader().SearchModels(ctx, query, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}
	if results == nil {
		results = []cppbackend.HFModelRepo{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results, "count": len(results), "query": query,
	})
}

func handleHFFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	modelID := r.URL.Query().Get("modelId")
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "modelId parameter is required")
		return
	}
	revision := r.URL.Query().Get("revision")
	if revision == "" {
		revision = "main"
	}
	hfToken := r.Header.Get("X-HF-Token")
	if hfToken == "" {
		hfToken = r.Header.Get("Authorization")
		if strings.HasPrefix(hfToken, "Bearer ") {
			hfToken = strings.TrimPrefix(hfToken, "Bearer ")
		}
	}
	if hfToken != "" {
		backend.HFDownloader().SetToken(hfToken)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	files, err := backend.HFDownloader().ListModelFiles(ctx, modelID, revision)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list files failed: "+err.Error())
		return
	}
	if files == nil {
		files = []cppbackend.HFFileInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"files": files, "count": len(files), "modelId": modelID, "revision": revision,
	})
}

func handleHFDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req cppbackend.HFDownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ModelID == "" {
		writeError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	hfToken := r.Header.Get("X-HF-Token")
	if hfToken == "" {
		hfToken = r.Header.Get("Authorization")
		if strings.HasPrefix(hfToken, "Bearer ") {
			hfToken = strings.TrimPrefix(hfToken, "Bearer ")
		}
	}
	if hfToken != "" {
		backend.HFDownloader().SetToken(hfToken)
	}
	progress, err := backend.HFDownloader().StartDownload(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "download failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status": "started", "progress": progress,
	})
}

func handleHFDownloadProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	modelID := r.URL.Query().Get("modelId")
	filename := r.URL.Query().Get("filename")
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	progress, err := backend.HFDownloader().GetDownloadProgress(modelID, filename)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func handleHFDownloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	active := backend.HFDownloader().ListActiveDownloads()
	history := backend.HFDownloader().ListDownloadHistory()
	if active == nil {
		active = []cppbackend.HFDownloadProgress{}
	}
	if history == nil {
		history = []cppbackend.HFDownloadProgress{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"active": active, "history": history})
}

func handleHFCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		ModelID  string `json:"modelId"`
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ModelID == "" {
		writeError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	if err := backend.HFDownloader().CancelDownload(req.ModelID, req.Filename); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// handlePull — Ollama-compatible /api/pull для llama.cpp бэкендов.
// Поддерживает загрузку GGUF-моделей с HuggingFace по имени вида:
//   hf:<repo>/<filename.gguf>  или  <repo>/<filename.gguf>
// Ollama-реестр (имена без '/') не поддерживается — возвращается 501.
func handlePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req struct {
		Name   string `json:"name"`
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	hf := backend.HFDownloader()
	if hf == nil {
		writeError(w, http.StatusServiceUnavailable, "HF downloader not available")
		return
	}

	ref := strings.TrimPrefix(name, "hf:")
	if !strings.Contains(ref, "/") {
		writeError(w, http.StatusNotImplemented, "Ollama registry pull is not supported for llama.cpp backends; use hf:<repo>/<filename.gguf> or the /api/hf/download endpoint")
		return
	}

	parts := strings.SplitN(ref, "/", 2)
	modelID := parts[0]
	filename := parts[1]
	if filename == "" {
		writeError(w, http.StatusBadRequest, "filename is required (expected repo/filename.gguf)")
		return
	}

	// Если файл уже скачан — сразу возвращаем успех.
	if localPath, err := hf.GetLocalPath(ref); err == nil && localPath != "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":    "present",
			"name":      name,
			"modelId":   modelID,
			"filename":  filename,
			"localPath": localPath,
		})
		return
	}

	dlReq := cppbackend.HFDownloadRequest{
		ModelID:  modelID,
		Filename: filename,
		Revision: "main",
	}
	progress, err := hf.StartDownload(dlReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pull failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":   "pulling",
		"name":     name,
		"modelId":  modelID,
		"filename": filename,
		"progress": progress,
	})
}

// ============================================================
// CppWorker Version handler (Ollama-compatible)
// ============================================================

func handleCppWorkerVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": backend.Version(),
	})
}

// ============================================================
// OpenAI-compatible /v1/ handlers
// ============================================================

// openAICompletionRequest — структура запроса OpenAI /v1/completions
type openAICompletionRequest struct {
	Model            string   `json:"model"`
	Prompt           string   `json:"prompt"`
	Suffix           string   `json:"suffix,omitempty"`
	MaxTokens        int      `json:"max_tokens,omitempty"`
	Temperature      float64  `json:"temperature,omitempty"`
	TopP             float64  `json:"top_p,omitempty"`
	N                int      `json:"n,omitempty"`
	Stream           bool     `json:"stream,omitempty"`
	Echo             bool     `json:"echo,omitempty"`
	Stop             []string `json:"stop,omitempty"`
	PresencePenalty  float64  `json:"presence_penalty,omitempty"`
	FrequencyPenalty float64  `json:"frequency_penalty,omitempty"`
	Seed             int      `json:"seed,omitempty"`
	// NumCtx — per-request переопределение n_ctx (OpenAI-совместимый).
	// 0 = использовать n_ctx модели. > 0 → C-bridge pre-flight check.
	// > effective n_ctx → C-bridge вернёт informative ошибку с предложением
	// перезагрузить модель через /api/models/reload.
	NumCtx int `json:"num_ctx,omitempty"`
}

// openAIChatCompletionRequest — структура запроса OpenAI /v1/chat/completions
type openAIChatCompletionRequest struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
	TopP        float64             `json:"top_p,omitempty"`
	N           int                 `json:"n,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
	Stop        []string            `json:"stop,omitempty"`
	Seed        int                 `json:"seed,omitempty"`
	// NumCtx — per-request переопределение n_ctx (OpenAI-совместимый).
	// 0 = использовать n_ctx модели. > 0 → C-bridge pre-flight check.
	NumCtx int `json:"num_ctx,omitempty"`
}

// applyCppCtxHeader — применяет X-Cpp-Ctx header (от балансировщика) к params.
//
// Семантика (Phase D.3-fix): header — это UPPER LIMIT (потолок), заданный
// профилем модели или defaultProfile. Если body задал num_ctx > header,
// params.NCtxOverride клампится к header (защита от ситуации, когда клиент
// вроде OpenWebUI шлёт options.num_ctx=16384, а VRAM бэкенда не позволяет
// больше 4096 — без clamp'а cppworker вернёт
// "requested n_ctx=16384 exceeds model's effective n_ctx=4096").
//
// Если body задал num_ctx <= header или не задал вовсе — header не меняет
// body num_ctx (т.е. клиент может запросить МЕНЬШЕ, чем потолок — это
// допустимо). Политика балансировщика НЕ повышает num_ctx — только ограничивает.
// ВАЖНО: applyCppCtxHeader вынесена в nctx_clamp.go (ApplyCppCtxHeader, Phase refactoring-2026-06-09).
// Старая сигнатура сохранена как тонкая обёртка для backwards-compat со всеми handler-ами.

// applyCppCtxHeader — backwards-compat алиас для ApplyCppCtxHeader из nctx_clamp.go.
// TODO(refactoring): заменить все вызовы на ApplyCppCtxHeader и удалить этот alias.
func applyCppCtxHeader(r *http.Request, params *bridge.GenerationParams) {
	ApplyCppCtxHeader(r, params)
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// buildChatPrompt builds a prompt from chat messages using the GGUF
// chat template embedded in the model. If the model has no template
// (e.g. base/non-instruction model), it returns bridge.ErrNoChatTemplate
// and the caller should fall back to a naive prompt.
func buildChatPrompt(modelName string, msgs []openAIChatMessage) (string, error) {
	var chatMsgs []bridge.ChatMessage
	var systemContent string
	for _, m := range msgs {
		switch m.Role {
		case "system":
			systemContent = m.Content
		default:
			chatMsgs = append(chatMsgs, bridge.ChatMessage{Role: m.Role, Content: m.Content})
		}
	}
	if len(chatMsgs) == 0 {
		return "", fmt.Errorf("no user/assistant messages")
	}
	return backend.ApplyChatTemplate(modelName, systemContent, chatMsgs, true)
}

// defaultAntipromptsForModel и isGemmaModel вынесены в nctx_clamp.go (Phase refactoring-2026-06-09).
// buildNaiveChatPrompt — fallback when GGUF has no chat template (or bridge_apply_chat_template fails).
// Строит простой chat-формат, совместимый с gemma-style instruction-tuned моделями.
// Если в имени модели встречается "gemma" — используется формат <start_of_turn>user/model<end_of_turn>,
// иначе — формат <|user|>...<|assistant|>, оба совместимы с большинством chat-моделей llama.cpp.
func buildNaiveChatPrompt(msgs []openAIChatMessage, modelName string) string {
	isGemma := strings.Contains(strings.ToLower(modelName), "gemma")
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>system\n%s<end_of_turn>\n", m.Content)
			} else {
				fmt.Fprintf(&sb, "<|system|>\n%s<|end|>\n", m.Content)
			}
		case "user":
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>user\n%s<end_of_turn>\n", m.Content)
			} else {
				fmt.Fprintf(&sb, "<|user|>\n%s<|end|>\n", m.Content)
			}
		case "assistant":
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>model\n%s<end_of_turn>\n", m.Content)
			} else {
				fmt.Fprintf(&sb, "<|assistant|>\n%s<|end|>\n", m.Content)
			}
		}
	}
	if isGemma {
		sb.WriteString("<start_of_turn>model\n")
	} else {
		sb.WriteString("<|assistant|>\n")
	}
	return sb.String()
}

// handleV1ChatCompletions — OpenAI-совместимый /v1/chat/completions endpoint.
// Поддерживает как streaming (SSE: text/event-stream), так и non-streaming ответы.
func handleV1ChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req openAIChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages array is required")
		return
	}

	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	// Собираем промпт из сообщений с применением chat template из GGUF (для instruction-tuned моделей).
	// Если template не найден в GGUF — fallback на простую конкатенацию.
	// Также fallback на простую конкатенацию при любой ошибке bridge_apply_chat_template (например,
	// ret=-1 для архитектур, которые llama.cpp ещё не поддерживает в chat template бридже, но
	// нормально работают через нативный путь /api/generate).
	prompt, err := buildChatPrompt(req.Model, req.Messages)
	var usedNaive bool
	if err != nil {
		logger.Get().Warnw("chat template bridge failed, falling back to naive prompt",
			"model", req.Model, "error", err)
		prompt = buildNaiveChatPrompt(req.Messages, req.Model)
		if prompt == "" {
			writeError(w, http.StatusInternalServerError, "build prompt failed: "+err.Error())
			return
		}
		usedNaive = true
	}

	params := bridge.DefaultGenerationParams()
	if req.Temperature > 0 {
		params.Temperature = float32(req.Temperature)
	}
	if req.TopP > 0 {
		params.TopP = float32(req.TopP)
	}
	if req.MaxTokens > 0 {
		params.NPredict = req.MaxTokens
	}
	if req.Seed != 0 {
		params.Seed = req.Seed
	}
	// Per-request n_ctx override (Шаг 1.3). 0 = использовать n_ctx модели.
	// Header X-Cpp-Ctx применяется ПОСЛЕ через applyCppCtxHeader.
	if req.NumCtx > 0 {
		params.NCtxOverride = req.NumCtx
	}
	// Политика балансировщика (X-Cpp-Ctx header) — только если body не задал.
	applyCppCtxHeader(r, &params)

	// Antiprompts: берём дефолтные для формата промпта (gemma/non-gemma) и
	// мерджим с пользовательскими req.Stop (если заданы). Это нужно, чтобы
	// модель останавливалась в конце своего хода даже когда llama_vocab_is_eog
	// не срабатывает (например, после long-form ответа в naive gemma-prompt).
	defaultAP := defaultAntipromptsForModel(req.Model)
	if len(defaultAP) > 0 {
		params.Antiprompts = append(params.Antiprompts, defaultAP...)
	}
	if !usedNaive {
		// Если шаблон GGUF сработал, всё равно добавим <end_of_turn>-like маркеры
		// для gemma, на случай если модель генерирует дополнительные ходы.
		if isGemmaModel(req.Model) {
			params.Antiprompts = append(params.Antiprompts, []string{"<end_of_turn>", "<start_of_turn>user"}...)
		}
	}
	for _, s := range req.Stop {
		if s != "" {
			params.Antiprompts = append(params.Antiprompts, s)
		}
	}
	logger.Get().Debugw("handleV1ChatCompletions: antiprompts",
		"model", req.Model, "used_naive", usedNaive,
		"antiprompts_count", len(params.Antiprompts),
		"antiprompts", params.Antiprompts)

	if req.Stream {
		writeOpenAIChatStream(w, r, req.Model, prompt, params)
		return
	}

	// cppworker side Stage 3.2.a: прокидываем структурированную информацию
	// об ошибке в JSON-теле (см. writeCppWorkerErrorWithBridgeInfo).
	start := time.Now()
	result, err := generateWithRamFallback(req.Model, prompt, params)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "chat completions failed", err)
		return
	}
	durationMs := time.Since(start).Milliseconds()

	// Формат ответа OpenAI
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": result.Output,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
		"duration_ms": durationMs,
	})
}

// writeOpenAIChatStream — streaming ответ в формате SSE для /v1/chat/completions.
// Отправляет типизированные SSE-события с полями choices, как OpenAI API.
func writeOpenAIChatStream(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Критично: немедленно отправляем HTTP-заголовки, иначе балансировщик/клиент
	// ждёт первого токена и таймаутится (ResponseHeaderTimeout / idle-timeout).
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	chatID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	// Канал активности: закрывается после generateStreamWithRamFallback,
	// чтобы keepalive-горутина завершилась.
	tokenDone := make(chan struct{})
	defer close(tokenDone)

	// SSE keepalive: посылаем комментарий каждые 15 секунд, пока модель
	// готовит первый токен или между редкими чанками. Это удерживает
	// соединение для балансера и OpenAI-клиентов (Roo Code/Cline/undici).
	keepaliveInterval := 15 * time.Second

	// Мьютекс защищает w от одновременной записи callback'ом и keepalive-горутиной.
	var writeMu sync.Mutex
	safeFlush := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		flusher.Flush()
	}
	safeFprintf := func(format string, a ...interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		fmt.Fprintf(w, format, a...)
	}

	keepaliveDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		defer close(keepaliveDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tokenDone:
				return
			case <-ticker.C:
				safeFprintf(": keepalive\n\n")
				safeFlush()
			}
		}
	}()

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]string{
						"role":    "assistant",
						"content": token,
					},
					"finish_reason": nil,
				},
			},
		}
		jsonData, _ := json.Marshal(chunk)
		safeFprintf("data: %s\n\n", jsonData)
		safeFlush()
		return true
	}
	if err := generateStreamWithRamFallback(modelName, prompt, params, callback); err != nil {
		// При memory slot leak контекст модели в llama.cpp не освобождается.
		// Перезапускаем процесс — docker-compose поднимет контейнер заново с чистым VRAM.
		maybeRestartOnMemorySlotError(err, modelName)
		errChunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]string{},
					"finish_reason": "error",
				},
			},
			"error": err.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		safeFprintf("data: %s\n\n", errJSON)
		safeFprintf("data: [DONE]\n\n")
		safeFlush()
		return
	}

	// Финальный stop-чанк
	stopChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]string{},
				"finish_reason": "stop",
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	safeFprintf("data: %s\n\n", stopJSON)
	// OpenAI завершающий маркер [DONE]
	safeFprintf("data: [DONE]\n\n")
	safeFlush()
}

// handleV1Completions — OpenAI-совместимый /v1/completions endpoint.
func handleV1Completions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req openAICompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}

	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	params := bridge.DefaultGenerationParams()
	if req.Temperature > 0 {
		params.Temperature = float32(req.Temperature)
	}
	if req.TopP > 0 {
		params.TopP = float32(req.TopP)
	}
	if req.MaxTokens > 0 {
		params.NPredict = req.MaxTokens
	}
	if req.PresencePenalty > 0 {
		params.PresencePenalty = float32(req.PresencePenalty)
	}
	if req.FrequencyPenalty > 0 {
		params.FrequencyPenalty = float32(req.FrequencyPenalty)
	}
	if req.Seed != 0 {
		params.Seed = req.Seed
	}
	// Per-request n_ctx override (Шаг 1.4). 0 = использовать n_ctx модели.
	// Header X-Cpp-Ctx применяется ПОСЛЕ через applyCppCtxHeader.
	if req.NumCtx > 0 {
		params.NCtxOverride = req.NumCtx
	}
	// Политика балансировщика (X-Cpp-Ctx header) — только если body не задал.
	applyCppCtxHeader(r, &params)

	if req.Stream {
		writeOpenAICompletionStream(w, r, req.Model, req.Prompt, params)
		return
	}

	// cppworker side Stage 3.2.a: прокидываем структурированную информацию
	// об ошибке в JSON-теле.
	start := time.Now()
	result, err := generateWithRamFallback(req.Model, req.Prompt, params)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "completions failed", err)
		return
	}
	durationMs := time.Since(start).Milliseconds()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":      fmt.Sprintf("cmpl-%d", time.Now().UnixNano()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"text":          result.Output,
				"index":         0,
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
		"duration_ms": durationMs,
	})
}

// writeOpenAICompletionStream — streaming ответ в формате SSE для /v1/completions.
func writeOpenAICompletionStream(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	completionID := fmt.Sprintf("cmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		chunk := map[string]interface{}{
			"id":      completionID,
			"object":  "text_completion",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"text":          token,
					"index":         0,
					"finish_reason": nil,
				},
			},
		}
		jsonData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
		return true
	}
	if err := generateStreamWithRamFallback(modelName, prompt, params, callback); err != nil {
		// При memory slot leak контекст модели в llama.cpp не освобождается.
		// Перезапускаем процесс — docker-compose поднимет контейнер заново с чистым VRAM.
		maybeRestartOnMemorySlotError(err, modelName)
		errChunk := map[string]interface{}{
			"id":      completionID,
			"object":  "text_completion",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"text":          "",
					"index":         0,
					"finish_reason": "error",
				},
			},
			"error": err.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "data: %s\n\n", errJSON)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	stopChunk := map[string]interface{}{
		"id":      completionID,
		"object":  "text_completion",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"text":          "",
				"index":         0,
				"finish_reason": "stop",
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	fmt.Fprintf(w, "data: %s\n\n", stopJSON)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// handleV1Embeddings — OpenAI-совместимый /v1/embeddings endpoint.
func handleV1Embeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" || req.Input == "" {
		writeError(w, http.StatusBadRequest, "model and input are required")
		return
	}
	embeddings, err := backend.GetEmbeddings(req.Model, req.Input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "embeddings failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data": []map[string]interface{}{
			{
				"object":    "embedding",
				"index":     0,
				"embedding": embeddings,
			},
		},
		"model": req.Model,
		"usage": map[string]interface{}{
			"prompt_tokens": 0,
			"total_tokens":  0,
		},
	})
}

// handleOllamaEmbeddings — Ollama-совместимый /api/embeddings endpoint.
// Принимает поле "prompt" (стандарт Ollama), маппит на "input" для backend.GetEmbeddings.
func handleOllamaEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Model  string `json:"model"`
		Input  string `json:"input"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	text := req.Input
	if text == "" {
		text = req.Prompt
	}
	if text == "" {
		writeError(w, http.StatusBadRequest, "input or prompt is required")
		return
	}

	// Ленивая загрузка модели для embeddings
	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	embeddings, err := backend.GetEmbeddings(req.Model, text)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "embeddings failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"embedding": embeddings,
	})
}

// handleV1Models — OpenAI-совместимый /v1/models endpoint.
func handleV1Models(w http.ResponseWriter, r *http.Request) {
	models := backend.ListModels()
	openaiModels := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		openaiModels = append(openaiModels, map[string]interface{}{
			"id":       m.Name,
			"object":   "model",
			"created":  m.LoadedAt.Unix(),
			"owned_by": "ollamalegion",
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   openaiModels,
	})
}

// ============================================================
// Model reload (Шаг 4)
// ============================================================

// reloadModelRequest — параметры для POST /api/models/reload.
// Позволяет перезагрузить уже-загруженную модель с новыми параметрами
// (в первую очередь — contextSize, batchSize, gpuLayers).
// Без этого параметры модели фиксируются при первичной загрузке.
type reloadModelRequest struct {
	Name        string `json:"name"`
	ContextSize *int   `json:"contextSize,omitempty"`
	BatchSize   *int   `json:"batchSize,omitempty"`
	GPULayers   *int   `json:"gpuLayers,omitempty"`
	FlashAttn   *int   `json:"flashAttn,omitempty"`
	NUMA        *bool  `json:"numa,omitempty"`
	UseMmap     *bool  `json:"useMmap,omitempty"`
}

// handleReloadModel — POST /api/models/reload.
// 1) Берём текущий путь модели через backend.GetModel(name)
// 2) UnloadModel(name) — синхронно освобождает VRAM
// 3) LoadModelWithOpts(name, path, opts) — загружаем с новыми параметрами
// 4) Возвращаем JSON {status: reloaded, model: ...}
//
// Это блокирующий endpoint (5-30 сек для типичной модели). WebUI
// показывает прогресс-бар (см. cppworker-params.js).
func handleReloadModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req reloadModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Получаем текущие параметры модели (включая путь к GGUF).
	current, err := backend.GetModel(req.Name)
	if err != nil {
		writeError(w, http.StatusNotFound, "model not currently loaded: "+err.Error()+
			" — use /api/models/load to load it first")
		return
	}

	modelPath := current.Path
	if modelPath == "" {
		// Резолвим через ModelManager если path не сохранён.
		mm := backend.ModelManager()
		if mm != nil {
			foundPath, ferr := mm.FindModelByPath(req.Name)
			if ferr == nil {
				modelPath = foundPath
			}
		}
	}
	if modelPath == "" {
		writeError(w, http.StatusInternalServerError, "could not resolve model path for "+req.Name)
		return
	}

	// Валидация: contextSize ∈ [256, 262144] (256K max для gemma-4)
	if req.ContextSize != nil {
		if *req.ContextSize < 256 || *req.ContextSize > 262144 {
			writeError(w, http.StatusBadRequest, "contextSize must be in [256, 262144], got "+strconv.Itoa(*req.ContextSize))
			return
		}
	}
	if req.BatchSize != nil && *req.BatchSize < 1 {
		writeError(w, http.StatusBadRequest, "batchSize must be >= 1, got "+strconv.Itoa(*req.BatchSize))
		return
	}
	if req.GPULayers != nil && *req.GPULayers < -1 {
		writeError(w, http.StatusBadRequest, "gpuLayers must be >= -1 (-1 = all layers)")
		return
	}

	// Собираем opts: новые из request + дефолты из текущей модели.
	opts := cppbackend.LoadModelOpts{
		GPULayers:     defaultIntPtr(req.GPULayers, current.GPULayers),
		ContextSize:   defaultIntPtr(req.ContextSize, current.ContextSize),
		BatchSize:     defaultIntPtr(req.BatchSize, current.BatchSize),
		FlashAttnType: defaultIntPtr(req.FlashAttn, current.FlashAttnType),
		NUMA:          defaultBoolPtr(req.NUMA, current.NUMA),
		UseMmap:       defaultBoolPtr(req.UseMmap, current.UseMmap),
		TensorSplit:   current.TensorSplit,
	}

	logger.Get().Infow("reloading model with new params",
		"name", req.Name, "path", modelPath,
		"old_ctx", current.ContextSize, "new_ctx", opts.ContextSize,
		"old_batch", current.BatchSize, "new_batch", opts.BatchSize,
		"old_gpu_layers", current.GPULayers, "new_gpu_layers", opts.GPULayers)

	// === Per-model blocking reload ===
	// Используем TryLockLoad для защиты от двойной загрузки той же модели.
	lockOk, lockErr := backend.TryLockLoad(req.Name)
	if lockErr != nil {
		// Модель уже загружена кем-то ещё — это нормально для reload,
		// но мы не можем эксклюзивно выгружать. Отвечаем 503.
		logger.Get().Infow("reload: model is already being loaded by another request",
			"name", req.Name)
		writeLoadingResponse(w, req.Name, errModelIsLoading)
		return
	}
	if !lockOk {
		logger.Get().Infow("reload: another goroutine is already loading this model, waiting",
			"name", req.Name)
		if backend.WaitForLoad(req.Name) {
			// Модель успешно загружена другой горутиной — reload не нужен.
			model, getErr := backend.GetModel(req.Name)
			if getErr == nil {
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status": "reloaded_by_other",
					"model":  model,
				})
				return
			}
		}
		writeLoadingResponse(w, req.Name, errModelIsLoading)
		return
	}
	defer backend.UnlockLoad(req.Name)

	// 1) Unload (синхронно).
	unloadStart := time.Now()
	if err := backend.UnloadModel(req.Name); err != nil {
		writeError(w, http.StatusInternalServerError, "unload failed: "+err.Error())
		return
	}
	logger.Get().Infow("reload: model unloaded",
		"name", req.Name, "unload_ms", time.Since(unloadStart).Milliseconds())

	// 2) Load с новыми параметрами.
	loadStart := time.Now()
	if err := backend.LoadModelWithOpts(req.Name, modelPath, opts); err != nil {
		logger.Get().Errorw("reload: load with new params failed",
			"name", req.Name, "error", err)
		// best-effort: возвращаем старую модель (с теми же параметрами)
		oldOpts := cppbackend.LoadModelOpts{
			GPULayers:     current.GPULayers,
			ContextSize:   current.ContextSize,
			BatchSize:     current.BatchSize,
			FlashAttnType: current.FlashAttnType,
			NUMA:          current.NUMA,
			UseMmap:       current.UseMmap,
			TensorSplit:   current.TensorSplit,
		}
		if rollbackErr := backend.LoadModelWithOpts(req.Name, modelPath, oldOpts); rollbackErr != nil {
			logger.Get().Errorw("reload rollback failed (model is no longer loaded!)",
				"name", req.Name, "rollback_error", rollbackErr)
		}
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	logger.Get().Infow("reload: model loaded with new params",
		"name", req.Name, "load_ms", time.Since(loadStart).Milliseconds())

	// 3) Возвращаем обновлённую модель.
	model, err := backend.GetModel(req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":           "reloaded",
		"name":             req.Name,
		"contextSize":      opts.ContextSize,
		"batchSize":        opts.BatchSize,
		"gpuLayers":        opts.GPULayers,
		"flashAttnType":    opts.FlashAttnType,
		"reloadDurationMs": time.Since(unloadStart).Milliseconds(),
		"model":            model,
	})
}

// ============================================================
// CppWorker Config handlers
// ============================================================

var currentConfig *cppbackend.Config

func handleCppWorkerGetConfig(w http.ResponseWriter, r *http.Request) {
	if currentConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"config":      currentConfig,
		"nodeName":    os.Getenv("NODE_NAME"),
		"nodeLabels":  os.Getenv("NODE_LABELS"),
		"balancerUrl": os.Getenv("BALANCER_URL"),
		"uptime":      time.Since(uptimeStart).String(),
	})
}

func handleCppWorkerUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "use PUT")
		return
	}
	if currentConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	applied := []string{}
	if v, ok := updates["defaultCtxSize"]; ok {
		if ctx, ok := v.(float64); ok && ctx >= 256 {
			currentConfig.DefaultCtxSize = int(ctx)
			applied = append(applied, "defaultCtxSize")
		}
	}
	if v, ok := updates["defaultBatchSize"]; ok {
		if batch, ok := v.(float64); ok && batch >= 1 {
			currentConfig.DefaultBatchSize = int(batch)
			applied = append(applied, "defaultBatchSize")
		}
	}
	if v, ok := updates["defaultGpuLayers"]; ok {
		if gpu, ok := v.(float64); ok {
			currentConfig.DefaultGPULayers = int(gpu)
			applied = append(applied, "defaultGpuLayers")
		}
	}
	if v, ok := updates["defaultFlashAttnType"]; ok {
		if fa, ok := v.(float64); ok {
			currentConfig.DefaultFlashAttnType = int(fa)
			applied = append(applied, "defaultFlashAttnType")
		}
	}
	if v, ok := updates["defaultNuma"]; ok {
		if numa, ok := v.(bool); ok {
			currentConfig.DefaultNUMA = numa
			applied = append(applied, "defaultNuma")
		}
	}
	if v, ok := updates["defaultNThreads"]; ok {
		if threads, ok := v.(float64); ok {
			currentConfig.DefaultNThreads = int(threads)
			applied = append(applied, "defaultNThreads")
		}
	}
	envPath := *envFile
	if envPath == "" {
		envPath = "/app/.env"
	}
	if err := currentConfig.SaveConfigToDotEnv(envPath); err != nil {
		logger.Get().Warnw("failed to save config to .env", "path", envPath, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "updated", "applied": applied})
}

func handleCppWorkerReloadConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	envPath := *envFile
	if envPath == "" {
		envPath = "/app/.env"
	}
	if _, err := os.Stat(envPath); err != nil {
		writeError(w, http.StatusNotFound, "env file not found: "+envPath)
		return
	}
	if err := cppbackend.LoadDotEnvFile(envPath); err != nil {
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	newCfg := cppbackend.LoadConfigFromEnv()
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			newCfg.Port = *port
		case "models-dir":
			newCfg.ModelsDir = *modelsDir
		case "ctx-size":
			newCfg.DefaultCtxSize = *ctxSize
		case "batch-size":
			newCfg.DefaultBatchSize = *batchSize
		case "gpu-layers":
			newCfg.DefaultGPULayers = *gpuLayers
		case "flash-attn":
			newCfg.DefaultFlashAttnType = *flashAttn
		case "numa":
			newCfg.DefaultNUMA = *numa
		case "no-mmap":
			newCfg.DefaultUseMmap = !*noMmap
		}
	})
	// RAM fallback flags не сохраняются в cppbackend.Config, но при reload
	// из .env мы перечитываем их, чтобы runtime-конфиг соответствовал env.
	if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_N_CTX"); envVal != "" {
		*ramFallbackNCtx = parseBoolEnv(envVal)
	}
	if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_GPU_LAYERS"); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v >= -1 {
			*ramFallbackGpuLayers = v
		}
	}
	if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_MAX_N_CTX"); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v >= 512 {
			*ramFallbackMaxNCtx = v
		}
	}
	*currentConfig = newCfg
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "reloaded", "config": currentConfig})
}

func parseInt(s string, defaultVal int) (int, error) {
	if s == "" {
		return defaultVal, nil
	}
	val := 0
	_, err := fmt.Sscanf(s, "%d", &val)
	if err != nil {
		return defaultVal, err
	}
	return val, nil
}

// parseBoolEnv парсит env-значение в bool. Поддерживает "true", "1",
// "yes", "on" как true; остальное (включая пустую строку) как false.
func parseBoolEnv(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "1" || s == "yes" || s == "on"
}

// isFlagSet — возвращает true, если флаг с именем name был явно передан
// пользователем в командной строке (а не оставлен со значением по умолчанию).
// Используется для определения, нужно ли применить CPPWORKER_PORT из env
// (если --port не задан, можно подменить default на значение env).
// Использует flag.Lookup, который ищет среди уже зарегистрированных флагов.
func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// runHealthCheck выполняет одноразовый probe на /health и завершает процесс.
// Используется в Docker HEALTHCHECK вместо curl/wget, которых может не быть
// в runtime-образе.
func runHealthCheck() {
	probePort := *port
	if !isFlagSet("port") {
		if envPort := os.Getenv("CPPWORKER_PORT"); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil && p > 0 && p <= 65535 {
				probePort = p
			}
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", probePort)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck failed: HTTP %d from %s\n", resp.StatusCode, url)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "healthcheck passed")
	os.Exit(0)
}

// ============================================================
// Router setup
// ============================================================

func setupRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/api/health", handleHealth)
	mux.HandleFunc("/info", handleInfo)
	mux.HandleFunc("/api/info", handleInfo)
	mux.HandleFunc("/api/gpu", handleGPUInfo)
	mux.HandleFunc("/api/models/load", handleLoadModel)
	mux.HandleFunc("/load", handleLoadModel)                        // alias for balancer warmup
	mux.HandleFunc("/api/models/load/progress", handleLoadProgress) // loading state polling
	mux.HandleFunc("/api/models/unload", handleUnloadModel)
	mux.HandleFunc("/api/models/reload", authMiddleware(handleReloadModel))
	mux.HandleFunc("/api/models", handleListModels)
	mux.HandleFunc("/api/model", handleGetModel)
	mux.HandleFunc("/api/models/files", handleListModelsDir)
	mux.HandleFunc("/api/models/delete", handleDeleteModel)
	mux.HandleFunc("/api/delete", handleDeleteModel) // Ollama-compatible alias
	mux.HandleFunc("/api/generate", handleGenerate)
	mux.HandleFunc("/api/chat", handleChat)
	mux.HandleFunc("/api/embeddings", handleOllamaEmbeddings)
	mux.HandleFunc("/api/ollama/generate", handleOllamaGenerate)
	mux.HandleFunc("/api/ollama/tags", handleOllamaTags)
	mux.HandleFunc("/api/tags", handleOllamaTags)
	mux.HandleFunc("/api/show", handleOllamaShow)
	mux.HandleFunc("/api/copy", handleOllamaCopy)
	mux.HandleFunc("/api/create", handleOllamaCreate)
	mux.HandleFunc("/api/push", handleOllamaPush)
	mux.HandleFunc("/api/version", handleCppWorkerVersion)
	mux.HandleFunc("/api/hf/search", handleHFSearch)
	mux.HandleFunc("/api/hf/files", handleHFFiles)
	mux.HandleFunc("/api/hf/download", handleHFDownload)
	mux.HandleFunc("/api/hf/progress", handleHFDownloadProgress)
	mux.HandleFunc("/api/hf/downloads", handleHFDownloads)
	mux.HandleFunc("/api/hf/cancel", handleHFCancel)
	mux.HandleFunc("/api/pull", handlePull)
	mux.HandleFunc("/api/v1/cppworker/config", handleCppWorkerGetConfig)
	mux.HandleFunc("/api/v1/cppworker/config/update", authMiddleware(handleCppWorkerUpdateConfig))
	mux.HandleFunc("/api/v1/cppworker/config/reload", authMiddleware(handleCppWorkerReloadConfig))
	mux.HandleFunc("/api/v1/cppworker/health", handleHealth)
	mux.HandleFunc("/api/v1/cppworker/metrics", handleInfo)
	// OpenAI-совместимые /v1/ endpoints
	mux.HandleFunc("/v1/chat/completions", handleV1ChatCompletions)
	mux.HandleFunc("/v1/completions", handleV1Completions)
	mux.HandleFunc("/v1/embeddings", handleV1Embeddings)
	mux.HandleFunc("/v1/models", handleV1Models)
	return corsMiddleware(loggingMiddleware(mux))
}

// ============================================================
// Main
// ============================================================

func main() {
	flag.Parse()

	if *healthCheck {
		runHealthCheck()
		return
	}

	logLevel := "info"
	if *verbose {
		logLevel = "debug"
	}
	logger.Init(logLevel)
	log := logger.Get()

	dotEnvPath := *envFile
	if dotEnvPath == "" {
		candidates := []string{".env", "config/cppworker.env", "/app/.env"}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				dotEnvPath = p
				break
			}
		}
	}
	if dotEnvPath != "" {
		if err := cppbackend.LoadDotEnvFile(dotEnvPath); err != nil {
			log.Warnw("failed to load .env file, continuing with defaults", "path", dotEnvPath, "error", err)
		} else {
			log.Infow("loaded configuration from .env", "path", dotEnvPath)
		}
	}

	cfg := cppbackend.LoadConfigFromEnv()
	// Если --port не передан явно (равен default 18092), а CPPWORKER_PORT
	// задан в env с отличным значением — применяем env. Это позволяет
	// запускать cppworker без compose (например, docker run -e CPPWORKER_PORT=18091)
	// и получать корректный порт вместо зашитого 18092. См. п.8.3 в
	// docs/cppworker-routing-fixes-2026-06-07.md.
	if !isFlagSet("port") {
		if envPort := os.Getenv("CPPWORKER_PORT"); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil && p > 0 && p <= 65535 {
				*port = p
				log.Infow("applied CPPWORKER_PORT from env (flag default not overridden)", "port", p)
			}
		}
	}
	// RAM fallback feature flags из env (если флаги не переданы явно).
	if !isFlagSet("ram-fallback-n-ctx") {
		if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_N_CTX"); envVal != "" {
			*ramFallbackNCtx = parseBoolEnv(envVal)
		}
	}
	if !isFlagSet("ram-fallback-gpu-layers") {
		if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_GPU_LAYERS"); envVal != "" {
			if v, err := strconv.Atoi(envVal); err == nil && v >= -1 {
				*ramFallbackGpuLayers = v
			}
		}
	}
	if !isFlagSet("ram-fallback-max-n-ctx") {
		if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_MAX_N_CTX"); envVal != "" {
			if v, err := strconv.Atoi(envVal); err == nil && v >= 512 {
				*ramFallbackMaxNCtx = v
			}
		}
	}

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			cfg.Port = *port
		case "models-dir":
			cfg.ModelsDir = *modelsDir
		case "ctx-size":
			cfg.DefaultCtxSize = *ctxSize
		case "batch-size":
			cfg.DefaultBatchSize = *batchSize
		case "gpu-layers":
			cfg.DefaultGPULayers = *gpuLayers
		case "flash-attn":
			cfg.DefaultFlashAttnType = *flashAttn
		case "numa":
			cfg.DefaultNUMA = *numa
		case "no-mmap":
			cfg.DefaultUseMmap = !*noMmap
		}
	})

	if err := cfg.Validate(); err != nil {
		log.Fatalw("invalid configuration", "error", err)
	}

	log.Infow("CppBackend Worker starting",
		"port", cfg.Port, "host", cfg.Host, "modelsDir", cfg.ModelsDir,
		"gpuLayers", cfg.DefaultGPULayers, "flashAttn", cfg.DefaultFlashAttnType,
		"numa", cfg.DefaultNUMA,
		"ramFallbackNCtx", *ramFallbackNCtx,
		"ramFallbackGpuLayers", *ramFallbackGpuLayers,
		"ramFallbackMaxNCtx", *ramFallbackMaxNCtx)

	if err := os.MkdirAll(cfg.ModelsDir, 0755); err != nil {
		log.Fatalw("failed to create models directory", "dir", cfg.ModelsDir, "error", err)
	}

	cc := cfg
	currentConfig = &cc

	backend = cppbackend.NewBackend(cfg)
	if err := backend.Init(); err != nil {
		log.Fatalw("failed to initialize backend", "error", err)
	}
	log.Infow("Backend initialized", "version", backend.Version(), "gpuCount", backend.GetGPUCount())

	router := setupRouter()
	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Infow("HTTP server listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalw("HTTP server error", "error", err)
		}
	}()

	// Авто-загрузка моделей при старте (только если --preload-models)
	// По умолчанию ОТКЛЮЧЕНА — модели загружаются лениво балансировщиком через /api/models/load
	if *preloadModels {
		go autoLoadModels(cfg)
	} else {
		log.Infow("preload-models disabled (models will be loaded lazily by the balancer on first request)")
	}

	// Auto-registration в балансировщике (если CPPWORKER_BALANCER_URL задан).
	// Если балансировщик недоступен — cppworker продолжает работать в standalone-режиме
	// (принимает запросы напрямую на свой порт).
	balancerRegCtx, balancerRegCancel := context.WithCancel(context.Background())
	defer balancerRegCancel()
	balancerReg = newBalancerRegistration(&cfg)
	if balancerReg != nil {
		balancerReg.start(balancerRegCtx, log)
	} else {
		log.Infow("balancer auto-registration disabled (CPPWORKER_BALANCER_URL not set); cppworker will work in standalone mode")
	}

	<-quit
	log.Infow("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	balancerRegCancel() // попросить auto-registration goroutine остановиться
	if balancerReg != nil {
		balancerReg.stop(ctx, log)
	}
	backend.Close()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalw("Server forced to shutdown", "error", err)
	}
	log.Infow("CppBackend Worker stopped")
}

// convertFlashAttn converts flash-attn flag value to FlashAttnType.
// If the flag wasn't set (value == -2), returns the config default.
func convertFlashAttn(flagVal int) int {
	if flagVal == -2 {
		if currentConfig != nil {
			return currentConfig.DefaultFlashAttnType
		}
		return -1
	}
	if flagVal < -1 {
		return -1
	}
	if flagVal > 1 {
		return 1
	}
	return flagVal
}

func countTokens(modelName, text string) int {
	if text == "" {
		return 0
	}
	return backend.CountTokens(modelName, text)
}

// countModelTokensByLoadedInfo — если модель ещё не загружена, не пытаться
// загружать её ради подсчёта токенов; вернуть грубую оценку.
func countModelTokensByLoadedInfo(modelName, text string) int {
	if text == "" {
		return 0
	}
	if _, err := backend.GetModel(modelName); err != nil {
		return len([]rune(text)) / 4
	}
	return backend.CountTokens(modelName, text)
}

// sameLoadOptions сравнивает параметры загруженной модели с запрошенными
// параметрами загрузки. Используется handleLoadModel для защиты от
// повторной загрузки модели с теми же параметрами.
func sameLoadOptions(info cppbackend.ModelInfo, opts cppbackend.LoadModelOpts) bool {
	if info.ContextSize != opts.ContextSize {
		return false
	}
	if info.BatchSize != opts.BatchSize {
		return false
	}
	if info.GPULayers != opts.GPULayers {
		return false
	}
	if info.FlashAttnType != opts.FlashAttnType {
		return false
	}
	if info.NUMA != opts.NUMA {
		return false
	}
	if info.UseMmap != opts.UseMmap {
		return false
	}
	if len(info.TensorSplit) != len(opts.TensorSplit) {
		return false
	}
	for i := range info.TensorSplit {
		if info.TensorSplit[i] != opts.TensorSplit[i] {
			return false
		}
	}
	return true
}

// isNCtxNeedsReload — true, если последняя ошибка C-bridge говорит
// "requested n_ctx exceeds model's effective n_ctx" (code 2).
func isNCtxNeedsReload() bool {
	info := bridge.GetLastErrorInfo()
	return info != nil && info.Code == bridge.ErrCodeNCtxNeedsReload
}

// isGpuOomOrNCtxNeedsReload — true, если последняя ошибка C-bridge
// требует перезагрузки модели с другими параметрами: либо n_ctx
// недостаточен (code 2), либо GPU OOM (code 4). В обоих случаях
// RAM fallback может помочь, снизив GPU-слои и/или увеличив n_ctx.
func isGpuOomOrNCtxNeedsReload() bool {
	info := bridge.GetLastErrorInfo()
	if info == nil {
		return false
	}
	return info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodeGPUOOM
}

// generateWithRamFallback пытается выполнить backend.Generate; если
// получает ErrCodeNCtxNeedsReload или ErrCodeGPUOOM и ram-fallback включён — 
// перезагружает модель с запрошенным n_ctx через mmap/RAM и повторяет генерацию.
// Используется для non-streaming эндпоинтов.
func generateWithRamFallback(modelName, prompt string, params bridge.GenerationParams) (*bridge.InferenceResult, error) {
	result, err := backend.Generate(modelName, prompt, params)
	if err == nil {
		return result, nil
	}
	if !isGpuOomOrNCtxNeedsReload() || params.NCtxOverride <= 0 {
		return result, err
	}
	if ok, fbErr := tryRamFallbackReload(modelName, params.NCtxOverride); !ok {
		return result, err // возвращаем исходную ошибку; fbErr только логируем
	} else if fbErr != nil {
		logger.Get().Warnw("RAM fallback declined", "model", modelName, "error", fbErr)
		return result, err
	}
	return backend.Generate(modelName, prompt, params)
}

// generateStreamWithRamFallback — аналог generateWithRamFallback для streaming.
// При ErrCodeNCtxNeedsReload или ErrCodeGPUOOM на старте (pre-flight) перезагружает
// модель и запускает стрим заново. Если стрим уже частично начался, fallback не
// применяется (вернётся текущая ошибка).
func generateStreamWithRamFallback(modelName, prompt string, params bridge.GenerationParams, callback bridge.StreamCallback) error {
	err := backend.GenerateStream(modelName, prompt, params, callback)
	if err == nil {
		return nil
	}
	if !isGpuOomOrNCtxNeedsReload() || params.NCtxOverride <= 0 {
		return err
	}
	if ok, fbErr := tryRamFallbackReload(modelName, params.NCtxOverride); !ok {
		return err
	} else if fbErr != nil {
		logger.Get().Warnw("RAM fallback declined", "model", modelName, "error", fbErr)
		return err
	}
	return backend.GenerateStream(modelName, prompt, params, callback)
}

// effectiveRamFallbackGPULayers возвращает целевое число GPU-слоёв для
// RAM fallback: если пользователь явно задал ram-fallback-gpu-layers >= 0,
// используем его; иначе сохраняем текущее значение (оставляем llama.cpp
// решать, но mmap позволит вытеснить часть в RAM при нехватке VRAM).
func effectiveRamFallbackGPULayers(current int) int {
	if *ramFallbackGpuLayers >= 0 {
		return *ramFallbackGpuLayers
	}
	return current
}

// tryRamFallbackReload пытается перезагрузить модель с запрошенным n_ctx,
// используя RAM через mmap, если VRAM недостаточна. Вызывается только
// после ErrCodeNCtxNeedsReload и только если ram-fallback-n-ctx включён.
// Возвращает (ok=true, nil) если модель успешно перезагружена.
func tryRamFallbackReload(modelName string, requestedNCtx int) (bool, error) {
	if !*ramFallbackNCtx {
		return false, nil
	}
	if requestedNCtx <= 0 {
		return false, fmt.Errorf("requested n_ctx must be > 0 for RAM fallback")
	}
	if *ramFallbackMaxNCtx > 0 && requestedNCtx > *ramFallbackMaxNCtx {
		return false, fmt.Errorf("requested n_ctx=%d exceeds ram-fallback-max-n-ctx=%d", requestedNCtx, *ramFallbackMaxNCtx)
	}

	current, err := backend.GetModel(modelName)
	if err != nil {
		return false, fmt.Errorf("cannot get current model info: %w", err)
	}
	modelPath := current.Path
	if modelPath == "" {
		mm := backend.ModelManager()
		if mm != nil {
			foundPath, ferr := mm.FindModelByPath(modelName)
			if ferr == nil {
				modelPath = foundPath
			}
		}
	}
	if modelPath == "" {
		return false, fmt.Errorf("cannot resolve model path for %s", modelName)
	}

	newGPULayers := effectiveRamFallbackGPULayers(current.GPULayers)
	opts := cppbackend.LoadModelOpts{
		GPULayers:     newGPULayers,
		ContextSize:   requestedNCtx,
		BatchSize:     current.BatchSize,
		FlashAttnType: current.FlashAttnType,
		NUMA:          current.NUMA,
		UseMmap:       true, // RAM fallback через mmap
		TensorSplit:   current.TensorSplit,
	}

	lockOk, lockErr := backend.TryLockLoad(modelName)
	if lockErr != nil {
		// модель уже загружена — RAM fallback тривиально успешен
		return true, nil
	}
	if !lockOk {
		// другая горутина грузит эту модель — ждём
		if backend.WaitForLoad(modelName) {
			return true, nil
		}
		// не удалось дождаться — fallback не сработал, но ошибку оставим оригинальную
		return false, fmt.Errorf("RAM fallback: model is being loaded by another request, but wait failed")
	}
	// lock acquired — мы отвечаем за загрузку
	defer backend.UnlockLoad(modelName)

	logger.Get().Infow("RAM fallback: reloading model with larger n_ctx",
		"model", modelName,
		"path", modelPath,
		"old_n_ctx", current.ContextSize,
		"new_n_ctx", requestedNCtx,
		"old_gpu_layers", current.GPULayers,
		"new_gpu_layers", newGPULayers,
		"use_mmap", true)

	unloadStart := time.Now()
	if err := backend.UnloadModel(modelName); err != nil {
		return false, fmt.Errorf("RAM fallback unload failed: %w", err)
	}
	logger.Get().Infow("RAM fallback: model unloaded",
		"model", modelName, "unload_ms", time.Since(unloadStart).Milliseconds())

	loadStart := time.Now()
	if err := backend.LoadModelWithOpts(modelName, modelPath, opts); err != nil {
		logger.Get().Errorw("RAM fallback: reload failed",
			"model", modelName, "error", err)
		// best-effort rollback к старым параметрам, чтобы модель не осталась выгруженной
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
			logger.Get().Errorw("RAM fallback: rollback failed (model no longer loaded!)",
				"model", modelName, "rollback_error", rollbackErr)
		}
		return false, fmt.Errorf("RAM fallback reload failed: %w", err)
	}
	logger.Get().Infow("RAM fallback: model reloaded successfully",
		"model", modelName,
		"new_n_ctx", requestedNCtx,
		"gpu_layers", newGPULayers,
		"load_ms", time.Since(loadStart).Milliseconds())


	if balancerReg != nil {
		if info, err := backend.GetModel(modelName); err == nil {
			balancerReg.notifyModelLoaded(modelName, info.SizeBytes, info.ContextSize, info.GPULayers)
		}
	}
	return true, nil
}

// ensureModelLoaded — ленивая загрузка модели из файловой системы если она ещё не в памяти.
//
// Возвращаемые ошибки:
//   - nil: модель уже загружена или успешно загружена сейчас
//   - errModelIsLoading: другая горутина уже грузит эту модель.
//     Хендлеры перехватывают эту ошибку и отвечают 503 с retryAfterMs=3000
//   - прочие: ошибка загрузки (validation, FS, C-bridge)
func ensureModelLoaded(modelName string) error {
	// 0) Если модель уже загружена — сразу выходим.
	if _, err := backend.GetModel(modelName); err == nil {
		return nil
	}

	// 1) Используем TryLockLoad для постановки в очередь загрузки.
	//    Если другая горутина уже грузит — ждём и проверяем результат.
	lockOk, lockErr := backend.TryLockLoad(modelName)
	if lockErr != nil {
		// Модель уже загружена (успели загрузить между нашей проверкой и TryLockLoad).
		return nil
	}
	if !lockOk {
		// Другая горутина уже грузит эту модель — ждём завершения.
		if backend.WaitForLoad(modelName) {
			return nil
		}
		// Если waitForLoad вернул false — загрузка провалилась.
		// Пробуем сами загрузить (другой поток мог оставить модель в errored state).
		// Просто возвращаем errModelIsLoading — клиент получит 503 и повторит.
		return errModelIsLoading
	}

	// lockOk == true — мы отвечаем за загрузку.
	defer backend.UnlockLoad(modelName)

	log := logger.Get()
	log.Infow("lazy-loading model from filesystem", "model", modelName)

	// Ищем модель в файловой системе
	mm := backend.ModelManager()
	if mm != nil {
		// Сначала пробуем найти через FindModelByPath
		modelPath, err := mm.FindModelByPath(modelName)
		if err != nil {
			// Fallback: конструируем путь из modelsDir
			modelPath = filepath.Join(*modelsDir, modelName)
			if !strings.HasSuffix(modelPath, ".gguf") {
				matches, globErr := filepath.Glob(modelPath + "*.gguf")
				if globErr == nil && len(matches) > 0 {
					modelPath = matches[0]
				} else {
					modelPath += ".gguf"
				}
			}
		}

		opts := cppbackend.LoadModelOpts{
			GPULayers:     *gpuLayers,
			ContextSize:   *ctxSize,
			BatchSize:     *batchSize,
			FlashAttnType: convertFlashAttn(*flashAttn),
			NUMA:          *numa,
			UseMmap:       !*noMmap,
		}

		if err := backend.LoadModelWithOpts(modelName, modelPath, opts); err != nil {
			log.Errorw("lazy-load failed", "model", modelName, "path", modelPath, "error", err)
			return fmt.Errorf("failed to load model %s: %w", modelName, err)
		}

		log.Infow("lazy-load successful", "model", modelName, "path", modelPath)
		return nil
	}

	return fmt.Errorf("model %s not found in filesystem", modelName)
}

// writeLoadingResponse — хелпер: отвечает 503 Service Unavailable с JSON
// {"error":"model is loading","loading":true,"retryAfterMs":3000,"elapsedMs":N}.
// Используется всеми хендлерами, которые вызвали ensureModelLoaded и получили
// errModelIsLoading. Удобно для одинакового поведения NDJSON/SSE/HTTP.
func writeLoadingResponse(w http.ResponseWriter, modelName string, loadErr error) {
	elapsedMs, retryAfter := loadingInfoFor(loadErr)
	// ВАЖНО: используем http.StatusServiceUnavailable, а не InternalServerError.
	// Ollama/OpenWebUI/curl трактуют 5xx как «повтори», и благодаря явному
	// JSON-полю loading=true UI может показать спиннер вместо ошибки.
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter/1000))
	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		"error":        "model is loading: " + modelName,
		"loading":      true,
		"model":        modelName,
		"elapsedMs":    elapsedMs,
		"retryAfterMs": retryAfter,
	})
}

// autoLoadModels сканирует modelsDir и загружает все найденные .gguf файлы при старте
func autoLoadModels(cfg cppbackend.Config) {
	log := logger.Get()
	modelsDir := cfg.ModelsDir

	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		log.Warnw("auto-load: failed to read models directory", "dir", modelsDir, "error", err)
		return
	}

	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}
		modelPath := filepath.Join(modelsDir, name)
		modelName := strings.TrimSuffix(name, ".gguf")

		opts := cppbackend.LoadModelOpts{
			GPULayers:     cfg.DefaultGPULayers,
			ContextSize:   cfg.DefaultCtxSize,
			BatchSize:     cfg.DefaultBatchSize,
			FlashAttnType: cfg.DefaultFlashAttnType,
			NUMA:          cfg.DefaultNUMA,
			UseMmap:       cfg.DefaultUseMmap,
		}

		log.Infow("auto-loading model",
			"name", modelName,
			"path", modelPath,
			"gpuLayers", opts.GPULayers)

		// Используем TryLockLoad для защиты от двойной загрузки (если ensureModelLoaded
		// запущен конкурентно). autoLoadModels выполняется на старте, но модель может
		// также загружаться через handleLoadModel или ensureModelLoaded при первом
		// запросе (race между autoLoadModels и первым /api/generate).
		lockOk, lockErr := backend.TryLockLoad(modelName)
		if lockErr != nil {
			// Модель уже загружена — пропускаем (auto-load не нужен).
			log.Infow("auto-load: model already loaded", "name", modelName)
			loaded++
			continue
		}
		if !lockOk {
			// Другая горутина уже грузит эту модель — ждём завершения.
			log.Infow("auto-load: another goroutine is loading this model, waiting",
				"name", modelName)
			if backend.WaitForLoad(modelName) {
				loaded++
				log.Infow("auto-load: model loaded by another goroutine", "name", modelName)
			} else {
				log.Warnw("auto-load: wait for model failed, skipping", "name", modelName)
			}
			continue
		}

		if err := backend.LoadModelWithOpts(modelName, modelPath, opts); err != nil {
			backend.UnlockLoad(modelName)
			log.Errorw("auto-load: failed to load model",
				"name", modelName, "path", modelPath, "error", err)
			continue
		}
		backend.UnlockLoad(modelName)
		loaded++
		log.Infow("auto-loaded model successfully", "name", modelName)
	}

	if loaded > 0 {
		log.Infow("auto-load complete", "loaded", loaded, "dir", modelsDir)
	} else {
		log.Infow("auto-load: no .gguf files found", "dir", modelsDir)
	}
}

// ============================================================
// OpenWebUI-compatible /api/chat handler
// ============================================================

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	// NumCtx — per-request переопределение n_ctx (Ollama-совместимый).
	// nil/0 = использовать n_ctx модели. > 0 → C-bridge pre-flight check.
	NumCtx *int `json:"num_ctx,omitempty"`
}

type chatResponse struct {
	Model         string      `json:"model"`
	CreatedAt     string      `json:"created_at"`
	Message       chatMessage `json:"message"`
	Done          bool        `json:"done"`
	TotalDuration int64       `json:"total_duration,omitempty"`
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages array is required")
		return
	}

	// Ленивая загрузка модели если она ещё не в памяти
	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	// Собираем prompt из messages с корректным форматированием для chat-моделей.
	// Для gemma используем формат <start_of_turn>user/model<end_of_turn>, для
	// прочих — общий <|user|>...<|assistant|> формат.
	var promptBuilder strings.Builder
	isGemma := isGemmaModel(req.Model)
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>system\n")
			} else {
				promptBuilder.WriteString("<|system|>\n")
			}
			promptBuilder.WriteString(msg.Content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "user":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>user\n")
			} else {
				promptBuilder.WriteString("<|user|>\n")
			}
			promptBuilder.WriteString(msg.Content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "assistant":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>model\n")
			} else {
				promptBuilder.WriteString("<|assistant|>\n")
			}
			promptBuilder.WriteString(msg.Content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		}
	}
	// Добавляем маркер для ответа ассистента
	if isGemma {
		promptBuilder.WriteString("<start_of_turn>model\n")
	} else {
		promptBuilder.WriteString("<|assistant|>\n")
	}
	prompt := promptBuilder.String()

	logger.Get().Debugw("handleChat: assembled prompt",
		"model", req.Model,
		"messages_count", len(req.Messages),
		"prompt_len", len(prompt),
		"is_gemma", isGemma,
		"stream", req.Stream)

	genReq := generateRequest{
		Model:  req.Model,
		Prompt: prompt,
		Stream: req.Stream,
	}
	if req.Temperature != nil {
		genReq.Temperature = *req.Temperature
	}
	if req.MaxTokens != nil {
		genReq.MaxTokens = *req.MaxTokens
	}
	if req.NumCtx != nil && *req.NumCtx > 0 {
		genReq.NumCtx = *req.NumCtx
	}

	params := buildGenerationParams(genReq)
	// Политика балансировщика (X-Cpp-Ctx header) — только если body не задал num_ctx.
	applyCppCtxHeader(r, &params)
	// Antiprompts для gemma/non-gemma, чтобы модель останавливалась в конце хода
	// и не генерировала <start_of_turn>user в выдачу.
	params.Antiprompts = append(params.Antiprompts, defaultAntipromptsForModel(req.Model)...)
	logger.Get().Debugw("handleChat: antiprompts",
		"model", req.Model, "count", len(params.Antiprompts),
		"antiprompts", params.Antiprompts)

	// Streaming chat — возвращает NDJSON с полями model, message (как Ollama /api/chat)
	if req.Stream {
		writeChatStreamResponse(w, r, req.Model, prompt, params)
		return
	}

	// cppworker side Stage 3.2.a: прокидываем структурированную информацию
	// об ошибке в JSON-теле.
	// Non-streaming chat
	start := time.Now()
	result, err := generateWithRamFallback(req.Model, prompt, params)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "chat generate failed", err)
		return
	}
	modelName := req.Model
	duration := time.Since(start)
	resp := chatResponse{
		Model:     modelName,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Message: chatMessage{
			Role:    "assistant",
			Content: result.Output,
		},
		Done:          true,
		TotalDuration: duration.Nanoseconds(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeChatStreamResponse — streaming ответ в формате NDJSON для /api/chat.
// Каждая строка — JSON объект с полями model, message (как Ollama chat API).
func writeChatStreamResponse(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Немедленный flush заголовков: гарантирует, что клиент получит заголовки
	// ДО того, как произойдёт ошибка инференса. Без этого flusher-а, если
	// generateStreamWithRamFallback мгновенно вернёт ошибку (например,
	// "prompt too long"), клиент (OpenWebUI reasoning) получит пустое тело
	// и упадёт с json.JSONDecodeError.
	flusher.Flush()
	ctx := r.Context()
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		chunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message": map[string]string{
				"role":    "assistant",
				"content": token,
			},
			"done": false,
		}
		jsonData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", jsonData)
		flusher.Flush()
		return true
	}
	if err := generateStreamWithRamFallback(modelName, prompt, params, callback); err != nil {
		// При memory slot leak контекст модели в llama.cpp не освобождается.
		// Перезапускаем процесс — docker-compose поднимет контейнер заново с чистым VRAM.
		maybeRestartOnMemorySlotError(err, modelName)
		errChunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message": map[string]string{
				"role":    "assistant",
				"content": "",
			},
			"done":  true,
			"error": err.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}

	duration := time.Since(start)
	doneChunk := map[string]interface{}{
		"model":      modelName,
		"created_at": createdAt,
		"message": map[string]string{
			"role":    "assistant",
			"content": "",
		},
		"done":           true,
		"total_duration": duration.Nanoseconds(),
		"eval_count":     0,
		"eval_duration":  duration.Nanoseconds(),
	}
	doneJSON, _ := json.Marshal(doneChunk)
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}
