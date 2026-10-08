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

// TestR83Fix_ClassifyUpstreamError_GenericTimeoutDoesNotPanic — регрессия на
// падение всего балансера.
//
// classifyUpstreamError звал determineErrorType(err, nil), а тот безусловно
// обращался к reqCtx.Err(): на сообщении, не попавшем в ранние case'ы
// («i/o timeout» без слов Client.Timeout/context deadline exceeded), это
// `invalid memory address or nil pointer dereference` и паника всего процесса.
// Живой сценарий — таймаут первого байта от cppworker; воспроизводился тестом
// tests/first_byte_timeout_test.go (TestOpenAIChat_HeaderTimeout_StillWorks),
// который до фикса валил пакет tests с goroutine dump.
func TestR83Fix_ClassifyUpstreamError_GenericTimeoutDoesNotPanic(t *testing.T) {
	for _, msg := range []string{
		"net/http: timeout awaiting response headers",
		"i/o timeout",
		"read tcp 172.23.0.1:5555->172.23.0.5:18092: i/o timeout",
	} {
		if got := classifyUpstreamError(errors.New(msg)); got == "" {
			t.Errorf("classifyUpstreamError(%q) = %q, ожидался непустой тип", msg, got)
		}
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

// ============================================================
// R88 (2026-10-08): image-плоскость обязана объяснять транспортные ошибки так
// же, как текстовая. До фикса image-роутер отдавал 502 с сырым
// «image backend request failed: Post ...: dial tcp ...: connect: connection
// refused» — без error_type, без retry_after и без единого слова «что делать».
// ============================================================

// TestR88_UpstreamErrorStatus_Mapping — один источник правды для статуса:
// и тело ответа, и метрики image-плоскости берут его отсюда.
func TestR88_UpstreamErrorStatus_Mapping(t *testing.T) {
	cases := []struct {
		errType string
		want    int
	}{
		{"connection_refused", http.StatusServiceUnavailable},
		{"network_unreachable", http.StatusServiceUnavailable},
		{"dns_error", http.StatusServiceUnavailable},
		{"timeout", http.StatusGatewayTimeout},
		{"context_deadline", http.StatusGatewayTimeout},
		{"unexpected_eof", http.StatusBadGateway},
		{"upstream_error", http.StatusBadGateway},
		{"", http.StatusBadGateway},
	}
	for _, c := range cases {
		if got := UpstreamErrorStatus(c.errType); got != c.want {
			t.Errorf("UpstreamErrorStatus(%q) = %d, want %d", c.errType, got, c.want)
		}
	}
}

// TestR88_UpstreamErrorType_FromRealTransportText — классификация по живому
// тексту ошибок (ровно такие строки приходят из net/http).
func TestR88_UpstreamErrorType_FromRealTransportText(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{`Post "http://imageworker:18093/v1/images/generations": dial tcp 172.23.0.5:18093: connect: connection refused`, "connection_refused"},
		{`Get "http://imageworker:18093/api/image/models": dial tcp: lookup imageworker on 127.0.0.11:53: no such host`, "dns_error"},
		{"context deadline exceeded", "timeout"},
		{"Client.Timeout exceeded while awaiting headers", "timeout"},
		{"unexpected EOF", "unexpected_eof"},
	}
	for _, c := range cases {
		if got := UpstreamErrorType(errors.New(c.msg)); got != c.want {
			t.Errorf("UpstreamErrorType(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}

// TestR88_WriteUpstreamError_AlwaysExplains — у КАЖДОГО класса ошибки есть hint.
//
// Требование оператора дословно: «отсутствие таймаутов и явное объяснение
// ошибок». Hint — это и есть объяснение: что случилось и что делать.
func TestR88_WriteUpstreamError_AlwaysExplains(t *testing.T) {
	for _, err := range []error{
		errors.New("connect: connection refused"),
		errors.New("context deadline exceeded"),
		errors.New("что-то невиданное"),
	} {
		w := httptest.NewRecorder()
		writeUpstreamError(w, "imageworker", "qwen-image-2.1-uncensored-gguf", err)

		var body map[string]interface{}
		if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
			t.Fatalf("тело не JSON: %v (%s)", e, w.Body.String())
		}
		hint, _ := body["hint"].(string)
		if hint == "" {
			t.Errorf("для ошибки %q нет hint — оператор снова увидит «HTTP %d» без объяснения", err, w.Code)
		}
		if got, _ := body["error_type"].(string); got == "" {
			t.Errorf("для ошибки %q нет error_type", err)
		}
		if got, _ := body["backend_id"].(string); got != "imageworker" {
			t.Errorf("backend_id = %q, want imageworker", got)
		}
		if got, _ := body["model"].(string); got != "qwen-image-2.1-uncensored-gguf" {
			t.Errorf("model = %q — клиент должен видеть, о какой модели речь", got)
		}
	}
}
