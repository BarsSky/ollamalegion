// session_move_need_r480_test.go — R480 (P1-4): «нужное окно» для переезда
// сессии = max(потребность запроса + резерв, окно АКТИВНОЙ сессии).
//
// Вариант C выбран 2026-11-09 (docs/architecture/r459-window-policy-options.ru.md).
// Тесты держат три обещания:
//  1. Активная сессия держит широкую копию — короткий запрос её не уводит
//     (это и есть защита R459 от «контекст перестал помещаться посреди хода»).
//  2. Остывшая сессия (простой дольше TTL) больше не диктует окно — переезд
//     решает потребность конкретного запроса (ради чего всё и делалось).
//  3. Ручки (LB_R459_REQUEST_NEED / NEED_RESERVE / SESSION_TTL_SEC) реально
//     меняют решение, а не просто читаются.
package balancer

import (
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// r480Proxy — два бэкенда с окнами from/cand и менеджером сессий.
func r480Proxy(fromPerSeq, candPerSeq int) *Proxy {
	p := r459Proxy(fromPerSeq, candPerSeq)
	p.sessionMgr = NewSessionManagerWithTTL(15*time.Minute, 5*time.Minute)
	return p
}

func (p *Proxy) seedSessionR480(id string, lastRequest time.Time) {
	p.sessionMgr.SetWithProtocol(id, "b1", r459Model, "client", "127.0.0.1", "ua", "ollama")
	s := p.sessionMgr.Get(id)
	if s != nil {
		s.LastRequestAt = lastRequest
	}
}

// TestR480_ActiveSessionPinsWideWindow — активная сессия на широкой копии:
// need = окно сессии, короткий запрос не уводит на узкую копию.
func TestR480_ActiveSessionPinsWideWindow(t *testing.T) {
	t.Setenv("LB_R459_SESSION_TTL_SEC", "900")
	t.Setenv("LB_R459_REQUEST_NEED", "on")
	p := r480Proxy(65536, 32768)
	p.seedSessionR480("s-active", time.Now())

	need, source := p.sessionMoveNeedR480("b1", r459Model, "s-active", 0, 1200)
	if need != 65536 {
		t.Fatalf("need = %d, ожидалось окно активной сессии 65536", need)
	}
	if source != "session" {
		t.Fatalf("need_source = %q, ожидался session", source)
	}
	if ok, why := p.sessionMoveKeepsWindowNeedR480("b2", r459Model, need, source); ok {
		t.Fatalf("переезд на узкую копию при активной сессии разрешён (%s)", why)
	}
}

// TestR480_IdleSessionUsesRequestNeed — сессия простояла дольше TTL: окно
// копии больше не требование, переезд решает потребность запроса.
func TestR480_IdleSessionUsesRequestNeed(t *testing.T) {
	t.Setenv("LB_R459_SESSION_TTL_SEC", "900")
	t.Setenv("LB_R459_REQUEST_NEED", "on")
	p := r480Proxy(65536, 32768)
	p.seedSessionR480("s-idle", time.Now().Add(-20*time.Minute))

	need, source := p.sessionMoveNeedR480("b1", r459Model, "s-idle", 0, 12000)
	if need != 12000 {
		t.Fatalf("need = %d, ожидалась потребность запроса 12000 (окно сессии остыло)", need)
	}
	if source != "request" {
		t.Fatalf("need_source = %q, ожидался request", source)
	}
	if ok, why := p.sessionMoveKeepsWindowNeedR480("b2", r459Model, need, source); !ok {
		t.Fatalf("кандидат 32768 покрывает 12000, а переезд запрещён: %s", why)
	}
}

// TestR480_TTLDisabledKeepsOldBehaviour — LB_R459_SESSION_TTL_SEC=0: окно
// сессии учитывается всегда (поведение R459), даже если она давно молчала.
func TestR480_TTLDisabledKeepsOldBehaviour(t *testing.T) {
	t.Setenv("LB_R459_SESSION_TTL_SEC", "0")
	p := r480Proxy(65536, 32768)
	p.seedSessionR480("s-old", time.Now().Add(-2*time.Hour))

	need, source := p.sessionMoveNeedR480("b1", r459Model, "s-old", 0, 1200)
	if need != 65536 || source != "session" {
		t.Fatalf("need=%d source=%q, ожидалось 65536/session (TTL выключен)", need, source)
	}
}

// TestR480_RequestNeedCanBeSwitchedOff — LB_R459_REQUEST_NEED=off: потребность
// запроса не участвует, остаётся окно сессии/клиента.
func TestR480_RequestNeedCanBeSwitchedOff(t *testing.T) {
	t.Setenv("LB_R459_SESSION_TTL_SEC", "900")
	t.Setenv("LB_R459_REQUEST_NEED", "off")
	p := r480Proxy(32768, 65536)
	p.seedSessionR480("s1", time.Now().Add(-time.Hour))

	need, source := p.sessionMoveNeedR480("b1", r459Model, "s1", 0, 12000)
	if need != 0 {
		t.Fatalf("need = %d при выключенной потребности запроса, ожидался 0", need)
	}
	if source != "unknown" {
		t.Fatalf("need_source = %q, ожидался unknown", source)
	}
}

// TestR480_ClientWindowAndRequestNeedMax — клиент назвал окно, но промпт
// больше: берём большее из двух (иначе 413 на генерации).
func TestR480_ClientWindowAndRequestNeedMax(t *testing.T) {
	t.Setenv("LB_R459_SESSION_TTL_SEC", "900")
	t.Setenv("LB_R459_REQUEST_NEED", "on")
	p := r480Proxy(65536, 32768)
	p.seedSessionR480("s1", time.Now().Add(-time.Hour))

	need, source := p.sessionMoveNeedR480("b1", r459Model, "s1", 8192, 20000)
	if need != 20000 {
		t.Fatalf("need = %d, ожидалось 20000 (промпт больше окна клиента)", need)
	}
	if source != "request" {
		t.Fatalf("need_source = %q, ожидался request", source)
	}
}

// TestR480_ReserveIsAddedToPrompt — резерв прибавляется к промпту, и ручка
// LB_R459_NEED_RESERVE его меняет.
func TestR480_ReserveIsAddedToPrompt(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("token ", 500) + `"}]}`)

	t.Setenv("LB_R459_NEED_RESERVE", "0")
	p := &Proxy{}
	noReserve := p.promptTokensForMoveR480(body, "/api/chat")
	if noReserve <= 0 {
		t.Fatalf("оценка промпта = %d, ожидалось > 0", noReserve)
	}

	t.Setenv("LB_R459_NEED_RESERVE", "4096")
	withReserve := p.promptTokensForMoveR480(body, "/api/chat")
	if withReserve != noReserve+4096 {
		t.Fatalf("с резервом = %d, ожидалось %d (промпт %d + резерв 4096)", withReserve, noReserve+4096, noReserve)
	}
}

// TestR480_RejectReasonHasNumbers — причина отказа несёт числа и источник need:
// без них «почему сессия не переехала» снова выясняется чтением исходников.
func TestR480_RejectReasonHasNumbers(t *testing.T) {
	t.Setenv("LB_R459_SESSION_TTL_SEC", "900")
	p := r480Proxy(65536, 16384)
	p.seedSessionR480("s1", time.Now())

	need, source := p.sessionMoveNeedR480("b1", r459Model, "s1", 0, 1000)
	ok, why := p.sessionMoveKeepsWindowNeedR480("b2", r459Model, need, source)
	if ok {
		t.Fatal("кандидат 16384 обязан быть отклонён при need 65536")
	}
	for _, want := range []string{"16384", "65536", "session"} {
		if !strings.Contains(why, want) {
			t.Fatalf("в причине нет %q: %s", want, why)
		}
	}
}

// TestR480_GateRejectsNarrowCandidate — тот же проход отбора: узкий кандидат
// отсечён, широкий — выбран.
func TestR480_GateRejectsNarrowCandidate(t *testing.T) {
	t.Setenv("LB_R459_SESSION_TTL_SEC", "900")
	p := r480Proxy(65536, 32768)
	p.seedSessionR480("s1", time.Now().Add(-time.Hour)) // остывшая: need = потребность запроса
	gate := p.sessionMoveGateR480("b1", r459Model, "s1", 0, 12000)
	if got := p.findLessLoadedBackendWithModelWhere(r459Model, "b1", nil, gate); got != "b2" {
		t.Fatalf("получено %q, ожидался b2: окно кандидата 32768 покрывает need 12000", got)
	}
	// А с потребностью 40000 тот же кандидат уже не покрывает need.
	gate2 := p.sessionMoveGateR480("b1", r459Model, "s1", 0, 40000)
	if got := p.findLessLoadedBackendWithModelWhere(r459Model, "b1", nil, gate2); got != "" {
		t.Fatalf("выбран %q, хотя 32768 < 40000 — переезд обязан быть запрещён", got)
	}
}

// TestR480_UnknownNeedKeepsOldBehaviour — ни окна клиента, ни сессии, ни
// промпта: предел не выдумываем (поведение R459).
func TestR480_UnknownNeedKeepsOldBehaviour(t *testing.T) {
	t.Setenv("LB_R459_REQUEST_NEED", "on")
	p := r480Proxy(0, 0)
	need, source := p.sessionMoveNeedR480("b1", r459Model, "s-none", 0, 0)
	if need != 0 || source != "unknown" {
		t.Fatalf("need=%d source=%q, ожидалось 0/unknown", need, source)
	}
	if ok, why := p.sessionMoveKeepsWindowNeedR480("b2", r459Model, need, source); !ok {
		t.Fatalf("при неизвестном окне ограничивать нечем, получено: %s", why)
	}
}

// TestR480_OllamaCandidateStillAllowed — у Ollama-кандидата окна в метриках
// нет; R478 разрешил такой переезд, и R480 это не ломает.
func TestR480_OllamaCandidateStillAllowed(t *testing.T) {
	p := &Proxy{backends: map[string]*BackendState{
		"ollama-1": {Backend: &types.Backend{ID: "ollama-1", Host: "h", Type: types.BackendTypeOllama}},
		"cpp-1":    {Backend: &types.Backend{ID: "cpp-1", Host: "h", Type: types.BackendTypeLlamaCpp}},
	}}
	if ok, why := p.sessionMoveKeepsWindowNeedR480("ollama-1", "m", 65536, "session"); !ok {
		t.Fatalf("Ollama-кандидат отклонён (%s)", why)
	}
}
