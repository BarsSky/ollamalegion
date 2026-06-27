// debug_last_stream.go — диагностический endpoint /api/v1/cppworker/debug/last-stream.
//
// Проблема (Issue «обрыв ответа без каких-либо ошибок»):
//
//   Когда клиент (Cline/OpenWebUI/Roo Code) отваливается во время streaming
//   генерации, пользователь видит просто пустой/обрезанный ответ без
//   объяснений. Без диагностического snapshot'а единственный способ найти
//   причину — лезть в сырые логи cppworker и grep'ать "broken pipe" или
//   "connection reset", что неудобно.
//
// Решение: после каждого stream disconnect (ctx.Done() или write error)
// safeStreamWriter записывает LastStreamInfo в thread-safe storage. Endpoint
// GET /api/v1/cppworker/debug/last-stream отдаёт этот snapshot в JSON.
//
// Структура:
//   - model: имя модели
//   - handler: какой хендлер писал стрим (writeOpenAIChatStream, ...)
//   - tokens_sent: число токенов, которые были успешно записаны
//   - bytes_written: байт записано до обрыва
//   - write_errors: количество write ошибок (>=1 = обрыв)
//   - duration_ms: длительность стрима до обрыва
//   - disconnected_at: RFC3339Nano время обрыва
//   - reason: "ctx_done_before_header" | "ctx_done_on_write" | "write_error"
//   - last_write_err: текст ошибки (если есть)
//   - last_write_at: когда был последний успешный write
package main

import (
	"net/http"
	"sync"

	"ollama-loadbalancer/pkg/logger"
)

// LastStreamInfo — snapshot последнего stream disconnect'а.
type LastStreamInfo struct {
	Model          string `json:"model"`
	Handler        string `json:"handler"`
	TokensSent     int64  `json:"tokens_sent"`
	BytesWritten   int64  `json:"bytes_written"`
	WriteErrors    int64  `json:"write_errors"`
	DurationMs     int64  `json:"duration_ms"`
	DisconnectedAt string `json:"disconnected_at,omitempty"`
	Reason         string `json:"reason,omitempty"`
	LastWriteErr   string `json:"last_write_err,omitempty"`
	LastWriteAt    string `json:"last_write_at,omitempty"`
}

var (
	lastStreamMu sync.RWMutex
	lastStream   *LastStreamInfo
)

// recordLastStreamInfo сохраняет snapshot последнего обрыва.
// Вызывается из safeStreamWriter.markBroken.
func recordLastStreamInfo(info LastStreamInfo) {
	lastStreamMu.Lock()
	defer lastStreamMu.Unlock()
	lastStream = &info
}

// getLastStreamInfo возвращает последний snapshot (или nil).
func getLastStreamInfo() *LastStreamInfo {
	lastStreamMu.RLock()
	defer lastStreamMu.RUnlock()
	return lastStream
}

// handleDebugLastStream — GET /api/v1/cppworker/debug/last-stream.
//
// Возвращает:
//   - 200 OK + JSON snapshot если есть хотя бы один записанный disconnect.
//   - 404 Not Found если disconnect'ов ещё не было.
//
// Требует X-API-Token (authMiddleware).
func handleDebugLastStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed, use GET")
		return
	}

	info := getLastStreamInfo()
	if info == nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error":   "no stream disconnects recorded yet",
			"hint":    "Send a streaming request to /v1/chat/completions and abort it before completion.",
		})
		return
	}

	logger.Get().Debugw("debug/last-stream requested",
		"remote", r.RemoteAddr,
		"model", info.Model,
		"reason", info.Reason)

	writeJSON(w, http.StatusOK, info)
}

// handleDebugLastStreamClear — POST /api/v1/cppworker/debug/last-stream/clear.
// Сбрасывает snapshot (для тестов и отладки).
func handleDebugLastStreamClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed, use POST")
		return
	}
	lastStreamMu.Lock()
	lastStream = nil
	lastStreamMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "cleared"})
}