// request_id_logs_r476_test.go — R476 (2026-11-07): request_id в логах решения.
//
// ЗАЧЕМ. Пользователь просил, чтобы «ход» можно было найти по одному id. Строка
// `request trace` на запрос и `request_id` в сводке воркера есть, но решения об
// окне (preflight, рост, точный подсчёт) печатались вообще без id — то есть
// найти в логе, ПОЧЕМУ ход перезагрузил модель, было нельзя. Тест держит эти
// поля: если их убрать, разбор снова вернётся к сопоставлению по времени.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/logger"
)

// TestR476_GrowthDecisionLogCarriesRequestID — решение R301 несёт request_id.
func TestR476_GrowthDecisionLogCarriesRequestID(t *testing.T) {
	buf := logger.NewCaptureBuffer()
	restore := logger.CaptureToBuffer(buf)
	defer restore()

	v := growthVerdictR301{
		RID:       "rid-growth-476",
		Defer:     true,
		Streaming: true,
		Estimated: 1000,
		Needed:    2000,
		Loaded:    1024,
	}
	logGrowthVerdictR301("test", "bk-1", "m", "/api/chat", v)

	out := buf.String()
	if !strings.Contains(out, "context growth decision (R301)") {
		t.Fatalf("строка решения не попала в лог: %s", out)
	}
	if !strings.Contains(out, "request_id=rid-growth-476") &&
		!strings.Contains(out, `"request_id":"rid-growth-476"`) &&
		!strings.Contains(out, `request_id\":\"rid-growth-476`) {
		t.Fatalf("в решении о росте нет request_id: %s", out)
	}
}

// TestR476_PreflightInputsLogCarriesRequestID — вход preflight несёт request_id
// из RequestMeta (его ставит вызывающий, у которого есть *http.Request).
func TestR476_PreflightInputsLogCarriesRequestID(t *testing.T) {
	buf := logger.NewCaptureBuffer()
	restore := logger.CaptureToBuffer(buf)
	defer restore()

	meta := &RequestMeta{
		RequestID:             "rid-preflight-476",
		ModelName:             "m",
		EstimatedPromptTokens: 1000,
	}
	DecidePreflight(meta, r451State(), DefaultNCtxReloadConfig())

	out := buf.String()
	if !strings.Contains(out, "preflight: DecidePreflight inputs") {
		t.Fatalf("строка preflight не попала в лог: %s", out)
	}
	if !strings.Contains(out, "rid-preflight-476") {
		t.Fatalf("во входе preflight нет request_id: %s", out)
	}
}

// TestR476_GrowthVerdictKeepsRID — id доезжает до структуры вердикта: его
// используют и лог, и запись чисел в тело отказа (R447).
func TestR476_GrowthVerdictKeepsRID(t *testing.T) {
	p := newGrowthGuardProxyR299()
	v := p.growthVerdictR301("rid-kept-476", "bk-1", "m", []byte(r301StreamBody), "/api/chat", true)
	if v.RID != "rid-kept-476" {
		t.Fatalf("verdict.RID = %q, ожидался id запроса", v.RID)
	}
}

// TestR476_TraceLineCarriesBackend — в строке `request trace` есть бэкенд:
// для нативного /api/* его выставляет транспорт, для OpenAI-путей — обработчик.
func TestR476_TraceLineCarriesBackend(t *testing.T) {
	buf := logger.NewCaptureBuffer()
	restore := logger.CaptureToBuffer(buf)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	rec := httptest.NewRecorder()
	w, finish := startRequestTraceR476(rec, req, "rid-backend-476")
	w.Header().Set("X-Backend-ID", "cppworker-gpu")
	w.WriteHeader(http.StatusOK)
	finish()

	out := buf.String()
	if !strings.Contains(out, "request trace") {
		t.Fatalf("строки трассировки нет: %s", out)
	}
	if !strings.Contains(out, "cppworker-gpu") {
		t.Fatalf("в трассировке нет бэкенда: %s", out)
	}
	if !strings.Contains(out, "rid-backend-476") {
		t.Fatalf("в трассировке нет request_id: %s", out)
	}
}

// hijacklessWriterR478 — ResponseWriter с Flush, но БЕЗ Hijack: так ведёт себя
// writer под HTTP/2 (или любой прокси без перехвата соединения).
type hijacklessWriterR478 struct {
	http.ResponseWriter
	flushed bool
}

func (w *hijacklessWriterR478) Flush() { w.flushed = true }

// TestR478_WrapperDoesNotClaimHijackWhenUnderlyingCannot — обёртка обязана
// объявлять только те интерфейсы, которые есть у нижнего writer.
//
// ЖИВОЙ РИСК (нашёл аудит): раньше traceRecorderR476 имел метод Hijack всегда,
// поэтому `w.(http.Hijacker)` был истинен и для HTTP/2. Код диалога ожидания
// перезагрузки (preflight_stream_dialog.go) шёл в hijack-ветку, получал
// ErrNotSupported и вместо Plain-фолбэка отдавал 503.
func TestR478_WrapperDoesNotClaimHijackWhenUnderlyingCannot(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	under := &hijacklessWriterR478{ResponseWriter: httptest.NewRecorder()}
	w, finish := startRequestTraceR476(under, req, "rid-hijack-478")
	defer finish()

	if _, ok := w.(http.Hijacker); ok {
		t.Fatal("обёртка объявляет Hijack(), хотя нижний writer перехват не умеет")
	}
	if _, ok := w.(http.Flusher); !ok {
		t.Fatal("обёртка потеряла Flush — стриминг сломается")
	}
}

// TestR478_WrapperKeepsHijackWhenUnderlyingCan — и наоборот: если нижний writer
// умеет hijack, обёртка обязана его сохранять (h2c/HTTP1.1 стримы).
func TestR478_WrapperKeepsHijackWhenUnderlyingCan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped, finish := startRequestTraceR476(w, r, "rid-hijack-ok-478")
		defer finish()
		if _, ok := wrapped.(http.Hijacker); !ok {
			t.Error("обёртка потеряла Hijack у hijack-способного writer")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}
	_ = resp.Body.Close()
}
