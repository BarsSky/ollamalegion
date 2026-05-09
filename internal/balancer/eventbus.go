package balancer

import (
	"fmt"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// EventBus — подписка/публикация событий кластера
type EventBus struct {
	subs map[string]chan types.Event
	mu   sync.RWMutex
}

// NewEventBus — создание нового EventBus
func NewEventBus() *EventBus {
	return &EventBus{
		subs: make(map[string]chan types.Event),
	}
}

// Subscribe — подписка на события, возвращает канал и ID подписки
func (eb *EventBus) Subscribe() (string, <-chan types.Event) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	id := fmt.Sprintf("sub-%d", time.Now().UnixNano())
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
	subs := make([]chan types.Event, 0, len(eb.subs))
	for _, ch := range eb.subs {
		subs = append(subs, ch)
	}
	eb.mu.RUnlock()

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