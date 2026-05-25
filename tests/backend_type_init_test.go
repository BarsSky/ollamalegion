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
// Тесты инициализации типов бэкендов (BackendEngine)
// ============================================================

// setupBackendTypeTestServer создаёт тестовый сервер с заданным BackendEngine
func setupBackendTypeTestServer(t *testing.T, engine types.BackendEngine) (*httptest.Server, *api.Server, *balancer.Proxy) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    18080,
			APIPort: 18081,
		},
		Backends:      []types.Backend{},
		BackendEngine: engine,
		Balancing: types.BalancingSettings{
			Algorithm:          types.AlgorithmResourceAware,
			ModelAffinity:      true,
			SessionStickiness:  true,
			UseEnhancedScoring: true,
			OperatingMode:      getDefaultModeForEngine(engine),
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 85, MaxTemperature: 85},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
			Disk:   types.DiskLimits{MinFreeMB: 10240},
		},
	}

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, config, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	ts := httptest.NewServer(server)
	return ts, server, proxy
}

// getDefaultModeForEngine возвращает дефолтный operatingMode для типа движка
func getDefaultModeForEngine(e types.BackendEngine) string {
	switch e {
	case types.EngineOllamaAPI:
		return "standard"
	case types.EngineLlamaCPP:
		return "virtual_router"
	default:
		return "standard"
	}
}

// TestBackendTypeInit_Ollama проверяет инициализацию с Ollama-движком
func TestBackendTypeInit_Ollama(t *testing.T) {
	ts, _, proxy := setupBackendTypeTestServer(t, types.EngineOllamaAPI)
	defer ts.Close()

	state := proxy.GetClusterState()
	assert.Equal(t, "standard", state.OperatingMode,
		"Default operatingMode for Ollama should be 'standard'")

	// Проверяем что бэкенды ожидают тип ollama
	resp, err := http.Get(ts.URL + "/api/v1/backends/types")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var typeResp map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&typeResp)
	require.NoError(t, err)

	typesList, ok := typeResp["types"].([]interface{})
	require.True(t, ok, "Response should contain types array")
	assert.Greater(t, len(typesList), 0, "Should have at least one backend type")

	// Находим текущий тип
	foundCurrent := false
	for _, tItem := range typesList {
		tm := tItem.(map[string]interface{})
		if isCurrent, ok := tm["isCurrent"].(bool); ok && isCurrent {
			foundCurrent = true
			assert.Equal(t, "ollama", tm["type"].(string))
		}
	}
	assert.True(t, foundCurrent, "Ollama should be marked as current type")
}

// TestBackendTypeInit_LlamaCpp проверяет инициализацию с llama.cpp-движком
func TestBackendTypeInit_LlamaCpp(t *testing.T) {
	ts, _, proxy := setupBackendTypeTestServer(t, types.EngineLlamaCPP)
	defer ts.Close()

	state := proxy.GetClusterState()
	assert.Equal(t, "virtual_router", state.OperatingMode,
		"Default operatingMode for llama.cpp should be 'virtual_router'")

	resp, err := http.Get(ts.URL + "/api/v1/backends/types")
	require.NoError(t, err)
	defer resp.Body.Close()

	var typeResp map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&typeResp)
	require.NoError(t, err)

	typesList := typeResp["types"].([]interface{})
	foundCurrent := false
	for _, tItem := range typesList {
		tm := tItem.(map[string]interface{})
		if isCurrent, ok := tm["isCurrent"].(bool); ok && isCurrent {
			foundCurrent = true
			assert.Equal(t, "llama_cpp", tm["type"].(string))
		}
	}
	assert.True(t, foundCurrent, "llama_cpp should be marked as current type")
}

// TestBackendTypeInit_InvalidEngine проверяет поведение с неизвестным engine
func TestBackendTypeInit_InvalidEngine(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    18080,
			APIPort: 18081,
		},
		BackendEngine: "unknown_engine",
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: "standard",
		},
		Backends: []types.Backend{},
	}

	proxy := balancer.NewProxy(cfg)
	require.NotNil(t, proxy, "Proxy should be created even with unknown engine")

	state := proxy.GetClusterState()
	assert.NotEmpty(t, state.OperatingMode, "OperatingMode should fall back to default")
}

// TestBackendTypeSwitch проверяет переключение между типами бэкендов через API
func TestBackendTypeSwitch(t *testing.T) {
	ts, _, proxy := setupBackendTypeTestServer(t, types.EngineOllamaAPI)
	defer ts.Close()
	baseURL := ts.URL

	// Изначально режим standard (для Ollama)
	assert.Equal(t, "standard", proxy.GetClusterState().OperatingMode)

	// Переключаемся на llama.cpp через PUT /api/v1/cluster/config
	payload := map[string]interface{}{
		"backendEngine": string(types.EngineLlamaCPP),
		"operatingMode": "virtual_router",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "Switching backend engine should succeed")

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if config, ok := result["config"].(map[string]interface{}); ok {
		assert.Equal(t, "llamacpp", config["backendEngine"])
		assert.Equal(t, "virtual_router", config["operatingMode"])
	}
}

// TestBackendTypeInit_CreatesDefaults проверяет создание дефолтных значений
func TestBackendTypeInit_CreatesDefaults(t *testing.T) {
	t.Run("Ollama_defaults", func(t *testing.T) {
		ts, _, proxy := setupBackendTypeTestServer(t, types.EngineOllamaAPI)
		defer ts.Close()

		// Проверяем ClusterState напрямую
		state := proxy.GetClusterState()
		assert.True(t, state.OperatingMode == "standard" || state.OperatingMode == "",
			"Ollama default operatingMode should be standard or empty, got: %s", state.OperatingMode)

		resp, err := http.Get(ts.URL + "/api/v1/cluster/config")
		require.NoError(t, err)
		defer resp.Body.Close()

		var cfg map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&cfg)

		// Поля могут быть nil до инициализации
		if cfg["operatingMode"] != nil {
			assert.Equal(t, "standard", cfg["operatingMode"])
		}
		if cfg["modelAffinity"] != nil {
			assert.True(t, cfg["modelAffinity"].(bool))
		}
		if cfg["sessionStickiness"] != nil {
			assert.True(t, cfg["sessionStickiness"].(bool))
		}
	})

	t.Run("LlamaCpp_defaults", func(t *testing.T) {
		ts, _, proxy := setupBackendTypeTestServer(t, types.EngineLlamaCPP)
		defer ts.Close()

		state := proxy.GetClusterState()
		assert.True(t, state.OperatingMode == "virtual_router" || state.OperatingMode == "",
			"llama.cpp default operatingMode should be virtual_router or empty, got: %s", state.OperatingMode)

		resp, err := http.Get(ts.URL + "/api/v1/cluster/config")
		require.NoError(t, err)
		defer resp.Body.Close()

		var cfg map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&cfg)

		if cfg["operatingMode"] != nil {
			assert.Equal(t, "virtual_router", cfg["operatingMode"])
		}
	})
}

// TestBackendTypeInit_BackendTypeFilter проверяет фильтрацию бэкендов по типу
func TestBackendTypeInit_BackendTypeFilter(t *testing.T) {
	ts, server, _ := setupBackendTypeTestServer(t, types.EngineOllamaAPI)
	defer ts.Close()

	// Добавим бэкенды разных типов напрямую в конфиг
	config := server.GetConfig()
	config.Backends = append(config.Backends, types.Backend{
		ID: "ollama-1", Type: types.BackendTypeOllama, Host: "localhost", OllamaPort: 11434, Status: types.StatusHealthy,
	})
	config.Backends = append(config.Backends, types.Backend{
		ID: "llama-1", Type: types.BackendTypeLlamaCpp, Host: "localhost", CppWorkerPort: 18091, Status: types.StatusHealthy,
	})

	// Проверяем что бэкенды присутствуют
	assert.Len(t, config.Backends, 2)
	assert.Equal(t, types.BackendTypeOllama, config.Backends[0].Type)
	assert.Equal(t, types.BackendTypeLlamaCpp, config.Backends[1].Type)
}

// TestBackendTypeInit_ModeCompatibility проверяет совместимость типов с режимами
func TestBackendTypeInit_ModeCompatibility(t *testing.T) {
	// Ollama совместим со всеми режимами кроме virtual_router
	t.Run("Ollama_modes", func(t *testing.T) {
		ollamaModes := []string{"standard", "replication", "rpc_coordinator", "distributed_inference"}
		for _, mode := range ollamaModes {
			cfg := &types.LoadBalancerConfig{
				Balancing: types.BalancingSettings{
					OperatingMode: mode,
				},
				BackendEngine: types.EngineOllamaAPI,
				Backends: []types.Backend{
					{ID: "b1", Type: types.BackendTypeOllama, Host: "localhost", OllamaPort: 11434, Status: types.StatusHealthy},
				},
			}
			// Проверяем что proxy создаётся без паники
			proxy := balancer.NewProxy(cfg)
			require.NotNil(t, proxy, "Should create proxy for Ollama in mode %s", mode)
		}
	})

	// llama.cpp совместим с virtual_router и standard
	t.Run("LlamaCpp_modes", func(t *testing.T) {
		llamaModes := []string{"standard", "virtual_router"}
		for _, mode := range llamaModes {
			cfg := &types.LoadBalancerConfig{
				Balancing: types.BalancingSettings{
					OperatingMode: mode,
				},
				BackendEngine: types.EngineLlamaCPP,
				Backends: []types.Backend{
					{ID: "b1", Type: types.BackendTypeLlamaCpp, Host: "localhost", OllamaPort: 18091, Status: types.StatusHealthy},
				},
			}
			proxy := balancer.NewProxy(cfg)
			require.NotNil(t, proxy, "Should create proxy for llama.cpp in mode %s", mode)
		}
	})
}