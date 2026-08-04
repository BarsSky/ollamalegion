package main

// ============================================================
// Tests for Round 25 (2026-08-04) SSE load progress
// ============================================================

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHandleLoadProgressStream_MethodNotAllowed — POST вместо GET → 405.
func TestHandleLoadProgressStream_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/models/load/progress/stream", nil)
	w := httptest.NewRecorder()

	handleLoadProgressStream(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestHandleLoadProgressStream_Headers — проверяем SSE-specific headers.
func TestHandleLoadProgressStream_Headers(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet,
		"/api/models/load/progress/stream?model=not-exists", nil)
	w := httptest.NewRecorder()

	handleLoadProgressStream(w, req)

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type: got %q, want text/event-stream", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control: got %q, want no-cache", cc)
	}
	if conn := w.Header().Get("Connection"); conn != "keep-alive" {
		t.Errorf("Connection: got %q, want keep-alive", conn)
	}
	if xb := w.Header().Get("X-Accel-Buffering"); xb != "no" {
		t.Errorf("X-Accel-Buffering: got %q, want no", xb)
	}
}

// TestWriteSSEEvent — sanity check на формат SSE event'а.
func TestWriteSSEEvent(t *testing.T) {
	w := httptest.NewRecorder()
	flusher := w // httptest.ResponseRecorder implements http.Flusher

	ok := writeSSEEvent(w, flusher, map[string]interface{}{
		"name": "test", "state": "loading", "elapsedMs": 1000,
	})
	if !ok {
		t.Errorf("expected true (no error)")
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("expected SSE data: prefix, got: %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("expected SSE event terminator \\n\\n, got: %q", body)
	}
	// Parse the JSON after "data: ".
	jsonStr := strings.TrimPrefix(body, "data: ")
	jsonStr = strings.TrimSuffix(jsonStr, "\n\n")
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		t.Fatalf("invalid JSON in SSE event: %v (body: %q)", err, body)
	}
	if parsed["name"] != "test" {
		t.Errorf("name: got %v, want test", parsed["name"])
	}
	if parsed["state"] != "loading" {
		t.Errorf("state: got %v, want loading", parsed["state"])
	}
	if e, _ := parsed["elapsedMs"].(float64); int64(e) != 1000 {
		t.Errorf("elapsedMs: got %v, want 1000", parsed["elapsedMs"])
	}
}

// TestWriteSSEEvent_MultipleEvents — два event'а подряд, корректный формат.
func TestWriteSSEEvent_MultipleEvents(t *testing.T) {
	w := httptest.NewRecorder()
	flusher := w
	writeSSEEvent(w, flusher, map[string]interface{}{"name": "a", "state": "loading"})
	writeSSEEvent(w, flusher, map[string]interface{}{"name": "b", "state": "loaded"})

	body := w.Body.String()
	if strings.Count(body, "data: ") != 2 {
		t.Errorf("expected 2 events, got body: %q", body)
	}
	// SSE stream: каждое event оканчивается \n\n.
	if strings.Count(body, "\n\n") < 2 {
		t.Errorf("expected >=2 terminators, got body: %q", body)
	}
}

// TestWriteSSEEvent_Scanner — проверяет что EventSource parser
// может прочитать наш output (стандартный SSE parser делает split по \n\n).
func TestWriteSSEEvent_Scanner(t *testing.T) {
	w := httptest.NewRecorder()
	flusher := w
	for i := 0; i < 3; i++ {
		writeSSEEvent(w, flusher, map[string]interface{}{
			"n": i, "state": "loading",
		})
	}

	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		// Split on \n\n
		for i := 0; i < len(data)-1; i++ {
			if data[i] == '\n' && data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	count := 0
	for scanner.Scan() {
		token := scanner.Text()
		if !strings.HasPrefix(token, "data: ") {
			t.Errorf("event %d: expected data: prefix, got: %q", count, token)
		}
		count++
	}
	if count != 3 {
		t.Errorf("expected 3 events from scanner, got %d", count)
	}
}

// TestSSEUpdateInterval_Reasonable — sanity check на интервалы.
func TestSSEUpdateInterval_Reasonable(t *testing.T) {
	if sseUpdateInterval > 2*time.Second {
		t.Errorf("sseUpdateInterval too high: %v (want <= 2s for real-time feel)", sseUpdateInterval)
	}
	if sseUpdateInterval < 100*time.Millisecond {
		t.Errorf("sseUpdateInterval too low: %v (want >= 100ms to avoid CPU waste)", sseUpdateInterval)
	}
	if sseHeartbeatInterval < 5*time.Second {
		t.Errorf("sseHeartbeatInterval too low: %v (want >= 5s)", sseHeartbeatInterval)
	}
	if sseHeartbeatInterval > 60*time.Second {
		t.Errorf("sseHeartbeatInterval too high: %v (want <= 60s for proxy compat)", sseHeartbeatInterval)
	}
}
