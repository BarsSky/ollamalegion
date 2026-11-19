// tool_calls.go — R638 (2026-11-19): разбор tool_calls из текста модели: parseToolCallsFromOutput* и «свободный» OpenAI-стиль.
//
// ЗАЧЕМ ВЫНЕСЕНО. `tool_calls.go` держал 1988 строк: кэш промпта, разбор
// девяти семейств форматов вызова, нормализацию и сборку системного промпта.
// Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// parseToolCallsFromOutput пытается распарсить tool_calls из plain text выхода модели.
//
// Поддерживаемые форматы (по приоритету):
//
//  1. Hermes / NousResearch / Qwen 2.5 формат (XML-обёртка):
//     <tool_call>{"name":"search","arguments":{"q":"test"}}</tool_call>
//
//  2. Llama-3.x формат с python_tag:
//     <|python_tag|>{"name":"search","parameters":{"q":"test"}}
//     или [{"name":"search","parameters":{...}}]
//
//  3. Mistral Nemo формат:
//     [TOOL_CALLS][{"name":"search","arguments":{...}}]
//
//  4. Чистый JSON-массив в конце ответа (Ollama-style):
//     Some reasoning text...
//     [{"id":"call_abc","type":"function","function":{"name":"search","arguments":"{\"q\":\"test\"}"}}]
//
//  5. JSON внутри ```json ... ``` блока
//
//  6. JSON-массив в конце строки
//
//  7. Single-object JSON (одна функция, не массив)
//
//  8. JSON-объект с полем "function" (Llama-3 style single)
func parseToolCallsFromOutput(output string) []openAIToolCall {
	return parseToolCallsFromOutputRaw(output)
}

// parseToolCallsFromOutputWithTools — R113 (2026-10-13): разбор вывода с
// приведением arguments к JSON-ОБЪЕКТУ по схеме инструментов из ЗАПРОСА.
//
// Зачем отдельная функция, а не изменение прежней: схема инструментов доступна
// только там, где есть сам запрос (req.Tools). Все клиентские пути (Ollama
// /api/chat, OpenAI /v1/chat/completions, /api/generate) обязаны использовать
// именно её — иначе клиент получает arguments-МАССИВ или ГОЛУЮ СТРОКУ и не
// может выполнить вызов (живой случай: run_commands → ["ls -A"],
// list_files → "c:\\Ollama\\ollamalegion"; Cline в этом случае показывает
// вызов текстом и считает задачу выполненной).
func parseToolCallsFromOutputWithTools(output string, tools []openAITool) []openAIToolCall {
	calls := parseToolCallsFromOutputRaw(output)
	if len(calls) == 0 {
		return nil
	}
	calls = coerceToolCallsToSchema(calls, tools)
	// R428: вызов инструмента, которого клиент НЕ объявлял, выполнить нельзя —
	// известные синонимы записи файла переводим на объявленный инструмент,
	// остальные отбрасываем (см. declared_tool_filter.go).
	return filterUndeclaredToolCalls(calls, tools)
}

func parseToolCallsFromOutputRaw(output string) []openAIToolCall {
	if output == "" {
		return nil
	}

	// R106d (2026-10-12): вызов может лежать внутри markdown-фенса
	// (```json … ```) или фенс может быть незакрыт — живой прогон Cline отдал
	// "```json\n[\n  {\"id\":…" и стратегия 5 (строгий regex с закрывающим ```)
	// его не распознала: вызов ушёл клиенту как текст, message.tool_calls не
	// пришёл вовсе. Снимаем обёртку ДО стратегий (stripJSONFence умеет и
	// незакрытый фенс, и короткое пояснение перед ним).
	if stripped := stripJSONFence(output); stripped != output && strings.TrimSpace(stripped) != "" {
		if calls := parseToolCallsFromOutputRaw(stripped); len(calls) > 0 {
			return calls
		}
	}

	// Стратегия 1: Hermes / Qwen 2.5 формат <tool_call>...</tool_call>
	if calls := extractHermesToolCalls(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 1a (R119 2026-10-15): angle-форма Hermes/Qwen3-Coder —
	// `<tool_call> <function=bash> <parameter=command> pwd && ls -la </parameter>`.
	//
	// ЖИВОЙ ДЕФЕКТ (скриншот оператора, DSH через балансер): JSON в блоке нет
	// вовсе, имя и аргументы несёт разметка `<function=…>`/`<parameter=…>`.
	// Ни один парсер её не знал → вызов уезжал клиенту ТЕКСТОМ, инструмент не
	// исполнялся. Стоит ДО gemma-native: тот ждёт `name{args}` и на
	// `<function=bash>` может распарсить имя как часть разметки.
	if calls := extractHermesAngleToolCalls(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 1c (R246 2026-10-26): диалект gemma `[tool_code]` /
	// ```tool_code — вызов питоновским синтаксисом `print(read(file_path="go.mod"))`.
	//
	// ЖИВОЙ ДЕФЕКТ (DeepSeek Harness → балансер → gemma-4, 25 инструментов):
	// вызов уехал в content, message.tool_calls не пришёл, агент остановился.
	// Стоит ДО gemma-native: тот ищет `name{...}` и на `name(k=v)` вернёт nil,
	// зато JSON-стратегии ниже могли бы принять блок за обычный текст.
	if calls := extractToolCodeCalls(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 1b (Round 25 2026-08-06): gemma-4 native формат <tool_call>name{args}</tool_call>.
	// gemma-4 обучена выдавать Hermes-style XML, но с сокращённым JSON:
	// имя функции идёт ПЕРЕД объектом аргументов, а не внутри.
	// Стандартный Hermes-парсер не справляется — добавляем отдельную стратегию.
	if calls := extractGemmaToolCalls(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 1d (R428 2026-11-04): нативный вызов gemma В ФОРМЕ ВЫЗОВА
	// ФУНКЦИИ — `call:NAME(key: value, …)` в круглых скобках, без закрывающего
	// тега и без JSON-объекта аргументов. Живой захват (R427) из сессии Cline:
	//   <|tool_call>call:editor(new_text: "…</html>", path: "d:\\x\\index.html"}]
	// Прежние стратегии его не видели → tool_calls=0 → Cline «COMPLETED».
	if calls := extractGemmaParenToolCalls(output); len(calls) > 0 {
		return calls
	}
	// Стратегия 2: Llama-3.x формат <|python_tag|>{...}
	if calls := extractLlamaPythonTagCalls(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 3: Mistral Nemo [TOOL_CALLS][...]
	if calls := extractMistralToolCalls(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 3b (R109, 2026-10-13): JSON-форма gemma-4 «tool_call + args».
	//
	// Живой случай (Cline CLI 3.0.70 + gemma-4-E4B-it-Q4_K_M, stream=true,
	// 18 инструментов): модель отдала
	//
	//	[ { "tool_call": "read_files", "args": { "path": "." } } ]
	//
	// Парсер знал пары `name`+`arguments` и `name`+`parameters`, но эту — нет:
	// вызов уехал клиенту как content с done_reason:"stop", Cline показал JSON
	// текстом (hadToolCalls=false), инструмент не вызвался.
	if calls := extractToolCallArgsShapeCalls(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 4: Чистый JSON-массив целиком (весь output — tool_calls).
	//
	// R66b (2026-09-22): некоторые модели (Qwen3-Instruct, Mistral-Nemo) эмитят
	// `arguments` как JSON-объект, а не как JSON-строку (что нарушает OpenAI
	// стандарт). При прямом json.Unmarshal в []openAIToolCall это падает, потому
	// что поле `Function.Arguments` имеет тип `string`. Анмаршалим поэлементно
	// в json.RawMessage и переводим через parseHermesSingleCall — он уже умеет
	// конвертировать object→string через stringifyArguments.
	if calls := parseRawArrayAsToolCalls(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 4b (2026-07-30, multi-tool recovery): модель может выдать
	// "почти-валидный" JSON с trailing garbage — например лишний "}" после
	// закрытия массива: `[{...},{...}]}`. Чистый json.Unmarshal падает.
	// Пробуем найти самый длинный префикс строки, который парсится как
	// валидный JSON, и использовать его. Стратегия 6 ниже тоже попробует,
	// но ищет "[", а этот recovery работает на всю строку.
	if calls := recoverToolCallsByPrefixTrimming(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 5: JSON внутри ```json ... ``` блока
	re := regexp.MustCompile("```json\n?(.*?)\n?```")
	if matches := re.FindStringSubmatch(output); len(matches) > 1 {
		if calls := parseRawArrayAsToolCalls(strings.TrimSpace(matches[1])); len(calls) > 0 {
			return calls
		}
	}

	// Стратегия 6: JSON-массив в конце строки
	idx := strings.LastIndex(output, "\n[")
	if idx < 0 {
		idx = strings.Index(output, "[")
	}
	if idx >= 0 {
		if calls := parseRawArrayAsToolCalls(output[idx:]); len(calls) > 0 {
			return calls
		}
	}

	// Стратегия 7: Single-object JSON (одна функция)
	if calls := extractSingleObjectToolCall(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 8 (R66b, 2026-09-22): «почти-JSON» OpenAI-массив.
	//
	// Реальный кейс (Cline CLI 3.0.63 + Qwen3-Instruct-2507-q4km, 25 tools):
	// модель выдаёт массив tool_calls, но допускает ровно те дефекты JSON,
	// которые ломают любой строгий парсер:
	//   * забывает закрывающую кавычку у значения arguments;
	//   * ставит лишнюю `}` перед `,{` (следующим элементом);
	//   * не экранирует обратные слэши в путях Windows (`\O` вместо `\\O`).
	//
	// Пример (реальный вывод, см. tool_calls_r66b_test.go):
	//   [{"id":"call_editor","type":"function","function":{"name":"editor",
	//     "arguments":"{\"path\":\"C:\\Ollama\\...\",\"new_text\":\"OK\\n\"}}},
	//    {...}]
	//
	// Из-за этого все стратегии выше возвращают nil, tool_calls не находятся,
	// и клиент (Cline) видит tool call как обычный текст → «Model returned
	// empty response» / «модель не вызвала инструмент». Здесь мы вытаскиваем
	// пары name/arguments терпимым сканером и починяем JSON аргументов.
	if calls := parseLooseOpenAIStyleToolCalls(output); len(calls) > 0 {
		return calls
	}

	return nil
}

// ============================================================
// R66b: терпимый разбор OpenAI-подобных tool_calls
// ============================================================

var (
	looseToolNameRe = regexp.MustCompile(`"name"\s*:\s*"([^"\\]{1,256})"`)
	looseToolIDRe   = regexp.MustCompile(`"id"\s*:\s*"([^"\\]{1,128})"`)
)

// parseLooseOpenAIStyleToolCalls — последняя линия обороны парсера.
//
// Работает по принципу «найти name, затем arguments, затем достать первое
// сбалансированное JSON-значение после него, починив типовые дефекты LLM».
// Требует, чтобы в тексте были и "name", и "arguments" — иначе возвращает nil,
// чтобы не превращать в tool_calls обычный текст с JSON в прозе.
func parseLooseOpenAIStyleToolCalls(output string) []openAIToolCall {
	if !strings.Contains(output, `"arguments"`) || !strings.Contains(output, `"name"`) {
		return nil
	}

	// Защита от ложных срабатываний на прозе, в которой просто приведён пример
	// JSON (например «Вот конфигурация: {"name":"test","arguments":{}}»).
	// Принимаем только кандидата, который ВЫГЛЯДИТ как ответ целиком или как
	// хвостовой JSON-блок:
	//   A) весь ответ — это JSON ([...] / {...}), ЛИБО
	//   B) ответ заканчивается на ']' / '}' и содержит "type":"function"
	//      (модель дописала пояснение перед массивом).
	body := strings.TrimSpace(output)
	if !strings.HasPrefix(body, "[") && !strings.HasPrefix(body, "{") {
		trimmed := strings.TrimRight(body, " \t\r\n`")
		if !strings.HasSuffix(trimmed, "]") && !strings.HasSuffix(trimmed, "}") {
			return nil
		}
		if !strings.Contains(trimmed, `"type"`) || !strings.Contains(trimmed, `"function"`) {
			return nil
		}
		if i := strings.LastIndex(trimmed, "["); i >= 0 {
			body = trimmed[i:]
		}
	}
	if !strings.Contains(body, `"function"`) {
		return nil
	}

	nameLocs := looseToolNameRe.FindAllStringSubmatchIndex(body, -1)
	if len(nameLocs) == 0 {
		return nil
	}
	idLocs := looseToolIDRe.FindAllStringSubmatch(body, -1)

	var result []openAIToolCall
	for i, loc := range nameLocs {
		name := body[loc[2]:loc[3]]
		searchFrom := loc[1]
		argRel := strings.Index(body[searchFrom:], `"arguments"`)
		if argRel < 0 {
			continue
		}
		v := searchFrom + argRel + len(`"arguments"`)
		v = skipSpaceColon(body, v)
		if v >= len(body) {
			continue
		}

		var args string
		if body[v] == '"' {
			// Строковая форма: модель кладёт JSON-текст внутрь JSON-строки
			// (иногда забывая закрыть её и/или не экранируя `\`).
			decoded := decodeJSONStringFragment(body[v+1:])
			args = firstCompleteJSONValue(decoded, true)
		} else {
			args = firstCompleteJSONValue(body[v:], true)
		}
		if args == "" {
			continue
		}

		call := openAIToolCall{
			Type: "function",
			Function: openAIFunctionCall{
				Name:      name,
				Arguments: args,
			},
		}
		if i < len(idLocs) {
			call.ID = idLocs[i][1]
		}
		result = append(result, call)
	}

	if len(result) == 0 {
		return nil
	}
	if !normalizeToolCalls(result) {
		return nil
	}
	return result
}

// skipSpaceColon пропускает пробелы и один ':' начиная с позиции i.
func skipSpaceColon(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n' || s[i] == ':') {
		i++
	}
	return i
}

// decodeJSONStringFragment декодирует фрагмент JSON-строки (без обрамляющих
// кавычек) в лучшем виде: понимает \", \\, \/, \b, \f, \n, \r, \t и \uXXXX;
// невалидные escape-последовательности (например \O в путях Windows)
// сохраняются как есть — их потом чинит repairJSONText. Сканирование
// останавливается на первой НЕэкранированной кавычке; если её нет (модель
// забыла закрыть строку) — берётся весь фрагмент.
func decodeJSONStringFragment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			break
		}
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		next := s[i+1]
		switch next {
		case '"':
			b.WriteByte('"')
			i++
		case '\\':
			b.WriteByte('\\')
			i++
		case '/':
			b.WriteByte('/')
			i++
		case 'n':
			b.WriteByte('\n')
			i++
		case 'r':
			b.WriteByte('\r')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'b':
			b.WriteByte('\b')
			i++
		case 'f':
			b.WriteByte('\f')
			i++
		case 'u':
			if i+5 < len(s) {
				var code int
				if _, err := fmt.Sscanf(s[i+2:i+6], "%04x", &code); err == nil {
					b.WriteRune(rune(code))
					i += 5
					continue
				}
			}
			b.WriteByte(c)
		default:
			// Невалидный escape (например "\O") — оставляем как есть,
			// repairJSONText превратит его в "\\O".
			b.WriteByte(c)
		}
	}
	return b.String()
}

// firstCompleteJSONValue возвращает первое синтаксически завершённое
// JSON-значение из текста s (начиная с первого '{', '[' или со строки/числа),
// применив repairJSONText при repair=true. Возвращает компактный JSON-текст
// или "" если ничего не нашли.
func firstCompleteJSONValue(s string, repair bool) string {
	s = strings.TrimLeft(s, " \t\r\n")
	if s == "" {
		return ""
	}
	if s[0] != '{' && s[0] != '[' {
		return ""
	}

	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				cand := s[:i+1]
				if json.Valid([]byte(cand)) {
					return compactJSON(cand)
				}
				if repair {
					if fixed := repairJSONText(cand); fixed != "" && json.Valid([]byte(fixed)) {
						return compactJSON(fixed)
					}
				}
				return ""
			}
		}
	}
	return ""
}

// repairJSONText чинит типовые дефекты JSON, которые генерируют LLM внутри
// arguments, и возвращает валидный компактный JSON (или "" если починить не
// удалось):
//  1. невалидные escape-последовательности в строках (\O, \-, \. и т.п.) →
//     обратный слэш экранируется (\\O). Именно так теряются Windows-пути:
//     JSON.parse у клиента падал на "C:\Ollama".
//  2. сырые управляющие символы (перевод строки, таб) внутри строк → \n, \t.
//  3. незакрытая строка / незакрытые скобки → добавляются закрывающие.
func repairJSONText(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)

	// Стек ожидаемых закрывающих символов.
	var stack []byte
	inString := false

	validEscapes := `"\/bfnrtu`

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch c {
			case '\\':
				if i+1 < len(s) && strings.IndexByte(validEscapes, s[i+1]) >= 0 {
					b.WriteByte(c)
					b.WriteByte(s[i+1])
					i++
					continue
				}
				// Невалидный escape → экранируем сам слэш.
				b.WriteString(`\\`)
				continue
			case '"':
				inString = false
				b.WriteByte(c)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if c < 0x20 {
					fmt.Fprintf(&b, `\u%04x`, c)
				} else {
					b.WriteByte(c)
				}
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			b.WriteByte(c)
		case '{':
			stack = append(stack, '}')
			b.WriteByte(c)
		case '[':
			stack = append(stack, ']')
			b.WriteByte(c)
		case '}', ']':
			if len(stack) > 0 && stack[len(stack)-1] == c {
				stack = stack[:len(stack)-1]
				b.WriteByte(c)
			}
			// Лишняя закрывающая скобка просто отбрасывается.
		default:
			b.WriteByte(c)
		}
	}

	if inString {
		b.WriteByte('"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteByte(stack[i])
	}
	return b.String()
}

// compactJSON — сжать JSON-строку до компактного вида (без лишних пробелов).
// Если компактизация невозможна — возвращает исходный текст.
//
// R439 (2026-11-05): кодирование БЕЗ HTML-эскейпинга. Раньше здесь стоял
// json.Marshal, который превращает `<`, `>` и `&` в `\u003c`, `\u003e`, `\u0026`.
// Для вызова вида `editor(new_text: "<!DOCTYPE html>…")` это раздувало аргументы
// в разы (каждый `<` — шесть символов вместо одного), то есть битое по смыслу
// ничего не несло, но гнало лишние килобайты через балансер и клиент. Для JSON
// `\u003c` и `<` эквивалентны, поэтому клиент видит то же самое.
func compactJSON(s string) string {
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, err := marshalNoHTMLEscape(v)
	if err != nil {
		return s
	}
	return string(out)
}
