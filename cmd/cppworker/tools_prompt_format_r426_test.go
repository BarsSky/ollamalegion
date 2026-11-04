//go:build llama_stub

// tools_prompt_format_r426_test.go — R426 (2026-11-04): преамбула инструментов
// не должна ДВУСМЫСЛЕННО показывать формат вызова.
//
// ЖИВОЙ ДЕФЕКТ (Cline CLI 4.1.26, провайдер ollama → /api/chat, модель
// Qwen3.8-9B-Distill-uncensored-heretic, 25 инструментов). Модель вместо
// вызова писала текст: «I'll start by reading … [run_commands(commands=["type
// TASK.md"])]», а затем вставила содержимое index.html в ответ. Cline считает
// ответ без tool_calls финальным — в TUI «PLAN → COMPLETED», файлы не созданы.
//
// Причина — двусмысленная преамбула: инструменты перечислялись В ВИДЕ ВЫЗОВА
// («- run_commands(commands)"), и слабые модели копировали список, а не
// JSON-пример. Проверено A/B на стенде: с явной формулировкой модель зовёт
// read_files/editor корректно.
//
// Контракт, который фиксируют тесты ниже:
//  1. преамбула содержит ровно один документированный формат вызова и запрет
//     на name(args)/[name(...)];
//  2. пример из преамбулы РЕАЛЬНО парсится (иначе модель «следует инструкции»,
//     а вызов теряется и превращается в текст — это была бы вторая поломка);
//  3. вызов, вставленный в прозу, всё равно распознаётся;
//  4. текст без вызова остаётся текстом (никаких ложных tool_calls).

package main

import (
	"strings"
	"testing"
)

func formatTestTools() []openAITool {
	return []openAITool{
		{Type: "function", Function: openAIFunction{
			Name:        "read_files",
			Description: "Read files.",
			Parameters:  []byte(`{"type":"object","properties":{"files":{"type":"array","items":{"type":"string"}}},"required":["files"]}`),
		}},
		{Type: "function", Function: openAIFunction{
			Name:        "editor",
			Description: "Create or edit a file.",
			Parameters:  []byte(`{"type":"object","properties":{"path":{"type":"string"},"new_text":{"type":"string"}},"required":["path","new_text"]}`),
		}},
	}
}

// TestR426_ToolsPromptStatesOneFormat — в преамбуле один формат, запрет на
// его имитации и правило «не вставлять артефакт в ответ».
func TestR426_ToolsPromptStatesOneFormat(t *testing.T) {
	prompt := buildToolsSystemPrompt(formatTestTools())

	for _, want := range []string{
		"[TOOL_CALLS][{",
		`"arguments":{`,
		"name(args)",
		"[name(...)]",
		"call the tool instead of pasting",
		"Available tools (signature: description):",
		"- read_files(",
		"- editor(",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("преамбула инструментов не содержит %q:\n%s", want, prompt)
		}
	}
	// Старая формулировка «Output ONLY the JSON array, no prose» мешала обычным
	// текстовым ответам при доступных инструментах — её больше нет.
	if strings.Contains(prompt, "Output ONLY the JSON array") {
		t.Errorf("преамбула сохранила старую безусловную инструкцию:\n%s", prompt)
	}
}

// TestR426_DocumentedExampleIsParsed — пример из преамбулы обязан парситься.
func TestR426_DocumentedExampleIsParsed(t *testing.T) {
	tools := formatTestTools()
	out := `[TOOL_CALLS][{"id":"call_read_files","type":"function","function":{"name":"read_files","arguments":{"files":["TASK.md"]}}}]`

	calls := parseToolCallsFromOutputWithTools(out, tools)
	if len(calls) != 1 {
		t.Fatalf("документированный формат не распознан: %d вызовов\n%s", len(calls), out)
	}
	if calls[0].Function.Name != "read_files" {
		t.Errorf("имя вызова = %q, ожидалось read_files", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "TASK.md") {
		t.Errorf("аргументы потеряны: %q", calls[0].Function.Arguments)
	}
}

// TestR426_CallInsideProseIsParsed — модель может предварить вызов фразой;
// вызов всё равно должен распознаваться.
func TestR426_CallInsideProseIsParsed(t *testing.T) {
	tools := formatTestTools()
	out := "I will read the requirements first.\n" +
		`[TOOL_CALLS][{"name":"read_files","arguments":{"files":["TASK.md"]}}]`

	calls := parseToolCallsFromOutputWithTools(out, tools)
	if len(calls) != 1 || calls[0].Function.Name != "read_files" {
		t.Fatalf("вызов внутри прозы не распознан: %+v\n%s", calls, out)
	}
}

// TestR426_PlainTextStaysText — текст без вызова не превращается в tool_call
// (иначе обычные ответы чат-клиентов ломались бы).
func TestR426_PlainTextStaysText(t *testing.T) {
	tools := formatTestTools()
	for _, out := range []string{
		"Here is a short answer without any call.",
		"Function read_files(files) reads files from disk — это описание, а не вызов.",
	} {
		if calls := parseToolCallsFromOutputWithTools(out, tools); len(calls) != 0 {
			t.Errorf("ложный tool_call из текста %q: %+v", out, calls)
		}
	}
}
