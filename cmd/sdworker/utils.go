// utils.go — HTTP-утилиты sdworker: middleware, JSON-ответы, CORS.
//
// Копия паттернов cmd/cppworker/utils.go (осознанно: это отдельный бинарь,
// общий пакет для двух middleware не окупается). Отличия:
//   - CORS/OPTIONS работает ВСЕГДА (в cppworker он был про WebUI, здесь —
//     про прямые подключения клиентов: SillyTavern «Connect» шлёт OPTIONS);
//   - есть OpenAI-конверт ошибок (§12.4 п.6): sd-server отдаёт
//     {"error":"строка"}, часть SDK такое не парсит.
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// Middleware
// ============================================================

// recoverMiddleware — паника в хендлере не должна рвать соединение без ответа.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				appLog().Errorw("panic in HTTP handler",
					"path", r.URL.Path, "method", r.Method, "panic", rec)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]any{
						"message": "internal server error",
						"type":    "server_error",
						"code":    "panic",
					},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware — CORS + OPTIONS.
//
// ПОЧЕМУ ЭТО ВАЖНО ИМЕННО ЗДЕСЬ: клиенты (SillyTavern, LibreChat, Open WebUI,
// n8n) бьют в воркер напрямую по 18093, если оператор так настроил. Без 204 на
// OPTIONS кнопка «Connect» в SillyTavern падает, хотя генерация работает.
//
// Allow-Credentials не ставим: с origin "*" это несовместимо по спецификации
// W3C (и браузер отбросит ответ).
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers",
			"Content-Type, Authorization, X-API-Token, X-Request-Id, X-User-Id, X-HF-Token")
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// pollingPaths — часто опрашиваемые пути: логируем на Debug, иначе лог тонет.
var pollingPaths = map[string]bool{
	"/health":                        true,
	"/api/health":                    true,
	"/api/image/models":              true,
	"/api/image/models/load/progress": true,
	"/api/image/capabilities":        true,
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		flusher, _ := w.(http.Flusher)
		lw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK, flusher: flusher}
		next.ServeHTTP(lw, r)
		dur := time.Since(start)
		remote := r.RemoteAddr
		if idx := strings.LastIndex(remote, ":"); idx > 0 {
			remote = remote[:idx]
		}
		if pollingPaths[r.URL.Path] && r.Method == http.MethodGet {
			appLog().Debugw("HTTP request", "method", r.Method, "path", r.URL.Path,
				"status", lw.statusCode, "duration", dur.String(), "remote", remote)
		} else {
			appLog().Infow("HTTP request", "method", r.Method, "path", r.URL.Path,
				"status", lw.statusCode, "duration", dur.String(), "remote", remote)
		}
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	flusher    http.Flusher
}

func (lw *loggingResponseWriter) WriteHeader(code int) {
	lw.statusCode = code
	lw.ResponseWriter.WriteHeader(code)
}

func (lw *loggingResponseWriter) Flush() {
	if lw.flusher != nil {
		lw.flusher.Flush()
	}
}

// ============================================================
// JSON-ответы
// ============================================================

func writeJSON(w http.ResponseWriter, status int, data any) {
	body, err := json.Marshal(data)
	if err != nil {
		logger.Get().Errorw("failed to marshal JSON response", "error", err)
		body = []byte(`{"error":"internal marshal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError — плоская ошибка (наш нативный контракт, /sdapi/*, /health).
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeJSONError — ошибка с машинным кодом.
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

// writeOpenAIError — OpenAI-конверт ошибки (§12.4 п.6).
//
// sd-server отдаёт {"error":"строка"} — часть SDK (OpenAI Python/Node,
// AnythingLLM) не умеет это парсить и показывает клиенту «Unknown error».
// Форма: {"error":{"message","type","code"}}.
func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	kind := "invalid_request_error"
	switch {
	case status >= 500:
		kind = "server_error"
	case status == http.StatusTooManyRequests:
		kind = "rate_limit_error"
	case status == http.StatusUnauthorized:
		kind = "authentication_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
			"code":    code,
		},
	})
}

// setRetryAfter — заголовок Retry-After для 429 (клиенты его читают не все, но
// балансер и curl-скрипты — да; отсутствие заголовка делает 429 «слепым»).
func setRetryAfter(w http.ResponseWriter, seconds int) {
	if seconds <= 0 {
		seconds = 3
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
}

// parseInt — строка в int с дефолтом (для query-параметров вроде limit).
// Пустая строка → defaultVal без ошибки, как в cppworker.
func parseInt(s string, defaultVal int) (int, error) {
	if s == "" {
		return defaultVal, nil
	}
	val, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal, err
	}
	return val, nil
}

// statusForError — HTTP-статус по классу ошибки сервиса.
func statusForError(err error) (status int, code string) {
	c := sdbackend.ErrorCodeOf(err)
	switch c {
	case "queue_full", "engine_queue_full":
		return http.StatusTooManyRequests, c
	case "model_not_found", "model_not_loaded", "model_loading":
		return http.StatusConflict, c
	case "cannot_cancel_generating":
		return http.StatusConflict, c
	case "generation_timeout":
		return http.StatusGatewayTimeout, c
	case "sd_server_incompatible", "sd_server_startup_failed":
		return http.StatusServiceUnavailable, c
	case "job_not_found", "job_gone":
		return http.StatusNotFound, c
	case "invalid_generation_params":
		return http.StatusBadRequest, c
	}
	return http.StatusInternalServerError, c
}
