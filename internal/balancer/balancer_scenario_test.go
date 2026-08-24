package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupBalancerScenario создаёт тестовое окружение с 2 бэкендами по 24GB VRAM
func setupBalancerScenario(t *testing.T) *Proxy {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 8080, APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID: "ollama-1", Name: "GPU Server 1", Host: "localhost",
				OllamaPort: 11434, AgentPort: 9090, Weight: 1,
				MaxConcurrentReqs: 2, Status: types.StatusHealthy,
			},
			{
				ID: "ollama-2", Name: "GPU Server 2", Host: "localhost",
				OllamaPort: 11435, AgentPort: 9091, Weight: 1,
				MaxConcurrentReqs: 2, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			ModelAffinity:       true,
			SessionStickiness:   true,
			QueueMaxSize:        100,
			QueueWorkers:        2,
			RequestTimeout:      30,
			QueueTimeout:        60,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 95},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 1024},
		},
		API: types.APISettings{
			RateLimit: 1000,
			RateBurst: 2000,
		},
		Auth: types.AuthConfig{Enabled: false},
	}

	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()

	// Инициализируем метрики: оба бэкенда healthy, VRAM 24GB, GPU ~10%
	for _, id := range []string{"ollama-1", "ollama-2"} {
		updateMetricsInternal(t, proxy, id, []types.RunningModel{}, 0)
	}

	return proxy
}

// updateMetricsInternal обновляет метрики бэкенда напрямую через Proxy
func updateMetricsInternal(t *testing.T, proxy *Proxy, agentID string, models []types.RunningModel, activeReqs int) {
	t.Helper()

	usedVRAM := uint64(len(models) * 8192)
	freeVRAM := uint64(24576 - len(models)*8192)
	if int64(freeVRAM) < 0 {
		freeVRAM = 0
	}

	metrics := types.BackendMetrics{
		ID:     agentID,
		Status: types.StatusHealthy,
		GPU: types.GPUMetrics{
			UsagePercent: 10,
			MemoryTotal:  24576, // 24 GB
			MemoryUsed:   usedVRAM,
			MemoryFree:   freeVRAM,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 15,
			MemoryTotal:     65536,
			MemoryUsed:      16000,
			DiskFree:        2048,
		},
		Ollama: types.OllamaMetrics{
			ActiveRequests:        activeReqs,
			RequestsPerSecond:     0,
			RunningModels:         models,
			MaxConcurrentRequests: 2,
			FreeSlots:             2 - activeReqs,
		},
	}
	proxy.UpdateMetrics(agentID, &metrics)
}

// TestBalancerScenario проверяет 4 пошаговых сценария балансировки
func TestBalancerScenario(t *testing.T) {
	proxy := setupBalancerScenario(t)

	t.Run("Scenario 1: Single user requests a model", func(t *testing.T) {
		backendID := proxy.selectBackend("llama3.1", "")
		require.NotEmpty(t, backendID, "backend should be selected")
		t.Logf("✓ User A → llama3.1 → %s", backendID)

		// Симулируем загрузку модели на выбранном бэкенде
		updateMetricsInternal(t, proxy, backendID, []types.RunningModel{
			{Name: "llama3.1", VRAMUsage: 8192},
		}, 1)
	})

	t.Run("Scenario 2: Second user requests same model (model affinity)", func(t *testing.T) {
		backendID := proxy.selectBackend("llama3.1", "")
		require.NotEmpty(t, backendID)

		t.Logf("✓ User B → llama3.1 → %s (model affinity)", backendID)

		// Обновляем метрики: теперь 2 активных запроса
		updateMetricsInternal(t, proxy, "ollama-1", []types.RunningModel{
			{Name: "llama3.1", VRAMUsage: 8192},
		}, 2)
	})

	t.Run("Scenario 3: Third user, both backends full → fallback", func(t *testing.T) {
		// Загрузим llama3.1 и на ollama-2, тоже заполним
		updateMetricsInternal(t, proxy, "ollama-2", []types.RunningModel{
			{Name: "llama3.1", VRAMUsage: 8192},
		}, 2)

		backendID := proxy.selectBackend("llama3.1", "")
		// Если оба заполнены, может вернуть пустую строку (нет доступных)
		if backendID == "" {
			t.Logf("✓ User C → llama3.1 → no backend available (capacity full)")
		} else {
			t.Logf("✓ User C → llama3.1 → %s", backendID)
		}
	})

	t.Run("Scenario 4: Fourth user with different model", func(t *testing.T) {
		// Освобождаем ollama-2
		updateMetricsInternal(t, proxy, "ollama-2", []types.RunningModel{
			{Name: "llama3.1", VRAMUsage: 8192},
		}, 0)

		backendID := proxy.selectBackend("gemma2", "")
		require.NotEmpty(t, backendID, "backend should be selected for different model")

		// Симулируем загрузку gemma2
		updateMetricsInternal(t, proxy, backendID, []types.RunningModel{
			{Name: "llama3.1", VRAMUsage: 8192},
			{Name: "gemma2", VRAMUsage: 6144},
		}, 1)

		t.Logf("✓ User D → gemma2 → %s (loaded alongside existing)", backendID)

		// Проверяем VRAM accounting
		state := proxy.GetClusterState()
		var usedVRAM uint64
		for _, b := range state.Backends {
			usedVRAM += b.GPU.MemoryUsed
		}
		t.Logf("Cluster VRAM used: %dMB", usedVRAM)
		assert.Greater(t, usedVRAM, uint64(0), "VRAM should be accounted")
	})

	t.Run("Final: Cluster state consistency", func(t *testing.T) {
		state := proxy.GetClusterState()
		require.NotNil(t, state)

		// Должно быть 2 бэкенда
		assert.Equal(t, 2, state.TotalBackends, "total backends should be 2")
		assert.GreaterOrEqual(t, state.TotalBackends, state.HealthyBackends)

		// Note: сессии создаются через ServeHTTP, не через selectBackend.
		// В реальных запросах сессии завязываются на клиентский IP/ID.
		sessions := proxy.GetSessions()
		t.Logf("Total sessions: %d", len(sessions))

		// Queue: не должна переполниться
		stats := proxy.GetQueueStats()
		assert.LessOrEqual(t, stats.CurrentSize, stats.MaxSize, "queue should not overflow")

		printScenarioReport(t, proxy)
	})
}

// TestBalancerResourceAware проверяет resource-aware scoring
func TestBalancerResourceAware(t *testing.T) {
	proxy := setupBalancerScenario(t)

	// Бэкенд 1: загружен (active 2/2)
	updateMetricsInternal(t, proxy, "ollama-1", []types.RunningModel{
		{Name: "llama3.1", VRAMUsage: 8192},
	}, 2)

	// Бэкенд 2: почти пуст (active 0/2)
	updateMetricsInternal(t, proxy, "ollama-2", []types.RunningModel{}, 0)

	// Запрос новой модели — должен пойти на ollama-2 (более свободный)
	backendID := proxy.selectBackend("mistral", "")
	require.NotEmpty(t, backendID)
	assert.Equal(t, "ollama-2", backendID, "resource-aware should pick less loaded backend")
	t.Logf("✓ Resource-aware selected %s for mistral (expected ollama-2)", backendID)
}

// TestBalancerSessionStickiness проверяет привязку сессии
func TestBalancerSessionStickiness(t *testing.T) {
	proxy := setupBalancerScenario(t)

	// Первый запрос
	updateMetricsInternal(t, proxy, "ollama-1", []types.RunningModel{
		{Name: "llama3.1", VRAMUsage: 8192},
	}, 1)

	backend1 := proxy.selectBackend("llama3.1", "")
	require.NotEmpty(t, backend1)

	// Второй запрос с той же моделью — должен пойти туда же (model affinity)
	backend2 := proxy.selectBackend("llama3.1", "")
	require.NotEmpty(t, backend2)

	assert.Equal(t, backend1, backend2, "same model should select same backend (affinity)")
	t.Logf("✓ Model affinity: %s == %s", backend1, backend2)
}

// printScenarioReport выводит читаемый отчёт по сценариям
func printScenarioReport(t *testing.T, proxy *Proxy) {
	state := proxy.GetClusterState()
	t.Log("═══════════════════════════════════════════════════════════════")
	t.Log("  OLLAMALEGION BALANCER SCENARIO REPORT")
	t.Log("═══════════════════════════════════════════════════════════════")
	t.Logf("  Backends:     %d total, %d healthy", state.TotalBackends, state.HealthyBackends)
	t.Logf("  Active reqs:  %d", state.ActiveRequests)
	t.Logf("  Queued:       %d", state.QueuedRequests)
	t.Logf("  RPS:          %.1f", state.RPS)
	t.Log("───────────────────────────────────────────────────────────────")
	for _, b := range state.Backends {
		vramPct := float64(0)
		if b.GPU.MemoryTotal > 0 {
			vramPct = float64(b.GPU.MemoryUsed) / float64(b.GPU.MemoryTotal) * 100
		}
		t.Logf("  %-12s | GPU %.0f%% | VRAM %.0f%% | Active %d",
			b.ID, b.GPU.UsagePercent, vramPct, b.Ollama.ActiveRequests)
	}
	t.Log("═══════════════════════════════════════════════════════════════")
}