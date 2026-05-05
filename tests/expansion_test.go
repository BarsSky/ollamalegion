package tests

import (
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// mockProxy создаёт минимальный Proxy для тестирования expandCandidates и dispatchWithModelLoad.
// Доступ через экспортируемую тестовую обёртку TestableProxy (см. ниже).
// Тесты используют публичное API через создание Proxy с подготовленными бэкендами и метриками.

func TestExpandCandidates_EmptyBackends(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18081, StatePath: "testdata/state.json"},
		Backends:     []types.Backend{},
		Balancing: types.BalancingSettings{
			ModelAffinity: true,
			Prewarm:       types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		},
	}
	proxy := balancer.NewProxy(cfg)

	groups := proxy.ExpandCandidates("llama3.1")
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for empty backends, got %d", len(groups))
	}
}

func TestExpandCandidates_ModelLoaded(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18081, StatePath: "testdata/state.json"},
		Backends: []types.Backend{
			{ID: "b1", Host: "10.0.0.1", OllamaPort: 11434, MaxConcurrentReqs: 8, Status: types.StatusHealthy, Weight: 1},
			{ID: "b2", Host: "10.0.0.2", OllamaPort: 11434, MaxConcurrentReqs: 8, Status: types.StatusHealthy, Weight: 1},
		},
		Balancing: types.BalancingSettings{
			ModelAffinity: true,
			Prewarm:       types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		},
	}
	proxy := balancer.NewProxy(cfg)

	// Инжектируем метрики: на b1 модель llama3.1 загружена, b2 — пустой
	proxy.UpdateMetrics("b1", &types.BackendMetrics{
		ID: "b1", Timestamp: time.Now(),
		GPU: types.GPUMetrics{MemoryTotal: 24576, MemoryFree: 16384, MemoryUsed: 8192},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:latest", Size: 8589934592, VRAMUsage: 8192},
			},
		},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryFree: 32768},
	})
	proxy.UpdateMetrics("b2", &types.BackendMetrics{
		ID: "b2", Timestamp: time.Now(),
		GPU: types.GPUMetrics{MemoryTotal: 24576, MemoryFree: 24576, MemoryUsed: 0},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryFree: 50000},
	})

	proxy.SetBackendMetrics("b1", &types.BackendMetrics{
		ID: "b1", Timestamp: time.Now(),
		GPU: types.GPUMetrics{MemoryTotal: 24576, MemoryFree: 16384, MemoryUsed: 8192},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:latest", Size: 8589934592, VRAMUsage: 8192},
			},
		},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryFree: 32768},
	})
	proxy.SetBackendMetrics("b2", &types.BackendMetrics{
		ID: "b2", Timestamp: time.Now(),
		GPU: types.GPUMetrics{MemoryTotal: 24576, MemoryFree: 24576, MemoryUsed: 0},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryFree: 50000},
	})

	groups := proxy.ExpandCandidates("llama3.1:latest")

	// Должна быть минимум P1 (loaded) и P4 (fallback)
	if len(groups) < 1 {
		t.Fatalf("expected at least P1 group, got %d", len(groups))
	}
	if groups[0].Priority != 1 {
		t.Errorf("expected P1 (LOADED) priority=1, got %d", groups[0].Priority)
	}
	if len(groups[0].BackendIDs) != 1 {
		t.Errorf("expected 1 backend in P1, got %d", len(groups[0].BackendIDs))
	}
	if groups[0].BackendIDs[0] != "b1" {
		t.Errorf("expected b1 in P1, got %s", groups[0].BackendIDs[0])
	}
}

func TestExpandCandidates_WarmingModel(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18081, StatePath: "testdata/state.json"},
		Backends: []types.Backend{
			{ID: "b1", Host: "10.0.0.1", OllamaPort: 11434, MaxConcurrentReqs: 8, Status: types.StatusHealthy, Weight: 1},
		},
		Balancing: types.BalancingSettings{
			ModelAffinity: true,
			Prewarm:       types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		},
	}
	proxy := balancer.NewProxy(cfg)

	proxy.UpdateMetrics("b1", &types.BackendMetrics{
		ID: "b1", Timestamp: time.Now(),
		GPU: types.GPUMetrics{MemoryTotal: 24576, MemoryFree: 24576, MemoryUsed: 0},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryFree: 32768},
	})
	proxy.SetBackendMetrics("b1", &types.BackendMetrics{
		ID: "b1", Timestamp: time.Now(),
		GPU: types.GPUMetrics{MemoryTotal: 24576, MemoryFree: 24576, MemoryUsed: 0},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryFree: 32768},
	})

	// Устанавливаем warming модель
	proxy.SetWarmingUpModel("b1", "gemma2:latest", time.Now().Add(60*time.Second))

	groups := proxy.ExpandCandidates("gemma2:latest")

	hasP2 := false
	for _, g := range groups {
		if g.Priority == 2 {
			hasP2 = true
			if len(g.BackendIDs) != 1 || g.BackendIDs[0] != "b1" {
				t.Errorf("expected b1 in P2, got %v", g.BackendIDs)
			}
		}
	}
	if !hasP2 {
		t.Error("expected P2 (WARMING) group for gemma2:latest")
	}
}

func TestExpandCandidates_FreeBackend(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18081, StatePath: "testdata/state.json"},
		Backends: []types.Backend{
			{ID: "b1", Host: "10.0.0.1", OllamaPort: 11434, MaxConcurrentReqs: 8, Status: types.StatusHealthy, Weight: 1},
		},
		Balancing: types.BalancingSettings{
			ModelAffinity: true,
			Prewarm:       types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		},
	}
	proxy := balancer.NewProxy(cfg)

	proxy.UpdateMetrics("b1", &types.BackendMetrics{
		ID: "b1", Timestamp: time.Now(),
		GPU: types.GPUMetrics{MemoryTotal: 24576, MemoryFree: 20000, MemoryUsed: 4576},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryFree: 32768},
	})
	proxy.SetBackendMetrics("b1", &types.BackendMetrics{
		ID: "b1", Timestamp: time.Now(),
		GPU: types.GPUMetrics{MemoryTotal: 24576, MemoryFree: 20000, MemoryUsed: 4576},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryFree: 32768},
	})

	groups := proxy.ExpandCandidates("llama3.1:latest")

	hasP3 := false
	for _, g := range groups {
		if g.Priority == 3 {
			hasP3 = true
			if len(g.BackendIDs) != 1 || g.BackendIDs[0] != "b1" {
				t.Errorf("expected b1 in P3 (FREE), got %v", g.BackendIDs)
			}
		}
	}
	if !hasP3 {
		t.Error("expected P3 (FREE) group for free backend")
	}
}

func TestExpandCandidates_UnhealthyExcluded(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18081, StatePath: "testdata/state.json"},
		Backends: []types.Backend{
			{ID: "b1", Host: "10.0.0.1", OllamaPort: 11434, MaxConcurrentReqs: 8, Status: types.StatusUnhealthy, Weight: 1},
			{ID: "b2", Host: "10.0.0.2", OllamaPort: 11434, MaxConcurrentReqs: 8, Status: types.StatusHealthy, Weight: 1},
		},
		Balancing: types.BalancingSettings{
			ModelAffinity: true,
			Prewarm:       types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		},
	}
	proxy := balancer.NewProxy(cfg)

	proxy.UpdateMetrics("b2", &types.BackendMetrics{
		ID: "b2", Timestamp: time.Now(),
		GPU: types.GPUMetrics{MemoryTotal: 24576, MemoryFree: 20000, MemoryUsed: 4576},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryFree: 32768},
	})
	proxy.SetBackendMetrics("b2", &types.BackendMetrics{
		ID: "b2", Timestamp: time.Now(),
		GPU: types.GPUMetrics{MemoryTotal: 24576, MemoryFree: 20000, MemoryUsed: 4576},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, MemoryFree: 32768},
	})

	groups := proxy.ExpandCandidates("llama3.1:latest")

	// b1 unhealthy → не должен попасть ни в одну группу
	for _, g := range groups {
		for _, bid := range g.BackendIDs {
			if bid == "b1" {
				t.Errorf("unhealthy backend b1 should not be a candidate, found in P%d", g.Priority)
			}
		}
	}
	// b2 должен быть в P3 и P4
	if len(groups) < 2 {
		t.Errorf("expected P3+P4 groups for healthy b2, got %d groups", len(groups))
	}
}

func TestDispatchWithModelLoad_NoFreeBackend(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18081, StatePath: "testdata/state.json"},
		Backends:     []types.Backend{},
		Balancing: types.BalancingSettings{
			Prewarm:         types.PrewarmConfig{TriggerLoadThreshold: 0.80},
			ModelLoadTimeout: 120,
		},
	}
	proxy := balancer.NewProxy(cfg)

	backendID, deadline := proxy.DispatchWithModelLoad("llama3.1:latest")
	if backendID != "" {
		t.Errorf("expected empty backendID for empty backends, got %s", backendID)
	}
	if !deadline.IsZero() {
		t.Errorf("expected zero deadline for empty backends, got %v", deadline)
	}
}