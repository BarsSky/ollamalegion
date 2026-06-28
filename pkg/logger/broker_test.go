package logger

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// TestLogBroker_PublishAndSnapshot проверяет базовый flow: Publish + Snapshot.
func TestLogBroker_PublishAndSnapshot(t *testing.T) {
	b := NewLogBroker(10)

	b.Publish(LogEntry{Level: "info", Message: "first"})
	b.Publish(LogEntry{Level: "warn", Message: "second"})

	snap := b.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(snap))
	}
	if snap[0].Message != "first" || snap[1].Message != "second" {
		t.Errorf("wrong order/messages: %+v", snap)
	}
	if snap[0].Time.IsZero() {
		t.Error("expected non-zero Time after Publish")
	}
	if snap[0].Level != "info" || snap[1].Level != "warn" {
		t.Errorf("wrong levels: %+v", snap)
	}
}

// TestLogBroker_RingBufferOverflow — при переполнении старые записи отбрасываются.
func TestLogBroker_RingBufferOverflow(t *testing.T) {
	b := NewLogBroker(3) // маленький буфер
	for i := 0; i < 10; i++ {
		b.Publish(LogEntry{Level: "info", Message: "msg"})
	}
	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected ring buffer cap=3, got %d", len(snap))
	}
}

// TestLogBroker_SubscribeReceivesPublish — подписчик получает все Publish'ы.
func TestLogBroker_SubscribeReceivesPublish(t *testing.T) {
	b := NewLogBroker(10)
	sub := b.Subscribe()
	defer sub.Close()

	b.Publish(LogEntry{Level: "info", Message: "hello"})
	b.Publish(LogEntry{Level: "warn", Message: "world"})

	// Читаем с таймаутом (чтобы тест не зависал при ошибке).
	timeout := time.After(1 * time.Second)
	got := []LogEntry{}
	for len(got) < 2 {
		select {
		case e := <-sub.Channel():
			got = append(got, e)
		case <-timeout:
			t.Fatalf("expected 2 entries, got %d", len(got))
		}
	}
	if got[0].Message != "hello" || got[1].Message != "world" {
		t.Errorf("wrong messages: %+v", got)
	}
}

// TestLogBroker_SubscribeDropSlowClient — медленный клиент не блокирует producer.
func TestLogBroker_SubscribeDropSlowClient(t *testing.T) {
	b := NewLogBroker(10)
	sub := b.Subscribe()
	defer sub.Close()

	// Заполняем канал подписчика, не читая.
	for i := 0; i < 200; i++ {
		b.Publish(LogEntry{Level: "info", Message: "flood"})
	}

	// Producer не должен зависнуть — мы здесь, значит OK.
	// Snapshot должен всё ещё содержать 10 записей (ring buffer).
	snap := b.Snapshot()
	if len(snap) != 10 {
		t.Errorf("expected ring buffer cap=10, got %d", len(snap))
	}
}

// TestLogBroker_MultipleSubscribers — несколько подписчиков получают одну и ту же запись.
func TestLogBroker_MultipleSubscribers(t *testing.T) {
	b := NewLogBroker(10)
	sub1 := b.Subscribe()
	sub2 := b.Subscribe()
	sub3 := b.Subscribe()
	defer sub1.Close()
	defer sub2.Close()
	defer sub3.Close()

	b.Publish(LogEntry{Level: "info", Message: "broadcast"})

	timeout := time.After(1 * time.Second)
	for i, sub := range []*LogSubscriber{sub1, sub2, sub3} {
		select {
		case e := <-sub.Channel():
			if e.Message != "broadcast" {
				t.Errorf("sub %d got %q, want broadcast", i, e.Message)
			}
		case <-timeout:
			t.Fatalf("sub %d did not receive entry", i)
		}
	}
}

// TestLogBroker_SubscriberAutoRemoved — после Close подписчик удаляется из брокера.
func TestLogBroker_SubscriberAutoRemoved(t *testing.T) {
	b := NewLogBroker(10)
	sub := b.Subscribe()
	if b.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", b.SubscriberCount())
	}
	sub.Close()
	// Даём горутине авто-удаления шанс отработать.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && b.SubscriberCount() > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if b.SubscriberCount() != 0 {
		t.Errorf("expected 0 subscribers after Close, got %d", b.SubscriberCount())
	}
}

// TestLogBroker_ConcurrentPublish — несколько goroutine публикуют одновременно.
func TestLogBroker_ConcurrentPublish(t *testing.T) {
	b := NewLogBroker(100)
	const goroutines = 10
	const perGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				b.Publish(LogEntry{Level: "info", Message: "concurrent"})
			}
		}()
	}
	wg.Wait()

	// Ring buffer cap=100, публиковали 500 записей → должно остаться 100.
	snap := b.Snapshot()
	if len(snap) != 100 {
		t.Errorf("expected ring buffer cap=100 after overflow, got %d", len(snap))
	}
}

// TestLogBroker_StopIdempotent — повторные Stop безопасны.
func TestLogBroker_StopIdempotent(t *testing.T) {
	b := NewLogBroker(10)
	b.Stop()
	b.Stop() // не должно паниковать
}

// TestLogBroker_SubscribeAfterStop — после Stop подписка возвращает no-op.
func TestLogBroker_SubscribeAfterStop(t *testing.T) {
	b := NewLogBroker(10)
	b.Stop()

	sub := b.Subscribe()
	defer func() {
		// close может быть уже закрыт — recover.
		_ = recover()
	}()

	// Channel должен быть закрыт.
	select {
	case _, ok := <-sub.Channel():
		if ok {
			t.Error("expected closed channel after broker Stop")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected immediate close after broker Stop")
	}
}

// TestLogEntry_JSONMarshaling — формат JSON стабилен для frontend.
func TestLogEntry_JSONMarshaling(t *testing.T) {
	e := LogEntry{
		Time:    time.Date(2026, 6, 28, 13, 0, 0, 0, time.UTC),
		Level:   "warn",
		Message: "test message",
		Source:  "balancer",
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	// Должно содержать ожидаемые ключи (порядок не важен).
	s := string(b)
	for _, k := range []string{`"time":"2026-06-28T13:00:00Z"`, `"level":"warn"`, `"message":"test message"`, `"source":"balancer"`} {
		if !contains(s, k) {
			t.Errorf("expected JSON to contain %s, got %s", k, s)
		}
	}
}

// TestLogEntry_JSONOmitsEmptySource — Source не сериализуется если пустой.
func TestLogEntry_JSONOmitsEmptySource(t *testing.T) {
	e := LogEntry{Time: time.Now(), Level: "info", Message: "no source"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if contains(string(b), `"source"`) {
		t.Errorf("expected empty Source to be omitted, got %s", string(b))
	}
}

// TestGlobalBroker — SetBroker/GetBroker/Publish через глобальный broker.
func TestGlobalBroker(t *testing.T) {
	b := NewLogBroker(10)
	SetBroker(b)

	if GetBroker() != b {
		t.Error("GetBroker returned wrong instance")
	}

	Publish(LogEntry{Level: "info", Message: "global test"})
	snap := b.Snapshot()
	if len(snap) != 1 || snap[0].Message != "global test" {
		t.Errorf("global Publish failed: %+v", snap)
	}

	// Cleanup.
	SetBroker(nil)
}

// TestPublishWithoutBroker — Publish без инициализированного broker — no-op (не паника).
func TestPublishWithoutBroker(t *testing.T) {
	SetBroker(nil)
	Publish(LogEntry{Level: "info", Message: "ignored"}) // не должно паниковать
}

// contains — простой substring-search (избегаем strings.Contains для скорости чтения).
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}