package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// Helper: создаёт реальный *balancer.Proxy для тестов CRUD.
// =====================================================================

func newCRUDTestProxy(t *testing.T) *balancer.Proxy {
	t.Helper()
	config := createTestCRUDConfig()
	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	// queueMgr cleanup not exposed; tests are short-lived so this is OK.
	return proxy
}

func createTestCRUDConfig() *types.LoadBalancerConfig {
	return &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:         "test-backend",
				Name:       "test",
				Host:       "localhost",
				OllamaPort: 11434,
				AgentPort:  9090,
				Weight:     1,
				Status:     types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:       types.AlgorithmRoundRobin,
			HealthCheckInterval: 60,
			VirtualModels: types.VirtualModelsConfig{
				Enabled: true,
			},
		},
		Resources: types.ResourceLimits{
			GPU:   types.GPULimits{MaxUsagePercent: 90},
			CPU:   types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:  types.DiskLimits{MinFreeMB: 1000},
		},
	}
}

// =====================================================================
// Tests
// =====================================================================

// newCRUDTestServer — создаёт минимальный Server с proxy + init'нутым mux.
func newCRUDTestServer(proxy *balancer.Proxy) *Server {
	s := &Server{
		proxy: proxy,
		// Disabled authenticator (no token required). enabled=false → middleware pass-through.
		authenticator: NewTokenAuthenticator(nil, "X-API-Token", false),
		// High limit (1000 burst, 1000/sec) so tests don't get rate-limited.
		rateLimiter: NewRateLimiter(1000, 1000),
		mux:         http.NewServeMux(),
	}
	s.setupRoutes()
	return s
}

// Test 1: GET /api/v1/virtual-models with no VMs → empty list.
func TestVirtualModelsCRUD_List_Empty(t *testing.T) {
	t.Parallel()
	proxy := newCRUDTestProxy(t)
	s := newCRUDTestServer(proxy)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/virtual-models", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"enabled": true`)
	assert.Contains(t, body, `"count": 0`)
}

// Test 2: POST /api/v1/virtual-models — create new VM.
func TestVirtualModelsCRUD_Create_Success(t *testing.T) {
	t.Parallel()
	proxy := newCRUDTestProxy(t)
	s := newCRUDTestServer(proxy)

	createBody := `{
		"name": "vm-test",
		"description": "Test VM",
		"selection": "round_robin",
		"backendPool": ["127.0.0.1:11434", "127.0.0.1:11435"],
		"modelName": "llama-70b"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/virtual-models",
		bytes.NewReader([]byte(createBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "vm-test", resp["name"])
	assert.Equal(t, "llama-70b", resp["modelName"])
	assert.Equal(t, "alias_on_pool", resp["mode"])
	assert.Equal(t, "round_robin", resp["selection"])

	// Verify registry was populated.
	registry := proxy.GetVirtualModelRegistry()
	require.NotNil(t, registry)
	vm := registry.Get("vm-test")
	require.NotNil(t, vm, "VM should be registered")
	assert.Equal(t, "llama-70b", vm.Config.ModelName)
	assert.True(t, registry.IsEnabled(), "registry should auto-enable on first register")
}

// Test 3: POST /api/v1/virtual-models with validation errors.
func TestVirtualModelsCRUD_Create_ValidationErrors(t *testing.T) {
	t.Parallel()
	proxy := newCRUDTestProxy(t)
	s := newCRUDTestServer(proxy)

	tests := []struct {
		name string
		body string
	}{
		{"missing name", `{"modelName":"x","backendPool":["a:1"]}`},
		{"missing modelName", `{"name":"x","backendPool":["a:1"]}`},
		{"empty backendPool", `{"name":"x","modelName":"y","backendPool":[]}`},
		{"invalid backend address", `{"name":"x","modelName":"y","backendPool":["invalid-no-port"]}`},
		{"invalid selection", `{"name":"x","modelName":"y","backendPool":["a:1"],"selection":"unknown"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/virtual-models",
				bytes.NewReader([]byte(tt.body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code,
				"body: %s", w.Body.String())
		})
	}
}

// Test 4: GET /api/v1/virtual-models/{name} — get details.
func TestVirtualModelsCRUD_Get_Success(t *testing.T) {
	t.Parallel()
	proxy := newCRUDTestProxy(t)
	// Pre-register a VM.
	registry := proxy.GetVirtualModelRegistry()
	require.NoError(t, registry.Register(types.VirtualModelConfig{
		Name:        "vm-get",
		Description: "For get test",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{"127.0.0.1:11434"},
		ModelName:   "llama",
	}))

	s := newCRUDTestServer(proxy)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/virtual-models/vm-get", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "vm-get", resp["name"])
}

// Test 5: GET /api/v1/virtual-models/{nonexistent} → 404.
func TestVirtualModelsCRUD_Get_NotFound(t *testing.T) {
	t.Parallel()
	proxy := newCRUDTestProxy(t)
	s := newCRUDTestServer(proxy)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/virtual-models/nonexistent", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// Test 6: DELETE /api/v1/virtual-models/{name} — unregister.
func TestVirtualModelsCRUD_Delete_Success(t *testing.T) {
	t.Parallel()
	proxy := newCRUDTestProxy(t)
	registry := proxy.GetVirtualModelRegistry()
	require.NoError(t, registry.Register(types.VirtualModelConfig{
		Name:        "vm-del",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{"127.0.0.1:11434"},
		ModelName:   "llama",
	}))

	s := newCRUDTestServer(proxy)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/virtual-models/vm-del", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Nil(t, registry.Get("vm-del"), "VM should be removed from registry")
}

// Test 7: DELETE /api/v1/virtual-models/{nonexistent} → 404.
func TestVirtualModelsCRUD_Delete_NotFound(t *testing.T) {
	t.Parallel()
	proxy := newCRUDTestProxy(t)
	s := newCRUDTestServer(proxy)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/virtual-models/nonexistent", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// Test 8: Invalid method on collection (PUT) → 405.
func TestVirtualModelsCRUD_InvalidMethod(t *testing.T) {
	t.Parallel()
	proxy := newCRUDTestProxy(t)
	s := newCRUDTestServer(proxy)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/virtual-models", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// Test 9: Full lifecycle: Create → Get → List → Delete → 404.
func TestVirtualModelsCRUD_FullLifecycle(t *testing.T) {
	t.Parallel()
	proxy := newCRUDTestProxy(t)
	s := newCRUDTestServer(proxy)

	// 1. Create.
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/virtual-models",
		bytes.NewReader([]byte(`{
			"name":"vm-lifecycle",
			"description":"full lifecycle test",
			"selection":"least_loaded",
			"backendPool":["a:1","b:2","c:3"],
			"modelName":"big-model"
		}`)))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, createReq)
	require.Equal(t, http.StatusCreated, w.Code, "create: %s", w.Body.String())

	// 2. Get.
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/virtual-models/vm-lifecycle", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, getReq)
	require.Equal(t, http.StatusOK, w.Code, "get: %s", w.Body.String())

	// 3. List.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/virtual-models", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, listReq)
	require.Equal(t, http.StatusOK, w.Code)
	var listResp struct {
		Count   int                      `json:"count"`
		Models  []map[string]interface{} `json:"models"`
		Enabled bool                     `json:"enabled"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listResp))
	assert.Equal(t, 1, listResp.Count)
	assert.True(t, listResp.Enabled)
	assert.Equal(t, "vm-lifecycle", listResp.Models[0]["name"])

	// 4. Delete.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/virtual-models/vm-lifecycle", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, delReq)
	require.Equal(t, http.StatusOK, w.Code, "delete: %s", w.Body.String())

	// 5. Get after delete → 404.
	getReq = httptest.NewRequest(http.MethodGet, "/api/v1/virtual-models/vm-lifecycle", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, getReq)
	assert.Equal(t, http.StatusNotFound, w.Code, "should be 404 after delete")
}

// Test 10: Alias-on-pool mode toggle.
func TestVirtualModelsCRUD_PipelineMode_AliasOnPoolToggle(t *testing.T) {
	t.Parallel()
	proxy := newCRUDTestProxy(t)
	s := newCRUDTestServer(proxy)

	// Pipeline mode (Slices instead of BackendPool) — should be stored as
	// pipeline mode (mode != "alias_on_pool" в response).
	createBody := `{
		"name": "vm-pipeline",
		"description": "Pipeline mode VM",
		"slices": [
			{"id":"s1","modelName":"emb","ordinal":0,"targetBackends":["a:1"]}
		],
		"coordination": {"mode":"sequential", "timeoutMs": 30000}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/virtual-models",
		bytes.NewReader([]byte(createBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "pipeline", resp["mode"], "should be 'pipeline' mode when Slices provided")
}
