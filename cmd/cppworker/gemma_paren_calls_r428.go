package main

// gemma_paren_calls_r428.go — R428 (2026-11-04): НАТИВНЫЙ ВЫЗОВ gemma-4 В ФОРМЕ
// ВЫЗОВА ФУНКЦИИ (`call:NAME(key: value, …)`) распознаётся.
//
// ЖИВОЙ ДЕФЕКТ (снят диагностикой R427 из живой сессии Cline через балансер,
// модель gemma-4-E4B-it). На большом содержимом (index.html целиком) модель
// выдаёт НЕ `name{json}</tool_call>`, а питоновскую запись вызова:
//
//	<|tool_call>call:editor(new_text: "<!-- Cluster Monitor … </html>", path:
//	"d:\\cline-e2e-web\\index.html"}]
//
// Отличия от того, что понимал extractGemmaToolCalls:
//   - КРУГЛЫЕ скобки вокруг аргументов, а не фигурные;
//   - закрывающего тега `<tool_call|>` нет вообще;
//   - аргументы — пары `key: value, key: value` без внешнего JSON-объекта;
//   - в конце мусор от модели: `"}]` вместо `)`.
//
// Итог до фикса: `extractGemmaToolCalls` (регекс `<tool_call>name\{json\}</tool_call>`)
// ничего не находил → tool_calls=0. Если перед вызовом была проза — Cline
// получал её как финальный ответ («COMPLETED», файл не создан); если прозы не
// было — cppworker отдавал `done_reason:error` и ход терялся.
//
// ПОДХОД. Не регекс, а разбор: находим маркер вызова, имя, открывающую скобку,
// затем берём тело «от скобки до конца вызова». Конец определяем терпимо: если
// есть закрывающая `)` вне строк — по ней; иначе (модель её забыла) — до конца
// вывода, с обрезкой хвостового мусора `)`, `}`, `]`. Тело приводим к JSON тем
// же `fixGemmaArgs`, что и остальные gemma-формы (он умеет unquoted-ключи).
//
// ПОЧЕМУ ЭТО НЕ ЛОМАЕТ ЧУЖИЕ ОТВЕТЫ: маркер `<|tool_call>`/`<tool_call>` обязан
// присутствовать (иначе функция молчит — обычный текст с `name(...)` не
// трогается), имя обязано быть непустым идентификатором, а после приведения
// аргументы обязаны быть валидным JSON-объектом.

import (
	"encoding/json"
	"strings"
)

// gemmaParenMaxBody — предохранитель: тело вызова больше этого не разбираем
// (модель с огромным файлом в аргументе даёт десятки килобайт — это нормально,
// но бесконечный мусор после обрыва стрима разбирать не нужно).
const gemmaParenMaxBody = 256 * 1024

// gemmaParenMarkers — маркеры, с которых начинается нативный вызов.
var gemmaParenMarkers = []string{
	"<|tool_call|>", "<|tool_call>", "<tool_call>",
}

// extractGemmaParenToolCalls — вызовы gemma в форме `call:NAME(key: value, …)`.
//
// Возвращает nil, если ни одного такого вызова нет (тогда работают прежние
// стратегии парсера).
func extractGemmaParenToolCalls(output string) []openAIToolCall {
	if output == "" {
		return nil
	}
	if !strings.Contains(output, "call:") && !strings.Contains(output, "(") {
		return nil
	}
	// R432 (2026-11-05): НАТИВНЫЕ спец-токены gemma внутри вызова.
	//
	// ЖИВОЙ ЗАХВАТ (Cline в VS Code + gemma-4, /app/diag/RAW-…-markup_unparsed.txt):
	//
	//	<|channel>thought …<channel|><|tool_call>call:read_files(files:[<|"|>TASK.md<|"|>])
	//
	// Строки в нативном синтаксисе gemma обёрнуты токеном `<|"|>` (в шаблоне это
	// кавычка). Здесь применялась только скобочная форма — токен оставался в
	// аргументах, JSON не собирался, вызов терялся (tool_calls=0). Та же
	// нормализация уже есть в extractGemmaToolCalls (R109) — теперь она общая для
	// обеих форм вызова.
	// R441: нормализуем ТОЛЬКО теги, а токены кавычек `<|"|>` оставляем сканеру
	// нативного тела: он знает, что это РАЗДЕЛИТЕЛЬ строки (внутри могут быть
	// настоящие кавычки). Полная нормализация (normalizeGemmaNativeToolCallSyntax)
	// заменяет их на `"` и делает содержимое файла неразбираемым.
	output = normalizeGemmaTagTokensOnlyR441(output)

	var result []openAIToolCall
	for _, marker := range gemmaParenMarkers {
		from := 0
		for {
			idx := strings.Index(output[from:], marker)
			if idx < 0 {
				break
			}
			start := from + idx + len(marker)
			call, next := parseGemmaParenCallAt(output, start)
			if call != nil {
				result = append(result, *call)
				from = next
				continue
			}
			from = start
		}
	}
	if len(result) == 0 {
		return nil
	}
	normalizeToolCalls(result)
	return result
}

// parseGemmaParenCallAt — разобрать один вызов, начиная с позиции сразу после
// маркера. Возвращает nil, если здесь не вызов, и позицию, с которой продолжать
// поиск.
func parseGemmaParenCallAt(output string, pos int) (*openAIToolCall, int) {
	n := len(output)
	skipSpace := func(i int) int {
		for i < n && (output[i] == ' ' || output[i] == '\t' || output[i] == '\n' || output[i] == '\r') {
			i++
		}
		return i
	}
	pos = skipSpace(pos)
	// Префикс вызова: call: / tool: / function:
	for _, prefix := range []string{"call:", "tool:", "function:"} {
		if strings.HasPrefix(output[pos:], prefix) {
			pos += len(prefix)
			pos = skipSpace(pos)
			break
		}
	}
	// Имя функции.
	nameStart := pos
	for pos < n && (isIdentStart(output[pos]) || (pos > nameStart && isIdentPart(output[pos]))) {
		pos++
	}
	if pos == nameStart {
		return nil, nameStart
	}
	name := output[nameStart:pos]
	pos = skipSpace(pos)
	if pos >= n || output[pos] != '(' {
		return nil, nameStart
	}
	pos++ // за '('
	bodyStart := pos
	// Ищем закрывающую ')' вне строк; если её нет — берём до конца вывода.
	end, closed := findGemmaParenClose(output, bodyStart)
	if !closed {
		end = n
	}
	rawArgs := output[bodyStart:end]
	if len(rawArgs) > gemmaParenMaxBody {
		rawArgs = rawArgs[:gemmaParenMaxBody]
	}
	// R441: нативный разбор тела (разделители <|"|> — это кавычки-разделители,
	// внутри них кавычки литеральны). Прежний путь через JSON ломался на файлах
	// с `lang="ru"` и на путях Windows.
	if nativeArgs, okNative := gemmaNativeArgsToJSONR441(rawArgs); okNative {
		call := &openAIToolCall{
			ID:   "call_" + name,
			Type: "function",
			Function: openAIFunctionCall{
				Name:      name,
				Arguments: nativeArgs,
			},
		}
		next := end
		if closed {
			next = end + 1
		}
		return call, next
	}

	// Запасной путь (нативный сканер не справился): приводим токены кавычек к
	// обычным, как делал R432, и разбираем как JSON-ish.
	rawArgs = strings.ReplaceAll(rawArgs, `<|"|>`, `"`)
	rawArgs = strings.ReplaceAll(rawArgs, `<|'|>`, `"`)

	// Хвостовой мусор модели: `)`/`}`/`]`, кавычки-обрывки и пробелы за
	// последним значением. Значимая часть — пары `key: value`; последний
	// символ валидных аргументов — закрывающая кавычка, число или `}`.
	rawArgs = trimGemmaParenArgs(rawArgs)
	if rawArgs == "" {
		return nil, bodyStart
	}
	argsJSON, ok := gemmaParenArgsToJSON(rawArgs)
	if !ok {
		if !closed {
			return nil, bodyStart
		}
		return nil, end
	}
	call := &openAIToolCall{
		ID:   "call_" + name,
		Type: "function",
		Function: openAIFunctionCall{
			Name:      name,
			Arguments: argsJSON,
		},
	}
	next := end
	if closed {
		next = end + 1
	}
	return call, next
}

// findGemmaParenClose — закрывающая ')' на нулевой вложенности скобок/фигурных
// и вне строк. Строки учитываем по двойным и одинарным кавычкам с экранированием
// обратным слэшем (gemma экранирует `\"` внутри значений).
func findGemmaParenClose(s string, from int) (int, bool) {
	paren, brace, bracket := 0, 0, 0
	inDouble, inSingle, escaped := false, false, false
	for i := from; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
			continue
		case inDouble:
			switch c {
			case '\\':
				escaped = true
			case '"':
				inDouble = false
			}
			continue
		case inSingle:
			switch c {
			case '\\':
				escaped = true
			case '\'':
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '(':
			paren++
		case ')':
			if paren == 0 && brace == 0 && bracket == 0 {
				return i, true
			}
			if paren > 0 {
				paren--
			}
		case '{':
			brace++
		case '}':
			if brace > 0 {
				brace--
			}
		case '[':
			bracket++
		case ']':
			if bracket > 0 {
				bracket--
			}
		}
	}
	return 0, false
}

// trimGemmaParenArgs — снять хвостовой мусор, который модель добавляет после
// последнего значения (`"}]`, `"",}`, `")` и т.п.).
//
// R432 (2026-11-05): режем ТОЛЬКО пока аргументы не станут валидным JSON.
//
// ЖИВОЙ ДЕФЕКТ (Cline в VS Code + gemma-4): нативный вызов с МАССИВОМ в
// аргументах
//
//	call:read_files(files:["TASK.md"])
//
// доезжал сюда как `files:["TASK.md"]`, прежняя версия вслепую снимала хвостовой
// `]` (он же «мусор» в других формах) → `files:["TASK.md"` → JSON невалиден →
// вызов терялся. Теперь `]`/`}`/`)` и запятые снимаются только как хвост, пока
// остаток не станет валидным JSON-объектом; балансовая скобка значения остаётся
// на месте.
func trimGemmaParenArgs(s string) string {
	s = strings.TrimSpace(s)
	for len(s) > 0 {
		if _, ok := gemmaParenArgsToJSON(s); ok {
			break
		}
		last := s[len(s)-1]
		if last == ')' || last == '}' || last == ']' || last == ',' || last == ' ' || last == '\n' || last == '\r' || last == '\t' {
			s = strings.TrimSpace(s[:len(s)-1])
			continue
		}
		break
	}
	return s
}

// gemmaParenArgsToJSON — `key: value, key: value` → JSON-объект.
//
// Обёртку добавляем сами и чиним её fixGemmaArgs (unquoted-ключи). Значения при
// этом уже в JSON-нотации: строки в кавычках с экранированием, числа/true/null
// как есть. Если после починки JSON невалиден или это не объект — вызова нет.
//
// R432 (2026-11-05): строки в ОДИНАРНЫХ кавычках (питоновская запись).
//
// ЖИВОЙ ЗАХВАТ (Qwen3.8-9B + Cline, /app/diag/RAW-…-markup_unparsed.txt):
//
//	<tool_call>read_files(files: ['C:\\Ollama\\…\\TASK.md'])
//
// JSON требует двойных кавычек, поэтому вызов терялся. Теперь второй попыткой
// аргументы конвертируются в JSON-строки (одинарные кавычки → двойные, кавычки
// внутри значения экранируются); первая попытка остаётся прежней — валидный JSON
// не переписываем.
func gemmaParenArgsToJSON(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}", true
	}
	for _, cand := range []string{raw, singleQuotesToJSONR432(raw)} {
		candidate := "{" + cand + "}"
		fixed := fixGemmaArgs(candidate)
		if !json.Valid([]byte(fixed)) {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(fixed), &obj); err != nil {
			continue
		}
		return compactJSON(fixed), true
	}
	return "", false
}

// singleQuotesToJSONR432 — строки в одинарных кавычках переводятся в JSON-строки.
//
// Внутри уже двойных кавычек ничего не меняется (там `'` — обычный символ).
// Экранирование обратным слэшем сохраняется как есть: пути Windows
// (`'C:\\Ollama\\…'`) после конверсии остаются валидными JSON-escape'ами.
func singleQuotesToJSONR432(s string) string {
	if !strings.Contains(s, "'") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	inDouble, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inDouble {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inDouble = false
			}
			continue
		}
		if c == '"' {
			inDouble = true
			b.WriteByte(c)
			continue
		}
		if c != '\'' {
			b.WriteByte(c)
			continue
		}
		// Начало строки в одинарных кавычках → JSON-строка.
		b.WriteByte('"')
		for i++; i < len(s); i++ {
			cc := s[i]
			if cc == '\\' && i+1 < len(s) {
				b.WriteByte(cc)
				b.WriteByte(s[i+1])
				i++
				continue
			}
			if cc == '\'' {
				break
			}
			if cc == '"' {
				b.WriteString(`\"`)
				continue
			}
			b.WriteByte(cc)
		}
		b.WriteByte('"')
	}
	return b.String()
}

// normalizeGemmaTagTokensOnlyR441 — привести ТОЛЬКО обёртки вызова gemma к
// каноничным тегам (`<tool_call>` / `</tool_call>`), не трогая токены кавычек
// `<|"|>`: их разбирает нативный сканер аргументов (R441).
func normalizeGemmaTagTokensOnlyR441(s string) string {
	if s == "" || !strings.Contains(s, "tool_call") {
		return s
	}
	out := strings.NewReplacer(
		"<|tool_call|>", "<tool_call>",
		"<|tool_call>", "<tool_call>",
		"<tool_call|>", "</tool_call>",
		"<|/tool_call|>", "</tool_call>",
		"<|/tool_call>", "</tool_call>",
	).Replace(s)
	for _, prefix := range []string{"call:", "tool:", "function:"} {
		out = strings.ReplaceAll(out, "<tool_call>"+prefix, "<tool_call>")
	}
	return out
}
