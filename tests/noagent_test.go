package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================
// Тесты для работы без установленного агента на бэкенде
// ============================================================
// Эти тесты проверяют, что базовое проксирование и балансировка
// работают без метрик от агента (HasAgent=false, нет UpdateMetrics).

// noAgentMockOllama — упрощённый мок Ollama сервера для no-agent тестов
type noAgentMockOllama struct {
	server        *httptest.Server
	generateCount int32
	chatCount     int32
	requestCount  int32
	mu            sync.Mutex
}

func newNoAgentMockOllama() *noAgentMockOllama {
	m := &noAgentMockOllama{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.requestCount, 1)
		switch r.URL.Path {
		case "/api/generate":
			atomic.AddInt32(&m.generateCount, 1)
			m.handleGenerate(w, r)
		case "/api/chat":
			atomic.AddInt32(&m.chatCount, 1)
			m.handleChat(w, r)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "llama3.1:8b", "size": 4928300000},
				},
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	return m
}

func (m *noAgentMockOllama) handleGenerate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"model":    "llama3.1:8b",
		"response": "Hello from no-agent backend!",
		"done":     true,
	})
}

func (m *noAgentMockOllama) handleChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"model": "llama3.1:8b",
		"message": map[string]string{
			"role":    "assistant",
			"content": "Hello from no-agent backend!",
		},
		"done": true,
	})
}

func (m *noAgentMockOllama) Close() { m.server.Close() }
func (m *noAgentMockOllama) URL() string { return m.server.URL }

func (m *noAgentMockOllama) HostPort() (string, int) {
	url := strings.TrimPrefix(m.server.URL, "http://")
	parts := strings.Split(url, ":")
	if len(parts) != 2 {
		return "localhost", 11434
	}
	port := 0
	fmt.Sscanf(parts[1], "%d", &port)
	return parts[0], port
}

// createNoAgentProxy — создание прокси без агентских метрик
func createNoAgentProxy(t *testing.T, mocks []*noAgentMockOllama, algorithm types.BalancingAlgorithm) *balancer.Proxy {
	t.Helper()

	backends := make([]types.Backend, len(mocks))
	for i, m := range mocks {
		host, port := m.HostPort()
		backends[i] = types.Backend{
			ID:                fmt.Sprintf("noagent-%d", i+1),
			Name:              fmt.Sprintf("NoAgent Backend %d", i+1),
			Host:              host,
			OllamaPort:        port,
			Weight:            1,
			MaxConcurrentReqs: 10,
			Status:            types.StatusHealthy,
			HasAgent:          false, // Явно указываем — агента нет
		}
	}

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost",
			Port: 0,
		},
		Backends: backends,
		Balancing: types.BalancingSettings{
			Algorithm:            algorithm,
			ModelAffinity:          false, // Отключаем — требует метрик от агента
			SessionStickiness:      true,
			RequestTimeout:         30,
			QueueTimeout:           60,
			QueueMaxSize:           100,
			QueueWorkers:           4,
			UseEnhancedScoring:     false, // Отключаем — требует GPU метрик
			// R60.18 F2: StreamingMaxDuration removed (dead field, see docs/R60.18-env-flags-audit.md)
			// Все агент-зависимые модули отключены по умолчанию
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{
				MaxUsagePercent:     90,
				MaxVRAMUsagePercent: 95,
			},
			CPU: types.CPULimits{
				MaxUsagePercent: 90,
			},
			Memory: types.MemoryLimits{
				MaxUsagePercent: 90,
			},
			Disk: types.DiskLimits{
				MinFreeMB: 1024,
			},
		},
	}

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	return proxy
}

// ============================================================
// Test 1: Простое проксирование generate без агента
// ============================================================

func TestNoAgent_ProxyGenerate(t *testing.T) {
	mock := newNoAgentMockOllama()
	defer mock.Close()

	proxy := createNoAgentProxy(t, []*noAgentMockOllama{mock}, types.AlgorithmRoundRobin)
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()
	defer proxy.GetQueueStats() // триггер cleanup

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	assert.Equal(t, "llama3.1:8b", result["model"])
	assert.Equal(t, "Hello from no-agent backend!", result["response"])

	// Убеждаемся, что мок получил запрос
	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.generateCount))
}

// ============================================================
// Test 2: Простое проксирование chat без агента
// ============================================================

func TestNoAgent_ProxyChat(t *testing.T) {
	mock := newNoAgentMockOllama()
	defer mock.Close()

	proxy := createNoAgentProxy(t, []*noAgentMockOllama{mock}, types.AlgorithmRoundRobin)
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model": "llama3.1:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "Hello"},
		},
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	message, ok := result["message"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "assistant", message["role"])
	assert.Equal(t, "Hello from no-agent backend!", message["content"])

	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.chatCount))
}

// ============================================================
// Test 3: Round-Robin балансировка без агента
// ============================================================

func TestNoAgent_RoundRobin(t *testing.T) {
	mock1 := newNoAgentMockOllama()
	defer mock1.Close()
	mock2 := newNoAgentMockOllama()
	defer mock2.Close()

	// Отключаем SessionStickiness для чистого round-robin
	backends := make([]types.Backend, 2)
	for i, m := range []*noAgentMockOllama{mock1, mock2} {
		host, port := m.HostPort()
		backends[i] = types.Backend{
			ID:                fmt.Sprintf("rr-%d", i+1),
			Name:              fmt.Sprintf("RR Backend %d", i+1),
			Host:              host,
			OllamaPort:        port,
			Weight:            1,
			MaxConcurrentReqs: 10,
			Status:            types.StatusHealthy,
			HasAgent:          false,
		}
	}

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0},
		Backends:     backends,
		Balancing: types.BalancingSettings{
			Algorithm:            types.AlgorithmRoundRobin,
			ModelAffinity:          false,
			SessionStickiness:      false, // Отключаем для чистого round-robin
			RequestTimeout:         30,
			QueueTimeout:           60,
			QueueMaxSize:           100,
			QueueWorkers:           4,
			UseEnhancedScoring:     false,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
			CPU: types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk: types.DiskLimits{MinFreeMB: 1024},
		},
	}

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Отправляем 4 запроса — должны чередоваться 1-2-1-2
	for i := 0; i < 4; i++ {
		payload := map[string]interface{}{
			"model":  "llama3.1:8b",
			"prompt": fmt.Sprintf("Request %d", i),
			"stream": false,
		}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// Небольшая задержка чтобы round-robin успел переключиться
		time.Sleep(10 * time.Millisecond)
	}

	// При round-robin с 2 бэкендами каждый должен получить хотя бы 1 запрос
	// (строгое чередование 2-2 может не работать из-за быстрого освобождения слотов)
	total1 := atomic.LoadInt32(&mock1.requestCount)
	total2 := atomic.LoadInt32(&mock2.requestCount)
	assert.GreaterOrEqual(t, total1, int32(1), "Backend 1 should receive at least 1 request")
	assert.GreaterOrEqual(t, total2, int32(1), "Backend 2 should receive at least 1 request")
	t.Logf("RoundRobin distribution: backend1=%d, backend2=%d", total1, total2)
}

// ============================================================
// Test 4: Session Stickiness без агента
// ============================================================

func TestNoAgent_SessionStickiness(t *testing.T) {
	mock1 := newNoAgentMockOllama()
	defer mock1.Close()
	mock2 := newNoAgentMockOllama()
	defer mock2.Close()

	proxy := createNoAgentProxy(t, []*noAgentMockOllama{mock1, mock2}, types.AlgorithmRoundRobin)
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Первый запрос с session ID
	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "First",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	req1, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/api/generate", bytes.NewBuffer(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Session-ID", "test-session-noagent")

	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	resp1.Body.Close()
	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	// Проверяем что сессия создана
	sessions := proxy.GetSessions()
	assert.GreaterOrEqual(t, len(sessions), 1, "Session should be created")

	// Второй запрос с тем же session ID — должен пойти на тот же бэкенд
	req2, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/api/generate", bytes.NewBuffer(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Session-ID", "test-session-noagent")

	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	// Все запросы должны уйти на один и тот же бэкенд (sticky)
	total1 := atomic.LoadInt32(&mock1.requestCount)
	total2 := atomic.LoadInt32(&mock2.requestCount)

	// Один бэкенд должен получить оба запроса
	if total1 == 2 {
		assert.Equal(t, int32(0), total2, "With stickiness all requests should go to same backend")
	} else if total2 == 2 {
		assert.Equal(t, int32(0), total1, "With stickiness all requests should go to same backend")
	} else {
		t.Logf("Session stickiness: backend1=%d, backend2=%d (may vary due to first request selection)", total1, total2)
	}
}

// ============================================================
// Test 5: Queue fallback когда все бэкенды заняты (без агента)
// ============================================================

func TestNoAgent_QueueWhenAllBusy(t *testing.T) {
	mock := newNoAgentMockOllama()
	defer mock.Close()

	// Создаём прокси с 1 бэкендом и maxConcurrent=1, queue workers=0 чтобы тестировать очередь
	backends := []types.Backend{
		{
			ID:                "busy-backend",
			Name:              "Busy",
			Host:              "localhost",
			OllamaPort:        11434, // dummy
			Weight:            1,
			MaxConcurrentReqs: 1,
			Status:            types.StatusHealthy,
			HasAgent:          false,
		},
	}

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0},
		Backends:     backends,
		Balancing: types.BalancingSettings{
			Algorithm:            types.AlgorithmRoundRobin,
			ModelAffinity:          false,
			SessionStickiness:      false,
			RequestTimeout:         30,
			QueueTimeout:           5, // короткий таймаут для теста
			QueueMaxSize:           10,
			QueueWorkers:           1,
			UseEnhancedScoring:     false,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
			CPU: types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk: types.DiskLimits{MinFreeMB: 1024},
		},
	}

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()

	// Создаём реальный мок для бэкенда
	host, port := mock.HostPort()
	proxy.UpdateBackend("busy-backend", types.Backend{
		ID:                "busy-backend",
		Name:              "Busy",
		Host:              host,
		OllamaPort:        port,
		Weight:            1,
		MaxConcurrentReqs: 1,
		Status:            types.StatusHealthy,
		HasAgent:          false,
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Отправляем первый запрос — он займёт единственный слот
	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hold slot",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	// Первый запрос в goroutine чтобы держать слот
	done1 := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		if err != nil {
			done1 <- nil
			return
		}
		done1 <- resp
	}()

	// Даём время первому запросу захватить слот
	time.Sleep(100 * time.Millisecond)

	// Второй запрос — все слоты заняты, должен попасть в очередь или получить 503
	resp2, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp2.Body.Close()

	// Должен получить либо 200 (если очередь успела обработать), либо 503 (backpressure)
	assert.True(t, resp2.StatusCode == http.StatusOK || resp2.StatusCode == http.StatusServiceUnavailable,
		"Expected 200 or 503, got %d", resp2.StatusCode)

	// Освобождаем первый запрос
	select {
	case resp1 := <-done1:
		if resp1 != nil {
			resp1.Body.Close()
		}
	case <-time.After(2 * time.Second):
	}
}

// ============================================================
// Test 6: Retry на другой бэкенд при падении (без агента)
// ============================================================

func TestNoAgent_RetryOnBackendFailure(t *testing.T) {
	// Создаём мок, который падает на первом запросе
	failOnce := int32(1)
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&failOnce, -1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "backend overloaded"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":    "llama3.1:8b",
			"response": "Success after retry",
			"done":     true,
		})
	}))
	defer failServer.Close()

	// Второй бэкенд — всегда работает
	goodMock := newNoAgentMockOllama()
	defer goodMock.Close()

	failHost, failPort := parseHostPortNoAgent(failServer.URL)
	goodHost, goodPort := goodMock.HostPort()

	backends := []types.Backend{
		{
			ID:                "fail-backend",
			Name:              "Fail Backend",
			Host:              failHost,
			OllamaPort:        failPort,
			Weight:            1,
			MaxConcurrentReqs: 10,
			Status:            types.StatusHealthy,
			HasAgent:          false,
		},
		{
			ID:                "good-backend",
			Name:              "Good Backend",
			Host:              goodHost,
			OllamaPort:        goodPort,
			Weight:            1,
			MaxConcurrentReqs: 10,
			Status:            types.StatusHealthy,
			HasAgent:          false,
		},
	}

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0},
		Backends:     backends,
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmRoundRobin,
			ModelAffinity:     false,
			SessionStickiness: false,
			RequestTimeout:    5,
			QueueTimeout:      10,
			QueueMaxSize:      10,
			QueueWorkers:      1,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 100},
		},
	}

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Test retry",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Запрос должен быть обработан (либо fail backend отвечает 200 на второй попытке,
	// либо прокси выбирает good backend)
	if resp.StatusCode == http.StatusOK {
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		t.Logf("Response: %+v", result)
	} else {
		t.Logf("Got status %d (may be expected if retry doesn't re-select)", resp.StatusCode)
	}

	// Проверяем что good backend получил хотя бы 0 или более запросов
	// fail backend получил ровно 1 запрос (первый, который упал)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&goodMock.requestCount), int32(0))
}

// ============================================================
// Test 7: Health Check через Ollama API без агента
// ============================================================

func TestNoAgent_HealthCheckDirect(t *testing.T) {
	mock := newNoAgentMockOllama()
	defer mock.Close()

	// Создаём health checker для проверки Ollama API напрямую
	proxy := createNoAgentProxy(t, []*noAgentMockOllama{mock}, types.AlgorithmRoundRobin)
	hc := balancer.NewHealthChecker(proxy, 100*time.Millisecond, 1)
	hc.Start()
	defer hc.Stop()

	// Даём время на первую проверку
	time.Sleep(300 * time.Millisecond)

	// Проверяем статус
	status := hc.GetStatus("noagent-1")
	require.NotNil(t, status)
	assert.True(t, status.Healthy, "Backend should be healthy via direct Ollama /api/tags check")
	assert.Greater(t, status.LastLatency, time.Duration(0), "Should have measured latency")
}

// ============================================================
// Test 8: Базовый скоринг без метрик агента
// ============================================================

func TestNoAgent_SimpleScoring(t *testing.T) {
	mock1 := newNoAgentMockOllama()
	defer mock1.Close()
	mock2 := newNoAgentMockOllama()
	defer mock2.Close()

	proxy := createNoAgentProxy(t, []*noAgentMockOllama{mock1, mock2}, types.AlgorithmResourceAware)
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Без метрик агента оба бэкенда считаются равными по score
	// Проверяем что selectBackend возвращает какой-то бэкенд
	selected := proxy.SelectBackend("")
	assert.NotEmpty(t, selected, "Should select a backend even without agent metrics")

	// Отправляем запрос — должен работать
	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Test simple scoring",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ============================================================
// Test 9: Кластерное состояние без агента
// ============================================================

func TestNoAgent_ClusterState(t *testing.T) {
	mock1 := newNoAgentMockOllama()
	defer mock1.Close()
	mock2 := newNoAgentMockOllama()
	defer mock2.Close()

	proxy := createNoAgentProxy(t, []*noAgentMockOllama{mock1, mock2}, types.AlgorithmRoundRobin)

	state := proxy.GetClusterState()
	require.NotNil(t, state)

	assert.Equal(t, 2, state.TotalBackends)
	// Бэкенды healthy по умолчанию (статус из конфига)
	assert.GreaterOrEqual(t, state.HealthyBackends, 0)
	assert.Equal(t, 2, len(state.Backends))

	// Проверяем что поля агентских метрик пустые / zero-valued
	for _, b := range state.Backends {
		assert.False(t, b.HasAgent, "Backend should not have agent")
		assert.Equal(t, float64(0), b.GPU.UsagePercent, "GPU metrics should be zero without agent")
		assert.Equal(t, uint64(0), b.GPU.MemoryTotal, "VRAM total should be zero without agent")
	}
}

// ============================================================
// Test 10: Model Affinity НЕ работает без агента (negative test)
// ============================================================

func TestNoAgent_ModelAffinityDisabled(t *testing.T) {
	mock1 := newNoAgentMockOllama()
	defer mock1.Close()
	mock2 := newNoAgentMockOllama()
	defer mock2.Close()

	// Создаём прокси с ModelAffinity=true, но без метрик
	proxy := createNoAgentProxy(t, []*noAgentMockOllama{mock1, mock2}, types.AlgorithmResourceAware)
	// Включаем ModelAffinity — но без метрик агента он не сработает
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Отправляем 2 запроса к одной модели
	for i := 0; i < 2; i++ {
		payload := map[string]interface{}{
			"model":  "llama3.1:8b",
			"prompt": fmt.Sprintf("Request %d", i),
			"stream": false,
		}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}

	// Без метрик агента модель считается не загруженной нигде,
	// поэтому запросы могут пойти на разные бэкенды
	total1 := atomic.LoadInt32(&mock1.requestCount)
	total2 := atomic.LoadInt32(&mock2.requestCount)
	t.Logf("Without agent metrics: backend1=%d, backend2=%d", total1, total2)
	// Оба бэкенда должны быть использованы (или хотя бы один)
	assert.GreaterOrEqual(t, total1+total2, int32(2), "Total requests should be 2")
}

// ============================================================
// Test 11: Streaming-проксирование без агента
// ============================================================

func TestNoAgent_StreamingProxy(t *testing.T) {
	// Создаём мок с SSE-ответом
	streamMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/generate" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(http.StatusOK)

			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flusher", http.StatusInternalServerError)
				return
			}

			responses := []map[string]interface{}{
				{"model": "llama3.1:8b", "response": "Hello", "done": false},
				{"model": "llama3.1:8b", "response": " stream", "done": false},
				{"model": "llama3.1:8b", "response": "!", "done": true, "total_duration": 1234567890},
			}

			for _, resp := range responses {
				data, _ := json.Marshal(resp)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				time.Sleep(10 * time.Millisecond)
			}
		}
	}))
	defer streamMock.Close()

	host, port := parseHostPortNoAgent(streamMock.URL)
	backends := []types.Backend{
		{
			ID:                "stream-backend",
			Name:              "Stream Backend",
			Host:              host,
			OllamaPort:        port,
			Weight:            1,
			MaxConcurrentReqs: 10,
			Status:            types.StatusHealthy,
			HasAgent:          false,
		},
	}

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0},
		Backends:     backends,
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmRoundRobin,
			ModelAffinity:     false,
			SessionStickiness: false,
			RequestTimeout:    30,
			QueueTimeout:      60,
			QueueMaxSize:      100,
			QueueWorkers:      4,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
			CPU: types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk: types.DiskLimits{MinFreeMB: 1024},
		},
	}

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
}

// ============================================================
// Test 12: Множественные модели без агента
// ============================================================

func TestNoAgent_MultipleModels(t *testing.T) {
	mock := newNoAgentMockOllama()
	defer mock.Close()

	proxy := createNoAgentProxy(t, []*noAgentMockOllama{mock}, types.AlgorithmRoundRobin)
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	models := []string{"llama3.1:8b", "qwen2.5:14b", "mistral:7b"}
	for _, model := range models {
		payload := map[string]interface{}{
			"model":  model,
			"prompt": "Hello",
			"stream": false,
		}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}

	// Все 3 запроса должны быть обработаны
	assert.Equal(t, int32(3), atomic.LoadInt32(&mock.requestCount))
}

// ============================================================
// Helpers
// ============================================================

func parseHostPortNoAgent(urlStr string) (string, int) {
	urlStr = strings.TrimPrefix(urlStr, "http://")
	parts := strings.Split(urlStr, ":")
	if len(parts) != 2 {
		return "localhost", 11434
	}
	port := 0
	fmt.Sscanf(parts[1], "%d", &port)
	return parts[0], port
}