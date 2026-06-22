// llamacpp_toolcall_detector.go — Детекция JSON tool_calls в content.
//
// Некоторые модели (например Gemma-4) не имеют встроенной поддержки tool calling
// через chat template. Вместо структурированного поля tool_calls модель выводит
// JSON-представление tool call как обычный текст в content:
//
//	 "[{\"id\":\"call_search\",\"type\":\"function\",\"function\":{\"name\":\"search\",...}}]"
//
// Этот файл содержит функции:
//   - detectAndExtractToolCallsFromContent — основная функция детекции JSON tool calls
//     в content (массив объектов с ключами id/type/function).
//   - extractToolCallsFromSSEContent — обработка SSE chunk для /v1/chat/completions.
//   - tryExtractHermesToolCallsFromContent — Hermes / Qwen 2.5 формат <tool_call>{...}</tool_call>.
//   - tryExtractLlamaPythonTagFromContent — Llama-3 формат <|python_tag|>{...}.
//   - tryExtractMistralToolCallsFromContent — Mistral Nemo формат [TOOL_CALLS][...].
//   - extractAllToolCallsFromContent — объединённая функция, проверяющая все форматы.
//
// Если хотя бы один формат распознан — возвращается массив tool_calls
// и remaining content (для последующей очистки и передачи клиенту).
package balancer

import (
	"encoding/json"
	"regexp"
	"strings"
)

// collapseDuplicateClosingBrace — устраняет артефакт кросс-чанковой склейки,
// когда закрывающая '}' JSON-объекта и начало </tool_call> попадают в разные чанки.
//
// Пример склейки чанков:
//   chunk N:     `{"name":"search","arguments":{"q":"AI news"}}`
//   chunk N+1:   `}</tool_call>`
//   buffer:      `...{"name":"search","arguments":{"q":"AI news"}}}</tool_call>`
//
// Regex в detectHermesToolCallsInContent (`\{.*?\}`) с lazy matching остановится
// на первой '}' и не найдёт </tool_call>. После collapseDuplicateClosingBrace
// буфер становится `...}}</tool_call>`, и parseHermesToOpenAI корректно
// парсит JSON-объект до первой '}' (которая была в chunk N).
func collapseDuplicateClosingBrace(s string) string {
	// Заменяем '}}}</tool_call>' → '}}</tool_call>' и '}}</tool_call>' → '}}</tool_call>'.
	// Идемпотентно: повторный вызов не меняет строку.
	for {
		new := strings.ReplaceAll(s, "}}}", "}}")
		if new == s {
			return s
		}
		s = new
	}
}

// detectAndExtractToolCallsFromContent — детектирует JSON tool calls в content
// и возвращает:
//   - toolCalls: распарсенный массив tool_calls (для помещения в tool_calls поле)
//   - remainingContent: оставшийся текст после удаления tool call JSON (может быть пустым)
//   - found: true если детекция успешна
//
// Модель может выводить tool calls в нескольких форматах:
//
//  1. Массив JSON объектов: [{"id":"call_xxx","type":"function","function":{...}}]
//  2. Массив с суффиксом: [{"id":"call_xxx",...}]</start_of_turn>
//  3. Массив с префиксом: <start_of_turn>[{"id":"call_xxx",...}]
//  4. Hermes / Qwen 2.5: <tool_call>{"name":"search","arguments":{...}}</tool_call>
//  5. Llama-3 python_tag: <|python_tag|>{"name":"search","parameters":{...}}
//  6. Mistral Nemo: [TOOL_CALLS][{"name":"search","arguments":{...}}]
//
// Функция ищет любой из этих форматов в любом месте content.
func detectAndExtractToolCallsFromContent(content string) (toolCalls []interface{}, remainingContent string, found bool) {
	if content == "" {
		return nil, content, false
	}

	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, "", false
	}

	// 1. Массив JSON объектов с полями id/type/function (оригинальный формат).
	calls, remaining, ok := detectStandardArrayToolCalls(trimmed)
	if ok {
		return calls, remaining, true
	}

	// 2. Hermes / Qwen 2.5 формат <tool_call>...</tool_call>.
	if calls, remaining, ok := detectHermesToolCallsInContent(trimmed); ok {
		return calls, remaining, true
	}

	// 3. Llama-3 python_tag формат.
	if calls, remaining, ok := detectLlamaPythonTagInContent(trimmed); ok {
		return calls, remaining, true
	}

	// 4. Mistral Nemo [TOOL_CALLS] формат.
	if calls, remaining, ok := detectMistralToolCallsInContent(trimmed); ok {
		return calls, remaining, true
	}

	// 5. Single-object JSON (одна функция).
	if calls, remaining, ok := detectSingleObjectToolCallInContent(trimmed); ok {
		return calls, remaining, true
	}

	return nil, content, false
}

// detectStandardArrayToolCalls — оригинальная логика детекции JSON-массива
// с объектами, содержащими id/type/function.
func detectStandardArrayToolCalls(trimmed string) (toolCalls []interface{}, remainingContent string, found bool) {
	// Ищем начало JSON массива
	arrayStart := strings.Index(trimmed, "[")
	if arrayStart < 0 {
		return nil, trimmed, false
	}

	// Извлекаем закрывающую позицию ']' с учётом вложенности
	endPos := findMatchingClosingBracket(trimmed, arrayStart)
	if endPos < 0 {
		return nil, trimmed, false
	}

	// Подстрока между '[' и ']' включительно
	potentialArray := trimmed[arrayStart : endPos+1]

	// Пробуем распарсить как JSON массив
	var rawArray []json.RawMessage
	if err := json.Unmarshal([]byte(potentialArray), &rawArray); err != nil {
		return nil, trimmed, false
	}

	if len(rawArray) == 0 {
		return nil, trimmed, false
	}

	// Парсим каждый элемент как tool call и проверяем минимальные поля
	var result []interface{}
	for _, raw := range rawArray {
		var tc struct {
			ID       string          `json:"id"`
			Type     string          `json:"type"`
			Function json.RawMessage `json:"function"`
		}
		if err := json.Unmarshal(raw, &tc); err != nil {
			return nil, trimmed, false
		}
		if tc.ID == "" || tc.Type == "" || len(tc.Function) == 0 {
			return nil, trimmed, false
		}

		var fnMap map[string]interface{}
		if err := json.Unmarshal(tc.Function, &fnMap); err != nil {
			return nil, trimmed, false
		}

		tcMap := map[string]interface{}{
			"id":       tc.ID,
			"type":     "function",
			"function": fnMap,
		}
		result = append(result, tcMap)
	}

	// Всё что до JSON массива
	beforeJSON := strings.TrimSpace(trimmed[:arrayStart])
	// Всё что после JSON массива
	afterJSON := strings.TrimSpace(trimmed[endPos+1:])

	remainingContent = beforeJSON + afterJSON
	if beforeJSON != "" && afterJSON != "" {
		remainingContent = beforeJSON + " " + afterJSON
	}

	return result, remainingContent, true
}

// detectHermesToolCallsInContent — ищет Hermes/Qwen 2.5 формат в content.
// Возвращает распарсенные tool_calls (в OpenAI-формате) и remaining content.
// hermesToolCallRegex — regex для Hermes/Qwen 2.5 формата <tool_call>{...}</tool_call>.
// Допускает дополнительную '}' после закрывающей скобки JSON — это артефакт
// кросс-чанковой склейки, когда '}' и '</tool_call>' приходят в разных SSE-чанках.
// Пример: '...{"q":"x"}}}</tool_call>' должно матчиться как '<tool_call>{"q":"x"}</tool_call>'.
var hermesToolCallRegex = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\}+|\[.*?\]+)\s*</tool_call>`)

func detectHermesToolCallsInContent(content string) (toolCalls []interface{}, remainingContent string, found bool) {
	matches := hermesToolCallRegex.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil, content, false
	}

	var result []interface{}
	for _, m := range matches {
		jsonStr := strings.TrimSpace(m[1])
		// На случай склейки чанков jsonStr может содержать лишнюю '}' в конце —
		// обрезаем её, чтобы json.Unmarshal ниже не упал.
		jsonStr = trimTrailingExtraBrace(jsonStr)
		// Попытка 1: массив tool_calls
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(jsonStr), &arr); err == nil {
			for _, raw := range arr {
				if tc := parseHermesToOpenAI(raw); tc != nil {
					result = append(result, tc)
				}
			}
			continue
		}
		// Попытка 2: одиночный объект
		if tc := parseHermesToOpenAI(json.RawMessage(jsonStr)); tc != nil {
			result = append(result, tc)
		}
	}

	if len(result) == 0 {
		return nil, content, false
	}

	// Удаляем найденные блоки из content
	remaining := hermesToolCallRegex.ReplaceAllString(content, "")
	remaining = strings.TrimSpace(remaining)
	return result, remaining, true
}

// trimTrailingExtraBrace — удаляет лишнюю '}' в конце строки, если она
// приводит к невалидному JSON. Используется для обработки склеенного
// Hermes-формата с двойной '}' перед </tool_call>.
func trimTrailingExtraBrace(jsonStr string) string {
	if !strings.HasSuffix(jsonStr, "}") {
		return jsonStr
	}
	// Пробуем парсить — если ок, ничего не делаем.
	if json.Valid([]byte(jsonStr)) {
		return jsonStr
	}
	// Иначе пробуем обрезать одну '}' с конца.
	trimmed := strings.TrimSuffix(jsonStr, "}")
	if json.Valid([]byte(trimmed)) {
		return trimmed
	}
	return jsonStr
}

// detectLlamaPythonTagInContent — ищет Llama-3 python_tag формат в content.
func detectLlamaPythonTagInContent(content string) (toolCalls []interface{}, remainingContent string, found bool) {
	re := regexp.MustCompile(`<\|python_tag\|>\s*(\{.*|\[.*)`)
	loc := re.FindStringIndex(content)
	if loc == nil {
		return nil, content, false
	}

	// Извлекаем JSON после python_tag до конца матча или до stop-токена.
	// Для упрощения: ищем парную закрывающую скобку после loc[0] + len("<|python_tag|>").
	prefixLen := len("<|python_tag|>")
	jsonStart := loc[0] + prefixLen
	// Пропускаем whitespace
	for jsonStart < len(content) && (content[jsonStart] == ' ' || content[jsonStart] == '\t' || content[jsonStart] == '\n') {
		jsonStart++
	}
	if jsonStart >= len(content) {
		return nil, content, false
	}

	var jsonEnd int
	if content[jsonStart] == '{' {
		jsonEnd = findMatchingClosingBrace(content, jsonStart)
	} else if content[jsonStart] == '[' {
		jsonEnd = findMatchingClosingBracket(content, jsonStart)
	} else {
		return nil, content, false
	}
	if jsonEnd < 0 {
		return nil, content, false
	}

	jsonStr := content[jsonStart : jsonEnd+1]
	// Обрезаем по stop-токенам
	stopTokens := []string{"<|eom_id|>", "<|eot_id|>", "<|end_of_text|>", "<|start_header_id|>"}
	for _, tok := range stopTokens {
		if idx := strings.Index(jsonStr, tok); idx >= 0 {
			jsonStr = jsonStr[:idx]
		}
	}
	jsonStr = strings.TrimSpace(jsonStr)

	var result []interface{}
	// Пробуем как массив
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &arr); err == nil {
		for _, raw := range arr {
			if tc := parseLlamaPythonToOpenAI(raw); tc != nil {
				result = append(result, tc)
			}
		}
	} else if tc := parseLlamaPythonToOpenAI(json.RawMessage(jsonStr)); tc != nil {
		// Пробуем как одиночный объект
		result = append(result, tc)
	}

	if len(result) == 0 {
		return nil, content, false
	}

	// Remaining = всё кроме <|python_tag|>...JSON
	remaining := content[:loc[0]] + content[jsonEnd+1:]
	remaining = strings.TrimSpace(remaining)
	return result, remaining, true
}

// detectMistralToolCallsInContent — ищет Mistral Nemo формат в content.
func detectMistralToolCallsInContent(content string) (toolCalls []interface{}, remainingContent string, found bool) {
	const marker = "[TOOL_CALLS]"
	idx := strings.Index(content, marker)
	if idx < 0 {
		return nil, content, false
	}

	// Найдём конец JSON-массива после [TOOL_CALLS]
	jsonStart := idx + len(marker)
	// Пропускаем whitespace
	for jsonStart < len(content) && (content[jsonStart] == ' ' || content[jsonStart] == '\n' || content[jsonStart] == '\t') {
		jsonStart++
	}
	if jsonStart >= len(content) || content[jsonStart] != '[' {
		return nil, content, false
	}
	jsonEnd := findMatchingClosingBracket(content, jsonStart)
	if jsonEnd < 0 {
		return nil, content, false
	}
	jsonStr := content[jsonStart : jsonEnd+1]
	// Обрезаем по [TOOL_RESULTS] или [INST]
	for _, cut := range []string{"[TOOL_RESULTS]", "[INST]", "</s>"} {
		if cutIdx := strings.Index(jsonStr, cut); cutIdx >= 0 {
			jsonStr = jsonStr[:cutIdx]
		}
	}
	jsonStr = strings.TrimSpace(jsonStr)

	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &arr); err != nil {
		return nil, content, false
	}

	var result []interface{}
	for _, raw := range arr {
		if tc := parseHermesToOpenAI(raw); tc != nil {
			result = append(result, tc)
		}
	}

	if len(result) == 0 {
		return nil, content, false
	}

	remaining := content[:idx] + content[jsonEnd+1:]
	remaining = strings.TrimSpace(remaining)
	return result, remaining, true
}

// detectSingleObjectToolCallInContent — ищет одиночный JSON-объект с признаками tool_call.
func detectSingleObjectToolCallInContent(content string) (toolCalls []interface{}, remainingContent string, found bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "{") {
		return nil, content, false
	}
	endIdx := findMatchingClosingBrace(trimmed, 0)
	if endIdx < 0 {
		return nil, content, false
	}
	candidate := trimmed[:endIdx+1]

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(candidate), &obj); err != nil {
		return nil, content, false
	}

	// Проверяем признаки tool_call: name/function + arguments/parameters
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
		return nil, content, false
	}

	// Конвертируем в OpenAI-формат (id/type/function)
	tc := parseHermesToOpenAI(json.RawMessage(candidate))
	if tc == nil {
		return nil, content, false
	}
	return []interface{}{tc}, "", true
}

// parseHermesToOpenAI конвертирует Hermes/Llama-3/Mistral формат JSON tool_call
// в OpenAI-формат {id, type, function: {name, arguments}}.
func parseHermesToOpenAI(raw json.RawMessage) map[string]interface{} {
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}

	tc := map[string]interface{}{"type": "function"}

	// id (опционально)
	if id, ok := obj["id"].(string); ok && id != "" {
		tc["id"] = id
	} else {
		tc["id"] = "call_" + generateCallID(obj)
	}

	// Имя функции
	name := ""
	if n, ok := obj["name"].(string); ok && n != "" {
		name = n
	} else if fn, ok := obj["function"].(string); ok && fn != "" {
		name = fn
	}
	if name == "" {
		return nil
	}

	// Аргументы
	var args interface{}
	if a, ok := obj["arguments"]; ok && a != nil {
		args = a
	} else if p, ok := obj["parameters"]; ok && p != nil {
		args = p
	} else {
		args = map[string]interface{}{}
	}

	// Сериализуем arguments в JSON-строку для OpenAI-совместимости
	argsJSON, _ := json.Marshal(args)
	tc["function"] = map[string]interface{}{
		"name":      name,
		"arguments": string(argsJSON),
	}
	return tc
}

// parseLlamaPythonToOpenAI конвертирует Llama-3 python_tag tool_call
// (с полем "parameters") в OpenAI-формат.
func parseLlamaPythonToOpenAI(raw json.RawMessage) map[string]interface{} {
	// Используем ту же логику что и Hermes — Llama-3 python_tag отличается только полем parameters,
	// которое мы уже обрабатываем в parseHermesToOpenAI.
	return parseHermesToOpenAI(raw)
}

// generateCallID генерирует стабильный call_id на основе имени функции.
func generateCallID(obj map[string]interface{}) string {
	if name, ok := obj["name"].(string); ok && name != "" {
		return name
	}
	if fn, ok := obj["function"].(string); ok && fn != "" {
		return fn
	}
	return "tool"
}

// findMatchingClosingBrace — находит позицию закрывающей '}' с учётом
// вложенности и строк (включая экранированные кавычки), начиная с позиции openPos.
func findMatchingClosingBrace(s string, openPos int) int {
	if openPos < 0 || openPos >= len(s) || s[openPos] != '{' {
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
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// findMatchingClosingBracket — находит позицию закрывающей ']' с учётом
// вложенности, начиная с позиции openPos.
func findMatchingClosingBracket(s string, openPos int) int {
	if openPos < 0 || openPos >= len(s) || s[openPos] != '[' {
		return -1
	}
	depth := 0
	for i := openPos; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// extractToolCallsFromSSEContent — проверяет SSE chunk на наличие JSON tool calls
// в choices[0].delta.content и, если обнаружены, перемещает их в
// choices[0].delta.tool_calls, очищая content. Возвращает модифицированный JSON
// или nil, если изменений не требуется.
//
// Используется в SSE→SSE passthrough для /v1/chat/completions.
func extractToolCallsFromSSEContent(sseData []byte) []byte {
	var chunk map[string]interface{}
	if err := json.Unmarshal(sseData, &chunk); err != nil {
		return nil
	}

	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return nil
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return nil
	}

	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return nil
	}

	content, ok := delta["content"].(string)
	if !ok || content == "" {
		return nil
	}

	// Проверяем, есть ли JSON tool calls в content
	toolCalls, _, found := detectAndExtractToolCallsFromContent(content)
	if !found || len(toolCalls) == 0 {
		return nil
	}

	// Модифицируем delta: удаляем content, добавляем tool_calls
	// remaining (сервисные токены) не возвращаем — они будут отфильтрованы
	// filterOpenAIStreamingLine в llamacpp_content_filter.go
	delete(delta, "content")
	delta["tool_calls"] = toolCalls

	modified, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}

	return modified
}
