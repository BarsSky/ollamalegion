// tool_angle_form_r119_test.go — R119 (2026-10-15).
//
// ЖИВОЙ ДЕФЕКТ (скриншот оператора, DSH через балансер). Модель отдала вызов
// инструмента angle-формой Hermes/Qwen3-Coder:
//
//	<tool_call> <function=bash> <parameter=command> pwd && ls -la </parameter>
//	<parameter=description> … </parameter> </function> </tool_call>
//
// cppworker эту форму не знал ни одним парсером (Hermes искал JSON внутри
// `<tool_call>`, gemma-native ждал `name{args}`), поэтому вызов уходил клиенту
// текстом и инструмент не исполнялся. Балансер разметку вырезает и синтезирует
// tool_calls на транспорте, но cppworker обязан отдать структуру сам — иначе её
// не увидят ни non-stream ответ, ни прямые клиенты cppworker.
//
// Тест падает до правки: parseToolCallsFromOutputRaw возвращает nil.
package main

import (
	"encoding/json"
	"testing"
)

// angleLiveOutput — строка, максимально близкая к скриншоту оператора.
const angleLiveOutput = `<tool_call> <function=bash> <parameter=command> pwd && ls -la </parameter> <parameter=description> List the current directory </parameter> </function> </tool_call>`

// TestR119_AngleForm_ParsedFromScreenshot — главное: строка со скриншота даёт
// структурный вызов с именем bash и командой внутри arguments.
func TestR119_AngleForm_ParsedFromScreenshot(t *testing.T) {
	calls := parseToolCallsFromOutputRaw(angleLiveOutput)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1 (angle-форма не разобрана — вызов уедет текстом)", len(calls))
	}
	if calls[0].Function.Name != "bash" {
		t.Errorf("name = %q, want bash", calls[0].Function.Name)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	if got, _ := args["command"].(string); got != "pwd && ls -la" {
		t.Errorf("command = %q, want %q (обрамляющие пробелы снимаются, внутренние — нет)",
			got, "pwd && ls -la")
	}
	if got, _ := args["description"].(string); got != "List the current directory" {
		t.Errorf("description = %q, want %q", got, "List the current directory")
	}
}

// TestR119_AngleForm_TruncatedStream — стрим оборвался до </parameter>: вызов всё
// равно должен быть распознан (на скриншоте оператора команда была видна, а
// закрывающие теги — нет).
func TestR119_AngleForm_TruncatedStream(t *testing.T) {
	truncated := `<tool_call> <function=bash> <parameter=command> pwd && ls -la`
	calls := parseToolCallsFromOutputRaw(truncated)
	if len(calls) != 1 {
		t.Fatalf("обрезанный стрим: tool_calls = %d, want 1 (инструмент обязан вызваться, "+
			"пусть и с обрезанным значением)", len(calls))
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	if got, _ := args["command"].(string); got == "" {
		t.Errorf("command пуст: %v", args)
	}
}

// TestR119_AngleForm_MultipleCalls — несколько вызовов в одном ответе.
func TestR119_AngleForm_MultipleCalls(t *testing.T) {
	multi := `<tool_call><function=bash><parameter=command>pwd</parameter></function></tool_call>` +
		`<tool_call><function=read_file><parameter=path>/etc/hosts</parameter></function></tool_call>`
	calls := parseToolCallsFromOutputRaw(multi)
	if len(calls) != 2 {
		t.Fatalf("tool_calls = %d, want 2", len(calls))
	}
	if calls[0].Function.Name != "bash" || calls[1].Function.Name != "read_file" {
		t.Errorf("имена = %q, %q; want bash, read_file", calls[0].Function.Name, calls[1].Function.Name)
	}
}

// TestR119_AngleForm_NoFalsePositives — обычный текст без разметки не должен
// превращаться в вызов инструмента.
func TestR119_AngleForm_NoFalsePositives(t *testing.T) {
	for _, text := range []string{
		"",
		"Просто ответ модели без вызовов.",
		"Функция function= не вызов, а текст.",
		`{"name":"bash","arguments":{"command":"pwd"}}`,
	} {
		if calls := extractHermesAngleToolCalls(text); len(calls) != 0 {
			t.Errorf("ложный вызов на тексте %q: %+v", text, calls)
		}
	}
}

// TestR119_AngleForm_CoexistsWithHermesJSON — JSON-форма Hermes по-прежнему
// разбирается первой стратегией (новая не должна её перехватывать).
func TestR119_AngleForm_CoexistsWithHermesJSON(t *testing.T) {
	jsonForm := `<tool_call>{"name":"bash","arguments":{"command":"pwd"}}</tool_call>`
	calls := parseToolCallsFromOutputRaw(jsonForm)
	if len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Fatalf("JSON-форма Hermes сломана новой стратегией: %+v", calls)
	}
}
