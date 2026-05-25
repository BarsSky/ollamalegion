package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"ollama-loadbalancer/pkg/types"
)

// createTestConfig - создание тестовой конфигурации
func createTestConfig() *types.LoadBalancerConfig {
	return &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:                  "backend-1",
				Name:                "Backend 1",
				Host:                "localhost",
				OllamaPort:          11434,
				AgentPort:           9090,
				Weight:              1,
				MaxConcurrentReqs:   10,
				Status:              types.StatusHealthy,
				ConsecutiveFailures: 0,
			},
			{
				ID:                  "backend-2",
				Name:                "Backend 2",
				Host:                "localhost",
				OllamaPort:          11435,
				AgentPort:           9091,
				Weight:              2,
				MaxConcurrentReqs:   20,
				Status:              types.StatusHealthy,
				ConsecutiveFailures: 0,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     false,
			SessionStickiness: false,
			RequestTimeout:    30,
			QueueTimeout:      10,
			QueueMaxSize:      100,
			QueueWorkers:      4,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{
				MaxUsagePercent: 90,
			},
			CPU: types.CPULimits{
				MaxUsagePercent: 90,
			},
			Memory: types.MemoryLimits{
				MaxUsagePercent: 90,
			},
			Disk: types.DiskLimits{
				MinFreeMB: 1000,
			},
		},
	}
}

// TestQueueManagerEnqueue - проверка постановки в очередь через канал
func TestQueueManagerEnqueue(t *testing.T) {
	t.Parallel()

	// Создаём QueueManager с 0 workers, чтобы элемент гарантированно остался в канале
	queueMgr := NewQueueManager(nil, 100, 0, 5*time.Second)
	defer queueMgr.Stop()

	// Проверяем начальное состояние
	assert.Equal(t, 0, len(queueMgr.queue))
	assert.Equal(t, 100, queueMgr.maxSize)
	assert.Equal(t, 0, queueMgr.numWorkers)

	// Создаем тестовый запрос
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model": "llama2"}`))
	w := httptest.NewRecorder()

	done := make(chan bool, 1)
	queuedReq := &QueuedRequest{
		Request:  req,
		Writer:   w,
		Model:    "llama2",
		Enqueued: time.Now(),
		Done:     done,
	}

	// Отправляем запрос в канал очереди
	select {
	case queueMgr.queue <- queuedReq:
		// Успешно
	default:
		t.Fatal("Не удалось отправить запрос в очередь")
	}

	// Проверяем что очередь увеличилась
	assert.Equal(t, 1, len(queueMgr.queue))
}

// TestQueueManagerFull - проверка переполненной очереди
func TestQueueManagerFull(t *testing.T) {
	t.Parallel()

	// Создаем QueueManager с буфером 1, 0 workers (без proxy чтобы не паниковали workers)
	queueMgr := NewQueueManager(nil, 1, 0, 5*time.Second)
	defer queueMgr.Stop()

	// Отправляем запрос в канал
	done1 := make(chan bool, 1)
	select {
	case queueMgr.queue <- &QueuedRequest{Model: "model1", Done: done1}:
		// Успешно
	default:
		t.Fatal("Не удалось отправить первый запрос")
	}

	// Пытаемся отправить второй — канал буферизованный на 1, должен заблокироваться
	done2 := make(chan bool, 1)
	select {
	case queueMgr.queue <- &QueuedRequest{Model: "model2", Done: done2}:
		t.Fatal("Очередь должна быть переполнена")
	default:
		// Ожидаемое поведение — канал переполнен
	}

	// Проверяем maxSize
	assert.Equal(t, 1, queueMgr.maxSize)
}

// TestQueueManagerProcess - проверка обработки очереди
func TestQueueManagerProcess(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Инициализируем метрики для бэкендов
	proxy.UpdateMetrics("backend-1", &types.BackendMetrics{
		ID: "backend-1",
		GPU: types.GPUMetrics{
			UsagePercent: 10,
			MemoryTotal:  16384,
			MemoryFree:   14000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     32768,
			MemoryFree:      28000,
		},
	})

	// Проверяем что selectBackend работает
	backend := proxy.selectBackend("", "")
	assert.NotEmpty(t, backend, "Должен быть выбран бэкенд")
}

// TestQueueManagerShutdown - проверка остановки QueueManager
func TestQueueManagerShutdown(t *testing.T) {
	t.Parallel()

	// 0 workers чтобы избежать panic с nil proxy
	queueMgr := NewQueueManager(nil, 10, 0, 5*time.Second)

	// Создаем запрос в канале
	done := make(chan bool, 1)
	select {
	case queueMgr.queue <- &QueuedRequest{
		Model: "test",
		Done:  done,
	}:
	default:
	}

	// Останавливаем — не должно паниковать
	assert.NotPanics(t, func() {
		queueMgr.Stop()
	})
}

// TestQueueManagerProcessedCount - проверка счетчика обработанных запросов
func TestQueueManagerProcessedCount(t *testing.T) {
	t.Parallel()

	// 0 workers чтобы избежать panic с nil proxy
	queueMgr := NewQueueManager(nil, 10, 0, 5*time.Second)
	defer queueMgr.Stop()

	// Начальное значение
	assert.Equal(t, int64(0), queueMgr.processed)
}

// TestNewQueueManager - проверка создания QueueManager
func TestNewQueueManager(t *testing.T) {
	t.Parallel()

	// 0 workers чтобы избежать panic с nil proxy
	queueMgr := NewQueueManager(nil, 50, 0, 10*time.Second)
	defer queueMgr.Stop()

	assert.Equal(t, 50, queueMgr.maxSize)
	assert.Equal(t, 10*time.Second, queueMgr.timeout)
	assert.Equal(t, 0, queueMgr.numWorkers)
	assert.NotNil(t, queueMgr.queue)
}

// TestProxyNew - проверка создания Proxy
func TestProxyNew(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	assert.NotNil(t, proxy)
	assert.NotNil(t, proxy.backends)
	assert.NotNil(t, proxy.sessionMgr)
	assert.NotNil(t, proxy.metricsMgr)
	assert.NotNil(t, proxy.queueMgr)
	assert.Equal(t, 2, len(proxy.backends))
}

// TestProxySelectBackend - проверка выбора бэкенда
func TestProxySelectBackend(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Инициализируем метрики
	proxy.UpdateMetrics("backend-1", &types.BackendMetrics{
		ID: "backend-1",
		GPU: types.GPUMetrics{
			UsagePercent: 10,
			MemoryTotal:  16384,
			MemoryFree:   14000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
		},
	})

	// Выбираем бэкенд
	backend := proxy.selectBackend("", "")
	assert.NotEmpty(t, backend)
}

// TestSelectBackend_WithReplicationGroup - проверка выбора бэкенда через replication selector
func TestSelectBackend_WithReplicationGroup(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	// Включаем model replication в конфиге
	config.Balancing.ModelReplication.Enabled = true
	config.Balancing.ModelReplication.Groups = []types.ModelGroupConfig{
		{
			ModelName:    "llama3",
			MinInstances: 1,
			MaxInstances: 2,
			TargetBackends: []string{"backend-1", "backend-2"},
		},
	}

	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Убеждаемся, что replicationSelector создан и callback'и настроены
	assert.NotNil(t, proxy.replicationSelector)
	assert.NotNil(t, proxy.modelReplication)

	// Создаём группу репликации вручную (initRpcModules уже создал из конфига)
	// Проверяем что группа создана
	group := proxy.modelReplication.GetGroup("llama3")
	assert.NotNil(t, group)
	assert.Equal(t, "llama3", group.ModelName)

	// Устанавливаем метрики: backend-1 загружен, backend-2 свободен
	proxy.UpdateMetrics("backend-1", &types.BackendMetrics{
		ID: "backend-1",
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{{Name: "llama3"}},
		},
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
	})

	proxy.UpdateMetrics("backend-2", &types.BackendMetrics{
		ID: "backend-2",
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{{Name: "llama3"}},
		},
		GPU: types.GPUMetrics{
			UsagePercent: 5,
			MemoryTotal:  16384,
			MemoryFree:   15000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 10,
			MemoryTotal:     32768,
			MemoryFree:      30000,
			DiskFree:        5000,
		},
	})

	// Проверяем что модель распознаётся как групповая
	assert.True(t, proxy.replicationSelector.IsGroupModel("llama3"))
	assert.False(t, proxy.replicationSelector.IsGroupModel("nonexistent"))

	// Добавляем инстансы вручную для проверки selectInstance
	proxy.modelReplication.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3-test",
		MinInstances: 1,
		MaxInstances: 2,
		TargetBackends: []string{"backend-1", "backend-2"},
	})
	// Удаляем старую группу и создаём новую с корректными instances
	// (в реальности instances добавляются через warmup callback)
	// Проверяем что GetGroupCandidates возвращает пустой список без instances
	candidates := proxy.replicationSelector.GetGroupCandidates("llama3")
	assert.Empty(t, candidates, "Без loaded instances кандидаты пустые")

	// Проверяем GetGroupStats
	stats := proxy.replicationSelector.GetGroupStats("llama3")
	assert.NotNil(t, stats)
	assert.Equal(t, "llama3", stats["modelName"])
	assert.Equal(t, 0, stats["loaded"])
}

// TestProxyGetClusterState - проверка получения состояния кластера
func TestProxyGetClusterState(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	state := proxy.GetClusterState()

	assert.NotNil(t, state)
	assert.Equal(t, 2, state.TotalBackends)
	assert.GreaterOrEqual(t, state.HealthyBackends, 0)
	assert.NotNil(t, state.Timestamp)
}

// TestProxyUpdateMetrics - проверка обновления метрик
func TestProxyUpdateMetrics(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	metrics := &types.BackendMetrics{
		ID: "backend-1",
		GPU: types.GPUMetrics{
			UsagePercent: 50,
		},
	}

	proxy.UpdateMetrics("backend-1", metrics)

	// Проверяем что метрики обновлены
	proxy.metricsMgr.mu.RLock()
	storedMetrics, exists := proxy.metricsMgr.metrics["backend-1"]
	proxy.metricsMgr.mu.RUnlock()

	assert.True(t, exists)
	assert.Equal(t, float64(50), storedMetrics.GPU.UsagePercent)
}

// TestProxyUpdateBackendStatus - проверка обновления статуса бэкенда
func TestProxyUpdateBackendStatus(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Обновляем статус
	proxy.UpdateBackendStatus("backend-1", types.StatusUnhealthy)

	// Проверяем статус
	proxy.mu.RLock()
	backend := proxy.backends["backend-1"]
	proxy.mu.RUnlock()

	assert.Equal(t, types.StatusUnhealthy, backend.Backend.Status)
}

// TestProxyBackendExists - проверка существования бэкенда
func TestProxyBackendExists(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	assert.True(t, proxy.BackendExists("backend-1"))
	assert.True(t, proxy.BackendExists("backend-2"))
	assert.False(t, proxy.BackendExists("non-existent"))
}

// TestProxyGetBackend - проверка получения бэкенда
func TestProxyGetBackend(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	backend := proxy.GetBackend("backend-1")
	assert.NotNil(t, backend)
	assert.Equal(t, "backend-1", backend.ID)
	assert.Equal(t, "Backend 1", backend.Name)
}

// TestProxyGetAllBackends - проверка получения всех бэкендов
func TestProxyGetAllBackends(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	backends := proxy.GetAllBackends()
	assert.Len(t, backends, 2)
}

// TestProxyAddBackend - проверка добавления бэкенда
func TestProxyAddBackend(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	newBackend := types.Backend{
		ID:         "backend-3",
		Name:       "Backend 3",
		Host:       "localhost",
		OllamaPort: 11436,
	}

	err := proxy.AddBackend(newBackend)
	assert.NoError(t, err)
	assert.True(t, proxy.BackendExists("backend-3"))
}

// TestProxyAddBackendDuplicate - проверка добавления дубликата бэкенда
func TestProxyAddBackendDuplicate(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Пытаемся добавить существующий бэкенд
	err := proxy.AddBackend(types.Backend{
		ID: "backend-1",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

// TestProxyRemoveBackend - проверка удаления бэкенда
func TestProxyRemoveBackend(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	err := proxy.RemoveBackend("backend-1")
	assert.NoError(t, err)
	assert.False(t, proxy.BackendExists("backend-1"))
}

// TestProxyRemoveBackendNotFound - проверка удаления несуществующего бэкенда
func TestProxyRemoveBackendNotFound(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	err := proxy.RemoveBackend("non-existent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestProxyUpdateBackend - проверка обновления бэкенда
func TestProxyUpdateBackend(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	updated := types.Backend{
		ID:       "backend-1",
		Name:     "Updated Backend 1",
		Host:     "localhost",
		Weight:   5,
	}

	err := proxy.UpdateBackend("backend-1", updated)
	assert.NoError(t, err)

	backend := proxy.GetBackend("backend-1")
	assert.Equal(t, "Updated Backend 1", backend.Name)
	assert.Equal(t, 5, backend.Weight)
}

// TestProxyUpdateBackendNotFound - проверка обновления несуществующего бэкенда
func TestProxyUpdateBackendNotFound(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	err := proxy.UpdateBackend("non-existent", types.Backend{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestSessionManager - проверка менеджера сессий
func TestSessionManager(t *testing.T) {
	t.Parallel()

	sm := NewSessionManager()
	defer sm.Clear()

	// Создаем сессию
	sm.Set("session-1", "backend-1", "llama2", "TestClient", "go-test", "TestAgent/1.0")

	// Получаем сессию
	session := sm.Get("session-1")
	assert.NotNil(t, session)
	assert.Equal(t, "backend-1", session.BackendID)
	assert.Equal(t, "llama2", session.Model)
	assert.Equal(t, "TestClient", session.ClientName)
	assert.Equal(t, 1, session.RequestCount)

	// Обновляем сессию
	sm.Set("session-1", "backend-1", "llama2", "TestClient", "go-test", "TestAgent/1.0")
	session = sm.Get("session-1")
	assert.Equal(t, 2, session.RequestCount)
}

// TestSessionManagerGetNonExistent - проверка получения несуществующей сессии
func TestSessionManagerGetNonExistent(t *testing.T) {
	t.Parallel()

	sm := NewSessionManager()

	session := sm.Get("non-existent")
	assert.Nil(t, session)
}

// TestSessionManagerDelete - проверка удаления сессии
func TestSessionManagerDelete(t *testing.T) {
	t.Parallel()

	sm := NewSessionManager()
	sm.Set("session-1", "backend-1", "llama2", "TestClient", "go-test", "TestAgent/1.0")

	assert.True(t, sm.Delete("session-1"))
	assert.Nil(t, sm.Get("session-1"))
	assert.False(t, sm.Delete("non-existent"))
}

// TestMetricsManager - проверка менеджера метрик
func TestMetricsManager(t *testing.T) {
	t.Parallel()

	mm := NewMetricsManager()
	assert.NotNil(t, mm)
	assert.NotNil(t, mm.metrics)
}

// TestQueueRequestTimeout - проверка таймаута запроса в очереди
func TestQueueRequestTimeout(t *testing.T) {
	t.Parallel()

	// Создаем новый queue manager для теста (0 workers чтобы не обрабатывать)
	queueMgr := NewQueueManager(nil, 10, 0, 100*time.Millisecond)
	defer queueMgr.Stop()

	done := make(chan bool, 1)
	queuedReq := &QueuedRequest{
		Model:    "test",
		Enqueued: time.Now(),
		Done:     done,
	}

	// Отправляем в канал
	select {
	case queueMgr.queue <- queuedReq:
	default:
		t.Fatal("Не удалось отправить в очередь")
	}

	// Ждем немного
	time.Sleep(50 * time.Millisecond)

	// Проверяем что запрос в очереди
	assert.Equal(t, 1, len(queueMgr.queue))
}

// TestCalculateScore - проверка вычисления scores
func TestCalculateScore(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Устанавливаем метрики
	proxy.UpdateMetrics("backend-1", &types.BackendMetrics{
		ID: "backend-1",
		GPU: types.GPUMetrics{
			UsagePercent: 30,
			MemoryTotal:  16384,
			MemoryFree:   12000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 40,
		},
	})

	score := proxy.calculateScore("backend-1")
	assert.Greater(t, score, float64(0))
}

// TestCheckResourceLimits - проверка проверки лимитов ресурсов
func TestCheckResourceLimits(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Устанавливаем метрики в пределах лимитов
	proxy.UpdateMetrics("backend-1", &types.BackendMetrics{
		ID: "backend-1",
		GPU: types.GPUMetrics{
			UsagePercent: 50,
			MemoryTotal:  16384,
			MemoryUsed:   8000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 50,
			MemoryTotal:     32768,
			MemoryUsed:      16000,
			DiskFree:        5000,
		},
	})

	// Проверяем что лимиты не превышены
	assert.True(t, proxy.checkResourceLimits("backend-1"))
}

// TestCheckResourceLimitsExceeded - проверка превышения лимитов
func TestCheckResourceLimitsExceeded(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Устанавливаем метрики с превышением лимитов
	proxy.UpdateMetrics("backend-1", &types.BackendMetrics{
		ID: "backend-1",
		GPU: types.GPUMetrics{
			UsagePercent: 95, // Превышает MaxUsagePercent 90
			MemoryTotal:  16384,
			MemoryUsed:   8000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 50,
			MemoryTotal:     32768,
			MemoryUsed:      16000,
			DiskFree:        5000,
		},
	})

	// Проверяем что лимиты превышены
	assert.False(t, proxy.checkResourceLimits("backend-1"))
}

// TestFindBackendWithModel - проверка поиска бэкенда с моделью
func TestFindBackendWithModel(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.ModelAffinity = true
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Устанавливаем метрики с запущенной моделью
	proxy.UpdateMetrics("backend-1", &types.BackendMetrics{
		ID: "backend-1",
		GPU: types.GPUMetrics{
			UsagePercent: 10,
			MemoryTotal:  16384,
			MemoryFree:   14000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			DiskFree:        20480,
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "llama2:7b"},
			},
		},
	})

	// Ищем бэкенд с моделью
	backend := proxy.findBackendWithModel("llama2:7b", nil)
	assert.Equal(t, "backend-1", backend)
}

// TestSelectByResourcesWithLimits - проверка выбора с учетом лимитов активных запросов
func TestSelectByResourcesWithLimits(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Устанавливаем метрики для обоих бэкендов
	proxy.UpdateMetrics("backend-1", &types.BackendMetrics{
		ID: "backend-1",
		GPU: types.GPUMetrics{
			UsagePercent: 50,
			MemoryTotal:  16384,
			MemoryFree:   8000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 50,
			DiskFree:        10240, // > MinFreeMB (1024)
		},
		Ollama: types.OllamaMetrics{
			OllamaAvailable: true,
		},
	})

	proxy.UpdateMetrics("backend-2", &types.BackendMetrics{
		ID: "backend-2",
		GPU: types.GPUMetrics{
			UsagePercent: 20,
			MemoryTotal:  16384,
			MemoryFree:   12000,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 30,
			MemoryTotal:     32768,
			MemoryFree:      28000,
			DiskFree:        10240, // > MinFreeMB (1024)
		},
		Ollama: types.OllamaMetrics{
			OllamaAvailable: true,
		},
	})

	// Выбираем бэкенд — должен выбрать backend-2 (меньше загрузка)
	backend := proxy.selectByResources(nil)
	assert.Equal(t, "backend-2", backend)
}

// TestSelectBackendAllBusy - проверка когда все бэкенды заняты
func TestSelectBackendAllBusy(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Устанавливаем max concurrent requests = 0 чтобы заблокировать
	proxy.mu.Lock()
	for _, state := range proxy.backends {
		state.Backend.MaxConcurrentReqs = 0
	}
	proxy.mu.Unlock()

	// Выбираем бэкенд — должен вернуть пустую строку
	backend := proxy.selectBackend("", "")
	assert.Empty(t, backend)
}

// TestGetQueueStats - проверка получения статистики очереди
func TestGetQueueStats(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	proxy := NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	stats := proxy.GetQueueStats()
	assert.Equal(t, 100, stats.MaxSize)
	assert.Equal(t, int64(0), stats.Processed)
}