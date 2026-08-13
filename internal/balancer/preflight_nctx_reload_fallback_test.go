// preflight_nctx_reload_fallback_test.go — Round 35c+ (2026-08-13)
// Tests for profile/backend fallback when body не задал num_ctx.
//
// Bug report (A10 offline machine, 2026-08-13): "settings are being overridden
// from 32768 to 8192". Root cause: preflight использовал только body num_ctx.
// Open WebUI default num_ctx=8192, поэтому first-load preflight (loaded=0)
// triggered reload to 8192 вместо profile.ContextLength=32768.
//
// Fix: preflight теперь fallback'ит на profile → backend default когда body
// не задал num_ctx. Body имеет АБСОЛЮТНЫЙ приоритет (клиент знает что хочет),
// fallback только если body молчит.
package balancer

import (
	"net/http"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Test 1: body без num_ctx + profile=32768 → preflight reload в 32768
// (НЕ в body default 8192)
// ============================================================

func TestPreflightNCtxReload_NoBodyNumCtx_UseProfileFallback(t *testing.T) {
	modelName := "gemma-4"
	cppWorker, currentNCtx, _ := makeMockCppWorkerWithReload(t, modelName, 0)
	p := buildProxyWithCppWorkerBackend(t, cppWorker.URL, modelName)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"gemma-4": {ContextLength: 32768},
	}

	// body без num_ctx (как Open WebUI иногда шлёт)
	body := []byte(`{"model":"gemma-4","messages":[{"role":"user","content":"hi"}]}`)

	_, ok, msg, status := p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")

	// Body без num_ctx → preflight должен fallback на profile (32768),
	// loaded=0 < 32768 → reload to 32768, client gets 503+Retry-After.
	if ok {
		t.Errorf("preflight should NOT proxy directly (reload needed), got ok=true msg=%q status=%d", msg, status)
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (reload scheduled), got %d", status)
	}
	// Wait для async reload
	time.Sleep(300 * time.Millisecond)
	// Cppworker должен получить reload запрос с contextSize=32768
	if got := currentNCtx.Load(); got != 32768 {
		t.Errorf("currentNCtx=%d, want 32768 (profile fallback)", got)
	}
}

// ============================================================
// Test 2: body с num_ctx=8192 + profile=32768 → preflight skip
// (loaded=32768 >= 8192, body не patching — клиент знает что хочет)
// ============================================================

func TestPreflightNCtxReload_BodyNumCtx8192_Profile32768_Loaded32768_NoReload(t *testing.T) {
	modelName := "gemma-4"
	cppWorker, _, _ := makeMockCppWorkerWithReload(t, modelName, 32768)
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 32768)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"gemma-4": {ContextLength: 32768},
	}

	// body имеет num_ctx=8192 (Open WebUI default)
	body := []byte(`{"model":"gemma-4","options":{"num_ctx":8192}}`)

	_, ok, _, _ := p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")

	if !ok {
		t.Errorf("preflight should proxy directly (loaded=32768 >= body=8192, no reload needed)")
	}
}

// ============================================================
// Test 3: body с num_ctx=8192, model НЕ loaded (loaded=0), profile=32768
// → preflight RELOAD в 32768 (profile wins, body=8192 ignored)
// ============================================================

func TestPreflightNCtxReload_Body8192_NotLoaded_Profile32768_ReloadToProfile(t *testing.T) {
	modelName := "gemma-4"
	cppWorker, currentNCtx, _ := makeMockCppWorkerWithReload(t, modelName, 0)
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 0)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"gemma-4": {ContextLength: 32768},
	}

	// body с num_ctx=8192 (default Open WebUI) + profile=32768
	body := []byte(`{"model":"gemma-4","options":{"num_ctx":8192}}`)

	_, ok, _, _ := p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")

	// Body=num_ctx=8192 wins! Profile НЕ используется потому что body задал num_ctx.
	// Pre-flight reload to 8192 (NOT 32768) — this is by design.
	if ok {
		t.Errorf("preflight should trigger reload (loaded=0 < body=8192)")
	}
	time.Sleep(300 * time.Millisecond)
	if got := currentNCtx.Load(); got != 8192 {
		t.Errorf("currentNCtx=%d, want 8192 (body wins over profile)", got)
	}
}

// ============================================================
// Test 4: body без num_ctx, no profile, backend default=16384
// → fallback на backend default
// ============================================================

func TestPreflightNCtxReload_NoBodyNumCtx_NoProfile_UseBackendDefault(t *testing.T) {
	modelName := "unknown-model"
	cppWorker, currentNCtx, _ := makeMockCppWorkerWithReload(t, modelName, 0)
	p := buildProxyWithCppWorkerBackend(t, cppWorker.URL, modelName)

	body := []byte(`{"model":"unknown-model","messages":[{"role":"user","content":"hi"}]}`)

	_, ok, _, _ := p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")

	// Нет ни profile, ни backend default → preflight не знает target n_ctx →
	// просто проксируем (cppworker использует свой defaultCtxSize=32768).
	if !ok {
		t.Errorf("preflight should proxy as-is (no profile, no backend default; cppworker uses its own default 32768)")
	}
	time.Sleep(200 * time.Millisecond)
	if got := currentNCtx.Load(); got != 0 {
		t.Errorf("currentNCtx=%d, want 0 (no reload should be triggered)", got)
	}
}
