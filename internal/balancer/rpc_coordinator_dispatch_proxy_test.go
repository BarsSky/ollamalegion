package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"ollama-loadbalancer/pkg/types"
)

// TestProxyServeHTTP_RpcCoordinatorIntercepts — регрессионный тест для
// Phase 8 (P.1) rpc_coordinator mode scaffold в Proxy.ServeHTTP.
//
// Условия для перехвата request (interceptor block in proxy.go:453-460):
//   1. IsRpcCoordinatorMode(cfg.Balancing.OperatingMode) == true
//   2. p.rpcDispatcher != nil
//   3. p.rpcDispatcher.IsRpcPath(r.URL.Path) == true
//
// Если все 3 условия true → request маршрутизируется через
// RpcCoordinatorDispatcher.ServeHTTP (501 streaming stub / 503 not initialized).
//
// Тест проверяет все 4 комбинации:
//   - standard mode + no dispatcher → NOT intercepted (falls through)
//   - rpc_coordinator mode + no dispatcher → NOT intercepted (scaffold safe)
//   - rpc_coordinator mode + dispatcher + non-rpc path → NOT intercepted
//   - rpc_coordinator mode + dispatcher + rpc path → INTERCEPTED (503 not init)
//
// R52.6 (2026-08-24): test refactored чтобы избежать 1s connection timeout
// на каждом subtest. createTestConfig() backends marked Status: Healthy
// но без реального сервера на localhost:11434 → proxyRequest connection
// timeout = 1s. Решение: empty backends в config — selectBackend returns "",
// proxyRequest не вызывается, тест проходит < 1ms.
func TestProxyServeHTTP_RpcCoordinatorIntercepts(t *testing.T) {
	t.Parallel()

	// R52.6: empty backends config + QueueTimeout=0 — no backends, no
	// proxyRequest, no 10s queue timeout. Subtests run < 1ms each.
	emptyConfig := func() *types.LoadBalancerConfig {
		c := createTestConfig()
		c.Backends = nil
		c.Balancing.QueueTimeout = 0 // disable queue — fail fast without 10s wait
		return c
	}

	// Case 1: standard mode (default) — no interception.
	// Даже если dispatcher случайно set, path не должен перехватываться
	// при mode != rpc_coordinator.
	t.Run("standard_mode_does_not_intercept", func(t *testing.T) {
		config := emptyConfig()
		// OperatingMode не задан → "standard" (legacy) → IsRpcCoordinatorMode == false
		config.Balancing.OperatingMode = string(types.OperatingModeStandard)
		proxy := newProxyWithCleanup(t, config)
		proxy.SetQueueManagerProxy()
		defer proxy.queueMgr.Stop()

		// Force dispatcher set (этот случай не должен происходить в prod, но
		// мы тестируем defense-in-depth: scaffold должен респектить mode).
		proxy.rpcDispatcher = &RpcCoordinatorDispatcher{
			coordinator: nil, // 503 stub path
			proxy:       proxy,
		}

		req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model":"x","prompt":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)

		// Standard flow + no backends → request fails early (503 "no healthy backends"
		// или "all backends failed") НЕ через dispatcher.
		body := w.Body.String()
		assert.NotContains(t, body, "rpc_coordinator not initialized",
			"standard mode should NOT route to dispatcher even if set")
	})

	// Case 2: rpc_coordinator mode но dispatcher nil → no interception.
	// Это критичный сценарий default bundled config (RpcCoordinator.Enabled=false
	// → coordinator nil → dispatcher не инициализирован через main.go).
	// Scaffold должен тихо fall through, не падать.
	t.Run("rpc_coordinator_mode_dispatcher_nil_does_not_intercept", func(t *testing.T) {
		config := emptyConfig()
		config.Balancing.OperatingMode = string(types.OperatingModeRpcCoordinator)
		proxy := newProxyWithCleanup(t, config)
		proxy.SetQueueManagerProxy()
		defer proxy.queueMgr.Stop()

		// rpcDispatcher остаётся nil (default)
		assert.Nil(t, proxy.rpcDispatcher, "precondition: dispatcher should be nil")

		req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model":"x","prompt":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)

		body := w.Body.String()
		assert.NotContains(t, body, "rpc_coordinator not initialized",
			"nil dispatcher must not produce coordinator_disabled error")
	})

	// Case 3: rpc_coordinator mode + dispatcher set, но path не RPC.
	// Например /api/tags или /health → НЕ должен перехватываться.
	t.Run("rpc_coordinator_mode_non_rpc_path_does_not_intercept", func(t *testing.T) {
		config := emptyConfig()
		config.Balancing.OperatingMode = string(types.OperatingModeRpcCoordinator)
		proxy := newProxyWithCleanup(t, config)
		proxy.SetQueueManagerProxy()
		defer proxy.queueMgr.Stop()

		proxy.rpcDispatcher = &RpcCoordinatorDispatcher{
			coordinator: nil,
			proxy:       proxy,
		}

		// /api/tags — НЕ входит в IsRpcPath list
		req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)

		body := w.Body.String()
		assert.NotContains(t, body, "rpc_coordinator not initialized",
			"non-RPC path should not be intercepted by dispatcher")
	})

	// Case 4: rpc_coordinator mode + dispatcher set + rpc path → INTERCEPTED.
	// Ожидаем 503 coordinator_disabled (dispatcher.coordinator == nil).
	t.Run("rpc_coordinator_mode_rpc_path_intercepts", func(t *testing.T) {
		config := emptyConfig()
		config.Balancing.OperatingMode = string(types.OperatingModeRpcCoordinator)
		proxy := newProxyWithCleanup(t, config)
		proxy.SetQueueManagerProxy()
		defer proxy.queueMgr.Stop()

		// Dispatcher с nil coordinator → ServeHTTP выдаёт 503 + "rpc_coordinator not initialized"
		proxy.rpcDispatcher = &RpcCoordinatorDispatcher{
			coordinator: nil,
			proxy:       proxy,
		}

		req := httptest.NewRequest(http.MethodPost, "/api/generate",
			strings.NewReader(`{"model":"test-model","prompt":"hello","stream":false}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code,
			"interceptor should return 503 when coordinator is nil")
		assert.Contains(t, w.Body.String(), "rpc_coordinator not initialized",
			"body should contain coordinator_disabled error message (Ollama format: {\"error\":<msg>})")
	})
}

// TestProxySetGetRpcCoordinatorDispatcher — accessor round-trip.
func TestProxySetGetRpcCoordinatorDispatcher(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// nil по умолчанию
	assert.Nil(t, proxy.GetRpcCoordinatorDispatcher())

	// Установка + чтение
	dispatcher := &RpcCoordinatorDispatcher{
		coordinator: nil,
		proxy:       proxy,
	}
	proxy.SetRpcCoordinatorDispatcher(dispatcher)
	got := proxy.GetRpcCoordinatorDispatcher()
	assert.NotNil(t, got)
	assert.Same(t, dispatcher, got, "should return the same instance")
}
