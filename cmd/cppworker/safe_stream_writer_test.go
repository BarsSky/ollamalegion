// safe_stream_writer_test.go — тесты для safeStreamWriter.
//
// Покрывают:
//   - безопасную проверку ctx.Done() (не пишем в мёртвый socket);
//   - обнаружение write error и переход в broken state;
//   - корректный snapshot в LastStreamInfo (для /api/v1/cppworker/debug/last-stream);
//   - потокобезопасность (concurrent writes).
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeFlusherResponseWriter — заглушка http.ResponseWriter с Flusher и
// возможностью симулировать write error.
type fakeFlusherResponseWriter struct {
	header     http.Header
	written    []byte
	writeErr   error
	flushed    int32
	statusCode int
}

func newFakeWriter() *fakeFlusherResponseWriter {
	return &fakeFlusherResponseWriter{
		header:     http.Header{},
		statusCode: 200,
	}
}

func (f *fakeFlusherResponseWriter) Header() http.Header {
	return f.header
}

func (f *fakeFlusherResponseWriter) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.written = append(f.written, p...)
	return len(p), nil
}

func (f *fakeFlusherResponseWriter) WriteHeader(status int) {
	f.statusCode = status
}

func (f *fakeFlusherResponseWriter) Flush() {
	atomic.AddInt32(&f.flushed, 1)
}

func newRequestWithCtx(ctx context.Context) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	return r
}

// TestSafeStreamWriter_ContextCanceled — после ctx.Done() Writef/Flush возвращают false.
func TestSafeStreamWriter_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // сразу отменяем

	fw := newFakeWriter()
	r := newRequestWithCtx(ctx)
	sw := newSafeStreamWriter(fw, r, "test", "model-x")

	if sw.Writef("data: %s\n\n", "hello") {
		t.Error("expected Writef to return false when ctx is cancelled")
	}
	if !sw.IsBroken() {
		t.Error("expected writer to be marked broken")
	}
	if fw.statusCode != 200 {
		t.Errorf("expected status code unchanged, got %d", fw.statusCode)
	}
}

// TestSafeStreamWriter_WriteError — write error помечает writer broken + snapshot.
func TestSafeStreamWriter_WriteError(t *testing.T) {
	fw := newFakeWriter()
	fw.writeErr = errors.New("broken pipe")

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	sw := newSafeStreamWriter(fw, r, "test", "model-x")

	if sw.Writef("hello") {
		t.Error("expected Writef to return false on write error")
	}
	if !sw.IsBroken() {
		t.Error("expected writer to be marked broken after write error")
	}

	// Snapshot должен быть записан.
	snap := getLastStreamInfo()
	if snap == nil {
		t.Fatal("expected LastStreamInfo to be recorded")
	}
	if snap.Reason != "write_error" {
		t.Errorf("expected reason=write_error, got %q", snap.Reason)
	}
	if snap.LastWriteErr != "broken pipe" {
		t.Errorf("expected LastWriteErr=broken pipe, got %q", snap.LastWriteErr)
	}
	if snap.WriteErrors != 1 {
		t.Errorf("expected WriteErrors=1, got %d", snap.WriteErrors)
	}

	// Cleanup snapshot для следующих тестов.
	lastStreamMu.Lock()
	lastStream = nil
	lastStreamMu.Unlock()
}

// TestSafeStreamWriter_NoOpAfterBroken — после первого write error все операции no-op.
func TestSafeStreamWriter_NoOpAfterBroken(t *testing.T) {
	fw := newFakeWriter()
	fw.writeErr = errors.New("connection reset")

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	sw := newSafeStreamWriter(fw, r, "test", "model-x")

	// Первая операция — провалится, broken=true.
	if sw.Writef("first") {
		t.Error("expected first write to fail")
	}

	// Reset writeErr — но writer всё ещё broken, ничего не пишем.
	fw.writeErr = nil
	if sw.Writef("second") {
		t.Error("expected second write to return false (writer broken)")
	}
	if len(fw.written) != 0 {
		t.Errorf("expected no bytes written, got %d", len(fw.written))
	}

	// Cleanup.
	lastStreamMu.Lock()
	lastStream = nil
	lastStreamMu.Unlock()
}

// TestSafeStreamWriter_ConcurrentWrites — 10 горутин пишут параллельно,
// bytesWritten == sum.
func TestSafeStreamWriter_ConcurrentWrites(t *testing.T) {
	fw := newFakeWriter()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	sw := newSafeStreamWriter(fw, r, "test", "model-x")

	const goroutines = 10
	const writesPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < writesPerGoroutine; i++ {
				sw.Writef("goroutine %d write %d\n", gid, i)
			}
		}(g)
	}
	wg.Wait()

	expectedBytes := goroutines * writesPerGoroutine * 30 // приблизительно
	if len(fw.written) == 0 {
		t.Errorf("expected bytes written, got 0")
	}
	if sw.IsBroken() {
		t.Error("writer should not be broken after successful writes")
	}
	t.Logf("concurrent writes: %d bytes (expected ≥ %d)", len(fw.written), expectedBytes)
}

// TestSafeStreamWriter_HeaderBeforeBody — SetHeader после WriteHeader возвращает false.
func TestSafeStreamWriter_HeaderBeforeBody(t *testing.T) {
	fw := newFakeWriter()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	sw := newSafeStreamWriter(fw, r, "test", "model-x")

	if !sw.SetHeader("Content-Type", "text/event-stream") {
		t.Error("SetHeader before WriteHeader should succeed")
	}
	if !sw.WriteHeader(http.StatusOK) {
		t.Error("WriteHeader should succeed")
	}
	if sw.SetHeader("X-Late-Header", "value") {
		t.Error("SetHeader after WriteHeader should fail")
	}
	if !sw.Writef("hello") {
		t.Error("Writef after WriteHeader should succeed")
	}
}

// TestSafeStreamWriter_ContextCanceledDuringWrite — ctx отменяется между WriteHeader и Writef.
func TestSafeStreamWriter_ContextCanceledDuringWrite(t *testing.T) {
	fw := newFakeWriter()
	ctx, cancel := context.WithCancel(context.Background())
	r := newRequestWithCtx(ctx)
	sw := newSafeStreamWriter(fw, r, "test", "model-x")

	if !sw.WriteHeader(http.StatusOK) {
		t.Error("WriteHeader should succeed")
	}
	cancel() // отменяем после WriteHeader

	if sw.Writef("after cancel") {
		t.Error("expected Writef to return false after ctx cancel")
	}
	if !sw.IsBroken() {
		t.Error("writer should be broken after ctx cancel")
	}

	// Cleanup.
	lastStreamMu.Lock()
	lastStream = nil
	lastStreamMu.Unlock()
}

// TestSafeStreamWriter_FlushNoFlusher — Flush без Flusher'а не паникует.
func TestSafeStreamWriter_FlushNoFlusher(t *testing.T) {
	// http.ResponseWriter без Flusher.
	rw := &nonFlusherWriter{header: http.Header{}}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	sw := newSafeStreamWriter(rw, r, "test", "model-x")

	// Не должно паниковать.
	sw.Flush()
	if sw.IsBroken() {
		t.Error("Flush without flusher should not mark writer as broken")
	}
}

// nonFlusherWriter — http.ResponseWriter без метода Flush (не реализует http.Flusher).
type nonFlusherWriter struct {
	header     http.Header
	written    []byte
	statusCode int
}

func (w *nonFlusherWriter) Header() http.Header        { return w.header }
func (w *nonFlusherWriter) Write(p []byte) (int, error) { w.written = append(w.written, p...); return len(p), nil }
func (w *nonFlusherWriter) WriteHeader(status int)      { w.statusCode = status }

// TestSafeStreamWriter_HeartbeatContext — keepalive пишет только если ctx не отменён.
func TestSafeStreamWriter_HeartbeatContext(t *testing.T) {
	fw := newFakeWriter()
	ctx, cancel := context.WithCancel(context.Background())
	r := newRequestWithCtx(ctx)
	sw := newSafeStreamWriter(fw, r, "test", "model-x")

	// Пишем первый heartbeat.
	if !sw.Writef(": keepalive\n\n") {
		t.Error("first heartbeat should succeed")
	}

	cancel()

	// Второй heartbeat должен провалиться.
	if sw.Writef(": keepalive\n\n") {
		t.Error("second heartbeat should fail after ctx cancel")
	}
	if !sw.IsBroken() {
		t.Error("writer should be broken")
	}

	// Cleanup.
	lastStreamMu.Lock()
	lastStream = nil
	lastStreamMu.Unlock()
}

// TestSafeStreamWriter_KeepaliveDoesNotDoubleWrite — повторный Writef с тем же
// broken writer не пишет в w (проверка через bytesWritten counter).
func TestSafeStreamWriter_KeepaliveDoesNotDoubleWrite(t *testing.T) {
	fw := newFakeWriter()
	ctx, cancel := context.WithCancel(context.Background())
	r := newRequestWithCtx(ctx)
	sw := newSafeStreamWriter(fw, r, "test", "model-x")

	// Пишем данные.
	for i := 0; i < 5; i++ {
		sw.Writef("data %d\n", i)
	}

	bytesBeforeCancel := len(fw.written)

	cancel()

	// После cancel — Writef возвращает false, ничего не пишет.
	for i := 0; i < 10; i++ {
		sw.Writef("after cancel %d\n", i)
	}

	if len(fw.written) != bytesBeforeCancel {
		t.Errorf("expected bytes_written=%d (unchanged), got %d",
			bytesBeforeCancel, len(fw.written))
	}

	// Cleanup.
	lastStreamMu.Lock()
	lastStream = nil
	lastStreamMu.Unlock()
}

// TestSafeStreamWriter_RealisticStreaming — реалистичный сценарий: пишем 50 токенов,
// отменяем контекст на 25-м, проверяем что writer broken.
func TestSafeStreamWriter_RealisticStreaming(t *testing.T) {
	fw := newFakeWriter()
	ctx, cancel := context.WithCancel(context.Background())
	r := newRequestWithCtx(ctx)
	sw := newSafeStreamWriter(fw, r, "test", "model-x")

	const totalTokens = 50
	for i := 0; i < totalTokens; i++ {
		if i == 25 {
			cancel()
		}
		sw.Writef("token_%d ", i)
		sw.Flush()
		sw.IncTokens(1)
	}

	if !sw.IsBroken() {
		t.Error("writer should be broken after ctx cancel")
	}

	output := string(fw.written)
	if !strings.Contains(output, "token_0 ") || !strings.Contains(output, "token_24 ") {
		t.Error("expected early tokens to be written")
	}
	if strings.Contains(output, "token_49 ") {
		t.Error("expected late tokens NOT to be written (writer broken)")
	}

	t.Logf("wrote %d bytes from %d total tokens (partial stream)", len(fw.written), totalTokens)

	// Cleanup.
	lastStreamMu.Lock()
	lastStream = nil
	lastStreamMu.Unlock()
}