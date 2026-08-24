package balancer

import (
	"fmt"
	"sync"

	"ollama-loadbalancer/pkg/types"
)

// EventBus — подписка/публикация событий кластера
type EventBus struct {
	subs     map[string]chan types.Event
	mu       sync.RWMutex
	stopped  bool
	stopCh   chan struct{}
	nextID   uint64 // R52.5 (2026-08-24): atomic counter for unique subscription IDs
	         // (replaces time.Now().UnixNano() который мог дать дубликаты
	         // при Subscribe() в одной nanosecond — тест это поймал)
}

// NewEventBus — создание нового EventBus
func NewEventBus() *EventBus {
	return &EventBus{
		subs:   make(map[string]chan types.Event),
		stopCh: make(chan struct{}),
	}
}

// Subscribe — подписка на события, возвращает канал и ID подписки
func (eb *EventBus) Subscribe() (string, <-chan types.Event) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	if eb.stopped {
		// Возвращаем закрытый канал — подписчик сразу увидит EOF
		// и завершит свой loop без висящей goroutine.
		ch := make(chan types.Event)
		close(ch)
		return "sub-closed", ch
	}

	eb.nextID++
	id := fmt.Sprintf("sub-%d", eb.nextID)
	ch := make(chan types.Event, 64)
	eb.subs[id] = ch
	return id, ch
}

// Unsubscribe — отписка от событий
func (eb *EventBus) Unsubscribe(id string) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	if ch, ok := eb.subs[id]; ok {
		close(ch)
		delete(eb.subs, id)
	}
}

// Publish — публикация события всем подписчикам (неблокирующая)
func (eb *EventBus) Publish(ev types.Event) {
	eb.mu.RLock()
	stopped := eb.stopped
	subs := make([]chan types.Event, 0, len(eb.subs))
	for _, ch := range eb.subs {
		subs = append(subs, ch)
	}
	eb.mu.RUnlock()

	if stopped {
		return // drop events после Stop
	}

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Count — количество активных подписчиков
func (eb *EventBus) Count() int {
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	return len(eb.subs)
}

// Stop — Round 52.5 (2026-08-24): graceful shutdown EventBus.
//
// Закрывает все active subscriber channels (вызывающий код увидит EOF и
// завершит свою goroutine). Идемпотентно — повторный вызов no-op.
// После Stop новые Subscribe() возвращают closed channel, Publish() drop'ит.
//
// NB: вызывающий код должен сам сделать Unsubscribe() в defer, иначе
// Stop закроет канал за него — это OK (только важно не делать Send
// после close). Обычно pattern: defer eb.Unsubscribe(id) — закрытие
// канала в defer идемпотентно, и Stop закрывает только тех кто не
// отписался.
func (eb *EventBus) Stop() {
	if eb == nil {
		return
	}
	eb.mu.Lock()
	if eb.stopped {
		eb.mu.Unlock()
		return
	}
	eb.stopped = true
	close(eb.stopCh)
	// Закрываем все active subscriber channels — горутины-подписчики
	// увидят zero-value при receive и завершатся.
	subs := make([]chan types.Event, 0, len(eb.subs))
	for _, ch := range eb.subs {
		subs = append(subs, ch)
	}
	eb.subs = make(map[string]chan types.Event) // clear map
	eb.mu.Unlock()

	for _, ch := range subs {
		close(ch)
	}
}

// Done — возвращает канал, который закрывается при Stop().
func (eb *EventBus) Done() <-chan struct{} {
	if eb == nil {
		return nil
	}
	return eb.stopCh
}