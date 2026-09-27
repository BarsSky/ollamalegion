//go:build llama_stub

// load_degraded_events_r83_test.go — R83 §9.4 шаг 1б, вариант D (2026-09-26).
//
// Проверяет уведомление «модель ЗАГРУЖЕНА, но без GPU». Это не провал загрузки,
// поэтому и событие отдельное (event_kind=load_degraded, severity=warning), и
// дедупликация отдельная. Тесты фиксируют оба пункта: смешивание с
// load_failure дало бы оператору «не хватило памяти» о работающей модели, а
// затем ложное «загрузка восстановлена».
package balancer

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func cpuOnlyDegraded(model string) *types.DegradedLoadInfo {
	return &types.DegradedLoadInfo{
		Model:  model,
		Stage:  "cpu_only",
		Reason: "insufficient_resources",
		Detail: "weights_exceed_free_vram: веса не влезли в свободную VRAM",
		At:     "2026-09-26T12:00:00Z",
		Diagnostics: map[string]interface{}{
			"requested_n_ctx": 32768,
			"usable_vram_mb":  1024,
		},
	}
}

// TestR83Degraded_FirstTransitionOnce — первая деградация публикуется один раз,
// повторные push'и (agent шлёт метрики каждые ~10 с) молчат.
func TestR83Degraded_FirstTransitionOnce(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	const backend = "cppworker-gpu-bundled-agent"

	for i := 0; i < 5; i++ {
		p.PublishLoadDegradedTransition(backend, cpuOnlyDegraded("qwen3"))
	}
	got := drain(ch)
	if len(got) != 1 {
		t.Fatalf("событий о деградации: %d, want 1 (дедупликация обязательна)", len(got))
	}
	ev := got[0]
	if ev.Type != types.EventNotification {
		t.Errorf("Type = %v, want %v", ev.Type, types.EventNotification)
	}
	// warning, а не error: модель работает, пугать оператора нельзя.
	if ev.Severity != types.SeverityWarning {
		t.Errorf("Severity = %v, want warning (модель загружена, это не провал)", ev.Severity)
	}
	if ev.Model != "qwen3" {
		t.Errorf("Model = %q, want qwen3", ev.Model)
	}
	if kind, _ := ev.Data["event_kind"].(string); kind != "load_degraded" {
		t.Errorf("event_kind = %v, want load_degraded (UI различает провал и деградацию)", ev.Data["event_kind"])
	}
	if stage, _ := ev.Data["stage"].(string); stage != "cpu_only" {
		t.Errorf("stage = %v, want cpu_only", ev.Data["stage"])
	}
	if ev.Data["diagnostics"] == nil {
		t.Error("diagnostics пусты — оператор не увидит чисел раскладки")
	}
	if !strings.Contains(strings.ToLower(ev.Message), "cpu_only") {
		t.Errorf("сообщение %q не объясняет режим — оператор должен понять, что модель работает медленно", ev.Message)
	}
}

// TestR83Degraded_KeyChangeRepublishes — смена стадии (другая модель или другая
// раскладка) — новое событие.
func TestR83Degraded_KeyChangeRepublishes(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	const backend = "b1"

	p.PublishLoadDegradedTransition(backend, cpuOnlyDegraded("m1"))
	p.PublishLoadDegradedTransition(backend, cpuOnlyDegraded("m1")) // дубль — молчит
	p.PublishLoadDegradedTransition(backend, cpuOnlyDegraded("m2"))
	if got := drain(ch); len(got) != 2 {
		t.Fatalf("событий: %d, want 2 (деградация другой модели — другое событие)", len(got))
	}
}

// TestR83Degraded_EmptyIsSilent — пустая запись (модель загружена нормально)
// не порождает событие и НЕ публикует «восстановление»: снятие деградации молчит,
// иначе каждый push агента без деградации давал бы уведомление.
func TestR83Degraded_EmptyIsSilent(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	const backend = "b1"

	p.PublishLoadDegradedTransition(backend, cpuOnlyDegraded("m1"))
	drain(ch) // первое событие — ожидаемо

	p.PublishLoadDegradedTransition(backend, nil)
	p.PublishLoadDegradedTransition(backend, &types.DegradedLoadInfo{})
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("событий: %d, want 0 (снятие деградации — не инцидент)", len(got))
	}

	// После снятия та же деградация снова публикуется: состояние реально
	// вернулось, и оператор должен узнать об этом ещё раз.
	p.PublishLoadDegradedTransition(backend, cpuOnlyDegraded("m1"))
	if got := drain(ch); len(got) != 1 {
		t.Fatalf("событий после возврата деградации: %d, want 1", len(got))
	}
}

// TestR83Degraded_DoesNotCollideWithFailure — ключевое требование: деградация и
// провал одной и той же модели не должны глушить друг друга. Ключи хранятся в
// общей карте, поэтому без префикса "degraded:" они бы столкнулись.
func TestR83Degraded_DoesNotCollideWithFailure(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	const backend = "b1"

	// Провал модели m1, затем её деградация — два разных события.
	p.PublishLoadFailureTransition(backend, configOutOfBoundsFailure("m1"))
	p.PublishLoadDegradedTransition(backend, cpuOnlyDegraded("m1"))
	if got := drain(ch); len(got) != 2 {
		t.Fatalf("событий: %d, want 2 (провал и деградация — разные состояния)", len(got))
	}

	// И повторный push каждого из них не должен ничего добавить.
	p.PublishLoadFailureTransition(backend, configOutOfBoundsFailure("m1"))
	p.PublishLoadDegradedTransition(backend, cpuOnlyDegraded("m1"))
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("событий: %d, want 0 (оба состояния уже сообщены)", len(got))
	}
}

// TestR83Degraded_MessageMentionsConsequence — текст уведомления обязан
// объяснять ПОСЛЕДСТВИЕ (медленно) и путь решения, иначе оператор читает
// «деградация» и не понимает, что делать.
func TestR83Degraded_MessageMentionsConsequence(t *testing.T) {
	d := cpuOnlyDegraded("qwen3.8-27b")
	msg := loadDegradedEventMessage(d)
	for _, want := range []string{"cpu_only", "медленн", "gpuLayers"} {
		if !strings.Contains(msg, want) {
			t.Errorf("сообщение %q не содержит %q", msg, want)
		}
	}
	if !strings.Contains(msg, "weights_exceed_free_vram") {
		t.Errorf("сообщение %q потеряло Detail из вердикта", msg)
	}

	// nil/пустое не должно паниковать: функция вызывается из publish-пути.
	if msg := loadDegradedEventMessage(nil); msg == "" {
		t.Error("пустое сообщение для nil — оператор увидит пустое уведомление")
	}
}
