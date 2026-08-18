// internal/balancer/nctx_round37_test.go — Round 37 (2026-08-18) tests.
//
// Защита от regression: 3-tier resolution для modelMaxContext. Pre-Round 37
// профиль был жёстким cap → 413 conservative-profile. Round 37:
//   1. profile.contextLengthAuto=false → profile (hard cap, старый behaviour)
//   2. profile.contextLengthAuto=true  → min(profile, feasibleMax)
//   3. fallback → metrics.ModelMaxContext
//go:build llama_stub

package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// setupProxyWithMetrics — helper для тестов resolveModelMaxContext.
// Создаёт Proxy с metricsMgr и заполняет llamaMetrics для backendID.
func setupProxyWithMetrics(backendID string, lm *types.LlamaCppMetrics) *Proxy {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: make(map[string]*types.LlamaCppMetrics),
		},
	}
	if lm != nil {
		p.metricsMgr.llamaMetrics[backendID] = lm
	}
	return p
}

// TestResolveModelMaxContext_Tier1_ProfileHardCap — auto=false → profile wins.
func TestResolveModelMaxContext_Tier1_ProfileHardCap(t *testing.T) {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		MaxFeasibleContext: 131072, // больше profile
		ModelMaxContext:    32768,
	})

	// profile=32768, auto=false → должно вернуть 32768 (profile жёсткий cap)
	got := p.resolveModelMaxContext("b1", "model", 32768, false, 0)
	if got != 32768 {
		t.Errorf("Tier 1 (profile hard cap): got %d, want 32768", got)
	}
}

// TestResolveModelMaxContext_Tier2_ProfileAutoAdapt — auto=true → feasible wins
// (auto-relax up). Profile is HINT, not cap.
func TestResolveModelMaxContext_Tier2_ProfileAutoAdapt(t *testing.T) {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		MaxFeasibleContext: 131072,
		ModelMaxContext:    32768,
	})

	// profile=32768, auto=true, feasible=131072 → 131072 (auto-relax, profile is hint)
	got := p.resolveModelMaxContext("b1", "model", 32768, true, 131072)
	if got != 131072 {
		t.Errorf("Tier 2 (auto, profile < feasible): got %d, want 131072 (feasible wins, profile is hint)", got)
	}
}

// TestResolveModelMaxContext_Tier2_AutoRelaxesUp — главный кейс Round 37:
// profile conservative, auto=true → feasible wins (auto-relax up).
func TestResolveModelMaxContext_Tier2_AutoRelaxesUp(t *testing.T) {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		MaxFeasibleContext: 65536, // > profile
		ModelMaxContext:    32768,
	})

	// profile=32768 (conservative), auto=true, feasible=65536 → auto-relax to 65536
	got := p.resolveModelMaxContext("b1", "model", 32768, true, 65536)
	if got != 65536 {
		t.Errorf("Tier 2 (auto-relax): got %d, want 65536 (feasible should win when larger)", got)
	}
}

// TestResolveModelMaxContext_Tier3_NoProfile_FallbackToMetrics — без profile.
func TestResolveModelMaxContext_Tier3_NoProfile_FallbackToMetrics(t *testing.T) {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		MaxFeasibleContext: 0,    // cppworker didn't report
		ModelMaxContext:    8192, // from old metrics poller
	})

	// profile=0 (нет profile), auto irrelevant → fallback на metrics ModelMaxContext
	got := p.resolveModelMaxContext("b1", "model", 0, false, 0)
	if got != 8192 {
		t.Errorf("Tier 3 (no profile, fallback): got %d, want 8192", got)
	}
}

// TestResolveModelMaxContext_NoMetrics_ReturnsZero — совсем нет данных.
func TestResolveModelMaxContext_NoMetrics_ReturnsZero(t *testing.T) {
	p := setupProxyWithMetrics("b1", nil) // нет metrics
	got := p.resolveModelMaxContext("b1", "model", 0, false, 0)
	if got != 0 {
		t.Errorf("no data: got %d, want 0", got)
	}
}

// TestCollectPreflightState_Round37_3Tier — проверяем что collectPreflightState
// заполняет state.MaxFeasibleContext + state.GGUFMaxContext.
func TestCollectPreflightState_Round37_3Tier(t *testing.T) {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: map[string]*types.LlamaCppMetrics{
				"b1": {
					LoadedModels: []types.LlamaCppModel{
						{
							Name:               "Qwen3.6-35B-A3B-UD-Q4_K_M",
							FeasibleMaxContext: 65536, // per-model feasible
							GGUFMaxContext:     262144, // per-model GGUF
						},
					},
					MaxFeasibleContext: 32768, // top-level (будет overridden per-model)
					GGUFMaxContext:     131072,
				},
			},
		},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{}),
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"Qwen3.6-35B-A3B-UD-Q4_K_M": {
					ContextLength:     32768, // conservative profile
					ContextLengthAuto: true,  // auto-adapt enabled
				},
			},
		},
	}
	// Set initial n_ctx
	p.nctxReload.SetLastKnownNCtx("b1", 32768)

	lr := &LlamaCppRouter{proxy: p}
	state := lr.collectPreflightState("b1", "Qwen3.6-35B-A3B-UD-Q4_K_M")
	if state == nil {
		t.Fatal("state is nil")
	}

	// per-model feasible должен перезаписать top-level
	if state.MaxFeasibleContext != 65536 {
		t.Errorf("MaxFeasibleContext = %d, want 65536 (per-model overrides top-level)", state.MaxFeasibleContext)
	}
	if state.GGUFMaxContext != 262144 {
		t.Errorf("GGUFMaxContext = %d, want 262144 (per-model)", state.GGUFMaxContext)
	}

	// 3-tier resolution: profile=32768, auto=true, feasible=65536 → 65536 (auto-relax).
	// (profile is hint, feasible is the real cap. When auto=true, profile is ignored.)
	if state.ModelMaxContext != 65536 {
		t.Errorf("ModelMaxContext = %d, want 65536 (3-tier: profile=32768 ignored, feasible=65536 wins when auto=true)",
			state.ModelMaxContext)
	}
}

// TestCollectPreflightState_Round37_AutoRelaxes — production bug case:
// profile=32768, auto=true, feasible=65536 → auto-relax to 65536.
//
// Это ГЛАВНЫЙ кейс Round 37. Pre-bug: 32768 hard cap → 413 для num_ctx=65536.
// Post-fix: auto=true → feasible=65536 wins, profile ignored.
func TestCollectPreflightState_Round37_AutoRelaxes(t *testing.T) {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: map[string]*types.LlamaCppMetrics{
				"b1": {
					LoadedModels: []types.LlamaCppModel{
						{
							Name:               "m1",
							FeasibleMaxContext: 65536,
							GGUFMaxContext:     262144,
						},
					},
				},
			},
		},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{}),
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"m1": {
					ContextLength:     32768, // conservative
					ContextLengthAuto: true,  // auto=true → relax!
				},
			},
		},
	}
	p.nctxReload.SetLastKnownNCtx("b1", 32768)

	lr := &LlamaCppRouter{proxy: p}
	state := lr.collectPreflightState("b1", "m1")
	if state == nil {
		t.Fatal("state is nil")
	}

	// Round 37 fixed: profile=32768 ignored, feasible=65536 wins.
	if state.ModelMaxContext != 65536 {
		t.Errorf("PRODUCTION BUG: ModelMaxContext = %d, want 65536 (auto=true should auto-relax to feasible)",
			state.ModelMaxContext)
	}
}
