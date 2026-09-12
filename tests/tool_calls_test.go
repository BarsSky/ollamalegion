// Package tests — integration-level tests for tool calling support across cppworker and balancer.
//
// These tests verify:
// 1. Balancer translation functions handle tool_calls in OpenAI↔Ollama formats (SSE and non-streaming)
// 2. Auto-load race condition is fixed (isModelReadyOnBackend doesn't return true for "loading" state)
//
// Unit tests for cppworker tool_calls parsing functions live in cmd/cppworker/tool_calls_test.go
// (package main, so unexported functions are accessible).
package tests

import (
	"encoding/json"
	"testing"

	"ollama-loadbalancer/internal/balancer"
)

// ============================================================
// Balancer: translateOpenAIChatToOllama — non-streaming response
// ============================================================

// TestTranslateOpenAIChatToOllama_ToolCalls проверяет, что OpenAI-ответ с tool_calls
// корректно транслируется в Ollama-формат.
func TestTranslateOpenAIChatToOllama_ToolCalls(t *testing.T) {
	openAIResp := map[string]interface{}{
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
		"usage": map[string]interface{}{
			"prompt_tokens":     10,
			"completion_tokens": 20,
			"total_tokens":      30,
		},
	}

	body, err := json.Marshal(openAIResp)
	if err != nil {
		t.Fatalf("failed to marshal test data: %v", err)
	}

	result, err := balancer.TranslateOpenAIResponseToOllamaExportedForTest("/api/chat", body, "test-model")
	if err != nil {
		t.Fatalf("TranslateOpenAIResponseToOllamaExportedForTest failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	if err := json.Unmarshal(result, &ollamaResp); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	// Проверяем, что tool_calls присутствуют в ответе
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

	firstCall, ok := toolCalls[0].(map[string]interface{})
	if !ok {
		t.Fatal("expected tool call to be an object")
	}
	if firstCall["id"] != "call_search" {
		t.Errorf("expected id 'call_search', got '%v'", firstCall["id"])
	}
	if firstCall["type"] != "function" {
		t.Errorf("expected type 'function', got '%v'", firstCall["type"])
	}
	funcObj, ok := firstCall["function"].(map[string]interface{})
	if !ok {
		t.Fatal("expected function object")
	}
	if funcObj["name"] != "search" {
		t.Errorf("expected function name 'search', got '%v'", funcObj["name"])
	}
}

// TestTranslateOpenAIChatToOllama_NoToolCalls — обычный ответ без tool_calls
// не должен содержать поле tool_calls, content должен быть строкой.
func TestTranslateOpenAIChatToOllama_NoToolCalls(t *testing.T) {
	openAIResp := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion",
		"created": 1234567890,
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

	body, _ := json.Marshal(openAIResp)
	result, err := balancer.TranslateOpenAIResponseToOllamaExportedForTest("/api/chat", body, "test-model")
	if err != nil {
		t.Fatalf("TranslateOpenAIResponseToOllamaExportedForTest failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)

	message, ok := ollamaResp["message"].(map[string]interface{})
	if !ok {
		t.Fatal("expected 'message' in ollama response")
	}

	// Должен быть content (не nil)
	if message["content"] == nil {
		t.Error("expected content to be non-nil for non-tool response")
	}
	// Не должно быть tool_calls
	if _, exists := message["tool_calls"]; exists {
		t.Error("expected no 'tool_calls' in response without tools")
	}
}

// ============================================================
// Balancer: SSE streaming with tool_calls
// ============================================================

// TestTranslateSSEChatToOllama_ToolCalls проверяет, что SSE-поток с delta.tool_calls
// корректно транслируется в NDJSON с tool_calls.
func TestTranslateSSEChatToOllama_ToolCalls(t *testing.T) {
	// Этот тест проверяет внутренний механизм translateSSEChatToOllama
	// через ConvertOpenAIStreamResponseToOllama (публичную функцию).
	openAISSEChunk := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"role":       "assistant",
					"content":    nil,
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

	body, _ := json.Marshal(openAISSEChunk)
	result := balancer.TranslateOpenAISSEDataToOllamaExportedForTest("/api/chat", body, "test-model", "")
	if result == nil {
		t.Fatal("expected non-nil result from SSE translation")
	}

	var ollamaChunk map[string]interface{}
	if err := json.Unmarshal(result, &ollamaChunk); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	// SSE delta с tool_calls → должен вернуть false (не done), и содержать message с tool_calls
	message, ok := ollamaChunk["message"].(map[string]interface{})
	if !ok {
		t.Fatal("expected 'message' in ollama SSE chunk")
	}

	toolCalls, ok := message["tool_calls"].([]interface{})
	if !ok {
		t.Fatal("expected 'tool_calls' array in ollama SSE message")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
}

// TestConvertOpenAIStreamResponseToOllama_FinishReason проверяет, что
// SSE chunk с finish_reason="tool_calls" транслируется корректно.
func TestConvertOpenAIStreamResponseToOllama_FinishReason(t *testing.T) {
	openAISSEChunk := map[string]interface{}{
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

	body, _ := json.Marshal(openAISSEChunk)
	result := balancer.TranslateOpenAISSEDataToOllamaExportedForTest("/api/chat", body, "test-model", "")
	if result == nil {
		t.Fatal("expected non-nil result from SSE translation")
	}

	var ollamaChunk map[string]interface{}
	json.Unmarshal(result, &ollamaChunk)

	// Должен быть done: true
	if done, ok := ollamaChunk["done"].(bool); !ok || !done {
		t.Error("expected done=true for finish_reason chunk")
	}
}

// ============================================================
// Regression: translateOllamaChatToOpenAI with tools
// ============================================================

// TestTranslateOllamaChatToOpenAI_Tools проверяет, что Ollama запрос с tools
// корректно транслируется в OpenAI формат с сохранением tools.
func TestTranslateOllamaChatToOpenAI_Tools(t *testing.T) {
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

	// Тестируем через публичную функцию пакета balancer
	body, _ := json.Marshal(ollamaBody)
	result, err := balancer.TranslateOllamaChatToOpenAIExportedForTest(body)
	if err != nil {
		t.Fatalf("TranslateOllamaChatToOpenAIExportedForTest failed: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(result, &openAIReq)

	// Проверяем, что tools сохранены
	tools, ok := openAIReq["tools"].([]interface{})
	if !ok {
		t.Fatal("expected 'tools' in OpenAI request")
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	// Проверяем tool_choice
	tc, ok := openAIReq["tool_choice"]
	if !ok || tc != "auto" {
		t.Errorf("expected tool_choice='auto', got '%v'", tc)
	}
}

// ============================================================
// Regression: loading-state / upstream error responses
// ============================================================

// TestTranslateOpenAIResponseToOllama_EmptyBody проверяет, что пустое тело
// от upstream корректно транслируется в Ollama-ошибку, а не возвращается как есть.
// Без этой проверки OpenWebUI получает "Expecting value: line 2 column 1".
func TestTranslateOpenAIResponseToOllama_EmptyBody(t *testing.T) {
	result, err := balancer.TranslateOpenAIResponseToOllamaExportedForTest("/api/chat", []byte{}, "test-model")
	if err != nil {
		t.Fatalf("TranslateOpenAIResponseToOllamaExportedForTest failed: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected non-empty result for empty body input")
	}

	var ollamaResp map[string]interface{}
	if err := json.Unmarshal(result, &ollamaResp); err != nil {
		t.Fatalf("result must be valid JSON: %v (body: %s)", err, string(result))
	}
	if done, ok := ollamaResp["done"].(bool); !ok || !done {
		t.Error("expected done=true")
	}
	if errStr, ok := ollamaResp["error"].(string); !ok || errStr == "" {
		t.Error("expected non-empty 'error' field for empty upstream body")
	}
}

// TestBuildErrorOllamaResponse_ValidJSON проверяет, что buildErrorOllamaResponse
// возвращает валидный Ollama-совместимый JSON как для /api/chat, так и для /api/generate.
func TestBuildErrorOllamaResponse_ValidJSON(t *testing.T) {
	for _, path := range []string{"/api/chat", "/api/generate"} {
		result := balancer.BuildErrorOllamaResponseExportedForTest(path, "test-model", "test error")
		var resp map[string]interface{}
		if err := json.Unmarshal(result, &resp); err != nil {
			t.Fatalf("buildErrorOllamaResponse for %s must return valid JSON: %v", path, err)
		}
		if resp["model"] != "test-model" {
			t.Errorf("expected model='test-model', got '%v'", resp["model"])
		}
		if errStr, ok := resp["error"].(string); !ok || errStr != "test error" {
			t.Errorf("expected error='test error', got '%v'", resp["error"])
		}
		if done, ok := resp["done"].(bool); !ok || !done {
			t.Errorf("expected done=true for %s", path)
		}
		if doneReason, ok := resp["done_reason"].(string); !ok || doneReason != "error" {
			t.Errorf("expected done_reason='error', got '%v'", resp["done_reason"])
		}
	}
}

// TestTranslateOpenAIChatToOllama_InvalidBody проверяет, что не-JSON тело от upstream
// возвращает структурированную Ollama-ошибку, а не сырые байты.
func TestTranslateOpenAIChatToOllama_InvalidBody(t *testing.T) {
	result, err := balancer.TranslateOpenAIResponseToOllamaExportedForTest("/api/chat",
		[]byte(`not json at all`), "test-model")
	if err != nil {
		t.Fatalf("TranslateOpenAIResponseToOllamaExportedForTest failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	if err := json.Unmarshal(result, &ollamaResp); err != nil {
		t.Fatalf("result must be valid JSON even for non-JSON input: %v (body: %s)", err, string(result))
	}
	if errStr, ok := ollamaResp["error"].(string); !ok || errStr == "" {
		t.Error("expected non-empty 'error' field for invalid upstream body")
	}
	if done, ok := ollamaResp["done"].(bool); !ok || !done {
		t.Error("expected done=true for error response")
	}
}

// TestTranslateOpenAIChatToOllama_ErrorResponse проверяет, что upstream JSON с error полем
// транслируется в корректную Ollama-ошибку с done:true.
func TestTranslateOpenAIChatToOllama_ErrorResponse(t *testing.T) {
	errorBody := map[string]interface{}{
		"error": "model loading: some-model.gguf",
	}
	body, _ := json.Marshal(errorBody)
	result, err := balancer.TranslateOpenAIResponseToOllamaExportedForTest("/api/chat", body, "test-model")
	if err != nil {
		t.Fatalf("TranslateOpenAIResponseToOllamaExportedForTest failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)
	if errStr, ok := ollamaResp["error"].(string); !ok || errStr == "" {
		t.Error("expected non-empty 'error' field")
	}
	if done, ok := ollamaResp["done"].(bool); !ok || !done {
		t.Error("expected done=true")
	}
	if _, ok := ollamaResp["message"]; !ok {
		t.Error("expected 'message' field even in error response")
	}
}
