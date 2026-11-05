// items_coerce_r435_test.go — R435 (2026-11-05): массив объектов-обёрток
// приводится к массиву строк, когда схема объявляет `items: string`.
//
// ФИКСТУРА — живой ответ gemma-4 после включения НАТИВНОЙ секции инструментов в
// chat template (R434): при схеме read_files `files: {type: array,
// items: {type: string}}` модель выдала
//
//	{"files": [{"path": "TASK.md"}]}
//
// Клиент такой вызов выполнить не может (валидатор ждёт массив строк), и агент
// тратит ход. Здесь фиксируется и починка, и её ГРАНИЦА: массив настоящих
// объектов и массив без объявленного items не трогаем.
package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestR435_WrapperArrayBecomesStrings — обёртки `{path: …}` → строки.
func TestR435_WrapperArrayBecomesStrings(t *testing.T) {
	calls := []openAIToolCall{{
		Type: "function",
		Function: openAIFunctionCall{
			Name:      "read_files",
			Arguments: `{"files": [{"path": "TASK.md"}, {"path": "README.md"}]}`,
		},
	}}
	out := coerceToolCallsToSchema(calls, formatTestTools())
	if len(out) != 1 {
		t.Fatalf("вызовов=%d", len(out))
	}
	var args struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(out[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не соответствуют схеме (%q): %v", out[0].Function.Arguments, err)
	}
	if len(args.Files) != 2 || args.Files[0] != "TASK.md" || args.Files[1] != "README.md" {
		t.Fatalf("files = %v", args.Files)
	}
}

// TestR435_RealObjectArrayUntouched — массив объектов, у которых больше одного
// поля, менять нельзя: это законная форма другой схемы.
func TestR435_RealObjectArrayUntouched(t *testing.T) {
	schema := []openAITool{{Type: "function", Function: openAIFunction{
		Name:       "search",
		Parameters: json.RawMessage(`{"type":"object","properties":{"hits":{"type":"array","items":{"type":"object"}}},"required":["hits"]}`),
	}}}
	// Значение сравниваем по СМЫСЛУ: приведение перекодирует JSON компактно.
	raw := `{"hits": [{"file": "a.go", "line": 3}]}`
	out := coerceToolCallsToSchema([]openAIToolCall{{Type: "function",
		Function: openAIFunctionCall{Name: "search", Arguments: raw}}}, schema)
	if len(out) != 1 {
		t.Fatalf("вызовов=%d", len(out))
	}
	var got, want map[string]interface{}
	_ = json.Unmarshal([]byte(out[0].Function.Arguments), &got)
	_ = json.Unmarshal([]byte(raw), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("законный массив объектов изменён: %v", got)
	}
}

// TestR435_NoItemsDeclaredUntouched — items не объявлен → не угадываем.
func TestR435_NoItemsDeclaredUntouched(t *testing.T) {
	schema := []openAITool{{Type: "function", Function: openAIFunction{
		Name:       "run",
		Parameters: json.RawMessage(`{"type":"object","properties":{"commands":{"type":"array"}},"required":["commands"]}`),
	}}}
	raw := `{"commands": [{"cmd": "ls"}]}`
	out := coerceToolCallsToSchema([]openAIToolCall{{Type: "function",
		Function: openAIFunctionCall{Name: "run", Arguments: raw}}}, schema)
	if len(out) != 1 {
		t.Fatalf("вызовов=%d", len(out))
	}
	var got, want map[string]interface{}
	_ = json.Unmarshal([]byte(out[0].Function.Arguments), &got)
	_ = json.Unmarshal([]byte(raw), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("массив без объявленного items изменён: %v", got)
	}
}

// TestR435_ParamsKeyAccepted — форма `{"tool_name":…,"params":{…}}` (живой
// ответ gemma-4 в нативном режиме) разбирается как вызов.
func TestR435_ParamsKeyAccepted(t *testing.T) {
	raw := "```json\n[\n  {\n    \"tool_name\": \"read_files\",\n    \"params\": {\n      \"files\": [{\"path\": \"TASK.md\"}]\n    }\n  }\n]\n```"
	calls := parseToolCallsForRequest(raw, formatTestTools())
	if len(calls) != 1 || calls[0].Function.Name != "read_files" {
		t.Fatalf("форма tool_name+params не разобрана: %+v", calls)
	}
	var args struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("аргументы не по схеме (%q): %v", calls[0].Function.Arguments, err)
	}
	if len(args.Files) != 1 || args.Files[0] != "TASK.md" {
		t.Fatalf("files = %v", args.Files)
	}
}
