package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestAuthedRequest_BalancerTokenSet — Round 41 контракт #1: если
// cfg.BalancerToken != "", заголовок X-API-Token должен быть установлен.
// Это базовая гарантия: agent проходит auth на балансере (default header
// name в internal/api/auth.go — "X-API-Token").
func TestAuthedRequest_BalancerTokenSet(t *testing.T) {
	a := &Agent{
		config: &types.AgentConfig{
			AgentID:       "test-agent-001",
			BalancerToken: "secret-token-abc123",
		},
	}

	req, err := a.authedRequest(context.Background(), http.MethodPost,
		"http://balancer:18081/api/v1/agents/register",
		bytes.NewReader([]byte(`{"agentId":"test-agent-001"}`)))
	if err != nil {
		t.Fatalf("authedRequest failed: %v", err)
	}

	if got := req.Header.Get("X-API-Token"); got != "secret-token-abc123" {
		t.Errorf("X-API-Token: got %q, want %q", got, "secret-token-abc123")
	}
	if got := req.Header.Get("X-Agent-ID"); got != "test-agent-001" {
		t.Errorf("X-Agent-ID: got %q, want %q", got, "test-agent-001")
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: got %q, want %q", got, "application/json")
	}
	if req.Method != http.MethodPost {
		t.Errorf("Method: got %q, want POST", req.Method)
	}
}

// TestAuthedRequest_BalancerTokenEmpty — Round 41 контракт #2: если
// cfg.BalancerToken == "", заголовок X-API-Token НЕ выставляется.
// Backward-compat для dev-режима (auth выключен на балансере, agent
// всё равно работает). В проде (auth: enabled) пустой токен даст 401 —
// это документированное поведение, и cmd/agent/main.go пишет WARNING.
func TestAuthedRequest_BalancerTokenEmpty(t *testing.T) {
	a := &Agent{
		config: &types.AgentConfig{
			AgentID:       "test-agent-002",
			BalancerToken: "", // dev mode
		},
	}

	req, err := a.authedRequest(context.Background(), http.MethodPost,
		"http://balancer:18081/api/v1/agents/register",
		bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("authedRequest failed: %v", err)
	}

	// Token header must be ABSENT, not just empty.
	if _, present := req.Header["X-Api-Token"]; present {
		t.Errorf("X-API-Token header should NOT be set when BalancerToken is empty")
	}
	// X-Agent-ID and Content-Type are still set — это не зависит от auth.
	if got := req.Header.Get("X-Agent-ID"); got != "test-agent-002" {
		t.Errorf("X-Agent-ID: got %q, want %q", got, "test-agent-002")
	}
}

// TestAuthedRequest_GET_NoContentType — для GET/DELETE Content-Type не
// нужен (нет body). Проверяем, что helper не ставит Content-Type
// зря на GET — это упрощает wire-debug и соответствует RFC.
func TestAuthedRequest_GET_NoContentType(t *testing.T) {
	a := &Agent{
		config: &types.AgentConfig{
			AgentID:       "test-agent-003",
			BalancerToken: "tok",
		},
	}

	req, err := a.authedRequest(context.Background(), http.MethodGet,
		"http://balancer:18081/api/v1/agents/stats", nil)
	if err != nil {
		t.Fatalf("authedRequest failed: %v", err)
	}

	if got := req.Header.Get("Content-Type"); got != "" {
		t.Errorf("Content-Type on GET: got %q, want empty", got)
	}
	if got := req.Header.Get("X-Agent-ID"); got != "test-agent-003" {
		t.Errorf("X-Agent-ID: got %q, want %q", got, "test-agent-003")
	}
	if got := req.Header.Get("X-API-Token"); got != "tok" {
		t.Errorf("X-API-Token: got %q, want %q", got, "tok")
	}
}

// TestAuthedRequest_ContextPropagated — context должен корректно
// прокидываться в http.Request. Это критично для cancellation через
// context.WithTimeout в sendMetrics / sendHeartbeat.
func TestAuthedRequest_ContextPropagated(t *testing.T) {
	a := &Agent{
		config: &types.AgentConfig{AgentID: "ctx-test"},
	}

	type ctxKey string
	ctx := context.WithValue(context.Background(), ctxKey("request-id"), "abc-123")

	req, err := a.authedRequest(ctx, http.MethodPost, "http://x/y", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("authedRequest failed: %v", err)
	}

	if got := req.Context().Value(ctxKey("request-id")); got != "abc-123" {
		t.Errorf("Context value not propagated: got %v, want %q", got, "abc-123")
	}
}

// TestRegister_SendsAPIToken — интеграционный test: эмулируем балансер
// с AuthMiddleware-семантикой (отбиваем 401 если нет правильного
// X-API-Token). Без фикса R41 register возвращал 401 и зацикливал
// agent; с фиксом — успешная регистрация.
func TestRegister_SendsAPIToken(t *testing.T) {
	const wantToken = "shared-bundled-secret-2026"

	// Мок-«балансер» с auth-семантикой
	var gotToken string
	var gotAgentID string
	var registeredOK bool
	srv := mockAuthBalancer(wantToken, &gotToken, &gotAgentID, &registeredOK)
	defer srv.Close()

	cfg := &types.AgentConfig{
		AgentID:       "register-test-agent",
		BalancerURL:   srv.URL,
		BalancerToken: wantToken,
		BackendType:   types.BackendTypeOllama, // не llama_cpp, чтобы не вызывать extractCppWorker*
	}
	a := NewAgent(cfg) // ← используем конструктор, чтобы инициализировать balancerURL/httpClient

	if err := a.register(); err != nil {
		t.Fatalf("register() failed: %v", err)
	}

	if gotToken != wantToken {
		t.Errorf("balancer did not receive X-API-Token: got %q, want %q", gotToken, wantToken)
	}
	if gotAgentID != "register-test-agent" {
		t.Errorf("balancer did not receive X-Agent-ID: got %q, want %q", gotAgentID, "register-test-agent")
	}
	if !registeredOK {
		t.Error("register did not complete (mock balancer did not see successful POST)")
	}
}

// TestRegister_NoToken_Rejected — без токена (dev mismatch) балансер
// отвечает 401 → register возвращает ошибку. Agent не паникует,
// а корректно пробрасывает ошибку наверх (для retry-логики).
func TestRegister_NoToken_Rejected(t *testing.T) {
	// Мок-«балансер» БЕЗ токена (auth enabled, но токен в compose не задан)
	srv := mockAuthBalancer("expected-but-not-sent", new(string), new(string), new(bool))
	defer srv.Close()

	cfg := &types.AgentConfig{
		AgentID:       "no-token-agent",
		BalancerURL:   srv.URL,
		BalancerToken: "", // ← env var not set
		BackendType:   types.BackendTypeOllama,
	}
	a := NewAgent(cfg)

	err := a.register()
	if err == nil {
		t.Fatal("expected register() to fail when BalancerToken is empty and auth is enabled, but it succeeded")
	}
	// Должна быть ошибка содержащая "401" — это документированный контракт.
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401 status: got %q", err.Error())
	}
}

// mockAuthBalancer — мок-сервер, эмулирующий AuthMiddleware семантику
// балансера из internal/api/auth.go. Возвращает 401 если expectedToken
// не совпадает с X-API-Token, иначе 200 + success-ответ register-формата.
//
// gotToken / gotAgentID / registeredOK — out-параметры для проверки в тесте
// (что именно «балансер» увидел в запросе).
func mockAuthBalancer(expectedToken string, gotToken, gotAgentID *string, registeredOK *bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotToken = r.Header.Get("X-API-Token")
		*gotAgentID = r.Header.Get("X-Agent-ID")

		if expectedToken != "" && *gotToken != expectedToken {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"success":false,"error":"Unauthorized: valid API token required"}`))
			return
		}

		// Successful register response (format из handlers_agents.go:245-254)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":true,"action":"created","agentId":"mock-agent","backendId":"mock-agent","message":"registered"}`))
		*registeredOK = true
	}))
}
