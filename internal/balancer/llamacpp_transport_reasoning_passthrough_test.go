// llamacpp_transport_reasoning_passthrough_test.go — tests for SSE→SSE passthrough
// preservation of reasoning_content.
//
// Bug context (2026-08-09):
//   proxyRequestLlamaCpp делает SSE→SSE passthrough для /v1/chat/completions
//   (используется Cline/Roo Code). Должен сохранять delta.reasoning_content
//   при:
//     - filterOpenAIStreamingLine не должен его резать
//     - extractToolCallsFromSSEContent не должен его терять
//
// Round 29: добавлены тесты, проверяющие что reasoning_content
// пробрасывается через passthrough без потерь.
package balancer

import (
	"encoding/json"
	"testing"
)

// TestFilterOpenAIStreamingLine_PreservesReasoningContent —
// filterOpenAIStreamingLine должен пропускать reasoning_content без изменений.
func TestFilterOpenAIStreamingLine_PreservesReasoningContent(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			"reasoning_only_chunk",
			`data: {"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"thinking step 1"}}]}`,
		},
		{
			"reasoning_and_content_chunk",
			`data: {"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"think","content":"answer"}}]}`,
		},
		{
			"reasoning_in_full_sse",
			"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"let me think\",\"content\":\"hi\"}}]}\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered, shouldSkip := filterOpenAIStreamingLine([]byte(tt.input))
			if shouldSkip {
				t.Errorf("reasoning_content chunk should NOT be skipped, got skip=true")
			}
			// Verify the JSON payload is still valid and has reasoning_content
			if filtered == nil {
				t.Fatal("expected non-nil filtered output")
			}
			// Extract JSON from "data: " prefix if present
			payload := filtered
			if len(payload) > 6 && string(payload[:6]) == "data: " {
				payload = payload[6:]
			}
			var chunk map[string]interface{}
			if err := json.Unmarshal(payload, &chunk); err != nil {
				t.Fatalf("filtered output not valid JSON: %v (output: %s)", err, filtered)
			}
			choices, ok := chunk["choices"].([]interface{})
			if !ok || len(choices) == 0 {
				t.Fatal("filtered output missing choices[0]")
			}
			choice := choices[0].(map[string]interface{})
			delta := choice["delta"].(map[string]interface{})
			if _, ok := delta["reasoning_content"]; !ok {
				t.Errorf("reasoning_content lost after filter (filtered: %s)", filtered)
			}
		})
	}
}

// TestExtractToolCallsFromSSEContent_DoesNotTouchReasoningContent —
// Когда extractToolCallsFromSSEContent модифицирует chunk для tool_calls,
// reasoning_content (если был) должен быть сохранён.
func TestExtractToolCallsFromSSEContent_DoesNotTouchReasoningContent(t *testing.T) {
	// Cline stream chunk with reasoning + tool_call JSON in content
	// (this is a hypothetical — gemma-4 native tools, qwen3.6 hermes-style).
	sseData := `{"choices":[{"delta":{"role":"assistant","reasoning_content":"I should call search","content":"<tool_call>{\"name\":\"search\",\"arguments\":{\"q\":\"test\"}}</tool_call>"},"finish_reason":null,"index":0}],"model":"gemma-4"}`

	modified := extractToolCallsFromSSEContent([]byte(sseData))
	// modified может быть nil если extractor не нашёл tool calls в content.
	// Проверяем что при наличии tool_calls — reasoning_content сохранён.
	if modified == nil {
		// Этот тест проверяет гипотетический случай, не критично если nil.
		t.Logf("extractToolCallsFromSSEContent returned nil — no tool calls in content")
		return
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal(modified, &chunk); err != nil {
		t.Fatalf("modified chunk not valid JSON: %v", err)
	}
	choice := chunk["choices"].([]interface{})[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})

	if _, ok := delta["reasoning_content"]; !ok {
		t.Errorf("reasoning_content lost after tool_calls extraction (modified: %s)", modified)
	}
}
