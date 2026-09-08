// proxy_helpers_r60_16_test.go — R60.16 (2026-09-08) tests for
// writeServiceUnavailable helper.
//
// R60.16 fix: pre-R60.16, the auto-load error paths in
// llamacpp_handlers_inference.go returned 503 with NO Retry-After
// header, leaving clients (Cline/Roo/openai-python) to either
// busy-loop or give up. The fix introduces writeServiceUnavailable
// which always sets a Retry-After header (default 30s).
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteServiceUnavailable_Default — 0 retryAfterSec → uses default 30s.
func TestWriteServiceUnavailable_Default(t *testing.T) {
	rec := httptest.NewRecorder()
	writeServiceUnavailable(rec, "auto-load failed: model is loading", 0)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30 (default)", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v. body = %s", err, rec.Body.String())
	}
	if body["error"] == "" {
		t.Errorf("body.error = empty, want non-empty error message")
	}
}

// TestWriteServiceUnavailable_Custom — custom retryAfterSec → uses that value.
func TestWriteServiceUnavailable_Custom(t *testing.T) {
	rec := httptest.NewRecorder()
	writeServiceUnavailable(rec, "load in progress", 60)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
}

// TestWriteServiceUnavailable_NegativeDefaultsTo30 — negative retryAfterSec
// is treated as "use default". Defensive against accidental negatives.
func TestWriteServiceUnavailable_NegativeDefaultsTo30(t *testing.T) {
	rec := httptest.NewRecorder()
	writeServiceUnavailable(rec, "load failed", -1)

	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30 (negative treated as default)", got)
	}
}

// TestWriteServiceUnavailable_NoEmptyRetryAfter — KEY regression test.
// Pre-R60.16: 503 with empty Retry-After caused client busy-loops.
// R60.16: Retry-After is always set (never empty) on 503 from this helper.
func TestWriteServiceUnavailable_NoEmptyRetryAfter(t *testing.T) {
	for _, retrySec := range []int{0, 5, 30, 60, 120, -1} {
		rec := httptest.NewRecorder()
		writeServiceUnavailable(rec, "test", retrySec)
		if got := rec.Header().Get("Retry-After"); got == "" {
			t.Errorf("Retry-After empty for retrySec=%d (must always be set)", retrySec)
		}
	}
}
