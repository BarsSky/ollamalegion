//go:build llama_stub

// events_pump_r83_test.go — R83 §9.5 (2026-09-26).
//
// Симптом: ring buffer нотификаций не наполнялся, пока не подключён SSE-клиент:
// buf.push вызывался только внутри live-цикла handleEvents. Следствие —
// GET /api/v1/events при переподключении не отдавал пропущенные нотификации, а
// секция recent-errors в /api/v1/health была всегда пуста.
//
// Теперь буфер наполняет фоновая подписка (eventsHub.startPump), живущая
// независимо от клиентов.
package api

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// fakeEventBus — минимальная EventBusLike для теста: Publish кладёт событие
// всем подписчикам.
type fakeEventBus struct {
	subs map[string]chan types.Event
}

func newFakeEventBus() *fakeEventBus {
	return &fakeEventBus{subs: map[string]chan types.Event{}}
}

func (b *fakeEventBus) Subscribe() (string, <-chan types.Event) {
	id := "sub-" + time.Now().Format("150405.000000000")
	ch := make(chan types.Event, 16)
	b.subs[id] = ch
	return id, ch
}

func (b *fakeEventBus) Unsubscribe(id string) {
	delete(b.subs, id)
}

// publish рассылает событие подписчикам.
func (b *fakeEventBus) publish(ev types.Event) {
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// TestR83_EventsPump_FillsBufferWithoutClients — ключевой критерий: событие,
// опубликованное БЕЗ подключённых клиентов, обязано попасть в snapshot.
func TestR83_EventsPump_FillsBufferWithoutClients(t *testing.T) {
	bus := newFakeEventBus()
	hub := newEventsHub()
	hub.startPump(bus)

	// Никаких SSE-клиентов — только публикация.
	bus.publish(types.Event{
		Type:      types.EventNotification,
		Timestamp: time.Now(),
		Severity:  types.SeverityWarning,
		Message:   "transport EOF without client",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(hub.buf.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	snap := hub.buf.snapshot()
	if len(snap) != 1 {
		t.Fatalf("в буфере %d событий, want 1 (событие без клиента потеряно — "+
			"recent-errors в /api/v1/health останется пустым)", len(snap))
	}
	if snap[0].Message != "transport EOF without client" {
		t.Errorf("сообщение = %q", snap[0].Message)
	}
	if hub.pushedCount() != 1 {
		t.Errorf("pushedCount = %d, want 1", hub.pushedCount())
	}
}

// TestR83_EventsPump_IgnoresNonNotifications — буфер хранит только нотификации
// (как и live-цикл handleEvents): иначе история вытеснялась бы служебными
// событиями.
func TestR83_EventsPump_IgnoresNonNotifications(t *testing.T) {
	bus := newFakeEventBus()
	hub := newEventsHub()
	hub.startPump(bus)

	bus.publish(types.Event{Type: "metrics_update", Message: "не нотификация"})
	bus.publish(types.Event{Type: types.EventNotification, Message: "нотификация"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(hub.buf.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	snap := hub.buf.snapshot()
	if len(snap) != 1 {
		t.Fatalf("в буфере %d событий, want 1 (только нотификация)", len(snap))
	}
	if snap[0].Message != "нотификация" {
		t.Errorf("в буфере %q, want нотификацию", snap[0].Message)
	}
}

// TestR83_EventsPump_Idempotent — повторный SetEventBus не должен плодить
// горутины-подписчики (иначе каждое событие попадало бы в буфер N раз).
func TestR83_EventsPump_Idempotent(t *testing.T) {
	bus := newFakeEventBus()
	hub := newEventsHub()
	hub.startPump(bus)
	hub.startPump(bus) // повторный вызов

	if len(bus.subs) != 1 {
		t.Errorf("подписок = %d, want 1 (startPump обязан быть идемпотентным)", len(bus.subs))
	}

	bus.publish(types.Event{Type: types.EventNotification, Message: "один раз"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(hub.buf.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(hub.buf.snapshot()); got != 1 {
		t.Errorf("в буфере %d копий события, want 1 (дублирующие подписки)", got)
	}
}
