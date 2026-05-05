package tests

import (
	"bytes"
	"encoding/json"
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

// ==================== Тесты структуры ответа /api/v1/cluster ====================

func TestClusterAPI_ResponseStructure(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	registerAgentWithFullMetrics(t, baseURL, proxy)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var state map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&state)
	require.NoError(t, err)

	requiredTopLevelFields := []string{
		"timestamp", "totalBackends", "healthyBackends", "totalRequests",
		"activeRequests", "queuedRequests", "rps", "totalGpuUsage", "backends",
	}
	for _, field := range requiredTopLevelFields {
		assert.Contains(t, state, field, "ClusterState must contain field: %s", field)
	}

	backends, ok := state["backends"].([]interface{})
	require.True(t, ok, "backends must be an array")
	assert.GreaterOrEqual(t, len(backends), 2, "Should have at least 2 backends")

	for i, b := range backends {
		backend, ok := b.(map[string]interface{})
		require.True(t, ok, "backend[%d] must be an object", i)

		requiredBackendFields := []string{
			"id", "timestamp", "status", "hasAgent",
			"host", "ollamaPort",
			"gpu", "system", "ollama", "prediction",
			"score", "maxConcurrentRequests", "models",
			"vramUsagePercent", "vramTotalGB", "vramUsedGB",
			"memoryUsagePercent",
		}
		for _, field := range requiredBackendFields {
			assert.Contains(t, backend, field, "BackendMetrics must contain field: %s (backend %d)", field, i)
		}
	}
}

func TestClusterAPI_BackendGPUFields(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	registerAgentWithFullMetrics(t, baseURL, proxy)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()

	var state map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&state)
	backends := state["backends"].([]interface{})

	for _, b := range backends {
		backend := b.(map[string]interface{})
		gpu := backend["gpu"].(map[string]interface{})

		requiredGPUFields := []string{
			"usagePercent", "memoryTotal", "memoryUsed", "memoryFree",
			"temperature", "powerUsage", "powerLimit", "gpuClock", "memClock",
		}
		for _, field := range requiredGPUFields {
			assert.Contains(t, gpu, field, "GPUMetrics must contain field: %s", field)
		}
	}
}

func TestClusterAPI_BackendSystemFields(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	registerAgentWithFullMetrics(t, baseURL, proxy)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()

	var state map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&state)
	backends := state["backends"].([]interface{})

	for _, b := range backends {
		backend := b.(map[string]interface{})
		system := backend["system"].(map[string]interface{})

		requiredSystemFields := []string{
			"cpuUsagePercent", "cpu",
			"memoryTotal", "memoryUsed", "memoryFree",
			"diskTotal", "diskUsed", "diskFree",
			"networkRX", "networkTX",
			"networkRXRate", "networkTXRate",
		}
		for _, field := range requiredSystemFields {
			assert.Contains(t, system, field, "SystemMetrics must contain field: %s", field)
		}

		cpu := system["cpu"].(map[string]interface{})
		requiredCPUFields := []string{
			"usagePercent", "usagePerCore", "coreCount", "threadCount",
			"model", "loadAverage1", "loadAverage5", "loadAverage15",
			"temperature", "throttled",
		}
		for _, field := range requiredCPUFields {
			assert.Contains(t, cpu, field, "CPUMetrics must contain field: %s", field)
		}
	}
}

func TestClusterAPI_BackendOllamaFields(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	registerAgentWithFullMetrics(t, baseURL, proxy)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()

	var state map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&state)
	backends := state["backends"].([]interface{})

	for _, b := range backends {
		backend := b.(map[string]interface{})
		ollama := backend["ollama"].(map[string]interface{})

		requiredOllamaFields := []string{
			"runningModels", "availableModels", "activeRequests",
			"totalRequests", "avgResponseTime", "requestsPerSecond",
			"maxModels", "maxConcurrentRequests",
			"freeSlots", "availableSlots",
			"runtimeFlags", "modelContexts", "backendCapacity",
		}
		for _, field := range requiredOllamaFields {
			assert.Contains(t, ollama, field, "OllamaMetrics must contain field: %s", field)
		}

		runtimeFlags := ollama["runtimeFlags"].(map[string]interface{})
		requiredFlagsFields := []string{
			"numGpuLayers", "contextLength", "numParallel", "numThreads",
			"batchSize", "gpuSplitMode", "mainGpu",
			"lowVram", "f16kv", "kvCacheQuant", "flashAttention", "source",
		}
		for _, field := range requiredFlagsFields {
			assert.Contains(t, runtimeFlags, field, "OllamaRuntimeFlags must contain field: %s", field)
		}

		backendCapacity := ollama["backendCapacity"].(map[string]interface{})
		requiredCapacityFields := []string{
			"freeVram", "guaranteedVram", "loadedModelVram",
			"contextOverheadMB", "availableModels", "loadableModelCount", "mode",
		}
		for _, field := range requiredCapacityFields {
			assert.Contains(t, backendCapacity, field, "BackendCapacity must contain field: %s", field)
		}
	}
}

func TestClusterAPI_BackendPredictionFields(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	registerAgentWithFullMetrics(t, baseURL, proxy)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()

	var state map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&state)
	backends := state["backends"].([]interface{})

	for _, b := range backends {
		backend := b.(map[string]interface{})
		prediction := backend["prediction"].(map[string]interface{})

		requiredPredictionFields := []string{
			"secondsToCritical", "criticalReason",
			"gpuUsageTrend", "vramUsageTrend", "ramUsageTrend",
			"freeSlotsTrend", "requestCapacity",
		}
		for _, field := range requiredPredictionFields {
			assert.Contains(t, prediction, field, "Prediction must contain field: %s", field)
		}
	}
}

// ==================== Тесты структуры ответа /api/v1/queue/details ====================

func TestQueueDetailsAPI_ResponseStructure(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	resp, err := http.Get(baseURL + "/api/v1/queue/details")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var details map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&details)
	require.NoError(t, err)

	requiredFields := []string{
		"pending", "processing", "all",
		"pending_count", "processing_count",
		"total", "current_size", "processed_total",
	}
	for _, field := range requiredFields {
		assert.Contains(t, details, field, "Queue details must contain field: %s", field)
	}

	assert.IsType(t, []interface{}{}, details["pending"])
	assert.IsType(t, []interface{}{}, details["processing"])
	assert.IsType(t, []interface{}{}, details["all"])
	assert.IsType(t, float64(0), details["pending_count"])
	assert.IsType(t, float64(0), details["processing_count"])
	assert.IsType(t, float64(0), details["total"])
}

func TestQueueDetailsAPI_QueueItemStructure(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	registerAgentWithModel(t, baseURL, proxy, "llama3.1")
	time.Sleep(100 * time.Millisecond)

	go func() {
		req, _ := http.NewRequest("POST", baseURL+"/api/generate",
			bytes.NewBufferString(`{"model":"llama3.1","prompt":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		http.DefaultClient.Do(req)
	}()
	time.Sleep(200 * time.Millisecond)

	resp, err := http.Get(baseURL + "/api/v1/queue/details")
	require.NoError(t, err)
	defer resp.Body.Close()

	var details map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&details)

	allItems := details["all"].([]interface{})
	if len(allItems) > 0 {
		item := allItems[0].(map[string]interface{})
		requiredItemFields := []string{
			"model", "target", "status", "enqueued", "sessionId",
		}
		for _, field := range requiredItemFields {
			assert.Contains(t, item, field, "Queue item must contain field: %s", field)
		}
	}
}

// ==================== Тесты структуры ответа /api/v1/sessions ====================

func TestSessionsAPI_ResponseStructure(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	resp, err := http.Get(baseURL + "/api/v1/sessions")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var data map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
	require.NoError(t, err)

	assert.Contains(t, data, "total")
	assert.Contains(t, data, "sessions")

	sessions, ok := data["sessions"].([]interface{})
	assert.True(t, ok, "sessions must be an array")
	t.Logf("Sessions count: %d", len(sessions))
}

// ==================== Тесты доступности API для монитора ====================

func TestMonitorAPI_AllEndpointsAccessible(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	endpoints := []struct {
		name   string
		path   string
		method string
	}{
		{"cluster", "/api/v1/cluster", "GET"},
		{"queue/details", "/api/v1/queue/details", "GET"},
		{"sessions", "/api/v1/sessions", "GET"},
		{"health", "/api/v1/health", "GET"},
		{"backends", "/api/v1/backends", "GET"},
		{"metrics", "/api/v1/metrics", "GET"},
	}

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			req, err := http.NewRequest(ep.method, baseURL+ep.path, nil)
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode,
				"Endpoint %s should return 200 OK, got %d", ep.path, resp.StatusCode)
		})
	}
}

func TestMonitorAPI_CORSHeaders(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	req, _ := http.NewRequest("OPTIONS", baseURL+"/api/v1/cluster", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Methods"), "GET")
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Headers"), "Content-Type")

	req2, _ := http.NewRequest("GET", baseURL+"/api/v1/cluster", nil)
	req2.Header.Set("Origin", "http://localhost:3000")
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()

	assert.Equal(t, "*", resp2.Header.Get("Access-Control-Allow-Origin"))
}

func TestMonitorAPI_AuthRequired(t *testing.T) {
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 8080, APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID: "agent-1", Name: "Agent 1", Host: "localhost",
				OllamaPort: 11434, AgentPort: 9090, Weight: 1,
				MaxConcurrentReqs: 10, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware,
			HealthCheckInterval: 10, MetricsInterval: 5,
			RequestTimeout: 30, QueueTimeout: 60, QueueMaxSize: 100, QueueWorkers: 4,
		},
		API: types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{
			Enabled:    true,
			Tokens:     []string{"test-token-12345"},
			HeaderName: "X-API-Token",
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := api.NewServer(proxy, config, healthChecker)
	testServer := httptest.NewServer(server)
	defer testServer.Close()
	baseURL := testServer.URL

	resp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"Should return 401 without token when auth enabled")

	req, _ := http.NewRequest("GET", baseURL+"/api/v1/cluster", nil)
	req.Header.Set("X-API-Token", "test-token-12345")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode,
		"Should return 200 with valid token")

	req3, _ := http.NewRequest("GET", baseURL+"/api/v1/cluster", nil)
	req3.Header.Set("X-API-Token", "wrong-token")
	resp3, err := http.DefaultClient.Do(req3)
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp3.StatusCode,
		"Should return 401 with invalid token")

	resp4, err := http.Get(baseURL + "/api/v1/health")
	require.NoError(t, err)
	defer resp4.Body.Close()
	assert.Equal(t, http.StatusOK, resp4.StatusCode,
		"Health endpoint should be accessible without token")
}

// ==================== Тест регрессии: проверка что ключевые поля не пропали ====================

var expectedClusterStateFields = map[string]bool{
	"timestamp": true, "totalBackends": true, "healthyBackends": true,
	"totalRequests": true, "activeRequests": true, "queuedRequests": true,
	"rps": true, "totalGpuUsage": true, "backends": true,
}

var expectedBackendMetricsFields = map[string]bool{
	"id": true, "timestamp": true, "status": true, "hasAgent": true,
	"host": true, "ollamaPort": true,
	"gpu": true, "system": true, "ollama": true, "prediction": true,
	"score": true, "maxConcurrentRequests": true, "models": true,
	"vramUsagePercent": true, "vramTotalGB": true, "vramUsedGB": true,
	"memoryUsagePercent": true,
}

var expectedGPUMetricsFields = map[string]bool{
	"usagePercent": true, "memoryTotal": true, "memoryUsed": true, "memoryFree": true,
	"temperature": true, "powerUsage": true, "powerLimit": true,
	"gpuClock": true, "memClock": true,
}

var expectedSystemMetricsFields = map[string]bool{
	"cpuUsagePercent": true, "cpu": true,
	"memoryTotal": true, "memoryUsed": true, "memoryFree": true,
	"diskTotal": true, "diskUsed": true, "diskFree": true,
	"networkRX": true, "networkTX": true,
	"networkRXRate": true, "networkTXRate": true,
}

var expectedOllamaMetricsFields = map[string]bool{
	"runningModels": true, "availableModels": true, "activeRequests": true,
	"totalRequests": true, "avgResponseTime": true, "requestsPerSecond": true,
	"maxModels": true, "maxConcurrentRequests": true,
	"freeSlots": true, "availableSlots": true,
	"runtimeFlags": true, "modelContexts": true, "backendCapacity": true,
}

var expectedQueueDetailsFields = map[string]bool{
	"pending": true, "processing": true, "all": true,
	"pending_count": true, "processing_count": true,
	"total": true, "current_size": true, "processed_total": true,
}

var expectedSessionFields = map[string]bool{
	"id": true, "clientName": true, "clientIP": true,
	"backendId": true, "backendName": true, "model": true,
	"createdAt": true, "lastRequestAt": true,
	"requestCount": true, "idleSeconds": true,
}

func TestRegression_ClusterStateFieldsComplete(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	resp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()

	var state map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&state)

	for field := range expectedClusterStateFields {
		assert.Contains(t, state, field,
			"REGRESSION: ClusterState field '%s' is missing from API response", field)
	}

	backends := state["backends"].([]interface{})
	if len(backends) > 0 {
		backend := backends[0].(map[string]interface{})
		for field := range expectedBackendMetricsFields {
			assert.Contains(t, backend, field,
				"REGRESSION: BackendMetrics field '%s' is missing from API response", field)
		}

		gpu := backend["gpu"].(map[string]interface{})
		for field := range expectedGPUMetricsFields {
			assert.Contains(t, gpu, field,
				"REGRESSION: GPUMetrics field '%s' is missing from API response", field)
		}

		system := backend["system"].(map[string]interface{})
		for field := range expectedSystemMetricsFields {
			assert.Contains(t, system, field,
				"REGRESSION: SystemMetrics field '%s' is missing from API response", field)
		}

		ollama := backend["ollama"].(map[string]interface{})
		for field := range expectedOllamaMetricsFields {
			assert.Contains(t, ollama, field,
				"REGRESSION: OllamaMetrics field '%s' is missing from API response", field)
		}
	}
}

func TestRegression_QueueDetailsFieldsComplete(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	resp, err := http.Get(baseURL + "/api/v1/queue/details")
	require.NoError(t, err)
	defer resp.Body.Close()

	var details map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&details)

	for field := range expectedQueueDetailsFields {
		assert.Contains(t, details, field,
			"REGRESSION: Queue details field '%s' is missing from API response", field)
	}
}

func TestRegression_SessionFieldsComplete(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	resp, err := http.Get(baseURL + "/api/v1/sessions")
	require.NoError(t, err)
	defer resp.Body.Close()

	var data map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&data)

	assert.Contains(t, data, "total", "REGRESSION: Sessions response missing 'total' field")
	assert.Contains(t, data, "sessions", "REGRESSION: Sessions response missing 'sessions' field")

	sessions, ok := data["sessions"].([]interface{})
	assert.True(t, ok, "sessions must be an array")

	if len(sessions) > 0 {
		session := sessions[0].(map[string]interface{})
		for field := range expectedSessionFields {
			assert.Contains(t, session, field,
				"REGRESSION: Session field '%s' is missing from API response", field)
		}
	}
}

// ==================== Хелперы ====================

func registerAgentWithFullMetrics(t *testing.T, baseURL string, _ *balancer.Proxy) {
	t.Helper()

	payload := map[string]interface{}{
		"agentId":    "agent-test-full",
		"hostname":   "test-host-full",
		"host":       "127.0.0.1",
		"ollamaPort": 11434,
		"agentPort":  9093,
		"gpuCount":   1,
		"name":       "Test Agent Full Metrics",
		"labels":     []string{"gpu:nvidia", "test:true"},
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(baseURL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	resp.Body.Close()

	metricsPayload := map[string]interface{}{
		"agentId": "agent-test-full",
		"gpu": map[string]interface{}{
			"usagePercent": 45.5,
			"memoryTotal":  24576,
			"memoryUsed":   12345,
			"memoryFree":   12231,
			"temperature":  65,
			"powerUsage":   180,
			"powerLimit":   250,
			"gpuClock":     1750,
			"memClock":     7000,
		},
		"system": map[string]interface{}{
			"cpuUsagePercent": 30.0,
			"cpu": map[string]interface{}{
				"usagePercent":  30.0,
				"usagePerCore":  []float64{25.0, 35.0, 28.0, 32.0},
				"coreCount":     4,
				"threadCount":   8,
				"model":         "Intel Core i7",
				"loadAverage1":  1.5,
				"loadAverage5":  1.2,
				"loadAverage15": 1.0,
				"temperature":   55,
				"throttled":     false,
			},
			"memoryTotal": 32768,
			"memoryUsed":  16384,
			"memoryFree":  16384,
			"diskTotal":   512000,
			"diskUsed":    256000,
			"diskFree":    256000,
			"networkRX":   1024000,
			"networkTX":   512000,
			"networkRXRate": 1024.5,
			"networkTXRate": 512.3,
		},
		"ollama": map[string]interface{}{
			"runningModels": []map[string]interface{}{
				{
					"name": "llama3.1", "size": 4294967296, "vramUsage": 4000,
					"ramUsage": 0, "digest": "abc123", "loadCount": 1,
					"family": "llama", "format": "gguf",
					"parameterSize": "8B", "quantization": "Q4_0",
				},
			},
			"activeRequests":        2,
			"totalRequests":         100,
			"avgResponseTime":       150.5,
			"requestsPerSecond":     3.2,
			"maxModels":             10,
			"maxConcurrentRequests": 8,
			"freeSlots":             6,
			"availableSlots":        5,
			"runtimeFlags": map[string]interface{}{
				"numGpuLayers":   35,
				"contextLength":  4096,
				"numParallel":    4,
				"numThreads":     8,
				"batchSize":      512,
				"gpuSplitMode":   "auto",
				"mainGpu":        0,
				"lowVram":        false,
				"f16kv":          true,
				"kvCacheQuant":   "f16",
				"flashAttention": true,
				"source":         "env",
			},
			"modelContexts": []map[string]interface{}{
				{
					"name": "llama3.1", "contextLength": 4096,
					"contextSource": "env", "effectiveContext": 4096,
					"contextMemoryMB": 1024, "kvCacheMemoryMB": 512,
					"modelMemoryMB": 4000, "totalMemoryMB": 5536,
					"numLayers": 32, "hiddenSize": 4096, "precisionBits": 16,
				},
			},
			"backendCapacity": map[string]interface{}{
				"freeVram":           12231,
				"guaranteedVram":     11000,
				"loadedModelVram":    4000,
				"contextOverheadMB":  1024,
				"availableModels":    []interface{}{},
				"loadableModelCount": 3,
				"mode":               "gpu",
			},
		},
	}
	body2, _ := json.Marshal(metricsPayload)
	resp2, err := http.Post(baseURL+"/api/v1/agents/metrics", "application/json", bytes.NewBuffer(body2))
	require.NoError(t, err)
	resp2.Body.Close()
}

func registerAgentWithModel(t *testing.T, baseURL string, _ *balancer.Proxy, modelName string) {
	t.Helper()

	payload := map[string]interface{}{
		"agentId":    "agent-model-test",
		"hostname":   "test-host-model",
		"host":       "127.0.0.1",
		"ollamaPort": 11434,
		"agentPort":  9094,
		"gpuCount":   1,
		"name":       "Test Agent Model",
		"labels":     []string{"gpu:nvidia"},
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(baseURL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	resp.Body.Close()

	metricsPayload := map[string]interface{}{
		"agentId": "agent-model-test",
		"gpu": map[string]interface{}{
			"usagePercent": 20.0,
			"memoryTotal":  24576,
			"memoryUsed":   8000,
			"memoryFree":   16576,
		},
		"system": map[string]interface{}{
			"cpuUsagePercent": 15.0,
			"memoryTotal":     32768,
			"memoryUsed":      8192,
			"memoryFree":      24576,
		},
		"ollama": map[string]interface{}{
			"runningModels": []map[string]interface{}{
				{"name": modelName, "size": 4294967296, "vramUsage": 4000},
			},
			"activeRequests":        1,
			"totalRequests":         50,
			"requestsPerSecond":     1.5,
			"maxConcurrentRequests": 8,
			"freeSlots":             7,
		},
	}
	body2, _ := json.Marshal(metricsPayload)
	resp2, err := http.Post(baseURL+"/api/v1/agents/metrics", "application/json", bytes.NewBuffer(body2))
	require.NoError(t, err)
	resp2.Body.Close()
}

// ==================== Интеграционный тест: монитор получает данные ====================

func TestMonitorIntegration_DataFlow(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	registerAgentWithFullMetrics(t, baseURL, proxy)
	time.Sleep(300 * time.Millisecond)

	clusterResp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer clusterResp.Body.Close()
	assert.Equal(t, http.StatusOK, clusterResp.StatusCode)

	var clusterState map[string]interface{}
	json.NewDecoder(clusterResp.Body).Decode(&clusterState)

	backends := clusterState["backends"].([]interface{})
	assert.Greater(t, len(backends), 0, "Cluster should have backends after agent registration")

	foundAgent := false
	for _, b := range backends {
		backend := b.(map[string]interface{})
		if hasAgent, ok := backend["hasAgent"].(bool); ok && hasAgent {
			foundAgent = true
			assert.Contains(t, backend, "score")
			assert.Contains(t, backend, "models")
			assert.Contains(t, backend, "vramUsagePercent")
			assert.Contains(t, backend, "vramTotalGB")
			assert.Contains(t, backend, "vramUsedGB")
			break
		}
	}
	assert.True(t, foundAgent, "At least one backend should have hasAgent=true")

	sessResp, err := http.Get(baseURL + "/api/v1/sessions")
	require.NoError(t, err)
	defer sessResp.Body.Close()
	assert.Equal(t, http.StatusOK, sessResp.StatusCode)

	var sessData map[string]interface{}
	json.NewDecoder(sessResp.Body).Decode(&sessData)
	assert.Contains(t, sessData, "sessions", "Sessions response must have 'sessions' field")

	queueResp, err := http.Get(baseURL + "/api/v1/queue/details")
	require.NoError(t, err)
	defer queueResp.Body.Close()
	assert.Equal(t, http.StatusOK, queueResp.StatusCode)

	var queueData map[string]interface{}
	json.NewDecoder(queueResp.Body).Decode(&queueData)
	assert.Contains(t, queueData, "pending_count")
	assert.Contains(t, queueData, "processing_count")

	var clusterModels []string
	for _, b := range backends {
		backend := b.(map[string]interface{})
		if models, ok := backend["models"].([]interface{}); ok {
			for _, m := range models {
				clusterModels = append(clusterModels, m.(string))
			}
		}
	}
	t.Logf("Cluster models: %v", clusterModels)
	assert.Greater(t, len(clusterModels), 0, "Cluster should have at least one model from agent")
}

func TestMonitorIntegration_EmptyState(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	endpoints := []string{
		"/api/v1/cluster",
		"/api/v1/queue/details",
		"/api/v1/sessions",
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			resp, err := http.Get(baseURL + ep)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode,
				"Endpoint %s should return 200 even with empty state", ep)

			var data map[string]interface{}
			err = json.NewDecoder(resp.Body).Decode(&data)
			require.NoError(t, err, "Response from %s should be valid JSON", ep)
		})
	}
}