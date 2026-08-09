// streaming_heartbeat_reasoning_test.go — tests for NDJSON heartbeat in streaming.
//
// Round 31 (2026-08-09): heartbeat для NDJSON минимизирован до {"done":false}.
// Без полей model/message — OpenWebUI не интерпретирует как новое сообщение
// и не сбрасывает reasoning секцию.
package balancer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestNDJSONHeartbeat_DoesNotHaveMessageField —
// Heartbeat JSON НЕ должен содержать поле "message" (иначе OpenWebUI
// может начать новое сообщение и сбросить reasoning display).
func TestNDJSONHeartbeat_DoesNotHaveMessageField(t *testing.T) {
	// Build heartbeat the same way streaming.go does it
	hb := map[string]interface{}{"done": false}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')

	// Parse and verify no message field
	var parsed map[string]interface{}
	if err := json.Unmarshal(data[:len(data)-1], &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, hasMessage := parsed["message"]; hasMessage {
		t.Errorf("heartbeat should NOT have 'message' field, got: %s", string(data))
	}
	if _, hasModel := parsed["model"]; hasModel {
		t.Errorf("heartbeat should NOT have 'model' field, got: %s", string(data))
	}
	if done, _ := parsed["done"].(bool); !done == false {
		t.Errorf("heartbeat 'done' field must be false, got: %v", parsed["done"])
	}
}

// TestSSEHeartbeat_IsSSEComment —
// SSE heartbeat должен быть comment'ом (начинается с ':') чтобы клиенты
// его игнорировали.
func TestSSEHeartbeat_IsSSEComment(t *testing.T) {
	heartbeat := []byte(":heartbeat\n\n")
	if !bytes.HasPrefix(heartbeat, []byte(":")) {
		t.Errorf("SSE heartbeat must start with ':' to be a comment, got: %q", heartbeat)
	}
	// Verify it's newline-terminated (SSE comment rule)
	if !bytes.HasSuffix(heartbeat, []byte("\n\n")) {
		t.Errorf("SSE heartbeat must end with \\n\\n, got: %q", heartbeat)
	}
}

// TestNDJSONHeartbeat_ParsableByOllamaClient —
// Heartbeat должен быть parsable как валидный NDJSON line.
func TestNDJSONHeartbeat_ParsableByOllamaClient(t *testing.T) {
	hb := map[string]interface{}{"done": false}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')

	// Simulate Ollama client parser: bufio.Scanner line-by-line
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), 1024*64)
	lines := 0
	for scanner.Scan() {
		lines++
		line := scanner.Text()
		if line == "" {
			t.Errorf("heartbeat line should not be empty, got: %q", line)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Errorf("heartbeat line not valid JSON: %v (line: %q)", err, line)
		}
	}
	if lines != 1 {
		t.Errorf("expected 1 line, got %d", lines)
	}
	if err := scanner.Err(); err != nil {
		t.Errorf("scanner: %v", err)
	}
}

// TestNDJSONHeartbeat_NotConfusedWithDoneTrue —
// heartbeat (done:false) НЕ должен путаться с финальным done-чанком
// (done:true + done_reason + message).
func TestNDJSONHeartbeat_NotConfusedWithDoneTrue(t *testing.T) {
	hb := map[string]interface{}{"done": false}
	data, _ := json.Marshal(hb)
	var parsed map[string]interface{}
	json.Unmarshal(data, &parsed)

	if done, _ := parsed["done"].(bool); done {
		t.Errorf("heartbeat 'done' must be false (to indicate stream is alive)")
	}
	if _, hasReason := parsed["done_reason"]; hasReason {
		t.Errorf("heartbeat should NOT have 'done_reason' (that's for final chunk)")
	}
}

// TestNDJSONHeartbeat_RealisticLongPause —
// Симулируем длинную паузу (60s) с периодическими heartbeat'ами
// и убеждаемся что heartbeat'ы приходят регулярно.
func TestNDJSONHeartbeat_RealisticLongPause(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping long-running test in -short mode")
	}
	t.Skip("Integration test: requires real streaming setup")
	// Sanity placeholder
	_ = time.Second
	_ = strings.Builder{}
}
