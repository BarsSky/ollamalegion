// capacity_r117_test.go — R117 (2026-10-14): вместимость бэкенда = ФАКТ.
//
// ЖИВОЙ ДЕФЕКТ (стенд). Балансер показывал вместимость 2, когда на узле не было
// ни одной загруженной модели:
//
//	[pwsh] unload → count=0 | /api/v1/backends: maxConcurrentRequests=2, runtimeMax=0
//
// «2» — это статическая заявка узла (n_parallel=2 из конфига cppworker), а не
// способность что-то обслужить сейчас. Пока модель не загружена, число слотов
// будущей загрузки неизвестно: тот же узел с тем же конфигом загрузил модель с
// parallel=1 и max_slots=1 (проверено живьём). В этом окне два одновременных
// запроса проходят admission балансера, и второй ждёт внутри llama.cpp
// (SlotManager.Acquire) — без позиции в очереди, без keepalive и без строки в
// WebUI. Это и есть жалоба «два запроса с одной машины не разделяются».
//
// Правило R117: при LB_CAPACITY_FROM_MODEL_SLOTS=true вместимость llama.cpp-бэкенда
// берётся из max_slots загруженной модели, а пока слотов нет — равна 1.
// Явный лимит оператора (runtimeMax > 0) по-прежнему сильнее.
package types

import "testing"

func r117Backend(bt BackendType, staticMax, runtimeMax, modelSlots int, fromNode bool) *Backend {
	return &Backend{
		ID:                           "bk",
		Type:                         bt,
		Status:                       StatusHealthy,
		MaxConcurrentReqs:            staticMax,
		RuntimeMaxConcurrentRequests: runtimeMax,
		RuntimeModelSlots:            modelSlots,
		RuntimeCapacityFromNode:      fromNode,
	}
}

// TestR117_CapacityTable — таблица решений: число + источник.
func TestR117_CapacityTable(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")

	cases := []struct {
		name        string
		backend     *Backend
		wantCap     int
		wantSource  string
		explanation string
	}{
		{
			name:        "нет загруженной модели: статическая заявка узла НЕ источник слотов",
			backend:     r117Backend(BackendTypeLlamaCpp, 2, 0, 0, true),
			wantCap:     1,
			wantSource:  CapacitySourceModelSlotsUnknown,
			explanation: "живой дефект: count=0, а отчёт показывал 2 — второй запрос уходил ждать внутрь llama.cpp",
		},
		{
			name:        "модель загружена с 1 слотом",
			backend:     r117Backend(BackendTypeLlamaCpp, 2, 0, 1, true),
			wantCap:     1,
			wantSource:  CapacitySourceModelSlots,
			explanation: "факт из /api/models (max_slots)",
		},
		{
			name:        "модель загружена с 2 слотами при статической заявке 1",
			backend:     r117Backend(BackendTypeLlamaCpp, 1, 0, 2, true),
			wantCap:     2,
			wantSource:  CapacitySourceModelSlots,
			explanation: "R83: факт важнее статической заявки",
		},
		{
			name:        "лимит оператора выше слотов не создаёт ложных слотов",
			backend:     r117Backend(BackendTypeLlamaCpp, 1, 8, 2, false),
			wantCap:     2,
			wantSource:  CapacitySourceModelSlots,
			explanation: "R83: перегрузку не открываем — итог ограничен фактом (слотами), а не лимитом 8",
		},
		{
			name:        "лимит оператора НИЖЕ слотов уважается",
			backend:     r117Backend(BackendTypeLlamaCpp, 1, 1, 4, false),
			wantCap:     1,
			wantSource:  CapacitySourceOperator,
			explanation: "R83: оператор ограничивает нагрузку",
		},
		{
			name:        "явный лимит оператора действует и без загруженной модели",
			backend:     r117Backend(BackendTypeLlamaCpp, 2, 4, 0, false),
			wantCap:     4,
			wantSource:  CapacitySourceOperator,
			explanation: "R117 не спорит с осознанным решением оператора — урезается только заявка узла",
		},
		{
			name:        "image-бэкенд правилом R117 не задет",
			backend:     r117Backend(BackendTypeImage, 64, 64, 0, false),
			wantCap:     64,
			wantSource:  CapacitySourceOperator,
			explanation: "у image-воркеров своя очередь и своя вместимость (64)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCap, gotSource := tc.backend.ResolveCapacity()
			if gotCap != tc.wantCap {
				t.Errorf("ResolveCapacity() cap = %d, want %d (%s)", gotCap, tc.wantCap, tc.explanation)
			}
			if gotSource != tc.wantSource {
				t.Errorf("ResolveCapacity() source = %q, want %q", gotSource, tc.wantSource)
			}
			if eff := tc.backend.EffectiveMaxConcurrentRequests(); eff != tc.wantCap {
				t.Errorf("EffectiveMaxConcurrentRequests() = %d, want %d (должно совпадать с ResolveCapacity)", eff, tc.wantCap)
			}
		})
	}
}

// TestR117_FlagOffKeepsLegacyBehaviour — без флага правила R117 нет: нода остаётся
// источником правды (прежнее поведение стендов, которые флаг не включали).
func TestR117_FlagOffKeepsLegacyBehaviour(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "")
	t.Setenv("CPPWORKER_CAPACITY_FROM_SLOTS", "")

	b := r117Backend(BackendTypeLlamaCpp, 2, 0, 0, true)
	if got := b.EffectiveMaxConcurrentRequests(); got != 2 {
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, want 2 (флаг выключен — прежнее поведение)", got)
	}

	// И слоты модели при выключенном флаге игнорируются (R83: opt-in).
	b2 := r117Backend(BackendTypeLlamaCpp, 1, 0, 4, true)
	if got := b2.EffectiveMaxConcurrentRequests(); got != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, want 1: при выключенном флаге "+
			"RuntimeModelSlots не учитывается", got)
	}
}
