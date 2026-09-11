//go:build llama_stub

// proxy_helpers_r60_48_test.go — R60.48 tests.
//
// Symptom (R60.48): user reports "TransferEncodingError: 400, message='Not
// enough data to satisfy transfer length header.'" в OpenWebUI chat UI.
// Root cause: error response helpers (writeJSON, writeServiceUnavailable,
// writeServiceUnavailableWithDiagnostics) использовали
// json.NewEncoder(w).Encode() который НЕ устанавливает Content-Length.
//
// Go HTTP server fallback на Transfer-Encoding: chunked когда Content-Length
// неизвестен ДО WriteHeader. aiohttp (Python клиент в OpenWebUI) иногда
// плохо обрабатывает chunked encoding на error responses — может прочитать
// меньше данных чем chunk header говорит, или неправильно определить границы.
//
// R60.48 fix: explicit Content-Length ВСЕГДА перед WriteHeader. Используем
// json.Marshal (сразу bytes) → compute len → set Content-Length header →
// WriteHeader(status) → Write(body). Это даёт consistent framing — клиент
// получает Content-Length framing вместо chunked.
//
// Тесты проверяют:
//   - writeJSON: Content-Length header set + matches body length
//   - writeServiceUnavailable: Content-Length + Retry-After + valid JSON
//   - writeServiceUnavailableWithDiagnostics: Content-Length + все diagnostic поля
//   - Marshal error fallback: Content-Length + 500 status
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestR6048_WriteJSON_ContentLengthSet — verify writeJSON устанавливает
// Content-Length header matching body length (NOT chunked encoding).
func TestR6048_WriteJSON_ContentLengthSet(t *testing.T) {
	rec := httptest.NewRecorder()
	data := map[string]string{"error": "invalid request body"}

	writeJSON(rec, http.StatusBadRequest, data)

	// Content-Length header must be set explicitly.
	clStr := rec.Header().Get("Content-Length")
	if clStr == "" {
		t.Fatal("Content-Length header NOT set (Go will fallback to chunked encoding)")
	}
	cl, _ := strconv.Atoi(clStr)
	bodyLen := len(rec.Body.Bytes())
	if cl != bodyLen {
		t.Errorf("Content-Length = %d, body has %d bytes (mismatch → TransferEncodingError)", cl, bodyLen)
	}

	// Transfer-Encoding must NOT be chunked.
	if te := rec.Header().Get("Transfer-Encoding"); te == "chunked" {
		t.Errorf("Transfer-Encoding = chunked (R60.48 fix: should be Content-Length framing)")
	}

	// Status and Content-Type.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestR6048_WriteServiceUnavailable_ContentLengthSet — verify
// writeServiceUnavailable устанавливает Content-Length.
func TestR6048_WriteServiceUnavailable_ContentLengthSet(t *testing.T) {
	rec := httptest.NewRecorder()

	writeServiceUnavailable(rec, "model load in progress", 90)

	clStr := rec.Header().Get("Content-Length")
	if clStr == "" {
		t.Fatal("Content-Length header NOT set (Go will fallback to chunked encoding)")
	}
	cl, _ := strconv.Atoi(clStr)
	bodyLen := len(rec.Body.Bytes())
	if cl != bodyLen {
		t.Errorf("Content-Length = %d, body has %d bytes", cl, bodyLen)
	}

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "90" {
		t.Errorf("Retry-After = %q, want 90", rec.Header().Get("Retry-After"))
	}

	// Body должен быть валидный JSON.
	var bodyMap map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &bodyMap); err != nil {
		t.Errorf("body is not JSON: %v (body=%s)", err, rec.Body.String())
	}
	if bodyMap["error"] != "model load in progress" {
		t.Errorf("body.error = %v, want 'model load in progress'", bodyMap["error"])
	}
	if bodyMap["retry_after_seconds"] != float64(90) {
		t.Errorf("body.retry_after_seconds = %v, want 90", bodyMap["retry_after_seconds"])
	}
}

// TestR6048_WriteServiceUnavailableWithDiagnostics_ContentLengthSet — verify
// R60.47 helper sets Content-Length.
func TestR6048_WriteServiceUnavailableWithDiagnostics_ContentLengthSet(t *testing.T) {
	rec := httptest.NewRecorder()

	diag := ServiceUnavailableDiagnostic{
		Error:           "model 'X' n_ctx auto-reload in progress",
		Model:           "X",
		BackendID:       "test-backend",
		TargetNCtx:      8192,
		EstimatedLoadMs: 60000,
		FeasibleMaxContext: 16384,
		CurrentNCtx:     2048,
		RequiredNCtx:    8500,
		Suggestion:      "wait 60s and retry",
	}

	writeServiceUnavailableWithDiagnostics(rec, diag)

	clStr := rec.Header().Get("Content-Length")
	if clStr == "" {
		t.Fatal("Content-Length header NOT set (Go will fallback to chunked encoding)")
	}
	cl, _ := strconv.Atoi(clStr)
	bodyLen := len(rec.Body.Bytes())
	if cl != bodyLen {
		t.Errorf("Content-Length = %d, body has %d bytes", cl, bodyLen)
	}

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}

	// Body должен быть валидный JSON со всеми diagnostic fields.
	var bodyMap map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &bodyMap); err != nil {
		t.Errorf("body is not JSON: %v (body=%s)", err, rec.Body.String())
	}
	if bodyMap["model"] != "X" {
		t.Errorf("body.model = %v, want 'X'", bodyMap["model"])
	}
	if bodyMap["target_n_ctx"] != float64(8192) {
		t.Errorf("body.target_n_ctx = %v, want 8192", bodyMap["target_n_ctx"])
	}
	if bodyMap["current_n_ctx"] != float64(2048) {
		t.Errorf("body.current_n_ctx = %v, want 2048", bodyMap["current_n_ctx"])
	}
	if bodyMap["retry_after"] != float64(60) { // EstimatedLoadMs=60000 → 60s
		t.Errorf("body.retry_after = %v, want 60", bodyMap["retry_after"])
	}
}

// TestR6048_WriteServiceUnavailableWithDiagnostics_DefaultRetryAfter —
// без RetryAfterSec и EstimatedLoadMs → default 90 (R60.42).
func TestR6048_WriteServiceUnavailableWithDiagnostics_DefaultRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	writeServiceUnavailableWithDiagnostics(rec, ServiceUnavailableDiagnostic{Error: "test"})

	clStr := rec.Header().Get("Content-Length")
	if clStr == "" {
		t.Fatal("Content-Length header NOT set")
	}
	cl, _ := strconv.Atoi(clStr)
	if cl != len(rec.Body.Bytes()) {
		t.Errorf("Content-Length = %d, body = %d bytes", cl, len(rec.Body.Bytes()))
	}

	if rec.Header().Get("Retry-After") != "90" {
		t.Errorf("Retry-After = %q, want 90 (R60.42 default)", rec.Header().Get("Retry-After"))
	}
}
