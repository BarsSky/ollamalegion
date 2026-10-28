// session_resolve_r252_test.go — R252/R252.1 (2026-10-28): ОДИН резолвер id
// сессии и устойчивость к СЖАТИЮ ИСТОРИИ клиентом.
//
// ЖИВОЙ ДЕФЕКТ (Cline, длинная задача). Когда история переполняет окно, Cline
// переписывает её НАЧАЛО (урезает/суммирует). id сессии выводился из отпечатка
// начала разговора, поэтому сжатие давало НОВЫЙ id: старая строка висела в панели
// до idle TTL, работа продолжалась под новой, а «кошелёк» admission-очереди
// (ключ = sessionID) обнулялся посреди задачи.
//
// R252.1 (по ревью): сжатие происходит МЕЖДУ ходами, когда стрима уже нет.
// Поэтому признак «сессия активна» из правила переиспользования убран —
// продолжение определяется по разговору: совпал первый system-промпт (он
// переживает сжатие) ИЛИ новое первое user-сообщение содержит хвост прежнего.
// Для параллельных задач того же клиента добавлена защита: активную сессию с
// НЕподтверждённым разговором не переиспользуем (регресс R118.9 недопустим).
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// r252ResolveProxy — прокси с менеджером сессий для проверки резолвера.
func r252ResolveProxy(t *testing.T) *Proxy {
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

// r252ChatRequest — POST-запрос с историей разговора (как у Cline).
func r252ChatRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "192.0.2.70:51234"
	req.Header.Set("User-Agent", "Cline/3.3.0")
	req.Header.Set("Content-Type", "application/json")
	return req
}

const (
	r252SystemAlpha = `You are Cline, a software engineering agent. Workspace: C:\\proj\\r252`
	// r252HistoryBefore — история задачи до сжатия.
	r252HistoryBefore = `{"model":"m1","messages":[` +
		`{"role":"system","content":"` + r252SystemAlpha + `"},` +
		`{"role":"user","content":"Создай файл internal/r252/demo.go, наполни его реализацией парсера, прочитай и поправь импорты"}]}`
	// r252HistoryCompressed — та же задача после сжатия начала истории: первое
	// сообщение пользователя заменено сводкой (так делает Cline), system — прежний.
	r252HistoryCompressed = `{"model":"m1","messages":[` +
		`{"role":"system","content":"` + r252SystemAlpha + `"},` +
		`{"role":"user","content":"[Summary of earlier conversation] Создание internal/r252/demo.go завершено, осталось поправить импорты"}]}`
	// r252HistoryOtherTask — ДРУГАЯ задача: другой workspace в system-промпте и
	// другое намерение (второе окно Cline / переключение Plan-Act).
	r252HistoryOtherTask = `{"model":"m1","messages":[` +
		`{"role":"system","content":"You are Cline, a software engineering agent. Workspace: C:\\proj\\other"},` +
		`{"role":"user","content":"Почини падающий тест TestR252 в internal/balancer и объясни причину"}]}`
	// r252HistorySameSystemOtherUser — тот же workspace, но ДРУГОЕ намерение:
	// параллельная задача в том же проекте.
	r252HistorySameSystemOtherUser = `{"model":"m1","messages":[` +
		`{"role":"system","content":"` + r252SystemAlpha + `"},` +
		`{"role":"user","content":"Отдельная задача: добавь бенчмарк для парсера и сравни с прошлой версией"}]}`
)

// TestR252_ResolveIDReusesByConversation — ResolveID переиспользует сессию, если
// разговор продолжается (совпал system-промпт), БЕЗ требования активного стрима.
func TestR252_ResolveIDReusesByConversation(t *testing.T) {
	sm := NewSessionManagerWithTTL(30*time.Minute, 30*time.Minute)
	defer sm.Stop()

	sm.SetWithProtocol("conv:old::m1", "b1", "m1", "cline", "192.0.2.10", "ua", "openai")
	sm.RecordIdentity("conv:old::m1", SessionIdentity{
		IP: "192.0.2.10", Client: "cline", Model: "m1", EndpointType: "api",
		SystemHead: "You are Cline, workspace alpha", UserHead: "task A", UserTail: "task A end",
	})

	// Сжатие истории: system прежний, первое user-сообщение другое.
	got := sm.ResolveID("conv:new::m1", func(s *types.Session, ident SessionIdentity) bool {
		return ident.IP == "192.0.2.10" && ident.Client == "cline" &&
			ident.Model == "m1" && ident.EndpointType == "api" &&
			ident.SystemHead == "You are Cline, workspace alpha" && !s.HasActiveStream
	})
	if got != "conv:old::m1" {
		t.Fatalf("ResolveID = %q, want conv:old::m1 (продолжение разговора)", got)
	}
}

// TestR252_ResolveIDKeepsParallelTasksApart — разные задачи/модели не сливаются.
func TestR252_ResolveIDKeepsParallelTasksApart(t *testing.T) {
	sm := NewSessionManagerWithTTL(30*time.Minute, 30*time.Minute)
	defer sm.Stop()

	sm.SetWithProtocol("conv:old::m1", "b1", "m1", "cline", "192.0.2.10", "ua", "openai")
	sm.RecordIdentity("conv:old::m1", SessionIdentity{
		IP: "192.0.2.10", Client: "cline", Model: "m1", EndpointType: "api",
		SystemHead: "workspace alpha", UserHead: "task A", UserTail: "task A end",
	})

	otherModel := func(s *types.Session, ident SessionIdentity) bool {
		return ident.Model == "m2" && ident.SystemHead == "workspace alpha"
	}
	if got := sm.ResolveID("conv:new::m2", otherModel); got != "conv:new::m2" {
		t.Fatalf("ResolveID = %q: сессии разных моделей склеились", got)
	}
	otherClient := func(s *types.Session, ident SessionIdentity) bool {
		return ident.Client == "roo" && ident.SystemHead == "workspace alpha"
	}
	if got := sm.ResolveID("conv:x::m1", otherClient); got != "conv:x::m1" {
		t.Fatalf("ResolveID = %q: сессии разных клиентов склеились", got)
	}
}

// TestR252_ResolveIDNoMatchReturnsCandidate — совпадений нет → новая сессия.
func TestR252_ResolveIDNoMatchReturnsCandidate(t *testing.T) {
	sm := NewSessionManagerWithTTL(30*time.Minute, 30*time.Minute)
	defer sm.Stop()

	if got := sm.ResolveID("conv:brand-new::m1", func(*types.Session, SessionIdentity) bool { return false }); got != "conv:brand-new::m1" {
		t.Fatalf("ResolveID = %q, want исходный candidate", got)
	}
	if got := sm.ResolveID("", func(*types.Session, SessionIdentity) bool { return true }); got != "" {
		t.Fatalf("ResolveID(\"\") = %q, want \"\"", got)
	}
}

// TestR252_SessionIDSurvivesCompressionBetweenTurns — ГЛАВНОЕ (по ревью):
// сжатие МЕЖДУ ходами (стрима нет) не создаёт новую сессию.
func TestR252_SessionIDSurvivesCompressionBetweenTurns(t *testing.T) {
	p := r252ResolveProxy(t)
	clientName := "Cline/3.3.0"

	reqBefore := r252ChatRequest(r252HistoryBefore)
	idBefore := p.getSessionIDWithModel(reqBefore, clientName, "m1")
	if !strings.HasPrefix(idBefore, "conv:") {
		t.Fatalf("id собран не по разговору: %q (предпосылка теста)", idBefore)
	}
	// Обработчик создаёт запись (SetWithProtocol) — идентичность уже записана
	// (R252.1: RecordIdentity пишет и до создания сессии).
	p.sessionMgr.SetWithProtocol(idBefore, "b1", "m1", clientName, "192.0.2.70", reqBefore.UserAgent(), "openai")

	// Ход закончился: стрима нет (именно так приходит сжатая история).
	p.sessionMgr.SetStreamActive(idBefore, false)

	idCompressed := p.getSessionIDWithModel(r252ChatRequest(r252HistoryCompressed), clientName, "m1")
	if idCompressed != idBefore {
		t.Fatalf("сжатие истории между ходами создало новую сессию: %q -> %q (живой дефект)", idBefore, idCompressed)
	}
}

// TestR252_ParallelTaskDoesNotStickToActiveSession — БЛОКЕР 2: вторая задача того
// же клиента не прилипает к активной сессии другого разговора (регресс R118.9).
func TestR252_ParallelTaskDoesNotStickToActiveSession(t *testing.T) {
	p := r252ResolveProxy(t)
	clientName := "Cline/3.3.0"

	reqBefore := r252ChatRequest(r252HistoryBefore)
	idBefore := p.getSessionIDWithModel(reqBefore, clientName, "m1")
	p.sessionMgr.SetWithProtocol(idBefore, "b1", "m1", clientName, "192.0.2.70", reqBefore.UserAgent(), "openai")
	p.sessionMgr.SetStreamActive(idBefore, true)

	// (а) другой workspace/намерение: ни system, ни хвост не совпали.
	idOther := p.getSessionIDWithModel(r252ChatRequest(r252HistoryOtherTask), clientName, "m1")
	if idOther == idBefore {
		t.Fatalf("другая задача прилипла к активной сессии: %q (регресс R118.9)", idOther)
	}

	// (б) тот же workspace, но другое намерение и сессия активна: system совпал,
	// но это НЕ подтверждённое продолжение — тоже отдельная сессия.
	idSameSystem := p.getSessionIDWithModel(r252ChatRequest(r252HistorySameSystemOtherUser), clientName, "m1")
	if idSameSystem == idBefore {
		t.Fatalf("параллельная задача в том же workspace прилипла к активной сессии: %q", idSameSystem)
	}
}

// TestR252_ContinuationWithActiveStreamKeepsID — продолжение разговора при
// активном стриме сохраняет id (сильный признак: начало не изменилось).
func TestR252_ContinuationWithActiveStreamKeepsID(t *testing.T) {
	p := r252ResolveProxy(t)
	clientName := "Cline/3.3.0"

	reqBefore := r252ChatRequest(r252HistoryBefore)
	idBefore := p.getSessionIDWithModel(reqBefore, clientName, "m1")
	p.sessionMgr.SetWithProtocol(idBefore, "b1", "m1", clientName, "192.0.2.70", reqBefore.UserAgent(), "openai")
	p.sessionMgr.SetStreamActive(idBefore, true)

	if got := p.getSessionIDWithModel(r252ChatRequest(r252HistoryBefore), clientName, "m1"); got != idBefore {
		t.Fatalf("продолжение при активном стриме сменило сессию: %q -> %q", idBefore, got)
	}
}

// TestR252_ExplicitSessionHeaderUnaffected — явные идентификаторы клиента
// правило переиспользования не затрагивает.
func TestR252_ExplicitSessionHeaderUnaffected(t *testing.T) {
	p := r252ResolveProxy(t)

	req1 := r252ChatRequest(r252HistoryBefore)
	req1.Header.Set("X-Session-ID", "explicit-1")
	id1 := p.getSessionIDWithModel(req1, p.getClientName(req1), "m1")
	if !strings.HasPrefix(id1, "explicit-1") {
		t.Fatalf("X-Session-ID проигнорирован: %q", id1)
	}

	req2 := r252ChatRequest(r252HistoryCompressed)
	req2.Header.Set("X-Session-ID", "explicit-2")
	id2 := p.getSessionIDWithModel(req2, p.getClientName(req2), "m1")
	if id1 == id2 {
		t.Fatalf("разные X-Session-ID склеились: %q", id1)
	}
	if !strings.HasPrefix(id2, "explicit-2") {
		t.Fatalf("X-Session-ID второго клиента проигнорирован: %q", id2)
	}
}
