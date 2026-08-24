// eventbus_test.go — Round 52.5 (2026-08-24): tests for EventBus.Stop.

package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func TestEventBus_Stop_ClosesSubscribers(t *testing.T) {
	eb := NewEventBus()
	subID, subCh := eb.Subscribe()
	if eb.Count() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", eb.Count())
	}

	eb.Stop()

	// После Stop — канал подписчика должен быть closed.
	select {
	case _, ok := <-subCh:
		if ok {
			t.Error("expected subCh to be closed after Stop")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for subCh to close after Stop")
	}

	// Подписчик должен быть удалён из map.
	if eb.Count() != 0 {
		t.Errorf("expected 0 subscribers after Stop, got %d", eb.Count())
	}

	// Unsubscribe после Stop должен быть no-op (не double-close).
	eb.Unsubscribe(subID)
}

func TestEventBus_Stop_Idempotent(t *testing.T) {
	eb := NewEventBus()
	eb.Subscribe()
	eb.Stop()
	// Второй вызов Stop не должен паниковать.
	eb.Stop()
	eb.Stop()
}

func TestEventBus_Stop_NilSafe(t *testing.T) {
	var eb *EventBus
	eb.Stop() // не должно паниковать
	if eb.Done() != nil {
		t.Error("nil EventBus Done() should return nil")
	}
}

func TestEventBus_Stop_DropsNewEvents(t *testing.T) {
	eb := NewEventBus()
	eb.Stop()

	// Publish после Stop — событие должно быть проигнорировано, не блокировать.
	eb.Publish(types.Event{Type: types.EventNotification, Message: "after stop"})
}

func TestEventBus_Subscribe_AfterStop(t *testing.T) {
	eb := NewEventBus()
	eb.Stop()

	// Subscribe после Stop должен вернуть closed channel (subscriber сразу выйдет).
	subID, subCh := eb.Subscribe()
	if subID != "sub-closed" {
		t.Errorf("expected subID=sub-closed, got %s", subID)
	}

	select {
	case _, ok := <-subCh:
		if ok {
			t.Error("expected subCh to be closed when subscribing after Stop")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for subCh to close")
	}
}

func TestEventBus_Done_Channel(t *testing.T) {
	eb := NewEventBus()

	// Done() должен блокироваться пока не вызван Stop.
	select {
	case <-eb.Done():
		t.Error("Done() should block before Stop")
	case <-time.After(50 * time.Millisecond):
		// OK — Done() blocked
	}

	// После Stop — Done() закрывается.
	eb.Stop()
	select {
	case <-eb.Done():
		// OK — closed
	case <-time.After(100 * time.Millisecond):
		t.Error("Done() should close after Stop")
	}
}

func TestEventBus_Stop_WithMultipleSubscribers(t *testing.T) {
	eb := NewEventBus()
	const N = 5
	channels := make([]<-chan types.Event, N)
	for i := 0; i < N; i++ {
		_, ch := eb.Subscribe()
		channels[i] = ch
	}

	if eb.Count() != N {
		t.Fatalf("expected %d subscribers, got %d", N, eb.Count())
	}

	eb.Stop()

	// Все каналы должны быть closed.
	for i, ch := range channels {
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("channel %d should be closed", i)
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("timeout waiting for channel %d to close", i)
		}
	}
}

func TestEventBus_Stop_DuringPublish(t *testing.T) {
	eb := NewEventBus()
	_, subCh := eb.Subscribe()

	// Конкурентный Publish + Stop. Publish не должен блокировать (drop-on-full)
	// даже если Stop() закрывает канал в процессе.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			eb.Publish(types.Event{Type: types.EventNotification, Message: "test"})
		}
		close(done)
	}()

	// Дать Publish'у стартовать, потом Stop.
	time.Sleep(5 * time.Millisecond)
	eb.Stop()

	<-done

	// Подписчик завершается по closed channel.
	select {
	case _, ok := <-subCh:
		if ok {
			// Может получить событие если оно было в буфере до close.
			// Главное — канал в итоге closed.
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("subCh should close eventually")
	}
}
