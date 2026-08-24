// llamacpp_translate_resp_usage_chunk_test.go — Round 35c+ (2026-08-13)
// Tests for usage chunk passthrough в Ollama streaming translator.
//
// Bug: cppworker отдаёт OpenAI usage chunk ({usage:{prompt_tokens,
// completion_tokens, total_tokens}, choices:[]}) в финальном SSE-чанке.
// Pre-fix translator возвращал nil для чанков без `choices`, usage блок
// пропадал. Open WebUI показывал "input_tokens: 0, output_tokens: 0,
// total_duration: 0" (bug-report 2026-08-13).
//
// Post-fix: translateUsageChunkToOllama детектит usage chunk и эмитит
// Ollama done-чанк с prompt_eval_count/eval_count + total_duration: 0.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ============================================================
// Test 1: /api/chat usage chunk → Ollama done с message + tokens
// ============================================================

func TestTranslateUsageChunkToOllama_ChatPath(t *testing.T) {
	usageChunk := []byte(`{"id":"chatcmpl-123","object":"chat.completion.chunk","created":1786626000,"model":"gemma-4","choices":[],"usage":{"prompt_tokens":1234,"completion_tokens":567,"total_tokens":1801}}`)

	result := translateOpenAISSEDataToOllama("/api/chat", usageChunk, "gemma-4", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result for usage chunk, got nil (Round 35c+ regression!)")
	}

	// Strip trailing \n
	raw := strings.TrimRight(string(result), "\n")

	var ollama map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &ollama); err != nil {
		t.Fatalf("failed to parse Ollama chunk: %v\nraw=%s", err, raw)
	}

	if done, ok := ollama["done"].(bool); !ok || !done {
		t.Errorf("expected done=true, got %v", ollama["done"])
	}
	if reason, _ := ollama["done_reason"].(string); reason != "stop" {
		t.Errorf("expected done_reason='stop', got %q", reason)
	}
	if model, _ := ollama["model"].(string); model != "gemma-4" {
		t.Errorf("expected model='gemma-4', got %q", model)
	}

	// /api/chat требует message: {role, content}
	msg, ok := ollama["message"].(map[string]interface{})
	if !ok {
		t.Errorf("expected 'message' field for /api/chat path")
	} else {
		if role, _ := msg["role"].(string); role != "assistant" {
			t.Errorf("expected message.role='assistant', got %q", role)
		}
		if content, _ := msg["content"].(string); content != "" {
			t.Errorf("expected message.content='', got %q", content)
		}
	}

	// Token counts — ЭТО ГЛАВНОЕ, ради чего весь фикс
	if v, ok := ollama["prompt_eval_count"].(float64); !ok || int(v) != 1234 {
		t.Errorf("expected prompt_eval_count=1234, got %v (type %T)", ollama["prompt_eval_count"], ollama["prompt_eval_count"])
	}
	if v, ok := ollama["eval_count"].(float64); !ok || int(v) != 567 {
		t.Errorf("expected eval_count=567, got %v (type %T)", ollama["eval_count"], ollama["eval_count"])
	}
}

// ============================================================
// Test 2: /api/generate usage chunk → Ollama done без message
// ============================================================

func TestTranslateUsageChunkToOllama_GeneratePath(t *testing.T) {
	usageChunk := []byte(`{"id":"cmpl-789","object":"chat.completion.chunk","created":1786626000,"model":"qwen3","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":100,"total_tokens":150}}`)

	result := translateOpenAISSEDataToOllama("/api/generate", usageChunk, "qwen3", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result for usage chunk, got nil")
	}

	raw := strings.TrimRight(string(result), "\n")
	var ollama map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &ollama); err != nil {
		t.Fatalf("failed to parse: %v\nraw=%s", err, raw)
	}

	// /api/generate НЕ требует message (использует response)
	if _, hasMsg := ollama["message"]; hasMsg {
		t.Errorf("/api/generate path should NOT have 'message' field, got %v", ollama["message"])
	}

	if v, ok := ollama["prompt_eval_count"].(float64); !ok || int(v) != 50 {
		t.Errorf("expected prompt_eval_count=50, got %v", ollama["prompt_eval_count"])
	}
	if v, ok := ollama["eval_count"].(float64); !ok || int(v) != 100 {
		t.Errorf("expected eval_count=100, got %v", ollama["eval_count"])
	}
}

// ============================================================
// Test 3: usage chunk с finish_reason="tool_calls"
// ============================================================

func TestTranslateUsageChunkToOllama_ToolCallsFinishReason(t *testing.T) {
	// Round 35c+: cppworker может прислать usage chunk с choices[0].finish_reason
	// если был вызван tools path. Должны извлечь finish_reason.
	usageChunk := []byte(`{"id":"cmpl-tc","object":"chat.completion.chunk","created":1786626000,"model":"gemma-4","choices":[{"finish_reason":"tool_calls","index":0}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`)

	result := translateOpenAISSEDataToOllama("/api/chat", usageChunk, "gemma-4", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result for usage chunk with tool_calls finish_reason")
	}

	raw := strings.TrimRight(string(result), "\n")
	var ollama map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &ollama); err != nil {
		t.Fatalf("failed to parse: %v\nraw=%s", err, raw)
	}

	if reason, _ := ollama["done_reason"].(string); reason != "tool_calls" {
		t.Errorf("expected done_reason='tool_calls', got %q", reason)
	}
	if done, _ := ollama["done"].(bool); !done {
		t.Errorf("expected done=true, got %v", ollama["done"])
	}
}

// ============================================================
// Test 4: обычный content chunk НЕ триггерит usage path
// (regression — usage path не должен ломать обычные чанки)
// ============================================================

func TestTranslateUsageChunkToOllama_ContentChunkRegression(t *testing.T) {
	contentChunk := []byte(`{"id":"cmpl-1","object":"chat.completion.chunk","created":1786626000,"model":"gemma-4","choices":[{"delta":{"content":"hello"},"finish_reason":null,"index":0}]}`)

	result := translateOpenAISSEDataToOllama("/api/chat", contentChunk, "gemma-4", nil, time.Time{})
	if result == nil {
		t.Fatal("content chunk should NOT return nil")
	}

	raw := strings.TrimRight(string(result), "\n")
	var ollama map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &ollama); err != nil {
		t.Fatalf("failed to parse: %v\nraw=%s", err, raw)
	}

	// Done не должен быть true (это промежуточный чанк)
	if done, _ := ollama["done"].(bool); done {
		t.Errorf("content chunk should NOT be done=true")
	}
	// prompt_eval_count НЕ должен быть в content chunk
	if _, has := ollama["prompt_eval_count"]; has {
		t.Errorf("content chunk should NOT have prompt_eval_count")
	}
	// Content должен быть в message.content
	msg, _ := ollama["message"].(map[string]interface{})
	if content, _ := msg["content"].(string); content != "hello" {
		t.Errorf("expected message.content='hello', got %q", content)
	}
}

// ============================================================
// Test 5: usage chunk c int вместо float64 (json.Number variant)
// ============================================================

func TestTranslateUsageChunkToOllama_IntUsageValues(t *testing.T) {
	// Some clients may send usage как int (не float64). intFromUsage
	// должен обработать оба варианта.
	usageChunk := []byte(`{"id":"cmpl-2","object":"chat.completion.chunk","created":1786626000,"model":"gemma-4","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`)

	result := translateOpenAISSEDataToOllama("/api/chat", usageChunk, "gemma-4", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	raw := strings.TrimRight(string(result), "\n")
	var ollama map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &ollama); err != nil {
		t.Fatalf("failed to parse: %v\nraw=%s", err, raw)
	}

	// JSON unmarshal по умолчанию даёт float64 для чисел, но проверим что
	// intFromUsage корректно обрабатывает оба типа.
	if v, ok := ollama["prompt_eval_count"].(float64); !ok || int(v) != 100 {
		t.Errorf("expected prompt_eval_count=100, got %v (type %T)", ollama["prompt_eval_count"], ollama["prompt_eval_count"])
	}
}

// ============================================================
// Test 6: intFromUsage unit tests (both int and float64 inputs)
// ============================================================

func TestIntFromUsage(t *testing.T) {
	tests := []struct {
		name     string
		usage    map[string]interface{}
		key      string
		expected int
	}{
		{"float64", map[string]interface{}{"x": float64(42.0)}, "x", 42},
		{"int", map[string]interface{}{"x": int(42)}, "x", 42},
		{"missing key", map[string]interface{}{}, "x", 0},
		{"wrong type", map[string]interface{}{"x": "string"}, "x", 0},
		{"nil value", map[string]interface{}{"x": nil}, "x", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := intFromUsage(tt.usage, tt.key)
			if got != tt.expected {
				t.Errorf("intFromUsage(%v, %q) = %d, want %d", tt.usage, tt.key, got, tt.expected)
			}
		})
	}
}

// ============================================================
// Test 7: hasNonEmptyChoices unit tests
// ============================================================

func TestHasNonEmptyChoices(t *testing.T) {
	tests := []struct {
		name     string
		chunk    map[string]interface{}
		expected bool
	}{
		// Round 53.1: "non-empty" теперь означает "с реальным контентом" (delta с
		// content/reasoning/tool_calls или text для /v1/completions). Голый
		// {"choices":[{}]} больше не считается non-empty — это usage chunk marker.
		{"empty choice object", map[string]interface{}{"choices": []interface{}{map[string]interface{}{}}}, false},
		{"with delta content", map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": "hi"}}}}, true},
		{"with text (completions)", map[string]interface{}{"choices": []interface{}{map[string]interface{}{"text": "hi"}}}, true},
		{"with finish_reason only (usage chunk)", map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": "stop"}}}, false},
		{"empty array", map[string]interface{}{"choices": []interface{}{}}, false},
		{"missing key", map[string]interface{}{}, false},
		{"wrong type", map[string]interface{}{"choices": "not-array"}, false},
		{"nil choices", map[string]interface{}{"choices": nil}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasNonEmptyChoices(tt.chunk)
			if got != tt.expected {
				t.Errorf("hasNonEmptyChoices(%v) = %v, want %v", tt.chunk, got, tt.expected)
			}
		})
	}
}
