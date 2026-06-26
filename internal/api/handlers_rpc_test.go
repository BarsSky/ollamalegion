package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// createTestServerWithRPC — сервер с опционально включённым RPC Coordinator.
//
// Использует уже существующий NewServer(proxy, config, healthChecker).
// Возвращает *Server для прямого вызова .mux через ServeHTTP.
func createTestServerWithRPC(t *testing.T, enableRPC bool) (*Server, *balancer.Proxy) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:                "backend1",
				Name:              "Test Backend 1",
				Host:               "localhost",
				OllamaPort:         11434,
				Weight:             1,
				MaxConcurrentReqs: 10,
				Status:             types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			ModelAffinity:       true,
			SessionStickiness:   true,
			HealthCheckInterval: 10,
			MetricsInterval:     5,
			RequestTimeout:      30,
		},
		API: types.APISettings{
			RateLimit: 100,
			RateBurst: 200,
		},
		Auth: types.AuthConfig{
			Enabled:    false, // Auth отключён для тестов.
			Tokens:     []string{"test-token"},
			HeaderName: "X-API-Token",
		},
	}

	if enableRPC {
		config.Balancing.RpcCoordinator = types.RpcCoordinatorConfig{
			Enabled:    true,
			Protocol:   "http",
			Timeout:    "5s",
			MaxRetries: 1,
		}
	}

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	srv := NewServer(proxy, config, healthChecker)
	return srv, proxy
}

// rpcDo — вспомогательный helper: делает HTTP-запрос к server.mux и возвращает response.
func rpcDo(t *testing.T, srv *Server, method, path, body string) *http.Response {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	return w.Result()
}

// skipIfNoRPC пропускает тест, если RPC coordinator не инициализирован.
func skipIfNoRPC(t *testing.T, srv *Server) {
	t.Helper()
	if srv.proxy.GetRpcCoordinator() == nil {
		t.Skip("RPC coordinator not initialized — требуется config.Balancing.RpcCoordinator.Enabled=true")
	}
}

// =====================================================================
// RPC disabled — все endpoints должны возвращать 503 rpc_disabled.
// =====================================================================

func TestRpc_AllEndpoints_Disabled(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, false)

	cases := []struct {
		method, path, body string
	}{
		{"GET", "/api/v1/rpc/workers", ""},
		{"POST", "/api/v1/rpc/workers/register", `{"worker_id":"w1","host":"x","port":18080}`},
		{"GET", "/api/v1/rpc/workers/w1", ""},
		{"DELETE", "/api/v1/rpc/workers/w1", ""},
		{"GET", "/api/v1/rpc/models", ""},
		{"POST", "/api/v1/rpc/models/m/infer", `{"prompt":"hi"}`},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			resp := rpcDo(t, srv, tc.method, tc.path, tc.body)
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("expected 503, got %d", resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

// =====================================================================
// RPC enabled — edge cases.
// =====================================================================

func TestRpcWorkers_GET_Empty(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	resp := rpcDo(t, srv, "GET", "/api/v1/rpc/workers", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	count, _ := body["count"].(float64)
	if count != 0 {
		t.Errorf("expected count=0, got %v", body["count"])
	}
}

func TestRpcWorkersRegister_POST_BadJSON(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	resp := rpcDo(t, srv, "POST", "/api/v1/rpc/workers/register", "not json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRpcWorkersRegister_POST_MissingHost(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	// Только worker_id — host отсутствует.
	body := `{"worker_id":"w1","port":18080}`
	resp := rpcDo(t, srv, "POST", "/api/v1/rpc/workers/register", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 missing_field, got %d", resp.StatusCode)
	}
}

func TestRpcWorkersRegister_POST_InvalidPort(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	body := `{"worker_id":"w1","host":"x","port":99999}`
	resp := rpcDo(t, srv, "POST", "/api/v1/rpc/workers/register", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 invalid_port, got %d", resp.StatusCode)
	}
}

// =====================================================================
// /api/v1/rpc/workers/{id} — GET/DELETE
// =====================================================================

func TestRpcWorkerItem_GET_NotFound(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	resp := rpcDo(t, srv, "GET", "/api/v1/rpc/workers/never-existed", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRpcWorkerItem_DELETE_Idempotent(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	resp := rpcDo(t, srv, "DELETE", "/api/v1/rpc/workers/never-existed", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 (idempotent), got %d", resp.StatusCode)
	}
}

func TestRpcWorkerItem_PUT_MethodNotAllowed(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	resp := rpcDo(t, srv, "PUT", "/api/v1/rpc/workers/w1", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

// =====================================================================
// /api/v1/rpc/models — GET (без register)
// =====================================================================

func TestRpcModels_GET_Empty(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	resp := rpcDo(t, srv, "GET", "/api/v1/rpc/models", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if c, _ := body["count"].(float64); c != 0 {
		t.Errorf("expected count=0, got %v", body["count"])
	}
}

func TestRpcModelsInfer_GET_MethodNotAllowed(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	resp := rpcDo(t, srv, "GET", "/api/v1/rpc/models/my-model/infer", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

func TestRpcModelsInfer_POST_MissingPrompt(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	resp := rpcDo(t, srv, "POST", "/api/v1/rpc/models/any-model/infer", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 missing_field, got %d", resp.StatusCode)
	}
}

func TestRpcModelsInfer_POST_ModelNotFound(t *testing.T) {
	srv, _ := createTestServerWithRPC(t, true)
	skipIfNoRPC(t, srv)

	resp := rpcDo(t, srv, "POST", "/api/v1/rpc/models/unknown-model/infer", `{"prompt":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 model_not_found, got %d", resp.StatusCode)
	}
}