// Тесты для nctx_reload.go: проверка, что makeRejectPlan использует
// разные error_code в JSON-ответе в зависимости от причины reject.
//
// Контекст (2026-06-25): пользователь сообщил, что при длинном prompt
// клиенту Cline возвращается "n_ctx_too_large_for_backend" хотя на
// самом деле проблема в prompt_exceeds_context (модель уже загружена
// с максимальным n_ctx, просто prompt слишком длинный). Это вводит
// в заблуждение — UI/Cline пытаются сделать reload, который не поможет.
//
// Фикс: в зависимости от bridgeErr.Code выбирается разный error_code:
//   - code 3 (PromptTooLong) → "prompt_exceeds_context"
//   - code 2 (NCtxNeedsReload) → "n_ctx_too_large_for_backend"
//   - AutoReloadMaxNCtx > required → "n_ctx_too_large_for_backend"
//   - maxVRAMNCtx == 0 → "n_ctx_too_large_for_backend"
//
// Эти тесты проверяют, что JSON поле "error" в RejectMsg соответствует
// ожидаемому для каждого сценария.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// makeTestCoordinator создаёт NCtxReloadCoordinator с дефолтным конфигом
// (AutoReloadNCtx=true, AutoReloadMaxNCtx=0, safety=0.85).
func makeTestCoordinator() *NCtxReloadCoordinator {
	return NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
}

// TestMakeRejectPlan_PromptExceedsContext_2026_06_25 проверяет, что для
// code=3 (PromptTooLong) используется error_code="prompt_exceeds_context"
// и suggestion объясняет, что reload не поможет.
func TestMakeRejectPlan_PromptExceedsContext_2026_06_25(t *testing.T) {
	c := makeTestCoordinator()
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodePromptTooLong,
		CurrentNCtx:  65536,
		RequiredNCtx: 68271,
		ActualTokens: 68271,
		MaxVRAMNCtx:  21952, // может быть занижено C-bridge для текущей конфигурации
		Message:      "prompt exceeds n_ctx",
	}
	plan := c.makeRejectPlan("cppworker-gpu-bundled", bridgeErr, bridgeErr.RequiredNCtx,
		"prompt_too_long: model is already loaded with maximum possible n_ctx",
		"prompt_exceeds_context")

	if plan.Decision != DecisionReject {
		t.Fatalf("expected DecisionReject, got %s", plan.Decision)
	}

	// Парсим JSON и проверяем поле error
	var rej map[string]interface{}
	if err := json.Unmarshal([]byte(plan.RejectMsg), &rej); err != nil {
		t.Fatalf("RejectMsg is not valid JSON: %v\nbody: %s", err, plan.RejectMsg)
	}

	if got := rej["error"]; got != "prompt_exceeds_context" {
		t.Errorf("expected error=\"prompt_exceeds_context\", got error=%v", got)
	}

	// Проверяем, что suggestion объясняет, что reload не поможет
	suggestion, _ := rej["suggestion"].(string)
	if !strings.Contains(strings.ToLower(suggestion), "reload") {
		t.Errorf("suggestion should mention that reload won't help, got: %s", suggestion)
	}

	// bridge_code должен быть передан в JSON
	if got := rej["bridge_code"]; got != float64(NCtxErrCodePromptTooLong) {
		t.Errorf("expected bridge_code=%d, got bridge_code=%v", NCtxErrCodePromptTooLong, got)
	}
}

// TestMakeRejectPlan_NCtxTooLargeForBackend_2026_06_25 проверяет, что для
// code=2 (NCtxNeedsReload) + maxVRAMNCtx слишком маленький → error_code="n_ctx_too_large_for_backend".
func TestMakeRejectPlan_NCtxTooLargeForBackend_2026_06_25(t *testing.T) {
	c := makeTestCoordinator()
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodeNCtxNeedsReload,
		CurrentNCtx:  4096,
		RequiredNCtx: 100000,
		MaxVRAMNCtx:  10000, // намного меньше required
		Message:      "n_ctx exceeds VRAM",
	}
	plan := c.makeRejectPlan("cppworker-gpu-bundled", bridgeErr, bridgeErr.RequiredNCtx,
		"required n_ctx=100000 exceeds safe VRAM limit=8500",
		"n_ctx_too_large_for_backend")

	if plan.Decision != DecisionReject {
		t.Fatalf("expected DecisionReject, got %s", plan.Decision)
	}

	var rej map[string]interface{}
	if err := json.Unmarshal([]byte(plan.RejectMsg), &rej); err != nil {
		t.Fatalf("RejectMsg is not valid JSON: %v\nbody: %s", err, plan.RejectMsg)
	}

	if got := rej["error"]; got != "n_ctx_too_large_for_backend" {
		t.Errorf("expected error=\"n_ctx_too_large_for_backend\", got error=%v", got)
	}

	// bridge_code должен быть передан в JSON
	if got := rej["bridge_code"]; got != float64(NCtxErrCodeNCtxNeedsReload) {
		t.Errorf("expected bridge_code=%d, got bridge_code=%v", NCtxErrCodeNCtxNeedsReload, got)
	}
}

// TestMakeRejectPlan_DefaultErrorCode проверяет, что при пустом errorCode
// используется default "n_ctx_too_large_for_backend" (обратная совместимость).
func TestMakeRejectPlan_DefaultErrorCode(t *testing.T) {
	c := makeTestCoordinator()
	bridgeErr := &NCtxBridgeError{
		Code:        NCtxErrCodeNCtxNeedsReload,
		CurrentNCtx: 4096,
		RequiredNCtx: 10000,
		MaxVRAMNCtx: 5000,
	}
	plan := c.makeRejectPlan("test", bridgeErr, 10000, "test reason", "")

	var rej map[string]interface{}
	if err := json.Unmarshal([]byte(plan.RejectMsg), &rej); err != nil {
		t.Fatalf("RejectMsg is not valid JSON: %v", err)
	}

	if got := rej["error"]; got != "n_ctx_too_large_for_backend" {
		t.Errorf("expected default error=\"n_ctx_too_large_for_backend\", got error=%v", got)
	}
}

// TestDecideReloadBackend_PromptTooLong_ReturnsPromptExceedsContext проверяет
// end-to-end flow: DecideReloadBackend с code=3 возвращает DecisionReject
// с error_code="prompt_exceeds_context" в RejectMsg.
func TestDecideReloadBackend_PromptTooLong_ReturnsPromptExceedsContext(t *testing.T) {
	c := makeTestCoordinator()
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodePromptTooLong,
		CurrentNCtx:  65536,
		RequiredNCtx: 68271,
		ActualTokens: 68271,
		MaxVRAMNCtx:  21952,
		Message:      "prompt exceeds n_ctx even with minimum n_predict floor",
	}
	plan := c.DecideReloadBackend("cppworker-gpu-bundled", bridgeErr, 65536)

	if plan == nil {
		t.Fatal("DecideReloadBackend returned nil")
	}
	if plan.Decision != DecisionReject {
		t.Fatalf("expected DecisionReject (не reload — текущий n_ctx уже на максимуме), got %s", plan.Decision)
	}

	var rej map[string]interface{}
	if err := json.Unmarshal([]byte(plan.RejectMsg), &rej); err != nil {
		t.Fatalf("RejectMsg is not valid JSON: %v", err)
	}

	if got := rej["error"]; got != "prompt_exceeds_context" {
		t.Errorf("expected error=\"prompt_exceeds_context\" for code=3 path, got error=%v", got)
	}

	// В Reason должно быть упоминание prompt_too_long (для логов)
	if !strings.Contains(plan.Reason, "prompt_too_long") {
		t.Errorf("expected Reason to mention 'prompt_too_long', got: %s", plan.Reason)
	}
}

// TestDecideReloadBackend_NCtxNeedsReload_ReturnsVRAMError проверяет, что для
// code=2 (NCtxNeedsReload) с превышением safeMax используется error_code="n_ctx_too_large_for_backend".
func TestDecideReloadBackend_NCtxNeedsReload_ReturnsVRAMError(t *testing.T) {
	c := makeTestCoordinator()
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodeNCtxNeedsReload,
		CurrentNCtx:  4096,
		RequiredNCtx: 100000,
		MaxVRAMNCtx:  10000, // намного меньше required
	}
	plan := c.DecideReloadBackend("test", bridgeErr, 100000)

	if plan == nil {
		t.Fatal("DecideReloadBackend returned nil")
	}
	if plan.Decision != DecisionReject {
		t.Fatalf("expected DecisionReject (required exceeds safeMax), got %s", plan.Decision)
	}

	var rej map[string]interface{}
	if err := json.Unmarshal([]byte(plan.RejectMsg), &rej); err != nil {
		t.Fatalf("RejectMsg is not valid JSON: %v", err)
	}

	if got := rej["error"]; got != "n_ctx_too_large_for_backend" {
		t.Errorf("expected error=\"n_ctx_too_large_for_backend\" for code=2+VRAM exceed, got error=%v", got)
	}
}