package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTokenAuthenticatorValid - проверка валидного токена
func TestTokenAuthenticatorValid(t *testing.T) {
	t.Parallel()

	tokens := []string{"valid-token-1", "valid-token-2", "master-token"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	// Создаем запрос с валидным токеном
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Token", "valid-token-1")

	valid, token := authenticator.Authenticate(req)
	assert.True(t, valid, "Валидный токен должен быть принят")
	assert.Equal(t, "valid-token-1", token)
}

// TestTokenAuthenticatorInvalid - проверка невалидного токена
func TestTokenAuthenticatorInvalid(t *testing.T) {
	t.Parallel()

	tokens := []string{"valid-token-1", "valid-token-2"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	// Создаем запрос с невалидным токеном
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Token", "invalid-token")

	valid, token := authenticator.Authenticate(req)
	assert.False(t, valid, "Невалидный токен должен быть отклонен")
	assert.Equal(t, "", token)
}

// TestTokenAuthenticatorMissing - проверка отсутствия токена
func TestTokenAuthenticatorMissing(t *testing.T) {
	t.Parallel()

	tokens := []string{"valid-token-1"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	// Не устанавливаем токен

	valid, token := authenticator.Authenticate(req)
	assert.False(t, valid, "Отсутствие токена должно быть отклонено")
	assert.Equal(t, "", token)
}

// TestTokenAuthenticatorDisabled - проверка отключенной аутентификации
func TestTokenAuthenticatorDisabled(t *testing.T) {
	t.Parallel()

	tokens := []string{"valid-token-1"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", false)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	// Токен не установлен, но аутентификация отключена

	valid, token := authenticator.Authenticate(req)
	assert.True(t, valid, "При отключенной аутентификации запрос должен проходить")
	assert.Equal(t, "", token)
}

// TestTokenAuthenticatorMaster - проверка master токена
func TestTokenAuthenticatorMaster(t *testing.T) {
	t.Parallel()

	// Master token - первый в списке
	tokens := []string{"master-token", "user-token-1", "user-token-2"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	// Проверяем что master токен распознается
	assert.True(t, authenticator.IsMasterToken("master-token"))
	assert.False(t, authenticator.IsMasterToken("user-token-1"))
	assert.False(t, authenticator.IsMasterToken("invalid-token"))
}

// TestTokenAuthenticatorMasterEmpty - проверка master токена для пустого списка
func TestTokenAuthenticatorMasterEmpty(t *testing.T) {
	t.Parallel()

	tokens := []string{}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	assert.False(t, authenticator.IsMasterToken("any-token"))
}

// TestTokenGenerate - проверка генерации токена
func TestTokenGenerate(t *testing.T) {
	t.Parallel()

	// Генерируем токен стандартной длины (32 байта = 64 hex символа)
	token, err := GenerateToken(32)
	assert.NoError(t, err)
	assert.Len(t, token, 64) // 32 байта в hex = 64 символа

	// Проверяем что токен состоит из hex символов
	for _, c := range token {
		assert.Contains(t, "0123456789abcdef", string(c))
	}

	// Генерируем второй токен - должен отличаться
	token2, err := GenerateToken(32)
	assert.NoError(t, err)
	assert.NotEqual(t, token, token2)
}

// TestTokenGenerateDefaultLength - проверка токена с длиной по умолчанию
func TestTokenGenerateDefaultLength(t *testing.T) {
	t.Parallel()

	token, err := GenerateToken(0) // 0 должно использовать значение по умолчанию
	assert.NoError(t, err)
	assert.Len(t, token, 64) // 32 байта по умолчанию
}

// TestTokenGenerateNegativeLength - проверка токена с отрицательной длиной
func TestTokenGenerateNegativeLength(t *testing.T) {
	t.Parallel()

	token, err := GenerateToken(-5) // отрицательное значение
	assert.NoError(t, err)
	assert.Len(t, token, 64) // должно использовать значение по умолчанию
}

// TestTokenAddRemove - проверка добавления и удаления токенов
func TestTokenAddRemove(t *testing.T) {
	t.Parallel()

	tokens := []string{"initial-token"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	assert.Equal(t, 1, authenticator.GetTokenCount())

	// Добавляем новый токен
	authenticator.AddToken("new-token")
	assert.Equal(t, 2, authenticator.GetTokenCount())

	// Проверяем что новый токен валиден
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Token", "new-token")
	valid, _ := authenticator.Authenticate(req)
	assert.True(t, valid)

	// Удаляем токен
	removed := authenticator.RemoveToken("new-token")
	assert.True(t, removed)
	assert.Equal(t, 1, authenticator.GetTokenCount())

	// Проверяем что удаленный токен больше не валиден
	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req2.Header.Set("X-API-Token", "new-token")
	valid2, _ := authenticator.Authenticate(req2)
	assert.False(t, valid2)
}

// TestTokenRemoveMaster - проверка что нельзя удалить master токен
func TestTokenRemoveMaster(t *testing.T) {
	t.Parallel()

	tokens := []string{"master-token", "user-token"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	// Пытаемся удалить master токен (первый в списке)
	removed := authenticator.RemoveToken("master-token")
	assert.False(t, removed, "Master токен нельзя удалить")
	assert.Equal(t, 2, authenticator.GetTokenCount())
}

// TestAuthMiddleware - проверка middleware (200 vs 401)
func TestAuthMiddleware(t *testing.T) {
	t.Parallel()

	tokens := []string{"valid-token"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	wrappedHandler := AuthMiddleware(handler, authenticator)

	// Запрос с валидным токеном - должен пройти
	req1 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req1.Header.Set("X-API-Token", "valid-token")
	w1 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(w1, req1)

	assert.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "OK", w1.Body.String())
	assert.Equal(t, "valid-token", req1.Header.Get("X-Authenticated-Token"))

	// Запрос без токена - должен получить 401
	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	w2 := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(w2, req2)

	assert.Equal(t, http.StatusUnauthorized, w2.Code)
	assert.Contains(t, w2.Body.String(), "Unauthorized")
}

// TestAuthMiddlewareDisabled - проверка middleware с отключенной аутентификацией
func TestAuthMiddlewareDisabled(t *testing.T) {
	t.Parallel()

	tokens := []string{"valid-token"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", false)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	wrappedHandler := AuthMiddleware(handler, authenticator)

	// Запрос без токена должен пройти (аутентификация отключена)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestAuthStatusHandler - проверка handler статуса аутентификации
func TestAuthStatusHandler(t *testing.T) {
	t.Parallel()

	tokens := []string{"token1", "token2", "token3"}
	authenticator := NewTokenAuthenticator(tokens, "X-Custom-Header", true)

	handler := AuthStatusHandler(authenticator)

	req := httptest.NewRequest(http.MethodGet, "/auth/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"enabled":true`)
	assert.Contains(t, w.Body.String(), `"headerName":"X-Custom-Header"`)
	assert.Contains(t, w.Body.String(), `"tokenCount":3`)
}

// TestTokenManagementHandlerGenerate - проверка генерации токена через handler
func TestTokenManagementHandlerGenerate(t *testing.T) {
	t.Parallel()

	tokens := []string{"master-token"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	handler := TokenManagementHandler(authenticator)

	// POST запрос с master токеном для генерации нового
	req := httptest.NewRequest(http.MethodPost, "/tokens", nil)
	req.Header.Set("X-Authenticated-Token", "master-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Contains(t, w.Body.String(), `"success":true`)
	assert.Contains(t, w.Body.String(), `"token"`)
	assert.Greater(t, authenticator.GetTokenCount(), 1)
}

// TestTokenManagementHandlerForbidden - проверка запрета генерации без master токена
func TestTokenManagementHandlerForbidden(t *testing.T) {
	t.Parallel()

	tokens := []string{"master-token", "user-token"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	handler := TokenManagementHandler(authenticator)

	// POST запрос с user токеном (не master)
	req := httptest.NewRequest(http.MethodPost, "/tokens", nil)
	req.Header.Set("X-Authenticated-Token", "user-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "master token required")
}

// TestTokenManagementHandlerRevoke - проверка отзыва токена
func TestTokenManagementHandlerRevoke(t *testing.T) {
	t.Parallel()

	tokens := []string{"master-token", "token-to-revoke"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	handler := TokenManagementHandler(authenticator)

	// DELETE запрос для отзыва токена
	req := httptest.NewRequest(http.MethodDelete, "/tokens?token=token-to-revoke", nil)
	req.Header.Set("X-Authenticated-Token", "master-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"success":true`)
	assert.Equal(t, 1, authenticator.GetTokenCount())
}

// TestNewTokenAuthenticatorDefaultHeader - проверка заголовка по умолчанию
func TestNewTokenAuthenticatorDefaultHeader(t *testing.T) {
	t.Parallel()

	tokens := []string{"test-token"}
	authenticator := NewTokenAuthenticator(tokens, "", true)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Token", "test-token")

	valid, _ := authenticator.Authenticate(req)
	assert.True(t, valid)
}

// TestAuthMiddlewareInvalidToken - проверка middleware с невалидным токеном
func TestAuthMiddlewareInvalidToken(t *testing.T) {
	t.Parallel()

	tokens := []string{"valid-token"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrappedHandler := AuthMiddleware(handler, authenticator)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Token", "invalid-token")
	w := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
}

// TestAuthMiddlewareMethodNotAllowed - проверка метода не разрешен для status handler
func TestAuthStatusHandlerMethodNotAllowed(t *testing.T) {
	t.Parallel()

	tokens := []string{"token"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	handler := AuthStatusHandler(authenticator)

	req := httptest.NewRequest(http.MethodPost, "/auth/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// TestTokenManagementHandlerMethodNotAllowed - проверка неподдерживаемого метода
func TestTokenManagementHandlerMethodNotAllowed(t *testing.T) {
	t.Parallel()

	tokens := []string{"token"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	handler := TokenManagementHandler(authenticator)

	req := httptest.NewRequest(http.MethodPut, "/tokens", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// TestTokenAuthenticatorConcurrent - проверка потокобезопасности
func TestTokenAuthenticatorConcurrent(t *testing.T) {
	t.Parallel()

	tokens := []string{"token1", "token2", "token3"}
	authenticator := NewTokenAuthenticator(tokens, "X-API-Token", true)

	done := make(chan bool)
	results := make(chan bool, 100)

	// Запускаем 10 горутин для проверки аутентификации
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 10; j++ {
				req := httptest.NewRequest(http.MethodGet, "/test", nil)
				req.Header.Set("X-API-Token", "token1")
				valid, _ := authenticator.Authenticate(req)
				results <- valid
			}
			done <- true
		}()
	}

	// Ждем завершения
	for i := 0; i < 10; i++ {
		<-done
	}
	close(results)

	// Все проверки должны вернуть true
	for result := range results {
		assert.True(t, result)
	}
}

// TestTokenGenerateSpecialChars - проверка что токен не содержит специальных символов
func TestTokenGenerateSpecialChars(t *testing.T) {
	t.Parallel()

	token, err := GenerateToken(32)
	assert.NoError(t, err)

	// Токен должен содержать только hex символы
	assert.Regexp(t, "^[0-9a-f]+$", token)

	// Не должен содержать специальные символы
	assert.False(t, strings.ContainsAny(token, "!@#$%^&*()"))
}
