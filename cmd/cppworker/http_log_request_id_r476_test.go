//go:build llama_stub

// http_log_request_id_r476_test.go — R476 (2026-11-07): id балансера в каждой
// строке HTTP-запроса воркера.
//
// ЗАЧЕМ. Сводка /api/chat несла request_id, а обычная строка «HTTP request» —
// нет: путь /api/generate и любой другой запрос нельзя было привязать к ходу.
// Тест держит поле в обеих ветках (обычной и polling-строке).
package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/logger"
)

// TestR476_HTTPLogCarriesRequestID — строка запроса содержит id из заголовка.
func TestR476_HTTPLogCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.CaptureToBuffer(&buf)
	defer restore()

	h := loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	req.Header.Set("X-Request-Id", "rid-http-476")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, "HTTP request") {
		t.Fatalf("нет строки запроса: %s", out)
	}
	if !strings.Contains(out, "rid-http-476") {
		t.Fatalf("в строке запроса нет request_id: %s", out)
	}
}

// TestR476_PollingLogCarriesRequestID — короткие poll-запросы пишутся в Debug,
// но id нужен и там: иначе ход «найти нельзя» ровно у тех запросов, что чаще всего.
func TestR476_PollingLogCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.CaptureToBuffer(&buf) // CaptureTo включает Debug
	defer restore()

	h := loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for path := range pollingPaths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Request-Id", "rid-poll-476")
		h.ServeHTTP(httptest.NewRecorder(), req)
		break
	}

	out := buf.String()
	if !strings.Contains(out, "rid-poll-476") {
		t.Fatalf("в polling-строке нет request_id: %s", out)
	}
}
