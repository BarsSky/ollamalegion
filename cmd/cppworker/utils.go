package main

import (
	"errors"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

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
	body, err := json.Marshal(data)
	if err != nil {
		logger.Get().Errorw("failed to marshal JSON response", "error", err)
		body = []byte(`{"error":"internal marshal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if _, writeErr := w.Write(body); writeErr != nil {
		logger.Get().Errorw("failed to write JSON response", "error", writeErr)
	}
}

// writeCppWorkerErrorWithBridgeInfo — структурированный JSON-ответ об ошибке
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

// writeReloadLoopLimitResponse — пишет HTTP 413 с детальным JSON-ответом,
// когда cppworker отказывается делать reload из-за превышения лимита попыток.
// Используется всеми handler'ами после генерации (если err это *ReloadLoopLimitError).
//
// Формат ответа:
//
//	{
//	  "error": "RAM fallback reload limit reached...",
//	  "code": "reload_loop_limit",
//	  "model": "...",
//	  "attempts": 3,
//	  "elapsed_seconds": 47.2,
//	  "suggestion": "Reduce tools/prompt size, or save a model profile with larger context_length"
//	}
//
// HTTP 413 — payload too large (стандартный код для "request не помещается в ресурсы").
func writeReloadLoopLimitResponse(w http.ResponseWriter, err *ReloadLoopLimitError) {
	body := map[string]interface{}{
		"error":            err.Error(),
		"code":             "reload_loop_limit",
		"model":            err.Model,
		"attempts":         err.Count,
		"elapsed_seconds":  err.Elapsed.Seconds(),
		"suggestion":       "Reduce tools/prompt size, or save a model profile with larger context_length",
		"profile_endpoint": "/api/v1/cppworker/model-profiles",
	}
	writeJSON(w, http.StatusRequestEntityTooLarge, body)
}

// writeReloadDisabledForToolsResponse — пишет HTTP 413 когда reload отключён
// для tools-запроса (см. inference.go:ReloadDisabledForToolsError).
// Причина: при tools каждая итерация диалога добавляет tool definitions + tool calls
// + tool results в prompt — reload не поможет, prompt будет только расти.
// Вместо reload клиент получает actionable-сообщение с предложением уменьшить
// tools/history или увеличить n_ctx в профиле модели.
//
// Формат ответа:
//
//	{
//	  "error": "RAM fallback reload disabled for tools-request...",
//	  "code": "tools_reload_disabled",
//	  "model": "...",
//	  "suggestion": "Reduce the number of tools, chat history length, or increase n_ctx in the model profile."
//	}
func writeReloadDisabledForToolsResponse(w http.ResponseWriter, err *ReloadDisabledForToolsError) {
	body := map[string]interface{}{
		"error":            err.Error(),
		"code":             "tools_reload_disabled",
		"model":            err.Model,
		"reason":           "each chat iteration adds tool results to context, reload would not help",
		"suggestion":       "Reduce the number of tools, chat history length, or increase n_ctx in the model profile.",
		"profile_endpoint": "/api/v1/cppworker/model-profiles",
	}
	writeJSON(w, http.StatusRequestEntityTooLarge, body)
}

// writePromptExceedsNCtxResponse — пишет HTTP 413 когда prompt (actual_tokens)
// превышает n_ctx даже с учётом минимального n_predict floor (512).
//
// Корневая причина (2026-06-24): Cline на gemma-4-E4B-it-Q4_K_M получал пустой
// ответ с done_reason="stop", потому что clampNPredictToFitContext молча клампил
// n_predict до 512 → модель эмитила только <end_of_turn> и завершалась.
//
// После фикса (2026-06-24): clampNPredictToFitContext возвращает
// *PromptExceedsNCtxError, и этот handler транслирует его в HTTP 413
// с actionable-сообщением: "увеличь n_ctx или уменьши history/tools/system prompt".
//
// Формат ответа:
//
//	{
//	  "error": "prompt exceeds n_ctx even with minimum n_predict floor: ...",
//	  "code": "prompt_exceeds_n_ctx",
//	  "model": "gemma-4-E4B-it-Q4_K_M",
//	  "actual_prompt_tokens": 9391,
//	  "n_ctx": 8196,
//	  "deficit_tokens": 1708,
//	  "min_n_predict_floor": 512,
//	  "suggestion": "Increase n_ctx (save model profile with larger context_length and reload), or reduce conversation history / tools[] / system prompt",
//	  "profile_endpoint": "/api/v1/cppworker/model-profiles"
//	}
//
// HTTP 413 — payload too large (стандартный код для "request не помещается в ресурсы").
// handleInferenceError — единая точка обработки ошибок инференса.
// Возвращает true, если ошибка была обработана (handler должен return).
// Поддерживает:
//   - *PromptExceedsNCtxError → HTTP 413 (prompt>n_ctx даже с min floor)
//   - *ReloadLoopLimitError → HTTP 413 (reload-cycle-limit)
//   - *ReloadDisabledForToolsError → HTTP 413 (tools-запрос с большим prompt)
//
// Любая другая ошибка: возвращает false, caller сам решает что делать
// (как правило, writeCppWorkerErrorWithBridgeInfo).
func handleInferenceError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var perr *PromptExceedsNCtxError
	if errors.As(err, &perr) {
		writePromptExceedsNCtxResponse(w, perr)
		return true
	}
	var rllErr *ReloadLoopLimitError
	if errors.As(err, &rllErr) {
		writeReloadLoopLimitResponse(w, rllErr)
		return true
	}
	var rdtErr *ReloadDisabledForToolsError
	if errors.As(err, &rdtErr) {
		writeReloadDisabledForToolsResponse(w, rdtErr)
		return true
	}
	// 2026-06-25: AutoTuneNCtx exhausted (n_ctx не помещается даже cpu-only + mmap в RAM).
	// Транслируем в HTTP 413 + InsufficientResourcesResponse со всеми details.
	var atErr *AutoTuneError
	if errors.As(err, &atErr) {
		writeInsufficientResourcesResponse(w, &InsufficientResourcesError{
			Model:             atErr.Model,
			RequestedNCtx:     atErr.RequestedNCtx,
			MaxViableNCtx:     atErr.MaxViableNCtx,
			AvailableRAMMB:    atErr.AvailableRAMMB,
			ModelSizeBytes:    atErr.ModelSizeBytes,
			GPULayersAttempted: 0, // cpu-only после auto_tune
		})
		return true
	}
	var irErr *InsufficientResourcesError
	if errors.As(err, &irErr) {
		writeInsufficientResourcesResponse(w, irErr)
		return true
	}
	return false
}

// writePromptExceedsNCtxResponse — пишет HTTP 413 когда prompt (actual_tokens)
// превышает n_ctx даже с учётом минимального n_predict floor (512).
//
// Корневая причина (2026-06-24): Cline на gemma-4-E4B-it-Q4_K_M получал пустой
// ответ с done_reason="stop", потому что clampNPredictToFitContext молча клампил
// n_predict до 512 → модель эмитила только <end_of_turn> и завершалась.
//
// После фикса (2026-06-24): clampNPredictToFitContext возвращает
// *PromptExceedsNCtxError, и этот handler транслирует его в HTTP 413
// с actionable-сообщением: "увеличь n_ctx или уменьши history/tools/system prompt".
//
// ВАЖНО (2026-06-24): чтобы балансировщик мог перехватить этот ответ и
// выполнить auto-reload (см. internal/balancer/llamacpp_error.go:ParseCppWorkerError),
// в JSON-ответе ОБЯЗАТЕЛЬНО должны быть top-level "code" (int = bridge.ErrCodePromptTooLong = 3)
// и "bridge_info" с полями current_n_ctx / required_n_ctx / actual_tokens / n_predict.
// Балансировщик по error code 3 вызывает NCtxReloadCoordinator.DecideReloadBackend,
// который reload'ит модель на n_ctx = required и повторяет запрос. Без этих полей
// балансировщик не сможет выполнить auto-reload и просто пробросит 413 клиенту.
//
// Формат ответа:
//
//	{
//	  "error": "prompt exceeds n_ctx even with minimum n_predict floor: ...",
//	  "code": 3,
//	  "code_str": "prompt_exceeds_n_ctx",
//	  "bridge_info": {
//	    "code": 3,
//	    "current_n_ctx": 8196,
//	    "required_n_ctx": 9391,
//	    "actual_tokens": 9391,
//	    "n_predict": 512,
//	    "n_ctx_override": 8196,
//	    "max_vram_n_ctx": 131072,
//	    "message": "..."
//	  },
//	  "model": "gemma-4-E4B-it-Q4_K_M",
//	  "deficit_tokens": 1708,
//	  "min_n_predict_floor": 512,
//	  "suggestion": "..."
//	}
//
// HTTP 413 — payload too large.
func writePromptExceedsNCtxResponse(w http.ResponseWriter, err *PromptExceedsNCtxError) {
	deficit := err.ActualTokens + err.MinNPredictFloor + 1 - err.NCtx
	required := err.ActualTokens + err.MinNPredictFloor + 1

	// required_n_ctx = actual_tokens + min_floor + 1, roundUpPow2.
	requiredRoundUp := roundUpPow2Local(required)

	// currentNCtxOverride (n_ctx_override) — что клиент прислал в options.num_ctx.
	// 0, если не задан. Берём из err (если есть) или 0.
	currentOverride := err.NCtxOverride

	body := map[string]interface{}{
		"error":                err.Error(),
		"code":                 bridge.ErrCodePromptTooLong, // int=3, чтобы балансер сработал
		"code_str":             "prompt_exceeds_n_ctx",
		"bridge_info": map[string]interface{}{
			"code":           bridge.ErrCodePromptTooLong,
			"current_n_ctx":  err.NCtx,
			"required_n_ctx": requiredRoundUp,
			"actual_tokens":  err.ActualTokens,
			"n_predict":      err.MinNPredictFloor, // min floor, который пытались использовать
			"n_ctx_override": currentOverride,
			"max_vram_n_ctx": err.MaxVRAMNCtx, // 0 если неизвестно — пусть балансер применит fallback
			"message":        err.Error(),
		},
		"model":                err.ModelName,
		"actual_prompt_tokens": err.ActualTokens,
		"n_ctx":                err.NCtx,
		"deficit_tokens":       deficit,
		"min_n_predict_floor":  err.MinNPredictFloor,
		"requested_n_predict":  err.RequestedNPredict,
		"suggestion":           "Increase n_ctx (save model profile with larger context_length and reload via POST /api/v1/cppworker/model-profiles/{name}/apply), or reduce conversation history / tools[] / system prompt",
		"profile_endpoint":     "/api/v1/cppworker/model-profiles",
	}
	writeJSON(w, http.StatusRequestEntityTooLarge, body)
}

// roundUpPow2Local — локальная копия roundUpPow2 для использования в writePromptExceedsNCtxResponse
// (не импортируем internal/balancer — это приведёт к циклической зависимости).
func roundUpPow2Local(v int) int {
	if v <= 0 {
		return 512
	}
	if v < 512 {
		return 512
	}
	p := 1
	for p < v {
		p <<= 1
	}
	return p
}

// ============================================================
// InsufficientResourcesError — недостаточно VRAM и RAM для загрузки модели
// с запрошенным n_ctx, даже после каскада (RAM fallback → partial offload →
// cpu-only + auto_tune n_ctx).
//
// Возвращается из tryRamFallbackReload, когда:
//   1. requested n_ctx не помещается в VRAM с текущим gpu_layers;
//   2. partial offload через mmap в RAM не помог;
//   3. cpu-only (gpu_layers=0) тоже не влезает (либо mmap весов модели
//      не помещается в RAM, либо KV-cache для requested n_ctx не помещается
//      даже в VRAM при cpu-only).
//
// HTTP 413 (как и PromptExceedsNCtxError), но с другим code (6) и bridge_info
// со всеми деталями ресурсов — балансер проксирует клиенту без изменений.
// Клиент (Cline/OpenWebUI) видит actionable JSON и может показать
// пользователю «уменьшите num_ctx / используйте меньшую модель».
//
// 2026-06-25: добавлен как часть каскадного auto-fallback (см. CHANGELOG,
// docs/runbook-tools.md сценарий G, .clinerules Section 4 + 17).
type InsufficientResourcesError struct {
	Model              string
	RequestedNCtx      int
	MaxViableNCtx      int
	AvailableVRAMMB    int64
	AvailableRAMMB     int64
	ModelSizeBytes     int64
	KVCacheRequiredMB  int64
	GPULayersAttempted int
}

// Error — human-readable message (используется в логах и в generic 5xx fallback).
func (e *InsufficientResourcesError) Error() string {
	return fmt.Sprintf(
		"insufficient resources for model %q: requested n_ctx=%d does not fit in "+
			"%d MB VRAM + %d MB RAM (model size %d MB, KV-cache %d MB, gpu_layers=%d). "+
			"Max viable n_ctx: %d. Reduce num_ctx in request or use a smaller model.",
		e.Model, e.RequestedNCtx, e.AvailableVRAMMB, e.AvailableRAMMB,
		e.ModelSizeBytes/(1024*1024), e.KVCacheRequiredMB, e.GPULayersAttempted,
		e.MaxViableNCtx)
}

// writeInsufficientResourcesResponse — HTTP 413 + structured JSON.
//
// Формат совместим с ParseCppWorkerError в балансере:
//   - top-level "code" = 6 (ErrCodeInsufficientResources)
//   - "bridge_info" со всеми details для диагностики
//
// Балансер проксирует этот JSON клиенту как есть (код 6 — не n_ctx-reloadable,
// клиент сам решит, уменьшать n_ctx или нет).
func writeInsufficientResourcesResponse(w http.ResponseWriter, err *InsufficientResourcesError) {
	body := map[string]interface{}{
		"error":   "insufficient_resources",
		"code":    6,
		"message": err.Error(),
		"details": err.Error(),
		"bridge_info": map[string]interface{}{
			"code":                  6,
			"requested_n_ctx":       err.RequestedNCtx,
			"max_viable_n_ctx":      err.MaxViableNCtx,
			"available_vram_mb":     err.AvailableVRAMMB,
			"available_ram_mb":      err.AvailableRAMMB,
			"model_size_mb":         err.ModelSizeBytes / (1024 * 1024),
			"kv_cache_required_mb":  err.KVCacheRequiredMB,
			"gpu_layers_attempted":  err.GPULayersAttempted,
			"model":                 err.Model,
			"suggestion":            "Reduce num_ctx to " + strconv.Itoa(err.MaxViableNCtx) + " or use a smaller model. You can also save a per-model profile via POST /api/v1/cppworker/model-profiles/{name} with smaller contextSize.",
		},
		"http_status": 413,
	}
	writeJSON(w, http.StatusRequestEntityTooLarge, body)
}

// ============================================================
// Хелперы
// ============================================================

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
func isMemorySlotError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "failed to find a memory slot") ||
		strings.Contains(msg, "memory slot") ||
		strings.Contains(msg, "no slot")
}

// maybeRestartOnMemorySlotError — если err связан с memory slot leak.
func maybeRestartOnMemorySlotError(err error, modelName string) {
	if !isMemorySlotError(err) {
		return
	}
	logger.Get().Errorw("CRITICAL: memory slot leak detected in llama.cpp; restarting process to recover",
		"model", modelName, "error", err.Error())
	time.Sleep(200 * time.Millisecond)
	os.Exit(1)
}

// isModelLoadingError — true, если модель сейчас в процессе загрузки.
func isModelLoadingError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "model is loading")
}

// loadingInfoFor — хелпер: возвращает loading-метаданные для UI из ошибки.
func loadingInfoFor(_ error) (elapsedMs int64, retryAfterMs int) {
	return 0, 3000
}

// parseInt парсит строку в int с дефолтным значением.
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

// parseBoolEnv парсит env-значение в bool.
func parseBoolEnv(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "1" || s == "yes" || s == "on"
}

// isFlagSet — возвращает true, если флаг был явно передан.
func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// convertFlashAttn converts flash-attn flag value to FlashAttnType.
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

// runHealthCheck выполняет одноразовый probe на /health и завершает процесс.
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
