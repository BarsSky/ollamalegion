package api

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimiter реализует алгоритм token bucket для ограничения частоты запросов
type RateLimiter struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // токенов в секунду
	lastRefill time.Time
	disabled   bool
	mu         sync.Mutex
}

// NewRateLimiter создает новый rate limiter с указанными параметрами
// maxTokens - максимальное количество токенов (burst capacity)
// refillRate - скорость пополнения токенов в секунду
// Если maxTokens <= 0 или refillRate <= 0, rate limiting считается отключённым
// и Allow() всегда возвращает true.
func NewRateLimiter(maxTokens, refillRate float64) *RateLimiter {
	if maxTokens <= 0 || refillRate <= 0 {
		return &RateLimiter{
			disabled: true,
		}
	}
	return &RateLimiter{
		tokens:     maxTokens, // начинаем с полным баком
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// refill пополняет токены на основе прошедшего времени
// должна вызываться с захваченным мьютексом
func (rl *RateLimiter) refill() {
	now := time.Now()
	elapsed := now.Sub(rl.lastRefill).Seconds()
	
	// Добавляем токены на основе прошедшего времени
	rl.tokens += elapsed * rl.refillRate
	
	// Ограничиваем максимальным количеством
	if rl.tokens > rl.maxTokens {
		rl.tokens = rl.maxTokens
	}
	
	rl.lastRefill = now
}

// Allow проверяет доступность токена и потребляет его если возможно
// возвращает true если запрос разрешен, false если лимит превышен
func (rl *RateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.disabled {
		return true
	}

	rl.refill()

	if rl.tokens >= 1.0 {
		rl.tokens -= 1.0
		return true
	}

	return false
}

// GetStatus возвращает текущий статус rate limiter
func (rl *RateLimiter) GetStatus() (tokens float64, maxTokens float64) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.disabled {
		return 0, 0
	}

	rl.refill()

	return rl.tokens, rl.maxTokens
}

// GetRefillRate возвращает скорость пополнения токенов
func (rl *RateLimiter) GetRefillRate() float64 {
	return rl.refillRate
}

// GetRetryAfter возвращает время в секундах до следующего доступного токена
// если токены доступны сейчас, возвращает 0
func (rl *RateLimiter) GetRetryAfter() float64 {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.disabled {
		return 0
	}

	rl.refill()

	if rl.tokens >= 1.0 {
		return 0
	}

	// Вычисляем время до получения одного токена
	tokensNeeded := 1.0 - rl.tokens
	return tokensNeeded / rl.refillRate
}

// RateLimitMiddleware создает middleware для ограничения частоты запросов
// next - следующий обработчик
// limiter - rate limiter для проверки
// возвращает http.HandlerFunc который оборачивает next с rate limiting
func RateLimitMiddleware(next http.HandlerFunc, limiter *RateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Проверяем доступность токена
		if !limiter.Allow() {
			// Лимит превышен - возвращаем 429
			retryAfter := limiter.GetRetryAfter()
			_, maxTokens := limiter.GetStatus()
			
			// Устанавливаем заголовки rate limiting
			w.Header().Set("X-RateLimit-Limit", formatFloat(maxTokens))
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", formatDuration(retryAfter))
			
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		
		// Токен доступен - устанавливаем заголовки и передаем запрос дальше
		tokens, maxTokens := limiter.GetStatus()
		w.Header().Set("X-RateLimit-Limit", formatFloat(maxTokens))
		w.Header().Set("X-RateLimit-Remaining", formatFloat(tokens))
		
		next(w, r)
	}
}

// formatFloat форматирует float64 в строку
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', 1, 64)
}

// formatDuration форматирует длительность в секунды для заголовка Retry-After
func formatDuration(seconds float64) string {
	if seconds < 1 {
		return "1"
	}
	return strconv.Itoa(int(seconds + 0.5))
}

// GetRateLimiter возвращает rate limiter для использования в middleware
// это вспомогательная функция для получения доступа к лимитеру из handlers
func (rl *RateLimiter) GetRateLimiter() *RateLimiter {
	return rl
}

// RateLimitStatusHandler создает обработчик для endpoint статуса rate limiter
func RateLimitStatusHandler(limiter *RateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		
		tokens, maxTokens := limiter.GetStatus()
		refillRate := limiter.GetRefillRate()
		
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"tokens": %.1f, "max_tokens": %.0f, "refill_rate": %.0f}`,
			tokens, maxTokens, refillRate)
	}
}
