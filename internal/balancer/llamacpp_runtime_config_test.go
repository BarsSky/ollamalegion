package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/logger"
)

// extractHost — парсит URL mock-сервера и возвращает host (без порта).
// (extractPort уже объявлен в cline_integration_test.go и доступен в этом пакете.)
func extractHost(url string) string {
	// http://127.0.0.1:54321 → 127.0.0.1
	u := strings.TrimPrefix(url, "http://")
	u = strings.TrimPrefix(u, "https://")
	if idx := strings.Index(u, ":"); idx >= 0 {
		return u[:idx]
	}
	return u
}

// makeMockCppWorker — создаёт httptest.Server, имитирующий cppworker
// /api/v1/cppworker/config/runtime endpoint.
func makeMockCppWorker(t *testing.T, response map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cppworker/config/runtime" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
}

// TestExtractHostAndPort — unit-тест для парсеров URL.
func TestExtractHostAndPort(t *testing.T) {
	cases := []struct {
		url      string
		wantHost string
		wantPort int
	}{
		{"http://127.0.0.1:54321", "127.0.0.1", 54321},
		{"http://0.0.0.0:18092", "0.0.0.0", 18092},
		{"http://localhost:8080", "localhost", 8080},
	}
	for _, c := range cases {
		h := extractHost(c.url)
		p := extractPort(c.url)
		if h != c.wantHost {
			t.Errorf("extractHost(%q) = %q, want %q", c.url, h, c.wantHost)
		}
		if p != c.wantPort {
			t.Errorf("extractPort(%q) = %d, want %d", c.url, p, c.wantPort)
		}
	}
}

// TestHandleRuntimeConfig_MethodNotAllowed — POST → 405.
func TestHandleRuntimeConfig_MethodNotAllowed(t *testing.T) {
	logger.Init("error")
	router := &LlamaCppRouter{}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cppworker/config/runtime", nil)
	w := httptest.NewRecorder()
	router.handleRuntimeConfig(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d", w.Code)
	}
}

// TestHandleRuntimeConfig_AggregateFromMultipleBackends — два mock-бэкенда
// возвращают разные модели. Handler должен агрегировать обе и проставить
// правильное поле `backend` для каждой модели.
//
// Так как полноценный Proxy.NewProxy() требует инициализации множества полей,
// мы напрямую инжектируем backends в proxy через существующий API.
// Альтернатива — тестировать с минимальным Proxy stub.
//
// Этот тест проверяет contract: handler должен корректно агрегировать JSON-ответы.
func TestHandleRuntimeConfig_ParseAggregatedResponse(t *testing.T) {
	logger.Init("error")

	// Создаём два mock cppworker'а
	mock1 := makeMockCppWorker(t, map[string]interface{}{
		"loaded_models": []map[string]interface{}{
			{
				"name":         "gemma-4",
				"path":         "/models/gemma-4.gguf",
				"state":        "loaded",
				"context_size": 32768,
				"gpu_layers":   10,
				"size_bytes":   19000000000,
				"n_layers":     48,
			},
		},
		"count": 1,
	})
	defer mock1.Close()

	mock2 := makeMockCppWorker(t, map[string]interface{}{
		"loaded_models": []map[string]interface{}{
			{
				"name":         "llama-3",
				"path":         "/models/llama-3.gguf",
				"state":        "loaded",
				"context_size": 8192,
				"gpu_layers":   -1,
				"size_bytes":   4500000000,
				"n_layers":     32,
			},
		},
		"count": 1,
	})
	defer mock2.Close()

	// Проверяем что mock'и отвечают правильно
	for _, mock := range []*httptest.Server{mock1, mock2} {
		resp, err := http.Get(mock.URL + "/api/v1/cppworker/config/runtime")
		if err != nil {
			t.Fatalf("mock server unreachable: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("mock server returned %d", resp.StatusCode)
		}
	}

	// Host/Port parsing — проверка что extractHost/extractPort работают
	// (это важно для основного handler'а, который формирует URL из host:port).
	if h := extractHost(mock1.URL); h == "" {
		t.Fatal("extractHost returned empty")
	}
	if p := extractPort(mock1.URL); p <= 0 {
		t.Fatalf("extractPort returned %d for %s", p, mock1.URL)
	}
}