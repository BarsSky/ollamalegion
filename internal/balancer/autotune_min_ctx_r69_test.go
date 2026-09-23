// autotune_min_ctx_r69_test.go — R69 (2026-09-23): AutoTune не имеет права
// уменьшать n_ctx ниже того, что просит клиент.
//
// ЖИВОЙ СТЕНД (логи, Cline с num_ctx=65536):
//
//	[nctx_reload] reload successful, new n_ctx=65536
//	autotune: async reload succeeded   reason="R54.4: n_ctx 65536 → 38385 (over-allocation fix)"
//	autotune: triggering async reload  reason="R54.4: n_ctx 65536 → 38385 (over-allocation fix)"
//	…cppworker: "requested n_ctx=65536 exceeds model's effective n_ctx=32768"
//	nctx_reload: R60.47 async reload kicked off, returning 503+Retry-After
//
// То есть балансер сам себе устраивал ping-pong: клиент просит 65536 → модель
// грузится на 65536 → AutoTune «оптимизирует» до 38385 → следующий запрос
// клиента снова требует 65536 → reload → 503. Клиент (Cline) не мог получить
// ни одного ответа.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// overAllocationAnalysisR69 — анализ с рекомендацией уменьшить n_ctx.
func overAllocationAnalysisR69() *AutoTuneAnalysis {
	return &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{
				Category:          "context",
				RecommendedNumCtx: 38385,
				Message:           "n_ctx=65536 сильно больше feasible_max=38385 (over-allocation)",
			},
		},
	}
}

// TestPlanApplyAutoTune_R69_SkipsDowngradeBelowClientRequest — главный кейс:
// рекомендация 38385 при клиентском запросе 65536 → плана нет (downgrade
// пропущен), поэтому reload не запускается.
func TestPlanApplyAutoTune_R69_SkipsDowngradeBelowClientRequest(t *testing.T) {
	loaded := types.LlamaCppModel{Name: "gemma", ContextLength: 65536}
	plan := PlanApplyAutoTuneWithMinContext(overAllocationAnalysisR69(), loaded, 65536)
	if plan != nil {
		t.Fatalf("R69: downgrade ниже клиентского запроса не должен планироваться, получено %+v", plan)
	}
}

// TestPlanApplyAutoTune_R69_LegacyStillDowngrades — обратная совместимость:
// без знания о клиентском запросе (minContext=0) поведение прежнее.
func TestPlanApplyAutoTune_R69_LegacyStillDowngrades(t *testing.T) {
	loaded := types.LlamaCppModel{Name: "gemma", ContextLength: 65536}
	plan := PlanApplyAutoTuneWithMinContext(overAllocationAnalysisR69(), loaded, 0)
	if plan == nil || plan.ContextSize != 38385 {
		t.Fatalf("legacy-режим: ожидался downgrade до 38385, получено %+v", plan)
	}
	// Публичная обёртка без minContext тоже сохраняет прежнее поведение.
	if p := PlanApplyAutoTune(overAllocationAnalysisR69(), loaded); p == nil || p.ContextSize != 38385 {
		t.Fatalf("PlanApplyAutoTune (обёртка): ожидался downgrade до 38385, получено %+v", p)
	}
}

// TestPlanApplyAutoTune_R69_AllowsDowngradeAboveClientRequest — рекомендация
// ВЫШЕ клиентского минимума применяется (клиент не страдает).
func TestPlanApplyAutoTune_R69_AllowsDowngradeAboveClientRequest(t *testing.T) {
	analysis := overAllocationAnalysisR69()
	analysis.Recommendations[0].RecommendedNumCtx = 49152
	loaded := types.LlamaCppModel{Name: "gemma", ContextLength: 65536}
	plan := PlanApplyAutoTuneWithMinContext(analysis, loaded, 32768)
	if plan == nil || plan.ContextSize != 49152 {
		t.Fatalf("ожидался downgrade до 49152 (выше клиентских 32768), получено %+v", plan)
	}
}

// TestDesiredNCtx_R69_TracksMaxAndExpires — координатор помнит максимум
// запрошенного n_ctx и забывает его по истечении окна.
func TestDesiredNCtx_R69_TracksMaxAndExpires(t *testing.T) {
	c := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	if got := c.DesiredNCtx("b1", "m"); got != 0 {
		t.Fatalf("без записей DesiredNCtx = %d, ожидалось 0", got)
	}
	c.RecordRequestedNCtx("b1", "m", 32768)
	c.RecordRequestedNCtx("b1", "m", 65536)
	c.RecordRequestedNCtx("b1", "m", 8192) // меньше — не понижает максимум
	if got := c.DesiredNCtx("b1", "m"); got != 65536 {
		t.Fatalf("DesiredNCtx = %d, ожидалось 65536 (максимум)", got)
	}
	// Другой бэкенд/модель не смешиваются.
	if got := c.DesiredNCtx("b2", "m"); got != 0 {
		t.Fatalf("DesiredNCtx(b2) = %d, ожидалось 0", got)
	}

	// Истечение окна: сдвигаем отметку времени в прошлое.
	c.desiredMu.Lock()
	entry := c.desired["b1\x00m"]
	entry.at = time.Now().Add(-desiredNCtxTTL - time.Minute)
	c.desired["b1\x00m"] = entry
	c.desiredMu.Unlock()
	if got := c.DesiredNCtx("b1", "m"); got != 0 {
		t.Fatalf("после истечения окна DesiredNCtx = %d, ожидалось 0", got)
	}
}

// TestResolveNumCtx_R69_RecordsClientRequest — значение из тела запроса
// доезжает до координатора (иначе AutoTune о нём не знает).
func TestResolveNumCtx_R69_RecordsClientRequest(t *testing.T) {
	p := admissionTestProxy(t, 4)
	if p.nctxReload == nil {
		t.Fatal("nctxReload не инициализирован в тестовом прокси")
	}
	// Сбрасываем замеры предыдущих тестов и клиентский профиль, чтобы значение
	// из тела не было поднято/зажато.
	p.config.LlamaCppModelProfiles = nil
	p.nctxReload.SetLastKnownNCtx("llama_adm", 0)

	res := p.ResolveNumCtx("gemma", []byte(`{"model":"gemma","options":{"num_ctx":65536}}`), "llama_adm")
	if res.Value != 65536 {
		t.Fatalf("ResolveNumCtx = %d, ожидалось 65536", res.Value)
	}
	if got := p.nctxReload.DesiredNCtx("llama_adm", "gemma"); got != 65536 {
		t.Fatalf("DesiredNCtx = %d, ожидалось 65536", got)
	}
}
