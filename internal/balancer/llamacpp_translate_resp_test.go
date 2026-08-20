// llamacpp_translate_resp_test.go — Unit tests for response translation
// functions in llamacpp_translate_resp.go.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ============================================================
// Test: translateOpenAIChatToOllama with tool_calls
// ============================================================

func TestTranslateOpenAIChatToOllama_ToolCalls(t *testing.T) {
	body := buildOpenAIRespWithToolCalls()
	result, err := translateOpenAIChatToOllama(body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	if err := json.Unmarshal(result, &ollamaResp); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	message, ok := ollamaResp["message"].(map[string]interface{})
	if !ok {
		t.Fatal("expected 'message' in ollama response")
	}

	toolCalls, ok := message["tool_calls"].([]interface{})
	if !ok {
		t.Fatal("expected 'tool_calls' array in ollama message")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}

	firstCall := toolCalls[0].(map[string]interface{})
	if firstCall["id"] != "call_search" {
		t.Errorf("expected id 'call_search', got '%v'", firstCall["id"])
	}
	if firstCall["type"] != "function" {
		t.Errorf("expected type 'function', got '%v'", firstCall["type"])
	}
	funcObj := firstCall["function"].(map[string]interface{})
	if funcObj["name"] != "search" {
		t.Errorf("expected function name 'search', got '%v'", funcObj["name"])
	}

	// Content should be "" (empty string, not nil) when tool_calls present
	if message["content"] == nil {
		t.Error("expected content to be empty string, not nil")
	}

	// done should be true for finish_reason="tool_calls"
	if done, ok := ollamaResp["done"].(bool); !ok || !done {
		t.Error("expected done=true for finish_reason=tool_calls")
	}

	// done_reason should be "tool_calls"
	if ollamaResp["done_reason"] != "tool_calls" {
		t.Errorf("expected done_reason 'tool_calls', got '%v'", ollamaResp["done_reason"])
	}
}

func TestTranslateOpenAIChatToOllama_NoToolCalls(t *testing.T) {
	body := buildOpenAIRespWithoutToolCalls()
	result, err := translateOpenAIChatToOllama(body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)

	message := ollamaResp["message"].(map[string]interface{})
	if message["content"] == nil || message["content"] == "" {
		t.Error("expected non-empty content for non-tool response")
	}
	if _, exists := message["tool_calls"]; exists {
		t.Error("expected no 'tool_calls' in response without tools")
	}
	if ollamaResp["done_reason"] != "stop" {
		t.Errorf("expected done_reason='stop', got '%v'", ollamaResp["done_reason"])
	}
}

func TestTranslateOpenAIChatToOllama_ErrorResponse(t *testing.T) {
	body := buildOpenAIErrorResp()
	result, err := translateOpenAIChatToOllama(body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)

	if ollamaResp["done_reason"] != "error" {
		t.Errorf("expected done_reason='error', got '%v'", ollamaResp["done_reason"])
	}
	if _, ok := ollamaResp["error"]; !ok {
		t.Error("expected 'error' field in ollama response")
	}
	if _, ok := ollamaResp["message"]; !ok {
		t.Error("expected 'message' field even in error response")
	}
}

// ============================================================
// Test: translateOpenAISSEDataToOllama / translateSSEChatToOllama
// ============================================================

func TestTranslateSSEChatToOllama_ToolCalls(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]interface{}{
						{
							"id":   "call_search",
							"type": "function",
							"function": map[string]interface{}{
								"name":      "search",
								"arguments": `{"q":"test"}`,
							},
						},
					},
				},
				"finish_reason": nil,
			},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result from SSE translation")
	}

	var ollamaChunk map[string]interface{}
	if err := json.Unmarshal(result, &ollamaChunk); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	message := ollamaChunk["message"].(map[string]interface{})
	toolCalls, ok := message["tool_calls"].([]interface{})
	if !ok {
		t.Fatal("expected 'tool_calls' array in ollama SSE chunk")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}

	// done should be false (this is a delta chunk, not finish)
	if done, ok := ollamaChunk["done"].(bool); ok && done {
		t.Error("expected done=false for delta chunk")
	}
}

func TestTranslateSSEChatToOllama_FinishReasonToolCalls(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{},
				"finish_reason": "tool_calls",
			},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result from SSE translation")
	}

	var ollamaChunk map[string]interface{}
	json.Unmarshal(result, &ollamaChunk)

	if done, ok := ollamaChunk["done"].(bool); !ok || !done {
		t.Error("expected done=true for finish_reason chunk")
	}
	if ollamaChunk["done_reason"] != "tool_calls" {
		t.Errorf("expected done_reason='tool_calls', got '%v'", ollamaChunk["done_reason"])
	}
}

func TestTranslateSSEChatToOllama_NormalContent(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"role":    "assistant",
					"content": "Hello",
				},
			},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	var ollamaChunk map[string]interface{}
	json.Unmarshal(result, &ollamaChunk)

	message := ollamaChunk["message"].(map[string]interface{})
	if message["content"] != "Hello" {
		t.Errorf("expected content 'Hello', got '%v'", message["content"])
	}
	if _, exists := message["tool_calls"]; exists {
		t.Error("unexpected tool_calls in normal content chunk")
	}
}

func TestTranslateSSEChatToOllama_DONE(t *testing.T) {
	result := translateOpenAISSEDataToOllama("/api/chat", []byte("[DONE]"), "test-model", nil, time.Time{})
	if result != nil {
		t.Error("expected nil for [DONE] marker")
	}
}

// ============================================================
// Round 51.3 (2026-08-20): regression tests for finish_reason
// handling. Cline (через ollama npm / langchain) валит с
// "Did not receive done or success response in stream" если
// финальный чанк имеет done=false. Прежний код ставил done:true
// только для finish_reason="stop" или "tool_calls" — для
// "length" (max_tokens hit) или "" (пустая) done оставался false.
// ============================================================

// TestTranslateSSEChatToOllama_FinishReasonLength — finish_reason="length"
// в streaming /api/chat → done:true. Cline path.
func TestTranslateSSEChatToOllama_FinishReasonLength(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-length",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "length",
			},
		},
	}
	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result for finish_reason=length chunk")
	}
	var ollamaChunk map[string]interface{}
	json.Unmarshal(result, &ollamaChunk)
	if done, ok := ollamaChunk["done"].(bool); !ok || !done {
		t.Errorf("expected done=true for finish_reason=length, got done=%v (ok=%v) — Cline will throw 'Did not receive done'",
			ollamaChunk["done"], ok)
	}
	if ollamaChunk["done_reason"] != "length" {
		t.Errorf("expected done_reason='length', got '%v'", ollamaChunk["done_reason"])
	}
}

// TestTranslateSSEChatToOllama_FinishReasonContentFilter — finish_reason="content_filter"
// → done:true (Cline/ollama npm).
func TestTranslateSSEChatToOllama_FinishReasonContentFilter(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-cf",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "content_filter",
			},
		},
	}
	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result for finish_reason=content_filter chunk")
	}
	var ollamaChunk map[string]interface{}
	json.Unmarshal(result, &ollamaChunk)
	if done, ok := ollamaChunk["done"].(bool); !ok || !done {
		t.Errorf("expected done=true for finish_reason=content_filter, got %v", ollamaChunk["done"])
	}
}

// TestTranslateSSEChatToOllama_FinishReasonNull — in-progress chunk (no finish_reason yet)
// → done:false. Sanity check that fix didn't break in-progress case.
func TestTranslateSSEChatToOllama_FinishReasonNull(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-progress",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"content": "tok"},
				"finish_reason": nil,
			},
		},
	}
	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result for in-progress chunk")
	}
	var ollamaChunk map[string]interface{}
	json.Unmarshal(result, &ollamaChunk)
	if done, ok := ollamaChunk["done"].(bool); !ok || done {
		t.Errorf("expected done=false for in-progress chunk (finish_reason=null), got %v", ollamaChunk["done"])
	}
}

// TestTranslateOpenAIChatToOllama_FinishReasonLength — non-streaming /api/chat
// translation: finish_reason="length" → done:true.
func TestTranslateOpenAIChatToOllama_FinishReasonLength(t *testing.T) {
	resp := map[string]interface{}{
		"id":      "chatcmpl-length",
		"object":  "chat.completion",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "truncated...",
				},
				"finish_reason": "length",
			},
		},
	}
	body, _ := json.Marshal(resp)
	result, err := translateOpenAIChatToOllama(body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama: %v", err)
	}
	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)
	if done, ok := ollamaResp["done"].(bool); !ok || !done {
		t.Errorf("expected done=true for finish_reason=length, got %v — Cline will throw", ollamaResp["done"])
	}
	if ollamaResp["done_reason"] != "length" {
		t.Errorf("expected done_reason='length', got '%v'", ollamaResp["done_reason"])
	}
}

// TestTranslateOpenAICompletionToOllama_FinishReasonLength — non-streaming /v1/completions
// translation: finish_reason="length" → done:true.
func TestTranslateOpenAICompletionToOllama_FinishReasonLength(t *testing.T) {
	resp := map[string]interface{}{
		"id":      "cmpl-length",
		"object":  "text_completion",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"text":          "truncated...",
				"finish_reason": "length",
			},
		},
	}
	body, _ := json.Marshal(resp)
	result, err := translateOpenAICompletionToOllama(body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAICompletionToOllama: %v", err)
	}
	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)
	if done, ok := ollamaResp["done"].(bool); !ok || !done {
		t.Errorf("expected done=true for finish_reason=length, got %v", ollamaResp["done"])
	}
}

// ============================================================
// Test: translateOllamaChatToOpenAI with tools
// ============================================================

func TestTranslateOllamaChatToOpenAI_ToolsPassthrough(t *testing.T) {
	ollamaBody := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Search for AI news"},
		},
		"stream": false,
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "search",
					"description": "Search the web",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"q": map[string]interface{}{
								"type":        "string",
								"description": "Search query",
							},
						},
					},
				},
			},
		},
		"tool_choice": "auto",
	}

	body, _ := json.Marshal(ollamaBody)
	result, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(result, &openAIReq)

	tools, ok := openAIReq["tools"].([]interface{})
	if !ok {
		t.Fatal("expected 'tools' in OpenAI request")
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	if openAIReq["tool_choice"] != "auto" {
		t.Errorf("expected tool_choice='auto', got '%v'", openAIReq["tool_choice"])
	}
}

func TestTranslateOllamaChatToOpenAI_NoTools(t *testing.T) {
	ollamaBody := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Hello"},
		},
		"stream": true,
	}

	body, _ := json.Marshal(ollamaBody)
	result, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(result, &openAIReq)

	if _, exists := openAIReq["tools"]; exists {
		t.Error("unexpected 'tools' in request without tools")
	}
	if _, exists := openAIReq["tool_choice"]; exists {
		t.Error("unexpected 'tool_choice' in request without tools")
	}
}

// ============================================================
// Test: translateOpenAIResponseToOllama — multiple scenarios
// ============================================================

func TestTranslateOpenAIResponseToOllama_Embeddings(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"data": []map[string]interface{}{
			{
				"embedding": []float64{0.1, 0.2, 0.3},
			},
		},
	})

	result, err := translateOpenAIResponseToOllama("/api/embeddings", body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIResponseToOllama failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)

	embedding, ok := ollamaResp["embedding"].([]interface{})
	if !ok {
		t.Fatal("expected 'embedding' array")
	}
	if len(embedding) != 3 {
		t.Fatalf("expected 3 values, got %d", len(embedding))
	}
}

func TestTranslateOllamaChatToOpenAI_ExtractsOptions(t *testing.T) {
	ollamaBody := map[string]interface{}{
		"model": "test",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hi"},
		},
		"options": map[string]interface{}{
			"temperature": 0.7,
			"top_p":      0.9,
			"num_predict": 100,
			"stop":       []string{"\n"},
		},
		"stream": true,
	}

	body, _ := json.Marshal(ollamaBody)
	result, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(result, &openAIReq)

	if openAIReq["temperature"] != 0.7 {
		t.Errorf("expected temperature 0.7, got %v", openAIReq["temperature"])
	}
	if openAIReq["top_p"] != 0.9 {
		t.Errorf("expected top_p 0.9, got %v", openAIReq["top_p"])
	}
	if openAIReq["max_tokens"] != 100.0 {
		t.Errorf("expected max_tokens 100, got %v", openAIReq["max_tokens"])
	}
}

// ============================================================
// Helpers
// ============================================================

func buildOpenAIRespWithToolCalls() []byte {
	resp := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]interface{}{
						{
							"id":   "call_search",
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
	body, _ := json.Marshal(resp)
	return body
}

func buildOpenAIRespWithoutToolCalls() []byte {
	resp := map[string]interface{}{
		"id":      "chatcmpl-456",
		"object":  "chat.completion",
		"created": 1234567891,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "Hello, how can I help?",
				},
				"finish_reason": "stop",
			},
		},
	}
	body, _ := json.Marshal(resp)
	return body
}

func buildOpenAIErrorResp() []byte {
	resp := map[string]interface{}{
		"error": "rate limit exceeded",
	}
	body, _ := json.Marshal(resp)
	return body
}

// Helper to check if a string contains a substring (case-insensitive)
func strContains(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

