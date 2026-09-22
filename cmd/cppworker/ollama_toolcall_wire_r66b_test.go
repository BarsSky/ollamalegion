package main

// ollama_toolcall_wire_r66b_test.go — R66b (2026-09-22): wire-format контракт
// tool_calls для Ollama-нативного /api/chat.
//
// Ollama и OpenAI используют РАЗНЫЕ схемы:
//
//	Ollama: {"function":{"name":"f","arguments":{"a":1}}}    // arguments — ОБЪЕКТ
//	OpenAI: {"function":{"name":"f","arguments":"{\"a\":1}"}} // arguments — СТРОКА
//
// cppworker до R66b всегда использовал OpenAI-форму. Последствия на реальном
// клиенте (Cline CLI 3.0.64, провайдер "ollama"):
//   * в ответе он получал строку там, где ждёт объект → повторная сериализация →
//     tool call без аргументов → "Model returned empty response";
//   * на втором шаге (assistant.tool_calls + tool-результат) cppworker отвечал
//     400 `cannot unmarshal object into Go struct field ... Arguments of type string`.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR66b_OllamaRequest_AcceptsObjectArguments — запрос с arguments-ОБЪЕКТОМ
// (как его шлёт Ollama-клиент на втором шаге) должен приниматься без ошибок.
func TestR66b_OllamaRequest_AcceptsObjectArguments(t *testing.T) {
	body := `{
		"model":"dummy",
		"messages":[
			{"role":"user","content":"write a file"},
			{"role":"assistant","content":"","tool_calls":[
				{"id":"call_editor","type":"function","function":{"name":"editor","arguments":{"path":"a.txt","new_text":"hi"}}}
			]},
			{"role":"tool","tool_call_id":"call_editor","name":"editor","content":"ok"}
		],
		"stream":false
	}`
	var req chatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("запрос с arguments-объектом должен приниматься, получена ошибка: %v", err)
	}
	if len(req.Messages) != 3 || len(req.Messages[1].ToolCalls) != 1 {
		t.Fatalf("tool_calls не разобраны: %+v", req.Messages[1].ToolCalls)
	}
	call := req.Messages[1].ToolCalls[0]
	if call.Function.Name != "editor" {
		t.Errorf("name = %q, ожидалось editor", call.Function.Name)
	}
	var args map[string]interface{}
	if err := json.Unmarshal(call.Function.Arguments, &args); err != nil {
		t.Fatalf("arguments не JSON-объект (%v): %s", err, call.Function.Arguments)
	}
	if args["path"] != "a.txt" {
		t.Errorf("args.path = %v, ожидалось a.txt", args["path"])
	}

	// Внутреннее (OpenAI) представление для bridge/промпта: arguments — строка.
	oa := openAIToolCallsFromOllama(req.Messages[1].ToolCalls)
	if len(oa) != 1 || !json.Valid([]byte(oa[0].Function.Arguments)) {
		t.Errorf("openAIToolCallsFromOllama дал невалидные arguments: %+v", oa)
	}
}

// TestR66b_OllamaRequest_AcceptsStringArguments — обратная совместимость:
// arguments-СТРОКА (OpenAI-стиль, как присылают некоторые клиенты) тоже ок.
func TestR66b_OllamaRequest_AcceptsStringArguments(t *testing.T) {
	body := `{"model":"dummy","messages":[
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"read_files","arguments":"{\"path\":\"b.txt\"}"}}
		]}]}`
	var req chatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("запрос с arguments-строкой должен приниматься: %v", err)
	}
	args := req.Messages[0].ToolCalls[0].Function.Arguments
	var m map[string]string
	if err := json.Unmarshal(args, &m); err != nil {
		t.Fatalf("строка должна нормализоваться в JSON-объект (%v): %s", err, args)
	}
	if m["path"] != "b.txt" {
		t.Errorf("path = %q, ожидалось b.txt", m["path"])
	}
}

// TestR66b_OllamaResponse_ArgumentsAreObject — то, что уходит клиенту в
// /api/chat: arguments ОБЯЗАН быть JSON-объектом (Ollama-схема).
func TestR66b_OllamaResponse_ArgumentsAreObject(t *testing.T) {
	msg := chatMessage{
		Role:    "assistant",
		Content: "",
		ToolCalls: ollamaToolCallsFromOpenAI([]openAIToolCall{{
			ID:       "call_editor",
			Type:     "function",
			Function: openAIFunctionCall{Name: "editor", Arguments: `{"path":"C:\\tmp\\a.txt","new_text":"hi"}`},
		}}),
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(out)
	if strings.Contains(got, `"arguments":"`) {
		t.Errorf("arguments сериализованы СТРОКОЙ (OpenAI-форма), Ollama-клиент это не поймёт: %s", got)
	}
	if !strings.Contains(got, `"arguments":{`) {
		t.Errorf("arguments должны быть JSON-объектом: %s", got)
	}
	// Проверяем, что путь не потерян при сериализации.
	var back chatMessage
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	var args map[string]string
	if err := json.Unmarshal(back.ToolCalls[0].Function.Arguments, &args); err != nil {
		t.Fatalf("round-trip arguments: %v", err)
	}
	if args["path"] != `C:\tmp\a.txt` {
		t.Errorf("path после round-trip = %q", args["path"])
	}
}
