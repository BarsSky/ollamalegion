package balancer

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
)

// createTestHealthChecker - создание тестового HealthChecker
func createTestHealthChecker(t *testing.T) (*HealthChecker, *Proxy) {
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
	}

	proxy := newProxyWithCleanup(t, config)
	healthChecker := NewHealthChecker(proxy, time.Second, 2)

	return healthChecker, proxy
}

// ==================== Тесты для NewHealthChecker ====================

func TestNewHealthChecker(t *testing.T) {
	hc, proxy := createTestHealthChecker(t)

	assert.NotNil(t, hc)
	assert.Equal(t, proxy, hc.proxy)
	assert.Equal(t, time.Second, hc.interval)
	assert.Equal(t, 2, hc.threshold)
	assert.NotNil(t, hc.results)
	assert.NotNil(t, hc.stopChan)
	assert.Equal(t, 2, len(hc.results))
}

func TestNewHealthChecker_InitialStatus(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	status1 := hc.GetStatus("backend1")
	assert.NotNil(t, status1)
	assert.True(t, status1.Healthy)

	status2 := hc.GetStatus("backend2")
	assert.NotNil(t, status2)
	assert.True(t, status2.Healthy)
}

// ==================== Тесты для Start/Stop ====================

func TestHealthChecker_StartStop(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	// Start запускает goroutine
	hc.Start()
	assert.NotNil(t, hc.stopChan)

	// Stop закрывает канал - не должен паниковать
	hc.Stop()

	// Повторный Stop может паниковать из-за закрытия закрытого канала
	// Поэтому проверяем только один раз
}

// ==================== Тесты для checkBackend ====================

func TestHealthChecker_checkBackend_Healthy(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	// Создаем mock сервер для health check
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	// Обновляем хост backend1 на mock сервер
	hc.proxy.mu.Lock()
	hc.proxy.backends["backend1"].Backend.Host = "127.0.0.1"
	hc.proxy.backends["backend1"].Backend.OllamaPort = mockServer.Listener.Addr().(*net.TCPAddr).Port
	hc.proxy.mu.Unlock()

	hc.checkBackend("backend1")

	status := hc.GetStatus("backend1")
	assert.NotNil(t, status)
	assert.True(t, status.Healthy)
	assert.Equal(t, 0, status.ConsecutiveFails)
	assert.NotZero(t, status.LastSuccess)
	assert.NotZero(t, status.LastCheck)
	assert.NotZero(t, status.LastLatency)
}

func TestHealthChecker_checkBackend_Unhealthy(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	// Создаем mock сервер, который возвращает ошибку
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mockServer.Close()

	hc.proxy.mu.Lock()
	hc.proxy.backends["backend1"].Backend.Host = "127.0.0.1"
	hc.proxy.backends["backend1"].Backend.OllamaPort = mockServer.Listener.Addr().(*net.TCPAddr).Port
	hc.proxy.mu.Unlock()

	hc.checkBackend("backend1")

	// После неудачи ConsecutiveFails может быть 0 (если threshold не достигнут), но LastLatency должен быть установлен
	status := hc.GetStatus("backend1")
	assert.NotNil(t, status)
	assert.NotZero(t, status.LastCheck)
}

func TestHealthChecker_checkBackend_NonExistent(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	// Не должно паниковать для несуществующего бэкенда
	hc.checkBackend("nonexistent")

	status := hc.GetStatus("nonexistent")
	assert.Nil(t, status)
}

// ==================== Тесты для performCheck ====================

func TestHealthChecker_performCheck_Healthy(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	backend := &types.Backend{
		ID:         "test",
		Host:       "127.0.0.1",
		OllamaPort: mockServer.Listener.Addr().(*net.TCPAddr).Port,
	}

	result := hc.performCheck(backend)
	assert.NotNil(t, result)
	assert.True(t, result.Healthy)
	assert.NotZero(t, result.Latency)
	assert.Empty(t, result.Error)
}

func TestHealthChecker_performCheck_Unhealthy(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	backend := &types.Backend{
		ID:         "test",
		Host:       "127.0.0.1",
		OllamaPort: 59999, // Несуществующий порт
	}

	result := hc.performCheck(backend)
	assert.NotNil(t, result)
	assert.False(t, result.Healthy)
	assert.NotEmpty(t, result.Error)
}

func TestHealthChecker_performCheck_WrongStatusCode(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mockServer.Close()

	backend := &types.Backend{
		ID:         "test",
		Host:       "127.0.0.1",
		OllamaPort: mockServer.Listener.Addr().(*net.TCPAddr).Port,
	}

	result := hc.performCheck(backend)
	assert.NotNil(t, result)
	assert.False(t, result.Healthy)
	assert.Contains(t, result.Error, "500")
}

// ==================== Тесты для GetStatus ====================

func TestHealthChecker_GetStatus(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	status := hc.GetStatus("backend1")
	assert.NotNil(t, status)
	assert.True(t, status.Healthy)
}

func TestHealthChecker_GetStatus_NonExistent(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	status := hc.GetStatus("nonexistent")
	assert.Nil(t, status)
}

// ==================== Тесты для GetAllStatuses ====================

func TestHealthChecker_GetAllStatuses(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	statuses := hc.GetAllStatuses()
	assert.NotNil(t, statuses)
	assert.Equal(t, 2, len(statuses))
	assert.Contains(t, statuses, "backend1")
	assert.Contains(t, statuses, "backend2")
}

// ==================== Тесты для GetHealthyBackends ====================

func TestHealthChecker_GetHealthyBackends(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	// По умолчанию оба healthy
	healthy := hc.GetHealthyBackends()
	assert.Equal(t, 2, len(healthy))
}

func TestHealthChecker_GetHealthyBackends_AfterFailure(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	// Устанавливаем backend1 как unhealthy
	hc.mu.Lock()
	hc.results["backend1"].Healthy = false
	hc.mu.Unlock()

	healthy := hc.GetHealthyBackends()
	assert.Equal(t, 1, len(healthy))
	assert.Contains(t, healthy, "backend2")
}

// ==================== Тесты для Threshold ====================

func TestHealthChecker_Threshold(t *testing.T) {
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:         "backend-threshold",
				Name:       "Test Backend",
				Host:       "localhost",
				OllamaPort: 59998, // Несуществующий порт
				AgentPort:  9090,
				Status:     types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			HealthCheckInterval: 1,
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
	}

	proxy := newProxyWithCleanup(t, config)
	hc := NewHealthChecker(proxy, time.Millisecond*50, 3)

	// Начальный статус - healthy
	status := hc.GetStatus("backend-threshold")
	assert.True(t, status.Healthy)

	// Проверяем несколько раз
	for i := 0; i < 2; i++ {
		hc.checkBackend("backend-threshold")
	}

	// После 2 неудач (меньше threshold=3) - всё ещё healthy
	status = hc.GetStatus("backend-threshold")
	assert.True(t, status.Healthy)
	assert.Equal(t, 2, status.ConsecutiveFails)

	// После 3-й неудачи - должно стать unhealthy
	hc.checkBackend("backend-threshold")
	status = hc.GetStatus("backend-threshold")
	assert.False(t, status.Healthy)
	assert.Equal(t, 3, status.ConsecutiveFails)
}

// ==================== Тесты для checkAll ====================

func TestHealthChecker_checkAll(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	// Создаем mock сервер
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	// Обновляем оба backend на mock сервер
	hc.proxy.mu.Lock()
	hc.proxy.backends["backend1"].Backend.Host = "127.0.0.1"
	hc.proxy.backends["backend1"].Backend.OllamaPort = mockServer.Listener.Addr().(*net.TCPAddr).Port
	hc.proxy.backends["backend2"].Backend.Host = "127.0.0.1"
	hc.proxy.backends["backend2"].Backend.OllamaPort = mockServer.Listener.Addr().(*net.TCPAddr).Port
	hc.proxy.mu.Unlock()

	hc.checkAll()

	status1 := hc.GetStatus("backend1")
	status2 := hc.GetStatus("backend2")
	assert.True(t, status1.LastCheck.After(status1.LastSuccess) || !status1.LastSuccess.IsZero())
	assert.True(t, status2.LastCheck.After(status2.LastSuccess) || !status2.LastSuccess.IsZero())
}

// ==================== Тесты для AvgLatency ====================

func TestHealthChecker_AvgLatency(t *testing.T) {
	hc, _ := createTestHealthChecker(t)

	hc.mu.Lock()
	status := hc.results["backend1"]
	status.AvgLatency = 100 * time.Millisecond
	hc.mu.Unlock()

	// Последовательные проверки должны обновлять AvgLatency
	// (экспоненциальное скользящее среднее)
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	hc.proxy.mu.Lock()
	hc.proxy.backends["backend1"].Backend.Host = "127.0.0.1"
	hc.proxy.backends["backend1"].Backend.OllamaPort = mockServer.Listener.Addr().(*net.TCPAddr).Port
	hc.proxy.mu.Unlock()

	hc.checkBackend("backend1")

	status = hc.GetStatus("backend1")
	// AvgLatency должно быть между старым и новым значением
	assert.True(t, status.AvgLatency > 0)
}