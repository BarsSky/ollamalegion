package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestNewUnloadScheduler — проверяет что UnloadScheduler ВКЛЮЧАЕТСЯ только при явном
// указании idleUnloadAfter (ВАЖНО 2026-06-22: раньше планировщик был всегда enabled=true
// с fallback 30m, теперь — по дефолту ВЫКЛЮЧЕН. Пользователь должен явно задать
// balancing.modelInstances.idle_unload_after для включения).
func TestNewUnloadScheduler(t *testing.T) {
	// Случай 1: idleUnloadAfter="10m" → планировщик ВКЛЮЧЁН с этим timeout.
	p := &Proxy{
		config: &types.LoadBalancerConfig{Balancing: types.BalancingSettings{
			ModelInstances: types.ModelInstanceConfig{IdleUnloadAfter: "10m"},
		}},
	}
	us := NewUnloadScheduler(p)
	if us == nil {
		t.Fatal("NewUnloadScheduler returned nil")
	}
	if us.idleTimeout != 10*time.Minute {
		t.Errorf("idleTimeout = %v, want 10m", us.idleTimeout)
	}
	if !us.enabled {
		t.Error("expected enabled when idleUnloadAfter is set")
	}

	// Invalid duration → планировщик ВЫКЛЮЧЁН (safety: лучше ничего не выгружать,
	// чем выгрузить неожиданно из-за опечатки в конфиге).
	p2 := &Proxy{
		config: &types.LoadBalancerConfig{Balancing: types.BalancingSettings{
			ModelInstances: types.ModelInstanceConfig{IdleUnloadAfter: "invalid"},
		}},
	}
	us2 := NewUnloadScheduler(p2)
	if us2.idleTimeout != 0 {
		t.Errorf("invalid duration fallback idleTimeout = %v, want 0 (disabled for safety)", us2.idleTimeout)
	}
	if us2.enabled {
		t.Error("expected DISABLED for invalid duration (no surprise unloads)")
	}

	// Пустой idleUnloadAfter → планировщик ВЫКЛЮЧЁН (дефолт).
	pEmpty := &Proxy{
		config: &types.LoadBalancerConfig{Balancing: types.BalancingSettings{
			ModelInstances: types.ModelInstanceConfig{IdleUnloadAfter: ""},
		}},
	}
	usEmpty := NewUnloadScheduler(pEmpty)
	if usEmpty.enabled {
		t.Error("expected DISABLED by default (empty idleUnloadAfter)")
	}
	if usEmpty.idleTimeout != 0 {
		t.Errorf("default idleTimeout = %v, want 0", usEmpty.idleTimeout)
	}
}

func TestRecordModelUse(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	us := NewUnloadScheduler(p)
	us.enabled = true

	us.RecordModelUse("m1", "b1")
	us.mu.Lock()
	_, ok := us.lastUsed["m1@b1"]
	us.mu.Unlock()
	if !ok {
		t.Error("expected lastUsed to be recorded")
	}

	// Disabled should not record
	us.SetEnabled(false)
	us.RecordModelUse("m2", "b1")
	us.mu.Lock()
	_, ok2 := us.lastUsed["m2@b1"]
	us.mu.Unlock()
	if ok2 {
		t.Error("disabled scheduler should not record")
	}

	// Empty model/backend should not panic
	us.RecordModelUse("", "b1")
	us.RecordModelUse("m1", "")
}

func TestGetUnloadCandidates(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend: &types.Backend{ID: "b1", Status: types.StatusHealthy},
			},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		queueMgr:   NewQueueManager(nil, 10, 1, 30),
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID: "b1",
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{{Name: "m1"}, {Name: "m2"}},
		},
	}
	p.metricsMgr.mu.Unlock()

	us := NewUnloadScheduler(p)
	us.idleTimeout = 200 * time.Millisecond
	us.enabled = true

	// Record m1 as used recently, m2 not used
	us.RecordModelUse("m1", "b1")
	// m2 will have no entry → should get now as lastUsed

	// Immediately: nothing should be idle
	candidates := us.getUnloadCandidates()
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates immediately, got %d", len(candidates))
	}

	// Wait past idle timeout
	time.Sleep(250 * time.Millisecond)

	// Both m1 and m2 should be candidates (m1 was used 250ms ago > 200ms)
	candidates = us.getUnloadCandidates()
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	// Re-record m1, then only m2 should be idle
	us.RecordModelUse("m1", "b1")
	candidates = us.getUnloadCandidates()
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate after re-record, got %d", len(candidates))
	}
	if candidates[0].Model != "m2" {
		t.Errorf("expected m2 as candidate, got %s", candidates[0].Model)
	}
}

func TestGetUnloadCandidates_ActiveModels(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend: &types.Backend{ID: "b1", Status: types.StatusHealthy},
			},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		queueMgr:   NewQueueManager(nil, 10, 1, 30),
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID: "b1",
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{{Name: "m1"}},
		},
	}
	p.metricsMgr.mu.Unlock()

	us := NewUnloadScheduler(p)
	us.idleTimeout = 1 * time.Millisecond
	us.enabled = true

	// Add model to queue processing
	p.queueMgr.processingMu.Lock()
	p.queueMgr.processing = []*QueuedRequest{{Model: "m1", Target: "b1"}}
	p.queueMgr.processingMu.Unlock()

	time.Sleep(10 * time.Millisecond)

	candidates := us.getUnloadCandidates()
	for _, c := range candidates {
		if c.Model == "m1" {
			t.Error("m1 should not be a candidate while active")
		}
	}
}

func TestGetUnloadCandidates_UnhealthyBackend(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend: &types.Backend{ID: "b1", Status: types.StatusUnhealthy},
			},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		queueMgr:   NewQueueManager(nil, 10, 1, 30),
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID: "b1",
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{{Name: "m1"}},
		},
	}
	p.metricsMgr.mu.Unlock()

	us := NewUnloadScheduler(p)
	us.idleTimeout = 1 * time.Millisecond
	us.enabled = true

	time.Sleep(10 * time.Millisecond)

	candidates := us.getUnloadCandidates()
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates for unhealthy backend, got %d", len(candidates))
	}
}

func TestGetUnloadCandidates_WarmingModel(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend:       &types.Backend{ID: "b1", Status: types.StatusHealthy},
				WarmingUpModels: map[string]*types.WarmupState{"m1": {StartedAt: time.Now()}},
			},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		queueMgr:   NewQueueManager(nil, 10, 1, 30),
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
		ID: "b1",
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{{Name: "m1"}},
		},
	}
	p.metricsMgr.mu.Unlock()

	us := NewUnloadScheduler(p)
	us.idleTimeout = 1 * time.Millisecond
	us.enabled = true

	time.Sleep(10 * time.Millisecond)

	candidates := us.getUnloadCandidates()
	for _, c := range candidates {
		if c.Model == "m1" {
			t.Error("warming model should not be a candidate")
		}
	}
}

func TestUnloadModel(t *testing.T) {
	p := &Proxy{
		backends: map[string]*BackendState{
			"b1": {
				Backend:         &types.Backend{ID: "b1", Status: types.StatusHealthy},
				WarmingUpModels: make(map[string]*types.WarmupState),
			},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
		queueMgr:   NewQueueManager(nil, 10, 1, 30),
	}

	us := NewUnloadScheduler(p)
	us.enabled = true
	us.RecordModelUse("m1", "b1")

	// Verify lastUsed exists
	us.mu.Lock()
	_, ok := us.lastUsed["m1@b1"]
	us.mu.Unlock()
	if !ok {
		t.Fatal("lastUsed should exist before unload")
	}

	us.unloadModel("m1", "b1")

	// Verify lastUsed cleared
	us.mu.Lock()
	_, ok2 := us.lastUsed["m1@b1"]
	us.mu.Unlock()
	if ok2 {
		t.Error("lastUsed should be cleared after unload")
	}

	// Verify model marked as warming (to block new requests)
	p.backends["b1"].mu.Lock()
	_, isWarming := p.backends["b1"].WarmingUpModels["m1"]
	p.backends["b1"].mu.Unlock()
	if !isWarming {
		t.Error("model should be marked as warming after unload")
	}

	// Missing backend should not panic
	us.unloadModel("m1", "missing")
}

func TestUnloadSchedulerStartStop(t *testing.T) {
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		metricsMgr: NewMetricsManager(),
		queueMgr:   NewQueueManager(nil, 10, 1, 30),
	}
	us := NewUnloadScheduler(p)
	us.checkInterval = 50 * time.Millisecond

	// Start
	us.Start()
	select {
	case <-time.After(100 * time.Millisecond):
		// ok
	}

	// Stop should not panic
	us.Stop()
	us.Stop() // double stop should be safe
}

func TestSetEnabled(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}, metricsMgr: NewMetricsManager(), queueMgr: NewQueueManager(nil, 10, 1, 30)}
	us := NewUnloadScheduler(p)
	us.checkInterval = 50 * time.Millisecond

	// Enable when already enabled
	us.SetEnabled(true)
	if !us.enabled {
		t.Error("should be enabled")
	}

	// Disable
	us.SetEnabled(false)
	if us.enabled {
		t.Error("should be disabled")
	}
	us.Stop()

	// Re-enable
	us.SetEnabled(true)
	if !us.enabled {
		t.Error("should be re-enabled")
	}
	us.Stop()
}

func TestActiveModelsMap(t *testing.T) {
	p := &Proxy{
		config:   &types.LoadBalancerConfig{},
		queueMgr: NewQueueManager(nil, 10, 1, 30),
	}

	p.queueMgr.processingMu.Lock()
	p.queueMgr.processing = []*QueuedRequest{
		{Model: "m1", Target: "b1"},
	}
	p.queueMgr.processingMu.Unlock()

	p.queueMgr.pendingMu.Lock()
	p.queueMgr.pending = []*QueuedRequest{
		{Model: "m2", Target: "b2"},
	}
	p.queueMgr.pendingMu.Unlock()

	us := NewUnloadScheduler(p)
	active := us.activeModelsMap()

	if !active["m1@b1"] {
		t.Error("m1@b1 should be active")
	}
	if !active["m2@b2"] {
		t.Error("m2@b2 should be active")
	}
	if active["m3@b3"] {
		t.Error("m3@b3 should not be active")
	}
}