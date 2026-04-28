package tests

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClusterStateRPSField - проверка, что ClusterState содержит поле RPS
func TestClusterStateRPSField(t *testing.T) {
	t.Parallel()

	// Создаем ClusterState вручную
	state := types.ClusterState{
		Timestamp:       time.Now().UTC(),
		TotalBackends:   1,
		HealthyBackends: 1,
		TotalRequests:   42,
		ActiveRequests:  3,
		QueuedRequests:  0,
		RPS:             12.5,
		TotalGPUUsage:   45.0,
		Backends: []types.BackendMetrics{
			{
				ID:     "test-backend",
				Status: types.StatusHealthy,
				Ollama: types.OllamaMetrics{
					ActiveRequests:    2,
					RequestsPerSecond: 8.3,
					TotalRequests:     25,
					FreeSlots:         8,
				},
			},
		},
	}

	// Проверяем наличие поля RPS
	assert.Equal(t, 12.5, state.RPS, "RPS должен быть установлен")
	assert.Equal(t, 1, state.TotalBackends)
	assert.Equal(t, 1, state.HealthyBackends)
	assert.Equal(t, int64(42), state.TotalRequests)
	assert.Equal(t, 3, state.ActiveRequests)

	// Проверяем бэкенд
	require.Len(t, state.Backends, 1)
	backend := state.Backends[0]
	assert.Equal(t, types.StatusHealthy, backend.Status)
	assert.Equal(t, 8.3, backend.Ollama.RequestsPerSecond, "RequestsPerSecond бэкенда должен быть установлен")
}

// TestClusterStateJSONSerialization - проверка сериализации ClusterState в JSON с сохранением RPS
func TestClusterStateJSONSerialization(t *testing.T) {
	t.Parallel()

	state := types.ClusterState{
		Timestamp:       time.Now().UTC(),
		TotalBackends:   2,
		HealthyBackends: 1,
		TotalRequests:   100,
		ActiveRequests:  5,
		QueuedRequests:  0,
		RPS:             12.5,
		TotalGPUUsage:   45.0,
		Backends: []types.BackendMetrics{
			{
				ID:     "backend-1",
				Status: types.StatusHealthy,
				Ollama: types.OllamaMetrics{
					ActiveRequests:    3,
					RequestsPerSecond: 10.5,
					TotalRequests:     50,
					FreeSlots:         7,
				},
			},
		},
	}

	// Сериализуем в JSON
	data, err := json.Marshal(state)
	require.NoError(t, err, "Сериализация должна работать")

	// Десериализуем обратно
	var decoded types.ClusterState
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err, "Десериализация должна работать")

	// Проверяем поля
	assert.Equal(t, state.TotalBackends, decoded.TotalBackends)
	assert.Equal(t, state.HealthyBackends, decoded.HealthyBackends)
	assert.Equal(t, state.TotalRequests, decoded.TotalRequests)
	assert.Equal(t, state.ActiveRequests, decoded.ActiveRequests)
	assert.Equal(t, state.RPS, decoded.RPS, "RPS должен сохраняться при сериализации")
	assert.Equal(t, state.TotalGPUUsage, decoded.TotalGPUUsage)
	assert.Len(t, decoded.Backends, 1)
	assert.Equal(t, state.Backends[0].Ollama.RequestsPerSecond, decoded.Backends[0].Ollama.RequestsPerSecond, "RequestsPerSecond должен сохраняться")
}

// TestLoadBalancerConfigStructure - проверка структуры конфигурации
func TestLoadBalancerConfigStructure(t *testing.T) {
	t.Parallel()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			HealthCheckInterval: 5,
			MetricsInterval:     5,
			RequestTimeout:      30,
			QueueMaxSize:        100,
			QueueWorkers:        4,
		},
		API: types.APISettings{
			RateLimit: 100,
			RateBurst: 200,
		},
		Auth: types.AuthConfig{
			Enabled: false,
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
				MaxUsagePercent: 95,
			},
			Disk: types.DiskLimits{
				MinFreeMB: 1024,
			},
		},
		Backends: []types.Backend{
			{
				ID:                "test-backend-1",
				Name:              "Test Backend 1",
				Host:              "localhost",
				OllamaPort:        11434,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
	}

	// Проверяем структуру
	assert.Equal(t, "localhost", config.LoadBalancer.Host)
	assert.Equal(t, 8080, config.LoadBalancer.Port)
	assert.Equal(t, 8081, config.LoadBalancer.APIPort)
	assert.Equal(t, types.AlgorithmResourceAware, config.Balancing.Algorithm)
	assert.Equal(t, 5, config.Balancing.HealthCheckInterval)
	assert.Equal(t, 5, config.Balancing.MetricsInterval)
	assert.Equal(t, 30, config.Balancing.RequestTimeout)
	assert.Equal(t, 100, config.Balancing.QueueMaxSize)
	assert.Equal(t, 4, config.Balancing.QueueWorkers)
	assert.Equal(t, 100.0, config.API.RateLimit)
	assert.Equal(t, 200.0, config.API.RateBurst)
	assert.False(t, config.Auth.Enabled)

	// Проверяем ресурсы
	assert.Equal(t, 90.0, config.Resources.GPU.MaxUsagePercent)
	assert.Equal(t, 95.0, config.Resources.GPU.MaxVRAMUsagePercent)
	assert.Equal(t, 85, config.Resources.GPU.MaxTemperature)
	assert.Equal(t, 90.0, config.Resources.CPU.MaxUsagePercent)
	assert.Equal(t, 95.0, config.Resources.Memory.MaxUsagePercent)
	assert.Equal(t, uint64(1024), config.Resources.Disk.MinFreeMB)

	// Проверяем бэкенды
	require.Len(t, config.Backends, 1)
	backend := config.Backends[0]
	assert.Equal(t, "test-backend-1", backend.ID)
	assert.Equal(t, "Test Backend 1", backend.Name)
	assert.Equal(t, types.StatusHealthy, backend.Status)
	assert.Equal(t, 11434, backend.OllamaPort)
}

// TestBackendStatusTypes - проверка типов статуса бэкенда
func TestBackendStatusTypes(t *testing.T) {
	t.Parallel()

	// Проверяем все возможные статусы
	assert.Equal(t, types.BackendStatus("healthy"), types.StatusHealthy)
	assert.Equal(t, types.BackendStatus("unhealthy"), types.StatusUnhealthy)
	assert.Equal(t, types.BackendStatus("offline"), types.StatusOffline)
	assert.Equal(t, types.BackendStatus("starting"), types.StatusStarting)

	// Проверяем использование в структурах
	backend := types.Backend{
		ID:     "test",
		Status: types.StatusHealthy,
	}
	assert.Equal(t, types.StatusHealthy, backend.Status)

	metrics := types.BackendMetrics{
		ID:     "test",
		Status: types.StatusHealthy,
	}
	assert.Equal(t, types.StatusHealthy, metrics.Status)
}

// TestOllamaMetricsStructure - проверка структуры OllamaMetrics
func TestOllamaMetricsStructure(t *testing.T) {
	t.Parallel()

	metrics := types.OllamaMetrics{
		RunningModels: []types.RunningModel{
			{
				Name:          "llama3.2",
				Size:          2000000000,
				VRAMUsage:     1500,
				RAMUsage:      0,
				LoadCount:     5,
				Family:        "llama",
				ParameterSize: "3.2B",
				Quantization:  "Q4_0",
			},
		},
		ActiveRequests:        3,
		TotalRequests:         42,
		AvgResponseTime:       150.5,
		RequestsPerSecond:     8.5,
		MaxModels:             -1,
		MaxConcurrentRequests: 10,
		FreeSlots:             7,
	}

	assert.Equal(t, 3, metrics.ActiveRequests)
	assert.Equal(t, int64(42), metrics.TotalRequests)
	assert.Equal(t, 150.5, metrics.AvgResponseTime)
	assert.Equal(t, 8.5, metrics.RequestsPerSecond)
	assert.Equal(t, -1, metrics.MaxModels)
	assert.Equal(t, 10, metrics.MaxConcurrentRequests)
	assert.Equal(t, 7, metrics.FreeSlots)
	require.Len(t, metrics.RunningModels, 1)
	assert.Equal(t, "llama3.2", metrics.RunningModels[0].Name)
}

// TestClusterStateWithEmptyBackends - проверка ClusterState с пустыми бэкендами
func TestClusterStateWithEmptyBackends(t *testing.T) {
	t.Parallel()

	state := types.ClusterState{
		Timestamp:       time.Now().UTC(),
		TotalBackends:   0,
		HealthyBackends: 0,
		TotalRequests:   0,
		ActiveRequests:  0,
		QueuedRequests:  0,
		RPS:             0,
		TotalGPUUsage:   0,
		Backends:        []types.BackendMetrics{},
	}

	assert.Equal(t, 0, state.TotalBackends)
	assert.Equal(t, 0, state.HealthyBackends)
	assert.Equal(t, int64(0), state.TotalRequests)
	assert.Equal(t, 0, state.ActiveRequests)
	assert.Equal(t, 0.0, state.RPS)
	assert.Equal(t, 0.0, state.TotalGPUUsage)
	assert.Empty(t, state.Backends)
}

// TestBalancingAlgorithmTypes - проверка алгоритмов балансировки
func TestBalancingAlgorithmTypes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, types.BalancingAlgorithm("roundrobin"), types.AlgorithmRoundRobin)
	assert.Equal(t, types.BalancingAlgorithm("leastconn"), types.AlgorithmLeastConn)
	assert.Equal(t, types.BalancingAlgorithm("resource-aware"), types.AlgorithmResourceAware)
	assert.Equal(t, types.BalancingAlgorithm("model-affinity"), types.AlgorithmModelAffinity)

	config := types.BalancingSettings{
		Algorithm: types.AlgorithmResourceAware,
	}
	assert.Equal(t, types.AlgorithmResourceAware, config.Algorithm)
}

// TestMetricsSnapshotStructure - проверка структуры MetricsSnapshot
func TestMetricsSnapshotStructure(t *testing.T) {
	t.Parallel()

	snapshot := types.MetricsSnapshot{
		Timestamp:         time.Now().UTC(),
		GPUUsagePercent:   45.5,
		VRAMUsagePercent:  60.2,
		RAMUsagePercent:   30.1,
		ActiveRequests:    5,
		RunningModels:     2,
		FreeSlots:         8,
		RequestsPerSecond: 12.3,
	}

	assert.Equal(t, 45.5, snapshot.GPUUsagePercent)
	assert.Equal(t, 60.2, snapshot.VRAMUsagePercent)
	assert.Equal(t, 30.1, snapshot.RAMUsagePercent)
	assert.Equal(t, 5, snapshot.ActiveRequests)
	assert.Equal(t, 2, snapshot.RunningModels)
	assert.Equal(t, 8, snapshot.FreeSlots)
	assert.Equal(t, 12.3, snapshot.RequestsPerSecond)
}

// TestResourceLimitsStructure - проверка структуры ResourceLimits
func TestResourceLimitsStructure(t *testing.T) {
	t.Parallel()

	limits := types.ResourceLimits{
		GPU: types.GPULimits{
			MaxUsagePercent:     90.0,
			MaxVRAMUsagePercent: 95.0,
			MaxTemperature:      85,
		},
		CPU: types.CPULimits{
			MaxUsagePercent: 80.0,
		},
		Memory: types.MemoryLimits{
			MaxUsagePercent: 85.0,
		},
		Disk: types.DiskLimits{
			MinFreeMB: 10240,
		},
	}

	assert.Equal(t, 90.0, limits.GPU.MaxUsagePercent)
	assert.Equal(t, 95.0, limits.GPU.MaxVRAMUsagePercent)
	assert.Equal(t, 85, limits.GPU.MaxTemperature)
	assert.Equal(t, 80.0, limits.CPU.MaxUsagePercent)
	assert.Equal(t, 85.0, limits.Memory.MaxUsagePercent)
	assert.Equal(t, uint64(10240), limits.Disk.MinFreeMB)
}

// TestURLParsing - базовая проверка работы с URL
func TestURLParsing(t *testing.T) {
	t.Parallel()

	// Проверяем парсинг URL для API endpoints
	apiURL, err := url.Parse("http://localhost:8081/api/v1/cluster")
	require.NoError(t, err)
	assert.Equal(t, "http", apiURL.Scheme)
	assert.Equal(t, "localhost:8081", apiURL.Host)
	assert.Equal(t, "/api/v1/cluster", apiURL.Path)
}