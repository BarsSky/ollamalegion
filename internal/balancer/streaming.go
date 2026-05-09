package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// handleStreamingResponse - обработка streaming ответа с использованием Flusher.
// Включает heartbeat для поддержания соединения и улучшенную обработку ошибок.
// Go's net/http автоматически управляет chunked transfer encoding.
// Ручная запись chunked terminator запрещена — это приводит к двойному chunking'у
// и вызывает TransferEncodingError в OpenWebUI.
func (p *Proxy) handleStreamingResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, backendID string) {
	// Удаляем Transfer-Encoding из проксируемых заголовков — Go управляет этим автоматически.
	resp.Header.Del("Transfer-Encoding")

	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		logger.Get().Warnw("streaming detected but Flusher not supported, falling back to regular copy", "backend", backendID)
		io.Copy(w, resp.Body)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(contentType, "text/event-stream")
	logger.Get().Infow("starting streaming session", "backend", backendID,
		"content_type", contentType, "is_sse", isSSE)

	buf := make([]byte, 32*1024)
	bytesStreamed := 0

	// Heartbeat goroutine для SSE — предотвращает разрыв соединения nginx/браузером
	// при длительных паузах между токенами
	var heartbeatStop chan struct{}
	var heartbeatWG sync.WaitGroup
	if isSSE {
		heartbeatStop = make(chan struct{})
		heartbeatWG.Add(1)
		go func() {
			defer heartbeatWG.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					_, err := w.Write([]byte(":heartbeat\n\n"))
					if err != nil {
						return
					}
					flusher.Flush()
				case <-heartbeatStop:
					return
				}
			}
		}()
	}

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				logger.Get().Errorw("error writing to client, aborting stream", "backend", backendID, "error", writeErr)
				break
			}
			bytesStreamed += n
			flusher.Flush()
		}
		if err != nil {
			if err == io.EOF {
				logger.Get().Infow("streaming session completed", "backend", backendID,
					"bytes_streamed", bytesStreamed)
			} else {
				logger.Get().Errorw("error reading from backend", "backend", backendID, "error", err)
				if isSSE {
					sendSSEError(w, flusher, "backend_read_error", "Connection lost during streaming")
				}
			}
			break
		}
	}

	if heartbeatStop != nil {
		close(heartbeatStop)
		heartbeatWG.Wait()
	}
}

// sendSSEError — отправляет клиенту SSE-событие с ошибкой.
// Используется при разрыве соединения с бэкендом, чтобы клиент получил явную ошибку
// вместо обрыва соединения.
func sendSSEError(w http.ResponseWriter, flusher http.Flusher, code, message string) {
	// Отправляем корректное SSE событие ошибки
	errorPayload, _ := json.Marshal(map[string]interface{}{
		"error":     code,
		"message":   message,
		"retryable": true,
		"done":      false,
	})
	_, _ = w.Write([]byte("event: error\ndata: " + string(errorPayload) + "\n\nevent: error\ndata: {\"done\":true}\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// logStreamingError - логирование ошибок streaming
func (p *Proxy) logStreamingError(backendID string, err error) {
	logger.Get().Errorw("streaming error", "backend", backendID, "error", err)
}

// logError - логирование ошибки
func (p *Proxy) logError(backendID string, err error) {
	logger.Get().Errorw("proxy error", "backend", backendID, "error", err)
}