// Тесты для фикса №2 (2026-06-26): поддержка нескольких env-имён для API-токена
// в authMiddleware (cmd/cppworker/utils.go).
//
// Корневая причина бага из env_log.txt: cppworker получал HTTP 401
// "invalid or missing API token" на /api/models/reload от балансировщика,
// потому что cppworker читал ТОЛЬКО env API_TOKEN, а bundled compose передавал
// CPPWORKER_API_TOKEN. После фикса authMiddleware резолвит токен из
// любой из трёх env-переменных (API_TOKEN, CPPWORKER_API_TOKEN, BALANCER_API_TOKEN).
//
// Тесты проверяют:
//   1. resolveAPIToken возвращает первый непустой токен по приоритету
//   2. authMiddleware пропускает запрос, если токен в env не задан
//   3. authMiddleware отвергает запрос с неверным Bearer-токеном
//   4. authMiddleware принимает запрос с правильным Bearer-токеном (через CPPWORKER_API_TOKEN)
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestResolveAPIToken_Priority(t *testing.T) {
	// Backup all relevant env vars to restore after test
	backup := map[string]string{
		"API_TOKEN":           os.Getenv("API_TOKEN"),
		"CPPWORKER_API_TOKEN": os.Getenv("CPPWORKER_API_TOKEN"),
		"BALANCER_API_TOKEN":  os.Getenv("BALANCER_API_TOKEN"),
	}
	defer func() {
		for k, v := range backup {
			if v == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	}()

	t.Run("all empty → returns empty", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
		if got := resolveAPIToken(); got != "" {
			t.Errorf("resolveAPIToken() = %q, want empty", got)
		}
	})

	t.Run("only API_TOKEN set → returns API_TOKEN", func(t *testing.T) {
		os.Setenv("API_TOKEN", "tok_api")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
		if got := resolveAPIToken(); got != "tok_api" {
			t.Errorf("resolveAPIToken() = %q, want tok_api", got)
		}
	})

	t.Run("API_TOKEN empty, CPPWORKER_API_TOKEN set → returns CPPWORKER_API_TOKEN", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Setenv("CPPWORKER_API_TOKEN", "tok_cpp")
		os.Unsetenv("BALANCER_API_TOKEN")
		if got := resolveAPIToken(); got != "tok_cpp" {
			t.Errorf("resolveAPIToken() = %q, want tok_cpp", got)
		}
	})

	t.Run("API_TOKEN wins over CPPWORKER_API_TOKEN (priority)", func(t *testing.T) {
		os.Setenv("API_TOKEN", "tok_api")
		os.Setenv("CPPWORKER_API_TOKEN", "tok_cpp")
		os.Setenv("BALANCER_API_TOKEN", "tok_bal")
		if got := resolveAPIToken(); got != "tok_api" {
			t.Errorf("resolveAPIToken() = %q, want tok_api (priority)", got)
		}
	})

	t.Run("BALANCER_API_TOKEN used as fallback when others empty", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Setenv("BALANCER_API_TOKEN", "tok_bal")
		if got := resolveAPIToken(); got != "tok_bal" {
			t.Errorf("resolveAPIToken() = %q, want tok_bal (fallback)", got)
		}
	})
}

func TestAuthMiddleware_TokenValidation(t *testing.T) {
	// Backup envs
	backupAPI := os.Getenv("API_TOKEN")
	backupCPP := os.Getenv("CPPWORKER_API_TOKEN")
	backupBAL := os.Getenv("BALANCER_API_TOKEN")
	defer func() {
		os.Setenv("API_TOKEN", backupAPI)
		os.Setenv("CPPWORKER_API_TOKEN", backupCPP)
		os.Setenv("BALANCER_API_TOKEN", backupBAL)
	}()

	// Inner handler that writes 200 OK
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	t.Run("no token in env → request passes (legacy open mode)", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("POST", "/api/models/reload", nil)
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 (no token in env = open), got %d", rec.Code)
		}
	})

	t.Run("token via CPPWORKER_API_TOKEN + correct Bearer → 200 OK", func(t *testing.T) {
		// Главный сценарий из env_log.txt — bundled compose шлёт CPPWORKER_API_TOKEN.
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
		os.Setenv("CPPWORKER_API_TOKEN", "secret_bundled")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("POST", "/api/models/reload", nil)
		req.Header.Set("Authorization", "Bearer secret_bundled")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 OK with correct Bearer token, got %d (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("token via CPPWORKER_API_TOKEN + wrong Bearer → 401", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
		os.Setenv("CPPWORKER_API_TOKEN", "secret_bundled")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("POST", "/api/models/reload", nil)
		req.Header.Set("Authorization", "Bearer wrong_token")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 with wrong Bearer, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "invalid or missing API token") {
			t.Errorf("expected error message in body, got: %s", rec.Body.String())
		}
	})

	t.Run("token via CPPWORKER_API_TOKEN + no Authorization header → 401", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
		os.Setenv("CPPWORKER_API_TOKEN", "secret_bundled")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("POST", "/api/models/reload", nil)
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 without Authorization header, got %d", rec.Code)
		}
	})

	t.Run("token via API_TOKEN + correct Bearer → 200 OK (legacy)", func(t *testing.T) {
		// Старая env (API_TOKEN) тоже должна работать — backward compat.
		os.Setenv("API_TOKEN", "legacy_token")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("POST", "/api/models/reload", nil)
		req.Header.Set("Authorization", "Bearer legacy_token")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 OK with API_TOKEN Bearer, got %d", rec.Code)
		}
	})

	t.Run("token via BALANCER_API_TOKEN + correct Bearer → 200 OK (fallback)", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Setenv("BALANCER_API_TOKEN", "bal_token")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("POST", "/api/models/reload", nil)
		req.Header.Set("Authorization", "Bearer bal_token")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 OK with BALANCER_API_TOKEN Bearer, got %d", rec.Code)
		}
	})
}