// nctx_ceiling_r267_test.go — R267 (2026-10-29): ОДИН потолок окна контекста.
//
// ЖИВАЯ ЖАЛОБА. В карточке модели выставлено окно 256000, модель обучена на
// 262144, VRAM хватает — а запрос агента на окно больше env-потолка получал
// отказ. Причина: потолок применялся в двух слоях, и правка обработчика не
// лечила РЕШАТЕЛЬ о перезагрузке (nctx_reload.go), через который идёт типовой
// агентский путь prompt_too_long. Эти тесты фиксируют инвариант:
//
//	решатель не имеет собственного мнения о потолке — он использует то, что
//	ему передали (одна формула: Proxy.effectiveAutoReloadCeiling).
package balancer

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newCeilingTestProxy — прокси с профилем модели и env-потолком.
func newCeilingTestProxy(t *testing.T, model string, profile *types.LlamaCppModelProfile, envCeiling int) *Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{}
	p := &Proxy{config: cfg, backends: map[string]*BackendState{}, metricsMgr: NewMetricsManager()}
	if profile != nil {
		cfg.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{model: *profile}
	}
	p.nctxReload = NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx:    true,
		AutoReloadMaxNCtx: envCeiling,
	})
	return p
}

// TestR267_ResolverUsesDeclaredWindowNotEnvCeiling — главный живой случай:
// профиль 256000 при env-потолке 32768 → решатель НЕ отказывает и НЕ клампит
// до 32768.
func TestR267_ResolverUsesDeclaredWindowNotEnvCeiling(t *testing.T) {
	p := newCeilingTestProxy(t, "qwen3.8-27b", &types.LlamaCppModelProfile{ContextLength: 256000}, 32768)

	ceil := ReloadCeiling{
		Effective: p.effectiveAutoReloadCeiling("qwen3.8-27b", 32768),
		Env:       32768,
	}
	if ceil.Effective != 256000 {
		t.Fatalf("эффективный потолок = %d, want 256000 (заявленное окно оператора)", ceil.Effective)
	}

	// prompt_too_long: нужно 200000 — раньше это был отказ «exceeds configured max=32768».
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodePromptTooLong,
		ActualTokens: 90000,
		NPredict:     10000,
		CurrentNCtx:  32768,
		MaxVRAMNCtx:  0, // физика неизвестна — не должна мешать
	}
	plan := p.nctxReload.DecideReloadBackendWithCeiling("b1", bridgeErr, 0, ceil)
	if plan.Decision != DecisionReload {
		t.Fatalf("decision = %s, want reload; reason=%s", plan.Decision.String(), plan.Reason)
	}
	if plan.NewNCtx < 100000 {
		t.Fatalf("NewNCtx = %d, want >= 100000 (клампа до env-потолка быть не должно)", plan.NewNCtx)
	}
	if plan.LimitedBy != "" {
		t.Errorf("LimitedBy = %q, want пусто (окно не резалось)", plan.LimitedBy)
	}
	if plan.EffectiveCeiling != 256000 {
		t.Errorf("EffectiveCeiling = %d, want 256000", plan.EffectiveCeiling)
	}
}

// TestR267_ClampUsesEffectiveCeiling — кламп-ветка (n_ctx_needs_reload) тоже
// должна считать по эффективному потолку, а не по env.
func TestR267_ClampUsesEffectiveCeiling(t *testing.T) {
	p := newCeilingTestProxy(t, "m", &types.LlamaCppModelProfile{ContextLength: 200000}, 32768)
	ceil := ReloadCeiling{Effective: p.effectiveAutoReloadCeiling("m", 32768), Env: 32768}

	// required 150000, физика позволяет больше, потолок 200000 → клампа нет.
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodeNCtxNeedsReload,
		RequiredNCtx: 150000,
		CurrentNCtx:  32768,
		MaxVRAMNCtx:  1000000,
	}
	plan := p.nctxReload.DecideReloadBackendWithCeiling("b1", bridgeErr, 0, ceil)
	if plan.Decision != DecisionReload {
		t.Fatalf("decision = %s, want reload", plan.Decision.String())
	}
	if plan.NewNCtx < 150000 {
		t.Fatalf("NewNCtx = %d, want >= 150000 (не клампить до 32768)", plan.NewNCtx)
	}

	// required 190000 <= потолка 200000, но roundUpPow2 даёт 262144 → кламп ровно
	// до потолка, с атрибуцией «profile» (потолок поднят заявленным окном).
	bridgeErr2 := &NCtxBridgeError{
		Code:         NCtxErrCodeNCtxNeedsReload,
		RequiredNCtx: 190000,
		CurrentNCtx:  32768,
		MaxVRAMNCtx:  1000000,
	}
	plan2 := p.nctxReload.DecideReloadBackendWithCeiling("b1", bridgeErr2, 0, ceil)
	if plan2.NewNCtx != 200000 {
		t.Fatalf("NewNCtx = %d, want 200000 (эффективный потолок)", plan2.NewNCtx)
	}
	if plan2.LimitedBy != "profile" {
		t.Errorf("LimitedBy = %q, want profile (потолок поднят заявленным окном)", plan2.LimitedBy)
	}
}

// TestR267_PhysicalLimitIsNamedPhysically — физическая нехватка VRAM остаётся
// физической причиной, а не «операторский лимит».
func TestR267_PhysicalLimitIsNamedPhysically(t *testing.T) {
	p := newCeilingTestProxy(t, "m", &types.LlamaCppModelProfile{ContextLength: 256000}, 32768)
	ceil := ReloadCeiling{Effective: p.effectiveAutoReloadCeiling("m", 32768), Env: 32768}

	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodeNCtxNeedsReload,
		RequiredNCtx: 200000,
		CurrentNCtx:  32768,
		MaxVRAMNCtx:  40000, // карта столько не даёт
	}
	plan := p.nctxReload.DecideReloadBackendWithCeiling("b1", bridgeErr, 0, ceil)
	if plan.Decision != DecisionReject {
		t.Fatalf("decision = %s, want reject (физика не позволяет)", plan.Decision.String())
	}
	if plan.LimitedBy != "physical" {
		t.Errorf("LimitedBy = %q, want physical", plan.LimitedBy)
	}
	if !strings.Contains(plan.Reason, "max_vram_n_ctx") {
		t.Errorf("в причине нет физического предела: %s", plan.Reason)
	}
	if !strings.Contains(plan.RejectMsg, `"limited_by":"physical"`) {
		t.Errorf("в теле отказа нет limited_by:physical: %s", plan.RejectMsg)
	}
}

// TestR267_NoProfileKeepsEnvCeiling — без профиля поведение прежнее: env-потолок
// действует и называется своим именем.
func TestR267_NoProfileKeepsEnvCeiling(t *testing.T) {
	p := newCeilingTestProxy(t, "other", nil, 32768)
	ceil := ReloadCeiling{Effective: p.effectiveAutoReloadCeiling("m", 32768), Env: 32768}

	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodePromptTooLong,
		ActualTokens: 40000,
		NPredict:     1000,
		CurrentNCtx:  32768,
		MaxVRAMNCtx:  1000000,
	}
	plan := p.nctxReload.DecideReloadBackendWithCeiling("b1", bridgeErr, 0, ceil)
	if plan.Decision != DecisionReject {
		t.Fatalf("decision = %s, want reject (env-потолок 32768)", plan.Decision.String())
	}
	if plan.LimitedBy != "env" {
		t.Errorf("LimitedBy = %q, want env", plan.LimitedBy)
	}
	if !strings.Contains(plan.Reason, "limited_by=env") {
		t.Errorf("причина не называет env: %s", plan.Reason)
	}
}

// TestR267_LegacyDecideKeepsRawCeiling — старый вызов (без явного потолка)
// сохраняет прежнюю семантику: сырое значение конфига. Это нужно, чтобы
// внешние вызовы/тесты не поменяли поведение молча.
func TestR267_LegacyDecideKeepsRawCeiling(t *testing.T) {
	p := newCeilingTestProxy(t, "qwen3.8-27b", &types.LlamaCppModelProfile{ContextLength: 256000}, 32768)
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodePromptTooLong,
		ActualTokens: 40000,
		NPredict:     1000,
		MaxVRAMNCtx:  1000000,
	}
	plan := p.nctxReload.DecideReloadBackend("b1", bridgeErr, 0)
	if plan.Decision != DecisionReject {
		t.Fatalf("старый вызов не должен поднимать потолок сам: decision=%s", plan.Decision.String())
	}
	if plan.EffectiveCeiling != 32768 {
		t.Errorf("EffectiveCeiling = %d, want 32768 (сырое значение конфига)", plan.EffectiveCeiling)
	}
}

// TestR267_ProfileEffectiveWindowReportsReason — то, что видит WebUI: «заявлено
// X, ограничено Y, причина Z».
func TestR267_ProfileEffectiveWindowReportsReason(t *testing.T) {
	p := newCeilingTestProxy(t, "qwen3.8-27b", &types.LlamaCppModelProfile{ContextLength: 256000}, 131072)

	effective, limitedBy, reason, env := p.ProfileEffectiveWindow("qwen3.8-27b")
	if effective != 256000 {
		t.Errorf("effective = %d, want 256000", effective)
	}
	if limitedBy != "" {
		t.Errorf("limitedBy = %q, want пусто (env 131072 ниже заявленного)", limitedBy)
	}
	if env != 131072 {
		t.Errorf("envCeiling = %d, want 131072", env)
	}
	if reason == "" {
		t.Error("причина не заполнена — оператор снова не поймёт, почему окно такое")
	}

	// Заявленное окно НИЖЕ env-потолка: тогда режет env, и это надо назвать.
	p2 := newCeilingTestProxy(t, "m", &types.LlamaCppModelProfile{ContextLength: 8192}, 131072)
	_, limitedBy2, reason2, _ := p2.ProfileEffectiveWindow("m")
	if limitedBy2 != "env" {
		t.Errorf("limitedBy = %q, want env", limitedBy2)
	}
	if !strings.Contains(reason2, "LB_NCTX_RELOAD_MAX_N_CTX") {
		t.Errorf("причина не называет переменную: %s", reason2)
	}

	// Без профиля — действует env и это тоже названо.
	p3 := newCeilingTestProxy(t, "other", nil, 131072)
	_, limitedBy3, _, env3 := p3.ProfileEffectiveWindow("нет-такой")
	if limitedBy3 != "env" || env3 != 131072 {
		t.Errorf("без профиля: limitedBy=%q env=%d, want env/131072", limitedBy3, env3)
	}
}
