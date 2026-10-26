// tool_args_schema_r246_test.go — R246 (2026-10-26): аргументы вызова приводятся
// к объявленной схеме так, чтобы валидатор клиента их принял.
//
// Живые дефекты (полная матрица инструментов на стенде, gemma-4):
//   - null у необязательного поля: ralph {"maxRounds":null} при схеме
//     `maxRounds: number` → клиент отвечал ошибкой валидации;
//   - обязательное поле в множественном написании: схема `query: string`, модель
//     отдала {"queries":["ollama"]} → «нет обязательного поля query».
package main

import (
	"encoding/json"
	"testing"
)

// TestR246_WorkerDropsNullOptional — null у необязательного поля снимается.
func TestR246_WorkerDropsNullOptional(t *testing.T) {
	tools := []openAITool{
		toolSchemaR246("ralph", []string{"objective"},
			map[string]interface{}{
				"objective":  map[string]interface{}{"type": "string"},
				"maxRounds":  map[string]interface{}{"type": "number"},
				"blocked_on": map[string]interface{}{"type": "string"},
			}),
	}
	calls := coerceToolCallsToSchema([]openAIToolCall{{
		Type: "function",
		Function: openAIFunctionCall{
			Name:      "ralph",
			Arguments: `{"objective":"probe","maxRounds":null,"blocked_on":null}`,
		},
	}}, tools)
	if len(calls) != 1 {
		t.Fatalf("вызовов %d, ожидался 1", len(calls))
	}
	args := argsOfR246(t, calls[0])
	if _, present := args["maxRounds"]; present {
		t.Errorf("maxRounds с null остался: %q", calls[0].Function.Arguments)
	}
	if _, present := args["blocked_on"]; present {
		t.Errorf("blocked_on с null остался: %q", calls[0].Function.Arguments)
	}
	if got, _ := args["objective"].(string); got != "probe" {
		t.Errorf("objective потерян: %q", calls[0].Function.Arguments)
	}
}

// TestR246_WorkerKeepsNullRequired — null в обязательном поле не трогаем.
func TestR246_WorkerKeepsNullRequired(t *testing.T) {
	tools := []openAITool{
		toolSchemaR246("read", []string{"file_path"},
			map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}}),
	}
	calls := coerceToolCallsToSchema([]openAIToolCall{{
		Type:     "function",
		Function: openAIFunctionCall{Name: "read", Arguments: `{"file_path":null}`},
	}}, tools)
	if len(calls) != 1 {
		t.Fatalf("вызовов %d, ожидался 1", len(calls))
	}
	args := argsOfR246(t, calls[0])
	if _, present := args["file_path"]; !present {
		t.Errorf("обязательное поле с null удалено (нельзя): %q", calls[0].Function.Arguments)
	}
}

// TestR246_WorkerPluralAlias — queries → query c приведением к строке.
func TestR246_WorkerPluralAlias(t *testing.T) {
	tools := []openAITool{
		toolSchemaR246("web_search", []string{"query"},
			map[string]interface{}{"query": map[string]interface{}{"type": "string"}}),
	}
	calls := coerceToolCallsToSchema([]openAIToolCall{{
		Type:     "function",
		Function: openAIFunctionCall{Name: "web_search", Arguments: `{"queries":["ollama"]}`},
	}}, tools)
	if len(calls) != 1 {
		t.Fatalf("вызовов %d, ожидался 1", len(calls))
	}
	args := argsOfR246(t, calls[0])
	if got, _ := args["query"].(string); got != "ollama" {
		t.Errorf("query = %#v, ожидалась строка ollama (%q)", args["query"], calls[0].Function.Arguments)
	}
	if _, present := args["queries"]; present {
		t.Errorf("старое поле queries осталось: %q", calls[0].Function.Arguments)
	}
}

// toolSchemaR246 — инструмент с объявленными свойствами и required.
func toolSchemaR246(name string, required []string, props map[string]interface{}) openAITool {
	params, _ := json.Marshal(map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	})
	return openAITool{
		Type: "function",
		Function: openAIFunction{
			Name:       name,
			Parameters: params,
		},
	}
}

// argsOfR246 — аргументы вызова как объект.
func argsOfR246(t *testing.T, call openAIToolCall) map[string]interface{} {
	t.Helper()
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, call.Function.Arguments)
	}
	return args
}
