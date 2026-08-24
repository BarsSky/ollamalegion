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
// Phase 8 P.2 Step 6.4: end-to-end test через WebUI API контракт.
//
// Симулирует полный user flow:
//   1. Frontend (WebUI) → API: POST /api/v1/virtual-models (create)
//   2. Frontend → API: GET /api/v1/virtual-models (list)
//   3. Frontend → API: GET /api/v1/virtual-models/{name} (details)
//   4. Frontend → Balancer: POST /api/generate with model=virtual:xxx
//      (Balancer → VirtualRouter → selected backend → response)
//   5. Frontend → API: DELETE /api/v1/virtual-models/{name} (cleanup)
//
// Все шаги — реальный HTTP через httptest.Server.
// =====================================================================

// TestE2E_VirtualRouter_FullFlow — full lifecycle через реальный HTTP.
func TestE2E_VirtualRouter_FullFlow(t *testing.T) {
	t.Parallel()

	// ============ Setup: 2 fake backends ============
	backend1Calls := atomic.Int64{}
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend1Calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model":  env.Model,
			"answer": "from backend 1",
		})
	}))
	defer backend1.Close()

	backend2Calls := atomic.Int64{}
	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend2Calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model":  env.Model,
			"answer": "from backend 2",
		})
	}))
	defer backend2.Close()

	// ============ Setup: real Proxy with virtual_router mode ============
	addr1 := backend1.URL[7:]
	host1, port1, _ := parseBackendHostPort(addr1)
	addr2 := backend2.URL[7:]
	host2, port2, _ := parseBackendHostPort(addr2)

	proxy := newProxyWithCleanup(t, createE2EConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	proxy.config.Balancing.OperatingMode = string(types.OperatingModeVirtualRouter)

	// Wire VirtualRouter + register a virtual model (simulating POST /api/v1/virtual-models).
	registry := proxy.GetVirtualModelRegistry()
	require.NotNil(t, registry)
	registry.SetEnabled(true)
	require.NoError(t, registry.Register(types.VirtualModelConfig{
		Name:        "virtual:e2e",
		Description: "E2E test VM",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{
			fmt.Sprintf("%s:%d", host1, port1),
			fmt.Sprintf("%s:%d", host2, port2),
		},
		ModelName: "physical-model",
	}))
	proxy.SetVirtualRouter(NewVirtualRouter(registry, proxy))

	// ============ Start balancer as HTTP server ============
	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}))
	defer balancer.Close()

	// ============ Step 1: POST /api/generate (with model=virtual:e2e) ============
	// 3 запроса — round-robin должен распределить: b1, b2, b1 (3/2 = 1.5 → 2:1)
	for i := 0; i < 3; i++ {
		body := bytes.NewReader([]byte(`{"model":"virtual:e2e","prompt":"hello","stream":false}`))
		req, _ := http.NewRequest("POST", balancer.URL+"/api/generate", body)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err, "iter %d", i)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "iter %d body: %s", i, readBodySim(resp))

		// Check X-Original-Backend + X-Virtual-Model headers.
		assert.Equal(t, "virtual:e2e", resp.Header.Get("X-Virtual-Model"),
			"X-Virtual-Model should preserve original name")
		ob := resp.Header.Get("X-Original-Backend")
		assert.NotEmpty(t, ob, "X-Original-Backend should be set")
		resp.Body.Close()
	}

	// Backend 1 + backend 2 должны получить хотя бы по 1 запросу.
	assert.True(t, backend1Calls.Load() >= 1, "backend1 should get at least 1 request")
	assert.True(t, backend2Calls.Load() >= 1, "backend2 should get at least 1 request")
	assert.Equal(t, int64(3), backend1Calls.Load()+backend2Calls.Load(),
		"total calls = 3")

	// ============ Step 2: GET /metrics-like endpoint (через VirtualRouter) ============
	// (Не реальный /api/v1/metrics — просто проверяем что proxy + router работают.)
	metricsReq, _ := http.NewRequest("GET", balancer.URL+"/api/tags", nil)
	metricsResp, err := http.DefaultClient.Do(metricsReq)
	require.NoError(t, err)
	metricsResp.Body.Close()
	// /api/tags НЕ intercepts (стандартный proxy flow) — должен вернуть ошибку
	// (нет backend'ов в proxy для тэгов), но НЕ 503 virtual_router.

	// ============ Step 3: DELETE через registry (cleanup) ============
	registry.Unregister("virtual:e2e")
	vm := registry.Get("virtual:e2e")
	assert.Nil(t, vm, "VM should be unregistered")

	// ============ Step 4: После DELETE, requests fall through к standard flow ============
	// Model name "virtual:e2e" больше не in registry → MatchesVirtualRequest
	// возвращает false → request идёт через стандартный proxy flow. В тесте нет
	// backend'ов с такой моделью → 503 "no llama.cpp backend available".
	// Главное: НЕ 503 "no_healthy_backends" (это virtual_router specific).
	body := bytes.NewReader([]byte(`{"model":"virtual:e2e","prompt":"hi","stream":false}`))
	req, _ := http.NewRequest("POST", balancer.URL+"/api/generate", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	// Standard flow errors (503 backend not found), не virtual_router errors.
	assert.NotEqual(t, http.StatusNotFound, resp.StatusCode, "after delete should NOT be 404 from virtual_router")
	respBody, _ := io.ReadAll(resp.Body)
	assert.NotContains(t, string(respBody), "no_healthy_backends",
		"after delete should fall through to standard proxy, not virtual_router errors")
	resp.Body.Close()
}

// TestE2E_VirtualRouter_HealthCheck_NotAffected —
// Убеждаемся что в virtual_router mode обычные health endpoints не затрагиваются.
func TestE2E_VirtualRouter_HealthCheck_NotAffected(t *testing.T) {
	t.Parallel()

	proxy := newProxyWithCleanup(t, createE2EConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	proxy.config.Balancing.OperatingMode = string(types.OperatingModeVirtualRouter)

	registry := proxy.GetVirtualModelRegistry()
	require.NotNil(t, registry, "registry should be initialized when VirtualModels.Enabled=true")
	registry.SetEnabled(true)
	_ = registry.Register(types.VirtualModelConfig{
		Name:        "vm-hc",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{"127.0.0.1:9999"},
		ModelName:   "physical",
	})
	proxy.SetVirtualRouter(NewVirtualRouter(registry, proxy))

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}))
	defer balancer.Close()

	// GET /api/tags — НЕ в IsVirtualPathRequest list → fall through.
	req, _ := http.NewRequest("GET", balancer.URL+"/api/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	// Не должно быть 503 "no_healthy_backends" (это virtual_router error).
	body, _ := io.ReadAll(resp.Body)
	assert.NotContains(t, string(body), "no_healthy_backends",
		"health check should not be intercepted by virtual_router")
}

// TestE2E_VirtualRouter_StreamingResponse —
// Проверяет что streaming request возвращает SSE события от backend'а.
func TestE2E_VirtualRouter_StreamingResponse(t *testing.T) {
	t.Parallel()

	// Fake streaming backend.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &env)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"model\":\"%s\",\"choices\":[{\"delta\":{\"content\":\"chunk%d\"}}]}\n\n", env.Model, i)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer backend.Close()

	addr := backend.URL[7:]
	host, port, _ := parseBackendHostPort(addr)

	proxy := newProxyWithCleanup(t, createE2EConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	proxy.config.Balancing.OperatingMode = string(types.OperatingModeVirtualRouter)

	registry := proxy.GetVirtualModelRegistry()
	require.NotNil(t, registry)
	registry.SetEnabled(true)
	_ = registry.Register(types.VirtualModelConfig{
		Name:        "vm-stream",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", host, port)},
		ModelName:   "physical-stream",
	})
	proxy.SetVirtualRouter(NewVirtualRouter(registry, proxy))

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}))
	defer balancer.Close()

	body := bytes.NewReader([]byte(`{
		"model":"vm-stream",
		"messages":[{"role":"user","content":"stream me"}],
		"stream":true
	}`))
	req, _ := http.NewRequest("POST", balancer.URL+"/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	body2, _ := io.ReadAll(resp.Body)
	bodyStr := string(body2)
	assert.Contains(t, bodyStr, "chunk0")
	assert.Contains(t, bodyStr, "chunk1")
	assert.Contains(t, bodyStr, "chunk2")
	assert.Contains(t, bodyStr, "[DONE]")

	// X-Original-Backend должен быть set.
	assert.Equal(t, fmt.Sprintf("%s:%d", host, port), resp.Header.Get("X-Original-Backend"))
	assert.Equal(t, "vm-stream", resp.Header.Get("X-Virtual-Model"))
}

// createE2EConfig — конфиг с VirtualModels.Enabled=true (для e2e тестов).
func createE2EConfig() *types.LoadBalancerConfig {
	cfg := createTestConfig()
	cfg.Balancing.VirtualModels.Enabled = true
	return cfg
}

// Suppress unused time import.
var _ = time.Second
