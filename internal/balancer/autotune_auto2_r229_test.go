// autotune_auto2_r229_test.go — R229 (2026-10-22): «-2» как цель reload'а.
//
// ЖИВОЙ ДЕФЕКТ. AutoTune считает «auto offload» (-2) рекомендацией для слоёв.
// Но после перезагрузки с -2 воркер сообщает ФАКТ — реальное число слоёв
// (например 20), а не -2. Следующий анализ видит «рекомендуется -2, а стоит 20»
// и строит ТОТ ЖЕ план: модель перезагружается снова и снова без видимой причины,
// пока не сработает circuit breaker. Клиент в это время получает 503.
//
// ГРАНИЦА: -2 — это режим воркера («сам реши»), а не значение, которое можно
// «применить». Включать авто должен профиль/панель; автоматика не перезагружает
// модель ради того, чтобы «включить авто» повторно. Конкретное число слоёв
// применяется как раньше, и если план содержит ещё и окно/KV — они сохраняются.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR229_AutoOffloadRecommendationNeverReloads — layers=-2 при факте 20 слоёв:
// плана нет вообще (раньше был бы reload ради «-2»).
func TestR229_AutoOffloadRecommendationNeverReloads(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "layers", CurrentNumGPULayers: 20, RecommendedNumGPULayers: -2},
		},
	}
	loaded := types.LlamaCppModel{Name: "gemma-4-E4B-it-Q4_K_M", NumGPULayers: 20, ContextLength: 12288}
	if plan := PlanApplyAutoTune(a, loaded); plan != nil {
		t.Fatalf("план ради -2 (auto) построен: %+v — это и даёт серию одинаковых reload'ов", plan)
	}
}

// TestR229_AutoOffloadDoesNotSuppressOtherFixes — если вместе с -2 есть полезная
// рекомендация (окно), reload случается, но слои в план НЕ попадают.
func TestR229_AutoOffloadDoesNotSuppressOtherFixes(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "layers", CurrentNumGPULayers: 20, RecommendedNumGPULayers: -2},
			{Category: "context", CurrentNumCtx: 65536, RecommendedNumCtx: 24576},
		},
	}
	loaded := types.LlamaCppModel{Name: "m", NumGPULayers: 20, ContextLength: 65536}
	plan := PlanApplyAutoTune(a, loaded)
	if plan == nil {
		t.Fatal("полезная рекомендация по окну потерялась")
	}
	if plan.ContextSize != 24576 {
		t.Errorf("ContextSize = %d, want 24576", plan.ContextSize)
	}
	if plan.NumGPULayers != 0 {
		t.Errorf("NumGPULayers = %d, want 0 (-2 не должен попадать в план)", plan.NumGPULayers)
	}
}

// TestR229_ConcreteLayerRecommendationStillApplies — обычная рекомендация слоёв
// (конкретное число) не затронута guard'ом.
func TestR229_ConcreteLayerRecommendationStillApplies(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "layers", CurrentNumGPULayers: 20, RecommendedNumGPULayers: 28},
		},
	}
	loaded := types.LlamaCppModel{Name: "m", NumGPULayers: 20, ContextLength: 12288}
	plan := PlanApplyAutoTune(a, loaded)
	if plan == nil {
		t.Fatal("конкретная рекомендация слоёв не применена")
	}
	if plan.NumGPULayers != 28 {
		t.Errorf("NumGPULayers = %d, want 28", plan.NumGPULayers)
	}
}

// TestR416b_AllLayersRecommendationOnFullyOffloadedModel — R416b (2026-11-03):
// layers=-1 при факте «все слои уже на GPU» не должен строить план reload'а.
//
// ЖИВОЙ ДЕФЕКТ. Qwen3.8-9B после R415 загрузился всеми слоями (воркер сообщает
// 33 при 32 слоях), анализ продолжал рекомендовать -1, и балансер выгружал/
// загружал модель на каждом запросе (~80 c). KV-часть плана к этому моменту уже
// отклонял гард R416 — в плане оставался только -1.
func TestR416b_AllLayersRecommendationOnFullyOffloadedModel(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "layers", CurrentNumGPULayers: 33, RecommendedNumGPULayers: -1},
		},
	}
	loaded := types.LlamaCppModel{
		Name:         "Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M",
		NumGPULayers: 33, NLayers: 32, ContextLength: 16384,
	}
	if plan := PlanApplyAutoTune(a, loaded); plan != nil {
		t.Fatalf("план ради -1 на полностью offloaded модели построен: %+v — "+
			"это серия reload'ов на каждом запросе", plan)
	}
}

// TestR416b_AllLayersRecommendationOnPartialOffloadStillApplies — если модель
// выгружена частично, «все слои» — реальная цель, план обязан строиться.
func TestR416b_AllLayersRecommendationOnPartialOffloadStillApplies(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "layers", CurrentNumGPULayers: 20, RecommendedNumGPULayers: -1},
		},
	}
	loaded := types.LlamaCppModel{Name: "m", NumGPULayers: 20, NLayers: 32, ContextLength: 16384}
	plan := PlanApplyAutoTune(a, loaded)
	if plan == nil {
		t.Fatal("частичный offload: рекомендация -1 потеряна")
	}
	if plan.NumGPULayers != -1 {
		t.Errorf("NumGPULayers = %d, want -1", plan.NumGPULayers)
	}
}
