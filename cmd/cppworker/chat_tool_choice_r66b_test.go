package main

// chat_tool_choice_r66b_test.go — R66b (2026-09-22).
//
// Контекст: реальный Cline CLI (провайдер "ollama") всегда шлёт
// "tool_choice":"auto" в Ollama-нативном /api/chat вместе с tools[]. До R66b
// строгий декодер (/api/chat, types.DecodeJSONRequest → DisallowUnknownFields)
// отвечал 400 `invalid JSON: json: unknown field "tool_choice"`, Cline падал с
// "Bad Request" и не мог вызвать ни одного инструмента.
//
// Тесты:
//  1. TestToolsDisabledByChoice — семантика разбора tool_choice.
//  2. TestR66b_OllamaChat_AcceptsClineToolChoice — реальный payload Cline
//     (захвачен scripts/mock_capture_server.js) доходит до стадии inference,
//     а не отбрасывается декодером (живёт в ollama_chat_contract_r65d_test.go
//     как часть общего набора, здесь — точечная проверка именно tool_choice).

import (
	"encoding/json"
	"testing"
)

func TestToolsDisabledByChoice(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty (field absent)", "", false},
		{"auto", `"auto"`, false},
		{"required", `"required"`, false},
		{"NONE uppercase", `"NONE"`, true},
		{"none", `"none"`, true},
		{"none with spaces", `" none "`, true},
		{"specific function object", `{"type":"function","function":{"name":"read_file"}}`, false},
		{"invalid json", `{`, false},
		{"null", `null`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolsDisabledByChoice(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("toolsDisabledByChoice(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestR66b_ChatRequest_ToolChoiceIsKnownField — страховка на уровне структуры:
// поле tool_choice обязано декодироваться в chatRequest без ошибки, иначе
// строгий декодер снова начнёт отвечать 400 реальному Cline.
func TestR66b_ChatRequest_ToolChoiceIsKnownField(t *testing.T) {
	body := `{"model":"dummy","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"x","parameters":{"type":"object"}}}],` +
		`"tool_choice":"auto","stream":false}`
	var req chatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("chatRequest должен принимать tool_choice, получена ошибка: %v", err)
	}
	if string(req.ToolChoice) == "" {
		t.Fatal("ToolChoice не заполнен — поле потеряно при декодировании")
	}
	if toolsDisabledByChoice(req.ToolChoice) {
		t.Errorf(`tool_choice="auto" не должен отключать инструменты`)
	}
}

// TestR66b_ChatRequest_ToolNameIsKnownField — R66b (2026-09-22).
//
// Второй шаг агентского цикла Cline CLI 3.0.64 (провайдер "ollama") шлёт
// сообщение результата инструмента в Ollama-схеме:
//
//	{"role":"tool","tool_name":"editor","tool_call_id":"call_editor","content":"..."}
//
// Поля `tool_name` в chatMessage не было → строгий декодер отвечал 400
// `unknown field "tool_name"` и цикл обрывался сразу после первого tool call
// (файл создавался, но итогового ответа клиент не получал).
func TestR66b_ChatRequest_ToolNameIsKnownField(t *testing.T) {
	body := `{"model":"dummy","messages":[
		{"role":"assistant","content":"","tool_calls":[
			{"id":"call_editor","type":"function","function":{"name":"editor","arguments":{"path":"a.txt"}}}
		]},
		{"role":"tool","tool_name":"editor","tool_call_id":"call_editor","content":"File created"}
	],"stream":true}`
	var req chatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("chatRequest должен принимать tool_name, получена ошибка: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("ожидалось 2 сообщения, получено %d", len(req.Messages))
	}
	toolMsg := req.Messages[1]
	if toolMsg.ToolName != "editor" {
		t.Errorf("ToolName = %q, ожидалось editor", toolMsg.ToolName)
	}
	if got := toolMsg.effectiveToolName(); got != "editor" {
		t.Errorf("effectiveToolName() = %q, ожидалось editor", got)
	}
	// Приоритет у OpenAI-поля name, если пришли оба.
	both := chatMessage{Name: "from_name", ToolName: "from_tool_name"}
	if got := both.effectiveToolName(); got != "from_name" {
		t.Errorf("effectiveToolName() при обоих полях = %q, ожидалось from_name", got)
	}
}
