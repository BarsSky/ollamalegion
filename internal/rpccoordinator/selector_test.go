// Package rpccoordinator — B6: Unit-тесты для Selector.

package rpccoordinator

import (
	"errors"
	"sync"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// WorkerLoadInfo.LoadRatio
// =====================================================================

func TestWorkerLoadInfo_LoadRatio(t *testing.T) {
	tests := []struct {
		name     string
		active   int
		capacity int
		want     float64
	}{
		{"empty worker", 0, 1, 0.0},
		{"half loaded", 5, 10, 0.5},
		{"fully loaded", 10, 10, 1.0},
		{"overloaded", 15, 10, 1.5},
		{"no capacity (1 active)", 1, 0, 1.0},
		{"no capacity (0 active)", 0, 0, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &WorkerLoadInfo{ActiveRequests: tt.active, Capacity: tt.capacity}
			got := w.LoadRatio()
			if got != tt.want {
				t.Errorf("LoadRatio: got %f, want %f", got, tt.want)
			}
		})
	}
}

// =====================================================================
// LeastLoadedSelector
// =====================================================================

func TestLeastLoadedSelector_EmptyCandidates(t *testing.T) {
	s := NewLeastLoadedSelector()
	_, err := s.Select(nil, nil)
	if !errors.Is(err, ErrNoCandidates) {
		t.Errorf("got err=%v, want ErrNoCandidates", err)
	}
}

func TestLeastLoadedSelector_AllUnknown(t *testing.T) {
	s := NewLeastLoadedSelector()
	// Нет loadInfo — все unknown → порядок сохраняется.
	candidates := []string{"a", "b", "c"}
	got, err := s.Select(candidates, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got)=%d, want 3", len(got))
	}
	// Порядок: original (т.к. unknown → нет reordering).
	for i, c := range candidates {
		if got[i] != c {
			t.Errorf("got[%d]=%s, want %s", i, got[i], c)
		}
	}
}

func TestLeastLoadedSelector_HealthyFirst(t *testing.T) {
	s := NewLeastLoadedSelector()
	loadInfo := map[string]WorkerLoadInfo{
		"healthy1": {WorkerID: "healthy1", ActiveRequests: 2, Capacity: 10, Healthy: true},
		"healthy2": {WorkerID: "healthy2", ActiveRequests: 8, Capacity: 10, Healthy: true},
		"unhealthy": {WorkerID: "unhealthy", ActiveRequests: 0, Capacity: 10, Healthy: false},
	}
	got, err := s.Select([]string{"healthy1", "unhealthy", "healthy2"}, loadInfo)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Expected: healthy1 (ratio=0.2), healthy2 (ratio=0.8), unhealthy.
	if got[0] != "healthy1" {
		t.Errorf("got[0]=%s, want healthy1 (least loaded)", got[0])
	}
	if got[1] != "healthy2" {
		t.Errorf("got[1]=%s, want healthy2", got[1])
	}
	if got[2] != "unhealthy" {
		t.Errorf("got[2]=%s, want unhealthy (last)", got[2])
	}
}

func TestLeastLoadedSelector_AllUnhealthy(t *testing.T) {
	s := NewLeastLoadedSelector()
	loadInfo := map[string]WorkerLoadInfo{
		"u1": {WorkerID: "u1", ActiveRequests: 0, Capacity: 10, Healthy: false},
		"u2": {WorkerID: "u2", ActiveRequests: 5, Capacity: 10, Healthy: false},
	}
	got, err := s.Select([]string{"u1", "u2"}, loadInfo)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Both unhealthy → best-effort по ratio (u1 < u2).
	if got[0] != "u1" {
		t.Errorf("got[0]=%s, want u1 (lower ratio)", got[0])
	}
	if got[1] != "u2" {
		t.Errorf("got[1]=%s, want u2", got[1])
	}
}

func TestLeastLoadedSelector_Name(t *testing.T) {
	s := NewLeastLoadedSelector()
	if s.Name() != "least_loaded" {
		t.Errorf("Name=%q, want least_loaded", s.Name())
	}
}

// =====================================================================
// RoundRobinSelector
// =====================================================================

func TestRoundRobinSelector_Empty(t *testing.T) {
	s := NewRoundRobinSelector()
	_, err := s.Select(nil, nil)
	if !errors.Is(err, ErrNoCandidates) {
		t.Errorf("err=%v, want ErrNoCandidates", err)
	}
}

func TestRoundRobinSelector_Rotates(t *testing.T) {
	s := NewRoundRobinSelector()
	candidates := []string{"a", "b", "c"}

	// Call 1: rotation 0 → [a, b, c].
	got1, _ := s.Select(candidates, nil)
	if got1[0] != "a" || got1[1] != "b" || got1[2] != "c" {
		t.Errorf("call 1: got %v, want [a b c]", got1)
	}

	// Call 2: rotation 1 → [b, c, a].
	got2, _ := s.Select(candidates, nil)
	if got2[0] != "b" || got2[1] != "c" || got2[2] != "a" {
		t.Errorf("call 2: got %v, want [b c a]", got2)
	}

	// Call 3: rotation 2 → [c, a, b].
	got3, _ := s.Select(candidates, nil)
	if got3[0] != "c" || got3[1] != "a" || got3[2] != "b" {
		t.Errorf("call 3: got %v, want [c a b]", got3)
	}

	// Call 4: rotation 0 (wrap).
	got4, _ := s.Select(candidates, nil)
	if got4[0] != "a" {
		t.Errorf("call 4 (wrap): got[0]=%s, want a", got4[0])
	}
}

func TestRoundRobinSelector_Name(t *testing.T) {
	s := NewRoundRobinSelector()
	if s.Name() != "round_robin" {
		t.Errorf("Name=%q, want round_robin", s.Name())
	}
}

// =====================================================================
// Coordinator integration
// =====================================================================

func TestModelCoordinator_SetGetSelector(t *testing.T) {
	cfg := newCoordinatorTestCfg(t)
	c := NewModelCoordinator(cfg)
	defer c.Close()

	// Default — LeastLoadedSelector.
	def := c.GetSelector()
	if def == nil {
		t.Fatal("default selector is nil")
	}
	if def.Name() != "least_loaded" {
		t.Errorf("default Name=%q, want least_loaded", def.Name())
	}

	// Set custom.
	c.SetSelector(NewRoundRobinSelector())
	if c.GetSelector().Name() != "round_robin" {
		t.Errorf("after Set: Name=%q, want round_robin", c.GetSelector().Name())
	}

	// Set nil — back to default.
	c.SetSelector(nil)
	if c.GetSelector().Name() != "least_loaded" {
		t.Errorf("after Set(nil): Name=%q, want least_loaded", c.GetSelector().Name())
	}
}

func TestModelCoordinator_SelectWorkersForSlice_NoCandidates(t *testing.T) {
	cfg := newCoordinatorTestCfg(t)
	c := NewModelCoordinator(cfg)
	defer c.Close()

	_, err := c.SelectWorkersForSlice(nil)
	if !errors.Is(err, ErrNoCandidates) {
		t.Errorf("err=%v, want ErrNoCandidates", err)
	}
}

func TestModelCoordinator_SelectWorkersForSlice_HealthyPreferred(t *testing.T) {
	cfg := newCoordinatorTestCfg(t)
	c := NewModelCoordinator(cfg)
	defer c.Close()

	// Зарегистрируем 2 worker'а (без реального HTTP — заполним LastMetrics вручную).
	c.workers["w1"] = &WorkerClient{WorkerID: "w1"}
	c.workers["w1"].LastMetrics.Store(WorkerMetricsSnapshot{ActiveRequests: 5, Capacity: 10})
	c.workers["w2"] = &WorkerClient{WorkerID: "w2"}
	c.workers["w2"].LastMetrics.Store(WorkerMetricsSnapshot{ActiveRequests: 1, Capacity: 10})

	got, err := c.SelectWorkersForSlice([]string{"w1", "w2"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got[0] != "w2" {
		t.Errorf("got[0]=%s, want w2 (least loaded)", got[0])
	}
	if got[1] != "w1" {
		t.Errorf("got[1]=%s, want w1", got[1])
	}
}

func TestModelCoordinator_GetWorkerLoadInfo_NoWorkers(t *testing.T) {
	cfg := newCoordinatorTestCfg(t)
	c := NewModelCoordinator(cfg)
	defer c.Close()

	info := c.GetWorkerLoadInfo()
	if len(info) != 0 {
		t.Errorf("len(info)=%d, want 0", len(info))
	}
}

// =====================================================================
// LayerSlice.Candidates
// =====================================================================

func TestLayerSlice_Candidates_OnlyWorkerID(t *testing.T) {
	s := LayerSlice{WorkerID: "primary"}
	got := s.Candidates()
	if len(got) != 1 || got[0] != "primary" {
		t.Errorf("got %v, want [primary]", got)
	}
}

func TestLayerSlice_Candidates_Empty(t *testing.T) {
	s := LayerSlice{}
	got := s.Candidates()
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestLayerSlice_Candidates_WorkerIDInList(t *testing.T) {
	s := LayerSlice{WorkerID: "primary", WorkerCandidates: []string{"primary", "secondary"}}
	got := s.Candidates()
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2", len(got))
	}
	if got[0] != "primary" || got[1] != "secondary" {
		t.Errorf("got %v, want [primary secondary]", got)
	}
}

func TestLayerSlice_Candidates_WorkerIDNotInList(t *testing.T) {
	s := LayerSlice{WorkerID: "primary", WorkerCandidates: []string{"secondary", "tertiary"}}
	got := s.Candidates()
	if len(got) != 3 {
		t.Fatalf("len(got)=%d, want 3 (primary должен быть добавлен)", len(got))
	}
	if got[0] != "primary" {
		t.Errorf("got[0]=%s, want primary (added first)", got[0])
	}
}

// =====================================================================
// helpers
// =====================================================================

func newCoordinatorTestCfg(t *testing.T) types.RpcCoordinatorConfig {
	t.Helper()
	return types.RpcCoordinatorConfig{
		Enabled:  true,
		Protocol: "http",
		Timeout:  "10s",
	}
}

// =====================================================================
// Concurrent safety
// =====================================================================

func TestRoundRobinSelector_ConcurrentSafe(t *testing.T) {
	s := NewRoundRobinSelector()
	candidates := []string{"a", "b", "c", "d", "e"}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Select(candidates, nil)
		}()
	}
	wg.Wait()
}

func TestLeastLoadedSelector_ConcurrentSafe(t *testing.T) {
	s := NewLeastLoadedSelector()
	loadInfo := map[string]WorkerLoadInfo{
		"a": {WorkerID: "a", ActiveRequests: 1, Capacity: 10, Healthy: true},
		"b": {WorkerID: "b", ActiveRequests: 2, Capacity: 10, Healthy: true},
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Select([]string{"a", "b"}, loadInfo)
		}()
	}
	wg.Wait()
}