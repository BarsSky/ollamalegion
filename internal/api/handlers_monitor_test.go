package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestServerForMonitorUI — минимальный сервер с /monitor и статикой
// WebUI, зарегистрированной так же, как в routes.go.
//
// Тест читает реальный webui/monitor.html с диска, поэтому требует cwd =
// internal/api (это обычный режим `go test ./internal/api/`): webuiStaticDir
// перебирает кандидатов, включая ../../webui.
func setupTestServerForMonitorUI(t *testing.T) (*httptest.Server, string) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{},
		API:      types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth:     types.AuthConfig{Enabled: false},
	}
	proxy := balancer.NewProxy(config)
	server := NewServer(proxy, config, nil)
	server.SetEventBus(nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/monitor", server.monitorHandler)
	// R66c: ровно то, что делает routes.go — без этих маршрутов модульная
	// страница монитора остаётся без js/css и выглядит пустой.
	if staticDir, ok := webuiStaticDir(); ok {
		fileServer := http.FileServer(http.Dir(staticDir))
		mux.Handle("/js/", fileServer)
		mux.Handle("/css/", fileServer)
		mux.Handle("/img/", fileServer)
	}

	ts := httptest.NewServer(mux)
	return ts, ts.URL
}

// TestMonitorHandler_ServesCanonicalPage — R66c (2026-09-22).
//
// Регресс: handler искал только /app/monitor.html и legacy-копию
// cmd/monitor/monitor.html, тогда как docker/balancer/Dockerfile копирует в
// образ канонический webui/monitor.html (/app/webui/monitor.html). В бандле
// GET /monitor всегда отдавал 404 «Monitor page not found», хотя файл в образе
// был. Теперь канонический путь ищется первым.
//
// R66d: сама legacy-копия cmd/monitor/monitor.html удалена (дубликат
// webui/monitor.html, который никто не отдавал) — тест проверяет именно
// каноническую страницу.
func TestMonitorHandler_ServesCanonicalPage(t *testing.T) {
	server, url := setupTestServerForMonitorUI(t)
	defer server.Close()

	resp, err := http.Get(url + "/monitor")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "GET /monitor должен отдавать страницу")
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"),
		"страница не должна кэшироваться: она получает inline-конфиг")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	assert.Contains(t, html, "window.WEBUI_CONFIG",
		"в страницу должен быть встроен WEBUI_CONFIG (apiBase/API_TOKEN и т.д.)")
	// Канонический monitor.html — модульный: он подключает js/monitor/*.js и
	// js/modules/*.js. Именно поэтому балансер обязан отдавать статику.
	assert.Contains(t, html, "js/monitor/",
		"ожидалась каноническая webui/monitor.html (модульная, подключает js/monitor/*)")
}

// TestMonitorHandler_StaticAssetsServed — js/css из webui должны отдаваться
// балансером, иначе модульная страница монитора пустая.
func TestMonitorHandler_StaticAssetsServed(t *testing.T) {
	server, url := setupTestServerForMonitorUI(t)
	defer server.Close()

	for _, path := range []string{
		"/js/monitor/init.js",
		"/js/modules/config.js",
		"/css/monitor.css",
	} {
		resp, err := http.Get(url + path)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode, "GET %s должен вернуть 200", path)
		assert.NotEmpty(t, body, "GET %s вернул пустое тело", path)
	}
}

// TestMonitorHandler_MethodNotAllowed — POST /monitor → 405.
func TestMonitorHandler_MethodNotAllowed(t *testing.T) {
	server, url := setupTestServerForMonitorUI(t)
	defer server.Close()

	resp, err := http.Post(url+"/monitor", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}
