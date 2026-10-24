// session_visibility_r118_test.go — R118 (2026-10-14): очередь показывает позицию,
// а по ключу сессии видно, различает ли балансер клиентов.
//
// ЖАЛОБА: «запрос от одной машины, но от двух экземпляров клиента Cline — балансер
// не разделяет». Ключ сессии порождается в client.go (getSessionIDWithModel) и в
// RequestSessionKey, и часть ключей — синтетические: `fp:` = хеш IP+User-Agent,
// `tkn:` = хеш Authorization. У двух экземпляров одной машины они совпадают, то
// есть очередь физически не может их различить — и обязана сказать это прямо, а не
// показывать две одинаковые строки как разных клиентов.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestR118_SessionIdentitySource — классификация ключа: client / derived / none.
func TestR118_SessionIdentitySource(t *testing.T) {
	cases := []struct {
		session string
		want    string
		why     string
	}{
		{"", sessionIdentityNone, "идентификатора нет"},
		{"fp:d1c51dcaef1cbdbe::curl/8.21.0::m::gen", sessionIdentityDerived,
			"отпечаток IP+User-Agent: у двух экземпляров одной машины совпадает"},
		{"tkn:9f2c::m::gen", sessionIdentityDerived, "общий Authorization-токен"},
		{"sess-B::Qwen3.8-27B-UD-Q4_K_M::gen", sessionIdentityClient,
			"клиент прислал X-Session-Id — ключи у экземпляров разные"},
		{"X-Session-Id: sess-B", sessionIdentityClient, "заголовок клиента (RequestSessionKey)"},
		{"X-User-Id: operator", sessionIdentityClient, "пользователь из заголовка"},
		{"usr:abc123::m", sessionIdentityClient, "пользователь из тела/заголовка"},
		{"cid:abc::m", sessionIdentityClient, "X-Client-ID клиента"},
		{"chat:42::m", sessionIdentityClient, "chat_id из тела"},
		{"request:req-777", sessionIdentityClient, "X-Request-Id — различает запросы"},
	}
	for _, tc := range cases {
		if got := SessionIdentitySource(tc.session); got != tc.want {
			t.Errorf("SessionIdentitySource(%q) = %q, want %q (%s)", tc.session, got, tc.want, tc.why)
		}
	}
}

// TestR118_WaitingDetailHasPositionAndSessionKnown — в очереди видно позицию и
// честную пометку «клиент не различается».
func TestR118_WaitingDetailHasPositionAndSessionKnown(t *testing.T) {
	aq := newAdmissionQueue(300 * time.Second)

	// Первый ожидающий — синтетический ключ (балансер не различает клиентов),
	// второй — клиентский X-Session-Id (различает).
	aq.enqueue("any", "fp:d1c51dcaef1cbdbe::curl/8.21.0::m::gen")
	aq.enqueue("any", "sess-B::m::gen")

	stats := aq.stats(300 * time.Second)

	if len(stats.WaitingDetail) != 2 {
		t.Fatalf("waiting_detail = %d записей, want 2", len(stats.WaitingDetail))
	}
	if got := stats.WaitingDetail[0].Position; got != 1 {
		t.Errorf("позиция первого ожидающего = %d, want 1", got)
	}
	if got := stats.WaitingDetail[1].Position; got != 2 {
		t.Errorf("позиция второго ожидающего = %d, want 2", got)
	}
	if stats.WaitingDetail[0].SessionKnown {
		t.Error("отпечаток IP+User-Agent помечен как известная сессия: оператор решит, " +
			"что балансер различает двух клиентов с одной машины")
	}
	if !stats.WaitingDetail[1].SessionKnown {
		t.Error("клиентский X-Session-Id помечен как неизвестная сессия")
	}
	if got := stats.WaitingDetail[0].SessionSource; got != sessionIdentityDerived {
		t.Errorf("session_source первого = %q, want %q", got, sessionIdentityDerived)
	}
	if stats.Unidentified != 1 {
		t.Errorf("unidentified_waiting = %d, want 1 (ровно один ожидающий не различается)",
			stats.Unidentified)
	}
}

// TestR118_RequestSessionKeyRequestIDLastResort — X-Request-Id берётся ПОСЛЕДНИМ
// рубежом: клиент без пользователя и сессии, но с id запроса, получает различимые
// строки в очереди; устойчивый идентификатор всегда приоритетнее.
func TestR118_RequestSessionKeyRequestIDLastResort(t *testing.T) {
	// Только X-Request-Id → различимый ключ запроса.
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	req.Header.Set("X-Request-Id", "req-777")
	if got := RequestSessionKey(req, nil); got != "request:req-777" {
		t.Errorf("RequestSessionKey(X-Request-Id) = %q, want %q", got, "request:req-777")
	}

	// Сессия + id запроса → выигрывает устойчивая сессия (иначе потерялась бы
	// группировка запросов одного клиента).
	req2 := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	req2.Header.Set("X-Session-Id", "sess-A")
	req2.Header.Set("X-Request-Id", "req-777")
	if got := RequestSessionKey(req2, nil); got != "X-Session-Id:sess-A" {
		t.Errorf("RequestSessionKey(X-Session-Id + X-Request-Id) = %q, want %q",
			got, "X-Session-Id:sess-A")
	}

	// Ничего → пусто (балансер честно сообщает, что клиента не различает).
	req3 := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	if got := RequestSessionKey(req3, nil); got != "" {
		t.Errorf("RequestSessionKey(без заголовков) = %q, want пусто", got)
	}
}
