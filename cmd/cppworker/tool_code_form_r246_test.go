// tool_code_form_r246_test.go — R246 (2026-10-26): диалект gemma `tool_code`.
//
// ЖИВОЙ ДЕФЕКТ (DeepSeek Harness → балансер → cppworker → gemma-4, 25
// инструментов, потоковый /v1/chat/completions): модель отдала вызов
// питоновским синтаксисом в теле ответа
//
//	[tool_code]
//	print(read(file_path="go.mod"))
//	[/tool_code]
//
// Ни один парсер такого не знал: вызов уехал клиенту ТЕКСТОМ, message.tool_calls
// не пришёл, агент остановился с ошибкой «инструмент не вызван».
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// toolCodeLiveString — точная форма из живой жалобы.
const toolCodeLiveString = "[tool_code]\nprint(read(file_path=\"go.mod\"))\n[/tool_code]"

// TestR246_ToolCodeParsed — вызов разобран, аргументы валидны.
func TestR246_ToolCodeParsed(t *testing.T) {
	calls := extractToolCodeCalls(toolCodeLiveString)
	if len(calls) != 1 {
		t.Fatalf("разобрано вызовов: %d, ожидался 1: %+v", len(calls), calls)
	}
	if calls[0].Function.Name != "read" {
		t.Fatalf("имя = %q, ожидалось read", calls[0].Function.Name)
	}
	if calls[0].Type != "function" {
		t.Errorf("type = %q, ожидалось function", calls[0].Type)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	if got, _ := args["file_path"].(string); got != "go.mod" {
		t.Errorf("arguments[file_path] = %v, ожидалось go.mod", args["file_path"])
	}
}

// TestR246_ToolCodeForms — варианты обёрток и синтаксиса.
func TestR246_ToolCodeForms(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{"фенс по-питоновски", "```tool_code\nprint(read(file_path=\"go.mod\"))\n```", 1},
		{"без print", "[tool_code]\nread(file_path=\"go.mod\")\n[/tool_code]", 1},
		{"несколько вызовов в блоке", "[tool_code]\nread(file_path=\"a\")\nread(file_path=\"b\")\n[/tool_code]", 2},
		{"два блока", "[tool_code]\nread(file_path=\"a\")\n[/tool_code]\n[tool_code]\nread(file_path=\"b\")\n[/tool_code]", 2},
		{"одинарные кавычки", "[tool_code]\nread(file_path='go.mod')\n[/tool_code]", 1},
		{"числа и bool", "[tool_code]\nread(file_path=\"x\", offset=3, limit=5)\n[/tool_code]", 1},
		{"незакрытый блок (обрыв max_tokens)", "[tool_code]\nread(file_path=\"go.mod\")", 1},
		{"проза вокруг", "Сейчас прочитаю.\n[tool_code]\nread(file_path=\"go.mod\")\n[/tool_code]\nГотово.", 1},
		{"обычный текст", "Просто ответ модели", 0},
		{"фенс другого языка", "```go\nfmt.Println(1)\n```", 0},
		{"пустой блок", "[tool_code]\n[/tool_code]", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := extractToolCodeCalls(tc.input)
			if len(calls) != tc.want {
				t.Fatalf("вызовов %d, ожидалось %d: %+v", len(calls), tc.want, calls)
			}
			for _, call := range calls {
				if call.Function.Name == "" {
					t.Errorf("пустое имя вызова: %+v", call)
				}
				var args map[string]interface{}
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
					t.Errorf("arguments не JSON: %v (%q)", err, call.Function.Arguments)
				}
			}
		})
	}
}

// TestR246_ToolCodeNumberAndBool — типы значений не превращаются в строки.
func TestR246_ToolCodeNumberAndBool(t *testing.T) {
	calls := extractToolCodeCalls("[tool_code]\nread(file_path=\"x\", offset=3, replace_all=True)\n[/tool_code]")
	if len(calls) != 1 {
		t.Fatalf("вызовов %d, ожидался 1", len(calls))
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v", err)
	}
	if got, ok := args["replace_all"].(bool); !ok || !got {
		t.Errorf("replace_all = %#v, ожидалось true", args["replace_all"])
	}
	if got := args["offset"]; got == nil || got == "" {
		t.Errorf("offset потерян: %#v", got)
	}
}

// TestR246_ToolCodeStrippedFromVisible — разметка не остаётся в видимом ответе.
func TestR246_ToolCodeStrippedFromVisible(t *testing.T) {
	cases := []struct{ in, want string }{
		{toolCodeLiveString, ""},
		{"Сейчас прочитаю.\n[tool_code]\nread(file_path=\"go.mod\")\n[/tool_code]", "Сейчас прочитаю."},
		{"```tool_code\nread(file_path=\"go.mod\")\n```", ""},
		{"обычный ответ без разметки", "обычный ответ без разметки"},
		{"```go\nfmt.Println(1)\n```", "```go\nfmt.Println(1)\n```"},
	}
	for _, tc := range cases {
		if got := stripToolCodeMarkup(tc.in); got != tc.want {
			t.Errorf("stripToolCodeMarkup(%q) = %q, ожидалось %q", tc.in, got, tc.want)
		}
	}
}

// TestR246_ToolCodeMarkupRecognized — разметка видна как разметка вызова.
func TestR246_ToolCodeMarkupRecognized(t *testing.T) {
	if !looksLikeToolCodeMarkup(toolCodeLiveString) {
		t.Error("tool_code не распознан как разметка вызова")
	}
	if looksLikeToolCodeMarkup("обычный текст ответа") {
		t.Error("обычный текст принят за разметку")
	}
	if !looksLikeToolCallMarkup(toolCodeLiveString) {
		t.Error("общий детектор разметки не видит tool_code")
	}
	// Вырезание через общий путь (его вызывают все SSE/Ollama-выходы).
	if cleaned := stripToolCallMarkup(toolCodeLiveString); strings.Contains(cleaned, "tool_code") {
		t.Errorf("в видимом тексте осталась разметка: %q", cleaned)
	}
}

// TestR246_ToolCodeEndToEndParse — вызов находится общим парсером запроса.
func TestR246_ToolCodeEndToEndParse(t *testing.T) {
	tools := []openAITool{{
		Type: "function",
		Function: openAIFunction{
			Name:       "read",
			Parameters: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`),
		},
	}}
	calls := parseToolCallsForRequest(toolCodeLiveString, tools)
	if len(calls) != 1 {
		t.Fatalf("parseToolCallsForRequest вернул %d вызовов, ожидался 1", len(calls))
	}
	if calls[0].Function.Name != "read" {
		t.Fatalf("имя = %q, ожидалось read", calls[0].Function.Name)
	}
}
