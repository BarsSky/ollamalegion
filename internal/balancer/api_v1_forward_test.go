package balancer

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestIsAPIv1Path — Round 40 #3 (2026-08-18): path classification.
//
// /api/v1/* пути — admin / management endpoints обслуживаемые
// API-сервером (порт 18081), не proxy flow. Без правильной классификации
// balancer проксирует /api/v1/* на cppworker → 404 → 30s timeout.
func TestIsAPIv1Path(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		// True cases — все под /api/v1/
		{"cppworker config", "/api/v1/cppworker/config", true},
		{"cppworker config update", "/api/v1/cppworker/config/update", true},
		{"cppworker config reload", "/api/v1/cppworker/config/reload", true},
		{"cppworker runtime", "/api/v1/cppworker/config/runtime", true},
		{"cppworker health", "/api/v1/cppworker/health", true},
		{"cppworker metrics", "/api/v1/cppworker/metrics", true},
		{"cluster models loaded", "/api/v1/cluster/models/loaded", true},
		{"cluster models info", "/api/v1/cluster/models/llama-3/info", true},
		{"cluster gguf backends", "/api/v1/gguf/backends", true},
		{"rpc tp infer", "/api/v1/rpc/tp/infer", true},
		{"events sse", "/api/v1/events", true},
		{"llama-cpp overrides", "/api/v1/cluster/llama-cpp/overrides", true},
		{"stats tokens", "/api/v1/stats/tokens", true},
		{"cppworker reset counter", "/api/v1/cppworker/reset-reload-counter", true},

		// False cases — должны идти через основной proxy flow
		{"root", "/", false},
		{"health", "/health", false},
		{"ollama chat", "/api/chat", false},
		{"ollama generate", "/api/generate", false},
		{"ollama tags", "/api/tags", false},
		{"ollama show", "/api/show", false},
		{"ollama pull", "/api/pull", false},
		{"ollama embeddings", "/api/embeddings", false},
		{"ollama embed", "/api/embed", false},
		{"openai chat completions", "/v1/chat/completions", false},
		{"openai completions", "/v1/completions", false},
		{"openai models", "/v1/models", false},
		{"openai embeddings", "/v1/embeddings", false},
		{"api v2 (future)", "/api/v2/cppworker/config", false}, // strict match
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isAPIv1Path(tc.path)
			if got != tc.want {
				t.Errorf("isAPIv1Path(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestServeAPIv1Request_Forward — Round 40 #3 (2026-08-18): проверяет что
// /api/v1/* запросы forward'ятся на локальный API-сервер, а не в proxy flow.
//
// Поднимаем реальный httptest.Server (имитирует API server на 18081) и
// реальный Proxy с apiReverseProxy, указывающим на этот сервер.
// Отправляем GET /api/v1/cppworker/config → должны получить тело,
// которое вернул API сервер, и proxy НЕ должен дёргать backend.
func TestServeAPIv1Request_Forward(t *testing.T) {
	// Имитация API-сервера: возвращает "API_OK" + 200 OK на /api/v1/*,
	// 404 на остальные.
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/cppworker/config" {
			w.Header().Set("X-Test-Forwarded", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"API_OK","path":"` + r.URL.Path + `"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer apiServer.Close()

	// Конфиг proxy с APIPort указывающим на apiServer
	apiURL := mustParseURL(t, apiServer.URL)
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "127.0.0.1",
			Port:    18080,
			APIPort: mustPortToInt(t, apiURL),
		},
		Backends: []types.Backend{},
	}

	proxy := newProxyWithCleanup(t, conf)
	defer proxy.queueMgr.Stop()

	// Переписываем apiReverseProxy чтобы он указывал на тестовый сервер
	// (NewProxy использует conf.LoadBalancer.APIPort, но мы хотим
	// ephemeral порт тестового сервера).
	proxy.apiReverseProxy = mustNewSingleHost(apiURL)

	// Отправляем /api/v1/cppworker/config через serveAPIv1Request
	req := httptest.NewRequest("GET", "/api/v1/cppworker/config", nil)
	req.Header.Set("X-API-Token", "test-token-12345")
	w := httptest.NewRecorder()

	handled := proxy.serveAPIv1Request(w, req)
	if !handled {
		t.Fatal("serveAPIv1Request returned false, expected true")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if body != `{"status":"API_OK","path":"/api/v1/cppworker/config"}` {
		t.Errorf("unexpected body: %s", body)
	}
	if w.Header().Get("X-Test-Forwarded") != "true" {
		t.Errorf("expected X-Test-Forwarded header from API server, got %q",
			w.Header().Get("X-Test-Forwarded"))
	}
}

// TestServeAPIv1Request_NotConfigured — Round 40 #3 (2026-08-18):
// если apiReverseProxy == nil, возвращаем 503 а не panic.
// (Defensive: NewProxy всегда инициализирует, но тест покрывает edge case.)
func TestServeAPIv1Request_NotConfigured(t *testing.T) {
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "127.0.0.1", Port: 18080, APIPort: 18081,
		},
		Backends: []types.Backend{},
	}
	proxy := newProxyWithCleanup(t, conf)
	defer proxy.queueMgr.Stop()
	proxy.apiReverseProxy = nil // force unconfigured

	req := httptest.NewRequest("GET", "/api/v1/cppworker/config", nil)
	w := httptest.NewRecorder()
	handled := proxy.serveAPIv1Request(w, req)
	if !handled {
		t.Fatal("expected handled=true even when not configured (must return 503)")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// TestRouteRequest_APIv1ShortCircuits — Round 40 #3 (2026-08-18):
// routeRequest при /api/v1/* возвращает true ДО проверки unsupported
// endpoints (audio/images). Это критично: audio/images 404 уже
// обрабатывается, но /api/v1/* должны идти на API server, а не 404.
func TestRouteRequest_APIv1ShortCircuits(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"from":"api"}`))
	}))
	defer apiServer.Close()

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "127.0.0.1", Port: 18080, APIPort: 18081,
		},
		Backends: []types.Backend{},
	}
	proxy := newProxyWithCleanup(t, conf)
	defer proxy.queueMgr.Stop()
	proxy.apiReverseProxy = mustNewSingleHost(mustParseURL(t, apiServer.URL))

	req := httptest.NewRequest("GET", "/api/v1/cluster/models/loaded", nil)
	w := httptest.NewRecorder()
	handled := proxy.routeRequest(w, req)
	if !handled {
		t.Error("routeRequest returned false, expected true for /api/v1/*")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"from":"api"}` {
		t.Errorf("body not from API server: %s", w.Body.String())
	}
}

// TestRouteRequest_APIv1_VsUnsupportedOpenAI — Round 40 #3 (2026-08-18):
// /api/v1/* имеет приоритет над isUnsupportedOpenAIEndpoint.
// (Прямо сейчас /api/v1/audio/* не матчится isUnsupportedOpenAIEndpoint,
// но проверим что в принципе ordering корректен.)
func TestRouteRequest_APIv1_VsUnsupportedOpenAI(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"forwarded":true}`))
	}))
	defer apiServer.Close()

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "127.0.0.1", Port: 18080, APIPort: 18081,
		},
		Backends: []types.Backend{},
	}
	proxy := newProxyWithCleanup(t, conf)
	defer proxy.queueMgr.Stop()
	proxy.apiReverseProxy = mustNewSingleHost(mustParseURL(t, apiServer.URL))

	// /api/v1/cppworker/something — должен forward'иться на API server,
	// а не возвращать 404 (даже если бы был в unsupported списке).
	req := httptest.NewRequest("GET", "/api/v1/cppworker/custom-action", nil)
	w := httptest.NewRecorder()
	handled := proxy.routeRequest(w, req)
	if !handled {
		t.Error("expected handled=true")
	}
	if w.Body.String() != `{"forwarded":true}` {
		t.Errorf("body not from API server: %s", w.Body.String())
	}
}

// TestNewProxy_InitializesAPIReverseProxy — Round 40 #3 (2026-08-18):
// NewProxy должен инициализировать apiReverseProxy (не nil) при любых
// корректных conf. Это контракт — если он сломан, /api/v1/* все сломается.
func TestNewProxy_InitializesAPIReverseProxy(t *testing.T) {
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "127.0.0.1", Port: 18080, APIPort: 18081,
		},
		Backends: []types.Backend{},
	}
	proxy := newProxyWithCleanup(t, conf)
	defer proxy.queueMgr.Stop()
	if proxy.apiReverseProxy == nil {
		t.Fatal("apiReverseProxy not initialized after NewProxy")
	}
}

// TestNewProxy_DefaultAPIPort — Round 40 #3 (2026-08-18):
// если APIPort = 0, используется default 18081.
func TestNewProxy_DefaultAPIPort(t *testing.T) {
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "127.0.0.1", Port: 18080, APIPort: 0, // default
		},
		Backends: []types.Backend{},
	}
	proxy := newProxyWithCleanup(t, conf)
	defer proxy.queueMgr.Stop()
	if proxy.apiReverseProxy == nil {
		t.Fatal("apiReverseProxy not initialized with default APIPort")
	}
	// Sanity: проверяем что URL указывает на порт 18081
	// (внутри Director httputil, не экспонируется, но мы можем
	// проверить что FlushInterval выставлен)
	if proxy.apiReverseProxy.FlushInterval == 0 {
		t.Error("FlushInterval not set (expected 100ms)")
	}
}

// --- helpers ---

// mustParseURL — обёртка url.Parse с t.Fatalf
func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	return u
}

// mustNewSingleHost — обёртка httputil.NewSingleHostReverseProxy
func mustNewSingleHost(target *url.URL) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = 100 * 1_000_000 // 100ms in nanoseconds
	return rp
}

// mustPortToInt — конвертирует url.URL.Port() (string) в int с t.Fatalf
func mustPortToInt(t *testing.T, u *url.URL) int {
	t.Helper()
	p, err := parsePort(u)
	if err != nil {
		t.Fatalf("parsePort(%v): %v", u, err)
	}
	return p
}

// parsePort — парсит port из url.URL (Go 1.20+ Port() возвращает string)
func parsePort(u *url.URL) (int, error) {
	if p := u.Port(); p != "" {
		return atoi(p)
	}
	// Default для scheme
	switch u.Scheme {
	case "https":
		return 443, nil
	case "http":
		return 80, nil
	}
	return 0, nil
}

// atoi — простой string→int без strconv import
func atoi(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, nil // мягкий fallback
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
