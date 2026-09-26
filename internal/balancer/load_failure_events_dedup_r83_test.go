//go:build llama_stub

// load_failure_events_dedup_r83_test.go — R83 §9.3 (2026-09-26).
//
// Оператор о провале авто-загрузки узнавал только из лога: события в EventBus
// (→ SSE ring buffer → bell-меню и recent-errors) не публиковались. Публикация
// из пути «async auto-load failed» повторяется на каждый запрос клиента, поэтому
// она обязана быть дедуплицированной: иначе 100-событийный ring buffer
// заполнялся бы копиями одного провала.
package balancer

import (
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// eventCounter — подписчик на реальный EventBus: считает доставленные события.
type eventCounter struct {
	mu     sync.Mutex
	events []types.Event
}

func (c *eventCounter) add(ev types.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *eventCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// newCountingBus — реальный EventBus + подписчик-счётчик.
//
// Реальный (а не мок) выбран намеренно: проверяем и то, что Publish доходит до
// подписчиков, а не только дедупликацию.
func newCountingBus(t *testing.T) (*EventBus, *eventCounter) {
	t.Helper()
	bus := NewEventBus()
	_, ch := bus.Subscribe()
	c := &eventCounter{}
	go func() {
		for ev := range ch {
			c.add(ev)
		}
	}()
	return bus, c
}

// waitEvents ждёт, пока счётчик достигнет want (публикация асинхронная).
func waitEvents(t *testing.T, c *eventCounter, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestR83_LoadFailureEvent_PublishedOncePerCause — одинаковая причина даёт одно
// событие, другая причина — новое.
func TestR83_LoadFailureEvent_PublishedOncePerCause(t *testing.T) {
	bus, counter := newCountingBus(t)
	p := &Proxy{eventBus: bus}

	for i := 0; i < 5; i++ {
		p.publishLoadFailureEventDeduped("bk1", "auto_load_failed|m|no such host",
			types.SeverityWarning, "Не удалось загрузить модель m: no such host", nil)
	}
	waitEvents(t, counter, 1)
	if got := counter.count(); got != 1 {
		t.Errorf("событий = %d, want 1 (дедупликация по причине)", got)
	}

	p.publishLoadFailureEventDeduped("bk1", "auto_load_failed|m|timeout",
		types.SeverityWarning, "Не удалось загрузить модель m: timeout", nil)
	waitEvents(t, counter, 2)
	if got := counter.count(); got != 2 {
		t.Errorf("после смены причины событий = %d, want 2", got)
	}

	// Пустой key = «сброс»: публикации нет.
	p.publishLoadFailureEventDeduped("bk1", "", types.SeverityWarning, "", nil)
	time.Sleep(50 * time.Millisecond)
	if got := counter.count(); got != 2 {
		t.Errorf("пустой key не должен публиковать: %d", got)
	}
}

// TestR83_LoadFailureEvent_PerBackend — дедупликация не склеивает разные бэкенды:
// провал на двух бэкендах — два события.
func TestR83_LoadFailureEvent_PerBackend(t *testing.T) {
	bus, counter := newCountingBus(t)
	p := &Proxy{eventBus: bus}

	p.publishLoadFailureEventDeduped("bk1", "same|cause",
		types.SeverityWarning, "провал", nil)
	p.publishLoadFailureEventDeduped("bk2", "same|cause",
		types.SeverityWarning, "провал", nil)

	waitEvents(t, counter, 2)
	if got := counter.count(); got != 2 {
		t.Errorf("событий = %d, want 2 (по одному на бэкенд)", got)
	}
}

// TestR83_LoadFailureEvent_NilSafe — без EventBus и с nil-прокси публикация не паникует.
func TestR83_LoadFailureEvent_NilSafe(t *testing.T) {
	p := &Proxy{}
	p.publishLoadFailureEventDeduped("bk", "k", types.SeverityWarning, "msg", nil)

	var nilP *Proxy
	nilP.publishLoadFailureEventDeduped("bk", "k", types.SeverityWarning, "msg", nil)
}
