// tool_args_repair_r186.go — R186 (2026-10-20): аргументы вызова инструмента
// приводятся к ОБЪЯВЛЕННОЙ схеме и до, и после того, как балансер САМ собрал
// вызов из текста.
//
// ЖИВОЙ РАЗБОР (Cline). Клиент получает `Error: {"error":"✖ Invalid input"}` и
// повторяет один и тот же вызов, пока не остановится по «6 identical calls».
// Причины две, обе на нашей стороне:
//
//  1. Вызов, собранный балансером из content (модель написала JSON текстом),
//     уходил БЕЗ поля `index` в delta.tool_calls. По спецификации OpenAI
//     `index` — ключ склейки фрагментов вызова; при параллельных вызовах
//     (Cline их прямо просит) клиент склеивает все фрагменты в один и получает
//     битый JSON.
//  2. Собранные из текста аргументы не приводились к схеме инструмента из
//     запроса: cppworker делает это для СВОИХ вызовов
//     (cmd/cppworker/tool_args_schema_r113.go), но путь «текст → tool_calls»
//     живёт в балансере и ту правку не проходил. Модель отдаёт
//     `{"commands":"git status"}` при схеме `commands: array` → валидатор
//     клиента (zod) отвергает вызов: «expected array, received string».
//
// Правило: НИКОГДА не отдавать клиенту вызов, который наш же слой мог привести
// к объявленной схеме. Что не приводится — оставляем как есть (лучше честная
// ошибка инструмента, чем выдуманные аргументы), но пишем в лог.
package balancer

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// toolSchemasCtxKey — ключ контекста запроса со схемами инструментов.
type toolSchemasCtxKey struct{}

// WithToolSchemas — положить схемы инструментов запроса в контекст.
//
// ЗАЧЕМ контекст, а не параметр: SSE-путей отдачи несколько, и все они получают
// *http.Request. Схемы разбираются ОДИН раз в обработчике (где есть тело), а не
// в каждом цикле потока.
func WithToolSchemas(ctx context.Context, schemas map[string]*toolParamSchema) context.Context {
	if ctx == nil || len(schemas) == 0 {
		return ctx
	}
	return context.WithValue(ctx, toolSchemasCtxKey{}, schemas)
}

// toolSchemasForRequest — схемы инструментов из контекста запроса.
func (p *Proxy) toolSchemasForRequest(r *http.Request) map[string]*toolParamSchema {
	if r == nil {
		return nil
	}
	schemas, _ := r.Context().Value(toolSchemasCtxKey{}).(map[string]*toolParamSchema)
	return schemas
}

// toolParamSchema — минимальная часть JSON-схемы параметров инструмента,
// которая нужна для приведения аргументов: тип, вложенные свойства, элементы
// массива и обязательные поля.
type toolParamSchema struct {
	Type       string
	Items      *toolParamSchema
	Properties map[string]*toolParamSchema
	Required   []string
}

// toolSchemasFromBody — схемы инструментов из тела запроса (tools[].function.parameters).
//
// Пустой результат = «схем нет»: тогда приведение не выполняется, поведение
// прежнее.
func toolSchemasFromBody(body []byte) map[string]*toolParamSchema {
	if len(body) == 0 {
		return nil
	}
	var doc struct {
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc.Tools) == 0 {
		return nil
	}
	out := make(map[string]*toolParamSchema, len(doc.Tools))
	for _, t := range doc.Tools {
		name := strings.TrimSpace(t.Function.Name)
		if name == "" || len(t.Function.Parameters) == 0 {
			continue
		}
		out[name] = parseToolParamSchema(t.Function.Parameters)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseToolParamSchema — разбор JSON-схемы параметра.
func parseToolParamSchema(raw json.RawMessage) *toolParamSchema {
	var node struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
		Items      json.RawMessage            `json:"items"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil
	}
	s := &toolParamSchema{Type: strings.ToLower(node.Type), Required: node.Required}
	if len(node.Properties) > 0 {
		s.Properties = make(map[string]*toolParamSchema, len(node.Properties))
		for name, sub := range node.Properties {
			s.Properties[name] = parseToolParamSchema(sub)
		}
	}
	if len(node.Items) > 0 {
		s.Items = parseToolParamSchema(node.Items)
	}
	return s
}

// repairToolArguments — привести JSON-строку аргументов к объявленной схеме.
//
// Возвращает (новыеАргументы, былоИзменено). Если приводить нечего или нечем —
// исходная строка и false.
func repairToolArguments(argsJSON string, schema *toolParamSchema) (string, bool) {
	if schema == nil || strings.TrimSpace(argsJSON) == "" {
		return argsJSON, false
	}
	var value interface{}
	if err := json.Unmarshal([]byte(argsJSON), &value); err != nil {
		// Аргументы вообще не JSON (модель написала голую команду). Безопасный
		// ремонт: единственное обязательное поле инструмента — вложить текст в
		// него с учётом объявленного типа (строка → строка, массив → [текст]).
		if name, sub := singleRequiredField(schema); name != "" {
			if wrapped, ok := wrapTextForField(argsJSON, sub); ok {
				fixed, _ := json.Marshal(map[string]interface{}{name: wrapped})
				return string(fixed), true
			}
		}
		return argsJSON, false
	}
	obj, ok := value.(map[string]interface{})
	if !ok {
		if name, sub := singleRequiredField(schema); name != "" {
			if wrapped, ok := wrapTextForField(value, sub); ok {
				fixed, _ := json.Marshal(map[string]interface{}{name: wrapped})
				return string(fixed), true
			}
		}
		return argsJSON, false
	}
	// R248: сначала пытаемся вытащить аргументы из вложенного JSON-мусора
	// (search_files: "path" содержал весь объект, regex терялся).
	// R249: сначала «жёсткая разборка» — модель завернула весь объект в
	// строковое поле (живой случай write: обязательный path «пропал»).
	if unwrapped, ok := unwrapFieldWrappedArgs(obj, schema); ok {
		if out, err := json.Marshal(unwrapped); err == nil {
			return string(out), true
		}
	}
	if salvaged, ok := salvageGarbledArgs(obj, schema); ok {
		if out, err := json.Marshal(salvaged); err == nil {
			return string(out), true
		}
	}
	fixed, changed := repairObjectToSchema(obj, schema)
	if !changed {
		return argsJSON, false
	}
	out, err := json.Marshal(fixed)
	if err != nil {
		return argsJSON, false
	}
	return string(out), true
}

// salvageGarbledArgs — R248 (2026-10-27): вытащить корректные аргументы из
// «вложенного мусора», который слабые модели периодически выдают.
//
// ЖИВОЙ СЛУЧАЙ (матрица инструментов, gemma-4, search_files):
//
//	{"path":"{\"path\":\"{\".\"\",\"regex\":\"package main\"}"}
//
// Модель вложила весь объект аргументов в строковое поле path, экранировав
// кавычки; обязательное поле regex отсутствует, и вызов уходил клиенту битым
// («search_files: нет обязательного поля regex»). Обычный ремонт (null-drop,
// алиасы, массив→строка) такое не лечит.
//
// Правило узкое: если в строковом значении лежит JSON-объект/обломок,
// содержащий обязательные поля схемы, — поднимаем их на верхний уровень;
// значения, оставшиеся JSON-мусором, для пути заменяются на ".".
func salvageGarbledArgs(obj map[string]interface{}, schema *toolParamSchema) (map[string]interface{}, bool) {
	if schema == nil || len(schema.Required) == 0 {
		return obj, false
	}
	missing := func(m map[string]interface{}) bool {
		for _, f := range schema.Required {
			if _, ok := m[f]; !ok {
				return true
			}
		}
		return false
	}
	if !missing(obj) {
		return obj, false
	}

	// 1. Вложенный JSON-объект внутри строкового значения.
	for _, val := range obj {
		s, ok := val.(string)
		if !ok {
			continue
		}
		start := strings.Index(s, "{")
		end := strings.LastIndex(s, "}")
		if start < 0 || end <= start {
			continue
		}
		var nested map[string]interface{}
		if json.Unmarshal([]byte(s[start:end+1]), &nested) != nil {
			continue
		}
		merged := map[string]interface{}{}
		for k, v := range nested {
			merged[k] = v
		}
		for k, v := range obj {
			if sv, isStr := v.(string); isStr && sv == s {
				continue // это контейнер мусора
			}
			merged[k] = v
		}
		if !missing(merged) {
			return scrubGarbledValues(merged, schema), true
		}
	}

	// 2. Фолбэк: обязательные поля регуляркой из сырых строк.
	merged := map[string]interface{}{}
	for k, v := range obj {
		merged[k] = v
	}
	for _, field := range schema.Required {
		if _, ok := merged[field]; ok {
			continue
		}
		pat := regexp.MustCompile(`\\?"` + regexp.QuoteMeta(field) + `\\?"\s*:\s*\\?"((?:[^"\\]|\\.)*)\\?"`)
		for _, val := range obj {
			s, ok := val.(string)
			if !ok {
				continue
			}
			if m := pat.FindStringSubmatch(s); len(m) == 2 {
				merged[field] = strings.ReplaceAll(m[1], `\"`, `"`)
				break
			}
		}
	}
	if missing(merged) {
		return obj, false
	}
	return scrubGarbledValues(merged, schema), true
}

// scrubGarbledValues — значения, в которых остались фрагменты JSON, заменяются
// безопасными: путь → ".", остальные обязательные строки → "".
func scrubGarbledValues(m map[string]interface{}, schema *toolParamSchema) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		s, isStr := v.(string)
		trimmed := strings.TrimSpace(s)
		// «Мусор» — значение, которое НАЧИНАЕТСЯ как JSON-объект/массив или
		// содержит экранированные кавычки. Обычный текст с фигурными скобками
		// (C++-код) мусором не считается — иначе content обнулялся.
		if !isStr || (!strings.Contains(s, `\"`) && !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[")) {
			out[k] = v
			continue
		}
		switch k {
		case "path", "file_path", "filepath", "directory", "dir":
			out[k] = "."
		default:
			if schema != nil && schemaFieldRequired(schema, k) {
				out[k] = ""
			}
		}
	}
	return out
}

// singleRequiredField — единственное обязательное поле инструмента и его схема.
func singleRequiredField(schema *toolParamSchema) (string, *toolParamSchema) {
	if schema == nil || len(schema.Required) != 1 {
		return "", nil
	}
	name := schema.Required[0]
	sub := schema.Properties[name]
	if sub == nil {
		return name, &toolParamSchema{}
	}
	return name, sub
}

// wrapTextForField — разложить «голое» значение по объявленному типу поля:
// строка остаётся строкой, массив получает [значение]. Для прочих типов ремонта
// не делаем (нельзя угадать структуру).
func wrapTextForField(value interface{}, sub *toolParamSchema) (interface{}, bool) {
	if sub == nil {
		return value, false
	}
	switch sub.Type {
	case "", "string":
		if s, ok := value.(string); ok {
			if strings.TrimSpace(s) == "" {
				return nil, false
			}
			return s, true
		}
		return nil, false
	case "array":
		switch sub.Items.Type {
		case "", "string":
			return []interface{}{value}, true
		}
	}
	return nil, false
}

// repairObjectToSchema — рекурсивное приведение объекта к схеме.
func repairObjectToSchema(obj map[string]interface{}, schema *toolParamSchema) (map[string]interface{}, bool) {
	if schema == nil {
		return obj, false
	}
	changed := false

	// Разворот обёртки: {"run_commands": {...}} при схеме, где такого поля нет.
	if len(schema.Properties) > 0 {
		for key, val := range obj {
			if _, declared := schema.Properties[key]; declared {
				continue
			}
			if nested, isObj := val.(map[string]interface{}); isObj && len(obj) == 1 {
				// Единственный ключ, которого нет в схеме, но его значение — объект:
				// это почти всегда обёртка именем инструмента.
				if innerFixed, innerChanged := repairObjectToSchema(nested, schema); innerChanged {
					return innerFixed, true
				}
				if schemaLooksCompatible(nested, schema) {
					return nested, true
				}
			}
		}
	}

	// Скаляр в поле-массиве, объект в поле-массиве, строка-число и т.п.
	for name, val := range obj {
		sub, declared := schema.Properties[name]
		if !declared || sub == nil {
			continue
		}
		if fixed, ok := coerceValueToSchema(val, sub); ok {
			obj[name] = fixed
			changed = true
		}
		if sub.Type == "object" && sub.Properties != nil {
			if nested, isObj := obj[name].(map[string]interface{}); isObj {
				if innerFixed, innerChanged := repairObjectToSchema(nested, sub); innerChanged {
					obj[name] = innerFixed
					changed = true
				}
			}
		}
		if sub.Type == "array" && sub.Items != nil && sub.Items.Type == "object" && sub.Items.Properties != nil {
			if arr, isArr := obj[name].([]interface{}); isArr {
				mutated := false
				for i, el := range arr {
					if nested, isObj := el.(map[string]interface{}); isObj {
						if innerFixed, innerChanged := repairObjectToSchema(nested, sub.Items); innerChanged {
							arr[i] = innerFixed
							mutated = true
						}
					}
				}
				if mutated {
					obj[name] = arr
					changed = true
				}
			}
		}
	}

	// R246 (2026-10-26): null у НЕобязательного поля — это «поле не задано», а
	// не значение объявленного типа. Валидатор клиента (typebox/zod) отвечает на
	// null ошибкой «expected number, received null», и агент тратит ход на
	// ошибку инструмента. Живой случай (gemma-4 через балансер, набор DSH):
	// ralph с {"objective":"…","maxRounds":null} при схеме `maxRounds: number`.
	//
	// Обязательные поля не трогаем: null там — честная ошибка клиента, и
	// подставлять выдуманное значение нельзя.
	for name, val := range obj {
		if val != nil {
			continue
		}
		if _, declared := schema.Properties[name]; declared && schemaFieldRequired(schema, name) {
			continue
		}
		delete(obj, name)
		changed = true
	}

	// Обязательное поле отсутствует, но есть «его же» в единственном числе
	// (commands ← command): модель часто отдаёт singular для plural-схемы.
	if len(schema.Required) > 0 {
		for _, req := range schema.Required {
			if _, present := obj[req]; present {
				continue
			}
			if sub, declared := schema.Properties[req]; declared {
				for _, alias := range singularAliases(req) {
					if aliasVal, exists := obj[alias]; exists {
						fixed, _ := coerceValueToSchema(aliasVal, sub)
						obj[req] = fixed
						delete(obj, alias)
						changed = true
						break
					}
				}
				// Схема требует единственное поле, а объект пуст: вкладываем
				// единственное присланное значение, если оно скаляр.
				if _, present := obj[req]; !present && len(obj) == 0 {
					// нечего вкладывать
					continue
				}
			}
		}
		// R246 (2026-10-26): и наоборот — обязательное поле пришло в
		// МНОЖЕСТВЕННОМ написании (схема `query`, модель отдала `queries`).
		// Живой случай: OpenWebUI-набор через балансер, gemma-4.
		for _, req := range schema.Required {
			if _, present := obj[req]; present {
				continue
			}
			sub, declared := schema.Properties[req]
			if !declared {
				continue
			}
			for _, alias := range pluralAliases(req) {
				if _, isField := schema.Properties[alias]; isField {
					continue
				}
				aliasVal, exists := obj[alias]
				if !exists {
					continue
				}
				fixed, _ := coerceValueToSchema(aliasVal, sub)
				obj[req] = fixed
				delete(obj, alias)
				changed = true
				break
			}
		}
	}
	return obj, changed
}

// schemaFieldRequired — поле объявлено обязательным (сравнение без учёта
// регистра: имена в схемах клиентов бывают в разном регистре).
func schemaFieldRequired(schema *toolParamSchema, name string) bool {
	if schema == nil {
		return false
	}
	for _, req := range schema.Required {
		if req == name || strings.EqualFold(req, name) {
			return true
		}
	}
	return false
}

// singularAliases — вероятные «единственные» имена для обязательного поля.
func singularAliases(field string) []string {
	switch {
	case strings.HasSuffix(field, "ies"):
		return []string{strings.TrimSuffix(field, "ies") + "y"}
	case strings.HasSuffix(field, "ses"), strings.HasSuffix(field, "xes"), strings.HasSuffix(field, "ches"), strings.HasSuffix(field, "shes"):
		return []string{strings.TrimSuffix(field, "es")}
	case strings.HasSuffix(field, "s"):
		return []string{strings.TrimSuffix(field, "s")}
	}
	return nil
}

// pluralAliases — вероятные МНОЖЕСТВЕННЫЕ написания имени обязательного поля.
//
// R246 (2026-10-26): схема объявляет `query: string`, модель отдала
// {"queries":["ollama"]} — клиент (typebox) отвечает «обязательного поля нет»,
// инструмент не применяется. Живой прогон OpenWebUI-набора через балансер.
func pluralAliases(field string) []string {
	if field == "" {
		return nil
	}
	lower := strings.ToLower(field)
	switch {
	case strings.HasSuffix(lower, "y"):
		return []string{field[:len(field)-1] + "ies"}
	case strings.HasSuffix(lower, "s"), strings.HasSuffix(lower, "x"),
		strings.HasSuffix(lower, "ch"), strings.HasSuffix(lower, "sh"):
		return []string{field + "es"}
	}
	return []string{field + "s"}
}

// coerceValueToSchema — привести значение к объявленному типу, если это
// однозначно (скаляр → массив, объект → массив, строка-число → число,
// строка-булево → bool). Возвращает (значение, изменено).
func coerceValueToSchema(val interface{}, schema *toolParamSchema) (interface{}, bool) {
	if schema == nil {
		return val, false
	}
	switch schema.Type {
	case "array":
		switch v := val.(type) {
		case []interface{}:
			return val, false
		case map[string]interface{}:
			return []interface{}{v}, true
		case string:
			if strings.TrimSpace(v) == "" {
				return val, false
			}
			return []interface{}{v}, true
		case float64, bool:
			return []interface{}{v}, true
		}
	case "number", "integer":
		if s, ok := val.(string); ok {
			trimmed := strings.TrimSpace(s)
			if trimmed == "" {
				return val, false
			}
			var num json.Number
			if err := json.Unmarshal([]byte(trimmed), &num); err == nil {
				return num, true
			}
		}
	case "boolean":
		if s, ok := val.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true":
				return true, true
			case "false":
				return false, true
			}
		}
	case "string":
		// R246 (2026-10-26): массив из ОДНОГО элемента при объявленной строке —
		// это та же строка (модель зовёт `query` значением ["ollama"]).
		if arr, isArr := val.([]interface{}); isArr && len(arr) == 1 {
			if s, ok := arr[0].(string); ok {
				return s, true
			}
		}
		if _, ok := val.(string); !ok {
			if num, isNum := val.(float64); isNum {
				out, _ := json.Marshal(num)
				return string(out), true
			}
		}
	}
	return val, false
}

// schemaLooksCompatible — объект уже похож на параметры схемы (есть хоть одно
// объявленное поле).
func schemaLooksCompatible(obj map[string]interface{}, schema *toolParamSchema) bool {
	if schema == nil || len(schema.Properties) == 0 {
		return false
	}
	for name := range obj {
		if _, declared := schema.Properties[name]; declared {
			return true
		}
	}
	return false
}

// normalizeToolCallForOpenAI — привести вызов к форме OpenAI: id, type, index,
// function.name и function.arguments-СТРОКА. `index` нужен клиентам для склейки
// фрагментов стрима (R186: раньше собранные из текста вызовы уходили без него).
func normalizeToolCallForOpenAI(call map[string]interface{}, index int) map[string]interface{} {
	if call == nil {
		return nil
	}
	call["index"] = index
	if _, ok := call["type"]; !ok {
		call["type"] = "function"
	}
	if id, _ := call["id"].(string); strings.TrimSpace(id) == "" {
		call["id"] = "call_" + generateCallID(call)
	}
	if fn, ok := call["function"].(map[string]interface{}); ok {
		if _, ok := fn["arguments"].(string); !ok {
			fn["arguments"] = stringifyToolArguments(fn["arguments"])
		}
	}
	return call
}

// normalizeOpenAISSEPayload — единая точка для SSE-строки OpenAI-поверхности:
//  1. если вызов написан ТЕКСТОМ в delta.content — извлечь его в tool_calls;
//  2. иначе привести НАТИВНЫЕ tool_calls к форме OpenAI (id/type/index) и к схеме.
//
// Возвращает изменённый JSON или nil, если менять нечего.
//
// ЗАЧЕМ ОДНА ФУНКЦИЯ. Путей отдачи SSE в балансере несколько
// (llamacpp_transport.go, proxy_request.go, proxy_request_hijack.go), и правка,
// сделанная только в одном из них, оставляет дефект живым на остальных — ровно
// так и вышло в R186: `index` пропадал на пути, который обслуживал Cline.
func normalizeOpenAISSEPayload(payload []byte, schemas map[string]*toolParamSchema) []byte {
	if modified := extractToolCallsFromSSEContentWithSchema(payload, schemas); modified != nil {
		return modified
	}
	return normalizeSSEToolCallChunks(payload, schemas)
}

// normalizeSSEToolCallChunks — привести НАТИВНЫЕ (пришедшие от cppworker) чанки
// tool_calls к форме OpenAI и к объявленной схеме.
//
// Возвращает изменённый JSON чанка или nil, если менять нечего. Нужно, потому что
// валидатор клиента проверяет схему независимо от того, кто собрал вызов —
// воркер или балансер.
func normalizeSSEToolCallChunks(sseData []byte, schemas map[string]*toolParamSchema) []byte {
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
	rawCalls, ok := delta["tool_calls"].([]interface{})
	if !ok || len(rawCalls) == 0 {
		return nil
	}
	// ВАЖНО: признак «нужна перезапись» снимаем ДО приведения. Функция ниже
	// меняет карты НА МЕСТЕ, поэтому сравнение «до/после» давало false и чанк
	// уходил клиенту байт-в-байт — ровно так `index` и продолжал теряться
	// (поймано живой проверкой, а не тестом: тест сравнивал уже собранный ответ).
	needsRewrite := false
	for _, raw := range rawCalls {
		call, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if _, hasIndex := call["index"]; !hasIndex {
			needsRewrite = true
		}
	}
	fixed, repaired := repairToolCallsForSchema(rawCalls, schemas)
	if !needsRewrite && repaired == 0 {
		return nil
	}
	delta["tool_calls"] = fixed
	choice["delta"] = delta
	choices[0] = choice
	chunk["choices"] = choices
	out, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return out
}

// repairToolCallsForSchema — приведение набора вызовов к схемам запроса и к форме
// OpenAI. Возвращает (вызовы, сколько исправлено).
func repairToolCallsForSchema(toolCalls []interface{}, schemas map[string]*toolParamSchema) ([]interface{}, int) {
	if len(toolCalls) == 0 {
		return toolCalls, 0
	}
	repaired := 0
	for i, raw := range toolCalls {
		call, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		normalizeToolCallForOpenAI(call, i)
		fn, _ := call["function"].(map[string]interface{})
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		schema := schemas[name]
		if schema == nil {
			continue
		}
		if fixed, changed := repairToolArguments(args, schema); changed {
			fn["arguments"] = fixed
			repaired++
			if logger.Get() != nil {
				logger.Get().Warnw("tool call arguments repaired to declared schema",
					"tool", name, "index", i, "before", truncateForLog(args, 200), "after", truncateForLog(fixed, 200))
			}
		}
	}
	return toolCalls, repaired
}
