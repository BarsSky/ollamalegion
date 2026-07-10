// rpc_coordinator_dispatcher.go — Phase 8: P.1 rpc_coordinator production mode.
//
// RpcCoordinatorDispatcher маршрутизирует inference-запросы через
// ModelCoordinator когда balancer в OperatingMode=rpc_coordinator.
//
// Phase 8 (Session 2 — Step 2 full):
//   - Body parsing (read JSON: model, prompt, messages, stream)
//   - ShouldRoute check (model registered in coordinator?)
//   - Non-streaming inference: coordinator.Infer(ctx, req) → format response
//     по endpoint (Ollama /api/generate, /api/chat; OpenAI /v1/chat/completions,
//     /v1/completions)
//   - Error mapping: 404 (model not distributed), 503 (coordinator disabled /
//     all workers unhealthy), 502 (upstream error), 504 (timeout), 499
//     (client closed request)
//   - Streaming: Phase 9 (not yet implemented — stub returns 501)
//
// Phase 8 (Session 1) skeleton: IsRpcPath, ShouldRoute, InferNonStreaming
// (basic wrapper) + circuit breaker cache. Phase 8 Session 2 заменяет
// stub ServeHTTP на полную реализацию с body parsing + response formatting.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/pkg/logger"
)

// RpcCoordinatorDispatcher routes inference через ModelCoordinator.
type RpcCoordinatorDispatcher struct {
	coordinator *rpccoordinator.ModelCoordinator
	proxy       *Proxy

	// circuitBreakers — per-worker CircuitBreaker (Phase 8.3: для failover).
	// Phase 9: integrate with coordinator.executePipeline() (обёртка для
	// каждого worker slice — Allow() before Infer, RecordSuccess/Failure after).
	circuitBreakers   map[string]*rpccoordinator.CircuitBreaker
	circuitBreakersMu sync.RWMutex

	// requestTimeout / streamTimeout — copy of RpcCoordinatorConfig (Phase 9: read from cfg).
	requestTimeout time.Duration
	streamTimeout  time.Duration

	// failFast — true если FailoverPolicy="fail_fast" (без retry).
	failFast bool
}

// NewRpcCoordinatorDispatcher создаёт dispatcher поверх coordinator.
// Вызывается из Proxy.initRpcModules() в Phase 9 (cmd/balancer/main.go).
func NewRpcCoordinatorDispatcher(coord *rpccoordinator.ModelCoordinator, p *Proxy) *RpcCoordinatorDispatcher {
	if coord == nil {
		logger.Get().Warnw("rpc_coordinator_dispatcher: coordinator is nil, dispatcher will be inactive")
		return &RpcCoordinatorDispatcher{
			coordinator: nil,
			proxy:       p,
		}
	}
	d := &RpcCoordinatorDispatcher{
		coordinator:       coord,
		proxy:             p,
		circuitBreakers:   make(map[string]*rpccoordinator.CircuitBreaker),
		requestTimeout:    30 * time.Second, // default
		streamTimeout:     5 * time.Minute, // default
		failFast:          false,           // default — retry через circuit breaker
	}
	logger.Get().Infow("rpc_coordinator_dispatcher initialized",
		"coordinator_present", coord != nil)
	return d
}

// IsRpcPath returns true если path — один из intercepted inference endpoints.
// Phase 8.2: hardcoded list. Phase 9: read from config (allow custom paths).
func (d *RpcCoordinatorDispatcher) IsRpcPath(path string) bool {
	switch path {
	case "/api/generate", "/api/ollama/generate",
		"/api/chat", "/api/ollama/chat",
		"/v1/chat/completions", "/v1/completions":
		return true
	}
	return false
}

// ShouldRoute returns true если dispatcher должен обработать request для этой
// модели. Используется в Phase 9 ServeHTTP для ранней проверки до чтения body.
//
// Conditions:
//   1. Coordinator инициализирован (не nil).
//   2. Модель зарегистрирована в coordinator (HasDistributedModel).
//   3. (Phase 9) Circuit breaker для всех workers не Open.
func (d *RpcCoordinatorDispatcher) ShouldRoute(modelName string) bool {
	if d == nil || d.coordinator == nil {
		return false
	}
	if !d.coordinator.HasDistributedModel(modelName) {
		return false
	}
	// TODO (Phase 9): check circuit breaker state per worker.
	return true
}

// getOrCreateCircuitBreaker returns CircuitBreaker для workerID (lazy).
func (d *RpcCoordinatorDispatcher) getOrCreateCircuitBreaker(workerID string) *rpccoordinator.CircuitBreaker {
	d.circuitBreakersMu.RLock()
	if cb, ok := d.circuitBreakers[workerID]; ok {
		d.circuitBreakersMu.RUnlock()
		return cb
	}
	d.circuitBreakersMu.RUnlock()

	d.circuitBreakersMu.Lock()
	defer d.circuitBreakersMu.Unlock()
	// Double-check (другой goroutine мог создать).
	if cb, ok := d.circuitBreakers[workerID]; ok {
		return cb
	}
	cb := rpccoordinator.NewCircuitBreaker(5, 30*time.Second)
	d.circuitBreakers[workerID] = cb
	return cb
}

// inferenceRequestEnvelope — общий envelope для парсинга всех inference endpoints.
// Phase 8.2: простой union — все endpoints читаются через одну struct.
type inferenceRequestEnvelope struct {
	// Model — имя модели (required).
	Model string `json:"model"`
	// Prompt — для /api/generate и /v1/completions.
	Prompt string `json:"prompt,omitempty"`
	// Messages — для /api/chat и /v1/chat/completions.
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages,omitempty"`
	// Stream — true для SSE streaming (Phase 9).
	Stream bool `json:"stream"`
}

// flattenPrompt — объединяет messages в single prompt для coordinator.
// Phase 8.2: простой формат "role: content\n".
// Phase 9: использовать chat template из coordinator.GetModel.
func flattenPrompt(env *inferenceRequestEnvelope) string {
	if env.Prompt != "" {
		return env.Prompt
	}
	var sb strings.Builder
	for _, m := range env.Messages {
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

// ServeHTTP — Phase 8 Session 2 full implementation (non-streaming).
//
// Flow:
//   1. Read body
//   2. Parse JSON envelope
//   3. ShouldRoute(model) — false → 404 (model not distributed)
//   4. stream=true → 501 (Phase 9 deferred)
//   5. coordinator.Infer(ctx, req)
//   6. Format response by path (Ollama /api/generate, /api/chat; OpenAI
//      /v1/chat/completions, /v1/completions)
//
// Streaming + circuit breaker integration: Phase 9.
func (d *RpcCoordinatorDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d == nil || d.coordinator == nil {
		writeRpcError(w, r.URL.Path, http.StatusServiceUnavailable,
			"rpc_coordinator not initialized", "coordinator_disabled")
		return
	}

	// Read body (must be read once; dispatcher owns the body).
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeRpcError(w, r.URL.Path, http.StatusBadRequest,
			"read body: "+err.Error(), "body_read_error")
		return
	}
	r.Body.Close()

	// Parse envelope.
	var env inferenceRequestEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		writeRpcError(w, r.URL.Path, http.StatusBadRequest,
			"parse JSON: "+err.Error(), "json_parse_error")
		return
	}

	if env.Model == "" {
		writeRpcError(w, r.URL.Path, http.StatusBadRequest,
			"model is required", "missing_model")
		return
	}

	// ShouldRoute check.
	if !d.ShouldRoute(env.Model) {
		writeRpcError(w, r.URL.Path, http.StatusNotFound,
			fmt.Sprintf("model %q is not distributed via rpc_coordinator", env.Model),
			"model_not_distributed")
		return
	}

	// Streaming — Phase 9.
	if env.Stream {
		writeRpcError(w, r.URL.Path, http.StatusNotImplemented,
			"streaming not yet implemented in rpc_coordinator dispatcher (Phase 9)",
			"streaming_not_implemented")
		return
	}

	// Build InferRequest.
	prompt := flattenPrompt(&env)
	inferReq := &rpccoordinator.InferRequest{
		ModelName: env.Model,
		Prompt:    prompt,
	}

	// Apply timeout.
	ctx := r.Context()
	if d.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.requestTimeout)
		defer cancel()
	}

	logger.Get().Debugw("rpc_coordinator_dispatcher: inferring",
		"path", r.URL.Path, "model", env.Model, "prompt_len", len(prompt))

	resp, err := d.coordinator.Infer(ctx, *inferReq)
	if err != nil {
		d.handleInferError(w, r, err)
		return
	}

	// Format response.
	d.writeSuccessResponse(w, r.URL.Path, env.Model, resp)
}

// handleInferError maps coordinator.Infer errors to HTTP responses.
func (d *RpcCoordinatorDispatcher) handleInferError(w http.ResponseWriter, r *http.Request, err error) {
	path := r.URL.Path
	errStr := err.Error()

	// Context errors first.
	if err == context.DeadlineExceeded {
		writeRpcError(w, path, http.StatusGatewayTimeout,
			"inference timeout: "+errStr, "timeout")
		return
	}
	if err == context.Canceled {
		writeRpcError(w, path, 499, // Client Closed Request (nginx convention)
			"client closed request", "client_disconnected")
		return
	}

	// Coordinator-specific errors (by message).
	switch {
	case strings.Contains(errStr, "rpc coordinator is disabled"):
		writeRpcError(w, path, http.StatusServiceUnavailable, errStr, "coordinator_disabled")
	case strings.Contains(errStr, "distributed model") && strings.Contains(errStr, "not found"):
		writeRpcError(w, path, http.StatusNotFound, errStr, "model_not_distributed")
	case strings.Contains(errStr, "selector for slice"):
		writeRpcError(w, path, http.StatusServiceUnavailable, errStr, "no_workers_available")
	case strings.Contains(errStr, "all candidates failed") || strings.Contains(errStr, "no healthy"):
		writeRpcError(w, path, http.StatusServiceUnavailable, errStr, "all_workers_unhealthy")
	default:
		// Generic upstream error.
		writeRpcError(w, path, http.StatusBadGateway,
			"upstream error: "+errStr, "upstream_error")
	}
}

// writeSuccessResponse — formats response по endpoint.
func (d *RpcCoordinatorDispatcher) writeSuccessResponse(w http.ResponseWriter, path, model string, resp *rpccoordinator.InferResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	now := time.Now().Unix()
	totalNs := resp.TotalMs * int64(time.Millisecond)

	switch path {
	case "/api/generate", "/api/ollama/generate":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model":         model,
			"response":      resp.Output,
			"done":          true,
			"done_reason":   "stop",
			"context":       []int{},
			"total_duration": totalNs,
		})
	case "/api/chat", "/api/ollama/chat":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model":       model,
			"message":     map[string]string{"role": "assistant", "content": resp.Output},
			"done":        true,
			"done_reason": "stop",
			"total_duration": totalNs,
		})
	case "/v1/chat/completions":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      fmt.Sprintf("chatcmpl-%d", now),
			"object":  "chat.completion",
			"created": now,
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": resp.Output},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{
				// TODO (Phase 9): token counts from coordinator (currently 0).
				"prompt_tokens":     0,
				"completion_tokens": 0,
				"total_tokens":      0,
			},
		})
	case "/v1/completions":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      fmt.Sprintf("cmpl-%d", now),
			"object":  "text_completion",
			"created": now,
			"model":   model,
			"choices": []map[string]interface{}{{
				"text":         resp.Output,
				"index":        0,
				"finish_reason": "stop",
			}},
		})
	default:
		// Should not happen (IsRpcPath filters). Pass-through raw response.
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// InferNonStreaming — Phase 8.4 backward-compat wrapper (deprecated в Session 2).
// Use ServeHTTP for HTTP integration.
func (d *RpcCoordinatorDispatcher) InferNonStreaming(ctx context.Context, req *rpccoordinator.InferRequest) (*rpccoordinator.InferResponse, error) {
	if d == nil || d.coordinator == nil {
		return nil, ErrDispatcherNotInitialized
	}
	if !d.coordinator.HasDistributedModel(req.ModelName) {
		return nil, ErrModelNotDistributed
	}
	if d.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.requestTimeout)
		defer cancel()
	}
	return d.coordinator.Infer(ctx, *req)
}

// Errors returned by Dispatcher.
var (
	// ErrDispatcherNotInitialized — coordinator == nil.
	ErrDispatcherNotInitialized = dispatcherError("rpc_coordinator dispatcher not initialized")
	// ErrModelNotDistributed — модель не зарегистрирована в coordinator.
	ErrModelNotDistributed = dispatcherError("model is not distributed via rpc_coordinator")
)

// dispatcherError — typed error для dispatcher's specific errors.
type dispatcherError string

func (e dispatcherError) Error() string { return string(e) }

// writeRpcError writes error response. Format depends on path (Ollama vs OpenAI).
// Both have "error" field; Ollama has it as string, OpenAI as object with message+type.
func writeRpcError(w http.ResponseWriter, path string, status int, msg, errorType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if strings.HasPrefix(path, "/v1/") {
		// OpenAI format.
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"message": msg,
				"type":    errorType,
			},
		})
		return
	}
	// Ollama format.
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
