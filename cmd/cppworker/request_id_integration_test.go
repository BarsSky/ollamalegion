// Round 36 Phase 4: integration tests for X-Request-Id propagation
// through the cppworker HTTP stack.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/observability"
)

// TestRequestID_EchoedInResponse tests that the response includes
// the X-Request-Id header (so the client can correlate logs).
func TestRequestID_EchoedInResponse(t *testing.T) {
	// Build a minimal handler chain with RequestIDMiddleware.
	handler := observability.RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/test", nil)
	r.Header.Set(observability.HeaderRequestID, "req-echo-test-12345")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	got := w.Header().Get(observability.HeaderRequestID)
	if got != "req-echo-test-12345" {
		t.Errorf("response X-Request-Id: got %q, want req-echo-test-12345", got)
	}
}

// TestRequestID_GeneratedWhenMissing tests that the middleware
// generates a new ID when the client didn't send one. This is
// important for debugging requests from clients that don't support
// X-Request-Id (e.g. plain curl without -H).
func TestRequestID_GeneratedWhenMissing(t *testing.T) {
	var seenID string
	handler := observability.RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = observability.RequestIDFromContext(r.Context())
	}))

	r := httptest.NewRequest("GET", "/api/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if seenID == "" {
		t.Error("middleware should generate ID when client doesn't send one")
	}
	if !strings.HasPrefix(seenID, "req-") {
		t.Errorf("generated ID should have req- prefix, got %q", seenID)
	}
	// Response should echo the generated ID.
	if w.Header().Get(observability.HeaderRequestID) != seenID {
		t.Errorf("response header mismatch: context=%q, response=%q",
			seenID, w.Header().Get(observability.HeaderRequestID))
	}
}

// TestRequestID_BothHeaderCasesAccepted tests that both the
// canonical "X-Request-Id" and lowercase "x-request-id" headers
// are accepted (some clients use lowercase).
func TestRequestID_BothHeaderCasesAccepted(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{"canonical", observability.HeaderRequestID},
		{"lowercase", observability.HeaderRequestIDLower},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seenID string
			handler := observability.RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenID = observability.RequestIDFromContext(r.Context())
			}))

			r := httptest.NewRequest("GET", "/api/test", nil)
			r.Header.Set(tc.header, "req-canonical-test")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			if seenID != "req-canonical-test" {
				t.Errorf("got %q, want req-canonical-test", seenID)
			}
		})
	}
}

// TestRequestID_SameRequestReturnsSameID tests that all calls to
// RequestIDFromContext within the same request return the same ID.
// (No regeneration mid-request.)
func TestRequestID_SameRequestReturnsSameID(t *testing.T) {
	const wantID = "req-stable-test"
	ids := make([]string, 5)
	handler := observability.RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := range ids {
			ids[i] = observability.RequestIDFromContext(r.Context())
		}
	}))

	r := httptest.NewRequest("GET", "/api/test", nil)
	r.Header.Set(observability.HeaderRequestID, wantID)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	for i, id := range ids {
		if id != wantID {
			t.Errorf("call %d: got %q, want %q", i, id, wantID)
		}
	}
}
