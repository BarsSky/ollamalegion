package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestExpandCandidates(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b-loaded": {
				Backend: &types.Backend{ID: "b-loaded", MaxConcurrentReqs: 10, Status: types.StatusHealthy},
			},
			"b-free": {
				Backend: &types.Backend{ID: "b-free", MaxConcurrentReqs: 10, Status: types.StatusHealthy},
			},
			"b-unhealthy": {
				Backend: &types.Backend{ID: "b-unhealthy", MaxConcurrentReqs: 10, Status: types.StatusUnhealthy},
			},
		},
		metricsMgr: NewMetricsManager(),
		config: &types.LoadBalancerConfig{Balancing: types.BalancingSettings{
			Prewarm: types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		}},
	}

	// b-loaded has the model running, low load
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b-loaded"] = &types.BackendMetrics{
		ID:     "b-loaded",
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 10},
		Ollama: types.OllamaMetrics{
			MaxConcurrentRequests: 10, ActiveRequests: 1,
			RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}},
		},
	}
	p.metricsMgr.metrics["b-free"] = &types.BackendMetrics{
		ID:     "b-free",
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 10},
		Ollama: types.OllamaMetrics{
			MaxConcurrentRequests: 10, ActiveRequests: 1,
			RunningModels: []types.RunningModel{},
		},
	}
	p.metricsMgr.mu.Unlock()

	candidates := p.expandCandidates("llama3.1:8b")
	if len(candidates) < 2 {
		t.Fatalf("expected at least 2 candidate groups, got %d", len(candidates))
	}

	// P1 should contain b-loaded
	if candidates[0].Priority != 1 {
		t.Errorf("first group priority = %d, want 1", candidates[0].Priority)
	}
	foundLoaded := false
	for _, id := range candidates[0].BackendIDs {
		if id == "b-loaded" {
			foundLoaded = true
			break
		}
	}
	if !foundLoaded {
		t.Errorf("P1 should contain b-loaded, got %v", candidates[0].BackendIDs)
	}

	// Unhealthy backend should not appear
	for _, group := range candidates {
		for _, id := range group.BackendIDs {
			if id == "b-unhealthy" {
				t.Errorf("unhealthy backend should not be in candidates")
			}
		}
	}
}

func TestFindBackendWithModelSelector(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend: &types.Backend{ID: "b1", MaxConcurrentReqs: 10, Weight: 100, Status: types.StatusHealthy},
			},
			"b2": {
				Backend: &types.Backend{ID: "b2", MaxConcurrentReqs: 10, Weight: 50, Status: types.StatusHealthy},
			},
		},
		metricsMgr: NewMetricsManager(),
		config: &types.LoadBalancerConfig{Balancing: types.BalancingSettings{
			Prewarm: types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		}},
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID:     "b1",
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 10},
		Ollama: types.OllamaMetrics{
			MaxConcurrentRequests: 10, ActiveRequests: 1,
			RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}},
		},
	}
	p.metricsMgr.metrics["b2"] = &types.BackendMetrics{
		ID:     "b2",
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 10},
		Ollama: types.OllamaMetrics{
			MaxConcurrentRequests: 10, ActiveRequests: 1,
			RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}},
		},
	}
	p.metricsMgr.mu.Unlock()

	got := p.findBackendWithModel("llama3.1:8b")
	if got != "b1" && got != "b2" {
		t.Errorf("findBackendWithModel() = %s, want b1 or b2", got)
	}

	// Model not present
	got2 := p.findBackendWithModel("nonexistent")
	if got2 != "" {
		t.Errorf("findBackendWithModel(nonexistent) = %s, want empty", got2)
	}
}

func TestSelectByResources(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend: &types.Backend{ID: "b1", MaxConcurrentReqs: 10, Weight: 100, Status: types.StatusHealthy},
			},
			"b2": {
				Backend: &types.Backend{ID: "b2", MaxConcurrentReqs: 10, Weight: 50, Status: types.StatusHealthy},
			},
		},
		metricsMgr: NewMetricsManager(),
		config: &types.LoadBalancerConfig{Balancing: types.BalancingSettings{
			UseEnhancedScoring: false,
			Prewarm:            types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		}},
		queueMgr: NewQueueManager(nil, 10, 1, 30),
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID:     "b1",
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 10},
		Ollama: types.OllamaMetrics{MaxConcurrentRequests: 10, ActiveRequests: 2},
	}
	p.metricsMgr.metrics["b2"] = &types.BackendMetrics{
		ID:     "b2",
		GPU:    types.GPUMetrics{UsagePercent: 80, MemoryTotal: 24576, MemoryUsed: 20000, MemoryFree: 4576},
		System: types.SystemMetrics{CPUUsagePercent: 70},
		Ollama: types.OllamaMetrics{MaxConcurrentRequests: 10, ActiveRequests: 8},
	}
	p.metricsMgr.mu.Unlock()

	got := p.selectByResources()
	if got != "b1" {
		t.Errorf("selectByResources() = %s, want b1 (less loaded)", got)
	}

	// All backends saturated
	p.backends["b1"].mu.Lock()
	p.backends["b1"].ActiveReqs = 10
	p.backends["b1"].mu.Unlock()
	p.backends["b2"].mu.Lock()
	p.backends["b2"].ActiveReqs = 10
	p.backends["b2"].mu.Unlock()

	got2 := p.selectByResources()
	if got2 != "" {
		t.Errorf("selectByResources() with full load = %s, want empty", got2)
	}
}

func TestFindLessLoadedBackendWithModel(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend: &types.Backend{ID: "b1", MaxConcurrentReqs: 10, Status: types.StatusHealthy},
			},
			"b2": {
				Backend: &types.Backend{ID: "b2", MaxConcurrentReqs: 10, Status: types.StatusHealthy},
			},
		},
		metricsMgr: NewMetricsManager(),
		config: &types.LoadBalancerConfig{},
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID:     "b1",
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}}},
	}
	p.metricsMgr.metrics["b2"] = &types.BackendMetrics{
		ID:     "b2",
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}}},
	}
	p.metricsMgr.mu.Unlock()

	p.backends["b1"].mu.Lock()
	p.backends["b1"].ActiveReqs = 8
	p.backends["b1"].mu.Unlock()
	p.backends["b2"].mu.Lock()
	p.backends["b2"].ActiveReqs = 2
	p.backends["b2"].mu.Unlock()

	got := p.findLessLoadedBackendWithModel("llama3.1:8b", "")
	if got != "b2" {
		t.Errorf("findLessLoadedBackendWithModel() = %s, want b2", got)
	}

	// Exclude b2
	got2 := p.findLessLoadedBackendWithModel("llama3.1:8b", "b2")
	if got2 != "b1" {
		t.Errorf("findLessLoadedBackendWithModel(exclude b2) = %s, want b1", got2)
	}
}

func TestFindLessLoadedBackendAny(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend: &types.Backend{ID: "b1", MaxConcurrentReqs: 10, Weight: 100, Status: types.StatusHealthy},
			},
			"b2": {
				Backend: &types.Backend{ID: "b2", MaxConcurrentReqs: 10, Weight: 50, Status: types.StatusHealthy},
			},
		},
		metricsMgr: NewMetricsManager(),
		config: &types.LoadBalancerConfig{Balancing: types.BalancingSettings{
			UseEnhancedScoring: false,
		}},
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID:     "b1",
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 10},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "existing-model"}}},
	}
	p.metricsMgr.metrics["b2"] = &types.BackendMetrics{
		ID:     "b2",
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 10},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
	}
	p.metricsMgr.mu.Unlock()

	p.backends["b1"].mu.Lock()
	p.backends["b1"].ActiveReqs = 8
	p.backends["b1"].mu.Unlock()
	p.backends["b2"].mu.Lock()
	p.backends["b2"].ActiveReqs = 2
	p.backends["b2"].mu.Unlock()

	got := p.findLessLoadedBackendAny("new-model", "")
	if got != "b2" {
		t.Errorf("findLessLoadedBackendAny() = %s, want b2", got)
	}

	// Same load ratio — use score tie-breaker
	p.backends["b1"].mu.Lock()
	p.backends["b1"].ActiveReqs = 2
	p.backends["b1"].mu.Unlock()

	got2 := p.findLessLoadedBackendAny("new-model", "")
	if got2 == "" {
		t.Errorf("findLessLoadedBackendAny() with tie = empty, want non-empty")
	}
}

func TestModelIsRunningOnBackendUnsafe(t *testing.T) {
	p := &Proxy{
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID:     "b1",
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}}},
	}
	p.metricsMgr.mu.Unlock()

	if !p.modelIsRunningOnBackendUnsafe("b1", "llama3.1:8b") {
		t.Error("model should be running on b1")
	}
	if p.modelIsRunningOnBackendUnsafe("b1", "missing") {
		t.Error("missing model should not be running")
	}
	if p.modelIsRunningOnBackendUnsafe("missing", "llama3.1:8b") {
		t.Error("missing backend should return false")
	}
	if p.modelIsRunningOnBackendUnsafe("b1", "") {
		t.Error("empty model should return false")
	}
}

func TestCheckModelReadyUnsafe(t *testing.T) {
	p := &Proxy{
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID:     "b1",
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}}},
	}
	p.metricsMgr.mu.Unlock()

	if !p.checkModelReadyUnsafe("b1", "llama3.1:8b") {
		t.Error("model should be ready")
	}
	if p.checkModelReadyUnsafe("b1", "other") {
		t.Error("other model should not be ready")
	}
}

func TestSelectBackendExcluding(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend: &types.Backend{ID: "b1", MaxConcurrentReqs: 10, Weight: 100, Status: types.StatusHealthy},
			},
			"b2": {
				Backend: &types.Backend{ID: "b2", MaxConcurrentReqs: 10, Weight: 50, Status: types.StatusHealthy},
			},
		},
		metricsMgr: NewMetricsManager(),
		config: &types.LoadBalancerConfig{Balancing: types.BalancingSettings{
			ModelAffinity: true,
			UseEnhancedScoring: false,
			Prewarm:       types.PrewarmConfig{TriggerLoadThreshold: 0.80},
		}},
		queueMgr: NewQueueManager(nil, 10, 1, 30),
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID:     "b1",
		Ollama: types.OllamaMetrics{
			MaxConcurrentRequests: 10, ActiveRequests: 1,
			RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}},
		},
	}
	p.metricsMgr.metrics["b2"] = &types.BackendMetrics{
		ID:     "b2",
		Ollama: types.OllamaMetrics{
			MaxConcurrentRequests: 10, ActiveRequests: 1,
			RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}},
		},
	}
	p.metricsMgr.mu.Unlock()

	// Both backends excluded
	got := p.selectBackendExcluding("llama3.1:8b", map[string]bool{"b1": true, "b2": true})
	if got != "" {
		t.Errorf("selectBackendExcluding(all excluded) = %s, want empty", got)
	}

	// b1 excluded — should pick b2
	got2 := p.selectBackendExcluding("llama3.1:8b", map[string]bool{"b1": true})
	if got2 != "b2" {
		t.Errorf("selectBackendExcluding(exclude b1) = %s, want b2", got2)
	}
}

func TestSelectFreeBackendAny(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend: &types.Backend{ID: "b1", MaxConcurrentReqs: 10, Status: types.StatusHealthy},
			},
			"b2": {
				Backend: &types.Backend{ID: "b2", MaxConcurrentReqs: 10, Status: types.StatusHealthy},
			},
		},
		metricsMgr: NewMetricsManager(),
		config: &types.LoadBalancerConfig{},
		queueMgr:   NewQueueManager(nil, 10, 1, 30),
	}

	p.backends["b1"].mu.Lock()
	p.backends["b1"].ActiveReqs = 5
	p.backends["b1"].mu.Unlock()
	p.backends["b2"].mu.Lock()
	p.backends["b2"].ActiveReqs = 2
	p.backends["b2"].mu.Unlock()

	got := p.selectFreeBackendAny()
	if got != "b2" {
		t.Errorf("selectFreeBackendAny() = %s, want b2", got)
	}

	// All saturated
	p.backends["b1"].mu.Lock()
	p.backends["b1"].ActiveReqs = 10
	p.backends["b1"].mu.Unlock()
	p.backends["b2"].mu.Lock()
	p.backends["b2"].ActiveReqs = 10
	p.backends["b2"].mu.Unlock()

	got2 := p.selectFreeBackendAny()
	if got2 != "" {
		t.Errorf("selectFreeBackendAny(saturated) = %s, want empty", got2)
	}
}