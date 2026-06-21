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

	// Если upstream вернул не-2xx — не начинаем streaming, а возвращаем
	// структурированную ошибку в формате, который клиент сможет прочитать.
	// Без этой проверки w.WriteHeader(200) пишется безусловно, SSE-сканнер
	// читает error body, не находит data:-префиксов, lastData остаётся пустым,
	// и клиент получает пустой SSE-поток → "Expecting value: line 2 column 1".
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			respBody = []byte(fmt.Sprintf(`{"error":"upstream read error: %v"}`, readErr))
		}
		logger.Get().Warnw("proxyRequestLlamaCpp: upstream returned non-2xx",
			"backend", backendID, "status", resp.StatusCode,
			"body", string(respBody)[:min(500, len(respBody))])

		// Для /v1/chat/completions возвращаем SSE с ошибкой
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

		// Для /api/chat и /api/generate возвращаем NDJSON с ошибкой
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(resp.StatusCode)
		errNDJSON := buildDoneResponse(originalPath, modelFromCtx, 2)
		fmt.Fprintf(w, "%s\n", string(errNDJSON))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}

	// Хеадер стриминга — первым делом проверяем, что у нас есть
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")

	// Устанавливаем Content-Type в зависимости от пути
	if originalPath == "/v1/chat/completions" {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	toolAccum := make(map[int]*accumulatedToolCall)

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
			errFwd := writeStreamingSSEDone(w, originalPath, modelFromCtx, toolAccum)
			if errFwd != nil {
				logger.Get().Warnw("proxyRequestLlamaCpp: writeStreamingSSEDone error",
					"backend", backendID, "error", errFwd)
			}
			break
		}

		lastData = data

		// Парсим SSE-чанк от llama.cpp
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			logger.Get().Debugw("proxyRequestLlamaCpp: skipping non-JSON SSE data",
				"data", data, "error", err)
			continue
		}

		// Извлекаем имя модели из первого чанка
		if modelFromCtx == "" {
			if m, ok := chunk["model"].(string); ok && m != "" {
				modelFromCtx = m
			}
		}

		if originalPath == "/v1/chat/completions" {
			// Прямой SSE → SSE: модифицируем только service_tokens для Cline
			// и пробрасываем остальные поля как есть.
			if filtered, shouldSkip := filterOpenAIStreamingLine([]byte(data)); shouldSkip {
				continue
			} else if filtered != nil {
				_, errFwd := fmt.Fprintf(w, "data: %s\n\n", string(filtered))
				if errFwd != nil {
					return fmt.Errorf("write filtered SSE: %v", errFwd)
				}
			} else {
				// Обрабатываем tool_calls в streaming-режиме
				if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]interface{}); ok {
						if delta, ok := choice["delta"].(map[string]interface{}); ok {
							accumulateToolCallsFromDelta(delta, toolAccum)
						}
					}
				}
				_, errFwd := fmt.Fprintf(w, "data: %s\n\n", data)
				if errFwd != nil {
					return fmt.Errorf("write SSE: %v", errFwd)
				}
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		} else {
			// SSE → NDJSON: для /api/generate и /api/chat
			// Аккумулируем tool_calls из SSE-чанков для /api/chat,
			// чтобы writeStreamingSSEDone мог включить их в финальный done-маркер.
			if originalPath == "/api/chat" {
				if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]interface{}); ok {
						if delta, ok := choice["delta"].(map[string]interface{}); ok {
							accumulateToolCallsFromDelta(delta, toolAccum)
						}
					}
				}
			}
			ollamaChunk := translateOpenAISSEDataToOllama(originalPath, []byte(data), modelFromCtx)
			if len(ollamaChunk) == 0 {
				continue
			}
			// translateOpenAISSEDataToOllama уже добавляет '\n' в конец chunk'а,
			// поэтому используем w.Write без дополнительного '\n'.
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

	// Сброс cycle counter после успешного завершения streaming-инференса.
	// Это критично для tool calling через OpenWebUI (которое использует streaming):
	// без этого вызова cycle counter никогда не сбрасывается в streaming-пути,
	// и после 3+ tool calling итераций с n_ctx overflow срабатывает cycle detection,
	// блокируя все последующие запросы к бэкенду.
	if p.nctxReload != nil {
		p.nctxReload.ResetCycleCounter(backendID)
	}

	// Если пришёл пустой SSE-поток (совсем ничего) без [DONE] и без данных,
	// пишем финальный NDJSON done-маркер, чтобы клиент не висел вечно.
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
