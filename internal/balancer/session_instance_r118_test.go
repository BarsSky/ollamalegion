//go:build llama_stub

// session_instance_r118_test.go — R118.9 (2026-10-14).
//
// ЖАЛОБА ОПЕРАТОРА (с начала сессии): «выдавать запрос от одной машины, но от двух
// экземпляров клиента Cline — балансер не разделяет».
//
// ПРИЧИНА, которую фиксируют тесты: идентификатор сессии собирался из отпечатка
// машины (IP+User-Agent) и имени клиента — у двух экземпляров Cline на одной
// машине он одинаковый, поэтому оба попадали в одну сессию (общая привязка к
// бэкенду, общий кошелёк справедливости в очереди, одна строка в панели).
//
// РЕШЕНИЕ: различитель — НАЧАЛО РАЗГОВОРА (первый стабильный блок сообщений,
// который Cline пересылает в каждом запросе). Тесты проверяют:
//  1. отпечаток различает разные задачи и совпадает для одной задачи, даже если
//     история выросла или текст переформатирован;
//  2. два экземпляра с одной машины получают РАЗНЫЕ session id;
//  3. одна задача — тот же session id (привязка к бэкенду не теряется);
//  4. короткие одинаковые промпты различителем не считаются (прежнее поведение);
//  5. в admission-ключе (RequestSessionKey) разговор важнее id запроса.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

const (
	taskAlpha = `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[` +
		`{"role":"system","content":"You are Cline, a software engineering agent. Workspace: C:\\proj\\alpha"},` +
		`{"role":"user","content":"Реализуй парсер конфигурации в internal/config/parser.go и тесты к нему"}]}`
	taskBeta = `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[` +
		`{"role":"system","content":"You are Cline, a software engineering agent. Workspace: C:\\proj\\beta"},` +
		`{"role":"user","content":"Почини падающий тест TestBalancerRetry в internal/balancer"}]}`
)

// taskAlphaNextTurn — та же задача, но история уже выросла (второй ход клиента).
const taskAlphaNextTurn = `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[` +
	`{"role":"system","content":"You are Cline, a software engineering agent. Workspace: C:\\proj\\alpha"},` +
	`{"role":"user","content":"Реализуй парсер конфигурации в internal/config/parser.go и тесты к нему"},` +
	`{"role":"assistant","content":"Начинаю с чтения текущего парсера"},` +
	`{"role":"user","content":"продолжай"}]}`

func newSessionTestProxy() *Proxy {
	return &Proxy{config: &types.LoadBalancerConfig{}}
}

func sessionTestRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.RemoteAddr = "192.0.2.50:51234"
	req.Header.Set("User-Agent", "Cline/3.2.0")
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestR118_9_FingerprintDistinguishesTasks — разные задачи → разные отпечатки,
// одна задача (в т.ч. со выросшей историей и другим форматированием) → один.
func TestR118_9_FingerprintDistinguishesTasks(t *testing.T) {
	a := conversationFingerprint([]byte(taskAlpha))
	b := conversationFingerprint([]byte(taskBeta))
	if a == "" || b == "" {
		t.Fatalf("отпечаток не построен: alpha=%q beta=%q", a, b)
	}
	if a == b {
		t.Fatal("две разные задачи дали одинаковый отпечаток — экземпляры снова сольются")
	}
	if again := conversationFingerprint([]byte(taskAlpha)); again != a {
		t.Errorf("отпечаток нестабилен: %q vs %q", a, again)
	}
	if next := conversationFingerprint([]byte(taskAlphaNextTurn)); next != a {
		t.Errorf("выросшая история изменила отпечаток (%q vs %q) — привязка к бэкенду терялась бы", next, a)
	}
	// Переформатирование (лишние пробелы/переводы строк) не должно менять ключ.
	reformatted := strings.ReplaceAll(taskAlpha, `\n`, `  \n `)
	if got := conversationFingerprint([]byte(reformatted)); got != a {
		t.Errorf("переформатированный запрос дал другой отпечаток: %q vs %q", got, a)
	}
}

// TestR118_9_ShortPromptsAreNotFingerprinted — короткие одинаковые промпты не
// различитель: для них поведение прежнее (отпечаток машины).
func TestR118_9_ShortPromptsAreNotFingerprinted(t *testing.T) {
	short := `{"model":"m","messages":[{"role":"user","content":"2+2?"}]}`
	if fp := conversationFingerprint([]byte(short)); fp != "" {
		t.Errorf("короткий промпт не должен давать отпечаток, получено %q", fp)
	}
	if fp := conversationFingerprint([]byte(`{"model":"m"}`)); fp != "" {
		t.Errorf("тело без сообщений не должно давать отпечаток, получено %q", fp)
	}
}

// TestR118_9_TwoInstancesGetDifferentSessions — ГЛАВНОЕ: два экземпляра клиента с
// одной машины (один IP, один User-Agent) получают РАЗНЫЕ session id, а одна
// задача — один и тот же (привязка к бэкенду сохраняется).
func TestR118_9_TwoInstancesGetDifferentSessions(t *testing.T) {
	p := newSessionTestProxy()

	idAlpha := p.getSessionIDWithModel(sessionTestRequest(taskAlpha), "Cline/3.2.0", "gemma-4-E4B-it-Q4_K_M")
	idBeta := p.getSessionIDWithModel(sessionTestRequest(taskBeta), "Cline/3.2.0", "gemma-4-E4B-it-Q4_K_M")
	idAlphaNext := p.getSessionIDWithModel(sessionTestRequest(taskAlphaNextTurn), "Cline/3.2.0", "gemma-4-E4B-it-Q4_K_M")

	if idAlpha == "" || idBeta == "" {
		t.Fatal("идентификаторы сессий пусты")
	}
	if idAlpha == idBeta {
		t.Fatalf("два экземпляра получили ОДИН session id (%q) — это и есть живая жалоба", idAlpha)
	}
	if !strings.HasPrefix(idAlpha, "conv:") {
		t.Errorf("сессия собрана не по разговору: %q", idAlpha)
	}
	if idAlphaNext != idAlpha {
		t.Errorf("следующий ход той же задачи сменил сессию: %q vs %q", idAlphaNext, idAlpha)
	}
	// Отпечаток машины у обоих одинаков — различие даёт именно разговор.
	if fpA, fpB := p.getClientFingerprint(sessionTestRequest(taskAlpha)), p.getClientFingerprint(sessionTestRequest(taskBeta)); fpA != fpB {
		t.Fatalf("предпосылка теста неверна: отпечатки машины различаются (%q vs %q)", fpA, fpB)
	}
}

// TestR118_9_ShortPromptKeepsLegacySession — без различителя сессия остаётся
// прежней (fp::client::model::endpoint) — регрессии для простых клиентов нет.
func TestR118_9_ShortPromptKeepsLegacySession(t *testing.T) {
	p := newSessionTestProxy()
	short := `{"model":"m","messages":[{"role":"user","content":"привет"}]}`
	id := p.getSessionIDWithModel(sessionTestRequest(short), "curl/8.0", "m")
	if strings.HasPrefix(id, "conv:") {
		t.Errorf("короткий промпт не должен давать conv-сессию: %q", id)
	}
	if !strings.Contains(id, "::m::chat") {
		t.Errorf("сессия собрана не по прежнему шаблону: %q", id)
	}
}

// TestR118_9_IdentitySourceCountsConvAsIdentified — для счётчика «неразличимых
// ожидающих» сессия по разговору считается идентифицированной: два экземпляра с
// одной машины в ней различимы (в отличие от fp:/tkn:).
func TestR118_9_IdentitySourceCountsConvAsIdentified(t *testing.T) {
	if got := SessionIdentitySource("conv:3b9f12540d8bdace::gemma-4-E4B-it-Q4_K_M"); got != sessionIdentityClient {
		t.Errorf("conv-сессия = %q, want %q", got, sessionIdentityClient)
	}
	if got := SessionIdentitySource("fp:f0482b91e95bb409::Cline::gemma::chat"); got != sessionIdentityDerived {
		t.Errorf("fp-сессия = %q, want %q", got, sessionIdentityDerived)
	}
	if got := SessionIdentitySource(""); got != sessionIdentityNone {
		t.Errorf("пустая сессия = %q, want %q", got, sessionIdentityNone)
	}
}

// TestR118_9_RequestSessionKeyPrefersConversation — в admission-ключе разговор
// важнее id запроса: два экземпляра видны как две сессии, и одна задача остаётся
// одной сессией между ходами.
func TestR118_9_RequestSessionKeyPrefersConversation(t *testing.T) {
	req := sessionTestRequest(taskAlpha)
	req.Header.Set("X-Request-Id", "req-42")
	key := RequestSessionKey(req, []byte(taskAlpha))
	if !strings.HasPrefix(key, "conv:") {
		t.Fatalf("ключ не по разговору: %q (id запроса не должен побеждать)", key)
	}
	keyBeta := RequestSessionKey(sessionTestRequest(taskBeta), []byte(taskBeta))
	if key == keyBeta {
		t.Fatalf("два экземпляра получили один admission-ключ: %q", key)
	}
	// Явный пользователь по-прежнему важнее разговора.
	reqUser := sessionTestRequest(taskAlpha)
	reqUser.Header.Set("X-User-Id", "user-7")
	if got := RequestSessionKey(reqUser, []byte(taskAlpha)); got != "X-User-Id:user-7" {
		t.Errorf("явный пользователь перестал быть приоритетным: %q", got)
	}
}
