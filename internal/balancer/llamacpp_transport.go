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

// shouldFilterLlamaCppContent — определяет, нужно ли отфильтровать строку
// content из streaming-ответа llama.cpp/cppworker как служебный токен
// (например Gemma `<end_of_turn>`, Llama3 `<|eot_id|>`, ChatML `<|im_end|>`).
//
// Зачем: cppworker иногда отдаёт служебные токены модели как обычный content
// (из-за неполного chat template или его отсутствия). OpenAI-клиенты типа
// Cline интерпретируют такие токены как невалидный tool-call-like вывод
// и выдают "Invalid API Response: The provider returned an empty or
// unparsable response". Фильтрация на стороне балансировщика покрывает
// любые модели с подобными артефактами.
//
// Возвращает true, если строка содержит служебный токен в любом месте
// (как самостоятельная строка, как префикс, в середине или как суффикс).
// Это критично для моделей вроде Gemma, которые из-за неполного chat template
// эмитят служебные токены не только отдельным чанком, но и вкраплениями
// в обычный текст: "hello<end_of_turn>world", "<|eot_id|>ok" и т.д.
func shouldFilterLlamaCppContent(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	// Список известных служебных токенов для разных моделей.
	filteredTokens := []string{
		"<end_of_turn>",       // Gemma
		"<start_of_turn>",     // Gemma
		"<bos>",               // Llama, общий
		"<eos>",               // Llama, общий
		"<endoftext>",         // GPT-2 / некоторые GGUF
		"<|endoftext|>",       // Llama2/3 (старый формат)
		"<|eot_id|>",          // Llama3 instruct
		"<|eot|>",             // некоторые варианты
		"<|im_start|>",        // ChatML
		"<|im_end|>",          // ChatML
		"<|start_header_id|>", // Llama3 header
		"<|end_header_id|>",   // Llama3 header
		"<|begin_of_text|>",   // Llama3 begin
		"<|end_of_text|>",     // Llama3 end
		"<sep>",               // BERT, некоторые GGUF
		"<pad>",               // служебные токены
		"<unk>",               // unknown token
	}
	// Служебные токены могут появляться в любом месте стрима: как самостоятельная
	// строка ("<end_of_turn>"), как префикс ("<end_of_turn>hello"), в середине
	// ("hello<|eot_id|>world") или как суффикс. Используем Contains вместо
	// == / HasPrefix, чтобы ловить их все.
	for _, tok := range filteredTokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// filterOpenAIStreamingLine — фильтрует SSE-строку с OpenAI chunk, удаляя
// content-токены, которые являются служебными (см. shouldFilterLlamaCppContent).
//
// Возвращает (filtered, wasFiltered):
//   - filtered: новая строка для записи клиенту (или nil, если нужно пропустить чанк)
//   - wasFiltered: true, если хотя бы один content-чанк был отфильтрован
//
// Работает на строке формата `data: {...}\n\n` или `data:{...}\n\n`.
func filterOpenAIStreamingLine(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return line, false
	}
	// Не data-строка — пропускаем как есть (": comment", "[DONE]", etc.)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line, false
	}
	// Извлекаем JSON-часть после "data: "
	jsonData := bytes.TrimPrefix(trimmed, []byte("data:"))
	jsonData = bytes.TrimSpace(jsonData)
	// [DONE] маркер — не трогаем
	if bytes.Equal(jsonData, []byte("[DONE]")) {
		return line, false
	}
	// Парсим JSON, чтобы достать content из choices[0].delta.content
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(jsonData, &chunk); err != nil {
		// Невалидный JSON — отдаём как есть (пусть клиент сам решает).
		return line, false
	}
	if len(chunk.Choices) == 0 {
		return line, false
	}
	content := chunk.Choices[0].Delta.Content
	if !shouldFilterLlamaCppContent(content) {
		return line, false
	}
	// content — служебный токен. Заменяем на пустую delta, чтобы чанк
	// пришёл клиенту, но с пустым content (это валидный OpenAI chunk).
	filteredChunk := map[string]interface{}{
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{},
			},
		},
	}
	// Сохраняем остальные поля (id, model, created, object) из оригинала.
	var orig map[string]interface{}
	if err := json.Unmarshal(jsonData, &orig); err == nil {
		for k, v := range orig {
			if k == "choices" {
				continue
			}
			filteredChunk[k] = v
		}
	}
	out, err := json.Marshal(filteredChunk)
	if err != nil {
		return line, false
	}
	// Возвращаем как `data: {...}\n\n` (с финальным \n для совместимости с SSE-форматом).
	return append([]byte("data: "+string(out)+"\n\n"), '\n'), true
}

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
	case "/api/embeddings":
		return translateOllamaEmbeddingsToOpenAI(body)
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

// translateOllamaEmbeddingsToOpenAI — маппит Ollama /api/embeddings на OpenAI /v1/embeddings.
// Ollama использует "prompt" (и иногда "input"), OpenAI — "input".
func translateOllamaEmbeddingsToOpenAI(body []byte) ([]byte, error) {
	var ollamaReq map[string]interface{}
	if err := json.Unmarshal(body, &ollamaReq); err != nil {
		return body, nil
	}
	openaiReq := map[string]interface{}{
		"model": ollamaReq["model"],
	}
	if input, ok := ollamaReq["input"].(string); ok && input != "" {
		openaiReq["input"] = input
	} else if prompt, ok := ollamaReq["prompt"].(string); ok && prompt != "" {
		openaiReq["input"] = prompt
	}
	return json.Marshal(openaiReq)
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

func translateOpenAIResponseToOllama(ollamaPath string, openaiBody []byte, modelName string) ([]byte, error) {
	if len(openaiBody) == 0 {
		return openaiBody, nil
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

func translateOpenAIChatToOllama(body []byte, modelName string) ([]byte, error) {
	var openaiResp map[string]interface{}
	if err := json.Unmarshal(body, &openaiResp); err != nil {
		return body, nil
	}
	ollamaResp := map[string]interface{}{
		"model":      modelName,
		"created_at": convertCreatedToRFC3339(openaiResp["created"]),
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
//  1. {"delta": {"role": "assistant"}}               — первый чанк, без контента
//  2. {"delta": {"content": "token"}}                — обычный токен
//  3. {"delta": {"content": "tok", "role": "..."}}   — токен с ролью
//  4. {"delta": {}, "finish_reason": "stop"}         — финальный
//  5. {"error": "...", "choices":[{"delta":{},"finish_reason":"error"}]} — ошибка от upstream
//
// Ollama-стрим ожидает валидный JSON-чанк с {"message": {"content": "..."}}.
// Role-only чанк (1) транслируется как {"message":{"role":"assistant","content":""}}
// — OpenWebUI использует его как маркер начала потока и игнорирует content.
// Content-чанки (2, 3) и финальный (4) эмитятся штатно. Невалидный/пустой
// чанк → nil (пропускается). Ошибочный чанк (5) — пробрасывает поле `error`
// в Ollama-чанк с done:true, done_reason:"error", иначе OpenWebUI получает
// «пустое сообщение ассистента» и не показывает диагностику.
//
// Фильтрация служебных токенов модели (Gemma `<end_of_turn>`, Llama3
// `<|eot_id|>` и т.д.) применяется через shouldFilterLlamaCppContent.
// Без этого Ollama-клиенты (Cline через ollama-js) получают «мусорный»
// content в message.content и падают с ошибкой парсинга JSON
// ("Invalid API Response"). Чанки со служебными токенами заменяются
// на чанк с пустым content (валидный OpenAI/Ollama-чанк).
func translateSSEChatToOllama(chunk map[string]interface{}, modelName string) []byte {
	ollamaChunk := map[string]interface{}{
		"model": modelName,
		"done":  false,
	}
	var hasContent bool
	var hasRoleOnly bool
	var roleStr string
	var contentStr string

	// cppworker при ошибке шлёт OpenAI чанк с полем `error` и пустым `choices[0].delta`
	// (см. Phase 9 фикс: translateSSEChatToOllama не теряет error). Если поле error есть —
	// пробрасываем его в Ollama-чанк с done:true, done_reason:"error", иначе OpenWebUI
	// получает "пустое сообщение ассистента" и не показывает диагностику.
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

	// Если есть контент — фильтруем служебные токены (Gemma `<end_of_turn>` и т.д.)
	// и формируем полный чанк. Если контент ПОЛНОСТЬЮ состоит из служебного токена —
	// эмитим чанк с пустым content (валидный Ollama NDJSON), чтобы клиент (Cline/ollama-js)
	// не упал с "Invalid API Response".
	if hasContent {
		if shouldFilterLlamaCppContent(contentStr) {
			// Служебный токен: заменяем на пустой content, но чанк остаётся валидным
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
//
// Если в чанке есть поле `error` (cppworker при ошибке шлёт его вместе с
// пустым choices[0].text и finish_reason:"error"), пробрасываем его в Ollama
// с done:true, done_reason:"error", иначе OpenWebUI рендерит пустое сообщение
// ассистента без диагностики.
func translateSSEGenerateToOllama(chunk map[string]interface{}, modelName string) []byte {
	ollamaChunk := map[string]interface{}{
		"model": modelName,
		"done":  false,
	}

	// Проброс ошибки от upstream (см. Phase 9 фикс: translateSSEGenerateToOllama не теряет error).
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
		// Минимальный таймаут для streaming inference — 10 минут.
		// LLM может генерировать длинный ответ (несколько тысяч токенов) и
		// не должен отваливаться по таймауту. По умолчанию StreamTimeout в
		// конфиге = 30 секунд, что слишком мало для реальной генерации.
		streamTimeout := time.Duration(p.config.Balancing.StreamTimeout) * time.Second
		const minStreamingTimeout = 10 * time.Minute
		if streamTimeout <= 0 || streamTimeout < minStreamingTimeout {
			streamTimeout = minStreamingTimeout
		}
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(r.Context(), streamTimeout)
		defer cancel()
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
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
		w.Write(errBody)
		w.Write([]byte("\n"))
		return nil
	}

	// Если body пустой и статус 200 — это обычно означает, что cppworker ещё
	// загружает модель (или вернул мусор). Возвращаем 502 с явной диагностикой,
	// чтобы OpenWebUI не рендерил "пустое сообщение ассистента".
	if len(respBody) == 0 {
		logger.Get().Warnw("proxyRequestLlamaCppNonStream: empty body from upstream",
			"backend", backendID, "model", modelFromCtx)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
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
		w.Write(errBody)
		w.Write([]byte("\n"))
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
