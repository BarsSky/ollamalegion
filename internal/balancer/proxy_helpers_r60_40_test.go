// proxy_helpers_r60_40_test.go — R60.40 (2026-09-11) tests for
// writeServiceUnavailableWithDiagnostics helper.
//
// R60.40 fix: pre-R60.40, balancer returned 503/502 with only
// `{"error": "..."}` for OpenWebUI when:
//   - Model was auto-loading after a previous failed request
//   - cppworker returned empty body (model still loading / busy)
//   - n_ctx mismatch forced a reload
//
// OpenWebUI got HTTP 503 with no actionable info — just a text error.
// User had no way to know:
//   - What's the optimal n_ctx for this model on this hardware?
//   - How long will the reload take?
//   - Should they reduce num_predict, increase n_ctx, or wait?
//
// R60.40 fix: new writeServiceUnavailableWithDiagnostics helper that
// includes:
//   - bridge_info from cppworker's last error (current_n_ctx, required_n_ctx, etc.)
//   - feasible_max_context from cppworker metrics
//   - gguf_max_context (model's hard upper bound)
//   - target_n_ctx (what we're loading/reloading to)
//   - estimated_load_time_ms (from cppworker's load response)
//   - retry_after_sec (accurate, from estimated_load_time_ms / 1000)
//   - suggestion (human-readable advice)
//
// OpenWebUI receives rich diagnostics it can show to the user.
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteServiceUnavailableWithDiagnostics_Basic — minimal diag, error only.
func TestWriteServiceUnavailableWithDiagnostics_Basic(t *testing.T) {
	rec := httptest.NewRecorder()
	diag := ServiceUnavailableDiagnostic{
		Error:         "auto-load failed: model is loading",
		RetryAfterSec: 30,
	}
	writeServiceUnavailableWithDiagnostics(rec, diag)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("body.error = empty, want non-empty")
	}
}

// TestWriteServiceUnavailableWithDiagnostics_FullDiagnostics — все поля.
func TestWriteServiceUnavailableWithDiagnostics_FullDiagnostics(t *testing.T) {
	rec := httptest.NewRecorder()
	diag := ServiceUnavailableDiagnostic{
		Error:              "model 'Qwen3-Instruct-2507-q4km' auto-load in progress",
		RetryAfterSec:      95,
		Model:              "Qwen3-Instruct-2507-q4km",
		BackendID:          "cppworker-gpu-bundled-agent",
		TargetNCtx:         8192,
		EstimatedLoadMs:    95000,
		FeasibleMaxContext: 25884,
		GGUFMaxContext:     262144,
		Suggestion:         "Reduce num_predict to 1024 OR save a model profile with contextLength=8192 and reload",
	}
	writeServiceUnavailableWithDiagnostics(rec, diag)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "95" {
		t.Errorf("Retry-After = %q, want 95 (accurate from estimated_load_time_ms)", got)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	// Check all diagnostic fields are present
	requiredFields := []string{
		"error", "model", "backend_id", "target_n_ctx",
		"estimated_load_time_ms", "feasible_max_context",
		"gguf_max_context", "suggestion", "retry_after",
	}
	for _, f := range requiredFields {
		if _, ok := body[f]; !ok {
			t.Errorf("body.%s missing (full diagnostics required)", f)
		}
	}
	if body["target_n_ctx"].(float64) != 8192 {
		t.Errorf("target_n_ctx = %v, want 8192", body["target_n_ctx"])
	}
	if body["retry_after"].(float64) != 95 {
		t.Errorf("retry_after = %v, want 95", body["retry_after"])
	}
	if !strings.Contains(body["suggestion"].(string), "Reduce num_predict") {
		t.Errorf("suggestion = %q, want contains 'Reduce num_predict'", body["suggestion"])
	}
}

// TestWriteServiceUnavailableWithDiagnostics_DefaultRetryAfter — без retry_after → default 30s.
func TestWriteServiceUnavailableWithDiagnostics_DefaultRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	diag := ServiceUnavailableDiagnostic{
		Error: "load failed",
		// RetryAfterSec = 0 → default 30
	}
	writeServiceUnavailableWithDiagnostics(rec, diag)

	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30 (default)", got)
	}
}

// TestWriteServiceUnavailableWithDiagnostics_EstimatedMsToRetryAfter —
// если EstimatedLoadMs > 0, Retry-After должен быть EstimatedLoadMs / 1000
// (округление вверх, минимум 5s, максимум 600s).
func TestWriteServiceUnavailableWithDiagnostics_EstimatedMsToRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		estMs    int
		wantSec  int
	}{
		{"5s", 5000, 5},
		{"30s", 30000, 30},
		{"95s", 95000, 95},
		{"180s", 180000, 180},
		{"600s capped", 700000, 600},  // cap at 10 min
		{"min 5s", 1000, 5},           // floor at 5s
		{"zero = default 30", 0, 30},   // 0 means use 30
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			diag := ServiceUnavailableDiagnostic{
				Error:           "load in progress",
				EstimatedLoadMs: tc.estMs,
			}
			writeServiceUnavailableWithDiagnostics(rec, diag)
			if got := rec.Header().Get("Retry-After"); got != intToStr(tc.wantSec) {
				t.Errorf("Retry-After = %q, want %d", got, tc.wantSec)
			}
		})
	}
}

// TestWriteServiceUnavailableWithDiagnostics_EmptyFieldsOmitted —
// пустые опциональные поля НЕ попадают в JSON (omitempty).
func TestWriteServiceUnavailableWithDiagnostics_EmptyFieldsOmitted(t *testing.T) {
	rec := httptest.NewRecorder()
	diag := ServiceUnavailableDiagnostic{
		Error:         "test",
		RetryAfterSec: 30,
		// Model, BackendID, TargetNCtx, etc. — all empty
	}
	writeServiceUnavailableWithDiagnostics(rec, diag)

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if _, ok := body["model"]; ok {
		t.Errorf("body.model should be omitted when empty")
	}
	if _, ok := body["target_n_ctx"]; ok {
		t.Errorf("body.target_n_ctx should be omitted when empty")
	}
	if _, ok := body["suggestion"]; ok {
		t.Errorf("body.suggestion should be omitted when empty")
	}
	// error всегда должен быть
	if _, ok := body["error"]; !ok {
		t.Errorf("body.error missing")
	}
}

// TestWriteServiceUnavailableWithDiagnostics_NCtxOverflowSuggestion —
// если у нас есть CurrentNCtx и RequiredNCtx из cppworker, suggestion
// должен объяснять что делать.
func TestWriteServiceUnavailableWithDiagnostics_NCtxOverflowSuggestion(t *testing.T) {
	rec := httptest.NewRecorder()
	diag := ServiceUnavailableDiagnostic{
		Error:              "n_ctx overflow",
		RetryAfterSec:      30,
		Model:              "test-model",
		TargetNCtx:         8192,
		FeasibleMaxContext: 25884,
		CurrentNCtx:        2048,
		RequiredNCtx:       8500,
		GGUFMaxContext:     262144,
	}
	writeServiceUnavailableWithDiagnostics(rec, diag)

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	// Should have bridge_info-like fields
	if body["current_n_ctx"].(float64) != 2048 {
		t.Errorf("current_n_ctx = %v, want 2048", body["current_n_ctx"])
	}
	if body["required_n_ctx"].(float64) != 8500 {
		t.Errorf("required_n_ctx = %v, want 8500", body["required_n_ctx"])
	}
	if body["target_n_ctx"].(float64) != 8192 {
		t.Errorf("target_n_ctx = %v, want 8192", body["target_n_ctx"])
	}
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	// strconv.Itoa without import
	const digits = "0123456789"
	if n < 0 {
		return "-" + intToStr(-n)
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}
