// internal/balancer/nctx_round37_test.go — Round 37 (2026-08-18) + Round 43 (2026-08-19) tests.
//
// Защита от regression: 3-tier resolution для modelMaxContext.
//   Round 37: profile.contextLengthAuto → min(profile, feasibleMax).
//   Round 43: profile.contextLengthAuto + contextLengthMax → min(ggufMax, contextLengthMax, feasible).
//             feasible is HINT in auto mode (not cap). profile.ContextLength is HINT, not cap.
//
// Pre-Round 37 профиль был жёстким cap → 413 conservative-profile. Round 37
//   1. profile.contextLengthAuto=false → profile (hard cap, backward compat)
//   2. profile.contextLengthAuto=true  → min(profile, feasibleMax)
//   3. fallback → metrics.ModelMaxContext
//
// Round 43: feasible is HINT (current state, grows after auto-tune), not cap.
//   1. profile.contextLengthAuto=false → min(profile, contextLengthMax) (operator cap respected)
//   2. profile.contextLengthAuto=true  → min(ggufMax, contextLengthMax) (feasible ignored)
//   3. fallback → ggufMax (R43: was metrics.ModelMaxContext)
//   New: profileMaxContext is HINT for initial load, never a cap.
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
		GGUFMaxContext:     262144,
	})

	// profile=32768, auto=false → должно вернуть 32768 (profile жёсткий cap)
	got := p.resolveModelMaxContext("b1", "model", 32768, false, 0, 0)
	if got != 32768 {
		t.Errorf("Tier 1 (profile hard cap): got %d, want 32768", got)
	}
}

// TestResolveModelMaxContext_Tier1_ContextLengthMax_ClampsHardCap — R43 fix:
// contextLengthMax operator cap is respected EVEN in hard-cap mode.
// Pre-R43: contextLengthMax field was in schema but ignored by resolver.
func TestResolveModelMaxContext_Tier1_ContextLengthMax_ClampsHardCap(t *testing.T) {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		GGUFMaxContext: 262144,
	})

	// profile=32768, auto=false, contextLengthMax=16384 (operator says "no more than 16K")
	// → 16384 (contextLengthMax wins over profile)
	got := p.resolveModelMaxContext("b1", "model", 32768, false, 16384, 0)
	if got != 16384 {
		t.Errorf("Tier 1 + contextLengthMax: got %d, want 16384 (operator cap clamps hard cap)", got)
	}
}

// TestResolveModelMaxContext_Tier2_ProfileAutoAdapt — auto=true → feasible wins
// (auto-relax up). Profile is HINT, not cap.
//
// R43 CRITICAL CHANGE: perModelFeasible is HINT, not cap. The real cap is
// min(ggufMax, contextLengthMax). Feasible reflects current gpu_layers state
// and grows after cppworker auto-tunes. Using it as a cap blocks legitimate
// requests that cppworker can actually handle after auto-tune.
//
// Cline case: num_ctx=65536, profile contextLength=32768+auto=true,
// ggufMax=262144, contextLengthMax=131072, current feasible=32719 (gpu_layers=-1).
// Pre-R43: cap=32719 → reject. Post-R43: cap=min(262144, 131072)=131072 → 65536 OK.
func TestResolveModelMaxContext_Tier2_ProfileAutoAdapt(t *testing.T) {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		MaxFeasibleContext: 131072, // top-level fallback
		ModelMaxContext:    32768,
		GGUFMaxContext:     262144, // hard upper bound
	})

	// profile=32768, auto=true, contextLengthMax=131072, feasible=131072
	// R43: cap = min(ggufMax=262144, contextLengthMax=131072) = 131072.
	// Feasible is HINT, ignored. Cline 65536 < 131072 → OK.
	got := p.resolveModelMaxContext("b1", "model", 32768, true, 131072, 131072)
	if got != 131072 {
		t.Errorf("Tier 2 (auto, ggufMax dominates): got %d, want 131072 (min(ggufMax, contextLengthMax) wins)", got)
	}
}

// TestResolveModelMaxContext_Tier2_R43_ProductionBugFix — R43 главный кейс:
// profile conservative, auto=true, feasible is SMALL (current gpu_layers=-1
// не влезает), но ggufMax=262144 → 65536 проходит.
//
// Pre-R37: 32768 hard cap → 413.
// Round 37: cap = min(32768 ignored, 65536 feasible) = 65536 → 65536 OK (но только если feasible >= 65536).
// Round 43: cap = min(ggufMax=262144, contextLengthMax=131072) = 131072 → 65536 OK
//           (даже если feasible=32719, что было с gpu_layers=-1 на 8GB VRAM).
func TestResolveModelMaxContext_Tier2_R43_ProductionBugFix(t *testing.T) {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		MaxFeasibleContext: 32719, // small! current gpu_layers=-1 на 8GB VRAM
		ModelMaxContext:    32768,
		GGUFMaxContext:     262144, // model supports 256K
	})

	// profile=32768, auto=true, contextLengthMax=131072, feasible=32719 (small)
	// R43: cap = min(ggufMax=262144, contextLengthMax=131072) = 131072.
	// Cline 65536 → 65536 < 131072 → OK. cppworker auto-tunes gpu_layers on reload.
	got := p.resolveModelMaxContext("b1", "model", 32768, true, 131072, 32719)
	if got != 131072 {
		t.Errorf("R43 PRODUCTION FIX: got %d, want 131072 (feasible=32719 should be IGNORED in auto mode, cap=min(ggufMax,contextLengthMax))",
			got)
	}
}

// TestResolveModelMaxContext_Tier2_ContextLengthMax_Dominates — contextLengthMax
// operator cap is smaller than ggufMax → contextLengthMax wins.
func TestResolveModelMaxContext_Tier2_ContextLengthMax_Dominates(t *testing.T) {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		GGUFMaxContext: 262144, // model supports 256K
	})

	// profile=32768, auto=true, contextLengthMax=65536 (operator says "no more than 64K")
	// R43: cap = min(ggufMax=262144, contextLengthMax=65536) = 65536.
	got := p.resolveModelMaxContext("b1", "model", 32768, true, 65536, 0)
	if got != 65536 {
		t.Errorf("R43 contextLengthMax clamp: got %d, want 65536 (operator cap wins over ggufMax)", got)
	}
}

// TestResolveModelMaxContext_Tier2_NoContextLengthMax_UsesGGUFMax — operator
// didn't set contextLengthMax → fall through to ggufMax.
func TestResolveModelMaxContext_Tier2_NoContextLengthMax_UsesGGUFMax(t *testing.T) {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		GGUFMaxContext: 262144,
	})

	// profile=32768, auto=true, contextLengthMax=0 (unlimited) → ggufMax wins
	got := p.resolveModelMaxContext("b1", "model", 32768, true, 0, 0)
	if got != 262144 {
		t.Errorf("R43 unlimited: got %d, want 262144 (ggufMax when contextLengthMax=0)", got)
	}
}

// TestResolveModelMaxContext_Tier2_NoGGUFMax_NoContextLengthMax_ReturnsZero —
// крайний случай: ничего не известно. Возвращаем 0 = no cap, cppworker decide.
func TestResolveModelMaxContext_Tier2_NoGGUFMax_NoContextLengthMax_ReturnsZero(t *testing.T) {
	p := setupProxyWithMetrics("b1", nil) // нет metrics
	// profile=32768, auto=true, contextLengthMax=0, ggufMax=0 → return 0 (trust the model)
	got := p.resolveModelMaxContext("b1", "model", 32768, true, 0, 0)
	if got != 0 {
		t.Errorf("R43 trust-the-model: got %d, want 0 (no cap when no data, cppworker decides)", got)
	}
}

// TestResolveModelMaxContext_Tier3_NoProfile_FallsThroughToGGUFMax — R43 fix:
// без profile раньше возвращал metrics.ModelMaxContext (conservative 8192).
// Теперь fall through to ggufMax (262144) — балансер не должен блокировать
// 65536 для моделей без явного profile.
func TestResolveModelMaxContext_Tier3_NoProfile_FallsThroughToGGUFMax(t *testing.T) {
	p := setupProxyWithMetrics("b1", &types.LlamaCppMetrics{
		GGUFMaxContext:  262144, // model's hard upper bound
		ModelMaxContext: 8192,   // old metrics poller (conservative)
	})

	// profile=0 (нет profile), auto irrelevant → fall through to ggufMax
	got := p.resolveModelMaxContext("b1", "model", 0, false, 0, 0)
	if got != 262144 {
		t.Errorf("R43 Tier 3: got %d, want 262144 (ggufMax wins over metrics.ModelMaxContext which is conservative)",
			got)
	}
}

// TestResolveModelMaxContext_NoMetrics_NoGGUF_ReturnsMetrics — final fallback
// when even ggufMax is unknown. Returns metrics.ModelMaxContext (preserves
// backward compat для legacy deployments без Round 37 metrics).
func TestResolveModelMaxContext_NoMetrics_NoGGUF_ReturnsMetrics(t *testing.T) {
	p := setupProxyWithMetrics("b1", nil)
	// profile=0, no metrics → return 0 (preserves old behavior: 0 = no info)
	got := p.resolveModelMaxContext("b1", "model", 0, false, 0, 0)
	if got != 0 {
		t.Errorf("R43 ultimate fallback: got %d, want 0 (no data → 0, no cap)", got)
	}
}

// TestMaxNumCtxForModel_R43_RespectsAutoFlag — R43 CRITICAL FIX:
// maxNumCtxForModel is the CLAMP function for body num_ctx. Pre-R43 it
// returned profile.ContextLength (32768) as hard cap → Cline 65536 was
// CLAMPED to 32768. Post-R43 it delegates to resolveModelMaxContext and
// returns min(ggufMax, contextLengthMax) when auto=true.
//
// IMPORTANT: For Cline asking 65536 < 131072 (the cap), NO clamp happens.
// The cap is a CEILING, not a forced downscale. Cline 65536 passes through
// unchanged. Cline 200000 would be clamped to 131072 (operator policy).
func TestMaxNumCtxForModel_R43_RespectsAutoFlag(t *testing.T) {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: map[string]*types.LlamaCppMetrics{
				"b1": {
					GGUFMaxContext: 262144,
				},
			},
		},
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"Qwen3.6-35B-A3B-UD-Q4_K_M": {
					ContextLength:     32768, // conservative profile (HINT)
					ContextLengthAuto: true,  // auto=true → profile IGNORED
					ContextLengthMax:  131072, // operator soft cap
				},
			},
		},
	}
	// auto=true + contextLengthMax=131072 → return 131072 (cap).
	// Cline 65536 < 131072 → no clamp, request passes through.
	// Cline 200000 > 131072 → clamp to 131072.
	got := p.maxNumCtxForModel("Qwen3.6-35B-A3B-UD-Q4_K_M", "b1")
	if got != 131072 {
		t.Errorf("R43 maxNumCtxForModel auto: got %d, want 131072 (auto=true + contextLengthMax=131072 → cap=131072, profile=32768 ignored)",
			got)
	}
}

// TestMaxNumCtxForModel_R43_AutoMode_NoClampBelowCap — explicit: confirms
// that for auto mode, body num_ctx=65536 < cap=131072 does NOT trigger clamp.
func TestMaxNumCtxForModel_R43_AutoMode_NoClampBelowCap(t *testing.T) {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: map[string]*types.LlamaCppMetrics{
				"b1": {GGUFMaxContext: 262144},
			},
		},
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"m1": {
					ContextLength:     32768, // HINT, ignored in auto
					ContextLengthAuto: true,
					ContextLengthMax:  131072,
				},
			},
		},
	}
	cap := p.maxNumCtxForModel("m1", "b1")
	if cap != 131072 {
		t.Fatalf("cap = %d, want 131072", cap)
	}
	// Simulate Cline 65536:
	clineBody := 65536
	if cap > 0 && clineBody > cap {
		t.Errorf("Cline 65536 should NOT be clamped (cap=131072), but logic would clamp")
	}
}

// TestMaxNumCtxForModel_R43_HardCap_StaysCap — backward compat:
// auto=false → profile.ContextLength is hard cap (Cline 65536 clamped to 32768).
func TestMaxNumCtxForModel_R43_HardCap_StaysCap(t *testing.T) {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: map[string]*types.LlamaCppMetrics{
				"b1": {
					GGUFMaxContext: 262144,
				},
			},
		},
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"old-model": {
					ContextLength:     32768,
					ContextLengthAuto: false, // backward compat: hard cap
					ContextLengthMax:  0,
				},
			},
		},
	}
	got := p.maxNumCtxForModel("old-model", "b1")
	if got != 32768 {
		t.Errorf("R43 maxNumCtxForModel hard cap: got %d, want 32768 (auto=false → profile is hard cap, backward compat)",
			got)
	}
}

// TestMaxNumCtxForModel_R43_NoProfile_UsesGGUFMax — R43: для моделей без
// profile раньше возвращал defaultModelProfile (32768 conservative).
// Теперь возвращает ggufMax (262144) — Cline 65536 на новой модели
// не будет clamp'иться к 32768.
func TestMaxNumCtxForModel_R43_NoProfile_UsesGGUFMax(t *testing.T) {
	p := &Proxy{
		metricsMgr: &MetricsManager{
			llamaMetrics: map[string]*types.LlamaCppMetrics{
				"b1": {
					GGUFMaxContext: 262144,
				},
			},
		},
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{},
		},
	}
	got := p.maxNumCtxForModel("new-model-no-profile", "b1")
	if got != 262144 {
		t.Errorf("R43 maxNumCtxForModel no profile: got %d, want 262144 (ggufMax when no profile)",
			got)
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
							FeasibleMaxContext: 65536, // per-model feasible (HINT in R43)
							GGUFMaxContext:     262144, // per-model GGUF (R43 cap)
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
					ContextLength:     32768, // conservative profile (HINT in R43)
					ContextLengthAuto: true,  // auto-adapt enabled
					ContextLengthMax:  131072, // operator soft cap (R43: applied!)
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

	// 3-tier R43: profile=32768, auto=true, contextLengthMax=131072, ggufMax=262144
	// → cap = min(ggufMax=262144, contextLengthMax=131072) = 131072.
	// Feasible=65536 is HINT, ignored in auto mode.
	if state.ModelMaxContext != 131072 {
		t.Errorf("ModelMaxContext = %d, want 131072 (R43: min(ggufMax, contextLengthMax) in auto mode)",
			state.ModelMaxContext)
	}
}

// TestCollectPreflightState_Round37_AutoRelaxes — production bug case.
// Pre-R37: 32768 hard cap → 413 для num_ctx=65536.
// R37: auto=true → feasible=65536 wins, profile ignored.
// R43: feasible is HINT, cap = min(ggufMax, contextLengthMax).
//
// Setup: top-level ggufMax set (realistic — cppworker reports it on top-level
// when a model is loaded). Per-model also set (cppworker reports both).
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
					// top-level: cppworker also reports here (same value in practice)
					GGUFMaxContext: 262144,
				},
			},
		},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{}),
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"m1": {
					ContextLength:     32768, // conservative
					ContextLengthAuto: true,  // auto=true → relax!
					ContextLengthMax:  0,     // unlimited
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

	// R43: feasible=65536 ignored (HINT), cap = min(ggufMax=262144, contextLengthMax=0=unlimited) = 262144.
	if state.ModelMaxContext != 262144 {
		t.Errorf("R43: ModelMaxContext = %d, want 262144 (auto=true, no contextLengthMax → ggufMax wins)",
			state.ModelMaxContext)
	}
}
