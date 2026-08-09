// proxy_request_openai_auto_stream_test.go — tests for Round 31 #1
// auto-stream workaround accumulator.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOpenAIAccumulator_BasicContent — простой случай: content + finish_reason.
func TestOpenAIAccumulator_BasicContent(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4")
	a.addChunk(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234,"model":"gemma-4","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`)
	a.addChunk(`{"choices":[{"delta":{"content":" world"}}]}`)
	a.addChunk(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)

	body := a.toNonStreamResponse(false)
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v, body=%s", err, body)
	}
	if resp["id"] != "chatcmpl-1" {
		t.Errorf("id: want chatcmpl-1, got %v", resp["id"])
	}
	choices, ok := resp["choices"].([]interface{})
	if !ok || len(choices) != 1 {
		t.Fatalf("choices: want 1, got %d", len(choices))
	}
	choice := choices[0].(map[string]interface{})
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason: want stop, got %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]interface{})
	if msg["role"] != "assistant" {
		t.Errorf("role: want assistant, got %v", msg["role"])
	}
	if msg["content"] != "Hello world" {
		t.Errorf("content: want 'Hello world', got %q", msg["content"])
	}
}

// TestOpenAIAccumulator_ReasoningContent — reasoning_content пробрасывается в message.reasoning.
func TestOpenAIAccumulator_ReasoningContent(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4")
	a.addChunk(`{"choices":[{"delta":{"reasoning_content":"Let me think..."}}]}`)
	a.addChunk(`{"choices":[{"delta":{"content":"42"}}]}`)
	a.addChunk(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)

	body := a.toNonStreamResponse(false)
	var resp map[string]interface{}
	_ = json.Unmarshal(body, &resp)
	msg := resp["choices"].([]interface{})[0].(map[string]interface{})["message"].(map[string]interface{})

	if msg["reasoning"] != "Let me think..." {
		t.Errorf("reasoning: want 'Let me think...', got %q", msg["reasoning"])
	}
	if msg["content"] != "42" {
		t.Errorf("content: want '42', got %q", msg["content"])
	}
	// reasoning + content в одном response — оба должны быть
	if _, hasReasoning := msg["reasoning"]; !hasReasoning {
		t.Error("expected message.reasoning field")
	}
}

// TestOpenAIAccumulator_ToolCalls — tool_calls накапливаются в массиве.
func TestOpenAIAccumulator_ToolCalls(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4")
	a.addChunk(`{"choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
	a.addChunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\""}}]}}]}`)
	a.addChunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"Moscow\"}"}}]}}]}`)
	a.addChunk(`{"choices":[{"finish_reason":"tool_calls"}]}`)

	body := a.toNonStreamResponse(false)
	var resp map[string]interface{}
	_ = json.Unmarshal(body, &resp)
	msg := resp["choices"].([]interface{})[0].(map[string]interface{})["message"].(map[string]interface{})

	tcs, ok := msg["tool_calls"].([]interface{})
	if !ok {
		t.Fatalf("expected tool_calls array, got %T: %v", msg["tool_calls"], msg["tool_calls"])
	}
	if len(tcs) != 2 {
		t.Errorf("expected 2 tool_calls chunks, got %d", len(tcs))
	}
}

// TestOpenAIAccumulator_UsageIncluded — usage из финального chunk пробрасывается.
func TestOpenAIAccumulator_UsageIncluded(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4")
	a.addChunk(`{"choices":[{"delta":{"content":"Hi"}}]}`)
	a.addChunk(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)

	body := a.toNonStreamResponse(false)
	var resp map[string]interface{}
	_ = json.Unmarshal(body, &resp)
	usage, ok := resp["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected usage object, got %T: %v", resp["usage"], resp["usage"])
	}
	if usage["prompt_tokens"].(float64) != 10 {
		t.Errorf("prompt_tokens: want 10, got %v", usage["prompt_tokens"])
	}
	if usage["completion_tokens"].(float64) != 2 {
		t.Errorf("completion_tokens: want 2, got %v", usage["completion_tokens"])
	}
}

// TestOpenAIAccumulator_EmptyContent — пустой content допустим.
func TestOpenAIAccumulator_EmptyContent(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4")
	a.addChunk(`{"choices":[{"delta":{"role":"assistant"},"finish_reason":"stop"}]}`)

	body := a.toNonStreamResponse(false)
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v, body=%s", err, body)
	}
	// content должен быть пустой строкой, не nil
	msg := resp["choices"].([]interface{})[0].(map[string]interface{})["message"].(map[string]interface{})
	content, ok := msg["content"].(string)
	if !ok {
		t.Errorf("expected content to be string (even empty), got %T", msg["content"])
	}
	if content != "" {
		t.Errorf("expected empty content, got %q", content)
	}
}

// TestOpenAIAccumulator_FullGemmaScenario — реальный сценарий gemma-4 reasoning.
func TestOpenAIAccumulator_FullGemmaScenario(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4")
	chunks := []string{
		`{"id":"cmpl-1","object":"chat.completion.chunk","created":1234567890,"model":"gemma-4","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"reasoning_content":"The user asks 2+2."},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"reasoning_content":" I need to calculate."}}]}`,
		`{"choices":[{"delta":{"content":"The answer is "}}]}`,
		`{"choices":[{"delta":{"content":"4."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":3,"total_tokens":18}}`,
	}
	for _, c := range chunks {
		a.addChunk(c)
	}
	if a.chunksProcessed != len(chunks) {
		t.Errorf("chunksProcessed: want %d, got %d", len(chunks), a.chunksProcessed)
	}
	body := a.toNonStreamResponse(false)
	if !strings.Contains(string(body), `"reasoning":"The user asks 2+2. I need to calculate."`) {
		t.Errorf("expected reasoning field, body=%s", body)
	}
	if !strings.Contains(string(body), `"content":"The answer is 4."`) {
		t.Errorf("expected content field, body=%s", body)
	}
	if !strings.Contains(string(body), `"finish_reason":"stop"`) {
		t.Errorf("expected finish_reason=stop, body=%s", body)
	}
	if !strings.Contains(string(body), `"prompt_tokens":15`) {
		t.Errorf("expected usage.prompt_tokens=15, body=%s", body)
	}
}

// TestIsOpenAIAutoStreamEnabled_DefaultFalse — по умолчанию выключено.
func TestIsOpenAIAutoStreamEnabled_DefaultFalse(t *testing.T) {
	// Не устанавливаем env var, должно быть false
	t.Setenv("LB_OPENAI_AUTO_STREAM", "")
	if IsOpenAIAutoStreamEnabled() {
		t.Error("default should be false when env var is empty")
	}
	t.Setenv("LB_OPENAI_AUTO_STREAM", "false")
	if IsOpenAIAutoStreamEnabled() {
		t.Error("should be false when env=explicit_false")
	}
}

// TestIsOpenAIAutoStreamEnabled_TrueValues — все truthy варианты работают.
func TestIsOpenAIAutoStreamEnabled_TrueValues(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "True", "1", "yes", "YES"} {
		t.Setenv("LB_OPENAI_AUTO_STREAM", v)
		if !IsOpenAIAutoStreamEnabled() {
			t.Errorf("env=%q should enable auto-stream", v)
		}
	}
}
