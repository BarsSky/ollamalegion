// proxy_request_hijack.go — Round 31 #6 real fix (2026-08-09):
// hijack-based client disconnect detection for streaming responses.
//
// Проблема (sub-bug): Go's net/http **не** detect'ит FIN-only client TCP close
// после request body read. Cancel latency 25-30s на Windows (Round 31 #6
// sub-bug investigation).
//
// Решение: hijack connection + periodic polling с SetReadDeadline(0).
// - SetReadDeadline(0) — read с immediate timeout (i/o timeout error)
// - Если client closed — read returns EOF или RST error
// - Cancel latency: <polling_interval (1s) после client close
//
// Подход:
// 1. Hijack http.ResponseWriter → net.Conn
// 2. Write HTTP response manually (status + headers + body через bufrw)
// 3. Spawn goroutine: periodic SetReadDeadline(0) + Read attempt
// 4. Если read fail'нет (не timeout) → client closed → cancel ctx
// 5. Main loop (existing) — write to bufrw вместо w
//
// Если hijack невозможен (HTTP/2, TLS without Hijacker) — fallback
// к существующему коду с w.Write + ReadTimeout=30s.
//
// См. также: https://github.com/golang/go/issues/29482
package balancer

import (
	"bufio"
	"fmt"
	"net/http"
	"strings"
)

// writeChunkedFrame — пишет один chunked-framed HTTP/1.1 chunk в bufrw.
// Round 32 #16 (2026-08-11): правильный chunked framing для SSE через
// hijack. Формат: "{hex_size}\r\n{data}\r\n". Для terminator (size=0)
// выводится "0\r\n\r\n".
func writeChunkedFrame(bufrw *bufio.ReadWriter, data []byte) error {
	if _, err := fmt.Fprintf(bufrw, "%x\r\n", len(data)); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := bufrw.Write(data); err != nil {
			return err
		}
	}
	if _, err := bufrw.WriteString("\r\n"); err != nil {
		return err
	}
	return bufrw.Flush()
}

// writeChunkedString — chunked-framed write для строки.
func writeChunkedString(bufrw *bufio.ReadWriter, s string) error {
	return writeChunkedFrame(bufrw, []byte(s))
}

// hijackReservedHeaders — заголовки, которые hijack-путь пишет вручную
// (content-length считается по фактически записанному телу, поэтому значение
// upstream'а использовать нельзя).
var hijackReservedHeaders = map[string]bool{
	"content-type":      true,
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
}

// hijackSSEReservedHeaders — то же + заголовки, которые SSE-ветка пишет
// вручную до вызова хелпера.
var hijackSSEReservedHeaders = map[string]bool{
	"content-type":      true,
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
	"cache-control":     true,
	"x-accel-buffering": true,
}

// writeHijackedHeaders — пишет заголовки вручную собранного HTTP/1.1 ответа
// (hijack-путь) в порядке приоритета:
//
//  1. всё, что балансер уже положил в w.Header() — X-Backend-ID, X-Session-ID,
//     X-Model-* (addModelCapabilitiesHeaders), CORS (выставляется в ServeHTTP);
//  2. заголовки upstream-ответа, которых ещё нет (кроме access-control-*:
//     CORS должен остаться тем, что выставил балансер).
//
// R66c (2026-09-22): РАНЬШЕ hijack-путь копировал только resp.Header и
// полностью игнорировал w.Header(). Из-за этого на всех hijack'нутых ответах
// клиент не получал ни X-Backend-ID (какой бэкенд обслужил запрос), ни
// X-Session-ID, ни X-Model-*, ни CORS — хотя обычный (не-hijack) путь их
// отдаёт, а в ServeHTTP CORS выставляется для всех ответов. Нашлось через
// tests/TestServeHTTP_LlamaCppRequest_RoutedToLlamaCppBackend: запрос
// обслуживался успешно (200), но X-Backend-ID в ответе отсутствовал.
func writeHijackedHeaders(bufrw *bufio.ReadWriter, w http.ResponseWriter, upstream http.Header, skip map[string]bool) {
	written := make(map[string]bool, 24)

	add := func(key string, values []string, skipKey func(string) bool) {
		lk := strings.ToLower(key)
		if written[lk] || skip[lk] || (skipKey != nil && skipKey(lk)) {
			return
		}
		written[lk] = true
		for _, v := range values {
			bufrw.WriteString(fmt.Sprintf("%s: %s\r\n", key, v))
		}
	}

	for key, values := range w.Header() {
		add(key, values, nil)
	}
	for key, values := range upstream {
		add(key, values, func(lk string) bool { return strings.HasPrefix(lk, "access-control-") })
	}
}

// proxyRequestOpenAIStreamingHijacked — hijack-based version для streaming
// ответов где нужна <1s client disconnect detection.
//
// Использует manual HTTP response writing + periodic polling goroutine
// для detect close. Fallback к w.Write path не работает на Windows
// без этого (см. Round 31 #6 sub-bug investigation).
//
// Принимает уже скопированные headers (из w.Header() — после копирования
// в proxyRequestOpenAIStreaming) + flusher. Manual response writing
// имитирует Go's HTTP server.
func (p *Proxy) proxyRequestOpenAIStreamingHijacked(w http.ResponseWriter, r *http.Request, resp *http.Response, backendID string, hj http.Hijacker) error {
	// Решение «перехватываем или пишем в ResponseWriter» принимает сток. Если
	// перехват не удался, сток вернёт обычный транспорт — и это НЕ повторный
	// диспатч (R66c: тот давал бесконечную рекурсию и падение процесса).
	if handled, err := p.openAIStreamNon2xxR493(w, resp); handled {
		return err
	}
	opened := p.openOpenAIStreamSinkR493(w, resp, backendID, hj, false)
	defer opened.close()
	return p.streamOpenAIWithSinkR493(w, r, resp, backendID, opened)
}
