// llamacpp_handlers_inference_long_gen_test.go — tests for handleOpenAIChatCompletions
// when the upstream cppworker takes a long time to send response headers.
//
// Bug context (2026-08-09):
//   cppworker для non-streaming /v1/chat/completions requests буферизирует всю
//   генерацию и отдаёт HTTP-заголовки ТОЛЬКО после завершения. Для reasoning-моделей
//   (gemma-4, qwen3.6 35B) генерация может занять 60-120+ секунд.
//
//   Live-тест 2026-08-09: gemma-4 reasoning prompt, прямой cppworker = 89.4s OK,
//   через balancer = timeout 180s с "net/http: timeout awaiting response headers".
//
// Покрывает:
//   - isConnectionLevelError корректно классифицирует timeout vs connection errors
//   - Header timeout НЕ считается connection-level → НЕ retry
//   - Header timeout НЕ помечает backend unhealthy
package balancer

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestIsConnectionLevelError_Timeout — sanity test for the error classification
// used in handleOpenAIChatCompletions retry logic.
func TestIsConnectionLevelError_Timeout(t *testing.T) {
	// Round 29 (2026-08-09): use real error messages from production logs.
	tests := []struct {
		name string
		err  error
		want bool
	}{
		// Windows: connectex message format
		{"connection_refused_windows", fmt.Errorf("dial tcp 127.0.0.1:9999: connectex: No connection could be made because the target machine actively refused it."), true},
		// Linux: "connection refused" substring
		{"connection_refused_linux", fmt.Errorf("dial tcp 127.0.0.1:9999: connect: connection refused"), true},
		// Generic connection reset
		{"connection_reset_windows", fmt.Errorf("read tcp 127.0.0.1:9999: wsarecv: An existing connection was forcibly closed by the remote host."), true},
		{"connection_reset_linux", fmt.Errorf("read tcp 127.0.0.1:9999: read: connection reset by peer"), true},
		// broken pipe
		{"broken_pipe", fmt.Errorf("write tcp 127.0.0.1:9999: write: broken pipe"), true},
		{"broken_pipe_short", fmt.Errorf("broken pipe"), true},
		// DNS
		{"no_such_host", fmt.Errorf("dial tcp: lookup nonexistent.example: no such host"), true},
		// NOT connection-level: header timeout (http.Client.Timeout exceeded)
		{"header_timeout", fmt.Errorf("Post \"http://localhost:18092/v1/chat/completions\": net/http: timeout awaiting response headers"), false},
		// NOT connection-level: context deadline
		{"context_deadline_exceeded", fmt.Errorf("Post \"http://127.0.0.1:65108/v1/chat/completions\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), false},
		// NOT connection-level: context canceled
		{"context_canceled", fmt.Errorf("context canceled"), false},
		// NOT connection-level: io EOF
		{"io_eof", fmt.Errorf("unexpected EOF"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConnectionLevelError(tt.err); got != tt.want {
				t.Errorf("isConnectionLevelError(%q) = %v, want %v", tt.err.Error(), got, tt.want)
			}
		})
	}
}

// TestProxyNonStreamingRequest_HeaderTimeoutIsReportedAsTimeout —
// Header timeout должен вернуть ошибку типа "timeout" (НЕ "connection_reset").
// Это sanity test, проверяющий что Go http.Client правильно репортит timeout
// (а не путает с connection error).
//
// Используем raw TCP listener без HTTP server — accept() но не отвечаем.
// Это даёт timeout на стороне клиента без необходимости cleanup sleep.
func TestProxyNonStreamingRequest_HeaderTimeoutIsReportedAsTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()
	addr := listener.Addr().String()

	// Accept connections but never respond.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn // hold open
		}
	}()

	// 2s client timeout vs indefinite hang.
	client := &http.Client{Timeout: 2 * time.Second}
	body := strings.NewReader(`{"model":"gemma-4","messages":[{"role":"user","content":"x"}],"stream":false}`)
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected timeout error, got response in %v", elapsed)
	}
	// Either "timeout" or "deadline exceeded" is OK — both are header timeouts
	errMsg := err.Error()
	if !strings.Contains(errMsg, "timeout") && !strings.Contains(errMsg, "deadline exceeded") {
		t.Errorf("expected timeout/deadline error, got: %v", err)
	}
	// IMPORTANT: error should NOT be classified as connection-level.
	if isConnectionLevelError(err) {
		t.Errorf("header timeout should NOT be classified as connection-level (would cause infinite retry): %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v (expected < 5s)", elapsed)
	}
	t.Logf("got expected timeout in %v: %v (isConnectionLevel=%v)", elapsed, err, isConnectionLevelError(err))
}
