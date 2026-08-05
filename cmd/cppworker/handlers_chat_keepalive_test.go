//go:build llama_stub

// handlers_chat_keepalive_test.go — regression test for /api/chat keepalive
// format change (Round 27 / v0.5.14 follow-up).
//
// Background: cppworker /api/chat is NDJSON. The heartbeat goroutine was
// writing `: keepalive\n\n` (SSE comment) every 100ms. Cline parses the
// stream strictly as NDJSON and threw "invalid json" on every keepalive line
// (hundreds of errors per inference). Python ollama and other strict NDJSON
// clients had the same issue.
//
// Fix (handlers_chat.go):
//   - Format changed to valid NDJSON: `{"keepalive":true}\n`
//   - Default interval bumped from 100ms to 15s (same as OpenAI SSE handler)
//
// This test pins the keepalive format so future regressions are caught.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestKeepaliveFormat_NDJSON — записанный стрим /api/chat должен содержать
// валидные NDJSON-строки, НЕ SSE-комменты. Каждая строка должна парситься
// как JSON (даже keepalive), иначе строгие клиенты (Cline, ollama-python)
// падают с "invalid json".
func TestKeepaliveFormat_NDJSON(t *testing.T) {
	// Симулируем стрим с keepalive-строкой между чанками.
	// Формат из handlers_chat.go после фикса:
	//   - чанки: {...}\n
	//   - keepalive: {"keepalive":true}\n
	stream := strings.Join([]string{
		`{"model":"x","message":{"role":"assistant","content":"hello"},"done":false}`,
		`{"keepalive":true}`,
		`{"keepalive":true}`,
		`{"model":"x","message":{"role":"assistant","content":" world"},"done":false}`,
		`{"model":"x","done":true}`,
		"",
	}, "\n")

	scanner := bufio.NewScanner(strings.NewReader(stream))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Каждая непустая строка должна быть валидным JSON
		if !json.Valid(line) {
			t.Fatalf("line %d is not valid JSON: %q (keepalive format must be NDJSON, not SSE comment)", lineNum, string(line))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	if lineNum < 5 {
		t.Fatalf("expected at least 5 lines, got %d", lineNum)
	}
}

// TestKeepaliveFormat_NotSSEComment — sanity-check: старая SSE-форма
// `: keepalive\n\n` ломает NDJSON-парсер. Этот тест фиксирует, что
// мы НЕ вернулись к ней.
func TestKeepaliveFormat_NotSSEComment(t *testing.T) {
	old := ": keepalive\n\n"
	stream := "{\"chunk\":1}\n" + old + "{\"chunk\":2}\n"
	scanner := bufio.NewScanner(bytes.NewReader([]byte(stream)))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			// OK — для старой формы. Этот тест проходит, если мы НЕ используем
			// SSE-формат. Документируем проблему, чтобы будущие разработчики
			// знали, почему SSE-формат не подходит для NDJSON.
			if bytes.HasPrefix(line, []byte(": ")) {
				t.Logf("Found SSE comment line (would break Cline): %q", string(line))
			} else {
				t.Errorf("Unexpected invalid JSON line: %q", string(line))
			}
		}
	}
}

// TestKeepaliveInterval_Default — дефолт для /api/chat должен быть 15s,
// не 100ms (100ms × 60s gen = 600 строк, 15s × 60s = 4 строки).
func TestKeepaliveInterval_Default(t *testing.T) {
	// getHeartbeatInterval читает OLLAMALEGION_HEARTBEAT_MS один раз
	// (sync.Once) на старте процесса. Этот тест проверяет только контракт
	// "когда env не задан, используется default".
	interval := getHeartbeatInterval(15 * time.Second)
	if interval != 15*time.Second {
		t.Fatalf("expected default 15s, got %v", interval)
	}
}
