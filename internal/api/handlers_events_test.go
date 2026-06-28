package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// F.α tests (2026-06-28): session F — SSE notifications endpoint.
//
// Acceptance criteria:
// 1. eventsBuffer — ring buffer с правильным overflow.
// 2. writeSSEEvent — корректный SSE format.
// 3. writeSSEEvent handles concurrent writes safely.
// 4. SSE Content-Type и headers установлены правильно.

// TestEventsBuffer_PushAndSnapshot — проверяет FIFO ring buffer.
func TestEventsBuffer_PushAndSnapshot(t *testing.T) {
	buf := newEventsBuffer(3)

	// Push 5 событий при capacity=3 — должно остаться только последние 3.
	for i := 0; i < 5; i++ {
		buf.push(types.Event{
			Type:      types.EventNotification,
			Timestamp: time.Now(),
			Severity:  types.SeverityInfo,
			Message:   "test-" + string(rune('0'+i)),
		})
	}

	snapshot := buf.snapshot()
	if len(snapshot) != 3 {
		t.Errorf("expected 3 events in snapshot, got %d", len(snapshot))
	}

	// Проверяем что это последние 3 (FIFO).
	expected := []string{"test-2", "test-3", "test-4"}
	for i, ev := range snapshot {
		if ev.Message != expected[i] {
			t.Errorf("event[%d].Message = %q, want %q", i, ev.Message, expected[i])
		}
	}
}

// TestEventsBuffer_ConcurrentPush — проверяет thread-safe push (race detector).
func TestEventsBuffer_ConcurrentPush(t *testing.T) {
	buf := newEventsBuffer(50)

	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				buf.push(types.Event{
					Type:     types.EventNotification,
					Severity: types.SeverityInfo,
					Message:  "concurrent",
				})
			}
		}()
	}
	wg.Wait()

	snapshot := buf.snapshot()
	if len(snapshot) != 50 {
		t.Errorf("expected 50 events after concurrent push, got %d", len(snapshot))
	}
}

// TestWriteSSEEvent_Format — проверяет корректный SSE-формат.
func TestWriteSSEEvent_Format(t *testing.T) {
	rec := httptest.NewRecorder()
	ev := types.Event{
		Type:      types.EventNotification,
		Timestamp: time.Unix(1700000000, 0).UTC(),
		Severity:  types.SeverityWarning,
		Source:    "transport",
		Message:   "EOF test",
	}

	if err := writeSSEEvent(rec, ev); err != nil {
		t.Fatalf("writeSSEEvent failed: %v", err)
	}

	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("SSE event should start with 'data: ', got: %s", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("SSE event should end with '\\n\\n', got: %s", body)
	}
	if !strings.Contains(body, `"type":"notification"`) {
		t.Errorf("SSE event should contain type:notification, got: %s", body)
	}
	if !strings.Contains(body, `"message":"EOF test"`) {
		t.Errorf("SSE event should contain message, got: %s", body)
	}
}

// TestHandleEvents_NilEventBus — без eventBus endpoint должен корректно завершиться по ctx.Done().
func TestHandleEvents_NilEventBus(t *testing.T) {
	s := &Server{
		// eventBus: nil — тестируем graceful degradation.
		eventsHub: newEventsHub(),
	}

	// Используем уже отменённый контекст чтобы endpoint быстро завершился.
	r := httptest.NewRequest("GET", "/api/v1/events", nil)
	ctx, cancel := newCancelledContext()
	defer cancel()
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	s.handleEvents(w, r)

	// Проверяем SSE headers (устанавливаются ДО любых блокирующих операций).
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("expected Content-Type=text/event-stream, got %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("expected Cache-Control=no-cache, got %q", got)
	}
	if got := w.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("expected X-Accel-Buffering=no, got %q", got)
	}
}

// newCancelledContext возвращает уже отменённый context.Context + cancel.
func newCancelledContext() (ctxLike, func()) {
	c := make(chan struct{})
	close(c)
	return ctxLike{done: c}, func() {}
}

// ctxLike — минимальная реализация context.Context для теста.
type ctxLike struct {
	done chan struct{}
}

func (c ctxLike) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c ctxLike) Done() <-chan struct{}        { return c.done }
func (c ctxLike) Err() error                   { return nil }
func (c ctxLike) Value(key any) any            { return nil }

// Не используется напрямую, но гарантирует что http импорт не пропадает.
var _ = http.MethodGet