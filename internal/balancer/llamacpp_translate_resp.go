// llamacpp_translate_resp.go — Translation of OpenAI-compatible responses from llama.cpp/cppworker
// back to Ollama NDJSON format. Includes response-level and SSE-streaming translations.
package balancer

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// stripReasoningTags — убирает opening/closing reasoning tags из content.
// Используется как defensive cleanup для gemma-4, который иногда leak'ит
// “ теги в content (cppworker's SplitReasoningContent разделяет
// правильно, но close tag `</think>` может остаться в content).
//
// Round 31 (2026-08-09).
// Round 32 (2026-08-09): добавлены gemma-4 native chat-template форматы
// (`<|channel>thought...<channel|>`) и Qwen-style `<|think>...<think|>`.
// cppworker SplitReasoningContent теперь их распознаёт (после фикса в
// reasoning_content.go), но balancer делает defensive cleanup если
// (а) cppworker старой версии, (б) tag разорван между чанками и split
// не сработал, (в) модель вне whitelist IsReasoningModel и reasoning
// не был отделён на стороне cppworker.
//
// ВАЖНО про close-теги: некоторые close разделяются между разными open
// (`<|channel>thought` и `<|channel>analysis` оба используют `<channel|>`).
// Для таких shared close мы НЕ делаем ReplaceAll после pair-strip — иначе
// iter для thought сожрал бы close, нужный для analysis пары.
// См. sharedClose[i] ниже.
//
// Также для orphan strip используем stripWithBoundary (не ReplaceAll)
// чтобы не сожрать prefix более длинного тега. Например, `<think` без
// границы сожрал бы `<think|>` (qwen-style с bar-обёрткой).
func stripReasoningTags(content string) string {
	// Поддерживаем все 4 типа тегов из thinkTagPairs (cppworker/cmd/cppworker/reasoning_content.go).
	openTags := []string{
		"<think>", "<thinking>", "<reasoning>", "<analysis>",
		// Round 31: <think (без ">") — gemma-4 иногда эмитит неполный open.
		// stripWithBoundary защищает от strip'а prefix <think|>.
		"<think",
		// Round 32: gemma-4 native channel format (chat template canonical).
		"<|channel>thought\n", "<|channel>thought",
		// Round 32: gemma-4 alternative analysis channel.
		"<|channel>analysis\n", "<|channel>analysis",
		// Round 32: Qwen-style with |...| wrapping.
		"<|think>",
		// Round 32: message separator (appears inside channel blocks).
		"<|message|>",
		// Round 32 #8 (2026-08-10): bare <|channel>...<channel|> variant.
		// Live test с gemma-4 показал: модель эмитит bare <|channel> (без
		// "thought"/"analysis" суффикса) — Round 32 patterns не матчили,
		// и tag leak'ал в content (пользователь видел "<|channel>Думаю..."
		// в OpenWebUI). ВАЖНО: bare <|channel> добавлен В КОНЕЦ — иначе
		// он бы сожрал prefix <|channel>thought (openTags итерируется
		// последовательно, stripWithBoundary не защищает от prefix'а
		// более длинного тега, начинающегося с того же префикса).
		"<|channel>",
	}
	closeTags := []string{
		"</think>", "</thinking>", "</reasoning>", "</analysis>",
		"", // <think — orphan open, no close pair
		"\n<channel|>", "<channel|>",
		"\n<channel|>", "<channel|>",
		"<think|>",
		"",           // <|message|> — не используется как парный close
		"<channel|>", // bare <|channel> → <channel|>
	}
	// sharedClose: true = НЕ делать ReplaceAll на close после pair-strip
	// (close используется несколькими open — нужно сохранить для следующего iter).
	// false = close уникален для этого open, можно безопасно strip'ать orphan close.
	sharedClose := []bool{
		false, false, false, false,
		false,      // <think has no close
		true, true, // \n<channel|> and <channel|> used by both thought and analysis
		true, true,
		false, // <think|> is unique
		false, // <|message|> has no close
		true,  // <channel|> is shared with thought/analysis pairs
	}

	for i, open := range openTags {
		close := closeTags[i]
		// Сначала удаляем пары открытие+закрытие (если close не пустой).
		if close != "" {
			for {
				openIdx := strings.Index(content, open)
				if openIdx < 0 {
					break
				}
				closeIdx := strings.Index(content[openIdx+len(open):], close)
				if closeIdx < 0 {
					break
				}
				// Удаляем пару (ВКЛЮЧАЯ тело между ними).
				// Round 32 (2026-08-09): для gemma-4 channel format это критично —
				// если cppworker не split'нул (старая версия, model не в whitelist),
				// reasoning текст лежит в content, и его надо убрать.
				absClose := openIdx + len(open) + closeIdx + len(close)
				content = content[:openIdx] + content[absClose:]
			}
		}
		// Затем удаляем одиночные opening tags (orphan open — close не нашло пары).
		// Используем stripWithBoundary вместо ReplaceAll, чтобы не сожрать
		// prefix более длинного тега. Например, "<think" без границы сожрал бы
		// "<think|>" (qwen-style с bar-обёрткой).
		content = stripWithBoundary(content, open)
		// Strip orphan close только если close уникален для этого open.
		// Для shared close — оставляем как есть, чтобы не сломать пары с другими open.
		if close != "" && !sharedClose[i] {
			content = stripWithBoundary(content, close)
		}
	}
	// Collapse multiple newlines/spaces в один (после удаления тегов
	// могут остаться "\n\n" или "  ").
	for strings.Contains(content, "\n\n") {
		content = strings.ReplaceAll(content, "\n\n", "\n")
	}
	// Trim leading newlines/tabs (gemma-4 часто эмитит "\n" между </think> и answer).
	// НЕ тримим space — легитимный контент может начинаться с пробела (list, code block).
	// Round 31 #4 (2026-08-09): only "\n\r\t", без " ".
	content = strings.TrimLeft(content, "\n\r\t")
	return content
}

// stripWithBoundary — заменяет все вхождения needle на "" в content,
// НО только если вхождение является полным тегом. Защищает от strip'а
// prefix более длинного тега (например, "<think" без границы сожрал бы
// "<think|>" (qwen-style с bar-обёрткой)).
//
// Оптимизация: для needle, которые заканчиваются на '>' (полные теги
// вроде `<think>` или `<|message|>`), после '>' не может быть продолжения
// того же тега — используем прямой strings.ReplaceAll.
//
// Для needle без '>' (например, "<think" или "<|channel>thought") нужна
// проверка границы: следующий символ должен быть '>', '\n', пробелом или
// концом строки. Если после needle идёт буква или '|' — это часть
// более длинного тега, оставляем как есть.
//
// Round 32 (2026-08-09): для orphan handling после pair-strip.
func stripWithBoundary(content, needle string) string {
	if needle == "" {
		return content
	}
	// Полные теги (заканчиваются на '>') — после '>' не может быть другого
	// тега с тем же началом, поэтому ReplaceAll безопасен.
	if needle[len(needle)-1] == '>' {
		return strings.ReplaceAll(content, needle, "")
	}
	// Неполные теги (без '>') — нужна проверка границы.
	var sb strings.Builder
	sb.Grow(len(content))
	pos := 0
	for {
		idx := strings.Index(content[pos:], needle)
		if idx < 0 {
			sb.WriteString(content[pos:])
			break
		}
		absIdx := pos + idx
		sb.WriteString(content[pos:absIdx])
		nextIdx := absIdx + len(needle)
		if nextIdx < len(content) {
			nextCh := content[nextIdx]
			// Tag boundary: '>' (full tag), '\n' (partial open), ' ' (rare).
			// NOT boundary: alphanumeric или '|' (part of longer tag).
			if nextCh == '>' || nextCh == '\n' || nextCh == ' ' {
				pos = nextIdx // strip
				continue
			}
			// Not a boundary — оставляем needle на месте
			sb.WriteString(needle)
			pos = absIdx + len(needle)
			continue
		}
		// End of string after needle — complete tag, strip it
		pos = nextIdx
	}
	return sb.String()
}

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
			// Round 31 (2026-08-09): cppworker's SplitReasoningContent иногда
			// leak'ит `` теги в content для gemma-4 (model emits
			// "<think\n...answer..." — close tag `</think>` остаётся в content).
			// Защитный strip: если `reasoning_content` заполнен, считаем что
			// cppworker split'ил, но могут быть остатки тегов.
			contentStr, _ := message["content"].(string)
			if msgReasoning, hasReasoning := message["reasoning_content"].(string); hasReasoning && msgReasoning != "" {
				// Есть reasoning — strip opening/closing tags из content.
				contentStr = stripReasoningTags(contentStr)
			}
			msgMap := map[string]interface{}{
				"role":    role,
				"content": contentStr,
			}
			// Round 29 (2026-08-09): reasoning_content → message.reasoning
			// (Ollama API). cppworker эмитит reasoning_content для reasoning-моделей
			// (gemma-4, qwen3.5/qwen3.6, deepseek-r1). OpenWebUI ожидает message.reasoning.
			if rc, ok := message["reasoning_content"].(string); ok && rc != "" {
				msgMap["reasoning"] = rc
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
//
// seenReasoning — pointer на per-stream state (Round 31 #4): если за всё время стрима
// уже встречался reasoning chunk, translator тримит leading whitespace у content
// (gemma-4 после SplitReasoningContent эмитит "\n" перед первым content токеном).
// nil = stateless (для тестов и non-stream вызовов).
func translateOpenAISSEDataToOllama(ollamaPath string, sseData []byte, modelName string, seenReasoning *bool) []byte {
	if len(sseData) == 0 || bytes.Equal(sseData, []byte("[DONE]")) {
		return nil
	}
	var openaiChunk map[string]interface{}
	if err := json.Unmarshal(sseData, &openaiChunk); err != nil {
		return sseData
	}
	switch ollamaPath {
	case "/api/chat":
		return translateSSEChatToOllama(openaiChunk, modelName, seenReasoning)
	case "/api/generate":
		return translateSSEGenerateToOllama(openaiChunk, modelName, seenReasoning)
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
//  6. {"delta": {"reasoning_content": "..."}}       — reasoning (gemma-4/qwen3.6/deepseek-r1)
//  7. {"delta": {"reasoning_content": "...", "content": "..."}} — mixed reasoning+content
//
// seenReasoning — pointer на per-stream state (Round 31 #4). Translator обновляет
// его когда видит reasoning chunk, и тримит leading whitespace content'а
// если reasoning уже был в этом стриме (gemma-4 после SplitReasoningContent
// эмитит "\n" перед первым content токеном).
// nil = stateless mode (для тестов).
func translateSSEChatToOllama(chunk map[string]interface{}, modelName string, seenReasoning *bool) []byte {
	ollamaChunk := map[string]interface{}{
		"model": modelName,
		"done":  false,
	}
	var hasContent bool
	var hasReasoning bool
	var hasRoleOnly bool
	var hasToolCalls bool
	var roleStr string
	var contentStr string
	var reasoningStr string
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
			// Round 29 (2026-08-09): extract reasoning_content для reasoning-моделей
			// (gemma-4, qwen3.5/qwen3.6, deepseek-r1). cppworker эмитит отдельный
			// chunk с delta.reasoning_content — маппим в message.thinking (Ollama API).
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
				reasoningStr = rc
				// Round 31 #4 (2026-08-09): per-stream state — если в стриме уже был
				// reasoning, content получит defensive strip от leading whitespace.
				if seenReasoning != nil {
					*seenReasoning = true
				}
			}
			hasContent = contentStr != ""
			hasReasoning = reasoningStr != ""
			hasRoleOnly = !hasContent && !hasReasoning && roleStr != ""
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

	// Reasoning-only chunk (cppworker separates reasoning from content for gemma-4 et al).
	// Emit NDJSON with message.thinking only — content stays empty.
	// Round 29 (2026-08-09): if reasoning comes WITH tool_calls, merge into single chunk.
	if hasReasoning && !hasContent {
		msg := map[string]interface{}{
			"role":     roleStr,
			"content":  "",
			"thinking": reasoningStr,
		}
		if msg["role"] == "" {
			msg["role"] = "assistant"
		}
		if hasToolCalls && len(toolCallsJSON) > 0 {
			var toolCallsArr []interface{}
			if err := json.Unmarshal(toolCallsJSON, &toolCallsArr); err == nil {
				msg["tool_calls"] = toolCallsArr
			}
		}
		ollamaChunk["message"] = msg
		result, _ := json.Marshal(ollamaChunk)
		return append(result, '\n')
	}

	// Content-чанк
	if hasContent {
		// Round 31 #4 (2026-08-09): defensive strip когда reasoning был в этом стриме.
		// gemma-4 после SplitReasoningContent эмитит "\n" перед первым content токеном
		// (как separator после </think>). Если reasoning уже был — strip'аем leading whitespace
		// чтобы content не начинался с "\n".
		if seenReasoning != nil && *seenReasoning {
			contentStr = stripReasoningTags(contentStr)
		}
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
		// Round 29 (2026-08-09): preserve reasoning_content when emitted in same chunk
		// as content (some cppworker emit formats include both).
		if hasReasoning {
			msg["thinking"] = reasoningStr
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
//
// seenReasoning — pointer на per-stream state (Round 31 #4). nil = stateless.
func translateSSEGenerateToOllama(chunk map[string]interface{}, modelName string, seenReasoning *bool) []byte {
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
	// Round 29 (2026-08-09): reasoning_content в /v1/completions streaming.
	// cppworker может эмитить reasoning_content в choice (legacy) или top-level (новый формат).
	var reasoningStr string
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		choice := choices[0].(map[string]interface{})
		if text, ok := choice["text"].(string); ok && text != "" {
			ollamaChunk["response"] = text
			hasContent = true
		}
		if rc, ok := choice["reasoning_content"].(string); ok && rc != "" {
			reasoningStr = rc
		}
		if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
			ollamaChunk["done"] = true
			ollamaChunk["done_reason"] = finishReason
		}
	}
	// Также проверяем top-level reasoning_content (новый формат cppworker)
	if rc, ok := chunk["reasoning_content"].(string); ok && rc != "" {
		reasoningStr = rc
	}
	// Round 31 #4 (2026-08-09): per-stream state — reasoning seen?
	if reasoningStr != "" && seenReasoning != nil {
		*seenReasoning = true
	}
	// Round 31 #4 (2026-08-09): defensive strip когда reasoning был в этом стриме.
	if hasContent && seenReasoning != nil && *seenReasoning {
		if s, ok := ollamaChunk["response"].(string); ok {
			ollamaChunk["response"] = stripReasoningTags(s)
		}
	}

	if reasoningStr != "" {
		ollamaChunk["thinking"] = reasoningStr
	}
	if !hasContent {
		if ollamaChunk["done"] == true {
			ollamaChunk["response"] = ""
			result, _ := json.Marshal(ollamaChunk)
			return append(result, '\n')
		}
		// Если есть reasoning без content — всё равно эмитим (иначе thinking потеряется)
		if reasoningStr != "" {
			ollamaChunk["response"] = ""
			result, _ := json.Marshal(ollamaChunk)
			return append(result, '\n')
		}
		return nil
	}
	result, _ := json.Marshal(ollamaChunk)
	return append(result, '\n')
}
