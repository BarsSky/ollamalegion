package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
)

// TokenAuthenticator - аутентификатор на основе API токенов
type TokenAuthenticator struct {
	tokens     map[string]bool
	headerName string
	enabled    bool
	mu         sync.RWMutex
}

// NewTokenAuthenticator - создание нового аутентификатора
// tokens - список валидных токенов
// headerName - имя заголовка для токена (по умолчанию "X-API-Token")
// enabled - флаг включения аутентификации
func NewTokenAuthenticator(tokens []string, headerName string, enabled bool) *TokenAuthenticator {
	tokenMap := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		tokenMap[token] = true
	}

	if headerName == "" {
		headerName = "X-API-Token"
	}

	return &TokenAuthenticator{
		tokens:     tokenMap,
		headerName: headerName,
		enabled:    enabled,
	}
}

// Authenticate - проверка токена в запросе
// Возвращает true и токен при успехе, false и пустую строку при неудаче
func (a *TokenAuthenticator) Authenticate(r *http.Request) (bool, string) {
	if !a.enabled {
		return true, "" // Аутентификация отключена
	}

	token := r.Header.Get(a.headerName)
	if token == "" {
		return false, ""
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.tokens[token] {
		return true, token
	}

	return false, ""
}

// IsMasterToken - проверка, является ли токен master токеном
// Master token - первый токен в списке
func (a *TokenAuthenticator) IsMasterToken(token string) bool {
	if !a.enabled || token == "" {
		return false
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	// Master token - первый в списке
	if len(a.tokens) == 0 {
		return false
	}

	// Получаем первый токен из map
	var masterToken string
	for t := range a.tokens {
		if masterToken == "" || t < masterToken {
			masterToken = t
		}
	}

	return token == masterToken
}

// AddToken - добавление нового токена
func (a *TokenAuthenticator) AddToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[token] = true
}

// RemoveToken - удаление токена (кроме master токена)
func (a *TokenAuthenticator) RemoveToken(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Нельзя удалить master token
	if a.isMasterTokenLocked(token) {
		return false
	}

	delete(a.tokens, token)
	return true
}

// GetTokenCount - получение количества токенов
func (a *TokenAuthenticator) GetTokenCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.tokens)
}

// isMasterTokenLocked - проверка master токена без блокировки (для внутреннего использования)
func (a *TokenAuthenticator) isMasterTokenLocked(token string) bool {
	if len(a.tokens) == 0 {
		return false
	}

	var masterToken string
	for t := range a.tokens {
		if masterToken == "" || t < masterToken {
			masterToken = t
		}
	}

	return token == masterToken
}

// GenerateToken - генерация случайного токена заданной длины
// length - длина токена в байтах (рекомендуется 32 для 64 hex символов)
func GenerateToken(length int) (string, error) {
	if length <= 0 {
		length = 32 // По умолчанию 32 байта = 64 hex символа
	}

	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}

	return hex.EncodeToString(bytes), nil
}

// AuthMiddleware - middleware для проверки аутентификации
func AuthMiddleware(next http.Handler, auth *TokenAuthenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Если аутентификация отключена, пропускаем запрос
		if !auth.enabled {
			next.ServeHTTP(w, r)
			return
		}

		valid, token := auth.Authenticate(r)
		if !valid {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Unauthorized: valid API token required",
			})
			return
		}

		// Добавляем токен в контекст запроса (для возможного использования в handlers)
		r.Header.Set("X-Authenticated-Token", token)

		next.ServeHTTP(w, r)
	})
}

// AuthStatusHandler - handler для получения статуса аутентификации
func AuthStatusHandler(auth *TokenAuthenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		response := map[string]interface{}{
			"enabled":    auth.enabled,
			"headerName": auth.headerName,
			"tokenCount": auth.GetTokenCount(),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// TokenManagementHandler - handler для управления токенами
func TokenManagementHandler(auth *TokenAuthenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodPost:
			// Генерация нового токена (требует master token)
			token := r.Header.Get("X-Authenticated-Token")
			if !auth.IsMasterToken(token) {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"error":   "Forbidden: master token required",
				})
				return
			}

			newToken, err := GenerateToken(32)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"error":   "Failed to generate token",
				})
				return
			}

			auth.AddToken(newToken)

			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"token":   newToken,
				"message": "Token generated successfully",
			})

		case http.MethodDelete:
			// Отзыв токена (требует master token)
			token := r.Header.Get("X-Authenticated-Token")
			if !auth.IsMasterToken(token) {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"error":   "Forbidden: master token required",
				})
				return
			}

			// Получаем токен для отзыва из query параметра
			tokenToRevoke := r.URL.Query().Get("token")
			if tokenToRevoke == "" {
				// Парсим тело запроса
				var req struct {
					Token string `json:"token"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"success": false,
						"error":   "Token to revoke is required",
					})
					return
				}
				tokenToRevoke = req.Token
			}

			if !auth.RemoveToken(tokenToRevoke) {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"error":   "Failed to revoke token (may be master token or not found)",
				})
				return
			}

			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Token revoked successfully",
			})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Method not allowed",
			})
		}
	}
}
