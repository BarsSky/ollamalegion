// Package observability provides cross-cutting infrastructure for
// diagnostics: request ID propagation, structured logging context,
// slow request detection, and failure capture.
//
// Round 36 Phase 4 (2026-08-17): X-Request-Id propagation across
// the entire balancer ↔ cppworker chain. Per the contract
// (docs/BALANCER_CPPWORKER_API_CONTRACT.md Section 7), every request
// MUST be traceable end-to-end with a single ID.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// HeaderRequestID is the canonical request ID header. The contract
// (Section 7) requires this exact name.
const HeaderRequestID = "X-Request-Id"

// HeaderRequestIDLower is the lowercase variant some clients use.
const HeaderRequestIDLower = "x-request-id"

// requestIDLength is the number of random bytes in a generated ID.
// 8 bytes = 16 hex chars = 64 bits of entropy (collision-safe for
// reasonable request rates).
const requestIDLength = 8

// contextKey is an unexported type to prevent context key collisions
// with other packages. This is the standard Go pattern.
type contextKey int

const (
	requestIDKey contextKey = iota
	startTimeKey
)

// WithRequestID returns a context that carries the given request ID.
// If id is empty, a new random ID is generated.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		id = NewRequestID()
	}
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request ID stored in ctx, or
// empty string if no ID is set.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// WithStartTime records the time at which a request started, for
// latency calculations. Use in conjunction with ElapsedSinceStart.
func WithStartTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, startTimeKey, t)
}

// StartTimeFromContext returns the start time stored in ctx, or
// the zero time if not set.
func StartTimeFromContext(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	if v, ok := ctx.Value(startTimeKey).(time.Time); ok {
		return v
	}
	return time.Time{}
}

// NewRequestID generates a new random request ID. The format is
// "req-<16 hex chars>" — human-readable prefix for log greppability.
func NewRequestID() string {
	b := make([]byte, requestIDLength)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail, but if it does, fall
		// back to a timestamp-based ID (still unique enough
		// for log correlation).
		return "req-ts-" + time.Now().Format("20060102T150405.000000000")
	}
	return "req-" + hex.EncodeToString(b)
}

// RequestIDFromHeader extracts the request ID from a request's
// headers, checking both the canonical and lowercase forms. Returns
// empty string if no X-Request-Id header is present.
func RequestIDFromHeader(r *http.Request) string {
	if r == nil {
		return ""
	}
	if id := r.Header.Get(HeaderRequestID); id != "" {
		return id
	}
	if id := r.Header.Get(HeaderRequestIDLower); id != "" {
		return id
	}
	return ""
}

// RequestIDMiddleware wraps an http.Handler to:
//  1. Extract X-Request-Id from incoming request (or generate one)
//  2. Store it in the request context
//  3. Set the start time in the request context
//  4. Echo the X-Request-Id header in the response
//
// This MUST be the first middleware in the chain for any handler
// that needs request tracing. The contract (Section 7) requires
// every response to include the X-Request-Id header so the client
// can correlate logs.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromHeader(r)
		ctx := r.Context()
		ctx = WithRequestID(ctx, id)
		ctx = WithStartTime(ctx, time.Now())
		// Echo the ID in the response header so the client sees it.
		w.Header().Set(HeaderRequestID, RequestIDFromContext(ctx))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// PropagateRequestID is a convenience for forwarding a request ID
// from the balancer to the cppworker backend. Returns the header
// value to set, or empty string if no ID is in the context.
func PropagateRequestID(ctx context.Context) string {
	id := RequestIDFromContext(ctx)
	if id == "" {
		return ""
	}
	return id
}

// ElapsedSinceStart returns the time elapsed since the request
// start time in ctx. Returns 0 if no start time is set.
func ElapsedSinceStart(ctx context.Context) time.Duration {
	start := StartTimeFromContext(ctx)
	if start.IsZero() {
		return 0
	}
	return time.Since(start)
}

// =====================================================================
// Slow request detection
// =====================================================================

// SlowRequestThreshold is the default duration after which a request
// is considered "slow" and should be logged with full context.
// Configurable via LB_SLOW_REQUEST_THRESHOLD_SEC env var.
const DefaultSlowRequestThreshold = 5 * time.Second

// SlowRequestCallback is invoked when a request exceeds the slow
// threshold. It receives the request ID, model name, and elapsed time.
// The callback should be NON-BLOCKING (e.g. log and return).
type SlowRequestCallback func(requestID, model string, elapsed time.Duration)

// slowRequestNotifier is the package-level callback. Set via
// SetSlowRequestCallback. Defaults to a no-op so handlers can be
// tested without a global state dependency.
var (
	slowRequestNotifier   SlowRequestCallback = func(string, string, time.Duration) {}
	slowRequestNotifierMu sync.RWMutex
)

// SetSlowRequestCallback sets the global callback for slow-request
// notifications. Pass nil to disable. The callback is invoked from
// a goroutine so it must be safe to call concurrently.
func SetSlowRequestCallback(cb SlowRequestCallback) {
	slowRequestNotifierMu.Lock()
	defer slowRequestNotifierMu.Unlock()
	if cb == nil {
		cb = func(string, string, time.Duration) {}
	}
	slowRequestNotifier = cb
}

// NotifySlowRequest fires the slow-request callback if elapsed
// exceeds the threshold. Safe to call from any goroutine.
func NotifySlowRequest(ctx context.Context, model string, threshold time.Duration) {
	if threshold <= 0 {
		threshold = DefaultSlowRequestThreshold
	}
	elapsed := ElapsedSinceStart(ctx)
	if elapsed < threshold {
		return
	}
	slowRequestNotifierMu.RLock()
	cb := slowRequestNotifier
	slowRequestNotifierMu.RUnlock()
	if cb != nil {
		go cb(RequestIDFromContext(ctx), model, elapsed)
	}
}
