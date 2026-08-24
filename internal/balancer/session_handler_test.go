package balancer

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"ollama-loadbalancer/pkg/types"
)

// TestResolveSessionBackend_NoSession — без сессии возвращает пустую строку
func TestResolveSessionBackend_NoSession(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = true
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	backend := proxy.resolveSessionBackend(req, "llama3", "", "client")
	assert.Empty(t, backend, "Без sessionID должен вернуть пустую строку")
}

// TestResolveSessionBackend_NewSession — сессия не существует
func TestResolveSessionBackend_NewSession(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = true
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	backend := proxy.resolveSessionBackend(req, "llama3", "new-session-id", "client")
	assert.Empty(t, backend, "Несуществующая сессия — пустая строка")
}

// TestResolveSessionBackend_HealthySession — сессия привязана к healthy backend
func TestResolveSessionBackend_HealthySession(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = true
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Создаём сессию
	proxy.sessionMgr.Set("sess-1", "backend-1", "llama3", "client1", "127.0.0.1", "test-agent")

	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	backend := proxy.resolveSessionBackend(req, "llama3", "sess-1", "client1")

	assert.Equal(t, "backend-1", backend, "Должен вернуть backend из сессии")
}

// TestResolveSessionBackend_UnhealthySession — backend unhealthy, сброс сессии
func TestResolveSessionBackend_UnhealthySession(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = true
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Создаём сессию, но backend становится unhealthy
	proxy.sessionMgr.Set("sess-1", "backend-1", "llama3", "client1", "127.0.0.1", "test-agent")
	proxy.UpdateBackendStatus("backend-1", types.StatusUnhealthy)

	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	backend := proxy.resolveSessionBackend(req, "llama3", "sess-1", "client1")

	assert.Empty(t, backend, "Unhealthy backend — сброс сессии")
}

// TestRebalanceIfNeeded_LowLoad — loadRatio < 0.5, rebalance не нужен
func TestRebalanceIfNeeded_LowLoad(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = true
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	proxy.sessionMgr.Set("sess-1", "backend-1", "llama3", "client1", "127.0.0.1", "test-agent")

	// backend-1: active=2, max=10 → loadRatio=0.2
	proxy.backends["backend-1"].mu.Lock()
	proxy.backends["backend-1"].ActiveReqs = 2
	proxy.backends["backend-1"].mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	backend := proxy.resolveSessionBackend(req, "llama3", "sess-1", "client1")

	assert.Equal(t, "backend-1", backend, "Низкая нагрузка — остаёмся на том же backend")
}

// TestRebalanceIfNeeded_HighLoad — loadRatio > 0.5, rebalance на менее загруженный
func TestRebalanceIfNeeded_HighLoad(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = true
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Создаём сессию на backend-1
	proxy.sessionMgr.Set("sess-1", "backend-1", "llama3", "client1", "127.0.0.1", "test-agent")

	// backend-1: active=6, max=10 → loadRatio=0.6
	proxy.backends["backend-1"].mu.Lock()
	proxy.backends["backend-1"].ActiveReqs = 6
	proxy.backends["backend-1"].mu.Unlock()

	// backend-2: active=1, max=20 → loadRatio=0.05 (менее загруженный)
	proxy.backends["backend-2"].mu.Lock()
	proxy.backends["backend-2"].ActiveReqs = 1
	proxy.backends["backend-2"].mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	backend := proxy.resolveSessionBackend(req, "llama3", "sess-1", "client1")

	// Должен ребалансировать на backend-2
	assert.Equal(t, "backend-2", backend, "Высокая нагрузка — rebalance на менее загруженный")
}

// TestBindSession — привязка сессии к backend
func TestBindSession(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = true
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	proxy.bindSession("sess-2", "backend-2", "llama3", "client1", req)

	session := proxy.sessionMgr.Get("sess-2")
	assert.NotNil(t, session)
	assert.Equal(t, "backend-2", session.BackendID)
	assert.Equal(t, "llama3", session.Model)
	assert.Equal(t, "client1", session.ClientName)
}

// TestBindSession_Disabled — stickiness отключён, привязки нет
func TestBindSession_Disabled(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = false
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	proxy.bindSession("sess-3", "backend-1", "llama3", "client1", req)

	session := proxy.sessionMgr.Get("sess-3")
	assert.Nil(t, session, "При отключенном stickiness сессия не создаётся")
}