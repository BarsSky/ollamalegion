package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/api"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================
// Тесты WebUI: корректное отображение llama.cpp параметров
// без перезаписи конфигурации балансера
// ============================================================

// setupWebUITestServer создаёт API сервер с заданным engine для тестов WebUI
func setupWebUITestServer(t *testing.T, engine types.BackendEngine, mode string) (*httptest.Server, *api.Server, *balancer.Proxy) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    18080,
			APIPort: 18081,
		},
		BackendEngine: engine,
		Balancing: types.BalancingSettings{
			Algorithm:          types.AlgorithmResourceAware,
			ModelAffinity:      true,
			SessionStickiness:  true,
			UseEnhancedScoring: true,
			OperatingMode:      mode,
			QueueMaxSize:       100,
			QueueTimeout:       60,
		},
		Backends: []types.Backend{
			{
				ID:                "llamacpp-1",
				Name:              "LlamaCpp Node",
				Host:              "10.0.1.1",
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     18091,
				OllamaPort:        11434,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
			{
				ID:                "ollama-1",
				Name:              "Ollama Node",
				Host:              "10.0.0.1",
				Type:              types.BackendTypeOllama,
				OllamaPort:        11434,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 85},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
		},
	}

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, config, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	ts := httptest.NewServer(server)
	return ts, server, proxy
}

// TestWebUI_ShowsBalancerEngineNotLocal проверяет что WebUI получает
// backendEngine и operatingMode от балансера, а не хардкодит на клиенте
func TestWebUI_ShowsBalancerEngineNotLocal(t *testing.T) {
	t.Run("llama_cpp_engine", func(t *testing.T) {
		ts, _, proxy := setupWebUITestServer(t, types.EngineLlamaCPP, "virtual_router")
		defer ts.Close()

		// Запрос конфигурации (как это делает WebUI)
		resp, err := http.Get(ts.URL + "/api/v1/cluster/config")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var cfg map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&cfg)
		require.NoError(t, err)

		// Проверяем что backendEngine в ответе соответствует балансеру
		assert.Equal(t, "llamacpp", cfg["backendEngine"],
			"WebUI should display balancer's backendEngine, not a hardcoded default")

		// Проверяем operatingMode
		assert.Equal(t, "virtual_router", cfg["operatingMode"],
			"WebUI should display balancer's operatingMode")

		// Проверяем что ClusterState тоже отдаёт правильные значения
		state := proxy.GetClusterState()
		assert.Equal(t, "llamacpp", state.BackendEngine)
		assert.Equal(t, "virtual_router", state.OperatingMode)
	})

	t.Run("ollama_engine", func(t *testing.T) {
		ts, _, proxy := setupWebUITestServer(t, types.EngineOllamaAPI, "standard")
		defer ts.Close()

		resp, err := http.Get(ts.URL + "/api/v1/cluster/config")
		require.NoError(t, err)
		defer resp.Body.Close()

		var cfg map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&cfg)

		assert.Equal(t, "ollama_api", cfg["backendEngine"])
		assert.Equal(t, "standard", cfg["operatingMode"])

		state := proxy.GetClusterState()
		assert.Equal(t, "ollama_api", state.BackendEngine)
	})
}

// TestWebUI_DoesNotOverrideBalancerConfig проверяет что PUT-запрос от WebUI
// с пустыми/неполными полями не перезаписывает конфигурацию балансера
func TestWebUI_DoesNotOverrideBalancerConfig(t *testing.T) {
	ts, _, proxy := setupWebUITestServer(t, types.EngineLlamaCPP, "virtual_router")
	defer ts.Close()

	// 1. Проверяем исходное состояние
	initialState := proxy.GetClusterState()
	assert.Equal(t, "virtual_router", initialState.OperatingMode)
	assert.Equal(t, "llamacpp", initialState.BackendEngine)

	// 2. Имитируем PUT от WebUI с пустыми полями (частичное обновление)
	// WebUI может отправить только изменившиеся поля
	partialUpdate := map[string]interface{}{
		// Не передаём backendEngine — он не должен перезаписаться
		"queueMaxSize": 200,
		// Не передаём operatingMode
	}

	body, _ := json.Marshal(partialUpdate)
	req, _ := http.NewRequest("PUT", ts.URL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// 3. Проверяем что backendEngine и operatingMode не изменились
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if config, ok := result["config"].(map[string]interface{}); ok {
		if config["backendEngine"] != nil {
			assert.Equal(t, "llamacpp", config["backendEngine"],
				"backendEngine should NOT be overwritten by partial WebUI update")
		}
		if config["operatingMode"] != nil {
			assert.Equal(t, "virtual_router", config["operatingMode"],
				"operatingMode should NOT be overwritten by partial WebUI update")
		}
	}

	// 4. Проверяем через ClusterState что ничего не сломалось
	stateAfter := proxy.GetClusterState()
	assert.Equal(t, "virtual_router", stateAfter.OperatingMode,
		"OperatingMode should survive partial update")
	assert.Equal(t, "llamacpp", stateAfter.BackendEngine,
		"BackendEngine should survive partial update")
}

// TestWebUI_LlamaCppFieldsVisible проверяет что в ответах API присутствуют
// поля специфичные для llama.cpp и они не маскируются под Ollama
func TestWebUI_LlamaCppFieldsVisible(t *testing.T) {
	ts, _, proxy := setupWebUITestServer(t, types.EngineLlamaCPP, "virtual_router")
	defer ts.Close()

	// 1. Проверяем /api/v1/cluster/config
	resp, err := http.Get(ts.URL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp.Body.Close()

	var cfg map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&cfg)

	// Проверяем что backendEngine присутствует и равен llamacpp
	engine, hasEngine := cfg["backendEngine"]
	assert.True(t, hasEngine, "Config response should include backendEngine field")
	if hasEngine {
		assert.Equal(t, "llamacpp", engine)
	}

	// Проверяем что llmaCpp конфиг присутствует (может быть null/пустым)
	if llamaCpp, ok := cfg["llamaCpp"]; ok {
		t.Logf("llamaCpp config present: %v", llamaCpp)
	}

	// 2. Проверяем /api/v1/cluster/state
	resp2, err := http.Get(ts.URL + "/api/v1/cluster/state")
	require.NoError(t, err)
	defer resp2.Body.Close()

	var state map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&state)

	assert.Equal(t, "llamacpp", state["backendEngine"],
		"ClusterState should expose backendEngine=llamacpp")
	assert.Equal(t, "virtual_router", state["operatingMode"],
		"ClusterState should expose operatingMode=virtual_router")

	// Проверяем BackendTypeCounts
	if typeCounts, ok := state["backendTypeCounts"].(map[string]interface{}); ok {
		assert.Contains(t, typeCounts, "llama_cpp", "BackendTypeCounts should include llama_cpp")
		t.Logf("BackendTypeCounts: %v", typeCounts)
	}

	// 3. Проверяем /api/v1/backends/types
	resp3, err := http.Get(ts.URL + "/api/v1/backends/types")
	require.NoError(t, err)
	defer resp3.Body.Close()

	var typesResp map[string]interface{}
	json.NewDecoder(resp3.Body).Decode(&typesResp)

	typesList, ok := typesResp["types"].([]interface{})
	require.True(t, ok, "Response should contain types array")

	foundLlamaCpp := false
	for _, tItem := range typesList {
		tm := tItem.(map[string]interface{})
		if tm["type"] == "llama_cpp" {
			foundLlamaCpp = true
			if isCurrent, ok := tm["isCurrent"].(bool); ok && isCurrent {
				t.Logf("llama_cpp is marked as current type")
			}
		}
	}
	assert.True(t, foundLlamaCpp, "Backend types should include llama_cpp")

	_ = proxy
}

// TestWebUI_BackendListShowsCorrectPorts проверяет что /api/v1/backends
// возвращает корректные порты для каждого типа бэкенда
func TestWebUI_BackendListShowsCorrectPorts(t *testing.T) {
	ts, _, _ := setupWebUITestServer(t, types.EngineAuto, "standard")
	defer ts.Close()

	// Запрашиваем список бэкендов
	resp, err := http.Get(ts.URL + "/api/v1/backends")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var backendsResp map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&backendsResp)

	backends, ok := backendsResp["backends"].([]interface{})
	require.True(t, ok, "Response should contain backends array")

	for _, b := range backends {
		backend := b.(map[string]interface{})
		bType := backend["type"].(string)
		id := backend["id"].(string)

		t.Logf("Backend %s: type=%s, ollamaPort=%v, cppWorkerPort=%v",
			id, bType, backend["ollamaPort"], backend["cppWorkerPort"])

		switch bType {
		case "llama_cpp":
			// Для llama.cpp бэкендов должен быть заполнен CppWorkerPort
			if cppPort, ok := backend["cppWorkerPort"]; ok {
				assert.NotZero(t, cppPort,
					"llama.cpp backend should have non-zero cppWorkerPort")
			}
			// OllamaPort может присутствовать, но не должен использоваться
		case "ollama":
			// Для ollama бэкендов должен быть заполнен OllamaPort
			if ollPort, ok := backend["ollamaPort"]; ok {
				assert.NotZero(t, ollPort,
					"Ollama backend should have non-zero ollamaPort")
			}
		}
	}
}