// request_trace_r476.go — R476 (2026-11-07): одна строка на каждый запрос,
// по которой этот запрос можно найти в логах обеих сторон.
//
// ЧТО БЫЛО. Балансер уже выдавал correlation-id (requestid.go), клал его в
// контекст, отдавал клиенту заголовком X-Request-ID и передавал воркеру
// (R376). Воркер даже писал его в лог старта. Но НАЙТИ по нему ход было
// нельзя: у балансера не было ни одной строки уровня INFO с длительностью и
// кодом ответа — только Debug «request received», который в проде не виден,
// а у воркера в сводке хода (/api/chat stream response summary) id не было
// вовсе. Следствие — разбор дефекта превращался в сопоставление по времени.
//
// ЧТО ЗДЕСЬ. Обёртка над http.ResponseWriter считает код ответа, объём и
// длительность, а на выходе пишет одну строку INFO:
//
//	request trace request_id=… method=POST path=/api/chat status=200
//	              duration_ms=64201 bytes=18244 backend=cppworker-gpu
//
// backend берётся из заголовка X-Backend-ID, который обработчики уже ставят
// (менять их не нужно). Обёртка обязана сохранять Flusher/Hijacker/Unwrap —
// иначе ломаются стриминговые ответы (SSE/NDJSON) и hijacked-стримы.
//
// Управление: LB_REQUEST_TRACE=off|summary|full (по умолчанию summary).
//   - off     — строки нет (заголовок X-Request-ID остаётся);
//   - summary — одна строка на запрос;
//   - full    — summary + DEBUG-строки о старте (размер тела, клиент).
package balancer

import (
	"bufio"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// requestTraceDefault — режим по умолчанию: одна строка на запрос. Это дешевле,
// чем потом гадать по времени, и не забивает лог построчными подробностями.
const requestTraceDefault = "summary"

// requestTraceModeR476 — режим трассировки из LB_REQUEST_TRACE.
func requestTraceModeR476() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("LB_REQUEST_TRACE")))
	switch raw {
	case "off", "0", "false", "no":
		return "off"
	case "full":
		return "full"
	case "", "summary", "on", "1", "true", "yes":
		return requestTraceDefault
	default:
		// Неизвестное значение не должно молча выключать трассировку.
		return requestTraceDefault
	}
}

// traceStateR476 — то, что считаем про ответ (код, байты).
//
// R478 (2026-11-09): состояние вынесено из обёртки, потому что обёрток ДВЕ.
// Было: один тип объявлял Hijack() ВСЕГДА, даже когда нижний writer перехват не
// умеет. Из-за этого код, проверяющий `w.(http.Hijacker)`, шёл в hijack-ветку и
// получал ErrNotSupported вместо Plain-фолбэка (живой риск:
// preflight_stream_dialog.go — диалог ожидания перезагрузки просто не
// показывался). Теперь тип обёртки выбирается по возможностям нижнего writer.
type traceStateR476 struct {
	status int
	bytes  int64
}

func (s *traceStateR476) onWriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
}

func (s *traceStateR476) onWrite(n int) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	s.bytes += int64(n)
}

// traceRecorderR476 — базовая обёртка: код, байты, Flush, Unwrap. Без Hijack.
type traceRecorderR476 struct {
	http.ResponseWriter
	state *traceStateR476
}

func (t *traceRecorderR476) WriteHeader(code int) {
	t.state.onWriteHeader(code)
	t.ResponseWriter.WriteHeader(code)
}

func (t *traceRecorderR476) Write(p []byte) (int, error) {
	n, err := t.ResponseWriter.Write(p)
	t.state.onWrite(n)
	return n, err
}

// Flush — сохранить стриминг (SSE/NDJSON).
func (t *traceRecorderR476) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap — для http.ResponseController в Go 1.20+.
func (t *traceRecorderR476) Unwrap() http.ResponseWriter {
	return t.ResponseWriter
}

// traceRecorderHijackR476 — обёртка для ResponseWriter, который УМЕЕТ hijack:
// тип добавляет метод Hijack() честно, только когда перехват реально доступен.
type traceRecorderHijackR476 struct {
	traceRecorderR476
}

// Hijack — перехват соединения для hijacked-стримов.
func (t *traceRecorderHijackR476) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// startRequestTraceR476 — включить трассировку запроса.
//
// Возвращает обёрнутый ResponseWriter (его и надо передавать дальше) и функцию,
// которую вызывающий ОБЯЗАН вызвать при выходе (defer). Возвращаемая функция
// приходит и как http.Handler-обёртка не нужна: привязки к горутине нет.
func startRequestTraceR476(w http.ResponseWriter, r *http.Request, rid string) (http.ResponseWriter, func()) {
	mode := requestTraceModeR476()
	if mode == "off" {
		return w, func() {}
	}
	start := time.Now()
	state := &traceStateR476{}
	var rec http.ResponseWriter
	if _, ok := w.(http.Hijacker); ok {
		rec = &traceRecorderHijackR476{traceRecorderR476{ResponseWriter: w, state: state}}
	} else {
		rec = &traceRecorderR476{ResponseWriter: w, state: state}
	}
	if mode == "full" && logger.Get() != nil {
		logger.Get().Debugw("request trace: started",
			"request_id", rid, "method", r.Method, "path", r.URL.Path,
			"client", traceClientAddrR476(r), "content_length", r.ContentLength)
	}
	return rec, func() {
		status := state.status
		if status == 0 {
			status = http.StatusOK
		}
		// X-Backend-ID ставят обработчики; если запрос до них не дошёл — пусто.
		backend := strings.TrimSpace(rec.Header().Get("X-Backend-ID"))
		// R477: стадия, на которой ход находится (для 5xx — где сломался).
		stage, stageAt := CurrentStageR477(r.Context())
		stageMs := int64(0)
		if !stageAt.IsZero() {
			stageMs = time.Since(stageAt).Milliseconds()
		}
		// R479: «дошло ли до клиента» — отдельное измерение, не равное коду 200.
		// Оборванный стрим (клиент отключился, контекст отменён) раньше выглядел
		// как успешный ответ и в диагностику не попадал.
		abort := AbortReasonR477(r.Context())
		// R479: финальная стадия. stage выше говорит, ГДЕ ход сейчас/умер;
		// stage_final — дошёл ли он до конца: done только при успехе без обрыва.
		stageFinal := stage
		if status < http.StatusBadRequest && abort == "" {
			stageFinal = stageDoneR477
		}
		logger.Get().Infow("request trace",
			"request_id", rid, "method", r.Method, "path", r.URL.Path,
			"status", status, "duration_ms", time.Since(start).Milliseconds(),
			"bytes", state.bytes, "backend", backend, "client", traceClientAddrR476(r),
			"stage", stage, "stage_final", stageFinal, "stage_ms", stageMs)
		if abort != "" {
			logger.Get().Warnw("request trace: доставка оборвана",
				"request_id", rid, "path", r.URL.Path, "stage", stage,
				"bytes", state.bytes, "status", status, "reason", abort)
		}
		// R477: сбой (5xx) или обрыв доставки попадают в кольцо — его видно в
		// сводке и в архиве, и по нему понятно, какой компонент не отдал ответ.
		reason := "http " + strconv.Itoa(status)
		if abort != "" {
			reason = abort
		}
		if status >= http.StatusInternalServerError || abort != "" {
			recordFailureR477(diagFailureR477{
				RequestID: rid, Method: r.Method, Path: r.URL.Path,
				Stage: stage, Status: status, Backend: backend,
				Reason: reason,
			})
		}
	}
}

// traceClientAddrR476 — адрес клиента (X-Forwarded-For первый, иначе RemoteAddr).
func traceClientAddrR476(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	return r.RemoteAddr
}
