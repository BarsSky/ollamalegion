package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// F.0b tests (2026-06-28): session F — defensive panic recovery middleware.
//
// Acceptance criteria:
// 1. Panic в handler'е до WriteHeader → JSON 500 + {"error":"internal_error", ...}.
// 2. Panic в handler'е после WriteHeader → логирование + truncated stream.
// 3. Без panic — middleware пропускает запрос без изменений.
// 4. middleware chain: panic в corsMiddleware тоже ловится.

// TestRecoverMiddleware_NoPanic — baseline: middleware пропускает нормальные запросы.
func TestRecoverMiddleware_NoPanic(t *testing.T) {
	handler := recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	r := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", w.Body.String())
	}
}

// TestRecoverMiddleware_PanicBeforeHeaders — главный тест F.0b:
// panic ДО WriteHeader → клиент получает JSON 500 вместо EOF.
func TestRecoverMiddleware_PanicBeforeHeaders(t *testing.T) {
	handler := recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Имитируем panic в обработчике (например, nil pointer dereference).
		var p *struct{ x int }
		_ = p.x // паника
	}))

	r := httptest.NewRequest("GET", "/api/test", nil)
	w := httptest.NewRecorder()

	// Recover не должен прокидывать panic наружу.
	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("recoverMiddleware should have caught panic, but it escaped: %v", rec)
		}
	}()

	handler.ServeHTTP(w, r)

	// Проверяем что клиент получил JSON 500, а не EOF.
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected Content-Type=application/json, got %q", ct)
	}

	// Проверяем структуру JSON.
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v, body=%q", err, w.Body.String())
	}
	if body["error"] != "internal_error" {
		t.Errorf("expected error=internal_error, got %v", body["error"])
	}
	if msg, ok := body["message"].(string); !ok || !strings.Contains(msg, "panic") {
		t.Errorf("expected message to contain 'panic', got %v", body["message"])
	}
}

// TestRecoverMiddleware_PanicAfterHeaders — panic после WriteHeader.
// В этом случае мы не можем изменить HTTP status, но логируем truncation.
func TestRecoverMiddleware_PanicAfterHeaders(t *testing.T) {
	handler := recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: partial\n\n"))
		// Паника после headers — клиент увидит truncated stream.
		panic("stream panic")
	}))

	r := httptest.NewRequest("GET", "/api/stream", nil)
	w := httptest.NewRecorder()

	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("recoverMiddleware should have caught panic, but it escaped: %v", rec)
		}
	}()

	handler.ServeHTTP(w, r)

	// Status остаётся 200 (мы не можем его изменить после WriteHeader).
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	// Partial stream должен быть записан до panic.
	if !strings.Contains(w.Body.String(), "data: partial") {
		t.Errorf("expected partial stream data before panic, got body=%q", w.Body.String())
	}
}

// TestRecoverResponseWriter_WriteHeaderSetsFlag — проверка helper'а.
func TestRecoverResponseWriter_WriteHeaderSetsFlag(t *testing.T) {
	inner := httptest.NewRecorder()
	rw := &recoverResponseWriter{ResponseWriter: inner}

	if rw.headersWritten {
		t.Errorf("headersWritten should be false initially")
	}
	rw.WriteHeader(http.StatusCreated)
	if !rw.headersWritten {
		t.Errorf("headersWritten should be true after WriteHeader")
	}
	if !rw.wroteHeader {
		t.Errorf("wroteHeader should be true after WriteHeader")
	}
	if inner.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", inner.Code)
	}
}

// TestRecoverResponseWriter_WriteAutoSendsHeader — поведение по умолчанию http.ResponseWriter:
// первый Write без WriteHeader вызывает 200 OK.
func TestRecoverResponseWriter_WriteAutoSendsHeader(t *testing.T) {
	inner := httptest.NewRecorder()
	rw := &recoverResponseWriter{ResponseWriter: inner}

	_, _ = rw.Write([]byte("hello"))
	if !rw.headersWritten {
		t.Errorf("headersWritten should be true after Write")
	}
	if inner.Code != http.StatusOK {
		t.Errorf("expected status 200 (auto-send), got %d", inner.Code)
	}
	if inner.Body.String() != "hello" {
		t.Errorf("expected body 'hello', got %q", inner.Body.String())
	}
}