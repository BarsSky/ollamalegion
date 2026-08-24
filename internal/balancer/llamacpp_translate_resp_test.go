// llamacpp_translate_resp_test.go — Unit tests for response translation
// functions in llamacpp_translate_resp.go.
package balancer

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// ============================================================
// Test: translateOpenAIChatToOllama with tool_calls
// ============================================================

func TestTranslateOpenAIChatToOllama_ToolCalls(t *testing.T) {
	body := buildOpenAIRespWithToolCalls()
	result, err := translateOpenAIChatToOllama(body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	if err := json.Unmarshal(result, &ollamaResp); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	message, ok := ollamaResp["message"].(map[string]interface{})
	if !ok {
		t.Fatal("expected 'message' in ollama response")
	}

	toolCalls, ok := message["tool_calls"].([]interface{})
	if !ok {
		t.Fatal("expected 'tool_calls' array in ollama message")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}

	firstCall := toolCalls[0].(map[string]interface{})
	if firstCall["id"] != "call_search" {
		t.Errorf("expected id 'call_search', got '%v'", firstCall["id"])
	}
	if firstCall["type"] != "function" {
		t.Errorf("expected type 'function', got '%v'", firstCall["type"])
	}
	funcObj := firstCall["function"].(map[string]interface{})
	if funcObj["name"] != "search" {
		t.Errorf("expected function name 'search', got '%v'", funcObj["name"])
	}

	// Content should be "" (empty string, not nil) when tool_calls present
	if message["content"] == nil {
		t.Error("expected content to be empty string, not nil")
	}

	// done should be true for finish_reason="tool_calls"
	if done, ok := ollamaResp["done"].(bool); !ok || !done {
		t.Error("expected done=true for finish_reason=tool_calls")
	}

	// done_reason should be "tool_calls"
	if ollamaResp["done_reason"] != "tool_calls" {
		t.Errorf("expected done_reason 'tool_calls', got '%v'", ollamaResp["done_reason"])
	}
}

func TestTranslateOpenAIChatToOllama_NoToolCalls(t *testing.T) {
	body := buildOpenAIRespWithoutToolCalls()
	result, err := translateOpenAIChatToOllama(body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)

	message := ollamaResp["message"].(map[string]interface{})
	if message["content"] == nil || message["content"] == "" {
		t.Error("expected non-empty content for non-tool response")
	}
	if _, exists := message["tool_calls"]; exists {
		t.Error("expected no 'tool_calls' in response without tools")
	}
	if ollamaResp["done_reason"] != "stop" {
		t.Errorf("expected done_reason='stop', got '%v'", ollamaResp["done_reason"])
	}
}

func TestTranslateOpenAIChatToOllama_ErrorResponse(t *testing.T) {
	body := buildOpenAIErrorResp()
	result, err := translateOpenAIChatToOllama(body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)

	if ollamaResp["done_reason"] != "error" {
		t.Errorf("expected done_reason='error', got '%v'", ollamaResp["done_reason"])
	}
	if _, ok := ollamaResp["error"]; !ok {
		t.Error("expected 'error' field in ollama response")
	}
	if _, ok := ollamaResp["message"]; !ok {
		t.Error("expected 'message' field even in error response")
	}
}

// ============================================================
// Test: translateOpenAISSEDataToOllama / translateSSEChatToOllama
// ============================================================

func TestTranslateSSEChatToOllama_ToolCalls(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]interface{}{
						{
							"id":   "call_search",
							"type": "function",
							"function": map[string]interface{}{
								"name":      "search",
								"arguments": `{"q":"test"}`,
							},
						},
					},
				},
				"finish_reason": nil,
			},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result from SSE translation")
	}

	var ollamaChunk map[string]interface{}
	if err := json.Unmarshal(result, &ollamaChunk); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	message := ollamaChunk["message"].(map[string]interface{})
	toolCalls, ok := message["tool_calls"].([]interface{})
	if !ok {
		t.Fatal("expected 'tool_calls' array in ollama SSE chunk")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}

	// done should be false (this is a delta chunk, not finish)
	if done, ok := ollamaChunk["done"].(bool); ok && done {
		t.Error("expected done=false for delta chunk")
	}
}

// TestTranslateSSEChatToOllama_FinishReasonToolCallsWrapper — Round 53.1
// (2026-08-24) regression test: empty-delta+finish_reason="tool_calls" wrapper
// chunk (which precedes a usage chunk in OpenAI streaming) MUST NOT be marked
// done:true. R51.3 marked it done:true → "phantom done" before the real usage
// chunk → Cline reports "Did not receive done or success response in stream".
//
// Корректная семантика: wrapper chunk suppresses (returns nil) — done:true
// is emitted only ONCE per stream, by translateUsageChunkToOllama (when usage
// chunk follows) or by writeStreamingSSEDone (fallback on [DONE]).
func TestTranslateSSEChatToOllama_FinishReasonToolCallsWrapper(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "tool_calls",
			},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})

	// Round 53.1: wrapper chunk is suppressed entirely (returns nil).
	// Done:true is emitted by the usage chunk (or writeStreamingSSEDone fallback),
	// not by the wrapper — otherwise the client sees TWO done:true chunks.
	if result != nil {
		t.Errorf("Round 53.1: expected nil (wrapper chunk suppressed), got %s", string(result))
	}
}

func TestTranslateSSEChatToOllama_NormalContent(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"role":    "assistant",
					"content": "Hello",
				},
			},
		},
	}

	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	var ollamaChunk map[string]interface{}
	json.Unmarshal(result, &ollamaChunk)

	message := ollamaChunk["message"].(map[string]interface{})
	if message["content"] != "Hello" {
		t.Errorf("expected content 'Hello', got '%v'", message["content"])
	}
	if _, exists := message["tool_calls"]; exists {
		t.Error("unexpected tool_calls in normal content chunk")
	}
}

func TestTranslateSSEChatToOllama_DONE(t *testing.T) {
	result := translateOpenAISSEDataToOllama("/api/chat", []byte("[DONE]"), "test-model", nil, time.Time{})
	if result != nil {
		t.Error("expected nil for [DONE] marker")
	}
}

// ============================================================
// Round 51.3 (2026-08-20): regression tests for finish_reason
// handling. Cline (через ollama npm / langchain) валит с
// "Did not receive done or success response in stream" если
// финальный чанк имеет done=false. Прежний код ставил done:true
// только для finish_reason="stop" или "tool_calls" — для
// "length" (max_tokens hit) или "" (пустая) done оставался false.
// ============================================================

// TestTranslateSSEChatToOllama_FinishReasonLengthWrapper — Round 53.1
// (2026-08-24) regression test: empty-delta+finish_reason="length" wrapper
// chunk (precedes usage chunk) MUST NOT be marked done:true. See full
// explanation in TestTranslateSSEChatToOllama_FinishReasonToolCallsWrapper.
//
// Pre-R53.1 (R51.3): wrapper marked done:true → "phantom done" with empty
// content and no stats → Cline "Did not receive done" error.
//
// Post-R53.1: wrapper returns nil (suppressed). done:true emitted only by
// usage chunk or [DONE] fallback.
func TestTranslateSSEChatToOllama_FinishReasonLengthWrapper(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-length",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "length",
			},
		},
	}
	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})

	if result != nil {
		t.Errorf("Round 53.1: expected nil (wrapper chunk suppressed), got %s", string(result))
	}
}

// TestTranslateSSEChatToOllama_FinishReasonContentFilterWrapper — Round 53.1
// regression test (see TestTranslateSSEChatToOllama_FinishReasonLengthWrapper
// for full context). content_filter wrapper chunk also returns nil.
func TestTranslateSSEChatToOllama_FinishReasonContentFilterWrapper(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-cf",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "content_filter",
			},
		},
	}
	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})

	if result != nil {
		t.Errorf("Round 53.1: expected nil (wrapper chunk suppressed), got %s", string(result))
	}
}

// TestTranslateSSEChatToOllama_FinishReasonLengthWithContent — Round 53.1
// safety net test: when finish_reason="length" arrives IN THE SAME chunk as
// content (some upstreams combine the final content token with finish_reason),
// the chunk IS the truly final chunk and MUST be marked done:true. Otherwise,
// if no usage chunk follows, the client would never receive done and hang
// (Cline "Did not receive done" error).
func TestTranslateSSEChatToOllama_FinishReasonLengthWithContent(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-length",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"content": "final truncated...",
				},
				"finish_reason": "length",
			},
		},
	}
	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result for combined content+finish_reason=length chunk")
	}
	var ollamaChunk map[string]interface{}
	json.Unmarshal(result, &ollamaChunk)
	if done, ok := ollamaChunk["done"].(bool); !ok || !done {
		t.Errorf("Round 53.1 safety net: expected done=true for content+finish_reason=length, got done=%v — client would hang",
			ollamaChunk["done"])
	}
	if ollamaChunk["done_reason"] != "length" {
		t.Errorf("expected done_reason='length', got '%v'", ollamaChunk["done_reason"])
	}
}

// TestTranslateSSEChatToOllama_FinishReasonNull — in-progress chunk (no finish_reason yet)
// → done:false. Sanity check that fix didn't break in-progress case.
func TestTranslateSSEChatToOllama_FinishReasonNull(t *testing.T) {
	sseChunk := map[string]interface{}{
		"id":      "chatcmpl-progress",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"content": "tok"},
				"finish_reason": nil,
			},
		},
	}
	sseData, _ := json.Marshal(sseChunk)
	result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", nil, time.Time{})
	if result == nil {
		t.Fatal("expected non-nil result for in-progress chunk")
	}
	var ollamaChunk map[string]interface{}
	json.Unmarshal(result, &ollamaChunk)
	if done, ok := ollamaChunk["done"].(bool); !ok || done {
		t.Errorf("expected done=false for in-progress chunk (finish_reason=null), got %v", ollamaChunk["done"])
	}
}

// TestTranslateOpenAIChatToOllama_FinishReasonLength — non-streaming /api/chat
// translation: finish_reason="length" → done:true.
func TestTranslateOpenAIChatToOllama_FinishReasonLength(t *testing.T) {
	resp := map[string]interface{}{
		"id":      "chatcmpl-length",
		"object":  "chat.completion",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "truncated...",
				},
				"finish_reason": "length",
			},
		},
	}
	body, _ := json.Marshal(resp)
	result, err := translateOpenAIChatToOllama(body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama: %v", err)
	}
	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)
	if done, ok := ollamaResp["done"].(bool); !ok || !done {
		t.Errorf("expected done=true for finish_reason=length, got %v — Cline will throw", ollamaResp["done"])
	}
	if ollamaResp["done_reason"] != "length" {
		t.Errorf("expected done_reason='length', got '%v'", ollamaResp["done_reason"])
	}
}

// TestTranslateOpenAICompletionToOllama_FinishReasonLength — non-streaming /v1/completions
// translation: finish_reason="length" → done:true.
func TestTranslateOpenAICompletionToOllama_FinishReasonLength(t *testing.T) {
	resp := map[string]interface{}{
		"id":      "cmpl-length",
		"object":  "text_completion",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"text":          "truncated...",
				"finish_reason": "length",
			},
		},
	}
	body, _ := json.Marshal(resp)
	result, err := translateOpenAICompletionToOllama(body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAICompletionToOllama: %v", err)
	}
	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)
	if done, ok := ollamaResp["done"].(bool); !ok || !done {
		t.Errorf("expected done=true for finish_reason=length, got %v", ollamaResp["done"])
	}
}

// ============================================================
// Round 53.1 (2026-08-24): end-to-end stream tests for Ollama API compliance.
//
// КРИТИЧЕСКИЙ ТЕСТ: полный OpenAI→Ollama стрим должен иметь ровно ОДИН done:true
// чанк с полным набором полей. R51.3 регрессия: два done:true чанка (пустой
// wrapper + usage чанк) → Cline/ollama npm парсер видит "phantom done" с
// пустым content и нет stats → "Did not receive done or success response in
// stream" ошибка.
// ============================================================

// TestTranslateSSEChatToOllama_FullStream_SingleDone — Round 53.1 structural
// compliance test. Симулирует полный cppworker stream (content chunks +
// finish_reason wrapper + usage chunk) и проверяет что:
//   1. Ровно ОДИН done:true чанк (от usage chunk)
//   2. Этот чанк содержит ВСЕ обязательные Ollama поля: eval_count,
//      prompt_eval_count, total_duration, model, done_reason, message
//   3. Никакой "phantom done" (с done:true но без stats) в стриме
//
// Это структурный тест compliance с Ollama NDJSON API spec — не просто
// "есть content", а "ответ имеет канонический вид".
func TestTranslateSSEChatToOllama_FullStream_SingleDone(t *testing.T) {
	// 1) content chunk
	contentChunk := map[string]interface{}{
		"id":      "chatcmpl-stream",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"role":    "assistant",
					"content": "Hello",
				},
			},
		},
	}
	// 2) finish_reason wrapper chunk (empty delta + finish_reason="stop")
	wrapperChunk := map[string]interface{}{
		"id":      "chatcmpl-stream",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	}
	// 3) usage chunk (no choices, only usage)
	usageChunk := map[string]interface{}{
		"id":      "chatcmpl-stream",
		"object":  "chat.completion.chunk",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []interface{}{},
		"usage": map[string]interface{}{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		},
	}

	var seenReasoning bool
	streamStart := time.Now()
	var ndjsonChunks []map[string]interface{}

	for i, chunk := range []map[string]interface{}{contentChunk, wrapperChunk, usageChunk} {
		sseData, _ := json.Marshal(chunk)
		result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", &seenReasoning, streamStart)
		if result == nil {
			continue
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("chunk %d: invalid NDJSON output: %v", i, err)
		}
		ndjsonChunks = append(ndjsonChunks, parsed)
	}

	// Подсчёт done:true чанков
	doneCount := 0
	var finalDone map[string]interface{}
	for i, c := range ndjsonChunks {
		if done, ok := c["done"].(bool); ok && done {
			doneCount++
			finalDone = c
			t.Logf("  chunk[%d] DONE: keys=%v, eval_count=%v, total_duration=%v",
				i, sortedKeys(c), c["eval_count"], c["total_duration"])
		}
	}

	// === STRUCTURAL ASSERTION: ровно ОДИН done:true ===
	if doneCount != 1 {
		t.Errorf("Round 53.1 compliance: expected exactly 1 done:true chunk, got %d (chunks=%d) — R51.3 regression",
			doneCount, len(ndjsonChunks))
	}
	if finalDone == nil {
		t.Fatal("no done:true chunk found")
	}

	// === STRUCTURAL ASSERTION: канонический done-чанк имеет все Ollama-поля ===
	// Ollama API: model, done, done_reason, eval_count, prompt_eval_count,
	// total_duration (опционально load_duration, prompt_eval_duration, eval_duration),
	// message (с role+content).
	requiredFields := []string{"model", "done", "done_reason", "eval_count", "prompt_eval_count", "total_duration", "message"}
	for _, field := range requiredFields {
		if _, ok := finalDone[field]; !ok {
			t.Errorf("compliance: done chunk missing required field %q (Ollama API spec). Got keys=%v",
				field, sortedKeys(finalDone))
		}
	}
	if msg, ok := finalDone["message"].(map[string]interface{}); !ok {
		t.Errorf("compliance: done chunk 'message' should be object, got %T", finalDone["message"])
	} else {
		if _, ok := msg["role"]; !ok {
			t.Error("compliance: done chunk message.role missing")
		}
		if _, ok := msg["content"]; !ok {
			t.Error("compliance: done chunk message.content missing")
		}
	}
	// eval_count и total_duration должны быть > 0 (из usage chunk)
	if evalCount, ok := finalDone["eval_count"].(float64); !ok || evalCount <= 0 {
		t.Errorf("compliance: done chunk eval_count should be >0 (from usage chunk), got %v", finalDone["eval_count"])
	}
}

// TestTranslateSSEChatToOllama_NoUsageChunk_FallbackDone — Round 53.1
// safety net: если cppworker НЕ шлёт usage chunk после finish_reason wrapper,
// writeStreamingSSEDone (called on [DONE] SSE marker) пишет финальный done-чанк.
// Translator (R53.1) НЕ ставит done:true на wrapper, поэтому writeStreamingSSEDone
// должен срабатывать.
//
// Этот тест проверяет логику translator: wrapper chunk должен подавляться
// (return nil), чтобы writeStreamingSSEDone мог эмитить canonical done-чанк
// с накопленным content.
func TestTranslateSSEChatToOllama_NoUsageChunk_WrapperSuppressed(t *testing.T) {
	// cppworker: content + finish_reason wrapper + [DONE] (no usage chunk)
	contentChunk := map[string]interface{}{
		"id":      "chatcmpl-nofallback",
		"object":  "chat.completion.chunk",
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{"content": "abc"}},
		},
	}
	wrapperChunk := map[string]interface{}{
		"id":      "chatcmpl-nofallback",
		"object":  "chat.completion.chunk",
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"},
		},
	}

	var seenReasoning bool
	streamStart := time.Now()
	var ndjsonChunks []map[string]interface{}
	for _, chunk := range []map[string]interface{}{contentChunk, wrapperChunk} {
		sseData, _ := json.Marshal(chunk)
		result := translateOpenAISSEDataToOllama("/api/chat", sseData, "test-model", &seenReasoning, streamStart)
		if result == nil {
			continue
		}
		var parsed map[string]interface{}
		json.Unmarshal(result, &parsed)
		ndjsonChunks = append(ndjsonChunks, parsed)
	}

	// Только content chunk должен быть эмитирован, wrapper подавлен.
	if len(ndjsonChunks) != 1 {
		t.Errorf("expected 1 chunk (content only, wrapper suppressed), got %d", len(ndjsonChunks))
	}
	for i, c := range ndjsonChunks {
		if done, _ := c["done"].(bool); done {
			t.Errorf("chunk[%d] should not have done:true (wrapper suppressed, fallback path expected)", i)
		}
	}
}

// TestTranslateSSEGenerateToOllama_FinishReasonWrapper — Round 53.1 regression
// test для /api/generate: empty-delta+finish_reason wrapper chunk тоже
// подавляется, чтобы избежать double-done с usage-чанком.
func TestTranslateSSEGenerateToOllama_FinishReasonWrapper(t *testing.T) {
	wrapperChunk := map[string]interface{}{
		"id":      "cmpl-gen",
		"object":  "text_completion",
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"},
		},
	}
	sseData, _ := json.Marshal(wrapperChunk)
	result := translateOpenAISSEDataToOllama("/api/generate", sseData, "test-model", nil, time.Time{})
	if result != nil {
		t.Errorf("Round 53.1: expected nil (wrapper chunk suppressed for /api/generate), got %s", string(result))
	}
}

// sortedKeys — helper для дебага (выводит ключи в алфавитном порядке).
func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ============================================================
// Test: translateOllamaChatToOpenAI with tools
// ============================================================

func TestTranslateOllamaChatToOpenAI_ToolsPassthrough(t *testing.T) {
	ollamaBody := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Search for AI news"},
		},
		"stream": false,
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "search",
					"description": "Search the web",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"q": map[string]interface{}{
								"type":        "string",
								"description": "Search query",
							},
						},
					},
				},
			},
		},
		"tool_choice": "auto",
	}

	body, _ := json.Marshal(ollamaBody)
	result, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(result, &openAIReq)

	tools, ok := openAIReq["tools"].([]interface{})
	if !ok {
		t.Fatal("expected 'tools' in OpenAI request")
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	if openAIReq["tool_choice"] != "auto" {
		t.Errorf("expected tool_choice='auto', got '%v'", openAIReq["tool_choice"])
	}
}

func TestTranslateOllamaChatToOpenAI_NoTools(t *testing.T) {
	ollamaBody := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Hello"},
		},
		"stream": true,
	}

	body, _ := json.Marshal(ollamaBody)
	result, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(result, &openAIReq)

	if _, exists := openAIReq["tools"]; exists {
		t.Error("unexpected 'tools' in request without tools")
	}
	if _, exists := openAIReq["tool_choice"]; exists {
		t.Error("unexpected 'tool_choice' in request without tools")
	}
}

// ============================================================
// Test: translateOpenAIResponseToOllama — multiple scenarios
// ============================================================

func TestTranslateOpenAIResponseToOllama_Embeddings(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"data": []map[string]interface{}{
			{
				"embedding": []float64{0.1, 0.2, 0.3},
			},
		},
	})

	result, err := translateOpenAIResponseToOllama("/api/embeddings", body, "test-model")
	if err != nil {
		t.Fatalf("translateOpenAIResponseToOllama failed: %v", err)
	}

	var ollamaResp map[string]interface{}
	json.Unmarshal(result, &ollamaResp)

	embedding, ok := ollamaResp["embedding"].([]interface{})
	if !ok {
		t.Fatal("expected 'embedding' array")
	}
	if len(embedding) != 3 {
		t.Fatalf("expected 3 values, got %d", len(embedding))
	}
}

func TestTranslateOllamaChatToOpenAI_ExtractsOptions(t *testing.T) {
	ollamaBody := map[string]interface{}{
		"model": "test",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hi"},
		},
		"options": map[string]interface{}{
			"temperature": 0.7,
			"top_p":      0.9,
			"num_predict": 100,
			"stop":       []string{"\n"},
		},
		"stream": true,
	}

	body, _ := json.Marshal(ollamaBody)
	result, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translateOllamaChatToOpenAI failed: %v", err)
	}

	var openAIReq map[string]interface{}
	json.Unmarshal(result, &openAIReq)

	if openAIReq["temperature"] != 0.7 {
		t.Errorf("expected temperature 0.7, got %v", openAIReq["temperature"])
	}
	if openAIReq["top_p"] != 0.9 {
		t.Errorf("expected top_p 0.9, got %v", openAIReq["top_p"])
	}
	if openAIReq["max_tokens"] != 100.0 {
		t.Errorf("expected max_tokens 100, got %v", openAIReq["max_tokens"])
	}
}

// ============================================================
// Helpers
// ============================================================

func buildOpenAIRespWithToolCalls() []byte {
	resp := map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion",
		"created": 1234567890,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]interface{}{
						{
							"id":   "call_search",
							"type": "function",
							"function": map[string]interface{}{
								"name":      "search",
								"arguments": `{"q":"test"}`,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	}
	body, _ := json.Marshal(resp)
	return body
}

func buildOpenAIRespWithoutToolCalls() []byte {
	resp := map[string]interface{}{
		"id":      "chatcmpl-456",
		"object":  "chat.completion",
		"created": 1234567891,
		"model":   "test-model",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "Hello, how can I help?",
				},
				"finish_reason": "stop",
			},
		},
	}
	body, _ := json.Marshal(resp)
	return body
}

func buildOpenAIErrorResp() []byte {
	resp := map[string]interface{}{
		"error": "rate limit exceeded",
	}
	body, _ := json.Marshal(resp)
	return body
}

// Helper to check if a string contains a substring (case-insensitive)
func strContains(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

