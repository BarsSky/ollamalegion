package rpcworker

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// =====================================================================
// Types
// =====================================================================

// SliceInferRequestExt — расширенная версия с поддержкой rpccoordinator-формата
// (input RawMessage вместо prompt string). WorkerClient шлёт JSON с полем input,
// а rpcworker ожидает prompt — поэтому handler читает оба варианта.
//
// Тесты на worker-овский API шлют prompt, тесты через WorkerClient — input.
type SliceInferRequestExt struct {
	ModelName   string            `json:"model_name"`
	Prompt      string            `json:"prompt"`
	Input       json.RawMessage   `json:"input"`
	SliceID     string            `json:"slice_id"`
	Direction   string            `json:"direction,omitempty"`
	Tokens      int               `json:"tokens,omitempty"`
	Temperature float32           `json:"temperature,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
	SessionID   string            `json:"session_id,omitempty"`
	Stream      bool              `json:"stream,omitempty"`
}

// SliceInferResponse — ответ inference среза.
type SliceInferResponse struct {
	WorkerID   string `json:"worker_id"`
	SliceID    string `json:"slice_id"`
	Output     string `json:"output,omitempty"`
	TokensUsed int    `json:"tokens_used"`
	LatencyMs  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

// KVCacheShard — структура шарда KV-cache (B1 stub, B4 full).
type KVCacheShard struct {
	SeqLen   int    `json:"seq_len"`
	KeyData  []byte `json:"key_data,omitempty"`
	ValData  []byte `json:"val_data,omitempty"`
	WorkerID string `json:"worker_id"`
}

// =====================================================================
// /rpc/health
// =====================================================================

// handleHealth — GET /rpc/health.
//
// Ответ совместим с ожиданиями `WorkerClient.HealthCheck`:
// статус 200 + JSON с полями status, loaded_slices, gpu_info, version.
func (s *WorkerServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}

	slices := s.manager.ListSlices()
	loadedSlices := make([]map[string]interface{}, 0, len(slices))
	for _, sl := range slices {
		loadedSlices = append(loadedSlices, map[string]interface{}{
			"modelName":  sl.ModelName,
			"layers":     sl.Layers,
			"startLayer": sl.StartLayer,
			"endLayer":   sl.EndLayer,
			"loadedAt":   sl.LoadedAt.Format(time.RFC3339Nano),
			"loadMs":     sl.LoadMs,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "ok",
		"worker_id":     s.cfg.WorkerID,
		"version":       s.version,
		"loaded_slices": loadedSlices,
		"gpu_info":      s.detectGPUInfo(),
		"stub_mode":     s.cfg.StubMode,
		"go_version":    runtime.Version(),
		"uptime_s":      int64(time.Since(s.startedAt()).Seconds()),
	})
}

// detectGPUInfo — заглушка для GPU info. В B1 stub всегда возвращает
// информацию об отсутствии GPU.
func (s *WorkerServer) detectGPUInfo() map[string]interface{} {
	return map[string]interface{}{
		"available": false,
		"reason":    "rpcworker B1 stub mode — no GPU detection",
	}
}

// =====================================================================
// /rpc/load
// =====================================================================

// handleLoad — POST /rpc/load.
//
// Тело запроса:
//
//	{ "model_name": "model.gguf", "layers": "1-40" }
//
// Ответ: { "status": "loaded", "model": ..., "layers": ..., "load_ms": ... }
func (s *WorkerServer) handleLoad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}

	var req struct {
		ModelName string `json:"model_name"`
		Layers    string `json:"layers"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.ModelName == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "model_name is required")
		return
	}

	loaded, err := s.manager.LoadSlice(req.ModelName, req.Layers)
	if err != nil {
		s.metrics.OnLoadErr()
		status := http.StatusInternalServerError
		if contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSONError(w, status, "load_failed", err.Error())
		return
	}
	s.metrics.OnLoadOk()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "loaded",
		"model":      loaded.ModelName,
		"layers":     loaded.Layers,
		"startLayer": loaded.StartLayer,
		"endLayer":   loaded.EndLayer,
		"load_ms":    loaded.LoadMs,
		"model_path": loaded.ModelPath,
	})
}

// =====================================================================
// /rpc/unload
// =====================================================================

// handleUnload — POST /rpc/unload. Идемпотентный.
//
// Тело запроса: { "model_name": "model.gguf" }
func (s *WorkerServer) handleUnload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}

	var req struct {
		ModelName string `json:"model_name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.ModelName == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "model_name is required")
		return
	}

	if err := s.manager.UnloadSlice(req.ModelName); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unload_failed", err.Error())
		return
	}
	s.metrics.OnUnload()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "unloaded",
		"model":  req.ModelName,
	})
}

// =====================================================================
// /rpc/infer
// =====================================================================

// handleInfer — POST /rpc/infer.
//
// Поддерживает оба формата:
//   - rpcworker-стиль: { "model_name": "...", "prompt": "..." }
//   - rpccoordinator-стиль (WorkerClient): { "model_name": "...", "input": "..." }
func (s *WorkerServer) handleInfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}

	var req SliceInferRequestExt
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.ModelName == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "model_name is required")
		return
	}

	// Совместимость: если prompt пустой, но есть input (от WorkerClient) —
	// декодируем input как строку.
	if trimSpace(req.Prompt) == "" && len(req.Input) > 0 {
		var s string
		if err := json.Unmarshal(req.Input, &s); err == nil {
			req.Prompt = s
		} else {
			req.Prompt = string(req.Input)
		}
	}
	if trimSpace(req.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "prompt is required (or input)")
		return
	}

	slice := s.manager.GetSlice(req.ModelName)
	if slice == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "model_not_loaded",
			"model not loaded on this worker; call /rpc/load first")
		return
	}
	if slice.Handle == nil {
		writeJSONError(w, http.StatusInternalServerError, "no_handle",
			"loaded slice has no bridge handle")
		return
	}

	params := bridge.DefaultGenerationParams()
	if req.Tokens > 0 {
		params.NPredict = req.Tokens
	}
	if req.Temperature > 0 {
		params.Temperature = req.Temperature
	}

	sliceID := req.SliceID
	if sliceID == "" {
		sliceID = slice.Layers
	}

	end := s.metrics.OnInferStart(req.ModelName)
	start := time.Now()

	var (
		output     string
		tokensUsed int
		err        error
	)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = errPanic
			}
		}()
		result, ierr := slice.Handle.Infer(req.Prompt, params)
		if ierr != nil {
			err = ierr
			return
		}
		if result == nil {
			err = errNilResult
			return
		}
		if result.Status != 0 {
			err = errBridge(result.ErrorMsg)
			return
		}
		output = result.Output
		tokensUsed = slice.Handle.CountTokens(output)
	}()
	elapsed := time.Since(start).Milliseconds()
	end(tokensUsed, elapsed, err)

	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "inference_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, SliceInferResponse{
		WorkerID:   s.cfg.WorkerID,
		SliceID:    sliceID,
		Output:     output,
		TokensUsed: tokensUsed,
		LatencyMs:  elapsed,
	})
}

// =====================================================================
// /rpc/kv_sync, /rpc/kv_fetch
// =====================================================================

// handleKvSync — POST /rpc/kv_sync. B1: 501 not_implemented (stub для B4).
func (s *WorkerServer) handleKvSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]interface{}{
		"error":   "not_implemented",
		"message": "kv_sync endpoint is a stub in B1; full implementation scheduled for B4 (Streaming Pipeline)",
		"phase":   "B1",
		"next":    "B4",
	})
}

// handleKvFetch — GET /rpc/kv_fetch?seq_len=N. B1: 501 not_implemented.
func (s *WorkerServer) handleKvFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]interface{}{
		"error":   "not_implemented",
		"message": "kv_fetch endpoint is a stub in B1; full implementation scheduled for B4 (Streaming Pipeline)",
		"phase":   "B1",
		"next":    "B4",
	})
}

// =====================================================================
// Helpers
// =====================================================================

// contains — тонкая обёртка strings.Contains, чтобы не тянуть strings.
func contains(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// trimSpace — тонкая обёрлка strings.TrimSpace.
func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end {
		c := s[start]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		start++
	}
	for end > start {
		c := s[end-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		end--
	}
	return s[start:end]
}

// errPanic, errNilResult, errBridge — sentinel errors для handleInfer.
var (
	errPanic     = stringError("inference panic")
	errNilResult = stringError("nil result from bridge")
)

// stringError — error из строки без fmt allocation.
type stringError string

func (e stringError) Error() string { return string(e) }

// errBridge — конструктор ошибки из bridge.ErrorMsg.
func errBridge(msg string) error {
	if msg == "" {
		msg = "bridge returned non-zero status"
	}
	return stringError(msg)
}