package main

import (
	"net/http/httptest"
	"strconv"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestComputeContextWarning_OK — нормальный случай, prompt < 60% n_ctx.
func TestComputeContextWarning_OK(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()
	backend.InFlight().Reset("m1")
	defer backend.InFlight().Reset("m1")

	// Loaded n_ctx=16384, prompt ~50 tokens (small)
	w, _ := computeWarningForTestLoaded("m1", makeLongPrompt(50), 1024, 0, 16384, 262144)
	if w.Level != contextWarnOK {
		t.Errorf("expected OK, got %q (nCtx=%d pct=%d)", w.Level, w.NCtx, w.UsedPercent)
	}
}

// TestComputeContextWarning_Approaching — prompt > 80% n_ctx.
func TestComputeContextWarning_Approaching(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()
	backend.InFlight().Reset("m1")
	defer backend.InFlight().Reset("m1")

	// 90% of 4096 = 3686 tokens
	w, _ := computeWarningForTestLoaded("m1", makeLongPrompt(3700), 1024, 0, 4096, 262144)
	if w.Level != contextWarnApproach {
		t.Errorf("expected approaching, got %q (nCtx=%d pct=%d)", w.Level, w.NCtx, w.UsedPercent)
	}
	if w.Suggestion == "" {
		t.Error("expected non-empty suggestion for approaching level")
	}
}

// TestComputeContextWarning_Overflow — prompt + n_predict > n_ctx,
// prompt настолько большой, что для n_predict не остаётся места.
func TestComputeContextWarning_Overflow(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()
	backend.InFlight().Reset("m1")
	defer backend.InFlight().Reset("m1")

	// 4032 tokens prompt (16128 chars), n_predict=4096, n_ctx=4096
	// maxOut = 4096-4032-64 = 0 → adjustedNPredict=0 → overflow
	w, adjusted := computeWarningForTestLoaded("m1", makeLongPrompt(4032), 4096, 0, 4096, 262144)
	if w.Level != contextWarnOverflow {
		t.Errorf("expected overflow, got %q (nCtx=%d pct=%d promptTokens=%d)", w.Level, w.NCtx, w.UsedPercent, w.PromptTokens)
	}
	// adjusted n_predict — то что caller должен использовать
	if adjusted != 0 {
		t.Errorf("expected adjusted n_predict=0 for overflow, got %d", adjusted)
	}
	if w.Suggestion == "" {
		t.Error("expected non-empty suggestion for overflow level")
	}
}

// TestComputeContextWarning_Impossible — prompt > n_ctx.
func TestComputeContextWarning_Impossible(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()
	backend.InFlight().Reset("m1")
	defer backend.InFlight().Reset("m1")

	// 5000 tokens prompt > 4096 n_ctx → impossible
	w, adjusted := computeWarningForTestLoaded("m1", makeLongPrompt(5000), 1024, 0, 4096, 262144)
	if w.Level != contextWarnImpossible {
		t.Errorf("expected impossible, got %q", w.Level)
	}
	if adjusted != 0 {
		t.Errorf("expected adjusted=0 for impossible, got %d", adjusted)
	}
}

// TestComputeContextWarning_NCtxOverride — n_ctx из body/header переопределяет loaded.
func TestComputeContextWarning_NCtxOverride(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()
	backend.InFlight().Reset("m1")
	defer backend.InFlight().Reset("m1")

	// Override n_ctx=8192 — prompt 5000 tokens, fits with room
	w, _ := computeWarningForTestLoaded("m1", makeLongPrompt(5000), 1024, 8192, 4096, 262144)
	if w.Level != contextWarnOK && w.Level != contextWarnApproach {
		t.Errorf("expected OK or approaching with override, got %q (nCtx=%d)", w.Level, w.NCtx)
	}
	if w.NCtx != 8192 {
		t.Errorf("expected nCtx=8192 from override, got %d", w.NCtx)
	}
}

// TestComputeContextWarning_NoModelLoaded — нет loaded info → OK.
func TestComputeContextWarning_NoModelLoaded(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	// loadedNCtx=0, no override → OK (функция возвращает OK без loaded info)
	w, _ := computeWarningForTestLoaded("never-loaded", "test", 1024, 0, 0, 0)
	if w.Level != contextWarnOK {
		t.Errorf("expected OK for no loaded info, got %q", w.Level)
	}
}

// TestSetContextWarningHeader_OK_NoHeader — при level=ok headers не ставятся.
func TestSetContextWarningHeader_OK_NoHeader(t *testing.T) {
	w := ContextWarning{Level: contextWarnOK, NCtx: 4096, PromptTokens: 100}
	rec := httptest.NewRecorder()
	SetContextWarningHeader(rec, w)
	if rec.Header().Get("X-Model-Context-Warning") != "" {
		t.Errorf("expected no X-Model-Context-Warning for OK level, got %q",
			rec.Header().Get("X-Model-Context-Warning"))
	}
}

// TestSetContextWarningHeader_Approach — для approaching ставится header с pct.
func TestSetContextWarningHeader_Approach(t *testing.T) {
	w := ContextWarning{
		Level: contextWarnApproach, NCtx: 4096, PromptTokens: 3300,
		NPredict: 700, EffectiveMaxOut: 700, UsedPercent: 80,
		Suggestion: "Test",
	}
	rec := httptest.NewRecorder()
	SetContextWarningHeader(rec, w)
	hdr := rec.Header().Get("X-Model-Context-Warning")
	if hdr == "" {
		t.Fatal("expected X-Model-Context-Warning header")
	}
	// Проверяем что содержит level, nCtx, pct
	if !headerContains(hdr, "approaching") {
		t.Errorf("expected 'approaching' in header, got %q", hdr)
	}
	if !headerContains(hdr, "nCtx=4096") {
		t.Errorf("expected nCtx=4096 in header, got %q", hdr)
	}
	if !headerContains(hdr, "pct=80") {
		t.Errorf("expected pct=80 in header, got %q", hdr)
	}
	if rec.Header().Get("X-Model-Context-Suggestion") == "" {
		t.Error("expected X-Model-Context-Suggestion header")
	}
	if rec.Header().Get("X-Model-Adjusted-NPredict") != strconv.Itoa(w.NPredict) {
		t.Errorf("expected X-Model-Adjusted-NPredict=%d, got %q", w.NPredict,
			rec.Header().Get("X-Model-Adjusted-NPredict"))
	}
}

// TestSetContextWarningHeader_GGUFMax — если gguf_max != n_ctx, добавляем.
func TestSetContextWarningHeader_GGUFMax(t *testing.T) {
	w := ContextWarning{
		Level: contextWarnApproach, NCtx: 4096, GGUFContextMax: 32768,
		PromptTokens: 3300, NPredict: 700, UsedPercent: 80,
	}
	rec := httptest.NewRecorder()
	SetContextWarningHeader(rec, w)
	hdr := rec.Header().Get("X-Model-Context-Warning")
	if !headerContains(hdr, "ggufMax=32768") {
		t.Errorf("expected ggufMax=32768 when different from nCtx, got %q", hdr)
	}
}

// TestSetContextWarningHeader_GGUFMax_Same — если одинаковые, не дублируем.
func TestSetContextWarningHeader_GGUFMax_Same(t *testing.T) {
	w := ContextWarning{
		Level: contextWarnApproach, NCtx: 16384, GGUFContextMax: 16384,
		PromptTokens: 14000, NPredict: 100, UsedPercent: 85,
	}
	rec := httptest.NewRecorder()
	SetContextWarningHeader(rec, w)
	hdr := rec.Header().Get("X-Model-Context-Warning")
	if headerContains(hdr, "ggufMax=") {
		t.Errorf("expected no ggufMax when equal to nCtx, got %q", hdr)
	}
}

// === helpers ===

// computeWarningForTest — обёртка для вызова ComputeContextWarning без полного
// HTTP request context. Использует globals (backend) как реальный handler.
func computeWarningForTest(t *testing.T, modelName, prompt string, nPredict, nCtxOverride int) (ContextWarning, int) {
	t.Helper()
	w, adj := ComputeContextWarning(modelName, prompt, nPredict, nCtxOverride)
	rec := httptest.NewRecorder()
	SetContextWarningHeader(rec, w)
	return w, adj
}

// computeWarningForTestLoaded — direct call to pure-function. Используется для
// тестов, которые не хотят зависеть от реальной loaded model registry
// (stub-build не имеет настоящего LoadModel).
func computeWarningForTestLoaded(modelName, prompt string, nPredict, nCtxOverride, loadedNCtx, ggufMax int) (ContextWarning, int) {
	return computeContextWarningFromLoaded(modelName, prompt, nPredict, nCtxOverride, loadedNCtx, ggufMax)
}

// makeLongPrompt — создаёт строку длиной N символов для имитации большого prompt.
// (Token estimation в test-build ~4 chars/token через stub backend.)
func makeLongPrompt(targetTokens int) string {
	// stub backend: countModelTokensByLoadedInfo returns len([]rune(text)) / 4
	// = N/4 if loaded, иначе N/4
	const charsPerToken = 4
	chars := targetTokens * charsPerToken
	s := make([]byte, chars)
	for i := 0; i < chars; i++ {
		s[i] = 'a'
	}
	return string(s)
}

func headerContains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > len(sub) && indexOfStr(s, sub) >= 0))
}

func indexOfStr(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// loadedAt — DEPRECATED в этом файле. Используется test-isolation
// через computeWarningForTestLoaded (pure-function) вместо реального
// backend.GetModel (который в stub-build возвращает error).
//
// Оставлен как no-op для обратной совместимости с будущими тестами.
func loadedAt(modelName string, nCtx, ggufMax int) {
	_ = modelName
	_ = nCtx
	_ = ggufMax
}

// TestStubBuildNote — placeholder для документирования: stub-build не имеет
// настоящей loaded model registry, поэтому часть тестов может давать
// level=OK вне зависимости от prompt. Это expected — функциональность
// покрыта live-тестами (manual verify в bundled-full).
func TestStubBuildNote(t *testing.T) {
	t.Log("Stub build: some tests rely on live integration with bundled-full")
	_ = cppbackend.ModelInfo{}
}
