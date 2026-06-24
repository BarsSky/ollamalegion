package balancer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// proxyRequestLlamaCpp — основной прокси с поддержкой streaming.
// Корректно работает с SSE от llama.cpp и транслирует в NDJSON Ollama.
//
// Ключевая особенность: кросс-чанковая детекция JSON tool calls из content.
// Некоторые модели (Gemma-4) выводят JSON tool calls как обычный текст в content,
// разбитый на множество SSE чанков. Этот прокси аккумулирует content из смежных
// чанков, детектирует полный JSON, и эмитит чистый NDJSON чанк с tool_calls.
func (p *Proxy) proxyRequestLlamaCpp(w http.ResponseWriter, r *http.Request, backendID string, bodyBuf []byte) error {
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
	isStreaming := isStreamingFromBody(originalPath, bodyBuf)

	translatedBody, err := translateOllamaBodyToOpenAI(originalPath, bodyBuf)
	if err != nil {
		translatedBody = bodyBuf
	}

	// PREFLIGHT n_ctx auto-reload: если клиент (OpenWebUI) задал options.num_ctx,
	// а loaded n_ctx на бэкенде меньше — синхронно перезагружаем модель на нужный n_ctx
	// ДО проксирования. Это критично для OpenWebUI с tools: первый запрос
	// получает reload один раз (10-30 сек), последующие — instant response.
	preflightBody, preflightOK, preflightMsg, preflightStatus := p.preflightNCtxReloadIfNeeded(
		r.Context(), backendID, modelFromCtx, bodyBuf)
	if preflightOK {
		bodyBuf = preflightBody // preflight удалил num_ctx из body (cppworker возьмёт new n_ctx)
		logger.Get().Debugw("proxyRequestLlamaCpp: preflight n_ctx reload applied",
			"backend", backendID, "model", modelFromCtx, "new_body_len", len(bodyBuf))
	} else if preflightMsg != "" || preflightStatus != http.StatusOK {
		// preflight вернул needsProxy=false: асинхронный reload запущен в фоне.
		// Отдаём клиенту HTTP 503 Service Unavailable + Retry-After: 5.
		// Клиент повторит запрос через 5 секунд — к этому моменту reload обычно завершён.
		// Стрим НЕ обрывается посередине.
		logger.Get().Infow("proxyRequestLlamaCpp: preflight triggered async n_ctx reload, returning 503 to client",
			"backend", backendID, "model", modelFromCtx,
			"msg", preflightMsg, "status", preflightStatus)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "5")
		w.Header().Set("X-NCtx-Reload-Decision", "async-reload")
		body := []byte(fmt.Sprintf(`{"error":%q,"decision":"async_reload","retry_after_seconds":5}`,
			preflightMsg))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(body)
		return nil
	}

	fullURL := targetURL + llamacppPath
	logger.Get().Infow("proxyRequestLlamaCpp: sending request",
		"backend", backendID, "url", fullURL, "stream", isStreaming)

	client := p.client
	if isStreaming {
		client = p.streamingClient
	}

	reqCtx := r.Context()
	if isStreaming {
		// Per-model адаптивный таймаут для streaming inference.
		streamTimeout := p.getModelStreamTimeout(modelFromCtx)
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(r.Context(), streamTimeout)
		defer cancel()
		logger.Get().Debugw("proxyRequestLlamaCpp: using per-model stream timeout",
			"backend", backendID, "model", modelFromCtx,
			"stream_timeout_sec", streamTimeout.Seconds())
	}

	req, err := http.NewRequestWithContext(reqCtx, r.Method, fullURL, bytes.NewReader(translatedBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for key, values := range r.Header {
		lowerKey := strings.ToLower(key)
		if lowerKey == "content-type" || lowerKey == "accept" || lowerKey == "content-length" || lowerKey == "host" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	clientRealIP := p.getClientRealIP(r)
	if existingXFF := r.Header.Get("X-Forwarded-For"); existingXFF != "" {
		req.Header.Set("X-Forwarded-For", existingXFF)
	} else {
		req.Header.Set("X-Forwarded-For", clientRealIP)
	}
	req.Header.Set("X-Real-IP", clientRealIP)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("llama.cpp request failed: %v", err)
	}
	defer resp.Body.Close()

	// Если upstream вернул не-2xx — не начинаем streaming, а возвращаем
	// структурированную ошибку в формате, который клиент сможет прочитать.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			respBody = []byte(fmt.Sprintf(`{"error":"upstream read error: %v"}`, readErr))
		}
		logger.Get().Warnw("proxyRequestLlamaCpp: upstream returned non-2xx",
			"backend", backendID, "status", resp.StatusCode,
			"body", string(respBody)[:min(500, len(respBody))])

		if originalPath == "/v1/chat/completions" {
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

		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(resp.StatusCode)
		errNDJSON := buildDoneResponse(originalPath, modelFromCtx, 0)
		fmt.Fprintf(w, "%s\n", string(errNDJSON))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}

	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	if originalPath == "/v1/chat/completions" {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	toolAccum := make(map[int]*accumulatedToolCall)

	// ==== Кросс-чанковое накопление content для JSON tool call детекции =====
	// Некоторые модели (Gemma-4) выводят JSON tool calls как обычный текст в
	// content, разбитый на множество SSE чанков. Мы аккумулируем content из
	// смежных чанков, детектируем полный JSON, и эмитим чистый NDJSON чанк.
	var contentBuffer string
	var contentToolCallsProcessed bool

	// ==== Накопление plain content для финального done-чанка ====
	// cppworker отдаёт полный ответ в финальном NDJSON-чанке (`done:true`), но
	// balancer его подавляет (см. ниже hasFinishReason) и формирует свой done-чанк.
	// Чтобы клиент (Cline) не получил пустой `content`, аккумулируем content
	// из streaming чанков и используем его в финальном done-чанке.
	var accumulatedPlainContent string
	// ==== Content, извлечённый из done-чанка upstream (если cppworker
	// уже сформировал content в финальном NDJSON; используем его приоритетно). ====
	var upstreamDoneContent string

	// ==== Основной цикл SSE → NDJSON / SSE → SSE ==========
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0), 1024*1024)

	lastData := ""

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			errFwd := writeStreamingSSEDone(w, originalPath, modelFromCtx, toolAccum,
				accumulatedPlainContent, upstreamDoneContent)
			if errFwd != nil {
				logger.Get().Warnw("proxyRequestLlamaCpp: writeStreamingSSEDone error",
					"backend", backendID, "error", errFwd)
			}
			break
		}

		lastData = data

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			logger.Get().Debugw("proxyRequestLlamaCpp: skipping non-JSON SSE data",
				"data", data, "error", err)
			continue
		}

		if modelFromCtx == "" {
			if m, ok := chunk["model"].(string); ok && m != "" {
				modelFromCtx = m
			}
		}

		// Для SSE→SSE passthrough (/v1/chat/completions) — проксируем как есть,
		// дополнительно накапливая delta.content на случай если клиент захочет
		// полный контент после [DONE].
		if originalPath == "/v1/chat/completions" {
			// Накапливаем plain content для /v1/chat/completions (на случай будущего использования).
			if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if delta, ok := choice["delta"].(map[string]interface{}); ok {
						if c, ok := delta["content"].(string); ok && c != "" {
							accumulatedPlainContent += c
						}
					}
				}
			}
			// SSE → SSE passthrough
			if filtered, shouldSkip := filterOpenAIStreamingLine([]byte(data)); shouldSkip {
				continue
			} else if filtered != nil {
				_, errFwd := fmt.Fprintf(w, "data: %s\n\n", string(filtered))
				if errFwd != nil {
					return fmt.Errorf("write filtered SSE: %v", errFwd)
				}
			} else {
				if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]interface{}); ok {
						if delta, ok := choice["delta"].(map[string]interface{}); ok {
							accumulateToolCallsFromDelta(delta, toolAccum)
						}
					}
				}
				if modified := extractToolCallsFromSSEContent([]byte(data)); modified != nil {
					_, errFwd := fmt.Fprintf(w, "data: %s\n\n", string(modified))
					if errFwd != nil {
						return fmt.Errorf("write modified SSE (tool calls): %v", errFwd)
					}
				} else {
					_, errFwd := fmt.Fprintf(w, "data: %s\n\n", data)
					if errFwd != nil {
						return fmt.Errorf("write SSE: %v", errFwd)
					}
				}
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		} else {
			// ==== SSE → NDJSON: для /api/generate и /api/chat =====
			var deltaContent string
			if originalPath == "/api/chat" {
				if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]interface{}); ok {
						if delta, ok := choice["delta"].(map[string]interface{}); ok {
							accumulateToolCallsFromDelta(delta, toolAccum)
							if c, ok := delta["content"].(string); ok {
								deltaContent = c
							}
						}
					}
				}

				// Если чанк содержит finish_reason (done:true) — извлекаем content и
				// done_reason из message, чтобы сохранить полный ответ cppworker'а.
				if upstreamHasDone, msgMap, _ := extractUpstreamDoneChunk(data); upstreamHasDone {
					if c, ok := msgMap["content"].(string); ok {
						upstreamDoneContent = c
					}
				}

				// Кросс-чанковое накопление content для детекции JSON tool calls.
				// Поддерживаемые триггеры накопления:
				//   - "[" — стандартный JSON-массив tool_calls
				//   - "<tool_call>" / "<|python_tag|>" — Hermes/Qwen/Llama-3 форматы
				//   - "{name" — начало одиночного JSON-объекта tool_call
				//   - contentBuffer не пуст — продолжаем накапливать уже начатый tool call
				if deltaContent != "" && !contentToolCallsProcessed {
					trimmed := strings.TrimSpace(deltaContent)
					startToolCallAccum := strings.HasPrefix(trimmed, "[") ||
						strings.HasPrefix(trimmed, "<tool_call>") ||
						strings.HasPrefix(trimmed, "<|python_tag|>") ||
						strings.HasPrefix(trimmed, "[TOOL_CALLS]") ||
						strings.HasPrefix(trimmed, "{name")
					if startToolCallAccum || contentBuffer != "" {
						contentBuffer += deltaContent

						// Кросс-чанковая склейка может оставить "лишний" символ '}' перед
						// закрывающим </tool_call> (когда JSON закрывается в одном чанке, а
						// '}'</tool_call>' приходит в следующем). Схлопываем дубликат.
						contentBuffer = collapseDuplicateClosingBrace(contentBuffer)

						// Проверяем, не накопился ли полный JSON tool call
						if detectedTC, remainingTC, found := detectAndExtractToolCallsFromContent(contentBuffer); found {
							contentToolCallsProcessed = true
							contentBuffer = ""
							logger.Get().Infow("proxyRequestLlamaCpp: cross-chunk tool_calls detected in content",
								"backend", backendID, "model", modelFromCtx,
								"tool_calls_count", len(detectedTC))

							cleanRemaining := stripServiceTokens(remainingTC)
							cleanRemaining = cleanContentAfterToolCallExtraction(cleanRemaining)

							msgMap := map[string]interface{}{
								"role":       "assistant",
								"content":    cleanRemaining,
								"tool_calls": detectedTC,
							}
							ollamaChunk := map[string]interface{}{
								"model":      modelFromCtx,
								"created_at": time.Now().UTC().Format(time.RFC3339),
								"done":       false,
								"message":    msgMap,
							}
							out, _ := json.Marshal(ollamaChunk)
							_, errFwd := fmt.Fprintf(w, "%s\n", string(out))
							if errFwd != nil {
								return fmt.Errorf("write NDJSON (cross-chunk tool calls): %v", errFwd)
							}
							if flusher, ok := w.(http.Flusher); ok {
								flusher.Flush()
							}
							continue
						}

						// Если JSON массив закрыт (найден ']' на правильной глубине),
						// но tool call не обнаружен — это не tool call JSON.
						// Сбрасываем буфер и даём чанку пройти через нормальный translate.
						if endPos := findMatchingClosingBracket(contentBuffer, 0); endPos >= 0 {
							contentBuffer = ""
						} else if len(contentBuffer) > 4096 {
							// Слишком большой буфер без закрывающей ']' — не tool call.
							contentBuffer = ""
						} else {
							// Всё ещё накапливаем — пропускаем этот чанк
							continue
						}
					}
				}

				// Накапливаем plain content (вне tool-call accumulation) для финального
				// done-чанка — это спасает от пустого content если upstream прислал
				// полный ответ только в done-чанке.
				if deltaContent != "" && !contentToolCallsProcessed {
					accumulatedPlainContent += deltaContent
				}
			} else if originalPath == "/api/generate" {
				// Для /api/generate content лежит в chunk["choices"][0].text или в done_chunk.response
				if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]interface{}); ok {
						if t, ok := choice["text"].(string); ok && t != "" {
							deltaContent = t
							accumulatedPlainContent += t
						}
					}
				}
				// done-чанк /api/generate от OpenAI: {"choices":[{"finish_reason":"stop","text":""}],...}
				if upstreamHasDone := extractUpstreamGenerateDoneChunk(data, &accumulatedPlainContent); upstreamHasDone {
					upstreamDoneContent = accumulatedPlainContent
				}
			}

			// Подавляем финальный done-чанк от translate, если:
			//   - tool_calls были уже обработаны (cross-chunk или delta),
			//   - или последний чанк от cppworker содержит finish_reason (done:true).
			// В обоих случаях финальный NDJSON формирует writeStreamingSSEDone.
			if contentToolCallsProcessed || len(toolAccum) > 0 || hasFinishReason(data) {
				continue
			}
			ollamaChunk := translateOpenAISSEDataToOllama(originalPath, []byte(data), modelFromCtx)
			if len(ollamaChunk) == 0 {
				continue
			}
			_, errFwd := w.Write(ollamaChunk)
			if errFwd != nil {
				return fmt.Errorf("write NDJSON: %v", errFwd)
			}

			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Get().Errorw("proxyRequestLlamaCpp: scanner error", "error", err, "backend", backendID)
	}

	if p.nctxReload != nil {
		p.nctxReload.ResetCycleCounter(backendID)
	}

	if lastData == "" && originalPath != "/v1/chat/completions" {
		done := buildDoneResponse(originalPath, modelFromCtx, 0)
		_, errFwd := fmt.Fprintf(w, "%s\n", string(done))
		if errFwd != nil {
			return fmt.Errorf("write empty-done NDJSON: %v", errFwd)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	return nil
}

// hasFinishReason — проверяет, содержит ли SSE data-чанк finish_reason (done:true).
// Используется для подавления дублирующего done-чанка от translate.
func hasFinishReason(data string) bool {
	if len(data) == 0 {
		return false
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false
	}
	fr, ok := choice["finish_reason"].(string)
	if !ok {
		return false
	}
	return fr != "" && fr != "null"
}

// extractUpstreamDoneChunk — извлекает content и done_reason из OpenAI SSE-чанка
// с finish_reason (например, финального чанка cppworker'а для /api/chat).
// Возвращает (true, msgMap, doneReason), если чанк содержит finish_reason.
func extractUpstreamDoneChunk(data string) (bool, map[string]interface{}, string) {
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false, nil, ""
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false, nil, ""
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false, nil, ""
	}
	fr, ok := choice["finish_reason"].(string)
	if !ok || fr == "" || fr == "null" {
		return false, nil, ""
	}
	msg, _ := choice["message"].(map[string]interface{})
	if msg == nil {
		// Иногда content лежит в delta
		if delta, ok := choice["delta"].(map[string]interface{}); ok {
			msg = map[string]interface{}{}
			if c, ok := delta["content"].(string); ok {
				msg["content"] = c
			}
		}
	}
	return true, msg, fr
}

// extractUpstreamGenerateDoneChunk — для /api/generate cppworker возвращает
// финальный чанк с finish_reason. Накапливает text (если есть) в plainContent.
// Возвращает true, если чанк содержит finish_reason.
func extractUpstreamGenerateDoneChunk(data string, plainContent *string) bool {
	if plainContent == nil {
		return false
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false
	}
	fr, ok := choice["finish_reason"].(string)
	if !ok || fr == "" || fr == "null" {
		return false
	}
	if t, ok := choice["text"].(string); ok && t != "" {
		*plainContent += t
	}
	return true
}