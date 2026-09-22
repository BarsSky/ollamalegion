package main

// tool_calls_r66b_test.go — R66b (2026-09-22): разбор РЕАЛЬНОГО вывода
// маленькой модели (Qwen3-Instruct-2507-q4km, 2.8B), снятого со стрима
// /api/chat во время прогона Cline CLI 3.0.63 (провайдер "ollama", 25 tools).
//
// Модель выдала структурно почти-JSON массив tool_calls с тремя дефектами:
//  1. забыла закрывающую кавычку у значения "arguments";
//  2. поставила лишнюю `}` перед следующим элементом;
//  3. не заэкранировала обратные слэши в Windows-путях (`\O` вместо `\\O`).
//
// До R66b ни одна стратегия парсера такой вывод не распознавала → Cline видел
// tool call как обычный текст и падал с "Model returned empty response".

import (
	"encoding/json"
	"strings"
	"testing"
)

// realClineToolCallOutput — вывод модели ВЕРБАТИМ (из _diag/cline_stream_out.ndjson,
// поле message.content первого done-чанка).
const realClineToolCallOutput = `[{"id":"call_editor","type":"function","function":{"name":"editor","arguments":"{\"path\":\"C:\\Ollama\\ollamalegion\\_diag\\cline-test\\cline_tool_test.txt\",\"new_text\":\"CLINE_TOOL_OK\n\"}}},{"id":"call_read_files","type":"function","function":{"name":"read_files","arguments":"[{\"path\":\"C:\\Ollama\\ollamalegion\\_diag\\cline-test\\cline_tool_test.txt\"}]"}}]`

func TestR66b_RealClineOutput_LooseParse(t *testing.T) {
	calls := parseToolCallsFromOutput(realClineToolCallOutput)
	if len(calls) != 2 {
		t.Fatalf("ожидалось 2 tool_call из реального вывода модели, получено %d: %+v", len(calls), calls)
	}
	if calls[0].Function.Name != "editor" {
		t.Errorf("call[0].name = %q, ожидалось editor", calls[0].Function.Name)
	}
	if calls[1].Function.Name != "read_files" {
		t.Errorf("call[1].name = %q, ожидалось read_files", calls[1].Function.Name)
	}
	if calls[0].ID != "call_editor" {
		t.Errorf("call[0].id = %q, ожидалось call_editor", calls[0].ID)
	}

	// Аргументы обязаны быть ВАЛИДНЫМ JSON-объектом с восстановленными путями.
	var args0 map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args0); err != nil {
		t.Fatalf("arguments[0] не валиден как JSON (%v): %s", err, calls[0].Function.Arguments)
	}
	path, _ := args0["path"].(string)
	if !strings.Contains(path, `C:\Ollama\ollamalegion\_diag\cline-test\cline_tool_test.txt`) {
		t.Errorf("path повреждён (обратные слэши потеряны): %q", path)
	}
	if args0["new_text"] != "CLINE_TOOL_OK\n" {
		t.Errorf("new_text = %q, ожидалось \"CLINE_TOOL_OK\\n\"", args0["new_text"])
	}

	var args1 []map[string]interface{}
	if err := json.Unmarshal([]byte(calls[1].Function.Arguments), &args1); err != nil {
		t.Fatalf("arguments[1] не валиден как JSON (%v): %s", err, calls[1].Function.Arguments)
	}
	if len(args1) != 1 || !strings.Contains(args1[0]["path"].(string), "cline_tool_test.txt") {
		t.Errorf("arguments[1] разобраны неверно: %v", args1)
	}
}

// TestR66b_RepairJSONText — юнит-тесты починки JSON.
func TestR66b_RepairJSONText(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		valid bool
	}{
		{"windows backslashes", `{"path":"C:\Ollama\models"}`, true},
		{"missing closing brace", `{"path":"a.txt"`, true},
		{"raw newline in string", "{\"text\":\"line1\nline2\"}", true},
		{"already valid", `{"path":"C:\\Ollama\\models"}`, true},
		{"trailing junk brace", `{"path":"a.txt"}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := repairJSONText(tc.in)
			if tc.valid && !json.Valid([]byte(out)) {
				t.Errorf("repairJSONText(%q) = %q — не валидный JSON", tc.in, out)
			}
		})
	}

	// Проверяем, что путь восстановлен с корректными слэшами.
	fixed := repairJSONText(`{"path":"C:\Ollama\models"}`)
	var m map[string]string
	if err := json.Unmarshal([]byte(fixed), &m); err != nil {
		t.Fatalf("не удалось разобрать починенный JSON %q: %v", fixed, err)
	}
	if m["path"] != `C:\Ollama\models` {
		t.Errorf("path = %q, ожидалось C:\\Ollama\\models", m["path"])
	}
}

// TestR66b_StringifyArguments_UnwrapsDoubleEncoded — Qwen3 кладёт в arguments
// JSON-строку, содержимое которой сам JSON-объект. Клиент (Cline) должен
// получить объект, а не строку — иначе вызов инструмента уходит без аргументов.
func TestR66b_StringifyArguments_UnwrapsDoubleEncoded(t *testing.T) {
	// Значение после парсинга outer-JSON: '"{\"path\":\"a.txt\"}"'
	doubleEncoded := `"{\"path\":\"a.txt\"}"`
	got := stringifyArguments(doubleEncoded)

	var m map[string]string
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("arguments должны быть JSON-объектом, получено %q (ошибка: %v)", got, err)
	}
	if m["path"] != "a.txt" {
		t.Errorf("path = %q, ожидалось a.txt", m["path"])
	}
}

// TestR66b_LooseParse_NotTriggeredOnPlainText — терпимый парсер не должен
// превращать в tool_calls обычный текст с JSON в прозе.
func TestR66b_LooseParse_NotTriggeredOnPlainText(t *testing.T) {
	texts := []string{
		"Вот пример конфигурации: {\"name\":\"test\",\"arguments\":{}} — используйте его.",
		"Просто текст без инструментов.",
	}
	for _, txt := range texts {
		if calls := parseToolCallsFromOutput(txt); len(calls) > 0 {
			t.Errorf("ложное срабатывание на тексте %q: %+v", txt, calls)
		}
	}
}
