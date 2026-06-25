// llamacpp_toolcall_gemma_test.go — Тесты для Gemma-4 и Hermes-prompt парсеров.
// Покрывает сценарии, добавленные 2026-06-25: опциональный </tool_call>, Gemma turn-parsing,
// валидацию arguments как JSON-строки.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGemmaTurnToolCallsInContent — проверяет детекцию JSON tool call внутри
// <start_of_turn>(model|assistant)\n[JSON]\n</start_of_turn> — специфика Gemma-4.
func TestGemmaTurnToolCallsInContent(t *testing.T) {
	tests := []struct {
		name              string
		content           string
		wantFound         bool
		wantToolCallCount int
		wantToolName      string
		wantRemaining     string
	}{
		{
			name:              "Gemma-4 model turn with array",
			content:           `<start_of_turn>model` + "\n" + `[{"name": "search", "arguments": {"q": "test"}}]` + "\n" + `</start_of_turn>`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "search",
			wantRemaining:     "",
		},
		{
			name:              "Gemma-4 assistant turn with array",
			content:           `<start_of_turn>assistant` + "\n" + `[{"name": "list_directory_with_sizes", "arguments": {"path": "c:\\Ollama\\bongo"}}]` + "\n" + `</start_of_turn>`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "list_directory_with_sizes",
			wantRemaining:     "",
		},
		{
			name:              "Gemma-4 model turn with single object",
			content:           `<start_of_turn>model` + "\n" + `{"name": "search", "arguments": {"q": "x"}}` + "\n" + `</start_of_turn>`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "search",
			wantRemaining:     "",
		},
		{
			name:              "Gemma-4 turn with text before",
			content:           `Let me search.` + "\n" + `<start_of_turn>model` + "\n" + `[{"name": "search", "arguments": {"q": "AI"}}]` + "\n" + `</start_of_turn>`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "search",
			wantRemaining:     "Let me search.",
		},
		{
			name:              "Gemma-4 with parameters instead of arguments",
			content:           `<start_of_turn>model` + "\n" + `[{"name": "calc", "parameters": {"x": 1}}]` + "\n" + `</start_of_turn>`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "calc",
			wantRemaining:     "",
		},
		{
			name:              "No Gemma turn — plain JSON array",
			content:           `[{"id":"call_x","type":"function","function":{"name":"search","arguments":"{}"}}]`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "search",
			wantRemaining:     "",
		},
		{
			name:              "Empty Gemma turn",
			content:           "",
			wantFound:         false,
			wantToolCallCount: 0,
		},
		{
			name:              "Plain text without Gemma markers",
			content:           `Hello world, no tools here.`,
			wantFound:         false,
			wantToolCallCount: 0,
			wantRemaining:     `Hello world, no tools here.`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toolCalls, remaining, found := detectAndExtractToolCallsFromContent(tt.content)
			if found != tt.wantFound {
				t.Errorf("found = %v, want %v", found, tt.wantFound)
				return
			}
			if found {
				if len(toolCalls) != tt.wantToolCallCount {
					t.Errorf("tool call count = %d, want %d", len(toolCalls), tt.wantToolCallCount)
				}
				if tt.wantToolName != "" && len(toolCalls) > 0 {
					tcMap, ok := toolCalls[0].(map[string]interface{})
					if !ok {
						t.Errorf("toolCall[0] is not a map")
						return
					}
					fn, ok := tcMap["function"].(map[string]interface{})
					if !ok {
						t.Errorf("toolCall[0].function is not a map")
						return
					}
					if fn["name"] != tt.wantToolName {
						t.Errorf("toolCall[0].function.name = %v, want %v", fn["name"], tt.wantToolName)
					}
				}
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

// TestHermesOptionalCloseTag — проверяет, что </tool_call> теперь опциональный.
func TestHermesOptionalCloseTag(t *testing.T) {
	tests := []struct {
		name              string
		content           string
		wantFound         bool
		wantToolCallCount int
		wantToolName      string
	}{
		{
			name:              "Hermes with </tool_call>",
			content:           `<tool_call>{"name": "search", "arguments": {"q": "x"}}</tool_call>`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "search",
		},
		{
			name:              "Hermes WITHOUT </tool_call> (qwen2.5-coder, deepseek-coder)",
			content:           `<tool_call>{"name": "search", "arguments": {"q": "x"}}`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "search",
		},
		{
			name:              "Hermes with answer tag instead",
			content:           `<answer>{"name": "calc", "arguments": {"x": 1}}</answer>`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "calc",
		},
		{
			name:              "Hermes answer tag without closing",
			content:           `<answer>{"name": "calc", "arguments": {"x": 1}}`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "calc",
		},
		{
			name:              "Hermes with double brace artifact",
			content:           `<tool_call>{"name": "search", "arguments": {"q": "AI news"}}}</tool_call>`,
			wantFound:         true,
			wantToolCallCount: 1,
			wantToolName:      "search",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toolCalls, _, found := detectAndExtractToolCallsFromContent(tt.content)
			if found != tt.wantFound {
				t.Errorf("found = %v, want %v", found, tt.wantFound)
				return
			}
			if found && len(toolCalls) > 0 {
				tcMap := toolCalls[0].(map[string]interface{})
				fn := tcMap["function"].(map[string]interface{})
				if fn["name"] != tt.wantToolName {
					t.Errorf("name = %v, want %v", fn["name"], tt.wantToolName)
				}
			}
		})
	}
}

// TestArgumentsAsJSONString — проверяет, что если модель прислала arguments как строку
// с валидным JSON, мы её не сериализуем повторно.
func TestArgumentsAsJSONString(t *testing.T) {
	// Модель прислала arguments как stringified JSON (типичная Gemma-4 проблема)
	raw := json.RawMessage(`{"name": "search", "arguments": "{\"q\": \"test\", \"limit\": 10}"}`)
	tc := parseHermesToOpenAI(raw)
	if tc == nil {
		t.Fatal("parseHermesToOpenAI returned nil")
	}
	fn := tc["function"].(map[string]interface{})
	args, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("arguments is not a string: %T", fn["arguments"])
	}
	// Аргументы должны быть валидным JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments is not valid JSON: %v (raw=%s)", err, args)
	}
	if parsed["q"] != "test" {
		t.Errorf("q = %v, want 'test'", parsed["q"])
	}
	if parsed["limit"] != float64(10) {
		t.Errorf("limit = %v, want 10", parsed["limit"])
	}
	// НЕ должно быть двойной сериализации (escaped quotes)
	if strings.Contains(args, `\"`) {
		t.Errorf("arguments has escaped quotes (double serialization): %s", args)
	}
}

// TestArgumentsAsMap — проверяет обратный случай: arguments пришёл как объект (map).
func TestArgumentsAsMap(t *testing.T) {
	raw := json.RawMessage(`{"name": "search", "arguments": {"q": "hello"}}`)
	tc := parseHermesToOpenAI(raw)
	if tc == nil {
		t.Fatal("parseHermesToOpenAI returned nil")
	}
	fn := tc["function"].(map[string]interface{})
	args, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("arguments is not a string: %T", fn["arguments"])
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments is not valid JSON: %v", err)
	}
	if parsed["q"] != "hello" {
		t.Errorf("q = %v, want 'hello'", parsed["q"])
	}
}