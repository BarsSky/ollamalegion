package tests

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== Тесты dispatch-логики в разных OperatingMode ====================

func TestOperatingMode_Dispatch_Standard(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})

	proxy := balancer.NewProxy(cfg)
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	// Стандартный режим — dispatch через selectBackend должен работать
	backendID := proxy.SelectBackend("llama3.1")
	assert.NotEmpty(t, backendID, "Standard mode: should select a backend for known model")

	// Для неизвестной модели тоже должен выбрать бэкенд
	backendID2 := proxy.SelectBackend("unknown-model")
	assert.NotEmpty(t, backendID2, "Standard mode: should select a backend even for unknown model")
}

func TestOperatingMode_Dispatch_Replication(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		{ID: "gpu-2", Name: "G2", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.OperatingMode = "replication"
	cfg.Balancing.ModelReplication = types.ModelReplicationConfig{
		Enabled:             true,
		DefaultMinInstances: 1,
		DefaultMaxInstances: 2,
	}

	proxy := balancer.NewProxy(cfg)
	require.NotNil(t, proxy.GetModelReplicationManager(), "Replication mode: ModelReplicationManager should be initialized")
	require.NotNil(t, proxy.GetReplicationSelector(), "Replication mode: ReplicationSelector should be initialized")

	// Устанавливаем метрики
	for _, id := range []string{"gpu-1", "gpu-2"} {
		proxy.UpdateMetrics(id, &types.BackendMetrics{
			GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
			System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
			Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
		})
	}

	// Dispatch должен работать в replication режиме
	backendID := proxy.SelectBackend("llama3.1")
	assert.NotEmpty(t, backendID, "Replication mode: should select a backend")

	// Проверяем что ClusterState содержит правильный operatingMode
	state := proxy.GetClusterState()
	assert.Equal(t, "replication", state.OperatingMode)
}

func TestOperatingMode_Dispatch_VirtualRouter(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", Type: types.BackendTypeLlamaCpp, CppWorkerPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.OperatingMode = "virtual_router"
	cfg.Balancing.VirtualModels = types.VirtualModelsConfig{
		Enabled: true,
	}

	proxy := balancer.NewProxy(cfg)
	require.NotNil(t, proxy.GetVirtualModelRouter(), "Virtual Router mode: VirtualModelRouter should be initialized")

	// Устанавливаем метрики
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	// Dispatch должен работать
	backendID := proxy.SelectBackend("any-model")
	assert.NotEmpty(t, backendID, "Virtual Router mode: should select a backend")

	state := proxy.GetClusterState()
	assert.Equal(t, "virtual_router", state.OperatingMode)
}

func TestOperatingMode_Dispatch_RpcCoordinator(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.OperatingMode = "rpc_coordinator"
	cfg.Balancing.RpcCoordinator = types.RpcCoordinatorConfig{
		Enabled:        true,
		CoordinatorURL: "http://localhost:18050",
		WorkerPort:     18050,
		Protocol:       "http",
	}

	proxy := balancer.NewProxy(cfg)

	// Устанавливаем метрики
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	// Dispatch должен работать
	backendID := proxy.SelectBackend("any-model")
	assert.NotEmpty(t, backendID, "RPC Coordinator mode: should select a backend")

	state := proxy.GetClusterState()
	assert.Equal(t, "rpc_coordinator", state.OperatingMode)
}

func TestOperatingMode_Dispatch_DistributedInference(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", Type: types.BackendTypeLlamaCpp, CppWorkerPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.OperatingMode = "distributed_inference"
	cfg.Balancing.DistInference = types.DistInferenceConfig{
		Enabled:  true,
		GrpcPort: 19000,
	}

	proxy := balancer.NewProxy(cfg)

	// Устанавливаем метрики
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	// Dispatch должен работать
	backendID := proxy.SelectBackend("any-model")
	assert.NotEmpty(t, backendID, "Distributed Inference mode: should select a backend")

	state := proxy.GetClusterState()
	assert.Equal(t, "distributed_inference", state.OperatingMode)
}

func TestOperatingMode_DispatchQueue_Standard(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.QueueWorkers = 2
	cfg.Balancing.QueueMaxSize = 10
	cfg.Balancing.QueueTimeout = 5

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	// Полный HTTP цикл через ServeHTTP
	lb := httptest.NewServer(proxy)
	defer lb.Close()

	req, _ := http.NewRequest("POST", lb.URL+"/api/chat",
		strings.NewReader(`{"model":"llama3.1","messages":[{"role":"user","content":"Hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Name", "TestClient")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "Standard mode: full HTTP request should succeed")
}

func TestOperatingMode_DispatchQueue_Replication(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.OperatingMode = "replication"
	cfg.Balancing.ModelReplication = types.ModelReplicationConfig{
		Enabled: true,
	}
	cfg.Balancing.QueueWorkers = 2
	cfg.Balancing.QueueMaxSize = 10

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	req, _ := http.NewRequest("POST", lb.URL+"/api/chat",
		strings.NewReader(`{"model":"llama3.1","messages":[{"role":"user","content":"Hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Name", "TestClient")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "Replication mode: full HTTP request should succeed")
}

func TestOperatingMode_Dispatch_AllModes_SelectBackendNotEmpty(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	modes := []struct {
		mode   string
		config *types.LoadBalancerConfig
	}{
		{
			mode: "standard",
			config: func() *types.LoadBalancerConfig {
				c := newTestConfig([]types.Backend{
					{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
				})
				c.Balancing.OperatingMode = "standard"
				return c
			}(),
		},
		{
			mode: "replication",
			config: func() *types.LoadBalancerConfig {
				c := newTestConfig([]types.Backend{
					{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
				})
				c.Balancing.OperatingMode = "replication"
				c.Balancing.ModelReplication = types.ModelReplicationConfig{Enabled: true}
				return c
			}(),
		},
		{
			mode: "rpc_coordinator",
			config: func() *types.LoadBalancerConfig {
				c := newTestConfig([]types.Backend{
					{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
				})
				c.Balancing.OperatingMode = "rpc_coordinator"
				c.Balancing.RpcCoordinator = types.RpcCoordinatorConfig{Enabled: true, CoordinatorURL: "http://localhost:18050", WorkerPort: 18050}
				return c
			}(),
		},
		{
			mode: "virtual_router",
			config: func() *types.LoadBalancerConfig {
				c := newTestConfig([]types.Backend{
					{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", Type: types.BackendTypeLlamaCpp, CppWorkerPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
				})
				c.Balancing.OperatingMode = "virtual_router"
				c.Balancing.VirtualModels = types.VirtualModelsConfig{Enabled: true}
				return c
			}(),
		},
		{
			mode: "distributed_inference",
			config: func() *types.LoadBalancerConfig {
				c := newTestConfig([]types.Backend{
					{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", Type: types.BackendTypeLlamaCpp, CppWorkerPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
				})
				c.Balancing.OperatingMode = "distributed_inference"
				c.Balancing.DistInference = types.DistInferenceConfig{Enabled: true, GrpcPort: 19000}
				return c
			}(),
		},
	}

	for _, tc := range modes {
		t.Run(tc.mode, func(t *testing.T) {
			proxy := balancer.NewProxy(tc.config)
			proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
				GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
				System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
				Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "test-model", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
			})

			backendID := proxy.SelectBackend("test-model")
			assert.NotEmpty(t, backendID, "%s mode: SelectBackend should return non-empty backendID", tc.mode)

			state := proxy.GetClusterState()
			assert.Equal(t, tc.mode, state.OperatingMode, "%s mode: ClusterState should have correct operatingMode", tc.mode)
		})
	}
}

func TestOperatingMode_DispatchStats_ResetAndRead(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "gpu-1", Name: "G1", Host: "127.0.0.1", OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})

	proxy := balancer.NewProxy(cfg)
	proxy.UpdateMetrics("gpu-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	// Сбрасываем статистику
	proxy.ResetDispatchCounters()

	// Делаем dispatch
	proxy.SelectBackend("llama3.1")

	// Читаем статистику
	stats := proxy.GetDispatchStats()
	assert.NotNil(t, stats, "Dispatch stats should be available")
	// В стандартном режиме dispatch может пойти по affinity или load
	// Просто проверяем что stats структура не nil
}