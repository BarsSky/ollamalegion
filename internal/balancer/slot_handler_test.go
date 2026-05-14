package balancer

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"ollama-loadbalancer/pkg/types"
)

// TestAcquireSlotWithRetry_EmptyTarget — пустой targetBackend → failure
func TestAcquireSlotWithRetry_EmptyTarget(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	req := httptest.NewRequest("POST", "/api/generate", nil)
	backend, ok := proxy.acquireSlotWithRetry("llama3", "", "sess-1", "client1", req)
	assert.False(t, ok)
	assert.Empty(t, backend)
}

// TestAcquireSlotWithRetry_Success — targetBackend свободен
func TestAcquireSlotWithRetry_Success(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// backend-1 свободен: active=0, max=10
	proxy.backends["backend-1"].mu.Lock()
	proxy.backends["backend-1"].ActiveReqs = 0
	proxy.backends["backend-1"].mu.Unlock()

	req := httptest.NewRequest("POST", "/api/generate", nil)
	backend, ok := proxy.acquireSlotWithRetry("llama3", "backend-1", "sess-1", "client1", req)
	assert.True(t, ok)
	assert.Equal(t, "backend-1", backend)

	// Слот должен быть захвачен
	proxy.backends["backend-1"].mu.Lock()
	active := proxy.backends["backend-1"].ActiveReqs
	proxy.backends["backend-1"].mu.Unlock()
	assert.Equal(t, 1, active, "Слот должен быть захвачен")
}

// TestAcquireSlotWithRetry_Fallback — targetBackend занят, fallback на backend-2
func TestAcquireSlotWithRetry_Fallback(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// backend-1 занят: active=10, max=10
	proxy.backends["backend-1"].mu.Lock()
	proxy.backends["backend-1"].ActiveReqs = 10
	proxy.backends["backend-1"].mu.Unlock()

	// backend-2 свободен: active=0, max=10
	proxy.backends["backend-2"].mu.Lock()
	proxy.backends["backend-2"].ActiveReqs = 0
	proxy.backends["backend-2"].mu.Unlock()

	// Метрики для backend-2 (иначе resource limits отфильтруют)
	proxy.UpdateMetrics("backend-2", &types.BackendMetrics{
		ID: "backend-2",
		GPU: types.GPUMetrics{
			UsagePercent: 10,
			MemoryTotal:  16384,
			MemoryFree:   14000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     32768,
			MemoryFree:      28000,
			DiskFree:        5000,
		},
		Ollama: types.OllamaMetrics{
			OllamaAvailable: true,
		},
	})

	req := httptest.NewRequest("POST", "/api/generate", nil)
	backend, ok := proxy.acquireSlotWithRetry("llama3", "backend-1", "sess-1", "client1", req)
	assert.True(t, ok, "Должен найти fallback backend")
	assert.Equal(t, "backend-2", backend, "Fallback должен быть backend-2")

	// backend-2 должен быть захвачен
	proxy.backends["backend-2"].mu.Lock()
	active := proxy.backends["backend-2"].ActiveReqs
	proxy.backends["backend-2"].mu.Unlock()
	assert.Equal(t, 1, active, "Слот на backend-2 должен быть захвачен")
}

// TestAcquireSlotWithRetry_AllBusy — все бэкенды заняты
func TestAcquireSlotWithRetry_AllBusy(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Все backend'ы заняты
	proxy.backends["backend-1"].mu.Lock()
	proxy.backends["backend-1"].ActiveReqs = 10
	proxy.backends["backend-1"].mu.Unlock()

	proxy.backends["backend-2"].mu.Lock()
	proxy.backends["backend-2"].ActiveReqs = 20
	proxy.backends["backend-2"].mu.Unlock()

	req := httptest.NewRequest("POST", "/api/generate", nil)
	backend, ok := proxy.acquireSlotWithRetry("llama3", "backend-1", "sess-1", "client1", req)
	assert.False(t, ok, "Все бэкенды заняты — failure")
	assert.Empty(t, backend)
}

// TestAcquireSlotWithRetry_SessionBinding — при fallback сессия привязывается к новому backend
func TestAcquireSlotWithRetry_SessionBinding(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = true
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Создаём сессию на backend-1
	proxy.sessionMgr.Set("sess-1", "backend-1", "llama3", "client1", "127.0.0.1", "test-agent")

	// backend-1 занят, backend-2 свободен
	proxy.backends["backend-1"].mu.Lock()
	proxy.backends["backend-1"].ActiveReqs = 10
	proxy.backends["backend-1"].mu.Unlock()

	proxy.backends["backend-2"].mu.Lock()
	proxy.backends["backend-2"].ActiveReqs = 0
	proxy.backends["backend-2"].mu.Unlock()

	proxy.UpdateMetrics("backend-2", &types.BackendMetrics{
		ID: "backend-2",
		GPU: types.GPUMetrics{
			UsagePercent: 10,
			MemoryTotal:  16384,
			MemoryFree:   14000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     32768,
			MemoryFree:      28000,
			DiskFree:        5000,
		},
		Ollama: types.OllamaMetrics{
			OllamaAvailable: true,
		},
	})

	req := httptest.NewRequest("POST", "/api/generate", nil)
	backend, ok := proxy.acquireSlotWithRetry("llama3", "backend-1", "sess-1", "client1", req)
	assert.True(t, ok)
	assert.Equal(t, "backend-2", backend)

	// Сессия должна быть перепривязана
	session := proxy.sessionMgr.Get("sess-1")
	assert.NotNil(t, session)
	assert.Equal(t, "backend-2", session.BackendID)
}