package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/api"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestEnvironment - создание полного тестового окружения
func setupTestEnvironment(t *testing.T) (*httptest.Server, *api.Server, *balancer.Proxy, *balancer.HealthChecker) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:                "agent-1",
				Name:              "Agent 1",
				Host:              "localhost",
				OllamaPort:        11434,
				AgentPort:         9090,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Labels:            []string{"gpu:nvidia", "zone:east"},
				Status:            types.StatusHealthy,
			},
			{
				ID:                "agent-2",
				Name:              "Agent 2",
				Host:              "localhost",
				OllamaPort:        11435,
				AgentPort:         9091,
				Weight:            2,
				MaxConcurrentReqs: 20,
				Labels:            []string{"gpu:nvidia", "zone:west"},
				Status:            types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			ModelAffinity:       true,
			SessionStickiness:   true,
			HealthCheckInterval: 10,
			MetricsInterval:     5,
			RequestTimeout:      30,
			QueueTimeout:        60,
			QueueMaxSize:        100,
			QueueWorkers:        4,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{
				MaxUsagePercent:     90,
				MaxVRAMUsagePercent: 95,
				MaxTemperature:      85,
			},
			CPU: types.CPULimits{
				MaxUsagePercent: 90,
			},
			Memory: types.MemoryLimits{
				MaxUsagePercent: 90,
			},
			Disk: types.DiskLimits{
				MinFreeMB: 1024,
			},
		},
		API: types.APISettings{
			RateLimit: 100,
			RateBurst: 200,
		},
		Auth: types.AuthConfig{
			Enabled:    false,
			Tokens:     []string{},
			HeaderName: "X-API-Token",
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()

	healthChecker := balancer.NewHealthChecker(proxy, time.Duration(config.Balancing.HealthCheckInterval)*time.Second, 3)
	server := api.NewServer(proxy, config, healthChecker)

	testServer := httptest.NewServer(server)

	return testServer, server, proxy, healthChecker
}

// ==================== E2E Тесты ====================

func TestE2E_FullWorkflow(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	t.Run("1. Health Check", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/v1/health")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var health map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&health)
		require.NoError(t, err)
		assert.Equal(t, "healthy", health["status"])
	})

	t.Run("2. Cluster State", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/v1/cluster")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var state types.ClusterState
		err = json.NewDecoder(resp.Body).Decode(&state)
		require.NoError(t, err)

		assert.Equal(t, 2, state.TotalBackends)
		assert.GreaterOrEqual(t, state.HealthyBackends, 0)
		assert.Equal(t, 2, len(state.Backends))

		// Проверяем наличие RPS и TotalGPUUsage
		assert.Equal(t, float64(0), state.RPS)
		assert.Equal(t, float64(0), state.TotalGPUUsage)
	})

	t.Run("3. Register New Agent", func(t *testing.T) {
		payload := map[string]interface{}{
			"agentId":    "agent-3",
			"hostname":   "new-host",
			"host":       "192.168.1.200",
			"ollamaPort": 11436,
			"agentPort":  9092,
			"gpuCount":   2,
			"name":       "New Agent",
			"labels":     []string{"gpu:nvidia", "zone:north"},
		}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(baseURL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusCreated, resp.StatusCode)

		var response map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&response)
		require.NoError(t, err)
		assert.Equal(t, true, response["success"])
		assert.Equal(t, "created", response["action"])

		// Проверяем, что агент добавлен
		backend := proxy.GetBackend("agent-3")
		assert.NotNil(t, backend)
		assert.Equal(t, "New Agent", backend.Name)
	})

	t.Run("4. Send Metrics", func(t *testing.T) {
		metrics := types.BackendMetrics{
			ID: "agent-1",
			GPU: types.GPUMetrics{
				UsagePercent:    65.5,
				MemoryTotal:     24576,
				MemoryUsed:      12000,
				MemoryFree:      12576,
				Temperature:     72,
				PowerUsage:      250,
				GPUClock:        1800,
			},
			System: types.SystemMetrics{
				CPUUsagePercent: 45.2,
				MemoryTotal:     65536,
				MemoryUsed:      32000,
				MemoryFree:      33536,
			},
			Ollama: types.OllamaMetrics{
				ActiveRequests:        3,
				TotalRequests:           150,
				RequestsPerSecond:     2.5,
				AvgResponseTime:       1200,
				RunningModels: []types.RunningModel{
					{
						Name:      "llama3:8b",
						Size:      4928300000,
						VRAMUsage: 8192,
						Family:    "llama",
						Format:    "gguf",
					},
				},
			},
		}
		body, _ := json.Marshal(metrics)

		req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/agents/metrics", bytes.NewBuffer(body))
		req.Header.Set("X-Agent-ID", "agent-1")
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// Проверяем, что метрики обновлены в кластере
		clusterState := proxy.GetClusterState()
		require.NotNil(t, clusterState)
		assert.Greater(t, clusterState.RPS, float64(0))
		assert.Greater(t, clusterState.TotalGPUUsage, float64(0))
	})

	t.Run("5. Send Heartbeat", func(t *testing.T) {
		// Сначала обновим статус на unhealthy для проверки
		proxy.UpdateBackendStatus("agent-2", types.StatusUnhealthy)

		req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/agents/heartbeat", nil)
		req.Header.Set("X-Agent-ID", "agent-2")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// Проверяем, что статус обновился
		backend := proxy.GetBackend("agent-2")
		assert.Equal(t, types.StatusHealthy, backend.Status)
	})

	t.Run("6. Agent Stats", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/v1/agents/stats")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var stats map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&stats)
		require.NoError(t, err)

		assert.Equal(t, float64(3), stats["totalAgents"])
		assert.GreaterOrEqual(t, stats["healthyAgents"], float64(0))

		agents, ok := stats["agents"].([]interface{})
		require.True(t, ok)
		assert.GreaterOrEqual(t, len(agents), 3)
	})

	t.Run("7. Get Backends", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/v1/backends")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var response map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&response)
		require.NoError(t, err)

		assert.Equal(t, float64(3), response["total"])

		backends, ok := response["backends"].([]interface{})
		require.True(t, ok)
		assert.Equal(t, 3, len(backends))
	})

	t.Run("8. Update Backend", func(t *testing.T) {
		updateData := map[string]interface{}{
			"name":                "Updated Agent 1",
			"host":                "localhost",
			"ollamaPort":          11434,
			"agentPort":           9090,
			"weight":              3,
			"maxConcurrentRequests": 25,
			"labels":              []string{"gpu:nvidia", "zone:east", "updated"},
		}
		body, _ := json.Marshal(updateData)

		req, _ := http.NewRequest(http.MethodPut, baseURL+"/api/v1/backends/agent-1", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var response map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&response)
		require.NoError(t, err)
		assert.Equal(t, true, response["success"])

		// Проверяем обновление
		backend := proxy.GetBackend("agent-1")
		assert.Equal(t, "Updated Agent 1", backend.Name)
		assert.Equal(t, 3, backend.Weight)
	})

	t.Run("9. Delete Backend", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, baseURL+"/api/v1/backends/agent-3", nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var response map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&response)
		require.NoError(t, err)
		assert.Equal(t, true, response["success"])

		// Проверяем удаление
		backend := proxy.GetBackend("agent-3")
		assert.Nil(t, backend)
	})

	t.Run("10. Sessions Management", func(t *testing.T) {
		// Получаем список сессий
		resp, err := http.Get(baseURL + "/api/v1/sessions")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// Удаляем все сессии
		req, _ := http.NewRequest(http.MethodDelete, baseURL+"/api/v1/sessions", nil)
		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var response map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&response)
		require.NoError(t, err)
		assert.Equal(t, true, response["success"])
	})

	t.Run("11. Final Cluster State", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/v1/cluster")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var state types.ClusterState
		err = json.NewDecoder(resp.Body).Decode(&state)
		require.NoError(t, err)

		// Должно быть 2 бэкенда после удаления agent-3
		assert.Equal(t, 2, state.TotalBackends)
		assert.NotNil(t, state.Timestamp)
		assert.GreaterOrEqual(t, state.HealthyBackends, 0)
	})
}

func TestE2E_MetricsAndClusterState(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Отправляем метрики для обоих агентов
	agents := []string{"agent-1", "agent-2"}
	for i, agentID := range agents {
		metrics := types.BackendMetrics{
			ID: agentID,
			GPU: types.GPUMetrics{
				UsagePercent: float64(50 + i*10), // 50% и 60%
				MemoryTotal:  24576,
				MemoryUsed:   12000,
			},
			System: types.SystemMetrics{
				CPUUsagePercent: float64(40 + i*5),
			},
			Ollama: types.OllamaMetrics{
				ActiveRequests:    i + 1,
				RequestsPerSecond: float64(1.5 + float64(i)),
				RunningModels: []types.RunningModel{
					{
						Name: fmt.Sprintf("model-%d", i),
						Size: 4928300000,
					},
				},
			},
		}
		body, _ := json.Marshal(metrics)

		req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/agents/metrics", bytes.NewBuffer(body))
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}

	// Проверяем ClusterState
	clusterState := proxy.GetClusterState()
	require.NotNil(t, clusterState)

	// RPS должен быть суммой RPS обоих агентов: 1.5 + 2.5 = 4.0
	assert.InDelta(t, 4.0, clusterState.RPS, 0.01)

	// TotalGPUUsage должен быть суммой: 50 + 60 = 110
	assert.InDelta(t, 110.0, clusterState.TotalGPUUsage, 0.01)

	// ActiveRequests должен быть суммой: 1 + 2 = 3
	assert.Equal(t, 3, clusterState.ActiveRequests)

	// Проверяем через API
	resp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()

	var apiState types.ClusterState
	err = json.NewDecoder(resp.Body).Decode(&apiState)
	require.NoError(t, err)

	assert.InDelta(t, 4.0, apiState.RPS, 0.01)
	assert.InDelta(t, 110.0, apiState.TotalGPUUsage, 0.01)
}

func TestE2E_AgentRegistrationFlow(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Шаг 1: Регистрация нового агента
	payload := map[string]interface{}{
		"agentId":    "flow-agent",
		"hostname":   "flow-host",
		"host":       "192.168.100.50",
		"ollamaPort": 11440,
		"agentPort":  9095,
		"gpuCount":   4,
		"name":       "Flow Test Agent",
		"labels":     []string{"gpu:nvidia", "test:true"},
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(baseURL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	// Шаг 2: Проверка, что агент появился в списке
	resp, err = http.Get(baseURL + "/api/v1/agents/stats")
	require.NoError(t, err)
	defer resp.Body.Close()

	var stats map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&stats)
	require.NoError(t, err)

	agents := stats["agents"].([]interface{})
	found := false
	for _, a := range agents {
		agent := a.(map[string]interface{})
		if agent["id"] == "flow-agent" {
			found = true
			break
		}
	}
	assert.True(t, found, "Registered agent should appear in stats")

	// Шаг 3: Обновление регистрации (re-register)
	payload["name"] = "Updated Flow Agent"
	body, _ = json.Marshal(payload)

	resp, err = http.Post(baseURL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var updateResponse map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&updateResponse)
	require.NoError(t, err)
	assert.Equal(t, "updated", updateResponse["action"])

	// Шаг 4: Проверка обновления
	backend := proxy.GetBackend("flow-agent")
	assert.Equal(t, "Updated Flow Agent", backend.Name)

	// Шаг 5: Отправка метрик
	metrics := types.BackendMetrics{
		ID: "flow-agent",
		GPU: types.GPUMetrics{
			UsagePercent: 75.0,
			MemoryTotal:  32768,
			MemoryUsed:   24000,
			Temperature:  65,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 30.0,
			MemoryTotal:     131072,
			MemoryUsed:      65536,
		},
		Ollama: types.OllamaMetrics{
			ActiveRequests:    5,
			RequestsPerSecond: 3.5,
			RunningModels: []types.RunningModel{
				{
					Name: "codellama:7b",
					Size: 3800000000,
				},
			},
		},
	}
	metricsBody, _ := json.Marshal(metrics)

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/agents/metrics", bytes.NewBuffer(metricsBody))
	req.Header.Set("X-Agent-ID", "flow-agent")
	req.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Шаг 6: Проверка ClusterState с метриками
	clusterState := proxy.GetClusterState()
	foundMetrics := false
	for _, b := range clusterState.Backends {
		if b.ID == "flow-agent" {
			foundMetrics = true
			assert.Equal(t, 75.0, b.GPU.UsagePercent)
			assert.Equal(t, 5, b.Ollama.ActiveRequests)
			assert.Equal(t, 3.5, b.Ollama.RequestsPerSecond)
			assert.Equal(t, 1, len(b.Ollama.RunningModels))
			break
		}
	}
	assert.True(t, foundMetrics, "Metrics should be in cluster state")

	// Шаг 7: Heartbeat
	req, _ = http.NewRequest(http.MethodPost, baseURL+"/api/v1/agents/heartbeat", nil)
	req.Header.Set("X-Agent-ID", "flow-agent")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Шаг 8: Удаление агента
	req, _ = http.NewRequest(http.MethodDelete, baseURL+"/api/v1/backends/flow-agent", nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Проверка удаления
	assert.Nil(t, proxy.GetBackend("flow-agent"))
}

func TestE2E_ErrorHandling(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	t.Run("Invalid JSON", func(t *testing.T) {
		resp, err := http.Post(baseURL+"/api/v1/agents/register", "application/json", bytes.NewBuffer([]byte("not json")))
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("Missing AgentID in metrics", func(t *testing.T) {
		metrics := types.BackendMetrics{ID: "test"}
		body, _ := json.Marshal(metrics)

		req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/agents/metrics", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		// Без X-Agent-ID

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("Missing AgentID in heartbeat", func(t *testing.T) {
		resp, err := http.Post(baseURL+"/api/v1/agents/heartbeat", "application/json", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("Non-existent backend", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/v1/backends/nonexistent")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("Method not allowed", func(t *testing.T) {
		resp, err := http.Post(baseURL+"/api/v1/health", "application/json", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})
}

func TestE2E_ClusterStateAggregation(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()

	// Отправляем различные метрики
	testCases := []struct {
		agentID string
		gpuUsage float64
		rps      float64
		activeReqs int
	}{
		{"agent-1", 30.0, 1.0, 2},
		{"agent-2", 45.0, 2.5, 3},
	}

	for _, tc := range testCases {
		metrics := types.BackendMetrics{
			ID: tc.agentID,
			GPU: types.GPUMetrics{
				UsagePercent: tc.gpuUsage,
				MemoryTotal:  16384,
				MemoryUsed:   8000,
			},
			Ollama: types.OllamaMetrics{
				ActiveRequests:    tc.activeReqs,
				RequestsPerSecond: tc.rps,
			},
		}
		body, _ := json.Marshal(metrics)

		req, _ := http.NewRequest(http.MethodPost, testServer.URL+"/api/v1/agents/metrics", bytes.NewBuffer(body))
		req.Header.Set("X-Agent-ID", tc.agentID)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
	}

	// Проверяем агрегацию
	state := proxy.GetClusterState()
	require.NotNil(t, state)

	// RPS: 1.0 + 2.5 = 3.5
	assert.InDelta(t, 3.5, state.RPS, 0.01)

	// TotalGPUUsage: 30.0 + 45.0 = 75.0
	assert.InDelta(t, 75.0, state.TotalGPUUsage, 0.01)

	// ActiveRequests: 2 + 3 = 5
	assert.Equal(t, 5, state.ActiveRequests)

	// TotalBackends: 2
	assert.Equal(t, 2, state.TotalBackends)
}

// TestE2E_ProxyStreaming — проверка проксирования streaming запросов
// через httptest.Server вместо реального Ollama бэкенда
func TestE2E_ProxyStreaming(t *testing.T) {
	// Мок-сервер, эмулирующий Ollama streaming endpoint
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/chat") || strings.Contains(r.URL.Path, "/api/generate") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			for i := 0; i < 5; i++ {
				fmt.Fprintf(w, "data: {\"token\":\"tok_%d\",\"index\":%d}\n\n", i, i)
				if ok {
					flusher.Flush()
				}
				time.Sleep(5 * time.Millisecond)
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if ok {
				flusher.Flush()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mockOllama.Close()

	hostParts := strings.Split(strings.TrimPrefix(mockOllama.URL, "http://"), ":")
	if len(hostParts) < 2 {
		t.Skip("Cannot parse mock server URL")
	}

	cfg := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "ollama-1", Host: hostParts[0], OllamaPort: mustParsePortStr(hostParts[1]), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           "resource-aware",
			FirstByteTimeout:    30,
			RequestTimeout:      30,
			StreamingIdleTimeout: 60,
			QueueMaxSize:        50,
			QueueWorkers:        4,
			QueueTimeout:        60,
		},
	}

	proxy := balancer.NewProxy(cfg)
	proxy.UpdateMetrics("ollama-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.2", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	req, _ := http.NewRequest("POST", lb.URL+"/api/chat",
		strings.NewReader(`{"model":"llama3.2","messages":[{"role":"user","content":"Hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Name", "test-client")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Streaming request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200, got %d", resp.StatusCode)
	}

	// Читаем стрим-ответ
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read streaming body: %v", err)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, "tok_") {
		t.Errorf("Streaming response missing tokens: %s", bodyStr[:min(len(bodyStr), 200)])
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("Streaming response missing [DONE]: %s", bodyStr[:min(len(bodyStr), 200)])
	}

	// Проверяем заголовок content-type
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Expected Content-Type text/event-stream, got %s", ct)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func mustParsePortStr(portStr string) int {
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return port
}
