// translate_double_done_r60_55_test.go — R60.55 (2026-09-13) regression tests.
//
// Bug: when cppworker sends content+finish_reason chunk (non-wrapper, e.g.
// content+finish_reason="length" combined), translateOpenAISSEDataToOllama
// emits done:true as R51.3 safety-net. If a usage chunk follows (canonical
// Ollama done chunk with eval_count + total_duration), translateUsageChunkToOllama
// ALSO emits done:true → client receives TWO done:true chunks.
//
// Qwen3-Instruct antipattern: model emits greeting+code, then finish_reason
// chunk with last bit of content, then usage chunk. Without priorDoneEmitted
// suppression, balancer emits 2 done-chunks → OpenWebUI displays as
// "duplicate response".
//
// Fix: translateOpenAISSEDataToOllamaWithDoneFlag accepts priorDoneEmitted
// pointer. When set AND chunk is a usage chunk, returns nil (suppressed).
package balancer

import (
	"strings"
	"testing"
	"time"
)

// TestR6055_TranslateSSEData_SuppressUsageWhenPriorDoneEmitted — R60.55 fix.
// Usage chunk after a prior done:true chunk (e.g. finish_reason+content)
// must be suppressed to avoid double done:true emission.
func TestR6055_TranslateSSEData_SuppressUsageWhenPriorDoneEmitted(t *testing.T) {
	usageChunk := []byte(`{"id":"chatcmpl-123","object":"chat.completion.chunk","created":1786626000,"model":"qwen3-instruct","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":2500,"total_tokens":2600}}`)

	// priorDoneEmitted=false → usage chunk emitted normally
	priorFalse := false
	gotNormal := translateOpenAISSEDataToOllamaWithDoneFlag("/api/chat", usageChunk, "qwen3-instruct", nil, time.Time{}, time.Time{}, "", &priorFalse)
	if gotNormal == nil {
		t.Fatal("with priorDoneEmitted=false, usage chunk must be emitted (regression!)")
	}
	if !strings.Contains(string(gotNormal), `"done":true`) {
		t.Errorf("usage chunk must contain done:true: %s", gotNormal)
	}

	// priorDoneEmitted=true → usage chunk suppressed
	priorTrue := true
	gotSuppressed := translateOpenAISSEDataToOllamaWithDoneFlag("/api/chat", usageChunk, "qwen3-instruct", nil, time.Time{}, time.Time{}, "", &priorTrue)
	if gotSuppressed != nil {
		t.Errorf("with priorDoneEmitted=true, usage chunk must be suppressed (nil), got: %s", gotSuppressed)
	}
}

// TestR6055_TranslateSSEData_NilPriorDoneFlagPreservesBackwardCompat —
// passing nil for priorDoneEmitted must behave identically to the
// pre-R60.55 behavior (no suppression). This ensures backward compat
// for any callers that haven't been updated.
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
	// content + finish_reason chunk (non-wrapper, would set done:true via R51.3 safety-net)
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
