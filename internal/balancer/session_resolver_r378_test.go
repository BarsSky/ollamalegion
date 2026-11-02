package balancer

// session_resolver_r378_test.go — R378 (2026-11-01): один резолвер ключей
// сессии/очереди (P2-3 из аудита регрессий).
//
// Тесты фиксируют две вещи:
//  1. ПОВЕДЕНИЕ не изменилось: ключ очереди на каждый набор источников даёт
//     ровно то же значение, что и до переноса в один файл (золотая матрица);
//  2. ИСТОЧНИКИ живут в одном месте: новый заголовок идентичности, добавленный
//     мимо session_resolver_r378.go, роняет стюард, а список проброса
//     self-proxy обязан включать все источники.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestR378_RequestSessionKey_GoldenMatrix — поведение ключа очереди до и после
// переноса обязано совпадать: это ключ кошелька справедливости, его смена
// перетасовала бы очередь у живых клиентов.
func TestR378_RequestSessionKey_GoldenMatrix(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		body    string
		want    string
	}{
		{
			name:    "пользователь в заголовке важнее всего",
			headers: map[string]string{"X-User-Id": "u1", "X-Session-Id": "s1"},
			body:    `{"user":"from-body"}`,
			want:    "X-User-Id:u1",
		},
		{
			name:    "OpenWebUI-заголовок пользователя",
			headers: map[string]string{"X-OpenWebUI-User-Id": "owui-7"},
			want:    "X-OpenWebUI-User-Id:owui-7",
		},
		{
			name:    "почта пользователя",
			headers: map[string]string{"X-User-Email": "a@b.c"},
			want:    "X-User-Email:a@b.c",
		},
		{
			name:    "сессия из заголовка",
			headers: map[string]string{"X-Session-Id": "sess-1"},
			want:    "X-Session-Id:sess-1",
		},
		{
			name:    "чат из заголовка считается сессией",
			headers: map[string]string{"X-Chat-Id": "chat-9"},
			want:    "X-Chat-Id:chat-9",
		},
		{
			name:    "разговор из заголовка",
			headers: map[string]string{"X-Conversation-Id": "conv-2"},
			want:    "X-Conversation-Id:conv-2",
		},
		{
			name: "пользователь из тела",
			body: `{"user":"body-user"}`,
			want: "user:body-user",
		},
		{
			name: "пользователь из metadata",
			body: `{"metadata":{"user_id":"md-user"}}`,
			want: "user:md-user",
		},
		{
			name: "сессия из тела",
			body: `{"session_id":"body-sess"}`,
			want: "session:body-sess",
		},
		{
			name: "чат из тела",
			body: `{"chat_id":"body-chat"}`,
			want: "chat:body-chat",
		},
		{
			name:    "id запроса как последний рубеж",
			headers: map[string]string{"X-Request-Id": "req-1"},
			want:    "request:req-1",
		},
		{
			name: "анонимный клиент",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader([]byte(tc.body)))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			got := RequestSessionKey(req, []byte(tc.body))
			if got != tc.want {
				t.Errorf("RequestSessionKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestR378_ResolveInferenceKeys_FromSameSources — оба ключа считаются вместе и
// видят ОДИН и тот же источник: если пользователь назван, ключ очереди помечен
// его заголовком, а маршрутная сессия — префиксом usr:.
func TestR378_ResolveInferenceKeys_FromSameSources(t *testing.T) {
	p := admissionTestProxy(t, 4)

	mk := func(user string) *http.Request {
		body := `{"model":"gemma","messages":[{"role":"user","content":"привет"}]}`
		req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader([]byte(body)))
		req.RemoteAddr = "172.18.0.1:5555"
		req.Header.Set("User-Agent", "openwebui/1.0")
		if user != "" {
			req.Header.Set("X-User-Id", user)
		}
		return req
	}

	alice := p.resolveInferenceKeysR378(mk("alice"), "openwebui", "gemma", []byte(`{"model":"gemma"}`))
	if !strings.HasPrefix(alice.QueueKey, "X-User-Id:alice") {
		t.Errorf("ключ очереди не увидел пользователя: %q", alice.QueueKey)
	}
	if !strings.HasPrefix(alice.SessionID, "usr:") {
		t.Errorf("маршрутная сессия не увидела пользователя: %q", alice.SessionID)
	}
	if alice.QueueSource != sessionIdentityClient {
		t.Errorf("источник ключа очереди = %q, ожидался %q", alice.QueueSource, sessionIdentityClient)
	}

	bob := p.resolveInferenceKeysR378(mk("bob"), "openwebui", "gemma", []byte(`{"model":"gemma"}`))
	if alice.QueueKey == bob.QueueKey {
		t.Errorf("разные пользователи получили один ключ очереди %q", alice.QueueKey)
	}
	if alice.SessionID == bob.SessionID {
		t.Errorf("разные пользователи получили одну сессию %q", alice.SessionID)
	}

	// Анонимный клиент: ключа очереди нет, но маршрутная сессия выводится из
	// отпечатка — так и было до R378.
	anon := p.resolveInferenceKeysR378(mk(""), "openwebui", "gemma", []byte(`{"model":"gemma"}`))
	if anon.QueueKey != "" {
		t.Errorf("анонимный клиент не должен получать ключ очереди, got %q", anon.QueueKey)
	}
	if anon.QueueSource != sessionIdentityNone {
		t.Errorf("источник анонимного ключа = %q, ожидался %q", anon.QueueSource, sessionIdentityNone)
	}
	if anon.SessionID == "" {
		t.Error("маршрутная сессия анонимного клиента не должна быть пустой")
	}
}

// TestR378_SelfProxyForwardsAllIdentityHeaders — проброс заголовков во
// внутренний запрос обязан включать ВСЕ источники идентичности: иначе пробный
// запрос уедет с сессией от 127.0.0.1 (регресс R253.1).
func TestR378_SelfProxyForwardsAllIdentityHeaders(t *testing.T) {
	have := make(map[string]bool, len(selfProxyForwardHeaders))
	for _, h := range selfProxyForwardHeaders {
		have[strings.ToLower(h)] = true
	}
	required := mergeUniqueR378(
		identityUserHeadersR378,
		identitySessionHeadersR378,
		identityChatHeadersR378,
		identityRequestIDHeadersR378,
	)
	for _, h := range required {
		if !have[strings.ToLower(h)] {
			t.Errorf("selfProxyForwardHeaders не пробрасывает %q — источники идентичности разошлись", h)
		}
	}
}

// TestR378_Steward_IdentitySourcesLiveHere — стюард: заголовки пользователя/чата
// упоминаются в коде только в резолвере. Если кто-то добавит новый источник в
// другом файле, ключ очереди и ключ маршрутизации снова начнут расходиться.
func TestR378_Steward_IdentitySourcesLiveHere(t *testing.T) {
	identityHeaders := []string{
		"X-User-Id", "X-OpenWebUI-User-Id", "X-User-Email",
		"X-Chat-Id", "X-Conversation-Id",
	}
	allowedFiles := map[string]bool{"session_resolver_r378.go": true}

	files, err := filepath.Glob("*.go") // тесты идут с cwd = каталог пакета
	if err != nil {
		t.Fatalf("не прочитать каталог пакета: %v", err)
	}
	seen := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("не разобрать %s: %v", path, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value := strings.Trim(lit.Value, "`\"")
			for _, h := range identityHeaders {
				if !strings.EqualFold(value, h) {
					continue
				}
				seen++
				base := filepath.Base(path)
				if !allowedFiles[base] {
					t.Errorf("%s: заголовок идентичности %q вне резолвера — добавьте источник в "+
						"session_resolver_r378.go, иначе ключ очереди и ключ сессии разойдутся", path, h)
				}
			}
			return true
		})
	}
	if seen == 0 {
		t.Fatal("стюард не нашёл ни одного заголовка идентичности — он проверяет не то")
	}
}
