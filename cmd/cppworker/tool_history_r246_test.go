// tool_history_r246_test.go — R246 (2026-10-26): форма tool-истории выбирается
// по шаблону модели, и результат инструмента ОБЯЗАН доехать до модели.
//
// ЖИВОЙ ДЕФЕКТ (DeepSeek Harness → балансер → cppworker → gemma-4): промпт
// второго хода содержал assistant с JSON-дампом вызова и НЕ содержал результата
// инструмента вообще. Модель писала «output is not provided yet, I must wait» и
// отдавала пустой ответ, агент не мог применить инструмент.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// gemmaTemplateExcerpt — фрагменты настоящего шаблона gemma-4 (GGUF
// tokenizer.chat_template): вызовы рисуются из message.tool_calls, а
// role="tool" в основном цикле пропускается и подставляется из tool_calls.
const gemmaTemplateExcerpt = `{%- for message in loop_messages -%}
    {%- if message['role'] != 'tool' -%}
        {%- if message.get('tool_calls') -%}
            {{- '<|tool_call>call:' + function['name'] + '{' -}}
        {%- endif -%}
        {%- if message.get('tool_responses') -%}
        {%- elif message.get('tool_calls') -%}
            {%- elif loop_messages[k]['role'] != 'tool' -%}
        {%- endif -%}
    {%- endif -%}
{%- endfor -%}`

// qwenTemplateExcerpt — фрагмент шаблона Qwen3.8: role="tool" рисуется сам,
// а arguments обязан быть объектом (JSON-строка вызывает raise_exception).
const qwenTemplateExcerpt = `{%- elif message.role == "tool" %}
    {{- '\n<tool_response>\n' }}
    {{- content }}
    {{- '\n</tool_response>' }}
{%- endif %}
{%- if message.tool_calls and message.tool_calls is iterable %}`

// TestR246_TemplateCaps — возможности шаблона определяются по исходнику.
func TestR246_TemplateCaps(t *testing.T) {
	gemma := analyzeTemplateToolCaps(gemmaTemplateExcerpt)
	if !gemma.StructuredCalls {
		t.Error("gemma: не увидели tool_calls в шаблоне")
	}
	if gemma.StandaloneTool {
		t.Error("gemma: шаблон НЕ рисует role=tool сам, признак не должен стоять")
	}

	qwen := analyzeTemplateToolCaps(qwenTemplateExcerpt)
	if !qwen.StructuredCalls {
		t.Error("qwen: не увидели tool_calls в шаблоне")
	}
	if !qwen.StandaloneTool {
		t.Error("qwen: шаблон рисует role=tool, признак обязателен")
	}

	plain := analyzeTemplateToolCaps("{%- for m in messages -%}{{- m.content }}{%- endfor -%}")
	if plain.StructuredCalls || plain.StandaloneTool {
		t.Errorf("шаблон без инструментов получил возможности: %+v", plain)
	}
}

// TestR246_StructuredHistoryForGemma — вызовы структурные, результат инструмента
// остаётся role="tool" с tool_call_id (шаблон сам подставит его из вызовов).
func TestR246_StructuredHistoryForGemma(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "read go.mod"},
		{Role: "assistant", ToolCalls: []ollamaToolCall{{
			ID: "call_read", Type: "function",
			Function: ollamaToolCallFunction{Name: "read", Arguments: json.RawMessage(`{"file_path":"go.mod"}`)},
		}}},
		{Role: "tool", ToolCallID: "call_read", Name: "read", Content: "1: module ollama-loadbalancer"},
	}
	history := buildNativeToolHistory(msgs, templateToolCaps{StructuredCalls: true, ConsumesToolResults: true, Known: true})

	if len(history) != 4 {
		t.Fatalf("сообщений %d, ожидалось 4", len(history))
	}
	assistant := history[2]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Name != "read" {
		t.Fatalf("вызов не передан структурно: %+v", assistant)
	}
	if strings.Contains(assistant.Content, "{") {
		t.Errorf("content ассистента не должен содержать JSON-дамп вызова: %q", assistant.Content)
	}
	tool := history[3]
	if tool.Role != "tool" {
		t.Errorf("результат инструмента потерял роль tool: %+v", tool)
	}
	if tool.ToolCallID != "call_read" {
		t.Errorf("tool_call_id = %q, ожидался call_read", tool.ToolCallID)
	}
	if !strings.Contains(tool.Content, "ollama-loadbalancer") {
		t.Errorf("содержимое результата потеряно: %q", tool.Content)
	}
}

// TestR246_StructuredHistoryFillsMissingIDs — пустой id вызова получает
// детерминированный, и результат склеивается с ним по порядку.
func TestR246_StructuredHistoryFillsMissingIDs(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ollamaToolCall{{
			Function: ollamaToolCallFunction{Name: "grep", Arguments: json.RawMessage(`{"pattern":"x"}`)},
		}}},
		{Role: "tool", Content: "match"},
	}
	history := buildNativeToolHistory(msgs, templateToolCaps{StructuredCalls: true, ConsumesToolResults: true, Known: true})
	id := history[1].ToolCalls[0].ID
	if id == "" {
		t.Fatal("id вызова не сгенерирован — результат инструмента не склеится")
	}
	if history[2].ToolCallID != id {
		t.Errorf("tool_call_id = %q, ожидался %q (id вызова)", history[2].ToolCallID, id)
	}
}

// TestR246_QwenKeepsTextCalls — для шаблона, который сам рисует role=tool,
// сохраняется прежняя форма: вызовы JSON-текстом, результат role=tool.
//
// Причина: шаблон Qwen3.8 ТРЕБУЕТ arguments объектом и падает с
// raise_exception на JSON-строке, поэтому структурные вызовы ему нельзя.
func TestR246_QwenKeepsTextCalls(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "read go.mod"},
		{Role: "assistant", ToolCalls: []ollamaToolCall{{
			ID: "call_read", Type: "function",
			Function: ollamaToolCallFunction{Name: "read", Arguments: json.RawMessage(`{"file_path":"go.mod"}`)},
		}}},
		{Role: "tool", ToolCallID: "call_read", Name: "read", Content: "module ollama-loadbalancer"},
	}
	history := buildNativeToolHistory(msgs, templateToolCaps{StructuredCalls: true, StandaloneTool: true, ConsumesToolResults: true, Known: true})

	if len(history[1].ToolCalls) != 0 {
		t.Errorf("qwen получил структурные вызовы — его шаблон на них падает: %+v", history[1])
	}
	if !strings.Contains(history[1].Content, `"name":"read"`) {
		t.Errorf("вызов не попал в текст для qwen: %q", history[1].Content)
	}
	if history[2].Role != "user" {
		t.Fatalf("результат инструмента должен уйти как user (шаблон без role=tool): %+v", history[2])
	}
	if !strings.Contains(history[2].Content, "module ollama-loadbalancer") {
		t.Errorf("результат потерян: %q", history[2].Content)
	}
}

// TestR246_UnknownTemplateGetsToolResultInText — шаблон без инструментов:
// результат уходит user-сообщением с именем инструмента.
func TestR246_UnknownTemplateGetsToolResultInText(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ollamaToolCall{{
			Function: ollamaToolCallFunction{Name: "read", Arguments: json.RawMessage(`{"file_path":"a"}`)},
		}}},
		{Role: "tool", Content: "file body"},
	}
	history := buildNativeToolHistory(msgs, templateToolCaps{Known: true})
	last := history[len(history)-1]
	if last.Role != "user" {
		t.Fatalf("результат инструмента не попал в user: %+v", last)
	}
	if !strings.Contains(last.Content, "file body") || !strings.Contains(last.Content, "read") {
		t.Errorf("в тексте результата нет тела или имени инструмента: %q", last.Content)
	}
}

// TestR246_OrphanToolResultBecomesUserText — результат инструмента без
// предшествующего model-сообщения с вызовом не должен исчезнуть: шаблон gemma-4
// такие сообщения пропускает молча.
func TestR246_OrphanToolResultBecomesUserText(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "task"},
		{Role: "tool", Content: "file body"},
	}
	history := buildNativeToolHistory(msgs, templateToolCaps{StructuredCalls: true, ConsumesToolResults: true, Known: true})
	last := history[len(history)-1]
	if last.Role != "user" {
		t.Fatalf("осиротевший результат не переведён в user: %+v", last)
	}
	if !strings.Contains(last.Content, "file body") {
		t.Errorf("тело результата потеряно: %q", last.Content)
	}
}

// TestR246_LegacyHistory — низкоуровневый путь тоже не теряет результат.
func TestR246_LegacyHistory(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ollamaToolCall{{
			Function: ollamaToolCallFunction{Name: "read", Arguments: json.RawMessage(`{"file_path":"a"}`)},
		}}},
		{Role: "tool", Name: "read", Content: "file body"},
	}
	history := buildLegacyToolHistory(msgs)
	if len(history) != 3 {
		t.Fatalf("сообщений %d, ожидалось 3", len(history))
	}
	if !strings.Contains(history[1].Content, `"name":"read"`) {
		t.Errorf("вызов не в тексте: %q", history[1].Content)
	}
	if history[2].Role != "user" || !strings.Contains(history[2].Content, "file body") {
		t.Errorf("результат инструмента потерян: %+v", history[2])
	}
}

// TestR246_OrderPreserved — порядок ходов не переставляется (прежний
// bridge_apply_chat_template складывал всех user перед всеми assistant).
func TestR246_OrderPreserved(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"},
	}
	history := buildLegacyToolHistory(msgs)
	for i, want := range []string{"user", "assistant", "user"} {
		if history[i].Role != want {
			t.Errorf("позиция %d: роль %q, ожидалась %q", i, history[i].Role, want)
		}
	}
}

