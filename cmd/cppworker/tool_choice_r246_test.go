// tool_choice_r246_test.go — R246 (2026-10-26): вынужденный выбор инструмента.
//
// ЖИВОЙ ДЕФЕКТ (проверка формы запросов разных клиентов на стенде): запрос с
// `tool_choice: {"type":"function","function":{"name":"read"}}` и нейтральной
// репликой возвращал текст и ноль вызовов. Поле принималось, но применялось
// только `"none"` (R66b). Клиенты, которые ОБЯЗЫВАЮТ вызвать инструмент
// (структурированный вывод LangChain, MCP-мосты, плагины OpenAI SDK), получали
// неверный ответ.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR246_ParseToolChoice — разбор всех форм поля.
func TestR246_ParseToolChoice(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want toolChoiceDirective
	}{
		{"auto", `"auto"`, toolChoiceDirective{}},
		{"none", `"none"`, toolChoiceDirective{}},
		{"required", `"required"`, toolChoiceDirective{Required: true}},
		{"вынужденная функция", `{"type":"function","function":{"name":"read"}}`, toolChoiceDirective{Forced: "read"}},
		{"плоская форма", `{"type":"function","name":"grep"}`, toolChoiceDirective{Forced: "grep"}},
		{"пусто", ``, toolChoiceDirective{}},
		{"null", `null`, toolChoiceDirective{}},
		{"мусор", `{"type":"tool"}`, toolChoiceDirective{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseToolChoice(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("parseToolChoice(%s) = %+v, ожидалось %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestR246_ToolChoiceInstruction — требование формулируется явно.
func TestR246_ToolChoiceInstruction(t *testing.T) {
	forced := toolChoiceDirective{Forced: "read"}.instruction()
	if !strings.Contains(forced, "MUST") || !strings.Contains(forced, `"read"`) {
		t.Errorf("вынужденный вызов не назван в требовании: %q", forced)
	}
	required := toolChoiceDirective{Required: true}.instruction()
	if !strings.Contains(required, "MUST") {
		t.Errorf("tool_choice=required не стал требованием: %q", required)
	}
	if instr := (toolChoiceDirective{}).instruction(); instr != "" {
		t.Errorf("auto/none не должны давать требование: %q", instr)
	}
}

// TestR246_AugmentAddsToolChoiceInstruction — требование попадает в промпт
// (в существующее system-сообщение), а без вынужденного выбора — не попадает.
func TestR246_AugmentAddsToolChoiceInstruction(t *testing.T) {
	tools := []openAITool{{
		Type: "function",
		Function: openAIFunction{
			Name:       "read",
			Parameters: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`),
		},
	}}
	msgs := []chatMessage{{Role: "system", Content: "base"}, {Role: "user", Content: "hi"}}

	plain := augmentSystemWithToolsForChoice(msgs, tools, nil)
	if strings.Contains(plain[0].Content, "MUST") {
		t.Errorf("без tool_choice требование не должно появляться: %q", plain[0].Content)
	}
	if !strings.Contains(plain[0].Content, "read(file_path:str)") {
		t.Errorf("преамбула инструментов потеряна: %q", plain[0].Content)
	}

	forced := augmentSystemWithToolsForChoice(msgs, tools, json.RawMessage(`{"type":"function","function":{"name":"read"}}`))
	if !strings.Contains(forced[0].Content, "MUST") {
		t.Errorf("вынужденный вызов не доехал до промпта: %q", forced[0].Content)
	}
	if !strings.Contains(forced[0].Content, "base") {
		t.Errorf("исходный system-промпт потерян: %q", forced[0].Content)
	}
}

// TestR246_AugmentNoToolsNoInstruction — без tools требование бессмысленно.
func TestR246_AugmentNoToolsNoInstruction(t *testing.T) {
	msgs := []chatMessage{{Role: "user", Content: "hi"}}
	out := augmentSystemWithToolsForChoice(msgs, nil, json.RawMessage(`"required"`))
	if len(out) != 1 || out[0].Role != "user" {
		t.Errorf("без tools сообщения не должны меняться: %+v", out)
	}
}
