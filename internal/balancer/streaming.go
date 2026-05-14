package balancer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

	// НЕ устанавливаем Connection: close на старте — это мешает Go корректно завершить
	// chunked encoding (0\r\n\r\n terminator) и вызывает TransferEncodingError у клиента.
	// Go net/http автоматически управляет закрытием соединения при ошибках.
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		logger.Get().Warnw("streaming detected but Flusher not supported, falling back to regular copy", "backend", backendID)
		io.Copy(w, resp.Body)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(contentType, "text/event-stream")
	isNDJSON := strings.Contains(contentType, "application/x-ndjson")

	modelFromCtx := ""
	if m, ok := r.Context().Value(modelContextKey).(string); ok {
		modelFromCtx = m
	}

	// Получаем sessionID для heartbeat и stream-active флага
	clientName := p.getClientName(r)
	sessionID := p.getSessionIDWithModel(r, clientName, modelFromCtx)

	// Помечаем сессию как имеющую активный streaming (защита от cleanup)
	if sessionID != "" && p.config.Balancing.SessionStickiness {
		p.sessionMgr.SetStreamActive(sessionID, true)
	}

	// Помечаем клиента в recentClients как имеющего активный стриминг для монитора
	p.markRecentClientStreamActive(r, true)

	// Регистрируем активный стрим для graceful shutdown
	p.activeStreams.Add(1)

	startTime := time.Now()

	logger.Get().Infow("starting streaming session",
		"backend", backendID, "model", modelFromCtx,
		"content_type", contentType, "is_sse", isSSE,
		"buf_size", 32*1024)

	buf := make([]byte, 32*1024)
	bytesStreamed := 0
	chunkCount := 0
	lastActivity := time.Now()
	var clientDisconnected atomic.Bool

	// Heartbeat goroutine для SSE и NDJSON — предотвращает разрыв соединения nginx/браузером
	// при длительных паузах между токенами.
	// Для SSE: отправляется ":heartbeat\n\n" (SSE-комментарий, игнорируется клиентами).
	// Для NDJSON: отправляется "{\"heartbeat\":true}\n" (валидная NDJSON-строка, не ломает парсер).
	var heartbeatStop chan struct{}
	var heartbeatWG sync.WaitGroup
	if isSSE || isNDJSON {
		heartbeatStop = make(chan struct{})
		heartbeatWG.Add(1)
		go func() {
			defer heartbeatWG.Done()
			ticker := time.NewTicker(p.getHeartbeatInterval())
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					sinceLastActivity := time.Since(lastActivity)
					logger.Get().Debugw("streaming heartbeat",
						"backend", backendID, "model", modelFromCtx,
						"idle_sec", sinceLastActivity.Seconds(),
						"bytes_streamed", bytesStreamed,
						"chunks", chunkCount,
						"is_sse", isSSE, "is_ndjson", isNDJSON)
					// Обновляем LastRequestAt в сессии чтобы предотвратить idle cleanup
					if sessionID != "" && p.config.Balancing.SessionStickiness {
						p.sessionMgr.HeartbeatStream(sessionID)
					}
					var heartbeatBytes []byte
					if isSSE {
						heartbeatBytes = []byte(":heartbeat\n\n")
					} else {
						heartbeatBytes = []byte("{\"heartbeat\":true}\n")
					}
					_, err := w.Write(heartbeatBytes)
					if err != nil {
						logger.Get().Warnw("streaming heartbeat write failed, stopping heartbeat",
							"backend", backendID, "error", err)
						clientDisconnected.Store(true)
						return
					}
					flusher.Flush()
				case <-heartbeatStop:
					return
				}
			}
		}()
	}

	// Гарантируем остановку heartbeat и освобождение ресурсов при любом выходе
	defer func() {
		// Уведомляем Shutdown о завершении стрима
		defer p.activeStreams.Done()

		// Снимаем флаг активного streaming в recentClients для монитора
		p.markRecentClientStreamActive(r, false)

		// Снимаем флаг активного streaming в сессии (разрешаем cleanup)
		if sessionID != "" && p.config.Balancing.SessionStickiness {
			p.sessionMgr.SetStreamActive(sessionID, false)
		}

		if heartbeatStop != nil {
			close(heartbeatStop)
			heartbeatWG.Wait()
		}

		// При clientDisconnected — немедленно закрываем соединение с бэкендом
		// без drain'а, чтобы не тратить ресурсы на уже ненужные данные.
		if resp.Body != nil {
			if clientDisconnected.Load() {
				// Прямое закрытие: обрываем TCP-соединение с бэкендом,
				// чтобы Ollama прекратила генерацию и освободила слот.
				resp.Body.Close()
			} else {
				// Нормальное завершение: drain оставшихся данных для возврата
				// HTTP-соединения в connection pool.
				drainLimit := int64(10 << 20)
				drained, _ := io.CopyN(io.Discard, resp.Body, drainLimit)
				if drained > 0 {
					logger.Get().Debugw("streaming: drained backend body",
						"backend", backendID, "bytes_drained", drained)
				}
				resp.Body.Close()
			}
		}
	}()

	for {
		// Проверяем отмену контекста клиента перед каждым чтением.
		// Это гарантирует быстрый выход при отмене запроса (Stop в Cline, закрытие вкладки).
		select {
		case <-r.Context().Done():
			ctxErr := r.Context().Err()
			logger.Get().Errorw("client cancelled streaming request",
				"backend", backendID, "model", modelFromCtx,
				"bytes_streamed", bytesStreamed, "chunks", chunkCount,
				"context_error", ctxErr,
				"elapsed_sec", time.Since(startTime).Seconds())
			clientDisconnected.Store(true)
			return
		default:
		}

		n, err := resp.Body.Read(buf)
		if n > 0 {
				// Проверяем, не отменён ли контекст перед записью клиенту.
			// Избегаем записи в закрытое соединение (write on closed connection).
			if r.Context().Err() != nil {
				logger.Get().Warnw("context cancelled during write, discarding chunk",
					"backend", backendID, "model", modelFromCtx,
					"bytes_streamed", bytesStreamed, "chunk_size", n)
				clientDisconnected.Store(true)
				return
			}
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				logger.Get().Errorw("error writing to client, aborting stream",
					"backend", backendID, "model", modelFromCtx,
					"write_error", writeErr, "bytes_before_error", bytesStreamed,
					"chunk_count", chunkCount,
					"elapsed_sec", time.Since(startTime).Seconds())
				clientDisconnected.Store(true)
				return
			}
			bytesStreamed += n
			chunkCount++
			lastActivity = time.Now()
			flusher.Flush()
		}
		if err != nil {
			if err == io.EOF {
				logger.Get().Infow("streaming session completed",
					"backend", backendID, "model", modelFromCtx,
					"bytes_streamed", bytesStreamed,
					"chunks", chunkCount,
					"total_duration_ms", time.Since(startTime).Milliseconds())
				// Явный flush после нормального завершения — гарантирует что все данные
				// (включая последний SSE done или NDJSON done) ушли в сокет до того
				// как Go отправит финальный chunked terminator (0\r\n\r\n).
				// Предотвращает гонку между TCP-буфером и HTTP-терминатором.
				flusher.Flush()
			} else {
				// КРИТИЧЕСКАЯ ОШИБКА: детальное логирование для диагностики TransferEncodingError
				ctxErr := ""
				if r.Context().Err() != nil {
					ctxErr = r.Context().Err().Error()
				}
				logger.Get().Errorw("STREAMING_BACKEND_READ_ERROR",
					"backend", backendID,
					"model", modelFromCtx,
				"read_error", err,
				"read_error_type", fmt.Sprintf("%T", err),
				"bytes_streamed", bytesStreamed,
				"chunk_count", chunkCount,
				"elapsed_ms", time.Since(startTime).Milliseconds(),
				"idle_ms", time.Since(lastActivity).Milliseconds(),
				"client_disconnected", clientDisconnected.Load(),
				"context_error", ctxErr,
					"is_sse", isSSE,
				)
				if !clientDisconnected.Load() {
				if isSSE {
					// Отправляем клиенту явную SSE-ошибку с done:true,
					// чтобы он корректно завершил поток и не получил TransferEncodingError
					logger.Get().Warnw("STREAMING_SENDING_SSE_ERROR",
						"backend", backendID,
						"model", modelFromCtx,
						"code", "backend_read_error",
						"message", "Connection lost during streaming",
						"bytes_streamed", bytesStreamed,
						"chunk_count", chunkCount,
					)
					p.SendSSEErrorSafe(w, flusher, "backend_read_error", "Connection lost during streaming", backendID)
				} else if isNDJSON {
					// Для NDJSON — отправляем done:true с ошибкой в том же формате,
					// чтобы клиент корректно завершил парсинг и не получил TransferEncodingError
					logger.Get().Warnw("STREAMING_SENDING_NDJSON_ERROR",
						"backend", backendID,
						"model", modelFromCtx,
						"code", "backend_read_error",
						"message", "Connection lost during streaming",
						"bytes_streamed", bytesStreamed,
						"chunk_count", chunkCount,
					)
					p.SendNDJSONErrorSafe(w, flusher, "Connection lost during streaming", backendID)
				}
				} else {
					// Клиент отключился — не отправляем done, просто выходим
				}
			}
			break
		}
	}
}

// sendSSEDone — отправляет клиенту финальное SSE-событие с done:true.
// Вызывается из sendSSEErrorSafe для завершения потока.
func sendSSEDone(w http.ResponseWriter, flusher http.Flusher) {
	donePayload, _ := json.Marshal(map[string]interface{}{
		"done":               true,
		"total_duration":     0,
		"prompt_eval_count":  0,
		"eval_count":         0,
	})
	_, err := w.Write([]byte("data: " + string(donePayload) + "\n\n"))
	if err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// SendNDJSONErrorSafe — отправляет клиенту NDJSON-сообщение с ошибкой и done:true.
// Предотвращает TransferEncodingError у клиента при ошибках чтения от бэкенда
// в NDJSON-стримах (/api/generate с Content-Type: application/x-ndjson).
func (p *Proxy) SendNDJSONErrorSafe(w http.ResponseWriter, flusher http.Flusher, message, backendID string) {
	donePayload, _ := json.Marshal(map[string]interface{}{
		"done":    true,
		"error":   "backend_read_error",
		"message": message,
	})
	msg := string(donePayload) + "\n"
	_, err := w.Write([]byte(msg))
	if err != nil {
		logger.Get().Debugw("sendNDJSONErrorSafe: client disconnected before done message",
			"backend", backendID, "write_error", err)
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// SendSSEErrorSafe — отправляет клиенту SSE-событие с ошибкой и завершающим done.
// Единый вызов, предотвращающий TransferEncodingError у клиента (OpenWebUI/aiohttp).
// Формат: сначала error событие, затем data с done:true.
func (p *Proxy) SendSSEErrorSafe(w http.ResponseWriter, flusher http.Flusher, code, message, backendID string) {
	// Отправляем корректное SSE событие ошибки с done:false — информируем о проблеме
	errorPayload, _ := json.Marshal(map[string]interface{}{
		"error":     code,
		"message":   message,
		"retryable": true,
		"done":      false,
	})
	// Единое сообщение: error-событие
	msg := "event: error\ndata: " + string(errorPayload) + "\n\n"
	_, err := w.Write([]byte(msg))
	if err != nil {
		logger.Get().Debugw("sendSSEErrorSafe: client disconnected before error event",
			"backend", backendID, "write_error", err)
		return
	}
	if flusher != nil {
		flusher.Flush()
	}

	// Отправляем done:true в формате data (как обычный завершающий чанк Ollama),
	// чтобы клиент получил корректный done и завершил поток без ошибок протокола.
	sendSSEDone(w, flusher)

	// Явный flush после done — гарантирует что все данные ушли в сокет
	// до того как Go отправит финальный chunked terminator (0\r\n\r\n).
	// Без этого возможна гонка: Go отправляет terminator, а не-flushed
	// данные всё ещё в буфере → nginx получает обрыв вместо done.
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