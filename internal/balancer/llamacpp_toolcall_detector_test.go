package balancer

import (
	"encoding/json"
	"testing"
)

func TestDetectAndExtractToolCallsFromContent(t *testing.T) {
	tests := []struct {
		name              string
		content           string
		wantFound         bool
		wantToolCallCount int
		wantRemaining     string
	}{
		{
			name:              "empty content",
			content:           "",
			wantFound:         false,
			wantToolCallCount: 0,
			wantRemaining:     "",
		},
		{
			name:              "no JSON array",
			content:           "Hello world",
			wantFound:         false,
			wantToolCallCount: 0,
			wantRemaining:     "Hello world",
		},
		{
			name:              "simple tool call JSON",
			content:           `[{"id":"call_search","type":"function","function":{"name":"search","arguments":"{\"q\":\"weather\"}"}}]`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantRemaining:     "",
		},
		{
			name:              "tool call with trailing service token",
			content:           `[{"id":"call_search","type":"function","function":{"name":"search","arguments":"{\"q\":\"weather\"}"}}]</start_of_turn>`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantRemaining:     `</start_of_turn>`,
		},
		{
			name:              "multiple tool calls",
			content:           `[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"calculator","arguments":"{}"}}]`,
			wantFound:         true,
			wantToolCallCount: 2,
			wantRemaining:     "",
		},
		{
			name:              "tool call with surrounding text",
			content:           `Sure! Let me search: [{"id":"call_s","type":"function","function":{"name":"search","arguments":"{\"q\":\"Paris\"}"}}]`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantRemaining:     "Sure! Let me search:",
		},
		{
			name:              "not a tool call (missing required fields)",
			content:           `[{"name":"test","value":123}]`,
			wantFound:         false,
			wantToolCallCount: 0,
			wantRemaining:     `[{"name":"test","value":123}]`,
		},
		{
			name:              "tool call with chatml tokens",
			content:           `<|im_start|>assistant[{"id":"call_x","type":"function","function":{"name":"test","arguments":"{}"}}]`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantRemaining:     `<|im_start|>assistant`,
		},
		{
			name:              "Gemma style: tool call + end_of_turn",
			content:           `[{"id":"call_gemma","type":"function","function":{"name":"search","arguments":"{\"q\":\"test\"}"}}]</end_of_turn>`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantRemaining:     `</end_of_turn>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toolCalls, remaining, found := detectAndExtractToolCallsFromContent(tt.content)
			if found != tt.wantFound {
				t.Errorf("detectAndExtractToolCallsFromContent() found = %v, want %v", found, tt.wantFound)
				return
			}
			if found {
				if len(toolCalls) != tt.wantToolCallCount {
					t.Errorf("detectAndExtractToolCallsFromContent() tool call count = %d, want %d", len(toolCalls), tt.wantToolCallCount)
				}
				// Verify each tool call has proper structure
				for i, tc := range toolCalls {
					tcMap, ok := tc.(map[string]interface{})
					if !ok {
						t.Errorf("toolCall[%d] is not a map", i)
						continue
					}
					if tcMap["id"] == "" {
						t.Errorf("toolCall[%d] has empty id", i)
					}
					if tcMap["type"] != "function" {
						t.Errorf("toolCall[%d] type = %v, want 'function'", i, tcMap["type"])
					}
					if fn, ok := tcMap["function"].(map[string]interface{}); !ok || len(fn) == 0 {
						t.Errorf("toolCall[%d] has no valid function", i)
					}
				}
			}
			if remaining != tt.wantRemaining {
				t.Errorf("detectAndExtractToolCallsFromContent() remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

func TestExtractToolCallsFromSSEContent(t *testing.T) {
	tests := []struct {
		name      string
		sseData   string
		wantDelta bool // expect tool_calls in delta after processing
	}{
		{
			name: "SSE chunk with tool call in content",
			sseData: `{"choices":[{"delta":{"content":"[{\"id\":\"call_test\",\"type\":\"function\",\"function\":{\"name\":\"search\",\"arguments\":\"{}\"}}]","role":"assistant"},"finish_reason":null,"index":0}],"model":"gemma-4"}`,
			wantDelta: true,
		},
		{
			name: "SSE chunk without tool call",
			sseData: `{"choices":[{"delta":{"content":"Hello","role":"assistant"},"finish_reason":null,"index":0}],"model":"gemma-4"}`,
			wantDelta: false,
		},
		{
			name: "SSE chunk with tool call + service token",
			sseData: `{"choices":[{"delta":{"content":"[{\"id\":\"call_svc\",\"type\":\"function\",\"function\":{\"name\":\"search\",\"arguments\":\"{\\\"q\\\":\\\"weather\\\"}\"}}]\u003c/start_of_turn\u003e","role":"assistant"},"finish_reason":null,"index":0}],"model":"gemma-4"}`,
			wantDelta: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modified := extractToolCallsFromSSEContent([]byte(tt.sseData))
			if tt.wantDelta {
				if modified == nil {
					t.Fatal("extractToolCallsFromSSEContent() returned nil, expected modified chunk")
				}
				// Verify the modified chunk has tool_calls in delta
				var chunk map[string]interface{}
				if err := json.Unmarshal(modified, &chunk); err != nil {
					t.Fatalf("failed to unmarshal modified chunk: %v", err)
				}
				choices, ok := chunk["choices"].([]interface{})
				if !ok || len(choices) == 0 {
					t.Fatal("no choices in modified chunk")
				}
				choice := choices[0].(map[string]interface{})
				delta, ok := choice["delta"].(map[string]interface{})
				if !ok {
					t.Fatal("no delta in modified chunk")
				}
				if _, hasContent := delta["content"]; hasContent {
					t.Errorf("modified chunk still has content field, should be moved to tool_calls")
				}
				if _, hasToolCalls := delta["tool_calls"]; !hasToolCalls {
					t.Errorf("modified chunk missing tool_calls field")
				}
				t.Logf("modified chunk: %s", string(modified))
			} else {
				if modified != nil {
					t.Errorf("extractToolCallsFromSSEContent() returned non-nil for non-tool-call content: %s", string(modified))
				}
			}
		})
	}
}

func TestFindMatchingClosingBracket(t *testing.T) {
	tests := []struct {
		name    string
		s       string
		openPos int
		want    int
	}{
		{"simple array", "[1,2,3]", 0, 6},
		{"nested array", "[[1,2],[3,4]]", 0, 12},
		{"no open bracket", "abc", 0, -1},
		{"unclosed", "[1,2,3", 0, -1},
		{"with surrounding text", "a[1,2]b", 1, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findMatchingClosingBracket(tt.s, tt.openPos)
			if got != tt.want {
				t.Errorf("findMatchingClosingBracket() = %v, want %v", got, tt.want)
			}
		})
	}
}
