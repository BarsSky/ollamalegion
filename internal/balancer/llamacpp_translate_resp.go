// llamacpp_translate_resp.go — Translation of OpenAI-compatible responses from llama.cpp/cppworker
// back to Ollama NDJSON format. Includes response-level and SSE-streaming translations.
package balancer

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"

	"ollama-loadbalancer/pkg/logger"
)


// buildErrorOllamaResponse — строит корректный Ollama-ответ с ошибкой для случаев,
// когда upstream вернул пустое или невалидное тело. Возвращает JSON в Ollama-формате
// с полями done:true, done_reason:"error", error:"<msg>" и пустым message/response.
func buildErrorOllamaResponse(ollamaPath, modelName, errMsg string) []byte {
	if ollamaPath == "/api/generate" {
		out, _ := json.Marshal(map[string]interface{}{
			"model":       modelName,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
			"done":        true,
			"done_reason": "error",
			"error":       errMsg,
			"response":    "",
		})
		return out
	}
	out, _ := json.Marshal(map[string]interface{}{
		"model":       modelName,
		"created_at":  time.Now().UTC().Format(time.RFC3339),
		"done":        true,
		"done_reason": "error",
		"error":       errMsg,
		"message": map[string]interface{}{
			"role":    "assistant",
			"content": "",
		},
	})
	return out
}

// convertCreatedToRFC3339 — нормализует поле created (Unix timestamp в секундах)

// из OpenAI-ответа в RFC3339-строку, которую ожидает Ollama API.
//
// Проблема: OpenAI возвращает `created` как целое число (Unix timestamp в секундах),
// а Ollama требует ISO-8601 / RFC3339 строку. Если балансер пробрасывает
// число «как есть», ollama-js (используется в Cline) может не разобрать
// created_at, что приводит к ошибке "Invalid API Response".
//
// Поддерживаемые входные форматы:
//   - float64 / int (Unix seconds, опционально .fractional)
//   - string (числовая или RFC3339; для RFC3339 — возвращается как есть)
//   - nil — возвращает текущее время
func convertCreatedToRFC3339(v interface{}) string {
	if v == nil {
		return time.Now().UTC().Format(time.RFC3339)
	}
	switch val := v.(type) {
	case string:
		if val == "" {
			return time.Now().UTC().Format(time.RFC3339)
		}
		// Если строка уже похожа на RFC3339 — возвращаем как есть
		if _, err := time.Parse(time.RFC3339, val); err == nil {
			return val
		}
		// Если строка — числовая, пробуем распарсить как Unix
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return time.Unix(int64(f), 0).UTC().Format(time.RFC3339)
		}
		return time.Now().UTC().Format(time.RFC3339)
	case float64:
		sec := int64(val)
		nsec := int64((val - float64(sec)) * 1e9)
		return time.Unix(sec, nsec).UTC().Format(time.RFC3339)
	case int:
		return time.Unix(int64(val), 0).UTC().Format(time.RFC3339)
	case int64:
		return time.Unix(val, 0).UTC().Format(time.RFC3339)
	case json.Number:
		if f, err := val.Float64(); err == nil {
			sec := int64(f)
			nsec := int64((f - float64(sec)) * 1e9)
			return time.Unix(sec, nsec).UTC().Format(time.RFC3339)
		}
		return time.Now().UTC().Format(time.RFC3339)
	default:
		return time.Now().UTC().Format(time.RFC3339)
	}
}

// translateOpenAIResponseToOllama — преобразует OpenAI response body в Ollama-формат.
func translateOpenAIResponseToOllama(ollamaPath string, openaiBody []byte, modelName string) ([]byte, error) {
	if len(openaiBody) == 0 {
		logger.Get().Warnw("translateOpenAIResponseToOllama: empty body from upstream",
			"model", modelName, "path", ollamaPath)
		return buildErrorOllamaResponse(ollamaPath, modelName, "upstream returned empty response"), nil
	}
	switch ollamaPath {
	case "/api/chat":
		return translateOpenAIChatToOllama(openaiBody, modelName)
	case "/api/generate":
		return translateOpenAICompletionToOllama(openaiBody, modelName)
	case "/api/embeddings":
		return translateOpenAIEmbeddingsToOllama(openaiBody, modelName)
	default:
		return openaiBody, nil
	}
}


// translateOpenAIChatToOllama — маппит OpenAI /v1/chat/completions ответ на Ollama /api/chat.
func translateOpenAIChatToOllama(body []byte, modelName string) ([]byte, error) {
	var openaiResp map[string]interface{}
	if err := json.Unmarshal(body, &openaiResp); err != nil {
		// Возвращаем структурированную Ollama-ошибку вместо сырого тела,
		// чтобы клиент (OpenWebUI) получил валидный JSON, а не "Expecting value".
		logger.Get().Warnw("translateOpenAIChatToOllama: failed to unmarshal upstream body",
			"model", modelName, "error", err, "body_len", len(body))
		return buildErrorOllamaResponse("/api/chat", modelName,
			"upstream returned invalid JSON: "+err.Error()), nil
	}
	ollamaResp := map[string]interface{}{
		"model":      modelName,
		"created_at": convertCreatedToRFC3339(openaiResp["created"]),
		"done":       true,
	}
	// ВАЖНО: если бэкенд вернул ошибку (нет поля choices, но есть error),
	// НЕЛЬЗЯ отдавать "done:true" без message — OpenWebUI интерпретирует это
	// как "успешный пустой ответ" и показывает пустое сообщение ассистента.
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
			msgMap := map[string]interface{}{
				"role":    role,
				"content": message["content"],
			}
			// Handle tool_calls in the response message
			if tc, ok := message["tool_calls"].([]interface{}); ok && len(tc) > 0 {
				msgMap["tool_calls"] = tc
				// Ollama клиенты ожидают content:"" если есть tool_calls
				if msgMap["content"] == nil {
					msgMap["content"] = ""
				}
			} else if contentStr, ok := message["content"].(string); ok && contentStr != "" {
				// Детектируем JSON tool calls в content (для моделей без поддержки tool calling)
				if detectedTC, remainingTC, found := detectAndExtractToolCallsFromContent(contentStr); found && len(detectedTC) > 0 {
					logger.Get().Infow("translateOpenAIChatToOllama: detected tool_calls in content, extracting",
						"model", modelName, "tool_calls_count", len(detectedTC),
						"original_content_len", len(contentStr),
						"remaining_content_len", len(remainingTC))
					msgMap["tool_calls"] = detectedTC
						// Clean remaining content: remove service tokens, duplicate tool calls, role markers
					cleaned := stripServiceTokens(remainingTC)
					cleaned = cleanContentAfterToolCallExtraction(cleaned)
					msgMap["content"] = cleaned
					if msgMap["content"] == "" {
						msgMap["content"] = ""
					}
				}
			}
			ollamaResp["message"] = msgMap
			if finishReason, ok := choice["finish_reason"].(string); ok {
				ollamaResp["done"] = finishReason == "stop" || finishReason == "tool_calls"
				ollamaResp["done_reason"] = finishReason
			}
		}
	} else {
		// Нет choices и нет error — пустой/непонятный ответ.
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

// translateOpenAICompletionToOllama — маппит OpenAI /v1/completions ответ на Ollama /api/generate.
func translateOpenAICompletionToOllama(body []byte, modelName string) ([]byte, error) {
	var openaiResp map[string]interface{}
	if err := json.Unmarshal(body, &openaiResp); err != nil {
		return body, nil
	}
	ollamaResp := map[string]interface{}{
		"model":      modelName,
		"created_at": convertCreatedToRFC3339(openaiResp["created"]),
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

// translateOpenAIEmbeddingsToOllama — маппит OpenAI /v1/embeddings ответ на Ollama /api/embeddings.
func translateOpenAIEmbeddingsToOllama(body []byte, modelName string) ([]byte, error) {
	var openaiResp map[string]interface{}
	if err := json.Unmarshal(body, &openaiResp); err != nil {
		return body, nil
	}
	if data, ok := openaiResp["data"].([]interface{}); ok && len(data) > 0 {
		if item, ok := data[0].(map[string]interface{}); ok {
			if embedding, ok := item["embedding"].([]interface{}); ok {
				return json.Marshal(map[string]interface{}{
					"embedding": embedding,
				})
			}
		}
	}
	if errStr, ok := openaiResp["error"].(string); ok && errStr != "" {
		return json.Marshal(map[string]interface{}{
			"error": errStr,
		})
	}
	return body, nil
}

// translateOpenAISSEDataToOllama — переводит OpenAI SSE streaming data в Ollama NDJSON.
// Если чанк — [DONE], возвращает nil.
func translateOpenAISSEDataToOllama(ollamaPath string, sseData []byte, modelName string) []byte {
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
//  1. {"delta": {"role": "assistant"}}               — первый чанк, без контента
//  2. {"delta": {"content": "token"}}                — обычный токен
//  3. {"delta": {"content": "tok", "role": "..."}}   — токен с ролью
//  4. {"delta": {}, "finish_reason": "stop"}         — финальный
//  5. {"error": "...", "choices":[{"delta":{},"finish_reason":"error"}]} — ошибка от upstream
func translateSSEChatToOllama(chunk map[string]interface{}, modelName string) []byte {
	ollamaChunk := map[string]interface{}{
		"model": modelName,
		"done":  false,
	}
	var hasContent bool
	var hasRoleOnly bool
	var hasToolCalls bool
	var roleStr string
	var contentStr string
	var toolCallsJSON json.RawMessage

	// Проброс ошибки от upstream.
	if errStr, ok := chunk["error"].(string); ok && errStr != "" {
		logger.Get().Errorw("translateSSEChatToOllama: upstream SSE chunk with error",
			"model", modelName, "error", errStr)
		ollamaChunk["done"] = true
		ollamaChunk["done_reason"] = "error"
		ollamaChunk["error"] = errStr
		ollamaChunk["message"] = map[string]interface{}{
			"role":    "assistant",
			"content": "",
		}
		result, _ := json.Marshal(ollamaChunk)
		return append(result, '\n')
	}

	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		choice := choices[0].(map[string]interface{})
		if delta, ok := choice["delta"].(map[string]interface{}); ok {
			if r, ok := delta["role"].(string); ok && r != "" {
				roleStr = r
			}
			if c, ok := delta["content"].(string); ok {
				contentStr = c
			}
			hasContent = contentStr != ""
			hasRoleOnly = !hasContent && roleStr != ""
			// Detect delta.tool_calls in SSE chunks from cppworker
			if tc, ok := delta["tool_calls"]; ok && tc != nil {
				hasToolCalls = true
				if tcRaw, err := json.Marshal(tc); err == nil {
					toolCallsJSON = tcRaw
				}
			}
		}
		if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
			ollamaChunk["done"] = finishReason == "stop" || finishReason == "tool_calls"
			ollamaChunk["done_reason"] = finishReason
		}
	}

	// Content-чанк
	if hasContent {
		if shouldFilterLlamaCppContent(contentStr) {
			logger.Get().Debugw("translateSSEChatToOllama: filtered service token from content",
				"model", modelName, "filtered_content_len", len(contentStr))
			msg := map[string]interface{}{"content": ""}
			if roleStr != "" {
				msg["role"] = roleStr
			} else {
				msg["role"] = "assistant"
			}
			ollamaChunk["message"] = msg
			result, _ := json.Marshal(ollamaChunk)
			return append(result, '\n')
		}

		// Детектируем JSON tool calls в content (для моделей без поддержки tool calling)
		if detectedTC, remainingTC, found := detectAndExtractToolCallsFromContent(contentStr); found && len(detectedTC) > 0 {
			logger.Get().Infow("translateSSEChatToOllama: detected tool_calls in content, extracting",
				"model", modelName, "tool_calls_count", len(detectedTC),
				"original_content_len", len(contentStr),
				"remaining_content_len", len(remainingTC))

			// Clean remaining: remove service tokens, duplicate tool calls, role markers
			cleanRemaining := stripServiceTokens(remainingTC)
			cleanRemaining = cleanContentAfterToolCallExtraction(cleanRemaining)
			msg := map[string]interface{}{"content": cleanRemaining}
			if roleStr != "" {
				msg["role"] = roleStr
			} else {
				msg["role"] = "assistant"
			}
			msg["tool_calls"] = detectedTC
			ollamaChunk["message"] = msg
			result, _ := json.Marshal(ollamaChunk)
			return append(result, '\n')
		}

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

	// Role-only чанк с tool_calls (первый от OpenAI при finish_reason="tool_calls").
	// CppWorker при стриминге с tools отправляет SSE chunk с delta.role + delta.tool_calls
	// (но без content). Этот чанк надо сконвертировать в NDJSON с message.tool_calls.
	if hasRoleOnly && hasToolCalls && len(toolCallsJSON) > 0 {
		msgMap := map[string]interface{}{
			"role":    roleStr,
			"content": "",
		}
		// Парсим toolCallsJSON обратно в []interface{} для корректной сериализации
		var toolCallsArr []interface{}
		if err := json.Unmarshal(toolCallsJSON, &toolCallsArr); err == nil {
			msgMap["tool_calls"] = toolCallsArr
		}
		ollamaChunk["message"] = msgMap
		result, _ := json.Marshal(ollamaChunk)
		return append(result, '\n')
	}

	// Tool-call content чанк (с content + tool_calls одновременно, если delta содержит оба).
	// В cppworker такого не бывает (tools стриминг всегда буферизированный), но
	// на случай future-совместимости — конвертируем как content + tool_calls.
	if hasContent && hasToolCalls && len(toolCallsJSON) > 0 {
		msgMap := map[string]interface{}{
			"role":    roleStr,
			"content": contentStr,
		}
		var toolCallsArr []interface{}
		if err := json.Unmarshal(toolCallsJSON, &toolCallsArr); err == nil {
			msgMap["tool_calls"] = toolCallsArr
		}
		ollamaChunk["message"] = msgMap
		result, _ := json.Marshal(ollamaChunk)
		return append(result, '\n')
	}

	// Role-only чанк (первый от OpenAI)
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
// в Ollama NDJSON для /api/generate.
func translateSSEGenerateToOllama(chunk map[string]interface{}, modelName string) []byte {
	ollamaChunk := map[string]interface{}{
		"model": modelName,
		"done":  false,
	}

	// Проброс ошибки от upstream.
	if errStr, ok := chunk["error"].(string); ok && errStr != "" {
		logger.Get().Errorw("translateSSEGenerateToOllama: upstream SSE chunk with error",
			"model", modelName, "error", errStr)
		ollamaChunk["done"] = true
		ollamaChunk["done_reason"] = "error"
		ollamaChunk["error"] = errStr
		ollamaChunk["response"] = ""
		result, _ := json.Marshal(ollamaChunk)
		return append(result, '\n')
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
