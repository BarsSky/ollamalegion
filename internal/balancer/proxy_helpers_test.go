// proxy_helpers_test.go — unit tests for writeJSON and copyResponse.
//
// R51.1 (2026-08-20): created when helpers moved to proxy_helpers.go.
// Tests guard against future regressions of these two heavily-used
// (50+ call sites) functions.

package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProxyHelpers_WriteJSON_Basic — minimal happy path: simple struct → JSON.
func TestProxyHelpers_WriteJSON_Basic(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"hello": "world"})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v, body=%q", err, rec.Body.String())
	}
	if got["hello"] != "world" {
		t.Errorf("hello = %q, want world", got["hello"])
	}
}

// TestProxyHelpers_WriteJSON_StatusCodes — non-200 status codes are
// preserved. (Important: 4xx/5xx errors use writeJSON too.)
func TestProxyHelpers_WriteJSON_StatusCodes(t *testing.T) {
	for _, code := range []int{
		http.StatusBadRequest,
		http.StatusServiceUnavailable,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeJSON(rec, code, map[string]string{"error": "test"})
			if rec.Code != code {
				t.Errorf("status = %d, want %d", rec.Code, code)
			}
			if rec.Header().Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type missing for %d", code)
			}
		})
	}
}

// TestProxyHelpers_WriteJSON_NilData — nil data should serialize to "null\n"
// (encoding/json behavior). Important: not panic.
func TestProxyHelpers_WriteJSON_NilData(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "null" {
		t.Errorf("body = %q, want null", body)
	}
}

// TestProxyHelpers_WriteJSON_EmptyMap — empty map should serialize to "{}".
// (Not nil, not "null" — distinguishes from nil case.)
func TestProxyHelpers_WriteJSON_EmptyMap(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]interface{}{})
	body := strings.TrimSpace(rec.Body.String())
	if body != "{}" {
		t.Errorf("body = %q, want {}", body)
	}
}

// TestProxyHelpers_CopyResponse_Basic — upstream response is forwarded
// with headers + status + body.
func TestProxyHelpers_CopyResponse_Basic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Custom-Header", "value-42")
		w.WriteHeader(http.StatusTeapot) // 418 — distinctive code for test
		_, _ = w.Write([]byte("upstream body"))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	rec := httptest.NewRecorder()
	copyResponse(rec, resp)

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418 (Teapot)", rec.Code)
	}
	if rec.Header().Get("X-Custom-Header") != "value-42" {
		t.Errorf("X-Custom-Header not forwarded, got %q", rec.Header().Get("X-Custom-Header"))
	}
	if rec.Body.String() != "upstream body" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "upstream body")
	}
}

// TestProxyHelpers_CopyResponse_ClosesUpstreamBody — after copyResponse
// returns, the upstream Body MUST be closed (defer resp.Body.Close() in
// copyResponse). We verify by checking that we can't read from it anymore.
func TestProxyHelpers_CopyResponse_ClosesUpstreamBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("test body"))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}

	rec := httptest.NewRecorder()
	copyResponse(rec, resp)

	// After copyResponse, resp.Body is closed. Reading from it should fail.
	// (defer resp.Body.Close() in the test would also call Close again, which
	// should be safe — Go's http.Body.Close() is idempotent.)
	buf := make([]byte, 1)
	n, err := resp.Body.Read(buf)
	if n != 0 || err == nil {
		t.Errorf("upstream Body should be closed after copyResponse, got n=%d err=%v", n, err)
	}
	_ = resp.Body.Close() // safe — idempotent
}

// TestProxyHelpers_CopyResponse_PreservesMultipleHeaders — headers with
// multiple values (e.g. Set-Cookie) should all be forwarded.
func TestProxyHelpers_CopyResponse_PreservesMultipleHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	rec := httptest.NewRecorder()
	copyResponse(rec, resp)

	cookies := rec.Header().Values("Set-Cookie")
	if len(cookies) != 2 {
		t.Errorf("Set-Cookie values = %d, want 2", len(cookies))
	}
}
