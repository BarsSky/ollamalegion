//go:build llama_stub

// per_slot_window_r83_test.go — R83-fix (2026-09-30).
//
// Слоты ДЕЛЯТ окно (llama.cpp, kv_unified=false: n_ctx_seq = PAD(n_ctx/slots, 256)).
// Значит и ограничивать клиентский запрос надо ОКНОМ СЛОТА, а не суммарным:
// модель с суммарным окном 32768 при parallel=2 даёт каждому клиенту 16384, и
// num_ctx=32768 слоту не по силам — хотя формально «≤ суммарного».
//
// Живой контекст: cppworker отдаёт в /api/models оба числа (context_size и
// context_per_seq), балансер хранит их в кэше метрик и обязан использовать
// второе при выборе X-Cpp-Ctx.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// seedLoadedModel — положить в кэш метрик загруженную модель с заданными
// суммарным окном, окном слота и числом слотов.
func seedLoadedModel(t *testing.T, p *Proxy, backendID, model string, total, perSeq, slots int) {
	t.Helper()
	p.metricsMgr.mu.Lock()
	defer p.metricsMgr.mu.Unlock()
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name:          model,
			State:         "loaded",
			ContextLength: total,
			ContextPerSeq: perSeq,
			MaxSlots:      slots,
			Parallel:      slots,
		}},
	}
}

func newProxyWithLoadedModel(t *testing.T, backendID, model string, total, perSeq, slots int) *Proxy {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends[backendID] = &BackendState{Backend: &types.Backend{ID: backendID, Type: types.BackendTypeLlamaCpp}}
	p.nctxReload = NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	seedLoadedModel(t, p, backendID, model, total, perSeq, slots)
	return p
}

// TestR83_ResolveNumCtx_ClampsToPerSlotWindow — клиент просит суммарное окно
// (32768), но слот вмещает 16384 → получает 16384.
func TestR83_ResolveNumCtx_ClampsToPerSlotWindow(t *testing.T) {
	const backendID = "b-perslot"
	const model = "qwen"

	p := newProxyWithLoadedModel(t, backendID, model, 32768, 16384, 2)

	body := []byte(`{"model":"` + model + `","options":{"num_ctx":32768}}`)
	got := p.ResolveNumCtx(model, body, backendID)

	if got.Value != 16384 {
		t.Errorf("ResolveNumCtx = %d, want 16384 (окно слота при parallel=2); "+
			"иначе cppworker примет запрос на 32768, а слот столько не вмещает", got.Value)
	}
}

// TestR83_ResolveNumCtx_PerSlotFromSlotsWhenFieldMissing — старая сборка
// cppworker не отдаёт context_per_seq: считаем сами (суммарное / слоты, PAD256).
func TestR83_ResolveNumCtx_PerSlotFromSlotsWhenFieldMissing(t *testing.T) {
	const backendID = "b-perslot-2"
	const model = "qwen"

	p := newProxyWithLoadedModel(t, backendID, model, 32768, 0, 2) // perSeq неизвестен

	body := []byte(`{"model":"` + model + `","options":{"num_ctx":32000}}`)
	got := p.ResolveNumCtx(model, body, backendID)

	if got.Value != 16384 {
		t.Errorf("ResolveNumCtx = %d, want 16384 (32768/2, выравнивание 256)", got.Value)
	}
}

// TestR83_ResolveNumCtx_SingleSlotKeepsTotal — при одном слоте окно слота равно
// суммарному: ничего не урезаем.
func TestR83_ResolveNumCtx_SingleSlotKeepsTotal(t *testing.T) {
	const backendID = "b-perslot-3"
	const model = "qwen"

	p := newProxyWithLoadedModel(t, backendID, model, 32768, 32768, 1)

	body := []byte(`{"model":"` + model + `","options":{"num_ctx":32768}}`)
	got := p.ResolveNumCtx(model, body, backendID)

	if got.Value != 32768 {
		t.Errorf("ResolveNumCtx = %d, want 32768 (один слот — окно слота = суммарному)", got.Value)
	}
}

// TestR83_LoadedPerSeqNCtx_UsesReportedValue — приоритет у значения от
// cppworker (context_per_seq), а не у собственного расчёта.
func TestR83_LoadedPerSeqNCtx_UsesReportedValue(t *testing.T) {
	const backendID = "b-perslot-4"
	const model = "qwen"

	// llama.cpp выровнял окно до 32256 и слот до 16128 — отдаём именно это.
	p := newProxyWithLoadedModel(t, backendID, model, 32256, 16128, 2)

	if got := p.loadedPerSeqNCtx(backendID, model); got != 16128 {
		t.Errorf("loadedPerSeqNCtx = %d, want 16128 (значение из /api/models)", got)
	}
	// Суммарное окно при этом остаётся суммарным — его использует закрепление
	// настроек и проверка «загружена ли модель».
	if got := p.loadedWindowNCtx(backendID, model); got != 32256 {
		t.Errorf("loadedWindowNCtx = %d, want 32256 (суммарное окно)", got)
	}
}
