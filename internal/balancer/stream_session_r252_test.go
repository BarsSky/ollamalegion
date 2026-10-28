// stream_session_r252_test.go — R252 (2026-10-28): единый вход стрима и гарантия
// завершения SSE-потока.
//
// ЧТО ФИКСИРУЕМ.
//  1. startStreamSession делает клиента ВИДИМЫМ на время стрима (сессия активна,
//     запись монитора помечена как streaming) и снимает всё после Stop, причём
//     повторный Stop безопасен. Без этого живой ответ «пропадал» из панели:
//     сессию удалял TTL, а запись монитора вычищалась через 60 с простоя.
//  2. ensurePlainSSEDone / ensureChunkedSSEDone дописывают РОВНО ОДИН
//     `data: [DONE]`, если последний отданный кадр им не был. Раньше основной
//     (hijack) путь не писал его вовсе: при обрыве upstream клиент видел просто
//     закрытый поток и считал ответ оборванным.
package balancer

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newR252StreamTestProxy — прокси, которому есть куда писать: менеджер сессий и
// карта клиентов монитора.
func newR252StreamTestProxy(t *testing.T) *Proxy {
	t.Helper()
	p := &Proxy{
		config: &types.LoadBalancerConfig{Balancing: types.BalancingSettings{
			SessionStickiness: true,
			AdvancedTiming:    types.AdvancedTimingConfig{HeartbeatIntervalSec: 1},
		}},
		sessionMgr:    NewSessionManagerWithTTL(30*time.Minute, 30*time.Minute),
		recentClients: make(map[string]*types.RecentClient),
	}
	t.Cleanup(p.sessionMgr.Stop)
	return p
}

// r252StreamRequest — запрос с явным X-Session-ID (id сессии детерминирован и не
// зависит от отпечатка разговора).
func r252StreamRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	req.RemoteAddr = "192.0.2.60:51234"
	req.Header.Set("User-Agent", "Cline/3.3.0")
	req.Header.Set("X-Session-ID", "sess-r252")
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestR252_StreamSessionMarksClientVisibleAndClears — главное: во время стрима
// клиент виден, после — снят; Stop идемпотентен.
func TestR252_StreamSessionMarksClientVisibleAndClears(t *testing.T) {
	p := newR252StreamTestProxy(t)
	req := r252StreamRequest(t)

	sessionID := p.getSessionIDWithModel(req, p.getClientName(req), "m1")
	if sessionID == "" {
		t.Fatal("не удалось построить id сессии")
	}
	// Как в реальном обработчике: сессия создаётся до стрима.
	p.sessionMgr.SetWithProtocol(sessionID, "b1", "m1", p.getClientName(req), "192.0.2.60", req.UserAgent(), "openai")
	// Запись в «клиентах» монитора создаётся на входе запроса.
	p.recordRecentClientParsed(req, &parsedRequest{Model: "m1"})

	sess := startStreamSession(p, req, "m1")
	if got := sess.SessionID(); got != sessionID {
		t.Fatalf("SessionID() = %q, want %q", got, sessionID)
	}
	if s := p.sessionMgr.Get(sessionID); s == nil || !s.HasActiveStream {
		t.Fatal("сессия не помечена как активный стрим — клиент «невидим» для защиты от TTL")
	}
	key := p.getClientRealIP(req) + "::" + p.getClientName(req)
	p.recentClientsMu.RLock()
	rc := p.recentClients[key]
	activeInMonitor := rc != nil && rc.IsStreamActive
	p.recentClientsMu.RUnlock()
	if !activeInMonitor {
		t.Fatal("запись монитора не помечена streaming — клиент пропадёт из монитора через 60 с")
	}

	sess.Stop()
	if s := p.sessionMgr.Get(sessionID); s == nil || s.HasActiveStream {
		t.Fatal("после Stop сессия осталась «активной» — получится вечный зомби")
	}
	p.recentClientsMu.RLock()
	rc = p.recentClients[key]
	stillActive := rc != nil && rc.IsStreamActive
	p.recentClientsMu.RUnlock()
	if stillActive {
		t.Fatal("после Stop запись монитора осталась streaming")
	}

	// Повторный Stop (defer после явного вызова) обязан быть безопасным:
	// паника/дедлок здесь означали бы «стоп уронил обработчик».
	sess.Stop()
}

// TestR252_PlainSSEDoneWrittenOnce — ровно один `data: [DONE]` на поток.
func TestR252_PlainSSEDoneWrittenOnce(t *testing.T) {
	w := httptest.NewRecorder()

	// Поток без DONE (upstream закрылся после финального чанка) — дописываем.
	last := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	if !ensurePlainSSEDone(w, last) {
		t.Fatal("ensurePlainSSEDone не дописал [DONE] после кадров без него")
	}
	if n := strings.Count(w.Body.String(), "data: [DONE]"); n != 1 {
		t.Fatalf("кадров [DONE] в теле %d, want 1 (%q)", n, w.Body.String())
	}

	// Берём последним кадром сам [DONE] — второй писать нельзя.
	w2 := httptest.NewRecorder()
	if ensurePlainSSEDone(w2, []byte(sseDoneFrame)) {
		t.Fatal("ensurePlainSSEDone дописал второй [DONE] — клиент увидит дубль")
	}
	if strings.Contains(w2.Body.String(), "data: [DONE]") {
		t.Fatalf("в тело что-то записано при уже отданном [DONE]: %q", w2.Body.String())
	}

	// Кадр с хвостовыми переводами строк — тоже «уже DONE».
	w3 := httptest.NewRecorder()
	if ensurePlainSSEDone(w3, []byte("data: [DONE]\n\n")) {
		t.Fatal("[DONE] с пустой строкой не распознан")
	}
}

// TestR252_ChunkedSSEDoneWrittenOnce — то же для hijack-пути (chunked framing).
func TestR252_ChunkedSSEDoneWrittenOnce(t *testing.T) {
	var buf bytes.Buffer
	bufrw := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))

	last := append([]byte("data: {\"a\":1}"), '\n')
	if !ensureChunkedSSEDone(bufrw, last) {
		t.Fatal("ensureChunkedSSEDone не дописал [DONE]")
	}
	got := buf.String()
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("кадров [DONE] в chunked-потоке %d, want 1 (%q)", n, got)
	}
	// Кадр должен быть корректно зафреймлен: <hex len>\r\ndata: [DONE]\n\n\r\n
	if !strings.Contains(got, "e\r\ndata: [DONE]\n\n\r\n") {
		t.Fatalf("кадр [DONE] не зафреймлен как chunked (размер %d): %q", len(sseDoneFrame), got)
	}

	// Повторный вызов с последним кадром [DONE] ничего не добавляет.
	before := buf.Len()
	if ensureChunkedSSEDone(bufrw, []byte(sseDoneFrame)) {
		t.Fatal("второй вызов дописал [DONE] повторно")
	}
	if buf.Len() != before {
		t.Fatalf("буфер вырос на %d байт при уже отданном [DONE]", buf.Len()-before)
	}
}

// TestR252_SSEDoneLineRecognition — распознавание кадра [DONE] (в т.ч. с \r\n).
func TestR252_SSEDoneLineRecognition(t *testing.T) {
	yes := [][]byte{
		[]byte("data: [DONE]"),
		[]byte("data: [DONE]\n"),
		[]byte("data: [DONE]\r\n\r\n"),
		[]byte("  data: [DONE]  "),
	}
	for _, line := range yes {
		if !isSSEDoneLine(line) {
			t.Errorf("не распознан [DONE]: %q", line)
		}
	}
	no := [][]byte{
		nil,
		[]byte(""),
		[]byte("data: {\"a\":1}"),
		[]byte(": keepalive"),
		[]byte("[DONE]"),
		[]byte("data: [DONE]extra"),
	}
	for _, line := range no {
		if isSSEDoneLine(line) {
			t.Errorf("ложно распознан [DONE]: %q", line)
		}
	}
}

// TestR252_UpstreamErrorGetsDone — ПРАВКА 4 (ревью): после error-кадра клиент
// обязан получить РОВНО ОДИН `data: [DONE]`, иначе поток выглядит оборванным.
func TestR252_UpstreamErrorGetsDone(t *testing.T) {
	// hijack-путь (chunked framing)
	var buf bytes.Buffer
	bufrw := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))
	if !writeChunkedStreamError(bufrw, "upstream read error") {
		t.Fatal("writeChunkedStreamError не записал кадр")
	}
	got := buf.String()
	if !strings.Contains(got, "upstream read error") {
		t.Fatalf("error-кадр не записан: %q", got)
	}
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] после error-кадра: %d, want 1 (%q)", n, got)
	}

	// legacy-путь (обычный writer)
	w := httptest.NewRecorder()
	if !writePlainStreamError(w, "upstream read error") {
		t.Fatal("writePlainStreamError не записал кадр")
	}
	body := w.Body.String()
	if !strings.Contains(body, "upstream read error") {
		t.Fatalf("error-кадр не записан (legacy): %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] после error-кадра (legacy): %d, want 1 (%q)", n, body)
	}
}

// TestR252_StreamSessionNilSafe — ПРАВКА 5 (ревью): помощник обязан быть
// nil-безопасным. Раньше тикер звал p.getHeartbeatInterval(), который читает
// p.config.Balancing без проверки: у прокси без конфига это паника в горутине, а
// она роняет весь процесс (recover обработчика её не ловит).
func TestR252_StreamSessionNilSafe(t *testing.T) {
	req := r252StreamRequest(t)

	// (а) nil-прокси
	var nilProxy *Proxy
	s := startStreamSession(nilProxy, req, "m1")
	if s == nil {
		t.Fatal("startStreamSession(nil) вернул nil — вызывающий код получит панику на Stop")
	}
	s.Stop()

	// (б) прокси без конфига и без менеджера сессий
	bare := &Proxy{}
	s2 := startStreamSession(bare, req, "m1")
	s2.Stop()

	// (в) интервал не паникует при nil-конфиге
	if d := bare.streamHeartbeatIntervalSafe(); d <= 0 {
		t.Fatalf("streamHeartbeatIntervalSafe() = %v, want дефолт", d)
	}
	if d := nilProxy.streamHeartbeatIntervalSafe(); d != defaultStreamHeartbeatInterval {
		t.Fatalf("nil-прокси: %v, want %v", d, defaultStreamHeartbeatInterval)
	}
}
