package main

// Regression test for the gemma-4 bug (2026-06-24):
//
//   Cline на gemma-4-E4B-it-Q4_K_M получал пустой ответ с done_reason="stop"
//   потому что cppworker МОЛЧА клампил n_predict до 512 при prompt>n_ctx.
//   Модель эмитила <end_of_turn> на 1-м токене и завершалась.
//
// Этот файл проверяет, что writePromptExceedsNCtxResponse пишет JSON,
// который балансировщик может распарсить и выполнить auto-reload.
//
// КРИТИЧНО: ответ должен содержать:
//   - top-level "code": int=3 (bridge.ErrCodePromptTooLong), чтобы балансер
//     через ParseCppWorkerError (см. internal/balancer/llamacpp_error.go)
//     распознал ошибку как n_ctx-reloadable.
//   - top-level "bridge_info" с полями current_n_ctx / required_n_ctx /
//     actual_tokens / n_predict / n_ctx_override / max_vram_n_ctx,
//     чтобы DecideReloadBackend мог принять решение reload.
//
// Без этих полей балансер не сможет выполнить auto-reload и просто
// пробросит 413 клиенту, что и было в production на gemma-4.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// TestWritePromptExceedsNCtxResponse_HasBridgeInfoForBalancer —
// верифицирует, что JSON-ответ содержит top-level code=3 (int) и bridge_info
// с обязательными полями для балансировщика.
func TestWritePromptExceedsNCtxResponse_HasBridgeInfoForBalancer(t *testing.T) {
	perr := &PromptExceedsNCtxError{
		ModelName:         "gemma-4-E4B-it-Q4_K_M",
		ActualTokens:      55111, // реальный кейс из production
		RequestedNPredict: 8192,
		NCtx:              32768, // модель загружена с n_ctx=32768
		MinNPredictFloor:  512,
		NCtxOverride:      32768, // клиент прислал options.num_ctx=32768
		MaxVRAMNCtx:       131072, // оценочный максимум для 20GB VRAM
	}

	rec := httptest.NewRecorder()
	writePromptExceedsNCtxResponse(rec, perr)

	// 1. HTTP 413
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected HTTP 413, got %d", rec.Code)
	}

	// 2. Content-Type JSON
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", ct)
	}

	// 3. Body — валидный JSON
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody=%s", err, rec.Body.String())
	}

	// 4. КРИТИЧНО: top-level "code" должен быть int=3 (bridge.ErrCodePromptTooLong),
	// чтобы балансер ParseCppWorkerError его распознал.
	codeRaw, ok := body["code"]
	if !ok {
		t.Fatal("top-level 'code' field MISSING — balancer won't trigger auto-reload")
	}
	// JSON unmarshal в interface{} даёт float64 для чисел.
	codeFloat, ok := codeRaw.(float64)
	if !ok {
		t.Fatalf("top-level 'code' is not a number: %T %v", codeRaw, codeRaw)
	}
	if int(codeFloat) != bridge.ErrCodePromptTooLong {
		t.Errorf("top-level 'code' = %v, want %d (ErrCodePromptTooLong). "+
			"Balancer needs int=3 to recognize this as n_ctx-reloadable error",
			codeFloat, bridge.ErrCodePromptTooLong)
	}

	// 5. КРИТИЧНО: bridge_info должен содержать все обязательные поля.
	biRaw, ok := body["bridge_info"]
	if !ok {
		t.Fatal("'bridge_info' field MISSING — balancer can't decide auto-reload without it")
	}
	bi, ok := biRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("'bridge_info' is not a map: %T", biRaw)
	}

	// required_n_ctx должен быть >= ActualTokens + MinNPredictFloor + 1
	// (roundUpPow2).
	requiredNCtx := int(bi["required_n_ctx"].(float64))
	wantMinRequired := perr.ActualTokens + perr.MinNPredictFloor + 1
	if requiredNCtx < wantMinRequired {
		t.Errorf("bridge_info.required_n_ctx=%d, must be >= %d (actual+floor+1)",
			requiredNCtx, wantMinRequired)
	}
	// roundUpPow2 ожидаемо: actual=55111+512+1=55624, roundUpPow2 → 65536.
	if requiredNCtx != 65536 {
		t.Logf("NOTE: required_n_ctx=%d (roundUpPow2 of %d=65536 expected). "+
			"Acceptable as long as >= %d", requiredNCtx, wantMinRequired, wantMinRequired)
	}

	// current_n_ctx должен совпадать с NCtx ошибки.
	if int(bi["current_n_ctx"].(float64)) != perr.NCtx {
		t.Errorf("bridge_info.current_n_ctx=%v, want %d",
			bi["current_n_ctx"], perr.NCtx)
	}

	// actual_tokens должен совпадать.
	if int(bi["actual_tokens"].(float64)) != perr.ActualTokens {
		t.Errorf("bridge_info.actual_tokens=%v, want %d",
			bi["actual_tokens"], perr.ActualTokens)
	}

	// n_ctx_override должен совпадать (то, что клиент прислал в options.num_ctx).
	if int(bi["n_ctx_override"].(float64)) != perr.NCtxOverride {
		t.Errorf("bridge_info.n_ctx_override=%v, want %d",
			bi["n_ctx_override"], perr.NCtxOverride)
	}

	// max_vram_n_ctx должен совпадать (нужен для VRAM safety check).
	if int(bi["max_vram_n_ctx"].(float64)) != perr.MaxVRAMNCtx {
		t.Errorf("bridge_info.max_vram_n_ctx=%v, want %d",
			bi["max_vram_n_ctx"], perr.MaxVRAMNCtx)
	}

	// code внутри bridge_info тоже должен быть 3 (для ParseCppWorkerError).
	if int(bi["code"].(float64)) != bridge.ErrCodePromptTooLong {
		t.Errorf("bridge_info.code=%v, want %d", bi["code"], bridge.ErrCodePromptTooLong)
	}

	t.Logf("OK: 413 with bridge_info (code=3, current=%d, required=%d, actual=%d, vram=%d)",
		int(bi["current_n_ctx"].(float64)),
		requiredNCtx,
		int(bi["actual_tokens"].(float64)),
		int(bi["max_vram_n_ctx"].(float64)))
}

// TestWritePromptExceedsNCtxResponse_DefaultsWhenNoVRAM —
// если MaxVRAMNCtx=0 (backend не сообщил VRAM info, CPU-only / unknown GPU),
// bridge_info.max_vram_n_ctx должен быть 0 — балансер применит safety fallback.
func TestWritePromptExceedsNCtxResponse_DefaultsWhenNoVRAM(t *testing.T) {
	perr := &PromptExceedsNCtxError{
		ModelName:         "test-model",
		ActualTokens:      10000,
		RequestedNPredict: 4096,
		NCtx:              4096,
		MinNPredictFloor:  512,
		NCtxOverride:      0, // клиент не прислал num_ctx
		MaxVRAMNCtx:       0, // CPU-only / unknown
	}

	rec := httptest.NewRecorder()
	writePromptExceedsNCtxResponse(rec, perr)

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	bi := body["bridge_info"].(map[string]interface{})
	if int(bi["max_vram_n_ctx"].(float64)) != 0 {
		t.Errorf("expected max_vram_n_ctx=0, got %v", bi["max_vram_n_ctx"])
	}
	if int(bi["n_ctx_override"].(float64)) != 0 {
		t.Errorf("expected n_ctx_override=0, got %v", bi["n_ctx_override"])
	}
	if int(bi["code"].(float64)) != bridge.ErrCodePromptTooLong {
		t.Errorf("expected bridge_info.code=%d, got %v", bridge.ErrCodePromptTooLong, bi["code"])
	}
}

// TestHandleInferenceError_DispatchesPromptExceeds —
// handleInferenceError должен вызвать writePromptExceedsNCtxResponse
// для *PromptExceedsNCtxError и вернуть true (handler должен return).
func TestHandleInferenceError_DispatchesPromptExceeds(t *testing.T) {
	perr := &PromptExceedsNCtxError{
		ModelName:         "test-model",
		ActualTokens:      10000,
		RequestedNPredict: 4096,
		NCtx:              4096,
		MinNPredictFloor:  512,
		MaxVRAMNCtx:       8192,
	}

	rec := httptest.NewRecorder()
	handled := handleInferenceError(rec, perr)

	if !handled {
		t.Error("handleInferenceError should return true for *PromptExceedsNCtxError")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected HTTP 413, got %d", rec.Code)
	}

	// Verify JSON contains the balancer-critical fields
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["code"].(float64) != float64(bridge.ErrCodePromptTooLong) {
		t.Error("handleInferenceError should produce JSON with code=3 for balancer")
	}
	if _, ok := body["bridge_info"]; !ok {
		t.Error("handleInferenceError should produce JSON with bridge_info for balancer")
	}
}