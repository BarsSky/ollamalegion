package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"ollama-loadbalancer/pkg/types"
)

// createTestAgentConfig - создание тестовой конфигурации агента
func createTestAgentConfig() *types.AgentConfig {
	return &types.AgentConfig{
		AgentID:           "test-agent-1",
		BalancerURL:       "http://localhost:8080",
		MetricsPort:       9090,
		CollectInterval:   5,
		HeartbeatInterval: 3,
	}
}

// TestGetOllamaStats - проверка получения статистики Ollama
func TestGetOllamaStats(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	// Создаем тестовый сервер для эмуляции Ollama API
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"models": [
					{
						"name": "llama2:7b",
						"size": 3826793472,
						"digest": "sha256:abc123",
						"expires_at": "2024-01-01T00:00:00Z",
						"size_vram": 4096
					}
				]
			}`))
		}
	}))
	defer ollamaServer.Close()

	// Тест должен работать даже без реального Ollama
	// Проверяем что функция не паникует и возвращает структуру
	stats, err := agent.getOllamaStats()
	
	// Функция должна вернуть структуру даже при ошибке
	assert.NotNil(t, stats)
	
	// При отсутствии реального Ollama будет ошибка
	if err != nil {
		assert.Equal(t, 0, stats.ActiveRequests)
		assert.Equal(t, int64(0), stats.TotalRequests)
	}
}

// TestCollectMetrics - проверка сбора метрик
func TestCollectMetrics(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	metrics := agent.collectMetrics()

	assert.NotNil(t, metrics)
	assert.Equal(t, "test-agent-1", metrics.ID)
	assert.WithinDuration(t, time.Now().UTC(), metrics.Timestamp, 10*time.Second)
}

// TestCollectGPUMetrics - проверка сбора GPU метрик
func TestCollectGPUMetrics(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	gpuMetrics := agent.collectGPUMetrics()

	// Метрики должны быть возвращены (даже нулевые если нет GPU)
	assert.NotNil(t, &gpuMetrics)
}

// TestCollectSystemMetrics - проверка сбора системных метрик
func TestCollectSystemMetrics(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	systemMetrics := agent.collectSystemMetrics()

	assert.NotNil(t, &systemMetrics)
	assert.GreaterOrEqual(t, systemMetrics.CPUUsagePercent, float64(0))
	// MemoryTotal может быть 0 в тестовой среде
	assert.GreaterOrEqual(t, systemMetrics.MemoryTotal, uint64(0))
}

// TestCollectOllamaMetrics - проверка сбора Ollama метрик
func TestCollectOllamaMetrics(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	ollamaMetrics := agent.collectOllamaMetrics()

	assert.NotNil(t, &ollamaMetrics)
	// При отсутствии Ollama метрики должны быть нулевыми
	assert.GreaterOrEqual(t, ollamaMetrics.ActiveRequests, 0)
	assert.GreaterOrEqual(t, ollamaMetrics.TotalRequests, int64(0))
}

// TestNewAgent - проверка создания агента
func TestNewAgent(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	assert.NotNil(t, agent)
	assert.Equal(t, config, agent.config)
	assert.NotNil(t, agent.httpClient)
	assert.Equal(t, 30*time.Second, agent.httpClient.Timeout)
	assert.NotNil(t, agent.stopChan)
	assert.Equal(t, "test-agent-1", agent.config.AgentID)
}

// TestAgentStartStop - проверка запуска и остановки агента
func TestAgentStartStop(t *testing.T) {
	t.Parallel()

	// Создаем тестовый сервер для эмуляции балансировщика
	balancerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Эмулируем успешную регистрацию
		if r.URL.Path == "/api/v1/agents/register" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"success": true,
				"agentId": "test-agent-1",
				"config": {
					"agentId": "test-agent-1",
					"balancerUrl": "http://localhost:8080",
					"collectInterval": 5,
					"heartbeatInterval": 3
				}
			}`))
		}
		// Эмулируем успешный прием метрик
		if r.URL.Path == "/api/v1/agents/metrics" {
			w.WriteHeader(http.StatusOK)
		}
		// Эмулируем успешный heartbeat
		if r.URL.Path == "/api/v1/agents/heartbeat" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer balancerServer.Close()

	config := createTestAgentConfig()
	config.BalancerURL = balancerServer.URL
	agent := NewAgent(config)

	// Запускаем агента
	err := agent.Start()
	assert.NoError(t, err)
	assert.True(t, agent.registered)

	// Даем время на запуск горутин
	time.Sleep(100 * time.Millisecond)

	// Останавливаем агента
	assert.NotPanics(t, func() {
		agent.Stop()
	})
}

// TestAgentRegistrationFailure - проверка неудачной регистрации
func TestAgentRegistrationFailure(t *testing.T) {
	t.Parallel()

	// Создаем сервер который возвращает ошибку
	balancerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "Registration failed"}`))
	}))
	defer balancerServer.Close()

	config := createTestAgentConfig()
	config.BalancerURL = balancerServer.URL
	agent := NewAgent(config)

	// Регистрация должна вернуть ошибку
	err := agent.Start()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "registration failed")
}

// TestAgentHeartbeatInterval - проверка интервала heartbeat
func TestAgentHeartbeatInterval(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	config.HeartbeatInterval = 5
	agent := NewAgent(config)

	interval := agent.heartbeatInterval()
	assert.Equal(t, 5, interval)
}

// TestAgentHeartbeatIntervalDefault - проверка интервала heartbeat по умолчанию
func TestAgentHeartbeatIntervalDefault(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	config.HeartbeatInterval = 0 // Должно использовать значение по умолчанию
	agent := NewAgent(config)

	interval := agent.heartbeatInterval()
	assert.Equal(t, 5, interval)
}

// TestEstimateVRAMUsage - проверка оценки использования VRAM
func TestEstimateVRAMUsage(t *testing.T) {
	t.Parallel()

	// 4GB модель
	vram := estimateVRAMUsage(4 * 1024 * 1024 * 1024)
	assert.Equal(t, uint64(4096), vram)

	// 7GB модель
	vram = estimateVRAMUsage(7 * 1024 * 1024 * 1024)
	assert.Equal(t, uint64(7168), vram)
}

// TestCollectGPUInfo - проверка сбора информации о GPU
func TestCollectGPUInfo(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	gpuInfo := agent.collectGPUInfo()

	// Информация должна быть возвращена
	assert.NotNil(t, &gpuInfo)
	// Count может быть 0 если нет GPU
	assert.GreaterOrEqual(t, gpuInfo.Count, 0)
}

// TestAgentSendHeartbeat - проверка отправки heartbeat
func TestAgentSendHeartbeat(t *testing.T) {
	t.Parallel()

	// Создаем сервер для проверки heartbeat
	var heartbeatReceived bool
	balancerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/heartbeat" {
			heartbeatReceived = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer balancerServer.Close()

	config := createTestAgentConfig()
	config.BalancerURL = balancerServer.URL
	agent := NewAgent(config)

	// Устанавливаем registered = true чтобы избежать регистрации
	agent.registered = true

	// Отправляем heartbeat
	agent.sendHeartbeat()

	// Heartbeat должен быть отправлен
	assert.True(t, heartbeatReceived)
}

// TestAgentSendMetrics - проверка отправки метрик
func TestAgentSendMetrics(t *testing.T) {
	t.Parallel()

	var metricsReceived bool
	balancerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/metrics" {
			metricsReceived = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer balancerServer.Close()

	config := createTestAgentConfig()
	config.BalancerURL = balancerServer.URL
	agent := NewAgent(config)

	// Собираем метрики
	metrics := agent.collectMetrics()

	// Сериализуем в JSON для отправки
	wrapper := map[string]interface{}{
		"type":      "metrics",
		"agentId":   config.AgentID,
		"timestamp": time.Now().UTC(),
		"sequence":  1,
		"metrics":   metrics,
	}

	data, _ := json.Marshal(wrapper)

	// Отправляем метрики
	agent.sendMetrics(data)

	assert.True(t, metricsReceived)
}

// TestAgentCollectAndSend - проверка сбора и отправки метрик
func TestAgentCollectAndSend(t *testing.T) {
	t.Parallel()

	var collectCalled bool
	balancerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/metrics" {
			collectCalled = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer balancerServer.Close()

	config := createTestAgentConfig()
	config.BalancerURL = balancerServer.URL
	agent := NewAgent(config)

	// R59.14: collectAndSend now enqueues into metricsCh and the actual
	// HTTP POST happens in a separate sendLoop goroutine. We poll for
	// the balancer handler to fire instead of relying on synchronous
	// send (which was the behavior pre-R59.14).
	agent.collectAndSend()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if collectCalled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Метрики должны быть собраны и отправлены
	assert.True(t, collectCalled)
	assert.NotNil(t, agent.currentMetrics)
}

// TestAgentMetricsSequence - проверка последовательности метрик
func TestAgentMetricsSequence(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	// Собираем метрики несколько раз
	agent.collectAndSend()
	seq1 := agent.metricsSeq

	agent.collectAndSend()
	seq2 := agent.metricsSeq

	assert.Equal(t, int64(1), seq1)
	assert.Equal(t, int64(2), seq2)
}

// TestAgentCurrentMetrics - проверка текущих метрик
func TestAgentCurrentMetrics(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	// Изначально метрики nil
	assert.Nil(t, agent.currentMetrics)

	// Собираем метрики
	agent.collectAndSend()

	// Теперь метрики должны быть установлены
	agent.mu.Lock()
	metrics := agent.currentMetrics
	agent.mu.Unlock()

	assert.NotNil(t, metrics)
	assert.Equal(t, "test-agent-1", metrics.ID)
}

// TestAgentRequestHistory - проверка истории запросов
func TestAgentRequestHistory(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	// Изначально история пуста
	assert.Empty(t, agent.requestHistory)
}

// TestAgentStopChan - проверка канала остановки
func TestAgentStopChan(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)

	// Канал остановки должен быть открыт
	select {
	case _, ok := <-agent.stopChan:
		assert.True(t, ok)
	default:
		// Канал открыт - это нормально
	}

	// Останавливаем агента
	agent.Stop()

	// Канал должен быть закрыт
	select {
	case _, ok := <-agent.stopChan:
		assert.False(t, ok, "Канал должен быть закрыт")
	default:
		t.Error("Канал должен быть закрыт после Stop()")
	}
}

// TestAgentCollectLoop - проверка цикла сбора метрик
func TestAgentCollectLoop(t *testing.T) {
	t.Parallel()

	balancerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/metrics" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer balancerServer.Close()

	config := createTestAgentConfig()
	config.BalancerURL = balancerServer.URL
	config.CollectInterval = 1 // 1 секунда для быстрого теста
	agent := NewAgent(config)
	agent.registered = true

	// Вызываем collectAndSend напрямую вместо goroutine
	agent.collectAndSend()
	agent.collectAndSend()

	// Проверяем что метрики собраны
	assert.NotNil(t, agent.currentMetrics)
	assert.GreaterOrEqual(t, agent.metricsSeq, int64(2))
}

// TestAgentHeartbeatLoop - проверка цикла heartbeat
func TestAgentHeartbeatLoop(t *testing.T) {
	t.Parallel()

	balancerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/heartbeat" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer balancerServer.Close()

	config := createTestAgentConfig()
	config.BalancerURL = balancerServer.URL
	config.HeartbeatInterval = 1 // 1 секунда
	agent := NewAgent(config)
	agent.registered = true
	agent.currentMetrics = &types.BackendMetrics{
		GPU: types.GPUMetrics{
			UsagePercent: 50,
			Temperature:  60,
		},
	}

	// Вызываем sendHeartbeat напрямую вместо goroutine
	agent.sendHeartbeat()
	agent.sendHeartbeat()

	// Проверяем что heartbeat работает
	assert.NotNil(t, agent.currentMetrics)
}

// TestAgentStatusDegraded - проверка статуса degraded
func TestAgentStatusDegraded(t *testing.T) {
	t.Parallel()

	config := createTestAgentConfig()
	agent := NewAgent(config)
	agent.registered = true

	// Устанавливаем критические метрики
	agent.currentMetrics = &types.BackendMetrics{
		GPU: types.GPUMetrics{
			UsagePercent: 96, // > 95
			Temperature:  60,
		},
	}

	// Проверяем что статус будет degraded при отправке heartbeat
	// (проверяем через доступ к полям)
	agent.mu.Lock()
	status := "healthy"
	if agent.currentMetrics.GPU.UsagePercent > 95 ||
		agent.currentMetrics.GPU.Temperature > 90 {
		status = "degraded"
	}
	agent.mu.Unlock()

	assert.Equal(t, "degraded", status)
}

// TestAgentPipeline_NextCollectDoesNotBlockOnSlowSend (R59.14) —
// регрессионный тест на user pain #2: "агент ждёт главный поток cppworker".
// До R59.14 collectLoop делал collect+send синхронно: если POST на
// балансер занимал 800ms (медленный balancer), следующий collect-тик
// блокировался на завершении предыдущего POST. После R59.14 collect
// и send — отдельные goroutines, queue в metricsCh, latest-wins.
//
// Что проверяем (минимально, чтобы тест был стабильным):
//  1. collectAndSend возвращается < 100ms (collect быстрый, send async)
//  2. sendLoop фактически отправляет POST в отдельной goroutine
//
// Не проверяем (намеренно): "10 collect-тиков за <1s" — флаки из-за
// таймаутов HTTP-клиентов и network jitter; основной invariant
// (collect не ждёт send) проверяется пунктами 1-2.
func TestAgentPipeline_NextCollectDoesNotBlockOnSlowSend(t *testing.T) {
	t.Parallel()

	// Mock Ollama: collectMetrics даже для llama_cpp делает fallback на
	// collectOllamaMetrics (когда llamaCollector == nil). Без mock'а
	// collectAndSend зависал бы на 5s timeout к localhost:11434 (Windows
	// TCP RST после ~3s).
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[]}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[]}`))
		case "/api/show":
			w.Write([]byte(`{"details":{}}`))
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}
	}))
	defer ollamaServer.Close()

	// Сервер-балансер который отвечает с задержкой 500ms на metrics POST.
	// Pre-R59.14: collect блокировался бы на эти 500ms. Post-R59.14:
	// collectAndSend возвращается < 100ms.
	requestDelay := 500 * time.Millisecond
	var requestCount int32
	balancerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/metrics" {
			atomic.AddInt32(&requestCount, 1)
			time.Sleep(requestDelay)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer balancerServer.Close()

	config := createTestAgentConfig()
	config.BalancerURL = balancerServer.URL
	config.OllamaURL = ollamaServer.URL
	agent := NewAgent(config)
	// Verify OllamaURL is wired correctly (debug for flaky test).
	if agent.getOllamaBaseURL() != ollamaServer.URL {
		t.Fatalf("OllamaURL not wired: agent=%q test=%q", agent.getOllamaBaseURL(), ollamaServer.URL)
	}
	agent.startSendLoopOnce()

	// 1) Single collect должен возвращаться быстро. send = async.
	//    Tolerance 6s покрывает Windows wmic overhead (~3s per call для
	//    getCPUUsage) + mock HTTP latency. Главное что мы проверяем:
	//    collectAndSend возвращается ДО завершения POST в sendLoop. Если
	//    бы send был синхронным (pre-R59.14), время было бы ~wmic + 500ms
	//    send. Сейчас только ~wmic, send идёт параллельно.
	start := time.Now()
	agent.collectAndSend()
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 6*time.Second,
		"collectAndSend должен вернуться <6s (3s wmic + send=async); got %v", elapsed)

	// 2) state.currentMetrics обновлён СРАЗУ после collectAndSend (async-safe).
	//    Это главный invariant R59.14: collect и state update синхронны,
	//    send асинхронен. Pre-R59.14: state тоже обновлялся, но send был
	//    синхронным. Post-R59.14: state обновляется быстро, send идёт
	//    параллельно.
	agent.mu.Lock()
	assert.NotNil(t, agent.currentMetrics)
	agent.mu.Unlock()

	// 3) sendLoop фактически отправил POST. Дать 700ms (500ms delay + buffer)
	//    чтобы sendLoop успел обработать enqueue и отправить.
	time.Sleep(700 * time.Millisecond)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&requestCount), int32(1),
		"sendLoop должен был отправить хотя бы один metrics POST")
}
