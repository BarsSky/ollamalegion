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

// writeJSONError — ошибка с машинным кодом и подсказкой «что делать».
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	body := map[string]any{"error": msg, "code": code}
	if h := hintForCode(code); h != "" {
		body["hint"] = h
	}
	writeJSON(w, status, body)
}

// writeOpenAIError — OpenAI-конверт ошибки (§12.4 п.6).
//
// sd-server отдаёт {"error":"строка"} — часть SDK (OpenAI Python/Node,
// AnythingLLM) не умеет это парсить и показывает клиенту «Unknown error».
// Форма: {"error":{"message","type","code"}}.
//
// R88: внутрь конверта добавлен `hint` (что делать), а также продублирован на
// верхнем уровне — панель и curl читают его оттуда, а SDK его игнорируют.
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
	inner := map[string]any{
		"message": message,
		"type":    kind,
		"code":    code,
	}
	body := map[string]any{"error": inner}
	if h := hintForCode(code); h != "" {
		inner["hint"] = h
		body["hint"] = h
	}
	writeJSON(w, status, body)
}

// hintForCode — ЧТО ДЕЛАТЬ при этой ошибке (явное объяснение, R88).
//
// ЗАЧЕМ: до R88 клиент и оператор видели только код и текст
// (`generation_timeout`, `model_not_loaded`), а «что делать» приходилось
// выяснять по документации. Балансер для своих отказов это уже делает
// (writeGateError / writeUpstreamError) — воркер обязан отвечать так же.
// Пустая строка = подсказки нет: выдумывать «на всякий случай» нельзя, иначе
// подсказка перестаёт означать конкретное действие.
func hintForCode(code string) string {
	switch code {
	case "generation_timeout":
		return "генерация не уложилась в кап времени. Кап взведён явно " +
			"(SDWORKER_GENERATION_TIMEOUT_SEC либо profile.timeoutSec при " +
			"SDWORKER_ALLOW_PROFILE_TIMEOUTS=on) — уберите его, чтобы ждать " +
			"терминального состояния генерации"
	case "queue_full", "engine_queue_full":
		return "очередь генераций заполнена: движок исполняет джобы последовательно. " +
			"Повторите позже (Retry-After) или ориентируйтесь на active_queries"
	case "model_not_found":
		return "такой модели нет в каталоге воркера: список — GET /api/image/models; " +
			"набор скачивается в разделе HuggingFace (WebUI) или через POST /api/hf/bundle"
	case "model_not_loaded":
		return "модель не загружена: POST /api/image/models/load {\"name\":\"<model>\"} " +
			"(в WebUI — кнопка «Загрузить» в разделе image-моделей)"
	case "model_loading":
		return "модель ещё поднимается: дождитесь готовности — GET /api/image/models/load/progress " +
			"(или поток .../progress/stream)"
	case "cannot_cancel_generating":
		return "джоба уже исполняется движком: отмена возможна только для очереди " +
			"(cancel_queued_only). Дождитесь завершения — VRAM освободится по факту"
	case "sd_server_startup_failed":
		return "sd-server не поднялся: в логе воркера найдите «spawning sd-server» (полный argv) " +
			"и следующий за ним вывод процесса — обычно это неверный плейсмент или отсутствующий файл роли"
	case "sd_server_incompatible":
		return "движок несовместим с набором файлов: проверьте роли diffusion/vae/llm в profile.json " +
			"и версию sd-server (GET /health → sd_server_revision)"
	case "job_not_found", "job_gone":
		return "джоба неизвестна или истекла (completed_job_ttl_seconds): запросите статус заново " +
			"либо перезапустите генерацию"
	case "generation_failed", "engine_job_failed":
		return "движок не отдал ни одной картинки. Чаще всего это холодный старт " +
			"(веса 13.6 ГБ ещё подкачиваются с диска — первый прогон после load " +
			"занимает минуты) либо нехватка VRAM на активациях: повторите запрос на " +
			"прогретой модели и посмотрите вывод процесса рядом со «spawning sd-server» " +
			"в логе воркера"
	case "invalid_generation_params":
		return "параметры вне границ движка: актуальные границы — GET /api/image/capabilities " +
			"(размеры 64..4096, шаги 1..100, batch 1..8)"
	case "invalid_size":
		return "размер должен быть кратен 64 и лежать в 64..4096: границы движка — GET /api/image/capabilities"
	case "invalid_strength":
		return "strength для img2img лежит в [0,1]: 0 = без изменений, 1 = игнорировать исходное изображение"
	case "payload_too_large":
		return "тело запроса слишком большое: для img2img/edit передавайте изображение файлом " +
			"(multipart), а не base64 в JSON"
	case "image_required", "init_images_required":
		return "для этой операции нужно исходное изображение: передайте его в multipart (image) " +
			"или в init_images (base64)"
	case "invalid_image", "invalid_mask":
		return "не удалось прочитать изображение/маску: поддерживаются PNG/JPEG/WebP, " +
			"маска — в оттенках серого той же геометрии, что и изображение"
	case "prompt_required":
		return "поле prompt обязательно (для img2img достаточно init_images + prompt)"
	case "invalid_json":
		return "тело запроса не разобралось как JSON: проверьте Content-Type и отсутствие BOM"
	case "invalid_content_type":
		return "ожидается application/json (или multipart/form-data для edit-эндпоинтов)"
	case "invalid_multipart":
		return "multipart-форма не разобралась: убедитесь, что поле с картинкой названо image/mask"
	case "method_not_allowed":
		return "метод не поддерживается этим эндпоинтом: POST для генерации, GET для моделей и capabilities"
	case "not_implemented":
		return "эндпоинт объявлен, но в этой сборке движка не реализован: " +
			"смотрите /api/image/capabilities (features) перед использованием"
	}
	return ""
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
