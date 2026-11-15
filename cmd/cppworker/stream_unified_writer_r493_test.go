// stream_unified_writer_r493_test.go — страж этапа 7 рефакторинга (R493):
// ВСЕ писатели потока в воркере идут через один safeStreamWriter.
//
// ЗАЧЕМ. safeStreamWriter появился раньше, но его получили только три
// обработчика (writeChatStreamResponse, writeChatStreamResponseWithTools,
// writeOpenAIChatStream). Остальные писали напрямую через fmt.Fprintf(w, ...) и
// игнорировали ошибку записи, поэтому:
//   - обрыв клиента НЕ попадал в /api/v1/cppworker/debug/last-stream (оператор
//     видел последний успешный поток), и
//   - RecordStreamBreak не вызывался — NaN-healer адаптивного загрузчика не
//     узнавал о сорванном стриме.
//
// Это ровно тот класс «обрыв ответа без каких-либо ошибок», ради которого
// safeStreamWriter и делался, но покрывал он 3 писателя из 9.
//
// Тесты идут в порядке доказательства: они писались ДО правки и падали
// (getLastStreamInfo() == nil после обрыва).
package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// brokenClientWriter — клиент, который отвалился: после failAfter успешных
// байт каждая запись возвращает ошибку (как «broken pipe» на реальном сокете).
// Flush() есть — иначе обработчик не стал бы отдавать поток вовсе и тест
// проверял бы ветку «streaming not supported», а не обрыв.
type brokenClientWriter struct {
	// Порядок полей — под fieldalignment: сначала указательные (8/16 байт),
	// потом числовые. Мьютекс не нужен: писатель однопоточный, его вызывает
	// один обработчик в одном goroutine.
	header    http.Header
	err       error
	status    int
	written   int
	failAfter int
}

func newBrokenClientWriter(failAfter int) *brokenClientWriter {
	return &brokenClientWriter{
		header:    http.Header{},
		status:    http.StatusOK,
		failAfter: failAfter,
		err:       errors.New("write tcp 127.0.0.1: write: broken pipe"),
	}
}

func (b *brokenClientWriter) Header() http.Header { return b.header }

func (b *brokenClientWriter) WriteHeader(code int) {
	b.status = code
}

func (b *brokenClientWriter) Write(p []byte) (int, error) {
	if b.written >= b.failAfter {
		return 0, b.err
	}
	b.written += len(p)
	return len(p), nil
}

func (b *brokenClientWriter) Flush() {}

// resetLastStreamInfo чистит диагностический snapshot между тестами: иначе
// «последний обрыв» от предыдущего теста маскирует отсутствие нового.
func resetLastStreamInfo(t *testing.T) {
	t.Helper()
	lastStreamMu.Lock()
	lastStream = nil
	lastStreamMu.Unlock()
	t.Cleanup(func() {
		lastStreamMu.Lock()
		lastStream = nil
		lastStreamMu.Unlock()
	})
}

// TestR493r_ToolCallsStreamRecordsDisconnect — обрыв на /v1/chat/completions
// (ветка tool_calls) обязан попасть в диагностику с именем писателя.
func TestR493r_ToolCallsStreamRecordsDisconnect(t *testing.T) {
	resetLastStreamInfo(t)
	w := newBrokenClientWriter(10) // первый кадр уйдёт, второй — уже в мёртвый сокет
	writeToolCallsStream(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "test-model", "chatcmpl-1", 1731000000,
		[]openAIToolCall{{ID: "call_1", Type: "function",
			Function: openAIFunctionCall{Name: "get_weather", Arguments: `{"city":"Москва"}`}}},
		"", "prompt", false)

	info := getLastStreamInfo()
	if info == nil {
		t.Fatalf("обрыв клиента на writeToolCallsStream не записан в диагностику: " +
			"getLastStreamInfo() == nil (оператор увидит последний успешный поток, NaN-healer — ничего)")
	}
	if info.Handler != "writeToolCallsStream" {
		t.Errorf("handler = %q, want writeToolCallsStream", info.Handler)
	}
	if info.WriteErrors == 0 {
		t.Error("write_errors = 0 при обрыве клиента")
	}
	if info.BytesWritten == 0 {
		t.Error("bytes_written = 0 — writer не учёл уже отправленные байты")
	}
}

// TestR493r_StaticTextStreamRecordsDisconnect — то же для буферизированной
// SSE-ветки (tools запрошены, ответ отдаётся одним куском).
func TestR493r_StaticTextStreamRecordsDisconnect(t *testing.T) {
	resetLastStreamInfo(t)
	w := newBrokenClientWriter(10)
	writeStaticTextStream(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "test-model", "chatcmpl-2", 1731000000,
		"", "Видимый ответ.", "prompt", false)

	info := getLastStreamInfo()
	if info == nil {
		t.Fatalf("обрыв клиента на writeStaticTextStream не записан в диагностику: " +
			"getLastStreamInfo() == nil")
	}
	if info.Handler != "writeStaticTextStream" {
		t.Errorf("handler = %q, want writeStaticTextStream", info.Handler)
	}
	if info.WriteErrors == 0 {
		t.Error("write_errors = 0 при обрыве клиента")
	}
}

// TestR493r_StaticStreamNeedsNoBridge — писатели не должны требовать Inference:
// проверка «клиент не умеет flush → поток не отдаём» сохраняется, и в этом
// случае в диагностику НЕ пишется ложный обрыв.
type noFlushWriter struct {
	header http.Header
	buf    strings.Builder
}

func (n *noFlushWriter) Header() http.Header         { return n.header }
func (n *noFlushWriter) Write(p []byte) (int, error) { return n.buf.Write(p) }
func (n *noFlushWriter) WriteHeader(int)             {}

func TestR493r_WriterWithoutFlusherWritesNothing(t *testing.T) {
	resetLastStreamInfo(t)
	w := &noFlushWriter{header: http.Header{}}
	writeToolCallsStream(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "test-model", "chatcmpl-3", 1731000000, nil, "", "p", false)
	if w.buf.Len() != 0 {
		t.Errorf("без http.Flusher поток не должен отдаваться, а записано %d байт: %q", w.buf.Len(), w.buf.String())
	}
	if info := getLastStreamInfo(); info != nil {
		t.Errorf("ложный обрыв в диагностике без Flusher: reason=%q handler=%q", info.Reason, info.Handler)
	}
}

// TestR493r_HeadPlusBodyIsOneWrite — тело и заголовки идут через один writer:
// голова пишется через WriteHeader/SetHeader, а не напрямую в http.ResponseWriter.
// Иначе обёртка (middleware_recover) снова сможет обойти учёт.
func TestR493r_StaticStreamHeadersGoThroughWriter(t *testing.T) {
	resetLastStreamInfo(t)
	w := newBrokenClientWriter(1 << 20)
	writeStaticTextStream(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "test-model", "chatcmpl-4", 1731000000, "", "ok", "p", false)
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if w.status != http.StatusOK {
		t.Errorf("status = %d, want 200", w.status)
	}
}

var _ http.Flusher = (*brokenClientWriter)(nil)
var _ http.ResponseWriter = (*brokenClientWriter)(nil)
var _ io.Writer = (*brokenClientWriter)(nil)
