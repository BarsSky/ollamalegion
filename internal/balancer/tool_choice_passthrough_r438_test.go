// tool_choice_passthrough_r438_test.go — R438 (2026-11-05): конвертер не
// подставляет tool_choice, которого клиент не присылал.
//
// Раньше перевод Ollama→OpenAI добавлял `"tool_choice":"auto"` всякий раз, когда
// в запросе были tools[]. Это выдуманное поле: клиент его не отправлял, а
// контракт OpenAI при наличии tools и так подразумевает "auto". На бэкенде со
// строгой валидацией лишнее поле — риск 400, а «заставить модель звать
// инструменты» решается промптом (R426–R434), а не флагом.
package balancer

import (
	"encoding/json"
	"testing"
)

// TestR438_NoInventedToolChoice — без tool_choice в запросе поле не появляется.
func TestR438_NoInventedToolChoice(t *testing.T) {
	body := []byte(`{"model":"m","stream":false,
	  "messages":[{"role":"user","content":"hi"}],
	  "tools":[{"type":"function","function":{"name":"read_files","parameters":{"type":"object"}}}]}`)

	out, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("перевод: %v", err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if _, has := req["tool_choice"]; has {
		t.Fatalf("добавлено выдуманное tool_choice=%v", req["tool_choice"])
	}
	if _, has := req["tools"]; !has {
		t.Fatal("tools потеряны при переводе")
	}
}

// TestR438_ExplicitToolChoicePassedThrough — заданное клиентом значение
// пробрасывается без изменений (в том числе объект-форма).
func TestR438_ExplicitToolChoicePassedThrough(t *testing.T) {
	body := []byte(`{"model":"m","stream":false,
	  "messages":[{"role":"user","content":"hi"}],
	  "tools":[{"type":"function","function":{"name":"read_files","parameters":{"type":"object"}}}],
	  "tool_choice":{"type":"function","function":{"name":"read_files"}}}`)

	out, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("перевод: %v", err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	tc, ok := req["tool_choice"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool_choice потерян: %v", req["tool_choice"])
	}
	if tc["type"] != "function" {
		t.Fatalf("tool_choice изменён: %v", tc)
	}
}
