// llamacpp_translate_req.go — Translation of Ollama API requests to OpenAI-compatible format
// for llama.cpp/cppworker backends.
package balancer

import (
	"encoding/json"
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
	if tools, ok := ollamaReq["tools"].([]interface{}); ok {
		openaiReq["tools"] = tools
	}
	if toolChoice, ok := ollamaReq["tool_choice"]; ok {
		openaiReq["tool_choice"] = toolChoice
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
