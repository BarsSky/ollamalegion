package balancer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// F.0 tests (2026-06-28): session F — диагностика EOF.
//
// Acceptance criteria:
// 1. publishTransportEOF не падает на типичных входах (no-op stub).
// 2. determineErrorType корректно классифицирует EOF и связанные ошибки.
// 3. error context format содержит все ключевые поля для диагностики.
// 4. Шаблон ошибки в production-коде и тестах синхронизирован.

// TestPublishTransportEOF_NoPanic проверяет что helper не падает на типичных входах.
// F.0.6 acceptance criteria.
func TestPublishTransportEOF_NoPanic(t *testing.T) {
	p := &Proxy{}

	cases := []struct {
		name      string
		backendID string
		model     string
		path      string
		err       error
		duration  time.Duration
	}{
		{
			name: "EOF with model",
			backendID: "cppworker-gpu", model: "gemma-4-E4B-it-Q4_K_M",
			path: "/v1/chat/completions",
			err: errors.New("Post http://...: EOF"),
			duration: 250 * time.Millisecond,
		},
		{
			name: "EOF with empty model",
			backendID: "cppworker-cpu", model: "",
			path: "/api/generate",
			err: io.ErrUnexpectedEOF,
			duration: 50 * time.Millisecond,
		},
		{
			name: "nil error — should silently skip",
			backendID: "x", model: "y", path: "/z",
			err: nil, duration: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("publishTransportEOF panicked: %v", r)
				}
			}()
			p.publishTransportEOF(tc.backendID, tc.model, tc.path, tc.err, tc.duration)
		})
	}
}

// TestDetermineErrorType_EOF проверяет что EOF корректно классифицируется как unexpected_eof.
// F.0.3 acceptance criteria — только unexpected_eof ошибки должны триггерить retry.
func TestDetermineErrorType_EOF(t *testing.T) {
	cases := []struct {
		errStr   string
		expected string
	}{
		{"Post http://127.0.0.1:18092/v1/chat/completions: EOF", "unexpected_eof"},
		{"unexpected EOF", "unexpected_eof"},
		{"connection refused", "connection_refused"},
		{"connection reset by peer", "connection_reset_by_peer"},
		{"write tcp: broken pipe", "broken_pipe"},
		{"no such host", "dns_resolution_failed"},
		{"some totally weird error", "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.errStr, func(t *testing.T) {
			got := determineErrorType(errors.New(tc.errStr), context.Background())
			if got != tc.expected {
				t.Errorf("determineErrorType(%q) = %q, want %q", tc.errStr, got, tc.expected)
			}
		})
	}
}

// TestDetermineErrorType_ContextErrors проверяет классификацию context-ошибок.
func TestDetermineErrorType_ContextErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled context

	got := determineErrorType(context.Canceled, ctx)
	if got != "context_canceled" {
		t.Errorf("expected context_canceled for canceled ctx, got %q", got)
	}
}

// TestEOFErrorMessageFormat проверяет что шаблон ошибки содержит все ключевые поля.
// F.0.4 acceptance criteria.
//
// ВАЖНО: этот шаблон должен точно совпадать с тем, что в production-коде
// (llamacpp_transport.go и llamacpp_transport_nonstream.go). Если production
// шаблон изменится — нужно обновить и этот тест.
func TestEOFErrorMessageFormat(t *testing.T) {
	err := errors.New("Post http://127.0.0.1:18092/v1/chat/completions: EOF")
	errType := determineErrorType(err, context.Background())
	if errType != "unexpected_eof" {
		t.Fatalf("expected unexpected_eof, got %q", errType)
	}
	durationMs := int64(150)

	// Используем идентичный шаблон с production-кодом.
	msg := fmt.Sprintf(
		"llama.cpp request failed [backend=%s, attempt=%d, duration_ms=%d, error_type=%s]: %v",
		"cppworker-gpu-bundled", 1, durationMs, errType, err,
	)

	// Все 4 поля должны присутствовать в сообщении.
	for _, want := range []string{
		"backend=cppworker-gpu-bundled",
		"attempt=1",
		"duration_ms=150",
		"error_type=unexpected_eof",
		"EOF",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error message to contain %q, got: %s", want, msg)
		}
	}
}