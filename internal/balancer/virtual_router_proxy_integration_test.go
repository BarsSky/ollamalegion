package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// Phase 8 P.2: Proxy.ServeHTTP + VirtualRouter integration tests.
// Verifies the interceptor block in proxy.go:467-486:
//   - mode=virtual_router + router set + path match + body has virtual model
//     → request goes through VirtualRouter.ServeHTTP.
//   - mode=standard → falls through to existing flow (no interception).
//   - mode=virtual_router but model NOT in registry → falls through.
//   - mode=virtual_router but path is NOT intercepted → falls through.
// =====================================================================

func TestProxyServeHTTP_VirtualRouter_Intercepts(t *testing.T) {
	t.Parallel()

	// Fake backend.
	calls := atomic.Int64{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := readAll(r.Body)
		var env struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model":  env.Model,
			"answer": "echo from backend",
		})
	}))
	defer backend.Close()
	addr := backend.URL[7:]
	host, port, _ := parseBackendHostPort(addr)

	// Set up Proxy with virtual_router mode + VirtualRouter.
	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	proxy.config.Balancing.OperatingMode = string(types.OperatingModeVirtualRouter)

	registry := virtualmodel.NewRegistry()
	registry.SetEnabled(true)
	require.NoError(t, registry.Register(types.VirtualModelConfig{
		Name:        "vm-e2e",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", host, port)},
		ModelName:   "physical-model",
	}))
	router := NewVirtualRouter(registry, proxy)
	proxy.SetVirtualRouter(router)

	// Make request with virtual model.
	body := bytes.NewReader([]byte(`{"model":"vm-e2e","prompt":"hello","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "request should pass through VirtualRouter to backend: %s", w.Body.String())
	assert.Equal(t, int64(1), calls.Load(), "backend should receive 1 request")
	assert.Equal(t, "vm-e2e", w.Header().Get("X-Virtual-Model"), "X-Virtual-Model should be set to original virtual name")
}

func TestProxyServeHTTP_VirtualRouter_StandardMode_DoesNotIntercept(t *testing.T) {
	t.Parallel()

	// Backend should NOT receive any request in this test.
	calls := atomic.Int64{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	addr := backend.URL[7:]
	host, port, _ := parseBackendHostPort(addr)

	// Standard mode (default) — interceptor should NOT fire.
	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	// OperatingMode остаётся пустым (default = standard).

	// Wire VirtualRouter, но mode != virtual_router → не сработает.
	registry := virtualmodel.NewRegistry()
	registry.SetEnabled(true)
	_ = registry.Register(types.VirtualModelConfig{
		Name:        "vm-x",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", host, port)},
		ModelName:   "physical",
	})
	proxy.SetVirtualRouter(NewVirtualRouter(registry, proxy))

	// Request with virtual model — должен пройти как обычный, НЕ через VirtualRouter.
	body := bytes.NewReader([]byte(`{"model":"vm-x","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	// Standard flow: нет backend'ов с моделью "vm-x" в стандартном pool,
	// значит вернётся 503/404/502 — но НЕ через наш backend.
	// Главное — наш fake backend не получил запрос.
	assert.Equal(t, int64(0), calls.Load(), "backend should NOT be called in standard mode")
}

func TestProxyServeHTTP_VirtualRouter_NonVirtualModel_FallsThrough(t *testing.T) {
	t.Parallel()

	// Fake backend с стандартным behavior.
	calls := atomic.Int64{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var env struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"model": env.Model, "echo": true})
	}))
	defer backend.Close()
	addr := backend.URL[7:]
	host, port, _ := parseBackendHostPort(addr)

	// virtual_router mode + VirtualRouter set, но model не в registry.
	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	proxy.config.Balancing.OperatingMode = string(types.OperatingModeVirtualRouter)

	registry := virtualmodel.NewRegistry()
	registry.SetEnabled(true)
	_ = registry.Register(types.VirtualModelConfig{
		Name:        "vm-registered",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", host, port)},
		ModelName:   "physical",
	})
	proxy.SetVirtualRouter(NewVirtualRouter(registry, proxy))

	// Add a "regular" backend in proxy.backends for the standard flow.
	proxy.backends["test-backend"] = &BackendState{
		Backend: &types.Backend{
			ID:   "test-backend",
			Name: "test",
			Host: host,
			OllamaPort: port,
		},
		ActiveReqs: 0,
	}

	// Request with NON-virtual model — должен fall through к стандартному flow.
	body := bytes.NewReader([]byte(`{"model":"some-regular-model","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	// Стандартный flow попытается направить на backend, но модель "some-regular-model"
	// не loaded → ожидаем error, но НЕ от VirtualRouter.
	// Проверяем что response не содержит virtual_router-специфичных errors.
	body2 := w.Body.String()
	assert.NotContains(t, body2, "unknown_virtual_model",
		"non-virtual model should not trigger virtual_router errors")
	assert.NotContains(t, body2, "no_healthy_backends",
		"non-virtual model should not trigger virtual_router errors")
}

func TestProxyServeHTTP_VirtualRouter_NonRpcPath_DoesNotIntercept(t *testing.T) {
	t.Parallel()

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	proxy.config.Balancing.OperatingMode = string(types.OperatingModeVirtualRouter)

	registry := virtualmodel.NewRegistry()
	registry.SetEnabled(true)
	_ = registry.Register(types.VirtualModelConfig{
		Name:        "vm",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{"127.0.0.1:1"},
		ModelName:   "physical",
	})
	proxy.SetVirtualRouter(NewVirtualRouter(registry, proxy))

	// GET /api/tags — НЕ в IsVirtualPathRequest list.
	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	// GET /api/tags — стандартный flow. Не должно быть virtual_router-специфичных errors.
	assert.NotContains(t, w.Body.String(), "no_healthy_backends")
}

// Helper for reading body (avoid importing io just for one call).
func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf.Bytes(), nil
			}
			return buf.Bytes(), err
		}
	}
}
