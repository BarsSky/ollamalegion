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

// backendHasModelByID — загружена ли модель на бэкенде по снапшоту его метрик.
//
// Отличие от getModelLoadedCtxFromMetrics: тот читает только llamaMetrics
// (данные cppworker-поллера с context_length) и требует ContextLength > 0;
// здесь используется общий путь backendHasModel, который видит и Ollama
// RunningModels, и llama.cpp LoadedModels.
func (p *Proxy) backendHasModelByID(backendID, modelName string) bool {
	if p == nil || p.metricsMgr == nil || backendID == "" || modelName == "" {
		return false
	}
	metrics, ok := p.metricsMgr.SnapshotBackendMetrics(backendID)
	if !ok || metrics == nil {
		return false
	}
	return p.backendHasModel(metrics, modelName)
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
		if p.getModelLoadedCtxFromMetrics(id, modelName) > 0 || p.backendHasModelByID(id, modelName) {
			out = append(out, id)
		}
	}
	return out
}
