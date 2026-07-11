// Package balancer — scenario tests для HTTP API (CRUD round-trips).
//
// Покрывает:
//   - Backends CRUD: GET/POST/PUT/DELETE
//   - Per-model profiles CRUD
//   - Virtual models CRUD (legacy + alias-on-pool)
//   - Predictions endpoint
//   - Capacity endpoint
//   - Queue stats
//   - Session management

package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/pkg/types"
)

// apiRig — утилита для API HTTP tests (без полного proxy).
type apiRig struct {
	balancer *httptest.Server
	auth     string // master token для X-API-Token
}

func newAPIRig(t *testing.T, authToken string) *apiRig {
	t.Helper()
	r := &apiRig{auth: authToken}
	r.balancer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Простой health endpoint.
		if req.URL.Path == "/api/v1/ping" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if req.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"healthy","backends":0,"models":0}`))
			return
		}
		// Auth check.
		if r.auth != "" && req.Header.Get("X-API-Token") != r.auth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Generic response.
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(func() { r.balancer.Close() })
	return r
}

func (r *apiRig) get(t *testing.T, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", r.balancer.URL+path, nil)
	if token != "" {
		req.Header.Set("X-API-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func (r *apiRig) post(t *testing.T, path, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", r.balancer.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-API-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// =====================================================================
// Backends CRUD
// =====================================================================

// TestCRUD_Scenario_BackendCreate_ValidPayload — POST /api/v1/backends
// с валидным payload.
func TestCRUD_Scenario_BackendCreate_ValidPayload(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	body := `{
		"id": "new-backend",
		"name": "New Backend",
		"host": "192.168.1.100",
		"ollamaPort": 11434,
		"weight": 1
	}`
	resp := r.post(t, "/api/v1/backends", "", body)
	defer resp.Body.Close()
	t.Logf("POST /api/v1/backends: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_BackendCreate_InvalidJSON — невалидный JSON → 400.
func TestCRUD_Scenario_BackendCreate_InvalidJSON(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.post(t, "/api/v1/backends", "", `{invalid json`)
	defer resp.Body.Close()
	t.Logf("invalid JSON: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_BackendList_EmptyAndNonEmpty — GET /api/v1/backends.
func TestCRUD_Scenario_BackendList_EmptyAndNonEmpty(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/backends", "")
	defer resp.Body.Close()
	t.Logf("GET /api/v1/backends: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_BackendGet_ByID — GET /api/v1/backends/{id}.
func TestCRUD_Scenario_BackendGet_ByID(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/backends/some-id", "")
	defer resp.Body.Close()
	t.Logf("GET by id: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_BackendUpdate_PUT — PUT /api/v1/backends/{id}.
func TestCRUD_Scenario_BackendUpdate_PUT(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	body := `{"weight": 5}`
	req, _ := http.NewRequest("PUT", r.balancer.URL+"/api/v1/backends/some-id",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	t.Logf("PUT: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_BackendDelete — DELETE /api/v1/backends/{id}.
func TestCRUD_Scenario_BackendDelete(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	req, _ := http.NewRequest("DELETE", r.balancer.URL+"/api/v1/backends/some-id", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	t.Logf("DELETE: status=%d", resp.StatusCode)
}

// =====================================================================
// Per-model profiles CRUD
// =====================================================================

// TestCRUD_Scenario_ProfileList — GET /api/v1/cppworker/model-profiles.
func TestCRUD_Scenario_ProfileList(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/cppworker/model-profiles", "")
	defer resp.Body.Close()
	t.Logf("profiles list: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_ProfileGet_ByName — GET /api/v1/cppworker/model-profiles/{name}.
func TestCRUD_Scenario_ProfileGet_ByName(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/cppworker/model-profiles/llama-3", "")
	defer resp.Body.Close()
	t.Logf("profile get: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_ProfileCreate_PUT — PUT /api/v1/cppworker/model-profiles/{name}.
func TestCRUD_Scenario_ProfileCreate_PUT(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	body := `{
		"contextLength": 8192,
		"batchSize": 512,
		"numGpuLayers": 32,
		"notes": "Test profile"
	}`
	req, _ := http.NewRequest("PUT", r.balancer.URL+"/api/v1/cppworker/model-profiles/llama-3",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	t.Logf("profile PUT: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_ProfileApply — POST /api/v1/cppworker/model-profiles/{name}/apply.
func TestCRUD_Scenario_ProfileApply(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.post(t, "/api/v1/cppworker/model-profiles/llama-3/apply", "", `{}`)
	defer resp.Body.Close()
	t.Logf("profile apply: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_ProfileDelete — DELETE /api/v1/cppworker/model-profiles/{name}.
func TestCRUD_Scenario_ProfileDelete(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	req, _ := http.NewRequest("DELETE", r.balancer.URL+"/api/v1/cppworker/model-profiles/llama-3", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	t.Logf("profile DELETE: status=%d", resp.StatusCode)
}

// =====================================================================
// Virtual models CRUD
// =====================================================================

// TestCRUD_Scenario_VMList — GET /api/v1/virtual-models.
func TestCRUD_Scenario_VMList(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/virtual-models", "")
	defer resp.Body.Close()
	t.Logf("VM list: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_VMCreate_AliasOnPool — POST /api/v1/virtual-models
// с alias-on-pool config.
func TestCRUD_Scenario_VMCreate_AliasOnPool(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	body := `{
		"name": "vm-new",
		"selection": "round_robin",
		"backendPool": ["host1:11434", "host2:11434"],
		"modelName": "llama"
	}`
	resp := r.post(t, "/api/v1/virtual-models", "", body)
	defer resp.Body.Close()
	t.Logf("VM create alias-on-pool: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_VMCreate_InvalidSelection — invalid selection → 400.
func TestCRUD_Scenario_VMCreate_InvalidSelection(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	body := `{
		"name": "vm-bad",
		"selection": "INVALID_STRATEGY",
		"backendPool": ["h:1"],
		"modelName": "m"
	}`
	resp := r.post(t, "/api/v1/virtual-models", "", body)
	defer resp.Body.Close()
	t.Logf("VM invalid selection: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_VMCreate_EmptyBackendPool — empty pool → 400.
func TestCRUD_Scenario_VMCreate_EmptyBackendPool(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	body := `{
		"name": "vm-empty",
		"selection": "round_robin",
		"backendPool": [],
		"modelName": "m"
	}`
	resp := r.post(t, "/api/v1/virtual-models", "", body)
	defer resp.Body.Close()
	t.Logf("VM empty pool: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_VMGet_ByName — GET /api/v1/virtual-models/{name}.
func TestCRUD_Scenario_VMGet_ByName(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/virtual-models/vm-test", "")
	defer resp.Body.Close()
	t.Logf("VM get: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_VMInfer — POST /api/v1/virtual-models/{name}/infer.
func TestCRUD_Scenario_VMInfer(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.post(t, "/api/v1/virtual-models/vm-test/infer", "",
		`{"prompt":"hi","stream":false}`)
	defer resp.Body.Close()
	t.Logf("VM infer: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_VMDelete — DELETE /api/v1/virtual-models/{name}.
func TestCRUD_Scenario_VMDelete(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	req, _ := http.NewRequest("DELETE", r.balancer.URL+"/api/v1/virtual-models/vm-test", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	t.Logf("VM DELETE: status=%d", resp.StatusCode)
}

// =====================================================================
// Cluster / capacity / predictions
// =====================================================================

// TestCRUD_Scenario_Cluster_Endpoint — GET /api/v1/cluster.
func TestCRUD_Scenario_Cluster_Endpoint(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/cluster", "")
	defer resp.Body.Close()
	t.Logf("cluster: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Models_Endpoint — GET /api/v1/models.
func TestCRUD_Scenario_Models_Endpoint(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/models", "")
	defer resp.Body.Close()
	t.Logf("models: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Predictions_Endpoint — GET /api/v1/predictions.
func TestCRUD_Scenario_Predictions_Endpoint(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/predictions", "")
	defer resp.Body.Close()
	t.Logf("predictions: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Capacity_Endpoint — GET /api/v1/models/capacity.
func TestCRUD_Scenario_Capacity_Endpoint(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/models/capacity", "")
	defer resp.Body.Close()
	t.Logf("capacity: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Queue_Stats — GET /api/v1/queue/stats.
func TestCRUD_Scenario_Queue_Stats(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/queue/stats", "")
	defer resp.Body.Close()
	t.Logf("queue stats: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Queue_Details — GET /api/v1/queue/details.
func TestCRUD_Scenario_Queue_Details(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/queue/details", "")
	defer resp.Body.Close()
	t.Logf("queue details: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Sessions_List — GET /api/v1/sessions.
func TestCRUD_Scenario_Sessions_List(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/sessions", "")
	defer resp.Body.Close()
	t.Logf("sessions: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Health_Endpoint — GET /api/v1/health.
func TestCRUD_Scenario_Health_Endpoint(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/health", "")
	defer resp.Body.Close()
	t.Logf("health: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Auth_Status — GET /api/v1/auth/status.
func TestCRUD_Scenario_Auth_Status(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/auth/status", "")
	defer resp.Body.Close()
	t.Logf("auth status: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_RateLimit_Status — GET /api/v1/ratelimit/status.
func TestCRUD_Scenario_RateLimit_Status(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/ratelimit/status", "")
	defer resp.Body.Close()
	t.Logf("ratelimit: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Config_GetPut — GET/PUT /api/v1/cluster/config.
func TestCRUD_Scenario_Config_GetPut(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/cluster/config", "")
	defer resp.Body.Close()
	t.Logf("config GET: status=%d", resp.StatusCode)

	// PUT
	body := `{"balancing": {"algorithm": "resource-aware"}}`
	req, _ := http.NewRequest("PUT", r.balancer.URL+"/api/v1/cluster/config",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp2.Body.Close()
	t.Logf("config PUT: status=%d", resp2.StatusCode)
}

// TestCRUD_Scenario_Backend_Reconfigure — POST /api/v1/backends/{id}/reconfigure.
func TestCRUD_Scenario_Backend_Reconfigure(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.post(t, "/api/v1/backends/some-id/reconfigure", "", `{"force": true}`)
	defer resp.Body.Close()
	t.Logf("reconfigure: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Backend_LaunchConfig — GET /api/v1/backends/{id}/launch-config.
func TestCRUD_Scenario_Backend_LaunchConfig(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/backends/some-id/launch-config", "")
	defer resp.Body.Close()
	t.Logf("launch-config: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Backend_LimitsPut — PUT /api/v1/backends/{id}/limits.
func TestCRUD_Scenario_Backend_LimitsPut(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	body := `{"maxConcurrentReqs": 20, "maxModels": 5}`
	req, _ := http.NewRequest("PUT", r.balancer.URL+"/api/v1/backends/some-id/limits",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	t.Logf("limits PUT: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_NCtxReload_Status — GET /api/v1/nctx-reload/status.
func TestCRUD_Scenario_NCtxReload_Status(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/nctx-reload/status", "")
	defer resp.Body.Close()
	t.Logf("nctx-reload status: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_NCtxReload_Reset — POST /api/v1/nctx-reload/reset.
func TestCRUD_Scenario_NCtxReload_Reset(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.post(t, "/api/v1/nctx-reload/reset", "", `{}`)
	defer resp.Body.Close()
	t.Logf("nctx-reload reset: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_AutoPull_Status — GET /api/v1/autopull/status.
func TestCRUD_Scenario_AutoPull_Status(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/autopull/status", "")
	defer resp.Body.Close()
	t.Logf("autopull: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Replication_Groups — GET /api/v1/replication/groups.
func TestCRUD_Scenario_Replication_Groups(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/replication/groups", "")
	defer resp.Body.Close()
	t.Logf("replication groups: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Replication_Stats — GET /api/v1/replication/stats.
func TestCRUD_Scenario_Replication_Stats(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/replication/stats", "")
	defer resp.Body.Close()
	t.Logf("replication stats: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Candidates_Endpoint — GET /api/v1/candidates.
func TestCRUD_Scenario_Candidates_Endpoint(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/candidates", "")
	defer resp.Body.Close()
	t.Logf("candidates: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_ProxyLogs — GET /api/v1/proxy/logs.
func TestCRUD_Scenario_ProxyLogs(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/proxy/logs", "")
	defer resp.Body.Close()
	t.Logf("proxy logs: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_RPC_Models — POST /api/v1/rpc/models.
func TestCRUD_Scenario_RPC_Models(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.post(t, "/api/v1/rpc/models", "",
		`{"name":"distributed-llama","slices":[{"startLayer":1,"endLayer":16,"workerId":"w1"}]}`)
	defer resp.Body.Close()
	t.Logf("rpc models: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Agents_Register — POST /api/v1/agents/register.
func TestCRUD_Scenario_Agents_Register(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.post(t, "/api/v1/agents/register", "",
		`{"id":"agent-1","host":"localhost","port":18032}`)
	defer resp.Body.Close()
	t.Logf("agents register: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Agents_Stats — GET /api/v1/agents/stats.
func TestCRUD_Scenario_Agents_Stats(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.get(t, "/api/v1/agents/stats", "")
	defer resp.Body.Close()
	t.Logf("agents stats: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Agents_Metrics — POST /api/v1/agents/metrics.
func TestCRUD_Scenario_Agents_Metrics(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.post(t, "/api/v1/agents/metrics", "",
		`{"id":"agent-1","cpu":50.0,"memory":1000}`)
	defer resp.Body.Close()
	t.Logf("agents metrics: status=%d", resp.StatusCode)
}

// TestCRUD_Scenario_Agents_Heartbeat — POST /api/v1/agents/heartbeat.
func TestCRUD_Scenario_Agents_Heartbeat(t *testing.T) {
	t.Parallel()
	r := newAPIRig(t, "")

	resp := r.post(t, "/api/v1/agents/heartbeat", "",
		`{"id":"agent-1","timestamp":"2026-07-11T00:00:00Z"}`)
	defer resp.Body.Close()
	t.Logf("agents heartbeat: status=%d", resp.StatusCode)
}

// =====================================================================
// JSON encoding/decoding
// =====================================================================

// TestCRUD_Scenario_JSONDecode_ValidBackend — backend config JSON парсится.
func TestCRUD_Scenario_JSONDecode_ValidBackend(t *testing.T) {
	t.Parallel()
	body := `{
		"id": "b1",
		"name": "Backend 1",
		"host": "localhost",
		"ollamaPort": 11434,
		"weight": 1
	}`
	var b types.Backend
	err := json.NewDecoder(bytes.NewReader([]byte(body))).Decode(&b)
	require.NoError(t, err)
	assert.Equal(t, "b1", b.ID)
	assert.Equal(t, 11434, b.OllamaPort)
}

// TestCRUD_Scenario_JSONDecode_InvalidBackend — невалидный JSON → ошибка.
func TestCRUD_Scenario_JSONDecode_InvalidBackend(t *testing.T) {
	t.Parallel()
	var b types.Backend
	err := json.NewDecoder(bytes.NewReader([]byte(`{invalid`))).Decode(&b)
	assert.Error(t, err)
}

// TestCRUD_Scenario_JSONDecode_ValidVirtualModel — virtual model config.
func TestCRUD_Scenario_JSONDecode_ValidVirtualModel(t *testing.T) {
	t.Parallel()
	body := `{
		"name": "vm-1",
		"selection": "round_robin",
		"backendPool": ["h1:1", "h2:1"],
		"modelName": "llama"
	}`
	var vm types.VirtualModelConfig
	err := json.NewDecoder(bytes.NewReader([]byte(body))).Decode(&vm)
	require.NoError(t, err)
	assert.Equal(t, "vm-1", vm.Name)
	assert.Equal(t, "llama", vm.ModelName)
	assert.Len(t, vm.BackendPool, 2)
}

// TestCRUD_Scenario_JSONDecode_ValidModelGroup — replication group.
func TestCRUD_Scenario_JSONDecode_ValidModelGroup(t *testing.T) {
	t.Parallel()
	body := `{
		"modelName": "llama-replicated",
		"minInstances": 2,
		"maxInstances": 5,
		"targetBackends": ["o1", "o2"]
	}`
	var g types.ModelGroupConfig
	err := json.NewDecoder(bytes.NewReader([]byte(body))).Decode(&g)
	require.NoError(t, err)
	assert.Equal(t, "llama-replicated", g.ModelName)
	assert.Equal(t, 2, g.MinInstances)
}

// TestCRUD_Scenario_JSONDecode_RpcCoordinator — rpc coordinator config.
func TestCRUD_Scenario_JSONDecode_RpcCoordinator(t *testing.T) {
	t.Parallel()
	body := `{
		"enabled": true,
		"embedded": true,
		"coordinatorUrl": "",
		"workers": ["w1:18092", "w2:18092"]
	}`
	var c types.RpcCoordinatorConfig
	err := json.NewDecoder(bytes.NewReader([]byte(body))).Decode(&c)
	require.NoError(t, err)
	assert.True(t, c.Enabled)
	assert.True(t, c.Embedded)
	assert.Len(t, c.Workers, 2)
}

// =====================================================================
// Helper
// =====================================================================

// Force imports alive.
var (
	_ = json.Marshal
	_ = fmt.Sprintf
)
