//go:build llama_stub

// load_failure_events_r83_test.go — R83 (2026-09-25).
//
// Проверяет звено балансера в связке cppworker → agent → webui: уведомление о
// провале загрузки публикуется в EventBus ТОЛЬКО на смену состояния.
//
// Почему дедупликация — не оптимизация, а требование: agent присылает метрики
// каждые ~10 с, запись о провале живёт в cppworker 10 минут. Без дедупликации
// это ~60 одинаковых уведомлений на один провал, а SSE ring buffer вмещает
// 100 событий (internal/api/handlers_events.go) — полезное было бы вытеснено.
package balancer

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func newLoadFailureTestProxy(t *testing.T) (*Proxy, <-chan types.Event) {
	t.Helper()
	p := &Proxy{
		eventBus:        NewEventBus(),
		loadFailureSeen: make(map[string]string),
	}
	_, ch := p.eventBus.Subscribe()
	return p, ch
}

func drain(ch <-chan types.Event) []types.Event {
	var out []types.Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func configOutOfBoundsFailure(model string) *types.LoadFailureInfo {
	return &types.LoadFailureInfo{
		Model:    model,
		Reason:   "config_out_of_bounds",
		Error:    "requested n_ctx=131072 exceeds ram-fallback-max-n-ctx=32768",
		At:       "2026-09-25T09:46:14Z",
		Severity: "error",
		Diagnostics: map[string]interface{}{
			"requested_n_ctx":      131072,
			"feasible_max_context": 32768,
		},
	}
}

// TestR83_LoadFailureEvent_FirstTransitionOnce — первый провал публикуется,
// повторные push'и с той же причиной молчат (иначе спам каждые 10 секунд).
func TestR83_LoadFailureEvent_FirstTransitionOnce(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	const backend = "cppworker-gpu-bundled-agent"

	p.PublishLoadFailureTransition(backend, configOutOfBoundsFailure("Qwen3.8:latest"))
	events := drain(ch)
	if len(events) != 1 {
		t.Fatalf("после первого провала событий %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Type != types.EventNotification {
		t.Errorf("Type = %q, want %q (SSE отдаёт только notification)", ev.Type, types.EventNotification)
	}
	if ev.Source != "load" {
		t.Errorf("Source = %q, want load", ev.Source)
	}
	if ev.Severity != types.SeverityError {
		t.Errorf("Severity = %q, want error", ev.Severity)
	}
	if ev.Model != "Qwen3.8:latest" {
		t.Errorf("Model = %q", ev.Model)
	}
	if ev.BackendID != backend {
		t.Errorf("BackendID = %q", ev.BackendID)
	}
	if kind, _ := ev.Data["event_kind"].(string); kind != "load_failed" {
		t.Errorf("Data.event_kind = %v, want load_failed", ev.Data["event_kind"])
	}
	if reason, _ := ev.Data["reason"].(string); reason != "config_out_of_bounds" {
		t.Errorf("Data.reason = %v, want config_out_of_bounds", ev.Data["reason"])
	}
	if _, ok := ev.Data["diagnostics"].(map[string]interface{}); !ok {
		t.Errorf("Data.diagnostics отсутствует — оператору нечего разбирать: %v", ev.Data["diagnostics"])
	}
	if raw, _ := ev.Data["raw_error"].(string); raw == "" {
		t.Error("Data.raw_error пуст — сырой текст потерян")
	}
	// Человеческий текст должен называть причину, а не быть «failed to load model».
	if !strings.Contains(ev.Message, "границ") {
		t.Errorf("Message не объясняет причину: %q", ev.Message)
	}

	// Дедупликация: те же данные ещё 6 раз (минута опроса агента).
	for i := 0; i < 6; i++ {
		p.PublishLoadFailureTransition(backend, configOutOfBoundsFailure("Qwen3.8:latest"))
	}
	if got := drain(ch); len(got) != 0 {
		t.Errorf("повторные push'и дали %d событий, want 0 (спам в bell-меню)", len(got))
	}
}

// TestR83_LoadFailureEvent_ChangedReasonRepublished — смена причины это новое
// событие: оператор должен увидеть, что проблема стала другой.
func TestR83_LoadFailureEvent_ChangedReasonRepublished(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	const backend = "b1"

	p.PublishLoadFailureTransition(backend, configOutOfBoundsFailure("m"))
	drain(ch)

	changed := &types.LoadFailureInfo{Model: "m", Reason: "insufficient_resources", Severity: "error"}
	p.PublishLoadFailureTransition(backend, changed)
	events := drain(ch)
	if len(events) != 1 {
		t.Fatalf("смена причины дала %d событий, want 1", len(events))
	}
	if reason, _ := events[0].Data["reason"].(string); reason != "insufficient_resources" {
		t.Errorf("Data.reason = %v", events[0].Data["reason"])
	}
}

// TestR83_LoadFailureEvent_RecoveryOnce — восстановление публикуется один раз,
// и только если провал реально был.
func TestR83_LoadFailureEvent_RecoveryOnce(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)
	const backend = "b1"

	// Провала не было — «восстановление» публиковать нечего.
	p.PublishLoadFailureTransition(backend, nil)
	if got := drain(ch); len(got) != 0 {
		t.Errorf("без предшествующего провала опубликовано %d событий, want 0", len(got))
	}

	p.PublishLoadFailureTransition(backend, configOutOfBoundsFailure("m"))
	drain(ch)

	p.PublishLoadFailureTransition(backend, nil)
	events := drain(ch)
	if len(events) != 1 {
		t.Fatalf("восстановление дало %d событий, want 1", len(events))
	}
	if kind, _ := events[0].Data["event_kind"].(string); kind != "load_recovered" {
		t.Errorf("Data.event_kind = %v, want load_recovered", events[0].Data["event_kind"])
	}
	if events[0].Severity != types.SeverityInfo {
		t.Errorf("Severity = %q, want info", events[0].Severity)
	}

	// Повторный nil — снова молчание.
	p.PublishLoadFailureTransition(backend, nil)
	if got := drain(ch); len(got) != 0 {
		t.Errorf("повторное восстановление дало %d событий, want 0", len(got))
	}
}

// TestR83_LoadFailureEvent_EmptyInfoIsNotAFailure — пустой объект (нет данных от
// cppworker) не должен выглядеть как провал.
func TestR83_LoadFailureEvent_EmptyInfoIsNotAFailure(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)

	p.PublishLoadFailureTransition("b1", &types.LoadFailureInfo{})
	if got := drain(ch); len(got) != 0 {
		t.Errorf("пустой LoadFailureInfo дал %d событий, want 0", len(got))
	}
}

// TestR83_LoadFailureEvent_PerBackendIsolation — провалы на разных бэкендах не
// глушат друг друга.
func TestR83_LoadFailureEvent_PerBackendIsolation(t *testing.T) {
	p, ch := newLoadFailureTestProxy(t)

	p.PublishLoadFailureTransition("b1", configOutOfBoundsFailure("m"))
	p.PublishLoadFailureTransition("b2", configOutOfBoundsFailure("m"))

	if got := drain(ch); len(got) != 2 {
		t.Errorf("два бэкенда дали %d событий, want 2", len(got))
	}
}

// TestR83_LoadFailureEvent_NilProxySafe — публикация на неготовом Proxy не
// должна паниковать (вызывается из api-хендлера).
func TestR83_LoadFailureEvent_NilProxySafe(t *testing.T) {
	var p *Proxy
	p.PublishLoadFailureTransition("b1", configOutOfBoundsFailure("m"))

	// Proxy без eventBus (события публиковать некуда) — тоже без паники.
	p2, _ := newLoadFailureTestProxy(t)
	p2.eventBus = nil
	p2.PublishLoadFailureTransition("b1", configOutOfBoundsFailure("m"))
}

// TestR83_LoadFailureEvent_SeverityMapping — severity переносится из причины,
// неизвестное значение не превращается в error.
func TestR83_LoadFailureEvent_SeverityMapping(t *testing.T) {
	for in, want := range map[string]types.EventSeverity{
		"error":    types.SeverityError,
		"warning":  types.SeverityWarning,
		"info":     types.SeverityInfo,
		"critical": types.SeverityCritical,
		"":         types.SeverityWarning,
		"мусор":    types.SeverityWarning,
	} {
		if got := loadFailureEventSeverity(in); got != want {
			t.Errorf("loadFailureEventSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}
