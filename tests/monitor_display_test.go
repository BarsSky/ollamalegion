package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/api"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// TestMonitorEndpointExists проверяет что endpoint /monitor доступен
func TestMonitorEndpointExists(t *testing.T) {
	// Создаём минимальный конфиг
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Port:    8080,
			APIPort: 8081,
			Host:    "localhost",
		},
		Balancing: types.BalancingSettings{
			Algorithm:           "roundrobin",
			HealthCheckInterval: 30,
			RequestTimeout:      300,
		},
		API: types.APISettings{
			RateLimit: 100,
			RateBurst: 10,
		},
		Auth: types.AuthConfig{
			Enabled: false,
		},
		Logging: types.LoggingSettings{
			Level: "info",
		},
	}

	proxy := balancer.NewProxy(cfg)
	healthChecker := balancer.NewHealthChecker(proxy, 30, 3)
	apiServer := api.NewServer(proxy, cfg, healthChecker)

	req := httptest.NewRequest(http.MethodGet, "/monitor", nil)
	w := httptest.NewRecorder()

	apiServer.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Errorf("Expected Content-Type text/html, got %s", contentType)
	}

	body := w.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") && !strings.Contains(body, "<html") {
		t.Error("Response does not contain HTML content")
	}

	// Проверяем наличие ключевых элементов монитора
	requiredElements := []string{
		"api/v1/cluster",
		"api/v1/sessions",
		"api/v1/queue",
	}

	for _, elem := range requiredElements {
		if !strings.Contains(body, elem) {
			t.Errorf("Monitor HTML missing required API endpoint: %s", elem)
		}
	}

	// Проверяем что есть polling через fetch или WebSocket
	hasPolling := strings.Contains(body, "setInterval") || strings.Contains(body, "fetchAll") || strings.Contains(body, "WebSocket") || strings.Contains(body, "ws")
	if !hasPolling {
		t.Error("Monitor HTML does not contain data polling mechanism")
	}
}

// TestAPIEndpointsForMonitor проверяет что все API endpoints для монитора работают
func TestAPIEndpointsForMonitor(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Port:    8080,
			APIPort: 8081,
			Host:    "localhost",
		},
		Balancing: types.BalancingSettings{
			Algorithm:           "roundrobin",
			HealthCheckInterval: 30,
			RequestTimeout:      300,
		},
		API: types.APISettings{
			RateLimit: 100,
			RateBurst: 10,
		},
		Auth: types.AuthConfig{
			Enabled: false,
		},
		Logging: types.LoggingSettings{
			Level: "info",
		},
	}

	proxy := balancer.NewProxy(cfg)
	healthChecker := balancer.NewHealthChecker(proxy, 30, 3)
	apiServer := api.NewServer(proxy, cfg, healthChecker)

	endpoints := []struct {
		path       string
		statusCode int
	}{
		{"/api/v1/health", http.StatusOK},
		{"/api/v1/cluster", http.StatusOK},
		{"/api/v1/backends", http.StatusOK},
		{"/api/v1/sessions", http.StatusOK},
		{"/api/v1/queue/stats", http.StatusOK},
		{"/api/v1/queue/details", http.StatusOK},
		{"/api/v1/queue/history", http.StatusOK},
		{"/api/v1/metrics", http.StatusOK},
	}

	for _, endpoint := range endpoints {
		t.Run(endpoint.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, endpoint.path, nil)
			w := httptest.NewRecorder()

			apiServer.ServeHTTP(w, req)

			if w.Code != endpoint.statusCode {
				t.Errorf("Endpoint %s: expected status %d, got %d", endpoint.path, endpoint.statusCode, w.Code)
			}

			contentType := w.Header().Get("Content-Type")
			if !strings.Contains(contentType, "application/json") {
				t.Errorf("Endpoint %s: expected Content-Type application/json, got %s", endpoint.path, contentType)
			}

			// Проверяем что тело - валидный JSON
			var result map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Errorf("Endpoint %s: invalid JSON response: %v", endpoint.path, err)
			}
		})
	}
}

// TestMonitorHTMLHasErrorHandling проверяет что monitor.html содержит обработку ошибок
func TestMonitorHTMLHasErrorHandling(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Port:    8080,
			APIPort: 8081,
			Host:    "localhost",
		},
		Balancing: types.BalancingSettings{
			Algorithm:           "roundrobin",
			HealthCheckInterval: 30,
			RequestTimeout:      300,
		},
		API: types.APISettings{
			RateLimit: 100,
			RateBurst: 10,
		},
		Auth: types.AuthConfig{
			Enabled: false,
		},
		Logging: types.LoggingSettings{
			Level: "info",
		},
	}

	proxy := balancer.NewProxy(cfg)
	healthChecker := balancer.NewHealthChecker(proxy, 30, 3)
	apiServer := api.NewServer(proxy, cfg, healthChecker)

	req := httptest.NewRequest(http.MethodGet, "/monitor", nil)
	w := httptest.NewRecorder()

	apiServer.ServeHTTP(w, req)

	body := w.Body.String()

	// Проверяем наличие обработки ошибок
	errorHandlingElements := []string{
		"catch",           // try/catch blocks
		"error",           // error handling
		"fallback",        // fallback UI
		"offline",         // offline mode
		"retry",           // retry logic
	}

	for _, elem := range errorHandlingElements {
		if !strings.Contains(strings.ToLower(body), elem) {
			t.Errorf("Monitor HTML missing error handling element: %s", elem)
		}
	}

	// Проверяем наличие fallback UI (чтобы не было пустого экрана)
	fallbackElements := []string{
		"demoData",        // демо-данные при ошибках
		"showError",       // функция показа ошибки
		"updateUI",        // обновление UI
		"updateTopology",  // обновление топологии
	}

	for _, elem := range fallbackElements {
		if !strings.Contains(body, elem) {
			t.Errorf("Monitor HTML missing fallback UI element: %s", elem)
		}
	}
}

// TestMonitorHandler_WEBUIConfig проверяет, что /monitor встраивает
// WEBUI_CONFIG, совместимый с webui/js/modules/config.js и monitor/state.js.
func TestMonitorHandler_WEBUIConfig(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Port:    8080,
			APIPort: 8081,
			Host:    "localhost",
		},
		Balancing: types.BalancingSettings{
			Algorithm:           "roundrobin",
			HealthCheckInterval: 30,
			RequestTimeout:      300,
		},
		API: types.APISettings{
			RateLimit: 100,
			RateBurst: 10,
		},
		Auth: types.AuthConfig{
			Enabled: false,
		},
		Logging: types.LoggingSettings{
			Level: "info",
		},
	}

	proxy := balancer.NewProxy(cfg)
	healthChecker := balancer.NewHealthChecker(proxy, 30, 3)
	apiServer := api.NewServer(proxy, cfg, healthChecker)

	req := httptest.NewRequest(http.MethodGet, "/monitor", nil)
	req.Header.Set("X-Forwarded-Prefix", "/legion")
	w := httptest.NewRecorder()

	apiServer.ServeHTTP(w, req)

	body := w.Body.String()

	// WEBUI_CONFIG должен содержать поля, используемые webui/js/modules/config.js
	// и monitor.html/state.js.
	requiredConfigFields := []string{
		`apiBase:"/legion"`,      // monitor.html/state.js
		`API_BASE:"/legion"`,     // webui/js/modules/api.js, gguf-api.js
		`API_BASE_URL:"/legion"`, // webui/js/modules/gguf-api.js
		`CPPWORKER_URL:`,         // gguf-api.js
		`REFRESH_INTERVAL:`,      // app.js
	}
	for _, field := range requiredConfigFields {
		if !strings.Contains(body, field) {
			t.Errorf("Monitor HTML missing WEBUI_CONFIG field: %s", field)
		}
	}
}
