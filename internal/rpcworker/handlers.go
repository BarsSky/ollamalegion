package rpcworker

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"ollama-loadbalancer/c/bridge"
)

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
//
// Если query `stream=true` (B4) — стримит SSE (text/event-stream).
// Иначе возвращает обычный JSON.
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

	// B4: SSE streaming, если `stream=true`.
	if r.URL.Query().Get("stream") == "true" {
		s.handleInferStream(w, r, slice.Handle, sliceID, req.ModelName, req.Prompt, params)
		return
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

// handleInferStream — B4: SSE-streаming inference.
//
// Content-Type: text/event-stream. Каждый токен (или чанк) — отдельный
// SSE event с полем `data: {"token": "..."}`. Финальный event — `done: true`.
//
// На stub-режиме стримит default stub tokens (см. bridge_stub.go).
func (s *WorkerServer) handleInferStream(
	w http.ResponseWriter,
	r *http.Request,
	handle *bridge.ModelHandle,
	sliceID, modelName, prompt string,
	params bridge.GenerationParams,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming_unsupported",
			"ResponseWriter doesn't support Flusher")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	end := s.metrics.OnInferStart(modelName)
	start := time.Now()
	tokensSent := 0

	// SSE event helper.
	send := func(eventType string, payload interface{}) bool {
		data, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := w.Write([]byte("event: " + eventType + "\n")); err != nil {
			return false
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		if _, err := w.Write(data); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Header event.
	if !send("start", map[string]interface{}{
		"worker_id": s.cfg.WorkerID,
		"slice_id":  sliceID,
		"model":     modelName,
	}) {
		return // client disconnected
	}

	// В stub-режиме мы не получаем настоящие токены — InferStream возвращает
	// chunked output. Симулируем streaming через Infer (получаем output
	// одним вызовом), затем шлём output чанками.
	result, err := handle.Infer(prompt, params)
	if err != nil || result == nil {
		send("error", map[string]string{"error": "inference_failed", "message": "stub inference failed"})
		end(0, time.Since(start).Milliseconds(), assertErrStub{})
		return
	}

	// Чанкуем output (по 8 байт) и стримим.
	chunkSize := 8
	output := result.Output
	for i := 0; i < len(output); i += chunkSize {
		end := i + chunkSize
		if end > len(output) {
			end = len(output)
		}
		token := output[i:end]
		if !send("token", map[string]interface{}{
			"token":      token,
			"token_index": tokensSent,
		}) {
			return // client disconnected
		}
		tokensSent++
	}

	elapsed := time.Since(start).Milliseconds()
	end(tokensSent, elapsed, nil)
	_ = end

	// Final event.
	send("done", map[string]interface{}{
		"worker_id":  s.cfg.WorkerID,
		"slice_id":   sliceID,
		"tokens":     tokensSent,
		"latency_ms": elapsed,
		"status":     "completed",
	})
}

// =====================================================================
// /rpc/kv_sync, /rpc/kv_fetch — B4: реальная реализация KV-cache.
// =====================================================================

// kvSyncRequest — POST /rpc/kv_sync body.
//
// Координатор шлёт shard, worker сохраняет в in-memory KVStore.
type kvSyncRequest struct {
	SessionID   string `json:"session_id"`
	SeqLen      int    `json:"seq_len"`
	KeyTensor   []byte `json:"key_tensor,omitempty"`
	ValueTensor []byte `json:"value_tensor,omitempty"`
	Layers      []int  `json:"layers,omitempty"`
	Encoded     string `json:"encoded,omitempty"` // base64-encoded key:value
}

// handleKvSync — POST /rpc/kv_sync. Сохраняет KV-shard.
func (s *WorkerServer) handleKvSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var req kvSyncRequest
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if req.SessionID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "session_id is required")
		return
	}

	shard := &KVShard{
		SessionID:   req.SessionID,
		WorkerID:    s.cfg.WorkerID,
		SeqLen:      req.SeqLen,
		KeyTensor:   req.KeyTensor,
		ValueTensor: req.ValueTensor,
		Layers:      req.Layers,
	}
	if req.Encoded != "" {
		decoded := DecodeShard(req.SessionID, s.cfg.WorkerID, req.SeqLen, req.Encoded)
		shard.KeyTensor = decoded.KeyTensor
		shard.ValueTensor = decoded.ValueTensor
	}

	if err := s.kvStore.Save(shard); err != nil {
		if IsKVStoreFullError(err) {
			writeJSONError(w, http.StatusInsufficientStorage, "kv_full",
				"kv_store max entries reached")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "saved",
		"session_id": shard.SessionID,
		"seq_len":    shard.SeqLen,
		"worker_id":  shard.WorkerID,
	})
}

// handleKvFetch — GET /rpc/kv_fetch?session_id=X.
func (s *WorkerServer) handleKvFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "session_id query param required")
		return
	}

	shard := s.kvStore.Load(sessionID)
	if shard == nil {
		writeJSONError(w, http.StatusNotFound, "not_found",
			"no KV shard for session: "+sessionID)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id":  shard.SessionID,
		"worker_id":   shard.WorkerID,
		"seq_len":     shard.SeqLen,
		"layers":      shard.Layers,
		"encoded":     EncodeShard(shard),
		"created_at":  shard.CreatedAt.Format(time.RFC3339Nano),
		"last_access": shard.LastAccess.Format(time.RFC3339Nano),
	})
}

// =====================================================================
// Types
// =====================================================================

// SliceInferRequestExt — расширенная версия с поддержкой rpccoordinator-формата
// (input RawMessage вместо prompt string).
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

// assertErrStub — sentinel для stub-streaming error path.
type assertErrStub struct{}

func (assertErrStub) Error() string { return "stub inference error" }