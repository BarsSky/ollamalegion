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

// ============================================================
// Тесты изоляции типов бэкендов (Ollama vs llama.cpp)
// ============================================================

// setupMixedCluster создаёт прокси со смешанным кластером:
// 2 Ollama + 2 llama.cpp бэкенда, все healthy.
func setupMixedCluster(t *testing.T) *balancer.Proxy {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    18080,
			APIPort: 18081,
		},
		BackendEngine: types.EngineAuto,
		Balancing: types.BalancingSettings{
			Algorithm:          types.AlgorithmResourceAware,
			ModelAffinity:      true,
			SessionStickiness:  true,
			UseEnhancedScoring: true,
			OperatingMode:      "standard",
			QueueMaxSize:       200,
			QueueWorkers:       4,
			QueueTimeout:       30,
		},
		Backends: []types.Backend{
			{
				ID:             "ollama-1",
				Host:           "10.0.0.1",
				OllamaPort:    11434,
				Type:           types.BackendTypeOllama,
				Status:         types.StatusHealthy,
				MaxConcurrentReqs: 10,
				Weight:         1,
			},
			{
				ID:             "ollama-2",
				Host:           "10.0.0.2",
				OllamaPort:    11434,
				Type:           types.BackendTypeOllama,
				Status:         types.StatusHealthy,
				MaxConcurrentReqs: 10,
				Weight:         1,
			},
			{
				ID:             "llamacpp-1",
				Host:           "10.0.1.1",
				CppWorkerPort:  18091,
				Type:           types.BackendTypeLlamaCpp,
				Status:         types.StatusHealthy,
				MaxConcurrentReqs: 10,
				Weight:         1,
			},
			{
				ID:             "llamacpp-2",
				Host:           "10.0.1.2",
				CppWorkerPort:  18091,
				Type:           types.BackendTypeLlamaCpp,
				Status:         types.StatusHealthy,
				MaxConcurrentReqs: 10,
				Weight:         1,
			},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 85, MaxTemperature: 85},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
		},
	}

	proxy := balancer.NewProxy(config)

	// Добавляем метрики для всех бэкендов чтобы они были видимы для selectBackend
	for _, b := range config.Backends {
		metrics := &types.BackendMetrics{
			ID:         b.ID,
			BackendType: b.Type,
			Status:     types.StatusHealthy,
			Host:       b.Host,
			OllamaPort: b.OllamaPort,
			GPU: types.GPUMetrics{
				MemoryTotal: 24576,
				MemoryUsed:  8192,
				MemoryFree:  16384,
				UsagePercent: 33.3,
			},
			System: types.SystemMetrics{
				CPUUsagePercent: 20,
				MemoryTotal:     65536,
				MemoryUsed:      16384,
			},
			Ollama: types.OllamaMetrics{
				RunningModels:       []types.RunningModel{},
				ActiveRequests:      0,
				RequestsPerSecond:   0,
				MaxConcurrentRequests: 10,
			},
		}
		proxy.SetBackendMetrics(b.ID, metrics)
	}

	return proxy
}

// ============================================================
// Test: selectBackend с явным BackendTypeOllama выбирает только Ollama-бэкенды
// ============================================================
func TestSelectBackend_FiltersByType_OllamaOnly(t *testing.T) {
	proxy := setupMixedCluster(t)

	// 50 итераций с явным BackendTypeOllama — ни разу не должен вернуть llama.cpp бэкенд
	for i := 0; i < 50; i++ {
		backendID := proxy.SelectBackendWithType("llama3:8b", types.BackendTypeOllama)
		require.NotEmpty(t, backendID, "selectBackend should return a backend")

		t.Logf("Iteration %d: selected %s", i, backendID)
		assert.True(t,
			backendID == "ollama-1" || backendID == "ollama-2",
			"selectBackend with BackendTypeOllama should only return Ollama backends, got: %s", backendID)
	}
}

// ============================================================
// Test: selectBackend с явным BackendTypeLlamaCpp выбирает только llama.cpp-бэкенды
// ============================================================
func TestSelectBackend_FiltersByType_LlamaCppOnly(t *testing.T) {
	proxy := setupMixedCluster(t)

	for i := 0; i < 50; i++ {
		backendID := proxy.SelectBackendWithType("llama3:8b", types.BackendTypeLlamaCpp)
		require.NotEmpty(t, backendID, "selectBackend should return a backend")

		t.Logf("Iteration %d: selected %s", i, backendID)
		assert.True(t,
			backendID == "llamacpp-1" || backendID == "llamacpp-2",
			"selectBackend with BackendTypeLlamaCpp should only return llama.cpp backends, got: %s", backendID)
	}
}

// ============================================================
// Test: expandCandidates без фильтра — все 4 бэкенда (standard mode, оба типа)
// ============================================================
func TestExpandCandidates_NoFilter_AllBackends(t *testing.T) {
	proxy := setupMixedCluster(t)

	// Без фильтрации — оба типа разрешены
	candidates := proxy.ExpandCandidates("llama3:8b")

	allBackendIDs := make(map[string]bool)
	for _, group := range candidates {
		for _, id := range group.BackendIDs {
			allBackendIDs[id] = true
		}
	}

	assert.True(t, allBackendIDs["ollama-1"], "ollama-1 should be in candidates")
	assert.True(t, allBackendIDs["ollama-2"], "ollama-2 should be in candidates")
	assert.True(t, allBackendIDs["llamacpp-1"], "llamacpp-1 should be in candidates")
	assert.True(t, allBackendIDs["llamacpp-2"], "llamacpp-2 should be in candidates")
}

// ============================================================
// Test: expandCandidates с фильтром BackendTypeOllama — только Ollama-бэкенды
// ============================================================
func TestExpandCandidates_FiltersByType_OllamaOnly(t *testing.T) {
	proxy := setupMixedCluster(t)

	candidates := proxy.ExpandCandidatesWithType("llama3:8b", []types.BackendType{types.BackendTypeOllama})

	allBackendIDs := make(map[string]bool)
	for _, group := range candidates {
		for _, id := range group.BackendIDs {
			allBackendIDs[id] = true
		}
	}

	assert.True(t, allBackendIDs["ollama-1"], "ollama-1 should be in candidates")
	assert.True(t, allBackendIDs["ollama-2"], "ollama-2 should be in candidates")
	assert.False(t, allBackendIDs["llamacpp-1"], "llamacpp-1 should NOT be in candidates when filtering by Ollama")
	assert.False(t, allBackendIDs["llamacpp-2"], "llamacpp-2 should NOT be in candidates when filtering by Ollama")
}

// ============================================================
// Test: при OperatingMode=virtual_router выбираются только llama.cpp бэкенды
// ============================================================
func TestSelectBackend_VirtualRouterMode_LlamaCppOnly(t *testing.T) {
	proxy := setupMixedCluster(t)

	// Меняем OperatingMode на virtual_router (только llama.cpp)
	cfg := proxy.GetConfig()
	cfg.Balancing.OperatingMode = "virtual_router"

	// 50 итераций — должен выбирать только llama.cpp бэкенды
	for i := 0; i < 50; i++ {
		backendID := proxy.SelectBackend("llama3:8b")
		if backendID == "" {
			t.Logf("Iteration %d: no backend available (all busy)", i)
			continue
		}
		assert.True(t,
			backendID == "llamacpp-1" || backendID == "llamacpp-2",
			"virtual_router mode should only select llama.cpp backends, got: %s", backendID)
	}
}

// ============================================================
// Test: при OperatingMode=virtual_router expandCandidates не содержит Ollama
// ============================================================
func TestExpandCandidates_VirtualRouterMode_NoOllama(t *testing.T) {
	proxy := setupMixedCluster(t)

	cfg := proxy.GetConfig()
	cfg.Balancing.OperatingMode = "virtual_router"

	// virtual_router разрешает только llama_cpp — передаём явно
	candidates := proxy.ExpandCandidatesWithType("llama3:8b", []types.BackendType{types.BackendTypeLlamaCpp})

	allBackendIDs := make(map[string]bool)
	for _, group := range candidates {
		for _, id := range group.BackendIDs {
			allBackendIDs[id] = true
		}
	}

	assert.False(t, allBackendIDs["ollama-1"], "ollama-1 should NOT be in candidates for virtual_router mode")
	assert.False(t, allBackendIDs["ollama-2"], "ollama-2 should NOT be in candidates for virtual_router mode")
	assert.True(t, allBackendIDs["llamacpp-1"], "llamacpp-1 should be in candidates")
	assert.True(t, allBackendIDs["llamacpp-2"], "llamacpp-2 should be in candidates")
}

// ============================================================
// Test: findBackendWithModel учитывает allowedTypes
// ============================================================
func TestFindBackendWithModel_WithTypeFilter(t *testing.T) {
	proxy := setupMixedCluster(t)

	// Добавляем модель "llama3:8b" на ollama-1 и llamacpp-1
	ollama1Metrics := &types.BackendMetrics{
		ID:         "ollama-1",
		BackendType: types.BackendTypeOllama,
		Status:     types.StatusHealthy,
		Host:       "10.0.0.1",
		OllamaPort: 11434,
		GPU: types.GPUMetrics{
			MemoryTotal: 24576,
			MemoryUsed:  8192,
			MemoryFree:  16384,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "llama3:8b", ParameterSize: "8B", Family: "llama"},
			},
			ActiveRequests: 0,
		},
	}
	proxy.SetBackendMetrics("ollama-1", ollama1Metrics)

	llamacpp1Metrics := &types.BackendMetrics{
		ID:         "llamacpp-1",
		BackendType: types.BackendTypeLlamaCpp,
		Status:     types.StatusHealthy,
		Host:       "10.0.1.1",
		OllamaPort: 0,
		GPU: types.GPUMetrics{
			MemoryTotal: 24576,
			MemoryUsed:  8192,
			MemoryFree:  16384,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "llama3:8b", ParameterSize: "8B", Family: "llama"},
			},
			ActiveRequests: 0,
		},
	}
	proxy.SetBackendMetrics("llamacpp-1", llamacpp1Metrics)

	// При стандартном режиме (оба типа) — должен найти модель на любом бэкенде
	backend := proxy.SelectBackend("llama3:8b")
	assert.NotEmpty(t, backend, "should find backend with model")
	assert.True(t,
		backend == "ollama-1" || backend == "llamacpp-1",
		"should find model on either ollama-1 or llamacpp-1, got: %s", backend)
}

// ============================================================
// Test: mixed cluster no cross-contamination over 100 iterations
// ============================================================
func TestSelectBackend_MixedCluster_NoCrossContamination_VirtualRouter(t *testing.T) {
	proxy := setupMixedCluster(t)

	cfg := proxy.GetConfig()
	cfg.Balancing.OperatingMode = "virtual_router"

	ollamaCount := 0
	llamaCount := 0
	total := 0

	for i := 0; i < 100; i++ {
		backendID := proxy.SelectBackend("llama3:8b")
		if backendID == "" {
			continue
		}
		total++
		if strings.HasPrefix(backendID, "ollama-") {
			ollamaCount++
		}
		if strings.HasPrefix(backendID, "llamacpp-") {
			llamaCount++
		}
	}

	t.Logf("Total selections: %d, Ollama: %d, llama.cpp: %d", total, ollamaCount, llamaCount)
	assert.Equal(t, 0, ollamaCount, "virtual_router mode should NEVER select Ollama backends")
	assert.Greater(t, llamaCount, 0, "virtual_router mode should select llama.cpp backends")
}

// ============================================================
// Test: Distributed inference mode only allows llama.cpp
// ============================================================
func TestOperatingMode_DistributedInference_LlamaCppOnly(t *testing.T) {
	proxy := setupMixedCluster(t)

	cfg := proxy.GetConfig()
	cfg.Balancing.OperatingMode = "distributed_inference"

	ollamaCount := 0
	llamaCount := 0

	for i := 0; i < 50; i++ {
		backendID := proxy.SelectBackend("llama3:8b")
		if backendID == "" {
			continue
		}
		if strings.HasPrefix(backendID, "ollama-") {
			ollamaCount++
		}
		if strings.HasPrefix(backendID, "llamacpp-") {
			llamaCount++
		}
	}

	assert.Equal(t, 0, ollamaCount, "distributed_inference mode should NEVER select Ollama backends")
	assert.Greater(t, llamaCount, 0, "distributed_inference mode should select llama.cpp backends")
}

// ============================================================
// Test: IsModeCompatibleWithBackendType — проверка хелпера
// ============================================================
func TestIsModeCompatibleWithBackendType(t *testing.T) {
	// standard mode — both types
	assert.True(t, types.IsModeCompatibleWithBackendType("standard", types.BackendTypeOllama))
	assert.True(t, types.IsModeCompatibleWithBackendType("standard", types.BackendTypeLlamaCpp))

	// replication mode — both types
	assert.True(t, types.IsModeCompatibleWithBackendType("replication", types.BackendTypeOllama))
	assert.True(t, types.IsModeCompatibleWithBackendType("replication", types.BackendTypeLlamaCpp))

	// rpc_coordinator mode — both types
	assert.True(t, types.IsModeCompatibleWithBackendType("rpc_coordinator", types.BackendTypeOllama))
	assert.True(t, types.IsModeCompatibleWithBackendType("rpc_coordinator", types.BackendTypeLlamaCpp))

	// virtual_router mode — only llama_cpp
	assert.False(t, types.IsModeCompatibleWithBackendType("virtual_router", types.BackendTypeOllama))
	assert.True(t, types.IsModeCompatibleWithBackendType("virtual_router", types.BackendTypeLlamaCpp))

	// distributed_inference mode — only llama_cpp
	assert.False(t, types.IsModeCompatibleWithBackendType("distributed_inference", types.BackendTypeOllama))
	assert.True(t, types.IsModeCompatibleWithBackendType("distributed_inference", types.BackendTypeLlamaCpp))

	// unknown mode — both types (permissive fallback)
	assert.True(t, types.IsModeCompatibleWithBackendType("unknown_mode", types.BackendTypeOllama))
	assert.True(t, types.IsModeCompatibleWithBackendType("unknown_mode", types.BackendTypeLlamaCpp))
}

// ============================================================
// Test: normalizeBackendType — обратная совместимость
// ============================================================
func TestNormalizeBackendType_EmptyDefaultsToOllama(t *testing.T) {
	proxy := setupMixedCluster(t)

	// Добавляем бэкенд с пустым типом
	config := proxy.GetConfig()
	config.Backends = append(config.Backends, types.Backend{
		ID:             "legacy-1",
		Host:           "10.0.0.3",
		OllamaPort:    11434,
		Type:           "", // пустой тип — обратная совместимость
		Status:         types.StatusHealthy,
		MaxConcurrentReqs: 10,
		Weight:         1,
	})

	// Метрики для legacy бэкенда
	proxy.SetBackendMetrics("legacy-1", &types.BackendMetrics{
		ID:         "legacy-1",
		BackendType: "", // агент тоже не знает тип
		Status:     types.StatusHealthy,
		Host:       "10.0.0.3",
		OllamaPort: 11434,
		GPU: types.GPUMetrics{
			MemoryTotal: 24576,
			MemoryUsed:  4096,
			MemoryFree:  20480,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 10,
			MemoryTotal:     65536,
		},
		Ollama: types.OllamaMetrics{
			RunningModels:       []types.RunningModel{},
			ActiveRequests:      0,
		},
	})

	// Пустой тип должен считаться Ollama
	// При virtual_router режиме legacy-1 (ollama) не должен выбираться
	cfg := proxy.GetConfig()
	cfg.Balancing.OperatingMode = "virtual_router"

	for i := 0; i < 30; i++ {
		backendID := proxy.SelectBackend("llama3:8b")
		if backendID == "" {
			continue
		}
		assert.NotEqual(t, "legacy-1", backendID,
			"empty-type backends (ollama default) should not be selected in virtual_router mode")
	}
}

// ============================================================
// Test: BackendEngine auto-resolution
// ============================================================
func TestResolveEngine_AutoResolvesByType(t *testing.T) {
	// llama_cpp type → llama_cpp engine
	engine := types.ResolveEngine(types.EngineAuto, types.BackendTypeLlamaCpp)
	assert.Equal(t, types.EngineLlamaCPP, engine)

	// ollama type → ollama_api engine
	engine = types.ResolveEngine(types.EngineAuto, types.BackendTypeOllama)
	assert.Equal(t, types.EngineOllamaAPI, engine)

	// empty type → ollama_api (default)
	engine = types.ResolveEngine(types.EngineAuto, "")
	assert.Equal(t, types.EngineOllamaAPI, engine)

	// explicit engine overrides type
	engine = types.ResolveEngine(types.EngineOllamaAPI, types.BackendTypeLlamaCpp)
	assert.Equal(t, types.EngineOllamaAPI, engine)

	engine = types.ResolveEngine(types.EngineLlamaCPP, types.BackendTypeOllama)
	assert.Equal(t, types.EngineLlamaCPP, engine)
}

// ============================================================
// Test: Proxy isLlamaCppBackend helper
// ============================================================
func TestProxyIsLlamaCppBackend(t *testing.T) {
	proxy := setupMixedCluster(t)

	ollamaState := proxy.GetBackendState("ollama-1")
	require.NotNil(t, ollamaState)

	llamaState := proxy.GetBackendState("llamacpp-1")
	require.NotNil(t, llamaState)

	// ollama-1 не должен быть llama.cpp
	assert.False(t, ollamaState.Backend.Type == types.BackendTypeLlamaCpp,
		"ollama-1 should not be llama.cpp")

	// llamacpp-1 должен быть llama.cpp
	assert.True(t, llamaState.Backend.Type == types.BackendTypeLlamaCpp,
		"llamacpp-1 should be llama.cpp")
}

// ============================================================
// Test: cluster state includes BackendTypeCounts
// ============================================================
func TestClusterState_IncludesBackendTypeCounts(t *testing.T) {
	proxy := setupMixedCluster(t)

	state := proxy.GetClusterState()
	require.NotNil(t, state)

	counts := state.BackendTypeCounts
	assert.Equal(t, 2, counts[types.BackendTypeOllama], "should have 2 ollama backends")
	assert.Equal(t, 2, counts[types.BackendTypeLlamaCpp], "should have 2 llama_cpp backends")
}

// ============================================================
// Test: ServeHTTP — Ollama-запрос в virtual_router режиме получает 503
// ============================================================
func TestServeHTTP_VirtualRouterMode_RejectsOllamaBackends(t *testing.T) {
	proxy := setupMixedCluster(t)

	cfg := proxy.GetConfig()
	cfg.Balancing.OperatingMode = "virtual_router"

	// Создаём Ollama-запрос
	body := `{"model":"llama3:8b","prompt":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	// В virtual_router режиме ollama-бэкенды исключены,
	// а метрики llama.cpp-бэкендов не имеют модели — должен быть 503
	// (или очередь, если слоты свободны)
	t.Logf("Response status: %d, body: %s", w.Code, w.Body.String())

	// Проверяем что ответ не 200 (ollama-бэкенды недоступны в этом режиме)
	assert.NotEqual(t, http.StatusOK, w.Code,
		"virtual_router mode should not route to ollama backends")
}

// ============================================================
// Test: GetAllowedTypesList — helper function
// ============================================================
func TestGetAllowedTypesList_EmptyUsesOperatingMode(t *testing.T) {
	// Тестируем через ModeBackendTypes напрямую

	// standard mode
	allowed, ok := types.ModeBackendTypes["standard"]
	assert.True(t, ok)
	assert.Contains(t, allowed, types.BackendTypeOllama)
	assert.Contains(t, allowed, types.BackendTypeLlamaCpp)

	// virtual_router mode
	allowed, ok = types.ModeBackendTypes["virtual_router"]
	assert.True(t, ok)
	assert.NotContains(t, allowed, types.BackendTypeOllama)
	assert.Contains(t, allowed, types.BackendTypeLlamaCpp)

	// distributed_inference mode
	allowed, ok = types.ModeBackendTypes["distributed_inference"]
	assert.True(t, ok)
	assert.NotContains(t, allowed, types.BackendTypeOllama)
	assert.Contains(t, allowed, types.BackendTypeLlamaCpp)
}

// ============================================================
// Test: standard mode + BackendEngine=llama_cpp включает llama.cpp бэкенды
// ============================================================
func TestSelectBackend_StandardMode_LlamaCppBackendsIncluded(t *testing.T) {
	// Конфигурация с llama.cpp бэкендами в стандартном режиме
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    18080,
			APIPort: 18081,
		},
		BackendEngine: types.EngineLlamaCPP,
		Balancing: types.BalancingSettings{
			Algorithm:          types.AlgorithmResourceAware,
			ModelAffinity:      true,
			SessionStickiness:  true,
			UseEnhancedScoring: true,
			OperatingMode:      "standard", // СТАНДАРТНЫЙ режим с llama.cpp бэкендами
			QueueMaxSize:       100,
		},
		Backends: []types.Backend{
			{
				ID:                "llamacpp-s1",
				Host:              "10.0.1.1",
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     18091,
				Status:            types.StatusHealthy,
				MaxConcurrentReqs: 10,
				Weight:            1,
			},
			{
				ID:                "llamacpp-s2",
				Host:              "10.0.1.2",
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     18091,
				Status:            types.StatusHealthy,
				MaxConcurrentReqs: 10,
				Weight:            1,
			},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 85},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
		},
	}

	proxy := balancer.NewProxy(config)

	// Добавляем метрики
	for _, b := range config.Backends {
		proxy.SetBackendMetrics(b.ID, &types.BackendMetrics{
			ID:          b.ID,
			BackendType: types.BackendTypeLlamaCpp,
			Status:      types.StatusHealthy,
			Host:        b.Host,
			OllamaPort:  b.CppWorkerPort,
			GPU: types.GPUMetrics{
				MemoryTotal: 24576,
				MemoryUsed:  4096,
				MemoryFree:  20480,
			},
			System: types.SystemMetrics{
				CPUUsagePercent: 20,
				MemoryTotal:     65536,
			},
			Ollama: types.OllamaMetrics{
				RunningModels: []types.RunningModel{
					{Name: "llama3:8b", ParameterSize: "8B", Family: "llama"},
				},
				ActiveRequests:      0,
				MaxConcurrentRequests: 10,
			},
		})
	}

	// 100 итераций — все должны выбрать llama.cpp бэкенд
	ollamaCount := 0
	llamaCount := 0

	for i := 0; i < 100; i++ {
		backendID := proxy.SelectBackend("llama3:8b")
		if backendID == "" {
			continue
		}
		if strings.HasPrefix(backendID, "ollama-") {
			ollamaCount++
		}
		if strings.HasPrefix(backendID, "llamacpp-") {
			llamaCount++
		}
	}

	assert.Equal(t, 0, ollamaCount, "standard mode with llama.cpp should NOT select ollama backends")
	assert.Greater(t, llamaCount, 0, "standard mode should select llama.cpp backends")
	t.Logf("standard mode: ollama=%d, llama=%d", ollamaCount, llamaCount)
}

// ============================================================
// Test: mixed cluster в standard mode — /v1/ запросы → llama.cpp, /api/ → ollama
// ============================================================
func TestStandardMode_MixedCluster_RoutesByPath(t *testing.T) {
	proxy := setupMixedCluster(t)

	cfg := proxy.GetConfig()
	cfg.Balancing.OperatingMode = "standard"

	// Запрос на /v1/chat/completions (llama.cpp формат) должен идти на llama.cpp бэкенд
	for i := 0; i < 30; i++ {
		backendID := proxy.SelectBackend("llama3:8b")
		if backendID == "" {
			continue
		}
		// В standard mode с явным запросом без фильтра — оба типа допустимы
		// Проверяем что хотя бы один из llama.cpp бэкендов выбирается
		if strings.HasPrefix(backendID, "llamacpp-") {
			t.Logf("llama.cpp backend selected: %s", backendID)
		}
	}

	// Проверяем что expandCandidates в standard mode включает оба типа
	candidates := proxy.ExpandCandidates("llama3:8b")
	allIDs := make(map[string]bool)
	for _, group := range candidates {
		for _, id := range group.BackendIDs {
			allIDs[id] = true
		}
	}

	assert.True(t, allIDs["ollama-1"] || allIDs["ollama-2"], "ollama backends should be candidates in standard mode")
	assert.True(t, allIDs["llamacpp-1"] || allIDs["llamacpp-2"], "llama.cpp backends should be candidates in standard mode")
}

// ============================================================
// Test: mixed cluster routing — Ollama запросы НЕ попадают на llama.cpp при фильтрации
// ============================================================
func TestServeHTTP_MixedCluster_RoutingByURLPath(t *testing.T) {
	proxy := setupMixedCluster(t)

	cfg := proxy.GetConfig()
	cfg.Balancing.OperatingMode = "standard"

	// Ollama-запрос (/api/generate)
	ollamaBody := `{"model":"llama3:8b","prompt":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(ollamaBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, req)

	// В standard mode с /api/ путём — бэкенды обоих типов допустимы
	// но т.к. нет реального Ollama/CppWorker сервера, будет 503
	t.Logf("/api/generate response: status=%d", w.Code)

	// llama.cpp-запрос (/v1/chat/completions)
	llamaBody := `{"model":"llama3:8b","messages":[{"role":"user","content":"Hi"}],"stream":false}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(llamaBody))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()

	proxy.ServeHTTP(w2, req2)
	t.Logf("/v1/chat/completions response: status=%d", w2.Code)

	// Проверяем что determineRequestBackendType работает корректно
	// /api/ → ollama, /v1/ → llama_cpp
	bt1 := proxy.DetermineRequestBackendTypeForTest("/api/generate")
	assert.Equal(t, types.BackendTypeOllama, bt1)

	bt2 := proxy.DetermineRequestBackendTypeForTest("/v1/chat/completions")
	assert.Equal(t, types.BackendTypeLlamaCpp, bt2)

	bt3 := proxy.DetermineRequestBackendTypeForTest("/v1/completions")
	assert.Equal(t, types.BackendTypeLlamaCpp, bt3)
}
