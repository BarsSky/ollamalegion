// session_surface_r490_test.go — R490 (2026-11-11): одна сессия на двух API.
//
// ЖИВОЙ ДЕФЕКТ. Рабочий разговор шёл по `/api/chat` (порт 18080), а новый — по
// `/v1/chat/completions` (18079). `getSessionIDWithModel` добавлял в ключ
// `endpointType`, а `getOllamaEndpointType` даёт для этих путей `chat` и `api`
// соответственно — то есть один и тот же разговор получал ДВЕ сессии: свою
// привязку к бэкенду, свой кошелёк admission-очереди и свою строку в панели.
// Балансер не мог ни увидеть, что бэкенд уже свободен для этой заявки, ни
// сопоставить отмену/очередь с тем же клиентом.
//
// ПРАВИЛО R490: если клиент назвал себя (X-Client-ID, X-Session-ID, cookie,
// пользователь, chat_id, отпечаток разговора) — поверхность API в ключ НЕ
// входит. Аноним (только IP+User-Agent) по-прежнему разделяется по поверхности:
// без явной личности отличить «чат» от «генерации» больше нечем.
package balancer

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sessionReqR490 собирает запрос к указанной поверхности с заданными заголовками.
func sessionReqR490(path, user, clientID, chat string) *http.Request {
	body := `{"model":"gemma","messages":[]}`
	if chat != "" {
		body = `{"model":"gemma","messages":[],"metadata":{"chat_id":"` + chat + `"}}`
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.RemoteAddr = "172.18.0.1:5555"
	req.Header.Set("User-Agent", "openwebui/1.0")
	if user != "" {
		req.Header.Set("X-User-Id", user)
	}
	if clientID != "" {
		req.Header.Set("X-Client-ID", clientID)
	}
	return req
}

// TestR490_SameUserAcrossSurfacesIsOneSession — пользователь на 18080 и 18079.
func TestR490_SameUserAcrossSurfacesIsOneSession(t *testing.T) {
	p := admissionTestProxy(t, 4)
	ollama := p.getSessionIDWithModel(sessionReqR490("/api/chat", "alice", "", ""), "openwebui", "gemma")
	openai := p.getSessionIDWithModel(sessionReqR490("/v1/chat/completions", "alice", "", ""), "openwebui", "gemma")
	if ollama != openai {
		t.Fatalf("один пользователь на двух поверхностях получил разные сессии:\n  /api/chat = %s\n  /v1/...   = %s", ollama, openai)
	}

	other := p.getSessionIDWithModel(sessionReqR490("/v1/chat/completions", "bob", "", ""), "openwebui", "gemma")
	if other == openai {
		t.Fatal("разные пользователи слились в одну сессию (регрессия R67b)")
	}
}

// TestR490_SameClientIDAcrossSurfacesIsOneSession — явный X-Client-ID.
func TestR490_SameClientIDAcrossSurfacesIsOneSession(t *testing.T) {
	p := admissionTestProxy(t, 4)
	a := p.getSessionIDWithModel(sessionReqR490("/api/chat", "", "cline-1", ""), "cline", "gemma")
	b := p.getSessionIDWithModel(sessionReqR490("/v1/chat/completions", "", "cline-1", ""), "cline", "gemma")
	if a != b {
		t.Fatalf("один клиент на двух поверхностях получил разные сессии: %s vs %s", a, b)
	}
	c := p.getSessionIDWithModel(sessionReqR490("/api/chat", "", "cline-2", ""), "cline", "gemma")
	if c == a {
		t.Fatal("разные клиенты слились в одну сессию")
	}
}

// TestR490_SameChatAcrossSurfacesIsOneSession — chat_id разговора.
func TestR490_SameChatAcrossSurfacesIsOneSession(t *testing.T) {
	p := admissionTestProxy(t, 4)
	a := p.getSessionIDWithModel(sessionReqR490("/api/chat", "", "", "chat-42"), "openwebui", "gemma")
	b := p.getSessionIDWithModel(sessionReqR490("/v1/chat/completions", "", "", "chat-42"), "openwebui", "gemma")
	if a != b {
		t.Fatalf("один чат на двух поверхностях получил разные сессии: %s vs %s", a, b)
	}
	d := p.getSessionIDWithModel(sessionReqR490("/api/chat", "", "", "chat-43"), "openwebui", "gemma")
	if d == a {
		t.Fatal("разные чаты слились в одну сессию")
	}
}

// TestR490_AnonymousStillSplitBySurface — без явной личности разделяем по
// поверхности: это единственный оставшийся признак «чат vs генерация».
func TestR490_AnonymousStillSplitBySurface(t *testing.T) {
	p := admissionTestProxy(t, 4)
	chat := p.getSessionIDWithModel(sessionReqR490("/api/chat", "", "", ""), "anon", "gemma")
	v1 := p.getSessionIDWithModel(sessionReqR490("/v1/chat/completions", "", "", ""), "anon", "gemma")
	if chat == v1 {
		t.Fatalf("анонимные запросы разных поверхностей слились в %q — по ним нет явной личности", chat)
	}
	same1 := p.getSessionIDWithModel(sessionReqR490("/api/chat", "", "", ""), "anon", "gemma")
	if same1 != chat {
		t.Fatalf("анонимные запросы одной поверхности разошлись: %s vs %s", same1, chat)
	}
}

// TestR490_ModelStillSeparatesSessions — модель остаётся частью ключа.
func TestR490_ModelStillSeparatesSessions(t *testing.T) {
	p := admissionTestProxy(t, 4)
	a := p.getSessionIDWithModel(sessionReqR490("/api/chat", "alice", "", ""), "openwebui", "gemma")
	b := p.getSessionIDWithModel(sessionReqR490("/api/chat", "alice", "", ""), "openwebui", "qwen")
	if a == b {
		t.Fatal("одна сессия на две разные модели — регрессия")
	}
}
