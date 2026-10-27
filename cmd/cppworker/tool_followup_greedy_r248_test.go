package main

import (
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// R248: ход с результатом инструмента обязан быть детерминированным, иначе
// модель периодически повторяет тот же вызов (живой дефект на стенде: один и
// тот же двухходовой запрос то отвечал по результату, то снова звал read).
func TestR248_ToolFollowupForcesGreedy(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "read main.cpp"},
		{Role: "assistant", ToolCalls: []ollamaToolCall{{ID: "call_read", Function: ollamaToolCallFunction{Name: "read"}}}},
		{Role: "tool", ToolCallID: "call_read", Content: "int main(){return 0;}"},
	}
	params := bridge.GenerationParams{Temperature: 0.7, TopP: 0.9, TopK: 40}
	applied := applyToolFollowupDeterminism(msgs, &params, false)
	if !applied {
		t.Fatal("политика должна примениться: в истории есть результат вызова")
	}
	if params.Temperature != 0 || params.TopP != 1 || params.TopK != 1 {
		t.Fatalf("ожидался greedy, получено temp=%v top_p=%v top_k=%v", params.Temperature, params.TopP, params.TopK)
	}
}

// R248: явная температура клиента — его решение, не вмешиваемся.
func TestR248_ToolFollowupRespectsExplicitTemperature(t *testing.T) {
	msgs := []chatMessage{
		{Role: "assistant", ToolCalls: []ollamaToolCall{{ID: "c1", Function: ollamaToolCallFunction{Name: "read"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	}
	params := bridge.GenerationParams{Temperature: 0.3, TopP: 0.8}
	if applyToolFollowupDeterminism(msgs, &params, true) {
		t.Fatal("при явной температуре политика не должна применяться")
	}
	if params.Temperature != 0.3 {
		t.Fatalf("температура клиента изменена: %v", params.Temperature)
	}
}

// R248: без tool-результата поведение не меняется.
func TestR248_ToolFollowupNoToolResult(t *testing.T) {
	msgs := []chatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}
	params := bridge.GenerationParams{Temperature: 0.7, TopP: 0.9}
	if applyToolFollowupDeterminism(msgs, &params, false) {
		t.Fatal("без результата инструмента политика применяться не должна")
	}
	if params.Temperature != 0.7 {
		t.Fatalf("температура изменена без причины: %v", params.Temperature)
	}
}

// R248: результат инструмента без tool_call_id (часть клиентов не шлёт id)
// тоже должен включать детерминированный ход.
func TestR248_ToolFollowupWithoutCallID(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "read"},
		{Role: "tool", Content: "file contents"},
	}
	params := bridge.GenerationParams{Temperature: 0.7}
	if !applyToolFollowupDeterminism(msgs, &params, false) {
		t.Fatal("результат без id — тоже повод ответить детерминированно")
	}
}
