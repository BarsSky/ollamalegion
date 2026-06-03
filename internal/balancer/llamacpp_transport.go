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

// translatePathForLlamaCpp — преобразует Ollama API путь в llama.cpp (OpenAI-совместимый)
func translatePathForLlamaCpp(ollamaPath string) string {
	switch ollamaPath {
	case "/api/chat":
		return "/v1/chat/completions"
	case "/api/generate":
		return "/v1/completions"
	case "/api/embeddings":
		return "/v1/embeddings"
	default:
		return ollamaPath
	}
}

func translateOllamaBodyToOpenAI(ollamaPath string, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	switch ollamaPath {
	case "/api/chat":
		return translateOllamaChatToOpenAI(body)
	case "/api/generate":
		return translateOllamaGenerateToOpenAI(body)
	default:
		return body, nil
	}
}

func translateOllamaChatToOpenAI(body []byte) ([]byte, error) {
	var ollamaReq map[string]interface{}
	if err := json.Unmarshal(body, &ollamaReq); err != nil {
		return body, nil
	}
	openaiReq := map[string]interface{}{
		"model": ollamaReq["model"],
	}
	if messages, ok := ollamaReq["messages"].([]interface{}); ok {
		openaiReq["messages"] = messages
	} else if prompt, ok := ollamaReq["prompt"].(string); ok {
		openaiReq["messages"] = []map[string]string{{"role": "user", "content": prompt}}
	}
	if stream, ok := ollamaReq["stream"]; ok {
		openaiReq["stream"] = stream
	} else {
		openaiReq["stream"] = true
	}
	if options, ok := ollamaReq["options"].(map[string]interface{}); ok {
		if temp, ok := options["temperature"]; ok {
			openaiReq["temperature"] = temp
		}
		if topP, ok := options["top_p"]; ok {
			openaiReq["top_p"] = topP
		}
		if topK, ok := options["top_k"]; ok {
			openaiReq["top_k"] = topK
		}
		if maxTokens, ok := options["num_predict"]; ok {
			openaiReq["max_tokens"] = maxTokens
		}
		if stop, ok := options["stop"]; ok {
			openaiReq["stop"] = stop
		}
	}
	return json.Marshal(openaiReq)
}

func translateOllamaGenerateToOpenAI(body []byte) ([]byte, error) {
	var ollamaReq map[string]interface{}
	if err := json.Unmarshal(body, &ollamaReq); err != nil {
		return body, nil
	}
	openaiReq := map[string]interface{}{
		"model": ollamaReq["model"],
	}
	if prompt, ok := ollamaReq["prompt"].(string); ok {
		openaiReq["prompt"] = prompt
	}
	if stream, ok := ollamaReq["stream"]; ok {
		openaiReq["stream"] = stream
	} else {
		openaiReq["stream"] = false
	}
	if options, ok := ollamaReq["options"].(map[string]interface{}); ok {
		if temp, ok := options["temperature"]; ok {
			openaiReq["temperature"] = temp
		}
		if topP, ok := options["top_p"]; ok {
			openaiReq["top_p"] = topP
		}
		if maxTokens, ok := options["num_predict"]; ok {
			openaiReq["max_tokens"] = maxTokens
		}
		if stop, ok := options["stop"]; ok {
			openaiReq["stop"] = stop
		}
	}
	return json.Marshal(openaiReq)
}

func translateOpenAIResponseToOllama(ollamaPath string, openaiBody []byte, modelName string) ([]byte, error) {
	if len(openaiBody) == 0 {
		return openaiBody, nil
	}
	switch ollamaPath {
	case "/api/chat":
		return translateOpenAIChatToOllama(openaiBody, modelName)
	case "/api/generate":
		return translateOpenAICompletionToOllama(openaiBody, modelName)
	default:
		return openaiBody, nil
	}
}

func translateOpenAIChatToOllama(body []byte, modelName string) ([]byte, error) {
	var openaiResp map[string]interface{}
	if err := json.Unmarshal(body, &openaiResp); err != nil {
		return body, nil
	}
	ollamaResp := map[string]interface{}{
		"model":      modelName,
		"created_at": openaiResp["created"],
		"done":       true,
	}
	// ВАЖНО: если бэкенд вернул ошибку (нет поля choices, но есть error),
	// НЕЛЬЗЯ отдавать "done:true" без message — OpenWebUI интерпретирует это
	// как "успешный пустой ответ" и показывает пустое сообщение ассистента.
	// Правильное поведение: пробросить error и НЕ ставить done:true (или поставить
	// done:true вместе с error, чтобы клиент завершил поток).
	if errStr, ok := openaiResp["error"].(string); ok && errStr != "" {
		ollamaResp["done"] = true
		ollamaResp["done_reason"] = "error"
		ollamaResp["error"] = errStr
		ollamaResp["message"] = map[string]interface{}{
			"role":    "assistant",
			"content": "",
		}
		logger.Get().Errorw("translateOpenAIChatToOllama: upstream returned error",
			"model", modelName, "error", errStr)
		return json.Marshal(ollamaResp)
	}
	if choices, ok := openaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
		choice := choices[0].(map[string]interface{})
		if message, ok := choice["message"].(map[string]interface{}); ok {
			role := message["role"]
			if role == nil || role == "" {
				role = "assistant"
			}
			ollamaResp["message"] = map[string]interface{}{
				"role":    role,
				"content": message["content"],
			}
			if finishReason, ok := choice["finish_reason"].(string); ok {
				ollamaResp["done"] = finishReason == "stop"
				ollamaResp["done_reason"] = finishReason
			}
		}
	} else {
		// Нет choices и нет error — пустой/непонятный ответ.
		// Логируем и возвращаем done:true с пустым message и error-полем,
		// чтобы OpenWebUI НЕ показывал ложный «успешный пустой» ответ.
		logger.Get().Warnw("translateOpenAIChatToOllama: upstream returned no choices and no error",
			"model", modelName, "body", string(body))
		ollamaResp["done"] = true
		ollamaResp["done_reason"] = "error"
		ollamaResp["error"] = "upstream returned empty response"
		ollamaResp["message"] = map[string]interface{}{
			"role":    "assistant",
			"content": "",
		}
		return json.Marshal(ollamaResp)
	}
	if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
		if completionTokens, ok := usage["completion_tokens"].(float64); ok {
			ollamaResp["eval_count"] = int(completionTokens)
		}
		if promptTokens, ok := usage["prompt_tokens"].(float64); ok {
			ollamaResp["prompt_eval_count"] = int(promptTokens)
		}
		ollamaResp["total_duration"] = 0
	}
	return json.Marshal(ollamaResp)
}

func translateOpenAICompletionToOllama(body []byte, modelName string) ([]byte, error) {
	var openaiResp map[string]interface{}
	if err := json.Unmarshal(body, &openaiResp); err != nil {
		return body, nil
	}
	ollamaResp := map[string]interface{}{
		"model":      modelName,
		"created_at": openaiResp["created"],
		"done":       true,
	}
	if choices, ok := openaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
		choice := choices[0].(map[string]interface{})
		if text, ok := choice["text"].(string); ok {
			ollamaResp["response"] = text
		}
		if finishReason, ok := choice["finish_reason"].(string); ok {
			ollamaResp["done"] = finishReason == "stop"
			ollamaResp["done_reason"] = finishReason
		}
	}
	if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
		if completionTokens, ok := usage["completion_tokens"].(float64); ok {
			ollamaResp["eval_count"] = int(completionTokens)
		}
		if promptTokens, ok := usage["prompt_tokens"].(float64); ok {
			ollamaResp["prompt_eval_count"] = int(promptTokens)
		}
	}
	return json.Marshal(ollamaResp)
}

func translateOpenAISSEDataToOllama(ollamaPath string, sseData []byte, modelName string) []byte {
	// OpenAI-маркер [DONE] — это чисто разделитель потока, не нужно отправлять свой
	// done:true после финального чанка (который уже имеет finish_reason: "stop" и транслируется
	// в done:true раньше). Возвращаем nil, чтобы стрим просто корректно завершился.
	if len(sseData) == 0 || bytes.Equal(sseData, []byte("[DONE]")) {
		return nil
	}
	var openaiChunk map[string]interface{}
	if err := json.Unmarshal(sseData, &openaiChunk); err != nil {
		return sseData
	}
	switch ollamaPath {
	case "/api/chat":
		return translateSSEChatToOllama(openaiChunk, modelName)
	case "/api/generate":
		return translateSSEGenerateToOllama(openaiChunk, modelName)
	default:
		return sseData
	}
}

// translateSSEChatToOllama — переводит OpenAI streaming чанк в Ollama NDJSON.
//
// OpenAI streaming может присылать:
//   1. {"delta": {"role": "assistant"}}               — первый чанк, без контента
//   2. {"delta": {"content": "token"}}                — обычный токен
//   3. {"delta": {"content": "tok", "role": "..."}}   — токен с ролью
//   4. {"delta": {}, "finish_reason": "stop"}         — финальный
//
// Ollama-стрим ожидает валидный JSON-чанк с {"message": {"content": "..."}}.
// Role-only чанк (1) транслируется как {"message":{"role":"assistant","content":""}}
// — OpenWebUI использует его как маркер начала потока и игнорирует content.
// Content-чанки (2, 3) и финальный (4) эмитятся штатно. Невалидный/пустой
// чанк → nil (пропускается).
func translateSSEChatToOllama(chunk map[string]interface{}, modelName string) []byte {
	ollamaChunk := map[string]interface{}{
		"model": modelName,
		"done":  false,
	}
	var hasContent bool
	var hasRoleOnly bool
	var roleStr string
	var contentStr string

	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		choice := choices[0].(map[string]interface{})
		if delta, ok := choice["delta"].(map[string]interface{}); ok {
			// Извлекаем role, если есть
			if r, ok := delta["role"].(string); ok && r != "" {
				roleStr = r
			}
			// Извлекаем content (может быть string или null)
			if c, ok := delta["content"].(string); ok {
				contentStr = c
			}
			hasContent = contentStr != ""
			hasRoleOnly = !hasContent && roleStr != ""
		}
		if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
			ollamaChunk["done"] = true
			ollamaChunk["done_reason"] = finishReason
		}
	}

	// Если есть контент — формируем полный чанк
	if hasContent {
		msg := map[string]interface{}{"content": contentStr}
		if roleStr != "" {
			msg["role"] = roleStr
		} else {
			msg["role"] = "assistant"
		}
		ollamaChunk["message"] = msg
		result, _ := json.Marshal(ollamaChunk)
		return append(result, '\n')
	}

	// Role-only чанк (первый от OpenAI) — отдаём как Ollama-NDJSON с role-маркером
	// и пустым content. OpenWebUI использует его как сигнал «ассистент начал
	// отвечать», и фиксирует место в DOM ДО прихода первого content-чанка.
	// Тест TestTranslateSSEChatToOllama_RoleOnly явно требует не-nil ответа
	// с model+message.role+message.content="".
	if hasRoleOnly {
		ollamaChunk["message"] = map[string]interface{}{
			"role":    roleStr,
			"content": "",
		}
		result, _ := json.Marshal(ollamaChunk)
		return append(result, '\n')
	}

	// Финальный чанк с finish_reason и без контента
	if ollamaChunk["done"] == true {
		ollamaChunk["done"] = true
		ollamaChunk["message"] = map[string]interface{}{
			"role":    "assistant",
			"content": "",
		}
		result, _ := json.Marshal(ollamaChunk)
		return append(result, '\n')
	}

	return nil
}

// translateSSEGenerateToOllama — переводит OpenAI /v1/completions streaming чанк
// в Ollama NDJSON. Аналогично chat-варианту: текст → "response",
// финальный чанк с finish_reason → done:true (НЕ nil),
// пустой/невалидный чанк → nil.
func translateSSEGenerateToOllama(chunk map[string]interface{}, modelName string) []byte {
	ollamaChunk := map[string]interface{}{
		"model": modelName,
		"done":  false,
	}
	hasContent := false
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		choice := choices[0].(map[string]interface{})
		if text, ok := choice["text"].(string); ok && text != "" {
			ollamaChunk["response"] = text
			hasContent = true
		}
		if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
			ollamaChunk["done"] = true
			ollamaChunk["done_reason"] = finishReason
		}
	}
	if !hasContent {
		// Финальный чанк с finish_reason и без текста — всё равно отдаём done-маркер
		if ollamaChunk["done"] == true {
			ollamaChunk["response"] = ""
			result, _ := json.Marshal(ollamaChunk)
			return append(result, '\n')
		}
		return nil
	}
	result, _ := json.Marshal(ollamaChunk)
	return append(result, '\n')
}

func isStreamingFromBody(path string, body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	if stream, ok := req["stream"].(bool); ok && stream {
		return true
	}
	return false
}

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
		streamTimeout := time.Duration(p.config.Balancing.StreamTimeout) * time.Second
		if streamTimeout > 0 {
			var cancel context.CancelFunc
			reqCtx, cancel = context.WithTimeout(r.Context(), streamTimeout)
			defer cancel()
		}
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

	if !isStreaming {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response: %v", err)
		}
		translatedResp, err := translateOpenAIResponseToOllama(originalPath, respBody, modelFromCtx)
		if err != nil {
			translatedResp = respBody
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(translatedResp)
		return nil
	}

	// Streaming: транслируем SSE → Ollama NDJSON
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
			logger.Get().Errorw("streaming read error", "error", err)
			return fmt.Errorf("streaming read error: %v", err)
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
			w.Write(line)
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
				"model":      modelFromCtx,
				"created_at": time.Now().UTC().Format(time.RFC3339),
				"done":       true,
				"done_reason": "empty_response",
				"error":      "backend returned no content",
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
				"model":      modelFromCtx,
				"created_at": time.Now().UTC().Format(time.RFC3339),
				"done":       true,
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
		w.WriteHeader(resp.StatusCode)
		w.Write(ollamaBody)
		return nil
	}

	translatedResp, err := translateOpenAIResponseToOllama(originalPath, respBody, modelFromCtx)
	if err != nil {
		translatedResp = respBody
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(translatedResp)
	return nil
}

func stripStreamFlag(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return body
	}
	req["stream"] = false
	out, _ := json.Marshal(req)
	return out
}

func minLenInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}