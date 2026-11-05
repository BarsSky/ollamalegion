// ollama_messages_openai_r436_test.go — R436 (2026-11-05): история сообщений
// Ollama → OpenAI приводится к контракту OpenAI (`arguments` — строка).
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR436_ObjectArgumentsBecomeString — главная конверсия.
func TestR436_ObjectArgumentsBecomeString(t *testing.T) {
	body := []byte(`{
	  "model": "m",
	  "messages": [
	    {"role": "user", "content": "прочитай TASK.md"},
	    {"role": "assistant", "content": "", "tool_calls": [
	      {"id": "call_1", "type": "function",
	       "function": {"name": "read_files", "arguments": {"files": ["TASK.md"]}}}
	    ]},
	    {"role": "tool", "tool_name": "read_files",
	     "content": "{\"content\": \"...\"}"}
	  ],
	  "stream": false
	}`)

	out, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("перевод запроса: %v", err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	msgs, ok := req["messages"].([]interface{})
	if !ok || len(msgs) != 3 {
		t.Fatalf("messages = %v", req["messages"])
	}
	assistant := msgs[1].(map[string]interface{})
	calls := assistant["tool_calls"].([]interface{})
	fn := calls[0].(map[string]interface{})["function"].(map[string]interface{})
	args, isString := fn["arguments"].(string)
	if !isString {
		t.Fatalf("arguments не строка: %T (%v)", fn["arguments"], fn["arguments"])
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments не JSON-строка: %q", args)
	}
	if !strings.Contains(args, "TASK.md") {
		t.Fatalf("аргументы потеряли данные: %q", args)
	}
	// id вызова сохранён — по нему клиент сопоставляет результат.
	if id, _ := calls[0].(map[string]interface{})["id"].(string); id != "call_1" {
		t.Fatalf("id вызова = %q", id)
	}
	// tool-сообщение не тронуто (ни id, ни имя не выдумываем).
	toolMsg := msgs[2].(map[string]interface{})
	if toolMsg["role"] != "tool" || toolMsg["tool_name"] != "read_files" {
		t.Fatalf("tool-сообщение изменено: %v", toolMsg)
	}
}

// TestR436_StringArgumentsNotDoubleEncoded — уже OpenAI-форма не переэкрaнируется.
func TestR436_StringArgumentsNotDoubleEncoded(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role": "assistant",
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":   "call_1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "read_files",
						"arguments": `{"files":["TASK.md"]}`,
					},
				},
			},
		},
	}
	out := convertMessagesToOpenAIForm(msgs)
	call := out[0].(map[string]interface{})["tool_calls"].([]interface{})[0].(map[string]interface{})
	args := call["function"].(map[string]interface{})["arguments"].(string)
	if args != `{"files":["TASK.md"]}` {
		t.Fatalf("строка аргументов переэкрaнирована: %q", args)
	}
}

// TestR436_NoToolCallsUntouched — обычные сообщения не переписываются.
func TestR436_NoToolCallsUntouched(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": "s"},
		map[string]interface{}{"role": "user", "content": "u"},
	}
	out := convertMessagesToOpenAIForm(msgs)
	for i := range msgs {
		if out[i].(map[string]interface{})["content"] != msgs[i].(map[string]interface{})["content"] {
			t.Fatalf("сообщение %d изменено", i)
		}
	}
}
