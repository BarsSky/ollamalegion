// kv_plan_profile_r707.go — R707 (2026-11-25): раскладка KV в профиле
// потребителя (R659/AutoTune).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ХЕЛПЕР. Профиль модели (ModelProfileInfo) собирается в ЧЕТЫРЁХ
// местах: поллер /api/models, анализ AutoTune, reload AutoTune и R659 (выбор узла
// под загрузку). Ровно на таком «четыре места — одно поле» в этом коде уже
// случался дефект (R660: геометрию KV воркер считал, а в /api/models не отдавал;
// R45: per-model feasible парсился и выбрасывался). Поэтому раскладка
// переносится ОДНОЙ функцией, и её вызов обязателен на каждом пути — тогда
// «в решении плоская формула, а в ответе послойная» невозможно по построению.
package balancer

import "ollama-loadbalancer/pkg/types"

// applyKVPlanToProfileR707 — перенести раскладку KV из модели воркера в профиль.
//
// nil-модель допустима: тогда профиль остаётся плоским (прежняя формула).
func applyKVPlanToProfileR707(prof *ModelProfileInfo, m *types.LlamaCppModel) {
	if prof == nil || m == nil {
		return
	}
	prof.KVPlanSource = m.KVPlanSource
	prof.KVGlobalLayers = m.KVGlobalLayers
	prof.KVSWALayers = m.KVSWALayers
	prof.KVSWAWindow = m.KVSWAWindow
	prof.HeadDimSWA = m.HeadDimSWA
	prof.HeadDimSWAV = m.HeadDimSWAV
	prof.NKvHeadsSWA = m.NKvHeadsSWA
}
