package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// Test harness: 2-3 fake backends + VirtualRouter
// =====================================================================

type fakeBackend struct {
	server   *httptest.Server
	host     string
	port     int
	calls    atomic.Int64
	lastBody []byte
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	fb := &fakeBackend{}
	fb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		fb.lastBody = body

		// Echo back: detect stream vs non-stream.
		var env struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &env)

		if env.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// 3 SSE events.
			fmt.Fprintf(w, "data: {\"model\":\"%s\",\"choices\":[{\"delta\":{\"content\":\"chunk1\"}}]}\n\n", env.Model)
			fmt.Fprintf(w, "data: {\"model\":\"%s\",\"choices\":[{\"delta\":{\"content\":\"chunk2\"}}]}\n\n", env.Model)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model":  env.Model,
			"answer": "echo: " + string(body),
		})
	}))
	t.Cleanup(func() { fb.server.Close() })

	addr := fb.server.URL[7:] // strip "http://"
	host, port, _ := parseBackendHostPort(addr)
	fb.host = host
	fb.port = port
	return fb
}

func newRegistryWithVMs(t *testing.T, vms ...types.VirtualModelConfig) *virtualmodel.Registry {
	t.Helper()
	registry := virtualmodel.NewRegistry()
	registry.SetEnabled(true)
	for _, vm := range vms {
		require.NoError(t, registry.Register(vm))
	}
	return registry
}

func newTestProxy(t *testing.T) *Proxy {
	t.Helper()
	proxy := NewProxy(createTestConfig())
	proxy.SetQueueManagerProxy()
	t.Cleanup(func() { proxy.queueMgr.Stop() })
	return proxy
}

// =====================================================================
// Tests
// =====================================================================

// Test 1: Non-virtual model → IsVirtualModelPath returns false.
func TestVirtualRouter_NonVirtualModel_Fallback(t *testing.T) {
	t.Parallel()
	registry := virtualmodel.NewRegistry()
	registry.SetEnabled(true)
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	// Без регистрации → IsVirtualModelPath=false.
	assert.False(t, router.IsVirtualModelPath("llama-3-70b"))
	assert.False(t, router.IsVirtualModelPath(""))
}

// Test 2: Round-robin — 3 запроса → 3 разных backend'а (1 each).
func TestVirtualRouter_RoundRobinSelection(t *testing.T) {
	t.Parallel()
	b1 := newFakeBackend(t)
	b2 := newFakeBackend(t)
	b3 := newFakeBackend(t)

	// Wait — parseBackendHostPort returns 0 for httptest default port.
	// httptest.NewServer assigns a random port; need to read URL.
	// Actually parseBackendHostPort above already parses from fb.server.URL.
	// Let me fix the call.
	_ = b1
	_ = b2
	_ = b3

	// Re-do with proper host:port strings.
	b1 = newFakeBackend(t)
	b2 = newFakeBackend(t)
	b3 = newFakeBackend(t)
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-test",
		Description: "Test VM",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", b1.host, b1.port),
			fmt.Sprintf("%s:%d", b2.host, b2.port),
			fmt.Sprintf("%s:%d", b3.host, b3.port)},
		ModelName: "physical-model",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	// 3 запроса → должны попасть на 3 разных backend'а (round-robin).
	for i := 0; i < 3; i++ {
		body := bytes.NewReader([]byte(`{"model":"vm-test","prompt":"hi","stream":false}`))
		req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "iteration %d: %s", i, w.Body.String())
	}

	// Проверяем что каждый backend получил ровно 1 запрос.
	assert.Equal(t, int64(1), b1.calls.Load(), "backend 1 should get 1 request")
	assert.Equal(t, int64(1), b2.calls.Load(), "backend 2 should get 1 request")
	assert.Equal(t, int64(1), b3.calls.Load(), "backend 3 should get 1 request")

	// Проверяем metrics.
	metrics := router.GetMetrics().Snapshot()
	assert.Equal(t, int64(3), metrics["inferenceTotal"])
	assert.Equal(t, int64(1), metrics["backendSelections"].(map[string]int64)[fmt.Sprintf("%s:%d", b1.host, b1.port)])
	assert.Equal(t, int64(1), metrics["backendSelections"].(map[string]int64)[fmt.Sprintf("%s:%d", b2.host, b2.port)])
	assert.Equal(t, int64(1), metrics["backendSelections"].(map[string]int64)[fmt.Sprintf("%s:%d", b3.host, b3.port)])
}

// Test 3: Least-loaded — b2 имеет max FreeSlots, всегда выбирается.
func TestVirtualRouter_LeastLoadedSelection(t *testing.T) {
	t.Parallel()
	b1 := newFakeBackend(t)
	b2 := newFakeBackend(t)
	b3 := newFakeBackend(t)
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:      "vm-ll",
		Selection: virtualmodel.SelectionLeastLoaded,
		BackendPool: []string{
			fmt.Sprintf("%s:%d", b1.host, b1.port),
			fmt.Sprintf("%s:%d", b2.host, b2.port),
			fmt.Sprintf("%s:%d", b3.host, b3.port),
		},
		ModelName: "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	// Configure load provider: b2 имеет max FreeSlots.
	if least, ok := router.selectors["vm-ll"].(*virtualmodel.LeastLoadedSelector); ok {
		// Selector not yet created — create via Select call first.
		_ = least
	}
	// Trigger selector creation by making one request (no body).
	_ = registry // suppress unused

	// Set LoadProvider через custom method (need to add) — пока используем pre-call hook.
	// Workaround: после первого запроса достанем selector и set'нем.
	body := bytes.NewReader([]byte(`{"model":"vm-ll","prompt":"a","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	router.ServeHTTP(httptest.NewRecorder(), req) // первый вызов — selector created

	// Достаём созданный selector и настраиваем LoadProvider.
	router.selectorsMu.RLock()
	sel := router.selectors["vm-ll"]
	router.selectorsMu.RUnlock()
	if least, ok := sel.(*virtualmodel.LeastLoadedSelector); ok {
		least.SetLoadProvider(func(backendID string) (int, bool) {
			if backendID == fmt.Sprintf("%s:%d", b2.host, b2.port) {
				return 100, true
			}
			if backendID == fmt.Sprintf("%s:%d", b1.host, b1.port) {
				return 5, true
			}
			return 3, true
		})
	}

	// Reset call counters.
	b1.calls.Store(0)
	b2.calls.Store(0)
	b3.calls.Store(0)

	// 5 запросов — все должны попасть на b2.
	for i := 0; i < 5; i++ {
		body := bytes.NewReader([]byte(`{"model":"vm-ll","prompt":"x","stream":false}`))
		req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}

	assert.Equal(t, int64(0), b1.calls.Load(), "b1 has lower load, should not be selected")
	assert.Equal(t, int64(0), b3.calls.Load(), "b3 has lower load, should not be selected")
	assert.Equal(t, int64(5), b2.calls.Load(), "b2 has max load, should get all 5 requests")
}

// Test 4: All backends down — fake backend returns 500.
// После Phase 8 P.2 backlog (auto-failover) поведение изменилось:
// 5xx теперь retryable. Если в pool'е только 1 backend, failover loop
// заканчивается без success → 502 "all_backends_failed".
func TestVirtualRouter_AllBackendsDown_502(t *testing.T) {
	t.Parallel()
	// 1 "down" backend.
	downServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer downServer.Close()
	downAddr := downServer.URL[7:]
	downHost, downPort, _ := parseBackendHostPort(downAddr)

	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-down",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", downHost, downPort)},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	body := bytes.NewReader([]byte(`{"model":"vm-down","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Single backend in pool, returns 500 (5xx = retryable). После 1 retry loop →
	// "all_backends_failed" (502). С pre-Phase-8-P.2-backlog поведение было 500.
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

// Test 5: Unknown virtual model → 404.
func TestVirtualRouter_UnknownVirtualModel_404(t *testing.T) {
	t.Parallel()
	registry := virtualmodel.NewRegistry()
	registry.SetEnabled(true)
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	body := bytes.NewReader([]byte(`{"model":"vm-unknown","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	// Ollama format: {"error": "<message>"} — error type в OpenAI format.
	assert.Contains(t, w.Body.String(), "not found")
}

// Test 6: Streaming passthrough — request with stream=true, response with SSE.
func TestVirtualRouter_StreamingPassthrough(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-stream",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", b.host, b.port)},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	body := bytes.NewReader([]byte(`{"model":"vm-stream","prompt":"hi","stream":true}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "chunk1")
	assert.Contains(t, w.Body.String(), "chunk2")
	assert.Contains(t, w.Body.String(), "[DONE]")

	// Метрики streaming.
	metrics := router.GetMetrics().Snapshot()
	assert.Equal(t, int64(1), metrics["streamPassThrough"])
}

// Test 7: X-Original-Backend и X-Virtual-Model headers пробрасываются.
func TestVirtualRouter_DebugHeaders(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-debug",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", b.host, b.port)},
		ModelName:   "physical-model",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	body := bytes.NewReader([]byte(`{"model":"vm-debug","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// X-Original-Backend и X-Virtual-Model должны быть в response.
	assert.Equal(t, fmt.Sprintf("%s:%d", b.host, b.port), w.Header().Get("X-Original-Backend"))
	assert.Equal(t, "vm-debug", w.Header().Get("X-Virtual-Model"))

	// Backend получил rewritten body с model=physical-model.
	var gotBody struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(b.lastBody, &gotBody))
	assert.Equal(t, "physical-model", gotBody.Model, "model should be rewritten to physical")
}

// Test 8: Body parse error (invalid JSON) → 400.
func TestVirtualRouter_BodyParseError(t *testing.T) {
	t.Parallel()
	registry := virtualmodel.NewRegistry()
	registry.SetEnabled(true)
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	body := bytes.NewReader([]byte(`{"model":`)) // truncated
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "parse JSON")
}

// Test 9: Backend unreachable (port 1) → 502 backend_unreachable.
func TestVirtualRouter_BackendUnreachable_502(t *testing.T) {
	t.Parallel()
	// Port 1 на localhost гарантированно не listening.
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-unreachable",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{"127.0.0.1:1"},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	body := bytes.NewReader([]byte(`{"model":"vm-unreachable","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should get 502 backend_unreachable (connection refused).
	// Может также вернуть 503 если connection timeout — зависит от OS.
	assert.True(t, w.Code == http.StatusBadGateway || w.Code == http.StatusServiceUnavailable,
		"expected 502 or 503, got %d: %s", w.Code, w.Body.String())
}

// Test 10: Registry not enabled → 503 router not active.
func TestVirtualRouter_RegistryDisabled_503(t *testing.T) {
	t.Parallel()
	registry := virtualmodel.NewRegistry() // not enabled
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	body := bytes.NewReader([]byte(`{"model":"any","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "virtual_router not active")
}

// Test 11: IsActive returns correct state.
func TestVirtualRouter_IsActive(t *testing.T) {
	t.Parallel()
	proxy := newTestProxy(t)

	// nil registry → not active.
	r1 := NewVirtualRouter(nil, proxy)
	assert.False(t, r1.IsActive())

	// registry but not enabled.
	reg1 := virtualmodel.NewRegistry()
	r2 := NewVirtualRouter(reg1, proxy)
	assert.False(t, r2.IsActive())

	// enabled.
	reg2 := virtualmodel.NewRegistry()
	reg2.SetEnabled(true)
	r3 := NewVirtualRouter(reg2, proxy)
	assert.True(t, r3.IsActive())
}

// Test 12: IsVirtualPathRequest фильтрует по path + method.
func TestVirtualRouter_IsVirtualPathRequest(t *testing.T) {
	t.Parallel()
	router := NewVirtualRouter(virtualmodel.NewRegistry(), nil)

	// Allowed paths (POST only).
	allowed := []string{"/v1/chat/completions", "/v1/completions",
		"/api/generate", "/api/ollama/generate",
		"/api/chat", "/api/ollama/chat"}
	for _, p := range allowed {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		assert.True(t, router.IsVirtualPathRequest(req), "POST %s should be allowed", p)
	}

	// Not allowed paths.
	notAllowed := []string{"/api/tags", "/", "/health", "/api/version", "/v1/models"}
	for _, p := range notAllowed {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		assert.False(t, router.IsVirtualPathRequest(req), "POST %s should NOT be allowed", p)
	}

	// GET not allowed.
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	assert.False(t, router.IsVirtualPathRequest(req), "GET should not be allowed")
}

// Test 13: parseBackendHostPort edge cases.
func TestParseBackendHostPort(t *testing.T) {
	t.Parallel()
	host, port, err := parseBackendHostPort("127.0.0.1:8080")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
	assert.Equal(t, 8080, port)

	host, port, err = parseBackendHostPort("backend.local:11434")
	require.NoError(t, err)
	assert.Equal(t, "backend.local", host)
	assert.Equal(t, 11434, port)

	// Invalid formats.
	_, _, err = parseBackendHostPort("127.0.0.1")
	assert.Error(t, err)
	_, _, err = parseBackendHostPort("host:notaport")
	assert.Error(t, err)
}

// Test 14: Metrics snapshot.
func TestVirtualRouter_MetricsRecorded(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-metrics",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", b.host, b.port)},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	// 3 успешных + 1 unknown.
	for i := 0; i < 3; i++ {
		body := bytes.NewReader([]byte(`{"model":"vm-metrics","prompt":"hi","stream":false}`))
		req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
		router.ServeHTTP(httptest.NewRecorder(), req)
	}
	body := bytes.NewReader([]byte(`{"model":"unknown","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	router.ServeHTTP(httptest.NewRecorder(), req)

	metrics := router.GetMetrics().Snapshot()
	assert.Equal(t, int64(3), metrics["inferenceTotal"])
	assert.Equal(t, int64(1), metrics["inferenceErrors"], "1 unknown model should be error")
	selections := metrics["backendSelections"].(map[string]int64)
	assert.Equal(t, int64(3), selections[fmt.Sprintf("%s:%d", b.host, b.port)])
}

// =====================================================================
// Phase 8 P.2 backlog: Auth middleware integration tests.
// =====================================================================

// TestVirtualRouter_Auth_NoToken_Rejected — auth enabled, no token → 401.
func TestVirtualRouter_Auth_NoToken_Rejected(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-auth",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", b.host, b.port)},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)
	router.SetAuthenticator(&fakeAuthChecker{enabled: true, tokens: map[string]bool{"valid": true}})

	body := bytes.NewReader([]byte(`{"model":"vm-auth","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestVirtualRouter_Auth_ValidToken_Allowed — auth enabled, valid token → request passes.
func TestVirtualRouter_Auth_ValidToken_Allowed(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-auth2",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", b.host, b.port)},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)
	router.SetAuthenticator(&fakeAuthChecker{enabled: true, tokens: map[string]bool{"valid": true}})

	body := bytes.NewReader([]byte(`{"model":"vm-auth2","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	req.Header.Set("X-API-Token", "valid")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

// TestVirtualRouter_Auth_Disabled_NoCheck — auth disabled → no check.
func TestVirtualRouter_Auth_Disabled_NoCheck(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-auth3",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", b.host, b.port)},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)
	router.SetAuthenticator(&fakeAuthChecker{enabled: false, tokens: nil})

	body := bytes.NewReader([]byte(`{"model":"vm-auth3","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestVirtualRouter_Auth_NilChecker_NoCheck — nil authenticator → no check.
func TestVirtualRouter_Auth_NilChecker_NoCheck(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-auth4",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", b.host, b.port)},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)
	// НЕ вызываем SetAuthenticator — d.authChecker == nil.

	body := bytes.NewReader([]byte(`{"model":"vm-auth4","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestVirtualRouter_Auth_OpenAIFormat_401 — 401 на OpenAI endpoint в OpenAI error format.
func TestVirtualRouter_Auth_OpenAIFormat_401(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-auth5",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", b.host, b.port)},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)
	router.SetAuthenticator(&fakeAuthChecker{enabled: true, tokens: map[string]bool{"valid": true}})

	body := bytes.NewReader([]byte(`{"model":"vm-auth5","messages":[{"role":"user","content":"x"}]}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	var errResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errResp))
	errObj, ok := errResp["error"].(map[string]interface{})
	require.True(t, ok, "expected OpenAI error format: {error: {message, type}}")
	assert.Equal(t, "unauthorized", errObj["type"])
}

// fakeAuthChecker — test double для AuthChecker.
// Определён в auth_checker_test.go (общий).

// =====================================================================
// Phase 8 Item 2: LoadProvider wire-up for least_loaded selector.
// =====================================================================

// TestVirtualRouter_LeastLoaded_LoadProvider_PrefersFreeBackend —
// LoadProvider correctly identifies backend with most free slots.
func TestVirtualRouter_LeastLoaded_LoadProvider_PrefersFreeBackend(t *testing.T) {
	t.Parallel()

	// Use real httptest backends.
	o1 := newOllamaFake(t, "o1")
	o2 := newOllamaFake(t, "o2")
	o1ID := fmt.Sprintf("%s:%d", o1.host, o1.port)
	o2ID := fmt.Sprintf("%s:%d", o2.host, o2.port)

	// o1 has 1/10 slots used (9 free), o2 has 5/10 slots used (5 free).
	// least_loaded should always pick o1 (more free).
	loadFn := func(backendID string) (int, bool) {
		switch backendID {
		case o1ID:
			return 9, true // 9 free
		case o2ID:
			return 5, true // 5 free
		}
		return 0, false
	}

	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-ll",
		Selection:   virtualmodel.SelectionLeastLoaded,
		BackendPool: []string{o1ID, o2ID},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)
	router.SetLoadProvider(loadFn)

	// Trigger selector creation (lazy) + apply LoadProvider to it.
	body := bytes.NewReader([]byte(`{"model":"vm-ll","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	router.selectorsMu.RLock()
	sel := router.selectors["vm-ll"]
	router.selectorsMu.RUnlock()
	if ll, ok := sel.(*virtualmodel.LeastLoadedSelector); ok {
		ll.SetLoadProvider(loadFn)
	}

	// 5 запросов — все должны идти на o1 (больше free slots).
	counts := map[string]int{}
	for i := 0; i < 5; i++ {
		body := bytes.NewReader([]byte(`{"model":"vm-ll","prompt":"x","stream":false}`))
		req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		backend := w.Header().Get("X-Original-Backend")
		counts[backend]++
	}
	// o1 should get all 5 (since it has 9 free vs 5 free).
	assert.Equal(t, 5, counts[o1ID], "%s should be preferred (9 free vs 5 free)", o1ID)
	assert.Equal(t, 0, counts[o2ID], "%s should NOT be selected", o2ID)
}

// TestVirtualRouter_LeastLoaded_LoadProvider_BalancesAfterChange —
// При изменении load provider'а selector rebalances.
func TestVirtualRouter_LeastLoaded_LoadProvider_BalancesAfterChange(t *testing.T) {
	t.Parallel()

	o1 := newOllamaFake(t, "o1")
	o2 := newOllamaFake(t, "o2")
	o1ID := fmt.Sprintf("%s:%d", o1.host, o1.port)
	o2ID := fmt.Sprintf("%s:%d", o2.host, o2.port)

	// Phase 1: o1 busy (3 free), o2 idle (10 free) → o2 gets all requests.
	loadFn1 := func(backendID string) (int, bool) {
		switch backendID {
		case o1ID:
			return 3, true
		case o2ID:
			return 10, true
		}
		return 0, false
	}

	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:        "vm-ll2",
		Selection:   virtualmodel.SelectionLeastLoaded,
		BackendPool: []string{o1ID, o2ID},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)
	router.SetLoadProvider(loadFn1)

	// Trigger selector creation + set provider.
	body := bytes.NewReader([]byte(`{"model":"vm-ll2","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	router.selectorsMu.RLock()
	sel := router.selectors["vm-ll2"]
	router.selectorsMu.RUnlock()
	ll, ok := sel.(*virtualmodel.LeastLoadedSelector)
	require.True(t, ok)
	ll.SetLoadProvider(loadFn1)

	// Phase 1: o2 wins (10 free).
	for i := 0; i < 3; i++ {
		body := bytes.NewReader([]byte(`{"model":"vm-ll2","prompt":"x","stream":false}`))
		req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, o2ID, w.Header().Get("X-Original-Backend"),
			"phase 1 iter %d: %s should win (10 free vs 3)", i, o2ID)
	}

	// Phase 2: flip — o1 now idle, o2 busy.
	loadFn2 := func(backendID string) (int, bool) {
		switch backendID {
		case o1ID:
			return 10, true
		case o2ID:
			return 1, true
		}
		return 0, false
	}
	ll.SetLoadProvider(loadFn2)

	// Phase 2: o1 wins (10 free).
	for i := 0; i < 3; i++ {
		body := bytes.NewReader([]byte(`{"model":"vm-ll2","prompt":"x","stream":false}`))
		req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, o1ID, w.Header().Get("X-Original-Backend"),
			"phase 2 iter %d: %s should win (10 free vs 1)", i, o1ID)
	}
}

// TestProxy_GetBackendFreeSlots — unit test для proxy.GetBackendFreeSlots.
func TestProxy_GetBackendFreeSlots(t *testing.T) {
	t.Parallel()
	conf := createE2EConfig()
	conf.Backends = []types.Backend{
		{ID: "b1", Name: "b1", Host: "localhost", OllamaPort: 11434, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		{ID: "b2", Name: "b2", Host: "localhost", OllamaPort: 11435, MaxConcurrentReqs: 5, Status: types.StatusHealthy},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// b1: 10 max, 0 active → 10 free.
	free, ok := proxy.GetBackendFreeSlots("b1")
	require.True(t, ok)
	assert.Equal(t, 10, free)

	// b2: 5 max, 0 active → 5 free.
	free, ok = proxy.GetBackendFreeSlots("b2")
	require.True(t, ok)
	assert.Equal(t, 5, free)

	// Симулируем active requests: increment ActiveReqs напрямую.
	proxy.mu.Lock()
	state := proxy.backends["b1"]
	state.mu.Lock()
	state.ActiveReqs = 7
	state.mu.Unlock()
	proxy.mu.Unlock()

	// b1: 10 max, 7 active → 3 free.
	free, ok = proxy.GetBackendFreeSlots("b1")
	require.True(t, ok)
	assert.Equal(t, 3, free)

	// Несуществующий backend.
	_, ok = proxy.GetBackendFreeSlots("nonexistent")
	assert.False(t, ok)
}

// TestVirtualRouter_Failover_PrimaryDown_RetryNext — primary backend down,
// retry на next candidate in pool. Должен вернуть success от 2-го backend.
func TestVirtualRouter_Failover_PrimaryDown_RetryNext(t *testing.T) {
	t.Parallel()

	// Backend 1: alive.
	b1 := newFakeBackend(t)
	// Backend 2: not listening (port 1).
	b2Host := "127.0.0.1"
	b2Port := 1

	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:      "vm-failover",
		Selection: virtualmodel.SelectionRoundRobin,
		// b2 first (selected, will fail), b1 second (failover target).
		// NB: round-robin counter might pick either; we use a single test
		// scenario where selector returns b2 first.
		BackendPool: []string{
			fmt.Sprintf("%s:%d", b2Host, b2Port),    // primary — will fail
			fmt.Sprintf("%s:%d", b1.host, b1.port), // failover — succeeds
		},
		ModelName: "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	// Force selector to always return b2 (so primary is the down one).
	if rr, ok := router.selectors["vm-failover"].(*virtualmodel.RoundRobinSelector); ok {
		_ = rr
	}
	// Reset round-robin + use a custom selector that always returns b2.
	router.selectorsMu.Lock()
	router.selectors["vm-failover"] = virtualmodel.NewSelector(virtualmodel.SelectionRoundRobin)
	router.selectorsMu.Unlock()

	// After reset, round-robin counter starts at 0, so first call returns pool[0] = b2.
	body := bytes.NewReader([]byte(`{"model":"vm-failover","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Должен succeed от b1 (failover).
	assert.Equal(t, http.StatusOK, w.Code,
		"body: %s", w.Body.String())
	assert.Equal(t, fmt.Sprintf("%s:%d", b1.host, b1.port), w.Header().Get("X-Original-Backend"),
		"X-Original-Backend should be the failover target b1")
	assert.Equal(t, "2", w.Header().Get("X-Failover-Attempts"),
		"X-Failover-Attempts should be 2 (1 primary failed, 1 retry succeeded)")

	// b1 должен получить ровно 1 запрос.
	assert.Equal(t, int64(1), b1.calls.Load())
}

// TestVirtualRouter_Failover_AllDown_502 — все backends down → 502.
func TestVirtualRouter_Failover_AllDown_502(t *testing.T) {
	t.Parallel()
	registry := newRegistryWithVMs(t, types.VirtualModelConfig{
		Name:      "vm-alldown",
		Selection: virtualmodel.SelectionRoundRobin,
		// 2 backends, оба на port 1 (not listening).
		BackendPool: []string{"127.0.0.1:1", "127.0.0.1:2"},
		ModelName:   "physical",
	})
	proxy := newTestProxy(t)
	router := NewVirtualRouter(registry, proxy)

	body := bytes.NewReader([]byte(`{"model":"vm-alldown","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code,
		"all backends down should return 502")
	// Ollama format: {error: "all 2 backends failed for model \"vm-alldown\", ..."}
	assert.Contains(t, w.Body.String(), "backends failed",
		"body should mention 'backends failed', got: %s", w.Body.String())
}

// TestBuildFailoverCandidates — unit test для candidate list builder.
func TestBuildFailoverCandidates(t *testing.T) {
	t.Parallel()

	// Single backend, no duplicates.
	got := buildFailoverCandidates("a:1", []string{"a:1"})
	assert.Equal(t, []string{"a:1"}, got)

	// Primary + 1 other.
	got = buildFailoverCandidates("b:2", []string{"a:1", "b:2", "c:3"})
	assert.Equal(t, []string{"b:2", "a:1", "c:3"}, got)

	// Primary not in pool (edge case).
	got = buildFailoverCandidates("d:4", []string{"a:1", "b:2"})
	assert.Equal(t, []string{"d:4", "a:1", "b:2"}, got)

	// Empty pool.
	got = buildFailoverCandidates("a:1", nil)
	assert.Equal(t, []string{"a:1"}, got)

	// Empty primary.
	got = buildFailoverCandidates("", []string{"a:1", "b:2"})
	assert.Equal(t, []string{"a:1", "b:2"}, got)
}

// Suppress unused time import warning (used in some test patterns).
var _ = time.Second
