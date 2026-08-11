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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
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
func (p *Proxy) proxyRequestOpenAIStreamingHijacked(
	w http.ResponseWriter,
	r *http.Request,
	resp *http.Response,
	backendID string,
	hj http.Hijacker,
) error {
	defer resp.Body.Close()

	// 0. Non-2xx handling — same as original (no streaming, just return error).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			body = []byte(fmt.Sprintf(`{"error":"upstream read error: %v"}`, readErr))
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		contentType := resp.Header.Get("Content-Type")
		if strings.Contains(contentType, "text/event-stream") {
			w.Header().Set("X-Accel-Buffering", "no")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(resp.StatusCode)
			fmt.Fprintf(w, "data: {\"error\":\"upstream returned HTTP %d\",\"choices\":[{\"delta\":{},\"finish_reason\":\"error\"}]}\n\n", resp.StatusCode)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return nil
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return nil
	}

	// 1. Hijack connection.
	clientConn, bufrw, hijackErr := hj.Hijack()
	if hijackErr != nil {
		logger.Get().Warnw("hijack failed, using fallback path",
			"backend", backendID, "error", hijackErr)
		return p.proxyRequestOpenAIStreaming(w, r, resp, backendID)
	}
	defer clientConn.Close()

	// Apply TCP keepalive (Round 31 #6 sub-bug fix).
	if tcp, ok := clientConn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(5 * time.Second)
	}

	logger.Get().Infow("hijack path active: client disconnect detection via SetReadDeadline polling",
		"backend", backendID, "cancel_latency_target", "<1s")

	// 2. Non-SSE handling — same as original.
	contentType := resp.Header.Get("Content-Type")
	isSSEResponse := strings.Contains(contentType, "text/event-stream") ||
		strings.Contains(contentType, "application/x-ndjson")
	if !isSSEResponse {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read non-streaming response: %w", err)
		}
		// Round 31 #7: token usage tracking.
		if resp.StatusCode < 400 {
			var openaiResp map[string]interface{}
			if json.Unmarshal(body, &openaiResp) == nil {
				if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
					var prompt, completion int64
					if v, ok := usage["prompt_tokens"].(float64); ok {
						prompt = int64(v)
					}
					if v, ok := usage["completion_tokens"].(float64); ok {
						completion = int64(v)
					}
					if prompt > 0 || completion > 0 {
						modelName, _ := openaiResp["model"].(string)
						p.recordTokenUsage(modelName, prompt, completion)
					}
				}
			}
		}
		// Write HTTP response manually via bufrw.
		// Status line.
		statusText := http.StatusText(resp.StatusCode)
		if statusText == "" {
			statusText = "Unknown"
		}
		bufrw.WriteString(fmt.Sprintf("HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText))
		// Headers.
		if contentType == "" {
			contentType = "application/json"
		}
		bufrw.WriteString(fmt.Sprintf("Content-Type: %s\r\n", contentType))
		bufrw.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(body)))
		bufrw.WriteString("Connection: close\r\n")
		bufrw.WriteString("\r\n")
		bufrw.Write(body)
		bufrw.Flush()
		return nil
	}

	// 3. SSE-стрим через bufrw.
	// Write HTTP/1.1 status + headers manually.
	//
	// Round 32 #16 (2026-08-11): chunked-framed SSE — fix для OpenWebUI aiohttp
	// TransferEncodingError. Round 32 #10 (2026-08-10) удалил Transfer-Encoding:
	// chunked header, и curl/raw socket парсили OK (они не проверяют HTTP/1.1
	// framing), НО OpenWebUI aiohttp строго требует либо Transfer-Encoding:
	// chunked С правильным фреймингом, либо Content-Length. Без обоих aiohttp
	// ругается "Not enough data to satisfy transfer length header" при обрыве
	// стрима (cancel/EOF без terminator).
	//
	// Правильный SSE через raw HTTP/1.1:
	//   1. Headers: Transfer-Encoding: chunked
	//   2. Каждый chunk данных фреймится как "{hex_size}\r\n{data}\r\n"
	//   3. Финальный chunk: "0\r\n\r\n" (terminator)
	//
	// Это то, что делает Go net/http автоматически для w.Write (в streaming.go).
	// Здесь в hijack mode Go не вмешивается — мы сами фреймим.
	//
	// Cancel detection (Round 31 #6): polling goroutine читает SetReadDeadline(0)
	// параллельно — работает независимо от chunked framing.
	statusText := http.StatusText(resp.StatusCode)
	if statusText == "" {
		statusText = "OK"
	}
	bufrw.WriteString(fmt.Sprintf("HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText))
	bufrw.WriteString("Content-Type: text/event-stream\r\n")
	bufrw.WriteString("Cache-Control: no-cache\r\n")
	bufrw.WriteString("Transfer-Encoding: chunked\r\n")
	bufrw.WriteString("Connection: keep-alive\r\n")
	bufrw.WriteString("X-Accel-Buffering: no\r\n")
	// Copy selected upstream headers.
	for key, values := range resp.Header {
		keyLower := strings.ToLower(key)
		if keyLower == "content-type" || keyLower == "transfer-encoding" ||
			keyLower == "content-length" || keyLower == "connection" ||
			strings.HasPrefix(keyLower, "access-control-") {
			continue
		}
		for _, value := range values {
			bufrw.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
		}
	}
	bufrw.WriteString("\r\n")
	bufrw.Flush()

	// 4. Polling goroutine — periodic SetReadDeadline(time.Unix(0,0)) для detect close.
	// time.Unix(0, 0) = epoch — deadline в прошлом, read returns **немедленно**
	// с timeout (i/o timeout). Если client closed — read returns EOF/RST (real error).
	// Cancel latency: <polling_interval после client close.
	clientCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	closed := atomic.Bool{}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Get().Warnw("hijack polling goroutine panic",
					"backend", backendID, "recover", r)
			}
		}()
		buf := make([]byte, 1)
		ticker := time.NewTicker(100 * time.Millisecond) // 10 polls/sec = <100ms detection
		defer ticker.Stop()
		for {
			select {
			case <-clientCtx.Done():
				return
			case <-ticker.C:
				if closed.Load() {
					return
				}
				// SetReadDeadline в epoch — read returns immediately.
				// - Client alive: i/o timeout error (ignorable, client ещё подключен)
				// - Client closed: EOF или connection reset (real error, detect close)
				_ = clientConn.SetReadDeadline(time.Unix(0, 0))
				_, err := clientConn.Read(buf)
				if err != nil {
					// i/o timeout = client alive, продолжаем polling
					var netErr net.Error
					if errors.As(err, &netErr) && netErr.Timeout() {
						continue
					}
					// EOF, RST, или другое = client closed
					if !closed.Load() {
						logger.Get().Infow("hijack polling: client disconnected (SetReadDeadline)",
							"backend", backendID, "error", err)
						closed.Store(true)
						cancel()
					}
					return
				}
				// Got 1 byte from client — это unexpected (мы не ждём данных от client в streaming),
				// но не критично. Продолжаем polling.
			}
		}
	}()

	// 5. Main loop — read from upstream, write to bufrw.
	heartbeatInterval := p.getHeartbeatInterval()
	type readResult struct {
		line []byte
		err  error
	}
	dataCh := make(chan readResult, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadBytes('\n')
			dataCh <- readResult{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()

	done := false
	for !done {
		select {
		case <-clientCtx.Done():
			// Polling goroutine detected client close (или ctx cancelled)
			logger.Get().Infow("hijack streaming: client disconnected, closing upstream",
				"backend", backendID)
			return nil

		case <-time.After(heartbeatInterval):
			// SSE-heartbeat: ": keepalive\r\n" — comment, игнорируется клиентом.
			// Round 32 #16: chunked-framed (был raw WriteString).
			if werr := writeChunkedString(bufrw, ": keepalive\r\n"); werr != nil {
				logger.Get().Warnw("hijack streaming: heartbeat write failed",
					"backend", backendID, "error", werr)
				return nil
			}

		case res := <-dataCh:
			if res.err != nil {
				if res.err == io.EOF {
					done = true
					break
				}
				logger.Get().Warnw("hijack streaming: read from backend error",
					"backend", backendID, "error", res.err)
				// Send final SSE done marker to client.
				_ = writeChunkedString(bufrw, "data: {\"error\":\"upstream read error\"}\n\n")
				return nil
			}

			lineToWrite, _ := filterOpenAIStreamingLine(res.line)
			if isOpenAISSEDataLine(lineToWrite) {
				if payload := extractSSEDataPayload(lineToWrite); payload != nil {
					if modified := extractToolCallsFromSSEContent(payload); modified != nil {
						lineToWrite = rewriteSSEDataPayload(lineToWrite, modified)
					}
				}
			}

			// Round 32 #16: chunked-framed write.
			if werr := writeChunkedFrame(bufrw, lineToWrite); werr != nil {
				logger.Get().Warnw("hijack streaming: write to client failed",
					"backend", backendID, "error", werr)
				return nil
			}
		}
	}

	// Round 32 #16 (2026-08-11): chunked terminator ("0\r\n\r\n") — ОБЯЗАТЕЛЕН
	// для strict clients (OpenWebUI aiohttp). Без него aiohttp ругается
	// "Not enough data to satisfy transfer length header" при чтении после EOF.
	// writeChunkedFrame с пустым data пишет "0\r\n\r\n" согласно HTTP/1.1 spec.
	if err := writeChunkedFrame(bufrw, nil); err != nil {
		logger.Get().Debugw("hijack streaming: chunked terminator write failed (client may have closed)",
			"backend", backendID, "error", err)
	}
	return nil
}
