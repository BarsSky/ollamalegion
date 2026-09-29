// autotune_preserve_context_r83_test.go — R83 (2026-09-29).
//
// ДЕФЕКТ (живой лог 2026-09-29, строки 33/86):
//
//	autotune: triggering async reload ... reason "R54.4: kv_cache f16 → q4_0
//	(quality fix)", new_context_size=0
//
// План менял ТОЛЬКО тип KV-кэша, ContextSize оставался нулём, ApplyAutoTunePlan
// при нуле НЕ кладёт contextSize в запрос — и cppworker поднимал модель со своим
// env-дефолтом (CPPWORKER_CTX_SIZE=32768) ВМЕСТО фактически загруженных 131072.
//
// Дальше оператор видел «после запроса от Cline модель принудительно сброшена до
// 32768», хотя Cline просил 128000, а балансер ещё долго считал, что загружено
// 128000 (его LastKnownNCtx не обновлялся) — и каждый запрос получал done-чанк с
// ошибкой code 2.
//
// Здесь фиксируется контракт: при reload ради kv_cache/layers план обязан нести
// ФАКТИЧЕСКИЙ контекст модели, а не 0 (0 означает «взять env-дефолт»).
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR83_PlanApplyAutoTune_KvCacheChangeKeepsContext — reload ради kv_cache не
// должен ронять контекст до env-дефолта.
func TestR83_PlanApplyAutoTune_KvCacheChangeKeepsContext(t *testing.T) {
	analysis := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "kv_cache", RecommendedKVCache: "q4_0"},
		},
	}
	loaded := types.LlamaCppModel{
		Name:          "qwen3.8:latest",
		ContextLength: 131072, // фактически загружено
		KvCacheType:   "f16",  // рекомендация требует смены на q4_0
	}

	plan := PlanApplyAutoTune(analysis, loaded)
	if plan == nil {
		t.Fatal("ожидался план reload (kv_cache меняется)")
	}
	if plan.KVCacheType != "q4_0" {
		t.Fatalf("KVCacheType = %q, want q4_0", plan.KVCacheType)
	}
	if plan.ContextSize != 131072 {
		t.Errorf("ContextSize = %d, want 131072 (фактически загруженный): "+
			"0 означает «взять env-дефолт CPPWORKER_CTX_SIZE», из-за чего живой стенд "+
			"получал принудительный откат 131072 → 32768 при правке одного kv_cache",
			plan.ContextSize)
	}
}

// TestR83_PlanApplyAutoTune_LayersChangeKeepsContext — то же для gpu_layers.
func TestR83_PlanApplyAutoTune_LayersChangeKeepsContext(t *testing.T) {
	analysis := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "layers", RecommendedNumGPULayers: 24},
		},
	}
	loaded := types.LlamaCppModel{Name: "m", ContextLength: 65536, NumGPULayers: 20}

	plan := PlanApplyAutoTune(analysis, loaded)
	if plan == nil {
		t.Fatal("ожидался план reload (gpu_layers меняется)")
	}
	if plan.ContextSize != 65536 {
		t.Errorf("ContextSize = %d, want 65536 (сохраняем загруженный при правке слоёв)",
			plan.ContextSize)
	}
}

// TestR83_PlanApplyAutoTune_ContextRecommendationStillWins — если autotune реально
// рекомендует ДРУГОЙ контекст, именно он и должен попасть в план (наша правка не
// подменяет осмысленную рекомендацию текущим значением).
func TestR83_PlanApplyAutoTune_ContextRecommendationStillWins(t *testing.T) {
	analysis := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "context", CurrentNumCtx: 65536, RecommendedNumCtx: 49152},
		},
	}
	loaded := types.LlamaCppModel{Name: "m", ContextLength: 65536}

	plan := PlanApplyAutoTune(analysis, loaded)
	if plan == nil {
		t.Fatal("ожидался план reload (контекст меняется)")
	}
	if plan.ContextSize != 49152 {
		t.Errorf("ContextSize = %d, want 49152 (рекомендация autotune имеет приоритет)",
			plan.ContextSize)
	}
}
