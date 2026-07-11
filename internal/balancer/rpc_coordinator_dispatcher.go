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
	// Phase 8 Session 3.3: lazy init в getOrCreateCircuitBreaker с
	// дефолтами из cbDefaults (или из RpcCoordinatorConfig.CircuitBreaker).
	circuitBreakers   map[string]*rpccoordinator.CircuitBreaker
	circuitBreakersMu sync.RWMutex
	cbDefaults        rpccoordinator.CircuitBreakerConfig

	// requestTimeout / streamTimeout — copy of RpcCoordinatorConfig (Phase 9: read from cfg).
	requestTimeout time.Duration
	streamTimeout  time.Duration

	// failFast — true если FailoverPolicy="fail_fast" (без retry).
	failFast bool

	// authChecker — Phase 8 Session 3.4: опциональная проверка auth перед
	// обработкой request. nil или IsEnabled()=false → пропускаем.
	authChecker AuthChecker
}

// SetCircuitBreakerConfig — устанавливает дефолты для circuit breaker'ов.
// Должно быть вызвано ДО первого request (иначе уже созданные CB останутся
// со старыми defaults). Применяется к getOrCreateCircuitBreaker.
//
// Phase 8 Session 3.3: вызывается из main.go с cfg.Balancing.RpcCoordinator.CircuitBreaker.
func (d *RpcCoordinatorDispatcher) SetCircuitBreakerConfig(cfg rpccoordinator.CircuitBreakerConfig) {
	if d == nil {
		return
	}
	d.circuitBreakersMu.Lock()
	defer d.circuitBreakersMu.Unlock()
	d.cbDefaults = cfg
	logger.Get().Debugw("rpc_coordinator_dispatcher: circuit breaker config updated",
		"failure_threshold", cfg.FailureThreshold,
		"success_threshold", cfg.SuccessThreshold,
		"reset_timeout", cfg.ResetTimeout)
}

// AuthChecker — минимальный interface для проверки auth в dispatcher'е.
// Phase 8 Session 3.4: позволяет избежать циклической зависимости
// balancer → api. Реальная реализация — *api.TokenAuthenticator
// (см. internal/api/auth.go). Interface содержит только то, что
// dispatcher'у нужно: IsEnabled() и Authenticate(r).
//
// Если interface не установлен (nil) или IsEnabled()=false, dispatcher
// не проверяет auth — поведение как в default bundled config.
type AuthChecker interface {
	IsEnabled() bool
	Authenticate(r *http.Request) (bool, string)
}

// SetAuthenticator — устанавливает AuthChecker. Должна вызываться
// ДО первого request. Если checker = nil, auth отключен в dispatcher'е
// (default для тестов и bundled config без auth).
//
// Phase 8 Session 3.4: в main.go вызывается с api.NewTokenAuthenticator
// если cfg.Auth.Enabled.
func (d *RpcCoordinatorDispatcher) SetAuthenticator(checker AuthChecker) {
	if d == nil {
		return
	}
	d.authChecker = checker
	if checker != nil {
		logger.Get().Infow("rpc_coordinator_dispatcher: auth enabled",
			"enabled", checker.IsEnabled())
	} else {
		logger.Get().Infow("rpc_coordinator_dispatcher: auth disabled (nil checker)")
	}
}

// checkAuth — Phase 8 Session 3.4: проверяет auth перед обработкой request.
// Возвращает true если auth passed (или disabled), false если 401.
func (d *RpcCoordinatorDispatcher) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if d == nil || d.authChecker == nil || !d.authChecker.IsEnabled() {
		return true // auth не настроен — пропускаем
	}
	valid, _ := d.authChecker.Authenticate(r)
	if valid {
		return true
	}
	// 401 Unauthorized
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	path := r.URL.Path
	if strings.HasPrefix(path, "/v1/") {
		// OpenAI format
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"message": "Unauthorized: valid API token required",
				"type":    "unauthorized",
			},
		})
	} else {
		// Ollama format
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "Unauthorized: valid API token required",
		})
	}
	logger.Get().Warnw("rpc_coordinator_dispatcher: auth failed",
		"path", path, "method", r.Method, "remote", r.RemoteAddr)
	return false
}

// NewRpcCoordinatorDispatcher создаёт dispatcher поверх coordinator.
// Вызывается из Proxy.initRpcModules() в Phase 9 (cmd/balancer/main.go).
//
// cbConfig — параметры circuit breaker. Если значения = 0, применяются defaults:
//   - FailureThreshold = 5
//   - SuccessThreshold = 1
//   - ResetTimeout     = 30s
//
// Phase 8 Session 3.3: CB per-worker (lazy init в getOrCreateCircuitBreaker).
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
	// Store CB defaults на dispatcher; применяются в getOrCreateCircuitBreaker.
	d.cbDefaults = defaultCBConfig()
	logger.Get().Infow("rpc_coordinator_dispatcher initialized",
		"coordinator_present", coord != nil,
		"cb_failure_threshold", d.cbDefaults.FailureThreshold,
		"cb_reset_timeout", d.cbDefaults.ResetTimeout)
	return d
}

// defaultCBConfig — sensible defaults для circuit breaker.
func defaultCBConfig() rpccoordinator.CircuitBreakerConfig {
	return rpccoordinator.CircuitBreakerConfig{
		FailureThreshold: 5,
		SuccessThreshold: 1,
		ResetTimeout:     30 * time.Second,
	}
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
//   3. (Phase 8.3) Хотя бы один worker для этой модели имеет CB != Open.
//
// Использует State() (без side effects на halfOpenInFlight), а не Allow().
func (d *RpcCoordinatorDispatcher) ShouldRoute(modelName string) bool {
	if d == nil || d.coordinator == nil {
		return false
	}
	if !d.coordinator.HasDistributedModel(modelName) {
		return false
	}
	// Phase 8 Session 3.3: проверяем что хотя бы один worker не Open.
	dm := d.coordinator.GetDistributedModel(modelName)
	if dm == nil {
		return false
	}
	for _, slice := range dm.SliceLayers {
		for _, workerID := range slice.Candidates() {
			if workerID == "" {
				continue
			}
			cb := d.getOrCreateCircuitBreaker(workerID)
			if cb.State() != rpccoordinator.StateOpen {
				return true
			}
		}
	}
	// Все workers Open → fail-fast.
	logger.Get().Warnw("rpc_coordinator: all workers are circuit-broken, refusing model",
		"model", modelName)
	return false
}

// getOrCreateCircuitBreaker returns CircuitBreaker для workerID (lazy).
// Использует d.cbDefaults (настройки из RpcCoordinatorConfig.CircuitBreaker
// или hardcoded defaults).
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
	cfg := d.cbDefaults
	if cfg.FailureThreshold == 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.SuccessThreshold == 0 {
		cfg.SuccessThreshold = 1
	}
	if cfg.ResetTimeout == 0 {
		cfg.ResetTimeout = 30 * time.Second
	}
	cb := rpccoordinator.NewCircuitBreakerWithConfig(cfg)
	d.circuitBreakers[workerID] = cb
	return cb
}

// recordCBFromStats — обновляет circuit breaker для каждого worker'а
// на основе slice stats из InferResponse. Вызывается после coordinator.Infer
// и coordinator.InferStream.
//
// Phase 8 Session 3.3: success → RecordSuccess, failure → RecordFailure.
// Это позволяет breaker'у автоматически skip'ать workers с cascade failures
// (при следующих вызовах ShouldRoute вернёт false для модели).
func (d *RpcCoordinatorDispatcher) recordCBFromStats(sliceStats []rpccoordinator.SliceStat) {
	if d == nil || len(sliceStats) == 0 {
		return
	}
	for _, s := range sliceStats {
		if s.WorkerID == "" {
			continue
		}
		cb := d.getOrCreateCircuitBreaker(s.WorkerID)
		if s.Success {
			cb.RecordSuccess()
		} else {
			cb.RecordFailure()
		}
	}
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
//   3. ShouldRoute(model) — false → 404 (model not distributed) или
//      503 если все workers circuit-broken (Phase 8 Session 3.3)
//   4. stream=true → serveStreaming() (Phase 8 Session 3.2)
//   5. coordinator.Infer(ctx, req)
//   6. recordCBFromStats() — обновляет circuit breaker per worker (Session 3.3)
//   7. Format response by path (Ollama /api/generate, /api/chat; OpenAI
//      /v1/chat/completions, /v1/completions)
//
// Session 3.3: circuit breaker integration per worker slice.
func (d *RpcCoordinatorDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Phase 8 Session 3.4: auth check ДО всего остального.
	// Если auth включен и токен невалиден — 401, request не обрабатывается.
	if !d.checkAuth(w, r) {
		return
	}

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
		// Различаем 2 причины отказа (Phase 8 Session 3.3):
		//   1. Модель не зарегистрирована → 404 model_not_distributed
		//   2. Все workers circuit-broken → 503 all_workers_unhealthy
		if !d.coordinator.HasDistributedModel(env.Model) {
			writeRpcError(w, r.URL.Path, http.StatusNotFound,
				fmt.Sprintf("model %q is not distributed via rpc_coordinator", env.Model),
				"model_not_distributed")
		} else {
			writeRpcError(w, r.URL.Path, http.StatusServiceUnavailable,
				fmt.Sprintf("all workers for model %q are circuit-broken", env.Model),
				"all_workers_unhealthy")
		}
		return
	}

	// Apply timeout.
	ctx := r.Context()
	if d.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.requestTimeout)
		defer cancel()
	}

	// Streaming path — Phase 8 Session 3.2.
	if env.Stream {
		d.serveStreaming(w, r, ctx, env)
		return
	}

	// Non-streaming path.
	prompt := flattenPrompt(&env)
	inferReq := &rpccoordinator.InferRequest{
		ModelName: env.Model,
		Prompt:    prompt,
	}

	logger.Get().Debugw("rpc_coordinator_dispatcher: inferring",
		"path", r.URL.Path, "model", env.Model, "prompt_len", len(prompt))

	resp, err := d.coordinator.Infer(ctx, *inferReq)
	// Phase 8 Session 3.3: record CB state per worker from slice stats.
	// Делаем ДО проверки err (для partial failure stats тоже важно).
	if resp != nil {
		d.recordCBFromStats(resp.SliceStats)
	}
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

// serveStreaming — Phase 8 Session 3.2: SSE passthrough для stream=true requests.
//
// Coordinator.InferStream оркестрирует pipeline с real-time callback'ом.
// Каждый token chunk, прочитанный из worker'а, пробрасывается в эту
// функцию через onToken. Здесь мы форматируем его per endpoint и пишем
// в ResponseWriter как SSE event.
//
// Format per path:
//   - /api/generate, /api/ollama/generate:
//       data: {"model":"...","response":"<token>","done":false}\n\n
//       data: {"model":"...","response":"","done":true,"done_reason":"stop",...}\n\n
//   - /api/chat, /api/ollama/chat:
//       data: {"model":"...","message":{"role":"assistant","content":"<token>"},"done":false}\n\n
//       data: {"model":"...","message":{"role":"assistant","content":""},"done":true,...}\n\n
//   - /v1/chat/completions:
//       data: {"id":"chatcmpl-...","choices":[{"delta":{"content":"<token>"}}]}\n\n
//       data: [DONE]\n\n
//   - /v1/completions:
//       data: {"id":"cmpl-...","choices":[{"text":"<token>"}]}\n\n
//       data: [DONE]\n\n
func (d *RpcCoordinatorDispatcher) serveStreaming(
	w http.ResponseWriter, r *http.Request,
	ctx context.Context, env inferenceRequestEnvelope,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRpcError(w, r.URL.Path, http.StatusInternalServerError,
			"streaming requires http.Flusher support", "streaming_unsupported")
		return
	}

	// SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Per-path event formatter.
	path := r.URL.Path
	isChat := path == "/api/chat" || path == "/api/ollama/chat"
	isOpenAIChat := path == "/v1/chat/completions"
	isOpenAICompletion := path == "/v1/completions"
	completionID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	if isOpenAICompletion {
		completionID = fmt.Sprintf("cmpl-%d", time.Now().UnixNano())
	}

	// sendSSE пишет один event и flush'ит.
	sendSSE := func(payload interface{}) bool {
		data, err := json.Marshal(payload)
		if err != nil {
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

	// Format per-token event.
	formatTokenEvent := func(token string) interface{} {
		switch {
		case isChat:
			return map[string]interface{}{
				"model":   env.Model,
				"message": map[string]string{"role": "assistant", "content": token},
				"done":    false,
			}
		case isOpenAIChat:
			return map[string]interface{}{
				"id":      completionID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   env.Model,
				"choices": []map[string]interface{}{{
					"index": 0,
					"delta": map[string]string{"content": token},
				}},
			}
		case isOpenAICompletion:
			return map[string]interface{}{
				"id":      completionID,
				"object":  "text_completion",
				"created": time.Now().Unix(),
				"model":   env.Model,
				"choices": []map[string]interface{}{{
					"index": 0,
					"text": token,
				}},
			}
		default:
			// /api/generate, /api/ollama/generate
			return map[string]interface{}{
				"model":    env.Model,
				"response": token,
				"done":     false,
			}
		}
	}

	// Format terminal event.
	formatDoneEvent := func(totalMs int64) interface{} {
		switch {
		case isChat:
			return map[string]interface{}{
				"model":          env.Model,
				"message":        map[string]string{"role": "assistant", "content": ""},
				"done":           true,
				"done_reason":    "stop",
				"total_duration": totalMs * int64(time.Millisecond),
			}
		case isOpenAIChat:
			return map[string]interface{}{
				"id":      completionID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   env.Model,
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]string{},
					"finish_reason": "stop",
				}},
			}
		case isOpenAICompletion:
			return map[string]interface{}{
				"id":      completionID,
				"object":  "text_completion",
				"created": time.Now().Unix(),
				"model":   env.Model,
				"choices": []map[string]interface{}{{
					"index":         0,
					"text":          "",
					"finish_reason": "stop",
				}},
			}
		default:
			// /api/generate
			return map[string]interface{}{
				"model":          env.Model,
				"response":       "",
				"done":           true,
				"done_reason":    "stop",
				"total_duration": totalMs * int64(time.Millisecond),
			}
		}
	}

	// Token callback — вызывается из coordinator.InferStream.
	tokenIndex := 0
	onToken := func(token string, _ int) error {
		if !sendSSE(formatTokenEvent(token)) {
			return fmt.Errorf("client disconnected")
		}
		tokenIndex++
		return nil
	}

	// Run streaming pipeline.
	prompt := flattenPrompt(&env)
	inferReq := &rpccoordinator.InferRequest{
		ModelName: env.Model,
		Prompt:    prompt,
	}

	logger.Get().Debugw("rpc_coordinator_dispatcher: streaming",
		"path", path, "model", env.Model, "prompt_len", len(prompt))

	resp, err := d.coordinator.InferStream(ctx, *inferReq, onToken)
	// Phase 8 Session 3.3: record CB state per worker from streaming slice stats.
	if resp != nil {
		d.recordCBFromStats(resp.SliceStats)
	}
	if err != nil {
		// Если streaming уже начался — error event, не меняем status code.
		errEv := map[string]interface{}{
			"error": map[string]string{
				"message": err.Error(),
				"type":    "stream_error",
			},
		}
		_ = sendSSE(errEv)
		// Для OpenAI — финальный [DONE] event.
		if isOpenAIChat || isOpenAICompletion {
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
		}
		return
	}

	// Terminal event.
	if !sendSSE(formatDoneEvent(0)) {
		return // client disconnected
	}
	// Для OpenAI — финальный [DONE] event.
	if isOpenAIChat || isOpenAICompletion {
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}
}

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
