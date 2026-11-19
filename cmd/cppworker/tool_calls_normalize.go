// tool_calls_normalize.go — R638 (2026-11-19): нормализация вызовов: stringifyArguments, marshalNoHTMLEscape, normalizeToolCalls.
//
// ЗАЧЕМ ВЫНЕСЕНО. `tool_calls.go` держал 1988 строк: кэш промпта, разбор
// девяти семейств форматов вызова, нормализацию и сборку системного промпта.
// Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// stringifyArguments сериализует аргументы в JSON-строку для OpenAI-совместимого формата.
// Если аргументы уже строкой (валидный JSON) — возвращает как есть.
// Если объектом/массивом — сериализует в JSON.
//
// R66b (2026-09-22): сериализация идёт через marshalNoHTMLEscape. Раньше
// использовался json.Marshal, который по умолчанию HTML-эскейпит `<`, `>` и `&`
// в `\u003c`/`\u003e`/`\u0026`. Для transport'а это валидный JSON (клиент после
// JSON.parse получает исходный текст), НО:
//   - аргументы tool_call — это прозрачные данные (путь, содержимое файла),
//     а не HTML-фрагмент, поэтому эскейпить их незачем;
//   - «сырой» HTML в arguments читается в логах/отладке и совпадает с тем,
//     что отдаёт настоящий OpenAI API.
func stringifyArguments(args interface{}) string {
	if args == nil {
		return "{}"
	}
	switch v := args.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		// Если строка уже валидный JSON — возвращаем как есть, НО сначала
		// проверяем двойное кодирование.
		if json.Valid([]byte(trimmed)) {
			// R66b: Qwen3-Instruct/Mistral-Nemo часто кладут в arguments
			// JSON-СТРОКУ, содержимое которой — сам JSON-объект:
			//   "arguments":"{\"path\":\"a.txt\"}"
			// После анмаршала outer-JSON это даёт Go-строку
			// `"{\"path\":\"a.txt\"}"` (с кавычками по краям). Если отдать её
			// клиенту как есть, Cline сделает JSON.parse и получит СТРОКУ, а не
			// объект → «tool call без аргументов». Разворачиваем один уровень.
			var inner string
			if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
				it := strings.TrimSpace(inner)
				if (strings.HasPrefix(it, "{") || strings.HasPrefix(it, "[")) && json.Valid([]byte(it)) {
					return compactJSON(it)
				}
			}
			return trimmed
		}
		// R66b: невалидный JSON (незакрытая кавычка, лишняя `}`, неэкранированный
		// `\` в Windows-путях) — пробуем починить, иначе клиент не сможет
		// разобрать аргументы.
		if fixed := repairJSONText(trimmed); fixed != "" && json.Valid([]byte(fixed)) &&
			(strings.HasPrefix(strings.TrimSpace(fixed), "{") || strings.HasPrefix(strings.TrimSpace(fixed), "[")) {
			return compactJSON(fixed)
		}
		// Иначе оборачиваем в JSON-строку
		b, err := marshalNoHTMLEscape(v)
		if err != nil {
			return "{}"
		}
		return string(b)
	default:
		b, err := marshalNoHTMLEscape(args)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
}

// marshalNoHTMLEscape — json.Marshal без HTML-эскейпинга (`<`/`>`/`&` остаются
// литералами). Trailing "\n" от Encoder отрезается. Используется только для
// сериализации arguments tool_call'ов (прозрачные данные).
func marshalNoHTMLEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
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
