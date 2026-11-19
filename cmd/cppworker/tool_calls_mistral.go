// tool_calls_mistral.go — R638 (2026-11-19): формат Mistral и одиночный JSON-объект; поиск парных скобок.
//
// ЗАЧЕМ ВЫНЕСЕНО. `tool_calls.go` держал 1988 строк: кэш промпта, разбор
// девяти семейств форматов вызова, нормализацию и сборку системного промпта.
// Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package main

import (
	"encoding/json"
	"strings"
)

// extractMistralToolCalls извлекает tool_calls из Mistral Nemo формата:
//
//	[TOOL_CALLS][{"name":"search","arguments":{...}}]
//
// Mistral использует [TOOL_CALLS] маркер перед JSON-массивом. [TOOL_RESULTS]
// для результатов — игнорируется при парсинге calls.
func extractMistralToolCalls(output string) []openAIToolCall {
	const marker = "[TOOL_CALLS]"
	idx := strings.Index(output, marker)
	if idx < 0 {
		return nil
	}

	rest := output[idx+len(marker):]
	// Обрезаем по [TOOL_RESULTS] или [INST] маркерам
	for _, cut := range []string{"[TOOL_RESULTS]", "[INST]", "</s>"} {
		if cutIdx := strings.Index(rest, cut); cutIdx >= 0 {
			rest = rest[:cutIdx]
		}
	}
	rest = strings.TrimSpace(rest)

	// Некоторые модели (Qwen3-Instruct, Mistral-Nemo variant) добавляют
	// между маркером и JSON-блоком разделитель " = ":
	//   [TOOL_CALLS] = [{"name":"foo",...}]
	// Без strip этого префикса json.Unmarshal ниже падает на " =", и tool_call
	// теряется (становится текстом в content). Strip'им и пробелы, и сам "=".
	if strings.HasPrefix(rest, "=") {
		rest = strings.TrimSpace(rest[1:])
	}

	// Должно начинаться с [
	if !strings.HasPrefix(rest, "[") {
		return nil
	}

	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(rest), &arr); err != nil {
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

// extractSingleObjectToolCall обрабатывает single-object JSON (не массив):
//
//	{"name":"search","arguments":{"q":"test"}}
//	{"function":"search","arguments":"{\"q\":\"test\"}"}
//
// Оборачивает в массив из одного элемента.
func extractSingleObjectToolCall(output string) []openAIToolCall {
	// Ищем одиночный JSON-объект с признаками tool_call.
	// Признаки: наличие полей name ИЛИ function, И arguments/parameters.
	// Игнорируем массивы — они обрабатываются раньше.
	trimmed := strings.TrimSpace(output)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}

	// Найти парные закрывающие скобки с учётом вложенности и строк.
	endIdx := findMatchingClosingBrace(trimmed, 0)
	if endIdx < 0 {
		return nil
	}
	candidate := trimmed[:endIdx+1]

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(candidate), &obj); err != nil {
		return nil
	}

	// Проверяем признаки tool_call
	hasName := false
	if _, ok := obj["name"].(string); ok {
		hasName = true
	}
	if _, ok := obj["function"].(string); ok {
		hasName = true
	}
	hasArgs := false
	if _, ok := obj["arguments"]; ok {
		hasArgs = true
	}
	if _, ok := obj["parameters"]; ok {
		hasArgs = true
	}

	if !hasName || !hasArgs {
		return nil
	}

	if call := parseHermesSingleCall(json.RawMessage(candidate)); call != nil {
		return []openAIToolCall{*call}
	}
	return nil
}

// findMatchingClose находит позицию closing-символа для opening-символа
// (openCh='{' → closeCh='}', openCh='[' → closeCh=']') с учётом вложенности
// и строк (включая экранированные кавычки). Возвращает индекс закрывающего
// символа или -1 если не найден.
//
// Bug fix (Round 5 Fix 2): старая findMatchingClosingBrace работала только
// для {…}, что не покрывало кейс с массивами tool calls в Llama-3 python_tag.
func findMatchingClose(s string, openPos int, openCh, closeCh byte) int {
	if openPos < 0 || openPos >= len(s) || s[openPos] != openCh {
		return -1
	}
	depth := 0
	inStr := false
	escaped := false
	for i := openPos; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case openCh:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// findMatchingClosingBrace находит позицию закрывающей '}' с учётом
// вложенности и строк (включая экранированные кавычки).
// Backward-compat обёртка вокруг findMatchingClose.
func findMatchingClosingBrace(s string, openPos int) int {
	return findMatchingClose(s, openPos, '{', '}')
}

// findMatchingClosingBracket — аналогично для '[' → ']'.
func findMatchingClosingBracket(s string, openPos int) int {
	return findMatchingClose(s, openPos, '[', ']')
}
