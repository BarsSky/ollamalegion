// preflight_nctx_r6031_test.go — TDD tests for R60.31 stickiness fix.
//
// R60.31 (2026-09-10): убрать reload loop, который возникает когда
// OpenWebUI посылает разные num_ctx в разных запросах. Текущая
// логика (preflight_nctx.go:335-360) trigger reload ВСЕГДА когда
// RequestedNCtxOverride > CurrentNCtx, без проверки required <= CurrentNCtx.
//
// Ollama и LM Studio: n_ctx sticky, нет auto-reload. Наш проект:
// auto-reload РАЗРЕШЁН (фича для удобства), но с защитой от loop.
//
// Stickiness rule: если loaded >= required → noop, даже если
// client указал меньший num_ctx. Loaded >= requested всегда
// работает.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestPreflight_R6031_Stickiness_LoadedCoversRequest — главная
// гипотеза R60.31. Loaded=8192, client num_ctx=2048, required=500.
// Loaded покрывает required, NO reload. (Round 34 баг: trigger
// reload потому что 8192 > 2048.)
func TestPreflight_R6031_Stickiness_LoadedCoversRequest(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens:   100,
		RequestedNPredict:       400, // required = 100+400+1+10 = 511
		RequestedNCtxOverride:   2048, // client thinks small is enough
	}
	state := &NCtxBackendState{
		BackendID:         "cppworker-gpu-bundled-agent",
		CurrentNCtx:       8192, // loaded 4x what client asked
		ModelMaxContext:   262144,
		MaxVRAMNCtx:       30000,
	}
	cfg := NCtxReloadConfig{
		AutoReloadMaxNCtx: 131072,
	}
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightNoOp {
		t.Errorf("loaded(8192) >= required(511), client num_ctx=2048 — should be NoOp, got Decision=%d TargetNCtx=%d",
			res.Decision, res.TargetNCtx)
	}
}

// TestPreflight_R6031_NoDownwardReload — Round 34 баг #2.
// Loaded=8192, client num_ctx=2048 — НЕ ДЕЛАТЬ reload вниз до 2048.
// (Round 34 trigger reload потому что 8192 > 2048 — уменьшает n_ctx,
// что не имеет смысла: prompt+gen влезет в 8192.)
func TestPreflight_R6031_NoDownwardReload(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens:   50,
		RequestedNPredict:       100, // required = 50+100+1+5 = 156
		RequestedNCtxOverride:   1024, // client asks for tiny n_ctx
	}
	state := &NCtxBackendState{
		BackendID:         "cppworker-gpu-bundled-agent",
		CurrentNCtx:       8192, // loaded BIG, client wants small
		ModelMaxContext:   262144,
		MaxVRAMNCtx:       30000,
	}
	cfg := NCtxReloadConfig{
		AutoReloadMaxNCtx: 131072,
	}
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightNoOp {
		t.Errorf("loaded(8192) >= required(156), client num_ctx=1024 — should be NoOp (no downward reload), got Decision=%d TargetNCtx=%d",
			res.Decision, res.TargetNCtx)
	}
}

// TestPreflight_R6031_UpgradeStillWorks — Sanity check: если
// loaded ДЕЙСТВИТЕЛЬНО мало (required > loaded), reload всё ещё
// происходит. Stickiness не ломает upgrade path.
func TestPreflight_R6031_UpgradeStillWorks(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens:   5000,
		RequestedNPredict:       3000, // required = 5000+3000+1+500 = 8501
		RequestedNCtxOverride:   16384,
	}
	state := &NCtxBackendState{
		BackendID:         "cppworker-gpu-bundled-agent",
		CurrentNCtx:       2048, // loaded TOO SMALL for required=8501
		ModelMaxContext:   262144,
		MaxVRAMNCtx:       30000,
	}
	cfg := NCtxReloadConfig{
		AutoReloadMaxNCtx: 131072,
	}
	res := DecidePreflight(meta, state, cfg)
	if res.Decision == PreflightNoOp {
		t.Errorf("loaded(2048) < required(8501) — should reload, got NoOp")
	}
	if res.TargetNCtx < 8192 {
		t.Errorf("TargetNCtx=%d, expected >= 8192 (roundUpPow2(8501)≈16384, but no-downward means >= loaded * 2)",
			res.TargetNCtx)
	}
}

// TestPreflight_R6031_NoExplicitClientOverride — если клиент
// НЕ указал num_ctx (override=0), поведение как раньше (без R60.31).
// required > loaded → reload.
func TestPreflight_R6031_NoExplicitClientOverride(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens:   5000,
		RequestedNPredict:       3000, // required = 8501
		RequestedNCtxOverride:   0,    // client didn't specify
	}
	state := &NCtxBackendState{
		BackendID:         "cppworker-gpu-bundled-agent",
		CurrentNCtx:       2048,
		ModelMaxContext:   262144,
		MaxVRAMNCtx:       30000,
	}
	cfg := NCtxReloadConfig{
		AutoReloadMaxNCtx: 131072,
	}
	res := DecidePreflight(meta, state, cfg)
	if res.Decision == PreflightNoOp {
		t.Errorf("required(8501) > loaded(2048) — should reload, got NoOp")
	}
}

// TestPreflight_R6031_ForwardStickiness — loaded=2048, request
// num_ctx=4096. Если required=500, loaded покрывает → NoOp.
// Если required=3000, loaded НЕ покрывает → reload 4096 (НЕ 5000).
func TestPreflight_R6031_ForwardStickiness(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens:   200,
		RequestedNPredict:       300, // required = 200+300+1+20 = 521
		RequestedNCtxOverride:   4096, // client asks more than loaded
	}
	state := &NCtxBackendState{
		BackendID:         "cppworker-gpu-bundled-agent",
		CurrentNCtx:       2048, // loaded 2x required → покрывает
		ModelMaxContext:   262144,
		MaxVRAMNCtx:       30000,
	}
	cfg := NCtxReloadConfig{
		AutoReloadMaxNCtx: 131072,
	}
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightNoOp {
		t.Errorf("loaded(2048) >= required(521) — should be NoOp (stickiness), got Decision=%d TargetNCtx=%d",
			res.Decision, res.TargetNCtx)
	}
}

// TestPreflight_R6031_ClientRequestExceedsLoaded — loaded=2048,
// client num_ctx=8192, required=500. Loaded покрывает required, но
// client явно просит 8192. R60.31: NoOp (stickiness, НЕ downgrade
// и НЕ upgrade — loaded OK).
func TestPreflight_R6031_ClientRequestExceedsLoaded(t *testing.T) {
	meta := &RequestMeta{
		EstimatedPromptTokens:   100,
		RequestedNPredict:       400, // required = 511
		RequestedNCtxOverride:   8192, // client asks 16x loaded
	}
	state := &NCtxBackendState{
		BackendID:         "cppworker-gpu-bundled-agent",
		CurrentNCtx:       2048, // loaded 4x required → покрывает
		ModelMaxContext:   262144,
		MaxVRAMNCtx:       30000,
	}
	cfg := NCtxReloadConfig{
		AutoReloadMaxNCtx: 131072,
	}
	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightNoOp {
		t.Errorf("loaded(2048) >= required(511) — should be NoOp (stickiness), got Decision=%d TargetNCtx=%d",
			res.Decision, res.TargetNCtx)
	}
}

// silence unused import warning for types
var _ = types.BalancingSettings{}
