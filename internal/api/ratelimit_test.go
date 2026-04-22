package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRateLimiterAllow - проверка разрешения запросов при наличии токенов
func TestRateLimiterAllow(t *testing.T) {
	t.Parallel()

	// Создаем rate limiter с 5 токенами и скоростью пополнения 1 токен/сек
	limiter := NewRateLimiter(5, 1)

	// Первые 5 запросов должны быть разрешены
	for i := 0; i < 5; i++ {
		assert.True(t, limiter.Allow(), "Запрос %d должен быть разрешен", i+1)
	}

	// 6-й запрос должен быть отклонен (токены исчерпаны)
	assert.False(t, limiter.Allow(), "6-й запрос должен быть отклонен")
}

// TestRateLimiterExhausted - проверка отказа при исчерпании токенов
func TestRateLimiterExhausted(t *testing.T) {
	t.Parallel()

	// Создаем rate limiter с 2 токенами
	limiter := NewRateLimiter(2, 0.5)

	// Исчерпываем все токены
	assert.True(t, limiter.Allow(), "Первый запрос должен быть разрешен")
	assert.True(t, limiter.Allow(), "Второй запрос должен быть разрешен")

	// Все последующие запросы должны быть отклонены
	for i := 0; i < 5; i++ {
		assert.False(t, limiter.Allow(), "Запрос после исчерпания должен быть отклонен")
	}
}

// TestRateLimiterRefill - проверка пополнения токенов со временем
func TestRateLimiterRefill(t *testing.T) {
	t.Parallel()

	// Создаем rate limiter с 2 токенами и скоростью 10 токенов/сек
	limiter := NewRateLimiter(2, 10)

	// Исчерпываем токены
	assert.True(t, limiter.Allow())
	assert.True(t, limiter.Allow())
	assert.False(t, limiter.Allow())

	// Ждем 200мс - должно пополниться ~2 токена
	time.Sleep(200 * time.Millisecond)

	// Теперь должен быть разрешен хотя бы один запрос
	assert.True(t, limiter.Allow(), "После ожидания должен быть доступен токен")
}

// TestRateLimiterGetStatus - проверка получения статуса
func TestRateLimiterGetStatus(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(10, 5)

	tokens, maxTokens := limiter.GetStatus()
	assert.Equal(t, 10.0, maxTokens)
	assert.Equal(t, 10.0, tokens)

	// Потребляем 3 токена
	limiter.Allow()
	limiter.Allow()
	limiter.Allow()

	tokens, _ = limiter.GetStatus()
	assert.Equal(t, 7.0, tokens)
}

// TestRateLimiterGetRetryAfter - проверка времени до следующего токена
func TestRateLimiterGetRetryAfter(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(1, 2) // 2 токена в секунду

	// Потребляем токен
	limiter.Allow()

	// Проверяем время ожидания
	retryAfter := limiter.GetRetryAfter()
	assert.Greater(t, retryAfter, 0.0)
	assert.LessOrEqual(t, retryAfter, 0.6) // ~0.5 секунды
}

// TestRateLimitMiddleware - проверка middleware (200 vs 429)
func TestRateLimitMiddleware(t *testing.T) {
	t.Parallel()

	// Создаем лимитер с 1 токеном
	limiter := NewRateLimiter(1, 0.1)

	// Создаем тестовый обработчик
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Оборачиваем в middleware
	wrappedHandler := RateLimitMiddleware(handler, limiter)

	// Первый запрос - должен пройти (200 OK)
	req1 := httptest.NewRequest(http.MethodGet, "/test", nil)
	w1 := httptest.NewRecorder()
	wrappedHandler(w1, req1)

	assert.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "OK", w1.Body.String())
	assert.Contains(t, w1.Header().Get("X-RateLimit-Limit"), "1.0")

	// Второй запрос - должен быть отклонен (429 Too Many Requests)
	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	w2 := httptest.NewRecorder()
	wrappedHandler(w2, req2)

	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
	assert.Contains(t, w2.Body.String(), "Too Many Requests")
	assert.Equal(t, "0", w2.Header().Get("X-RateLimit-Remaining"))
	assert.NotEmpty(t, w2.Header().Get("Retry-After"))
}

// TestRateLimitMiddlewareHeaders - проверка заголовков rate limiting
func TestRateLimitMiddlewareHeaders(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(5, 2)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrappedHandler := RateLimitMiddleware(handler, limiter)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	wrappedHandler(w, req)

	// Проверяем заголовки
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("X-RateLimit-Limit"), "5.0")
	assert.Contains(t, w.Header().Get("X-RateLimit-Remaining"), "4.0")
}

// TestRateLimiterConcurrent - проверка потокобезопасности
func TestRateLimiterConcurrent(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(100, 50)

	done := make(chan bool)
	allowed := make(chan bool, 200)

	// Запускаем 10 горутин, каждая делает по 20 запросов
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 20; j++ {
				allowed <- limiter.Allow()
			}
			done <- true
		}()
	}

	// Ждем завершения всех горутин
	for i := 0; i < 10; i++ {
		<-done
	}
	close(allowed)

	// Считаем разрешенные запросы
	allowedCount := 0
	for result := range allowed {
		if result {
			allowedCount++
		}
	}

	// Должно быть разрешено ровно 100 запросов (начальные токены)
	assert.Equal(t, 100, allowedCount, "Должно быть разрешено ровно 100 запросов")
}

// TestNewRateLimiter - проверка создания лимитера
func TestNewRateLimiter(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(50, 10)

	assert.Equal(t, 50.0, limiter.maxTokens)
	assert.Equal(t, 10.0, limiter.refillRate)
	assert.Equal(t, 50.0, limiter.tokens)
}

// TestRateLimiterRefillNoOverflow - проверка что токены не превышают максимум
func TestRateLimiterRefillNoOverflow(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(5, 100) // Высокая скорость пополнения

	// Потребляем 2 токена
	limiter.Allow()
	limiter.Allow()

	// Ждем немного
	time.Sleep(100 * time.Millisecond)

	// Получаем статус
	tokens, maxTokens := limiter.GetStatus()

	// Токены не должны превышать максимум
	assert.LessOrEqual(t, tokens, maxTokens)
}
