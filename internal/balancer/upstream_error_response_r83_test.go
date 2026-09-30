//go:build llama_stub

// upstream_error_response_r83_test.go — R83-fix (2026-09-30).
//
// ЖИВОЙ РЕПОРТ (Cline): при недоступном cppworker клиент получал 502 с сырым
// транспортым текстом:
//
//	llama.cpp request failed [backend=..., attempt=2, duration_ms=1120,
//	error_type=connection_refused]: Post "http://cppworker-gpu:18092/api/chat":
//	dial tcp 172.23.0.5:18092: connect: connection refused
//
// Cline принимает это за переполнение КОНТЕКСТА: «context window exceeded —
// compacting and retrying» → сжимает историю → повтор → настоящий 413 preflight
// («prompt + n_predict exceeds n_ctx») → «conversation still exceeds the model's
// context window». Падение бэкенда маскировалось под проблему окна.
//
// Тесты фиксируют контракт: «узел недоступен» — это 503 + Retry-After с
// машиночитаемым телом, а не 502 с единственной строкой dial-ошибки.
package balancer

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestR83_ClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"connection refused",
			errors.New(`Post "http://cppworker-gpu:18092/api/chat": dial tcp 172.23.0.5:18092: connect: connection refused`),
			"connection_refused"},
		{"dns",
			errors.New(`Post "http://cppworker-gpu:18092/api/chat": dial tcp: lookup cppworker-gpu on 127.0.0.11:53: no such host`),
			"dns_error"},
		{"timeout", errors.New(`context deadline exceeded (Client.Timeout exceeded while awaiting headers)`), "timeout"},
		{"unreachable", errors.New(`dial tcp 10.0.0.5:18092: connect: network is unreachable`), "network_unreachable"},
		{"eof", errors.New(`unexpected EOF`), "unexpected_eof"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyUpstreamError(tc.err); got != tc.want {
				t.Errorf("classifyUpstreamError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestR83_WriteUpstreamError_ConnectionRefusedIsRetryable(t *testing.T) {
	w := httptest.NewRecorder()
	err := errors.New(`Post "http://cppworker-gpu:18092/api/chat": dial tcp 172.23.0.5:18092: connect: connection refused`)

	writeUpstreamError(w, "cppworker-gpu-bundled-agent", "gemma-4-E4B-it-Q4_K_M", err)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (клиент должен повторить, а не «сжимать контекст»)", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("нет заголовка Retry-After — клиенты не поймут, когда повторять")
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, w.Body.String())
	}
	if got, _ := body["error_type"].(string); got != "connection_refused" {
		t.Errorf("error_type = %q, want connection_refused", got)
	}
	if got, _ := body["backend_id"].(string); got != "cppworker-gpu-bundled-agent" {
		t.Errorf("backend_id = %q — оператор должен видеть, какой узел упал", got)
	}
	if _, ok := body["retry_after"]; !ok {
		t.Error("нет retry_after — клиент не знает, через сколько повторять")
	}
	// Текст ошибки обязан остаться в detail (диагностика), но не как
	// единственное содержимое ответа.
	msg, _ := body["error"].(string)
	if msg == "" || msg == err.Error() {
		t.Errorf("поле error = %q: ожидалось человеческое объяснение, а не сырой dial-текст", msg)
	}
	if detail, _ := body["detail"].(string); detail == "" {
		t.Error("сырой текст потерян — оператору он нужен для диагностики")
	}
}

func TestR83_WriteUpstreamError_TimeoutIs504(t *testing.T) {
	w := httptest.NewRecorder()
	writeUpstreamError(w, "b1", "m", errors.New("context deadline exceeded"))

	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504 для таймаута", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("для таймаута тоже нужен Retry-After")
	}
}
