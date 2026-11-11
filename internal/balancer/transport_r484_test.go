// transport_r484_test.go — R484: фактический транспорт в диагностике.
//
// stream в прокси-логах — ФЛАГ запроса, а не транспорт. Запрос без stream на
// legacy-поверхности считается стримовым, но транспорт llama.cpp уходит в
// non-stream; оператор видел stream=true и искал причину «всё одним куском» не
// там. Теперь рядом лежит transport: "stream" | "json".
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/logger"
)

// TestR484_TransportTracker — запись/чтение фактического транспорта.
func TestR484_TransportTracker(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	req = withTransportTrackerR484(req)
	if got := TransportR484(req.Context()); got != "" {
		t.Fatalf("до установки transport = %q, ожидалось пусто", got)
	}
	SetTransportR484(req.Context(), transportStreamR484)
	if got := TransportR484(req.Context()); got != "stream" {
		t.Fatalf("transport = %q, ожидалось stream", got)
	}
	SetTransportR484(req.Context(), transportJSONR484)
	if got := TransportR484(req.Context()); got != "json" {
		t.Fatalf("transport = %q, ожидалось json", got)
	}
}

// TestR484_TransportTrackerWithoutAttachNoPanic — без трекера вызовы безопасны
// (многие юнит-тесты дергают транспортные функции напрямую).
func TestR484_TransportTrackerWithoutAttachNoPanic(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	SetTransportR484(req.Context(), transportStreamR484)
	if got := TransportR484(req.Context()); got != "" {
		t.Fatalf("без трекера transport = %q, ожидалось пусто", got)
	}
}

// TestR484_TraceLineCarriesTransport — строка трассировки печатает транспорт.
func TestR484_TraceLineCarriesTransport(t *testing.T) {
	buf := logger.NewCaptureBuffer()
	restore := logger.CaptureToBuffer(buf)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	req = withTransportTrackerR484(req)
	SetTransportR484(req.Context(), transportStreamR484)
	rec := httptest.NewRecorder()
	w, finish := startRequestTraceR476(rec, req, "rid-transport-484")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("chunk"))
	finish()

	out := buf.String()
	if !strings.Contains(out, `"transport":"stream"`) && !strings.Contains(out, "transport=stream") {
		t.Fatalf("в трассировке нет transport=stream: %s", out)
	}
}

// TestR484_ProxyLogEntryCarriesTransport — контракт /api/v1/proxy/logs.
func TestR484_ProxyLogEntryCarriesTransport(t *testing.T) {
	raw, err := json.Marshal(ProxyLogEntry{
		Method: "POST", Path: "/api/chat", StatusCode: 200,
		RequestID: "req-484", Transport: "json",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"transport":"json"`) {
		t.Fatalf("в JSON прокси-лога нет transport: %s", raw)
	}
}
