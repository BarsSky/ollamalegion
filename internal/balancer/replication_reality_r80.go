// Package balancer — R80: группа репликации видит реально загруженные копии.
//
// Что было (обнаружено нагрузочным стендом P4, R79):
//
//   - группа репликации знала только те копии, которые загрузил сам менеджер.
//     После рестарта балансера при двух реально загруженных копиях группа была
//     пуста (`total: 0`), контроллер грузил ЛИШНЮЮ копию на «свободный» бэкенд,
//     а трафик `replicated` уходил в одну копию (7:1 по счётчикам cppworker);
//   - когда обе копии уже загружены и свободного VRAM почти нет, VRAM-fit
//     (R75/R79) объявлял «подходящих бэкендов 0» и при `fallback=error`
//     отклонял ВСЕ запросы 503-й — при том, что модель лежала в VRAM.
//
// Решение: единый источник правды «модель загружена на бэкенде» — метрики
// бэкенда (Ollama RunningModels + llama.cpp LoadedModels, как в
// backendHasModel), и передача этого списка в менеджер репликации
// (SetLoadedBackendsFn) — группа усыновляет готовые копии и не плодит лишние.
package balancer

import "ollama-loadbalancer/pkg/types"

// MarkReplicationInstanceLoaded — R81: событийное подтверждение реплики.
//
// cppworker после успешной загрузки модели сам зовёт
// POST /api/v1/internal/llama-model-loaded; подтверждаем инстанс группы сразу,
// не дожидаясь опроса метрик (иначе восстановление реплики ждало тика поллера).
func (p *Proxy) MarkReplicationInstanceLoaded(model, backendID string) {
	if p == nil || p.modelReplication == nil {
		return
	}
	p.modelReplication.AdoptLoadedInstance(model, backendID)
}

// MarkReplicationInstanceUnloaded — R81: событийная выгрузка реплики.
//
// Модель выгружена (idle unload, ручной unload, рестарт cppworker) — инстанс
// группы убираем сразу; если группе не хватает копий, контроллер запросит
// загрузку на следующем тике.
func (p *Proxy) MarkReplicationInstanceUnloaded(model, backendID string) {
	if p == nil || p.modelReplication == nil {
		return
	}
	if p.modelReplication.DropInstance(model, backendID) {
		go p.ensurePlacementInstances()
	}
}

// backendHasModelByID — загружена ли модель на бэкенде по снапшотам метрик.
//
// R80/R81: проверяем ВСЕ доступные источники, потому что «где модель загружена»
// в балансере живёт в трёх местах:
//
//  1. metrics[id].Ollama.RunningModels / .LlamaCpp.LoadedModels (метрики агента);
//  2. metrics[id].Models — объединённый список имён (R32 доклеивает туда
//     LlamaCpp.LoadedModels, см. cluster_state.go);
//  3. llamaMetrics[id].LoadedModels — то, что наполняет поллер cppworker
//     (/api/models, раз в 30 с).
//
// Раньше проверялись только (1) и getModelLoadedCtxFromMetrics (3, но с
// требованием ContextLength > 0). Поллер может отдать загруженную модель без
// context_length — и группа репликации не видела уже загруженную копию
// (замер: два cppworker с моделью в VRAM, группа показывала loading=2/loaded=0).
func (p *Proxy) backendHasModelByID(backendID, modelName string) bool {
	if p == nil || p.metricsMgr == nil || backendID == "" || modelName == "" {
		return false
	}
	if metrics, ok := p.metricsMgr.SnapshotBackendMetrics(backendID); ok && metrics != nil {
		if p.backendHasModel(metrics, modelName) {
			return true
		}
		for _, name := range metrics.Models {
			if name == modelName || containsFold(name, modelName) {
				return true
			}
		}
	}
	if lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics(backendID); ok && lm != nil {
		for _, m := range lm.LoadedModels {
			if m.Name == modelName || containsFold(m.Name, modelName) {
				return true
			}
		}
	}
	return false
}

// backendsWithModelLoaded — ID healthy-бэкендов, на которых модель загружена
// прямо сейчас (для усыновления группой репликации).
func (p *Proxy) backendsWithModelLoaded(modelName string) []string {
	return p.backendsWithModelLoadedFiltered(modelName, true)
}

// backendsWithModelLoadedIncludingUnhealthy — то же, но включая бэкенды в
// статусе unhealthy: нужен, чтобы понять, есть ли ЖИВАЯ копия модели, когда
// часть реплик отвалилась (R80, §6: «один из бэкендов реплик unhealthy» →
// обслуживаем с оставшейся копии, а не отказываем 503-й).
func (p *Proxy) backendsWithModelLoadedIncludingUnhealthy(modelName string) []string {
	return p.backendsWithModelLoadedFiltered(modelName, false)
}

func (p *Proxy) backendsWithModelLoadedFiltered(modelName string, healthyOnly bool) []string {
	if p == nil || modelName == "" {
		return nil
	}
	p.mu.RLock()
	ids := make([]string, 0, len(p.backends))
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if healthyOnly && state.Backend.Status != types.StatusHealthy {
			continue
		}
		ids = append(ids, id)
	}
	p.mu.RUnlock()

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if p.backendHasModelByID(id, modelName) {
			out = append(out, id)
		}
	}
	return out
}
