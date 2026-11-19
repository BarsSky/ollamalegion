// tool_calls_model_syntax.go — R638 (2026-11-19): синтаксис конкретных семейств: Gemma (включая native-форму) и llama.cpp python-теги.
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

// extractGemmaToolCalls извлекает tool_calls из gemma-4 native формата.
//
// Round 25 (2026-08-06): gemma-4 обучена выводить tool calls в Hermes-style
// XML-обёртке, но с сокращённым JSON внутри: имя функции идёт ПЕРЕД объектом
// аргументов, а не внутри него. Пример:
//
//	<tool_call>list_files{}</tool_call>
//	<tool_call>read_file{"path":"/foo"}</tool_call>
//	<tool_call>write_to_file{"path":"/bar","content":"baz"}</tool_call>
//
// Стандартный Hermes-парсер (Strategy 1) ожидает: <tool_call>{"name":"...","arguments":{...}}</tool_call>
// и для этого формата НЕ срабатывает. Эта функция парсит сокращённый формат.
//
// Round 25 P2 fix: gemma-4-it часто выдаёт "расслабленный" JSON без кавычек
// вокруг ключей (`{path: ".", count: 5}` вместо `{"path": ".", "count": 5}`).
// OpenAI-клиенты (Cline/Roo/openai-python) ожидают СТРОГИЙ JSON в поле
// `arguments` — иначе `json.loads(arguments)` падает. Применяем fixGemmaArgs
// чтобы превратить relaxed JSON в валидный.
//
// Поддерживает как пустые аргументы `name{}`, так и с аргументами `name{json}`.
// normalizeGemmaNativeToolCallSyntax — R109 (2026-10-13): привести НАТИВНЫЙ
// синтаксис tool call от gemma-4 к форме, которую понимает extractGemmaToolCalls
// (`<tool_call>имя{аргументы}</tool_call>`).
//
// Живой формат (Cline CLI 3.0.70 + gemma-4-E4B-it-Q4_K_M, stream=true):
//
//	<|tool_call>call:run_commands{commands:[<|"|>dir<|"|>]}<tool_call|>
//
// Что нормализуется:
//   - теги с пайпами: `<|tool_call>` → `<tool_call>`, `<tool_call|>` → `</tool_call>`
//     (в т.ч. вариант `<|tool_call|>`);
//   - служебные токены кавычек: `<|"|>` → `"`, `<|'|>` → `'`;
//   - префикс роли вызова перед именем функции: `call:`, `tool:`, `function:`.
//
// Аргументы остаются «расслабленным» JS-подобным объектом — их приводит к JSON
// fixGemmaArgs (он для этого и написан).
func normalizeGemmaNativeToolCallSyntax(s string) string {
	if s == "" || !strings.Contains(s, "tool_call") {
		return s
	}
	replacer := strings.NewReplacer(
		"<|tool_call|>", "<tool_call>",
		"<|tool_call>", "<tool_call>",
		"<tool_call|>", "</tool_call>",
		"<|/tool_call|>", "</tool_call>",
		"<|/tool_call>", "</tool_call>",
		`<|"|>`, `"`,
		"<|'|>", "'",
	)
	out := replacer.Replace(s)
	// Префикс вызова идёт сразу после открывающего тега.
	for _, prefix := range []string{"call:", "tool:", "function:"} {
		out = strings.ReplaceAll(out, "<tool_call>"+prefix, "<tool_call>")
	}
	return out
}

// jsonObjectCandidates — все JSON-объекты верхнего уровня в произвольном тексте.
//
// Сканер учитывает строки и экранирование, поэтому `{`/`}` внутри значений не
// сбивают подсчёт вложенности. Нужен там, где формат вызова неизвестен заранее
// (модель кладёт вызов в массив, в текст или в фенс).
func jsonObjectCandidates(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		depth := 0
		inString := false
		escaped := false
		closed := false
		for j := i; j < len(s); j++ {
			c := s[j]
			if inString {
				switch {
				case escaped:
					escaped = false
				case c == '\\':
					escaped = true
				case c == '"':
					inString = false
				}
				continue
			}
			switch c {
			case '"':
				inString = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					out = append(out, s[i:j+1])
					i = j
					closed = true
				}
			}
			if closed {
				break
			}
		}
	}
	return out
}

// extractToolCallArgsShapeCalls — R109 (2026-10-13): распознать вызов в форме
// «явный ключ вызова + аргументы», которую использует gemma-4:
//
//	{"tool_call": "read_files", "args": {"path": "."}}
//	[{"tool_call": "run_commands", "args": {"commands": ["dir"]}}]
//
// Пары `name`+`arguments` и `name`+`parameters` парсер знал, эту — нет.
//
// Требование к объекту намеренно узкое: должен быть И явный ключ вызова
// (`tool_call` / `tool_name` / `tool`), И ключ аргументов. Иначе легко принять за
// вызов обычный JSON из прозы модели или эхо схемы инструмента.
func extractToolCallArgsShapeCalls(output string) []openAIToolCall {
	if output == "" || !strings.Contains(output, "{") {
		return nil
	}
	var result []openAIToolCall
	for _, obj := range jsonObjectCandidates(output) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(obj), &fields); err != nil {
			continue
		}
		name := ""
		for _, key := range []string{"tool_call", "tool_name", "tool"} {
			if raw, ok := fields[key]; ok {
				var s string
				if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
					name = strings.TrimSpace(s)
					break
				}
			}
		}
		if name == "" {
			continue
		}
		var argsRaw json.RawMessage
		for _, key := range []string{"args", "arguments", "parameters", "input"} {
			if raw, ok := fields[key]; ok {
				argsRaw = raw
				break
			}
		}
		if argsRaw == nil {
			continue
		}
		args := strings.TrimSpace(string(argsRaw))
		if args == "" || args == "null" {
			args = "{}"
		}
		// arguments в OpenAI-форме — JSON-СТРОКА, поэтому объект сериализуем.
		if !json.Valid([]byte(args)) || !strings.HasPrefix(args, "{") {
			quoted, err := json.Marshal(args)
			if err != nil {
				continue
			}
			args = string(quoted)
		}
		result = append(result, openAIToolCall{
			Type: "function",
			Function: openAIFunctionCall{
				Name:      name,
				Arguments: args,
			},
		})
	}
	if len(result) == 0 {
		return nil
	}
	if !normalizeToolCalls(result) {
		return nil
	}
	return result
}

func extractGemmaToolCalls(output string) []openAIToolCall {
	// R109 (2026-10-13): сначала приводим НАТИВНЫЙ синтаксис gemma-4 к тому, что
	// понимает регекс ниже. Живой формат (Cline CLI + gemma-4, stream=true):
	//
	//	<|tool_call>call:run_commands{commands:[<|"|>dir<|"|>]}<tool_call|>
	//
	// Отличия от того, что парсер знал раньше: теги с пайпами, префикс `call:`
	// перед именем функции и служебный токен кавычки `<|"|>`. Без нормализации
	// вызов не распознавался и уходил клиенту текстом (Cline: toolCallCount=0).
	// R441: только теги; токены кавычек `<|"|>` оставляем сканеру нативного тела
	// (см. gemma_native_args.go).
	output = normalizeGemmaTagTokensOnlyR441(output)

	// Регекс: <tool_call>имя{json}</tool_call>
	// Имя функции: [a-zA-Z_][a-zA-Z0-9_]* (допускаем подчёркивания и цифры)
	// JSON-объект аргументов: {...} (включая пустой {})
	re := regexp.MustCompile(`(?s)<tool_call>([a-zA-Z_][a-zA-Z0-9_]*)(\{.*?\})</tool_call>`)
	matches := re.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return nil
	}

	var result []openAIToolCall
	for _, m := range matches {
		name := m[1]
		// R441: сначала пробуем НАТИВНЫЙ разбор тела (разделители <|"|>,
		// одинарные кавычки, кавычки-литералы внутри значений). fixGemmaArgs
		// остаётся запасным путём: он чинит unquoted-ключи, но не спасает
		// содержимое файла с кавычками внутри.
		argsJSON, okNative := gemmaNativeArgsToJSONR441(strings.TrimSpace(m[2]))
		if !okNative {
			// Запасной путь: токены кавычек → обычные кавычки, затем fixGemmaArgs.
			legacy := strings.ReplaceAll(m[2], `<|"|>`, `"`)
			legacy = strings.ReplaceAll(legacy, `<|'|>`, `"`)
			argsJSON = fixGemmaArgs(strings.TrimSpace(legacy))
		}
		call := openAIToolCall{
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      name,
				Arguments: argsJSON, // raw JSON string (may be "{}"); fixGemmaArgs guarantees valid JSON
			},
		}
		// Генерируем стабильный ID на основе имени + индекс (Round 25: id валиден для OpenAI)
		call.ID = "call_" + name
		result = append(result, call)
	}

	if len(result) > 0 {
		normalizeToolCalls(result)
		return result
	}
	return nil
}

// fixGemmaArgs превращает "расслабленный" JSON от gemma-4-it (без кавычек
// вокруг ключей) в валидный JSON. Пример:
//
//	{path: ".", count: 5}              → {"path": ".", "count": 5}
//	{path: "/foo", verbose: true}      → {"path": "/foo", "verbose": true}
//	{user: {name: "foo", age: 30}}     → {"user": {"name": "foo", "age": 30}}
//
// gemma-4-it не была обучена строгому JSON — она выдаёт JS/Python-style объекты
// (ключи без кавычек, иногда значения тоже). OpenAI spec требует `arguments`
// быть валидным JSON-string-of-JSON-object, иначе `json.loads(arguments)` у клиента
// падает → "tool call failed" → "Cline hit repeated tool call failures".
//
// Алгоритм:
//  1. Если строка уже валидный JSON — возвращаем как есть.
//  2. State machine: идём по символам, отслеживая "внутри строки" vs "снаружи".
//     Снаружи: находим последовательность identifier-символов, за которой идёт
//     опциональный whitespace и ':' — это unquoted key, оборачиваем в кавычки.
//     Внутри строки: пропускаем всё как есть (чтобы ":" в значениях не трогать).
//
// Числовые/boolean/null значения (5, true, false, null) не трогаем — JSON их
// принимает без кавычек. Строки-значения с одинарными кавычками ('foo')
// оставляем — openai-python обычно их понимает, а ломать regex'ом опасно.
func fixGemmaArgs(s string) string {
	if s == "" {
		return s
	}
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return s
	}
	// Шаг 1: уже валидный JSON? Возвращаем как есть.
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return s
	}

	// Шаг 2: state machine — оборачиваем unquoted keys.
	var b strings.Builder
	b.Grow(len(s) + 16)
	inString := false
	escaped := false
	i := 0
	for i < len(s) {
		c := s[i]
		if inString {
			b.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			i++
			continue
		}
		// Not in string.
		if c == '"' {
			inString = true
			b.WriteByte(c)
			i++
			continue
		}
		// Ищем identifier, за которым идёт ':' — это unquoted key.
		if isIdentStart(c) {
			j := i
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			// Пропускаем whitespace
			k := j
			for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
				k++
			}
			if k < len(s) && s[k] == ':' {
				// Unquoted key: оборачиваем.
				b.WriteByte('"')
				b.WriteString(s[i:j])
				b.WriteByte('"')
				i = j
				continue
			}
			// Не ключ — просто пишем identifier как есть.
			b.WriteString(s[i:j])
			i = j
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// extractLlamaPythonTagCalls извлекает tool_calls из Llama-3.x python_tag формата:
//
//	<|python_tag|>{"name":"search","parameters":{"q":"test"}}
//	<|python_tag|>[{"name":"search","parameters":{...}}, {...}]
//
// Аргументы передаются в поле "parameters" (не arguments).
//
// Bug fix (Round 5 Fix 2): старая версия использовала greedy regex
// `<\|python_tag\|>\s*(\{.*|\[.*)` — захватывала ВСЁ до конца строки, ломая
// много-call-сценарии и случаи когда модель продолжает писать текст после
// JSON. Теперь используем findMatchingClosingBrace для аккуратного
// определения конца JSON-блока (учитывает вложенные {} и строки).
func extractLlamaPythonTagCalls(output string) []openAIToolCall {
	// Ищем позицию <|python_tag|> и берём ПОСЛЕДУЮЩИЙ { или [.
	tagIdx := strings.Index(output, "<|python_tag|>")
	if tagIdx < 0 {
		return nil
	}
	rest := output[tagIdx+len("<|python_tag|>"):]
	rest = strings.TrimLeft(rest, " \t\r\n")

	if len(rest) == 0 || (rest[0] != '{' && rest[0] != '[') {
		return nil
	}

	// Находим matching closing bracket (для массива или объекта).
	// findMatchingClose учитывает вложенность и строки (включая экранирование).
	var closeIdx int
	if rest[0] == '{' {
		closeIdx = findMatchingClosingBrace(rest, 0)
	} else {
		closeIdx = findMatchingClosingBracket(rest, 0)
	}
	if closeIdx < 0 {
		// Fallback: используем старый подход с stop-токенами.
		stopTokens := []string{"<|eom_id|>", "<|eot_id|>", "<|end_of_text|>", "<|start_header_id|>"}
		for _, tok := range stopTokens {
			if idx := strings.Index(rest, tok); idx >= 0 {
				rest = rest[:idx]
				break
			}
		}
		rest = strings.TrimSpace(rest)
	} else {
		rest = rest[:closeIdx+1]
	}

	// Пытаемся распарсить как JSON
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(rest), &raw); err != nil {
		return nil
	}

	var result []openAIToolCall

	// Вариант A: массив
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, item := range arr {
			if call := parseLlamaPythonSingle(item); call != nil {
				result = append(result, *call)
			}
		}
	} else {
		// Вариант B: одиночный объект
		if call := parseLlamaPythonSingle(raw); call != nil {
			result = append(result, *call)
		}
	}

	if len(result) > 0 {
		normalizeToolCalls(result)
		return result
	}
	return nil
}

// parseLlamaPythonSingle парсит один Llama-3 python_tag tool_call.
// Llama-3 использует поле "parameters" вместо "arguments".
func parseLlamaPythonSingle(raw json.RawMessage) *openAIToolCall {
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}

	call := &openAIToolCall{Type: "function"}
	if id, ok := obj["id"].(string); ok && id != "" {
		call.ID = id
	}

	// name → call.Function.Name
	if name, ok := obj["name"].(string); ok && name != "" {
		call.Function.Name = name
	}

	// parameters → call.Function.Arguments (переименовываем в arguments для OpenAI-совместимости)
	if params, ok := obj["parameters"]; ok && params != nil {
		call.Function.Arguments = stringifyArguments(params)
	} else if args, ok := obj["arguments"]; ok && args != nil {
		call.Function.Arguments = stringifyArguments(args)
	}

	if call.Function.Name == "" {
		return nil
	}
	return call
}
