// proxy_request_openai_auto_stream.go — Round 31 #1 (2026-08-09):
// workaround для 120-300s timeout на non-streaming /v1/chat/completions.
//
// Проблема: cppworker для non-stream запросов БУФЕРИЗИРУЕТ всю генерацию
// и НЕ отдаёт HTTP headers пока генерация не завершена. Cline/OpenWebUI
// default timeout 120-300s → balancer (и клиент) убивают запрос ДО того как
// cppworker отдал первый байт. Reasoning модели (gemma-4, qwen3.6) +
// длинные prompts = 60-90s генерация, что ломает UX.
//
// Решение (auto-stream workaround): balancer сам конвертирует non-stream в
// upstream-streaming. cppworker СРАЗУ отдаёт 200 + headers + начинает стримить.
// Balancer читает SSE чанки, накапливает content / reasoning / tool_calls.
// На финальном chunk — формирует OpenAI non-stream JSON и отправляет.
//
// Клиент видит 200 ОК мгновенно (через chunked transfer encoding) —
// больше нет 120-300s блокировки на headers.
//
// Активация: env var LB_OPENAI_AUTO_STREAM=true (default false, backward compat).
package balancer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// IsOpenAIAutoStreamEnabled — Round 31 #1 feature flag.
//
// Проверяет env var LB_OPENAI_AUTO_STREAM=true.
// Default false — backward compat (старое blocking поведение).
func IsOpenAIAutoStreamEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_OPENAI_AUTO_STREAM")))
	return v == "true" || v == "1" || v == "yes"
}

// proxyRequestOpenAIStreamAsNonStream — Round 31 #1 workaround.
//
// clientReqBody: исходный body клиента (с stream=false).
// upstreamURL: target URL cppworker.
// Возвращает ошибку только в случае системных сбоев; HTTP-ошибки проксируются.
//
// Важно: НЕ retry'им при ошибках — пусть клиент повторит сам (Cline умеет).
// retry может привести к дублированию генерации в cppworker.
func (p *Proxy) proxyRequestOpenAIStreamAsNonStream(
	w http.ResponseWriter,
	r *http.Request,
	clientReqBody []byte,
	upstreamURL string,
	backendID string,
) error {
	// Парсим body клиента
	var clientReq map[string]interface{}
	if err := json.Unmarshal(clientReqBody, &clientReq); err != nil {
		return fmt.Errorf("failed to parse client body: %w", err)
	}
	modelName, _ := clientReq["model"].(string)

	// Модифицируем body для upstream: stream=true
	upstreamBodyMap := make(map[string]interface{}, len(clientReq))
	for k, v := range clientReq {
		upstreamBodyMap[k] = v
	}
	upstreamBodyMap["stream"] = true
	upstreamBody, err := json.Marshal(upstreamBodyMap)
	if err != nil {
		return fmt.Errorf("failed to marshal modified body: %w", err)
	}

	// Создаём upstream request
	upstreamReq, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodPost,
		upstreamURL,
		bytes.NewReader(upstreamBody),
	)
	if err != nil {
		return fmt.Errorf("failed to create upstream request: %w", err)
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "text/event-stream, application/json")
	upstreamReq.ContentLength = int64(len(upstreamBody))
	// Копируем XFF
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		upstreamReq.Header.Set("X-Forwarded-For", xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		upstreamReq.Header.Set("X-Real-IP", xri)
	}

	// Streaming client — НЕ ждёт body, только headers
	clientForReq := p.streamingClient
	if firstByte := p.getModelFirstByteTimeout(modelName); firstByte > 0 {
		clientForReq = p.newStreamingClientWithResponseHeaderTimeout(firstByte)
	}

	logger.Get().Infow("Round 31 #1: auto-stream workaround active",
		"backend", backendID, "upstream_url", upstreamURL, "model", modelName)

	upstreamResp, err := clientForReq.Do(upstreamReq)
	if err != nil {
		return fmt.Errorf("upstream request failed: %w", err)
	}
	defer upstreamResp.Body.Close()

	// Если upstream сразу вернул non-200 — проксируем как есть.
	if upstreamResp.StatusCode < 200 || upstreamResp.StatusCode >= 300 {
		body, _ := io.ReadAll(upstreamResp.Body)
		for k, v := range upstreamResp.Header {
			kLower := strings.ToLower(k)
			if kLower == "transfer-encoding" || kLower == "content-length" || kLower == "connection" {
				continue
			}
			for _, vv := range v {
				w.Header().Add(k, vv)
			}
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(upstreamResp.StatusCode)
		_, _ = w.Write(body)
		return nil
	}

	// === Headers получены → сразу отдаём 200 клиенту (chunked transfer) ===
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Round-31-1-Workaround", "non-stream-to-upstream-stream")
	p.addModelCapabilitiesHeaders(w, modelName)
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	// === Аккумулируем SSE чанки ===
	accum := newOpenAIStreamAccumulator(modelName)
	scanner := bufio.NewScanner(upstreamResp.Body)
	// SSE строки могут быть длинными (reasoning chunks)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // до 4MB на строку

	for scanner.Scan() {
		// Context cancellation check
		select {
		case <-r.Context().Done():
			logger.Get().Warnw("Round 31 #1: client cancelled during accumulation",
				"backend", backendID,
				"elapsed_sec", time.Since(accum.startedAt).Seconds(),
				"chunks_processed", accum.chunksProcessed)
			return nil
		default:
		}

		line := scanner.Text()
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		accum.addChunk(payload)
	}

	if err := scanner.Err(); err != nil {
		logger.Get().Errorw("Round 31 #1: scanner error",
			"backend", backendID, "error", err,
			"chunks_processed", accum.chunksProcessed)
		// Best effort: отправляем что накопили
		body := accum.toNonStreamResponse(false)
		_, _ = w.Write(body)
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	// === Финальный чанк: формируем non-stream JSON ===
	finalResp := accum.toNonStreamResponse(false)
	if _, err := w.Write(finalResp); err != nil {
		logger.Get().Errorw("Round 31 #1: failed to write final response",
			"backend", backendID, "error", err)
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	logger.Get().Infow("Round 31 #1: workaround complete",
		"backend", backendID, "model", accum.model,
		"content_len", len(accum.content.String()),
		"reasoning_len", len(accum.reasoning.String()),
		"tool_calls_count", len(accum.toolCalls),
		"usage_present", accum.usage != nil,
		"elapsed_sec", time.Since(accum.startedAt).Seconds())
	return nil
}

// openAIStreamAccumulator — собирает SSE чанки от cppworker для финального
// non-stream OpenAI JSON.
type openAIStreamAccumulator struct {
	model       string
	created     int64
	role        string
	content     strings.Builder
	reasoning   strings.Builder
	toolCalls   []map[string]interface{}
	finishReason string
	usage       map[string]interface{}
	id          string
	object      string
	startedAt   time.Time
	chunksProcessed int
}

func newOpenAIStreamAccumulator(model string) *openAIStreamAccumulator {
	return &openAIStreamAccumulator{
		model:     model,
		created:   time.Now().Unix(),
		object:    "chat.completion",
		startedAt: time.Now(),
	}
}

func (a *openAIStreamAccumulator) addChunk(payload string) {
	a.chunksProcessed++
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	if v, ok := chunk["id"].(string); ok && a.id == "" {
		a.id = v
	}
	if v, ok := chunk["model"].(string); ok && a.model == "" {
		a.model = v
	}
	if v, ok := chunk["created"].(float64); ok && a.created == 0 {
		a.created = int64(v)
	}
	if v, ok := chunk["object"].(string); ok {
		a.object = v
	}
	if u, ok := chunk["usage"].(map[string]interface{}); ok {
		a.usage = u
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return
	}
	if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
		a.finishReason = fr
	}
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return
	}
	if r, ok := delta["role"].(string); ok && r != "" {
		a.role = r
	}
	if c, ok := delta["content"].(string); ok && c != "" {
		a.content.WriteString(c)
	}
	// Round 31 #1: reasoning content для reasoning моделей.
	// Пробрасываем в message.reasoning (Ollama-стиль) для Cline/OpenWebUI.
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		a.reasoning.WriteString(rc)
	}
	if tc, ok := delta["tool_calls"].([]interface{}); ok && len(tc) > 0 {
		for _, rawTC := range tc {
			if tcMap, ok := rawTC.(map[string]interface{}); ok {
				a.toolCalls = append(a.toolCalls, tcMap)
			}
		}
	}
}

// toNonStreamResponse — финальный OpenAI non-stream JSON.
func (a *openAIStreamAccumulator) toNonStreamResponse(fallback bool) []byte {
	role := a.role
	if role == "" {
		role = "assistant"
	}
	message := map[string]interface{}{
		"role":    role,
		"content": a.content.String(),
	}
	if a.reasoning.Len() > 0 {
		message["reasoning"] = a.reasoning.String()
	}
	if len(a.toolCalls) > 0 {
		message["tool_calls"] = a.toolCalls
	}
	resp := map[string]interface{}{
		"id":      a.id,
		"object":  a.object,
		"created": a.created,
		"model":   a.model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       message,
				"finish_reason": a.finishReason,
			},
		},
	}
	if a.usage != nil {
		resp["usage"] = a.usage
	}
	body, _ := json.Marshal(resp)
	return body
}
