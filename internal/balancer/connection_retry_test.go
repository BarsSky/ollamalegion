package balancer

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// Round 8 (2026-07-10): tests for connection-level error detection.

func TestIsConnectionLevelError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "connection_refused",
			err:  errors.New(`Post "http://cppworker-gpu:18092/v1/chat/completions": dial tcp 172.18.0.4:18092: connect: connection refused`),
			want: true,
		},
		{
			name: "connection_reset",
			err:  errors.New(`read tcp 172.18.0.4:18092: read: connection reset by peer`),
			want: true,
		},
		{
			name: "dns_resolution_failed",
			err:  errors.New(`dial tcp: lookup nonexistent.example.com: no such host`),
			want: true,
		},
		{
			name: "broken_pipe",
			err:  errors.New(`write tcp 127.0.0.1:18092: write: broken pipe`),
			want: true,
		},
		// HTTP-level errors should NOT be connection-level (handled elsewhere).
		{
			name: "EOF",
			err:  errors.New("unexpected EOF"),
			want: false,
		},
		{
			name: "timeout",
			err:  errors.New(`Post "http://cppworker:18092/v1/chat/completions": context deadline exceeded`),
			want: false,
		},
		{
			name: "context_canceled",
			err:  errors.New("context canceled"),
			want: false,
		},
		{
			name: "generic_http_error",
			err:  errors.New(`server returned 500: internal error`),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isConnectionLevelError(tc.err)
			if got != tc.want {
				t.Errorf("isConnectionLevelError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsConnectionLevelError_RealErrorsFromBalancer validates the function
// matches actual error strings observed in production logs (2026-07-09 case).
func TestIsConnectionLevelError_RealErrorsFromBalancer(t *testing.T) {
	realErrors := []string{
		`Post "http://cppworker-gpu:18092/v1/chat/completions": dial tcp 172.18.0.4:18092: connect: connection refused`,
		`Get "http://cppworker-gpu:18092/api/models": dial tcp 172.18.0.4:18092: connect: connection refused`,
	}
	for _, msg := range realErrors {
		t.Run(msg, func(t *testing.T) {
			if !isConnectionLevelError(errors.New(msg)) {
				t.Errorf("expected real connection_refused error to be detected as connection-level")
			}
		})
	}
}

// TestDetermineErrorType_Compatibility ensures determineErrorType returns the
// expected category for connection_refused (Round 8: separate from HTTP errors).
func TestDetermineErrorType_Compatibility(t *testing.T) {
	err := errors.New(`dial tcp: connection refused`)
	// Pass a real (non-nil) context — determineErrorType calls reqCtx.Err()
	// before checking the error string, nil context panics.
	got := determineErrorType(err, context.Background())
	if got != "connection_refused" {
		t.Errorf("determineErrorType(connection refused) = %q, want connection_refused", got)
	}
}

// TestIsConnectionLevelError_NetworkError verifies net.Error types are recognized
// (defensive check — currently we use string match, but should also handle typed errors).
func TestIsConnectionLevelError_NetworkError(t *testing.T) {
	netErr := &net.OpError{
		Op:  "dial",
		Err: errors.New("connection refused"),
	}
	if !isConnectionLevelError(netErr) {
		t.Errorf("expected net.OpError wrapping connection refused to be detected")
	}
	// Sanity: empty message
	if isConnectionLevelError(errors.New("")) {
		t.Errorf("empty error should not be detected as connection-level")
	}
	// Sanity: completely unrelated error
	if isConnectionLevelError(errors.New(strings.Repeat("x", 200))) {
		t.Errorf("arbitrary long error should not be detected as connection-level")
	}
}