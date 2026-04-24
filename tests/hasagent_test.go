package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/api"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupHasAgentTestEnvironment - создание тестового окружения с одним бэкендом
func setupHasAgentTestEnvironment(t *testing.T) (*httptest.Server, *balancer.Proxy) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:       "test-backend",
				Name:     "Test",
				Host:     "127.0.0.1",
				Status:   types.StatusHealthy,
				HasAgent: false, // изначально false
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			HealthCheckInterval: 1,
			MetricsInterval:     5,
			RequestTimeout:      30,
			QueueMaxSize:        100,
			QueueWorkers:        4,
			QueueTimeout:        60,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 95, MaxVRAMUsagePercent: 95, MaxTemperature: 90},
			CPU: types.CPULimits{MaxUsagePercent: 95},
			Memory: types.MemoryLimits{MaxUsagePercent: 95},
			Disk: types.DiskLimits{MinFreeMB: 1024},
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

	healthChecker := balancer.NewHealthChecker(
		proxy,
		time.Duration(config.Balancing.HealthCheckInterval)*time.Second,
		3,
	)

	server := api.NewServer(proxy, config, healthChecker)
	testServer := httptest.NewServer(server)

	return testServer, proxy
}

// TestHasAgentFlagPropagation проверяет, что HasAgent корректно
// сохраняется при метриках от агента и отдаётся в API
func TestHasAgentFlagPropagation(t *testing.T) {
	testServer, _ := setupHasAgentTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	t.Run("Initial state - hasAgent should be false", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/v1/backends")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)

		backends := result["backends"].([]interface{})
		require.Len(t, backends, 1, "expected exactly 1 backend")

		first := backends[0].(map[string]interface{})
		assert.Equal(t, false, first["hasAgent"], "initial hasAgent should be false")
	})

	t.Run("Send metrics from agent - hasAgent should become true", func(t *testing.T) {
		metricsPayload := map[string]interface{}{
			"id":        "test-backend",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"status":    "healthy",
			"gpu": map[string]interface{}{
				"usagePercent":  10.0,
				"memoryTotal":   8192,
				"memoryUsed":    1024,
				"memoryFree":    7168,
				"temperature":   45,
				"powerUsage":    50,
				"powerLimit":    150,
				"gpuClock":      1500,
				"memClock":      6000,
			},
			"system": map[string]interface{}{
				"cpuUsagePercent": 20.0,
				"memoryTotal":     16384,
				"memoryUsed":      4096,
				"memoryFree":      12288,
				"diskTotal":       512000,
				"diskUsed":        256000,
				"diskFree":        256000,
				"networkRX":       0,
				"networkTX":       0,
			},
			"ollama": map[string]interface{}{
				"runningModels":         []interface{}{},
				"activeRequests":        0,
				"totalRequests":         0,
				"avgResponseTime":       0.0,
				"requestsPerSecond":     0.0,
				"maxModels":             5,
				"maxConcurrentRequests": 10,
			},
		}

		body, _ := json.Marshal(metricsPayload)
		req, err := http.NewRequest("POST", baseURL+"/api/v1/agents/metrics", bytes.NewBuffer(body))
		require.NoError(t, err)
		req.Header.Set("X-Agent-ID", "test-backend")
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		// Читаем и проверяем ответ
		respBody, _ := io.ReadAll(resp.Body)
		t.Logf("Metrics response: %d, body: %s", resp.StatusCode, string(respBody))
		assert.Equal(t, http.StatusOK, resp.StatusCode, "metrics handler should succeed")
	})

	t.Run("After metrics - backends API shows hasAgent=true", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/v1/backends")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)

		backends := result["backends"].([]interface{})
		first := backends[0].(map[string]interface{})

		assert.Equal(t, true, first["hasAgent"], "hasAgent should be true after metrics")
	})

	t.Run("After metrics - cluster state shows hasAgent=true", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/v1/cluster")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var state types.ClusterState
		err = json.NewDecoder(resp.Body).Decode(&state)
		require.NoError(t, err)

		require.Len(t, state.Backends, 1, "expected 1 backend in cluster state")

		assert.Equal(t, true, state.Backends[0].HasAgent, "cluster state hasAgent should be true")
	})
}