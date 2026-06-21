// tool_calls.go — Parsing tool_calls from model output and building
// properly structured OpenAI-compatible responses with finish_reason="tool_calls".
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// parseToolCallsFromOutput пытается распарсить tool_calls из plain text выхода модели.
//
// Современные модели (Llama 3.1+, Qwen 2.5, Mistral Nemo) генерируют tool_calls
// в JSON-формате. Модели могут выводить их как:
//
//  1. Чистый JSON-массив в конце ответа:
//     Some reasoning text...
//     [{"id":"call_abc","type":"function","function":{"name":"search","arguments":"{\"q\":\"test\"}"}}]
//
//  2. JSON внутри ```json ... ``` блока:
//     ```json
//     [{"id":"call_abc","type":"function","function":{"name":"search","arguments":"{\"q\":\"test\"}"}}]
//     ```
//
//  3. Чистый JSON-массив целиком (весь output — это tool_calls).
func parseToolCallsFromOutput(output string) []openAIToolCall {
	if output == "" {
		return nil
	}

	// Стратегия 1: Проверить, не является ли весь output JSON-массивом tool_calls
	var calls []openAIToolCall
	if err := json.Unmarshal([]byte(output), &calls); err == nil && len(calls) > 0 {
		if normalizeToolCalls(calls) {
			return calls
		}
	}

	// Стратегия 2: Попробовать извлечь JSON из ```json ... ``` блока
	re := regexp.MustCompile("```json\n?(.*?)\n?```")
	if matches := re.FindStringSubmatch(output); len(matches) > 1 {
		jsonStr := strings.TrimSpace(matches[1])
		if err := json.Unmarshal([]byte(jsonStr), &calls); err == nil && len(calls) > 0 {
			if normalizeToolCalls(calls) {
				return calls
			}
		}
	}

	// Стратегия 3: Попробовать извлечь JSON-массив в конце строки
	idx := strings.LastIndex(output, "\n[")
	if idx < 0 {
		idx = strings.Index(output, "[")
	}
	if idx >= 0 {
		jsonCandidate := output[idx:]
		if err := json.Unmarshal([]byte(jsonCandidate), &calls); err == nil && len(calls) > 0 {
			if normalizeToolCalls(calls) {
				return calls
			}
		}
	}

	return nil
}

// normalizeToolCalls проверяет и нормализует tool_calls:
// - присваивает ID, если отсутствует
// - проверяет, что type="function" и function.name не пуст
func normalizeToolCalls(calls []openAIToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for i := range calls {
		if calls[i].Type == "" {
			calls[i].Type = "function"
		}
		if calls[i].Type != "function" {
			return false
		}
		if calls[i].Function.Name == "" {
			return false
		}
		if calls[i].ID == "" {
			// OpenWebUI ожидает call_xxx формат
			calls[i].ID = "call_" + calls[i].Function.Name
		}
		// Аргументы должны быть JSON-строкой
		if calls[i].Function.Arguments != "" {
			if !json.Valid([]byte(calls[i].Function.Arguments)) {
				// Если arguments — это объект, а не строка, сериализуем
				var argsObj interface{}
				if err := json.Unmarshal([]byte(calls[i].Function.Arguments), &argsObj); err == nil {
					// Уже валидный JSON
				} else {
					// Пробуем экранировать
					escaped, err := json.Marshal(calls[i].Function.Arguments)
					if err == nil {
						calls[i].Function.Arguments = string(escaped)
					}
				}
			}
		}
	}
	return true
}

// buildToolsSystemPrompt создаёт system prompt augmentation на основе определений инструментов.
// Этот prompt добавляется к существующему system prompt, чтобы модель знала о доступных инструментах.
func buildToolsSystemPrompt(tools []openAITool) string {
	if len(tools) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("You have access to the following tools:\n\n")

	for _, tool := range tools {
		if tool.Type != "function" {
			continue
		}
		sb.WriteString("## ")
		sb.WriteString(tool.Function.Name)
		sb.WriteString("\n")
		if tool.Function.Description != "" {
			sb.WriteString("Description: ")
			sb.WriteString(tool.Function.Description)
			sb.WriteString("\n")
		}
		if len(tool.Function.Parameters) > 0 {
			// Пробуем красиво отформатировать параметры
			var params map[string]interface{}
			if err := json.Unmarshal(tool.Function.Parameters, &params); err == nil {
				if props, ok := params["properties"].(map[string]interface{}); ok {
					sb.WriteString("Parameters:\n")
					for name, prop := range props {
						propMap, _ := prop.(map[string]interface{})
						propType, _ := propMap["type"].(string)
						propDesc, _ := propMap["description"].(string)
						sb.WriteString("  - ")
						sb.WriteString(name)
						sb.WriteString(" (")
						sb.WriteString(propType)
						if propDesc != "" {
							sb.WriteString("): ")
							sb.WriteString(propDesc)
						} else {
							sb.WriteString(")")
						}
						sb.WriteString("\n")
					}
				}
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("To call a tool, respond with a JSON array of tool calls in the following format:\n")
	sb.WriteString(`[{"id":"call_<tool_name>","type":"function","function":{"name":"<tool_name>","arguments":"<JSON-encoded arguments>"}}]`)
	sb.WriteString("\n\nIf you need to call multiple tools, include multiple objects in the array.")
	sb.WriteString(" Do NOT explain what you are doing — just output the JSON array of tool calls.")

	return sb.String()
}
