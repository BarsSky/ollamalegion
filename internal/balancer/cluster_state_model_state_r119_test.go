// R119: metrics.Models (список «что реально загружено на бэкенде», его читают
// монитор и страница бэкендов) не должен включать loading-модель.
//
// Живой дефект (2026-10-15, стенд): cppworker сообщал state="loading",
// context_size=0, а в UI модель числилась загруженной. Раньше hasLoadedModels
// считался как len(LoadedModels) > 0 — одной loading-записи хватало, чтобы
// подать её как загруженную и заодно скрыть Ollama-проекцию.
//
// Тест падает до правки: loading-модель попадала в Metrics.Models.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// setLoadedModels — подменить снапшот поллера для бэкенда.
func setLoadedModels(t *testing.T, p *Proxy, id string, models []types.LlamaCppModel) {
	t.Helper()
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics[id] = &types.LlamaCppMetrics{LoadedModels: models}
	p.metricsMgr.mu.Unlock()
}

func TestR119_ClusterState_ModelsExcludeLoading(t *testing.T) {
	p := newLoadedModelsProxy(t)
	const id = "cppworker-gpu-bundled-agent"

	setLoadedModels(t, p, id, []types.LlamaCppModel{
		{Name: "m-loaded", State: "loaded", ContextLength: 16384},
		{Name: "m-loading", State: "loading", ContextLength: 0},
	})

	got := clusterBackend(t, p, id)
	if len(got.Models) != 1 || got.Models[0] != "m-loaded" {
		t.Errorf("metrics.models = %v, ожидался только m-loaded: "+
			"loading-модель не обслуживает запросы (state=loading, ctx=0)", got.Models)
	}
	// Сырой снапшот обязан сохранить обе записи: UI рисует loading-карточку
	// из loadedModels, а /api/ps фильтрует по state сам.
	if n := len(got.LlamaCpp.LoadedModels); n != 2 {
		t.Errorf("loadedModels = %d, ожидались обе записи (факт от поллера не теряем)", n)
	}
}

// Модель только с state=loading не должна подавлять Ollama-проекцию: пока
// ничего не загружено, проекция — единственный источник данных о модели.
func TestR119_ClusterState_LoadingOnlyKeepsProjection(t *testing.T) {
	p := newLoadedModelsProxy(t)
	const id = "cppworker-gpu-bundled-agent"

	p.updateRunningModelInMetrics(id, "m-projecting")
	setLoadedModels(t, p, id, []types.LlamaCppModel{
		{Name: "m-loading", State: "loading"},
	})

	got := clusterBackend(t, p, id)
	if len(got.Ollama.RunningModels) != 1 {
		t.Errorf("runningModels = %d, ожидалась 1 запись: loading-модель — не повод "+
			"скрывать проекцию (загруженных моделей нет)", len(got.Ollama.RunningModels))
	}
	for _, name := range got.Models {
		if name == "m-loading" {
			t.Errorf("metrics.models = %v содержит loading-модель", got.Models)
		}
	}
}
