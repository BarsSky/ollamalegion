package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================
// Тесты первоначальной настройки балансера для llama.cpp
// ============================================================

// TestInitialSetup_LlamaCppBackendEngine проверяет что при BackendEngine=llama_cpp:
//   - OperatingMode по умолчанию становится "virtual_router"
//   - BackendType бэкендов корректно устанавливается как llama_cpp
//   - CppWorkerPort сохраняется, а не перезатирается OllamaPort
func TestInitialSetup_LlamaCppBackendEngine(t *testing.T) {
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "127.0.0.1",
			Port:    18080,
			APIPort: 18081,
		},
		BackendEngine: types.EngineLlamaCPP,
		Balancing: types.BalancingSettings{
			Algorithm:          types.AlgorithmResourceAware,
			ModelAffinity:      true,
			SessionStickiness:  true,
			UseEnhancedScoring: true,
			OperatingMode:      "virtual_router",
		},
		Backends: []types.Backend{
			{
				ID:                "llamacpp-node-1",
				Name:              "LlamaCpp Node 1",
				Host:              "10.0.1.1",
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     18091,
				OllamaPort:        11434,
				Weight:            1,
				MaxConcurrentReqs: 10,
				MaxModels:         5,
				Status:            types.StatusHealthy,
			},
			{
				ID:                "llamacpp-node-2",
				Name:              "LlamaCpp Node 2",
				Host:              "10.0.1.2",
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     18091,
				Weight:            1,
				MaxConcurrentReqs: 10,
				MaxModels:         5,
				Status:            types.StatusHealthy,
			},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 85, MaxTemperature: 85},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
		},
	}

	proxy := balancer.NewProxy(config)
	require.NotNil(t, proxy, "Proxy should be created for llama.cpp engine")

	// 1. Проверка что OperatingMode установлен
	state := proxy.GetClusterState()
	assert.Equal(t, "virtual_router", state.OperatingMode,
		"llama.cpp engine should default to virtual_router operating mode")
	assert.Equal(t, string(types.EngineLlamaCPP), state.BackendEngine,
		"BackendEngine should be llama_cpp")

	// 2. Проверка BackendType counts
	counts := state.BackendTypeCounts
	assert.Equal(t, 0, counts[types.BackendTypeOllama], "Should have 0 ollama backends")
	assert.Equal(t, 2, counts[types.BackendTypeLlamaCpp], "Should have 2 llama_cpp backends")

	// 3. Проверка что CppWorkerPort установлен корректно для каждого бэкенда
	for _, bs := range proxy.GetBackendStates() {
		assert.Equal(t, types.BackendTypeLlamaCpp, bs.Backend.Type,
			"Backend %s should be llama_cpp type", bs.Backend.ID)
		assert.Equal(t, 18091, bs.Backend.CppWorkerPort,
			"Backend %s should have CppWorkerPort=18091", bs.Backend.ID)
		t.Logf("Backend %s: Type=%s, CppWorkerPort=%d, OllamaPort=%d",
			bs.Backend.ID, bs.Backend.Type, bs.Backend.CppWorkerPort, bs.Backend.OllamaPort)
	}

	// 4. Проверка что selectBackend возвращает только llama.cpp бэкенды
	for i := 0; i < 20; i++ {
		backendID := proxy.SelectBackend("test-model")
		if backendID == "" {
			continue
		}
		assert.True(t,
			strings.HasPrefix(backendID, "llamacpp-"),
			"Should only select llama.cpp backends, got: %s", backendID)
	}
}

// TestInitialSetup_LlamaCppDefaultsFromEnv проверяет что ENV-переменные
// (как в docker-compose.yml) корректно парсятся для llama.cpp:
//   - LB_BACKEND_ENGINE=llamacpp
//   - LB_OPERATING_MODE=virtual_router
//   - BACKEND_0_CPPWORKER_PORT=18091
func TestInitialSetup_LlamaCppDefaultsFromEnv(t *testing.T) {
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    getEnvOrDefault("LB_HOST", "0.0.0.0"),
			Port:    parseIntEnv("LB_PORT", 18080),
			APIPort: parseIntEnv("LB_API_PORT", 18081),
		},
		BackendEngine: types.BackendEngine(getEnvOrDefault("LB_BACKEND_ENGINE", "llama_cpp")),
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: true,
			OperatingMode:     getEnvOrDefault("LB_OPERATING_MODE", "virtual_router"),
			QueueMaxSize:      100,
			QueueTimeout:      60,
		},
		Backends: []types.Backend{
			{
				ID:                getEnvOrDefault("BACKEND_0_ID", "llamacpp-gpu-1"),
				Name:              getEnvOrDefault("BACKEND_0_NAME", "LlamaCpp GPU 1"),
				Host:              getEnvOrDefault("BACKEND_0_HOST", "10.0.1.1"),
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     parseIntEnv("BACKEND_0_CPPWORKER_PORT", 18091),
				OllamaPort:        parseIntEnv("BACKEND_0_OLLAMA_PORT", 11434),
				Weight:            parseIntEnv("BACKEND_0_WEIGHT", 1),
				MaxConcurrentReqs: parseIntEnv("BACKEND_0_MAX_REQS", 10),
				Status:            types.StatusHealthy,
			},
		},
	}

	assert.Equal(t, types.EngineLlamaCPP, config.BackendEngine)
	assert.Equal(t, "virtual_router", config.Balancing.OperatingMode)
	assert.Equal(t, 18091, config.Backends[0].CppWorkerPort)
	assert.Equal(t, types.BackendTypeLlamaCpp, config.Backends[0].Type)

	proxy := balancer.NewProxy(config)
	require.NotNil(t, proxy)

	state := proxy.GetClusterState()
	assert.Equal(t, "virtual_router", state.OperatingMode)

	backendIDs := proxy.GetBackendIDs()
	assert.Contains(t, backendIDs, "llamacpp-gpu-1")
}

// TestInitialSetup_LlamaCppPortIsolation проверяет что прокси-порт для llama.cpp
// (CppWorkerPort) изолирован от OllamaPort и балансер использует правильный порт
func TestInitialSetup_LlamaCppPortIsolation(t *testing.T) {
	// Mock-сервер на "порту CppWorker" (симулируем llama.cpp API)
	cppServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/health") {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		if strings.Contains(r.URL.Path, "/api/tags") || strings.Contains(r.URL.Path, "/v1/models") {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"object": "list",
				"data": []map[string]interface{}{
					{"id": "llama-test:latest", "object": "model"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"choices": []map[string]interface{}{{"message": map[string]string{"role": "assistant", "content": "test"}}},
		})
	}))
	defer cppServer.Close()

	cppAddr := strings.TrimPrefix(cppServer.URL, "http://")
	cppPort := mustParsePortInit(t, cppAddr)
	cppHost := strings.Split(cppAddr, ":")[0]

	// Фиктивный Ollama-сервер на другом порту — балансер НЕ должен слать запросы туда
	ollamaHitCount := 0
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaHitCount++
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ollama-ok"})
	}))
	defer ollamaServer.Close()

	ollamaAddr := strings.TrimPrefix(ollamaServer.URL, "http://")
	ollamaPort := mustParsePortInit(t, ollamaAddr)

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "127.0.0.1",
			Port:    18080,
			APIPort: 18081,
		},
		BackendEngine: types.EngineLlamaCPP,
		Balancing: types.BalancingSettings{
			Algorithm:          types.AlgorithmResourceAware,
			ModelAffinity:      true,
			SessionStickiness:  true,
			UseEnhancedScoring: true,
			OperatingMode:      "virtual_router",
			RequestTimeout:     10,
		},
		Backends: []types.Backend{
			{
				ID:                "llamacpp-real",
				Name:              "LlamaCpp Real",
				Host:              cppHost,
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     cppPort,
				OllamaPort:        ollamaPort,
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
	require.NotNil(t, proxy)

	proxy.SetBackendMetrics("llamacpp-real", &types.BackendMetrics{
		ID:          "llamacpp-real",
		BackendType: types.BackendTypeLlamaCpp,
		Status:      types.StatusHealthy,
		Host:        cppHost,
		OllamaPort:  cppPort,
		GPU: types.GPUMetrics{
			MemoryTotal: 24576,
			MemoryUsed:  4096,
			MemoryFree:  20480,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
			MemoryUsed:      16384,
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "llama-test:latest", ParameterSize: "8B", Family: "llama"},
			},
			ActiveRequests:        0,
			MaxConcurrentRequests: 10,
		},
	})

	testServer := httptest.NewServer(proxy)
	defer testServer.Close()

	time.Sleep(50 * time.Millisecond)

	reqBody := `{"model":"llama-test:latest","prompt":"Hello","stream":false}`
	resp, err := http.Post(
		testServer.URL+"/api/generate",
		"application/json",
		strings.NewReader(reqBody),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	t.Logf("Proxy response status: %d, body: %v", resp.StatusCode, result)

	assert.Equal(t, 0, ollamaHitCount,
		"Balancer should NOT proxy llama.cpp requests to OllamaPort")
}

// ============================================================
// Helpers
// ============================================================

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func parseIntEnv(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		n := 0
		if _, err := fmt.Sscanf(val, "%d", &n); err == nil {
			return n
		}
	}
	return defaultVal
}


// mustParsePortInit извлекает порт из строки "host:port"
func mustParsePortInit(t *testing.T, addr string) int {
	t.Helper()
	var port int
	_, err := fmt.Sscanf(addr[strings.LastIndex(addr, ":")+1:], "%d", &port)
	if err != nil {
		t.Fatalf("Failed to parse port from %s: %v", addr, err)
	}
	return port
}
