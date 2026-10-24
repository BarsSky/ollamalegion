// model_presence_r229_test.go — R229 (2026-10-22): «модели нет нигде» → 404.
//
// ЖИВОЙ ДЕФЕКТ. Нативная Ollama-поверхность отвечала 503 «stale metric snapshot …
// retry» на модель, которой нет ни на одной машине (воркер уже сказал «not found»),
// и клиент уходил в retry-цикл. OpenAI-поверхность в том же случае отдавала 404.
//
// Обратная ошибка тоже недопустима: каталог может быть ещё не опрошен, и тогда
// «файла не видно» ≠ «файла нет» — в этом случае решение НЕ definitive, и балансер
// обязан остаться на 503 с повтором.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func presenceTestProxy(t *testing.T) *Proxy {
	t.Helper()
	p := &Proxy{
		config:       &types.LoadBalancerConfig{},
		backends:     map[string]*BackendState{},
		metricsMgr:   NewMetricsManager(),
		modelCatalog: newModelCatalog(),
	}
	p.backends["main-1"] = &BackendState{Backend: &types.Backend{
		ID: "main-1", Host: "10.0.0.1", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}}
	return p
}

func TestR229_ModelPresenceDefinitiveMissing(t *testing.T) {
	p := presenceTestProxy(t)
	p.modelCatalog.store("main-1", []string{"gemma-4-E4B-it-Q4_K_M"}, "", time.Now())

	if present, definitive := p.modelPresence("no-such-model-xyz"); present || !definitive {
		t.Fatalf("отсутствующая модель: present=%v definitive=%v, want false/true — иначе 404 не будет", present, definitive)
	}
	if present, definitive := p.modelPresence("gemma-4-E4B-it-Q4_K_M"); !present || !definitive {
		t.Fatalf("существующая модель: present=%v definitive=%v, want true/true", present, definitive)
	}
}

func TestR229_ModelPresenceUnknownCatalogIsNotDefinitive(t *testing.T) {
	p := presenceTestProxy(t) // каталог ещё не опрашивался
	if present, definitive := p.modelPresence("no-such-model-xyz"); present || definitive {
		t.Fatalf("без каталога: present=%v definitive=%v, want false/false — 404 был бы догадкой", present, definitive)
	}
}

func TestR229_ModelPresenceLoadedModelCounts(t *testing.T) {
	p := presenceTestProxy(t)
	// Модель загружена, но каталог пуст: файл точно есть — present=true.
	p.metricsMgr.UpdateLlamaCppMetrics("main-1", &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: "loaded-model", State: "loaded"}},
	})
	if present, _ := p.modelPresence("loaded-model"); !present {
		t.Fatal("загруженная модель не признана присутствующей")
	}
}

func TestR229_CatalogModelNamesUnionAndSorted(t *testing.T) {
	p := presenceTestProxy(t)
	p.backends["donor-1"] = &BackendState{Backend: &types.Backend{
		ID: "donor-1", Host: "10.0.0.2", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}}
	p.modelCatalog.store("main-1", []string{"b-model", "a-model"}, "", time.Now())
	p.modelCatalog.store("donor-1", []string{"c-model", "a-model"}, "", time.Now())

	got := p.catalogModelNames()
	want := []string{"a-model", "b-model", "c-model"}
	if len(got) != len(want) {
		t.Fatalf("catalogModelNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("catalogModelNames = %v, want %v (объединение без дублей, сортировка)", got, want)
		}
	}
}
