package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"

	"ollama-loadbalancer/pkg/logger"
)

// F.0b (2026-06-28): session F — defensive panic recovery middleware.
//
// Контекст: до этого фикса panic в любом хендлере приводил к закрытию TCP-соединения
// без записи HTTP-ответа — клиент (Cline/OpenWebUI/Roo Code) получал EOF без
// какой-либо диагностики. Cppworker выживал (HTTP server в Go автоматически
// recover'ит горутины), но клиент оставался в подвешенном состоянии.
//
// Решение: middleware recoverMiddleware оборачивает все handler'ы и при panic:
//  1. Логирует stack trace через zap (с structured fields).
//  2. Пишет HTTP 500 + JSON {"error":"internal_error","message":...} если
//     headers ещё не записаны.
//  3. Если headers уже записаны (streaming in progress) — логирует и завершает
//     соединение; клиент увидит truncated stream с явным diagnostic event
//     (а не EOF без диагностики).
//
// Размещение: внешний слой (последний в цепочке wrap), чтобы catch'ить panic
// от corsMiddleware, loggingMiddleware и любого handler'а.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Используем кастомный ResponseWriter для отслеживания состояния headers.
		rw := &recoverResponseWriter{
			ResponseWriter: w,
			headersWritten: false,
		}

		defer func() {
			if rec := recover(); rec != nil {
				stack := debug.Stack()

				logger.Get().Errorw("recoverMiddleware: panic caught",
					"method", r.Method,
					"path", r.URL.Path,
					"remote", r.RemoteAddr,
					"panic", fmt.Sprintf("%v", rec),
					"stack", string(stack),
					"headers_written", rw.headersWritten,
				)

				// Если headers ещё не записаны — возвращаем JSON 500.
				if !rw.headersWritten {
					rw.ResponseWriter.Header().Set("Content-Type", "application/json")
					rw.ResponseWriter.WriteHeader(http.StatusInternalServerError)
					errBody, _ := json.Marshal(map[string]interface{}{
						"error":   "internal_error",
						"message": fmt.Sprintf("server panic: %v", rec),
					})
					_, _ = rw.ResponseWriter.Write(errBody)
					return
				}

				// Headers уже записаны (streaming in progress) — невозможно
				// поменять HTTP status. Логируем и завершаем соединение.
				// Клиент увидит truncated stream; safeStreamWriter / SSE done
				// detection в proxyRequestLlamaCpp покажут ошибку корректно.
				logger.Get().Warnw("recoverMiddleware: panic after headers written, stream will be truncated",
					"path", r.URL.Path,
				)
			}
		}()

		next.ServeHTTP(rw, r)
	})
}

// recoverResponseWriter — обёртка для отслеживания состояния headers.
// Когда handler вызывает WriteHeader или Write, мы помечаем headersWritten=true.
type recoverResponseWriter struct {
	http.ResponseWriter
	headersWritten bool
	wroteHeader    bool
}

// WriteHeader перехватывает вызов и помечает headersWritten=true.
func (rw *recoverResponseWriter) WriteHeader(statusCode int) {
	if !rw.wroteHeader {
		rw.headersWritten = true
		rw.wroteHeader = true
	}
	rw.ResponseWriter.WriteHeader(statusCode)
}

// Write перехватывает вызовы записи — если WriteHeader ещё не вызывался,
// автоматически вызываем 200 OK (стандартное поведение http.ResponseWriter).
func (rw *recoverResponseWriter) Write(p []byte) (int, error) {
	if !rw.wroteHeader {
		rw.headersWritten = true
		rw.wroteHeader = true
	}
	return rw.ResponseWriter.Write(p)
}

// Flush — R189 (2026-10-20): ПРОБРАСЫВАЕМ Flush в нижний writer.
//
// ЖИВОЙ РАЗБОР. safeStreamWriter берёт флашер так:
//
//	if f, ok := w.(http.Flusher); ok { sw.flusher = f }
//
// recoverResponseWriter встраивает ИНТЕРФЕЙС http.ResponseWriter, а Flush в
// этот интерфейс не входит — значит обёртка http.Flusher НЕ реализует, и
// sw.flusher оставался nil. Все Flush() в стриминговых путях были no-op:
// Go-сервер копил NDJSON/SSE в своём буфере и отдавал разом при возврате
// хендлера. Снаружи это ровно «модель думает и молчит минутами, потом всё
// сразу» и обрыв сессии клиентом, хотя воркер отдавал кадры по мере генерации
// (живой замер: firstByte = 12447 мс при total = 12447 мс, два кадра в конце).
func (rw *recoverResponseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
