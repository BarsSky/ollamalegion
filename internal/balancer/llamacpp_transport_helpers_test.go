package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteStreamingSSEDone_SSE_Passthrough — /v1/chat/completions должен
// всегда писать "data: [DONE]\n\n" + flush, независимо от tool_calls.
func TestWriteStreamingSSEDone_SSE_Passthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flushingResponseWriter{ResponseWriter: rec}

	// tool_calls нет
	err := writeStreamingSSEDone(w, "/v1/chat/completions", "gemma-4", nil, "Hello", "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("expected [DONE] marker, got: %q", body)
	}

	// tool_calls есть — всё равно [DONE]
	rec2 := httptest.NewRecorder()
	w2 := &flushingResponseWriter{ResponseWriter: rec2}
	toolAccum := map[int]*accumulatedToolCall{
		0: {id: "call_1", function: map[string]interface{}{"name": "search", "arguments": "{}"}},
	}
	err = writeStreamingSSEDone(w2, "/v1/chat/completions", "gemma-4", toolAccum, "Hello", "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(rec2.Body.String(), "data: [DONE]") {
		t.Errorf("expected [DONE] marker (with tool_calls), got: %q", rec2.Body.String())
	}
}

// TestWriteStreamingSSEDone_Chat_NoToolCalls_NoDuplicate — Bug #12 фикс:
// для /api/chat без tool_calls финальный NDJSON-чанк НЕ пишется
// (translate уже отправил финальный чанк с done:true).
//
// До фикса: writeStreamingSSEDone писал финальный чанк с message.content="Hello",
// клиент видел дубликат ответа.
func TestWriteStreamingSSEDone_Chat_NoToolCalls_NoDuplicate(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flushingResponseWriter{ResponseWriter: rec}

	err := writeStreamingSSEDone(w, "/api/chat", "gemma-4", nil, "Hello from llama.cpp!", "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := rec.Body.String()
	if body != "" {
		t.Errorf("expected empty body (no duplicate final chunk), got: %q", body)
	}
	// Flush должен быть вызван (для NDJSON)
	if !w.flushed {
		t.Error("expected Flush to be called")
	}
}

// TestWriteStreamingSSEDone_Chat_WithToolCalls — для /api/chat с tool_calls
// пишется финальный NDJSON-чанк с message.tool_calls и пустым content
// (tool_calls заменяют content по семантике OpenAI).
func TestWriteStreamingSSEDone_Chat_WithToolCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flushingResponseWriter{ResponseWriter: rec}

	toolAccum := map[int]*accumulatedToolCall{
		0: {id: "call_1", function: map[string]interface{}{"name": "search", "arguments": "{\"q\":\"test\"}"}},
	}
	err := writeStreamingSSEDone(w, "/api/chat", "gemma-4", toolAccum, "Hello", "World", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &chunk); err != nil {
		t.Fatalf("invalid JSON: %v (body=%q)", err, rec.Body.String())
	}
	if chunk["done"] != true {
		t.Errorf("expected done=true, got %v", chunk["done"])
	}
	if chunk["done_reason"] != "tool_calls" {
		t.Errorf("expected done_reason=tool_calls, got %v", chunk["done_reason"])
	}
	msg, ok := chunk["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected message object, got %T", chunk["message"])
	}
	if msg["content"] != "" {
		t.Errorf("expected empty content (tool_calls replace content), got: %v", msg["content"])
	}
	tcs, ok := msg["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %v", msg["tool_calls"])
	}
}

// TestWriteStreamingSSEDone_Generate_NoToolCalls_NoDuplicate — для /api/generate
// без tool_calls финальный NDJSON-чанк НЕ пишется.
func TestWriteStreamingSSEDone_Generate_NoToolCalls_NoDuplicate(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flushingResponseWriter{ResponseWriter: rec}

	err := writeStreamingSSEDone(w, "/api/generate", "gemma-4", nil, "Hello from llama.cpp!", "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := rec.Body.String()
	if body != "" {
		t.Errorf("expected empty body (no duplicate final chunk), got: %q", body)
	}
}

// TestWriteStreamingSSEDone_Generate_WithToolCalls — для /api/generate
// с tool_calls пишется финальный NDJSON с done_reason=tool_calls.
func TestWriteStreamingSSEDone_Generate_WithToolCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flushingResponseWriter{ResponseWriter: rec}

	toolAccum := map[int]*accumulatedToolCall{
		0: {id: "call_1", function: map[string]interface{}{"name": "search", "arguments": "{}"}},
	}
	err := writeStreamingSSEDone(w, "/api/generate", "gemma-4", toolAccum, "Hello", "World", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &chunk); err != nil {
		t.Fatalf("invalid JSON: %v (body=%q)", err, rec.Body.String())
	}
	if chunk["done"] != true {
		t.Errorf("expected done=true, got %v", chunk["done"])
	}
	if chunk["done_reason"] != "tool_calls" {
		t.Errorf("expected done_reason=tool_calls, got %v", chunk["done_reason"])
	}
	if chunk["response"] != "" {
		t.Errorf("expected empty response, got: %v", chunk["response"])
	}
}

// TestWriteStreamingSSEDone_Chat_UpstreamContentIgnored — даже если upstream
// прислал полный content, writeStreamingSSEDone его НЕ использует (без tool_calls).
// Это предотвращает дубль с translate-чанком.
func TestWriteStreamingSSEDone_Chat_UpstreamContentIgnored(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flushingResponseWriter{ResponseWriter: rec}

	// upstreamContent = "Hello from upstream" — НЕ должно попасть в output
	err := writeStreamingSSEDone(w, "/api/chat", "gemma-4", nil,
		"accumulated", "Hello from upstream", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := rec.Body.String()
	if body != "" {
		t.Errorf("expected empty body, got: %q", body)
	}
	if strings.Contains(body, "Hello from upstream") {
		t.Errorf("upstream content must not appear (no duplicate): %q", body)
	}
}

// flushingResponseWriter — обёртка для тестов, чтобы отслеживать flush.
type flushingResponseWriter struct {
	http.ResponseWriter
	flushed bool
}

func (f *flushingResponseWriter) Flush() {
	f.flushed = true
	if fl, ok := f.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}