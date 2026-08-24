package balancer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// proxyRequestLlamaCppNonStream — проксирует запрос к llama.cpp с принудительным
// отключением streaming (для совместимости с клиентами которые ломают streaming).
func (p *Proxy) proxyRequestLlamaCppNonStream(w http.ResponseWriter, r *http.Request, backendID string, bodyBuf []byte) error {
	state, ok := p.backends[backendID]
	if !ok {
		return fmt.Errorf("backend %s not found", backendID)
	}

	targetURL := p.getBackendBaseURL(state.Backend)
	modelFromCtx := ""
	if m, ok := r.Context().Value(modelContextKey).(string); ok {
		modelFromCtx = m
	}

	originalPath := r.URL.Path
	llamacppPath := translatePathForLlamaCpp(originalPath)
	bodyNoStream := stripStreamFlag(bodyBuf)

	// PREFLIGHT n_ctx auto-reload: если клиент задал options.num_ctx,
	// а loaded n_ctx на бэкенде меньше — перезагружаем модель на нужный n_ctx
	// ДО проксирования (аналогично proxyRequestLlamaCpp).
	//
	// Стратегия по умолчанию (PreflightSyncEnabled=true, default): СИНХРОННО ждём
	// завершения reload (до PreflightSyncTimeoutMs, default 60s), затем проксируем.
	// Round-trip один — клиент не получает EOF. При таймауте fallback на async
	// 503 + Retry-After: 15.
	//
	// Для streaming-клиентов остаётся старая async логика (Reload-Disabled-For-Tools,
	// чтобы не блокировать стрим на reload).
	var preflightOK bool
	var preflightMsg string
	var preflightStatus int
	var retryAfter int = 30 // Round 23 (2026-08-04): 5 → 30 (реальное время reload)
	var preflightBody []byte
	if p.config != nil && p.config.Balancing.PreflightSyncEnabled {
		preflightBody, preflightOK, preflightMsg, preflightStatus, retryAfter =
			p.preflightNCtxReloadIfNeededSync(r.Context(), backendID, modelFromCtx, bodyNoStream, originalPath)
		if preflightOK {
			bodyNoStream = preflightBody
			logger.Get().Debugw("proxyRequestLlamaCppNonStream: preflight n_ctx sync-reload applied",
				"backend", backendID, "model", modelFromCtx, "new_body_len", len(bodyNoStream))
		}
	} else {
		preflightBody, preflightOK, preflightMsg, preflightStatus =
			p.preflightNCtxReloadIfNeeded(r.Context(), backendID, modelFromCtx, bodyNoStream, originalPath)
		if preflightOK {
			bodyNoStream = preflightBody
			logger.Get().Debugw("proxyRequestLlamaCppNonStream: preflight n_ctx reload applied",
				"backend", backendID, "model", modelFromCtx, "new_body_len", len(bodyNoStream))
		}
	}
	if !preflightOK && (preflightMsg != "" || preflightStatus != http.StatusOK) {
		// Reload запущен в фоне (async) ИЛИ sync timeout — отдаём 503.
		logger.Get().Infow("proxyRequestLlamaCppNonStream: preflight n_ctx reload, returning 503",
			"backend", backendID, "model", modelFromCtx,
			"msg", preflightMsg, "status", preflightStatus, "retry_after", retryAfter)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		w.Header().Set("X-NCtx-Reload-Decision", "async-reload")
		body := []byte(fmt.Sprintf(`{"error":%q,"decision":"async_reload","retry_after_seconds":%d}`,
			preflightMsg, retryAfter))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(body)
		return nil
	}

	// R54.4 (2026-08-24): AutoTune autonomous reload hook (non-stream path).
	// Тот же hook что и в proxyRequestLlamaCpp — AutoTune ловит ДРУГИЕ
	// sub-optimal state'ы которые n_ctx preflight пропускает (kv_cache, layers).
	if modelFromCtx != "" {
		var freeVRAM, freeRAM, totalVRAM uint64
		if p.metricsMgr != nil {
			clusterState := p.GetClusterState()
			for _, b := range clusterState.Backends {
				if b.ID == backendID {
					totalVRAM = b.GPU.MemoryTotal
					if b.GPU.MemoryFree > 0 {
						freeVRAM = b.GPU.MemoryFree
					}
					freeRAM = b.System.MemoryFree
					break
				}
			}
		}
		autoTuneRes := p.triggerAutoTuneReload(backendID, modelFromCtx, freeVRAM, freeRAM, totalVRAM)
		if autoTuneRes != nil && autoTuneRes.Triggered {
			logger.Get().Infow("proxyRequestLlamaCppNonStream: AutoTune async reload triggered",
				"backend", backendID, "model", modelFromCtx,
				"reason", autoTuneRes.Plan.Reason)
		} else if autoTuneRes != nil && autoTuneRes.SkippedReason != "" {
			logger.Get().Debugw("proxyRequestLlamaCppNonStream: AutoTune check",
				"backend", backendID, "model", modelFromCtx,
				"skipped_reason", autoTuneRes.SkippedReason)
		}
	}

	translatedBody, err := translateOllamaBodyToOpenAI(originalPath, bodyNoStream)
	if err != nil {
		translatedBody = bodyNoStream
	}

	fullURL := targetURL + llamacppPath
	logger.Get().Debugw("proxyRequestLlamaCppNonStream",
		"backend", backendID, "url", fullURL,
		"translated_body", string(translatedBody)[:min(300, len(translatedBody))])

	req, err := http.NewRequestWithContext(r.Context(), r.Method, fullURL, bytes.NewReader(translatedBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for key, values := range r.Header {
		lowerKey := strings.ToLower(key)
		if lowerKey == "content-type" || lowerKey == "accept" || lowerKey == "content-length" || lowerKey == "host" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// F.0.4 (2026-06-28): session F — error context + EOF retry для non-streaming.
	//
	// Тот же фикс что и в proxyRequestLlamaCpp: добавляем backend_id, attempt,
	// duration_ms в error message и публикуем EOF event в EventPublisher.
	client := p.client
	nonStreamStart := time.Now()
	nonStreamAttempt := 1
	resp, err := client.Do(req)
	// 2026-07-10: connection-level error retry with exponential backoff.
	//
	// cppworker может быть в recreate-фазе (docker restart, build deploy):
	//   - Порт 18092 ещё не listening → dial tcp: connection refused
	//   - Контейнер запускается, но init не закончен → connection reset
	//   - DNS не резолвится (редко) → no such host
	//
	// Раньше balancer делал мгновенный retry (attempt=1, attempt=2 без задержки),
	// что приводило к OpenWebUI ошибкам "connection_refused" в transition window.
	// Теперь: 3 retry с backoff 1s, 2s, 4s (cap 7s total) перед тем как сдаться.
	// Только для connection-level ошибок — HTTP ошибки (5xx, 4xx) идут через
	// существующий 503/loading_retry путь без backoff.
	if err != nil && isConnectionLevelError(err) {
		maxConnectionRetries := 3
		connectionBackoffs := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
		for retryIdx := 0; retryIdx < maxConnectionRetries; retryIdx++ {
			backoff := connectionBackoffs[retryIdx]
			logger.Get().Warnw("proxyRequestLlamaCppNonStream: connection-level error, backing off before retry",
				"backend", backendID,
				"model", modelFromCtx,
				"attempt", nonStreamAttempt,
				"retry_idx", retryIdx+1,
				"max_retries", maxConnectionRetries,
				"backoff_ms", backoff.Milliseconds(),
				"error", err)
			// Respect request context cancellation during backoff
			select {
			case <-time.After(backoff):
			case <-r.Context().Done():
				durationMs := time.Since(nonStreamStart).Milliseconds()
				return fmt.Errorf("llama.cpp request failed [backend=%s, attempt=%d, duration_ms=%d, error_type=%s, cancelled_during_backoff]: %v",
					backendID, nonStreamAttempt, durationMs, determineErrorType(err, r.Context()), err)
			}
			nonStreamAttempt++
			// Recreate req body reader (it's been consumed)
			req2, reqErr := http.NewRequestWithContext(r.Context(), r.Method, fullURL, bytes.NewReader(translatedBody))
			if reqErr != nil {
				return fmt.Errorf("failed to create retry request: %v", reqErr)
			}
			req2.Header.Set("Content-Type", "application/json")
			req2.Header.Set("Accept", "application/json")
			resp, err = client.Do(req2)
			if err == nil {
				break // success
			}
			if !isConnectionLevelError(err) {
				// Если ошибка изменилась на non-connection-level, выходим из retry loop
				break
			}
		}
	}
	if err != nil {
		durationMs := time.Since(nonStreamStart).Milliseconds()
		errType := determineErrorType(err, r.Context())
		logger.Get().Errorw("proxyRequestLlamaCppNonStream: upstream Do() failed",
			"backend", backendID,
			"model", modelFromCtx,
			"url", fullURL,
			"attempt", nonStreamAttempt,
			"duration_ms", durationMs,
			"error_type", errType,
			"error", err)
		if errType == "unexpected_eof" {
			p.publishTransportEOF(backendID, modelFromCtx, originalPath, err, time.Duration(durationMs)*time.Millisecond)
		}
		// Если после всех retry connection_refused — помечаем backend unhealthy.
		// Это предотвращает routing новых запросов к мёртвому backend до следующего healthcheck.
		if errType == "connection_refused" {
			p.markBackendConnectionFailed(backendID)
		}
		return fmt.Errorf("llama.cpp request failed [backend=%s, attempt=%d, duration_ms=%d, error_type=%s]: %v",
			backendID, nonStreamAttempt, durationMs, errType, err)
	}
	defer resp.Body.Close()

	// 2026-07-01: transparent lazy-load retry (см. loading_retry.go).
	//
	// cppworker при первом inference-запросе к незагруженной модели отвечает
	// HTTP 503 с body {"error":"model is loading: <name>","loading":true,...}.
	// Без retry OpenWebUI/Cline видел оборванный JSON-ответ. Теперь:
	//   1. Читаем 503 body.
	//   2. Если loading — polling /api/models/load/progress (llama.cpp) или
	//      /api/ps (ollama) до 30 сек.
	//   3. После успешного polling повторяем client.Do(req) с тем же body.
	if resp.StatusCode == http.StatusServiceUnavailable {
		loadingBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			loadingBody = []byte(fmt.Sprintf(`{"error":"upstream read error: %v"}`, readErr))
		}
		isLoading, loadingModelName, _ := loadingSignalFromBody(resp.StatusCode, loadingBody)
		if isLoading {
			if loadingModelName == "" {
				loadingModelName = modelFromCtx
			}
			logger.Get().Infow("proxyRequestLlamaCppNonStream: 503 'model is loading', polling for load completion",
				"backend", backendID, "model", loadingModelName,
				"duration_ms", time.Since(nonStreamStart).Milliseconds())

			loaded, waitErr := p.waitForBackendModelLoaded(r.Context(), state.Backend, loadingModelName, backendID)
			if waitErr != nil {
				logger.Get().Warnw("proxyRequestLlamaCppNonStream: waitForBackendModelLoaded error",
					"backend", backendID, "model", loadingModelName, "error", waitErr)
			}
			if loaded {
				nonStreamAttempt++
				// Пересоздаём req: body — это bytes.NewReader(translatedBody) (т.к. translateOllamaBodyToOpenAI выше).
				req2, reqErr := http.NewRequestWithContext(r.Context(), r.Method, fullURL, bytes.NewReader(translatedBody))
				if reqErr != nil {
					return fmt.Errorf("failed to create retry request: %v", reqErr)
				}
				req2.Header.Set("Content-Type", "application/json")
				req2.Header.Set("Accept", "application/json")
				for key, values := range r.Header {
					lowerKey := strings.ToLower(key)
					if lowerKey == "content-type" || lowerKey == "accept" || lowerKey == "content-length" || lowerKey == "host" {
						continue
					}
					for _, value := range values {
						req2.Header.Add(key, value)
					}
				}
				resp, err = client.Do(req2)
				if err != nil {
					durationMs := time.Since(nonStreamStart).Milliseconds()
					errType := determineErrorType(err, r.Context())
					logger.Get().Errorw("proxyRequestLlamaCppNonStream: upstream Do() failed after loading retry",
						"backend", backendID, "model", modelFromCtx, "attempt", nonStreamAttempt,
						"duration_ms", durationMs, "error_type", errType, "error", err)
					if errType == "unexpected_eof" {
						p.publishTransportEOF(backendID, modelFromCtx, originalPath, err, time.Duration(durationMs)*time.Millisecond)
					}
					return fmt.Errorf("llama.cpp request failed after load retry [backend=%s, attempt=%d, duration_ms=%d, error_type=%s]: %v",
						backendID, nonStreamAttempt, durationMs, errType, err)
				}
				defer resp.Body.Close()
				logger.Get().Infow("proxyRequestLlamaCppNonStream: retry after load successful",
					"backend", backendID, "model", modelFromCtx,
					"attempt", nonStreamAttempt, "duration_ms", time.Since(nonStreamStart).Milliseconds())
			} else {
				// Polling исчерпан — отдаём 503 клиенту с понятным сообщением.
				logger.Get().Warnw("proxyRequestLlamaCppNonStream: load wait timeout, returning 503 to client",
					"backend", backendID, "model", loadingModelName)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "5")
				w.Header().Set("X-Model-Loading-Retry", "exhausted")
				errBody, _ := json.Marshal(map[string]interface{}{
					"error":         fmt.Sprintf("model is still loading after %ds: %s", loadingRetryMaxAttempts*int(loadingRetryInterval.Seconds()), loadingModelName),
					"loading":       true,
					"model":         loadingModelName,
					"retryAfterMs":  5000,
				})
				w.Header().Set("Content-Length", strconv.Itoa(len(errBody)))
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write(errBody)
				return nil
			}
		} else {
			// 503 без признака loading — восстанавливаем body и идём по обычному пути.
			resp = &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(bytes.NewReader(loadingBody)),
				Header:     make(http.Header),
			}
		}
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %v", err)
	}

	logger.Get().Infow("proxyRequestLlamaCppNonStream: response",
		"backend", backendID, "status", resp.StatusCode,
		"body_len", len(respBody),
		"body", string(respBody)[:min(500, len(respBody))])

	// ==== Сброс cycle counter при успешном ответе ====
	// Если upstream вернул 200 OK — это инференс без n_ctx ошибки.
	// Сбрасываем счётчик cycle detection, чтобы stale failures не накапливались.
	if resp.StatusCode == http.StatusOK && len(respBody) > 0 {
		if p.nctxReload != nil {
			p.nctxReload.ResetCycleCounter(backendID)
		}
	}

	// ==== n_ctx auto-reload: перехват структурированной ошибки от cppworker ====
	// cppworker при n_ctx overflow отвечает HTTP 400 (Bad Request) с JSON
	// {error, code: 2 (N_CTX_NEEDS_RELOAD), bridge_info: {...}}. Если
	// auto-reload включён — перезагружаем модель с большим n_ctx и повторяем.
	if resp.StatusCode >= 400 {
		if nctxErr := ParseCppWorkerError(respBody, resp.StatusCode, backendID); nctxErr != nil {
			if errors.Is(nctxErr, bridge.ErrNCtxNeedsReload) || errors.Is(nctxErr, bridge.ErrPromptTooLong) {
				logger.Get().Infow("proxyRequestLlamaCppNonStream: detected n_ctx error from cppworker, invoking auto-reload",
					"backend", backendID, "model", modelFromCtx,
					"status", resp.StatusCode,
					"is_n_ctx", errors.Is(nctxErr, bridge.ErrNCtxNeedsReload),
					"is_prompt_too_long", errors.Is(nctxErr, bridge.ErrPromptTooLong))
				p.handleNCtxReload(r.Context(), w, r, backendID, modelFromCtx, nctxErr, bodyBuf)
				return nil
			}
		}
	}

	// Если upstream вернул НЕ-200 — пробрасываем как Ollama-ошибку с done:true.
	// Без этого OpenWebUI получает "пустой ответ" с done:true и рендерит пустое
	// сообщение ассистента. С этой правкой клиент видит error в NDJSON-чанке
	// и может корректно показать диагностику.
	// ВАЖНО: устанавливаем Content-Length явно, чтобы aiohttp/OpenWebUI не получили
	// chunked transfer encoding для plain JSON — иначе TransferEncodingError.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := json.Marshal(map[string]interface{}{
			"model":       modelFromCtx,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
			"done":        true,
			"done_reason": "error",
			"error":       fmt.Sprintf("upstream returned HTTP %d: %s", resp.StatusCode, string(respBody)),
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": "",
			},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(errBody)))
		w.WriteHeader(http.StatusBadGateway)
		w.Write(errBody)
		return nil
	}

	// Если body пустой и статус 200 — это обычно означает, что cppworker ещё
	// загружает модель (или вернул мусор). Возвращаем 502 с явной диагностикой,
	// чтобы OpenWebUI не рендерил "пустое сообщение ассистента".
	// ВАЖНО: устанавливаем Content-Length явно — предотвращает TransferEncodingError.
	if len(respBody) == 0 {
		logger.Get().Warnw("proxyRequestLlamaCppNonStream: empty body from upstream",
			"backend", backendID, "model", modelFromCtx)
		errBody, _ := json.Marshal(map[string]interface{}{
			"model":       modelFromCtx,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
			"done":        true,
			"done_reason": "error",
			"error":       "upstream returned empty response (model may still be loading)",
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": "",
			},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(errBody)))
		w.WriteHeader(http.StatusBadGateway)
		w.Write(errBody)
		return nil
	}

	// Если пришёл SSE (вдруг), собираем — с поддержкой tool_calls
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") || bytes.HasPrefix(respBody, []byte("data:")) {
		var fullContent string
		var fullReasoning string // 2026-07-01: для reasoning-моделей собираем reasoning_content
		var modelName = modelFromCtx
		var finishReason = "stop"
		toolAccum := make(map[int]*accumulatedToolCall)
		scanner := bufio.NewScanner(bytes.NewReader(respBody))
		scanner.Buffer(make([]byte, 0), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if delta, ok := choice["delta"].(map[string]interface{}); ok {
						// Извлекаем tool_calls из delta (инкрементально)
						accumulateToolCallsFromDelta(delta, toolAccum)
						if c, ok := delta["content"].(string); ok {
							fullContent += c
						}
						// 2026-07-01: reasoning-парсер (qwen3.5/qwen3.6/deepseek-r1/gemma-4).
						// cppworker эмитит отдельный SSE-чанк с delta.reasoning_content, аккумулируем.
						if r, ok := delta["reasoning_content"].(string); ok && r != "" {
							fullReasoning += r
						}
					} else if msg, ok := choice["message"].(map[string]interface{}); ok {
						if c, ok := msg["content"].(string); ok {
							// Детектируем JSON tool calls в content для non-stream
							if detectedTC, remainingTC, found := detectAndExtractToolCallsFromContent(c); found && len(detectedTC) > 0 {
								logger.Get().Infow("proxyRequestLlamaCppNonStream: detected tool_calls in content",
									"tool_calls_count", len(detectedTC))
								// Clean remaining: remove service tokens, duplicate tool calls, role markers
							cleanedTC := stripServiceTokens(remainingTC)
							cleanedTC = cleanContentAfterToolCallExtraction(cleanedTC)
							fullContent += cleanedTC
								// Конвертируем детектированные tool_calls в accumulatedToolCall формат
								for i, rawTC := range detectedTC {
									if tcMap, ok := rawTC.(map[string]interface{}); ok {
										acc := &accumulatedToolCall{
											index:    len(toolAccum) + i,
											function: make(map[string]interface{}),
										}
										if id, ok := tcMap["id"].(string); ok && id != "" {
											acc.id = id
										}
										if fn, ok := tcMap["function"].(map[string]interface{}); ok {
											for k, v := range fn {
												acc.function[k] = v
											}
										}
										toolAccum[len(toolAccum)] = acc
									}
								}
							} else {
								fullContent += c
							}
						}
						// Проверяем tool_calls в message (если чанк — это уже готовый message)
						if tc, ok := msg["tool_calls"].([]interface{}); ok && len(tc) > 0 {
							for _, rawTC := range tc {
								if tcMap, ok := rawTC.(map[string]interface{}); ok {
									idx := len(toolAccum)
									acc := &accumulatedToolCall{
										index:    idx,
										function: make(map[string]interface{}),
									}
									if id, ok := tcMap["id"].(string); ok && id != "" {
										acc.id = id
									}
									if fn, ok := tcMap["function"].(map[string]interface{}); ok {
										for k, v := range fn {
											acc.function[k] = v
										}
									}
									toolAccum[idx] = acc
								}
							}
						}
					}
					if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
						finishReason = fr
					}
				}
			}
			if m, ok := chunk["model"].(string); ok && m != "" {
				modelName = m
			}
		}
		var ollamaBody []byte
		if originalPath == "/api/generate" {
			// Round 51.3 (2026-08-20): done:true для ЛЮБОГО непустого finish_reason.
			// Раньше done:true только для "stop" и "tool_calls" → Cline (ollama npm)
			// валился с "Did not receive done or success response" для finish_reason="length".
			resp := map[string]interface{}{
				"model":       modelName,
				"created_at":  time.Now().UTC().Format(time.RFC3339),
				"response":    fullContent,
				"done":        finishReason != "",
				"done_reason": finishReason,
			}
			// 2026-07-01: для reasoning-моделей (qwen3.5/qwen3.6/deepseek-r1/gemma-4) добавляем
			// поле `thinking` (Ollama API) с накопленным reasoning_content.
			if fullReasoning != "" {
				resp["thinking"] = fullReasoning
			}
			ollamaBody, _ = json.Marshal(resp)
		} else {
			msgMap := map[string]interface{}{
				"role":    "assistant",
				"content": fullContent,
			}
			// 2026-07-01: для reasoning-моделей добавляем поле `reasoning` (Ollama API).
			if fullReasoning != "" {
				msgMap["reasoning"] = fullReasoning
			}
			// Добавляем накопленные tool_calls в финальный ответ
			if len(toolAccum) > 0 {
				toolCallsArr := make([]map[string]interface{}, 0, len(toolAccum))
				for i := 0; i < len(toolAccum); i++ {
					acc, ok := toolAccum[i]
					if !ok {
						continue
					}
					tcMap := map[string]interface{}{
						"type":     "function",
						"function": acc.function,
					}
					if acc.id != "" {
						tcMap["id"] = acc.id
					}
					toolCallsArr = append(toolCallsArr, tcMap)
				}
				msgMap["tool_calls"] = toolCallsArr
			}
			// Round 51.3 (2026-08-20): done:true для ЛЮБОГО непустого finish_reason.
			// См. комментарий выше для /api/generate — та же логика для /api/chat.
			resp := map[string]interface{}{
				"model":       modelName,
				"created_at":  time.Now().UTC().Format(time.RFC3339),
				"done":        finishReason != "",
				"done_reason": finishReason,
				"message":     msgMap,
			}
			ollamaBody, _ = json.Marshal(resp)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(ollamaBody)))
		w.WriteHeader(resp.StatusCode)
		w.Write(ollamaBody)
		return nil
	}

	translatedResp, err := translateOpenAIResponseToOllama(originalPath, respBody, modelFromCtx)
	if err != nil {
		translatedResp = respBody
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(translatedResp)))
	w.WriteHeader(resp.StatusCode)
	w.Write(translatedResp)

	// Round 31 #7 (2026-08-09): record token usage для мониторинга.
	// Парсим raw OpenAI body (respBody) — usage стандартный OpenAI формат.
	if resp.StatusCode < 400 {
		var openaiResp map[string]interface{}
		if json.Unmarshal(respBody, &openaiResp) == nil {
			if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
				var prompt, completion int64
				if v, ok := usage["prompt_tokens"].(float64); ok {
					prompt = int64(v)
				}
				if v, ok := usage["completion_tokens"].(float64); ok {
					completion = int64(v)
				}
				if prompt > 0 || completion > 0 {
					p.recordTokenUsage(modelFromCtx, prompt, completion)
				}
			}
		}
	}
	return nil
}

func minLenInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
