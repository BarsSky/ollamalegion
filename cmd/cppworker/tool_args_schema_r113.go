// tool_args_schema_r113.go — R113 (2026-10-13): аргументы вызова инструмента
// приводятся к ОБЪЕКТУ по СХЕМЕ инструмента из запроса.
//
// ЖИВОЙ ДЕФЕКТ (воспроизведён на стенде запросом настоящего Cline CLI,
// gemma-4-E4B-it-Q4_K_M, 25 инструментов, stream=true):
//
//	{"function":{"name":"run_commands","arguments":["ls -A"]}}
//	{"function":{"name":"list_files","arguments":"c:\\Ollama\\ollamalegion"}}
//
// То есть arguments — МАССИВ или ГОЛАЯ СТРОКА, а не объект. Клиент (Cline,
// OpenAI SDK, ollama-python) ждёт JSON-объект по схеме инструмента и такой вызов
// выполнить не может: инструмент не запускается, ответ остаётся «последним
// словом» модели → задача помечается выполненной (COMPLETE) после первой же
// итерации, а текст вызова оператор видит в теле ответа.
//
// ПОЧЕМУ ЭТО НЕ «КОСТЫЛЬ ПОД МОДЕЛЬ». Имена параметров берутся ИЗ ЗАПРОСА —
// из JSON-схемы инструмента, которую прислал сам клиент (req.Tools[].function.
// parameters). Никаких имён моделей, никаких зашитых списков: правило одно —
// «значение, не являющееся объектом, принадлежит объявленному параметру».
//
// ПРАВИЛА ПРИВЕДЕНИЯ (детерминированные, по схеме):
//
//	arguments уже объект              → оставляем как есть;
//	аргументы — массив                → в первый объявленный параметр-массив
//	                                    (required в приоритете): run_commands.commands,
//	                                    read_files.files, search_codebase.queries;
//	аргумент — скаляр (строка/число)  → в единственный параметр, иначе в первый
//	                                    required-строку, иначе в первый параметр;
//	строка, СОДЕРЖАЩАЯ JSON           → разворачиваем один уровень и повторяем;
//	объект-обёртка {"args":{…}}       → разворачиваем, если ключ не объявлен,
//	                                    а внутренние ключи объявлены;
//	инструмент не объявлен в запросе  → вызов отбрасывается (нельзя угадать схему),
//	                                    вызывающий код считает вывод нераспознанным
//	                                    и повторяет генерацию.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// toolSchemaInfo — разобранная схема одного инструмента.
type toolSchemaInfo struct {
	name       string
	properties []toolSchemaProp
	required   map[string]bool
}

type toolSchemaProp struct {
	name string
	typ  string // "array" | "string" | "integer" | ... | "" (не объявлен)
}

// buildToolSchemaIndex — индекс «имя инструмента → схема параметров».
// Имена сравниваются без учёта регистра: модели иногда меняют регистр.
//
// Порядок параметров сохраняется ТАК, КАК ИХ ОБЪЯВИЛ КЛИЕНТ (см.
// parseSchemaPropertiesOrdered): из него выбирается параметр для «одинокого»
// значения. Map в Go порядок не хранит, а порядок объявления — часть контракта
// схемы, поэтому читаем его декодером, а не через map.
func buildToolSchemaIndex(tools []openAITool) map[string]toolSchemaInfo {
	if len(tools) == 0 {
		return nil
	}
	idx := make(map[string]toolSchemaInfo, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			continue
		}
		info := toolSchemaInfo{name: name, required: map[string]bool{}}
		info.properties = parseSchemaPropertiesOrdered(t.Function.Parameters)
		var schema struct {
			Required []string `json:"required"`
		}
		if len(t.Function.Parameters) > 0 {
			_ = json.Unmarshal(t.Function.Parameters, &schema)
		}
		for _, r := range schema.Required {
			if strings.TrimSpace(r) != "" {
				info.required[strings.TrimSpace(r)] = true
			}
		}
		idx[strings.ToLower(name)] = info
	}
	if len(idx) == 0 {
		return nil
	}
	return idx
}

// parseSchemaPropertiesOrdered — параметры схемы в порядке объявления.
//
// Реализация через json.Decoder: сначала достаём сырой объект "properties", затем
// идём по нему токенами, запоминая ключи в порядке появления. Это единственный
// способ не потерять порядок (map[string]… его не хранит), а порядок нужен для
// детерминированного выбора параметра под значение без имени.
func parseSchemaPropertiesOrdered(params json.RawMessage) []toolSchemaProp {
	if len(params) == 0 {
		return nil
	}
	var wrapper struct {
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(params, &wrapper); err != nil || len(wrapper.Properties) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(wrapper.Properties))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil
	}
	var props []toolSchemaProp
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := keyTok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break
		}
		var meta struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &meta)
		props = append(props, toolSchemaProp{name: key, typ: meta.Type})
	}
	return props
}

// pickArrayParam — параметр, которому принадлежит массив.
func (t toolSchemaInfo) pickArrayParam() string {
	for _, p := range t.properties {
		if p.typ == "array" && t.required[p.name] {
			return p.name
		}
	}
	for _, p := range t.properties {
		if p.typ == "array" {
			return p.name
		}
	}
	if len(t.properties) == 1 {
		return t.properties[0].name
	}
	return ""
}

// pickScalarParam — параметр, которому принадлежит скаляр (строка/число/бул).
func (t toolSchemaInfo) pickScalarParam() string {
	if len(t.properties) == 1 {
		return t.properties[0].name
	}
	for _, p := range t.properties {
		if (p.typ == "string" || p.typ == "") && t.required[p.name] {
			return p.name
		}
	}
	for _, p := range t.properties {
		if p.typ == "string" || p.typ == "" {
			return p.name
		}
	}
	for _, p := range t.properties {
		if t.required[p.name] {
			return p.name
		}
	}
	if len(t.properties) > 0 {
		return t.properties[0].name
	}
	return ""
}

// paramType — объявленный тип параметра ("" — не объявлен/неизвестен).
func (t toolSchemaInfo) paramType(name string) string {
	for _, p := range t.properties {
		if strings.EqualFold(p.name, name) {
			return p.typ
		}
	}
	return ""
}

// isRequired — поле объявлено обязательным (сравнение без учёта регистра).
func (t toolSchemaInfo) isRequired(name string) bool {
	if t.required == nil {
		return false
	}
	if t.required[name] {
		return true
	}
	for req := range t.required {
		if strings.EqualFold(req, name) {
			return true
		}
	}
	return false
}

// dropNullOptionalArgs — убрать null у НЕобязательных полей аргументов.
//
// R246 (2026-10-26): null — это «поле не задано», а не значение объявленного
// типа. Валидаторы клиентов (typebox/zod) на null отвечают ошибкой
// «expected number, received null»; агент тратит ход на ошибку инструмента.
// Живой случай (gemma-4 через балансер, набор DSH): вызов ralph с
// {"objective":"…","maxRounds":null} при схеме `maxRounds: number`.
//
// Обязательные поля НЕ трогаем: null в обязательном поле — честная ошибка
// клиента, и подставлять выдуманное значение нельзя.
func dropNullOptionalArgs(obj map[string]interface{}, schema toolSchemaInfo) {
	for key, val := range obj {
		if val != nil {
			continue
		}
		if schema.isRequired(key) {
			continue
		}
		delete(obj, key)
	}
}

// applyPluralAliases — обязательное поле пришло в МНОЖЕСТВЕННОМ написании.
//
// R246 (2026-10-26). Живой прогон OpenWebUI-набора инструментов через балансер
// (gemma-4): схема объявляет `query: string` (required), модель отдала
// {"queries":["ollama"]}. Клиент (typebox) отвечает «expected string, received
// undefined» — обязательного поля нет, инструмент не применяется. Это то же
// слово в другом числе, значение переносится как есть (с приведением к
// объявленному типу), а не выдумывается.
func applyPluralAliases(obj map[string]interface{}, schema toolSchemaInfo) {
	for _, p := range schema.properties {
		if !schema.isRequired(p.name) {
			continue
		}
		if _, present := obj[p.name]; present {
			continue
		}
		for _, alias := range pluralAliases(p.name) {
			// Ключ-алиас не должен быть ОТДЕЛЬНЫМ объявленным полем схемы:
			// иначе переименование съест соседний параметр.
			if schema.paramType(alias) != "" {
				continue
			}
			val, exists := obj[alias]
			if !exists {
				continue
			}
			obj[p.name] = coerceValueToDeclaredType(val, p.typ)
			delete(obj, alias)
			break
		}
	}
}

// pluralAliases — вероятные МНОЖЕСТВЕННЫЕ написания имени поля.
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

// coerceValueToDeclaredType — привести значение к ОБЪЯВЛЕННОМУ типу параметра.//
// R114d (2026-10-13). Живой случай (Cline + gemma-4, схема `run_commands`:
// `commands: array`): модель отдала СКАЛЯР, приведение положило его в объявленный
// параметр, и клиент получил `{"commands":"dir"}` — объект, но тип не тот.
// Валидатор клиента такой вызов отвергает («expected array, received string»), и
// агентский цикл тратит ход на ошибку инструмента.
//
// Правила (по схеме, без имён моделей):
//
//	объявлен array, а значение скаляр  → массив из одного элемента;
//	объявлен string, а значение массив → единственный элемент-строка, иначе JSON;
//	объявлен integer/number, строка    → число, если строка числовая;
//	объявлен boolean, строка           → true/false, если строка распознаётся.
func coerceValueToDeclaredType(value interface{}, declared string) interface{} {
	switch declared {
	case "array":
		if _, isArr := value.([]interface{}); !isArr {
			return []interface{}{value}
		}
	case "string":
		if arr, isArr := value.([]interface{}); isArr {
			if len(arr) == 1 {
				if s, ok := arr[0].(string); ok {
					return s
				}
			}
			if b, err := marshalNoHTMLEscape(arr); err == nil {
				return string(b)
			}
		}
	case "integer", "number":
		if s, ok := value.(string); ok {
			trimmed := strings.TrimSpace(s)
			if trimmed != "" {
				var num json.Number
				if err := json.Unmarshal([]byte(trimmed), &num); err == nil {
					if declared == "integer" {
						if i, err := num.Int64(); err == nil {
							return i
						}
					} else if f, err := num.Float64(); err == nil {
						return f
					}
				}
			}
		}
	case "boolean":
		if s, ok := value.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true", "1", "yes", "on":
				return true
			case "false", "0", "no", "off":
				return false
			}
		}
	}
	return value
}

// coerceToolCallsToSchema приводит arguments каждого вызова к JSON-объекту,
// согласованному со схемой инструмента из запроса.
//
// Возвращает новый срез: вызовы, для которых схему установить нельзя и значение
// не является объектом, ОТБРАСЫВАЮТСЯ (угадывать имя параметра нельзя — клиент
// получил бы невыполнимый вызов). Если tools пуст, поведение прежнее: меняем
// только явно невалидные значения на "{}" — обратная совместимость.
func coerceToolCallsToSchema(calls []openAIToolCall, tools []openAITool) []openAIToolCall {
	if len(calls) == 0 {
		return calls
	}
	idx := buildToolSchemaIndex(tools)
	out := make([]openAIToolCall, 0, len(calls))
	for _, call := range calls {
		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			args = "{}"
		}

		var schema toolSchemaInfo
		haveSchema := false
		if idx != nil {
			if s, ok := idx[strings.ToLower(strings.TrimSpace(call.Function.Name))]; ok {
				schema, haveSchema = s, true
			}
		}

		coerced, ok := coerceArgumentsValue(args, schema, haveSchema)
		// Схема ЕСТЬ, но этого инструмента в ней нет: имя параметра взять неоткуда.
		// Значение-объект оставляем (клиент может разобрать его сам), всё прочее
		// отбрасываем — невыполнимый вызов хуже, чем его отсутствие: агентский
		// клиент считает такое сообщение финальным ответом.
		if idx != nil && !haveSchema && toolArgShape(args) != "object" {
			continue
		}
		if !ok {
			continue // вызов отброшен: клиент не сможет его выполнить
		}
		call.Function.Arguments = coerced
		out = append(out, call)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// coerceArgumentsValue — одна попытка приведения значения к объекту.
func coerceArgumentsValue(raw string, schema toolSchemaInfo, haveSchema bool) (string, bool) {
	value, ok := decodeJSONValue(raw)
	if !ok {
		// Не JSON вообще (например Windows-путь с обратными слэшами).
		if !haveSchema {
			quoted, err := marshalNoHTMLEscape(raw)
			if err != nil {
				return "", false
			}
			return string(quoted), true // как было: строка вместо объекта
		}
		param := schema.pickScalarParam()
		if param == "" {
			return "", false
		}
		return encodeArgsObject(map[string]interface{}{param: raw})
	}

		switch v := value.(type) {
	case map[string]interface{}:
		if haveSchema {
			if unwrapped, ok := unwrapArgsWrapper(v, schema); ok {
				v = unwrapped
			}
			// R114d (2026-10-13): значения ВНУТРИ объекта тоже приводим к
			// объявленным типам. Живой случай: модель отдала
			// {"commands":"dir"} при схеме `commands: array` — объект валиден, а
			// тип поля нет, и валидатор клиента отверг вызов.
			for key, val := range v {
				if typ := schema.paramType(key); typ != "" {
					v[key] = coerceValueToDeclaredType(val, typ)
				}
			}
			// R246 (2026-10-26): null у НЕобязательного поля — не значение, а
			// «поле не задано». Валидатор клиента (typebox/zod) отвергает null
			// там, где объявлен тип: живой прогон gemma-4 звал ralph с
			// {"maxRounds": null} при схеме `maxRounds: number` — клиент отвечал
			// ошибкой валидации, агент повторял вызов.
			dropNullOptionalArgs(v, schema)
			// R246: обязательное поле в множественном написании (queries ← query).
			applyPluralAliases(v, schema)
		}
		return encodeArgsObject(v)

	case []interface{}:
		if !haveSchema {
			return raw, true
		}
		param := schema.pickArrayParam()
		if param == "" {
			return "", false
		}
		return encodeArgsObject(map[string]interface{}{
			param: coerceValueToDeclaredType(v, schema.paramType(param)),
		})

	case string:
		trimmed := strings.TrimSpace(v)
		// Двойное кодирование: строка внутри строки с JSON.
		if trimmed != "" && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(trimmed)) {
			if inner, ok := coerceArgumentsValue(trimmed, schema, haveSchema); ok {
				return inner, true
			}
		}
		if !haveSchema {
			quoted, err := marshalNoHTMLEscape(v)
			if err != nil {
				return "", false
			}
			return string(quoted), true
		}
		param := schema.pickScalarParam()
		if param == "" {
			return "", false
		}
		// R114d: приводим значение к ОБЪЯВЛЕННОМУ типу параметра (скаляр в поле
		// массива → массив, числовая строка в поле числа → число и т.д.).
		return encodeArgsObject(map[string]interface{}{
			param: coerceValueToDeclaredType(v, schema.paramType(param)),
		})

	default:
		// Число/бул/null.
		if !haveSchema {
			return raw, true
		}
		param := schema.pickScalarParam()
		if param == "" {
			return "", false
		}
		return encodeArgsObject(map[string]interface{}{
			param: coerceValueToDeclaredType(v, schema.paramType(param)),
		})
	}
}

// unwrapArgsWrapper — {"args":{...}} / {"arguments":{...}} / {"parameters":{...}},
// когда сам ключ-обёртка в схеме не объявлен, а внутренние ключи объявлены.
func unwrapArgsWrapper(obj map[string]interface{}, schema toolSchemaInfo) (map[string]interface{}, bool) {
	if len(obj) != 1 {
		return nil, false
	}
	declared := func(name string) bool {
		for _, p := range schema.properties {
			if strings.EqualFold(p.name, name) {
				return true
			}
		}
		return false
	}
	for _, key := range []string{"args", "arguments", "parameters", "input", "kwargs"} {
		inner, ok := obj[key].(map[string]interface{})
		if !ok || len(inner) == 0 || declared(key) {
			continue
		}
		matched := 0
		for k := range inner {
			if declared(k) {
				matched++
			}
		}
		if matched > 0 {
			return inner, true
		}
	}
	return nil, false
}

// decodeJSONValue — разбор JSON-значения; строки разворачиваются на один уровень,
// если их содержимое само является JSON-объектом/массивом.
func decodeJSONValue(raw string) (interface{}, bool) {
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, false
	}
	return v, true
}

// encodeArgsObject — сериализация объекта аргументов без HTML-эскейпинга.
func encodeArgsObject(obj map[string]interface{}) (string, bool) {
	b, err := marshalNoHTMLEscape(obj)
	if err != nil {
		return "", false
	}
	return compactJSON(string(b)), true
}

// toolArgShape — форма значения arguments для лога/трассировки:
// object | array | string | number | bool | null | invalid.
//
// Нужна оператору: «object» — клиент сможет выполнить вызов, всё остальное —
// повод разбираться (R113: раньше приходило array/string).
func toolArgShape(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return "empty"
	}
	var v interface{}
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return "invalid"
	}
	switch v.(type) {
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	}
	return "other"
}

// toolCallFromJSONObject — вызов из ОДНОГО JSON-объекта в известных формах:
//
//	{"name":"run_commands","arguments":{"commands":["dir"]}}          (Hermes/Qwen)
//	{"function":{"name":"run_commands","arguments":{…}}}              (OpenAI)
//	{"tool_call":"run_commands","args":{…}}                           (R109, gemma)
//	{"tool_name":"editor","parameters":{…}}                           (вариант tool_name)
//
// Возвращает nil, если объект не похож на вызов. Схема здесь не нужна: приведение
// аргументов делает coerceToolCallsToSchema.
func toolCallFromJSONObject(raw string) *openAIToolCall {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil
	}
	// OpenAI-форма: function — ОБЪЕКТ с name/arguments.
	if fnRaw, ok := obj["function"]; ok {
		var fn struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(fnRaw, &fn); err == nil && strings.TrimSpace(fn.Name) != "" {
			args := strings.TrimSpace(string(fn.Arguments))
			if args == "" || args == "null" {
				args = "{}"
			}
			return &openAIToolCall{
				Type:     "function",
				Function: openAIFunctionCall{Name: strings.TrimSpace(fn.Name), Arguments: args},
			}
		}
	}
	// Плоские формы: имя в name/tool_call/tool_name, аргументы в arguments/parameters/args/input.
	var name string
	for _, nameKey := range []string{"name", "tool_call", "tool_name", "tool"} {
		if rawName, ok := obj[nameKey]; ok {
			var s string
			if json.Unmarshal(rawName, &s) == nil && strings.TrimSpace(s) != "" {
				name = strings.TrimSpace(s)
				break
			}
		}
	}
	if name == "" {
		return nil
	}
	for _, argKey := range []string{"arguments", "parameters", "args", "input"} {
		rawArgs, ok := obj[argKey]
		if !ok {
			continue
		}
		args := strings.TrimSpace(string(rawArgs))
		if args == "" || args == "null" {
			args = "{}"
		}
		return &openAIToolCall{
			Type:     "function",
			Function: openAIFunctionCall{Name: name, Arguments: args},
		}
	}
	return nil
}

// isToolCallJSONObject — объект ПОХОЖ на вызов инструмента (без разбора имени
// по схеме). Нужен, чтобы не отдавать такой JSON клиенту как текст ответа.
func isToolCallJSONObject(raw string) bool {
	return toolCallFromJSONObject(raw) != nil
}

// recoverToolCallsFromText — последняя попытка распознать вызов, когда он не
// обёрнут тегами и не разобран обычными стратегиями.
//
// ЖИВОЙ СЛУЧАЙ (Cline + gemma-4, второй ход агентского цикла). Клиент присылает
// историю, где вызов ассистента записан как JSON (`[{…"function":{…}}]`), мост
// кладёт его в промпт текстом — и модель его ПОВТОРЯЕТ в ответе:
//
//	… I will execute it again.
//	[[{"id":"call_1","type":"function","function":{"name":"run_commands",
//	   "arguments":{"commands":["dir"]}}}]]
//
// Раньше это уезжало клиенту как content («инструмент в теле ответа»), вызова в
// tool_calls не было — агентский цикл обрывался. Теперь такой JSON распознаётся
// как вызов: клиент получает tool_calls и продолжает работу.
func recoverToolCallsFromText(text string, tools []openAITool) []openAIToolCall {
	if text == "" || !strings.Contains(text, "{") {
		return nil
	}
	var out []openAIToolCall
	for _, cand := range jsonObjectCandidates(text) {
		if call := toolCallFromJSONObject(cand); call != nil {
			out = append(out, *call)
		}
	}
	if len(out) == 0 {
		return nil
	}
	if !normalizeToolCalls(out) {
		return nil
	}
	return coerceToolCallsToSchema(out, tools)
}

// parseToolCallsForRequest — ЕДИНАЯ точка разбора вызова для клиентских путей:
//
//  1. обычные стратегии разбора (теги, фенсы, JSON) с приведением аргументов по
//     схеме инструментов запроса;
//  2. если ничего не найдено — восстановление вызова из JSON-объекта в тексте
//     (модель повторяет присланный историей вызов: живой случай Cline + gemma-4).
//
// Единая точка нужна, чтобы поведение не расходилось между /api/chat (stream и
// non-stream) и /v1/chat/completions.
func parseToolCallsForRequest(output string, tools []openAITool) []openAIToolCall {
	if calls := parseToolCallsFromOutputWithTools(output, tools); len(calls) > 0 {
		return calls
	}
	return recoverToolCallsFromText(output, tools)
}

// --- Маркеры вызова в тексте ответа -----------------------------------------

// toolCallMarkupMarkers — ВСЕ известные обёртки вызова инструмента в выводе
// моделей. Список один на весь пакет: по нему и решается «это разметка вызова»,
// и вырезается она из видимого текста. Никаких имён моделей.
var toolCallMarkupMarkers = []string{
	"<tool_call>", "</tool_call>", "<tool_call|>", "<|tool_call>", "<|tool_call|>",
	"<|python_tag|>", "[TOOL_CALLS]", "<<tool_call>>", "<function_call>", "</function_call>",
	"<start_function_call>", "<end_function_call>",
	// TOOLCALL-ANGLE (живой дефект: скриншот оператора): Hermes angle-форма —
	// <tool_call> <function=bash> <parameter=command> pwd && ls -la </parameter>
	// <parameter=description> … </parameter> </function> </tool_call>.
	// JSON в ней нет, поэтому раньше НИ ОДИН маркер не срабатывал и разметка
	// уезжала клиенту телом ответа.
	"<function=", "</function>", "<parameter=", "</parameter>",
	// R246 (2026-10-26): диалект gemma `[tool_code]` / ```tool_code —
	// вызов питоновским синтаксисом. Живой дефект: разметка уехала клиенту
	// телом ответа, инструмент не вызвался.
	"[tool_code]", "[/tool_code]", "```tool_code",
}

// looksLikeToolCallMarkup — вывод содержит обёртку вызова инструмента ЛИБО
// JSON-объект, похожий на вызов, в любом месте текста (модель повторяет вызов,
// присланный историей, — живой случай Cline + gemma-4).
func looksLikeToolCallMarkup(text string) bool {
	if text == "" {
		return false
	}
	for _, m := range toolCallMarkupMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	if !strings.Contains(text, "{") {
		return false
	}
	for _, cand := range jsonObjectCandidates(text) {
		if isToolCallJSONObject(cand) {
			return true
		}
	}
	return false
}

// hasToolCallTagMarkup — в тексте есть теговая обёртка вызова.
func hasToolCallTagMarkup(text string) bool {
	for _, m := range toolCallMarkupMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// stripToolCallMarkup удаляет из видимого текста блоки вызова инструмента —
// теговые и JSON-формы. Используется как ПОСЛЕДНИЙ рубеж: даже если вызов не
// распознан, его разметка не должна уехать клиенту как ответ (живая жалоба
// «инструмент опять оказался в теле ответа»).
func stripToolCallMarkup(text string) string {
	if text == "" {
		return text
	}
	cleaned := text
	if !hasToolCallTagMarkup(cleaned) && !strings.Contains(cleaned, "{") {
		return cleaned
	}
	if hasToolCallTagMarkup(cleaned) {
		// Парные обёртки: <tag> … </tag> (и пайповые варианты).
		pairs := [][2]string{
			{"<|tool_call>", "<tool_call|>"},
			{"<tool_call>", "</tool_call>"},
			{"<function_call>", "</function_call>"},
			{"<start_function_call>", "<end_function_call>"},
			{"<<tool_call>>", "<</tool_call>>"},
			// TOOLCALL-ANGLE: `<function=NAME> … </function>` — снимает angle-форму
			// даже когда открывающий <tool_call> потерян при обрыве стрима.
			{"<function=", "</function>"},
		}
		for _, p := range pairs {
			for {
				start := strings.Index(cleaned, p[0])
				if start < 0 {
					break
				}
				end := strings.Index(cleaned[start:], p[1])
				if end < 0 {
					// Незакрытый блок — режем до конца (хвост генерации обрезан).
					cleaned = cleaned[:start]
					break
				}
				cleaned = cleaned[:start] + cleaned[start+end+len(p[1]):]
			}
		}
		// Одиночные маркеры-префиксы (модель оборвала генерацию).
		for _, m := range []string{"[TOOL_CALLS]", "<|python_tag|>"} {
			if i := strings.Index(cleaned, m); i >= 0 {
				cleaned = cleaned[:i]
			}
		}
	}
	// R246: блоки tool_code снимаются отдельно: у них своя пара маркеров
	// ([tool_code]/[/tool_code] и ```tool_code/```), внутри — питоновский вызов.
	cleaned = stripToolCodeMarkup(cleaned)
	// R113: JSON-формы вызова без тегов (модель повторяет присланный историей
	// вызов). Такой JSON — не текст ответа: вырезаем сам объект и, если он был
	// обёрнут в массив (`[{…}]`), пустые скобки массива тоже.
	cleaned = stripToolCallJSONObjects(cleaned)
	return strings.TrimSpace(cleaned)
}

// stripToolCallJSONObjects — вырезает из текста JSON-объекты, похожие на вызов
// инструмента, вместе с оставшейся от них обёрткой-массивом.
func stripToolCallJSONObjects(text string) string {
	if text == "" || !strings.Contains(text, "{") {
		return text
	}
	for _, cand := range jsonObjectCandidates(text) {
		if !isToolCallJSONObject(cand) {
			continue
		}
		idx := strings.Index(text, cand)
		if idx < 0 {
			continue
		}
		start, end := idx, idx+len(cand)
		// Съедаем обрамляющие [ ] (в т.ч. вложенные [[ ]]).
		for start > 0 && (text[start-1] == '[' || text[start-1] == ' ' || text[start-1] == '\n' || text[start-1] == '\r' || text[start-1] == '\t') {
			if text[start-1] == ' ' || text[start-1] == '\n' || text[start-1] == '\r' || text[start-1] == '\t' {
				start--
				continue
			}
			start--
		}
		for end < len(text) && (text[end] == ']' || text[end] == ' ' || text[end] == '\n' || text[end] == '\r' || text[end] == '\t') {
			end++
		}
		text = text[:start] + text[end:]
	}
	return text
}

