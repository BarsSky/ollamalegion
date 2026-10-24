package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/types"
)

// newI18nServer — минимальный Server для проверки GET/PUT /api/v1/i18n.
func newI18nServer(t *testing.T) *Server {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 8080, APIPort: 8081},
	}
	p := balancer.NewProxy(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return NewServer(p, cfg, nil)
}

// TestI18nGetReportsLanguage — GET отдаёт действующий язык и список поддерживаемых.
func TestI18nGetReportsLanguage(t *testing.T) {
	s := newI18nServer(t)
	i18n.Set(i18n.RU)
	defer i18n.Set(i18n.RU)

	rec := httptest.NewRecorder()
	s.handleI18n(rec, httptest.NewRequest(http.MethodGet, "/api/v1/i18n", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var body struct {
		Language  string   `json:"language"`
		Resolved  string   `json:"resolved"`
		Supported []string `json:"supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if body.Language != "ru" || body.Resolved != "ru" {
		t.Fatalf("language=%q resolved=%q, want ru/ru", body.Language, body.Resolved)
	}
	if len(body.Supported) != 2 {
		t.Fatalf("supported=%v, want [ru en]", body.Supported)
	}
}

// TestI18nPutRequiresAdmin — viewer/аноним не может переключить язык стенда.
func TestI18nPutRequiresAdmin(t *testing.T) {
	s := newI18nServer(t)
	i18n.Set(i18n.RU)
	defer i18n.Set(i18n.RU)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/i18n",
		strings.NewReader(`{"language":"en"}`))
	// Без X-Authenticated-Token (его ставит AuthMiddleware) запрос не админский.
	rec := httptest.NewRecorder()
	s.handleI18n(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT without admin = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if i18n.Get() != i18n.RU {
		t.Fatalf("language changed by a non-admin request")
	}
}

// TestI18nPutSwitchesLanguageAndPersists — админ переключает язык, значение
// уходит в конфиг и сохраняется на диск.
func TestI18nPutSwitchesLanguageAndPersists(t *testing.T) {
	s := newI18nServer(t)
	i18n.Set(i18n.RU)
	defer i18n.Set(i18n.RU)
	saves := 0
	s.SetConfigSaver(func() error { saves++; return nil })

	req := httptest.NewRequest(http.MethodPut, "/api/v1/i18n",
		strings.NewReader(`{"language":"en"}`))
	req.Header.Set("X-Authenticated-Token", "token:break-glass")
	rec := httptest.NewRecorder()
	s.handleI18n(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := i18n.Get(); got != i18n.EN {
		t.Fatalf("language after PUT = %q, want en", got)
	}
	if s.config.Logging.Language != "en" {
		t.Fatalf("config.logging.language = %q, want en", s.config.Logging.Language)
	}
	if saves != 1 {
		t.Fatalf("config saver called %d times, want 1", saves)
	}
}

// TestI18nPutRejectsUnknownLanguage — мусор в языке не меняет настройку.
func TestI18nPutRejectsUnknownLanguage(t *testing.T) {
	s := newI18nServer(t)
	i18n.Set(i18n.RU)
	defer i18n.Set(i18n.RU)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/i18n",
		strings.NewReader(`{"language":"de"}`))
	req.Header.Set("X-Authenticated-Token", "token:break-glass")
	rec := httptest.NewRecorder()
	s.handleI18n(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT de = %d, want 400", rec.Code)
	}
	if i18n.Get() != i18n.RU {
		t.Fatalf("unsupported language changed the setting")
	}
	if !strings.Contains(rec.Body.String(), "ru") {
		t.Fatalf("error must list allowed values: %s", rec.Body.String())
	}
}

// TestI18nPutAutoUsesLocale — "auto" применяет язык из локали, но в конфиг
// сохраняется именно "auto".
func TestI18nPutAutoUsesLocale(t *testing.T) {
	s := newI18nServer(t)
	i18n.Set(i18n.RU)
	defer i18n.Set(i18n.RU)
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "en_GB.UTF-8")
	s.SetConfigSaver(func() error { return nil })

	req := httptest.NewRequest(http.MethodPut, "/api/v1/i18n",
		strings.NewReader(`{"language":"auto"}`))
	req.Header.Set("X-Authenticated-Token", "token:break-glass")
	rec := httptest.NewRecorder()
	s.handleI18n(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT auto = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := i18n.Get(); got != i18n.EN {
		t.Fatalf("LANG=en_GB must resolve to en, got %q", got)
	}
	if s.config.Logging.Language != "auto" {
		t.Fatalf("config must keep %q, got %q", "auto", s.config.Logging.Language)
	}
}
