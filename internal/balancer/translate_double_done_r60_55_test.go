// translate_double_done_r60_55_test.go — R60.55 (2026-09-13) + H2 (2026-10-16) regression tests.
//
// Bug (R60.55): when cppworker sends content+finish_reason chunk (non-wrapper, e.g.
// content+finish_reason="length" combined), translateOpenAISSEDataToOllama
// emits done:true as R51.3 safety-net. If a usage chunk follows (canonical
// Ollama done chunk with eval_count + total_duration), translateUsageChunkToOllama
// ALSO emitted done:true → client received TWO done:true chunks.
//
// Fix (R60.55): translateOpenAISSEDataToOllamaWithDoneFlag accepts priorDoneEmitted
// pointer. When set AND chunk is a usage chunk, the usage chunk was suppressed.
//
// Bug (H2, 2026-10-16): that suppression threw away the statistics too — when
// done:true had already been emitted (R51.3 safety-net / tool_calls path), the
// following usage chunk was dropped COMPLETELY, so eval_count / prompt_eval_count /
// total_duration never reached the client (OpenWebUI showed 0 / N/A).
//
// Fix (H2): track "done emitted" and "stats emitted" SEPARATELY. A usage chunk is
// suppressed only when the stats have ALREADY been delivered. If done:true was
// already emitted but the stats were not, the stats are forwarded as a line with
// done:false — so the stream keeps EXACTLY ONE done:true (R51.3/R53.1 invariant)
// and the statistics still arrive.
package balancer

import (
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

// r6055ParseChunk — parses one Ollama NDJSON line produced by the translator.
func r6055ParseChunk(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &m); err != nil {
		t.Fatalf("invalid NDJSON chunk: %v (raw=%s)", err, raw)
	}
	return m
}

// TestR6055_TranslateSSEData_SuppressUsageWhenPriorDoneEmitted — R60.55/H2 fix.
//
// H2 rewrite: a usage chunk that follows an already-emitted done:true must NOT be
// dropped blindly. The stream has to keep EXACTLY ONE done:true AND the statistics
// (eval_count / prompt_eval_count / total_duration) must still reach the client.
// The stats are forwarded as a non-final line (done:false).
func TestR6055_TranslateSSEData_SuppressUsageWhenPriorDoneEmitted(t *testing.T) {
	// 1) content+finish_reason chunk — R51.3 safety-net emits done:true for it.
	contentFinishChunk := []byte(`{"id":"chatcmpl-123","object":"chat.completion.chunk","created":1786626000,"model":"qwen3-instruct","choices":[{"delta":{"content":"last bit of code"},"finish_reason":"length","index":0}]}`)
	usageChunk := []byte(`{"id":"chatcmpl-123","object":"chat.completion.chunk","created":1786626000,"model":"qwen3-instruct","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":2500,"total_tokens":2600}}`)

	streamStart := time.Now().Add(-3 * time.Second)
	priorDone := false

	doneCount := 0
	statsChunks := 0
	var statsFields []map[string]interface{}

	for _, raw := range [][]byte{contentFinishChunk, usageChunk} {
		got := translateOpenAISSEDataToOllamaWithDoneFlag("/api/chat", raw, "qwen3-instruct", nil, streamStart, time.Time{}, "last bit of code", &priorDone)
		if got == nil {
			continue
		}
		parsed := r6055ParseChunk(t, got)
		if done, _ := parsed["done"].(bool); done {
			doneCount++
			priorDone = true
		}
		if _, ok := parsed["eval_count"]; ok {
			statsChunks++
			statsFields = append(statsFields, parsed)
		}
	}

	// Инвариант R51.3/R53.1: ровно один done:true в стриме.
	if doneCount != 1 {
		t.Errorf("H2 regression: stream must contain exactly one done:true, got %d", doneCount)
	}
	// H2: статистика обязана дойти (раньше usage-чанк гасился целиком).
	if statsChunks != 1 {
		t.Fatalf("H2 regression: statistics must reach the client exactly once, got %d stats chunks", statsChunks)
	}
	stats := statsFields[0]
	if done, _ := stats["done"].(bool); done {
		t.Errorf("H2 regression: stats line must NOT carry a second done:true (got done=true): %v", stats)
	}
	if got := stats["eval_count"]; got != float64(2500) {
		t.Errorf("H2 regression: eval_count must survive, got %v (want 2500)", got)
	}
	if got := stats["prompt_eval_count"]; got != float64(100) {
		t.Errorf("H2 regression: prompt_eval_count must survive, got %v (want 100)", got)
	}
	if got, ok := stats["total_duration"]; !ok {
		t.Error("H2 regression: total_duration missing from stats line")
	} else if ns, _ := got.(float64); ns <= 0 {
		t.Errorf("H2 regression: total_duration must be a positive duration, got %v", got)
	}
}

// TestR6055_TranslateSSEData_NilPriorDoneFlagPreservesBackwardCompat —
// passing nil for priorDoneEmitted must behave identically to the
// pre-R60.55 behavior (no suppression, canonical done:true with stats).
func TestR6055_TranslateSSEData_NilPriorDoneFlagPreservesBackwardCompat(t *testing.T) {
	usageChunk := []byte(`{"id":"chatcmpl-789","object":"chat.completion.chunk","created":1786626000,"model":"qwen3-instruct","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":100,"total_tokens":150}}`)

	got := translateOpenAISSEDataToOllamaWithDoneFlag("/api/chat", usageChunk, "qwen3-instruct", nil, time.Time{}, time.Time{}, "", nil)
	if got == nil {
		t.Fatal("with priorDoneEmitted=nil, usage chunk must be emitted (no suppression)")
	}
	if !strings.Contains(string(got), `"done":true`) {
		t.Errorf("usage chunk must contain done:true: %s", got)
	}
}

// TestR6055_TranslateSSEData_NonUsageChunkNeverSuppressed — only usage chunks
// are subject to priorDoneEmitted suppression. Content chunks, even with
// done:true (e.g. content+finish_reason safety-net), must always be emitted.
func TestR6055_TranslateSSEData_NonUsageChunkNeverSuppressed(t *testing.T) {
	// content + finish_reason chunk (non-wrapper, sets done:true via R51.3 safety-net)
	contentFinishChunk := []byte(`{"id":"chatcmpl-cf","object":"chat.completion.chunk","created":1786626000,"model":"qwen3-instruct","choices":[{"delta":{"content":"last bit of code"},"finish_reason":"stop","index":0}]}`)

	priorTrue := true
	got := translateOpenAISSEDataToOllamaWithDoneFlag("/api/chat", contentFinishChunk, "qwen3-instruct", nil, time.Time{}, time.Time{}, "", &priorTrue)
	if got == nil {
		t.Fatal("content+finish_reason chunk must NOT be suppressed by priorDoneEmitted (only usage chunks are)")
	}
	if !strings.Contains(string(got), `"done":true`) {
		t.Errorf("content+finish_reason chunk must contain done:true (R51.3 safety-net): %s", got)
	}
}

// TestR6055_TranslateSSEData_UsageChunkWithChoicesNotSuppressed — usage
// chunk detection requires !hasNonEmptyChoices. If chunk has BOTH usage
// AND non-empty choices (mixed), it's NOT a pure usage chunk and must
// NOT be suppressed (it carries content that client needs).
func TestR6055_TranslateSSEData_UsageChunkWithChoicesNotSuppressed(t *testing.T) {
	// Mixed chunk: usage + non-empty choices[0].delta.content. translateOpenAISSEDataToOllama
	// goes into translateSSEChatToOllama path (not translateUsageChunkToOllama).
	mixedChunk := []byte(`{"id":"chatcmpl-mix","object":"chat.completion.chunk","created":1786626000,"model":"qwen3-instruct","choices":[{"delta":{"content":"hello"},"finish_reason":null,"index":0}],"usage":{"prompt_tokens":50,"completion_tokens":100,"total_tokens":150}}`)

	priorTrue := true
	got := translateOpenAISSEDataToOllamaWithDoneFlag("/api/chat", mixedChunk, "qwen3-instruct", nil, time.Time{}, time.Time{}, "", &priorTrue)
	if got == nil {
		t.Fatal("mixed usage+choices chunk must NOT be suppressed (has content)")
	}
	if !strings.Contains(string(got), `"content":"hello"`) {
		t.Errorf("mixed chunk must contain content (regression!): %s", got)
	}
}

// TestR6055_H2_SuppressOnlyWhenStatsAlreadyEmitted — H2 unit test of the two
// independent per-stream flags (priorDoneEmitted / priorStatsEmitted):
//
//	(a) done:true уже ушёл, статистики не было → stats-only строка (done:false);
//	(b) статистика уже ушла → usage-чанк подавлен (nil);
//	(c) ничего не ушло → обычный canonical done:true со статистикой.
func TestR6055_H2_SuppressOnlyWhenStatsAlreadyEmitted(t *testing.T) {
	usageChunk := []byte(`{"id":"chatcmpl-h2","object":"chat.completion.chunk","created":1786626000,"model":"qwen3-instruct","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":2500,"total_tokens":2600}}`)

	// (a) — ровно сценарий H2.
	doneTrue, statsFalse := true, false
	got := translateOpenAISSEDataToOllamaWithFlags("/api/chat", usageChunk, "qwen3-instruct", nil, time.Time{}, time.Time{}, "some accumulated text", &doneTrue, &statsFalse)
	if got == nil {
		t.Fatal("H2: статистика после done:true обязана дойти до клиента (usage chunk suppressed regression)")
	}
	c := r6055ParseChunk(t, got)
	if done, _ := c["done"].(bool); done {
		t.Error("H2: stats-only строка не должна нести второй done:true")
	}
	if _, ok := c["done_reason"]; ok {
		t.Error("H2: done_reason допустим только у финального done:true чанка")
	}
	if msg, ok := c["message"].(map[string]interface{}); !ok {
		t.Error("H2: /api/chat чанк обязан иметь message")
	} else if msg["content"] != "" {
		t.Errorf("H2: stats-only строка не должна повторять content (иначе дубль ответа), got %v", msg["content"])
	}
	if c["eval_count"] != float64(2500) || c["prompt_eval_count"] != float64(100) {
		t.Errorf("H2: статистика потеряна: eval_count=%v prompt_eval_count=%v", c["eval_count"], c["prompt_eval_count"])
	}

	// (b) — статистика уже ушла → подавляем (никакого дубля).
	doneTrue2, statsTrue := true, true
	if got := translateOpenAISSEDataToOllamaWithFlags("/api/chat", usageChunk, "qwen3-instruct", nil, time.Time{}, time.Time{}, "", &doneTrue2, &statsTrue); got != nil {
		t.Errorf("H2: при statsEmitted=true usage-чанк должен подавляться, got %s", got)
	}

	// (c) — ничего не уходило → canonical done:true со статистикой (R51.3/R53.1).
	doneFalse, statsFalse2 := false, false
	got = translateOpenAISSEDataToOllamaWithFlags("/api/chat", usageChunk, "qwen3-instruct", nil, time.Time{}, time.Time{}, "", &doneFalse, &statsFalse2)
	if got == nil {
		t.Fatal("H2: первый usage-чанк обязан эмитить canonical done:true")
	}
	c = r6055ParseChunk(t, got)
	if done, _ := c["done"].(bool); !done {
		t.Error("H2: первый usage-чанк обязан быть финальным (done:true)")
	}
	if c["eval_count"] != float64(2500) {
		t.Errorf("H2: canonical done:true обязан нести статистику, got %v", c["eval_count"])
	}
}

// TestR6055_H2_UsageAfterSafetyNetDone_EndToEnd — H2 сквозь весь транспорт
// proxyRequestLlamaCpp (SSE cppworker → NDJSON клиенту).
//
// Сценарий: content+finish_reason в ОДНОМ чанке (R51.3 safety-net эмитит
// done:true), затем usage-чанк. До фикса статистика исчезала полностью.
// После фикса клиент получает РОВНО ОДИН done:true (R51.3/R53.1) и всю
// статистику (eval_count/prompt_eval_count/total_duration) отдельной строкой.
func TestR6055_H2_UsageAfterSafetyNetDone_EndToEnd(t *testing.T) {
	const (
		wantEval   = 2500
		wantPrompt = 100
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/chat") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		writeSSE := func(v map[string]interface{}) {
			payload, _ := json.Marshal(v)
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(payload)
			_, _ = w.Write([]byte("\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}

		writeSSE(map[string]interface{}{
			"id": "chatcmpl-h2", "object": "chat.completion.chunk", "created": 1700000000, "model": "qwen3-instruct",
			"choices": []interface{}{map[string]interface{}{"index": 0, "delta": map[string]interface{}{"role": "assistant"}}},
		})
		writeSSE(map[string]interface{}{
			"id": "chatcmpl-h2", "object": "chat.completion.chunk", "created": 1700000000, "model": "qwen3-instruct",
			"choices": []interface{}{map[string]interface{}{"index": 0, "delta": map[string]interface{}{"content": "Hello"}}},
		})
		// Триггер H2: content И finish_reason в одном чанке → R51.3 safety-net
		// эмитит done:true ДО того, как придёт статистика.
		writeSSE(map[string]interface{}{
			"id": "chatcmpl-h2", "object": "chat.completion.chunk", "created": 1700000000, "model": "qwen3-instruct",
			"choices": []interface{}{map[string]interface{}{"index": 0, "delta": map[string]interface{}{"content": " world"}, "finish_reason": "stop"}},
		})
		// usage-чанк (canonical Ollama done-чанк со статистикой).
		writeSSE(map[string]interface{}{
			"id": "chatcmpl-h2", "object": "chat.completion.chunk", "created": 1700000000, "model": "qwen3-instruct",
			"choices": []interface{}{},
			"usage":   map[string]interface{}{"prompt_tokens": wantPrompt, "completion_tokens": wantEval, "total_tokens": wantPrompt + wantEval},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	bodyObj := map[string]interface{}{
		"model":    "qwen3-instruct",
		"messages": []interface{}{map[string]string{"role": "user", "content": "hello"}},
		"stream":   true,
	}
	bodyBytes, err := json.Marshal(bodyObj)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()

	if err := p.proxyRequestLlamaCpp(rec, req.WithContext(ctx), "llama_test", bodyBytes); err != nil {
		t.Fatalf("proxyRequestLlamaCpp: %v", err)
	}

	chunks := parseNDJSONResponse(t, bytes.NewReader(rec.Body.Bytes()))
	doneCount := 0
	statsCount := 0
	for _, c := range chunks {
		if done, _ := c["done"].(bool); done {
			doneCount++
		}
		if _, ok := c["eval_count"]; ok {
			statsCount++
			if c["eval_count"] != float64(wantEval) {
				t.Errorf("H2 e2e: eval_count = %v, want %d", c["eval_count"], wantEval)
			}
			if c["prompt_eval_count"] != float64(wantPrompt) {
				t.Errorf("H2 e2e: prompt_eval_count = %v, want %d", c["prompt_eval_count"], wantPrompt)
			}
			if ns, ok := c["total_duration"].(float64); !ok || ns <= 0 {
				t.Errorf("H2 e2e: total_duration должен быть положительным, got %v", c["total_duration"])
			}
			if done, _ := c["done"].(bool); done {
				t.Error("H2 e2e: статистика пришла в чанке с done:true — ожидалась отдельная строка (done:false)")
			}
		}
	}
	if doneCount != 1 {
		t.Errorf("R51.3/R53.1: клиент обязан получить ровно один done:true, got %d (body=%s)", doneCount, rec.Body.String())
	}
	if statsCount != 1 {
		t.Errorf("H2: статистика обязана дойти ровно один раз, got %d (body=%s)", statsCount, rec.Body.String())
	}
}
