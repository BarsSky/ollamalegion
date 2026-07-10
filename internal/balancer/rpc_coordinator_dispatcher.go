// rpc_coordinator_dispatcher.go — Phase 8.4: dispatcher skeleton.
//
// RpcCoordinatorDispatcher маршрутизирует inference-запросы через
// ModelCoordinator когда balancer находится в OperatingMode=rpc_coordinator.
//
// Phase 8.4 (Session 1): только тип + базовая структура (IsRpcPath, ShouldRoute,
// InferNonStreaming) + кэш circuit breakers per worker. Полная реализация
// ServeHTTP + streaming — Phase 9.
//
// Использование (Phase 9 — в Proxy.ServeHTTP):
//
//	if IsRpcCoordinatorMode(p.config.Balancing.OperatingMode) {
//	    if p.rpcDispatcher != nil && p.rpcDispatcher.IsRpcPath(r.URL.Path) {
//	        p.rpcDispatcher.ServeHTTP(w, r)
//	        return
//	    }
//	}
package balancer

import (
	"context"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/pkg/logger"
)

// RpcCoordinatorDispatcher routes inference через ModelCoordinator.
//
// Сценарий:
//   1. POST /api/generate (или /api/chat, /v1/chat/completions) приходит в balancer.
//   2. Dispatcher проверяет ShouldRoute(model) — true если модель registered.
//   3. Вызывает coordinator.Infer(ctx, req) — pipeline по workers (RPC).
//   4. Возвращает результат клиенту.
//
// Streaming: см. InferStream (Phase 9).
type RpcCoordinatorDispatcher struct {
	coordinator *rpccoordinator.ModelCoordinator
	proxy       *Proxy

	// circuitBreakers — per-worker CircuitBreaker (Phase 8.4: lazy init).
	// Phase 9 dispatcher вызывает cb.Allow() перед каждым worker request.
	circuitBreakers   map[string]*rpccoordinator.CircuitBreaker
	circuitBreakersMu sync.RWMutex

	// requestTimeout / streamTimeout — copy of RpcCoordinatorConfig (Phase 9).
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
// Phase 8.4: hardcoded list. Phase 9: read from config (allow custom paths).
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
//   3. Circuit breaker для всех workers не Open (если все Open — fallback).
func (d *RpcCoordinatorDispatcher) ShouldRoute(modelName string) bool {
	if d == nil || d.coordinator == nil {
		return false
	}
	if !d.coordinator.HasDistributedModel(modelName) {
		return false
	}
	// TODO (Phase 9): check circuit breaker state per worker.
	// Если все workers Open — fallback на обычный proxy (return false).
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

// InferNonStreaming выполняет non-streaming inference через coordinator.
//
// Phase 8.4: базовая обёртка — parse req, call coordinator.Infer, return response.
// Phase 9: добавит circuit breaker integration, retries, error mapping.
//
// Returns:
//   - *InferResponse с результатом
//   - error: ErrNoWorkersAvailable (503), ErrInferenceFailed (502), ErrContextCanceled
func (d *RpcCoordinatorDispatcher) InferNonStreaming(ctx context.Context, req *rpccoordinator.InferRequest) (*rpccoordinator.InferResponse, error) {
	if d == nil || d.coordinator == nil {
		return nil, ErrDispatcherNotInitialized
	}
	if !d.coordinator.HasDistributedModel(req.ModelName) {
		return nil, ErrModelNotDistributed
	}

	// Apply timeout.
	if d.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.requestTimeout)
		defer cancel()
	}

	logger.Get().Debugw("rpc_coordinator_dispatcher: inferring",
		"model", req.ModelName, "session_id", req.SessionID, "stream", req.Stream)

	resp, err := d.coordinator.Infer(ctx, *req)
	if err != nil {
		logger.Get().Warnw("rpc_coordinator_dispatcher: inference failed",
			"model", req.ModelName, "error", err)
		return nil, err
	}
	return resp, nil
}

// ServeHTTP — Phase 9 (full implementation with streaming + circuit breakers).
// Phase 8.4: stub, возвращает 503 Not Implemented.
//
// Сигнатура: http.Handler interface, регистрируется в Proxy.ServeHTTP.
func (d *RpcCoordinatorDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d == nil || d.coordinator == nil {
		http.Error(w, "rpc_coordinator not initialized", http.StatusServiceUnavailable)
		return
	}
	// Phase 9: read model from body, call ShouldRoute, dispatch to Infer/Stream.
	// Phase 8.4: stub — returns 501.
	http.Error(w,
		"rpc_coordinator dispatcher is not yet fully implemented (Phase 8.4 — Session 1 skeleton); "+
			"will be wired in Phase 9 (Session 2).",
		http.StatusNotImplemented)
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
