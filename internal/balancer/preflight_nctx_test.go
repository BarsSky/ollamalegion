// preflight_nctx_test.go — unit-тесты для preflight n_ctx check.
package balancer

import (
	"context"
		"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ============================================================
// EstimatePromptTokens
// ============================================================

func TestEstimatePromptTokens_Empty(t *testing.T) {
	if got := EstimatePromptTokens(""); got != 0 {
		t.Errorf("expected 0 for empty prompt, got %d", got)
	}
}

func TestEstimatePromptTokens_OneRune(t *testing.T) {
	// 1 rune → 1 token (минимум 1).
	if got := EstimatePromptTokens("a"); got != 1 {
		t.Errorf("expected 1 for single rune, got %d", got)
	}
}

func TestEstimatePromptTokens_HundredChars(t *testing.T) {
	// 100 ASCII chars → 25 токенов (4 chars/token).
	prompt := strings.Repeat("a", 100)
	if got := EstimatePromptTokens(prompt); got != 25 {
		t.Errorf("expected 25 for 100 chars, got %d", got)
	}
}

func TestEstimatePromptTokens_Cyrillic(t *testing.T) {
	// 100 cyrillic chars (2 bytes each in UTF-8) → должно работать по rune count.
	prompt := strings.Repeat("я", 100)
	got := EstimatePromptTokens(prompt)
	if got != 25 {
		t.Errorf("expected 25 for 100 cyrillic runes, got %d", got)
	}
}

// ============================================================
// ExtractRequestMeta
// ============================================================

func TestExtractRequestMeta_OllamaChat(t *testing.T) {
	body := []byte(`{
		"model": "gemma-4-E4B-it-Q4_K_M",
		"messages": [
			{"role":"system","content":"You are helpful"},
			{"role":"user","content":"hello world"}
		],
		"options":{"num_ctx":16384,"num_predict":512}
	}`)
	meta := ExtractRequestMeta(body, "/api/chat")
	if meta == nil {
		t.Fatal("expected meta, got nil")
	}
	if meta.ModelName != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("wrong model: %s", meta.ModelName)
	}
	if meta.RequestedNCtxOverride != 16384 {
		t.Errorf("wrong num_ctx: %d", meta.RequestedNCtxOverride)
	}
	if meta.RequestedNPredict != 512 {
		t.Errorf("wrong num_predict: %d", meta.RequestedNPredict)
	}
	if meta.HasTools {
		t.Error("HasTools should be false")
	}
	if meta.EstimatedPromptTokens == 0 {
		t.Error("expected non-zero EstimatedPromptTokens")
	}
}

func TestExtractRequestMeta_OllamaGenerate(t *testing.T) {
	body := []byte(`{
		"model":"gemma-4",
		"prompt":"Explain quantum physics",
		"system":"You are a physicist",
		"options":{"num_ctx":8192}
	}`)
	meta := ExtractRequestMeta(body, "/api/generate")
	if meta == nil {
		t.Fatal("expected meta, got nil")
	}
	if meta.ModelName != "gemma-4" {
		t.Errorf("wrong model: %s", meta.ModelName)
	}
	if meta.RequestedNCtxOverride != 8192 {
		t.Errorf("wrong num_ctx: %d", meta.RequestedNCtxOverride)
	}
	if meta.EstimatedPromptTokens == 0 {
		t.Error("expected non-zero EstimatedPromptTokens")
	}
}

func TestExtractRequestMeta_OpenAIChat_WithTools(t *testing.T) {
	body := []byte(`{
		"model":"gpt-oss-120b",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"search"}}],
		"max_tokens":2048
	}`)
	meta := ExtractRequestMeta(body, "/v1/chat/completions")
	if meta == nil {
		t.Fatal("expected meta, got nil")
	}
	if meta.ModelName != "gpt-oss-120b" {
		t.Errorf("wrong model: %s", meta.ModelName)
	}
	if !meta.HasTools {
		t.Error("HasTools should be true")
	}
	if meta.RequestedNPredict != 2048 {
		t.Errorf("wrong max_tokens: %d", meta.RequestedNPredict)
	}
}

func TestExtractRequestMeta_UnknownPath(t *testing.T) {
	body := []byte(`{"foo":"bar"}`)
	if meta := ExtractRequestMeta(body, "/api/tags"); meta != nil {
		t.Errorf("expected nil for unknown path, got %+v", meta)
	}
}

func TestExtractRequestMeta_InvalidJSON(t *testing.T) {
	body := []byte(`not json`)
	if meta := ExtractRequestMeta(body, "/api/chat"); meta != nil {
		t.Errorf("expected nil for invalid JSON, got %+v", meta)
	}
}

func TestExtractRequestMeta_EmptyBody(t *testing.T) {
	if meta := ExtractRequestMeta(nil, "/api/chat"); meta != nil {
		t.Errorf("expected nil for empty body, got %+v", meta)
	}
}

// ============================================================
// DecidePreflight
// ============================================================

func TestDecidePreflight_NoOp_CurrentNCtxCovers(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens: 1000,
		RequestedNPredict:     512,
	}
	state := &NCtxBackendState{
		BackendID:   "cppworker-1",
		CurrentNCtx: 8192, // 1000 + 512 + 100 (10% slack) + 1 = 1613 < 8192
		MaxVRAMNCtx: 32768,
	}
	cfg := DefaultNCtxReloadConfig()
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightNoOp {
		t.Errorf("expected PreflightNoOp, got %v", res.Decision)
	}
}

// TestDecidePreflight_NoOp_ParamsMismatchButCtxCovers — Round 53.2
// (2026-08-24) regression test. До R53.2: если клиент прислал options.kv_cache_type="q4_0",
// а модель загружена с "f16" → reload, даже если n_ctx покрывает. R53.2:
// флаговые различия ИГНОРИРУЮТСЯ, решение только по n_ctx.
//
// Сценарий: модель загружена с CurrentNCtx=8192, kv_cache_type="f16".
// Клиент присылает запрос с n_ctx=4096 (fits), но с requested kv_cache_type="q4_0"
// (mismatch). Post-R53.2: PreflightNoOp — reload НЕ нужен.
// Pre-R53.2: PreflightReload — избыточный reload модели.
func TestDecidePreflight_NoOp_ParamsMismatchButCtxCovers(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens:   1000,
		RequestedNPredict:       512,
		RequestedNCtxOverride:   0, // не указан
		RequestedKvCacheType:    "q4_0", // MISMATCH с current f16
		RequestedFlashAttnType:  1,      // MISMATCH с current -1
		RequestedUseMmap:        testBoolPtr(false), // MISMATCH с current true
	}
	state := &NCtxBackendState{
		BackendID:          "cppworker-1",
		CurrentNCtx:        8192, // 1000 + 512 + 100 + 1 = 1613 < 8192, fits
		MaxVRAMNCtx:        32768,
		CurrentKvCacheType: "f16",
		CurrentFlashAttnType: -1,
		CurrentUseMmap:     true,
	}
	cfg := DefaultNCtxReloadConfig()
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightNoOp {
		t.Errorf("Round 53.2: expected PreflightNoOp (ctx covers, params mismatch IGNORED), got %v — R53.2 regression",
			res.Decision)
	}
}

// TestDecidePreflight_Reload_NCtxTooSmall_IgnoresParams — Round 53.2 sanity check.
// Если n_ctx не хватает, reload срабатывает независимо от того, совпадают ли
// флаги (потому что target n_ctx вычисляется из required, не из params).
func TestDecidePreflight_Reload_NCtxTooSmall_IgnoresParams(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens: 100000, // огромный prompt
		RequestedNPredict:     4096,
		RequestedKvCacheType:  "f16", // matches current
		RequestedFlashAttnType: -1,   // matches current
	}
	state := &NCtxBackendState{
		BackendID:            "cppworker-1",
		CurrentNCtx:          8192, // 100000 + 4096 + 10000 (slack) + 1 > 8192
		MaxVRAMNCtx:          65536,
		ModelMaxContext:      262144,
		CurrentKvCacheType:   "f16",
		CurrentFlashAttnType: -1,
	}
	cfg := DefaultNCtxReloadConfig()
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReload {
		t.Errorf("expected PreflightReload (n_ctx too small), got %v", res.Decision)
	}
}

// testBoolPtr — local helper для optional *bool в RequestMeta.RequestedUseMmap.
// Конфликтует с boolPtr из profiles_persistence_test.go, поэтому префикс test.
func testBoolPtr(b bool) *bool { return &b }

func TestDecidePreflight_Reload_FitsInVRAM(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens: 50000, // ~200KB символов
		RequestedNPredict:     512,
	}
	state := &NCtxBackendState{
		BackendID:       "cppworker-1",
		CurrentNCtx:     8192,
		MaxVRAMNCtx:     65536,
		ModelMaxContext: 262144,
	}
	cfg := DefaultNCtxReloadConfig()
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReload {
		t.Fatalf("expected PreflightReload, got %v (reason: %s)", res.Decision, res.RejectBody)
	}
	// required = 50000 + 512 + 1 + 5000 (slack) = 55513 → roundUp → 65536
	if res.TargetNCtx < 55513 {
		t.Errorf("expected target >= 55513, got %d", res.TargetNCtx)
	}
}

func TestDecidePreflight_Reload_VRAMLimit_TriggersPartialOffload(t *testing.T) {
	// ВАЖНО (2026-06-24): при required > max_vram_n_ctx*safety мы БОЛЬШЕ НЕ
	// reject, а trigger reload — cppworker применит AutoTuneNCtx для partial
	// offload (уменьшит gpu_layers, вытеснит часть весов в RAM, освободит VRAM
	// для KV-cache большего размера).
	meta := &RequestMeta{
		EstimatedPromptTokens: 60000,
		RequestedNPredict:     512,
	}
	state := &NCtxBackendState{
		BackendID:       "cppworker-1",
		CurrentNCtx:     8192,
		MaxVRAMNCtx:     32768,
		ModelMaxContext: 262144,
	}
	cfg := DefaultNCtxReloadConfig()
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReload {
		t.Fatalf("expected PreflightReload (partial offload), got %v (reason: %s)", res.Decision, res.RejectBody)
	}
	// required = 60000 + 512 + 1 + 6000 (slack) = 66513 → roundUp → 131072
	if res.TargetNCtx < 66513 {
		t.Errorf("expected target >= 66513, got %d", res.TargetNCtx)
	}
}

func TestDecidePreflight_Reject_ModelMaxLimit(t *testing.T) {
	// Required = 300000 + 512 + 1 + 30000 (slack) = 330513 > gemma-4 max 262144.
	meta := &RequestMeta{
		EstimatedPromptTokens: 300000,
		RequestedNPredict:     512,
	}
	state := &NCtxBackendState{
		BackendID:       "cppworker-1",
		CurrentNCtx:     32768,
		MaxVRAMNCtx:     524288, // искусственно большой VRAM
		ModelMaxContext: 262144, // gemma-4 max
	}
	cfg := DefaultNCtxReloadConfig()
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReject {
		t.Fatalf("expected PreflightReject, got %v", res.Decision)
	}
	if !strings.Contains(res.RejectBody, "model max context") {
		t.Errorf("expected reason about model max context, got: %s", res.RejectBody)
	}
}

func TestDecidePreflight_Reject_ConfigMax(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens: 100000,
		RequestedNPredict:     512,
	}
	state := &NCtxBackendState{
		BackendID:   "cppworker-1",
		CurrentNCtx: 32768,
		MaxVRAMNCtx: 524288,
	}
	cfg := NCtxReloadConfig{
		AutoReloadNCtx:    true,
		AutoReloadMaxNCtx: 65536,
	}
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReject {
		t.Fatalf("expected PreflightReject, got %v", res.Decision)
	}
	if !strings.Contains(res.RejectBody, "auto_reload_max_n_ctx") {
		t.Errorf("expected reason about auto_reload_max_n_ctx, got: %s", res.RejectBody)
	}
}

func TestDecidePreflight_NilInputs(t *testing.T) {
	cfg := DefaultNCtxReloadConfig()
	if res := DecidePreflight(nil, nil, cfg); res.Decision != PreflightNoOp {
		t.Errorf("nil inputs → expected NoOp, got %v", res.Decision)
	}
}

func TestDecidePreflight_DefaultNPredictWhenZero(t *testing.T) {
	// Current 4096 еле покрывает required=3149 (1000 + 2048 default + 1 + 100 slack).
	// Чтобы получить Reload — увеличим prompt, чтобы он не влезал.
	meta := &RequestMeta{
		EstimatedPromptTokens: 8000,
		RequestedNPredict:     0, // default 2048
	}
	state := &NCtxBackendState{
		BackendID:   "cppworker-1",
		CurrentNCtx: 4096,
		MaxVRAMNCtx: 32768,
	}
	cfg := DefaultNCtxReloadConfig()
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReload {
		t.Errorf("expected PreflightReload, got %v", res.Decision)
	}
	// required = 8000 + 2048 (default) + 1 + 800 (slack) = 10849 → 16384
	if res.TargetNCtx < 10849 {
		t.Errorf("expected target >= 10849, got %d", res.TargetNCtx)
	}
}

// ============================================================
// RoundUpPow2 (exported alias)
// ============================================================

func TestRoundUpPow2_Preflight(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 512},
		{-5, 512},
		{1, 512},
		{512, 512},
		{513, 1024},
		{1024, 1024},
		{1025, 2048},
		{4096, 4096},
		{4097, 8192},
		{32768, 32768},
		{65535, 65536},
		{100000, 131072},
	}
	for _, tc := range cases {
		if got := RoundUpPow2(tc.in); got != tc.want {
			t.Errorf("RoundUpPow2(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// ============================================================
// RunPreflight с mock NCtxReloadHTTPClient
// ============================================================

type mockReloadClient struct {
	calls    atomic.Int32
	respCode int
	respBody string
}

func (m *mockReloadClient) PostReload(ctx context.Context, endpoint string, payload []byte) (*http.Response, error) {
	m.calls.Add(1)
	w := httptest.NewRecorder()
	w.Code = m.respCode
	if m.respBody == "" {
		w.Body.WriteString(`{"context_size": 65536, "status": "reloaded"}`)
	} else {
		w.Body.WriteString(m.respBody)
	}
	return w.Result(), nil
}

func TestRunPreflight_NoOp_ShortPrompt(t *testing.T) {
	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	mock := &mockReloadClient{respCode: 200}
	meta := &RequestMeta{EstimatedPromptTokens: 100, RequestedNPredict: 100}
	state := &NCtxBackendState{BackendID: "b1", CurrentNCtx: 4096, MaxVRAMNCtx: 32768}
	res, err := coord.RunPreflight(context.Background(), "b1", "http://backend",
		meta, state, mock)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Decision != PreflightNoOp {
		t.Errorf("expected NoOp, got %v", res.Decision)
	}
	if mock.calls.Load() != 0 {
		t.Errorf("reload should not be called, got %d calls", mock.calls.Load())
	}
}

func TestRunPreflight_Reload_Success(t *testing.T) {
	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	mock := &mockReloadClient{respCode: 200}
	meta := &RequestMeta{
		ModelName:             "gemma-4",
		EstimatedPromptTokens: 50000,
		RequestedNPredict:     512,
		HasTools:              true,
	}
	state := &NCtxBackendState{
		BackendID:       "b1",
		CurrentNCtx:     8192,
		MaxVRAMNCtx:     65536,
		ModelMaxContext: 262144,
	}
	res, err := coord.RunPreflight(context.Background(), "b1", "http://backend",
		meta, state, mock)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Decision != PreflightReload {
		t.Errorf("expected Reload, got %v", res.Decision)
	}
	if mock.calls.Load() != 1 {
		t.Errorf("expected 1 reload call, got %d", mock.calls.Load())
	}
	// lastKnownNCtx должен обновиться.
	if got := coord.LastKnownNCtx("b1"); got != res.TargetNCtx {
		t.Errorf("expected lastKnownNCtx=%d, got %d", res.TargetNCtx, got)
	}
}

func TestRunPreflight_Reject_VRAMExceeded(t *testing.T) {
	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	mock := &mockReloadClient{respCode: 200}
	meta := &RequestMeta{
		ModelName:             "gemma-4",
		EstimatedPromptTokens: 100000,
		RequestedNPredict:     2048,
	}
	state := &NCtxBackendState{
		BackendID:       "b1",
		CurrentNCtx:     8192,
		MaxVRAMNCtx:     32768,
		ModelMaxContext: 262144,
	}
	res, err := coord.RunPreflight(context.Background(), "b1", "http://backend",
		meta, state, mock)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Decision != PreflightReload {
		t.Errorf("expected PreflightReload (partial offload), got %v", res.Decision)
	}
	if mock.calls.Load() != 1 {
		t.Errorf("expected 1 reload call for partial offload, got %d", mock.calls.Load())
	}
}

func TestRunPreflight_KillSwitch(t *testing.T) {
	cfg := DefaultNCtxReloadConfig()
	cfg.AutoReloadNCtx = false // kill switch
	coord := NewNCtxReloadCoordinator(cfg)
	mock := &mockReloadClient{respCode: 200}
	meta := &RequestMeta{
		ModelName:             "gemma-4",
		EstimatedPromptTokens: 50000,
		RequestedNPredict:     512,
	}
	state := &NCtxBackendState{
		BackendID:       "b1",
		CurrentNCtx:     8192,
		MaxVRAMNCtx:     65536,
		ModelMaxContext: 262144,
	}
	res, err := coord.RunPreflight(context.Background(), "b1", "http://backend",
		meta, state, mock)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Decision != PreflightReject {
		t.Errorf("expected Reject when kill-switch off, got %v", res.Decision)
	}
	if !strings.Contains(res.RejectBody, "auto_reload_n_ctx is disabled") {
		t.Errorf("expected reason about disabled flag, got: %s", res.RejectBody)
	}
}

// ============================================================
// AutoReloadAllowToolsEnabled
// ============================================================

func TestAutoReloadAllowToolsEnabled_BothFlags(t *testing.T) {
	cfg := NCtxReloadConfig{AutoReloadNCtx: true, AutoReloadAllowTools: true}
	if !cfg.AutoReloadAllowToolsEnabled() {
		t.Error("expected true when both flags on")
	}
}

func TestAutoReloadAllowToolsEnabled_KillSwitch(t *testing.T) {
	cfg := NCtxReloadConfig{AutoReloadNCtx: false, AutoReloadAllowTools: true}
	if cfg.AutoReloadAllowToolsEnabled() {
		t.Error("expected false when global kill-switch off")
	}
}

func TestAutoReloadAllowToolsEnabled_LocalOff(t *testing.T) {
	cfg := NCtxReloadConfig{AutoReloadNCtx: true, AutoReloadAllowTools: false}
	if cfg.AutoReloadAllowToolsEnabled() {
		t.Error("expected false when local flag off")
	}
}

// ============================================================
// Real-world scenario: Cline → prompt 55111 + n_predict 512, current 32K, max_vram 32K
// ============================================================

func TestPreflight_RealClineScenario(t *testing.T) {
	// Cline scenario: prompt_tokens=55111 + n_predict=512 + 1 = 55624 > n_ctx=32768.
	// current=32768, max_vram=32719. VRAM НЕ позволяет расширить → reject.
	meta := &RequestMeta{
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		EstimatedPromptTokens: 55111,
		RequestedNPredict:     512,
		HasTools:              true,
	}
	state := &NCtxBackendState{
		BackendID:       "cppworker-gpu",
		CurrentNCtx:     32768,
		MaxVRAMNCtx:     32719,
		ModelMaxContext: 262144,
	}
	cfg := DefaultNCtxReloadConfig()
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReload {
		t.Fatalf("expected PreflightReload (partial offload), got %v (target=%d, reason: %s)",
			res.Decision, res.TargetNCtx, res.RejectBody)
	}
	// required = 55111 + 512 + 1 + 5511 (slack) = 61135
	// safeMax = 32719 * 0.85 = 27811
	// 61135 > 27811 → trigger reload (не reject!).
	if res.TargetNCtx < 61135 {
		t.Errorf("expected target >= 61135, got %d", res.TargetNCtx)
	}
}

func TestPreflight_RealClineScenario_VRAMIncreased(t *testing.T) {
	// Тот же сценарий, но пользователь поднял VRAM (например, 80GB A100).
	meta := &RequestMeta{
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		EstimatedPromptTokens: 55111,
		RequestedNPredict:     512,
	}
	state := &NCtxBackendState{
		BackendID:       "cppworker-a100",
		CurrentNCtx:     32768,
		MaxVRAMNCtx:     200000, // 200K ctx помещается
		ModelMaxContext: 262144,
	}
	cfg := DefaultNCtxReloadConfig()
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReload {
		t.Fatalf("expected Reload, got %v", res.Decision)
	}
	// required ≈ 61135 → target должен покрывать.
	if res.TargetNCtx < 61135 {
		t.Errorf("expected target >= 61135, got %d", res.TargetNCtx)
	}
}
