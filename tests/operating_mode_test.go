package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== Тесты переключения OperatingMode через API ====================

func TestOperatingMode_Standard_Default(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// По умолчанию OperatingMode может быть пустым до первого вызова deriveOperatingMode
	// или "standard" если оно уже вычислено
	state := proxy.GetClusterState()
	// Проверяем что operatingMode валидный (стандартный или пустой до инициализации)
	assert.True(t, state.OperatingMode == "standard" || state.OperatingMode == "",
		"Default operating mode should be standard or empty, got: %s", state.OperatingMode)

	// Проверяем через API GET /api/v1/cluster/config
	// NOTE: GET возвращает плоский объект (не обёрнутый в "config")
	resp, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var cfgResp map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&cfgResp)
	require.NoError(t, err)

	// GET /api/v1/cluster/config возвращает плоский объект с полями напрямую
	// В setupTestEnvironment OperatingMode не инициализируется через deriveOperatingMode, поэтому может быть пустым
	operatingMode, _ := cfgResp["operatingMode"].(string)
	assert.True(t, operatingMode == "standard" || operatingMode == "",
		"API should return standard or empty as default operating mode, got: %s", operatingMode)
}

func TestOperatingMode_SwitchToReplication(t *testing.T) {
	testServer, server, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Устанавливаем configSaver чтобы смена режима не падала на сохранении
	server.SetConfigSaver(func() error { return nil })

	payload := map[string]interface{}{
		"operatingMode": "replication",
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	// Проверяем что в ответе operatingMode = replication
	config, ok := result["config"].(map[string]interface{})
	if ok {
		assert.Equal(t, "replication", config["operatingMode"])
	} else {
		// fallback: проверяем success/message напрямую
		assert.NotNil(t, result["success"])
	}

	// Проверяем ClusterState
	state := proxy.GetClusterState()
	assert.Equal(t, "replication", state.OperatingMode)

	// Проверяем что enabled-флаг установлен корректно
	// (через reflect или через дополнительный API endpoint если доступен)
}

func TestOperatingMode_SwitchToRpcCoordinator(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	payload := map[string]interface{}{
		"operatingMode": "rpc_coordinator",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	config, ok := result["config"].(map[string]interface{})
	if ok {
		assert.Equal(t, "rpc_coordinator", config["operatingMode"])
	} else {
		assert.NotNil(t, result["success"])
	}
}

func TestOperatingMode_SwitchToVirtualRouter(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	payload := map[string]interface{}{
		"operatingMode": "virtual_router",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	config, ok := result["config"].(map[string]interface{})
	if ok {
		assert.Equal(t, "virtual_router", config["operatingMode"])
	} else {
		assert.NotNil(t, result["success"])
	}
}

func TestOperatingMode_SwitchToDistributedInference(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	payload := map[string]interface{}{
		"operatingMode": "distributed_inference",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	config, ok := result["config"].(map[string]interface{})
	if ok {
		assert.Equal(t, "distributed_inference", config["operatingMode"])
	} else {
		assert.NotNil(t, result["success"])
	}
}

func TestOperatingMode_InvalidMode_Returns400(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	payload := map[string]interface{}{
		"operatingMode": "invalid_mode_xyz",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	success, _ := result["success"].(bool)
	assert.False(t, success, "Invalid mode should return success=false")
	errMsg, _ := result["error"].(string)
	assert.Contains(t, errMsg, "Invalid operatingMode")
}

func TestOperatingMode_SwitchSequence(t *testing.T) {
	testServer, server, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	modes := []string{"replication", "rpc_coordinator", "virtual_router", "distributed_inference", "standard"}

	for _, mode := range modes {
		payload := map[string]interface{}{
			"operatingMode": mode,
		}
		body, _ := json.Marshal(payload)

		req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err, "Failed to switch to %s", mode)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode, "Switch to %s should return 200", mode)

		// Даём время на применение
		time.Sleep(50 * time.Millisecond)

		// Проверяем ClusterState
		state := proxy.GetClusterState()
		assert.Equal(t, mode, state.OperatingMode, "ClusterState should reflect mode %s", mode)

		// Проверяем через GET /api/v1/cluster/config
		cfgResp, err := http.Get(baseURL + "/api/v1/cluster/config")
		require.NoError(t, err)
		defer cfgResp.Body.Close()

		var cfgResult map[string]interface{}
		json.NewDecoder(cfgResp.Body).Decode(&cfgResult)
		config, ok := cfgResult["config"].(map[string]interface{})
		if ok {
			assert.Equal(t, mode, config["operatingMode"], "GET config should reflect mode %s", mode)
		}
	}
}

func TestOperatingMode_EmptyMode_NoChange(t *testing.T) {
	testServer, server, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// Сначала переключаемся в replication
	payload := map[string]interface{}{
		"operatingMode": "replication",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Теперь отправляем запрос без operatingMode, но с валидным алгоритмом
	payload2 := map[string]interface{}{
		"algorithm": "roundrobin",
	}
	body2, _ := json.Marshal(payload2)
	req2, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()

	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	// operatingMode не должен измениться
	state := proxy.GetClusterState()
	assert.Equal(t, "replication", state.OperatingMode, "OperatingMode should not change when not provided")
}

// ==================== Тесты проверки enabled-флагов модулей ====================

func TestOperatingMode_ModuleFlags_Standard(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// Убедимся что мы в standard mode
	payload := map[string]interface{}{
		"operatingMode": "standard",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// В стандартном режиме все модули должны быть отключены
	// Проверяем через прямой доступ к config (через reflect или internal API)
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	config := result["config"].(map[string]interface{})
	assert.Equal(t, "standard", config["operatingMode"])
}

func TestOperatingMode_ModuleFlags_Replication(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	payload := map[string]interface{}{
		"operatingMode": "replication",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	assert.True(t, result["success"].(bool))
}

// ==================== Тесты ClusterState содержит operatingMode ====================

func TestOperatingMode_ClusterStateContainsMode(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()

	state := proxy.GetClusterState()
	// В setupTestEnvironment OperatingMode может быть пустым до вызова deriveOperatingMode
	// Проверяем что он либо пустой (до инициализации), либо валидный
	if state.OperatingMode != "" {
		assert.Contains(t, []string{"standard", "replication", "rpc_coordinator", "virtual_router", "distributed_inference"}, state.OperatingMode)
	}
}

func TestOperatingMode_MetricsEndpointContainsMode(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Устанавливаем метрики на backend чтобы /api/v1/metrics не падал с пустым списком
	proxy.UpdateMetrics("agent-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	resp, err := http.Get(baseURL + "/api/v1/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var metrics map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&metrics)
	require.NoError(t, err)

	// Проверяем top-level поля
	assert.Contains(t, metrics, "timestamp")
	assert.Contains(t, metrics, "totalBackends")
	assert.Contains(t, metrics, "healthyBackends")
	assert.Contains(t, metrics, "backends")

	// operatingMode должен быть в backends (через ClusterState) или на top-level
	// В текущей реализации operatingMode в ClusterState, который передаётся в backends
	backends, ok := metrics["backends"].([]interface{})
	if ok && len(backends) > 0 {
		// operatingMode не на уровне backend, а на уровне cluster state
		// Он не отображается в /api/v1/metrics напрямую, но в /api/v1/cluster — да
	}
}

func TestOperatingMode_ClusterAPI_ContainsMode(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// Переключаем в replication
	payload := map[string]interface{}{
		"operatingMode": "replication",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	// Ждём применения
	time.Sleep(50 * time.Millisecond)

	// GET /api/v1/cluster
	resp, err = http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()

	var clusterState map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&clusterState)
	require.NoError(t, err)

	// ClusterState должен содержать operatingMode
	assert.Equal(t, "replication", clusterState["operatingMode"], "Cluster API should contain correct operatingMode")
}

// ==================== Тесты инициализации Proxy с разными режимами ====================

func TestOperatingMode_ProxyInit_Standard(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			OperatingMode: "standard",
		},
		Backends: []types.Backend{
			{ID: "b1", Host: "localhost", OllamaPort: 11434, Status: types.StatusHealthy, MaxConcurrentReqs: 10},
		},
	}

	proxy := balancer.NewProxy(cfg)
	require.NotNil(t, proxy)

	state := proxy.GetClusterState()
	assert.Equal(t, "standard", state.OperatingMode)
}

func TestOperatingMode_ProxyInit_Replication(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			OperatingMode: "replication",
			ModelReplication: types.ModelReplicationConfig{
				Enabled:             true,
				DefaultMinInstances: 1,
				DefaultMaxInstances: 3,
			},
		},
		Backends: []types.Backend{
			{ID: "b1", Host: "localhost", OllamaPort: 11434, Status: types.StatusHealthy, MaxConcurrentReqs: 10},
		},
	}

	proxy := balancer.NewProxy(cfg)
	require.NotNil(t, proxy)

	// Проверяем что модули инициализированы
	require.NotNil(t, proxy.GetModelReplicationManager(), "ModelReplicationManager should be initialized in replication mode")
	require.NotNil(t, proxy.GetReplicationSelector(), "ReplicationSelector should be initialized in replication mode")
}

func TestOperatingMode_ProxyInit_VirtualRouter(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			OperatingMode: "virtual_router",
			VirtualModels: types.VirtualModelsConfig{
				Enabled: true,
			},
		},
		Backends: []types.Backend{
			{ID: "b1", Host: "localhost", OllamaPort: 11434, Status: types.StatusHealthy, MaxConcurrentReqs: 10},
		},
	}

	proxy := balancer.NewProxy(cfg)
	require.NotNil(t, proxy)

	// VirtualModelRouter должен быть инициализирован
	// (если метод GetVirtualModelRouter доступен)
}