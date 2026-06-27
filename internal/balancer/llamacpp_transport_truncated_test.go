package balancer

// Regression test for stream-truncation detection (2026-06-26).
//
// See internal/balancer/llamacpp_transport.go around lines 514-561
// (the if !streamCompleted block).
//
// Scenario: cppworker broke off the SSE stream (broken pipe, OOM kill, client
// context cancel) BEFORE sending the final [DONE] marker. The balancer proxied
// only what arrived before the break and closed the connection WITHOUT the final
// marker. The client (Cline/OpenWebUI/Roo Code) considered the stream successfully
// finished (TCP FIN == normal end), while actually receiving a truncated response.
//
// After the fix (2026-06-26), proxyRequestLlamaCpp explicitly tracks whether
// the final [DONE] marker (for SSE passthrough) or done:true (for NDJSON) was
// received. If not, it logs SSE stream truncated before [DONE] marker and
// emits a final chunk to the client with error/done:true and finish_reason=truncated.
//
// This test verifies both code paths:
//   - /api/chat (NDJSON): chunk with done:true, done_reason:truncated,
//     error:stream truncated by upstream before completion
//   - /v1/chat/completions (SSE): data chunk with choices:[{finish_reason:truncated}],
//     error:{...} + data: [DONE]
//
// Compiles WITHOUT the llama_stub build tag - uses only stdlib + already-imported
// packages. The test does NOT import ollama-loadbalancer/c/bridge, so it runs
// under both build tags.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// makeTruncatingUpstream is a mock cppworker that responds with SSE chunks on
// /v1/chat/completions, then SIMULATES A BROKEN CONNECTION via http.Hijacker
// by closing the TCP socket without sending the final [DONE]. This is a precise
// reproduction of the symptom from env_log.txt lines 88-90.
//
// Implementation: write numChunks SSE chunks via w.Write + Flusher.Flush, then
// hijack the connection and close it. The Go HTTP transport on the client side
// will see EOF / unexpected EOF when reading the response body, and
// bufio.Scanner.Scan() in proxyRequestLlamaCpp will return false with a
// non-nil scanner.Err().
func makeTruncatingUpstream(t *testing.T, numChunks int) *httptest.Server {
	t.Helper()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}

		// 1. Reply with valid SSE headers.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support Flusher")
		}

		// 2. Send numChunks valid data: chunks with useful content.
		//    The content intentionally does NOT contain model service tokens
		//    (Gemma <end_of_turn>, Llama3 etc.) that filterOpenAIStreamingLine
		//    might filter out.
		for i := 0; i < numChunks; i++ {
			chunk := map[string]interface{}{
				"id":      "chatcmpl-trunc",
				"object":  "chat.completion.chunk",
				"created": 1700000000,
				"model":   "gemma-4-E4B-it-Q4_K_M",
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"delta": map[string]interface{}{
							"content": "partial-content-chunk-" + string(rune('0'+i)),
						},
						"finish_reason": nil,
					},
				},
			}
			payload, err := json.Marshal(chunk)
			if err != nil {
				t.Fatalf("marshal chunk %d: %v", i, err)
			}
			if _, werr := w.Write([]byte("data: ")); werr != nil {
				return
			}
			if _, werr := w.Write(payload); werr != nil {
				return
			}
			// SSE frame ends with two newlines (RFC: empty line).
			if _, werr := w.Write([]byte("\n\n")); werr != nil {
				return
			}
			flusher.Flush()
		}

		// 3. Simulate broken pipe: hijack and close TCP socket WITHOUT final [DONE].
		hj, hijackOK := w.(http.Hijacker)
		if !hijackOK {
			t.Fatal("ResponseWriter does not support Hijacker")
		}
		conn, _, _ := hj.Hijack()
		if conn != nil {
			_ = conn.Close()
		}
	})

	return httptest.NewServer(handler)
}

// parseSSEResponse parses an SSE response into a list of data: chunks (without prefix).
// Used to verify the order and contents of SSE chunks in the balancer response
// for /v1/chat/completions.
func parseSSEResponse(t *testing.T, body io.Reader) []string {
	t.Helper()
	var chunks []string
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		chunks = append(chunks, data)
	}
	return chunks
}

// parseNDJSONResponse parses an NDJSON response (Ollama format) into a list of objects.
// Each line is a separate JSON chunk. Used to verify the balancer response for
// /api/chat and /api/generate.
func parseNDJSONResponse(t *testing.T, body io.Reader) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("parseNDJSONResponse: invalid JSON line %q: %v", line, err)
		}
		out = append(out, obj)
	}
	return out
}

// TestProxyRequestLlamaCpp_StreamTruncation_NDJSON is the main regression test
// for the stream-truncation detection fix (2026-06-26).
//
// Scenario: client (OpenWebUI/Cline) sends a streaming /api/chat to the balancer.
// The balancer proxies the request to cppworker. cppworker responds with 3 valid
// SSE chunks and then BREAKS the connection (broken pipe) without the final [DONE].
//
// Expected behavior after the fix:
//   1. Client HTTP status: 200 (headers already sent before proxying).
//   2. Client body contains NDJSON chunks with partial content (translated
//      from SSE upstream) AND a final NDJSON chunk with done:true,
//      done_reason:truncated, error:stream truncated by upstream before completion.
//   3. proxyRequestLlamaCpp returns nil (no error) since truncation is handled.
func TestProxyRequestLlamaCpp_StreamTruncation_NDJSON(t *testing.T) {
	upstream := makeTruncatingUpstream(t, 3)
	defer upstream.Close()

	// Set up Proxy via existing helper (see proxy_streaming_413_test.go).
	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	bodyObj := map[string]interface{}{
		"model": "gemma-4-E4B-it-Q4_K_M",
		"messages": []map[string]string{
			{"role": "user", "content": "Hello, describe yourself briefly"},
		},
		"stream": true,
	}
	bodyBytes, err := json.Marshal(bodyObj)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	if err := p.proxyRequestLlamaCpp(rec, req, "llama_test", bodyBytes); err != nil {
		t.Fatalf("proxyRequestLlamaCpp returned error (expected nil after truncation fix): %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("HTTP status = %d, want 200 (headers already sent before truncation)", rec.Code)
	}

	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/x-ndjson") {
		t.Errorf("Content-Type = %q, want it to contain application/x-ndjson", got)
	}

	recorderBody := rec.Body.Bytes()
	chunks := parseNDJSONResponse(t, bytes.NewReader(recorderBody))
	if len(chunks) == 0 {
		t.Fatalf("recorder body is empty or unparseable; body=%q", string(recorderBody))
	}

	var truncChunk map[string]interface{}
	var contentChunks int
	for _, c := range chunks {
		if done, _ := c["done"].(bool); done {
			if reason, _ := c["done_reason"].(string); reason == "truncated" {
				truncChunk = c
				break
			}
		} else {
			contentChunks++
		}
	}

	if truncChunk == nil {
		t.Fatalf("expected final NDJSON chunk with done_reason=truncated; got chunks=%d; body=%s", len(chunks), string(recorderBody))
	}

	if gotErr, _ := truncChunk["error"].(string); gotErr != "stream truncated by upstream before completion" {
		t.Errorf("truncation chunk error=%q, want %q", gotErr, "stream truncated by upstream before completion")
	}
	if got, _ := truncChunk["done"].(bool); !got {
		t.Errorf("truncation chunk done=%v, want true", got)
	}
	if got, _ := truncChunk["done_reason"].(string); got != "truncated" {
		t.Errorf("truncation chunk done_reason=%q, want %q", got, "truncated")
	}

	if contentChunks == 0 && len(chunks) <= 1 {
		t.Errorf("expected at least 1 forwarded content chunk + 1 truncation chunk; got %d chunks total", len(chunks))
	}

	t.Logf("OK: NDJSON truncation handled correctly; chunks=%d content_chunks=%d", len(chunks), contentChunks)
}

// TestProxyRequestLlamaCpp_StreamTruncation_SSEPassthrough is the regression test
// for the stream-truncation detection fix in the SSE-to-SSE passthrough mode
// (used by Roo Code / Cline / Continue.dev, which send requests to
// /v1/chat/completions and expect OpenAI-format responses).
//
// Scenario: cppworker responds with 3 SSE chunks, then breaks the connection
// without the final [DONE]. The client (Cline) sees TCP FIN and considers the
// stream finished - WITHOUT the final chunk it thinks the response was successful.
//
// Expected behavior after the fix:
//   1. Client HTTP status: 200.
//   2. Content-Type: text/event-stream.
//   3. Client body contains:
//      - 3 forwarded SSE chunks from upstream.
//      - Final data: chunk with choices:[{finish_reason:truncated}],
//        error:{message:stream truncated by upstream before completion,
//        type:stream_truncated}.
//      - Final data: [DONE] marker.
func TestProxyRequestLlamaCpp_StreamTruncation_SSEPassthrough(t *testing.T) {
	upstream := makeTruncatingUpstream(t, 3)
	defer upstream.Close()

	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	// OpenAI-style streaming request (as sent by Cline / Roo Code).
	bodyObj := map[string]interface{}{
		"model": "gemma-4-E4B-it-Q4_K_M",
		"messages": []map[string]string{
			{"role": "user", "content": "Hello"},
		},
		"stream": true,
	}
	bodyBytes, err := json.Marshal(bodyObj)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	if err := p.proxyRequestLlamaCpp(rec, req, "llama_test", bodyBytes); err != nil {
		t.Fatalf("proxyRequestLlamaCpp returned error (expected nil after truncation fix): %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("HTTP status = %d, want 200 (headers already sent before truncation)", rec.Code)
	}

	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want it to contain text/event-stream", got)
	}

	recorderBody := rec.Body.Bytes()
	sseChunks := parseSSEResponse(t, bytes.NewReader(recorderBody))
	if len(sseChunks) == 0 {
		t.Fatalf("recorder body is empty or has no SSE chunks; body=%q", string(recorderBody))
	}

	var hasDone bool
	var truncChunk map[string]interface{}
	for _, raw := range sseChunks {
		if raw == "[DONE]" {
			hasDone = true
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			t.Errorf("invalid SSE JSON chunk: %q (err=%v)", raw, err)
			continue
		}
		if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if reason, _ := choice["finish_reason"].(string); reason == "truncated" {
					truncChunk = chunk
				}
			}
		}
	}

	if !hasDone {
		t.Errorf("expected [DONE] SSE marker at end of stream; chunks=%v", sseChunks)
	}

	if truncChunk == nil {
		t.Fatalf("expected SSE truncation chunk with finish_reason=truncated; body=%s", string(recorderBody))
	}

	if errObj, ok := truncChunk["error"].(map[string]interface{}); !ok {
		t.Errorf("truncation chunk missing error object; got=%v", truncChunk)
	} else {
		if msg, _ := errObj["message"].(string); msg != "stream truncated by upstream before completion" {
			t.Errorf("truncation chunk error.message=%q, want %q", msg, "stream truncated by upstream before completion")
		}
		if typ, _ := errObj["type"].(string); typ != "stream_truncated" {
			t.Errorf("truncation chunk error.type=%q, want %q", typ, "stream_truncated")
		}
	}

	t.Logf("OK: SSE passthrough truncation handled correctly; chunks=%d has_done_marker=%v", len(sseChunks), hasDone)
}

// TestProxyRequestLlamaCpp_StreamTruncation_NoTruncationWhenDoneSent is a
// negative test: verifies that the fix does NOT break the happy path. If upstream
// sends ALL expected chunks + [DONE], the final truncation chunk must NOT be emitted.
//
// This is important for regression: before the fix, the happy path was working
// (writeStreamingSSEDone triggered on [DONE]), and the fix must not break it.
func TestProxyRequestLlamaCpp_StreamTruncation_NoTruncationWhenDoneSent(t *testing.T) {
	// Mock upstream with a complete SSE stream (3 chunks + [DONE]).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"id":"chatcmpl-ok","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello "}}]}}` + "\n\n",
			`data: {"id":"chatcmpl-ok","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"world"}}]}}` + "\n\n",
			`data: {"id":"chatcmpl-ok","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}}` + "\n\n",
			`data: [DONE]` + "\n\n",
		}
		for _, c := range chunks {
			if _, err := w.Write([]byte(c)); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	bodyObj := map[string]interface{}{
		"model":    "gemma-4-E4B-it-Q4_K_M",
		"messages": []map[string]string{{"role": "user", "content": "Hi"}},
		"stream":   true,
	}
	bodyBytes, _ := json.Marshal(bodyObj)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	if err := p.proxyRequestLlamaCpp(rec, req, "llama_test", bodyBytes); err != nil {
		t.Fatalf("proxyRequestLlamaCpp returned error: %v", err)
	}

	recorderBody := rec.Body.Bytes()
	chunks := parseNDJSONResponse(t, bytes.NewReader(recorderBody))

	var sawTruncation bool
	for _, c := range chunks {
		if reason, _ := c["done_reason"].(string); reason == "truncated" {
			sawTruncation = true
			break
		}
	}

	if sawTruncation {
		t.Errorf("REGRESSION: truncation chunk emitted even though upstream sent [DONE]; chunks=%v", chunks)
	}

	var finalChunk map[string]interface{}
	for _, c := range chunks {
		if done, _ := c["done"].(bool); done {
			finalChunk = c
			break
		}
	}
	if finalChunk == nil {
		t.Fatalf("expected final done-chunk in NDJSON response; body=%s", string(recorderBody))
	}
	// Happy-path допускает done_reason="" если upstream не прислал finish_reason
	// (тестовый мок не имитирует финальный чанк модели). Главное — НЕ должно
	// быть "truncated", потому что [DONE] был отправлен.
	if reason, _ := finalChunk["done_reason"].(string); reason == "truncated" {
		t.Errorf("REGRESSION: happy-path эмитит truncated хотя upstream прислал [DONE]; chunks=%v", chunks)
	}

	t.Logf("OK: happy-path NOT affected by truncation fix; final_done_reason=%v", finalChunk["done_reason"])
}
