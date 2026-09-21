//go:build llama_stub

// admin_auth_r65d_test.go — R65d (2026-09-20): регрессия на авторизацию
// admin-эндпоинтов.
//
// Найденные дефекты (аудит 2026-09-20, находка 1.4):
//
//  1. GET/PUT /api/v1/admin/autotune/config регистрировались через HandleFunc
//     БЕЗ AuthMiddleware, хотя соседние /api/v1/admin/autotune и
//     /api/v1/admin/autotune/ были защищены. Literal-паттерн в http.ServeMux
//     выигрывает у prefix-паттерна, поэтому PUT без токена менял
//     balancing.autoTune и per-model autoTune и сохранял конфиг на диск.
//
//  2. /api/v1/gguf/backends/ (прокси к cppworker) тоже был без авторизации и
//     принимает GET/POST/PUT/DELETE, передавая любой путь в cppworker —
//     включая load/unload/delete моделей и запуск загрузок.
//
// Эти тесты фиксируют, что ручки отвечают 401 без токена и что
// публичный список бэкендов (без слэша) остаётся доступным.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// authEnabledServer создаёт сервер с включённой аутентификацией.
//
// ВАЖНО: токены должны быть в конфиге ДО NewServer. `setupRoutes` замыкает
// s.authenticator в момент регистрации маршрутов, поэтому подмена поля после
// создания сервера не влияет на уже зарегистрированные обёртки AuthMiddleware
// (именно на этом сначала «сломался» этот тест: auth включался, а маршруты
// продолжали пускать без токена).
func authEnabledServer(t *testing.T, backends ...types.Backend) *Server {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends:     backends,
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			ModelAffinity:       true,
			HealthCheckInterval: 10,
			MetricsInterval:     5,
			RequestTimeout:      30,
			QueueTimeout:        60,
			QueueMaxSize:        100,
			QueueWorkers:        4,
		},
		API: types.APISettings{RateLimit: 1000, RateBurst: 2000},
		Auth: types.AuthConfig{
			Enabled: true,
			Tokens:  []string{"r65d-secret-token"},
		},
	}

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	return NewServer(proxy, config, healthChecker)
}

// doRequest выполняет запрос через mux сервера и возвращает код ответа.
func doRequest(t *testing.T, s *Server, method, path, token string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("X-API-Token", token)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec.Code
}

// TestR65d_AdminAutotuneConfig_RequiresAuth — PUT/GET config без токена → 401.
func TestR65d_AdminAutotuneConfig_RequiresAuth(t *testing.T) {
	s := authEnabledServer(t)

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			if code := doRequest(t, s, method, "/api/v1/admin/autotune/config", ""); code != http.StatusUnauthorized {
				t.Errorf("%s /api/v1/admin/autotune/config without token = %d, want 401 "+
					"(было: 200 — эндпоинт мутировал конфиг без авторизации)", method, code)
			}
			// С токеном ручка должна быть доступна (не 401/403).
			if code := doRequest(t, s, method, "/api/v1/admin/autotune/config", "r65d-secret-token"); code == http.StatusUnauthorized {
				t.Errorf("%s with valid token unexpectedly 401", method)
			}
		})
	}
}

// TestR65d_AdminAutotuneHistory_RequiresAuth — история reload'ов без токена → 401.
func TestR65d_AdminAutotuneHistory_RequiresAuth(t *testing.T) {
	s := authEnabledServer(t)

	if code := doRequest(t, s, http.MethodGet, "/api/v1/admin/autotune/history", ""); code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/admin/autotune/history without token = %d, want 401", code)
	}
}

// TestR65d_GgufBackendProxy_RequiresAuth — прокси к cppworker без токена → 401.
//
// Это критично: через прокси можно вызвать POST /api/models/load,
// /api/models/delete, /api/hf/download на cppworker.
func TestR65d_GgufBackendProxy_RequiresAuth(t *testing.T) {
	s := authEnabledServer(t)

	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/gguf/backends/b1/proxy/api/models"},
		{http.MethodPost, "/api/v1/gguf/backends/b1/proxy/api/models/load"},
		{http.MethodPost, "/api/v1/gguf/backends/b1/proxy/api/models/delete"},
		{http.MethodPost, "/api/v1/gguf/backends/b1/proxy/api/hf/download"},
		{http.MethodDelete, "/api/v1/gguf/backends/b1/proxy/api/models"},
	}
	for _, tc := range paths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if code := doRequest(t, s, tc.method, tc.path, ""); code != http.StatusUnauthorized {
				t.Errorf("%s %s without token = %d, want 401 "+
					"(было: запрос без токена доходил до cppworker)", tc.method, tc.path, code)
			}
		})
	}
}

// TestR65d_GgufBackendProxy_AcceptsQueryToken — EventSource не умеет custom
// headers, поэтому AuthMiddleware должен принимать токен и из ?token=.
//
// Именно так работает SSE-прогресс загрузки в WebUI
// (gguf-api.js buildBackendProxyUrl добавляет ?token=).
func TestR65d_GgufBackendProxy_AcceptsQueryToken(t *testing.T) {
	s := authEnabledServer(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/gguf/backends/b1/proxy/api/models?token=r65d-secret-token", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Errorf("query token rejected (401) — SSE-прогресс загрузки сломается, "+
			"т.к. EventSource не может передать заголовок. Body: %s", rec.Body.String())
	}
}

// TestR65d_GgufBackendsList_StaysPublic — список бэкендов (без слэша) остаётся
// публичным: он отдаёт только метаданные и нужен странице для первичной отрисовки.
func TestR65d_GgufBackendsList_StaysPublic(t *testing.T) {
	s := authEnabledServer(t)

	if code := doRequest(t, s, http.MethodGet, "/api/v1/gguf/backends", ""); code == http.StatusUnauthorized {
		t.Errorf("GET /api/v1/gguf/backends without token = 401, want public access")
	}
}

// TestR65d_CppWorkerTokenNotLeakedFromClient — заголовок клиента не должен
// уходить в cppworker как его токен; используется серверный токен бэкенда.
func TestR65d_CppWorkerTokenNotLeakedFromClient(t *testing.T) {
	srv := authEnabledServer(t, types.Backend{
		ID:                "llama-tok",
		Name:              "llama-tok",
		Host:              "127.0.0.1",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		CppWorkerApiToken: "server-side-cppworker-token",
	})

	if got := srv.cppWorkerAPIToken("llama-tok"); got != "server-side-cppworker-token" {
		t.Errorf("cppWorkerAPIToken = %q, want server-side-cppworker-token", got)
	}
	// Неизвестный бэкенд → пустой токен (заголовок будет удалён, а не проброшен).
	if got := srv.cppWorkerAPIToken("does-not-exist"); got != "" {
		t.Errorf("cppWorkerAPIToken(unknown) = %q, want empty", got)
	}
}
