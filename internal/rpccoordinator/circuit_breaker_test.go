// circuit_breaker_test.go — Phase 8.3: tests for CircuitBreaker.
//
// 8 test cases per production plan §P.1 step 4:
//   1. TestCircuitBreaker_InitialState_Closed
//   2. TestCircuitBreaker_NFailures_Opens
//   3. TestCircuitBreaker_AfterTimeout_HalfOpen
//   4. TestCircuitBreaker_HalfOpen_Success_Closes
//   5. TestCircuitBreaker_HalfOpen_Failure_Reopens
//   6. TestCircuitBreaker_ConcurrentAllow_ThreadSafe
//   7. TestCircuitBreaker_OnStateChange_Fires
//   8. TestCircuitBreaker_CustomThresholds
package rpccoordinator

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCircuitBreaker_InitialState_Closed — fresh breaker starts in Closed.
func TestCircuitBreaker_InitialState_Closed(t *testing.T) {
	cb := NewCircuitBreaker(5, 30*time.Second)
	if state := cb.State(); state != StateClosed {
		t.Errorf("initial state = %v, want %v", state, StateClosed)
	}
	if !cb.Allow() {
		t.Error("initial Closed should allow requests")
	}
}

// TestCircuitBreaker_NFailures_Opens — N consecutive failures → Open.
func TestCircuitBreaker_NFailures_Opens(t *testing.T) {
	cb := NewCircuitBreaker(3, 30*time.Second)
	// 3 failures
	for i := 0; i < 3; i++ {
		if !cb.Allow() {
			t.Fatalf("Allow() should be true in Closed (iter %d)", i)
		}
		cb.RecordFailure()
	}
	// После 3 failures — Open.
	if state := cb.State(); state != StateOpen {
		t.Errorf("state after 3 failures = %v, want %v", state, StateOpen)
	}
	if cb.Allow() {
		t.Error("Allow() should be false in Open")
	}
	stats := cb.Stats()
	if stats.OpenSince.IsZero() {
		t.Error("OpenSince should be set after Open transition")
	}
}

// TestCircuitBreaker_AfterTimeout_HalfOpen — после resetTimeout → HalfOpen.
func TestCircuitBreaker_AfterTimeout_HalfOpen(t *testing.T) {
	cb := NewCircuitBreaker(2, 50*time.Millisecond) // short timeout для теста.
	// Trip breaker.
	cb.Allow()
	cb.RecordFailure()
	cb.Allow()
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Fatalf("expected Open, got %v", cb.State())
	}
	// Wait for resetTimeout.
	time.Sleep(60 * time.Millisecond)
	// Next State() call должен перевести в HalfOpen (lazy transition).
	if state := cb.State(); state != StateHalfOpen {
		t.Errorf("after timeout, state = %v, want %v", state, StateHalfOpen)
	}
	// Allow() в HalfOpen — true (первый in-flight probe).
	if !cb.Allow() {
		t.Error("Allow() should be true in HalfOpen (first probe)")
	}
	// Второй Allow() в HalfOpen — false (probe уже in-flight).
	if cb.Allow() {
		t.Error("Allow() should be false in HalfOpen (probe in-flight)")
	}
}

// TestCircuitBreaker_HalfOpen_Success_Closes — success в HalfOpen → Closed.
func TestCircuitBreaker_HalfOpen_Success_Closes(t *testing.T) {
	cb := NewCircuitBreaker(2, 30*time.Millisecond)
	cb.Allow()
	cb.RecordFailure()
	cb.Allow()
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Fatalf("expected Open, got %v", cb.State())
	}
	// Wait + trigger HalfOpen.
	time.Sleep(40 * time.Millisecond)
	if !cb.Allow() {
		t.Fatal("Allow() should be true in HalfOpen")
	}
	cb.RecordSuccess()
	// successThreshold=1 (default) → Closed.
	if state := cb.State(); state != StateClosed {
		t.Errorf("after HalfOpen success, state = %v, want %v", state, StateClosed)
	}
	// В Closed — failure counter сброшен.
	stats := cb.Stats()
	if stats.FailureCount != 0 {
		t.Errorf("FailureCount after recovery = %d, want 0", stats.FailureCount)
	}
}

// TestCircuitBreaker_HalfOpen_Failure_Reopens — failure в HalfOpen → Open.
func TestCircuitBreaker_HalfOpen_Failure_Reopens(t *testing.T) {
	cb := NewCircuitBreaker(2, 30*time.Millisecond)
	cb.Allow()
	cb.RecordFailure()
	cb.Allow()
	cb.RecordFailure()
	time.Sleep(40 * time.Millisecond)
	// HalfOpen probe fails.
	if !cb.Allow() {
		t.Fatal("Allow() should be true in HalfOpen")
	}
	cb.RecordFailure()
	// → Open (с обновлённым openSince).
	if state := cb.State(); state != StateOpen {
		t.Errorf("after HalfOpen failure, state = %v, want %v", state, StateOpen)
	}
	// OpenSince должен обновиться (не zero).
	stats := cb.Stats()
	if stats.OpenSince.IsZero() {
		t.Error("OpenSince should be set after re-Open")
	}
}

// TestCircuitBreaker_ConcurrentAllow_ThreadSafe — concurrent Allow() не
// приводит к race condition или неконсистентному state.
func TestCircuitBreaker_ConcurrentAllow_ThreadSafe(t *testing.T) {
	cb := NewCircuitBreaker(1000, 30*time.Second) // высокий threshold, breaker stays Closed.
	const goroutines = 100
	const perGoroutine = 1000

	var allowedCount int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if cb.Allow() {
					atomic.AddInt64(&allowedCount, 1)
				}
			}
		}()
	}
	wg.Wait()
	// Все должны быть allowed (Closed state, high threshold).
	expected := int64(goroutines * perGoroutine)
	if allowedCount != expected {
		t.Errorf("allowedCount = %d, want %d (race or state corruption)", allowedCount, expected)
	}
}

// TestCircuitBreaker_OnStateChange_Fires — callback при смене state.
func TestCircuitBreaker_OnStateChange_Fires(t *testing.T) {
	var transitions []struct{ From, To CBState }
	var mu sync.Mutex
	done := make(chan struct{})

	cb := NewCircuitBreakerWithConfig(CircuitBreakerConfig{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     10 * time.Millisecond,
		OnStateChange: func(from, to CBState) {
			mu.Lock()
			transitions = append(transitions, struct{ From, To CBState }{from, to})
			mu.Unlock()
			if len(transitions) >= 3 {
				select {
				case <-done:
				default:
					close(done)
				}
			}
		},
	})

	// Closed → Open
	cb.Allow()
	cb.RecordFailure()

	// Wait → HalfOpen
	time.Sleep(15 * time.Millisecond)
	cb.State() // trigger advance

	// HalfOpen → Closed (success)
	cb.Allow()
	cb.RecordSuccess()

	// Wait for callback (async goroutine)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("OnStateChange callback did not fire in time")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(transitions) < 3 {
		t.Errorf("got %d transitions, want exactly 3 (Closed->Open, Open->HalfOpen, HalfOpen->Closed): %+v",
			len(transitions), transitions)
	}
	if len(transitions) > 3 {
		t.Errorf("got %d transitions, want exactly 3 (extra transitions): %+v",
			len(transitions), transitions)
	}
	// Verify expected transitions (order may vary due to async).
	hasExpectedTransition := func(from, to CBState) bool {
		for _, tr := range transitions {
			if tr.From == from && tr.To == to {
				return true
			}
		}
		return false
	}
	if !hasExpectedTransition(StateClosed, StateOpen) {
		t.Errorf("missing Closed->Open transition: %+v", transitions)
	}
	if !hasExpectedTransition(StateOpen, StateHalfOpen) {
		t.Errorf("missing Open->HalfOpen transition: %+v", transitions)
	}
	if !hasExpectedTransition(StateHalfOpen, StateClosed) {
		t.Errorf("missing HalfOpen->Closed transition: %+v", transitions)
	}
}

// TestCircuitBreaker_CustomThresholds — кастомные threshold'ы работают.
func TestCircuitBreaker_CustomThresholds(t *testing.T) {
	// Custom: 10 failures threshold.
	cb := NewCircuitBreakerWithConfig(CircuitBreakerConfig{
		FailureThreshold: 10,
		SuccessThreshold: 3, // need 3 successes in HalfOpen
		ResetTimeout:     30 * time.Millisecond,
	})

	// 9 failures — still Closed.
	for i := 0; i < 9; i++ {
		if !cb.Allow() {
			t.Fatalf("Allow() at iter %d should be true (Closed)", i)
		}
		cb.RecordFailure()
	}
	if cb.State() != StateClosed {
		t.Errorf("after 9/10 failures, state = %v, want Closed", cb.State())
	}

	// 10-я failure — Open.
	cb.Allow()
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Errorf("after 10/10 failures, state = %v, want Open", cb.State())
	}

	// Wait → HalfOpen.
	time.Sleep(40 * time.Millisecond)
	cb.State() // trigger advance

	// 2 successes — still HalfOpen (successThreshold=3).
	cb.Allow()
	cb.RecordSuccess()
	if cb.State() != StateHalfOpen {
		t.Errorf("after 1/3 successes, state = %v, want HalfOpen", cb.State())
	}
	// Hmm — после первого success, halfOpenInFlight=0, можно Allow() снова.
	// Но фактически state остается HalfOpen пока successCount < 3.
	// (В реальном dispatcher Allow() дергается перед каждым request, не bulk.)
}
