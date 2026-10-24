// protocol_r229_test.go — R229 (2026-10-22): бейджи протокола в сессиях/очереди.
//
// ЖИВЫЕ ЖАЛОБЫ ОПЕРАТОРА: «Cline жалуется на инструменты», «Open WebUI получает не
// тот n_ctx», «сессия висит». Почти все относятся к одному из двух путей, а в
// сессиях и очереди пути были неразличимы: видно клиента, но не видно, говорил он
// по OpenAI-совместимому API (/v1/...) или по родному Ollama (/api/...).
//
// Тесты фиксируют:
//   - определение протокола по пути (и пусто для служебных путей — выдумывать нельзя);
//   - SessionManager хранит протокол и НЕ стирает его последующим Set без протокола;
//   - admission-очередь отдаёт протокол в waiting_detail через резолвер сессий;
//   - без резолвера — пусто, а не паника.
package balancer

import (
	"testing"
	"time"
)

func TestR229_RequestProtocol(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1/chat/completions", ProtocolOpenAI},
		{"/v1/models", ProtocolOpenAI},
		{"/V1/EMBEDDINGS", ProtocolOpenAI},
		{"/api/chat", ProtocolOllama},
		{"/api/generate", ProtocolOllama},
		{"/api/tags", ProtocolOllama},
		{"/health", ""},
		{"/api/v1/backends", ""}, // служебный API балансера, не протокол клиента
		{"", ""},
	}
	for _, c := range cases {
		if got := RequestProtocol(c.path); got != c.want {
			t.Errorf("RequestProtocol(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestR229_SessionProtocolStoredAndKept(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()

	sm.SetWithProtocol("s1", "b1", "model", "Cline", "10.0.0.1", "ua", ProtocolOpenAI)
	if got := sm.ProtocolOf("s1"); got != ProtocolOpenAI {
		t.Fatalf("ProtocolOf = %q, want %q", got, ProtocolOpenAI)
	}

	// Обычный Set (место, где протокол не определён) НЕ должен стирать известный:
	// иначе бейдж исчезал бы после первого же служебного обновления сессии.
	sm.Set("s1", "b1", "model", "Cline", "10.0.0.1", "ua")
	if got := sm.ProtocolOf("s1"); got != ProtocolOpenAI {
		t.Errorf("Set без протокола стёр известный: %q", got)
	}

	// Смена протокола (клиент перешёл на другой API) — обновляется.
	sm.SetWithProtocol("s1", "b1", "model", "Cline", "10.0.0.1", "ua", ProtocolOllama)
	if got := sm.ProtocolOf("s1"); got != ProtocolOllama {
		t.Errorf("смена протокола не сохранилась: %q", got)
	}

	// Неизвестная сессия — пусто, без паники.
	if got := sm.ProtocolOf("nope"); got != "" {
		t.Errorf("ProtocolOf(unknown) = %q, want пусто", got)
	}
}

func TestR229_AdmissionDetailCarriesProtocol(t *testing.T) {
	aq := newAdmissionQueue(0)
	aq.protocolOf = func(session string) string {
		if session == "s-openai" {
			return ProtocolOpenAI
		}
		return ""
	}
	aq.enqueue("backend-1", "s-openai")
	aq.enqueue("backend-1", "s-unknown")

	stats := aq.stats(5 * time.Second)
	if len(stats.WaitingDetail) != 2 {
		t.Fatalf("waiting_detail = %d строк, want 2", len(stats.WaitingDetail))
	}
	got := map[string]string{}
	for _, w := range stats.WaitingDetail {
		got[w.Session] = w.Protocol
	}
	if got["s-openai"] != ProtocolOpenAI {
		t.Errorf("protocol для известной сессии = %q, want openai", got["s-openai"])
	}
	if got["s-unknown"] != "" {
		t.Errorf("protocol для неизвестной сессии = %q, want пусто (не выдумывать)", got["s-unknown"])
	}
}

func TestR229_AdmissionDetailWithoutResolverIsSafe(t *testing.T) {
	aq := newAdmissionQueue(0) // резолвер не задан (тесты/старая инициализация)
	aq.enqueue("be", "s1")
	stats := aq.stats(5 * time.Second)
	if len(stats.WaitingDetail) != 1 || stats.WaitingDetail[0].Protocol != "" {
		t.Fatalf("без резолвера detail = %+v, want одну строку с пустым протоколом", stats.WaitingDetail)
	}
}

func TestR229_ProtocolOfSessionFallsBackToAdmissionKey(t *testing.T) {
	p := &Proxy{sessionMgr: NewSessionManager(), admissionProto: newAdmissionProtocols()}
	defer p.sessionMgr.Stop()

	p.sessionMgr.SetWithProtocol("sess-1", "b", "m", "c", "ip", "ua", ProtocolOpenAI)
	if got := p.protocolOfSession("sess-1"); got != ProtocolOpenAI {
		t.Errorf("по ID сессии = %q, want openai", got)
	}

	// Ключ admission-очереди (conv:...) НЕ равен ID сессии — живая проверка:
	// waiting_detail.session=conv:..., в SessionManager такого ID нет.
	p.noteAdmissionProtocol("conv:abc", ProtocolOllama)
	if got := p.protocolOfSession("conv:abc"); got != ProtocolOllama {
		t.Errorf("по ключу очереди = %q, want ollama (второй источник не сработал)", got)
	}

	if got := p.protocolOfSession("unknown"); got != "" {
		t.Errorf("неизвестный ключ = %q, want пусто", got)
	}
}