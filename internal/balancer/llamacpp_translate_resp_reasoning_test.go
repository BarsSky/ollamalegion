// llamacpp_translate_resp_reasoning_test.go — tests for reasoning_content translation
// between OpenAI (cppworker) and Ollama (OpenWebUI) formats.
//
// Bug context (2026-08-09):
//   cppworker эмитит delta.reasoning_content для reasoning-моделей (gemma-4,
//   qwen3.5/qwen3.6, deepseek-r1). Текущий translateSSEChatToOllama в
//   internal/balancer/llamacpp_translate_resp.go НЕ извлекает reasoning_content —
//   OpenWebUI получает стрим без thinking-секции.
//
//   translateOpenAIChatToOllama (non-streaming) тоже не маппит
//   message.reasoning_content → message.reasoning.
//
// Покрывает:
//   - SSE→NDJSON: delta.reasoning_content → message.thinking
//   - SSE→NDJSON: multiple reasoning chunks → accumulated thinking
//   - SSE→NDJSON: mixed reasoning + content chunks → both fields
//   - SSE→NDJSON: reasoning + tool_calls coexistence
//   - Non-stream JSON: message.reasoning_content → message.reasoning
//   - Non-stream JSON: reasoning + tool_calls coexistence
//   - Edge cases: empty reasoning, only reasoning (no content)
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// ============================================================
// SSE → NDJSON reasoning_content translation
// ============================================================

func TestTranslateSSEChatToOllama_ReasoningContent(t *testing.T) {
	// cppworker emits a delta with ONLY reasoning_content (no content).
	// Translate should produce a NDJSON chunk with message.thinking set.
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"reasoning_content": "Let me think about this...",
				},
				"finish_reason": nil,
			},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil)
	if result == nil {
		t.Fatal("expected non-nil result for reasoning_content chunk")
	}

	var ollamaChunk map[string]interface{}
	if err := json.Unmarshal(result, &ollamaChunk); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	message, ok := ollamaChunk["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected message in ollama chunk, got %v", ollamaChunk)
	}

	thinking, ok := message["thinking"].(string)
	if !ok {
		t.Fatalf("expected message.thinking to be a string, got %T (%v)",
			message["thinking"], message["thinking"])
	}
	if thinking != "Let me think about this..." {
		t.Errorf("expected thinking=%q, got %q", "Let me think about this...", thinking)
	}

	if done, _ := ollamaChunk["done"].(bool); done {
		t.Error("expected done=false for delta chunk")
	}
}

func TestTranslateSSEChatToOllama_ReasoningAndContentSeparate(t *testing.T) {
	// cppworker emits reasoning_content AND content in separate chunks.
	// Each chunk should be translated independently:
	//   chunk 1 (reasoning only) → message.thinking
	//   chunk 2 (content only)   → message.content
	chunk1 := map[string]interface{}{
		"id":      "chatcmpl-1",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "gemma-4",
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{"reasoning_content": "Thinking..."}},
		},
	}
	chunk2 := map[string]interface{}{
		"id":      "chatcmpl-1",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "gemma-4",
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{"content": "Hello!"}},
		},
	}

	// Chunk 1
	sseData1, _ := json.Marshal(chunk1)
	r1 := translateOpenAISSEDataToOllama("/api/chat", sseData1, "gemma-4", nil)
	if r1 == nil {
		t.Fatal("chunk 1 (reasoning) returned nil")
	}
	var c1 map[string]interface{}
	json.Unmarshal(r1, &c1)
	m1 := c1["message"].(map[string]interface{})
	if m1["thinking"] != "Thinking..." {
		t.Errorf("chunk 1: expected thinking=%q, got %v", "Thinking...", m1["thinking"])
	}
	if c, ok := m1["content"]; ok && c != "" && c != nil {
		t.Errorf("chunk 1: expected empty content, got %v", c)
	}

	// Chunk 2
	sseData2, _ := json.Marshal(chunk2)
	r2 := translateOpenAISSEDataToOllama("/api/chat", sseData2, "gemma-4", nil)
	if r2 == nil {
		t.Fatal("chunk 2 (content) returned nil")
	}
	var c2 map[string]interface{}
	json.Unmarshal(r2, &c2)
	m2 := c2["message"].(map[string]interface{})
	if m2["content"] != "Hello!" {
		t.Errorf("chunk 2: expected content=%q, got %v", "Hello!", m2["content"])
	}
	if thinkingVal, ok := m2["thinking"]; ok && thinkingVal != "" && thinkingVal != nil {
		t.Errorf("chunk 2: expected empty thinking, got %v", thinkingVal)
	}
}

func TestTranslateSSEChatToOllama_ReasoningAndContentSameChunk(t *testing.T) {
	// Some emitters send reasoning_content + content in the SAME chunk.
	// Both should be present in the resulting ollama chunk.
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-1",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "gemma-4",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"reasoning_content": "I need to think",
					"content":           "The answer is 42",
				},
			},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "gemma-4", nil)
	if result == nil {
		t.Fatal("expected non-nil for combined reasoning+content chunk")
	}

	var c map[string]interface{}
	json.Unmarshal(result, &c)
	m := c["message"].(map[string]interface{})
	if m["thinking"] != "I need to think" {
		t.Errorf("expected thinking=%q, got %v", "I need to think", m["thinking"])
	}
	if m["content"] != "The answer is 42" {
		t.Errorf("expected content=%q, got %v", "The answer is 42", m["content"])
	}
}

func TestTranslateSSEChatToOllama_ReasoningWithToolCalls(t *testing.T) {
	// gemma-4 with tools: cppworker emits reasoning_content first, then tool_calls.
	// Reasoning must survive in the final response, and tool_calls must be preserved.
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-1",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "gemma-4",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"reasoning_content": "I need to call search",
					"tool_calls": []map[string]interface{}{
						{
							"id":   "call_1",
							"type": "function",
							"function": map[string]interface{}{
								"name":      "search",
								"arguments": `{"q":"test"}`,
							},
						},
					},
				},
			},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "gemma-4", nil)
	if result == nil {
		t.Fatal("expected non-nil for reasoning+tool_calls chunk")
	}

	var c map[string]interface{}
	json.Unmarshal(result, &c)
	m := c["message"].(map[string]interface{})

	if m["thinking"] != "I need to call search" {
		t.Errorf("expected thinking=%q, got %v", "I need to call search", m["thinking"])
	}

	tcs, ok := m["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %v", m["tool_calls"])
	}
	first := tcs[0].(map[string]interface{})
	if first["id"] != "call_1" {
		t.Errorf("expected tool_call id=call_1, got %v", first["id"])
	}
}

func TestTranslateSSEGenerateToOllama_ReasoningContent(t *testing.T) {
	// /api/generate OpenAI→Ollama: reasoning_content comes in choice.text or top-level.
	// cppworker emit format (legacy): {"choices":[{"text":"thinking", "reasoning_content":"..."}]}.
	// OR: {"choices":[{"text":""}], "reasoning_content":"..."} (top-level).
	// Translate should output thinking field in ollama response.

	// Variant A: reasoning in choice
	sseChunkA := map[string]interface{}{
		"id":      "cmpl-1",
		"object":  "text_completion",
		"created": 1234567890,
		"model":   "gemma-4",
		"choices": []map[string]interface{}{
			{
				"text":             "Some text",
				"reasoning_content": "thinking...",
				"index":            0,
			},
		},
	}
	sseDataA, _ := json.Marshal(sseChunkA)
	rA := translateOpenAISSEDataToOllama("/api/generate", sseDataA, "gemma-4", nil)
	if rA == nil {
		t.Fatal("variant A returned nil")
	}
	var cA map[string]interface{}
	json.Unmarshal(rA, &cA)
	// Verify the response field is set
	if cA["response"] != "Some text" {
		t.Errorf("expected response=%q, got %v", "Some text", cA["response"])
	}
	// Note: thinking field for /api/generate Ollama format goes into "thinking" key.
	if cA["thinking"] != "thinking..." {
		t.Errorf("expected thinking=%q, got %v (variant A: in-choice)", "thinking...", cA["thinking"])
	}
}

// ============================================================
// Non-streaming reasoning_content translation
// ============================================================

func TestTranslateOpenAIChatToOllama_ReasoningContent(t *testing.T) {
	// OpenAI non-streaming response: message contains reasoning_content field.
	// Ollama expects message.reasoning (per Ollama API spec).
	openaiResp := map[string]interface{}{
		"id":      "chatcmpl-1",
		"object":  "chat.completion",
		"created": 1234567890,
		"model":   "gemma-4",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":             "assistant",
					"content":          "The answer is 42",
					"reasoning_content": "Let me think step by step...",
				},
				"finish_reason": "stop",
			},
		},
	}
	body, _ := json.Marshal(openaiResp)

	result, err := translateOpenAIChatToOllama(body, "gemma-4")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	if err := json.Unmarshal(result, &ollamaResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	message, ok := ollamaResp["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected message, got %v", ollamaResp)
	}

	if message["content"] != "The answer is 42" {
		t.Errorf("expected content=%q, got %v", "The answer is 42", message["content"])
	}

	// BUG: reasoning_content не маппится в message.reasoning
	reasoning, ok := message["reasoning"].(string)
	if !ok {
		t.Fatalf("expected message.reasoning to be a string, got %T (%v)",
			message["reasoning"], message["reasoning"])
	}
	if reasoning != "Let me think step by step..." {
		t.Errorf("expected reasoning=%q, got %q", "Let me think step by step...", reasoning)
	}
}

func TestTranslateOpenAIChatToOllama_ReasoningContentWithToolCalls(t *testing.T) {
	// gemma-4 with tools: reasoning + tool_calls in non-streaming response.
	openaiResp := map[string]interface{}{
		"id":      "chatcmpl-1",
		"object":  "chat.completion",
		"created": 1234567890,
		"model":   "gemma-4",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":              "assistant",
					"content":           "",
					"reasoning_content": "I should call search",
					"tool_calls": []map[string]interface{}{
						{
							"id":   "call_1",
							"type": "function",
							"function": map[string]interface{}{
								"name":      "search",
								"arguments": `{"q":"test"}`,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	}
	body, _ := json.Marshal(openaiResp)

	result, err := translateOpenAIChatToOllama(body, "gemma-4")
	if err != nil {
		t.Fatalf("translate failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)
	message := ollamaResp["message"].(map[string]interface{})

	// BUG: reasoning_content не маппится в message.reasoning
	if reasoning, ok := message["reasoning"].(string); !ok || reasoning != "I should call search" {
		t.Errorf("expected message.reasoning=%q, got %v", "I should call search", message["reasoning"])
	}

	tcs, ok := message["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %v", message["tool_calls"])
	}
}

func TestTranslateOpenAIChatToOllama_NoReasoning(t *testing.T) {
	// Sanity: non-reasoning model response should NOT have reasoning field.
	openaiResp := map[string]interface{}{
		"id":      "chatcmpl-1",
		"object":  "chat.completion",
		"created": 1234567890,
		"model":   "non-reasoning-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "Hello!",
				},
				"finish_reason": "stop",
			},
		},
	}
	body, _ := json.Marshal(openaiResp)

	result, _ := translateOpenAIChatToOllama(body, "non-reasoning-model")
	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)
	message := ollamaResp["message"].(map[string]interface{})

	if _, hasReasoning := message["reasoning"]; hasReasoning {
		t.Errorf("non-reasoning model should not have reasoning field, got %v", message["reasoning"])
	}
	if message["content"] != "Hello!" {
		t.Errorf("expected content=%q, got %v", "Hello!", message["content"])
	}
}

// ============================================================
// Edge cases
// ============================================================

func TestTranslateSSEChatToOllama_EmptyReasoning(t *testing.T) {
	// Empty reasoning_content should not produce thinking field.
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-1",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "gemma-4",
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{"reasoning_content": ""}},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "gemma-4", nil)
	// Empty reasoning alone is not useful — translator may emit empty chunk or nil.
	// Either is acceptable, but if it emits, the message should not have content.
	if result != nil {
		var c map[string]interface{}
		json.Unmarshal(result, &c)
		if m, ok := c["message"].(map[string]interface{}); ok {
			if thinkingVal, ok := m["thinking"].(string); ok && thinkingVal != "" {
				t.Errorf("expected empty thinking, got %q", thinkingVal)
			}
		}
	}
}

func TestTranslateSSEChatToOllama_FinalReasoningChunk(t *testing.T) {
	// Final chunk with finish_reason + reasoning_content.
	// cppworker may emit reasoning content right before finish_reason.
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-1",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "gemma-4",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"reasoning_content": "Final thought"},
				"finish_reason": "stop",
			},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "gemma-4", nil)
	if result == nil {
		t.Fatal("final reasoning chunk returned nil")
	}

	var c map[string]interface{}
	json.Unmarshal(result, &c)
	if c["done_reason"] != "stop" {
		t.Errorf("expected done_reason=stop, got %v", c["done_reason"])
	}
	m := c["message"].(map[string]interface{})
	if m["thinking"] != "Final thought" {
		t.Errorf("expected thinking=%q, got %v", "Final thought", m["thinking"])
	}
}

func TestTranslateSSEChatToOllama_ReasoningMultipleChunksAccumulation(t *testing.T) {
	// Simulate the proxyRequestLlamaCpp-style accumulation pattern:
	// Each chunk returns its own delta.reasoning_content, caller accumulates.
	// This test verifies the per-chunk behavior is consistent (no double-counting).
	chunks := []string{
		`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"I think"}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":" therefore"}}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":" ..."}}]}`,
	}

	var fullThinking string
	for _, rawChunk := range chunks {
		r := translateOpenAISSEDataToOllama("/api/chat", []byte(rawChunk), "gemma-4", nil)
		if r == nil {
			t.Fatalf("chunk %q returned nil", rawChunk)
		}
		var parsed map[string]interface{}
		json.Unmarshal(r, &parsed)
		if m, ok := parsed["message"].(map[string]interface{}); ok {
			if thinkingVal, ok := m["thinking"].(string); ok {
				fullThinking += thinkingVal
			}
		}
	}
	if fullThinking != "I think therefore ..." {
		t.Errorf("accumulated thinking=%q, want %q", fullThinking, "I think therefore ...")
	}
	_ = strings.Join  // silence unused import warning
}

