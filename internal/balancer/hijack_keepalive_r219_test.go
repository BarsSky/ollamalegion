//go:build llama_stub

// hijack_keepalive_r219_test.go — R219 (2026-10-21).
//
// Живой дефект: hijack-путь /v1 SSE объявлял "Connection: keep-alive", но после
// стрима закрывал соединение (defer clientConn.Close()). Клиенты с пулом
// соединений (Node/OpenAI SDK/OpenWebUI) переиспользовали мёртвый сокет и
// получали ECONNRESET на следующем запросе — под параллельной нагрузкой это
// выглядело как «один из трёх запросов не получил ответ».
//
// Тест держит контракт: заголовок НЕ обещает keep-alive, пока соединение
// закрывается; иначе регрессия вернётся молча.
package balancer

import (
	"os"
	"strings"
	"testing"
)

func TestR219_HijackDoesNotAdvertiseKeepAlive(t *testing.T) {
	raw, err := os.ReadFile("proxy_request_hijack.go")
	if err != nil {
		t.Fatalf("не читается proxy_request_hijack.go: %v", err)
	}
	src := string(raw)

	if strings.Contains(src, `"Connection: keep-alive\r\n"`) {
		t.Error("hijack-путь снова обещает keep-alive, но закрывает соединение: клиент получит ECONNRESET при переиспользовании сокета")
	}
	if !strings.Contains(src, `"Connection: close\r\n"`) {
		t.Error("hijack-путь не сообщает Connection: close")
	}
	// Соединение действительно закрывается — на этом и основан запрет keep-alive.
	if !strings.Contains(src, "clientConn.Close()") {
		t.Error("hijack-путь больше не закрывает соединение — тогда решение о Connection нужно пересмотреть")
	}
}

// Shutdown обязан дождаться стримов, зарегистрированных в hijack-пути:
//
// R271 (2026-10-29): регистрация переехала в общий помощник
// beginClientStreamR271 (active_streams_r271.go) — он ведёт и глобальный
// счётчик для Shutdown, и счётчики по бэкенду/модели. Поэтому проверяем две
// вещи: вызов помощника стоит ДО старта цикла чтения, и сам помощник реально
// инкрементирует activeStreams.
func TestR219_HijackRegistersStreamBeforeDrain(t *testing.T) {
	raw, err := os.ReadFile("proxy_request_hijack.go")
	if err != nil {
		t.Fatalf("не читается proxy_request_hijack.go: %v", err)
	}
	src := string(raw)
	addIdx := strings.Index(src, "beginClientStreamR271(")
	loopIdx := strings.Index(src, "clientCtx, cancel := context.WithCancel")
	if addIdx < 0 || loopIdx < 0 {
		t.Fatal("не найдены регистрация стрима (beginClientStreamR271) или старт цикла")
	}
	if addIdx > loopIdx {
		t.Error("стрим регистрируется ПОСЛЕ старта цикла — Shutdown может не успеть увидеть стрим")
	}

	helperRaw, err := os.ReadFile("active_streams_r271.go")
	if err != nil {
		t.Fatalf("не читается active_streams_r271.go: %v", err)
	}
	if !strings.Contains(string(helperRaw), "p.activeStreams.Add(1)") {
		t.Error("помощник не инкрементирует activeStreams — Shutdown не увидит стрим")
	}
}
