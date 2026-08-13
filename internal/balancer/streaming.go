package balancer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// handleStreamingResponse - обработка streaming ответа с использованием Flusher.
// Поддерживает оба движка: Ollama (SSE/NDJSON) и llama.cpp (SSE/chunked).
// Включает heartbeat для поддержания соединения и улучшенную обработку ошибок.
// Go's net/http автоматически управляет chunked transfer encoding.
// Ручная запись chunked terminator запрещена — это приводит к двойному chunking'у
// и вызывает TransferEncodingError в OpenWebUI.
// Transfer-Encoding header не удаляем — Go управляет chunked encoding автоматически,
// и удаление заголовка вызывает конфликт с собственным chunking'ом Go.
func (p *Proxy) handleStreamingResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, backendID string) {
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

	// Loop detection: кольцевой буфер последних N чанков для детекции зацикливания LLM.
	recentChunksLimit := 16
	recentChunks := make([]string, 0, recentChunksLimit+1)

	// Устанавливаем read-deadline на backend-соединение, чтобы зависший стрим
	// (SIGSEGV в llama.cpp / timeout между чанками) корректно детектировался
	// как ошибка чтения, а не молчаливо "успешно завершался" по EOF.
	// Дедлайн продлевается на каждом успешном чанке.
	// Per-model адаптивный idle timeout: используется getModelStreamingIdleTimeout,
	// который применяет 3-tier resolver (profile → ModelLatencyTracker → global → default).
	idleTimeout := p.getModelStreamingIdleTimeout(modelFromCtx)
	if idleTimeout > 0 {
		if rc, ok := resp.Body.(interface {
			SetReadDeadline(time.Time) error
		}); ok {
			if err := rc.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
				logger.Get().Warnw("failed to set initial read deadline on backend body",
					"backend", backendID, "model", modelFromCtx, "error", err)
			}
		}
		logger.Get().Debugw("handleStreamingResponse: using per-model idle timeout",
			"backend", backendID, "model", modelFromCtx,
			"idle_timeout_sec", idleTimeout.Seconds())
	}


	// Heartbeat goroutine для SSE и NDJSON — предотвращает разрыв соединения nginx/браузером
	// при длительных паузах между токенами.
	// Для SSE: отправляется ":heartbeat\n\n" (SSE-комментарий, игнорируется клиентами).
	// Для NDJSON: отправляется "\n" (пустая строка — валидный NDJSON, игнорируется парсерами,
	// не содержит полей, которые могут сломать OpenWebUI).
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
					// КРИТИЧЕСКИ ВАЖНО: проверяем doneSent флаг перед отправкой heartbeat.
					// Если стрим уже завершился (done-чанк отправлен), но heartbeat
					// goroutine ещё не получила сигнал через heartbeatStop — не пишем
					// в ResponseWriter. Без этой проверки возможна гонка:
					//   1. Стрим завершён, done-чанк отправлен, main loop вышел
					//   2. Heartbeat тикает и пишет ещё один NDJSON с done:false
					//   3. Go net/http уже начал закрывать chunked encoding
					//   4. Дополнительные данные портят chunked terminator → TransferEncodingError
					if clientDisconnected.Load() {
						return
					}
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
					// Round 31 (2026-08-09): минимальный NDJSON heartbeat — только {"done":false}.
					// Без полей "model" и "message" (раньше они были) — OpenWebUI мог
					// интерпретировать "message":{"role":"assistant"} как начало нового
					// сообщения и сбрасывать ассемблирование reasoning секции.
					// {"done":false} — JSON-line валидный для Ollama-parser'а,
					// который игнорирует строки без "message" поля.
					hb := map[string]interface{}{"done": false}
					hbJSON, _ := json.Marshal(hb)
					heartbeatBytes = append(hbJSON, '\n')
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
			// Продлеваем read-deadline — пока идут данные, бэкенд жив.
			if idleTimeout > 0 {
				if rc, ok := resp.Body.(interface {
					SetReadDeadline(time.Time) error
				}); ok {
					_ = rc.SetReadDeadline(time.Now().Add(idleTimeout))
				}
			}
			// Детектор зацикливания (loop detection).
			// Если последние N чанков идентичны — модель, скорее всего, зациклилась
			// (распространённый паттерн при рассинхроне KV-cache или сломанной модели).
			// Прерываем стрим только при ЯВНЫХ косвенных признаках, не по таймауту.
			// См. также: bash-style loops в LLM при температуре 0 и chat templates.
			if repeated, pattern := detectLoopingChunk(buf[:n], recentChunks); repeated {
				logger.Get().Errorw("streaming loop detected — aborting stream",
					"backend", backendID, "model", modelFromCtx,
					"reason", "repeated_chunk_pattern",
					"pattern", pattern,
					"bytes_streamed", bytesStreamed,
					"chunk_count", chunkCount,
					"elapsed_sec", time.Since(startTime).Seconds())
				if isSSE {
					p.SendSSEErrorSafe(w, flusher, "loop_detected",
						"Model produced a repeating pattern, aborting stream to prevent infinite output", backendID, startTime)
				} else if isNDJSON {
					p.SendNDJSONErrorSafe(w, flusher,
						"Model produced a repeating pattern, aborting stream to prevent infinite output", backendID)
				}
				clientDisconnected.Store(true)
				return
			}
			recentChunks = append(recentChunks, string(buf[:n]))
			if len(recentChunks) > recentChunksLimit {
				recentChunks = recentChunks[len(recentChunks)-recentChunksLimit:]
			}
		}
		if err != nil {
			// === Различение реального EOF от idle-timeout ===
			// НА 20GB GPU с большими моделями (>12B params, partial offload):
			// генерация может иметь паузы между чанками >120s (особенно при
			// "thinking" или batch-обработке). SetReadDeadline срабатывает,
			// Go возвращает net.Error.Timeout() (НЕ io.EOF!). Однако в
			// streaming-loop многие HTTP-body wrappers конвертируют timeout
			// в io.EOF при чтении из переиспользуемого соединения. Поэтому
			// проверяем ОБА случая.
			//
			// Дополнительный сигнал: если lastActivity был > 60% idleTimeout назад
			// — это ВЫСОКОВЕРОЯТНО timeout, а не реальное завершение модели.
			isIdleTimeout := false
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				isIdleTimeout = true
			} else if err == io.EOF && time.Since(lastActivity) > (idleTimeout * 60 / 100) {
				// EOF пришёл после >60% idleTimeout с последнего чанка —
				// вероятно Go-сокет конвертировал timeout в EOF. Помечаем как timeout.
				isIdleTimeout = true
			}
			if isIdleTimeout {
				logger.Get().Errorw("STREAMING_IDLE_TIMEOUT",
					"backend", backendID, "model", modelFromCtx,
					"idle_timeout_sec", idleTimeout.Seconds(),
					"idle_actual_sec", time.Since(lastActivity).Seconds(),
					"bytes_streamed", bytesStreamed,
					"chunk_count", chunkCount,
					"elapsed_ms", time.Since(startTime).Milliseconds(),
					"raw_err", err.Error(),
					"hint", "Increase Balancing.StreamingIdleTimeout (or per-model profile.StreamingIdleTimeoutSec) for this model",
				)
				if !clientDisconnected.Load() {
					if isSSE {
						p.SendSSEErrorSafe(w, flusher, "idle_timeout",
							"Backend did not produce a chunk within idle_timeout ("+strconv.FormatFloat(idleTimeout.Seconds(), 'f', 0, 64)+"s). Increase LB_STREAMING_IDLE_TIMEOUT_SEC or model profile StreamingIdleTimeoutSec.", backendID, startTime)
					} else if isNDJSON {
						p.SendNDJSONErrorSafe(w, flusher,
							"Backend did not produce a chunk within idle_timeout. Increase LB_STREAMING_IDLE_TIMEOUT_SEC.", backendID)
					}
				}
				clientDisconnected.Store(true)
				return
			}
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
					p.SendSSEErrorSafe(w, flusher, "backend_read_error", "Connection lost during streaming", backendID, startTime)
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
//
// Round 35c+ (2026-08-13): real duration вместо hardcoded 0.
// streamStart — время начала запроса (для total_duration).
// Если streamStart.IsZero() (legacy callers), total_duration=0 (fallback).
func sendSSEDone(w http.ResponseWriter, flusher http.Flusher, streamStart time.Time) {
	totalDuration := int64(0)
	if !streamStart.IsZero() {
		totalDuration = time.Since(streamStart).Nanoseconds()
		if totalDuration < 0 {
			totalDuration = 0
		}
	}
	donePayload, _ := json.Marshal(map[string]interface{}{
		"done":              true,
		"total_duration":    totalDuration,
		"prompt_eval_count": 0,
		"eval_count":        0,
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
//
// Round 35c+ (2026-08-13): streamStart параметр для real total_duration в done-чанке.
// Если streamStart.IsZero() (legacy callers), total_duration=0.
func (p *Proxy) SendSSEErrorSafe(w http.ResponseWriter, flusher http.Flusher, code, message, backendID string, streamStart time.Time) {
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
	sendSSEDone(w, flusher, streamStart)

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

// detectLoopingChunk — детектор зацикливания LLM-стрима.
//
// Причины, по которым LLM зацикливается:
//   - KV-cache рассинхронизирован (баг в модели или квантизации);
//   - chat template вызывает модель войти в бесконечный repetition;
//   - модель упала в «echo mode» (повторяет последний токен/чанк);
//   - температура 0 + degenerate prompt → модель выбирает один и тот же токен.
//
// Стратегия: анализируем последние recentChunks (до 16) и ищем признаки цикла.
//   1. Текущий чанк совпадает с предыдущим ≥3 раза подряд
//   2. Pattern длиной ≤32 байта встречается в 8+ последних чанках
//   3. ≥5 из последних 6 чанков одинаковы полностью
//
// Возвращает (true, pattern) если зацикливание обнаружено, иначе (false, "").
//
// ВАЖНО (2026-06-22): используется для решения проблемы «обрыв стрима по непонятным причинам».
// Стрим теперь прерывается только при ЯВНЫХ косвенных признаках (loop detection),
// а не по таймаутам. Таймауты (idleTimeout) продлеваются на каждом чанке.
func detectLoopingChunk(current []byte, recent []string) (bool, string) {
	cur := string(current)
	if len(cur) < 4 {
		// Слишком короткий чанк (одиночный токен типа ".") — нечего детектировать.
		return false, ""
	}

	// 1) Точное совпадение с предыдущими чанками ≥3 раз подряд.
	if len(recent) >= 2 {
		sameCount := 1
		for i := len(recent) - 1; i >= 0 && recent[i] == cur; i-- {
			sameCount++
		}
		if sameCount >= 3 {
			return true, fmt.Sprintf("exact_match_x%d", sameCount)
		}
	}

	// 2) Pattern длиной ≤32 байта встречается в 8+ из последних чанков.
	if len(recent) >= 8 {
		for plen := 4; plen <= 32 && plen*2 <= len(cur)+32; plen++ {
			if len(cur) < plen {
				continue
			}
			pat := cur[len(cur)-plen:]
			if strings.TrimSpace(pat) == "" {
				continue
			}
			matches := 1 // текущий чанк
			for _, prev := range recent {
				if strings.Contains(prev, pat) {
					matches++
				}
			}
			if matches >= 8 {
				return true, fmt.Sprintf("pattern_len_%d_x%d", plen, matches)
			}
		}
	}

	// 3) ≥5 из последних 6 чанков одинаковы полностью.
	if len(recent) >= 6 {
		same := 0
		for _, prev := range recent[len(recent)-6:] {
			if prev == cur {
				same++
			}
		}
		if same >= 5 {
			return true, fmt.Sprintf("consecutive_copy_x%d", same+1)
		}
	}

	return false, ""
}
