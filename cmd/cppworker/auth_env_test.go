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

	// Session 17 P.3 (2026-07-27): WebUI → balancer → cppworker прокси-цепочка.
	// WebUI ставит X-API-Token (см. webui/js/modules/gguf-api.js:787), balancer
	// проксирует его через copyProxyHeaders (internal/api/gguf_backend_proxy.go:287-305).
	// До фикса cppworker понимал только Authorization: Bearer → 401 на per-backend save.
	t.Run("X-API-Token + correct token → 200 OK (Session 17 P.3 fix)", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
		os.Setenv("API_TOKEN", "secret_bundled")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("PUT", "/api/v1/cppworker/config/update", nil)
		req.Header.Set("X-API-Token", "secret_bundled")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 OK with X-API-Token, got %d (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("X-API-Token + wrong token → 401", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
		os.Setenv("API_TOKEN", "secret_bundled")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("PUT", "/api/v1/cppworker/config/update", nil)
		req.Header.Set("X-API-Token", "wrong_token")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 with wrong X-API-Token, got %d", rec.Code)
		}
	})

	t.Run("Authorization: Bearer wins over X-API-Token (priority)", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
		os.Setenv("API_TOKEN", "secret_bundled")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("PUT", "/api/v1/cppworker/config/update", nil)
		// Authorization правильный, X-API-Token неправильный — должно пройти (Bearer wins).
		req.Header.Set("Authorization", "Bearer secret_bundled")
		req.Header.Set("X-API-Token", "wrong_token")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 OK (Authorization takes priority over X-API-Token), got %d", rec.Code)
		}
	})

	t.Run("Authorization without Bearer prefix + correct X-API-Token → 200 OK", func(t *testing.T) {
		// Сценарий: клиент прислал "Token xyz" вместо "Bearer xyz" в Authorization,
		// но X-API-Token корректный — должно пройти.
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
		os.Setenv("API_TOKEN", "secret_bundled")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("PUT", "/api/v1/cppworker/config/update", nil)
		req.Header.Set("Authorization", "Token secret_bundled") // не "Bearer "
		req.Header.Set("X-API-Token", "secret_bundled")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 OK (X-API-Token fallback), got %d (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty X-API-Token header + no Authorization → 401", func(t *testing.T) {
		os.Unsetenv("API_TOKEN")
		os.Unsetenv("CPPWORKER_API_TOKEN")
		os.Unsetenv("BALANCER_API_TOKEN")
		os.Setenv("API_TOKEN", "secret_bundled")

		wrapped := authMiddleware(okHandler)
		req := httptest.NewRequest("PUT", "/api/v1/cppworker/config/update", nil)
		req.Header.Set("X-API-Token", "") // пустой header
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 with empty X-API-Token and no Authorization, got %d", rec.Code)
		}
	})
}

// TestExtractClientToken — тесты для вспомогательной функции извлечения токена.
// Покрывает оба пути (Authorization: Bearer, X-API-Token) и приоритет.
func TestExtractClientToken(t *testing.T) {
	t.Run("Authorization: Bearer (canonical)", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/x", nil)
		req.Header.Set("Authorization", "Bearer abc123")
		if got := extractClientToken(req); got != "abc123" {
			t.Errorf("got %q, want abc123", got)
		}
	})

	t.Run("X-API-Token only (no Authorization)", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/x", nil)
		req.Header.Set("X-API-Token", "xyz789")
		if got := extractClientToken(req); got != "xyz789" {
			t.Errorf("got %q, want xyz789", got)
		}
	})

	t.Run("both: Authorization wins", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/x", nil)
		req.Header.Set("Authorization", "Bearer primary")
		req.Header.Set("X-API-Token", "secondary")
		if got := extractClientToken(req); got != "primary" {
			t.Errorf("got %q, want primary (Authorization priority)", got)
		}
	})

	t.Run("Authorization without Bearer prefix → falls back to X-API-Token", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/x", nil)
		req.Header.Set("Authorization", "Token primary")
		req.Header.Set("X-API-Token", "fallback")
		if got := extractClientToken(req); got != "fallback" {
			t.Errorf("got %q, want fallback (X-API-Token used when Authorization has wrong prefix)", got)
		}
	})

	t.Run("neither set → empty", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/x", nil)
		if got := extractClientToken(req); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("Bearer with empty token → falls back to X-API-Token", func(t *testing.T) {
		// Edge case: "Bearer " (с пробелом, без токена) — TrimSpace даёт "".
		req := httptest.NewRequest("PUT", "/x", nil)
		req.Header.Set("Authorization", "Bearer ")
		req.Header.Set("X-API-Token", "real_token")
		if got := extractClientToken(req); got != "real_token" {
			t.Errorf("got %q, want real_token (empty Bearer falls back to X-API-Token)", got)
		}
	})
}