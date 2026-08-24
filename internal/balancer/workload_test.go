// workload_test.go — Round 54.9 (2026-08-24): tests for WorkloadTracker.

package balancer

import (
	"testing"
)

func TestWorkloadTracker_RecordAndStats(t *testing.T) {
	tr := NewWorkloadTracker(DefaultWorkloadTrackerConfig())

	// Record 5 samples
	for i, n := range []int{100, 200, 300, 400, 500} {
		tr.Record("b1", "m1", n)
		if tr.Stats("b1", "m1").SampleCount != i+1 {
			t.Errorf("after %d records, SampleCount should be %d", i+1, i+1)
		}
	}

	stats := tr.Stats("b1", "m1")
	if stats.SampleCount != 5 {
		t.Errorf("expected 5 samples, got %d", stats.SampleCount)
	}
	if stats.AvgNumCtx != 300 { // (100+200+300+400+500)/5
		t.Errorf("expected avg=300, got %d", stats.AvgNumCtx)
	}
	if stats.P95NumCtx != 500 { // 95% of 5 = 4.75 → index 4 = 500
		t.Errorf("expected p95=500, got %d", stats.P95NumCtx)
	}
	if stats.MaxNumCtx != 500 {
		t.Errorf("expected max=500, got %d", stats.MaxNumCtx)
	}
}

func TestWorkloadTracker_IgnoreZeroAndNegative(t *testing.T) {
	tr := NewWorkloadTracker(DefaultWorkloadTrackerConfig())
	tr.Record("b1", "m1", 0)  // ignored
	tr.Record("b1", "m1", -1) // ignored
	tr.Record("b1", "m1", 100)
	stats := tr.Stats("b1", "m1")
	if stats.SampleCount != 1 {
		t.Errorf("expected 1 sample (zero/negative ignored), got %d", stats.SampleCount)
	}
}

func TestWorkloadTracker_StatsEmptyReturnsZero(t *testing.T) {
	tr := NewWorkloadTracker(DefaultWorkloadTrackerConfig())
	stats := tr.Stats("b1", "m1")
	if stats.SampleCount != 0 || stats.AvgNumCtx != 0 || stats.P95NumCtx != 0 {
		t.Errorf("empty stats should be zero, got %+v", stats)
	}
}

func TestWorkloadTracker_NilSafe(t *testing.T) {
	var tr *WorkloadTracker
	tr.Record("b1", "m1", 100) // should not panic
	stats := tr.Stats("b1", "m1")
	if stats.SampleCount != 0 {
		t.Error("nil tracker should return zero stats")
	}
	tr.Snapshot() // should not panic
}

func TestWorkloadTracker_SlidingWindow(t *testing.T) {
	cfg := WorkloadTrackerConfig{MaxSamples: 3, MinSamplesForRecommendation: 5}
	tr := NewWorkloadTracker(cfg)
	tr.Record("b1", "m1", 1)
	tr.Record("b1", "m1", 2)
	tr.Record("b1", "m1", 3)
	tr.Record("b1", "m1", 4) // evicts sample=1
	tr.Record("b1", "m1", 5) // evicts sample=2

	stats := tr.Stats("b1", "m1")
	if stats.SampleCount != 3 {
		t.Errorf("expected 3 samples after window eviction, got %d", stats.SampleCount)
	}
	if stats.MinNumCtx != 3 { // [3, 4, 5]
		t.Errorf("expected min=3, got %d", stats.MinNumCtx)
	}
}

func TestWorkloadStats_IsLight(t *testing.T) {
	tests := []struct {
		name     string
		stats    WorkloadStats
		loaded   int
		min      int
		expected bool
	}{
		{
			name:     "light: p95=1000, loaded=8000, 25% = 2000 → 1000 < 2000 = light",
			stats:    WorkloadStats{SampleCount: 10, P95NumCtx: 1000},
			loaded:   8000,
			min:      5,
			expected: true,
		},
		{
			name:     "medium: p95=2000, loaded=8000, 25% = 2000 → 2000 < 2000 = false (not light)",
			stats:    WorkloadStats{SampleCount: 10, P95NumCtx: 2000},
			loaded:   8000,
			min:      5,
			expected: false,
		},
		{
			name:     "no samples: SampleCount=0 → false",
			stats:    WorkloadStats{SampleCount: 0, P95NumCtx: 100},
			loaded:   8000,
			min:      5,
			expected: false,
		},
		{
			name:     "below minSamples: SampleCount=3 < 5 → false",
			stats:    WorkloadStats{SampleCount: 3, P95NumCtx: 100},
			loaded:   8000,
			min:      5,
			expected: false,
		},
		{
			name:     "no loaded n_ctx: loaded=0 → false",
			stats:    WorkloadStats{SampleCount: 10, P95NumCtx: 100},
			loaded:   0,
			min:      5,
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stats.IsLight(tc.loaded, tc.min); got != tc.expected {
				t.Errorf("IsLight: expected %v, got %v", tc.expected, got)
			}
		})
	}
}

func TestWorkloadStats_IsHeavy(t *testing.T) {
	tests := []struct {
		name     string
		stats    WorkloadStats
		loaded   int
		min      int
		expected bool
	}{
		{
			name:     "heavy: p95=7000, loaded=8000, 75% = 6000 → 7000 > 6000 = heavy",
			stats:    WorkloadStats{SampleCount: 10, P95NumCtx: 7000},
			loaded:   8000,
			min:      5,
			expected: true,
		},
		{
			name:     "not heavy: p95=5000, loaded=8000, 75% = 6000 → 5000 not > 6000",
			stats:    WorkloadStats{SampleCount: 10, P95NumCtx: 5000},
			loaded:   8000,
			min:      5,
			expected: false,
		},
		{
			name:     "no samples: false",
			stats:    WorkloadStats{SampleCount: 0, P95NumCtx: 7000},
			loaded:   8000,
			min:      5,
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stats.IsHeavy(tc.loaded, tc.min); got != tc.expected {
				t.Errorf("IsHeavy: expected %v, got %v", tc.expected, got)
			}
		})
	}
}

func TestComputeOptimalKVCacheTypeWithWorkload(t *testing.T) {
	p := &ModelProfileInfo{
		Name:                 "Qwen3-4B",
		SizeBytes:            2_500_000_000, // 2.5GB Q4
		NLayers:              36,
		NEmbd:                2560,
		NKvHeads:             8,
		HeadDimK:             80,
		CurrentContextLength: 65536,
		CurrentKVCacheType:   "q4_0",
		FreeVRAMBytes:        6_000_000_000, // 6GB free
		TotalVRAMBytes:       8_000_000_000, // RTX 3070
	}

	t.Run("workload light: p95=4096, loaded=65536 → f16", func(t *testing.T) {
		workload := WorkloadStats{SampleCount: 10, P95NumCtx: 4096}
		rec, reasoning := computeOptimalKVCacheTypeWithWorkload(p, workload, 5)
		if rec != "f16" {
			t.Errorf("expected f16, got %s (reasoning: %s)", rec, reasoning)
		}
	})

	t.Run("workload heavy: p95=60000, loaded=65536 → q4_0", func(t *testing.T) {
		workload := WorkloadStats{SampleCount: 10, P95NumCtx: 60000}
		rec, reasoning := computeOptimalKVCacheTypeWithWorkload(p, workload, 5)
		if rec != "q4_0" {
			t.Errorf("expected q4_0, got %s (reasoning: %s)", rec, reasoning)
		}
	})

	t.Run("workload medium: p95=20000, loaded=65536 → R54.1 logic (f16 since model<3.5GB)", func(t *testing.T) {
		workload := WorkloadStats{SampleCount: 10, P95NumCtx: 20000}
		rec, _ := computeOptimalKVCacheTypeWithWorkload(p, workload, 5)
		// 25% of 65536 = 16384 → 20000 > 16384 → not light
		// 75% of 65536 = 49152 → 20000 not > 49152 → not heavy
		// → fallback to R54.1: model=2.5GB ≤ 3.5GB → f16
		if rec != "f16" {
			t.Errorf("expected f16 from R54.1 fallback, got %s", rec)
		}
	})

	t.Run("insufficient samples → R54.1 logic", func(t *testing.T) {
		workload := WorkloadStats{SampleCount: 2, P95NumCtx: 4096} // would be "light" but too few samples
		rec, _ := computeOptimalKVCacheTypeWithWorkload(p, workload, 5)
		if rec != "f16" {
			// R54.1: small model + freeVRAM → f16, same result
			t.Errorf("expected R54.1 fallback (f16 for small model), got %s", rec)
		}
	})
}

// TestProxy_WorkloadTracker_AutoInit — verifies lazy init works.
func TestProxy_WorkloadTracker_AutoInit(t *testing.T) {
	p := &Proxy{}
	tr := p.WorkloadTracker()
	if tr == nil {
		t.Fatal("WorkloadTracker() should auto-initialize")
	}
	// Second call returns same instance
	if p.WorkloadTracker() != tr {
		t.Error("WorkloadTracker() should return same instance on second call")
	}
}

func TestProxy_WorkloadTracker_NilProxy(t *testing.T) {
	var p *Proxy
	if p.WorkloadTracker() != nil {
		t.Error("nil proxy should return nil WorkloadTracker")
	}
}
