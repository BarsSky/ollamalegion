// Round 36 Phase 4: tests for X-Request-Id propagation.
package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewRequestID_Format(t *testing.T) {
	id := NewRequestID()
	if !strings.HasPrefix(id, "req-") {
		t.Errorf("ID should start with 'req-', got %q", id)
	}
	// Length: "req-" (4) + 16 hex chars = 20
	if len(id) != 20 {
		t.Errorf("ID length: got %d, want 20", len(id))
	}
}

func TestNewRequestID_Unique(t *testing.T) {
	const n = 1000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := NewRequestID()
		if seen[id] {
			t.Errorf("duplicate ID at iteration %d: %s", i, id)
		}
		seen[id] = true
	}
}

func TestWithRequestID_EmptyID_GeneratesNew(t *testing.T) {
	ctx := WithRequestID(context.Background(), "")
	id := RequestIDFromContext(ctx)
	if id == "" {
		t.Error("empty input should generate a new ID")
	}
	if !strings.HasPrefix(id, "req-") {
		t.Errorf("generated ID should have req- prefix, got %q", id)
	}
}

func TestWithRequestID_PreservesID(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req-deadbeefcafebabe")
	id := RequestIDFromContext(ctx)
	if id != "req-deadbeefcafebabe" {
		t.Errorf("ID: got %q, want req-deadbeefcafebabe", id)
	}
}

func TestRequestIDFromContext_Empty(t *testing.T) {
	id := RequestIDFromContext(context.Background())
	if id != "" {
		t.Errorf("expected empty, got %q", id)
	}
}

func TestRequestIDFromContext_NilContext(t *testing.T) {
	id := RequestIDFromContext(nil) //nolint:staticcheck
	if id != "" {
		t.Errorf("nil context should return empty, got %q", id)
	}
}

func TestRequestIDFromHeader(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
		want   string
	}{
		{"canonical", HeaderRequestID, "req-abc123", "req-abc123"},
		{"lowercase", HeaderRequestIDLower, "req-xyz789", "req-xyz789"},
		{"empty", HeaderRequestID, "", ""},
		{"no header", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.header != "" && tc.value != "" {
				r.Header.Set(tc.header, tc.value)
			}
			got := RequestIDFromHeader(r)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRequestIDMiddleware_SetsContextAndHeader(t *testing.T) {
	// Build a chain: RequestIDMiddleware → handler that reads context.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromContext(r.Context())
		if id != "req-test1234" {
			t.Errorf("context ID: got %q, want req-test1234", id)
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := RequestIDMiddleware(inner)

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(HeaderRequestID, "req-test1234")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	// Response should echo the request ID.
	if got := w.Header().Get(HeaderRequestID); got != "req-test1234" {
		t.Errorf("response header: got %q, want req-test1234", got)
	}
}

func TestRequestIDMiddleware_GeneratesIDWhenMissing(t *testing.T) {
	var seenID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = RequestIDFromContext(r.Context())
	})
	handler := RequestIDMiddleware(inner)

	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if seenID == "" {
		t.Error("middleware should generate ID when none provided")
	}
	if !strings.HasPrefix(seenID, "req-") {
		t.Errorf("generated ID should have prefix, got %q", seenID)
	}
	if w.Header().Get(HeaderRequestID) != seenID {
		t.Errorf("response header should match context ID")
	}
}

func TestRequestIDMiddleware_RecordsStartTime(t *testing.T) {
	var startTime time.Time
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime = StartTimeFromContext(r.Context())
	})
	handler := RequestIDMiddleware(inner)

	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if startTime.IsZero() {
		t.Error("start time should be set by middleware")
	}
	if time.Since(startTime) > time.Second {
		t.Errorf("start time should be recent, got %v", startTime)
	}
}

func TestElapsedSinceStart(t *testing.T) {
	ctx := WithStartTime(context.Background(), time.Now().Add(-2*time.Second))
	elapsed := ElapsedSinceStart(ctx)
	if elapsed < 1500*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("elapsed: got %v, want ~2s", elapsed)
	}
}

func TestElapsedSinceStart_NoStartTime(t *testing.T) {
	elapsed := ElapsedSinceStart(context.Background())
	if elapsed != 0 {
		t.Errorf("expected 0, got %v", elapsed)
	}
}

func TestPropagateRequestID(t *testing.T) {
	// With ID
	ctx := WithRequestID(context.Background(), "req-prop123")
	if got := PropagateRequestID(ctx); got != "req-prop123" {
		t.Errorf("got %q, want req-prop123", got)
	}
	// Without ID
	if got := PropagateRequestID(context.Background()); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestNotifySlowRequest_TriggersAboveThreshold(t *testing.T) {
	var (
		mu          sync.Mutex
		capturedID  string
		capturedMod string
		capturedEla time.Duration
		called      bool
	)
	SetSlowRequestCallback(func(id, model string, elapsed time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		capturedID = id
		capturedMod = model
		capturedEla = elapsed
		called = true
	})
	t.Cleanup(func() {
		SetSlowRequestCallback(nil)
	})

	// Simulate a request that took 100ms (well above 50ms threshold)
	ctx := WithRequestID(context.Background(), "req-slowtest")
	ctx = WithStartTime(ctx, time.Now().Add(-100*time.Millisecond))
	NotifySlowRequest(ctx, "gemma-4", 50*time.Millisecond)

	// Callback fires in a goroutine; wait briefly. Use a channel
	// to avoid deadlock from accidentally holding the mutex.
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			done := called
			mu.Unlock()
			if done {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatal("callback should have been called for slow request")
	}
	if capturedID != "req-slowtest" {
		t.Errorf("ID: got %q", capturedID)
	}
	if capturedMod != "gemma-4" {
		t.Errorf("model: got %q", capturedMod)
	}
	if capturedEla < 50*time.Millisecond {
		t.Errorf("elapsed: got %v, want >= 50ms", capturedEla)
	}
}

func TestNotifySlowRequest_NotTriggeredBelowThreshold(t *testing.T) {
	called := false
	SetSlowRequestCallback(func(string, string, time.Duration) {
		called = true
	})
	t.Cleanup(func() { SetSlowRequestCallback(nil) })

	// Simulate a fast request (5ms, below 1s threshold)
	ctx := WithStartTime(context.Background(), time.Now().Add(-5*time.Millisecond))
	NotifySlowRequest(ctx, "gemma-4", time.Second)

	// Wait briefly to ensure the callback would have fired if scheduled.
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("callback should NOT have been called for fast request")
	}
}

func TestNotifySlowRequest_DefaultThreshold(t *testing.T) {
	// When threshold is 0, use the default (5 seconds).
	called := false
	SetSlowRequestCallback(func(string, string, time.Duration) {
		called = true
	})
	t.Cleanup(func() { SetSlowRequestCallback(nil) })

	// Simulate a 100ms request (below 5s default).
	ctx := WithStartTime(context.Background(), time.Now().Add(-100*time.Millisecond))
	NotifySlowRequest(ctx, "gemma-4", 0)
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("callback should NOT fire when elapsed < default threshold")
	}
}

func TestSetSlowRequestCallback_NilDisables(t *testing.T) {
	called := false
	SetSlowRequestCallback(func(string, string, time.Duration) {
		called = true
	})
	SetSlowRequestCallback(nil) // disable

	ctx := WithStartTime(context.Background(), time.Now().Add(-1*time.Second))
	NotifySlowRequest(ctx, "model", 100*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("disabled callback should not fire")
	}
}
