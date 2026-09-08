// proxy_helpers_r60_15_test.go — R60.15 (2026-09-07): tests for
// X-Request-Id duplicate header fix in copyResponse.
//
// Pre-R60.15: balancer's ServeHTTP (proxy.go:524) sets its own
// X-Request-Id. Then copyResponse does `w.Header().Add(key, value)`
// for upstream's X-Request-Id → result is TWO X-Request-Id headers
// in the response (one balancer, one upstream).
//
// R60.15: copyResponse strips upstream X-Request-Id and re-adds as
// X-Upstream-Request-Id (only if not already set). Result: clean
// single X-Request-Id + X-Upstream-Request-Id pair for tracing.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCopyResponse_NoDuplicateXRequestID(t *testing.T) {
	// Upstream response with X-Request-Id
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "upstream-req-abc123")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET upstream: %v", err)
	}

	// Simulate balancer pre-setting its own X-Request-Id
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "balancer-req-xyz789")

	copyResponse(rec, resp)

	// R60.15 assertion: only ONE X-Request-Id, value = balancer's
	rid := rec.Header().Get("X-Request-Id")
	if rid != "balancer-req-xyz789" {
		t.Errorf("X-Request-Id: got %q, want balancer-req-xyz789 (no overwrite)", rid)
	}
	ridAll := rec.Header().Values("X-Request-Id")
	if len(ridAll) != 1 {
		t.Errorf("X-Request-Id should appear exactly once, got %d: %v", len(ridAll), ridAll)
	}

	// R60.15 assertion: X-Upstream-Request-Id = upstream's
	upRid := rec.Header().Get("X-Upstream-Request-Id")
	if upRid != "upstream-req-abc123" {
		t.Errorf("X-Upstream-Request-Id: got %q, want upstream-req-abc123", upRid)
	}

	// Other headers should still be copied
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json prefix", ct)
	}
}

func TestCopyResponse_NoXRequestID_StillWorks(t *testing.T) {
	// Upstream WITHOUT X-Request-Id header (some backends don't set it)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET upstream: %v", err)
	}

	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "balancer-req-foo")

	copyResponse(rec, resp)

	// Should keep balancer's X-Request-Id
	if got := rec.Header().Get("X-Request-Id"); got != "balancer-req-foo" {
		t.Errorf("X-Request-Id: got %q, want balancer-req-foo", got)
	}
	// X-Upstream-Request-Id should NOT be set (no upstream value)
	if got := rec.Header().Get("X-Upstream-Request-Id"); got != "" {
		t.Errorf("X-Upstream-Request-Id: should be empty, got %q", got)
	}
}

func TestCopyResponse_MultipleXRequestID_OnlyFirstPreserved(t *testing.T) {
	// Upstream sends multiple X-Request-Id headers (rare but possible)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Request-Id", "upstream-req-first")
		w.Header().Add("X-Request-Id", "upstream-req-second")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET upstream: %v", err)
	}

	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "balancer-req-bar")

	copyResponse(rec, resp)

	// Only first upstream value preserved (per design)
	upRid := rec.Header().Get("X-Upstream-Request-Id")
	if upRid != "upstream-req-first" {
		t.Errorf("X-Upstream-Request-Id: got %q, want upstream-req-first (first wins)", upRid)
	}
}
