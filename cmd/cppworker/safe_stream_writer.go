// safe_stream_writer.go — потокобезопасная обёртка над http.ResponseWriter
// для streaming-эндпоинтов (SSE/NDJSON).
//
// Проблема (Issue «обрыв ответа без каких-либо ошибок»):
//
//	В writeOpenAIChatStream (и аналогах) использовались локальные
//	safeFprintf/safeFlush, которые:
//	  1. НЕ проверяли ctx.Done() перед Write/Flush — после обрыва клиента
//	     продолжали писать в мёртвый socket, получая "broken pipe" без
//	     какого-либо логирования.
//	  2. НЕ возвращали write error — caller не мог остановить keepalive
//	     goroutine и inference продолжал тратить CPU после обрыва.
//
// Решение: safeStreamWriter с:
//   - Проверкой ctx.Done() перед каждой операцией.
//   - Логированием первого write error (категория "client disconnected?").
//   - Счётчиками bytesWritten и writeErrors для /api/v1/cppworker/debug/last-stream.
//   - Потокобезопасностью через sync.Mutex.
//
// Использование:
//
//	w := newSafeStreamWriter(w, r, "writeOpenAIChatStream", modelName)
//	w.WriteHeader(http.StatusOK)
//	w.WriteHeaderKey("Content-Type", "text/event-stream")
//	if !w.Writef("data: %s\n\n", jsonData) {
//	    return // client disconnected
//	}
//	w.Flush()
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// safeStreamWriter — потокобезопасная обёртка для streaming-ответов.
//
// Все методы возвращают bool: true если операция выполнена, false если
// клиент отвалился (ctx.Done()) или write вернул ошибку.
//
// После первого write error writer считается "сломанным" — все
// последующие операции no-op (Writef/Flush возвращают false без
// реальной попытки записи). Это предотвращает шум в логах при
// длинных стримах после обрыва клиента.
type safeStreamWriter struct {
	mu          sync.Mutex
	w           http.ResponseWriter
	flusher     http.Flusher
	ctx         context.Context
	handlerName string
	modelName   string

	// Метрики для /api/v1/cppworker/debug/last-stream.
	startTime     time.Time
	bytesWritten  int64 // atomic для lock-free чтения из debug endpoint
	writeErrors   int64 // atomic
	tokensSent    int64 // atomic (callback сам инкрементирует)
	headerWritten bool
	broken        bool // true после первого write error

	// R476: CPPWORKER_STREAM_TRACE — побайтовое доказательство отдачи.
	traceHash   hash.Hash
	traceChunks int64
	traceBytes  int64

	// Snapshot для диагностики.
	lastWriteErr  error
	lastWriteAt   time.Time
	disconnectAt  time.Time
	disconnectWhy string
}

// newSafeStreamWriter создаёт writer для streaming-ответа.
// handlerName используется в логах (например "writeOpenAIChatStream").
func newSafeStreamWriter(w http.ResponseWriter, r *http.Request, handlerName, modelName string) *safeStreamWriter {
	sw := &safeStreamWriter{
		w:           w,
		ctx:         r.Context(),
		handlerName: handlerName,
		modelName:   modelName,
		startTime:   time.Now(),
	}
	if f, ok := w.(http.Flusher); ok {
		sw.flusher = f
	}
	if streamTraceEnvR476() {
		sw.traceHash = sha256.New()
	}
	return sw
}

// WriteHeader вызывает underlying ResponseWriter.WriteHeader.
// После вызова WriteHeader нельзя менять headers.
//
// Возвращает false если ctx.Done() — в этом случае writer помечается
// как broken, WriteHeader не вызывается (но клиент уже не получит ничего).
func (sw *safeStreamWriter) WriteHeader(status int) bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.broken {
		return false
	}
	if err := sw.ctx.Err(); err != nil {
		sw.markBroken("ctx_done_before_header", err)
		return false
	}

	sw.w.WriteHeader(status)
	sw.headerWritten = true
	return true
}

// SetHeader устанавливает HTTP header. Должен быть вызван ДО WriteHeader.
// Безопасен для concurrent вызовов.
func (sw *safeStreamWriter) SetHeader(key, value string) bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.headerWritten {
		return false
	}
	sw.w.Header().Set(key, value)
	return true
}

// Writef пишет форматированную строку в writer.
// Возвращает false если ctx.Done() или write error.
func (sw *safeStreamWriter) Writef(format string, a ...interface{}) bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.broken {
		return false
	}
	if err := sw.ctx.Err(); err != nil {
		sw.markBroken("ctx_done_on_write", err)
		return false
	}

	var n int
	var err error
	if sw.traceHash != nil {
		msg := fmt.Sprintf(format, a...)
		n, err = io.WriteString(sw.w, msg)
		if err == nil {
			sw.traceWrite([]byte(msg))
		}
	} else {
		n, err = fmt.Fprintf(sw.w, format, a...)
	}
	if err != nil {
		sw.markBroken("write_error", err)
		sw.lastWriteErr = err
		return false
	}
	atomic.AddInt64(&sw.bytesWritten, int64(n))
	sw.lastWriteAt = time.Now()
	return true
}

// Write пишет байты напрямую (для SSE-heartbeat и т.п.).
func (sw *safeStreamWriter) Write(p []byte) bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.broken {
		return false
	}
	if err := sw.ctx.Err(); err != nil {
		sw.markBroken("ctx_done_on_write", err)
		return false
	}

	n, err := sw.w.Write(p)
	if err != nil {
		sw.markBroken("write_error", err)
		sw.lastWriteErr = err
		return false
	}
	sw.traceWrite(p[:n])
	atomic.AddInt64(&sw.bytesWritten, int64(n))
	sw.lastWriteAt = time.Now()
	return true
}

// Flush сбрасывает буфер writer'а. Возвращает false если writer broken.
func (sw *safeStreamWriter) Flush() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.broken {
		return false
	}
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
	return true
}

// IncTokens увеличивает счётчик отправленных токенов (для debug snapshot).
func (sw *safeStreamWriter) IncTokens(n int) {
	atomic.AddInt64(&sw.tokensSent, int64(n))
}

// TokensSent — сколько токенов ушло клиенту (R91).
//
// Нужен, чтобы отличить «модель закончила сама» (EOS) от «упёрлись в кап
// max_tokens»: во втором случае finish_reason обязан быть "length", иначе клиент
// считает обрезанный ответ полным, а авто-продолжение балансера
// (LB_AUTO_CONTINUE_ON_TRUNCATION) не срабатывает.
func (sw *safeStreamWriter) TokensSent() int64 {
	return atomic.LoadInt64(&sw.tokensSent)
}

// IsBroken возвращает true если writer уже сломан (ctx.Done или write error).
func (sw *safeStreamWriter) IsBroken() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.broken
}

// markBroken — internal: пометить writer как сломанный и записать snapshot.
func (sw *safeStreamWriter) markBroken(why string, err error) {
	if sw.broken {
		return
	}
	sw.broken = true
	sw.disconnectAt = time.Now()
	sw.disconnectWhy = why
	atomic.AddInt64(&sw.writeErrors, 1)

	// Уведомляем NaN-healer об обрыве стрима (адаптивный загрузчик)
	if RecordStreamBreakWrapper != nil {
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		RecordStreamBreakWrapper(sw.modelName, why, errStr)
	}

	// Snapshot для /api/v1/cppworker/debug/last-stream.
	snapshot := LastStreamInfo{
		Model:          sw.modelName,
		Handler:        sw.handlerName,
		TokensSent:     atomic.LoadInt64(&sw.tokensSent),
		BytesWritten:   atomic.LoadInt64(&sw.bytesWritten),
		WriteErrors:    atomic.LoadInt64(&sw.writeErrors),
		DurationMs:     time.Since(sw.startTime).Milliseconds(),
		DisconnectedAt: sw.disconnectAt.Format(time.RFC3339Nano),
		Reason:         why,
		LastWriteErr:   errString(err),
		LastWriteAt:    sw.lastWriteAt.Format(time.RFC3339Nano),
	}
	recordLastStreamInfo(snapshot)

	logger.Get().Warnw("safeStreamWriter: stream broken",
		"handler", sw.handlerName,
		"model", sw.modelName,
		"why", why,
		"error", err,
		"tokens_sent", snapshot.TokensSent,
		"bytes_written", snapshot.BytesWritten,
		"duration_ms", snapshot.DurationMs)
}

// Snapshot возвращает текущее состояние writer'а (для отладки).
func (sw *safeStreamWriter) Snapshot() LastStreamInfo {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return LastStreamInfo{
		Model:          sw.modelName,
		Handler:        sw.handlerName,
		TokensSent:     atomic.LoadInt64(&sw.tokensSent),
		BytesWritten:   atomic.LoadInt64(&sw.bytesWritten),
		WriteErrors:    atomic.LoadInt64(&sw.writeErrors),
		DurationMs:     time.Since(sw.startTime).Milliseconds(),
		DisconnectedAt: sw.disconnectAt.Format(time.RFC3339Nano),
		Reason:         sw.disconnectWhy,
		LastWriteErr:   errString(sw.lastWriteErr),
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ── R476 (2026-11-07): CPPWORKER_STREAM_TRACE ────────────────────────────────
//
// ЗАЧЕМ. Спор «клиент получил неполный ответ» без доказательства решается
// догадкой. Отдельный счётчик байт уже есть, но он не отвечает на вопрос, ДОШЛИ
// ли байты до сокета в том же виде. При включённом CPPWORKER_STREAM_TRACE=1
// writer считает чанки и sha256 ВСЕГО записанного: в сводке хода появляются
// stream_chunks и stream_sha256, и их можно сверить с тем, что видит клиент.
//
// Выключено по умолчанию: хеширование на каждом чанке — лишняя работа, а для
// обычных ходов достаточно streamed_content_bytes.

// streamTraceEnvR476 — включён ли побайтовый учёт стрима.
func streamTraceEnvR476() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_STREAM_TRACE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// traceWrite добавить записанные байты в хеш (только при включённой трассировке).
func (sw *safeStreamWriter) traceWrite(p []byte) {
	if sw.traceHash == nil || len(p) == 0 {
		return
	}
	_, _ = sw.traceHash.Write(p)
	sw.traceChunks++
	atomic.AddInt64(&sw.traceBytes, int64(len(p)))
}

// StreamTrace — чанки, байты и sha256 записанного ("" если трассировка выключена).
func (sw *safeStreamWriter) StreamTrace() (chunks int64, bytes int64, sha string) {
	if sw.traceHash == nil {
		return 0, 0, ""
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.traceChunks, atomic.LoadInt64(&sw.traceBytes), hex.EncodeToString(sw.traceHash.Sum(nil))
}
