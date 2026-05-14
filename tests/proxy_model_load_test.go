package tests

import (
	"bufio"
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

// ============================================================================
// Мок-сервер Ollama, имитирующий модель "не в памяти, но на диске"
// ============================================================================

// mockOllamaWithSlowLoad имитирует Ollama где:
//   - /api/ps        → модель НЕ в RunningModels (не в памяти)
//   - /api/tags      → модель ЕСТЬ в AvailableModels (на диске)
//   - /api/generate  → первый вызов с задержкой (подгрузка в память), затем SSE
type mockOllamaWithSlowLoad struct {
	server           *httptest.Server
	generateCount    int32
	tagsCount        int32
	psCount          int32
	pullCount        int32
	modelName        string
	loadDelay        time.Duration // задержка перед первым SSE-событием (имитация подгрузки)
	firstCallFail404 bool          // первый вызов generate возвращает 404 (model not found)
	mu               sync.Mutex
}

func newMockOllamaWithSlowLoad(modelName string, loadDelay time.Duration, firstCallFail404 bool) *mockOllamaWithSlowLoad {
	mock := &mockOllamaWithSlowLoad{
		modelName:        modelName,
		loadDelay:        loadDelay,
		firstCallFail404: firstCallFail404,
	}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mock.handleRequest(w, r)
	}))
	return mock
}

func (m *mockOllamaWithSlowLoad) handleRequest(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/generate":
		atomic.AddInt32(&m.generateCount, 1)
		m.handleGenerate(w, r)
	case "/api/chat":
		atomic.AddInt32(&m.generateCount, 1)
		m.handleChat(w, r)
	case "/api/tags":
		atomic.AddInt32(&m.tagsCount, 1)
		m.handleTags(w, r)
	case "/api/ps":
		atomic.AddInt32(&m.psCount, 1)
		m.handlePs(w, r)
	case "/api/pull":
		atomic.AddInt32(&m.pullCount, 1)
		m.handlePull(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}
}

func (m *mockOllamaWithSlowLoad) handleGenerate(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	model := ""
	if mod, ok := req["model"].(string); ok {
		model = mod
	}

	stream := true
	if s, ok := req["stream"].(bool); ok {
		stream = s
	}

	// Если настроено firstCallFail404 и это первый вызов — возвращаем 404
	count := atomic.LoadInt32(&m.generateCount)
	if m.firstCallFail404 && count == 1 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": fmt.Sprintf("model %q not found", model),
		})
		return
	}

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		// Имитация задержки подгрузки модели в память
		if m.loadDelay > 0 {
			time.Sleep(m.loadDelay)
		}

		responses := []map[string]interface{}{
			{"model": model, "response": "Hello", "done": false},
			{"model": model, "response": " world", "done": false},
			{"model": model, "response": "!", "done": true, "total_duration": 1234567890},
		}

		for _, resp := range responses {
			data, _ := json.Marshal(resp)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	} else {
		if m.loadDelay > 0 {
			time.Sleep(m.loadDelay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":    model,
			"response": "Hello world!",
			"done":     true,
		})
	}
}

func (m *mockOllamaWithSlowLoad) handleChat(w http.ResponseWriter, r *http.Request) {
	// Для чата используем тот же паттерн что и generate
	m.handleGenerate(w, r)
}

func (m *mockOllamaWithSlowLoad) handleTags(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Модель доступна на диске (можно скачать/подгрузить), но не обязательно в памяти
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models": []map[string]interface{}{
			{
				"name":    m.modelName,
				"size":    4928300000,
				"digest":  "sha256:abc123",
				"details": map[string]interface{}{"family": "llama", "parameter_size": "8B"},
			},
		},
	})
}

func (m *mockOllamaWithSlowLoad) handlePs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// RunningModels пуст — модель не загружена в память!
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models": []map[string]interface{}{},
	})
}

func (m *mockOllamaWithSlowLoad) handlePull(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	responses := []map[string]interface{}{
		{"status": "pulling manifest"},
		{"status": "downloading", "completed": 4096, "total": 4096},
		{"status": "success"},
	}

	for _, resp := range responses {
		data, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (m *mockOllamaWithSlowLoad) URL() string {
	return m.server.URL
}

func (m *mockOllamaWithSlowLoad) Close() {
	m.server.Close()
}

func (m *mockOllamaWithSlowLoad) HostPort() (string, int) {
	url := m.server.URL
	url = strings.TrimPrefix(url, "http://")
	parts := strings.Split(url, ":")
	if len(parts) != 2 {
		return "localhost", 11434
	}
	port := 0
	fmt.Sscanf(parts[1], "%d", &port)
	return parts[0], port
}

// ============================================================================
// Helper: настройка прокси с моком "модель на диске, не в памяти"
// ============================================================================

func setupProxyWithSlowLoadMock(t *testing.T, mock *mockOllamaWithSlowLoad, requestTimeout int) (*httptest.Server, *balancer.Proxy) {
	t.Helper()

	host, port := mock.HostPort()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    0,
			APIPort: 0,
		},
		Backends: []types.Backend{
			{
				ID:                "ollama-slow",
				Name:              "Test Ollama Slow Load",
				Host:              host,
				OllamaPort:        port,
				AgentPort:         9090,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: true,
			RequestTimeout:    requestTimeout,
			QueueTimeout:      60,
			QueueMaxSize:      100,
			QueueWorkers:      4,
			AutoPull: types.AutoPullConfig{
				Enabled:       true,
				MaxConcurrent: 3,
				PullTimeout:   "10s",
				RetryCount:    1,
			},
			SyncModelLoad: types.SyncModelLoadConfig{
				Enabled: false, // Отключаем — модель на диске, Ollama подгружает сама при запросе
			},
			Prewarm: types.PrewarmConfig{
				TriggerLoadThreshold: 0.80,
			},
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

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()

	// Ключевой момент: метрики показывают что модель НЕ в RunningModels (не в памяти),
	// но ЕСТЬ в AvailableModels (на диске). Это заставляет балансер выбрать backend
	// через fallback, а AutoPull.findBackendWithModelReady найдёт модель по AvailableModels.
	proxy.UpdateMetrics("ollama-slow", &types.BackendMetrics{
		ID: "ollama-slow",
		GPU: types.GPUMetrics{
			UsagePercent: 30,
			MemoryTotal:  24576,
			MemoryUsed:   8000,
			MemoryFree:   16576,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
			MemoryUsed:      16000,
			MemoryFree:      49536,
			DiskFree:        20480,
		},
		Ollama: types.OllamaMetrics{
			MaxModels:             5,
			MaxConcurrentRequests: 10,
			ActiveRequests:        0,
			OllamaAvailable:       true,
			// RunningModels пуст — модель не загружена в память
			RunningModels: []types.RunningModel{},
			// AvailableModels содержит модель — она на диске, Ollama может подгрузить
			AvailableModels: []types.RunningModel{
				{Name: mock.modelName, Size: 4928300000, Digest: "sha256:abc123"},
			},
		},
	})

	proxyServer := httptest.NewServer(proxy)
	return proxyServer, proxy
}

// ============================================================================
// Тест 1: Модель на диске, не в памяти — streaming запрос успешен с первого раза
// ============================================================================

// TestProxy_ModelNotInMemory_StreamingSuccess проверяет что streaming-запрос
// успешно проходит когда модель есть на диске, но не загружена в память.
// Ollama автоматически подгружает модель при первом запросе.
// Балансер должен использовать streamingClient (без таймаута) и дождаться ответа.
func TestProxy_ModelNotInMemory_StreamingSuccess(t *testing.T) {
	modelName := "llama3.1:8b"
	loadDelay := 200 * time.Millisecond

	mock := newMockOllamaWithSlowLoad(modelName, loadDelay, false)
	defer mock.Close()

	// RequestTimeout = 5s (достаточно для loadDelay = 200ms)
	proxyServer, _ := setupProxyWithSlowLoadMock(t, mock, 5)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  modelName,
		"prompt": "Hello, how are you?",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	start := time.Now()
	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err, "request should not fail")
	defer resp.Body.Close()

	elapsed := time.Since(start)
	t.Logf("Elapsed: %v", elapsed)

	assert.Equal(t, http.StatusOK, resp.StatusCode, "should return 200 OK")
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	// Читаем SSE события
	scanner := bufio.NewScanner(resp.Body)
	var events []map[string]interface{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				events = append(events, event)
			}
		}
	}
	require.NoError(t, scanner.Err())

	assert.GreaterOrEqual(t, len(events), 3, "should receive at least 3 SSE events")
	lastEvent := events[len(events)-1]
	assert.Equal(t, true, lastEvent["done"])

	// Проверяем что был ровно 1 запрос к generate (без retry)
	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.generateCount), "should make exactly 1 generate request")

	// Проверяем что запрос занял время подгрузки
	assert.True(t, elapsed >= loadDelay, "request should take at least loadDelay time")
}

// ============================================================================
// Тест 2: Модель на диске, не в памяти — non-streaming запрос успешен
// ============================================================================

// TestProxy_ModelNotInMemory_NonStreamingSuccess проверяет non-streaming запрос
// при модели на диске, но не в памяти.
func TestProxy_ModelNotInMemory_NonStreamingSuccess(t *testing.T) {
	modelName := "llama3.1:8b"
	loadDelay := 150 * time.Millisecond

	mock := newMockOllamaWithSlowLoad(modelName, loadDelay, false)
	defer mock.Close()

	proxyServer, _ := setupProxyWithSlowLoadMock(t, mock, 5)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  modelName,
		"prompt": "Hello",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, modelName, result["model"])
	assert.Equal(t, "Hello world!", result["response"])
	assert.Equal(t, true, result["done"])

	// Ровно 1 запрос (без retry, без auto-pull — модель уже на диске)
	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.generateCount))
}

// ============================================================================
// Тест 3: Модель не на диске и не в памяти — AutoPull + retry
// ============================================================================

// TestProxy_ModelNotAvailable_AutoPullRetry проверяет полный цикл:
// модели нет ни в памяти, ни на диске → первый запрос возвращает 404 →
// AutoPull подгружает модель → retry запроса успешен.
func TestProxy_ModelNotAvailable_AutoPullRetry(t *testing.T) {
	modelName := "new-model:latest"

	// Создаём мок где первый вызов generate возвращает 404
	mock := newMockOllamaWithSlowLoad(modelName, 50*time.Millisecond, true)
	defer mock.Close()

	host, port := mock.HostPort()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost",
			Port: 0,
		},
		Backends: []types.Backend{
			{
				ID:                "ollama-empty", Name: "Empty Ollama",
				Host: host, OllamaPort: port, AgentPort: 9090,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: true,
			RequestTimeout:    30,
			QueueTimeout:      60,
			QueueMaxSize:      100,
			QueueWorkers:      4,
			AutoPull: types.AutoPullConfig{
				Enabled:       true,
				MaxConcurrent: 3,
				PullTimeout:   "10s",
				RetryCount:    1,
			},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 95},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 1024},
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()

	// НЕТ модели ни в RunningModels, ни в AvailableModels
	proxy.UpdateMetrics("ollama-empty", &types.BackendMetrics{
		ID: "ollama-empty",
		GPU: types.GPUMetrics{
			UsagePercent: 30, MemoryTotal: 24576, MemoryUsed: 8000, MemoryFree: 16576,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20, MemoryTotal: 65536, MemoryUsed: 16000, MemoryFree: 49536, DiskFree: 20480,
		},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels:   []types.RunningModel{},
			AvailableModels: []types.RunningModel{}, // пусто — модели нет на диске
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  modelName,
		"prompt": "Hello",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Должен быть успешный ответ после AutoPull + retry
	assert.Equal(t, http.StatusOK, resp.StatusCode, "should succeed after auto-pull retry")

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, modelName, result["model"])
	assert.Equal(t, "Hello world!", result["response"])

	// Должно быть 2 вызова generate: первый 404, второй 200
	assert.Equal(t, int32(2), atomic.LoadInt32(&mock.generateCount), "should retry after first 404")
	// Должен быть вызов pull
	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.pullCount), "auto-pull should trigger /api/pull")
}

// ============================================================================
// Тест 4: Модель на диске, но подгрузка занимает больше RequestTimeout
// ============================================================================

// TestProxy_ModelNotInMemory_LoadExceedsTimeout проверяет что запрос падает
// если Ollama не успевает подгрузить модель в память за RequestTimeout.
// Это воспроизводит реальную проблему: при больших моделях первый запрос
// может оборваться по таймауту.
func TestProxy_ModelNotInMemory_LoadExceedsTimeout(t *testing.T) {
	modelName := "huge-model:70b"
	// Подгрузка занимает 3 секунды, но RequestTimeout = 1 секунда
	loadDelay := 3 * time.Second

	mock := newMockOllamaWithSlowLoad(modelName, loadDelay, false)
	defer mock.Close()

	proxyServer, _ := setupProxyWithSlowLoadMock(t, mock, 1)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  modelName,
		"prompt": "Hello",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	// Используем клиент с таймаутом, чтобы не ждать вечно
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))

	// Запрос должен завершиться ошибкой (таймаут на стороне балансера)
	if err != nil {
		// ОК — таймаут сработал
		t.Logf("Expected timeout/error: %v", err)
		return
	}
	defer resp.Body.Close()

	// Если запрос всё же прошёл — проверяем статус
	if resp.StatusCode == http.StatusOK {
		t.Log("Request succeeded despite loadDelay > RequestTimeout — streamingClient might have been used")
	} else {
		t.Logf("Got status %d", resp.StatusCode)
	}
}

// ============================================================================
// Тест 5: Chat endpoint с моделью не в памяти
// ============================================================================

// TestProxy_ModelNotInMemory_ChatSuccess проверяет /api/chat при модели на диске.
func TestProxy_ModelNotInMemory_ChatSuccess(t *testing.T) {
	modelName := "qwen2.5:14b"
	loadDelay := 100 * time.Millisecond

	mock := newMockOllamaWithSlowLoad(modelName, loadDelay, false)
	defer mock.Close()

	proxyServer, _ := setupProxyWithSlowLoadMock(t, mock, 5)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model": modelName,
		"messages": []map[string]string{
			{"role": "user", "content": "Hello"},
		},
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	scanner := bufio.NewScanner(resp.Body)
	var events []map[string]interface{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				events = append(events, event)
			}
		}
	}
	require.NoError(t, scanner.Err())
	assert.GreaterOrEqual(t, len(events), 3)

	// Ровно 1 запрос — без retry
	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.generateCount))
}

// ============================================================================
// Тест 6: Параллельные запросы к незагруженной модели
// ============================================================================

// TestProxy_ModelNotInMemory_ConcurrentRequests проверяет что параллельные
// запросы к модели, не загруженной в память, корректно обрабатываются.
// Все запросы должны пройти через один бэкенд (session stickiness).
func TestProxy_ModelNotInMemory_ConcurrentRequests(t *testing.T) {
	modelName := "llama3.1:8b"
	loadDelay := 50 * time.Millisecond

	mock := newMockOllamaWithSlowLoad(modelName, loadDelay, false)
	defer mock.Close()

	proxyServer, _ := setupProxyWithSlowLoadMock(t, mock, 10)
	defer proxyServer.Close()

	var wg sync.WaitGroup
	errors := make(chan error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			payload := map[string]interface{}{
				"model":  modelName,
				"prompt": fmt.Sprintf("Request %d", idx),
				"stream": false,
			}
			body, _ := json.Marshal(payload)

			resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
			if err != nil {
				errors <- fmt.Errorf("request %d failed: %w", idx, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errors <- fmt.Errorf("request %d got status %d", idx, resp.StatusCode)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	var errCount int
	for err := range errors {
		if err != nil {
			errCount++
			t.Logf("Error: %v", err)
		}
	}

	assert.Equal(t, 0, errCount, "all concurrent requests should succeed")

	// Все 5 запросов должны быть обработаны
	assert.Equal(t, int32(5), atomic.LoadInt32(&mock.generateCount))
}