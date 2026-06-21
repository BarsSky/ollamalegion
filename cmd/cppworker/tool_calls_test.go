// tool_calls_test.go — Unit tests for tool_calls parsing and normalization.
// Package: main (same as tool_calls.go), so unexported functions are accessible.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ============================================================
// Test: parseToolCallsFromOutput
// ============================================================

func TestParseToolCallsFromOutput_EmptyInput(t *testing.T) {
	result := parseToolCallsFromOutput("")
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestParseToolCallsFromOutput_Strategy1_FullJSONArray(t *testing.T) {
	// Strategy 1: entire output is a JSON array of tool calls
	output := `[{"id":"call_search","type":"function","function":{"name":"search","arguments":"{\"q\":\"test\"}"}}]`
	result := parseToolCallsFromOutput(output)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result))
	}
	if result[0].Function.Name != "search" {
		t.Errorf("expected function name 'search', got '%s'", result[0].Function.Name)
	}
	if result[0].ID != "call_search" {
		t.Errorf("expected ID 'call_search', got '%s'", result[0].ID)
	}
	if result[0].Type != "function" {
		t.Errorf("expected type 'function', got '%s'", result[0].Type)
	}
}

func TestParseToolCallsFromOutput_Strategy2_JSONBlock(t *testing.T) {
	// Strategy 2: JSON inside ```json ... ``` block
	output := "Let me search for that.\n```json\n[{\"id\":\"call_search\",\"type\":\"function\",\"function\":{\"name\":\"search\",\"arguments\":\"{\\\"q\\\":\\\"test\\\"}\"}}]\n```"
	result := parseToolCallsFromOutput(output)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result))
	}
	if result[0].Function.Name != "search" {
		t.Errorf("expected 'search', got '%s'", result[0].Function.Name)
	}
}

func TestParseToolCallsFromOutput_Strategy3_JSONAtEnd(t *testing.T) {
	// Strategy 3: JSON array at the end of a line
	output := "Some reasoning text...\n[{\"id\":\"call_search\",\"type\":\"function\",\"function\":{\"name\":\"search\",\"arguments\":\"{\\\"q\\\":\\\"test\\\"}\"}}]"
	result := parseToolCallsFromOutput(output)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result))
	}
	if result[0].Function.Name != "search" {
		t.Errorf("expected 'search', got '%s'", result[0].Function.Name)
	}
}

func TestParseToolCallsFromOutput_MultipleTools(t *testing.T) {
	output := `[{"id":"call_search","type":"function","function":{"name":"search","arguments":"{\"q\":\"hello\"}"}},{"id":"call_calc","type":"function","function":{"name":"calculator","arguments":"{\"expr\":\"2+2\"}"}}]`
	result := parseToolCallsFromOutput(output)
	if len(result) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(result))
	}
	if result[0].Function.Name != "search" {
		t.Errorf("expected first function 'search', got '%s'", result[0].Function.Name)
	}
	if result[1].Function.Name != "calculator" {
		t.Errorf("expected second function 'calculator', got '%s'", result[1].Function.Name)
	}
}

func TestParseToolCallsFromOutput_InvalidJSON(t *testing.T) {
	output := "Just some random text without any tool calls"
	result := parseToolCallsFromOutput(output)
	if result != nil {
		t.Errorf("expected nil for invalid input, got %v", result)
	}
}

func TestParseToolCallsFromOutput_NoToolCalls(t *testing.T) {
	output := "This is a normal response without any tool calls. Here is the answer to your question."
	result := parseToolCallsFromOutput(output)
	if result != nil {
		t.Errorf("expected nil for normal text, got %v", result)
	}
}

func TestParseToolCallsFromOutput_EmptyArray(t *testing.T) {
	// Empty JSON array should return nil
	result := parseToolCallsFromOutput("[]")
	if result != nil {
		t.Errorf("expected nil for empty array, got %v", result)
	}
}

// ============================================================
// Test: normalizeToolCalls
// ============================================================

func TestNormalizeToolCalls_Empty(t *testing.T) {
	calls := []openAIToolCall{}
	if normalizeToolCalls(calls) {
		t.Error("expected false for empty calls")
	}
}

func TestNormalizeToolCalls_AssignsID(t *testing.T) {
	calls := []openAIToolCall{
		{
			Type: "function",
			Function: openAIFunctionCall{
				Name:      "search",
				Arguments: `{"q":"test"}`,
			},
		},
	}
	if !normalizeToolCalls(calls) {
		t.Fatal("expected normalize to succeed")
	}
	if calls[0].ID != "call_search" {
		t.Errorf("expected auto-generated ID 'call_search', got '%s'", calls[0].ID)
	}
}

func TestNormalizeToolCalls_FillsType(t *testing.T) {
	calls := []openAIToolCall{
		{
			Function: openAIFunctionCall{
				Name:      "search",
				Arguments: `{"q":"test"}`,
			},
		},
	}
	if !normalizeToolCalls(calls) {
		t.Fatal("expected normalize to succeed")
	}
	if calls[0].Type != "function" {
		t.Errorf("expected type 'function', got '%s'", calls[0].Type)
	}
}

func TestNormalizeToolCalls_EmptyName(t *testing.T) {
	calls := []openAIToolCall{
		{
			Type: "function",
			Function: openAIFunctionCall{
				Name:      "",
				Arguments: `{}`,
			},
		},
	}
	if normalizeToolCalls(calls) {
		t.Error("expected false for empty function name")
	}
}

func TestNormalizeToolCalls_WrongType(t *testing.T) {
	calls := []openAIToolCall{
		{
			Type: "builtin",
			Function: openAIFunctionCall{
				Name:      "search",
				Arguments: `{}`,
			},
		},
	}
	if normalizeToolCalls(calls) {
		t.Error("expected false for non-function type")
	}
}

func TestNormalizeToolCalls_PreservesExistingID(t *testing.T) {
	calls := []openAIToolCall{
		{
			ID:   "custom-id-123",
			Type: "function",
			Function: openAIFunctionCall{
				Name:      "search",
				Arguments: `{}`,
			},
		},
	}
	if !normalizeToolCalls(calls) {
		t.Fatal("expected normalize to succeed")
	}
	if calls[0].ID != "custom-id-123" {
		t.Errorf("expected preserved ID 'custom-id-123', got '%s'", calls[0].ID)
	}
}

// ============================================================
// Test: buildToolsSystemPrompt
// ============================================================

func TestBuildToolsSystemPrompt_Empty(t *testing.T) {
	prompt := buildToolsSystemPrompt(nil)
	if prompt != "" {
		t.Errorf("expected empty prompt, got '%s'", prompt)
	}
}

func TestBuildToolsSystemPrompt_EmptySlice(t *testing.T) {
	prompt := buildToolsSystemPrompt([]openAITool{})
	if prompt != "" {
		t.Errorf("expected empty prompt for empty slice, got '%s'", prompt)
	}
}

func TestBuildToolsSystemPrompt_SingleTool(t *testing.T) {
	tools := []openAITool{
		{
			Type: "function",
			Function: openAIFunction{
				Name:        "search",
				Description: "Search the web",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"q": {"type": "string", "description": "Search query"}
					}
				}`),
			},
		},
	}
	prompt := buildToolsSystemPrompt(tools)
	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	if !strings.Contains(prompt, "search") {
		t.Error("expected prompt to contain tool name 'search'")
	}
	if !strings.Contains(prompt, "Search the web") {
		t.Error("expected prompt to contain tool description")
	}
	if !strings.Contains(prompt, "q") {
		t.Error("expected prompt to contain parameter name")
	}
}

func TestBuildToolsSystemPrompt_MultipleTools(t *testing.T) {
	tools := []openAITool{
		{
			Type: "function",
			Function: openAIFunction{
				Name:        "search",
				Description: "Search the web",
			},
		},
		{
			Type: "function",
			Function: openAIFunction{
				Name:        "calculator",
				Description: "Perform calculations",
			},
		},
	}
	prompt := buildToolsSystemPrompt(tools)
	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	if !strings.Contains(prompt, "search") {
		t.Error("expected prompt to contain 'search'")
	}
	if !strings.Contains(prompt, "calculator") {
		t.Error("expected prompt to contain 'calculator'")
	}
	if !strings.Contains(prompt, "JSON array") {
		t.Error("expected prompt to contain JSON format instructions")
	}
}

// ============================================================
// Integration-style: augmentSystemWithTools
// ============================================================

func TestAugmentSystemWithTools_NoSystemMessage(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "Hello"},
	}
	tools := []openAITool{
		{
			Type: "function",
			Function: openAIFunction{
				Name: "search",
			},
		},
	}
	result := augmentSystemWithTools(msgs, tools)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Role != "system" {
		t.Errorf("expected first message role 'system', got '%s'", result[0].Role)
	}
	if !strings.Contains(result[0].Content, "search") {
		t.Error("expected system message to contain tool definition")
	}
	if result[1].Role != "user" {
		t.Errorf("expected second message role 'user', got '%s'", result[1].Role)
	}
}

func TestAugmentSystemWithTools_ExistingSystemMessage(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello"},
	}
	tools := []openAITool{
		{
			Type: "function",
			Function: openAIFunction{
				Name: "search",
			},
		},
	}
	result := augmentSystemWithTools(msgs, tools)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if !strings.Contains(result[0].Content, "You are a helpful assistant") {
		t.Error("expected original system content preserved")
	}
	if !strings.Contains(result[0].Content, "search") {
		t.Error("expected system message augmented with tool info")
	}
}

func TestAugmentSystemWithTools_EmptyTools(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "Hello"},
	}
	result := augmentSystemWithTools(msgs, nil)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
}

// ============================================================
// Test: openAIToChatMessage
// ============================================================

func TestOpenAIToChatMessage_PreservesToolFields(t *testing.T) {
	openAIMsgs := []openAIChatMessage{
		{
			Role:    "assistant",
			Content: "Let me search for that",
			ToolCalls: []openAIToolCall{
				{ID: "call_1", Type: "function", Function: openAIFunctionCall{Name: "search", Arguments: `{}`}},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_1",
			Name:       "search",
			Content:    `{"result": "data"}`,
		},
	}
	result := openAIToChatMessage(openAIMsgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if len(result[0].ToolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(result[0].ToolCalls))
	}
	if result[1].ToolCallID != "call_1" {
		t.Errorf("expected ToolCallID 'call_1', got '%s'", result[1].ToolCallID)
	}
	if result[1].Name != "search" {
		t.Errorf("expected Name 'search', got '%s'", result[1].Name)
	}
}

// ============================================================
// Test: buildChatPromptFromMessages with tool roles
// ============================================================

func TestBuildChatPromptFromMessages_WithToolMessages(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: "Search for AI news."},
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []openAIToolCall{
				{ID: "call_1", Type: "function", Function: openAIFunctionCall{Name: "search", Arguments: `{"q":"AI news"}`}},
			},
		},
		{Role: "tool", Name: "search", ToolCallID: "call_1", Content: `{"results":["AI advances"]}`},
	}
	prompt := buildChatPromptFromMessages(msgs, "llama3")
	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	if !strings.Contains(prompt, "tool_call_result") {
		t.Error("expected tool_call_result marker in prompt")
	}
	if !strings.Contains(prompt, "call_1") {
		t.Error("expected tool call ID in serialized output")
	}
}

// ============================================================
// Test: msgsToBridge with tool calls
// ============================================================

func TestMsgsToBridge_WithToolCalls(t *testing.T) {
	msgs := []chatMessage{
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []openAIToolCall{
				{ID: "call_1", Type: "function", Function: openAIFunctionCall{Name: "search", Arguments: `{}`}},
			},
		},
	}
	result := msgsToBridge(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 bridge message, got %d", len(result))
	}
	if !strings.Contains(result[0].Content, "call_1") {
		t.Error("expected tool call serialized in bridge content")
	}
	if !strings.Contains(result[0].Content, "search") {
		t.Error("expected function name in serialized bridge content")
	}
}

func TestMsgsToBridge_ToolRole(t *testing.T) {
	msgs := []chatMessage{
		{Role: "tool", ToolCallID: "call_1", Content: `{"result":"ok"}`},
	}
	result := msgsToBridge(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 bridge message, got %d", len(result))
	}
	if result[0].Role != "tool" {
		t.Errorf("expected role 'tool', got '%s'", result[0].Role)
	}
}
