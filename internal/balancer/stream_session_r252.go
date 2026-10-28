package balancer

// stream_session_r252.go — R252 (2026-10-28): ОДИН вход в стрим и гарантия [DONE].
//
// ЖИВОЙ ДЕФЕКТ, который это лечит (Cline, длинная серия tool-call'ов):
//
//  1. «Клиент виден как живой» (флаг HasActiveStream + heartbeat сессии + запись
//     в recentClients монитора) размазывалось по трём местам и различалось:
//     native-путь (streaming.go) делал всё, hijack-путь и транспорт llama.cpp —
//     ничего (исправлено в R251, но КОПИЕЙ кода в двух файлах). Копия — это
//     ровно то, из-за чего на следующем новом пути снова забудут: третий
//     стриминговый тракт появится и опять будет невидим в панели.
//  2. Завершение потока различалось: legacy-путь писал `data: [DONE]`, а
//     основной (hijack) — только chunked-terminator. Если upstream закрывался,
//     не прислав [DONE]/finish_reason, клиент видел «поток просто закрылся» и
//     трактовал ответ как оборванный (Cline/Roo: пустой ответ или ошибка).
//
// Здесь два помощника, которые обязаны использовать ВСЕ SSE-пути:
//   - startStreamSession: сессия/монитор на время стрима, Stop() идемпотентен;
//   - ensurePlainSSEDone / ensureChunkedSSEDone: дописать ровно один
//     `data: [DONE]`, если последний отданный кадр им не был.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// sseDoneFrame — кадр завершения OpenAI SSE (с пустой строкой-разделителем).
const sseDoneFrame = "data: [DONE]\n\n"

// defaultStreamHeartbeatInterval — R252.1: интервал heartbeat, когда конфиг
// недоступен (nil-прокси/пустой конфиг — тесты и лёгкие сценарии).
//
// ПОЧЕМУ ЯВНАЯ КОНСТАНТА. Тикер живёт в горутине и звал p.getHeartbeatInterval(),
// который читает p.config.Balancing без проверки: у прокси без конфига это паника
// в горутине, а она роняет весь процесс (recover обработчика её не ловит).
const defaultStreamHeartbeatInterval = 15 * time.Second

// streamHeartbeatIntervalSafe — интервал heartbeat без паники на nil-конфиге.
func (p *Proxy) streamHeartbeatIntervalSafe() time.Duration {
	if p == nil || p.config == nil {
		return defaultStreamHeartbeatInterval
	}
	if d := p.getHeartbeatInterval(); d > 0 {
		return d
	}
	return defaultStreamHeartbeatInterval
}

// streamSession — «клиент сейчас стримит» на время одного ответа.
//
// Поля намеренно приватные: снаружи нужен только Stop() (и SessionID() для
// логов). Всё остальное — деталь реализации, чтобы новые пути не начинали
// трогать sessionMgr напрямую и не расходились в поведении.
type streamSession struct {
	p   *Proxy
	r   *http.Request
	id  string
	ch  chan struct{}
	wg  sync.WaitGroup
	mu  sync.Mutex
	off bool
	// armed — bookkeeping реально включён (конфиг есть и метки выставлены).
	// Stop() снимает только то, что ставил: иначе на nil-конфиге он повторно
	// заходил в getClientRealIP и паниковал (R252.1, nil-guard).
	armed bool
}

// startStreamSession — отметить начало стрима: сессия активна, клиент виден в
// мониторе, heartbeat каждые getHeartbeatInterval().
//
// Возвращает всегда непустой объект (даже при nil-прокси/запросе): Stop() тогда
// просто ничего не делает. Это защищает вызывающие пути от nil-проверок в
// горячем коде — цена ошибки здесь выше, чем цена одного no-op.
func startStreamSession(p *Proxy, r *http.Request, model string) *streamSession {
	s := &streamSession{p: p, r: r, ch: make(chan struct{})}
	if p == nil || r == nil {
		return s
	}
	// R252.1: без конфига прокси не может ни посчитать IP клиента
	// (getClientRealIP -> config.LoadBalancer), ни решить про stickiness. Раньше
	// nil-конфиг ронял горутину тикера — то есть процесс.
	if p.config == nil {
		return s
	}

	if p.config.Balancing.SessionStickiness && p.sessionMgr != nil {
		s.id = p.getSessionIDWithModel(r, p.getClientName(r), model)
		if s.id != "" {
			p.sessionMgr.SetStreamActive(s.id, true)
		}
	}
	p.markRecentClientStreamActive(r, true)
	s.armed = true

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(p.streamHeartbeatIntervalSafe())
		defer ticker.Stop()
		for {
			select {
			case <-s.ch:
				return
			case <-ticker.C:
				// Обновляем и сессию (idle TTL), и запись монитора (60-секундное
				// окно recentClients): обе живут по «простою» и обе умирали во
				// время одной длинной генерации.
				if s.id != "" && p.sessionMgr != nil {
					p.sessionMgr.HeartbeatStream(s.id)
				}
				p.markRecentClientStreamActive(r, true)
			}
		}
	}()
	return s
}

// SessionID — id сессии, к которой отнесён стрим ("" — сессии нет).
func (s *streamSession) SessionID() string {
	if s == nil {
		return ""
	}
	return s.id
}

// Stop — снять флаги и погасить heartbeat. Идемпотентен: повторный вызов (в том
// числе из defer после явного Stop) безопасен и ничего не делает.
func (s *streamSession) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.off {
		s.mu.Unlock()
		return
	}
	s.off = true
	s.mu.Unlock()

	close(s.ch)
	s.wg.Wait()

	if s.p == nil || !s.armed {
		return
	}
	if s.id != "" && s.p.sessionMgr != nil {
		s.p.sessionMgr.SetStreamActive(s.id, false)
	}
	if s.r != nil {
		s.p.markRecentClientStreamActive(s.r, false)
	}
}

// isSSEDoneLine — эта строка уже является кадром `data: [DONE]`?
//
// Переносы/пробелы по краям допускаются: hijack-путь дописывает к data-строке
// `\n` (восстановление границы SSE-события), legacy-путь пишет как есть.
func isSSEDoneLine(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	const prefix = "data:"
	if !bytes.HasPrefix(trimmed, []byte(prefix)) {
		return false
	}
	payload := bytes.TrimSpace(trimmed[len(prefix):])
	return bytes.Equal(payload, []byte("[DONE]"))
}

// ensurePlainSSEDone — дописать `data: [DONE]`, если последний кадр им не был.
//
// Для путей, которые пишут в обычный http.ResponseWriter (legacy SSE-цикл).
// Возвращает true, если кадр реально записан (нужно для тестов и логов).
func ensurePlainSSEDone(w io.Writer, lastLine []byte) bool {
	if w == nil || isSSEDoneLine(lastLine) {
		return false
	}
	if _, err := io.WriteString(w, sseDoneFrame); err != nil {
		return false
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return true
}

// ensureChunkedSSEDone — то же для hijack-пути, где клиенту отдаётся ручной
// chunked-стрим (обычная запись в conn сломала бы framing).
func ensureChunkedSSEDone(bufrw *bufio.ReadWriter, lastLine []byte) bool {
	if bufrw == nil || isSSEDoneLine(lastLine) {
		return false
	}
	if err := writeChunkedString(bufrw, sseDoneFrame); err != nil {
		return false
	}
	return true
}

// sseErrorFrame — SSE-кадр ошибки в форме OpenAI (клиент покажет причину, а не
// «пустой ответ»).
func sseErrorFrame(message string) string {
	payload, _ := json.Marshal(map[string]string{"error": message})
	return "data: " + string(payload) + "\n\n"
}

// writeChunkedStreamError — R252.1: error-кадр + РОВНО ОДИН `data: [DONE]` для
// hijack-пути.
//
// ЖИВОЙ ДЕФЕКТ. При ошибке чтения upstream этот путь писал error-кадр и сразу
// выходил: [DONE] клиент не получал вовсе, и OpenAI-совместимые клиенты
// трактовали ответ как оборванный (Cline/Roo: пустой ответ или остановка задачи),
// хотя причина была известна и уже отправлена.
func writeChunkedStreamError(bufrw *bufio.ReadWriter, message string) bool {
	if bufrw == nil {
		return false
	}
	frame := sseErrorFrame(message)
	if err := writeChunkedString(bufrw, frame); err != nil {
		return false
	}
	ensureChunkedSSEDone(bufrw, []byte(frame))
	return true
}

// writePlainStreamError — то же для legacy-пути (обычный http.ResponseWriter).
func writePlainStreamError(w io.Writer, message string) bool {
	if w == nil {
		return false
	}
	frame := sseErrorFrame(message)
	if _, err := io.WriteString(w, frame); err != nil {
		return false
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	ensurePlainSSEDone(w, []byte(frame))
	return true
}
