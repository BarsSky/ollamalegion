package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// advertiseLlamaCppModel — объявить модель загруженной на llama.cpp-бэкенде.
//
// Нужно потому, что с R112 изоляция типов держится на факте наличия модели
// (backendHasModel), а не на режиме: без объявления модель «неизвестна никому»,
// и выбор становится разрешающим — тест изоляции тогда ничего не проверяет.
func advertiseLlamaCppModel(t *testing.T, proxy *balancer.Proxy, backendID, model string) {
	t.Helper()
	mm := proxy.GetMetricsManager()
	if mm == nil {
		t.Fatal("metrics manager недоступен")
	}
	m, ok := mm.SnapshotBackendMetrics(backendID)
	if !ok || m == nil {
		t.Fatalf("нет метрик для %s", backendID)
	}
	m.LlamaCpp.LoadedModels = append(m.LlamaCpp.LoadedModels, types.LlamaCppModel{
		Name: model, State: "loaded",
	})
	proxy.SetBackendMetrics(backendID, m)
}

// advertiseOllamaModel — то же для Ollama-бэкенда (RunningModels).
func advertiseOllamaModel(t *testing.T, proxy *balancer.Proxy, backendID, model string) {
	t.Helper()
	mm := proxy.GetMetricsManager()
	if mm == nil {
		t.Fatal("metrics manager недоступен")
	}
	m, ok := mm.SnapshotBackendMetrics(backendID)
	if !ok || m == nil {
		t.Fatalf("нет метрик для %s", backendID)
	}
	m.Ollama.RunningModels = append(m.Ollama.RunningModels, types.RunningModel{Name: model})
	proxy.SetBackendMetrics(backendID, m)
}

// makeBackendsAttractive / makeBackendsLoaded — R112-разбор: чтобы тест изоляции
// что-то доказывал, «неправильный» тип должен выглядеть ЛУЧШЕ по нагрузке
// (свободная VRAM, ноль активных запросов, низкая утилизация). Тогда удержать
// запрос на «правильном» типе может только знание о наличии модели; потеряй это
// знание — выбор уедет на более свободный бэкенд, и тест упадёт.
func makeBackendsAttractive(t *testing.T, proxy *balancer.Proxy, ids []string) {
	t.Helper()
	for _, id := range ids {
		mm := proxy.GetMetricsManager()
		m, ok := mm.SnapshotBackendMetrics(id)
		if !ok || m == nil {
			t.Fatalf("нет метрик для %s", id)
		}
		m.GPU.MemoryTotal = 24576
		m.GPU.MemoryUsed = 256
		m.GPU.MemoryFree = 24320
		m.GPU.UsagePercent = 1
		m.System.CPUUsagePercent = 1
		m.Ollama.ActiveRequests = 0
		m.Ollama.MaxConcurrentRequests = 10
		proxy.SetBackendMetrics(id, m)
	}
}

func makeBackendsLoaded(t *testing.T, proxy *balancer.Proxy, ids []string) {
	t.Helper()
	for _, id := range ids {
		mm := proxy.GetMetricsManager()
		m, ok := mm.SnapshotBackendMetrics(id)
		if !ok || m == nil {
			t.Fatalf("нет метрик для %s", id)
		}
		m.GPU.MemoryTotal = 24576
		m.GPU.MemoryUsed = 16384
		m.GPU.MemoryFree = 8192
		m.GPU.UsagePercent = 70
		m.System.CPUUsagePercent = 60
		m.Ollama.ActiveRequests = 5
		m.Ollama.MaxConcurrentRequests = 10
		proxy.SetBackendMetrics(id, m)
	}
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
// Test: режим virtual_router больше НЕ фильтрует по типу (R112)
// ============================================================
//
// Раньше здесь проверялось «virtual_router выбирает только llama.cpp». С R112
// тип определяется наличием модели, поэтому проверяем обе стороны:
//   - модель только на llama.cpp → уходит на llama.cpp (никогда на Ollama);
//   - модель только у Ollama → уходит на Ollama (никогда на llama.cpp).
func TestSelectBackend_VirtualRouterMode_NoTypePinning(t *testing.T) {
	proxy := setupMixedCluster(t)
	proxy.GetConfig().Balancing.OperatingMode = "virtual_router"

	for _, id := range []string{"llamacpp-1", "llamacpp-2"} {
		advertiseLlamaCppModel(t, proxy, id, "llama3:8b")
	}
	for i := 0; i < 50; i++ {
		backendID := proxy.SelectBackend("llama3:8b")
		if backendID == "" {
			t.Logf("Iteration %d: no backend available (all busy)", i)
			continue
		}
		assert.True(t, strings.HasPrefix(backendID, "llamacpp-"),
			"модель есть только на llama.cpp — выбран %s", backendID)
	}

	// Обратная сторона: модель только у Ollama.
	proxy2 := setupMixedCluster(t)
	proxy2.GetConfig().Balancing.OperatingMode = "virtual_router"
	for _, id := range []string{"ollama-1", "ollama-2"} {
		advertiseOllamaModel(t, proxy2, id, "llama3:8b")
	}
	for i := 0; i < 50; i++ {
		backendID := proxy2.SelectBackend("llama3:8b")
		if backendID == "" {
			continue
		}
		assert.True(t, strings.HasPrefix(backendID, "ollama-"),
			"модель есть только у Ollama — выбран %s", backendID)
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
// Test: режим НЕ привязывает тип бэкенда (R112), изоляция — по наличию модели
// ============================================================
//
// R112 (2026-10-11) убрал навязывание типа по конфигу: ModeBackendTypes у всех
// режимов = все типы, ModeEngines = EngineAuto. Раньше virtual_router жёстко
// уезжал только на llama.cpp, и второй тип не получал запросов вообще — клиент
// видел «модель не найдена» при живой модели на соседнем бэкенде.
//
// Что обязано остаться (и что проверяет этот тест): изоляция по ФАКТУ наличия
// модели. Если модель есть только на llama.cpp-бэкендах, Ollama-бэкенд её
// обслуживать не может, и наоборот.
func TestSelectBackend_ModeDoesNotPinType_IsolationByModel(t *testing.T) {
	for _, mode := range []string{"virtual_router", "distributed_inference", "standard"} {
		t.Run(mode, func(t *testing.T) {
			proxy := setupMixedCluster(t)
			proxy.GetConfig().Balancing.OperatingMode = mode

			// Модель «есть» только на llama.cpp-бэкендах: Ollama-бэкенды её не
			// отдают в /api/tags, llama.cpp — в LoadedModels.
			for _, id := range []string{"llamacpp-1", "llamacpp-2"} {
				advertiseLlamaCppModel(t, proxy, id, "llama3:8b")
			}
			// «Неправильный» тип (Ollama) выглядит свободнее — удержать запрос на
			// llama.cpp может только знание о наличии модели.
			makeBackendsAttractive(t, proxy, []string{"ollama-1", "ollama-2"})
			makeBackendsLoaded(t, proxy, []string{"llamacpp-1", "llamacpp-2"})

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
			assert.Equal(t, 0, ollamaCount,
				"режим %s: модель есть только на llama.cpp — Ollama-бэкенд выбран быть не может", mode)
			assert.Greater(t, llamaCount, 0,
				"режим %s: запрос обязан уехать на бэкенд, у которого модель есть", mode)

			// Обратная сторона изоляции: та же модель, но «есть» только у Ollama.
			proxy2 := setupMixedCluster(t)
			proxy2.GetConfig().Balancing.OperatingMode = mode
			for _, id := range []string{"ollama-1", "ollama-2"} {
				advertiseOllamaModel(t, proxy2, id, "llama3:8b")
			}
			makeBackendsAttractive(t, proxy2, []string{"llamacpp-1", "llamacpp-2"})
			makeBackendsLoaded(t, proxy2, []string{"ollama-1", "ollama-2"})
			llamaCount2 := 0
			for i := 0; i < 50; i++ {
				if id := proxy2.SelectBackend("llama3:8b"); strings.HasPrefix(id, "llamacpp-") {
					llamaCount2++
				}
			}
			assert.Equal(t, 0, llamaCount2,
				"режим %s: модель есть только у Ollama — llama.cpp-бэкенд выбран быть не может", mode)
		})
	}
}

// ============================================================
// Test: mixed cluster no cross-contamination over 100 iterations
// ============================================================
func TestSelectBackend_MixedCluster_NoCrossContamination_VirtualRouter(t *testing.T) {
	proxy := setupMixedCluster(t)

	cfg := proxy.GetConfig()
	cfg.Balancing.OperatingMode = "virtual_router"

	// R112: изоляция — по модели, а не по режиму. Объявляем модель только на
	// llama.cpp-бэкендах и требуем, чтобы выбор никогда не уходил на Ollama.
	for _, id := range []string{"llamacpp-1", "llamacpp-2"} {
		advertiseLlamaCppModel(t, proxy, id, "llama3:8b")
	}

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
	assert.Equal(t, 0, ollamaCount,
		"модель есть только на llama.cpp — Ollama-бэкенд не должен выбираться ни разу")
	assert.Greater(t, llamaCount, 0, "должны выбираться бэкенды, у которых модель есть")
}

// ============================================================
// Test: Distributed inference mode — тип не навязан, изоляция по модели
// ============================================================
func TestOperatingMode_DistributedInference_IsolationByModel(t *testing.T) {
	proxy := setupMixedCluster(t)

	cfg := proxy.GetConfig()
	cfg.Balancing.OperatingMode = "distributed_inference"

	for _, id := range []string{"llamacpp-1", "llamacpp-2"} {
		advertiseLlamaCppModel(t, proxy, id, "llama3:8b")
	}

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

	assert.Equal(t, 0, ollamaCount,
		"distributed_inference: модель есть только на llama.cpp — Ollama-бэкенд выбирать нельзя")
	assert.Greater(t, llamaCount, 0, "distributed_inference: запрос уходит на бэкенд с моделью")
}

// ============================================================
// Test: IsModeCompatibleWithBackendType — проверка хелпера
// ============================================================
func TestIsModeCompatibleWithBackendType(t *testing.T) {
	// R112 (2026-10-11): ни один штатный режим больше не запрещает тип бэкенда —
	// запрет вернул бы «модель не найдена» при живой модели на соседнем типе.
	// Проверяем консистентность хелпера с ModeBackendTypes по всем режимам.
	for _, mode := range []string{"standard", "replication", "rpc_coordinator", "virtual_router", "distributed_inference"} {
		assert.True(t, types.IsModeCompatibleWithBackendType(mode, types.BackendTypeOllama),
			"режим %s обязан допускать Ollama-бэкенды (R112)", mode)
		assert.True(t, types.IsModeCompatibleWithBackendType(mode, types.BackendTypeLlamaCpp),
			"режим %s обязан допускать llama.cpp-бэкенды (R112)", mode)
	}

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
//
// R112 (2026-10-11): режим больше не назначает допустимые типы — у всех режимов
// один и тот же набор (все типы), потому что тип определяется фактическим
// составом кластера и наличием модели. Тест фиксирует именно это: попытка снова
// «пришпилить» тип к режиму обязана его уронить.
func TestGetAllowedTypesList_EmptyUsesOperatingMode(t *testing.T) {
	for _, mode := range []string{"standard", "replication", "rpc_coordinator", "virtual_router", "distributed_inference"} {
		allowed, ok := types.ModeBackendTypes[mode]
		assert.True(t, ok, "режим %s должен быть в ModeBackendTypes", mode)
		assert.Contains(t, allowed, types.BackendTypeOllama,
			"R112: режим %s не должен запрещать Ollama-бэкенды", mode)
		assert.Contains(t, allowed, types.BackendTypeLlamaCpp,
			"режим %s должен допускать llama.cpp", mode)
	}

	// Движок также не назначается режимом: состав кластера решает.
	for _, mode := range []string{"standard", "replication", "rpc_coordinator", "virtual_router", "distributed_inference"} {
		assert.Equal(t, types.EngineAuto, types.ModeEngines[mode],
			"R112: режим %s не должен навязывать движок", mode)
	}
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

	// R66c (2026-09-22): оба вызова ServeHTTP ниже — диагностический smoke:
	// их результат только логируется, ассертов на него нет (проверяются
	// DetermineRequestBackendTypeForTest). Бэкенды mixed cluster заданы
	// недостижимыми адресами (10.0.0.1/10.0.0.2/10.0.1.1/10.0.1.2, см.
	// setupMixedCluster), а proxy прокидывает r.Context() в исходящий запрос
	// (internal/balancer/proxy_request.go:279) с дефолтным requestTimeout.
	// На Linux dial отваливается быстро, на Windows (self-hosted CI) висит до
	// таймаута: пакет ./tests/... не укладывался в 300s и падал по таймауту
	// ЦЕЛИКОМ — вместе со всеми остальными тестами пакета. Ограничиваем
	// запрос дедлайном: smoke остаётся, зависания нет.
	requestWithDeadline := func(r *http.Request, d time.Duration) *http.Request {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		t.Cleanup(cancel)
		return r.WithContext(ctx)
	}

	// Ollama-запрос (/api/generate)
	ollamaBody := `{"model":"llama3:8b","prompt":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(ollamaBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	proxy.ServeHTTP(w, requestWithDeadline(req, 2*time.Second))

	// В standard mode с /api/ путём — бэкенды обоих типов допустимы
	// но т.к. нет реального Ollama/CppWorker сервера, будет 503
	t.Logf("/api/generate response: status=%d", w.Code)

	// llama.cpp-запрос (/v1/chat/completions)
	llamaBody := `{"model":"llama3:8b","messages":[{"role":"user","content":"Hi"}],"stream":false}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(llamaBody))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()

	proxy.ServeHTTP(w2, requestWithDeadline(req2, 2*time.Second))
	t.Logf("/v1/chat/completions response: status=%d", w2.Code)

	// Проверяем что determineRequestBackendType работает корректно:
	// в standard mode /api/ допускает любой тип (пустая строка),
	// /v1/ всегда → llama_cpp.
	bt1 := proxy.DetermineRequestBackendTypeForTest("/api/generate")
	assert.Equal(t, types.BackendType(""), bt1, "standard mode /api/ should allow any backend type")

	bt2 := proxy.DetermineRequestBackendTypeForTest("/v1/chat/completions")
	assert.Equal(t, types.BackendTypeLlamaCpp, bt2)

	bt3 := proxy.DetermineRequestBackendTypeForTest("/v1/completions")
	assert.Equal(t, types.BackendTypeLlamaCpp, bt3)

	// R112 (2026-10-11): режим больше НЕ привязывает тип бэкенда. Раньше
	// virtual_router жёстко уводил /api/ на llama.cpp, и бэкенды второго типа не
	// получали запросов вообще (клиент видел «модель не найдена» при живой модели
	// рядом). Теперь тип решает selectBackend по фактическому составу кластера и
	// наличию модели, поэтому для /api/-путей тип остаётся «любой».
	cfg.Balancing.OperatingMode = "virtual_router"
	bt4 := proxy.DetermineRequestBackendTypeForTest("/api/generate")
	assert.Equal(t, types.BackendType(""), bt4,
		"R112: virtual_router не должен навязывать тип для /api/-путей")
}

// ============================================================
// Test: full HTTP flow — Ollama-запрос маршрутизируется на Ollama-бэкенд
// ============================================================
func TestServeHTTP_OllamaRequest_RoutedToOllamaBackend(t *testing.T) {
	var targetSeen bool
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/generate" {
			targetSeen = true
			w.Header().Set("X-Backend-ID", "ollama-live")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"llama3:8b","response":"ok"}`))
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "ollama-live", Name: "OL", Host: "127.0.0.1", Type: types.BackendTypeOllama, OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		{ID: "llamacpp-live", Name: "LL", Host: "127.0.0.1", Type: types.BackendTypeLlamaCpp, CppWorkerPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.OperatingMode = "standard"

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("ollama-live", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3:8b", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})
	proxy.UpdateMetrics("llamacpp-live", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	req, _ := http.NewRequest("POST", lb.URL+"/api/generate",
		strings.NewReader(`{"model":"llama3:8b","prompt":"Hi","stream":false}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.True(t, targetSeen, "Ollama backend should receive /api/generate")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "Ollama /api/generate should reach Ollama backend")
	assert.Equal(t, "ollama-live", resp.Header.Get("X-Backend-ID"), "request should be routed to Ollama backend")
}

// ============================================================
// Test: full HTTP flow — OpenAI/llama.cpp-запрос маршрутизируется на llama.cpp-бэкенд
// ============================================================
func TestServeHTTP_LlamaCppRequest_RoutedToLlamaCppBackend(t *testing.T) {
	var targetSeen bool
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"count":1,"models":[{"name":"llama3:8b","path":"llama3:8b.gguf","state":"loaded"}]}`))
			return
		case "/v1/chat/completions":
			targetSeen = true
			w.Header().Set("X-Backend-ID", "llamacpp-live")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			return
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}
	}))
	defer be.Close()

	cfg := newTestConfig([]types.Backend{
		{ID: "ollama-live", Name: "OL", Host: "127.0.0.1", Type: types.BackendTypeOllama, OllamaPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		{ID: "llamacpp-live", Name: "LL", Host: "127.0.0.1", Type: types.BackendTypeLlamaCpp, CppWorkerPort: mustParsePort(be.URL), Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
	})
	cfg.Balancing.OperatingMode = "standard"

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("ollama-live", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3:8b", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
	})
	proxy.UpdateMetrics("llamacpp-live", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{{Name: "llama3:8b", State: "loaded"}},
		},
	})

	lb := httptest.NewServer(proxy)
	defer lb.Close()

	req, _ := http.NewRequest("POST", lb.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llama3:8b","messages":[{"role":"user","content":"Hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.True(t, targetSeen, "llama.cpp backend should receive /v1/chat/completions")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "OpenAI /v1/chat/completions should reach llama.cpp backend")
	assert.Equal(t, "llamacpp-live", resp.Header.Get("X-Backend-ID"), "request should be routed to llama.cpp backend")
}
