package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
)

// createTestServer - создание тестового сервера с настроенным API сервером
func createTestServer(t *testing.T) (*httptest.Server, *Server, *balancer.Proxy) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:                "backend1",
				Name:              "Test Backend 1",
				Host:              "localhost",
				OllamaPort:        11434,
				AgentPort:         9090,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Labels:            []string{"gpu:nvidia"},
				Status:            types.StatusHealthy,
			},
			{
				ID:                "backend2",
				Name:              "Test Backend 2",
				Host:              "localhost",
				OllamaPort:        11435,
				AgentPort:         9091,
				Weight:            2,
				MaxConcurrentReqs: 20,
				Labels:            []string{"gpu:amd"},
				Status:            types.StatusUnhealthy,
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
			Enabled:    false, // Отключаем аутентификацию для тестов
			Tokens:     []string{"test-token"},
			HeaderName: "X-API-Token",
		},
	}

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, time.Duration(config.Balancing.HealthCheckInterval)*time.Second, 3)
	server := NewServer(proxy, config, healthChecker)

	testServer := httptest.NewServer(server)

	return testServer, server, proxy
}

// ==================== Тесты для healthHandler ====================

func TestHealthHandler(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/health")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Equal(t, "healthy", response["status"])
	assert.Contains(t, response, "timestamp")
	assert.Equal(t, "1.0.0", response["version"])
}

func TestHealthHandlerMethodNotAllowed(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/health", "application/json", nil)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// ==================== Тесты для clusterHandler ====================

func TestClusterHandler(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/cluster")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response types.ClusterState
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Equal(t, 2, response.TotalBackends)
	assert.GreaterOrEqual(t, response.HealthyBackends, 0)
	assert.NotNil(t, response.Backends)
	assert.NotNil(t, response.Timestamp)
}

func TestClusterHandlerMethodNotAllowed(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/cluster", "application/json", nil)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// ==================== Тесты для backendsHandler ====================

// TestBackendsHandler_Get проверяет обработчик GET /api/v1/backends.
//
// ВАЖНО: createTestServer создаёт 2 бэкенда — backend1 (healthy) и backend2
// (unhealthy). По умолчанию listBackends фильтрует unhealthy-бэкенды (чтобы
// WebUI не показывал «мёртвые» ноды), поэтому передаём ?includeUnhealthy=true,
// чтобы убедиться, что обработчик корректно отдаёт оба бэкенда и что фильтр
// можно отключить явно (используется в monitor/agent endpoints для диагностики).
func TestBackendsHandler_Get(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/backends?includeUnhealthy=true")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Contains(t, response, "backends")
	assert.Contains(t, response, "total")
	assert.Equal(t, float64(2), response["total"])
}

func TestBackendsHandler_Post(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	newBackend := map[string]interface{}{
		"id":                  "backend3",
		"name":                "Test Backend 3",
		"host":                "localhost",
		"ollamaPort":          11436,
		"agentPort":           9092,
		"weight":              1,
		"maxConcurrentRequests": 15,
		"labels":              []string{"gpu:intel"},
	}

	body, _ := json.Marshal(newBackend)
	resp, err := http.Post(server.URL+"/api/v1/backends", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Equal(t, true, response["success"])
	assert.Contains(t, response, "backend")
}

func TestBackendsHandler_Post_InvalidBody(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/backends", "application/json", bytes.NewBuffer([]byte("invalid json")))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, false, response["success"])
}

func TestBackendsHandler_Post_MissingID(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	newBackend := map[string]interface{}{
		"name":       "Test Backend",
		"host":       "localhost",
		"ollamaPort": 11434,
	}

	body, _ := json.Marshal(newBackend)
	resp, err := http.Post(server.URL+"/api/v1/backends", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "Backend ID is required", response["error"])
}

func TestBackendsHandler_Post_MissingHost(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	newBackend := map[string]interface{}{
		"id":         "new-backend",
		"name":       "Test Backend",
		"ollamaPort": 11434,
	}

	body, _ := json.Marshal(newBackend)
	resp, err := http.Post(server.URL+"/api/v1/backends", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "Backend host is required", response["error"])
}

func TestBackendsHandler_Post_Duplicate(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	newBackend := map[string]interface{}{
		"id":         "backend1",
		"name":       "Duplicate Backend",
		"host":       "localhost",
		"ollamaPort": 11434,
	}

	body, _ := json.Marshal(newBackend)
	resp, err := http.Post(server.URL+"/api/v1/backends", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, false, response["success"])
}

func TestBackendsHandler_MethodNotAllowed(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPut, server.URL+"/api/v1/backends", nil)
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// ==================== Тесты для backendHandler ====================

func TestBackendHandler_Get(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/backends/backend1")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// getBackend возвращает BackendMetrics (если есть метрики) или Backend (если нет)
	// Проверяем как map для универсальности
	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Equal(t, "backend1", response["id"])
	// Name может быть в поле "name" (для Backend) или отсутствовать (для BackendMetrics)
	// BackendMetrics не имеет поля name, поэтому проверяем только id
}

func TestBackendHandler_Get_NotFound(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/backends/nonexistent")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestBackendHandler_Put(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	updateData := map[string]interface{}{
		"name":                "Updated Backend 2",
		"host":                "localhost",
		"ollamaPort":          11435,
		"agentPort":           9091,
		"weight":              3,
		"maxConcurrentRequests": 25,
		"labels":              []string{"gpu:amd", "updated"},
	}

	body, _ := json.Marshal(updateData)
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/api/v1/backends/backend2", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Equal(t, true, response["success"])
	assert.Contains(t, response, "backend")
}

func TestBackendHandler_Put_NotFound(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	updateData := map[string]interface{}{
		"name": "Updated Backend",
	}

	body, _ := json.Marshal(updateData)
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/api/v1/backends/nonexistent", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, false, response["success"])
}

func TestBackendHandler_Delete(t *testing.T) {
	server, _, proxy := createTestServer(t)
	defer server.Close()

	// Сначала добавим бэкенд для удаления
	newBackend := types.Backend{
		ID:                "to-delete",
		Name:              "To Delete",
		Host:              "localhost",
		OllamaPort:        11437,
		AgentPort:         9093,
		Weight:            1,
		MaxConcurrentReqs: 10,
		Status:            types.StatusHealthy,
	}
	proxy.AddBackend(newBackend)

	req, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/backends/to-delete", nil)
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Equal(t, true, response["success"])
	assert.Equal(t, "to-delete", response["id"])
}

func TestBackendHandler_Delete_NotFound(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/backends/nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, false, response["success"])
}

func TestBackendHandler_MethodNotAllowed(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPatch, server.URL+"/api/v1/backends/backend1", nil)
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// ==================== Тесты для metricsHandler ====================

func TestMetricsHandler(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/metrics")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Contains(t, response, "timestamp")
	assert.Contains(t, response, "totalBackends")
	assert.Contains(t, response, "healthyBackends")
	assert.Contains(t, response, "backends")
}

func TestMetricsHandler_MethodNotAllowed(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/metrics", "application/json", nil)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// ==================== Тесты для sessionsHandler ====================

func TestSessionsHandler_Get(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/sessions")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Contains(t, response, "sessions")
	assert.Contains(t, response, "total")
}

func TestSessionsHandler_Delete(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Equal(t, true, response["success"])
	assert.Contains(t, response, "message")
}

func TestSessionsHandler_MethodNotAllowed(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPut, server.URL+"/api/v1/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// ==================== Тесты для modelsHandler ====================

func TestModelsHandler(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/models")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Contains(t, response, "backends")
	assert.Contains(t, response, "total")
}

func TestModelsHandler_MethodNotAllowed(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/models", "application/json", nil)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// ==================== Тесты для agentStatsHandler ====================

func TestAgentStatsHandler(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/agents/stats")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Contains(t, response, "totalAgents")
	assert.Contains(t, response, "healthyAgents")
	assert.Contains(t, response, "agents")
	
	agents := response["agents"].([]interface{})
	assert.GreaterOrEqual(t, len(agents), 2)
}

func TestAgentStatsHandler_MethodNotAllowed(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/agents/stats", "application/json", nil)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// ==================== Тесты с аутентификацией ====================

func TestHandlersWithAuthentication(t *testing.T) {
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:                "backend1",
				Name:              "Test Backend 1",
				Host:              "localhost",
				OllamaPort:        11434,
				AgentPort:         9090,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Labels:            []string{"gpu:nvidia"},
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
			Enabled:    true, // Включаем аутентификацию
			Tokens:     []string{"valid-token"},
			HeaderName: "X-API-Token",
		},
	}

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, healthChecker)

	testServer := httptest.NewServer(server)
	defer testServer.Close()

	// Тест без токена - должен вернуть 401
	resp, err := http.Get(testServer.URL + "/api/v1/backends")
	assert.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Тест с валидным токеном - должен вернуть 200
	req, _ := http.NewRequest(http.MethodGet, testServer.URL+"/api/v1/backends", nil)
	req.Header.Set("X-API-Token", "valid-token")
	resp, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Тест с невалидным токеном - должен вернуть 401
	req, _ = http.NewRequest(http.MethodGet, testServer.URL+"/api/v1/backends", nil)
	req.Header.Set("X-API-Token", "invalid-token")
	resp, err = http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// ==================== Тесты граничных случаев ====================

func TestBackendHandler_EmptyID(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/backends/")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestBackendsHandler_InvalidJSON(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/backends", "application/json", bytes.NewBuffer([]byte("{invalid json}")))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHealthHandler_ResponseStructure(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/health")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	// Проверяем, что timestamp можно распарсить как время
	timestampStr, ok := response["timestamp"].(string)
	assert.True(t, ok, "timestamp должен быть строкой")
	_, err = time.Parse(time.RFC3339, timestampStr)
	assert.NoError(t, err, "timestamp должен быть валидным временем RFC3339")
}

func TestClusterHandler_EmptyState(t *testing.T) {
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{}, // Пустой список бэкендов
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
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
			Enabled: false,
			Tokens:  []string{},
		},
	}

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, healthChecker)

	testServer := httptest.NewServer(server)
	defer testServer.Close()

	resp, err := http.Get(testServer.URL + "/api/v1/cluster")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response types.ClusterState
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)

	assert.Equal(t, 0, response.TotalBackends)
	assert.Equal(t, 0, response.HealthyBackends)
	assert.Empty(t, response.Backends)
}

func TestSessionHandler_Delete(t *testing.T) {
	server, _, proxy := createTestServer(t)
	defer server.Close()

	// Создадим сессию для удаления
	proxy.GetSessions() // Инициализация менеджера сессий

	req, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/sessions/test-session-id", nil)
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	// Session handler может вернуть 404 если сессия не найдена
	// или 200 если сессия удалена
	assert.Contains(t, []int{http.StatusOK, http.StatusNotFound}, resp.StatusCode)
}

// ==================== Тесты для agentRegisterHandler ====================

func TestAgentRegisterHandler(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	payload := map[string]interface{}{
		"agentId":    "agent-test-1",
		"hostname":   "test-host",
		"host":       "192.168.1.100",
		"ollamaPort": 11434,
		"agentPort":  9090,
		"gpuCount":   1,
		"name":       "Test Agent",
		"labels":     []string{"gpu:nvidia"},
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(server.URL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, true, response["success"])
	assert.Equal(t, "created", response["action"])
	assert.Equal(t, "agent-test-1", response["agentId"])
}

func TestAgentRegisterHandler_MissingAgentID(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	payload := map[string]interface{}{
		"hostname": "test-host",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(server.URL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "agentId is required", response["error"])
}

func TestAgentRegisterHandler_UpdateExisting(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	// Сначала регистрируем агента
	payload := map[string]interface{}{
		"agentId":  "agent-test-2",
		"host":     "192.168.1.101",
		"name":     "Original Name",
		"labels":   []string{"gpu:nvidia"},
	}
	body, _ := json.Marshal(payload)
	resp, _ := http.Post(server.URL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	resp.Body.Close()

	// Теперь обновляем
	payload["name"] = "Updated Name"
	body, _ = json.Marshal(payload)
	resp, err := http.Post(server.URL+"/api/v1/agents/register", "application/json", bytes.NewBuffer(body))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, true, response["success"])
	assert.Equal(t, "updated", response["action"])
}

func TestAgentRegisterHandler_InvalidJSON(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/agents/register", "application/json", bytes.NewBuffer([]byte("invalid")))
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ==================== Тесты для agentMetricsHandler ====================

func TestAgentMetricsHandler(t *testing.T) {
	server, _, proxy := createTestServer(t)
	defer server.Close()

	// Сначала регистрируем агента
	proxy.AddBackend(types.Backend{
		ID:         "agent-metrics-1",
		Name:       "Agent Metrics",
		Host:       "localhost",
		OllamaPort: 11434,
		AgentPort:  9090,
	})

	metrics := types.BackendMetrics{
		ID: "agent-metrics-1",
		GPU: types.GPUMetrics{
			UsagePercent: 45.5,
			MemoryTotal:  16384,
			MemoryUsed:   8000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 30,
		},
		Ollama: types.OllamaMetrics{
			ActiveRequests:    2,
			RequestsPerSecond: 5.5,
		},
	}
	body, _ := json.Marshal(metrics)

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/agents/metrics", bytes.NewBuffer(body))
	req.Header.Set("X-Agent-ID", "agent-metrics-1")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, "received", response["status"])
}

func TestAgentMetricsHandler_MissingAgentID(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	metrics := types.BackendMetrics{ID: "test"}
	body, _ := json.Marshal(metrics)

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/agents/metrics", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAgentMetricsHandler_InvalidMetrics(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/agents/metrics", bytes.NewBuffer([]byte("invalid")))
	req.Header.Set("X-Agent-ID", "agent-1")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ==================== Тесты для agentHeartbeatHandler ====================

func TestAgentHeartbeatHandler(t *testing.T) {
	server, _, proxy := createTestServer(t)
	defer server.Close()

	proxy.AddBackend(types.Backend{
		ID:         "agent-hb-1",
		Name:       "Agent HB",
		Host:       "localhost",
		OllamaPort: 11434,
		AgentPort:  9090,
	})

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/agents/heartbeat", nil)
	req.Header.Set("X-Agent-ID", "agent-hb-1")

	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	assert.NoError(t, err)
	assert.Equal(t, "ok", response["status"])

	// Расширенный heartbeat: serverTime, acknowledged, config
	assert.Contains(t, response, "serverTime")
	assert.Contains(t, response, "acknowledged")
	assert.Contains(t, response, "config")

	// Проверяем, что serverTime и acknowledged валидные RFC3339
	for _, field := range []string{"serverTime", "acknowledged"} {
		ts, ok := response[field].(string)
		assert.True(t, ok, "%s должен быть строкой", field)
		_, err = time.Parse(time.RFC3339Nano, ts)
		assert.NoError(t, err, "%s должен быть валидным временем", field)
	}

	// config содержит runtime-лимиты
	config, ok := response["config"].(map[string]interface{})
	assert.True(t, ok, "config должен быть объектом")
	assert.Contains(t, config, "maxModels")
	assert.Contains(t, config, "maxConcurrentRequests")
}

func TestAgentHeartbeatHandler_MissingAgentID(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/agents/heartbeat", nil)

	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAgentHeartbeatHandler_MethodNotAllowed(t *testing.T) {
	server, _, _ := createTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/agents/heartbeat")
	assert.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}
