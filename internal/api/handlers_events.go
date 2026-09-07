package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// F.α (2026-06-28): session F — Server-Sent Events endpoint для нотификаций.
//
// Контекст: после F.0a Proxy.publishTransportEOF публикует события в EventBus
// при EOF от upstream cppworker. Чтобы оператор видел эти события в WebUI
// (bell icon + dropdown), нужно транслировать их через SSE.
//
// Endpoint: GET /api/v1/events?token=...
//
// Auth: token через query param (т.к. EventSource API браузера не поддерживает
// custom headers). Это та же схема что и для /ws/metrics.
//
// События шлются сразу (snapshot) + live-stream. Heartbeat : ping каждые 10 сек
// для поддержания соединения через прокси.

const (
	eventsBufferCapacity  = 100
	eventsHeartbeatPeriod = 10 * time.Second
)

// R60.7: переменная для override heartbeat period в unit-тестах.
// Default = eventsHeartbeatPeriod. Тесты могут установить 100ms для
// быстрой проверки long-running SSE без 10-секундного ожидания.
var eventsHeartbeatPeriodOverride time.Duration

// getHeartbeatPeriod возвращает eventsHeartbeatPeriodOverride если он
// задан (используется в unit-тестах для ускорения), иначе default.
func getHeartbeatPeriod() time.Duration {
	if eventsHeartbeatPeriodOverride > 0 {
		return eventsHeartbeatPeriodOverride
	}
	return eventsHeartbeatPeriod
}

// eventsBuffer — thread-safe ring buffer последних событий (для snapshot при reconnect).
type eventsBuffer struct {
	mu       sync.RWMutex
	events   []types.Event
	capacity int
}

func newEventsBuffer(capacity int) *eventsBuffer {
	return &eventsBuffer{
		events:   make([]types.Event, 0, capacity),
		capacity: capacity,
	}
}

func (eb *eventsBuffer) push(ev types.Event) {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	if len(eb.events) >= eb.capacity {
		// Ring buffer: удаляем самый старый
		eb.events = eb.events[1:]
	}
	eb.events = append(eb.events, ev)
}

func (eb *eventsBuffer) snapshot() []types.Event {
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	out := make([]types.Event, len(eb.events))
	copy(out, eb.events)
	return out
}

// eventsHub связывает EventBus (publisher) с SSE clients (subscribers) + ring buffer.
type eventsHub struct {
	buf *eventsBuffer
}

func newEventsHub() *eventsHub {
	return &eventsHub{
		buf: newEventsBuffer(eventsBufferCapacity),
	}
}

// handleEvents — SSE endpoint для notifications.
//
// GET /api/v1/events?token=<X-API-Token>
// Content-Type: text/event-stream
// Cache-Control: no-cache
// X-Accel-Buffering: no (отключает nginx buffering)
//
// Формат: data: {json}\n\n
// Heartbeat: : ping\n\n каждые 10 сек (SSE comment, игнорируется клиентом).
//
// R60.7 (2026-09-07): cmd/balancer/main.go apiHTTPServer.WriteTimeout=60s
// принудительно ставит SetWriteDeadline ровно на 60s от старта response.
// Go НЕ reset'ит deadline на каждой Write (deadlines — это absolute time,
// не timeouts). Без bypass SSE ровно через 60s получает EOF. Фикс:
// http.NewResponseController(w).SetWriteDeadline(time.Time{}) В НАЧАЛЕ
// handler снимает deadline, stream живёт до client disconnect.
//
// R60.7 также: убран заголовок "Connection: keep-alive" — HTTP/2 его
// запрещает (RFC 7540 §8.1.2.2), Go's chunked transfer-encoding уже
// подразумевает keep-alive для HTTP/1.1.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// R60.7: снимаем WriteDeadline на per-handler уровне. Без этого
	// http.Server.WriteTimeout=60s убивает SSE stream ровно через 60s,
	// независимо от частоты heartbeat (Go ставит SetWriteDeadline ОДИН раз
	// при старте response и не сбрасывает на каждом Write — это documented
	// поведение net/http: "Deadlines are not timeouts. Once set they stay
	// in force forever.").
	//
	// time.Time{} (zero value) означает "no deadline" — connection будет
	// жить пока client не отвалится или TCP keepalive не сработает.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		// Не критично — продолжим с server-level WriteTimeout, но логируем.
		// На старых Go (<1.20) или на http.ResponseWriter без underlying
		// net.Conn доступа http.NewResponseController вернёт error.
		logger.Get().Warnw("handleEvents: SetWriteDeadline failed (SSE may drop at server WriteTimeout)",
			"error", err, "remote", r.RemoteAddr)
	}

	// SSE headers (стандартный набор, R60.7: убран "Connection: keep-alive").
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	// Snapshot из ring buffer (последние 100 событий).
	if s.eventsHub != nil {
		for _, ev := range s.eventsHub.buf.snapshot() {
			if err := writeSSEEvent(w, ev); err != nil {
				logger.Get().Debugw("handleEvents: client disconnected during snapshot",
					"remote", r.RemoteAddr, "error", err)
				return
			}
		}
		flusher.Flush()
	}

	// Без eventBus — только heartbeat, чтобы клиент не висел.
	if s.eventBus == nil {
		ticker := time.NewTicker(getHeartbeatPeriod())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}

	// Подписка на EventBus — live stream.
	subID, subCh := s.eventBus.Subscribe()
	defer s.eventBus.Unsubscribe(subID)

	ticker := time.NewTicker(getHeartbeatPeriod())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-subCh:
			if !ok {
				return
			}
			// Фильтруем только notifications (для других типов есть свои endpoints).
			if ev.Type != types.EventNotification {
				continue
			}
			// Сохраняем в ring buffer для будущих подключений.
			if s.eventsHub != nil {
				s.eventsHub.buf.push(ev)
			}
			if err := writeSSEEvent(w, ev); err != nil {
				logger.Get().Debugw("handleEvents: client disconnected during live stream",
					"remote", r.RemoteAddr, "error", err)
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent пишет событие в формате SSE: "data: {json}\n\n".
func writeSSEEvent(w http.ResponseWriter, ev types.Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	return err
}