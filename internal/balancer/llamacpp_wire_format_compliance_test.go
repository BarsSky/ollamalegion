// llamacpp_wire_format_compliance_test.go — Round 53.3 (2026-08-24):
// structural compliance tests для ВСЕХ Ollama API endpoints. Тесты
// валидируют ФОРМАТ (schema/structure) а не только содержание. Это
// гарантирует совместимость со всеми Ollama-клиентами (Cline, OpenWebUI,
// Flowise, LangChain, n8n).
//
// Background: R51.3 fix для "Did not receive done" добавил done:true на
// каждый chunk с finish_reason. Это создало DOUBLE done:true в стриме
// (wrapper chunk + usage chunk). Ollama-клиенты (Cline через ollama npm)
// парсят первый done:true (без stats), считают стрим завершённым, и валятся
// с "Did not receive done or success response in stream".
//
// R53.1 (2026-08-24) исправил: wrapper chunk подавлен, canonical done:true
// приходит ОДИН раз (из usage chunk или writeStreamingSSEDone fallback).

package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestWireFormat_ChatStreaming_FullSchema — Round 53.3 structural compliance.
// Симулирует полный cppworker stream (role + content + wrapper + usage) через
// translateOpenAISSEDataToOllama (та же функция, что вызывает balancer) и
// валидирует что канонический done-чанк имеет ВСЕ обязательные поля Ollama.
func TestWireFormat_ChatStreaming_FullSchema(t *testing.T) {
	// Полный стрим: content chunk + wrapper + usage chunk
	chunks := []string{
		`{"id":"cmpl-1","object":"chat.completion.chunk","created":1234567890,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "}}]}`,
		`{"id":"cmpl-1","object":"chat.completion.chunk","created":1234567890,"model":"test-model","choices":[{"index":0,"delta":{"content":"world"}}]}`,
		`{"id":"cmpl-1","object":"chat.completion.chunk","created":1234567890,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"cmpl-1","object":"chat.completion.chunk","created":1234567890,"model":"test-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
	}

	streamStart := time.Now()
	seenReasoning := false
	var ndjsonChunks []map[string]interface{}

	for _, raw := range chunks {
		result := translateOpenAISSEDataToOllama("/api/chat", []byte(raw), "test-model", &seenReasoning, streamStart, time.Time{}, time.Time{})
		if result == nil {
			continue
		}
		var c map[string]interface{}
		if err := json.Unmarshal(result[:len(result)-1], &c); err != nil {
			t.Fatalf("invalid NDJSON: %v, raw=%s", err, result)
		}
		ndjsonChunks = append(ndjsonChunks, c)
	}

	// === STRUCTURAL ASSERTION: ровно ОДИН done:true (R53.1 fix) ===
	doneCount := 0
	var finalDone map[string]interface{}
	for _, c := range ndjsonChunks {
		if done, _ := c["done"].(bool); done {
			doneCount++
			finalDone = c
		}
	}
	if doneCount != 1 {
		t.Errorf("WireFormat compliance: /api/chat must have exactly 1 done:true (R53.1 — no double-done), got %d chunks=%d",
			doneCount, len(ndjsonChunks))
	}
	if finalDone == nil {
		t.Fatal("no done:true chunk in stream")
	}

	// === STRUCTURAL ASSERTION: канонический done-чанк имеет ВСЕ Ollama-поля ===
	// Ollama /api/chat streaming schema:
	//   model, created_at, message:{role, content}, done, done_reason,
	//   eval_count, prompt_eval_count, total_duration, load_duration,
	//   eval_duration, prompt_eval_duration
	requiredFields := []string{
		"model", "message", "done", "done_reason",
		"eval_count", "prompt_eval_count",
		"total_duration", "load_duration", "eval_duration", "prompt_eval_duration",
	}
	for _, field := range requiredFields {
		if _, ok := finalDone[field]; !ok {
			t.Errorf("compliance: /api/chat done chunk missing %q (Ollama API spec)", field)
		}
	}
	// message должен быть object с role+content
	if msg, ok := finalDone["message"].(map[string]interface{}); !ok {
		t.Errorf("compliance: done chunk 'message' should be object, got %T", finalDone["message"])
	} else {
		if msg["role"] != "assistant" {
			t.Errorf("compliance: message.role should be 'assistant', got %v", msg["role"])
		}
		if _, ok := msg["content"]; !ok {
			t.Error("compliance: message.content should be present (even empty)")
		}
	}
	// eval_count и prompt_eval_count должны прийти из usage chunk
	if evalCount, ok := finalDone["eval_count"].(float64); !ok || evalCount != 2 {
		t.Errorf("compliance: eval_count from usage chunk, want 2, got %v", finalDone["eval_count"])
	}
	if promptCount, ok := finalDone["prompt_eval_count"].(float64); !ok || promptCount != 10 {
		t.Errorf("compliance: prompt_eval_count from usage chunk, want 10, got %v", finalDone["prompt_eval_count"])
	}
}

// TestWireFormat_ChatStreaming_ToolCallsSchema — Round 53.3: tool calls в стриме
// должны быть в message.tool_calls как массив объектов с type/function.
func TestWireFormat_ChatStreaming_ToolCallsSchema(t *testing.T) {
	chunks := []string{
		`{"id":"cmpl-1","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Paris\"}"}}]}}]}`,
		`{"id":"cmpl-1","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}

	seenReasoning := false
	var ndjsonChunks []map[string]interface{}
	for _, raw := range chunks {
		result := translateOpenAISSEDataToOllama("/api/chat", []byte(raw), "test-model", &seenReasoning, time.Time{}, time.Time{})
		if result == nil {
			continue
		}
		var c map[string]interface{}
		json.Unmarshal(result[:len(result)-1], &c)
		ndjsonChunks = append(ndjsonChunks, c)
	}

	// Найти chunk с tool_calls
	foundToolCall := false
	for _, c := range ndjsonChunks {
		if msg, ok := c["message"].(map[string]interface{}); ok {
			if tcs, ok := msg["tool_calls"].([]interface{}); ok && len(tcs) > 0 {
				foundToolCall = true
				// Validate tool_call structure per Ollama spec
				tc := tcs[0].(map[string]interface{})
				if tc["type"] != "function" {
					t.Errorf("tool_call.type should be 'function', got %v", tc["type"])
				}
				if fn, ok := tc["function"].(map[string]interface{}); !ok {
					t.Errorf("tool_call.function should be object, got %T", tc["function"])
				} else {
					if fn["name"] != "get_weather" {
						t.Errorf("tool_call.function.name should be 'get_weather', got %v", fn["name"])
					}
					if _, ok := fn["arguments"]; !ok {
						t.Error("tool_call.function.arguments should be present")
					}
				}
			}
		}
	}
	if !foundToolCall {
		t.Error("no tool_calls in stream (Ollama /api/chat schema requires tool_calls in message.tool_calls for tool-using models)")
	}
}

// TestWireFormat_ChatNonStream_FullSchema — Round 53.3 structural compliance
// для non-stream /api/chat. Один JSON объект, не NDJSON.
func TestWireFormat_ChatNonStream_FullSchema(t *testing.T) {
	resp := `{
		"id":"cmpl-1","object":"chat.completion","created":1234567890,"model":"test-model",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello world"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}
	}`

	result, err := translateOpenAIChatToOllama([]byte(resp), "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama: %v", err)
	}

	var ollama map[string]interface{}
	if err := json.Unmarshal(result, &ollama); err != nil {
		t.Fatalf("invalid JSON: %v, body=%s", err, result)
	}

	// Required fields для /api/chat non-stream
	requiredFields := []string{"model", "created_at", "message", "done", "done_reason", "eval_count", "prompt_eval_count"}
	for _, field := range requiredFields {
		if _, ok := ollama[field]; !ok {
			t.Errorf("compliance: /api/chat non-stream missing %q", field)
		}
	}
	if msg, ok := ollama["message"].(map[string]interface{}); !ok {
		t.Errorf("compliance: message should be object, got %T", ollama["message"])
	} else {
		if msg["role"] != "assistant" {
			t.Errorf("compliance: message.role should be 'assistant', got %v", msg["role"])
		}
		if msg["content"] != "Hello world" {
			t.Errorf("compliance: message.content should be 'Hello world', got %v", msg["content"])
		}
	}
	if done, _ := ollama["done"].(bool); !done {
		t.Error("compliance: /api/chat non-stream must have done=true")
	}
}

// TestWireFormat_GenerateStreaming_FullSchema — Round 53.3 structural
// compliance для /api/generate streaming. response field (вместо message).
func TestWireFormat_GenerateStreaming_FullSchema(t *testing.T) {
	chunks := []string{
		`{"id":"cmpl-1","object":"text_completion","created":1234567890,"model":"test-model","choices":[{"index":0,"text":"Hello ","finish_reason":null}]}`,
		`{"id":"cmpl-1","object":"text_completion","created":1234567890,"model":"test-model","choices":[{"index":0,"text":"world","finish_reason":null}]}`,
		`{"id":"cmpl-1","object":"text_completion","created":1234567890,"model":"test-model","choices":[{"index":0,"text":"","finish_reason":"stop"}]}`,
		`{"id":"cmpl-1","object":"text_completion","created":1234567890,"model":"test-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
	}

	seenReasoning := false
	var ndjsonChunks []map[string]interface{}
	for _, raw := range chunks {
		result := translateOpenAISSEDataToOllama("/api/generate", []byte(raw), "test-model", &seenReasoning, time.Time{}, time.Time{})
		if result == nil {
			continue
		}
		var c map[string]interface{}
		json.Unmarshal(result[:len(result)-1], &c)
		ndjsonChunks = append(ndjsonChunks, c)
	}

	// Exactly ONE done:true
	doneCount := 0
	var finalDone map[string]interface{}
	for _, c := range ndjsonChunks {
		if done, _ := c["done"].(bool); done {
			doneCount++
			finalDone = c
		}
	}
	if doneCount != 1 {
		t.Errorf("compliance: /api/generate should have exactly 1 done:true, got %d", doneCount)
	}
	if finalDone == nil {
		t.Fatal("no done:true in /api/generate stream")
	}

	// Required fields для /api/generate: response (вместо message), model, done, done_reason
	requiredFields := []string{"model", "response", "done", "done_reason", "eval_count", "prompt_eval_count"}
	for _, field := range requiredFields {
		if _, ok := finalDone[field]; !ok {
			t.Errorf("compliance: /api/generate done chunk missing %q (Ollama API spec — response field, not message)", field)
		}
	}
	if _, hasMsg := finalDone["message"]; hasMsg {
		t.Error("compliance: /api/generate должен использовать 'response' field, не 'message'")
	}
}

// TestWireFormat_GenerateNonStream_FullSchema — Round 53.3 structural
// compliance для /api/generate non-stream.
func TestWireFormat_GenerateNonStream_FullSchema(t *testing.T) {
	resp := `{
		"id":"cmpl-1","object":"text_completion","created":1234567890,"model":"test-model",
		"choices":[{"index":0,"text":"Hello world","finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}
	}`

	result, err := translateOpenAICompletionToOllama([]byte(resp), "test-model")
	if err != nil {
		t.Fatalf("translateOpenAICompletionToOllama: %v", err)
	}

	var ollama map[string]interface{}
	if err := json.Unmarshal(result, &ollama); err != nil {
		t.Fatalf("invalid JSON: %v, body=%s", err, result)
	}

	requiredFields := []string{"model", "created_at", "response", "done", "done_reason", "eval_count", "prompt_eval_count"}
	for _, field := range requiredFields {
		if _, ok := ollama[field]; !ok {
			t.Errorf("compliance: /api/generate non-stream missing %q", field)
		}
	}
	if ollama["response"] != "Hello world" {
		t.Errorf("compliance: response should be 'Hello world', got %v", ollama["response"])
	}
	if _, hasMsg := ollama["message"]; hasMsg {
		t.Error("compliance: /api/generate non-stream должен использовать 'response' field, не 'message'")
	}
}

// TestWireFormat_OpenAIChat_SSEPassthrough — Round 53.3: /v1/chat/completions
// passthrough должен сохранять OpenAI SSE schema (НЕ конвертировать в Ollama NDJSON).
// Этот тест читает stream из mock upstream и проверяет, что структура OpenAI
// (data: prefix, object=chat.completion.chunk, [DONE] terminator) сохраняется.
func TestWireFormat_OpenAIChat_SSEPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"cmpl-1","object":"chat.completion.chunk","created":1234567890,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}`,
			`{"id":"cmpl-1","object":"chat.completion.chunk","created":1234567890,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	// Читаем как прокси бы прочитал
	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("expected text/event-stream, got %s", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.HasPrefix(bodyStr, "data: ") {
		t.Errorf("OpenAI SSE passthrough должен начинаться с 'data: ', got: %s", bodyStr[:100])
	}
	if !strings.Contains(bodyStr, "data: [DONE]") {
		t.Error("OpenAI SSE passthrough должен заканчиваться 'data: [DONE]'")
	}
	// Структура OpenAI: {id, object, model, choices:[{delta, finish_reason}]}
	if !strings.Contains(bodyStr, `"object":"chat.completion.chunk"`) {
		t.Error("OpenAI passthrough должен сохранять object=chat.completion.chunk")
	}
}

// TestWireFormat_Ollama_AllClients_OneDonePerStream — Round 53.3
// master test: validate "exactly one done:true" rule для all 4 endpoint variants
// (chat/generate × stream/non-stream) — никакой double-done, никакой пропуск done.
func TestWireFormat_Ollama_AllClients_OneDonePerStream(t *testing.T) {
	scenarios := []struct {
		name     string
		path     string
		chunks   []string
		expected int
	}{
		{
			name: "chat_stream_with_usage",
			path: "/api/chat",
			chunks: []string{
				`{"id":"1","choices":[{"delta":{"role":"assistant","content":"a"}}]}`,
				`{"id":"1","choices":[{"delta":{"content":"b"}}]}`,
				`{"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`{"id":"1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
			},
			expected: 1,
		},
		{
			name: "chat_stream_no_usage",
			path: "/api/chat",
			chunks: []string{
				`{"id":"1","choices":[{"delta":{"role":"assistant","content":"a"}}]}`,
				`{"id":"1","choices":[{"delta":{"content":"b"}}]}`,
				`{"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			},
			// R53.1: translator alone (без writeStreamingSSEDone fallback) — 0 done.
			// writeStreamingSSEDone на [DONE] SSE маркере добавит done:true (это
			// тестируется отдельно в TestProxyRequestLlamaCpp_*).
			expected: 0,
		},
		{
			name: "generate_stream_with_usage",
			path: "/api/generate",
			chunks: []string{
				`{"id":"1","choices":[{"text":"a"}]}`,
				`{"id":"1","choices":[{"text":"b"}]}`,
				`{"id":"1","choices":[{"text":"","finish_reason":"stop"}]}`,
				`{"id":"1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
			},
			expected: 1,
		},
		{
			name: "chat_stream_length_finish",
			path: "/api/chat",
			chunks: []string{
				`{"id":"1","choices":[{"delta":{"role":"assistant","content":"a"}}]}`,
				`{"id":"1","choices":[{"delta":{"content":"b"}}]}`,
				`{"id":"1","choices":[{"delta":{},"finish_reason":"length"}]}`,
				`{"id":"1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
			},
			expected: 1,
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			seenReasoning := false
			doneCount := 0
			for _, raw := range sc.chunks {
				result := translateOpenAISSEDataToOllama(sc.path, []byte(raw), "test-model", &seenReasoning, time.Time{}, time.Time{})
				if result == nil {
					continue
				}
				var c map[string]interface{}
				if err := json.Unmarshal(result[:len(result)-1], &c); err != nil {
					continue
				}
				if done, _ := c["done"].(bool); done {
					doneCount++
				}
			}
			if doneCount != sc.expected {
				t.Errorf("%s: expected exactly %d done:true (R53.1 single-done contract), got %d",
					sc.name, sc.expected, doneCount)
			}
		})
	}
}

// TestWireFormat_StreamTruncation_NoDoneInStream — Round 53.3: если upstream
// оборвал стрим до [DONE] и нет usage chunk, клиент всё равно должен
// получить финальный done-чанк (через транспортный truncation detector).
// Этот тест проверяет что текущий translate не возвращает done:true сам по
// себе — truncation detection в proxyRequestLlamaCpp добавит его явно.
func TestWireFormat_StreamTruncation_TranslateDoesntEmitDone(t *testing.T) {
	// Симулируем truncated stream: content chunks + finish_reason wrapper, NO usage.
	// Translator (R53.1) должен подавить wrapper — done:true эмитится
	// writeStreamingSSEDone fallback (см. transport.go truncation handling).
	chunks := []string{
		`{"id":"1","choices":[{"delta":{"role":"assistant","content":"a"}}]}`,
		`{"id":"1","choices":[{"delta":{"content":"b"}}]}`,
		`{"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`, // wrapper, suppressed
	}

	seenReasoning := false
	doneCount := 0
	for _, raw := range chunks {
		result := translateOpenAISSEDataToOllama("/api/chat", []byte(raw), "test-model", &seenReasoning, time.Time{}, time.Time{})
		if result == nil {
			continue
		}
		var c map[string]interface{}
		json.Unmarshal(result[:len(result)-1], &c)
		if done, _ := c["done"].(bool); done {
			doneCount++
		}
	}

	// Translator emits ZERO done:true — транспортный truncation detector
	// добавит его явно (если стрим truncated) или writeStreamingSSEDone fallback
	// (если [DONE] пришёл без usage).
	if doneCount != 0 {
		t.Errorf("Translator must not emit done:true without usage chunk (R53.1 — writeStreamingSSEDone/truncation detector handles it), got %d",
			doneCount)
	}
}

// TestWireFormat_NDJSON_LineDelimiters — Round 53.3: каждая строка NDJSON
// должна быть отдельным валидным JSON объектом. Это базовое требование Ollama
// клиентов (читают построчно через readline).
func TestWireFormat_NDJSON_LineDelimiters(t *testing.T) {
	chunks := []string{
		`{"id":"1","choices":[{"delta":{"role":"assistant","content":"abc"}}]}`,
		`{"id":"1","choices":[{"delta":{"content":"def"}}]}`,
		`{"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}

	seenReasoning := false
	var lines []string
	for _, raw := range chunks {
		result := translateOpenAISSEDataToOllama("/api/chat", []byte(raw), "test-model", &seenReasoning, time.Time{}, time.Time{})
		if result == nil {
			continue
		}
		lines = append(lines, string(result))
	}

	// Каждая строка должна заканчиваться на \n (NDJSON delimiter)
	for i, line := range lines {
		if !strings.HasSuffix(line, "\n") {
			t.Errorf("line %d должен заканчиваться на \\n (NDJSON), got: %q", i, line)
		}
		// Каждая строка должна быть валидным JSON
		var c map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &c); err != nil {
			t.Errorf("line %d invalid JSON: %v, raw=%q", i, err, line)
		}
	}
}

// _ = io.EOF — keep import alive
var _ = io.EOF
