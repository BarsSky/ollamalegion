//go:build llama_stub

// r479_type_parity_test.go — R479 (2026-11-09): паритет типов на поверхностях.
//
// Живые жалобы (аудит типов бэкендов, P0):
//  1. «потеряна работа с разными типами моделей»: /v1/* жёстко помечался
//     llama_cpp, поэтому на кластере без cppworker клиент получал 503, хотя
//     Ollama умеет OpenAI-совместимый API;
//  2. префикс /ollama нормализовал только LlamaCppRouter — на Ollama-only
//     кластере /ollama/api/tags был пуст;
//  3. /v1/models не содержал моделей Ollama-пула.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR479_SurfaceAliasNormalized — алиасы снимаются, чужие пути не трогаем.
func TestR479_SurfaceAliasNormalized(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		alias  bool
		reason string
	}{
		{"/ollama/api/tags", "/api/tags", true, "префикс OpenWebUI"},
		{"/ollama/api/chat", "/api/chat", true, "инференс через алиас"},
		{"/ollama/v1/models", "/v1/models", true, "OpenAI через алиас ollama"},
		{"/openai/v1/chat/completions", "/v1/chat/completions", true, "алиас openai"},
		{"/api/tags", "/api/tags", false, "прямой путь не меняем"},
		{"/ollama/health", "/ollama/health", false, "неизвестный путь не выдумываем"},
		{"/v1/models", "/v1/models", false, "прямой OpenAI-путь"},
	}
	for _, c := range cases {
		got, alias := normalizeSurfaceAliasPathR479(c.in)
		if got != c.want || alias != c.alias {
			t.Errorf("%s: got (%q,%v), want (%q,%v) [%s]", c.in, got, alias, c.want, c.alias, c.reason)
		}
	}
}

// ollamaFakeForR479 — фальшивый Ollama: /api/tags и стриминговый
// /v1/chat/completions. Возвращает сервер и канал «что пришло».
func ollamaFakeForR479(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"ollama-model-a"},{"name":"ollama-model-b"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"привет"}}]}` + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if fl != nil {
				fl.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// proxyWithOllamaR479 — Proxy с одним Ollama-бэкендом, указывающим на fake.
func proxyWithOllamaR479(t *testing.T, srv *httptest.Server) *Proxy {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url: %v", err)
	}
	port := 80
	if p := u.Port(); p != "" {
		if _, err := fmtSscanfR479(p, &port); err != nil {
			t.Fatalf("port: %v", err)
		}
	}
	return &Proxy{
		backends: map[string]*BackendState{
			"o1": {Backend: &types.Backend{
				ID: "o1", Host: u.Hostname(), OllamaPort: port,
				Type: types.BackendTypeOllama, Status: types.StatusHealthy,
				MaxConcurrentReqs: 2,
			}},
		},
		metricsMgr: NewMetricsManager(),
		admission:  newAdmissionQueue(time.Second),
		config:     &types.LoadBalancerConfig{},
	}
}

// fmtSscanfR479 — маленький парсер порта без лишних зависимостей.
func fmtSscanfR479(s string, out *int) (int, error) {
	n := 0
	if s == "" {
		return 0, nil
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errBadPortR479
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return 1, nil
}

// errBadPortR479 — ошибка разбора порта в тесте.
var errBadPortR479 = &portErrorR479{}

type portErrorR479 struct{}

func (e *portErrorR479) Error() string { return "bad port" }

// TestR479_OpenAIChatGoesToOllama — запрос /v1/chat/completions без cppworker
// доходит до Ollama и клиент получает поток, а не 503.
func TestR479_OpenAIChatGoesToOllama(t *testing.T) {
	srv, seen := ollamaFakeForR479(t)
	p := proxyWithOllamaR479(t, srv)

	body := []byte(`{"model":"ollama-model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	if ok := p.proxyOpenAINativeToOllama(rec, req, body); !ok {
		t.Fatal("запрос не обслужен Ollama-бэкендом (вернулся бы 503)")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, ожидался 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "привет") {
		t.Fatalf("поток не дошёл до клиента: %q", rec.Body.String())
	}
	if got := rec.Header().Get("X-Backend-ID"); got != "o1" {
		t.Fatalf("X-Backend-ID = %q, ожидался o1", got)
	}
	if len(*seen) == 0 || (*seen)[0] != "/v1/chat/completions" {
		t.Fatalf("апстриму ушёл не тот путь: %v", *seen)
	}
	if req := p.backends["o1"]; req.ActiveReqs != 0 {
		t.Fatalf("слот не освобождён: ActiveReqs=%d", req.ActiveReqs)
	}
}

// TestR479_UnsupportedOpenAIPathNotForwarded — /v1/responses не отдаём в Ollama
// «как есть»: там другой формат, честный путь — прежний код.
func TestR479_UnsupportedOpenAIPathNotForwarded(t *testing.T) {
	srv, seen := ollamaFakeForR479(t)
	p := proxyWithOllamaR479(t, srv)

	body := []byte(`{"model":"m"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	if ok := p.proxyOpenAINativeToOllama(rec, req, body); ok {
		t.Fatal("неподдерживаемый путь ушёл в Ollama passthrough")
	}
	if len(*seen) != 0 {
		t.Fatalf("апстрим тронут для неподдерживаемого пути: %v", *seen)
	}
}

// TestR479_OpenAIModelsIncludeOllama — /v1/models видит модели Ollama-пула.
func TestR479_OpenAIModelsIncludeOllama(t *testing.T) {
	srv, _ := ollamaFakeForR479(t)
	p := proxyWithOllamaR479(t, srv)
	lr := &LlamaCppRouter{proxy: p}

	unique := make(map[string]bool)
	lr.addOllamaModelIDsR479(unique)

	for _, want := range []string{"ollama-model-a", "ollama-model-b"} {
		if !unique[want] {
			t.Fatalf("в /v1/models нет %q (получено: %v)", want, unique)
		}
	}
}
