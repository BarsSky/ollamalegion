// gemma_quotes_r432_test.go — R432 (2026-11-05): нативный вызов gemma/Qwen с
// ПИТОНОВСКИМИ кавычками и спец-токенами кавычек разбирается.
//
// ФИКСТУРЫ — живые захваты /app/diag (Cline, стенд):
//
//	gemma-4:  <|tool_call>call:read_files(files:[<|"|>TASK.md<|"|>])
//	Qwen3.8:  <tool_call>read_files(files: ['C:\\Ollama\\…\\TASK.md'])
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR432_GemmaQuoteTokens — спец-токены кавычек gemma (`<|"|>`) внутри
// аргументов не мешают разбору.
func TestR432_GemmaQuoteTokens(t *testing.T) {
	out := `<|tool_call>call:read_files(files:[<|"|>TASK.md<|"|>])`
	calls := parseToolCallsForRequest(out, formatTestTools())
	if len(calls) != 1 || calls[0].Function.Name != "read_files" {
		t.Fatalf("вызов не разобран: %+v", calls)
	}
	if !strings.Contains(calls[0].Function.Arguments, "TASK.md") {
		t.Fatalf("аргументы потеряны: %q", calls[0].Function.Arguments)
	}
}

// TestR432_SingleQuotedArgs — питоновские кавычки в аргументах.
func TestR432_SingleQuotedArgs(t *testing.T) {
	out := `<tool_call>read_files(files: ['C:\\Ollama\\cline-accept-r423\\TASK.md'])`
	calls := parseToolCallsForRequest(out, formatTestTools())
	if len(calls) != 1 || calls[0].Function.Name != "read_files" {
		t.Fatalf("вызов не разобран: %+v", calls)
	}
	if !strings.Contains(calls[0].Function.Arguments, `TASK.md`) {
		t.Fatalf("аргументы потеряны: %q", calls[0].Function.Arguments)
	}
	// Путь Windows приезжает к клиенту одной строкой (экранирование JSON снято).
	var args struct{ Files []string }
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	if len(args.Files) != 1 || !strings.HasSuffix(args.Files[0], "TASK.md") {
		t.Fatalf("files = %v", args.Files)
	}
}

// TestR432_ArrayValueKeepsBracket — хвостовое значение-МАССИВ не должно
// терять закрывающую скобку (прежний trim снимал её как «мусор»).
func TestR432_ArrayValueKeepsBracket(t *testing.T) {
	out := `<|tool_call>call:run_commands(commands:["node","--check","app.js"])`
	tools := []openAITool{{Type: "function", Function: openAIFunction{
		Name:       "run_commands",
		Parameters: []byte(`{"type":"object","properties":{"commands":{"type":"array","items":{"type":"string"}}},"required":["commands"]}`),
	}}}
	calls := parseToolCallsForRequest(out, tools)
	if len(calls) != 1 || calls[0].Function.Name != "run_commands" {
		t.Fatalf("вызов не разобран: %+v", calls)
	}
	if !strings.Contains(calls[0].Function.Arguments, "app.js") {
		t.Fatalf("аргументы потеряны: %q", calls[0].Function.Arguments)
	}
}

// TestR432_TrimStillRemovesRealGarbage — регресс R428: хвостовой мусор модели
// (`"}]` без баланса) по-прежнему срезается.
func TestR432_TrimStillRemovesRealGarbage(t *testing.T) {
	out := `<|tool_call>call:editor(new_text: "<html>x</html>", path: "a.txt"}]`
	tools := []openAITool{{Type: "function", Function: openAIFunction{
		Name:       "editor",
		Parameters: []byte(`{"type":"object","properties":{"path":{"type":"string"},"new_text":{"type":"string"}}}`),
	}}}
	calls := parseToolCallsForRequest(out, tools)
	if len(calls) != 1 || calls[0].Function.Name != "editor" {
		t.Fatalf("вызов не разобран: %+v", calls)
	}
	if !strings.Contains(calls[0].Function.Arguments, "a.txt") {
		t.Fatalf("аргументы потеряны: %q", calls[0].Function.Arguments)
	}
}
