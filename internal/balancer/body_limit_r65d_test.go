//go:build llama_stub

// body_limit_r65d_test.go — R65d (2026-09-20): ограничение размера тела запроса
// на стороне балансера.
//
// Зачем: cppworker ограничивает тело 4 MB (types.MaxStreamingBodyBytes) и на
// превышение отвечает 413. Балансер же читал тело неограниченно и часто дважды
// (bodyBuf + translatedBody), поэтому запрос с большой историей и base64-картинками
// сначала съедал память балансера, и только потом получал 413 от upstream.
package balancer

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestR65d_MaxRequestBodyBytes_EnvOverride — лимит читается из ENV,
// 0 отключает ограничение.
func TestR65d_MaxRequestBodyBytes_EnvOverride(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int64
	}{
		{"default", "", int64(shadowDefaultMaxBodyMB) << 20},
		{"explicit 8 MB", "8", int64(8) << 20},
		{"zero disables", "0", 0},
		{"negative disables", "-5", 0},
		{"invalid falls back to default", "abc", int64(shadowDefaultMaxBodyMB) << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LB_MAX_REQUEST_BODY_MB", tc.env)
			if got := maxRequestBodyBytes(); got != tc.want {
				t.Errorf("maxRequestBodyBytes() = %d, want %d (env=%q)", got, tc.want, tc.env)
			}
		})
	}
}

// TestR65d_ReadRequestBodyLimited_UnderLimit — тело меньше лимита читается целиком.
func TestR65d_ReadRequestBodyLimited_UnderLimit(t *testing.T) {
	t.Setenv("LB_MAX_REQUEST_BODY_MB", "1") // 1 MB
	payload := bytes.Repeat([]byte("a"), 1024)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(payload))

	got, err := readRequestBodyLimited(req)
	if err != nil {
		t.Fatalf("readRequestBodyLimited returned error: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("read %d bytes, want %d", len(got), len(payload))
	}
}

// TestR65d_ReadRequestBodyLimited_OverLimit — тело больше лимита отвергается
// БЕЗ полной буферизации.
func TestR65d_ReadRequestBodyLimited_OverLimit(t *testing.T) {
	t.Setenv("LB_MAX_REQUEST_BODY_MB", "1") // 1 MB
	payload := bytes.Repeat([]byte("a"), (1<<20)+1)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(payload))

	got, err := readRequestBodyLimited(req)
	if err == nil {
		t.Fatalf("expected error for oversized body, got %d bytes", len(got))
	}
	if !strings.Contains(err.Error(), "exceeds configured limit") {
		t.Errorf("error = %v, want errBodyTooLarge", err)
	}
}

// TestR65d_ReadRequestBodyLimited_ExactlyAtLimit — тело ровно в лимит проходит
// (граничное условие off-by-one).
func TestR65d_ReadRequestBodyLimited_ExactlyAtLimit(t *testing.T) {
	t.Setenv("LB_MAX_REQUEST_BODY_MB", "1")
	payload := bytes.Repeat([]byte("a"), 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(payload))

	got, err := readRequestBodyLimited(req)
	if err != nil {
		t.Fatalf("body exactly at limit must be accepted, got error: %v", err)
	}
	if len(got) != 1<<20 {
		t.Errorf("read %d bytes, want %d", len(got), 1<<20)
	}
}

// TestR65d_ReadRequestBodyLimited_Disabled — лимит 0 = без ограничения.
func TestR65d_ReadRequestBodyLimited_Disabled(t *testing.T) {
	t.Setenv("LB_MAX_REQUEST_BODY_MB", "0")
	payload := bytes.Repeat([]byte("a"), 3<<20) // 3 MB, больше дефолтного cppworker-лимита
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(payload))

	got, err := readRequestBodyLimited(req)
	if err != nil {
		t.Fatalf("disabled limit must not error: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("read %d bytes, want %d", len(got), len(payload))
	}
}

// TestR65d_SendBodyTooLarge_OllamaShape — Ollama-клиент получает
// {"error": "..."}, а не OpenAI-структуру.
func TestR65d_SendBodyTooLarge_OllamaShape(t *testing.T) {
	t.Setenv("LB_MAX_REQUEST_BODY_MB", "8")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)

	sendBodyTooLarge(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if _, isString := body["error"].(string); !isString {
		t.Errorf("Ollama error must be a string, got %T: %v", body["error"], body["error"])
	}
	if body["done"] != true {
		t.Errorf("Ollama error response must set done=true, got %v", body["done"])
	}
	if !strings.Contains(body["error"].(string), "8 MB") {
		t.Errorf("error message should mention the limit, got %q", body["error"])
	}
}

// TestR65d_SendBodyTooLarge_OpenAIShape — OpenAI-клиент получает
// {"error": {"message": ...}} по спеке.
func TestR65d_SendBodyTooLarge_OpenAIShape(t *testing.T) {
	t.Setenv("LB_MAX_REQUEST_BODY_MB", "8")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	sendBodyTooLarge(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("OpenAI error must be an object, got %T: %v", body["error"], body["error"])
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Errorf("OpenAI error object must have a message string: %v", errObj)
	}
}
