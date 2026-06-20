package balancer

import (
	"bufio"
	"bytes"
	"context"
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

// proxyRequestLlamaCpp — основной прокси с поддержкой streaming.
// Корректно работает с SSE от llama.cpp и транслирует в NDJSON Ollama.
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
		// Использует 3-tier resolver:
		//   1. Per-model profile (config.LlamaCppModelProfiles[modelName].StreamingTimeoutSec)
		//   2. ModelLatencyTracker (автоматический расчёт на основе истории генерации)
		//   3. Глобальный конфиг StreamTimeout (дефолт 600s)
		// Для CPU-моделей с partial offload автоматически вычисляет 1800+ секунд.
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

	logger.Get().Debugw("proxyRequestLlamaCpp: got response",
		"backend", backendID, "status", resp.StatusCode,
		"content_type", resp.Header.Get("Content-Type"))

	// ==== n_ctx auto-reload: перехват структурированной ошибки от cppworker ====
	// cppworker при n_ctx overflow отвечает HTTP 400 (Bad Request) с JSON
	// {error, code: 2 (N_CTX_NEEDS_RELOAD), bridge_info: {...}}. Если
	// auto-reload включён в конфиге — перезагружаем модель с большим n_ctx
	// и повторяем запрос. Если выключен — пробрасываем оригинальную ошибку
	// клиенту (или возвращаем 413/503, см. handleNCtxReload).
	//
	// ВАЖНО: мы ДОЛЖНЫ проверить nctx-ошибку ДО fast-path 503 loading,
	// потому что reload и loading — это разные вещи (reload = модель нужно
	// перезагрузить с другим n_ctx, loading = модель в процессе загрузки в VRAM).
	if resp.StatusCode >= 400 && !isStreaming {
		// Для streaming (SSE) reload-логика работает иначе — см. handleNCtxReload
		// и отдельный fast-path в streaming-блоке ниже.
		if peekBody, peekErr := io.ReadAll(resp.Body); peekErr == nil {
			if nctxErr := ParseCppWorkerError(peekBody, resp.StatusCode, backendID); nctxErr != nil {
				if errors.Is(nctxErr, bridge.ErrNCtxNeedsReload) || errors.Is(nctxErr, bridge.ErrPromptTooLong) {
					logger.Get().Infow("proxyRequestLlamaCpp: detected n_ctx error from cppworker, invoking auto-reload",
						"backend", backendID, "model", modelFromCtx,
						"status", resp.StatusCode, "decision_errors_is",
						"is_n_ctx", errors.Is(nctxErr, bridge.ErrNCtxNeedsReload),
						"is_prompt_too_long", errors.Is(nctxErr, bridge.ErrPromptTooLong))
					p.handleNCtxReload(r.Context(), w, r, backendID, modelFromCtx, nctxErr, bodyBuf)
					return nil
				}
			}
			// Не nctx-ошибка — восстанавливаем body для дальнейшей обработки.
			resp.Body = io.NopCloser(bytes.NewReader(peekBody))
		}
	}

	// ==== Fast-path: upstream вернул 503 "model is loading" ====
	// cppworker при CGo-блокировке LoadModel() отвечает 503 + JSON
	// {error, loading:true, model, elapsedMs, retryAfterMs: 3000} + Retry-After: 3.
	// Прокси должен передать этот ответ ВЕРБАТИМ клиенту как JSON (а не как NDJSON
	// с пустым телом) — иначе OpenWebUI получает пустой ответ и «Ollama: Server disconnected»,
	// а Ollama-совместимые клиенты не понимают, что делать retry.
	//
	// Детектим по комбинации:
	//   - статус 503
	//   - Content-Type = application/json (т.е. не streaming SSE)
	//   - в первых ~512 байт тела есть ключ "loading":true
	if resp.StatusCode == http.StatusServiceUnavailable &&
		strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		previewBuf := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, previewBuf)
		preview := previewBuf[:n]
		// Восстановим body, объединив preview + остаток
		rest, _ := io.ReadAll(resp.Body)
		fullBody := append(preview, rest...)
		if bytes.Contains(fullBody, []byte(`"loading":true`)) {
			// Передаём JSON-ответ клиенту как есть. Сохраняем заголовки Retry-After
			// (cppworker их выставляет) и Content-Type.
			for key, values := range resp.Header {
				if key == "Content-Length" {
					continue
				}
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(fullBody)))
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write(fullBody)
			logger.Get().Infow("proxyRequestLlamaCpp: passed through 503 loading fast-path",
				"backend", backendID, "model", modelFromCtx,
				"body_len", len(fullBody))
			return nil
		}
		// Не нашли "loading":true — это обычный 503 (не loading), продолжаем обычный путь.
		// Восстановим resp.Body из прочитанных данных.
		resp.Body = io.NopCloser(bytes.NewReader(fullBody))
	}

	if !isStreaming {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response: %v", err)
		}
		// ДИАГНОСТИКА: логируем первые 500 байт тела ответа от cppworker.
		// Помогает при диагностике проблем вроде «Cline получает мусор» или
		// «некорректный created_at» — позволяет увидеть, ЧТО ИМЕННО отдал
		// upstream до того, как мы транслируем это в Ollama-формат.
		// Содержимое чувствительно (промпт пользователя), но мы логируем
		// только ассистентский ответ, который обычно не PII.
		bodyPreviewLen := len(respBody)
		if bodyPreviewLen > 500 {
			bodyPreviewLen = 500
		}
		logger.Get().Debugw("proxyRequestLlamaCpp: non-stream body preview",
			"backend", backendID,
			"model", modelFromCtx,
			"body_len", len(respBody),
			"body_preview", string(respBody)[:bodyPreviewLen])
		translatedResp, err := translateOpenAIResponseToOllama(originalPath, respBody, modelFromCtx)
		if err != nil {
			translatedResp = respBody
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(translatedResp)))
		w.WriteHeader(resp.StatusCode)
		w.Write(translatedResp)
		return nil
	}

	// Streaming: транслируем SSE → Ollama NDJSON (или проксируем NDJSON как есть)
	// Проверяем Content-Type upstream: если это не SSE и не NDJSON (например,
	// JSON-ошибка от cppworker), не входим в streaming-цикл, а проксируем как
	// обычный JSON-ответ с Content-Length.
	// Это предотвращает TransferEncodingError, когда Go пытается chunk-кодировать
	// не-SSE/не-NDJSON ответ (ошибку), а клиент ждёт NDJSON.
	upstreamContentType := resp.Header.Get("Content-Type")
	if !strings.Contains(upstreamContentType, "text/event-stream") &&
		!strings.Contains(upstreamContentType, "application/x-ndjson") {
		// Upstream вернул не-SSE и не-NDJSON ответ (обычно JSON-ошибка: модель не загружена,
		// контекст превышен, bridge code 2 и т.п.) — отдаём как есть с Content-Length.
		// Читаем тело ДО установки заголовков, чтобы вычислить Content-Length.
		// Без этого aiohttp/OpenWebUI может получить TransferEncodingError,
		// если тело меньше ожидаемого chunked-encoding размера.
		errBody, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(errBody)))
		w.WriteHeader(resp.StatusCode)
		w.Write(errBody)
		return nil
	}


	for key, values := range resp.Header {
		if key == "Content-Length" || key == "Content-Type" {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)

	// Отслеживаем, был ли в стриме финальный чанк с done:true.
	// Это нужно, чтобы НЕ дублировать терминатор после стрима.
	// Ollama-клиенты (например, OpenWebUI) корректно понимают done-чанк,
	// который приходит в финальном SSE-сообщении (после data: [DONE] балансер
	// уже отдаёт `{"done":true}` — см. translateOpenAISSEDataToOllama).
	doneSent := false
	// Считаем количество content/done-чанков, которые реально были отправлены клиенту.
	// Если их ноль — значит, cppworker не отдал ни одного полезного чанка (например,
	// модель не загружена, или контекст превышен, или бэкенд прислал сразу [DONE]).
	// В этом случае НЕ отправляем синтетический «пустой успешный» done — иначе
	// OpenWebUI рендерит пустое сообщение ассистента, и пользователь видит
	// «ответ пустой строкой». Вместо этого шлём явный NDJSON-чанк с error,
	// чтобы клиент корректно завершил поток с диагностикой.
	contentChunksSent := 0

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			// КРИТИЧЕСКИ ВАЖНО: перед возвратом ошибки отправляем done-маркер,
			// иначе клиент (aiohttp/OpenWebUI) получает оборванный chunked response
			// и TransferEncodingError: "Not enough data to satisfy transfer length header".
			// Заголовки уже отправлены (w.WriteHeader вызван выше), поэтому мы не можем
			// изменить статус — только завершить стрим корректным NDJSON-чанком.
			errPayload, _ := json.Marshal(map[string]interface{}{
				"model":       modelFromCtx,
				"created_at":  time.Now().UTC().Format(time.RFC3339),
				"done":        true,
				"done_reason": "error",
				"error":       "upstream stream read error: " + err.Error(),
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "",
				},
			})
			w.Write(append(errPayload, '\n'))
			if canFlush {
				flusher.Flush()
			}
			logger.Get().Errorw("streaming read error after done marker", "error", err)
			// КРИТИЧЕСКИ ВАЖНО: возвращаем nil, а НЕ error!
			//
			// Заголовки уже отправлены (w.WriteHeader вызван выше), и мы уже отправили
			// done:true маркер с описанием ошибки. Если вернуть error, он всплывёт
			// в ServeHTTP (proxy.go:934-967), который вызовет http.Error(w, ...) —
			// это запишет ДОПОЛНИТЕЛЬНЫЕ данные ПОСЛЕ завершённого NDJSON-стрима.
			// Go chunked encoding обернёт этот мусор как дополнительный chunk,
			// и aiohttp (OpenWebUI) получит данные после done:true →
			// TransferEncodingError "Not enough data to satisfy transfer length header".
			//
			// Правильное поведение: стрим уже завершён (с ошибкой в контенте),
			// клиент получил диагностику — не пишем больше ничего в ResponseWriter.
			return nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var translated []byte
		if bytes.HasPrefix(line, []byte("data: ")) {
			data := bytes.TrimPrefix(line, []byte("data: "))
			translated = translateOpenAISSEDataToOllama(originalPath, data, modelFromCtx)
		} else if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimPrefix(line, []byte("data:"))
			data = bytes.TrimSpace(data)
			translated = translateOpenAISSEDataToOllama(originalPath, data, modelFromCtx)
		} else {
			// Non-SSE данные в streaming-режиме: upstream cppworker мог
			// прислать не-SSE контент (например, ошибку без префикса "data: ").
			// Вместо сырой записи, которая сломает NDJSON-формат, оборачиваем
			// как NDJSON error chunk. Это гарантирует, что клиент (OpenWebUI
			// reasoning, Cline) не получит ломаный NDJSON и json.JSONDecodeError.
			errJSON, _ := json.Marshal(map[string]interface{}{
				"error":    "upstream non-sse data: " + string(line),
				"done":     true,
				"response": "",
			})
			w.Write(errJSON)
			w.Write([]byte("\n"))
			if canFlush {
				flusher.Flush()
			}
			continue
		}

		if len(translated) == 0 {
			continue
		}
		// Помечаем, что финальный done-чанк уже отправлен (если он действительно done)
		if bytes.Contains(translated, []byte(`"done":true`)) {
			doneSent = true
		}
		// Считаем только content/done чанки, у которых есть message или response.
		// Role-only чанки ({"message":{"role":"assistant","content":""}}) — не считаем,
		// иначе heartbeat от streaming.go (который тоже шлёт role-only) мог бы
		// исказить этот счётчик, но streaming.go не используется здесь, поэтому
		// считаем все отправленные message/done чанки.
		if bytes.Contains(translated, []byte(`"message":`)) || bytes.Contains(translated, []byte(`"response":`)) {
			contentChunksSent++
		}
		w.Write(translated)
		if canFlush {
			flusher.Flush()
		}
	}

	// Страховка: если cppworker оборвал стрим ДО финального чанка с done,
	// отправляем done-маркер сами, чтобы клиент (OpenWebUI) корректно закрыл стрим.
	// ВАЖНО: шлём ПОЛНЫЙ Ollama-чанк с model и message (а не только {"done":true}),
	// иначе OpenWebUI на втором запросе в одной сессии рендерит пустое сообщение:
	// модель пустая, message нет, и UI «сбрасывает» накопленные токены.
	if !doneSent {
		if contentChunksSent == 0 {
			// Бэкенд не отдал ни одного content/done-чанка — не отправляем фейковый
			// «успешный пустой» ответ, иначе OpenWebUI рендерит пустое сообщение
			// ассистента (симптом: «следующий вопрос завершается пустой строкой»).
			// Отправляем явный NDJSON с done:true и error, чтобы клиент корректно
			// завершил поток и пользователь увидел осмысленную диагностику.
			logger.Get().Warnw("proxyRequestLlamaCpp: backend returned no content chunks, sending empty-response error",
				"backend", backendID, "model", modelFromCtx)
			emptyPayload, _ := json.Marshal(map[string]interface{}{
				"model":       modelFromCtx,
				"created_at":  time.Now().UTC().Format(time.RFC3339),
				"done":        true,
				"done_reason": "empty_response",
				"error":       "backend returned no content",
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "",
				},
			})
			w.Write(append(emptyPayload, '\n'))
			if canFlush {
				flusher.Flush()
			}
		} else {
			donePayload, _ := json.Marshal(map[string]interface{}{
				"model":       modelFromCtx,
				"created_at":  time.Now().UTC().Format(time.RFC3339),
				"done":        true,
				"done_reason": "stop",
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "",
				},
			})
			w.Write(append(donePayload, '\n'))
			if canFlush {
				flusher.Flush()
			}
		}
	}
	return nil
}

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

	client := p.client
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("llama.cpp request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %v", err)
	}

	logger.Get().Infow("proxyRequestLlamaCppNonStream: response",
		"backend", backendID, "status", resp.StatusCode,
		"body_len", len(respBody),
		"body", string(respBody)[:min(500, len(respBody))])

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

	// Если пришёл SSE (вдруг), собираем
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") || bytes.HasPrefix(respBody, []byte("data:")) {
		var fullContent string
		var modelName = modelFromCtx
		var finishReason = "stop"
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
						if c, ok := delta["content"].(string); ok {
							fullContent += c
						}
					} else if msg, ok := choice["message"].(map[string]interface{}); ok {
						if c, ok := msg["content"].(string); ok {
							fullContent += c
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
			resp := map[string]interface{}{
				"model":       modelName,
				"created_at":  time.Now().UTC().Format(time.RFC3339),
				"response":    fullContent,
				"done":        finishReason == "stop",
				"done_reason": finishReason,
			}
			ollamaBody, _ = json.Marshal(resp)
		} else {
			resp := map[string]interface{}{
				"model":       modelName,
				"created_at":  time.Now().UTC().Format(time.RFC3339),
				"done":        finishReason == "stop",
				"done_reason": finishReason,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": fullContent,
				},
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
	return nil
}

func minLenInt(a, b int) int {

	if a < b {
		return a
	}
	return b
}
