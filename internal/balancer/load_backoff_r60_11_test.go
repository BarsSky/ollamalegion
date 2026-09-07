// load_backoff_r60_11_test.go — R60.11 (2026-09-07): tests for new
// Reset/ResetAll/Snapshot methods on loadBackoff.
//
// Motivation: pre-R60.11, circuit breaker could only be cleared by waiting
// 60s (TTL default) or restarting the balancer container. R60.11 adds
// explicit Reset/ResetAll methods exposed via admin endpoint.

package balancer

import (
	"testing"
	"time"
)

// TestLoadBackoff_Reset_Specific — R60.11: Reset(backend, model) clears
// the state for that pair, returns true. Reset of non-existent state
// returns false.
func TestLoadBackoff_Reset_Specific(t *testing.T) {
	lb := newLoadBackoff()

	// No state initially → Reset returns false.
	if lb.Reset("backend-1", "model-A") {
		t.Error("Reset of non-existent state should return false")
	}

	// Force 3 failures to open breaker.
	for i := 0; i < 3; i++ {
		lb.recordFailure("backend-1", "model-A", "test error")
	}

	// Verify breaker is open.
	skip, reason, _ := lb.shouldSkip("backend-1", "model-A")
	if !skip {
		t.Errorf("breaker should be open after 3 failures, got skip=%v reason=%q", skip, reason)
	}

	// R60.11: Reset should clear it.
	if !lb.Reset("backend-1", "model-A") {
		t.Error("Reset of existing state should return true")
	}

	// Verify breaker is closed now.
	skip, _, _ = lb.shouldSkip("backend-1", "model-A")
	if skip {
		t.Error("breaker should be closed after Reset")
	}

	// Snapshot should not include the reset state.
	snap := lb.Snapshot()
	if len(snap) != 0 {
		t.Errorf("Snapshot should be empty after Reset, got %d states", len(snap))
	}
}

// TestLoadBackoff_ResetAll — R60.11: ResetAll() clears all states,
// returns count cleared.
func TestLoadBackoff_ResetAll(t *testing.T) {
	lb := newLoadBackoff()

	// Add 3 distinct states.
	lb.recordFailure("backend-1", "model-A", "err1")
	lb.recordFailure("backend-2", "model-B", "err2")
	lb.recordFailure("backend-3", "model-C", "err3")

	// Open breakers for backend-2 and backend-3 (need 3 failures each).
	for i := 0; i < 2; i++ {
		lb.recordFailure("backend-2", "model-B", "err")
	}
	for i := 0; i < 2; i++ {
		lb.recordFailure("backend-3", "model-C", "err")
	}

	// Verify all 3 states exist.
	if len(lb.Snapshot()) != 3 {
		t.Errorf("expected 3 states, got %d", len(lb.Snapshot()))
	}

	// R60.11: ResetAll clears everything, returns count.
	count := lb.ResetAll()
	if count != 3 {
		t.Errorf("ResetAll should return 3, got %d", count)
	}

	// Verify all cleared.
	if len(lb.Snapshot()) != 0 {
		t.Errorf("Snapshot should be empty after ResetAll, got %d states", len(lb.Snapshot()))
	}
}

// TestLoadBackoff_ResetAll_Empty — R60.11: ResetAll on empty state returns 0.
func TestLoadBackoff_ResetAll_Empty(t *testing.T) {
	lb := newLoadBackoff()
	count := lb.ResetAll()
	if count != 0 {
		t.Errorf("ResetAll on empty should return 0, got %d", count)
	}
}

// TestLoadBackoff_Reset_PreservesOtherKeys — R60.11: Reset of one
// (backend, model) should not affect other keys.
func TestLoadBackoff_Reset_PreservesOtherKeys(t *testing.T) {
	lb := newLoadBackoff()

	lb.recordFailure("backend-1", "model-A", "err1")
	lb.recordFailure("backend-1", "model-B", "err2")
	lb.recordFailure("backend-2", "model-A", "err3")

	// Reset only (backend-1, model-A).
	if !lb.Reset("backend-1", "model-A") {
		t.Fatal("Reset should return true")
	}

	// Verify (backend-1, model-B) and (backend-2, model-A) still present.
	snap := lb.Snapshot()
	if len(snap) != 2 {
		t.Errorf("expected 2 remaining states, got %d", len(snap))
	}

	foundBM := false
	found2A := false
	for _, s := range snap {
		if s.BackendID == "backend-1" && s.ModelName == "model-B" {
			foundBM = true
		}
		if s.BackendID == "backend-2" && s.ModelName == "model-A" {
			found2A = true
		}
	}
	if !foundBM {
		t.Error("backend-1|model-B should still be present")
	}
	if !found2A {
		t.Error("backend-2|model-A should still be present")
	}
}

// TestLoadBackoff_Snapshot_ReflectsBreakerState — R60.11: Snapshot
// includes the breaker state correctly.
func TestLoadBackoff_Snapshot_ReflectsBreakerState(t *testing.T) {
	lb := newLoadBackoff()
	lb.recordFailure("backend-1", "model-A", "err1")
	lb.recordFailure("backend-1", "model-A", "err2")
	lb.recordFailure("backend-1", "model-A", "err3") // opens breaker

	snap := lb.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 state, got %d", len(snap))
	}
	if snap[0].Failures != 3 {
		t.Errorf("Failures = %d, want 3", snap[0].Failures)
	}
	if snap[0].BreakerOpenUntil == "" {
		t.Error("BreakerOpenUntil should be set after 3 failures")
	}
	// BreakerOpenUntil should be ~60s in the future.
	until, err := time.Parse(time.RFC3339, snap[0].BreakerOpenUntil)
	if err != nil {
		t.Fatalf("parse BreakerOpenUntil: %v", err)
	}
	now := time.Now()
	delta := until.Sub(now)
	if delta < 30*time.Second || delta > 90*time.Second {
		t.Errorf("BreakerOpenUntil should be ~60s in future, got %v", delta)
	}
}
