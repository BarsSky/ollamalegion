// cancel_targets_streams_r359_test.go — R359 (2026-10-31), аудит abort A3.
//
// ЖИВОЙ ДЕФЕКТ. `cancelTargets` выбирал адресатов отмены по кэшу поллера
// (`llamaMetrics.LoadedModels`, до 30 с устаревания) и требовал `Status == healthy`.
// Отсюда два сценария «кнопка Stop не сработала»:
//
//  1. лаг кэша: модель только что поднялась/переехала на бэкенд B, в кэше она ещё на A —
//     отмена уходит на A, а на B генерация продолжается;
//  2. фильтр здоровья: у работающего llama.cpp-бэкенда статус не healthy (например
//     ollama_unavailable без Ollama-порта) — приказ ему не отправляется вообще.
//
// R359 добавляет к выбору ЖИВОЙ учёт стримов балансера (active_streams_r271.go):
// если стрим по этой модели идёт, бэкенд — адресат, независимо от кэша и статуса.
//
// Проверяется:
//  1. бэкенд с активным стримом попадает в адресаты, даже если кэш про него молчит;
//  2. бэкенд с активным стримом попадает в адресаты, даже если статус не healthy;
//  3. прежнее поведение сохранено: загруженная по кэшу модель на здоровом бэкенде —
//     адресат, дублей нет;
//  4. модель, которой нет ни в кэше, ни в стримах, — приказ всем (как раньше);
//  5. имя сравнивается «мягко»: клиент с .gguf/тегом всё равно находит живой стрим.
package balancer

import (
	"context"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// r359Proxy — прокси с бэкендами и (опционально) активным стримом.
func r359Proxy(t *testing.T, backends []types.Backend) *Proxy {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	for i := range backends {
		b := backends[i]
		p.backends[b.ID] = &BackendState{Backend: &b}
	}
	return p
}

// r359LoadModel — положить модель в кэш поллера для бэкенда.
func r359LoadModel(p *Proxy, backendID, model string) {
	p.metricsMgr.mu.Lock()
	defer p.metricsMgr.mu.Unlock()
	lm := p.metricsMgr.llamaMetrics[backendID]
	if lm == nil {
		lm = &types.LlamaCppMetrics{}
		p.metricsMgr.llamaMetrics[backendID] = lm
	}
	lm.LoadedModels = append(lm.LoadedModels, types.LlamaCppModel{Name: model, State: "loaded"})
}

// r359StartStream — учесть активный клиентский стрим.
func r359StartStream(t *testing.T, p *Proxy, backendID, model string) {
	t.Helper()
	done := beginClientStreamR271(p, backendID, model)
	t.Cleanup(done)
	// Дать учёту зафиксироваться не нужно (это синхронная операция), но держим ссылку
	// на время теста — завершение произойдёт в Cleanup.
	_ = context.Background()
	_ = time.Second
}

// TestR359_StreamBackendIsTargetDespiteStaleCache — главный сценарий лага кэша.
func TestR359_StreamBackendIsTargetDespiteStaleCache(t *testing.T) {
	p := r359Proxy(t, []types.Backend{
		{ID: "a", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
		{ID: "b", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	// Кэш говорит «модель на a», а реально стрим идёт на b.
	r359LoadModel(p, "a", "gemma-4-E4B-it-Q4_K_M")
	r359StartStream(t, p, "b", "gemma-4-E4B-it-Q4_K_M")

	targets := p.cancelTargets("gemma-4-E4B-it-Q4_K_M")
	hasA, hasB := false, false
	for _, id := range targets {
		switch id {
		case "a":
			hasA = true
		case "b":
			hasB = true
		}
	}
	if !hasA {
		t.Errorf("адресаты %v не содержат a (кэш поллера), want содержать", targets)
	}
	if !hasB {
		t.Fatalf("адресаты %v не содержат b, где стрим РЕАЛЬНО идёт: отмена уйдёт в "+
			"пустоту, а генерация на b продолжится (аудит A3, лаг кэша)", targets)
	}
}

// TestR359_UnhealthyBackendWithStreamIsTarget — фильтр здоровья не должен отнимать
// приказ у бэкенда, который очевидно работает (на нём идёт стрим).
func TestR359_UnhealthyBackendWithStreamIsTarget(t *testing.T) {
	p := r359Proxy(t, []types.Backend{
		{ID: "b", Type: types.BackendTypeLlamaCpp, Status: types.StatusOllamaUnavailable},
	})
	r359StartStream(t, p, "b", "gemma-4-E4B-it-Q4_K_M")

	targets := p.cancelTargets("gemma-4-E4B-it-Q4_K_M")
	found := false
	for _, id := range targets {
		if id == "b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("адресаты %v не содержат b со статусом %q, хотя на нём идёт стрим: "+
			"у работающего llama.cpp-бэкенда статус может быть ollama_unavailable "+
			"(нет Ollama-порта), и Stop до него не доходил", targets, types.StatusOllamaUnavailable)
	}
}

// TestR359_NoDuplicatesAndCacheStillWorks — прежнее поведение сохранено.
func TestR359_NoDuplicatesAndCacheStillWorks(t *testing.T) {
	p := r359Proxy(t, []types.Backend{
		{ID: "a", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	r359LoadModel(p, "a", "gemma-4-E4B-it-Q4_K_M")
	r359StartStream(t, p, "a", "gemma-4-E4B-it-Q4_K_M")

	targets := p.cancelTargets("gemma-4-E4B-it-Q4_K_M")
	count := 0
	for _, id := range targets {
		if id == "a" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("бэкенд a встречается %d раз в %v, want 1 (кэш + стрим не должны "+
			"порождать дубли приказов)", count, targets)
	}
}

// TestR359_UnknownModelStillBroadcasts — модель не найдена нигде: приказ всем, как раньше.
func TestR359_UnknownModelStillBroadcasts(t *testing.T) {
	p := r359Proxy(t, []types.Backend{
		{ID: "a", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
		{ID: "b", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	targets := p.cancelTargets("unknown-model")
	if len(targets) != 2 {
		t.Fatalf("адресаты %v, want обоих (неизвестная модель → приказ всем)", targets)
	}
}

// TestR359_SoftModelNameMatch — клиент с .gguf находит живой стрим.
func TestR359_SoftModelNameMatch(t *testing.T) {
	p := r359Proxy(t, []types.Backend{
		{ID: "b", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	r359StartStream(t, p, "b", "gemma-4-E4B-it-Q4_K_M")

	targets := p.cancelTargets("gemma-4-E4B-it-Q4_K_M.gguf")
	found := false
	for _, id := range targets {
		if id == "b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("адресаты %v не содержат b: клиент назвал модель с расширением .gguf, "+
			"а стрим зарегистрирован под каноническим именем", targets)
	}
}
