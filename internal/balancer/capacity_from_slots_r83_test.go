// capacity_from_slots_r83_test.go — R83 (2026-09-29).
//
// ЗАЧЕМ. Балансер не знал, сколько параллельных сессий держит модель, и работал с
// вместимостью 1: запросы к модели с parallel=3 СЕРИАЛИЗОВАЛИСЬ, хотя cppworker
// готов обслуживать их одновременно (проверено на живом: два одновременных запроса
// при parallel=2 — оба 200).
//
// Теперь cppworker отдаёт `max_slots`/`parallel` в /api/models, а poller пишет факт
// в RuntimeModelSlots — ОТДЕЛЬНОЕ поле, потому что MaxConcurrentReqs каждые 30 c
// переписывает перерегистрация агента (на живом стенде poller ставил 2, а
// ближайший heartbeat возвращал 1). Итоговую вместимость считает
// EffectiveMaxConcurrentRequests().
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// capacityHarness — Proxy с одним llama.cpp-бэкендом.
func capacityHarness(t *testing.T, maxConcurrent, runtimeMax int) *Proxy {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["bk"] = &BackendState{
		Backend: &types.Backend{
			ID:                           "bk",
			Host:                         "127.0.0.1",
			Type:                         types.BackendTypeLlamaCpp,
			CppWorkerPort:                18092,
			Status:                       types.StatusHealthy,
			MaxConcurrentReqs:            maxConcurrent,
			RuntimeMaxConcurrentRequests: runtimeMax,
		},
	}
	return p
}

// TestR83_Poller_AdoptsSlotsFromModel — модель загружена с parallel=3, значит
// эффективная вместимость обязана стать 3.
func TestR83_Poller_AdoptsSlotsFromModel(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	p := capacityHarness(t, 1, 0)
	poller := &llamaCppMetricsPoller{proxy: p}

	poller.adoptCapacityFromLoadedModels("bk", []types.LlamaCppModel{
		{Name: "m", State: "loaded", MaxSlots: 3, Parallel: 3},
	})

	b := p.backends["bk"].Backend
	if b.RuntimeModelSlots != 3 {
		t.Errorf("RuntimeModelSlots = %d, want 3 (факт из cppworker)", b.RuntimeModelSlots)
	}
	if got := b.EffectiveMaxConcurrentRequests(); got != 3 {
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, want 3. При 1 балансер "+
			"сериализует запросы к модели, которая держит три сессии", got)
	}
}

// TestR83_Poller_OperatorLimitWins — операторский лимит НИЖЕ слотов уважается.
func TestR83_Poller_OperatorLimitWins(t *testing.T) {
	// Оператор поставил 2, узел сообщает 3 слота → работаем по 2.
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	p := capacityHarness(t, 2, 2)
	poller := &llamaCppMetricsPoller{proxy: p}

	poller.adoptCapacityFromLoadedModels("bk", []types.LlamaCppModel{
		{Name: "m", State: "loaded", MaxSlots: 3},
	})

	if got := p.backends["bk"].Backend.EffectiveMaxConcurrentRequests(); got != 2 {
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, want 2: лимит оператора ниже "+
			"фактических слотов должен ограничивать нагрузку", got)
	}
}

// TestR83_Poller_OperatorCannotExceedSlots — операторский лимит ВЫШЕ слотов не
// должен открывать ложные слоты: иначе балансер пошлёт больше запросов, чем
// cppworker обслуживает.
func TestR83_Poller_OperatorCannotExceedSlots(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	p := capacityHarness(t, 1, 8) // оператор выставил 8, слотов 2
	poller := &llamaCppMetricsPoller{proxy: p}

	poller.adoptCapacityFromLoadedModels("bk", []types.LlamaCppModel{
		{Name: "m", State: "loaded", MaxSlots: 2},
	})

	if got := p.backends["bk"].Backend.EffectiveMaxConcurrentRequests(); got != 2 {
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, want 2: завышенный лимит "+
			"оператора не должен создавать ложные свободные слоты", got)
	}
}

// TestR83_Poller_TakesMaxAcrossModels — при нескольких загруженных моделях
// вместимость = максимум слотов.
func TestR83_Poller_TakesMaxAcrossModels(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	p := capacityHarness(t, 1, 0)
	poller := &llamaCppMetricsPoller{proxy: p}

	poller.adoptCapacityFromLoadedModels("bk", []types.LlamaCppModel{
		{Name: "small", State: "loaded", MaxSlots: 1},
		{Name: "big", State: "loaded", MaxSlots: 4},
	})

	if got := p.backends["bk"].Backend.EffectiveMaxConcurrentRequests(); got != 4 {
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, want 4 (максимум по моделям)", got)
	}
}

// TestR83_Poller_IgnoresNotLoadedAndLegacyPayload — незагруженные модели не
// учитываются, а старая сборка cppworker (без max_slots) не должна менять
// вместимость.
func TestR83_Poller_IgnoresNotLoadedAndLegacyPayload(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	p := capacityHarness(t, 2, 0)
	poller := &llamaCppMetricsPoller{proxy: p}

	poller.adoptCapacityFromLoadedModels("bk", []types.LlamaCppModel{
		{Name: "loading", State: "loading", MaxSlots: 4},
		{Name: "legacy", State: "loaded"}, // 0/0 — старый cppworker
	})

	b := p.backends["bk"].Backend
	if b.RuntimeModelSlots != 0 {
		t.Errorf("RuntimeModelSlots = %d, want 0: незагруженные модели и legacy-payload "+
			"не должны менять вместимость", b.RuntimeModelSlots)
	}
	if got := b.EffectiveMaxConcurrentRequests(); got != 2 {
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, want 2 (прежнее поведение)", got)
	}
}

// TestR83_Poller_AgentReregistrationDoesNotResetSlots — ключевой сценарий дефекта:
// перерегистрация агента переписывает MaxConcurrentReqs, но НЕ должна сбрасывать
// фактические слоты модели.
func TestR83_Poller_AgentReregistrationDoesNotResetSlots(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	p := capacityHarness(t, 1, 0)
	poller := &llamaCppMetricsPoller{proxy: p}

	poller.adoptCapacityFromLoadedModels("bk", []types.LlamaCppModel{
		{Name: "m", State: "loaded", MaxSlots: 2},
	})

	// Имитируем heartbeat/перерегистрацию агента с его значением 1.
	b := p.backends["bk"].Backend
	b.MaxConcurrentReqs = 1
	b.RuntimeMaxConcurrentRequests = 1
	b.RuntimeCapacityFromNode = true

	if got := b.EffectiveMaxConcurrentRequests(); got != 1 {
		// Операторское значение (1) НИЖЕ слотов (2) → оно и выигрывает.
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, want 1 (лимит ниже слотов)", got)
	}

	// А если агент не ограничивает (0), фактические слоты должны работать.
	b.RuntimeMaxConcurrentRequests = 0
	if got := b.EffectiveMaxConcurrentRequests(); got != 2 {
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, want 2: фактические слоты модели "+
			"не должны теряться от перерегистрации агента", got)
	}
}
