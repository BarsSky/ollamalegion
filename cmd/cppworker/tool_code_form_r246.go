// tool_code_form_r246.go — R246 (2026-10-26): диалект gemma <<tool_code>> в
// выводе модели.
//
// ЖИВОЙ ДЕФЕКТ (DeepSeek Harness -> балансер -> cppworker -> gemma-4, 25
// инструментов, потоковый /v1/chat/completions). Вместо нативного
// `<|tool_call>call:read{...}<tool_call|>` модель отдала вызов в теле ответа:
//
//	[tool_code]
//	print(read(file_path="go.mod"))
//	[/tool_code]
//
// Ни один детектор такого не знал (знали `<|tool_call>`, Hermes/JSON, angle-
// форму): вызов уехал клиенту ТЕКСТОМ, message.tool_calls не пришёл, агент
// получил «инструмент не вызван» и остановился с ошибкой.
//
// Формат: gemma печатает вызов как питоновский вызов функции —
//
//	print(name(key="value", ...))
//
// в блоке [tool_code]...[/tool_code] либо в ```tool_code ... ``` фенсе.
// Разбираем оба вида, с print(...) и без, несколько вызовов на блок.
package main

import (
	"encoding/json"
	"strings"
)

// toolCodeBlockMarkers — открывающие обёртки диалекта (длинные раньше коротких).
var toolCodeBlockMarkers = []string{
	"```tool_code", "```tool call", "```tool",
	"[tool_code]",
}

// isToolCodeForm — в выводе есть блоки диалекта tool_code.
func isToolCodeForm(text string) bool {
	for _, m := range toolCodeBlockMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// looksLikeToolCodeMarkup — вывод содержит разметку tool_code (для вырезания из
// видимого текста даже когда вызов не распознан).
func looksLikeToolCodeMarkup(text string) bool {
	return isToolCodeForm(text)
}

// toolCodeBlocks — тела блоков tool_code в порядке появления.
//
// Незакрытый блок (обрыв генерации/стрима) тоже возвращается: лучше собрать
// вызов из неполного блока, чем отдать разметку клиенту как ответ.
func toolCodeBlocks(text string) []string {
	blocks := make([]string, 0, 1)
	rest := text
	for {
		start, marker := earliestToolCodeOpen(rest)
		if start < 0 {
			return blocks
		}
		body := rest[start+len(marker):]
		end, closeLen := toolCodeClose(body, marker)
		if end < 0 {
			blocks = append(blocks, body)
			return blocks
		}
		blocks = append(blocks, body[:end])
		rest = body[end+closeLen:]
	}
}

// earliestToolCodeOpen — позиция самого раннего открывающего маркера.
func earliestToolCodeOpen(text string) (int, string) {
	best, marker := -1, ""
	for _, m := range toolCodeBlockMarkers {
		if i := strings.Index(text, m); i >= 0 && (best < 0 || i < best) {
			best, marker = i, m
		}
	}
	return best, marker
}

// toolCodeClose — позиция и длина закрывающего маркера для конкретного
// открывающего (фенс закрывается фенсом, тег — тегом).
func toolCodeClose(body, openMarker string) (int, int) {
	closers := []string{"[/tool_code]"}
	if strings.HasPrefix(openMarker, "```") {
		closers = []string{"```"}
	}
	for _, m := range closers {
		if i := strings.Index(body, m); i >= 0 {
			return i, len(m)
		}
	}
	return -1, 0
}

// stripToolCodeMarkup — вырезает блоки tool_code из видимого текста.
func stripToolCodeMarkup(text string) string {
	if !isToolCodeForm(text) {
		return text
	}
	cleaned := text
	for {
		start, marker := earliestToolCodeOpen(cleaned)
		if start < 0 {
			break
		}
		head := cleaned[:start]
		body := cleaned[start+len(marker):]
		end, closeLen := toolCodeClose(body, marker)
		if end < 0 {
			// Незакрытый блок — режем до конца: хвост генерации обрезан.
			cleaned = head
			break
		}
		cleaned = head + body[end+closeLen:]
	}
	return strings.TrimSpace(cleaned)
}

// extractToolCodeCalls — вызовы инструментов из блоков tool_code.
func extractToolCodeCalls(output string) []openAIToolCall {
	if !isToolCodeForm(output) {
		return nil
	}
	var calls []openAIToolCall
	for _, block := range toolCodeBlocks(output) {
		calls = append(calls, parseToolCodeBlock(block)...)
	}
	return calls
}

// parseToolCodeBlock — разбор тела блока: по одному вызову на строку.
func parseToolCodeBlock(block string) []openAIToolCall {
	var calls []openAIToolCall
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, call := range parseToolCodeLine(line) {
			calls = append(calls, call)
		}
	}
	return calls
}

// parseToolCodeLine — одна строка вида `print(name(a="x", b=2))` (print опционален).
func parseToolCodeLine(line string) []openAIToolCall {
	trimmed := strings.TrimSpace(line)
	// print(...) — обёртка, снимаем.
	if strings.HasPrefix(trimmed, "print(") && strings.HasSuffix(trimmed, ")") {
		trimmed = strings.TrimSpace(trimmed[len("print(") : len(trimmed)-1])
	}
	// Возможное разделение нескольких вызовов через ";" — редкость, но дешево.
	var out []openAIToolCall
	for _, part := range splitTopLevel(trimmed, ';') {
		if call, ok := parseToolCodeCall(part); ok {
			out = append(out, call)
		}
	}
	return out
}

// parseToolCodeCall — `name(key=value, ...)` -> openAIToolCall.
func parseToolCodeCall(s string) (openAIToolCall, bool) {
	s = strings.TrimSpace(s)
	open := strings.Index(s, "(")
	if open <= 0 || !strings.HasSuffix(s, ")") {
		return openAIToolCall{}, false
	}
	name := strings.TrimSpace(s[:open])
	if name == "" || !isToolName(name) {
		return openAIToolCall{}, false
	}
	argSrc := strings.TrimSpace(s[open+1 : len(s)-1])
	args := map[string]interface{}{}
	if argSrc != "" {
		for _, pair := range splitTopLevel(argSrc, ',') {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			eq := topLevelIndex(pair, '=')
			if eq <= 0 {
				continue
			}
			key := strings.TrimSpace(pair[:eq])
			key = strings.Trim(key, `"'`)
			if key == "" {
				continue
			}
			args[key] = parseToolCodeValue(strings.TrimSpace(pair[eq+1:]))
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return openAIToolCall{}, false
	}
	return openAIToolCall{
		Type: "function",
		Function: openAIFunctionCall{
			Name:      name,
			Arguments: string(encoded),
		},
	}, true
}

// isToolName — имя функции в python-подобной записи.
func isToolName(name string) bool {
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && (r >= '0' && r <= '9' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return name != ""
}

// parseToolCodeValue — литерал питоновского вида в значение Go.
func parseToolCodeValue(raw string) interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Строки: двойные кавычки — как в JSON.
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err == nil {
			return s
		}
		return strings.Trim(raw, `"`)
	}
	// Одинарные кавычки: JSON их не понимает — разворачиваем парные.
	if strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'") && len(raw) >= 2 {
		inner := raw[1 : len(raw)-1]
		inner = strings.ReplaceAll(inner, `\'`, `'`)
		return inner
	}
	switch raw {
	case "True", "true":
		return true
	case "False", "false":
		return false
	case "None", "none", "null", "nil":
		return nil
	}
	var num json.Number
	if err := json.Unmarshal([]byte(raw), &num); err == nil {
		return num
	}
	// Вложенные объекты/массивы в питоновской записи: пробуем как JSON.
	var any interface{}
	if err := json.Unmarshal([]byte(raw), &any); err == nil {
		return any
	}
	return raw
}

// splitTopLevel — разбиение по разделителю вне строк и скобок.
func splitTopLevel(s string, sep rune) []string {
	var parts []string
	depth := 0
	quote := rune(0)
	start := 0
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote && (i == 0 || s[i-1] != '\\') {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '(' || r == '[' || r == '{':
			depth++
		case r == ')' || r == ']' || r == '}':
			if depth > 0 {
				depth--
			}
		case r == sep && depth == 0:
			parts = append(parts, s[start:i])
			start = i + len(string(r))
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// topLevelIndex — позиция разделителя вне строк и скобок (-1 если нет).
func topLevelIndex(s string, sep byte) int {
	depth := 0
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote && (i == 0 || s[i-1] != '\\') {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			if depth > 0 {
				depth--
			}
		case c == sep && depth == 0:
			return i
		}
	}
	return -1
}
