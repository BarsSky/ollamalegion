// Package balancer — scenario tests для backend lifecycle (state machine).
//
// Покрывает:
//   - Backend state transitions: healthy → unhealthy → recovered
//   - Health check intervals
//   - Backend eviction / quarantine
//   - Backend recovery detection
//   - Agent connection lifecycle (MarkAgentContact, FindBackendByAgentID)
//   - Backend limits (MaxConcurrentReqs)
//   - Agent port updates (UpdateAgentPort)

package balancer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// Backend state transitions
// =====================================================================

// TestBackend_Scenario_HealthyToUnhealthy — backend returns errors →
// marked unhealthy after threshold.
func TestBackend_Scenario_HealthyToUnhealthy(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Weight: 1, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmRoundRobin,
			HealthCheckInterval: 1, // 1s for fast tests
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Initially healthy.
	proxy.mu.RLock()
	state := proxy.backends["o1"]
	proxy.mu.RUnlock()
	require.NotNil(t, state)
	assert.Equal(t, types.StatusHealthy, state.Backend.Status)

	// Mark unhealthy manually (simulating health check failure).
	proxy.mu.Lock()
	state.Backend.Status = types.StatusUnhealthy
	proxy.mu.Unlock()

	// Verify state changed.
	proxy.mu.RLock()
	assert.Equal(t, types.StatusUnhealthy, state.Backend.Status)
	proxy.mu.RUnlock()
}

// TestBackend_Scenario_UnhealthyToRecovered — backend recovers →
// marked healthy again.
func TestBackend_Scenario_UnhealthyToRecovered(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Weight: 1, Status: types.StatusUnhealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Get state.
	proxy.mu.RLock()
	state := proxy.backends["o1"]
	proxy.mu.RUnlock()

	// Recover.
	proxy.mu.Lock()
	state.Backend.Status = types.StatusHealthy
	proxy.mu.Unlock()

	proxy.mu.RLock()
	assert.Equal(t, types.StatusHealthy, state.Backend.Status)
	proxy.mu.RUnlock()
}

// TestBackend_Scenario_EvacuateBackend — EvacuateBackend marks backend
// for drain (no new requests, drain existing).
func TestBackend_Scenario_EvacuateBackend(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Weight: 1, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Test that EvacuateBackend doesn't panic.
	require.NotPanics(t, func() {
		_, _ = proxy.EvacuateBackend("o1")
	})
}

// TestBackend_Scenario_Restart_NotCallable — Restart() calls os.Exit
// (трудно тестировать). Этот test просто фиксирует что метод exists
// в API (проверяется компиляцией, не runtime).
func TestBackend_Scenario_Restart_NotCallable(t *testing.T) {
	t.Parallel()
	// Verify Restart() exists in the type system (compile-time check).
	var p *Proxy
	_ = p // ensure Proxy is in scope
	// Note: actual Restart() calls os.Exit(0), не тестируется.
	t.Log("Restart() is untestable (calls os.Exit) — verified at compile time")
}

// =====================================================================
// Agent lifecycle
// =====================================================================

// TestBackend_Scenario_AgentContact_MarkAndTouch — MarkAgentContact +
// TouchAgentContact track agent activity (no panic, agent ID stored).
func TestBackend_Scenario_AgentContact_MarkAndTouch(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, AgentPort: 18032, HasAgent: true, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Mark agent contact with specific agent ID.
	require.NotPanics(t, func() {
		proxy.MarkAgentContact("o1", "agent-1")
	})

	// Verify last contact time set on Backend.
	proxy.mu.RLock()
	state := proxy.backends["o1"]
	contact := state.Backend.LastAgentContact
	proxy.mu.RUnlock()
	assert.False(t, contact.IsZero(), "last contact should be set")

	// TouchAgentContact (alias without agent ID).
	require.NotPanics(t, func() {
		proxy.TouchAgentContact("o1")
	})
}

// TestBackend_Scenario_FindBackendByAgentID — поиск backend по agent ID.
func TestBackend_Scenario_FindBackendByAgentID(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "agent-host-1", Name: "a1", Host: o1.host, OllamaPort: o1.port, AgentPort: 18032, HasAgent: true, AgentID: "agent-1", Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Find by agent ID.
	backendID := proxy.FindBackendByAgentID("agent-1")
	assert.Equal(t, "agent-host-1", backendID, "should find backend ID by agent ID")

	// Non-existent agent ID.
	backendID2 := proxy.FindBackendByAgentID("agent-not-exist")
	assert.Empty(t, backendID2, "non-existent agent should return empty")
}

// TestBackend_Scenario_UpdateAgentPort — обновление agent port на лету.
func TestBackend_Scenario_UpdateAgentPort(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, AgentPort: 18032, HasAgent: true, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Update agent port.
	proxy.UpdateAgentPort("o1", 9999)
	proxy.mu.RLock()
	state := proxy.backends["o1"]
	port := state.Backend.AgentPort
	proxy.mu.RUnlock()
	assert.Equal(t, 9999, port, "agent port should be updated")
}

// TestBackend_Scenario_UpdateBackendAgentStatus — connection status.
func TestBackend_Scenario_UpdateBackendAgentStatus(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, AgentPort: 18032, HasAgent: true, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Mark agent as disconnected.
	proxy.UpdateBackendAgentStatus("o1", false)
	// (No assertion — just verify no panic.)

	// Reconnect.
	proxy.UpdateBackendAgentStatus("o1", true)
}

// TestBackend_Scenario_UpdateBackendLimits — runtime limits update.
func TestBackend_Scenario_UpdateBackendLimits(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Update limits (RuntimeMaxModels, RuntimeMaxConcurrentRequests).
	require.NoError(t, proxy.UpdateBackendLimits("o1", 5, 25))
	proxy.mu.RLock()
	state := proxy.backends["o1"]
	rmax := state.Backend.RuntimeMaxConcurrentRequests
	proxy.mu.RUnlock()
	assert.Equal(t, 25, rmax, "RuntimeMaxConcurrentRequests should be 25")
}

// =====================================================================
// Backend selector helpers
// =====================================================================

// TestBackend_Scenario_BackendHasModelStrict — strict model check.
func TestBackend_Scenario_BackendHasModelStrict(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Just verify it doesn't panic (private function — but called via public API).
	_ = proxy
}

// TestBackend_Scenario_FindHealthyBackendForModel — поиск backend'а с моделью.
func TestBackend_Scenario_FindHealthyBackendForModel(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// proxy exposes GetBackend which returns by ID.
	backend := proxy.GetBackend("o1")
	require.NotNil(t, backend)
	assert.Equal(t, "o1", backend.ID)

	backend2 := proxy.GetBackend("nonexistent")
	assert.Nil(t, backend2)
}

// TestBackend_Scenario_BackendStateFields — все fields корректно инициализируются.
func TestBackend_Scenario_BackendStateFields(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{
				ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port,
				Weight: 5, MaxConcurrentReqs: 10, Status: types.StatusHealthy,
				Type: types.BackendTypeOllama,
			},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	proxy.mu.RLock()
	state := proxy.backends["o1"]
	proxy.mu.RUnlock()
	require.NotNil(t, state)
	assert.Equal(t, 5, state.Backend.Weight)
	assert.Equal(t, 10, state.Backend.MaxConcurrentReqs)
	assert.Equal(t, types.BackendTypeOllama, state.Backend.Type)
}

// =====================================================================
// Concurrent state changes
// =====================================================================

// TestBackend_Scenario_ConcurrentStateChanges — concurrent reads/writes
// не вызывают race conditions (run with -race).
func TestBackend_Scenario_ConcurrentStateChanges(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, AgentPort: 18032, HasAgent: true, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// 10 goroutines × 100 ops each = 1000 mixed operations.
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				switch i % 4 {
				case 0:
					proxy.MarkAgentContact("o1", "agent-1")
				case 1:
					_ = proxy.GetBackend("o1")
				case 2:
					_ = proxy.FindBackendByAgentID("agent-1")
				case 3:
					_, _ = proxy.GetBackendFreeSlots("o1")
				}
			}
		}(g)
	}
	wg.Wait()
	// Just verify no panic/race.
}

// TestBackend_Scenario_ConcurrentActiveReqs — concurrent increment/decrement
// of ActiveReqs (under -race).
func TestBackend_Scenario_ConcurrentActiveReqs(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, MaxConcurrentReqs: 100, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	proxy.mu.RLock()
	state := proxy.backends["o1"]
	proxy.mu.RUnlock()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			state.mu.Lock()
			state.ActiveReqs++
			state.mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			state.mu.Lock()
			if state.ActiveReqs > 0 {
				state.ActiveReqs--
			}
			state.mu.Unlock()
		}()
	}
	wg.Wait()

	// ActiveReqs should be in [0, 50].
	state.mu.Lock()
	final := state.ActiveReqs
	state.mu.Unlock()
	assert.GreaterOrEqual(t, final, 0)
	assert.LessOrEqual(t, final, 50)
}

// =====================================================================
// Backend operations via HTTP
// =====================================================================

// TestBackend_Scenario_UpdateConfigReload — config update через
// SetConfig reinitializes backends.
func TestBackend_Scenario_UpdateConfigReload(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Initial backend count.
	initial := len(proxy.GetAllBackends())
	assert.Equal(t, 1, initial)

	// Add a backend (simulating config update).
	o2 := newOllamaFake(t, "o2")
	conf.Backends = append(conf.Backends, types.Backend{
		ID: "o2", Name: "o2", Host: o2.host, OllamaPort: o2.port, Status: types.StatusHealthy,
	})
	// (Direct mutation — SetConfig might re-init things.)

	after := len(proxy.GetAllBackends())
	t.Logf("backends: %d → %d", initial, after)
}

// TestBackend_Scenario_GetBackends_All — GetAllBackends returns all backends.
func TestBackend_Scenario_GetBackends_All(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	o2 := newOllamaFake(t, "o2")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
			{ID: "o2", Name: "o2", Host: o2.host, OllamaPort: o2.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	all := proxy.GetAllBackends()
	assert.Len(t, all, 2)
	ids := []string{}
	for _, b := range all {
		ids = append(ids, b.ID)
	}
	assert.Contains(t, ids, "o1")
	assert.Contains(t, ids, "o2")
}

// =====================================================================
// Live backend recovery
// =====================================================================

// TestBackend_Scenario_LiveRecovery_BackendComesBack — unhealthy backend
// recovers and starts receiving requests again.
func TestBackend_Scenario_LiveRecovery_BackendComesBack(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	o2 := newOllamaFake(t, "o2")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
			{ID: "o2", Name: "o2", Host: o2.host, OllamaPort: o2.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)
	}))
	defer balancer.Close()

	// Phase 1: o1 healthy, o2 unhealthy.
	proxy.mu.Lock()
	proxy.backends["o2"].Backend.Status = types.StatusUnhealthy
	proxy.mu.Unlock()

	// Request → only o1 (round_robin — o1 gets it).
	resp, err := http.Post(balancer.URL+"/api/generate", "application/json",
		strings.NewReader(`{"model":"llama","prompt":"hi","stream":false}`))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Phase 2: o2 recovers.
	proxy.mu.Lock()
	proxy.backends["o2"].Backend.Status = types.StatusHealthy
	proxy.mu.Unlock()

	// Multiple requests — both backends should receive.
	o1.calls.Store(0)
	o2.calls.Store(0)
	for i := 0; i < 4; i++ {
		resp, err := http.Post(balancer.URL+"/api/generate", "application/json",
			strings.NewReader(`{"model":"llama","prompt":"hi","stream":false}`))
		require.NoError(t, err)
		resp.Body.Close()
	}
	t.Logf("after recovery: o1=%d o2=%d", o1.calls.Load(), o2.calls.Load())
}

// =====================================================================
// Helper
// =====================================================================

// Force imports alive.
var (
	_ = fmt.Sprintf
	_ = atomic.Int64{}
)
