// array_string_json_r437_test.go — R437 (2026-11-05): строка с JSON-массивом в
// поле, объявленном как массив, разворачивается в массив.
//
// ФИКСТУРА — живой ответ Qwen3.8-9B на стенде после включения нативной секции
// инструментов в chat template (R434):
//
//	{"files": ["[{\"path\": \"C:\\\\Ollama\\\\...\\\\TASK.md\"}]"]}
//
// До фикса клиент получал массив из одной строки с JSON внутри и выполнить такой
// вызов не мог.
package main

import (
	"encoding/json"
	"testing"
)

// readFilesObjectItemsR437 — схема read_files как у Cline: files — массив
// ОБЪЕКТОВ ({path, start_line, end_line}), а не строк.
func readFilesObjectItemsR437() []openAITool {
	return []openAITool{{Type: "function", Function: openAIFunction{
		Name: "read_files",
		Parameters: json.RawMessage(`{"type":"object","properties":{"files":{"type":"array",` +
			`"items":{"type":"object","properties":{"path":{"type":"string"}},` +
			`"required":["path"]}}},"required":["files"]}`),
	}}}
}

// TestR437_StringifiedArrayBecomesArray — строка-массив → массив объектов.
func TestR437_StringifiedArrayBecomesArray(t *testing.T) {
	calls := []openAIToolCall{{
		Type: "function",
		Function: openAIFunctionCall{
			Name:      "read_files",
			Arguments: `{"files": ["[{\"path\": \"TASK.md\"}]"]}`,
		},
	}}
	out := coerceToolCallsToSchema(calls, readFilesObjectItemsR437())
	if len(out) != 1 {
		t.Fatalf("вызовов=%d", len(out))
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(out[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, out[0].Function.Arguments)
	}
	files, ok := args["files"].([]interface{})
	if !ok {
		t.Fatalf("files не массив: %T", args["files"])
	}
	if len(files) != 1 {
		t.Fatalf("files = %v, ожидался массив из одного объекта", files)
	}
	obj, ok := files[0].(map[string]interface{})
	if !ok {
		t.Fatalf("элемент массива не объект: %T (%v)", files[0], files[0])
	}
	if obj["path"] != "TASK.md" {
		t.Fatalf("path = %v", obj["path"])
	}
}

// TestR437_PlainStringInArrayFieldStillWrapped — одиночная строка, не являющаяся
// JSON-массивом, по-прежнему оборачивается в массив (поведение R114d).
func TestR437_PlainStringInArrayFieldStillWrapped(t *testing.T) {
	calls := []openAIToolCall{{
		Type: "function",
		Function: openAIFunctionCall{
			Name:      "read_files",
			Arguments: `{"files": "TASK.md"}`,
		},
	}}
	out := coerceToolCallsToSchema(calls, formatTestTools())
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(out[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v", err)
	}
	files, ok := args["files"].([]interface{})
	if !ok || len(files) != 1 || files[0] != "TASK.md" {
		t.Fatalf("files = %v", args["files"])
	}
}
