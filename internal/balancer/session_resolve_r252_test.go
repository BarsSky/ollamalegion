// session_resolve_r252_test.go — R252 (2026-10-28): один резолвер id сессии и
// устойчивость к СЖАТИЮ ИСТОРИИ клиентом.
//
// ЖИВОЙ ДЕФЕКТ (Cline, длинная задача). Когда история переполняет окно, Cline
// переписывает её НАЧАЛО (урезает/суммирует). id сессии выводился из отпечатка
// начала разговора (client.go), поэтому сжатие давало НОВЫЙ id: старая строка
// висела в панели до idle TTL, работа продолжалась под новой, а «кошелёк»
// admission-очереди (ключ = sessionID) обнулялся посреди задачи. Оператор видел
// это как «балансер потерял клиента, началась другая сессия».
//
// Что фиксируем:
//  1. ResolveID переиспользует существующую сессию, если клиент/модель/эндпоинт
//     те же и разговор продолжается (совпало начало ИЛИ ход ещё идёт);
//  2. другой клиент/модель → НЕ переиспользуют (иначе R118.9 вернулся бы:
//     два экземпляра Cline на одной машине слились бы в одну сессию);
//  3. нет совпадения → возвращается новый id;
//  4. getSessionIDWithModel: сжатие истории при активном ходе сохраняет id;
//     без активности и при другом начале — id новый (честная граница правила);
//  5. явные идентификаторы клиента (X-Session-ID) правило не затрагивает.
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

// TestR252_ResolveIDReusesSessionForSameConversation — начало разговора совпало
// → тот же id (обычный следующий ход после сжатия начала).
func TestR252_ResolveIDReusesSessionForSameConversation(t *testing.T) {
	sm := NewSessionManagerWithTTL(30*time.Minute, 30*time.Minute)
	defer sm.Stop()

	sm.SetWithProtocol("conv:old::m1", "b1", "m1", "cline", "192.0.2.10", "ua", "openai")
	sm.RecordIdentity("conv:old::m1", SessionIdentity{
		IP: "192.0.2.10", Client: "cline", Model: "m1", EndpointType: "api", ConvKey: "AAA",
	})

	got := sm.ResolveID("conv:new::m1", func(s *types.Session) bool {
		ident := sm.IdentityOf(s.ID)
		return ident.IP == "192.0.2.10" && ident.Client == "cline" &&
			ident.Model == "m1" && ident.EndpointType == "api" && ident.ConvKey == "AAA"
	})
	if got != "conv:old::m1" {
		t.Fatalf("ResolveID = %q, want conv:old::m1 (переиспользование существующей сессии)", got)
	}
}

// TestR252_ResolveIDKeepsParallelTasksApart — разные задачи/модели не сливаются.
func TestR252_ResolveIDKeepsParallelTasksApart(t *testing.T) {
	sm := NewSessionManagerWithTTL(30*time.Minute, 30*time.Minute)
	defer sm.Stop()

	sm.SetWithProtocol("conv:old::m1", "b1", "m1", "cline", "192.0.2.10", "ua", "openai")
	sm.RecordIdentity("conv:old::m1", SessionIdentity{
		IP: "192.0.2.10", Client: "cline", Model: "m1", EndpointType: "api", ConvKey: "AAA",
	})

	// Другая модель и другой отпечаток начала — это ДРУГАЯ сессия (второй экземпляр
	// Cline на той же машине, см. R118.9).
	matcher := func(s *types.Session) bool {
		ident := sm.IdentityOf(s.ID)
		return ident.Model == "m2" && ident.ConvKey == "BBB"
	}
	if got := sm.ResolveID("conv:new::m2", matcher); got != "conv:new::m2" {
		t.Fatalf("ResolveID = %q: сессии разных задач/моделей склеились", got)
	}
	// Другой клиент с тем же разговором — тоже отдельная сессия.
	otherClient := func(s *types.Session) bool {
		ident := sm.IdentityOf(s.ID)
		return ident.Client == "roo" && ident.ConvKey == "AAA"
	}
	if got := sm.ResolveID("conv:x::m1", otherClient); got != "conv:x::m1" {
		t.Fatalf("ResolveID = %q: сессии разных клиентов склеились", got)
	}
}

// TestR252_ResolveIDNoMatchReturnsCandidate — совпадений нет → новая сессия.
func TestR252_ResolveIDNoMatchReturnsCandidate(t *testing.T) {
	sm := NewSessionManagerWithTTL(30*time.Minute, 30*time.Minute)
	defer sm.Stop()

	if got := sm.ResolveID("conv:brand-new::m1", func(*types.Session) bool { return false }); got != "conv:brand-new::m1" {
		t.Fatalf("ResolveID = %q, want исходный candidate", got)
	}
	if got := sm.ResolveID("", func(*types.Session) bool { return true }); got != "" {
		t.Fatalf("ResolveID(\"\") = %q, want \"\"", got)
	}
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
	// r252HistoryBefore — история задачи до сжатия.
	r252HistoryBefore = `{"model":"m1","messages":[` +
		`{"role":"system","content":"You are Cline, a software engineering agent. Workspace: C:\\proj\\r252"},` +
		`{"role":"user","content":"Создай файл internal/r252/demo.go, наполни его, прочитай и поправь"}]}`
	// r252HistoryCompressed — та же задача после сжатия начала истории:
	// первое сообщение пользователя заменено сводкой (так делает Cline).
	r252HistoryCompressed = `{"model":"m1","messages":[` +
		`{"role":"system","content":"You are Cline, a software engineering agent. Workspace: C:\\proj\\r252"},` +
		`{"role":"user","content":"[Summary of earlier conversation] Создание internal/r252/demo.go завершено, осталось поправить импорт"}]}`
	// r252HistoryAnotherTask — ДРУГАЯ задача того же клиента (другое начало,
	// другое намерение). Нужна для проверки границы правила переиспользования.
	r252HistoryAnotherTask = `{"model":"m1","messages":[` +
		`{"role":"system","content":"You are Cline, a software engineering agent. Workspace: C:\\proj\\r252"},` +
		`{"role":"user","content":"Почини падающий тест TestR252 в internal/balancer и объясни причину"}]}`
)

// TestR252_SessionIDSurvivesHistoryCompression — ГЛАВНОЕ: сжатие истории посреди
// активного хода НЕ создаёт новую сессию.
func TestR252_SessionIDSurvivesHistoryCompression(t *testing.T) {
	p := r252ResolveProxy(t)

	req1 := r252ChatRequest(r252HistoryBefore)
	clientName := p.getClientName(req1)
	idBefore := p.getSessionIDWithModel(req1, clientName, "m1")
	if !strings.HasPrefix(idBefore, "conv:") {
		t.Fatalf("id собран не по разговору: %q (предпосылка теста)", idBefore)
	}
	// Обработчик создаёт сессию (SetWithProtocol) — как в реальном пути.
	p.sessionMgr.SetWithProtocol(idBefore, "b1", "m1", clientName, "192.0.2.70", req1.UserAgent(), "openai")
	// Второй ход той же задачи: начало не изменилось, идентичность запоминается.
	_ = p.getSessionIDWithModel(r252ChatRequest(r252HistoryBefore), clientName, "m1")
	// Ход идёт (R251/R252 выставляют флаг на всех стриминговых путях).
	p.sessionMgr.SetStreamActive(idBefore, true)

	// Приходит запрос со СЖАТОЙ историей — начало разговора другое.
	idAfter := p.getSessionIDWithModel(r252ChatRequest(r252HistoryCompressed), clientName, "m1")
	if idAfter != idBefore {
		t.Fatalf("сжатие истории создало новую сессию: %q -> %q (живой дефект)", idBefore, idAfter)
	}

	// Граница правила (честно): НОВАЯ задача того же клиента при неактивной
	// сессии — это отдельная сессия. Иначе два экземпляра Cline на одной машине
	// снова слились бы (регресс R118.9).
	p.sessionMgr.SetStreamActive(idBefore, false)
	idOther := p.getSessionIDWithModel(r252ChatRequest(r252HistoryAnotherTask), clientName, "m1")
	if idOther == idBefore {
		t.Fatalf("другая задача при неактивной сессии склеилась со старой: %q", idOther)
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
