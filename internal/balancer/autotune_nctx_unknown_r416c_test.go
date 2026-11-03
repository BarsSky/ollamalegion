// autotune_nctx_unknown_r416c_test.go — R416c (2026-11-03): рекомендация по окну
// при НЕИЗВЕСТНОМ текущем n_ctx не должна строить план reload'а.
//
// ЖИВОЙ ДЕФЕКТ. Qwen3.8-9B только что загружен всеми слоями (33/32, ctx=16384).
// Метрики ещё не успели обновиться (ContextLength=0), анализ видит
// «рекомендуется 16384» и строит план с ContextSize=16384. План считается
// изменением (NeedsReload: ContextSize > 0), балансер выгружает и снова грузит
// модель ПОСЛЕ КАЖДОГО ответа — клиент видит «модель тормозит», хотя окно уже
// ровно такое.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR416c_UnknownCurrentContext_NoPlan — текущее окно неизвестно (0), а
// рекомендация 16384: плана нет.
func TestR416c_UnknownCurrentContext_NoPlan(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "context", RecommendedNumCtx: 16384},
		},
	}
	loaded := types.LlamaCppModel{Name: "m", ContextLength: 0, NLayers: 32, NumGPULayers: 20}
	if plan := PlanApplyAutoTune(a, loaded); plan != nil {
		t.Fatalf("план при неизвестном текущем n_ctx построен: %+v — это reload после каждого ответа", plan)
	}
}

// TestR416c_KnownCurrentContext_PlanStillBuilt — известное текущее окно 8192,
// рекомендация 16384: настоящий рост, план обязан строиться.
func TestR416c_KnownCurrentContext_PlanStillBuilt(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "context", RecommendedNumCtx: 16384},
		},
	}
	loaded := types.LlamaCppModel{Name: "m", ContextLength: 8192, NLayers: 32, NumGPULayers: 20}
	plan := PlanApplyAutoTune(a, loaded)
	if plan == nil {
		t.Fatal("настоящий рост окна потерян")
	}
	if plan.ContextSize != 16384 {
		t.Fatalf("ContextSize = %d, want 16384", plan.ContextSize)
	}
}

// TestR416c_EqualCurrentContextNoPlan — окно уже равно рекомендации: плана нет.
func TestR416c_EqualCurrentContextNoPlan(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "context", RecommendedNumCtx: 16384},
		},
	}
	loaded := types.LlamaCppModel{Name: "m", ContextLength: 16384, NLayers: 32, NumGPULayers: 33}
	if plan := PlanApplyAutoTune(a, loaded); plan != nil {
		t.Fatalf("план при равном окне построен: %+v", plan)
	}
}
