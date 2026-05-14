package balancer

import (
	"math"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestComputeBaseScoreAndPenalty(t *testing.T) {
	tests := []struct {
		name          string
		metrics       *types.BackendMetrics
		wantBaseMin   float64
		wantBaseMax   float64
		wantPenalty   float64
	}{
		{
			name: "idle backend",
			metrics: &types.BackendMetrics{
				GPU:    types.GPUMetrics{UsagePercent: 5, MemoryTotal: 24576, MemoryUsed: 1000, MemoryFree: 23576},
				System: types.SystemMetrics{CPUUsagePercent: 10},
				Ollama: types.OllamaMetrics{MaxConcurrentRequests: 10, ActiveRequests: 0},
			},
			wantBaseMin: 55,
			wantBaseMax: 65,
			wantPenalty: 0,
		},
		{
			name: "busy backend",
			metrics: &types.BackendMetrics{
				GPU:    types.GPUMetrics{UsagePercent: 80, MemoryTotal: 24576, MemoryUsed: 20000, MemoryFree: 4576},
				System: types.SystemMetrics{CPUUsagePercent: 70},
				Ollama: types.OllamaMetrics{MaxConcurrentRequests: 10, ActiveRequests: 8},
			},
			wantBaseMin: 0,
			wantBaseMax: 40,
			wantPenalty: 12,
		},
		{
			name: "no GPU metrics (MemoryTotal=0)",
			metrics: &types.BackendMetrics{
				GPU:    types.GPUMetrics{UsagePercent: 50, MemoryTotal: 0, MemoryUsed: 0, MemoryFree: 0},
				System: types.SystemMetrics{CPUUsagePercent: 30},
				Ollama: types.OllamaMetrics{MaxConcurrentRequests: 0, ActiveRequests: 10},
			},
			wantBaseMin: 0,
			wantBaseMax: 80,
			wantPenalty: 15,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseScore, penalty := computeBaseScoreAndPenalty(tt.metrics)
			if baseScore < tt.wantBaseMin || baseScore > tt.wantBaseMax {
				t.Errorf("baseScore = %v, want in [%v, %v]", baseScore, tt.wantBaseMin, tt.wantBaseMax)
			}
			if math.Abs(penalty-tt.wantPenalty) > 0.1 {
				t.Errorf("penalty = %v, want %v", penalty, tt.wantPenalty)
			}
		})
	}
}

func TestComputeModelCapacityScore(t *testing.T) {
	tests := []struct {
		name      string
		metrics   *types.BackendMetrics
		wantScore float64
	}{
		{
			name: "plenty of capacity (1/5 models)",
			metrics: &types.BackendMetrics{
				Ollama: types.OllamaMetrics{MaxModels: 5, RunningModels: []types.RunningModel{{Name: "a"}}},
			},
			wantScore: 8.0,
		},
		{
			name: "full capacity",
			metrics: &types.BackendMetrics{
				Ollama: types.OllamaMetrics{MaxModels: 5, RunningModels: []types.RunningModel{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"}}},
			},
			wantScore: 0,
		},
		{
			name: "using loadableModelCount >= 5",
			metrics: &types.BackendMetrics{
				Ollama: types.OllamaMetrics{MaxModels: 0, BackendCapacity: types.BackendCapacity{LoadableModelCount: 5}},
			},
			wantScore: 10.0,
		},
		{
			name: "using loadableModelCount = 2",
			metrics: &types.BackendMetrics{
				Ollama: types.OllamaMetrics{MaxModels: 0, BackendCapacity: types.BackendCapacity{LoadableModelCount: 2}},
			},
			wantScore: 4.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeModelCapacityScore(tt.metrics)
			if math.Abs(got-tt.wantScore) > 0.1 {
				t.Errorf("computeModelCapacityScore() = %v, want %v", got, tt.wantScore)
			}
		})
	}
}

func TestComputeNoMetricsScore(t *testing.T) {
	state := &BackendState{
		Backend: &types.Backend{Weight: 100, MaxConcurrentReqs: 10},
	}
	state.mu.Lock()
	state.ActiveReqs = 2
	state.mu.Unlock()

	score := computeNoMetricsScore(state)
	if score <= 0 {
		t.Errorf("computeNoMetricsScore() = %v, want > 0", score)
	}

	// High active requests should reduce score
	state.mu.Lock()
	state.ActiveReqs = 10
	state.mu.Unlock()
	score2 := computeNoMetricsScore(state)
	if score2 >= score {
		t.Errorf("score with high load (%v) should be lower than with low load (%v)", score2, score)
	}
}

func TestComputeWeightFactor(t *testing.T) {
	tests := []struct {
		weight int
		want   float64
	}{
		{100, 100},
		{0, 1},
		{-10, 1},
	}

	for _, tt := range tests {
		state := &BackendState{Backend: &types.Backend{Weight: tt.weight}}
		got := computeWeightFactor(state)
		if got != tt.want {
			t.Errorf("computeWeightFactor(weight=%d) = %v, want %v", tt.weight, got, tt.want)
		}
	}
}

func TestDefaultIfZero(t *testing.T) {
	if defaultIfZero(0, 0.15) != 0.15 {
		t.Error("defaultIfZero(0, 0.15) should be 0.15")
	}
	if defaultIfZero(-1, 0.15) != 0.15 {
		t.Error("defaultIfZero(-1, 0.15) should be 0.15")
	}
	if defaultIfZero(0.10, 0.15) != 0.10 {
		t.Error("defaultIfZero(0.10, 0.15) should be 0.10")
	}
}

func TestCalculateScoreSimple(t *testing.T) {
	// Build a minimal proxy with one backend
	backends := map[string]*BackendState{
		"b1": {
			Backend: &types.Backend{ID: "b1", Weight: 100, MaxConcurrentReqs: 10},
		},
	}
	p := &Proxy{
		backends:    backends,
		metricsMgr:  NewMetricsManager(),
		config:      &types.LoadBalancerConfig{Balancing: types.BalancingSettings{UseEnhancedScoring: false}},
	}

	// Set metrics
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID:     "b1",
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 15},
		Ollama: types.OllamaMetrics{MaxConcurrentRequests: 10, ActiveRequests: 1},
	}
	p.metricsMgr.mu.Unlock()

	score := p.calculateScoreSimple("b1")
	if score <= 0 {
		t.Errorf("calculateScoreSimple() = %v, want > 0", score)
	}

	// Missing metrics should fallback to computeNoMetricsScore
	score2 := p.calculateScoreSimple("missing")
	if score2 != 0 {
		t.Errorf("calculateScoreSimple(missing) = %v, want 0", score2)
	}
}

func TestCalculateScoreEnhanced(t *testing.T) {
	backends := map[string]*BackendState{
		"b1": {
			Backend: &types.Backend{ID: "b1", Weight: 100, MaxConcurrentReqs: 10},
		},
	}
	p := &Proxy{
		backends:    backends,
		metricsMgr:  NewMetricsManager(),
		config:      &types.LoadBalancerConfig{Balancing: types.BalancingSettings{UseEnhancedScoring: true}},
		queueMgr:    NewQueueManager(nil, 10, 1, 30),
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID:     "b1",
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 15},
		Ollama: types.OllamaMetrics{MaxConcurrentRequests: 10, ActiveRequests: 1, RunningModels: []types.RunningModel{{Name: "m1"}}},
	}
	p.metricsMgr.mu.Unlock()

	score := p.calculateScore("b1")
	if score <= 0 {
		t.Errorf("calculateScore() = %v, want > 0", score)
	}

	// With UseEnhancedScoring=false should delegate to simple
	p.config.Balancing.UseEnhancedScoring = false
	score2 := p.calculateScore("b1")
	if score2 <= 0 {
		t.Errorf("calculateScore(simple) = %v, want > 0", score2)
	}
}