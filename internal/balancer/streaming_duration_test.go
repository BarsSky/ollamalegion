// streaming_duration_test.go — Round 35c+ (2026-08-13)
// Tests for real duration tracking в sendSSEDone и translateUsageChunkToOllama.
//
// User report (A10, 2026-08-13):
// "total_duration/load_duration/prompt_eval_duration/eval_duration остаются 0
// — для них нужен time tracking от start of request в balancer".
//
// Fix:
// 1. sendSSEDone (streaming.go:398) теперь принимает streamStart time.Time
//    и пишет реальный total_duration в nanoseconds в done-чанке.
// 2. translateUsageChunkToOllama (llamacpp_translate_resp.go) тоже принимает
//    streamStart и пишет real total_duration в usage-чанке.
package balancer

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// flusherRecorder — wraps a bytes.Buffer и реализует http.Flusher (no-op)
// + http.ResponseWriter (через embedded bytes.Buffer). Используется в тестах
// для capture'а SSE-вывода из sendSSEDone.
type flusherRecorder struct {
	*bytes.Buffer
}

func (f *flusherRecorder) Header() http.Header {
	return http.Header{}
}
func (f *flusherRecorder) WriteHeader(statusCode int) {}
func (f *flusherRecorder) Flush()                     {}

// Реализация остальных методов http.ResponseWriter — не нужна для тестов.

// readBody — helper для получения body из flusherRecorder.
func readBody(r io.Reader) string {
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

// TestSendSSEDone_RealDuration — sendSSEDone пишет реальный total_duration
// в наносекундах, вычисленный из streamStart.
func TestSendSSEDone_RealDuration(t *testing.T) {
	buf := &flusherRecorder{Buffer: &bytes.Buffer{}}
	// 100ms прошло с начала запроса
	streamStart := time.Now().Add(-100 * time.Millisecond)

	sendSSEDone(buf, buf, streamStart)

	body := buf.String()
	t.Logf("sendSSEDone body: %s", body)

	if !strings.Contains(body, `"done":true`) {
		t.Errorf("expected done=true in SSE body, got: %s", body)
	}

	// Извлекаем total_duration из JSON
	cleanBody := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(body), "\n\n"), "data: ")
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(cleanBody), &parsed); err != nil {
		t.Fatalf("failed to parse SSE body: %v", err)
	}

	totalDur, ok := parsed["total_duration"].(float64)
	if !ok {
		t.Fatalf("expected total_duration float64, got %T: %v", parsed["total_duration"], parsed["total_duration"])
	}

	// 100ms = ~100_000_000 ns. Допускаем диапазон [50ms, 500ms] для test latency.
	if totalDur < 50_000_000 || totalDur > 500_000_000 {
		t.Errorf("total_duration=%v ns, want ~100_000_000 (100ms ±)", totalDur)
	}
}

// TestSendSSEDone_ZeroStreamStart — legacy callers (streamStart.IsZero())
// → total_duration=0 (fallback для backward compat).
func TestSendSSEDone_ZeroStreamStart(t *testing.T) {
	buf := &flusherRecorder{Buffer: &bytes.Buffer{}}

	sendSSEDone(buf, buf, time.Time{}) // zero = legacy caller

	body := buf.String()
	cleanBody := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(body), "\n\n"), "data: ")
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(cleanBody), &parsed); err != nil {
		t.Fatalf("failed to parse SSE body: %v", err)
	}

	if dur, _ := parsed["total_duration"].(float64); dur != 0 {
		t.Errorf("expected total_duration=0 for zero streamStart, got %v", dur)
	}
}

// TestTranslateUsageChunkToOllama_RealTotalDuration — usage chunk translation
// пишет real total_duration в Ollama done-чанке.
func TestTranslateUsageChunkToOllama_RealTotalDuration(t *testing.T) {
	usageChunk := []byte(`{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1786626000,"model":"gemma-4","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`)

	streamStart := time.Now().Add(-200 * time.Millisecond)
	result := translateOpenAISSEDataToOllama("/api/chat", usageChunk, "gemma-4", nil, streamStart, time.Time{})

	if result == nil {
		t.Fatal("expected non-nil result for usage chunk")
	}

	raw := strings.TrimRight(string(result), "\n")
	var ollama map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &ollama); err != nil {
		t.Fatalf("failed to parse Ollama chunk: %v", err)
	}

	totalDur, ok := ollama["total_duration"].(float64)
	if !ok {
		t.Fatalf("expected total_duration float64, got %T: %v", ollama["total_duration"], ollama["total_duration"])
	}

	// 200ms = ~200_000_000 ns. Допускаем [100ms, 500ms] для test latency.
	if totalDur < 100_000_000 || totalDur > 500_000_000 {
		t.Errorf("total_duration=%v ns, want ~200_000_000 (200ms ±)", totalDur)
	}

	// Sanity: other fields тоже должны быть
	if ollama["prompt_eval_count"].(float64) != 100 {
		t.Errorf("expected prompt_eval_count=100, got %v", ollama["prompt_eval_count"])
	}
	if ollama["eval_count"].(float64) != 50 {
		t.Errorf("expected eval_count=50, got %v", ollama["eval_count"])
	}
}

// TestTranslateUsageChunkToOllama_ZeroStreamStart — legacy callers (test mocks).
func TestTranslateUsageChunkToOllama_ZeroStreamStart(t *testing.T) {
	usageChunk := []byte(`{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1786626000,"model":"gemma-4","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`)

	result := translateOpenAISSEDataToOllama("/api/chat", usageChunk, "gemma-4", nil, time.Time{}, time.Time{})

	if result == nil {
		t.Fatal("expected non-nil result for usage chunk")
	}

	raw := strings.TrimRight(string(result), "\n")
	var ollama map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &ollama); err != nil {
		t.Fatalf("failed to parse Ollama chunk: %v", err)
	}

	// zero streamStart → all durations are 0
	for _, field := range []string{"total_duration", "load_duration", "prompt_eval_duration", "eval_duration"} {
		if v, _ := ollama[field].(float64); v != 0 {
			t.Errorf("expected %s=0 for zero streamStart, got %v", field, v)
		}
	}
}
