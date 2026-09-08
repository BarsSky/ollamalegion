// handlers_reset_reload.go — HTTP endpoint для сброса ramFallbackAttempts.
//
// Эндпоинт позволяет администратору сбросить счётчик reload-попыток
// для модели (или всех моделей) без перезапуска cppworker-контейнера.
//
// Использование:
//
//	curl -X POST http://localhost:18091/api/v1/cppworker/reset-reload-counter
//	curl -X POST http://localhost:18091/api/v1/cppworker/reset-reload-counter -d '{"model":"gemma-4"}'
//
// Без body → сбрасывает счётчик для всех моделей.
// С {"model": "..."} → сбрасывает только для указанной модели.
package main

import (
		"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// handleResetReloadCounter — POST /api/v1/cppworker/reset-reload-counter.
//
// Опциональное JSON-тело:
//
//	{"model": "model-name"}  // сбросить только для указанной модели
//
// Без тела — сбросить счётчик для всех моделей.
//
// Защищён authMiddleware (если задан API_TOKEN).
func handleResetReloadCounter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use POST")
		return
	}

	var req struct {
		Model string `json:"model,omitempty"`
	}

	// Body опционален — если нет, оставляем req.Model = "" и сбрасываем все.
	if r.Body != nil && r.ContentLength > 0 {
		if err := decodeJSONRequest(r, &req, 0); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}

	var resetCount int
	if req.Model != "" {
		// Сброс для конкретной модели
		resetReloadAttempts(req.Model)
		resetCount = 1
		logger.Get().Infow("reset-reload-counter: reset for single model",
			"model", req.Model, "request_id", getRequestID(r))
	} else {
		// Сброс для всех моделей — обход sync.Map
		ramFallbackAttempts.Range(func(key, value interface{}) bool {
			ramFallbackAttempts.Delete(key)
			resetCount++
			return true
		})
		logger.Get().Infow("reset-reload-counter: reset for ALL models",
			"reset_count", resetCount, "request_id", getRequestID(r))
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "ok",
		"model":      req.Model, // пусто = reset all
		"resetCount": resetCount,
		"message":    "ramFallbackAttempts cleared. Cppworker can now attempt reload again.",
	})
}

// getRequestID — извлекает X-Request-ID для логирования (best-effort).
func getRequestID(r *http.Request) string {
	if r == nil {
		return ""
	}
	return r.Header.Get("X-Request-ID")
}