package api

import (
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// F.2 (Session F) — Live tail Logs через WebSocket
// ============================================================
//
// Endpoint: GET /ws/logs (с auth через query ?token=... если включена).
//
// При подключении:
//  1. Проверяем токен (если auth включена).
//  2. Upgrade до WebSocket.
//  3. Отправляем snapshot ring buffer (последние ~100 записей) одной пачкой.
//  4. Подписываемся на broker и форвардим каждую новую запись клиенту.
//  5. Каждые 30 сек шлём {"type":"ping"} как keep-alive.
//
// Каждое сообщение — JSON-объект LogEntry: {time, level, message, source}.
// Snapshot: {"type":"snapshot","entries":[...],"count":N}.
// Ping: {"type":"ping"}.
//
// Graceful close: при отмене context клиента или ошибке WriteMessage — defer conn.Close()
// и broker auto-remove subscriber через <-sub.Done() горутину.

// wsLogsHandler — WebSocket endpoint для live tail системных логов.
//
// Подключается через logger.GetBroker() — глобальный broker, инициализированный
// в cmd/balancer/main.go с ring buffer 100.
//
// Формат сообщений (JSON-per-message):
//   - Snapshot: {"type":"snapshot", "entries":[LogEntry...], "count":N}
//   - Live entry: <JSON-encoded LogEntry>
//   - Ping (keep-alive): {"type":"ping"}
func (s *Server) wsLogsHandler(w http.ResponseWriter, r *http.Request) {
	// Auth до Upgrade — стандартная проверка через query token (EventSource/WS не поддерживают custom headers).
	if s.authenticator != nil && s.authenticator.IsEnabled() {
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "Unauthorized: token is required", http.StatusUnauthorized)
			return
		}
		valid, _ := s.authenticator.Authenticate(r)
		if !valid {
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Get().Errorw("ws/logs upgrade error", "error", err)
		return
	}
	defer conn.Close()

	// Получаем broker. Если nil (broker не инициализирован) — отдаём пустой snapshot и закрываем.
	broker := logger.GetBroker()
	if broker == nil {
		_ = conn.WriteJSON(map[string]interface{}{
			"type":    "snapshot",
			"entries": []logger.LogEntry{},
			"count":   0,
		})
		return
	}

	// Подписка на live stream.
	sub := broker.Subscribe()
	defer sub.Close()

	// Write deadline для всех операций.
	conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		return nil
	})

	// Goroutine для чтения (чтобы ловить close/ping от клиента).
	errCh := make(chan error, 1)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// 1) Отправляем snapshot (initial backlog).
	snapshot := broker.Snapshot()
	if err := conn.WriteJSON(map[string]interface{}{
		"type":    "snapshot",
		"entries": snapshot,
		"count":   len(snapshot),
	}); err != nil {
		logger.Get().Debugw("ws/logs snapshot write failed", "error", err)
		return
	}

	// 2) Live stream + ping ticker.
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case entry, ok := <-sub.Channel():
			if !ok {
				// broker закрыт — выходим, defer conn.Close() сделает cleanup.
				return
			}
			conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := conn.WriteJSON(entry); err != nil {
				logger.Get().Debugw("ws/logs entry write failed", "error", err)
				return
			}

		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(map[string]string{"type": "ping"}); err != nil {
				logger.Get().Debugw("ws/logs ping write failed", "error", err)
				return
			}

		case err := <-errCh:
			// Клиент отвалился или read timeout — закрываем соединение.
			if err != nil {
				logger.Get().Debugw("ws/logs client disconnected", "error", err)
			}
			return
		}
	}
}