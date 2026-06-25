// llamacpp_translate_req_test.go — Тесты для трансляции Ollama → OpenAI для tools.
// Покрывает сценарии 2026-06-25: tool_choice=auto, options.tools fallback.
package balancer

import (
	"encoding/json"
	"testing"
)

// TestTranslateOllamaChatToOpenAI_ToolChoiceAuto — проверяет дефолтный tool_choice=auto
// для моделей без нативного tool support (Gemma-4 и т.п.).
func TestTranslateOllamaChatToOpenAI_ToolChoiceAuto(t *testing.T) {
	body := []byte(`{
		"model": "gemma-4-it-Q4_K_M",
		"messages": [{"role": "user", "content": "list directory"}],
		"tools": [{
			"type": "function",
			"function": {
				"name": "list_directory_with_sizes",
				"description": "List files",
				"parameters": {"type": "object", "properties": {"path": {"type": "string"}}}
			}
		}]
	}`)

	out, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("invalid output JSON: %v\n%s", err, out)
	}

	// tool_choice должен быть "auto" по умолчанию
	tc, ok := req["tool_choice"]
	if !ok {
		t.Fatalf("tool_choice missing in output: %s", out)
	}
	if tc != "auto" {
		t.Errorf("tool_choice = %v, want \"auto\"", tc)
	}

	// tools должны быть прокинуты
	tools, ok := req["tools"].([]interface{})
	if !ok || len(tools) == 0 {
		t.Errorf("tools not propagated: %s", out)
	}
}

// TestTranslateOllamaChatToOpenAI_ToolChoiceExplicit — проверяет, что явный tool_choice клиента не перезаписывается.
func TestTranslateOllamaChatToOpenAI_ToolChoiceExplicit(t *testing.T) {
	body := []byte(`{
		"model": "llama-3.1-8b",
		"messages": [{"role": "user", "content": "test"}],
		"tools": [{"type": "function", "function": {"name": "test"}}],
		"tool_choice": "none"
	}`)

	out, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var req map[string]interface{}
	json.Unmarshal(out, &req)

	if tc := req["tool_choice"]; tc != "none" {
		t.Errorf("tool_choice = %v, want \"none\" (explicit)", tc)
	}
}

// TestTranslateOllamaChatToOpenAI_OptionsToolsFallback — проверяет, что tools из
// options.tools прокидываются (fallback для клиентов типа OpenWebUI).
func TestTranslateOllamaChatToOpenAI_OptionsToolsFallback(t *testing.T) {
	body := []byte(`{
		"model": "qwen2.5:7b",
		"messages": [{"role": "user", "content": "test"}],
		"options": {
			"tools": [{
				"type": "function",
				"function": {"name": "search", "description": "search"}
			}]
		}
	}`)

	out, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var req map[string]interface{}
	json.Unmarshal(out, &req)

	tools, ok := req["tools"].([]interface{})
	if !ok || len(tools) == 0 {
		t.Fatalf("tools from options.tools not propagated: %s", out)
	}
	if tools[0].(map[string]interface{})["function"].(map[string]interface{})["name"] != "search" {
		t.Errorf("tool name = %v, want 'search'", tools[0])
	}

	// tool_choice тоже должен быть выставлен, т.к. tools появились
	if req["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want \"auto\"", req["tool_choice"])
	}
}

// TestTranslateOllamaChatToOpenAI_NoToolsNoToolChoice — без tools tool_choice не должен появляться.
func TestTranslateOllamaChatToOpenAI_NoToolsNoToolChoice(t *testing.T) {
	body := []byte(`{
		"model": "qwen2.5:7b",
		"messages": [{"role": "user", "content": "test"}]
	}`)

	out, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var req map[string]interface{}
	json.Unmarshal(out, &req)

	if _, has := req["tool_choice"]; has {
		t.Errorf("tool_choice should NOT be present when no tools: %s", out)
	}
}

// TestTranslateOllamaChatToOpenAI_PromptToMessages — Ollama-native /api/chat с prompt
// (не messages) должен быть сконвертирован в messages.
func TestTranslateOllamaChatToOpenAI_PromptToMessages(t *testing.T) {
	body := []byte(`{
		"model": "qwen2.5:7b",
		"prompt": "Hello world"
	}`)

	out, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var req map[string]interface{}
	json.Unmarshal(out, &req)

	msgs, ok := req["messages"].([]interface{})
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages not created from prompt: %s", out)
	}
	msg := msgs[0].(map[string]interface{})
	if msg["role"] != "user" || msg["content"] != "Hello world" {
		t.Errorf("message = %v, want role=user content='Hello world'", msg)
	}
}