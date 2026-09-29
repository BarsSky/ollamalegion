// effective_capacity_r83_test.go — R83 (2026-09-29).
//
// Проверяет правило резолвера вместимости бэкенда. Оно появилось из-за дефекта на
// живом стенде: балансер держал вместимость 1 и СЕРИАЛИЗОВАЛ запросы к модели,
// загруженной с parallel=2/3, хотя cppworker готов обслуживать их одновременно
// (проверено напрямую: два одновременных запроса при parallel=2 — оба 200).
//
// Отдельно важна причина, по которой факт слотов живёт в ОТДЕЛЬНОМ поле
// (RuntimeModelSlots), а не в MaxConcurrentReqs: последнее каждые 30 секунд
// переписывает перерегистрация агента своим значением
// (AGENT_MAX_CONCURRENT_REQUESTS) — на живом стенде poller ставил 2, а ближайший
// heartbeat возвращал 1.
package types

import "testing"

func TestResolveEffectiveCapacity_SlotsWin(t *testing.T) {
	cases := []struct {
		name       string
		modelSlots int
		runtimeMax int
		fromNode   bool
		staticMax  int
		want       int
	}{
		{"слоты 3, без лимитов → 3", 3, 0, false, 1, 3},
		{"слоты 2, оператор 1 → 1 (лимит ниже)", 2, 1, false, 1, 1},
		{"слоты 2, оператор 2 → 2", 2, 2, false, 1, 2},
		{"слоты 2, оператор 8 → 2 (нельзя открыть ложные слоты)", 2, 8, false, 1, 2},
		{"слоты 1 (однослотовая модель) → 1", 1, 0, true, 1, 1},
		// Старое поведение — когда cppworker слотов не сообщил (старая сборка).
		{"нет слотов, узел сообщил 4 → 4", 0, 0, true, 4, 4},
		{"нет слотов, оператор 2 → 2", 0, 2, false, 1, 2},
		{"нет слотов, ничего не задано → статика", 0, 0, false, 3, 3},
		{"нет слотов, fromNode и runtime: узел важнее", 0, 5, true, 4, 4},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveEffectiveCapacity(c.modelSlots, c.runtimeMax, c.fromNode, c.staticMax)
			if got != c.want {
				t.Errorf("ResolveEffectiveCapacity(%d, %d, %v, %d) = %d, want %d",
					c.modelSlots, c.runtimeMax, c.fromNode, c.staticMax, got, c.want)
			}
		})
	}
}

// TestResolveEffectiveCapacity_AgentReregistration — сценарий дефекта: агент
// перерегистрировался и вернул свою вместимость 1, но фактические слоты модели (2)
// не должны из-за этого потеряться, если оператор не ограничивает.
func TestResolveEffectiveCapacity_AgentReregistration(t *testing.T) {
	// До heartbeat: poller записал 2 слота.
	if got := ResolveEffectiveCapacity(2, 0, false, 1); got != 2 {
		t.Fatalf("до перерегистрации = %d, want 2", got)
	}
	// После перерегистрации агент выставил свою вместимость 1 (MaxConcurrentReqs=1),
	// но отдельного операторского лимита нет (runtimeMax=0).
	if got := ResolveEffectiveCapacity(2, 0, true, 1); got != 2 {
		t.Errorf("после перерегистрации = %d, want 2: факт слотов модели не должен "+
			"теряться от heartbeat агента", got)
	}
}

func TestBackend_EffectiveMaxConcurrentRequests_NilSafe(t *testing.T) {
	var b *Backend
	if got := b.EffectiveMaxConcurrentRequests(); got != 0 {
		t.Errorf("nil Backend → %d, want 0", got)
	}
}

// TestBackend_CapacityFromSlots_OptIn — учёт слотов модели выключен по умолчанию и
// включается переменной окружения. Это защищает уже проверенные пути
// (admission-очередь, слот-менеджер, выбор бэкенда) от смены поведения при
// обновлении: пока оператор не включил флаг, всё работает как раньше.
func TestBackend_CapacityFromSlots_OptIn(t *testing.T) {
	b := &Backend{
		MaxConcurrentReqs: 1,
		RuntimeModelSlots: 3, // cppworker сообщил три слота
	}

	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "")
	t.Setenv("CPPWORKER_CAPACITY_FROM_SLOTS", "")
	if got := b.EffectiveMaxConcurrentRequests(); got != 1 {
		t.Errorf("при выключенном флаге effective = %d, want 1 (прежнее поведение)", got)
	}

	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	if got := b.EffectiveMaxConcurrentRequests(); got != 3 {
		t.Errorf("при включённом флаге effective = %d, want 3 (слоты модели)", got)
	}
}
