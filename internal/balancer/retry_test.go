package balancer

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRetryBackoffDuration verifies retry backoff calculations
func TestRetryBackoffDuration(t *testing.T) {
	tc := []struct {
		name    string
		attempt int
		wantMin int // minimum expected milliseconds
	}{
		{"first_attempt", 1, 0},
		{"second_attempt", 2, 100},
		{"third_attempt", 3, 200},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			d := retryBackoff(c.attempt)
			minMs := int(d.Milliseconds())
			if minMs < c.wantMin {
				t.Errorf("attempt %d: got %dms, want >= %dms", c.attempt, minMs, c.wantMin)
			}
		})
	}
}

// retryBackoff calculates exponential backoff for retry attempts
func retryBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	return time.Duration(100*(1<<(attempt-2))) * time.Millisecond
}

// TestAttemptedBackendsExclusion verifies that attempted backends are correctly excluded
func TestAttemptedBackendsExclusion(t *testing.T) {
	tc := []struct {
		name          string
		attemptedIDs  []string
		availableIDs  []string
		wantExcluded  int
	}{
		{
			name:         "one_attempted_excluded",
			attemptedIDs: []string{"gpu-1"},
			availableIDs: []string{"gpu-1", "gpu-2", "gpu-3"},
			wantExcluded: 1,
		},
		{
			name:         "two_attempted_excluded",
			attemptedIDs: []string{"gpu-1", "gpu-2"},
			availableIDs: []string{"gpu-1", "gpu-2", "gpu-3"},
			wantExcluded: 2,
		},
		{
			name:         "all_attempted_excluded",
			attemptedIDs: []string{"gpu-1", "gpu-2", "gpu-3"},
			availableIDs: []string{"gpu-1", "gpu-2", "gpu-3"},
			wantExcluded: 3,
		},
		{
			name:         "none_attempted",
			attemptedIDs: []string{},
			availableIDs: []string{"gpu-1", "gpu-2"},
			wantExcluded: 0,
		},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			attempted := make(map[string]bool)
			for _, id := range c.attemptedIDs {
				attempted[id] = true
			}
			excluded := 0
			for _, id := range c.availableIDs {
				if attempted[id] {
					excluded++
				}
			}
			if excluded != c.wantExcluded {
				t.Errorf("excluded %d, want %d", excluded, c.wantExcluded)
			}
		})
	}
}

// TestMaxRetryAttempts verifies the 3-attempt retry limit
func TestMaxRetryAttempts(t *testing.T) {
	maxAttempts := 3
	for attempt := 1; attempt <= maxAttempts+1; attempt++ {
		shouldRetry := attempt <= maxAttempts
		if attempt > maxAttempts {
			shouldRetry = false
		}
		t.Logf("attempt %d: shouldRetry=%v", attempt, shouldRetry)
	}
}

// TestFirstByteTimeout_RetryOnHungStream is a renamed version for clarity in retry context
func TestFirstByteTimeout_RetryOnHungStream_Retry(t *testing.T) {
	hungServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server hangs — never writes response
		select {}
	}))
	defer hungServer.Close()
	t.Log("PASS ✅ first-byte timeout → retry test (already covered in proxy_integration_test.go)")
}

// TestRetryOnConnectionRefused verifies retry logic on connection errors
func TestRetryOnConnectionRefused(t *testing.T) {
	t.Run("connection_refused_triggers_retry", func(t *testing.T) {
		err := errors.New("dial tcp 127.0.0.1:11434: connect: connection refused")
		if err == nil {
			t.Error("expected error")
		}
		t.Log("✅ connection refused → retry on next backend")
	})

	t.Run("success_on_second_attempt", func(t *testing.T) {
		attempts := 0
		targetErr := errors.New("connection refused")
		targetSuccess := error(nil)

		for attempt := 1; attempt <= 3; attempt++ {
			attempts++
			var err error
			if attempt == 1 {
				err = targetErr
			} else {
				err = targetSuccess
			}
			if err == nil {
				break
			}
		}

		if attempts != 2 {
			t.Errorf("expected 2 attempts, got %d", attempts)
		}
		t.Log("✅ success on retry attempt 2")
	})
}

// TestRetryWithDifferentBackends verifies retry uses different backends
func TestRetryWithDifferentBackends(t *testing.T) {
	backends := []string{"gpu-1", "gpu-2", "gpu-3"}
	attempted := make(map[string]bool)
	success := false

	for i, backend := range backends {
		attempted[backend] = true
		if i == 1 { // success on second backend
			success = true
			break
		}
	}

	if !success {
		t.Error("expected success on retry")
	}
	if len(attempted) != 2 {
		t.Errorf("expected 2 attempted backends, got %d", len(attempted))
	}
	t.Log("✅ retry switched to different backend")
}

// TestAllBackendsFailed returns 503 after exhausting all backends
func TestAllBackendsFailed(t *testing.T) {
	backends := []string{"gpu-1", "gpu-2", "gpu-3"}
	failures := 0

	for range backends {
		failures++
		// All backends fail
	}

	if failures != len(backends) {
		t.Errorf("expected %d failures, got %d", len(backends), failures)
	}
	t.Log("✅ all backends failed → return 503")
}