// tool_schema_validate_r247.go — R247, этап A плана
// (docs/plans/2026-10-26-clients-agents-contract-r247.md).
//
// ЗАЧЕМ. Клиенты structured-output (OpenAI SDK `strict:true`, LangChain,
// Pydantic-AI, Continue) считают ответ невалидным, если аргументы вызова не
// соответствуют ОБЪЯВЛЕННОЙ схеме. До R247 ремонт аргументов был эвристическим
// (null/типы/множественное число): часть нарушений уходила клиенту, и он
// отвечал ошибкой инструмента, тратя ход агента.
//
// Здесь — проверка «по схеме» и решение, можно ли отдавать вызов как есть.
// Для инструментов БЕЗ strict поведение прежнее: проверка только пишется в лог.
package main

import (
	"encoding/json"
	"os"
	"strings"
)

// jsonSchemaNode — минимальная, но достаточная для tool-схем часть JSON Schema.
type jsonSchemaNode struct {
	HasType              bool
	Type                 string
	Enum                 []interface{}
	HasConst             bool
	Const                interface{}
	Properties           map[string]*jsonSchemaNode
	Required             []string
	Items                *jsonSchemaNode
	AdditionalProperties *bool
	AnyOf                []*jsonSchemaNode
	OneOf                []*jsonSchemaNode
}

// parseJSONSchemaNode — разбор схемы; nil при пустой/битой схеме (тогда проверок нет).
func parseJSONSchemaNode(raw json.RawMessage) *jsonSchemaNode {
	if len(raw) == 0 {
		return nil
	}
	var doc struct {
		Type                 json.RawMessage            `json:"type"`
		Enum                 []interface{}              `json:"enum"`
		Const                json.RawMessage            `json:"const"`
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		Items                json.RawMessage            `json:"items"`
		AdditionalProperties json.RawMessage            `json:"additionalProperties"`
		AnyOf                []json.RawMessage          `json:"anyOf"`
		OneOf                []json.RawMessage          `json:"oneOf"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	node := &jsonSchemaNode{Enum: doc.Enum, Required: doc.Required}
	// type может быть строкой ИЛИ массивом строк (["string","null"]).
	if len(doc.Type) > 0 {
		var single string
		if err := json.Unmarshal(doc.Type, &single); err == nil {
			node.HasType, node.Type = true, strings.ToLower(single)
		} else {
			var list []string
			if err := json.Unmarshal(doc.Type, &list); err == nil && len(list) > 0 {
				node.HasType, node.Type = true, strings.ToLower(list[0])
			}
		}
	}
	if len(doc.Const) > 0 && string(doc.Const) != "null" {
		node.HasConst = true
		_ = json.Unmarshal(doc.Const, &node.Const)
	}
	if len(doc.Properties) > 0 {
		node.Properties = make(map[string]*jsonSchemaNode, len(doc.Properties))
		for name, sub := range doc.Properties {
			node.Properties[name] = parseJSONSchemaNode(sub)
		}
	}
	if len(doc.Items) > 0 {
		node.Items = parseJSONSchemaNode(doc.Items)
	}
	if len(doc.AdditionalProperties) > 0 {
		var allow bool
		if err := json.Unmarshal(doc.AdditionalProperties, &allow); err == nil {
			node.AdditionalProperties = &allow
		}
	}
	for _, sub := range doc.AnyOf {
		if n := parseJSONSchemaNode(sub); n != nil {
			node.AnyOf = append(node.AnyOf, n)
		}
	}
	for _, sub := range doc.OneOf {
		if n := parseJSONSchemaNode(sub); n != nil {
			node.OneOf = append(node.OneOf, n)
		}
	}
	return node
}

// strictToolSchemas — схемы только тех инструментов, что объявлены strict:true.
//
// strict может стоять в двух местах, встречаются оба:
//   - tools[].function.strict (OpenAI SDK/Pydantic-AI);
//   - strict:true внутри самой схемы параметров (LangChain).
func strictToolSchemas(tools []openAITool) map[string]*jsonSchemaNode {
	out := make(map[string]*jsonSchemaNode)
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			continue
		}
		strict := tool.Function.Strict
		if !strict && len(tool.Function.Parameters) > 0 {
			var probe struct {
				Strict bool `json:"strict"`
			}
			_ = json.Unmarshal(tool.Function.Parameters, &probe)
			strict = probe.Strict
		}
		if !strict {
			continue
		}
		if node := parseJSONSchemaNode(tool.Function.Parameters); node != nil {
			out[strings.ToLower(name)] = node
		}
	}
	return out
}

// validateNode — рекурсивная проверка значения по схеме; violations пополняется
// путями вида `arguments.foo[0].bar: expected integer`.
func validateNode(node *jsonSchemaNode, value interface{}, path string, violations *[]string) {
	if node == nil {
		return
	}
	// anyOf/oneOf: значение обязано пройти хотя бы одну ветку.
	if len(node.AnyOf) > 0 || len(node.OneOf) > 0 {
		branches := node.AnyOf
		if len(branches) == 0 {
			branches = node.OneOf
		}
		for _, branch := range branches {
			var inner []string
			validateNode(branch, value, path, &inner)
			if len(inner) == 0 {
				return
			}
		}
		*violations = append(*violations, path+": не соответствует ни одной ветке anyOf/oneOf")
		return
	}
	if len(node.Enum) > 0 {
		matched := false
		for _, allowed := range node.Enum {
			if schemaJSONEqual(allowed, value) {
				matched = true
				break
			}
		}
		if !matched {
			*violations = append(*violations, path+": значение вне enum")
			return
		}
	}
	if node.HasConst && !schemaJSONEqual(node.Const, value) {
		*violations = append(*violations, path+": значение не равно const")
		return
	}
	if node.HasType && !valueMatchesType(node.Type, value) {
		*violations = append(*violations, path+": expected "+node.Type+", got "+jsonTypeName(value))
		return
	}
	switch node.Type {
	case "object":
		obj, ok := value.(map[string]interface{})
		if !ok {
			return
		}
		for _, req := range node.Required {
			if _, present := obj[req]; !present {
				*violations = append(*violations, path+"."+req+": required field missing")
			}
		}
		for key, val := range obj {
			if sub, declared := node.Properties[key]; declared {
				validateNode(sub, val, path+"."+key, violations)
				continue
			}
			if node.AdditionalProperties != nil && !*node.AdditionalProperties {
				*violations = append(*violations, path+"."+key+": additional property not allowed")
			}
		}
	case "array":
		arr, ok := value.([]interface{})
		if !ok || node.Items == nil {
			return
		}
		for i, el := range arr {
			validateNode(node.Items, el, path+"["+schemaIntToStr(i)+"]", violations)
		}
	}
}

func valueMatchesType(declared string, value interface{}) bool {
	switch declared {
	case "object":
		_, ok := value.(map[string]interface{})
		return ok
	case "array":
		_, ok := value.([]interface{})
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == float64(int64(f))
	case "null":
		return value == nil
	}
	return true
}

func jsonTypeName(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if v == float64(int64(v)) {
			return "integer"
		}
		return "number"
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	}
	return "unknown"
}

func schemaJSONEqual(a, b interface{}) bool {
	left, err1 := json.Marshal(a)
	right, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(left) == string(right)
}

func schemaIntToStr(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// strictToolCallViolations — нарушения схемы по всем ВЫЗВАННЫМ strict-инструментам.
// Пусто = можно отдавать клиенту как есть.
func strictToolCallViolations(calls []openAIToolCall, tools []openAITool) []string {
	schemas := strictToolSchemas(tools)
	if len(schemas) == 0 || len(calls) == 0 {
		return nil
	}
	var violations []string
	for _, call := range calls {
		name := strings.ToLower(strings.TrimSpace(call.Function.Name))
		node, strict := schemas[name]
		if !strict {
			continue
		}
		var args interface{}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			violations = append(violations, call.Function.Name+": arguments is not valid JSON")
			continue
		}
		var found []string
		validateNode(node, args, "arguments", &found)
		for _, v := range found {
			violations = append(violations, call.Function.Name+": "+v)
		}
	}
	return violations
}

// appendToolRepairInstruction — добавить к системному сообщению перечень
// нарушений схемы для повторной генерации.
//
// R247, этап F (nudge-вариант): слепой повтор тем же промптом помогает не всегда —
// модель снова выбирает ту же форму аргументов. Перечень нарушений в промпте
// делает повтор осмысленным: модель видит, ЧТО именно не приняли.
func appendToolRepairInstruction(msgs []chatMessage, violations []string) []chatMessage {
	if len(violations) == 0 {
		return msgs
	}
	var b strings.Builder
	b.WriteString("Your previous tool call was rejected because its arguments do not match the requested JSON schema:\n")
	for _, v := range violations {
		b.WriteString("- ")
		b.WriteString(v)
		b.WriteString("\n")
	}
	b.WriteString("Call the tool again and return arguments that satisfy the schema exactly (required fields, types, enum, no extra fields). Do not explain.")
	instruction := b.String()

	out := make([]chatMessage, len(msgs))
	copy(out, msgs)
	for i := range out {
		if out[i].Role == "system" {
			out[i].Content = out[i].Content + "\n\n" + instruction
			return out
		}
	}
	return append([]chatMessage{{Role: "system", Content: instruction}}, out...)
}

// strictArgsRetryEnabled — выключатель повтора для невалидных strict-аргументов.
func strictArgsRetryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_STRICT_ARGS_RETRY"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}
