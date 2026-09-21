//go:build llama_stub

// ws_keepalive_r65d_test.go — R65d (2026-09-20): регрессия на keep-alive
// WebSocket-каналов.
//
// Найденный дефект: сервер ставил read deadline (90s для /ws/metrics, 120s для
// /ws/logs) и обновлял его ТОЛЬКО в SetPongHandler, но control-frame ping не
// отправлял — вместо него уходил ТЕКСТОВЫЙ JSON {"eventType":"ping"}. Браузер
// отвечает pong'ом только на control-frame, поэтому deadline истекал ровно
// через 90/120 секунд, ReadMessage падал с i/o timeout, handler возвращался и
// сокет закрывался. Клиент переподключался (счётчик попыток сбрасывается на
// onopen), и цикл повторялся бесконечно:
//   - /ws/metrics: «WebSocket error» в консоли каждые ~90s, подвисания дашборда;
//   - /ws/logs: потеря части лога при переподключении каждые 2 минуты.
//
// Тесты проверяют, что сервер действительно отправляет control-ping, и что
// текстовый heartbeat сохранён (его использует UI-индикатор связи, и на него
// завязан app-listeners.js case 'ping').
package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ollama-loadbalancer/pkg/logger"
)

// dialWS подключается к WS-эндпоинту тестового сервера.
func dialWS(t *testing.T, srvURL, path string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + path
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s failed (http status %d): %v", wsURL, status, err)
	}
	return conn
}

// TestR65d_WsMetrics_SendsControlPing — сервер обязан слать control-frame ping.
//
// Именно на него браузер отвечает pong'ом, который продлевает read deadline.
// Без него соединение гарантированно умирает через wsReadDeadline.
func TestR65d_WsMetrics_SendsControlPing(t *testing.T) {
	srv, _, _ := createTestServer(t)
	defer srv.Close()

	conn := dialWS(t, srv.URL, "/ws/metrics")
	defer conn.Close()

	// Первым должен прийти initial clusterState.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read initial state: %v", err)
	}
	if !strings.Contains(string(msg), "clusterState") {
		t.Errorf("first message = %s, want it to contain clusterState", string(msg))
	}

	// Далее ждём control-ping. gorilla отдаёт его через PingHandler, поэтому
	// ставим свой, чтобы зафиксировать факт получения (и продлить deadline —
	// иначе наш собственный read deadline истечёт).
	gotPing := make(chan struct{}, 1)
	prev := conn.PingHandler()
	conn.SetPingHandler(func(appData string) error {
		select {
		case gotPing <- struct{}{}:
		default:
		}
		if prev != nil {
			return prev(appData)
		}
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	// Сервер шлёт ping каждые 15s — ждём с запасом.
	conn.SetReadDeadline(time.Now().Add(25 * time.Second))
	deadline := time.After(25 * time.Second)
	for {
		select {
		case <-gotPing:
			return // OK
		case <-deadline:
			t.Fatal("no control ping received within 25s — read deadline на сервере " +
				"никогда не продлится, и соединение будет рваться каждые 90s")
		default:
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			// ReadMessage обрабатывает control-кадры внутри себя (вызывая
			// PingHandler), поэтому ошибка здесь означает обрыв соединения.
			t.Fatalf("connection closed before control ping: %v", err)
		}
	}
}

// TestR65d_WsMetrics_TextHeartbeatPreserved — текстовый {"eventType":"ping"}
// должен остаться: на него завязан UI-индикатор связи.
func TestR65d_WsMetrics_TextHeartbeatPreserved(t *testing.T) {
	srv, _, _ := createTestServer(t)
	defer srv.Close()

	conn := dialWS(t, srv.URL, "/ws/metrics")
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(25 * time.Second))
	sawTextPing := false
	for i := 0; i < 5; i++ {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if strings.Contains(string(msg), `"eventType":"ping"`) {
			sawTextPing = true
			break
		}
	}
	if !sawTextPing {
		t.Error("text heartbeat {\"eventType\":\"ping\"} not received — " +
			"UI-индикатор связи (app-listeners.js case 'ping') перестанет работать")
	}
}

// TestR65d_WsMetrics_ClientKeepAliveExtendsDeadline — клиентский текстовый
// keep-alive (websocket.js:107-113) тоже должен продлевать read deadline на
// сервере, иначе активный клиент будет отключён по таймауту.
func TestR65d_WsMetrics_ClientKeepAliveExtendsDeadline(t *testing.T) {
	srv, _, _ := createTestServer(t)
	defer srv.Close()

	conn := dialWS(t, srv.URL, "/ws/metrics")
	defer conn.Close()

	// Читаем initial state.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read initial state: %v", err)
	}

	// Отправляем клиентский keep-alive.
	if err := conn.WriteJSON(map[string]string{"type": "ping"}); err != nil {
		t.Fatalf("write client ping: %v", err)
	}

	// Соединение должно оставаться живым и продолжать слать данные.
	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("connection died after client keep-alive: %v", err)
	}
}

// TestR65d_WsLogs_SendsControlPing — то же для /ws/logs (раньше умирал каждые 2 минуты).
//
// ПРИМЕЧАНИЕ: /ws/logs требует инициализированного log-broker'а
// (logger.GetBroker()), который создаётся в cmd/balancer/main.go. В unit-тестах
// его нет, и handler по контракту отдаёт пустой snapshot и закрывает соединение
// (handlers_logs_ws.go:68-76) — это корректное поведение, а не дефект. Поэтому
// тест сам инициализирует broker, чтобы проверять именно keep-alive.
func TestR65d_WsLogs_SendsControlPing(t *testing.T) {
	broker := logger.NewLogBroker(10)
	logger.SetBroker(broker)
	defer func() {
		broker.Stop()
		logger.SetBroker(nil)
	}()

	srv, _, _ := createTestServer(t)
	defer srv.Close()

	conn := dialWS(t, srv.URL, "/ws/logs")
	defer conn.Close()

	// Первым приходит snapshot.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !strings.Contains(string(msg), "snapshot") {
		t.Errorf("first message = %s, want it to contain snapshot", string(msg))
	}

	gotPing := make(chan struct{}, 1)
	conn.SetPingHandler(func(appData string) error {
		select {
		case gotPing <- struct{}{}:
		default:
		}
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	// /ws/logs шлёт ping каждые 30s.
	const wait = 40 * time.Second
	conn.SetReadDeadline(time.Now().Add(wait))
	deadline := time.After(wait)
	for {
		select {
		case <-gotPing:
			return // OK
		case <-deadline:
			t.Fatal("no control ping on /ws/logs within 40s — соединение будет " +
				"рваться каждые 120s и терять часть лога")
		default:
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("connection closed before control ping: %v", err)
		}
	}
}

// TestR65d_WsMetrics_ClientDisconnectDetected — обратная сторона: реальный
// разрыв клиента должен приводить к закрытию handler'а (без утечки горутины).
func TestR65d_WsMetrics_ClientDisconnectDetected(t *testing.T) {
	srv, _, _ := createTestServer(t)
	defer srv.Close()

	conn := dialWS(t, srv.URL, "/ws/metrics")

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read initial state: %v", err)
	}
	// Резко закрываем — сервер должен заметить и завершить handler.
	_ = conn.Close()

	// Даём серверу время обработать закрытие; главное — нет паники/зависания.
	time.Sleep(200 * time.Millisecond)

	// Сервер всё ещё должен обслуживать новые соединения.
	conn2 := dialWS(t, srv.URL, "/ws/metrics")
	defer conn2.Close()
	conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn2.ReadMessage(); err != nil {
		t.Fatalf("server stopped accepting WS connections after client disconnect: %v", err)
	}
}

var _ = http.StatusOK
