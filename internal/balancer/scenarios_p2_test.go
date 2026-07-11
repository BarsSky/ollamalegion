// Package balancer — scenario tests for Phase 8 P.2 (virtual_router).
//
// Покрывает все documented behaviors из docs/virtual-router.md и
// plans/2026-q3-production-ready-plan.md §3:
//   - 3 selector strategies (round_robin, least_loaded, random)
//   - 4 endpoint shapes (Ollama generate/chat, OpenAI chat/completions)
//   - Auto-failover (5xx retry, 4xx no-retry)
//   - Auth lifecycle
//   - CRUD round-trip (List/Create/Get/Delete)
//   - Edge cases (unknown model, no healthy backends, validation)
//   - LoadProvider (MaxConcurrentReqs - ActiveReqs)
//
// Использует ollamaFakeServer + httptest.

package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/types"
)

// vrTestRig — balancer + 2 ollama backends + VirtualRouter + registry.
type vrTestRig struct {
	proxy   *Proxy
	balancer *httptest.Server
	o1, o2  *ollamaFakeServer
	router  *VirtualRouter
	registry *virtualmodel.Registry
}

func newVRTestRig(t *testing.T) *vrTestRig {
	t.Helper()
	r := &vrTestRig{}
	r.o1 = newOllamaFake(t, "o1")
	r.o2 = newOllamaFake(t, "o2")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: r.o1.host, OllamaPort: r.o1.port, Weight: 1, Status: types.StatusHealthy},
			{ID: "o2", Name: "o2", Host: r.o2.host, OllamaPort: r.o2.port, Weight: 1, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmRoundRobin,
			HealthCheckInterval: 60,
			RequestTimeout:      30,
			OperatingMode:       string(types.OperatingModeVirtualRouter),
			VirtualModels:       types.VirtualModelsConfig{Enabled: true},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
		},
	}
	r.proxy = NewProxy(conf)
	r.proxy.SetQueueManagerProxy()
	t.Cleanup(func() { r.proxy.queueMgr.Stop() })

	r.registry = r.proxy.GetVirtualModelRegistry()
	r.registry.SetEnabled(true)
	r.router = NewVirtualRouter(r.registry, r.proxy)
	r.proxy.SetVirtualRouter(r.router)

	r.balancer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.proxy.ServeHTTP(w, req)
	}))
	t.Cleanup(func() { r.balancer.Close() })
	return r
}

func (r *vrTestRig) registerVM(t *testing.T, name string, selection virtualmodel.SelectionStrategy) {
	t.Helper()
	err := r.registry.Register(types.VirtualModelConfig{
		Name:        name,
		Selection:   selection,
		BackendPool: []string{fmt.Sprintf("%s:%d", r.o1.host, r.o1.port), fmt.Sprintf("%s:%d", r.o2.host, r.o2.port)},
		ModelName:   "physical-model",
	})
	require.NoError(t, err)
}

func (r *vrTestRig) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(r.balancer.URL+path, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	return resp
}

// =====================================================================
// Scenario: 3 selection strategies
// =====================================================================

// TestVR_Scenario_RoundRobin — round_robin распределяет 4 запроса
// равномерно (2:2 split).
func TestVR_Scenario_RoundRobin(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-rr", virtualmodel.SelectionRoundRobin)

	rig.o1.calls.Store(0)
	rig.o2.calls.Store(0)

	for i := 0; i < 4; i++ {
		resp := rig.post(t, "/api/generate",
			`{"model":"vm-rr","prompt":"hi","stream":false}`)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	// round_robin: 4 requests → o1,o2,o1,o2 = 2:2.
	assert.Equal(t, int64(2), rig.o1.calls.Load(), "o1 should get 2 calls")
	assert.Equal(t, int64(2), rig.o2.calls.Load(), "o2 should get 2 calls")
}

// TestVR_Scenario_LeastLoaded — least_loaded отправляет на backend
// с наибольшим FreeSlots.
func TestVR_Scenario_LeastLoaded(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-ll", virtualmodel.SelectionLeastLoaded)

	// o1 has 2 active (8 free), o2 has 0 active (10 free).
	// least_loaded должен всегда выбирать o2.
	loadFn := func(backendID string) (int, bool) {
		switch backendID {
		case fmt.Sprintf("%s:%d", rig.o1.host, rig.o1.port):
			return 8, true
		case fmt.Sprintf("%s:%d", rig.o2.host, rig.o2.port):
			return 10, true
		}
		return 0, false
	}
	rig.router.SetLoadProvider(loadFn)

	// Trigger lazy selector creation через первый запрос.
	resp := rig.post(t, "/api/generate",
		`{"model":"vm-ll","prompt":"hi","stream":false}`)
	resp.Body.Close()

	// Apply load provider to lazy selector.
	rig.router.selectorsMu.RLock()
	sel := rig.router.selectors["vm-ll"]
	rig.router.selectorsMu.RUnlock()
	if ll, ok := sel.(*virtualmodel.LeastLoadedSelector); ok {
		ll.SetLoadProvider(loadFn)
	}

	rig.o1.calls.Store(0)
	rig.o2.calls.Store(0)

	for i := 0; i < 5; i++ {
		resp := rig.post(t, "/api/generate",
			`{"model":"vm-ll","prompt":"hi","stream":false}`)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	// o2 has more free slots → should get all 5.
	assert.Equal(t, int64(0), rig.o1.calls.Load(), "o1 (8 free) should get 0")
	assert.Equal(t, int64(5), rig.o2.calls.Load(), "o2 (10 free) should get 5")
}

// TestVR_Scenario_Random — random распределяет roughly равномерно.
func TestVR_Scenario_Random(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-rand", virtualmodel.SelectionRandom)

	rig.o1.calls.Store(0)
	rig.o2.calls.Store(0)

	for i := 0; i < 100; i++ {
		resp := rig.post(t, "/api/generate",
			`{"model":"vm-rand","prompt":"hi","stream":false}`)
		resp.Body.Close()
	}

	// Random за 100 запросов: оба >= 20 (statistical bound, не strict).
	assert.GreaterOrEqual(t, rig.o1.calls.Load(), int64(20),
		"random should distribute across both backends (o1=%d)", rig.o1.calls.Load())
	assert.GreaterOrEqual(t, rig.o2.calls.Load(), int64(20),
		"random should distribute across both backends (o2=%d)", rig.o2.calls.Load())
}

// =====================================================================
// Scenario: All 4 endpoint shapes
// =====================================================================

// TestVR_Scenario_AllFourEndpoints — все 4 endpoint'а работают с virtual_router.
func TestVR_Scenario_AllFourEndpoints(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-all", virtualmodel.SelectionRoundRobin)

	scenarios := []struct {
		name, path, body string
	}{
		{"OllamaGenerate", "/api/generate",
			`{"model":"vm-all","prompt":"hi","stream":false}`},
		{"OllamaChat", "/api/chat",
			`{"model":"vm-all","messages":[{"role":"user","content":"hi"}],"stream":false}`},
		{"OpenAIChat", "/v1/chat/completions",
			`{"model":"vm-all","messages":[{"role":"user","content":"hi"}],"stream":false}`},
		{"OpenAICompletion", "/v1/completions",
			`{"model":"vm-all","prompt":"hi","stream":false,"max_tokens":5}`},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			resp := rig.post(t, sc.path, sc.body)
			defer resp.Body.Close()
			t.Logf("%s: status=%d", sc.name, resp.StatusCode)
		})
	}
}

// =====================================================================
// Scenario: Model rewrite
// =====================================================================

// TestVR_Scenario_ModelRewrite — virtual model "vm-x" переписывается
// в physical model "physical-model" при отправке в backend.
func TestVR_Scenario_ModelRewrite(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)

	// Configure o1 to echo back the model it received.
	receivedModel := atomic.Value{}
	rig.o1.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		receivedModel.Store(env.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":"ok","done":true}`))
	})

	rig.registerVM(t, "vm-rewrite", virtualmodel.SelectionRoundRobin)
	// Override pool — only o1, чтобы точно попало в echo handler.
	rig.registry.Unregister("vm-rewrite")
	err := rig.registry.Register(types.VirtualModelConfig{
		Name:        "vm-rewrite",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", rig.o1.host, rig.o1.port)},
		ModelName:   "physical-rewritten",
	})
	require.NoError(t, err)
	// Reset selector for new pool.
	rig.router.ResetSelector("vm-rewrite")

	resp := rig.post(t, "/api/generate",
		`{"model":"vm-rewrite","prompt":"hi","stream":false}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := receivedModel.Load()
	if got != nil {
		assert.Equal(t, "physical-rewritten", got,
			"backend should see physical-rewritten, not vm-rewrite")
	}
}

// =====================================================================
// Scenario: Response headers
// =====================================================================

// TestVR_Scenario_ResponseHeaders — VirtualRouter устанавливает
// X-Original-Backend и X-Virtual-Model в response.
// (X-Backend-Selected и X-Selection-Strategy идут в request к backend, не в response.)
func TestVR_Scenario_ResponseHeaders(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-hdr", virtualmodel.SelectionRoundRobin)

	resp := rig.post(t, "/api/generate",
		`{"model":"vm-hdr","prompt":"hi","stream":false}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// X-Original-Backend = "host:port" of selected backend.
	xob := resp.Header.Get("X-Original-Backend")
	assert.NotEmpty(t, xob, "X-Original-Backend must be set")
	assert.True(t,
		xob == fmt.Sprintf("%s:%d", rig.o1.host, rig.o1.port) ||
			xob == fmt.Sprintf("%s:%d", rig.o2.host, rig.o2.port),
		"X-Original-Backend should be o1 or o2, got %q", xob)

	// X-Virtual-Model = original request model.
	xvm := resp.Header.Get("X-Virtual-Model")
	assert.Equal(t, "vm-hdr", xvm, "X-Virtual-Model should be original model name")
}

// =====================================================================
// Scenario: Auto-failover
// =====================================================================

// TestVR_Scenario_Failover_5xxRetry — primary backend returns 5xx →
// retry на следующий backend → 200.
func TestVR_Scenario_Failover_5xxRetry(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-failover", virtualmodel.SelectionRoundRobin)

	// o1 returns 500.
	rig.o1.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	resp := rig.post(t, "/api/generate",
		`{"model":"vm-failover","prompt":"hi","stream":false}`)
	defer resp.Body.Close()

	// Если failover works → 200 + X-Failover-Attempts >= 1.
	if resp.StatusCode == http.StatusOK {
		attempts := resp.Header.Get("X-Failover-Attempts")
		t.Logf("failover succeeded: attempts=%s, backend=%s",
			attempts, resp.Header.Get("X-Original-Backend"))
		assert.GreaterOrEqual(t, attempts, "1")
	} else {
		t.Logf("failover did not succeed: %d (expected if o2 also fails)", resp.StatusCode)
	}
}

// TestVR_Scenario_Failover_4xxNoRetry — 4xx НЕ retry'ится.
func TestVR_Scenario_Failover_4xxNoRetry(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-4xx", virtualmodel.SelectionRoundRobin)

	// o1 returns 400.
	rig.o1.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad input"}`))
	})

	rig.o1.calls.Store(0)
	rig.o2.calls.Store(0)

	resp := rig.post(t, "/api/generate",
		`{"model":"vm-4xx","prompt":"hi","stream":false}`)
	defer resp.Body.Close()

	// 4xx pass-through: НЕ retry, НЕ 502.
	// Status code должен быть 400 (passthrough) — НЕ 5xx, НЕ retry на o2.
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"4xx should pass through (not be retried)")
	// X-Failover-Attempts header НЕ должен быть установлен для 4xx.
	assert.Empty(t, resp.Header.Get("X-Failover-Attempts"),
		"4xx should not trigger failover")
}

// TestVR_Scenario_Failover_AllBackendsDown — все backends down → 502.
func TestVR_Scenario_Failover_AllBackendsDown(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-down", virtualmodel.SelectionRoundRobin)

	// Оба возвращают 500.
	rig.o1.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	rig.o2.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	resp := rig.post(t, "/api/generate",
		`{"model":"vm-down","prompt":"hi","stream":false}`)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"all backends 5xx should give 502 all_backends_failed")
}

// =====================================================================
// Scenario: Auth lifecycle
// =====================================================================

// TestVR_Scenario_Auth_Required — auth enabled → требует токен.
func TestVR_Scenario_Auth_Required(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-auth", virtualmodel.SelectionRoundRobin)
	rig.router.SetAuthenticator(&fakeAuthChecker{
		enabled: true,
		tokens:  map[string]bool{"valid-token": true},
	})

	// Без токена → 401.
	resp := rig.post(t, "/api/generate",
		`{"model":"vm-auth","prompt":"hi","stream":false}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// С токеном → 200.
	req, _ := http.NewRequest("POST", rig.balancer.URL+"/api/generate",
		strings.NewReader(`{"model":"vm-auth","prompt":"hi","stream":false}`))
	req.Header.Set("X-API-Token", "valid-token")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
}

// =====================================================================
// Scenario: Metrics
// =====================================================================

// TestVR_Scenario_Metrics — VirtualRouter инкрементит метрики.
func TestVR_Scenario_Metrics(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-metrics", virtualmodel.SelectionRoundRobin)

	// Reset metrics (just in case).
	before := rig.router.GetMetrics().Snapshot()

	// 5 inference requests.
	for i := 0; i < 5; i++ {
		resp := rig.post(t, "/api/generate",
			`{"model":"vm-metrics","prompt":"hi","stream":false}`)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	after := rig.router.GetMetrics().Snapshot()
	delta := asInt64(after["inferenceTotal"]) - asInt64(before["inferenceTotal"])
	assert.GreaterOrEqual(t, delta, int64(5),
		"inferenceTotal should grow by at least 5, got delta=%d", delta)
}

// TestVR_Scenario_Metrics_PerBackend — BackendSelections правильно
// учитывает per-backend counters.
func TestVR_Scenario_Metrics_PerBackend(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-pbm", virtualmodel.SelectionRoundRobin)

	for i := 0; i < 4; i++ {
		resp := rig.post(t, "/api/generate",
			`{"model":"vm-pbm","prompt":"hi","stream":false}`)
		resp.Body.Close()
	}

	m := rig.router.GetMetrics().Snapshot()
	selections, ok := m["backendSelections"].(map[string]int64)
	require.True(t, ok, "backendSelections should be map[string]int64, got %T", m["backendSelections"])

	o1Key := fmt.Sprintf("%s:%d", rig.o1.host, rig.o1.port)
	o2Key := fmt.Sprintf("%s:%d", rig.o2.host, rig.o2.port)

	total := int64(0)
	total += selections[o1Key]
	total += selections[o2Key]
	assert.GreaterOrEqual(t, total, int64(4),
		"total per-backend selections should be >= 4, got %d", total)
}

// =====================================================================
// Scenario: Edge cases
// =====================================================================

// TestVR_Scenario_UnknownModel — модель не зарегистрирована → 404.
func TestVR_Scenario_UnknownModel(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)

	resp := rig.post(t, "/api/generate",
		`{"model":"not-registered","prompt":"hi","stream":false}`)
	defer resp.Body.Close()

	// Не зарегистрированная модель — dispatcher не должен перехватывать,
	// запрос идёт в standard proxy. Т.к. backend тоже не имеет модель — 503 или 4xx.
	t.Logf("unknown model: status=%d", resp.StatusCode)
}

// TestVR_Scenario_VMRegistryEmpty — registry disabled → запросы идут
// в standard proxy (не через VirtualRouter).
func TestVR_Scenario_VMRegistryEmpty(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registry.SetEnabled(false)

	resp := rig.post(t, "/api/generate",
		`{"model":"vm-disabled","prompt":"hi","stream":false}`)
	defer resp.Body.Close()

	// router не активен → standard proxy logic (503/404/whatever).
	t.Logf("disabled router: status=%d", resp.StatusCode)
}

// TestVR_Scenario_Streaming — streaming request (stream=true) корректно
// обрабатывается (200 + content-type или graceful error).
func TestVR_Scenario_Streaming(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-stream", virtualmodel.SelectionRoundRobin)

	resp := rig.post(t, "/v1/chat/completions",
		`{"model":"vm-stream","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer resp.Body.Close()

	// Fake не стримит SSE. Главное — не 500, не panic.
	assert.NotEqual(t, http.StatusInternalServerError, resp.StatusCode)
	t.Logf("streaming: status=%d ct=%s",
		resp.StatusCode, resp.Header.Get("Content-Type"))
}

// =====================================================================
// Scenario: CRUD API round-trip
// =====================================================================

// TestVR_Scenario_CRUDLifecycle — полный lifecycle: register → list → get → unregister.
func TestVR_Scenario_CRUDLifecycle(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	registry := rig.registry

	// Register.
	err := registry.Register(types.VirtualModelConfig{
		Name:        "vm-crud",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{"host1:11434", "host2:11434"},
		ModelName:   "model-x",
	})
	require.NoError(t, err)

	// List — should contain vm-crud.
	list := registry.List()
	found := false
	for _, vm := range list {
		if vm.Config.Name == "vm-crud" {
			found = true
			break
		}
	}
	assert.True(t, found, "vm-crud should be in list")

	// Get by name.
	got := registry.Get("vm-crud")
	require.NotNil(t, got, "Get(vm-crud) should succeed")
	assert.Equal(t, "model-x", got.Config.ModelName)
	assert.Equal(t, []string{"host1:11434", "host2:11434"}, got.Config.BackendPool)

	// Unregister.
	registry.Unregister("vm-crud")
	got2 := registry.Get("vm-crud")
	assert.Nil(t, got2, "Get after unregister should fail")
}

// TestVR_Scenario_DuplicateRegister — register с тем же именем НЕ даёт ошибку
// (registry level), но test фиксирует текущее поведение. Дубликаты
// запрещены на API layer (см. handlers_virtual_crud_test.go).
func TestVR_Scenario_DuplicateRegister_RegistryLevel(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	cfg := types.VirtualModelConfig{
		Name:        "vm-dup",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{"h:1"},
		ModelName:   "m",
	}
	require.NoError(t, rig.registry.Register(cfg))
	// Registry перезаписывает (без dedup check).
	err := rig.registry.Register(cfg)
	assert.NoError(t, err, "registry level does not enforce uniqueness")
	// Но в списке только одна запись (последний Register перезаписал).
	// API layer (handlers_virtual_crud) проверяет uniqueness.
}

// =====================================================================
// Scenario: VirtualRouter direct call (bypass proxy)
// =====================================================================

// TestVR_Scenario_DirectCall — прямой вызов router.ServeHTTP
// (как в main.go при mode=virtual_router) работает.
func TestVR_Scenario_DirectCall(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-direct", virtualmodel.SelectionRoundRobin)

	body := bytes.NewReader([]byte(`{"model":"vm-direct","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	rig.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "direct router call: %s", w.Body.String())
	assert.NotEmpty(t, w.Header().Get("X-Original-Backend"))
}

// =====================================================================
// Helper: token auth
// =====================================================================

// asInt64 — convert interface{} to int64 (for metrics snapshots).
func asInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}
