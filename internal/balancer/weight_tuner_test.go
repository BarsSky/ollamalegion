package balancer

import (
	"math"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func TestNewAdaptiveWeightTuner(t *testing.T) {
	p := &Proxy{
		config: &types.LoadBalancerConfig{},
	}
	awt := NewAdaptiveWeightTuner(p)
	if awt == nil {
		t.Fatal("NewAdaptiveWeightTuner returned nil")
	}
	if !awt.enabled {
		t.Error("expected enabled by default")
	}
	if awt.windowSize != 100 {
		t.Errorf("windowSize = %d, want 100", awt.windowSize)
	}
	if awt.weights.GPUFree != 0.30 {
		t.Errorf("GPUFree weight = %v, want 0.30", awt.weights.GPUFree)
	}
	if awt.baseline.GPUFree != 0.30 {
		t.Error("baseline should match initial weights")
	}
}

func TestRecordOutcome(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	awt := NewAdaptiveWeightTuner(p)
	awt.enabled = true

	awt.RecordOutcome("b1", "m1", 100, true)
	awt.RecordOutcome("b1", "m1", 200, false)
	awt.RecordOutcome("b2", "m2", 150, true)

	awt.mu.Lock()
	if len(awt.history) != 3 {
		t.Errorf("history len = %d, want 3", len(awt.history))
	}
	awt.mu.Unlock()

	// Disabled should not record
	awt.SetEnabled(false)
	awt.RecordOutcome("b3", "m3", 50, true)
	awt.mu.Lock()
	if len(awt.history) != 3 {
		t.Errorf("disabled should not add history, got %d", len(awt.history))
	}
	awt.mu.Unlock()
}

func TestTune_InsufficientHistory(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	awt := NewAdaptiveWeightTuner(p)
	awt.enabled = true

	// Add only a few samples
	for i := 0; i < 5; i++ {
		awt.RecordOutcome("b1", "m1", 100, true)
	}

	weightsBefore := awt.GetWeights()
	awt.tune()
	weightsAfter := awt.GetWeights()

	if weightsAfter != weightsBefore {
		t.Error("tune should not change weights with insufficient history")
	}
}

func TestTune_LowSuccessRate(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	awt := NewAdaptiveWeightTuner(p)
	awt.enabled = true
	awt.windowSize = 10

	// 90% success rate (below 0.95 threshold)
	for i := 0; i < 9; i++ {
		awt.RecordOutcome("b1", "m1", 200, true)
	}
	awt.RecordOutcome("b1", "m1", 500, false)

	weightsBefore := awt.GetWeights()
	awt.tune()
	weightsAfter := awt.GetWeights()

	if weightsAfter.ErrorRate <= weightsBefore.ErrorRate {
		t.Error("ErrorRate weight should increase with low success rate")
	}
	if weightsAfter.QueueDepth <= weightsBefore.QueueDepth {
		t.Error("QueueDepth weight should increase with low success rate")
	}
}

func TestTune_HighSuccessRate(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	awt := NewAdaptiveWeightTuner(p)
	awt.enabled = true
	awt.windowSize = 10

	// 100% success rate + low latency (below 1000ms)
	for i := 0; i < 10; i++ {
		awt.RecordOutcome("b1", "m1", 100, true)
	}

	// Move weights away from baseline
	awt.mu.Lock()
	awt.weights.ErrorRate = 0.20
	awt.weights.QueueDepth = 0.15
	awt.mu.Unlock()

	awt.tune()
	weightsAfter := awt.GetWeights()

	// Should approach baseline (ErrorRate baseline = 0.05)
	if weightsAfter.ErrorRate >= 0.20 {
		t.Error("ErrorRate should decrease toward baseline with high success rate")
	}
}

func TestApproach(t *testing.T) {
	tests := []struct {
		current float64
		target  float64
		step    float64
		want    float64
	}{
		{0.10, 0.05, 0.02, 0.08},  // decrease
		{0.05, 0.10, 0.02, 0.07},  // increase
		{0.06, 0.05, 0.02, 0.05},  // within step → target
		{0.05, 0.05, 0.02, 0.05},  // already at target
	}

	for _, tt := range tests {
		got := approach(tt.current, tt.target, tt.step)
		if math.Abs(got-tt.want) > 0.0001 {
			t.Errorf("approach(%v, %v, %v) = %v, want %v", tt.current, tt.target, tt.step, got, tt.want)
		}
	}
}

func TestApproachBaseline(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	awt := NewAdaptiveWeightTuner(p)

	// Move weights away from baseline
	awt.mu.Lock()
	awt.weights.ErrorRate = 0.20
	awt.weights.QueueDepth = 0.15
	awt.weights.GPUFree = 0.25
	awt.mu.Unlock()

	newWeights := awt.approachBaseline(0.02)

	if newWeights.ErrorRate >= 0.20 {
		t.Error("ErrorRate should move toward baseline")
	}
	if newWeights.QueueDepth >= 0.15 {
		t.Error("QueueDepth should move toward baseline")
	}
	if newWeights.GPUFree <= 0.25 {
		t.Error("GPUFree should move toward baseline (increase)")
	}
}

func TestGetWeights(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	awt := NewAdaptiveWeightTuner(p)

	w := awt.GetWeights()
	if w.GPUFree != 0.30 {
		t.Errorf("GetWeights GPUFree = %v, want 0.30", w.GPUFree)
	}
}

func TestReset(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	awt := NewAdaptiveWeightTuner(p)

	// Modify weights and history
	awt.mu.Lock()
	awt.weights.ErrorRate = 0.20
	awt.history = []TuningSample{{BackendID: "b1"}}
	awt.mu.Unlock()

	awt.Reset()

	w := awt.GetWeights()
	if w.ErrorRate != awt.baseline.ErrorRate {
		t.Error("Reset should restore baseline weights")
	}
	awt.mu.Lock()
	if len(awt.history) != 0 {
		t.Error("Reset should clear history")
	}
	awt.mu.Unlock()
}

func TestWeightTunerStartStop(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	awt := NewAdaptiveWeightTuner(p)
	awt.adjustInterval = 50 * time.Millisecond

	awt.Start()
	select {
	case <-time.After(100 * time.Millisecond):
		// ok
	}

	awt.Stop()
	awt.Stop() // double stop should be safe
}

func TestWeightTunerSetEnabled(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	awt := NewAdaptiveWeightTuner(p)
	awt.adjustInterval = 50 * time.Millisecond

	// Already enabled
	awt.SetEnabled(true)
	if !awt.enabled {
		t.Error("should be enabled")
	}

	// Disable
	awt.SetEnabled(false)
	if awt.enabled {
		t.Error("should be disabled")
	}
	awt.Stop()

	// Re-enable
	awt.SetEnabled(true)
	if !awt.enabled {
		t.Error("should be re-enabled")
	}
	awt.Stop()
}

func TestRecordOutcome_WindowTrimming(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	awt := NewAdaptiveWeightTuner(p)
	awt.enabled = true
	awt.windowSize = 5

	// Add exactly windowSize*2 + 1 samples to trigger trimming
	for i := 0; i < 11; i++ {
		awt.RecordOutcome("b1", "m1", int64(i*10), true)
	}

	awt.mu.Lock()
	historyLen := len(awt.history)
	awt.mu.Unlock()

	// After trimming, history should be at most windowSize
	if historyLen > awt.windowSize {
		t.Errorf("history should be trimmed to <= windowSize (%d), got %d", awt.windowSize, historyLen)
	}
	// And at least some history should remain
	if historyLen == 0 {
		t.Error("history should not be empty after recording")
	}
}
