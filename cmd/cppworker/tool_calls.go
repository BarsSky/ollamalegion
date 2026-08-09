// tool_calls.go — Parsing tool_calls from model output and building
// properly structured OpenAI-compatible responses with finish_reason="tool_calls".
package main

import (
	"container/list"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================
// Tools system prompt cache (Шаг 2 — компактный prompt + кеш по fingerprint)
// ============================================================
//
// Проблема (см. docs/remaining-real-plan.md, OpenWebUI tools-flow):
// При работе с OpenWebUI tools каждая итерация диалога добавляет в запрос
// один и тот же набор tool definitions (несколько KB JSON Schema на каждый
// tool). buildToolsSystemPrompt вызывался на КАЖДОМ запросе и пересобирал
// один и тот же текст. При 5-10 tools суммарно prompt занимает 3-5K токенов,
// что:
//  1. Съедает n_ctx, вызывая overflow → reload-loop (см. inference.go:ram-fallback).
//  2. Замедляет inference (длиннее prefill).
//
// Решение:
//  1. Компактный формат: только name + 1 строка description + сигнатура параметров.
//  2. Кеш по fingerprint (SHA1 от JSON сериализованных tools) — если набор
//     tools не изменился между запросами (типичный случай для OpenWebUI),
//     возвращаем уже собранный prompt.
//  3. TTL 30 мин — если tools обновились в OpenWebUI, кеш устареет.
//
// Фишка: fingerprint считается от нормализованного JSON (sorted keys), чтобы
// порядок полей в JSON OpenWebUI не влиял на ключ кеша.
const (
	// toolsPromptCacheTTL — время жизни записи в кеше. OpenWebUI обычно не
	// меняет набор tools в течение сессии, поэтому 30 мин — разумный компромисс
	// между экономией памяти и свежестью.
	toolsPromptCacheTTL = 30 * time.Minute
	// toolsPromptMaxDescriptionChars — лимит на описание одного tool.
	// Большие описания (>300 символов) обрезаются с многоточием — модель всё
	// равно не читает длинные JSDoc, для function calling достаточно сути.
	toolsPromptMaxDescriptionChars = 300
	// toolsPromptMaxTotalChars — жёсткий лимит на размер tools-prompt. Если
	// даже компактный формат превышает этот лимит, дополнительно обрезаем
	// описания пропорционально. Защита от случая, когда OpenWebUI регистрирует
	// 50+ tools.
	toolsPromptMaxTotalChars = 8000
)

// toolsPromptCacheEntry — запись кеша для конкретного fingerprint.
// Round 16 P2 fix (2026-07-30): хранит `key` для O(1) eviction — раньше
// нужен был O(N) scan map при каждом eviction (256 entries → 256 scan).
type toolsPromptCacheEntry struct {
	key       string
	prompt    string
	createdAt time.Time
}

// toolsPromptCacheMaxEntries — лимит LRU-кеша. OpenWebUI часто держит один и
// тот же набор tools на всю сессию (1-3 fingerprint'а), но при разных tooling
// profiles в рамках одной ноды кеш может расти. 256 — разумный предел:
// каждый entry ~1-10 KB → ~1-3 MB максимум при полном заполнении.
//
// Round 6 #3: было sync.Map без лимита — медленная утечка при OpenWebUI с
// динамическими tool sets (каждый чат свои tools → новый fingerprint).
const toolsPromptCacheMaxEntries = 256

// toolsPromptCache — потокобезопасный LRU-кеш для tools prompts.
// TTL + LRU eviction (Round 6 #3). sync.Map.Lock уже не используется —
// единый mutex защищает и map, и list.
//
// Round 16 P2 fix (2026-07-30): key хранится в list element (через
// toolsPromptCacheEntry.key), eviction O(1) вместо O(N) scan map.
// При MaxEntries=256 + 1000 evictions/sec → было 256K comparisons/sec,
// теперь 1000 O(1) lookups. Не критично при низком трафике, но правильнее.
type toolsPromptCacheLRU struct {
	mu    sync.Mutex
	items map[string]*list.Element // fingerprint -> doubly-linked list element
	order *list.List               // front = most-recently-used, back = LRU
}

func newToolsPromptCacheLRU() *toolsPromptCacheLRU {
	return &toolsPromptCacheLRU{
		items: make(map[string]*list.Element),
		order: list.New(),
	}
}

// Get returns cached prompt if present and not expired; updates LRU position.
func (c *toolsPromptCacheLRU) Get(fp string) (string, bool) {
	if fp == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[fp]
	if !ok {
		return "", false
	}
	entry := el.Value.(*toolsPromptCacheEntry)
	if time.Since(entry.createdAt) > toolsPromptCacheTTL {
		// Expired — drop and treat as miss.
		c.order.Remove(el)
		delete(c.items, fp)
		return "", false
	}
	c.order.MoveToFront(el)
	return entry.prompt, true
}

// Put inserts/updates entry, evicting the oldest if over capacity.
func (c *toolsPromptCacheLRU) Put(fp, prompt string) {
	if fp == "" || prompt == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if el, ok := c.items[fp]; ok {
		entry := el.Value.(*toolsPromptCacheEntry)
		entry.prompt = prompt
		entry.createdAt = now
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&toolsPromptCacheEntry{key: fp, prompt: prompt, createdAt: now})
	c.items[fp] = el
	// Evict LRU until under cap. O(1) — key хранится в list element.
	for c.order.Len() > toolsPromptCacheMaxEntries {
		back := c.order.Back()
		if back == nil {
			break
		}
		backEntry := back.Value.(*toolsPromptCacheEntry)
		c.order.Remove(back)
		delete(c.items, backEntry.key)
	}
}

// Delete removes a specific fingerprint (used on expiry).
func (c *toolsPromptCacheLRU) Delete(fp string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[fp]; ok {
		c.order.Remove(el)
		delete(c.items, fp)
	}
}

// toolsPromptCache — singleton LRU-кеш.
var toolsPromptCache = newToolsPromptCacheLRU()

// toolsPromptCacheStats — счётчики попаданий/промахов для отладки.
var (
	toolsPromptCacheHits   atomic.Int64
	toolsPromptCacheMisses atomic.Int64
)

// toolsPromptFingerprint вычисляет стабильный fingerprint для набора tools.
// Используется SHA1 от canonical JSON (sorted keys).
func toolsPromptFingerprint(tools []openAITool) string {
	if len(tools) == 0 {
		return ""
	}
	// Marshal each tool separately to avoid зависимости от порядка полей.
	// Затем склеиваем через null-разделитель и хешируем.
	h := sha1.New()
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		// Marshal Function (без Type — он всегда "function" после фильтра).
		b, err := json.Marshal(t.Function)
		if err != nil {
			// fallback: используем строку (кеш не сработает, но не упадём).
			b = []byte(t.Function.Name)
		}
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// getCachedToolsPrompt возвращает промпт из кеша или пустую строку если miss.
// Также применяет TTL — устаревшие записи удаляются сразу (Round 6 #3 LRU).
func getCachedToolsPrompt(fingerprint string) string {
	if fingerprint == "" {
		return ""
	}
	prompt, _ := toolsPromptCache.Get(fingerprint)
	return prompt
}

// putCachedToolsPrompt сохраняет промпт в кеш с текущей меткой времени.
func putCachedToolsPrompt(fingerprint, prompt string) {
	if fingerprint == "" || prompt == "" {
		return
	}
	toolsPromptCache.Put(fingerprint, prompt)
}

// trimDescription обрезает описание tool до разумной длины.
// Сохраняем целостность слов: режем по ближайшему пробелу/точке.
func trimDescription(desc string, maxChars int) string {
	if desc == "" || maxChars <= 0 {
		return ""
	}
	if len(desc) <= maxChars {
		return desc
	}
	trimmed := desc[:maxChars]
	// Ищем последний пробел для аккуратного обрезания.
	if idx := strings.LastIndexAny(trimmed, " .,:;"); idx > maxChars/2 {
		trimmed = trimmed[:idx]
	}
	return strings.TrimRight(trimmed, " .,:;") + "..."
}

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
	if output == "" {
		return nil
	}

	// Стратегия 1: Hermes / Qwen 2.5 формат <tool_call>...</tool_call>
	if calls := extractHermesToolCalls(output); len(calls) > 0 {
		return calls
	}

	// Стратегия 1b (Round 25 2026-08-06): gemma-4 native формат <tool_call>name{args}</tool_call>.
	// gemma-4 обучена выдавать Hermes-style XML, но с сокращённым JSON:
	// имя функции идёт ПЕРЕД объектом аргументов, а не внутри.
	// Стандартный Hermes-парсер не справляется — добавляем отдельную стратегию.
	if calls := extractGemmaToolCalls(output); len(calls) > 0 {
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

	// Стратегия 4: Чистый JSON-массив целиком (весь output — tool_calls)
	var calls []openAIToolCall
	if err := json.Unmarshal([]byte(output), &calls); err == nil && len(calls) > 0 {
		if normalizeToolCalls(calls) {
			return calls
		}
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
		jsonStr := strings.TrimSpace(matches[1])
		if err := json.Unmarshal([]byte(jsonStr), &calls); err == nil && len(calls) > 0 {
			if normalizeToolCalls(calls) {
				return calls
			}
		}
	}

	// Стратегия 6: JSON-массив в конце строки
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

	// Стратегия 7: Single-object JSON (одна функция)
	if calls := extractSingleObjectToolCall(output); len(calls) > 0 {
		return calls
	}

	return nil
}

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
// id (опционально).
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

	// Имя функции: сначала "name", затем "function" (строка)
	if name, ok := obj["name"].(string); ok && name != "" {
		call.Function.Name = name
	} else if fn, ok := obj["function"].(string); ok && fn != "" {
		call.Function.Name = fn
	}

	// Аргументы: сначала "arguments", затем "parameters"
	if args, ok := obj["arguments"]; ok && args != nil {
		call.Function.Arguments = stringifyArguments(args)
	} else if params, ok := obj["parameters"]; ok && params != nil {
		call.Function.Arguments = stringifyArguments(params)
	}

	if call.Function.Name == "" {
		return nil
	}
	return call
}

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
func extractGemmaToolCalls(output string) []openAIToolCall {
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
		argsJSON := fixGemmaArgs(strings.TrimSpace(m[2]))
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

// stringifyArguments сериализует аргументы в JSON-строку для OpenAI-совместимого формата.
// Если аргументы уже строкой (валидный JSON) — возвращает как есть.
// Если объектом/массивом — сериализует в JSON.
func stringifyArguments(args interface{}) string {
	if args == nil {
		return "{}"
	}
	switch v := args.(type) {
	case string:
		// Если строка уже валидный JSON — возвращаем как есть
		if json.Valid([]byte(v)) {
			return v
		}
		// Иначе оборачиваем в JSON-строку
		b, err := json.Marshal(v)
		if err != nil {
			return "{}"
		}
		return string(b)
	default:
		b, err := json.Marshal(args)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
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
//
// Особенности (Шаг 2 — фикс n_ctx overflow с tools):
//  1. Компактный формат: только имя + обрезанное описание + сигнатура параметров
//     (раньше выводился полный JSON Schema — тысячи токенов на 5-10 tools).
//  2. Кеш по fingerprint: для повторяющихся запросов OpenWebUI (типичный случай)
//     возвращается уже собранный промпт — нет затрат на Marshal/Unmarshal + strings.Builder.
//  3. Лимит на суммарный размер: toolsPromptMaxTotalChars — защита от переполнения n_ctx
//     при большом количестве tools.
//
// Возвращаемая строка стабильно детерминирована для одного и того же набора tools —
// это гарантирует попадание в кеш.
func buildToolsSystemPrompt(tools []openAITool) string {
	if len(tools) == 0 {
		return ""
	}

	// Кеш-проверка: если fingerprint совпадает и запись свежая — возвращаем как есть.
	fp := toolsPromptFingerprint(tools)
	if cached := getCachedToolsPrompt(fp); cached != "" {
		toolsPromptCacheHits.Add(1)
		return cached
	}
	toolsPromptCacheMisses.Add(1)

	// Сначала собираем в буфер без жёсткого ограничения, чтобы понять реальный размер.
	// Если превышает toolsPromptMaxTotalChars — обрезаем описания пропорционально.
	var sb strings.Builder
	sb.WriteString("You have access to tools. Call them via JSON array [TOOL_CALLS] with objects: ")
	sb.WriteString(`{"id":"call_<name>","type":"function","function":{"name":"<name>","arguments":"<json-str>"}}. `)
	sb.WriteString("Output ONLY the JSON array, no prose.\n\n")

	perToolLimit := toolsPromptMaxDescriptionChars
	// Адаптивное уменьшение лимита, если tools слишком много.
	if len(tools) > 5 {
		perToolLimit = toolsPromptMaxTotalChars / len(tools)
		if perToolLimit < 80 {
			perToolLimit = 80 // жёсткий минимум, чтобы сигнатура помещалась
		}
		if perToolLimit > toolsPromptMaxDescriptionChars {
			perToolLimit = toolsPromptMaxDescriptionChars
		}
	}

	for _, tool := range tools {
		if tool.Type != "function" {
			continue
		}
		// Формат: "- name(args): desc"
		sb.WriteString("- ")
		sb.WriteString(tool.Function.Name)
		sb.WriteString("(")
		sig := toolSignature(tool.Function.Parameters)
		sb.WriteString(sig)
		sb.WriteString(")")
		desc := trimDescription(tool.Function.Description, perToolLimit)
		if desc != "" {
			sb.WriteString(": ")
			sb.WriteString(desc)
		}
		sb.WriteString("\n")
	}

	result := sb.String()

	// Пост-проверка: если превышаем жёсткий лимит — обрезаем хвост.
	if len(result) > toolsPromptMaxTotalChars {
		result = result[:toolsPromptMaxTotalChars]
		// Обрезаем по последнему переводу строки, чтобы не разрывать строку tool'а.
		if idx := strings.LastIndex(result, "\n"); idx > toolsPromptMaxTotalChars/2 {
			result = result[:idx]
		}
		result += "\n[truncated]"
	}

	putCachedToolsPrompt(fp, result)
	return result
}

// toolSignature возвращает компактную сигнатуру параметров: "q:str, limit?:int".
// required помечаются без "?", опциональные — с "?".
// Если парсинг не удался — возвращает "..." как fallback (модель попробует угадать).
func toolSignature(paramsRaw json.RawMessage) string {
	if len(paramsRaw) == 0 {
		return ""
	}
	var params map[string]interface{}
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return "..."
	}
	props, _ := params["properties"].(map[string]interface{})
	if len(props) == 0 {
		return ""
	}
	required := map[string]bool{}
	if req, ok := params["required"].([]interface{}); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				required[name] = true
			}
		}
	}
	// Стабильный порядок: сортируем ключи properties.
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	var parts []string
	for _, name := range names {
		propMap, _ := props[name].(map[string]interface{})
		propType, _ := propMap["type"].(string)
		if propType == "" {
			// array, oneOf и т.п. — компактно представляем как "any".
			propType = "any"
		}
		// Компактные алиасы типов.
		switch propType {
		case "string":
			propType = "str"
		case "integer":
			propType = "int"
		case "number":
			propType = "num"
		case "boolean":
			propType = "bool"
		case "array":
			propType = "arr"
		case "object":
			propType = "obj"
		}
		part := name + ":" + propType
		if !required[name] {
			part += "?"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// cleanFinalContent выполняет минимальную очистку текстового ответа модели
// перед отдачей клиенту в финальном NDJSON-чанке /api/chat (streaming, tools!=nil).
//
// Зачем нужно:
//  1. Хвостовые служебные токены: модель иногда добавляет </tool_call>, [TOOL_CALLS],
//     <|python_tag|> и т.п. в конец plain-text ответа (когда модель "думала" про tool calls,
//     но не решила их вызывать — артефакт prompt'а с tool definitions).
//  2. Лишние пробелы / переносы строк, которые портят отображение в OpenWebUI.
//
// Не делаем агрессивной очистки (как cleanContentAfterToolCallExtraction в balancer),
// чтобы не повредить нормальный текст ответа.
//
// Используется ТОЛЬКО для случая, когда parseToolCallsFromOutput вернул пустой
// результат (модель не сгенерировала tool_calls), но text response всё равно
// содержит подозрительные токены.
func cleanFinalContent(output string) string {
	if output == "" {
		return ""
	}
	s := output
	// Удаляем хвостовые служебные токены (могут быть артефактом из prompt с tools).
	// Идём в цикле, потому что их может быть несколько подряд.
	for {
		trimmed := false
		// Прямое вхождение + пробельные символы вокруг.
		for _, tok := range trailingToolTokens {
			if strings.HasSuffix(s, tok) {
				s = strings.TrimSuffix(s, tok)
				trimmed = true
			}
		}
		if !trimmed {
			break
		}
	}
	// Trim whitespace и trailing newlines.
	return strings.TrimSpace(s)
}

// trailingToolTokens — служебные токены, которые модель может эмитить в конце
// plain-text ответа при наличии tools в prompt (артефакты chat template'ов).
var trailingToolTokens = []string{
	"</tool_call>",
	"</tool_call>\n",
	"<|python_tag|>",
	"<|python_tag|>\n",
	"<|eom_id|>",
	"<|eot_id|>",
	"<|end|>",
	"<|end|>\n",
	"[TOOL_CALLS]",
	"[TOOL_CALLS]\n",
	"[TOOL_RESULTS]",
	"[TOOL_RESULTS]\n",
	"<start_of_turn>model\n",
	"<start_of_turn>model",
	"<end_of_turn>",
	"<end_of_turn>\n",
}

// recoverToolCallsByPrefixTrimming — Round 16 (2026-07-30) multi-tool recovery.
//
// Symptom: Qwen3-4B-Instruct и аналогичные instruct-модели при multi-tool
// запросах иногда выдают "почти-валидный" JSON с лишним trailing garbage
// (см. live test TestLiveParser_TrailingBraceBug, 228 байт, последний `}`
// лишний). Чистый json.Unmarshal падает.
//
// Алгоритм: пробуем серию простых фиксов, после каждого — парсим. Фиксы:
//  1. Trim trailing non-JSON символов (`}`, whitespace, etc.)
//  2. Trim trailing `}` после `]` (самый частый случай)
//  3. Strip extra `}` INSIDE string values (Qwen3-4B quirk: model пишет
//     `"arguments":"{...}}"}` вместо `"arguments":"{...}"}` — лишний `}` внутри строки)
//  4. Prefix trimming (last resort: отрезаем по 1 символу с конца)
func recoverToolCallsByPrefixTrimming(output string) []openAIToolCall {
	if len(output) < 4 {
		return nil
	}
	if !strings.ContainsAny(output, "[{") {
		return nil
	}

	// Helper: try to parse the given string as tool_calls.
	tryParse := func(s string) []openAIToolCall {
		s = strings.TrimSpace(s)
		if !strings.ContainsAny(s, "[{") {
			return nil
		}
		var arr []openAIToolCall
		if err := json.Unmarshal([]byte(s), &arr); err == nil && len(arr) > 0 {
			if normalizeToolCalls(arr) {
				return arr
			}
		}
		var single openAIToolCall
		if err := json.Unmarshal([]byte(s), &single); err == nil && single.Function.Name != "" {
			if normalizeToolCalls([]openAIToolCall{single}) {
				return []openAIToolCall{single}
			}
		}
		return nil
	}

	// 1. Trim trailing non-JSON chars (whitespace, stray `}` after `]`).
	candidate := strings.TrimRight(output, " \t\r\n}")
	if calls := tryParse(candidate); len(calls) > 0 {
		return calls
	}

	// 2. Strip extra `}` INSIDE string values. Типичный кейс:
	//    `"arguments":"{\"city\":\"Paris\"}}"}` → `"arguments":"{\"city\":\"Paris\"}\""`
	//    Модель добавляет лишний `}` перед закрывающей кавычкой string value.
	//    Паттерн: `}` followed by `"` followed by `}` — внутри string это лишний brace.
	//    Ищем `}}"` где первая `}` — лишний brace в string, вторая `}` — закрытие объекта.
	//    Простой regex: заменяем `}}` на `}` если после `}}` идёт `,` или `}` или `]`.
	fixed := fixTrailingBraceMisorder(output)
	if fixed != output {
		if calls := tryParse(fixed); len(calls) > 0 {
			return calls
		}
		// Также попробуем после trimRight
		if calls := tryParse(strings.TrimRight(fixed, " \t\r\n}")); len(calls) > 0 {
			return calls
		}
	}

	// 3. Prefix trimming (last resort): отрезаем 1-2 символа с конца.
	for trim := 1; trim <= 4; trim++ {
		if len(output) <= trim {
			break
		}
		if calls := tryParse(output[:len(output)-trim]); len(calls) > 0 {
			return calls
		}
	}
	return nil
}

// fixTrailingBraceMisorder — Round 16 code-review fix (2026-07-30): переименована из
// misleadingly-named `stripExtraBracesInStringValues`. Оригинальное имя предполагало
// работу внутри string values, но функция делает ровно одно: переставляет trailing
// `]}` → `}]` (move `}` before `]`). Никаких "string values" внутри не трогает.
//
// Конкретный баг Qwen3-4B-Instruct (2026-07-30, live multi-tool test):
// Модель ТЕРЯЕТ `}` для закрытия последнего call-объекта и добавляет лишний `}`
// в самом конце. Точные позиции в live input (228 байт):
//   - Промежуточный call (Paris): input `..."}},"` (4 chars между string close и `,`)
//     — ПРАВИЛЬНАЯ структура (`}"` + `}}` close function + close call), НЕ трогаем.
//   - Последний call (London) + array: input `..."}"]}` (4 chars в конце)
//     — НЕПРАВИЛЬНАЯ. Модель забыла close call `}`, потом `]` array close, потом extra `}`.
//     Correct: `..."}"}]` (close function, close call, array close) — нужно ВСТАВИТЬ `}` перед `]`.
//
// Эвристика: одна точечная вставка в самом конце.
// (Идемпотентно: повторный прогон не меняет результат.)
func fixTrailingBraceMisorder(s string) string {
	prev := ""
	for iter := 0; iter < 8 && prev != s; iter++ {
		prev = s
		// Паттерн: trailing `]}` (2 chars) → `}]` (INSERT `}` BEFORE `]`)
		//   Live end:      `..."}"]}` (4 chars: function close, array close, extra `}`)
		//   Correct end:   `..."}"}]` (4 chars: function close, call close, array close)
		// Модель "потеряла" `}` для close call и "вставила" его после `]`.
		// Чиним: перемещаем trailing `}` ПЕРЕД `]`.
		if strings.HasSuffix(s, string([]byte{0x5D, 0x7D})) {
			s = s[:len(s)-2] + string([]byte{0x7D, 0x5D})
		}
	}
	return s
}
