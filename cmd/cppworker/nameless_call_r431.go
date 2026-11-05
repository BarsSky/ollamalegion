// nameless_call_r431.go — R431 (2026-11-05): вызов инструмента БЕЗ ПОЛЯ ИМЕНИ
// восстанавливается по идентификатору `call_<имя>` и по вложенным аргументам.
//
// ЖИВОЙ ДЕФЕКТ (Cline в VS Code, gemma-4-E4B-it, стенд через балансер). Модель
// выдала вызов в форме Mistral, но БЕЗ `name` и БЕЗ `arguments` — аргументы
// вложены прямо в объект `function`, а имя осталось только в `id`:
//
//	…Начну с чтения файла `TASK.md`.
//	[TOOL_CALLS][{"id":"call_read_files","type":"function",
//	              "function":{"files":["TASK.md"]}}]<channel|>
//
// Разбор до фикса: стратегия Mistral требует `name`, `recoverToolCallsFromText`
// тоже (`toolCallFromJSONObject` смотрит name/tool_call/tool_name/tool) → 0
// вызовов. Итог для клиента: `tool_calls` пуст, `done_reason=stop`, Cline
// показывает ответ как «COMPLETED» с нашим пояснением в теле — файл не создан,
// агентский цикл стоит. Именно это оператор и прислал скриншотом.
//
// ПОЧЕМУ ЭТО БЕЗОПАСНО ВОССТАНОВИТЬ. Имя берётся НЕ из догадки, а из `id`,
// который модель пишет по контракту `call_<имя>` (то же соглашение, что в
// истории OpenAI-вызовов), и принимается ТОЛЬКО если совпало с инструментом,
// объявленным в самом запросе. Аргументы — либо готовый объект `arguments`, либо
// остальные ключи `function` (кроме name/arguments). Дальше по конвейеру всё
// равно работают coerceToolCallsToSchema (приведение по схеме) и
// filterUndeclaredToolCalls (R428): чужой инструмент не выполнится.
package main

import (
	"encoding/json"
	"strings"
)

// namelessCallIDPrefixesR431 — префиксы, которыми модели украшают id вызова.
var namelessCallIDPrefixesR431 = []string{
	"call_", "call-", "call.", "calls_", "functions.", "function_", "tool_", "tool.",
}

// namelessCallFromObjectR431 — вызов из JSON-объекта, у которого ВНУТРИ
// `function` нет `name`.
//
// Поддерживаемые формы:
//
//	{"id":"call_read_files","function":{"files":["TASK.md"]}}
//	{"id":"call_read_files","function":{"arguments":{"files":["TASK.md"]}}}
//	{"function":{"read_files":{"files":["TASK.md"]}}}   — имя как ключ-инструмент
//
// Возвращает nil, если восстановить нечего (нет имени, имя не объявлено, нет
// аргументов) — тогда работают прежние ветки и пояснение клиенту.
func namelessCallFromObjectR431(raw string, declared map[string]bool) *openAIToolCall {
	if len(declared) == 0 || !strings.Contains(raw, `"function"`) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil
	}
	fnRaw, ok := obj["function"]
	if !ok {
		return nil
	}
	var fn map[string]json.RawMessage
	if err := json.Unmarshal(fnRaw, &fn); err != nil {
		return nil
	}
	// Полноценный вызов (name есть) — это не наша форма: его разбирают прежние
	// стратегии, и подменять там имя нельзя.
	if _, hasName := fn["name"]; hasName {
		return nil
	}

	name := ""
	if idRaw, ok := obj["id"]; ok {
		var id string
		if json.Unmarshal(idRaw, &id) == nil {
			name = declaredToolFromCallIDR431(id, declared)
		}
	}

	args := map[string]json.RawMessage{}
	if argRaw, ok := fn["arguments"]; ok {
		var m map[string]json.RawMessage
		if json.Unmarshal(argRaw, &m) == nil {
			args = m
		}
	}
	if len(args) == 0 && name == "" && len(fn) == 1 {
		// {"function":{"read_files":{…}}} — имя инструмента как единственный ключ.
		for k, v := range fn {
			if !declared[k] {
				continue
			}
			var m map[string]json.RawMessage
			if json.Unmarshal(v, &m) == nil {
				name, args = k, m
			}
		}
	}
	if len(args) == 0 {
		// Аргументы вложены прямо в `function`.
		for k, v := range fn {
			if k == "name" || k == "arguments" {
				continue
			}
			args[k] = v
		}
	}
	if name == "" || !declared[name] || len(args) == 0 {
		return nil
	}
	argBytes, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	id := "call_" + name
	if idRaw, ok := obj["id"]; ok {
		var s string
		if json.Unmarshal(idRaw, &s) == nil && strings.TrimSpace(s) != "" {
			id = strings.TrimSpace(s)
		}
	}
	return &openAIToolCall{
		ID:       id,
		Type:     "function",
		Function: openAIFunctionCall{Name: name, Arguments: string(argBytes)},
	}
}

// declaredToolFromCallIDR431 — имя объявленного инструмента из идентификатора
// вызова (`call_read_files` → `read_files`).
//
// Пробуем id как есть, с известными префиксами, и с обрезанным числовым
// суффиксом (`call_editor_1` → `editor`). Возвращает "" если ни один вариант не
// совпал с объявленным инструментом.
func declaredToolFromCallIDR431(id string, declared map[string]bool) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if declared[id] {
		return id
	}
	candidates := []string{}
	trimmed := id
	for _, p := range namelessCallIDPrefixesR431 {
		if strings.HasPrefix(strings.ToLower(trimmed), p) {
			trimmed = trimmed[len(p):]
			candidates = append(candidates, trimmed)
		}
	}
	for _, c := range append([]string{}, candidates...) {
		// Числовой суффикс: call_editor_1, call_editor-2.
		base := strings.TrimRight(c, "0123456789")
		base = strings.TrimRight(base, "_-.")
		if base != "" && base != c {
			candidates = append(candidates, base)
		}
	}
	for _, c := range candidates {
		if declared[c] {
			return c
		}
	}
	return ""
}
