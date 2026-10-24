//go:build llama_stub

// gemma_tool_call_formats_r109_test.go — R109 (2026-10-13): распознавание двух
// форм вызова инструмента, которые gemma-4 отдаёт живьём, а парсер не знал.
//
// Обе формы найдены прогоном НАСТОЯЩЕГО клиента Cline CLI 3.0.70 через балансер
// (Ollama API, gemma-4-E4B-it-Q4_K_M, 18 инструментов):
//
//  1. стрим, ответ начинался с размышлений:
//     <|tool_call>call:run_commands{commands:[<|"|>dir<|"|>]}<tool_call|>
//     → Cline: hadToolCalls=false, toolCallCount=0, вызов показан текстом
//     («команда инструмента в квадратных скобках» — исходная жалоба).
//
//  2. нестримовый путь, та же модель:
//     [ { "tool_call": "read_files", "args": { "path": "." } } ]
//     → уехало в content с done_reason:"stop", message.tool_calls не пришёл.
package main

import (
	"encoding/json"
	"testing"
)

// TestR109_GemmaNativePipeTagToolCall — нативный синтаксис gemma-4: теги с
// пайпами, префикс `call:`, токены кавычек <|"|>.
func TestR109_GemmaNativePipeTagToolCall(t *testing.T) {
	output := `<|tool_call>call:run_commands{commands:[<|"|>dir<|"|>]}<tool_call|>`

	calls := parseToolCallsFromOutput(output)
	if len(calls) != 1 {
		t.Fatalf("распознано вызовов: %d, ожидался 1 (форма gemma-4 не разобрана: %q)", len(calls), output)
	}
	if got := calls[0].Function.Name; got != "run_commands" {
		t.Errorf("имя функции = %q, ожидалось run_commands (префикс call: не снят)", got)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON (%q): %v", calls[0].Function.Arguments, err)
	}
	cmds, _ := args["commands"].([]interface{})
	if len(cmds) != 1 || cmds[0] != "dir" {
		t.Errorf("arguments = %q, ожидалось {\"commands\":[\"dir\"]} (токены <|\"|> не преобразованы)",
			calls[0].Function.Arguments)
	}
}

// TestR109_GemmaNativeTagWithoutCallPrefix — тот же тег, но без префикса `call:`
// и с уже валидным JSON внутри.
func TestR109_GemmaNativeTagWithoutCallPrefix(t *testing.T) {
	output := `<|tool_call>read_files{"path": "."}<tool_call|>`
	calls := parseToolCallsFromOutput(output)
	if len(calls) != 1 || calls[0].Function.Name != "read_files" {
		t.Fatalf("распознано %d вызовов, ожидался read_files: %+v", len(calls), calls)
	}
}

// TestR109_ToolCallArgsShape — JSON-форма gemma-4 «tool_call + args» (в массиве
// и одиночным объектом), с проверкой, что объект прозы не принимается за вызов.
func TestR109_ToolCallArgsShape(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		wantName  string
		wantArgs  string
		wantCalls int
	}{
		{
			name:      "массив из одного вызова",
			output:    "[\n  {\n    \"tool_call\": \"read_files\",\n    \"args\": {\n      \"path\": \".\"\n    }\n  }\n]",
			wantName:  "read_files",
			wantArgs:  `{"path":"."}`,
			wantCalls: 1,
		},
		{
			name:      "одиночный объект",
			output:    `{"tool_call": "run_commands", "args": {"commands": ["dir"]}}`,
			wantName:  "run_commands",
			wantArgs:  `{"commands":["dir"]}`,
			wantCalls: 1,
		},
		{
			name:      "tool_name вместо tool_call",
			output:    `{"tool_name": "editor", "args": {"path": "a.txt"}}`,
			wantName:  "editor",
			wantArgs:  `{"path":"a.txt"}`,
			wantCalls: 1,
		},
		{
			name:      "без ключа вызова — не вызов",
			output:    `{"path": ".", "args": {"x": 1}}`,
			wantCalls: 0,
		},
		{
			name:      "обычная проза с JSON — не вызов",
			output:    `Файлов 3. Вот структура: {"path": ".", "count": 3}`,
			wantCalls: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := parseToolCallsFromOutput(tc.output)
			if len(calls) != tc.wantCalls {
				t.Fatalf("распознано %d вызовов, ожидалось %d (вывод: %q)", len(calls), tc.wantCalls, tc.output)
			}
			if tc.wantCalls == 0 {
				return
			}
			if calls[0].Function.Name != tc.wantName {
				t.Errorf("имя = %q, ожидалось %q", calls[0].Function.Name, tc.wantName)
			}
			var got, want interface{}
			if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &got); err != nil {
				t.Fatalf("arguments не JSON (%q): %v", calls[0].Function.Arguments, err)
			}
			if err := json.Unmarshal([]byte(tc.wantArgs), &want); err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(got, want) {
				t.Errorf("arguments = %q, ожидалось %q", calls[0].Function.Arguments, tc.wantArgs)
			}
		})
	}
}

// TestR109_StreamHoldbackKnowsGemmaOpenTag — маркер <|tool_call> обязан быть в
// списке удержания, иначе блок уезжает клиенту content-дельтами.
func TestR109_StreamHoldbackKnowsGemmaOpenTag(t *testing.T) {
	found := false
	for _, m := range toolsStreamMarkers {
		if m == "<|tool_call>" {
			found = true
			break
		}
	}
	if !found {
		t.Error("в toolsStreamMarkers нет <|tool_call>: нативный маркер gemma-4 не удерживается, " +
			"и вызов инструмента уходит клиенту как обычный текст")
	}
}

func jsonEqual(a, b interface{}) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}
