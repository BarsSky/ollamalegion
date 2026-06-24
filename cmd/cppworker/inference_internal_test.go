// inference_internal_test.go — Unit-тесты для узких мест логики инференса,
// которые приводят к «сбросу» запросов с tools/tool_calls в cppworker.
//
// Build tag: llama_stub (компилируется со stub-bridge, без CGo/llama.cpp).
//
// Каждый тест соответствует конкретному симптому из docs/tools-request-debug-runbook.md:
//
//   B1. TestClampNPredictToFitContext_ToolsPromptOverflow —
//       длинный prompt + tools, n_predict должен быть занижен до n_ctx - actualTokens.
//   B2. TestClampNPredictToFitContext_FloorMinNPredict —
//       prompt уже больше n_ctx → n_predict не опускается ниже 512 (minNPredictClamp).
//   B3. TestClampNPredictToFitContext_NoNCtxOverride —
//       без NCtxOverride функция no-op (не можем оценить).
//   B4. TestRamFallbackReload_ToolsRequest_DeclineAndError —
//       при hasTools=true reload НЕ выполняется, возвращается *ReloadDisabledForToolsError.
//   B5. TestReloadLoopLimit_CycleTriggersAfterMaxAttempts —
//       после 3 попыток getReloadAttempts возвращает isLimit=true (поведение, описанное
//       в docs/remaining-real-plan.md как «бесконечный reload-loop»).
//   B6. TestReloadLoopLimit_ResetAfterWindowExpires —
//       по истечении ramFallbackCycleResetInterval счётчик сбрасывается.
//   B7. TestReloadLoopLimit_ResetReloadAttemptsClears —
//       успешный инференс сбрасывает счётчик (моделируется вызовом resetReloadAttempts).
//
// Runbook: docs/tools-request-debug-runbook.md
// Диагностические логи: cmd/cppworker/inference.go (clampNPredictToFitContext, tryRamFallbackReload).

package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// =============================================================================
// B1. clampNPredictToFitContext: длинный prompt + tools → n_predict занижается
// =============================================================================

// makePromptOfLen возвращает строку из N ASCII-символов (грубо 4 символа = 1 токен
// при оценке в countModelTokensByLoadedInfo, когда модель не загружена).
func makePromptOfLen(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestClampNPredictToFitContext_ToolsPromptOverflow(t *testing.T) {
	// Сценарий: клиент задал n_predict=31744, n_ctx=32768, prompt ~6887 символов
	// (~1722 грубых токенов). Без клампинга: 6887_символов/4 + 31744 + 1 > 32768.
	// Реально tokenizer модели отсутствует (мы в stub), поэтому
	// countModelTokensByLoadedInfo использует fallback len(rune)/4 = 1722.
	// 2026-06-24: prompt+n_predict_min=1722+512+1=2235 < 32768 → NO overflow → no error.
	params := bridge.DefaultGenerationParams()
	params.NCtxOverride = 32768
	params.NPredict = 31744

	prompt := makePromptOfLen(6887)
	if err := clampNPredictToFitContext("test-model", prompt, &params); err != nil {
		t.Errorf("expected no error (prompt+n_predict fits n_ctx), got: %v", err)
	}

	// После клампинга NPredict должен быть <= n_ctx - actualTokens - 1
	// С грубой оценкой 6887/4 = 1722: maxAllowedNPredict ≈ 32768 - 1722 - 1 = 31045.
	// Ожидаем, что params.NPredict снизилось с 31744 до ~31045.
	if params.NPredict >= 31744 {
		t.Errorf("NPredict not clamped: got %d, want < 31744 (n_ctx=32768, prompt=%d chars)",
			params.NPredict, len(prompt))
	}
	if params.NPredict < 30000 {
		t.Errorf("NPredict over-clamped: got %d, want >= 30000", params.NPredict)
	}
	t.Logf("OK: NPredict clamped from 31744 to %d (n_ctx=32768, prompt=%d chars)",
		params.NPredict, len(prompt))
}

func TestClampNPredictToFitContext_ClientExplicitSmallNPredict(t *testing.T) {
	// Клиент задал n_predict=128 явно — clamp не должен перезаписывать.
	// ВАЖНО: clampNPredictToFitContext срабатывает только если params.NPredict > maxAllowedNPredict.
	// Если клиент задал 128, а maxAllowedNPredict=31045, то 128 < 31045 → клампинг не нужен.
	params := bridge.DefaultGenerationParams()
	params.NCtxOverride = 32768
	params.NPredict = 128

	prompt := makePromptOfLen(100)
	if err := clampNPredictToFitContext("test-model", prompt, &params); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	if params.NPredict != 128 {
		t.Errorf("client explicit n_predict=128 must be preserved, got %d", params.NPredict)
	}
}

func TestClampNPredictToFitContext_FloorMinNPredict(t *testing.T) {
	// Сценарий: prompt ЗНАЧИТЕЛЬНО больше n_ctx.
	// n_ctx=2048, prompt=10000 символов (~2500 грубых токенов).
	//
	// ПОВЕДЕНИЕ ДО 2026-06-24: тихий клампинг n_predict до 512 → модель
	//   эмитит <end_of_turn> мгновенно → "пустой ответ с done_reason=stop"
	//   в Cline/OpenWebUI (см. регрессию 2026-06-24 12:37 UTC).
	//
	// ПОВЕДЕНИЕ ПОСЛЕ 2026-06-24: функция возвращает *PromptExceedsNCtxError,
	//   а caller (generateWithRamFallback/generateStreamWithRamFallback)
	//   транслирует его в HTTP 413.
	params := bridge.DefaultGenerationParams()
	params.NCtxOverride = 2048
	params.NPredict = 2048 // bridge default

	prompt := makePromptOfLen(10000)
	err := clampNPredictToFitContext("test-model", prompt, &params)

	if err == nil {
		t.Errorf("expected *PromptExceedsNCtxError, got nil (silent clamp to 512 = REGRESSION)")
	}
	var perr *PromptExceedsNCtxError
	if !errors.As(err, &perr) {
		t.Errorf("expected *PromptExceedsNCtxError, got %T: %v", err, err)
	} else {
		if perr.ActualTokens <= 0 {
			t.Errorf("ActualTokens should be > 0, got %d", perr.ActualTokens)
		}
		if perr.NCtx != 2048 {
			t.Errorf("NCtx should be 2048, got %d", perr.NCtx)
		}
		if perr.MinNPredictFloor != 512 {
			t.Errorf("MinNPredictFloor should be 512, got %d", perr.MinNPredictFloor)
		}
		// params не должны быть модифицированы при overflow (caller сам вернет ошибку).
		if params.NPredict != 2048 {
			t.Errorf("params.NPredict must NOT be modified when overflow detected, got %d", params.NPredict)
		}
		t.Logf("OK: returned *PromptExceedsNCtxError: actual=%d n_ctx=%d floor=%d",
			perr.ActualTokens, perr.NCtx, perr.MinNPredictFloor)
	}
}

func TestClampNPredictToFitContext_NoNCtxOverride(t *testing.T) {
	// Без NCtxOverride — функция не может оценить, поэтому no-op (returns nil).
	params := bridge.DefaultGenerationParams()
	params.NCtxOverride = 0
	original := params.NPredict

	prompt := makePromptOfLen(100000) // огромный prompt
	if err := clampNPredictToFitContext("test-model", prompt, &params); err != nil {
		t.Errorf("expected nil error when NCtxOverride=0, got: %v", err)
	}

	if params.NPredict != original {
		t.Errorf("NPredict must not change when NCtxOverride=0, got %d (was %d)",
			params.NPredict, original)
	}
}

func TestClampNPredictToFitContext_NilParams(t *testing.T) {
	// Защита от nil — функция не должна паниковать.
	// 2026-06-24: теперь возвращает ошибку (не молча no-op).
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("clampNPredictToFitContext panicked on nil params: %v", r)
		}
	}()
	err := clampNPredictToFitContext("test-model", "hello", nil)
	if err == nil {
		t.Errorf("expected non-nil error on nil params, got nil")
	}
}


// =============================================================================
// B2-2026-06-24. clampNPredictToFitContext: prompt > n_ctx → *PromptExceedsNCtxError
// =============================================================================
//
// Регрессия: 2026-06-24 12:37 UTC. Cline на gemma-4-E4B-it-Q4_K_M получал пустой
// ответ с done_reason="stop" из-за того, что prompt=9391 токенов > n_ctx=8196,
// и clampNPredictToFitContext молча клампил n_predict до 512 (minNPredictFloor).
// После фикса функция возвращает *PromptExceedsNCtxError → caller транслирует
// в HTTP 413.

func TestClampNPredictToFitContext_PromptExceedsNCtx_ReturnsError(t *testing.T) {
	// Точная репродукция production-случая:
	// gemma-4-E4B-it-Q4_K_M: n_ctx=8196, prompt=9391 токенов.
	// params.NPredict=7172 (как в логе).
	params := bridge.DefaultGenerationParams()
	params.NCtxOverride = 8196
	params.NPredict = 7172

	prompt := makePromptOfLen(37564) // 37564/4 = 9391 токенов по грубой оценке
	err := clampNPredictToFitContext("gemma-4-E4B-it-Q4_K_M", prompt, &params)

	if err == nil {
		t.Fatal("expected *PromptExceedsNCtxError for gemma-4 prompt>n_ctx case, got nil (REGRESSION!)")
	}
	var perr *PromptExceedsNCtxError
	if !errors.As(err, &perr) {
		t.Fatalf("expected *PromptExceedsNCtxError, got %T: %v", err, err)
	}
	if perr.ModelName != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("ModelName=%q, want gemma-4-E4B-it-Q4_K_M", perr.ModelName)
	}
	if perr.NCtx != 8196 {
		t.Errorf("NCtx=%d, want 8196", perr.NCtx)
	}
	if perr.ActualTokens != 9391 {
		t.Errorf("ActualTokens=%d, want 9391", perr.ActualTokens)
	}
	if perr.MinNPredictFloor != 512 {
		t.Errorf("MinNPredictFloor=%d, want 512", perr.MinNPredictFloor)
	}
	if perr.RequestedNPredict != 7172 {
		t.Errorf("RequestedNPredict=%d, want 7172", perr.RequestedNPredict)
	}
	errMsg := perr.Error()
	for _, want := range []string{"gemma-4", "8196", "9391", "increase n_ctx", "reduce"} {
		if !strings.Contains(errMsg, want) {
			t.Errorf("error message missing %q. Got: %s", want, errMsg)
		}
	}
	if params.NPredict != 7172 {
		t.Errorf("params.NPredict must NOT be modified on overflow, got %d", params.NPredict)
	}
	t.Logf("OK: *PromptExceedsNCtxError returned with actionable advice: %s", errMsg)
}

func TestClampNPredictToFitContext_PromptAtBoundary_NoError(t *testing.T) {
	// Граничный случай: prompt ровно влезает с учётом minNPredictFloor+1.
	// n_ctx=1024, prompt=510 токенов → 510+512+1=1023 < 1024 → OK.
	params := bridge.DefaultGenerationParams()
	params.NCtxOverride = 1024
	params.NPredict = 1024

	prompt := makePromptOfLen(2040) // 2040/4 = 510 токенов
	if err := clampNPredictToFitContext("test-model", prompt, &params); err != nil {
		t.Errorf("expected nil error at boundary (prompt=510, n_ctx=1024, min_floor=512), got: %v", err)
	}
}

func TestClampNPredictToFitContext_PromptJustOverBoundary_ReturnsError(t *testing.T) {
	// Граничный случай: prompt ровно на 1 больше, чем допустимо.
	// n_ctx=1024, prompt=512 токенов → 512+512+1=1025 > 1024 → error.
	params := bridge.DefaultGenerationParams()
	params.NCtxOverride = 1024
	params.NPredict = 1024

	prompt := makePromptOfLen(2048) // 2048/4 = 512 токенов
	err := clampNPredictToFitContext("test-model", prompt, &params)
	if err == nil {
		t.Errorf("expected *PromptExceedsNCtxError at boundary+1, got nil")
	}
	var perr *PromptExceedsNCtxError
	if !errors.As(err, &perr) {
		t.Errorf("expected *PromptExceedsNCtxError, got %T", err)
	}
}
// =============================================================================
// B4. tryRamFallbackReload: при hasTools=true возвращает *ReloadDisabledForToolsError
// =============================================================================

func TestReloadDisabledForToolsError_Message(t *testing.T) {
	// Проверяем, что сообщение об ошибке содержит actionable совет.
	err := &ReloadDisabledForToolsError{Model: "qwen2.5:7b-instruct-q4_K_M"}
	msg := err.Error()

	if !strings.Contains(msg, "qwen2.5:7b-instruct-q4_K_M") {
		t.Errorf("error message must contain model name, got: %s", msg)
	}
	if !strings.Contains(msg, "Reduce") {
		t.Errorf("error message must contain actionable advice, got: %s", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "n_ctx") {
		t.Errorf("error message must mention n_ctx, got: %s", msg)
	}
	if !isReloadDisabledForToolsError(err) {
		t.Error("isReloadDisabledForToolsError must return true for *ReloadDisabledForToolsError")
	}
	if isReloadDisabledForToolsError(errors.New("other error")) {
		t.Error("isReloadDisabledForToolsError must return false for non-matching errors")
	}
}

// =============================================================================
// B5. Цикл-reload: после ramFallbackMaxAttempts попыток isLimit=true
// =============================================================================

func TestReloadAttempts_CycleLimitTriggered(t *testing.T) {
	// Проверяем, что после 3 попыток (default ramFallbackMaxAttempts)
	// getReloadAttempts возвращает isLimit=true — что и приводит к
	// "RAM fallback: cycle limit reached, refusing reload" в inference.go.
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	const modelName = "model-cycle-test"

	// До попыток — лимит не достигнут.
	count, isLimit := getReloadAttempts(modelName)
	if count != 0 || isLimit {
		t.Errorf("initial state: count=%d, isLimit=%v, want 0, false", count, isLimit)
	}

	// 3 попытки — лимит достигнут.
	for i := 0; i < ramFallbackMaxAttempts; i++ {
		recordReloadAttempt(modelName)
	}
	count, isLimit = getReloadAttempts(modelName)
	if count != ramFallbackMaxAttempts {
		t.Errorf("after %d attempts: count=%d, want %d",
			ramFallbackMaxAttempts, count, ramFallbackMaxAttempts)
	}
	if !isLimit {
		t.Errorf("after %d attempts: isLimit=false, want true (cycle limit reached)",
			ramFallbackMaxAttempts)
	}
	t.Logf("OK: cycle limit triggered after %d attempts", ramFallbackMaxAttempts)
}

// =============================================================================
// B6. Цикл-reload: по истечении окна счётчик сбрасывается
// =============================================================================

func TestReloadAttempts_ResetAfterWindowExpires(t *testing.T) {
	// Эмулируем «истечение окна»: вручную ставим firstAttemptTime в прошлое.
	// После этого getReloadAttempts должен сбросить счётчик и вернуть 0, false.
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	const modelName = "model-window-test"

	// Регистрируем попытку и форсируем устаревание окна.
	recordReloadAttempt(modelName)
	stateRaw, _ := ramFallbackAttempts.Load(modelName)
	state := stateRaw.(*ramFallbackAttemptState)
	state.mu.Lock()
	state.firstAttemptTime = time.Now().Add(-2 * ramFallbackCycleResetInterval)
	state.lastAttemptTime = time.Now().Add(-2 * ramFallbackCycleResetInterval)
	state.mu.Unlock()

	count, isLimit := getReloadAttempts(modelName)
	if count != 0 {
		t.Errorf("after window expired: count=%d, want 0 (reset)", count)
	}
	if isLimit {
		t.Errorf("after window expired: isLimit=true, want false (reset)")
	}
	t.Logf("OK: counter reset after window expired (%s)", ramFallbackCycleResetInterval)
}

// =============================================================================
// B7. Сброс после успешного инференса
// =============================================================================

func TestReloadAttempts_ResetClearsCounter(t *testing.T) {
	// После успешного инференса (вызов resetReloadAttempts) счётчик
	// должен сброситься, чтобы следующий overflow мог заново триггернуть reload.
	resetRamFallbackAttemptsForTest()
	defer resetRamFallbackAttemptsForTest()

	const modelName = "model-reset-test"

	// Исчерпываем лимит.
	for i := 0; i < ramFallbackMaxAttempts; i++ {
		recordReloadAttempt(modelName)
	}
	_, isLimit := getReloadAttempts(modelName)
	if !isLimit {
		t.Fatal("expected cycle limit reached before reset")
	}

	// Имитируем успешный инференс.
	resetReloadAttempts(modelName)

	count, isLimit := getReloadAttempts(modelName)
	if count != 0 {
		t.Errorf("after resetReloadAttempts: count=%d, want 0", count)
	}
	if isLimit {
		t.Errorf("after resetReloadAttempts: isLimit=true, want false")
	}
	t.Logf("OK: counter cleared after reset")
}

// =============================================================================
// Дополнительно: проверка типов ошибок
// =============================================================================

func TestReloadLoopLimitError_Fields(t *testing.T) {
	// ReloadLoopLimitError должен корректно сериализовать model/count/elapsed.
	err := &ReloadLoopLimitError{
		Model:   "test-model",
		Count:   3,
		Elapsed: 45 * time.Second,
	}
	msg := err.Error()
	if !strings.Contains(msg, "test-model") {
		t.Errorf("error must contain model name: %s", msg)
	}
	if !strings.Contains(msg, "3 reloads") {
		t.Errorf("error must contain count: %s", msg)
	}
	if !strings.Contains(msg, "45s") {
		t.Errorf("error must contain elapsed: %s", msg)
	}
	if !isReloadLoopLimitError(err) {
		t.Error("isReloadLoopLimitError must recognize *ReloadLoopLimitError")
	}
}