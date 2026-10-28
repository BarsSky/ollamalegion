// tool_args_unwrap_r249.go — R249 (2026-10-28): жёсткая разборка аргументов
// инструмента, которые модель завернула в строковое поле.
//
// ЖИВОЙ ДЕФЕКТ (gemma-4, задача «создай файл» через инструмент write; 6 прогонов
// при temperature=0 — все 6 битые, ответ детерминированный):
//
//	{"content":"{\"content\": \"#include <iostream>\n...\n}\";\n\", \"path\": \"C:/work/hello.cpp\"}"}
//
// Верхний уровень содержит только `content`, обязательный `path` «пропал»,
// внутри строки лежит почти-JSON с ЛИШНЕЙ `;` перед закрывающей кавычкой, а в
// теле C++ кода есть неэкранированные кавычки. clients/DSH такой вызов применить
// не могут — «модель не редактирует файлы».
//
// Обычный ремонт (R186: null-drop, алиасы, коэрция; R248: salvageGarbledArgs)
// такое не лечит, потому что:
//   - вложенный JSON не парсится (лишняя `;`);
//   - после снятия экранирования в content появляются настоящие кавычки задачи,
//     и JSON снова невалиден.
//
// Здесь — узкая, но детерминированная разборка: находим позиции ключей схемы в
// строке и берём значение как текст ДО следующего ключа (а не до ближайшей
// кавычки). Тогда кавычки внутри content безопасны, а `path` в конце строки
// извлекается точно.
package balancer

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// fieldMarker — позиция ключа вида "name": внутри грязной строки.
type fieldMarker struct {
	name  string
	start int // индекс начала кавычки имени ключа
	value int // индекс сразу после ':' (и пробелов)
}

// extractFieldsFromGarbled вынимает значения обязательных полей схемы из
// «почти-JSON» строки. Порядок берётся по появлению ключей в тексте.
func extractFieldsFromGarbled(text string, fields []string) map[string]string {
	var marks []fieldMarker
	for _, name := range fields {
		// Ключ может быть экранирован (строка-контейнер): `\"path\":`.
		pat := regexp.MustCompile(`(?:^|[{\s,])\\?"?` + regexp.QuoteMeta(name) + `\\?"?\s*:\s*`)
		if loc := pat.FindStringIndex(text); loc != nil {
			marks = append(marks, fieldMarker{name: name, start: loc[0], value: loc[1]})
		}
	}
	if len(marks) == 0 {
		return nil
	}
	sort.Slice(marks, func(i, j int) bool { return marks[i].value < marks[j].value })

	out := map[string]string{}
	for i, m := range marks {
		end := len(text)
		if i+1 < len(marks) {
			end = marks[i+1].start
		} else if k := strings.LastIndex(text[end:], "}"); k >= 0 {
			end = end + k
		}
		raw := strings.TrimSpace(text[m.value:end])
		raw = strings.TrimSpace(strings.TrimSuffix(raw, ","))
		// Снимаем закрывающую кавычку и мусорную ';' ПЕРЕД ней (живой случай:
		// `...return 0;\n}\";\n\"` — точка с запятой после закрывающей кавычки).
		for {
			before := raw
			raw = strings.TrimSpace(raw)
			raw = strings.TrimSuffix(raw, ";")
			raw = strings.TrimSuffix(raw, "}")
			raw = strings.TrimSuffix(raw, "]")
			raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(raw, `"`)), `\"`))
			if raw == before {
				break
			}
		}
		raw = strings.TrimPrefix(strings.TrimPrefix(raw, `\"`), `"`)
		val := unescapeArgText(raw)
		// После снятия экранирования убираем хвостовую точку с запятой: в живом
		// payload она стоит внутри значения (перед закрывающей кавычкой).
		val = strings.TrimSpace(val)
		for strings.HasSuffix(val, ";") {
			val = strings.TrimSpace(strings.TrimSuffix(val, ";"))
		}
		out[m.name] = val
	}
	return out
}

// unescapeArgText снимает один уровень JSON-экранирования с текста значения.
func unescapeArgText(s string) string {
	if !strings.ContainsAny(s, `\`) {
		return s
	}
	replacer := strings.NewReplacer(
		`\"`, `"`,
		`\n`, "\n",
		`\r`, "\r",
		`\t`, "\t",
		`\\`, `\`,
	)
	return replacer.Replace(s)
}

// unwrapFieldWrappedArgs — верхний уровень: единственное строковое поле
// содержит внутри объект с обязательными полями схемы.
//
// Возвращает собранные аргументы и true, если удалось восстановить ВСЕ
// обязательные поля.
func unwrapFieldWrappedArgs(obj map[string]interface{}, schema *toolParamSchema) (map[string]interface{}, bool) {
	if schema == nil || len(schema.Required) < 2 {
		return nil, false
	}
	for _, val := range obj {
		s, ok := val.(string)
		if !ok || !strings.Contains(s, "{") {
			continue
		}
		// Быстрый путь: внутри лежит корректный JSON.
		if start := strings.Index(s, "{"); start >= 0 {
			if end := strings.LastIndex(s, "}"); end > start {
				var nested map[string]interface{}
				if json.Unmarshal([]byte(s[start:end+1]), &nested) == nil {
					if allRequiredPresent(nested, schema) {
						return nested, true
					}
				}
			}
		}
		// Медленный путь: почти-JSON с лишними `;` и кавычками в значениях.
		fields := extractFieldsFromGarbled(s, schema.Required)
		if len(fields) != len(schema.Required) {
			continue
		}
		merged := map[string]interface{}{}
		for k, v := range fields {
			merged[k] = v
		}
		// Поля, которые были на верхнем уровне и не являются контейнером мусора.
		for k, v := range obj {
			if sv, isStr := v.(string); isStr && sv == s {
				continue
			}
			merged[k] = v
		}
		// Короткие поля не должны остаться JSON-мусором: если после разборки
		// path всё ещё похож на объект — контейнер был не тот, отдаём случай
		// следующему ремонту (salvage).
		if !plausibleFieldValues(merged) {
			continue
		}
		if allRequiredPresent(merged, schema) {
			// Значения уже разобраны и очищены: scrub здесь недопустим — он
			// принял бы C++-код с фигурными скобками за «JSON-мусор» и обнулил
			// content (живой прогон: content="" после scrub).
			return merged, true
		}
	}
	return nil, false
}

// plausibleFieldValues — короткие (не «контентные») поля не должны оставаться
// JSON-мусором. Живой случай: после неудачной разборки path выглядел как
// "{\".\"" — значит найденный контейнер был не тот, и разборку надо пропустить.
func plausibleFieldValues(m map[string]interface{}) bool {
	longFields := map[string]bool{
		"content": true, "new_string": true, "old_string": true,
		"text": true, "prompt": true, "message": true, "query": true,
	}
	for k, v := range m {
		s, ok := v.(string)
		if !ok || longFields[k] {
			continue
		}
		trimmed := strings.TrimSpace(s)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") || strings.Contains(s, `\"`) {
			return false
		}
	}
	return true
}

// allRequiredPresent — все обязательные поля схемы есть в объекте.
func allRequiredPresent(m map[string]interface{}, schema *toolParamSchema) bool {
	if schema == nil {
		return false
	}
	for _, f := range schema.Required {
		if _, ok := m[f]; !ok {
			return false
		}
	}
	return len(schema.Required) > 0
}
