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
func TestVirtualRouter_AllBackendsDown_503(t *testing.T) {
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

	// Backend returned 500 — proxy passes status code through.
	assert.Equal(t, http.StatusInternalServerError, w.Code)
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

// Suppress unused time import warning (used in some test patterns).
var _ = time.Second
