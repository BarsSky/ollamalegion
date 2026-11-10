// request_trace_first_byte_r483_test.go — R483: доказать, ГДЕ копился ответ.
//
// Живая жалоба (второй стенд, Qwen3.8-27B): «не отдаёт сгенерированное, пока не
// сделает всё одним огромным куском». По duration_ms этого не видно: он
// одинаков и для потокового хода, и для «всё в конце». first_byte_ms показывает,
// когда клиенту ушёл ПЕРВЫЙ байт: близко к duration_ms — держал воркер (или
// балансер буферизовал), мало — отдавали сразу, вопрос к клиенту/сети.
package balancer

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// TestR483_TraceStateFirstByte — момент первого байта фиксируется один раз.
func TestR483_TraceStateFirstByte(t *testing.T) {
	s := &traceStateR476{}
	s.onWrite(0)
	if !s.firstByteAt.IsZero() {
		t.Fatal("нулевая запись не должна считаться первым байтом")
	}
	s.onWrite(10)
	first := s.firstByteAt
	if first.IsZero() {
		t.Fatal("первый байт не зафиксирован")
	}
	time.Sleep(2 * time.Millisecond)
	s.onWrite(10)
	if !s.firstByteAt.Equal(first) {
		t.Fatal("время первого байта перезаписано вторым Write")
	}
}

// TestR483_TraceLineCarriesFirstByteMs — строка трассировки печатает first_byte_ms.
func TestR483_TraceLineCarriesFirstByteMs(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.CaptureToBuffer(&buf)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()
	w, finish := startRequestTraceR476(rec, req, "rid-first-byte-483")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("chunk"))
	finish()

	out := buf.String()
	if !strings.Contains(out, "request trace") {
		t.Fatalf("нет строки трассировки: %s", out)
	}
	if !strings.Contains(out, `"first_byte_ms"`) && !strings.Contains(out, "first_byte_ms=") {
		t.Fatalf("в трассировке нет first_byte_ms: %s", out)
	}
	if strings.Contains(out, `"first_byte_ms":-1`) {
		t.Fatalf("first_byte_ms = -1 после успешной записи: %s", out)
	}
}

// TestR483_TraceLineFirstByteMinusOneWhenNothingWritten — клиенту не ушло ни
// байта (отказ до записи): first_byte_ms = -1, а не 0 (0 значил бы «сразу»).
func TestR483_TraceLineFirstByteMinusOneWhenNothingWritten(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.CaptureToBuffer(&buf)
	defer restore()

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rec := httptest.NewRecorder()
	_, finish := startRequestTraceR476(rec, req, "rid-no-byte-483")
	finish()

	out := buf.String()
	if !strings.Contains(out, `"first_byte_ms":-1`) && !strings.Contains(out, "first_byte_ms=-1") {
		t.Fatalf("без записи ожидался first_byte_ms=-1: %s", out)
	}
}
