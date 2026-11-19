// tool_calls_hermes.go — R638 (2026-11-19): формат Hermes: extractHermesToolCalls, parseHermesSingleCall, массив как список вызовов.
//
// ЗАЧЕМ ВЫНЕСЕНО. `tool_calls.go` держал 1988 строк: кэш промпта, разбор
// девяти семейств форматов вызова, нормализацию и сборку системного промпта.
// Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// extractHermesToolCalls извлекает tool_calls из Hermes/Qwen 2.5 формата:
//
//	<tool_call>{"name":"search","arguments":{"q":"test"}}</tool_call>
//	<tool_call>{...}</tool_call>
//
// Поддерживает как массив [...], так и одиночный объект {...}.
func extractHermesToolCalls(output string) []openAIToolCall {
	re := regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\}|\[.*?\])\s*</tool_call>`)
	matches := re.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return nil
	}

	var result []openAIToolCall
	for _, m := range matches {
		jsonStr := strings.TrimSpace(m[1])
		// Попытка 1: массив tool_calls
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(jsonStr), &arr); err == nil {
			for _, raw := range arr {
				if call := parseHermesSingleCall(raw); call != nil {
					result = append(result, *call)
				}
			}
			continue
		}
		// Попытка 2: одиночный объект
		if call := parseHermesSingleCall(json.RawMessage(jsonStr)); call != nil {
			result = append(result, *call)
		}
	}

	if len(result) > 0 {
		normalizeToolCalls(result)
		return result
	}
	return nil
}

// parseHermesSingleCall парсит один Hermes-формат tool_call JSON.
// Поддерживает поля: name/function (имя), arguments/parameters (аргументы),
// id (опционально). Также OpenAI-формат с вложенным объектом function:
//
//	{"id":"x","type":"function","function":{"name":"y","arguments":"{}"}}
//
// R66b (2026-09-22): добавлена поддержка вложенного function-объекта — раньше
// parser искал name только на верхнем уровне, что ломало OpenAI-формат.
func parseHermesSingleCall(raw json.RawMessage) *openAIToolCall {
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}

	call := &openAIToolCall{Type: "function"}

	// id (опционально)
	if id, ok := obj["id"].(string); ok && id != "" {
		call.ID = id
	}

	// R66b: если есть вложенный "function"-объект (OpenAI tool_call format),
	// поднимаем name/arguments из него на верхний уровень — дальнейшая логика
	// работает одинаково для обеих схем.
	if fnObj, ok := obj["function"].(map[string]interface{}); ok {
		if name, ok := fnObj["name"].(string); ok && name != "" && call.Function.Name == "" {
			call.Function.Name = name
		}
		if args, ok := fnObj["arguments"]; ok && args != nil && call.Function.Arguments == "" {
			call.Function.Arguments = stringifyArguments(args)
		}
	}

	// Имя функции: сначала "name" (top-level), затем "function" (как строка)
	if call.Function.Name == "" {
		if name, ok := obj["name"].(string); ok && name != "" {
			call.Function.Name = name
		} else if fn, ok := obj["function"].(string); ok && fn != "" {
			call.Function.Name = fn
		}
	}

	// Аргументы: сначала "arguments" (top-level), затем "parameters"
	if call.Function.Arguments == "" {
		if args, ok := obj["arguments"]; ok && args != nil {
			call.Function.Arguments = stringifyArguments(args)
		} else if params, ok := obj["parameters"]; ok && params != nil {
			call.Function.Arguments = stringifyArguments(params)
		}
	}

	if call.Function.Name == "" {
		return nil
	}
	return call
}

// parseRawArrayAsToolCalls — R66b (2026-09-22) helper.
//
// Принимает строку, которая должна быть валидным JSON-массивом tool_call
// объектов (например, `[{...},{...}]`). Анмаршалит поэлементно в
// json.RawMessage и для каждого элемента вызывает parseHermesSingleCall,
// который умеет принимать `arguments` как объект ИЛИ как строку
// (конвертирует в строку через stringifyArguments).
//
// Это замена прямого `json.Unmarshal(&[]openAIToolCall{})` в стратегиях 4/4b/5/6,
// который падал, когда модель эмитит `arguments` как объект, а не как строку
// (что нарушает OpenAI-стандарт, но делают Qwen3-Instruct, некоторые Mistral
// варианты и другие модели, обученные на JSON-mode tools).
func parseRawArrayAsToolCalls(jsonStr string) []openAIToolCall {
	jsonStr = strings.TrimSpace(jsonStr)
	if !strings.HasPrefix(jsonStr, "[") {
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &arr); err != nil {
		return nil
	}
	var result []openAIToolCall
	for _, raw := range arr {
		if call := parseHermesSingleCall(raw); call != nil {
			result = append(result, *call)
		}
	}
	if len(result) > 0 {
		normalizeToolCalls(result)
		return result
	}
	return nil
}
