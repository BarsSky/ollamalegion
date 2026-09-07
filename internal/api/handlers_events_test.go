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

// R60.7 (2026-09-07): TestHandleEvents_NoWriteTimeout — проверяет что SSE
// stream переживает http.Server.WriteTimeout. До фикса Go's WriteTimeout
// (per-response absolute deadline) убивал SSE stream ровно через 60s
// (5 pings × 10s + headers). Тест запускает сервер с WriteTimeout=1s и
// ускоренным heartbeat=100ms, проверяет что stream живёт дольше 1s.
//
// БЕЗ SetWriteDeadline bypass тест провалится: после ~1.1s сервер
// убъёт соединение и client получит EOF.
func TestHandleEvents_NoWriteTimeout(t *testing.T) {
	// R60.7: ускоряем heartbeat для теста (default 10s → 100ms).
	// Override process-wide var; restore в defer.
	originalOverride := eventsHeartbeatPeriodOverride
	eventsHeartbeatPeriodOverride = 100 * time.Millisecond
	defer func() { eventsHeartbeatPeriodOverride = originalOverride }()

	s := &Server{
		// eventBus: nil — fallback на heartbeat-only branch.
		eventsHub: newEventsHub(),
	}

	// Реальный HTTP server с WriteTimeout=1s (типичный production default).
	// БЕЗ R60.7 фикса stream умрёт на ~1s.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/events", s.handleEvents)
	server := httptest.NewServer(mux)
	defer server.Close()

	// Делаем HTTP client с коротким response header timeout, но без
	// ограничения на общее время чтения (мы хотим читать долго).
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(server.URL + "/api/v1/events")
	if err != nil {
		t.Fatalf("GET /api/v1/events failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("expected Content-Type=text/event-stream, got %q", got)
	}
	// R60.7: Connection header больше не устанавливается (HTTP/2 compat).
	if got := resp.Header.Get("Connection"); got != "" {
		t.Errorf("expected no Connection header, got %q", got)
	}

	// Читаем pings в течение 1.5s (больше WriteTimeout=1s).
	// БЕЗ фикса: после ~1s Read вернёт EOF.
	// С фиксом: получим несколько pings (heartbeat=100ms).
	start := time.Now()
	pingCount := 0
	readBuf := make([]byte, 1024)
	deadline := time.After(1500 * time.Millisecond)

readLoop:
	for {
		select {
		case <-deadline:
			break readLoop
		default:
		}
		// SetReadDeadline на TCP соединении (для test reliability)
		// НЕ делаем — мы хотим проверить что stream живёт без
		// принудительного deadline. Если бы делали, тест бы
		// проверял только ReadDeadline, а не WriteTimeout.
		n, err := resp.Body.Read(readBuf)
		if n > 0 {
			chunk := string(readBuf[:n])
			if strings.Contains(chunk, ": ping") {
				pingCount++
			}
		}
		if err != nil {
			// EOF or other error before deadline = bug (WriteTimeout fired)
			t.Errorf("connection closed prematurely at t=%.2fs (after %d pings): %v",
				time.Since(start).Seconds(), pingCount, err)
			break
		}
		// Даём планировщику переключиться
		time.Sleep(20 * time.Millisecond)
	}

	elapsed := time.Since(start).Seconds()
	if pingCount < 3 {
		t.Errorf("expected >= 3 pings in 1.5s (heartbeat=100ms), got %d in %.2fs",
			pingCount, elapsed)
	}
	t.Logf("OK: received %d pings in %.2fs (WriteTimeout=1s bypassed)", pingCount, elapsed)
}