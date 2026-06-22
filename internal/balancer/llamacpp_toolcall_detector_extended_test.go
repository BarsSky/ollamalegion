// llamacpp_toolcall_detector_extended_test.go — Tests for new tool_call formats in balancer.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// ============================================================
// Tests for Hermes / Qwen 2.5 <tool_call> format detection
// ============================================================

func TestDetectHermesToolCallsInContent_SingleObject(t *testing.T) {
	content := `<tool_call>
{"name": "search", "arguments": {"q": "AI news"}}
</tool_call>`
	calls, remaining, found := detectHermesToolCallsInContent(content)
	if !found {
		t.Fatal("expected to detect Hermes tool call")
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	tc := calls[0].(map[string]interface{})
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "search" {
		t.Errorf("expected name 'search', got %v", fn["name"])
	}
	// После удаления <tool_call>...</tool_call>, remaining должен быть пустым.
	if strings.Contains(remaining, "<tool_call>") || strings.Contains(remaining, "</tool_call>") {
		t.Errorf("expected <tool_call> tags to be removed, got %q", remaining)
	}
}

func TestDetectHermesToolCallsInContent_ArrayOfObjects(t *testing.T) {
	content := `<tool_call>
[{"name": "a", "arguments": {"x": 1}}, {"name": "b", "arguments": {"y": 2}}]
</tool_call>`
	calls, _, found := detectHermesToolCallsInContent(content)
	if !found {
		t.Fatal("expected detection")
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
}

func TestDetectHermesToolCallsInContent_NoMatch(t *testing.T) {
	content := `Just plain text without tool calls`
	calls, _, found := detectHermesToolCallsInContent(content)
	if found {
		t.Errorf("expected no detection, got %d calls", len(calls))
	}
}

// ============================================================
// Tests for Llama-3 <|python_tag|> format detection
// ============================================================

func TestDetectLlamaPythonTagInContent_SingleObject(t *testing.T) {
	content := `<|python_tag|>{"name": "search", "parameters": {"q": "test"}}`
	calls, _, found := detectLlamaPythonTagInContent(content)
	if !found {
		t.Fatal("expected detection")
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	tc := calls[0].(map[string]interface{})
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "search" {
		t.Errorf("expected name 'search', got %v", fn["name"])
	}
}

func TestDetectLlamaPythonTagInContent_WithEomId(t *testing.T) {
	content := `<|python_tag|>{"name":"search","parameters":{"q":"x"}}<|eom_id|>`
	calls, remaining, found := detectLlamaPythonTagInContent(content)
	if !found {
		t.Fatal("expected detection")
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	// remaining должен содержать только <|eom_id|>
	if !strings.Contains(remaining, "<|eom_id|>") {
		t.Errorf("expected remaining to contain <|eom_id|>, got %q", remaining)
	}
	if strings.Contains(remaining, "<|python_tag|>") {
		t.Errorf("remaining should not contain <|python_tag|>, got %q", remaining)
	}
}

func TestDetectLlamaPythonTagInContent_NoMatch(t *testing.T) {
	content := `Some text without python_tag`
	_, _, found := detectLlamaPythonTagInContent(content)
	if found {
		t.Error("expected no detection")
	}
}

// ============================================================
// Tests for Mistral Nemo [TOOL_CALLS] format detection
// ============================================================

func TestDetectMistralToolCallsInContent_Basic(t *testing.T) {
	content := `[TOOL_CALLS][{"name": "search", "arguments": {"q": "x"}}]`
	calls, remaining, found := detectMistralToolCallsInContent(content)
	if !found {
		t.Fatal("expected detection")
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if strings.Contains(remaining, "[TOOL_CALLS]") {
		t.Errorf("expected [TOOL_CALLS] removed from remaining")
	}
}

func TestDetectMistralToolCallsInContent_WithToolResults(t *testing.T) {
	content := `[TOOL_CALLS][{"name": "search", "arguments": {"q": "x"}}][TOOL_RESULTS][{"name": "search", "content": "result"}]`
	calls, _, found := detectMistralToolCallsInContent(content)
	if !found {
		t.Fatal("expected detection")
	}
	if len(calls) != 1 {
		t.Errorf("expected 1 call (TOOL_RESULTS should be ignored), got %d", len(calls))
	}
}

func TestDetectMistralToolCallsInContent_NoMatch(t *testing.T) {
	content := `No tool calls here`
	_, _, found := detectMistralToolCallsInContent(content)
	if found {
		t.Error("expected no detection")
	}
}

// ============================================================
// Tests for Single-object JSON detection
// ============================================================

func TestDetectSingleObjectToolCallInContent_Basic(t *testing.T) {
	content := `{"name": "search", "arguments": {"q": "test"}}`
	calls, _, found := detectSingleObjectToolCallInContent(content)
	if !found {
		t.Fatal("expected detection")
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
}

func TestDetectSingleObjectToolCallInContent_WithFunctionField(t *testing.T) {
	content := `{"function": "search", "arguments": {"q": "test"}}`
	calls, _, found := detectSingleObjectToolCallInContent(content)
	if !found {
		t.Fatal("expected detection")
	}
	tc := calls[0].(map[string]interface{})
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "search" {
		t.Errorf("expected name 'search', got %v", fn["name"])
	}
}

func TestDetectSingleObjectToolCallInContent_NoToolCall(t *testing.T) {
	content := `{"unrelated": "value"}`
	_, _, found := detectSingleObjectToolCallInContent(content)
	if found {
		t.Error("expected no detection for non-tool-call object")
	}
}

// ============================================================
// Tests for unified detectAndExtractToolCallsFromContent
// ============================================================

func TestDetectUnified_Hermes(t *testing.T) {
	content := `<tool_call>{"name": "search", "arguments": {"q": "test"}}</tool_call>`
	calls, _, found := detectAndExtractToolCallsFromContent(content)
	if !found || len(calls) != 1 {
		t.Errorf("expected 1 Hermes tool call, got %d found=%v", len(calls), found)
	}
}

func TestDetectUnified_Llama3(t *testing.T) {
	content := `<|python_tag|>{"name": "search", "parameters": {"q": "test"}}`
	calls, _, found := detectAndExtractToolCallsFromContent(content)
	if !found || len(calls) != 1 {
		t.Errorf("expected 1 Llama-3 tool call, got %d found=%v", len(calls), found)
	}
}

func TestDetectUnified_Mistral(t *testing.T) {
	content := `[TOOL_CALLS][{"name": "search", "arguments": {"q": "test"}}]`
	calls, _, found := detectAndExtractToolCallsFromContent(content)
	if !found || len(calls) != 1 {
		t.Errorf("expected 1 Mistral tool call, got %d found=%v", len(calls), found)
	}
}

func TestDetectUnified_StandardArray(t *testing.T) {
	content := `[{"id": "call_abc", "type": "function", "function": {"name": "search", "arguments": "{}"}}]`
	calls, _, found := detectAndExtractToolCallsFromContent(content)
	if !found || len(calls) != 1 {
		t.Errorf("expected 1 standard tool call, got %d found=%v", len(calls), found)
	}
}

// ============================================================
// Tests for extractToolCallsFromSSEContent with new formats
// ============================================================

func TestExtractToolCallsFromSSEContent_Hermes(t *testing.T) {
	sseData := `{"choices":[{"delta":{"role":"assistant","content":"<tool_call>{\"name\":\"search\",\"arguments\":{\"q\":\"test\"}}</tool_call>"},"finish_reason":null,"index":0}],"model":"qwen"}`
	modified := extractToolCallsFromSSEContent([]byte(sseData))
	if modified == nil {
		t.Fatal("expected modified chunk")
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal(modified, &chunk); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	delta := chunk["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	if _, hasContent := delta["content"]; hasContent {
		t.Error("expected content to be removed")
	}
	if _, hasTC := delta["tool_calls"]; !hasTC {
		t.Error("expected tool_calls to be added")
	}
}

func TestExtractToolCallsFromSSEContent_Llama3(t *testing.T) {
	sseData := `{"choices":[{"delta":{"role":"assistant","content":"<|python_tag|>{\"name\":\"search\",\"parameters\":{\"q\":\"test\"}}"},"finish_reason":null,"index":0}],"model":"llama3"}`
	modified := extractToolCallsFromSSEContent([]byte(sseData))
	if modified == nil {
		t.Fatal("expected modified chunk")
	}
	var chunk map[string]interface{}
	json.Unmarshal(modified, &chunk)
	delta := chunk["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	if _, hasContent := delta["content"]; hasContent {
		t.Error("expected content to be removed")
	}
	if _, hasTC := delta["tool_calls"]; !hasTC {
		t.Error("expected tool_calls to be added")
	}
}

// ============================================================
// Tests for findMatchingClosingBrace
// ============================================================

func TestFindMatchingClosingBrace_Simple(t *testing.T) {
	s := `{"a":1}`
	pos := findMatchingClosingBrace(s, 0)
	if pos != 6 {
		t.Errorf("expected pos 6, got %d", pos)
	}
}

func TestFindMatchingClosingBrace_Nested(t *testing.T) {
	s := `{"a":{"b":1}}`
	pos := findMatchingClosingBrace(s, 0)
	// Длина строки = 13, последний '}' на позиции 12 (0-based).
	if pos != 12 {
		t.Errorf("expected pos 12 (last char), got %d", pos)
	}
}

func TestFindMatchingClosingBrace_WithString(t *testing.T) {
	s := `{"a":"}inner}"}`
	pos := findMatchingClosingBrace(s, 0)
	if pos != len(s)-1 {
		t.Errorf("expected last char, got pos %d", pos)
	}
}

// ============================================================
// Real OpenWebUI scenarios
// ============================================================

func TestRealScenario_OpenWebUI_ToolCall_Gemma(t *testing.T) {
	content := `[{"id":"call_abc","type":"function","function":{"name":"search","arguments":"{\"q\":\"AI\"}"}}]`
	calls, _, found := detectAndExtractToolCallsFromContent(content)
	if !found || len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d found=%v", len(calls), found)
	}
}

func TestRealScenario_OpenWebUI_ToolCall_Qwen(t *testing.T) {
	content := `<tool_call>
{"name": "search", "arguments": {"q": "AI news"}}
</tool_call>`
	calls, _, found := detectAndExtractToolCallsFromContent(content)
	if !found || len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d found=%v", len(calls), found)
	}
}

func TestRealScenario_OpenWebUI_ToolCall_Llama3(t *testing.T) {
	content := `<|python_tag|>{"name":"search","parameters":{"q":"x"}}<|eom_id|>`
	calls, _, found := detectAndExtractToolCallsFromContent(content)
	if !found || len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d found=%v", len(calls), found)
	}
}
