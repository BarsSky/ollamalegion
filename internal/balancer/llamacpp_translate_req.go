// llamacpp_translate_req.go — Translation of Ollama API requests to OpenAI-compatible format
// for llama.cpp/cppworker backends.
package balancer

import (
	"encoding/json"
	"strings"
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

// translateOllamaBodyToOpenAI — преобразует тело Ollama-запроса в OpenAI-формат
// в зависимости от пути. Если путь не требует трансляции — возвращает тело как есть.
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

// isOpenAIPath — R60.10 (2026-09-07): true для /v1/* paths (OpenAI-compatible).
// Используется для выбора формата error body при wrap upstream 5xx ответов.
// OpenAI clients (openai-python, OpenWebUI OpenAI mode) ждут
// `{"error":{"message":...,"type":...,"code":N}}` формат, а Ollama ждёт
// `{"error":"...","done":true,"done_reason":"error"}`.
func isOpenAIPath(path string) bool {
	return strings.HasPrefix(path, "/v1/")
}

// extractNameFromBody — извлекает поле "name" из JSON body.
// Возвращает (name, modifiedBody) где modifiedBody — body без поля "name"
// (для случая когда name должно быть передано как query param).
// Если body невалидный JSON или name отсутствует — возвращает ("", body) без изменений.
//
// R60.9 (2026-09-07): для /api/models/unload cppworker требует ?name=...
// в QUERY STRING, а не в body. Ollama-стиль клиенты (включая OpenWebUI
// и webui) шлют name в body. Без этого helper'а balancer передаёт
// `{"name":"foo"}` в body и получает 400 "name query parameter is required".
func extractNameFromBody(body []byte) (name string, modifiedBody []byte) {
	if len(body) == 0 {
		return "", body
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", body // невалидный JSON — не трогаем, cppworker сам разберётся
	}
	nameVal, ok := req["name"].(string)
	if !ok || nameVal == "" {
		return "", body
	}
	delete(req, "name")
	modifiedBody, err := json.Marshal(req)
	if err != nil {
		return nameVal, body // marshal failed — return name but keep body
	}
	return nameVal, modifiedBody
}

// translateOllamaChatToOpenAI — маппит Ollama /api/chat на OpenAI /v1/chat/completions.
// Ollama использует более простой формат: model + messages + options.stop/temperature/top_p.
// OpenAI — тот же формат, но без вложенного options блока.
// Также пробрасывает tools и tool_choice для поддержки function calling.
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
	// Пробрасываем tools и tool_choice для function calling (OpenAI-формат в Ollama
	// /api/chat — это часть сообщения "options.tools" или прямой ключ "tools").
	//
	// 2026-06-25: добавлена поддержка `options.tools` (Ollama-native стиль),
	// который часто используют Cline/Roo Code/OpenWebUI как fallback. Также
	// добавлен дефолтный `tool_choice: "auto"` для моделей без нативного tool-call
	// (Gemma-4, Hermes-prompt) — без явного tool_choice они часто возвращают
	// обычный текст вместо JSON tool_call.
	toolsSet := false
	if tools, ok := ollamaReq["tools"].([]interface{}); ok && len(tools) > 0 {
		openaiReq["tools"] = tools
		toolsSet = true
	}
	if !toolsSet {
		// Fallback: некоторые клиенты шлют tools через options.tools
		if options, ok := ollamaReq["options"].(map[string]interface{}); ok {
			if tools, ok := options["tools"].([]interface{}); ok && len(tools) > 0 {
				openaiReq["tools"] = tools
				toolsSet = true
			}
		}
	}
	if toolsSet {
		// tool_choice: пропускаем только если клиент явно задал; иначе — "auto".
		// Без этого Gemma-4 и аналогичные модели без нативного tool support
		// не понимают, что нужно вызвать tool, и возвращают текст.
		if toolChoice, ok := ollamaReq["tool_choice"]; ok {
			openaiReq["tool_choice"] = toolChoice
		} else {
			openaiReq["tool_choice"] = "auto"
		}
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

// translateOllamaGenerateToOpenAI — маппит Ollama /api/generate на OpenAI /v1/completions.
// Ollama использует prompt + options. OpenAI — prompt + model.
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

// isStreamingFromBody — определяет, является ли запрос streaming, по полю stream в теле.
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

// stripStreamFlag — принудительно отключает streaming в теле запроса.
//
// R60.5 (2026-09-07) fix: skip для management endpoints (load/unload/delete/copy/show).
// cppworker использует strict JSON decoder (DisallowUnknownFields, см.
// pkg/types/contract_validation.go) и отвергает поле "stream" в /api/models/load
// (и других management endpoints) с 400 "unknown field 'stream'". До R60.5 это
// не проявлялось потому что webui напрямую ходил к cppworker'у, минуя balancer.
// После R60.4 webui ходит через balancer proxy для всех /api/* — баг стал видимым.
//
// Список management endpoints (cppworker не принимает "stream" в них):
//   - /api/models/load
//   - /api/models/load-with-params
//   - /api/models/unload
//   - /api/models/delete
//   - /api/copy
//   - /api/delete
//   - /api/pull
//   - /api/push
//   - /api/create
//   - /api/blobs/* (если есть)
func stripStreamFlag(body []byte) []byte {
	return stripStreamFlagForPath(body, "")
}

// stripStreamFlagForPath — версия с явным указанием path.
// Если path — management endpoint, body не модифицируется.
func stripStreamFlagForPath(body []byte, path string) []byte {
	if len(body) == 0 {
		return body
	}
	// Management endpoints не принимают поле "stream" (cppworker strict decoder).
	if isManagementEndpoint(path) {
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

// isManagementEndpoint — возвращает true для путей, которые НЕ принимают
// поле "stream" в теле запроса (cppworker DisallowUnknownFields).
func isManagementEndpoint(path string) bool {
	if path == "" {
		return false
	}
	// Нормализуем trailing slash для сравнения.
	normalized := strings.TrimSuffix(path, "/")
	managementPaths := []string{
		"/api/models/load",
		"/api/models/load-with-params",
		"/api/models/unload",
		"/api/models/delete",
		"/api/copy",
		"/api/delete",
		"/api/pull",
		"/api/push",
		"/api/create",
		"/api/blobs",
	}
	for _, p := range managementPaths {
		if normalized == p || strings.HasPrefix(normalized, p+"/") {
			return true
		}
	}
	return false
}
