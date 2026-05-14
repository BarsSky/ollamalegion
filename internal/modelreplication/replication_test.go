package modelreplication

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// ModelGroupManager — CRUD tests
// ============================================================

func TestNewModelGroupManager(t *testing.T) {
	mgr := NewModelGroupManager()
	require.NotNil(t, mgr)
	assert.NotNil(t, mgr.groups)
	assert.False(t, mgr.IsEnabled())
}

func TestCreateGroup_Success(t *testing.T) {
	mgr := NewModelGroupManager()
	cfg := types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	}
	err := mgr.CreateGroup(cfg)
	assert.NoError(t, err)

	// Verify group was created
	groups := mgr.GetGroups()
	assert.Len(t, groups, 1)
	assert.Equal(t, "llama3:8b", groups[0].ModelName)
	assert.Equal(t, 1, groups[0].MinInstances)
	assert.Equal(t, 3, groups[0].MaxInstances)
	// Default idleUnloadAfter should be set
	assert.Equal(t, "15m", groups[0].IdleUnloadAfter)
}

func TestCreateGroup_MissingModelName(t *testing.T) {
	mgr := NewModelGroupManager()
	cfg := types.ModelGroupConfig{
		MinInstances: 1,
		MaxInstances: 3,
	}
	err := mgr.CreateGroup(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "modelName is required")
}

func TestCreateGroup_InvalidMinInstances(t *testing.T) {
	mgr := NewModelGroupManager()
	cfg := types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 0,
		MaxInstances: 3,
	}
	err := mgr.CreateGroup(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "minInstances must be > 0")
}

func TestCreateGroup_MaxLessThanMin(t *testing.T) {
	mgr := NewModelGroupManager()
	cfg := types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 5,
		MaxInstances: 3,
	}
	err := mgr.CreateGroup(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "maxInstances must be >= minInstances")
}

func TestCreateGroup_Duplicate(t *testing.T) {
	mgr := NewModelGroupManager()
	cfg := types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	}
	err := mgr.CreateGroup(cfg)
	assert.NoError(t, err)

	// Try to create the same group again
	err = mgr.CreateGroup(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestCreateGroup_CustomIdleUnload(t *testing.T) {
	mgr := NewModelGroupManager()
	cfg := types.ModelGroupConfig{
		ModelName:       "llama3:8b",
		MinInstances:    2,
		MaxInstances:    4,
		IdleUnloadAfter: "30m",
	}
	err := mgr.CreateGroup(cfg)
	assert.NoError(t, err)

	groups := mgr.GetGroups()
	assert.Len(t, groups, 1)
	assert.Equal(t, "30m", groups[0].IdleUnloadAfter)
}

func TestGetGroup_Found(t *testing.T) {
	mgr := NewModelGroupManager()
	cfg := types.ModelGroupConfig{
		ModelName:    "mistral:7b",
		MinInstances: 2,
		MaxInstances: 5,
	}
	_ = mgr.CreateGroup(cfg)

	got := mgr.GetGroup("mistral:7b")
	require.NotNil(t, got)
	assert.Equal(t, "mistral:7b", got.ModelName)
	assert.Equal(t, 2, got.MinInstances)
}

func TestGetGroup_NotFound(t *testing.T) {
	mgr := NewModelGroupManager()
	got := mgr.GetGroup("nonexistent")
	assert.Nil(t, got)
}

func TestGetGroups_Multiple(t *testing.T) {
	mgr := NewModelGroupManager()
	_ = mgr.CreateGroup(types.ModelGroupConfig{ModelName: "model-a", MinInstances: 1, MaxInstances: 2})
	_ = mgr.CreateGroup(types.ModelGroupConfig{ModelName: "model-b", MinInstances: 1, MaxInstances: 2})
	_ = mgr.CreateGroup(types.ModelGroupConfig{ModelName: "model-c", MinInstances: 1, MaxInstances: 2})

	groups := mgr.GetGroups()
	assert.Len(t, groups, 3)

	names := make(map[string]bool)
	for _, g := range groups {
		names[g.ModelName] = true
	}
	assert.True(t, names["model-a"])
	assert.True(t, names["model-b"])
	assert.True(t, names["model-c"])
}

func TestGetGroups_Empty(t *testing.T) {
	mgr := NewModelGroupManager()
	groups := mgr.GetGroups()
	assert.Empty(t, groups)
}

func TestUpdateGroup_Success(t *testing.T) {
	mgr := NewModelGroupManager()
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	err := mgr.UpdateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 2,
		MaxInstances: 5,
	})
	assert.NoError(t, err)

	got := mgr.GetGroup("llama3:8b")
	require.NotNil(t, got)
	assert.Equal(t, 2, got.MinInstances)
	assert.Equal(t, 5, got.MaxInstances)
}

func TestUpdateGroup_NotFound(t *testing.T) {
	mgr := NewModelGroupManager()
	err := mgr.UpdateGroup(types.ModelGroupConfig{
		ModelName:    "nonexistent",
		MinInstances: 1,
		MaxInstances: 3,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestUpdateGroup_ClearsNonTargetBackends(t *testing.T) {
	mgr := NewModelGroupManager()
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	// Add some instances
	mgr.mu.Lock()
	g := mgr.groups["llama3:8b"]
	g.mu.Lock()
	g.Instances["backend-1"] = &types.ModelInstanceState{BackendID: "backend-1", Status: types.ModelStateLoaded}
	g.Instances["backend-2"] = &types.ModelInstanceState{BackendID: "backend-2", Status: types.ModelStateLoaded}
	g.Instances["backend-3"] = &types.ModelInstanceState{BackendID: "backend-3", Status: types.ModelStateLoaded}
	g.mu.Unlock()
	mgr.mu.Unlock()

	// Update with specific target backends
	err := mgr.UpdateGroup(types.ModelGroupConfig{
		ModelName:      "llama3:8b",
		MinInstances:   1,
		MaxInstances:   3,
		TargetBackends: []string{"backend-1", "backend-3"},
	})
	assert.NoError(t, err)

	states := mgr.GetInstanceStates("llama3:8b")
	assert.Len(t, states, 2)
	for _, s := range states {
		assert.Contains(t, []string{"backend-1", "backend-3"}, s.BackendID)
	}
}

func TestDeleteGroup_Success(t *testing.T) {
	mgr := NewModelGroupManager()
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	err := mgr.DeleteGroup("llama3:8b")
	assert.NoError(t, err)

	got := mgr.GetGroup("llama3:8b")
	assert.Nil(t, got)
	assert.Empty(t, mgr.GetGroups())
}

func TestDeleteGroup_NotFound(t *testing.T) {
	mgr := NewModelGroupManager()
	err := mgr.DeleteGroup("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ============================================================
// ModelGroupManager — Enable/Disable tests
// ============================================================

func TestSetEnabled(t *testing.T) {
	mgr := NewModelGroupManager()
	assert.False(t, mgr.IsEnabled())

	mgr.SetEnabled(true)
	assert.True(t, mgr.IsEnabled())

	mgr.SetEnabled(false)
	assert.False(t, mgr.IsEnabled())
}

// ============================================================
// ModelGroupManager — Instance State tests
// ============================================================

func TestGetInstanceStates_NotFound(t *testing.T) {
	mgr := NewModelGroupManager()
	states := mgr.GetInstanceStates("nonexistent")
	assert.Nil(t, states)
}

func TestGetInstanceStates_Success(t *testing.T) {
	mgr := NewModelGroupManager()
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	// Manually add instances
	mgr.mu.Lock()
	g := mgr.groups["llama3:8b"]
	g.mu.Lock()
	g.Instances["backend-1"] = &types.ModelInstanceState{
		BackendID: "backend-1",
		Status:    types.ModelStateLoaded,
		LoadedAt:  time.Now(),
		UseCount:  10,
	}
	g.Instances["backend-2"] = &types.ModelInstanceState{
		BackendID: "backend-2",
		Status:    types.ModelStateLoaded,
		LoadedAt:  time.Now(),
		UseCount:  5,
	}
	g.mu.Unlock()
	mgr.mu.Unlock()

	states := mgr.GetInstanceStates("llama3:8b")
	assert.Len(t, states, 2)

	ids := make(map[string]bool)
	for _, s := range states {
		ids[s.BackendID] = true
	}
	assert.True(t, ids["backend-1"])
	assert.True(t, ids["backend-2"])
}

func TestGetGroupStats_Success(t *testing.T) {
	mgr := NewModelGroupManager()
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	// Add instances with different statuses
	mgr.mu.Lock()
	g := mgr.groups["llama3:8b"]
	g.mu.Lock()
	g.Instances["backend-1"] = &types.ModelInstanceState{
		BackendID: "backend-1",
		Status:    types.ModelStateLoaded,
		LoadedAt:  time.Now(),
		UseCount:  10,
	}
	g.Instances["backend-2"] = &types.ModelInstanceState{
		BackendID: "backend-2",
		Status:    types.ModelStateLoading,
		LoadedAt:  time.Now(),
		UseCount:  0,
	}
	g.Instances["backend-3"] = &types.ModelInstanceState{
		BackendID: "backend-3",
		Status:    types.ModelStateNotLoaded,
		UseCount:  5,
	}
	g.mu.Unlock()
	mgr.mu.Unlock()

	stats := mgr.GetGroupStats("llama3:8b")
	require.NotNil(t, stats)
	assert.Equal(t, "llama3:8b", stats["modelName"])
	assert.Equal(t, 3, stats["total"])
	assert.Equal(t, 1, stats["loaded"])
	assert.Equal(t, 1, stats["loading"])
	assert.Equal(t, 1, stats["idle"])
	assert.EqualValues(t, 15, stats["totalUses"])
	assert.Equal(t, 1, stats["minInstances"])
	assert.Equal(t, 3, stats["maxInstances"])
}

func TestGetGroupStats_NotFound(t *testing.T) {
	mgr := NewModelGroupManager()
	stats := mgr.GetGroupStats("nonexistent")
	assert.Nil(t, stats)
}

// ============================================================
// ModelGroupManager — SelectInstance tests
// ============================================================

func TestSelectInstance_NoGroup(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetBackendLoadFn(func(id string) float64 { return 0.5 })

	selected := mgr.selectInstance("nonexistent")
	assert.Empty(t, selected)
}

func TestSelectInstance_NoLoadFn(t *testing.T) {
	mgr := NewModelGroupManager()
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	selected := mgr.selectInstance("llama3:8b")
	assert.Empty(t, selected)
}

func TestSelectInstance_SelectsLeastLoaded(t *testing.T) {
	mgr := NewModelGroupManager()
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	// Add instances with use counts
	mgr.mu.Lock()
	g := mgr.groups["llama3:8b"]
	g.mu.Lock()
	g.Instances["loaded-heavy"] = &types.ModelInstanceState{
		BackendID: "loaded-heavy",
		Status:    types.ModelStateLoaded,
		LoadedAt:  time.Now(),
		UseCount:  100,
	}
	g.Instances["loaded-light"] = &types.ModelInstanceState{
		BackendID: "loaded-light",
		Status:    types.ModelStateLoaded,
		UseCount:  10,
	}
	g.Instances["not-loaded"] = &types.ModelInstanceState{
		BackendID: "not-loaded",
		Status:    types.ModelStateNotLoaded,
	}
	g.mu.Unlock()
	mgr.mu.Unlock()

	mgr.SetBackendLoadFn(func(id string) float64 {
		if id == "loaded-heavy" {
			return 0.9
		}
		if id == "loaded-light" {
			return 0.2
		}
		return 1.0
	})

	selected := mgr.selectInstance("llama3:8b")
	assert.Equal(t, "loaded-light", selected)
}

// ============================================================
// ModelGroupManager — Warmup tests
// ============================================================

func TestWarmup_WithCallback(t *testing.T) {
	mgr := NewModelGroupManager()
	warmupCalled := false
	mgr.SetWarmupFn(func(backendID, modelName string) error {
		warmupCalled = true
		assert.Equal(t, "backend-1", backendID)
		assert.Equal(t, "llama3:8b", modelName)
		return nil
	})

	err := mgr.Warmup("llama3:8b", "backend-1")
	assert.NoError(t, err)
	assert.True(t, warmupCalled)
}

func TestWarmup_NoCallback(t *testing.T) {
	mgr := NewModelGroupManager()
	err := mgr.Warmup("llama3:8b", "backend-1")
	assert.NoError(t, err)
}

// ============================================================
// ModelGroupManager — EnsureInstances tests
// ============================================================

func TestEnsureInstances_WhenDisabled(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(false)
	// Should not panic or do anything
	mgr.EnsureInstances()
}

func TestEnsureInstances_ScaleUp(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)

	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 2,
		MaxInstances: 4,
	})

	backendCalled := false

	mgr.SetFreeBackendFn(func(modelName string, targets []string) []string {
		backendCalled = true
		assert.Equal(t, "llama3:8b", modelName)
		return []string{"backend-1", "backend-2", "backend-3"}
	})

	mgr.SetWarmupFn(func(backendID, modelName string) error {
		assert.Equal(t, "llama3:8b", modelName)
		return nil
	})

	mgr.EnsureInstances()

	// Give warmup goroutines time to execute
	time.Sleep(50 * time.Millisecond)

	assert.True(t, backendCalled, "FreeBackendFn should have been called")
	// At least one warmup should have been triggered
	states := mgr.GetInstanceStates("llama3:8b")
	assert.GreaterOrEqual(t, len(states), 1, "should have at least one instance")
}

func TestEnsureInstances_ScaleUpAlreadyLoading(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)

	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 2,
		MaxInstances: 4,
	})

	// Add a loading instance
	mgr.mu.Lock()
	g := mgr.groups["llama3:8b"]
	g.mu.Lock()
	g.Instances["backend-loading"] = &types.ModelInstanceState{
		BackendID: "backend-loading",
		Status:    types.ModelStateLoading,
	}
	g.mu.Unlock()
	mgr.mu.Unlock()

	warmupCount := 0
	mgr.SetFreeBackendFn(func(modelName string, targets []string) []string {
		return []string{"backend-1", "backend-2"}
	})
	mgr.SetWarmupFn(func(backendID, modelName string) error {
		warmupCount++
		return nil
	})

	mgr.EnsureInstances()
	time.Sleep(50 * time.Millisecond)

	// Should only warmup 1 more (to reach min 2, but 1 is already loading)
	assert.GreaterOrEqual(t, warmupCount, 1)
}

// ============================================================
// Callback tests
// ============================================================

func TestSetBackendLoadFn(t *testing.T) {
	mgr := NewModelGroupManager()
	fn := func(id string) float64 { return 0.5 }
	mgr.SetBackendLoadFn(fn)
	assert.NotNil(t, mgr.loadFn)
	// Verify it's the correct function
	assert.Equal(t, 0.5, mgr.loadFn("test"))
}

func TestSetFreeBackendFn(t *testing.T) {
	mgr := NewModelGroupManager()
	fn := func(modelName string, targets []string) []string {
		return []string{"backend-1"}
	}
	mgr.SetFreeBackendFn(fn)
	assert.NotNil(t, mgr.backendFn)
	assert.Equal(t, []string{"backend-1"}, mgr.backendFn("test", nil))
}

func TestSetWarmupFn(t *testing.T) {
	mgr := NewModelGroupManager()
	fn := func(backendID, modelName string) error { return nil }
	mgr.SetWarmupFn(fn)
	assert.NotNil(t, mgr.warmupFn)
}

// ============================================================
// GroupController tests
// ============================================================

func TestNewGroupController(t *testing.T) {
	mgr := NewModelGroupManager()
	gc := NewGroupController(mgr)
	require.NotNil(t, gc)
	assert.Equal(t, mgr, gc.manager)
	assert.Equal(t, 10*time.Second, gc.interval)
	assert.False(t, gc.IsRunning())
}

func TestGroupController_SetInterval(t *testing.T) {
	mgr := NewModelGroupManager()
	gc := NewGroupController(mgr)
	assert.Equal(t, 10*time.Second, gc.interval)

	gc.SetInterval(5 * time.Second)
	assert.Equal(t, 5*time.Second, gc.interval)

	// Zero interval should be ignored
	gc.SetInterval(0)
	assert.Equal(t, 5*time.Second, gc.interval)
}

func TestGroupController_StartStop(t *testing.T) {
	mgr := NewModelGroupManager()
	gc := NewGroupController(mgr)

	err := gc.Start()
	assert.NoError(t, err)
	assert.True(t, gc.IsRunning())

	// Double start should not error
	err = gc.Start()
	assert.NoError(t, err)
	assert.True(t, gc.IsRunning())

	err = gc.Stop()
	assert.NoError(t, err)
	assert.False(t, gc.IsRunning())

	// Double stop should not error
	err = gc.Stop()
	assert.NoError(t, err)
}

func TestGroupController_ReconcileDisabled(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(false)
	gc := NewGroupController(mgr)

	// Should not panic when manager is disabled
	gc.reconcileGroups()
}

func TestGroupController_TriggerReconcile(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	mgr.SetFreeBackendFn(func(modelName string, targets []string) []string {
		return []string{"backend-1"}
	})
	mgr.SetWarmupFn(func(backendID, modelName string) error {
		return nil
	})

	gc := NewGroupController(mgr)
	gc.TriggerReconcile()

	// Give warmup time
	time.Sleep(50 * time.Millisecond)
	states := mgr.GetInstanceStates("llama3:8b")
	assert.GreaterOrEqual(t, len(states), 1)
}

func TestGroupController_TriggerReconcileDisabled(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(false)
	gc := NewGroupController(mgr)
	// Should not panic
	gc.TriggerReconcile()
}

// ============================================================
// GroupAwareSelector tests
// ============================================================

func TestNewGroupAwareSelector(t *testing.T) {
	mgr := NewModelGroupManager()
	sel := NewGroupAwareSelector(mgr)
	require.NotNil(t, sel)
	assert.Equal(t, mgr, sel.manager)
}

func TestSelect_DisabledManager(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(false)
	sel := NewGroupAwareSelector(mgr)

	selected := sel.Select("llama3:8b")
	assert.Empty(t, selected)
}

func TestSelect_NotInGroup(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	sel := NewGroupAwareSelector(mgr)

	selected := sel.Select("nonexistent")
	assert.Empty(t, selected)
}

func TestSelect_InGroup(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	// Add instances
	mgr.mu.Lock()
	g := mgr.groups["llama3:8b"]
	g.mu.Lock()
	g.Instances["backend-1"] = &types.ModelInstanceState{
		BackendID: "backend-1",
		Status:    types.ModelStateLoaded,
	}
	g.mu.Unlock()
	mgr.mu.Unlock()

	mgr.SetBackendLoadFn(func(id string) float64 { return 0.3 })

	sel := NewGroupAwareSelector(mgr)
	selected := sel.Select("llama3:8b")
	assert.Equal(t, "backend-1", selected)
}

func TestIsGroupModel(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	sel := NewGroupAwareSelector(mgr)
	assert.True(t, sel.IsGroupModel("llama3:8b"))
	assert.False(t, sel.IsGroupModel("nonexistent"))
}

func TestIsGroupModel_Disabled(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(false)
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	sel := NewGroupAwareSelector(mgr)
	assert.False(t, sel.IsGroupModel("llama3:8b"))
}

func TestGetGroupCandidates(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	// Add loaded and not-loaded instances
	mgr.mu.Lock()
	g := mgr.groups["llama3:8b"]
	g.mu.Lock()
	g.Instances["backend-1"] = &types.ModelInstanceState{
		BackendID: "backend-1",
		Status:    types.ModelStateLoaded,
	}
	g.Instances["backend-2"] = &types.ModelInstanceState{
		BackendID: "backend-2",
		Status:    types.ModelStateNotLoaded,
	}
	g.mu.Unlock()
	mgr.mu.Unlock()

	sel := NewGroupAwareSelector(mgr)
	candidates := sel.GetGroupCandidates("llama3:8b")
	assert.Len(t, candidates, 1)
	assert.Equal(t, "backend-1", candidates[0])
}

func TestGetGroupCandidates_Disabled(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(false)
	sel := NewGroupAwareSelector(mgr)

	candidates := sel.GetGroupCandidates("llama3:8b")
	assert.Nil(t, candidates)
}

func TestGetGroupCandidates_NotInGroup(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	sel := NewGroupAwareSelector(mgr)

	candidates := sel.GetGroupCandidates("nonexistent")
	assert.Nil(t, candidates)
}

func TestGetGroupStats_Selector(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 3,
	})

	sel := NewGroupAwareSelector(mgr)
	stats := sel.GetGroupStats("llama3:8b")
	require.NotNil(t, stats)
	assert.Equal(t, "llama3:8b", stats["modelName"])
	assert.Equal(t, 0, stats["total"])
}

// ============================================================
// Concurrency tests
// ============================================================

func TestConcurrentGroupAccess(t *testing.T) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3:8b",
		MinInstances: 1,
		MaxInstances: 5,
	})

	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			switch idx % 4 {
			case 0:
				mgr.GetGroup("llama3:8b")
			case 1:
				mgr.GetGroups()
			case 2:
				mgr.GetInstanceStates("llama3:8b")
			case 3:
				mgr.GetGroupStats("llama3:8b")
			}
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

// ============================================================
// ModelGroupManager — Start/Stop tests (lifecycle)
// ============================================================

func TestStartStop(t *testing.T) {
	mgr := NewModelGroupManager()
	err := mgr.Start()
	assert.NoError(t, err)

	err = mgr.Stop()
	assert.NoError(t, err)
}
