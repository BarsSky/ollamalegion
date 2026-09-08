//go:build llama_stub

// json_bom_strip_r60_16_test.go — R60.16 (2026-09-08) tests for the
// BOM-stripping JSON request helper (decodeJSONRequest).
//
// R60.16 fix: the R60.15 Fix B helper caused production hangs due to
// r.Body wrapping. The R60.16 helper reads the body once into a
// byte slice, strips the BOM in-place, and replaces r.Body with a
// stateless io.NopCloser. This test verifies the fix works AND
// does not exhibit the hang behavior.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeJSONRequest_NoBOM — body without BOM decodes normally.
// Regression test: ensure the helper doesn't break the common case.
func TestDecodeJSONRequest_NoBOM(t *testing.T) {
	body := `{"name":"qwen3","n_ctx":32768}`
	req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(body))

	var got struct {
		Name  string `json:"name"`
		NCtx  int    `json:"n_ctx"`
	}
	if err := decodeJSONRequest(req, &got, 0); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if got.Name != "qwen3" {
		t.Errorf("name = %q, want qwen3", got.Name)
	}
	if got.NCtx != 32768 {
		t.Errorf("n_ctx = %d, want 32768", got.NCtx)
	}
}

// TestDecodeJSONRequest_WithBOM — body WITH UTF-8 BOM decodes correctly.
// Pre-R60.16: json.NewDecoder would fail with "invalid character ï".
// R60.16: BOM is stripped, body decodes as expected.
func TestDecodeJSONRequest_WithBOM(t *testing.T) {
	bomBody := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"name":"qwen3","n_ctx":8192}`)...)
	req := httptest.NewRequest(http.MethodPost, "/api/x", bytes.NewReader(bomBody))

	var got struct {
		Name string `json:"name"`
		NCtx int    `json:"n_ctx"`
	}
	if err := decodeJSONRequest(req, &got, 0); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if got.Name != "qwen3" {
		t.Errorf("name = %q, want qwen3 (BOM should be stripped)", got.Name)
	}
	if got.NCtx != 8192 {
		t.Errorf("n_ctx = %d, want 8192 (BOM should be stripped)", got.NCtx)
	}
}

// TestDecodeJSONRequest_ReplacesBody — after the call, r.Body should
// hold the cleaned (BOM-stripped) body. Verifies the API contract:
// callers (and any code after) can re-read r.Body safely.
func TestDecodeJSONRequest_ReplacesBody(t *testing.T) {
	bomBody := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"x":1}`)...)
	req := httptest.NewRequest(http.MethodPost, "/api/x", bytes.NewReader(bomBody))

	var got struct {
		X int `json:"x"`
	}
	if err := decodeJSONRequest(req, &got, 0); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	// Re-read r.Body — should give the cleaned body, not raw body.
	reRead, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("re-read body: %v", err)
	}
	if bytes.HasPrefix(reRead, []byte{0xEF, 0xBB, 0xBF}) {
		t.Errorf("r.Body still has BOM after decode: %x", reRead[:3])
	}
	var again struct {
		X int `json:"x"`
	}
	if err := json.Unmarshal(reRead, &again); err != nil {
		t.Errorf("r.Body not valid JSON after replace: %v. body = %s", err, reRead)
	}
	if again.X != 1 {
		t.Errorf("re-decoded x = %d, want 1", again.X)
	}
}

// TestDecodeJSONRequest_InvalidJSON — malformed JSON returns error
// (not panic, not empty result). Regression test for the new code path.
func TestDecodeJSONRequest_InvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/x",
		strings.NewReader("not json {{{"))

	var got map[string]interface{}
	err := decodeJSONRequest(req, &got, 0)
	if err == nil {
		t.Errorf("expected error for invalid JSON, got nil")
	}
}

// TestDecodeJSONRequest_OversizeBody — body larger than maxBodySize
// returns an error. Prevents OOM from huge uploads.
func TestDecodeJSONRequest_OversizeBody(t *testing.T) {
	// 1 KB body, 100 B limit.
	body := strings.Repeat("a", 1024) // not valid JSON, but exceeds size before parse
	body = `{"x":"` + body + `"}`      // 1 KB + wrapper
	req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(body))

	var got map[string]interface{}
	err := decodeJSONRequest(req, &got, 100)
	if err == nil {
		t.Errorf("expected error for oversize body, got nil")
	}
}

// TestDecodeJSONRequest_EmptyBody — empty body returns EOF.
func TestDecodeJSONRequest_EmptyBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(""))
	var got map[string]interface{}
	err := decodeJSONRequest(req, &got, 0)
	if err == nil {
		t.Errorf("expected EOF for empty body, got nil")
	}
}
