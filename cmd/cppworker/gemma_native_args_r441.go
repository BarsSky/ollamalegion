// gemma_native_args_r441.go — R441 (2026-11-05): аргументы НАТИВНОГО вызова
// gemma разбираются сканером, а не переделкой в JSON.
//
// ЖИВОЙ СЫРОЙ ВЫВОД (Cline в VS Code + gemma-4, снят диагностикой R440):
//
//	<|tool_call>call:editor{new_text:<|"|><!DOCTYPE html><html lang="ru">…</html><|"|>,
//	                       path:<|"|>d:\cline-e2e-web\index.html<|"|>}<tool_call|>
//
// ПОЧЕМУ ПРЕЖНИЙ ПУТЬ ЛОМАЛСЯ. Мы заменяли `<|"|>` на обычную кавычку и пытались
// получить JSON. Но в нативном синтаксисе gemma `<|"|>` — это РАЗДЕЛИТЕЛЬ строки:
// всё между двумя такими токенами и есть значение, даже если внутри стоят
// настоящие `"` (в примере — `lang="ru"`). Плюс путь Windows приходит с
// одинарными обратными слэшами (`\c`, `\i` — недопустимые escape в JSON).
// Итог: JSON невалиден, и аргументы уезжали в один скалярный параметр
// (`new_text` = весь блок), клиент отвечал
// `Invalid input: expected string, received undefined → at new_text`.
//
// КАК ТЕПЕРЬ. Тело вызова разбирается тем же способом, каким его породил шаблон:
//   - пары `ключ: значение` делим по запятым ВНЕ строк-разделителей и вне
//     вложенных `{}`/`[]`;
//   - значение в разделителях — литеральная строка (кавычки внутри не
//     экранируются, JSON-эскейпинг делаем сами);
//   - значение без разделителей: число/true/false/null, строка в одинарных
//     кавычках, вложенный объект/массив (рекурсивно);
//   - ключи могут быть без кавычек (обычное дело у gemma).
//
// Функция НЕ догадывается: если тело не удаётся разобрать в валидный JSON-объект,
// возвращает false, и работают прежние пути (fixGemmaArgs / приведение по схеме).
package main

import (
	"encoding/json"
	"strings"
)

// gemmaQuoteSentinelR441 — временная замена токена `<|"|>`: позволяет отличить
// РАЗДЕЛИТЕЛЬ строки от обычной кавычки внутри значения.
const gemmaQuoteSentinelR441 = "\x01"

// gemmaNativeArgsToJSONR441 — тело нативного вызова → JSON-объект аргументов.
func gemmaNativeArgsToJSONR441(body string) (string, bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "{}", true
	}
	// Уже валидный JSON (модель заэкранировала как надо) — отдаём КАК ЕСТЬ:
	// пересборка меняла бы форматирование без пользы, а «прозрачный» проход для
	// корректного вывода — ожидаемое поведение (см. тест AlreadyValidJSON).
	if json.Valid([]byte(body)) {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(body), &obj); err == nil && len(obj) > 0 {
			return body, true
		}
	}
	// Скобочную обёртку снимаем: разбор идёт по парам, а не по JSON.
	if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
		body = strings.TrimSpace(body[1 : len(body)-1])
	}
	body = strings.ReplaceAll(body, `<|"|>`, gemmaQuoteSentinelR441)
	body = strings.ReplaceAll(body, `<|'|>`, gemmaQuoteSentinelR441)
	body = strings.ReplaceAll(body, `\`+"\n", "\n")

	pairs := splitGemmaPairsR441(body)
	if len(pairs) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		key, value, ok := parseGemmaPairR441(p)
		if !ok {
			return "", false
		}
		parts = append(parts, key+":"+value)
	}
	candidate := "{" + strings.Join(parts, ",") + "}"
	if !json.Valid([]byte(candidate)) {
		return "", false
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(candidate), &obj); err != nil {
		return "", false
	}
	if len(obj) == 0 {
		return "", false
	}
	return compactJSON(candidate), true
}

// splitGemmaPairsR441 — делим на пары по запятым вне строк-разделителей и вне
// вложенных объектов/массивов.
func splitGemmaPairsR441(s string) []string {
	var out []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], gemmaQuoteSentinelR441):
			inQuote = !inQuote
			i += len(gemmaQuoteSentinelR441) - 1
		case inQuote:
			// внутри строки ничего не считаем
		case s[i] == '{' || s[i] == '[':
			depth++
		case s[i] == '}' || s[i] == ']':
			if depth > 0 {
				depth--
			}
		case s[i] == ',' && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if tail := strings.TrimSpace(s[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}

// parseGemmaPairR441 — одна пара `ключ: значение` → JSON-фрагменты
// (`"ключ"` и значение в JSON-нотации).
func parseGemmaPairR441(pair string) (string, string, bool) {
	idx := findGemmaColonR441(pair)
	if idx <= 0 {
		return "", "", false
	}
	key := strings.Trim(strings.TrimSpace(pair[:idx]), `"'`)
	if key == "" {
		return "", "", false
	}
	keyJSON, err := marshalNoHTMLEscape(key)
	if err != nil {
		return "", "", false
	}
	value, ok := gemmaValueToJSONR441(strings.TrimSpace(pair[idx+1:]))
	if !ok {
		return "", "", false
	}
	return string(keyJSON), value, true
}

// findGemmaColonR441 — двоеточие на верхнем уровне (не внутри строки/скобок).
func findGemmaColonR441(s string) int {
	inQuote := false
	depth := 0
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], gemmaQuoteSentinelR441):
			inQuote = !inQuote
			i += len(gemmaQuoteSentinelR441) - 1
		case inQuote:
		case s[i] == '{' || s[i] == '[':
			depth++
		case s[i] == '}' || s[i] == ']':
			if depth > 0 {
				depth--
			}
		case s[i] == ':' && depth == 0:
			return i
		}
	}
	return -1
}

// gemmaValueToJSONR441 — значение в JSON-нотации.
func gemmaValueToJSONR441(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	// Строка в разделителях: кавычки внутри — литеральные символы.
	if strings.HasPrefix(raw, gemmaQuoteSentinelR441) {
		inner := strings.TrimPrefix(raw, gemmaQuoteSentinelR441)
		if end := strings.Index(inner, gemmaQuoteSentinelR441); end >= 0 {
			inner = inner[:end]
		}
		b, err := marshalNoHTMLEscape(inner)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	// Вложенный объект/массив: строки внутри тоже могут быть в разделителях.
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		converted, ok := gemmaNestedToJSONR441(raw)
		if !ok {
			return "", false
		}
		return converted, true
	}
	// Строка в ОБЫЧНЫХ кавычках (JSON-нотация): `"HELLO"`, `"…\n…\"ru\"…"`.
	// Здесь кавычки — часть синтаксиса, экранирование значимо; хвостовой мусор
	// модели (`"}]`) отрезаем по закрывающей кавычке.
	if strings.HasPrefix(raw, `"`) {
		if literal, found := gemmaJSONStringLiteralR441(raw); found {
			var decoded string
			if err := json.Unmarshal([]byte(literal), &decoded); err == nil {
				b, errMarshal := marshalNoHTMLEscape(decoded)
				if errMarshal == nil {
					return string(b), true
				}
			}
			// Недопустимые escape (Windows-путь) — чиним и пробуем снова.
			if repaired, changed := repairInvalidJSONEscapesR439(literal); changed {
				if err := json.Unmarshal([]byte(repaired), &decoded); err == nil {
					b, errMarshal := marshalNoHTMLEscape(decoded)
					if errMarshal == nil {
						return string(b), true
					}
				}
			}
		}
	}
	// Строка в одинарных кавычках (питоновская запись).
	if strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'") && len(raw) >= 2 {
		b, err := marshalNoHTMLEscape(raw[1 : len(raw)-1])
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	// Число / true / false / null / голая строка.
	switch strings.ToLower(raw) {
	case "true", "false", "null":
		return strings.ToLower(raw), true
	}
	var num json.Number
	if err := json.Unmarshal([]byte(raw), &num); err == nil {
		return raw, true
	}
	// Голое значение (путь, имя) — отдаём строкой, а не роняем разбор.
	b, err := marshalNoHTMLEscape(raw)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// gemmaNestedToJSONR441 — вложенный объект/массив: рекурсивно по парам,
// разделители строк превращаются в JSON-строки.
func gemmaNestedToJSONR441(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "[") {
		items := splitGemmaPairsR441(strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]"))
		out := make([]string, 0, len(items))
		for _, it := range items {
			v, ok := gemmaValueToJSONR441(it)
			if !ok {
				return "", false
			}
			out = append(out, v)
		}
		return "[" + strings.Join(out, ",") + "]", true
	}
	if strings.HasPrefix(raw, "{") {
		body := strings.TrimSuffix(strings.TrimPrefix(raw, "{"), "}")
		items := splitGemmaPairsR441(body)
		out := make([]string, 0, len(items))
		for _, it := range items {
			k, v, ok := parseGemmaPairR441(it)
			if !ok {
				return "", false
			}
			out = append(out, k+":"+v)
		}
		return "{" + strings.Join(out, ",") + "}", true
	}
	return "", false
}

// gemmaJSONStringLiteralR441 — взять JSON-литерал строки с начала raw,
// включая хвостовое экранирование, и БЕЗ мусора после закрывающей кавычки
// (`"…"}]` у обрезанного вызова).
func gemmaJSONStringLiteralR441(raw string) (string, bool) {
	if !strings.HasPrefix(raw, `"`) {
		return "", false
	}
	escaped := false
	for i := 1; i < len(raw); i++ {
		c := raw[i]
		if escaped {
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '"':
			return raw[:i+1], true
		}
	}
	return "", false
}
