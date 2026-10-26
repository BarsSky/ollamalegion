// toolcall_tool_code_r246.go — R246 (2026-10-26): диалект gemma `[tool_code]` на
// границе балансера.
//
// ЖИВОЙ ДЕФЕКТ (DeepSeek Harness → балансер → gemma-4, 25 инструментов):
// модель отдала вызов в теле ответа
//
//	[tool_code]
//	print(read(file_path="go.mod"))
//	[/tool_code]
//
// Ни один детектор балансера такой формы не знал, поэтому вызов уехал клиенту
// как content (message.tool_calls не пришёл). Агент не смог применить инструмент
// и остановился с ошибкой.
//
// Разбор тот же, что в cppworker (tool_code_form_r246.go): блок `[tool_code]` или
// ```tool_code, внутри — питоновский вызов `name(key="value", ...)`, print(…)
// необязателен, вызовов в блоке может быть несколько. Дублирование намеренное:
// SSE-путь балансера отдаёт чанки и без воркера-парсера, и правка только в
// воркере оставила бы дефект живым на этих путях.
package balancer

import (
	"encoding/json"
	"strings"
)

// toolCodeOpenMarkers — открывающие обёртки диалекта.
var toolCodeOpenMarkers = []string{"```tool_code", "```tool call", "[tool_code]"}

// looksLikeToolCodeMarkup — в тексте есть блок tool_code.
func looksLikeToolCodeMarkup(text string) bool {
	for _, m := range toolCodeOpenMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// detectToolCodeCallsInContent — вызовы из блоков tool_code и остаток текста.
func detectToolCodeCallsInContent(content string) (toolCalls []interface{}, remainingContent string, found bool) {
	if !looksLikeToolCodeMarkup(content) {
		return nil, content, false
	}
	var calls []interface{}
	remaining := content
	for {
		start, marker := earliestToolCodeOpen(remaining)
		if start < 0 {
			break
		}
		body := remaining[start+len(marker):]
		end, closeLen := toolCodeCloseFor(body, marker)
		var block string
		if end < 0 {
			block = body
			remaining = remaining[:start]
		} else {
			block = body[:end]
			remaining = remaining[:start] + body[end+closeLen:]
		}
		calls = append(calls, parseToolCodeBlockCalls(block)...)
	}
	if len(calls) == 0 {
		return nil, content, false
	}
	return calls, strings.TrimSpace(remaining), true
}

// earliestToolCodeOpen — позиция самого раннего открывающего маркера.
func earliestToolCodeOpen(text string) (int, string) {
	best, marker := -1, ""
	for _, m := range toolCodeOpenMarkers {
		if i := strings.Index(text, m); i >= 0 && (best < 0 || i < best) {
			best, marker = i, m
		}
	}
	return best, marker
}

// toolCodeCloseFor — закрывающий маркер для конкретной обёртки.
func toolCodeCloseFor(body, openMarker string) (int, int) {
	closer := "[/tool_code]"
	if strings.HasPrefix(openMarker, "```") {
		closer = "```"
	}
	if i := strings.Index(body, closer); i >= 0 {
		return i, len(closer)
	}
	return -1, 0
}

// parseToolCodeBlockCalls — вызовы из тела блока (по одному на строку).
func parseToolCodeBlockCalls(block string) []interface{} {
	var calls []interface{}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "print(") && strings.HasSuffix(line, ")") {
			line = strings.TrimSpace(line[len("print(") : len(line)-1])
		}
		if call := toolCodeCallToOpenAI(line); call != nil {
			calls = append(calls, call)
		}
	}
	return calls
}

// toolCodeCallToOpenAI — `name(key=value, ...)` в форму OpenAI tool_call.
func toolCodeCallToOpenAI(line string) map[string]interface{} {
	open := strings.Index(line, "(")
	if open <= 0 || !strings.HasSuffix(line, ")") {
		return nil
	}
	name := strings.TrimSpace(line[:open])
	if name == "" || strings.ContainsAny(name, " \t\"'=,{}[]") {
		return nil
	}
	args := map[string]interface{}{}
	argSrc := strings.TrimSpace(line[open+1 : len(line)-1])
	if argSrc != "" {
		for _, pair := range splitToolCodeTopLevel(argSrc, ',') {
			eq := toolCodeTopLevelIndex(pair, '=')
			if eq <= 0 {
				continue
			}
			key := strings.Trim(strings.TrimSpace(pair[:eq]), `"'`)
			if key == "" {
				continue
			}
			args[key] = toolCodeValue(strings.TrimSpace(pair[eq+1:]))
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	return map[string]interface{}{
		"id":   "call_" + name,
		"type": "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": string(encoded),
		},
	}
}

// toolCodeValue — литерал питоновского вида в значение Go.
func toolCodeValue(raw string) interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err == nil {
			return s
		}
		return strings.Trim(raw, `"`)
	}
	if strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'") && len(raw) >= 2 {
		return strings.ReplaceAll(raw[1:len(raw)-1], `\'`, `'`)
	}
	switch raw {
	case "True", "true":
		return true
	case "False", "false":
		return false
	case "None", "none", "null":
		return nil
	}
	var num json.Number
	if err := json.Unmarshal([]byte(raw), &num); err == nil {
		return num
	}
	var any interface{}
	if err := json.Unmarshal([]byte(raw), &any); err == nil {
		return any
	}
	return raw
}

// splitToolCodeTopLevel — разбиение по разделителю вне строк и скобок.
func splitToolCodeTopLevel(s string, sep rune) []string {
	var parts []string
	depth := 0
	quote := rune(0)
	start := 0
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
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
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// toolCodeTopLevelIndex — позиция разделителя вне строк и скобок (-1 если нет).
func toolCodeTopLevelIndex(s string, sep byte) int {
	depth := 0
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
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
