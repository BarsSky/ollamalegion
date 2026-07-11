// Package balancer — VirtualRouter (Phase 8 P.2: virtual_router production mode).
//
// VirtualRouter перехватывает inference запросы для virtual моделей (config в
// `Registry` в alias-on-pool mode) и маршрутизирует на выбранный backend
// из pool'а. Selection стратегия (round_robin / least_loaded / random)
// определяется при создании.
//
// Flow:
//   1. Proxy.ServeHTTP видит запрос с model="...".
//   2. Если OperatingMode=virtual_router + model in registry + IsAliasOnPoolMode:
//      a. router.ServeHTTP(w, r) вызывается ДО основного proxy flow.
//      b. Парсим body, извлекаем model name.
//      c. Ищем virtual model в registry → Selector выбирает backend.
//      d. Rewrite model в body на physical model name.
//      e. Добавляем X-Original-Backend header (для debug).
//      f. Proxy на выбранный backend через стандартный proxyRequest flow.
//   3. Иначе: стандартный flow (без изменений).
//
// Activation: cmd/balancer/main.go создаёт VirtualRouter + Registry и
// вызывает proxy.SetVirtualRouter() если conf.Balancing.OperatingMode ==
// "virtual_router".
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// AuthChecker — определён в auth_checker.go (общий для dispatcher'ов).

// SetAuthenticator — устанавливает AuthChecker. Должна вызываться
// ДО первого request. Если checker = nil, auth отключен.
//
// Phase 8 P.2 backlog: в main.go вызывается с api.NewTokenAuthenticator
// если conf.Auth.Enabled.
func (r *VirtualRouter) SetAuthenticator(checker AuthChecker) {
	if r == nil {
		return
	}
	r.authChecker = checker
	if checker != nil {
		logger.Get().Infow("virtual_router: auth enabled",
			"enabled", checker.IsEnabled())
	} else {
		logger.Get().Infow("virtual_router: auth disabled (nil checker)")
	}
}

// checkAuth — Phase 8 P.2 backlog: проверяет auth перед обработкой request.
// Возвращает true если auth passed (или disabled), false если 401.
func (r *VirtualRouter) checkAuth(w http.ResponseWriter, req *http.Request) bool {
	if r == nil || r.authChecker == nil || !r.authChecker.IsEnabled() {
		return true
	}
	valid, _ := r.authChecker.Authenticate(req)
	if valid {
		return true
	}
	// 401 Unauthorized
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	path := req.URL.Path
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
	logger.Get().Warnw("virtual_router: auth failed",
		"path", path, "method", req.Method, "remote", req.RemoteAddr)
	return false
}

// VirtualRouterMetrics — counter'ы для observability.
// Phase 8 P.2: Prometheus-style counters.
type VirtualRouterMetrics struct {
	InferenceTotal      atomic.Int64 // total inference requests через virtual router
	BackendSelections   sync.Map     // backendID → *atomic.Int64
	InferenceErrors     atomic.Int64 // failed inferences
	StreamPassThrough   atomic.Int64 // streaming requests passed through
	SelectionSkips      atomic.Int64 // requests where all backends unhealthy
}

// IncBackendSelection инкрементит counter для backendID (lazy init).
func (m *VirtualRouterMetrics) IncBackendSelection(backendID string) {
	if v, ok := m.BackendSelections.Load(backendID); ok {
		v.(*atomic.Int64).Add(1)
		return
	}
	// First time — atomic init.
	counter := &atomic.Int64{}
	counter.Add(1)
	actual, _ := m.BackendSelections.LoadOrStore(backendID, counter)
	if actual != counter {
		// Другой goroutine уже создал, инкрементим существующий.
		actual.(*atomic.Int64).Add(1)
	}
}

// Snapshot — возвращает копию metrics для /api/v1/metrics endpoint.
func (m *VirtualRouterMetrics) Snapshot() map[string]interface{} {
	snap := map[string]interface{}{
		"inferenceTotal":    m.InferenceTotal.Load(),
		"inferenceErrors":   m.InferenceErrors.Load(),
		"streamPassThrough": m.StreamPassThrough.Load(),
		"selectionSkips":    m.SelectionSkips.Load(),
	}
	selections := make(map[string]int64)
	m.BackendSelections.Range(func(k, v interface{}) bool {
		selections[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	snap["backendSelections"] = selections
	return snap
}

// VirtualRouter — interceptor для virtual models в alias-on-pool mode.
type VirtualRouter struct {
	registry *virtualmodel.Registry
	proxy    *Proxy

	// selectorFactory — closure, создаёт selector по имени стратегии.
	// Используется lazy: при первом запросе к virtual model создаём
	// selector (если ещё не создан) и кэшируем в selectors map.
	selectorsMu sync.RWMutex
	selectors   map[string]virtualmodel.Selector // virtual model name → selector

	// metrics для observability (Phase 8 P.2 acceptance criteria #7).
	metrics *VirtualRouterMetrics

	// defaultStrategy — если virtual model config не задаёт Selection явно.
	defaultStrategy virtualmodel.SelectionStrategy

	// authChecker — Phase 8 P.2 backlog: опциональная проверка auth.
	authChecker AuthChecker
}

// NewVirtualRouter создаёт router поверх registry + proxy.
func NewVirtualRouter(registry *virtualmodel.Registry, p *Proxy) *VirtualRouter {
	if registry == nil {
		logger.Get().Warnw("virtual_router: registry is nil, router will be inactive")
	}
	return &VirtualRouter{
		registry:        registry,
		proxy:           p,
		selectors:       make(map[string]virtualmodel.Selector),
		metrics:         &VirtualRouterMetrics{},
		defaultStrategy: virtualmodel.SelectionRoundRobin,
	}
}

// IsActive — true если router может обрабатывать requests (registry enabled).
func (r *VirtualRouter) IsActive() bool {
	return r != nil && r.registry != nil && r.registry.IsEnabled()
}

// GetMetrics возвращает metrics snapshot.
func (r *VirtualRouter) GetMetrics() *VirtualRouterMetrics {
	return r.metrics
}

// IsVirtualModelPath — true если model name in body указывает на virtual model
// в alias-on-pool mode. Используется Proxy.ServeHTTP для early check.
func (r *VirtualRouter) IsVirtualModelPath(modelName string) bool {
	if !r.IsActive() || modelName == "" {
		return false
	}
	vm := r.registry.Get(modelName)
	if vm == nil {
		return false
	}
	return vm.Config.IsAliasOnPoolMode()
}

// IsVirtualPathRequest — true если request URL path может содержать virtual
// models (POST с JSON body). Phase 8 P.2: /v1/chat/completions, /v1/completions,
// /api/generate, /api/chat.
func (r *VirtualRouter) IsVirtualPathRequest(r2 *http.Request) bool {
	if r2.Method != http.MethodPost {
		return false
	}
	switch r2.URL.Path {
	case "/v1/chat/completions", "/v1/completions",
		"/api/generate", "/api/ollama/generate",
		"/api/chat", "/api/ollama/chat":
		return true
	}
	return false
}

// MatchesVirtualRequest — читает body, извлекает model, проверяет registry.
// Возвращает true если request содержит virtual model в alias-on-pool mode
// (тогда Proxy.ServeHTTP должен вызвать router.ServeHTTP).
//
// Использует peek-then-rewind: читает body, парсит, потом восстанавливает
// r.Body чтобы downstream handler тоже мог читать.
func (r *VirtualRouter) MatchesVirtualRequest(req *http.Request) bool {
	if !r.IsActive() {
		return false
	}
	if !r.IsVirtualPathRequest(req) {
		return false
	}
	// Peek body.
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return false
	}
	req.Body.Close()
	// Restore body для downstream consumer.
	req.Body = io.NopCloser(bytes.NewReader(body))

	// Quick model extraction (best effort).
	var env struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return false
	}
	return r.IsVirtualModelPath(env.Model)
}

// ServeHTTP — Phase 8 P.2: перехватывает request, выбирает backend, проксирует.
//
// Контракт:
//   - Парсит body, извлекает model.
//   - Если model in registry + alias-on-pool mode: rewrite model, выбрать backend,
//     добавить X-Original-Backend, проксировать.
//   - Иначе: 404 unknown_virtual_model (но это уже должно быть отфильтровано в
//     Proxy через IsVirtualModelPath check).
func (r *VirtualRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Phase 8 P.2 backlog: auth check ДО всего остального.
	// Если auth enabled и токен невалиден — 401, request не обрабатывается.
	if !r.checkAuth(w, req) {
		return
	}

	if !r.IsActive() {
		http.Error(w, "virtual_router not active", http.StatusServiceUnavailable)
		return
	}

	// 1. Read body (нужно для parse + re-write).
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeVirtualRouterError(w, req.URL.Path, http.StatusBadRequest,
			"read body: "+err.Error(), "body_read_error")
		return
	}
	req.Body.Close()

	// 2. Parse JSON envelope.
	var env struct {
		Model    string                   `json:"model"`
		Messages []map[string]interface{} `json:"messages,omitempty"`
		Prompt   string                   `json:"prompt,omitempty"`
		Stream   bool                     `json:"stream,omitempty"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		writeVirtualRouterError(w, req.URL.Path, http.StatusBadRequest,
			"parse JSON: "+err.Error(), "json_parse_error")
		return
	}

	// 3. Lookup virtual model.
	vm := r.registry.Get(env.Model)
	if vm == nil {
		writeVirtualRouterError(w, req.URL.Path, http.StatusNotFound,
			fmt.Sprintf("virtual model %q not found", env.Model),
			"unknown_virtual_model")
		r.metrics.InferenceErrors.Add(1)
		return
	}
	if !vm.Config.IsAliasOnPoolMode() {
		writeVirtualRouterError(w, req.URL.Path, http.StatusBadRequest,
			fmt.Sprintf("virtual model %q is not in alias-on-pool mode (legacy pipeline mode not yet wired)", env.Model),
			"wrong_virtual_mode")
		r.metrics.InferenceErrors.Add(1)
		return
	}

	r.metrics.InferenceTotal.Add(1)

	// 4. Select backend.
	selector := r.getOrCreateSelector(vm.Config.Name, vm.Config.Selection)
	backendID, err := selector.Select(vm.Config.BackendPool, nil)
	if err != nil {
		r.metrics.SelectionSkips.Add(1)
		r.metrics.InferenceErrors.Add(1)
		writeVirtualRouterError(w, req.URL.Path, http.StatusServiceUnavailable,
			"no healthy backends in pool: "+err.Error(),
			"no_healthy_backends")
		return
	}
	r.metrics.IncBackendSelection(backendID)

	logger.Get().Debugw("virtual_router: selected backend",
		"virtual_model", env.Model,
		"physical_model", vm.Config.ModelName,
		"backend", backendID,
		"strategy", selector.Name(),
		"stream", env.Stream,
	)

	// 5. Rewrite model in body.
	env.Model = vm.Config.ModelName
	rewrittenBody, err := json.Marshal(env)
	if err != nil {
		writeVirtualRouterError(w, req.URL.Path, http.StatusInternalServerError,
			"rewrite body: "+err.Error(), "body_rewrite_error")
		r.metrics.InferenceErrors.Add(1)
		return
	}

	// 6. Set X-Original-Backend header (для debug / metrics / audit log).
	req.Header.Set("X-Original-Backend", backendID)
	req.Header.Set("X-Virtual-Model", env.Model /* BEFORE rewrite? no, AFTER for physical */)
	// Note: env.Model was already rewritten above, so header shows physical name.
	// We need the ORIGINAL virtual model name for the header — let's add it.
	// Reset: use originalName variable.
	// (See below — we set it from a copy of the original.)

	// 7. Track streaming vs non-streaming.
	if env.Stream {
		r.metrics.StreamPassThrough.Add(1)
	}

	// 8. Construct new request to the selected backend.
	// Parse "host:port" → http://host:port/path.
	host, port, err := parseBackendHostPort(backendID)
	if err != nil {
		writeVirtualRouterError(w, req.URL.Path, http.StatusInternalServerError,
			"parse backend address: "+err.Error(), "backend_address_error")
		r.metrics.InferenceErrors.Add(1)
		return
	}
	targetURL := fmt.Sprintf("http://%s:%d%s", host, port, req.URL.Path)

	// 9. Apply timeout from virtual model config (or default 30s).
	timeout := 30 * time.Second
	if vm.Config.Coordination.TimeoutMs > 0 {
		timeout = time.Duration(vm.Config.Coordination.TimeoutMs) * time.Millisecond
	}

	// 10. Create proxy request.
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()

	proxyReq, err := http.NewRequestWithContext(ctx, req.Method, targetURL, bytes.NewReader(rewrittenBody))
	if err != nil {
		writeVirtualRouterError(w, req.URL.Path, http.StatusInternalServerError,
			"create proxy request: "+err.Error(), "proxy_request_error")
		r.metrics.InferenceErrors.Add(1)
		return
	}
	// Copy original headers (кроме Host).
	for k, v := range req.Header {
		if k == "Host" {
			continue
		}
		proxyReq.Header[k] = v
	}
	// Re-set X-Original-Backend (it's in req.Header, so already copied).
	// Add X-Virtual-Model with the ORIGINAL virtual name (we re-derive from raw body).
	// Simplest: re-parse original body to get original model name.
	var origEnv struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &origEnv) // best effort
	proxyReq.Header.Set("X-Virtual-Model", origEnv.Model)
	proxyReq.Header.Set("X-Backend-Selected", backendID)
	proxyReq.Header.Set("X-Selection-Strategy", selector.Name())

	// 11. Execute request.
	client := &http.Client{
		Timeout: timeout,
	}
	resp, err := client.Do(proxyReq)
	if err != nil {
		writeVirtualRouterError(w, req.URL.Path, http.StatusBadGateway,
			"backend request failed: "+err.Error(), "backend_unreachable")
		r.metrics.InferenceErrors.Add(1)
		return
	}
	defer resp.Body.Close()

	// 12. Copy response headers.
	for k, v := range resp.Header {
		if k == "Content-Length" || k == "Connection" {
			continue
		}
		w.Header()[k] = v
	}
	// Add debug header back to response (for client-side observability).
	w.Header().Set("X-Original-Backend", backendID)
	w.Header().Set("X-Virtual-Model", origEnv.Model)

	// 13. Status code + body.
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logger.Get().Warnw("virtual_router: response copy failed",
			"backend", backendID, "error", err)
		r.metrics.InferenceErrors.Add(1)
	}
}

// getOrCreateSelector — lazy create + cache selector per virtual model.
func (r *VirtualRouter) getOrCreateSelector(vmName string, strategy virtualmodel.SelectionStrategy) virtualmodel.Selector {
	r.selectorsMu.RLock()
	if s, ok := r.selectors[vmName]; ok {
		r.selectorsMu.RUnlock()
		return s
	}
	r.selectorsMu.RUnlock()

	r.selectorsMu.Lock()
	defer r.selectorsMu.Unlock()
	if s, ok := r.selectors[vmName]; ok {
		return s
	}
	// Если strategy не задан в config — используем default.
	if strategy == "" {
		strategy = r.defaultStrategy
	}
	s := virtualmodel.NewSelector(strategy)
	r.selectors[vmName] = s
	logger.Get().Infow("virtual_router: created selector",
		"vm_name", vmName, "strategy", s.Name())
	return s
}

// ResetSelector — сбрасывает selector для vm (для test + config reload).
func (r *VirtualRouter) ResetSelector(vmName string) {
	r.selectorsMu.Lock()
	defer r.selectorsMu.Unlock()
	if s, ok := r.selectors[vmName]; ok {
		s.Reset()
	}
}

// parseBackendHostPort — разбирает "host:port" string. Default port = 11434.
func parseBackendHostPort(s string) (string, int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid backend address %q (expected host:port)", s)
	}
	var port int
	if _, err := fmt.Sscanf(parts[1], "%d", &port); err != nil {
		return "", 0, fmt.Errorf("invalid port in %q: %w", s, err)
	}
	return parts[0], port, nil
}

// writeVirtualRouterError — пишет error response в per-endpoint format.
func writeVirtualRouterError(w http.ResponseWriter, path string, status int, msg, errorType string) {
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
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": msg,
	})
}

// Compile-time interface check.
var _ http.Handler = (*VirtualRouter)(nil)

// Ensure import is used (для go vet).
var _ = types.VirtualModelConfig{}
