// llamacpp_handlers_inference_r53_5_test.go — Round 53.5 (2026-08-24):
// regression test для mid-stream error chunk emission.
//
// Background: pre-R53.5, when proxyRequestLlamaCpp() failed mid-stream
// (cppworker timeout, hang, network blip), the balancer:
//   1. Logged the error
//   2. Returned from the handler without emitting any final chunk
//   3. Connection closed mid-stream
//   4. Client (Cline / OpenWebUI / Flowise) never saw done:true
//   5. Client reported "Did not receive done or success response in stream"
//
// R53.5 fix: emit a final done-чанк with done_reason="error" and error message
// so the client can properly terminate the stream and report the error.

package balancer

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteStreamErrorChunk_OllamaChat — R53.5: /api/chat error chunk
// should be valid Ollama NDJSON with done=true, done_reason="error", error field.
func TestWriteStreamErrorChunk_OllamaChat(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStreamErrorChunk(rec, "/api/chat", "Qwen3-Instruct-2507-q4km", "upstream timeout")

	body := strings.TrimSpace(rec.Body.String())
	var c map[string]interface{}
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("invalid NDJSON: %v, body=%q", err, body)
	}
	if done, _ := c["done"].(bool); !done {
		t.Errorf("expected done=true, got %v", c["done"])
	}
	if reason, _ := c["done_reason"].(string); reason != "error" {
		t.Errorf("expected done_reason='error', got %v", c["done_reason"])
	}
	if errMsg, _ := c["error"].(string); errMsg != "upstream timeout" {
		t.Errorf("expected error='upstream timeout', got %v", c["error"])
	}
	if model, _ := c["model"].(string); model != "Qwen3-Instruct-2507-q4km" {
		t.Errorf("expected model preserved, got %v", c["model"])
	}
	if msg, ok := c["message"].(map[string]interface{}); !ok {
		t.Errorf("expected message object, got %T", c["message"])
	} else {
		if msg["role"] != "assistant" {
			t.Errorf("expected message.role=assistant, got %v", msg["role"])
		}
	}
}

// TestWriteStreamErrorChunk_OllamaGenerate — R53.5: /api/generate error chunk
// should use 'response' field (not 'message') per Ollama schema.
func TestWriteStreamErrorChunk_OllamaGenerate(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStreamErrorChunk(rec, "/api/generate", "Qwen3-Instruct-2507-q4km", "context canceled")

	body := strings.TrimSpace(rec.Body.String())
	var c map[string]interface{}
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("invalid JSON: %v, body=%q", err, body)
	}
	if done, _ := c["done"].(bool); !done {
		t.Errorf("expected done=true, got %v", c["done"])
	}
	if reason, _ := c["done_reason"].(string); reason != "error" {
		t.Errorf("expected done_reason='error', got %v", c["done_reason"])
	}
	if _, hasMsg := c["message"]; hasMsg {
		t.Errorf("/api/generate should use 'response' field, not 'message'")
	}
	if resp, _ := c["response"].(string); resp != "" {
		t.Errorf("expected empty response, got %q", resp)
	}
}

// TestWriteStreamErrorChunk_OpenAIChat — R53.5: /v1/chat/completions error
// chunk should be valid OpenAI SSE format with finish_reason="error" + [DONE].
func TestWriteStreamErrorChunk_OpenAIChat(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStreamErrorChunk(rec, "/v1/chat/completions", "Qwen3-Instruct-2507-q4km", "upstream hang")

	body := rec.Body.String()
	if !strings.Contains(body, "data: ") {
		t.Fatalf("OpenAI SSE should start with 'data: ', got: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"error"`) {
		t.Errorf("expected finish_reason=error in SSE chunk, got: %s", body)
	}
	if !strings.Contains(body, `"type":"stream_proxy_error"`) {
		t.Errorf("expected error.type=stream_proxy_error, got: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("OpenAI SSE should end with 'data: [DONE]', got: %s", body)
	}
}

// TestWriteStreamErrorChunk_OpenAICompletion — R53.5: /v1/completions
// (legacy text completion) error chunk.
func TestWriteStreamErrorChunk_OpenAICompletion(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStreamErrorChunk(rec, "/v1/completions", "Qwen3-Instruct-2507-q4km", "stream broken")

	body := rec.Body.String()
	if !strings.Contains(body, `"object":"text_completion"`) {
		t.Errorf("expected text_completion object, got: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"error"`) {
		t.Errorf("expected finish_reason=error, got: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("expected [DONE] terminator, got: %s", body)
	}
}

// TestWriteStreamErrorChunk_UnknownPath — R53.5: unknown path defaults to
// /api/chat schema (most common case). Should not panic.
func TestWriteStreamErrorChunk_UnknownPath(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStreamErrorChunk(rec, "/api/unknown", "test-model", "fallback test")

	body := strings.TrimSpace(rec.Body.String())
	var c map[string]interface{}
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if done, _ := c["done"].(bool); !done {
		t.Errorf("expected done=true even for unknown path, got %v", c["done"])
	}
}
