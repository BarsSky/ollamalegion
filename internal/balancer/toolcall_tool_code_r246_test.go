// toolcall_tool_code_r246_test.go — R246 (2026-10-26): диалект gemma `tool_code`
// на границе балансера.
//
// ЖИВОЙ ДЕФЕКТ (DeepSeek Harness → балансер → gemma-4, 25 инструментов):
// ответ содержал
//
//	[tool_code]
//	print(read(file_path="go.mod"))
//	[/tool_code]
//
// Ни один детектор балансера такой формы не знал: вызов уехал клиенту как
// content, message.tool_calls не пришёл, агент не смог применить инструмент.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

const toolCodeLiveContent = "[tool_code]\nprint(read(file_path=\"go.mod\"))\n[/tool_code]"

// TestR246_ToolCodeDetected — вызов извлекается из content.
func TestR246_ToolCodeDetected(t *testing.T) {
	calls, remaining, found := detectAndExtractToolCallsFromContent(toolCodeLiveContent)
	if !found {
		t.Fatal("tool_code не распознан как вызов инструмента")
	}
	if len(calls) != 1 {
		t.Fatalf("вызовов %d, ожидался 1", len(calls))
	}
	call, ok := calls[0].(map[string]interface{})
	if !ok {
		t.Fatalf("вызов не map: %T", calls[0])
	}
	fn, ok := call["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("нет function: %+v", call)
	}
	if fn["name"] != "read" {
		t.Fatalf("имя = %v, ожидалось read", fn["name"])
	}
	argsStr, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("arguments не строка: %T", fn["arguments"])
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, argsStr)
	}
	if got, _ := args["file_path"].(string); got != "go.mod" {
		t.Errorf("arguments[file_path] = %v, ожидалось go.mod", args["file_path"])
	}
	if remaining != "" {
		t.Errorf("остаток = %q, ожидалась пустая строка", remaining)
	}
}

// TestR246_ToolCodeVariants — обёртки и синтаксис.
func TestR246_ToolCodeVariants(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"фенс", "```tool_code\nprint(read(file_path=\"a\"))\n```", 1},
		{"без print", "[tool_code]\nread(file_path=\"a\")\n[/tool_code]", 1},
		{"два вызова", "[tool_code]\nread(file_path=\"a\")\nread(file_path=\"b\")\n[/tool_code]", 2},
		{"одинарные кавычки", "[tool_code]\nread(file_path='a')\n[/tool_code]", 1},
		{"незакрытый блок", "[tool_code]\nread(file_path=\"a\")", 1},
		{"текст без разметки", "обычный ответ модели", 0},
		{"фенс другого языка", "```go\nfmt.Println(1)\n```", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, _, found := detectAndExtractToolCallsFromContent(tc.in)
			if tc.want == 0 {
				if found {
					t.Fatalf("ложное срабатывание детектора: %+v", calls)
				}
				return
			}
			if !found {
				t.Fatalf("вызов не найден: %q", tc.in)
			}
			if len(calls) != tc.want {
				t.Fatalf("вызовов %d, ожидалось %d", len(calls), tc.want)
			}
		})
	}
}

// TestR246_ToolCodeDoesNotLeakIntoVisible — разметка вырезается из видимого
// текста (её не должен получить клиент как ответ).
func TestR246_ToolCodeDoesNotLeakIntoVisible(t *testing.T) {
	visible := cleanContentAfterToolCallExtraction("Сейчас прочитаю.\n" + toolCodeLiveContent)
	if strings.Contains(visible, "tool_code") {
		t.Errorf("разметка tool_code уехала в видимый текст: %q", visible)
	}
	if !strings.Contains(visible, "Сейчас прочитаю.") {
		t.Errorf("проза потеряна при вырезании вызова: %q", visible)
	}
}
