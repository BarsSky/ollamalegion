// R46 (2026-08-19): regression test for LastHealthCheck zero-value bug.
//
// Pre-R46: UpdateBackendStatus mutated state.Backend.Status but never
// touched state.Backend.LastHealthCheck. The only place the field was
// written was the restore path in state.go:79 (loaded from saved state
// on startup). After a long uptime with healthy backends, every backend's
// LastHealthCheck was still the zero value `0001-01-01T00:00:00Z`, which
// broke any client logic checking "when was the backend last seen alive"
// (e.g. /api/v1/backends payload, dashboards, alerting).
//
// R46 fix: UpdateBackendStatus now also stamps LastHealthCheck = time.Now().
// This test pins the behaviour so a future refactor can't silently
// regress it.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR46_UpdateBackendStatus_StampsLastHealthCheck — the core R46 fix.
// Two snapshots: before vs after UpdateBackendStatus. The "after" must
// have a non-zero LastHealthCheck within 5 sec of now.
func TestR46_UpdateBackendStatus_StampsLastHealthCheck(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "r46-1", Type: types.BackendTypeLlamaCpp, Status: types.StatusUnhealthy},
		},
	}
	p := newProxyWithCleanup(t, cfg)

	// pre-R46 zero value
	before := p.backends["r46-1"].Backend.LastHealthCheck
	if !before.IsZero() {
		t.Fatalf("test precondition: LastHealthCheck should be zero before R46, got %v", before)
	}

	beforeCall := time.Now().UTC()
	p.UpdateBackendStatus("r46-1", types.StatusHealthy)
	afterCall := time.Now().UTC()

	after := p.backends["r46-1"].Backend.LastHealthCheck
	if after.IsZero() {
		t.Fatal("R46 REGRESSION: LastHealthCheck still zero after UpdateBackendStatus")
	}
	if after.Before(beforeCall) || after.After(afterCall.Add(5*time.Second)) {
		t.Errorf("LastHealthCheck=%v not within [%v, %v]", after, beforeCall, afterCall.Add(5*time.Second))
	}

	if p.backends["r46-1"].Backend.Status != types.StatusHealthy {
		t.Errorf("Status = %v, want StatusHealthy", p.backends["r46-1"].Backend.Status)
	}
}

// TestR46_UpdateBackendStatus_StampsOnNoOpStatusChange — R46 fix must
// stamp LastHealthCheck even when status DOESN'T change. Pre-R46, calling
// UpdateBackendStatus with the same status it already had was a no-op for
// status but ALSO a no-op for LastHealthCheck (bug). After R46, every
// call updates the timestamp — health checks happen every 10s, so a
// healthy backend has its LastHealthCheck refreshed each cycle.
func TestR46_UpdateBackendStatus_StampsOnNoOpStatusChange(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "r46-2", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
		},
	}
	p := newProxyWithCleanup(t, cfg)

	// Set timestamp to a stale value (pre-R46 simulated state).
	first := time.Now().UTC().Add(-time.Hour)
	p.backends["r46-2"].Backend.LastHealthCheck = first

	// Same status (StatusHealthy → StatusHealthy). Pre-R46: still first.
	p.UpdateBackendStatus("r46-2", types.StatusHealthy)

	after := p.backends["r46-2"].Backend.LastHealthCheck
	if after.Equal(first) {
		t.Fatal("R46 REGRESSION: LastHealthCheck not updated when status unchanged (health-check tick should still refresh it)")
	}
	if time.Since(after) > 5*time.Second {
		t.Errorf("LastHealthCheck not freshly stamped: %v (delta %v)", after, time.Since(after))
	}
}

// TestR46_UpdateBackendStatus_UnknownBackend — safety net. The function
// must NOT panic when given an unknown backendID; it should silently
// return (the original code already did this — R46 preserves the
// behaviour).
func TestR46_UpdateBackendStatus_UnknownBackend(t *testing.T) {
	cfg := &types.LoadBalancerConfig{}
	p := newProxyWithCleanup(t, cfg)
	// Should not panic.
	p.UpdateBackendStatus("does-not-exist", types.StatusHealthy)
}
