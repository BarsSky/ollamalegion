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
// События шлются сразу (snapshot) + live-stream. Heartbeat : ping каждые 30 сек
// для поддержания соединения через прокси.

const (
	eventsBufferCapacity  = 100
	eventsHeartbeatPeriod = 10 * time.Second
)

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
// Connection: keep-alive
// X-Accel-Buffering: no (отключает nginx buffering)
//
// Формат: data: {json}\n\n
// Heartbeat: : ping\n\n каждые 30 сек (SSE comment, игнорируется клиентом).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// SSE headers (стандартный набор).
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
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
		ticker := time.NewTicker(eventsHeartbeatPeriod)
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

	ticker := time.NewTicker(eventsHeartbeatPeriod)
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