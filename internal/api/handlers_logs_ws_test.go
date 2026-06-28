package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// TestWSLogsHandler_BrokerNil — без инициализированного broker handler не должен
// паниковать. На httptest (без WS-headers) Upgrade вернёт ошибку, и handler
// тихо выйдет. Это OK для smoke-теста.
func TestWSLogsHandler_BrokerNil(t *testing.T) {
	logger.SetBroker(nil)

	s := &Server{} // authenticator == nil → auth check пропускается
	req := httptest.NewRequest(http.MethodGet, "/ws/logs", nil)
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	s.wsLogsHandler(w, req)
	// Ожидание: 0..N байт тела (httptest без Upgrade, ничего не пишется),
	// но НЕ 401 (т.к. authenticator == nil).
	if w.Code == http.StatusUnauthorized {
		t.Errorf("expected NOT 401 with nil authenticator, got %d", w.Code)
	}
}

// TestWSLogsHandler_NoPanicWithoutAuth — handler не должен падать, когда
// authenticator не инициализирован (== nil). Покрывает сценарий «auth выключен
// в config.json» (authenticator остаётся nil после NewServer).
func TestWSLogsHandler_NoPanicWithoutAuth(t *testing.T) {
	logger.SetBroker(nil)
	s := &Server{authenticator: nil}

	req := httptest.NewRequest(http.MethodGet, "/ws/logs", nil)
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic with nil authenticator: %v", r)
		}
	}()
	s.wsLogsHandler(w, req)
	// Upgrade в httptest без ws-headers вернёт ошибку — handler тихо вернётся.
	// Главное — нет panic и нет 401.
	if w.Code == http.StatusUnauthorized {
		t.Errorf("expected NOT 401 with nil authenticator, got %d (body=%s)", w.Code, w.Body.String())
	}
}

// TestWSLogsHandler_AuthTokenRejected — handler отклоняет запрос при наличии
// authenticator и отсутствии/невалидности токена. Используем NewTokenAuthenticator
// с фиктивным токеном, чтобы получить реальный *TokenAuthenticator в состоянии enabled.
func TestWSLogsHandler_AuthTokenRejected(t *testing.T) {
	logger.SetBroker(nil)

	// Создаём реальный TokenAuthenticator с токеном "secret123".
	auth := NewTokenAuthenticator([]string{"secret123"}, "X-API-Token", true)
	s := &Server{authenticator: auth}

	// Без токена — 401.
	req := httptest.NewRequest(http.MethodGet, "/ws/logs", nil)
	w := httptest.NewRecorder()
	s.wsLogsHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "token") {
		t.Errorf("expected error to mention 'token', got %q", w.Body.String())
	}

	// С невалидным токеном — 401.
	req2 := httptest.NewRequest(http.MethodGet, "/ws/logs?token=wrong", nil)
	w2 := httptest.NewRecorder()
	s.wsLogsHandler(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with invalid token, got %d", w2.Code)
	}

	// С валидным токеном — НЕ 401 (Upgrade может упасть, но это после auth).
	req3 := httptest.NewRequest(http.MethodGet, "/ws/logs?token=secret123", nil)
	w3 := httptest.NewRecorder()
	s.wsLogsHandler(w3, req3)
	if w3.Code == http.StatusUnauthorized {
		t.Errorf("expected NOT 401 with valid token, got %d", w3.Code)
	}
}

// TestLogEntry_JSONMarshaling_WS — формат JSON для WS совпадает с тем, что ждёт frontend.
func TestLogEntry_JSONMarshaling_WS(t *testing.T) {
	entry := logger.LogEntry{
		Time:    mustParseTimeWS(t, "2026-06-28T13:00:00Z"),
		Level:   "info",
		Message: "test from broker",
		Source:  "balancer",
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	for _, k := range []string{`"time":"2026-06-28T13:00:00Z"`, `"level":"info"`, `"message":"test from broker"`, `"source":"balancer"`} {
		if !strings.Contains(string(data), k) {
			t.Errorf("expected JSON to contain %s, got %s", k, string(data))
		}
	}
}

// mustParseTimeWS — helper для тестов.
func mustParseTimeWS(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tt
}