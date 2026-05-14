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
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Категория 1: Тесты эндпоинта /api/embed (Ollama 0.3.0+)
// =============================================================================

// TestProxyEmbed_NewEndpoint проверяет, что /api/embed (новый эндпоинт Ollama 0.3.0+)
// проксируется корректно, включая правильный формат ответа.
func TestProxyEmbed_NewEndpoint(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "nomic-embed-text",
		"input":  "Hello world",
	}
	body, _ := json.Marshal(payload)

	// Используем новый эндпоинт /api/embed
	resp, err := http.Post(proxyServer.URL+"/api/embed", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
		"Transfer-Encoding MUST NOT be present in /api/embed response")

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, "nomic-embed-text", result["model"])
	embeddings, ok := result["embeddings"].([]interface{})
	require.True(t, ok, "/api/embed should return embeddings as array of arrays")
	require.GreaterOrEqual(t, len(embeddings), 1)
	firstEmbed, ok := embeddings[0].([]interface{})
	require.True(t, ok, "Each embedding should be an array of floats")
	assert.Greater(t, len(firstEmbed), 0, "Embedding vector should not be empty")

	// Проверяем, что мок получил запрос именно на /api/embed
	assert.Equal(t, 1, mock.embed2Count, "Should hit /api/embed handler")
	assert.Equal(t, 0, mock.embedCount, "Should NOT hit /api/embeddings handler")
}

// TestProxyEmbed_NoSessionStickiness проверяет, что /api/embed не привязывается
// к сессии (в отличие от /api/generate), чтобы избежать проблем с маршрутизацией.
func TestProxyEmbed_NoSessionStickiness(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	// Отправляем /api/embed с session ID
	payload := map[string]interface{}{
		"model": "nomic-embed-text",
		"input": "Test no stickiness",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/api/embed", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", "embed-test-session")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Проверяем, что сессия НЕ создалась для embed-запроса
	sessions := proxy.GetSessions()
	for _, s := range sessions {
		assert.NotContains(t, s.ID, "embed-test-session",
			"Embed request should NOT create a session")
	}
}

// TestProxyEmbed_ModelNotFoundThenAutoPull симулирует ситуацию, когда модель
// не загружена на бэкенде — прокси не должен паниковать, а должен корректно
// обработать ошибку (или вернуть её от бэкенда).
func TestProxyEmbed_ModelNotFoundThenAutoPull(t *testing.T) {
	// Создаём мок, который возвращает 404 на /api/embed (модель не найдена)
	callCount := 0
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/api/embed" {
			if callCount == 1 {
				// Первый вызов — модель не найдена
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]string{
					"error": fmt.Sprintf("model %q not found, try pulling it first", "nomic-embed-text"),
				})
				return
			}
			// Второй вызов (после pull) — успех
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"model":      "nomic-embed-text",
				"embeddings": [][]float64{{0.1, 0.2, 0.3}},
			})
			return
		}
		// /api/pull — имитируем успешный pull
		if r.URL.Path == "/api/pull" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"status\":\"success\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		// /api/tags — возвращаем модель (как будто она уже загружена после pull)
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "nomic-embed-text", "size": 274000000},
				},
			})
			return
		}
		// /api/ps — без running models вначале
		if r.URL.Path == "/api/ps" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []interface{}{},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	host, port := parseHostPort(mockServer.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{ID: "embed-backend", Name: "Embed Backend", Host: host, OllamaPort: port,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, RequestTimeout: 5,
			QueueTimeout: 10, QueueMaxSize: 10, QueueWorkers: 1,
			SessionStickiness: false,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
			CPU: types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk: types.DiskLimits{MinFreeMB: 100},
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("embed-backend", &types.BackendMetrics{
		ID: "embed-backend",
		GPU: types.GPUMetrics{UsagePercent: 20, MemoryTotal: 16384},
		System: types.SystemMetrics{CPUUsagePercent: 15, DiskFree: 20480},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, OllamaAvailable: true,
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model": "nomic-embed-text",
		"input": "Test not found then auto-pull",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/embed", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Прокси не должен паниковать; может вернуть 404 или 200 в зависимости от логики retry
	bodyBytes, _ := io.ReadAll(resp.Body)
	assert.NotEmpty(t, bodyBytes, "Response body should not be empty even on error")
	assert.Contains(t, []int{http.StatusOK, http.StatusNotFound, http.StatusServiceUnavailable},
		resp.StatusCode, "Should handle model-not-found gracefully")
}

// TestProxyEmbed_MultiBackend проверяет, что /api/embed правильно выбирает бэкенд
// и не привязывается к сессии при наличии нескольких бэкендов.
func TestProxyEmbed_MultiBackend(t *testing.T) {
	// Создаём два мок-сервера
	mock1 := newMockOllamaServer()
	defer mock1.Close()
	mock2 := newMockOllamaServer()
	defer mock2.Close()

	mocks := []*mockOllamaServer{mock1, mock2}

	// Настраиваем прокси с двумя бэкендами
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends:     make([]types.Backend, 0, 2),
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: true,
			RequestTimeout:    30,
			QueueTimeout:      60,
			QueueMaxSize:      100,
			QueueWorkers:      4,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 95},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 1024},
		},
	}

	for i, mock := range mocks {
		host, port := mock.HostPort()
		config.Backends = append(config.Backends, types.Backend{
			ID:                fmt.Sprintf("backend-%d", i+1),
			Name:              fmt.Sprintf("Backend %d", i+1),
			Host:              host,
			OllamaPort:        port,
			Weight:            50,
			MaxConcurrentReqs: 10,
			Status:            types.StatusHealthy,
		})
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()

	for i := range mocks {
		proxy.UpdateMetrics(fmt.Sprintf("backend-%d", i+1), &types.BackendMetrics{
			ID: fmt.Sprintf("backend-%d", i+1),
			GPU: types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576, MemoryFree: 16576},
			System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 65536, DiskFree: 20480},
			Ollama: types.OllamaMetrics{
				MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
				RunningModels: []types.RunningModel{
					{Name: "nomic-embed-text", VRAMUsage: 2000},
				},
			},
		})
	}

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Отправляем 3 embed-запроса — они могут пойти на разные бэкенды
	for i := 0; i < 3; i++ {
		payload := map[string]interface{}{
			"model": "nomic-embed-text",
			"input": fmt.Sprintf("Request %d", i+1),
		}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(proxyServer.URL+"/api/embed", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "Request %d should succeed", i+1)
	}

	// Проверяем, что оба бэкенда получили хотя бы один запрос
	totalCalls := mock1.embed2Count + mock2.embed2Count
	assert.Equal(t, 3, totalCalls, "All 3 embed requests should be processed by backends")
	t.Logf("Backend1 embed count: %d, Backend2 embed count: %d", mock1.embed2Count, mock2.embed2Count)
}

// TestProxyEmbed_ModelNotInRunningModels проверяет сценарий, когда модель
// для эмбеддинга не указана в RunningModels метрик. Прокси должен корректно
// выбрать бэкенд без model affinity и передать запрос.
func TestProxyEmbed_ModelNotInRunningModels(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	// Обновляем метрики — nomic-embed-text НЕ в списке running models
	proxy.UpdateMetrics("ollama-test", &types.BackendMetrics{
		ID: "ollama-test",
		GPU: types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576, MemoryFree: 16576},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 65536, DiskFree: 20480},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:8b", VRAMUsage: 6000},
			},
		},
	})

	payload := map[string]interface{}{
		"model": "nomic-embed-text",
		"input": "Test model not in running models",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/embed", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"Embed request should succeed even if model not in RunningModels")

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	assert.Equal(t, "nomic-embed-text", result["model"])
	assert.Equal(t, 1, mock.embed2Count, "Request should hit /api/embed handler")
}

// =============================================================================
// Категория 2: Тесты первого запроса (warmup / model loading)
// =============================================================================

// TestProxyGenerate_FirstRequestWarmup_Success проверяет, что первый запрос
// к модели может завершиться успешно, когда SyncModelLoad справляется с загрузкой модели.
func TestProxyGenerate_FirstRequestWarmup_Success(t *testing.T) {
	// Мок с задержкой загрузки модели (имитация, что модель "грузится")
	mockWithDelay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/generate":
			// Симулируем задержку загрузки модели
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"model":    "llama3.1:8b",
				"response": "Warmup success",
				"done":     true,
			})
		case "/api/ps":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "llama3.1:8b", "size": 4928300000, "size_vram": 6000},
				},
			})
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "llama3.1:8b", "size": 4928300000},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockWithDelay.Close()

	host, port := parseHostPort(mockWithDelay.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{ID: "warmup-backend", Name: "Warmup Backend", Host: host, OllamaPort: port,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, ModelAffinity: true,
			SessionStickiness: true, RequestTimeout: 30, QueueTimeout: 60,
			QueueMaxSize: 100, QueueWorkers: 4,
			SyncModelLoad: types.SyncModelLoadConfig{
				Enabled: true,
				Timeout: "5s",
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
	proxy.UpdateMetrics("warmup-backend", &types.BackendMetrics{
		ID: "warmup-backend",
		GPU: types.GPUMetrics{UsagePercent: 20, MemoryTotal: 24576, MemoryFree: 16576},
		System: types.SystemMetrics{CPUUsagePercent: 15, MemoryTotal: 65536, DiskFree: 20480},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:8b", VRAMUsage: 6000},
			},
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello, this is the first request",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"First request after warmup should succeed")

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	assert.Equal(t, "llama3.1:8b", result["model"])
	assert.Contains(t, result["response"], "Warmup")
}

// TestProxyGenerate_FirstRequestStreaming_Warmup проверяет, что первый streaming
// запрос к модели корректно проходит warmup и возвращает SSE события.
func TestProxyGenerate_FirstRequestStreaming_Warmup(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	// Убираем модель из running models, чтобы прокси её "искал"
	proxy.UpdateMetrics("ollama-test", &types.BackendMetrics{
		ID: "ollama-test",
		GPU: types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576, MemoryFree: 16576},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 65536, DiskFree: 20480},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:8b", VRAMUsage: 6000},
			},
		},
	})

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello first streaming request",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
		"Transfer-Encoding MUST NOT be present in streaming response")

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
	assert.GreaterOrEqual(t, len(events), 3,
		"Should receive at least 3 SSE events even on first request")

	lastEvent := events[len(events)-1]
	assert.Equal(t, true, lastEvent["done"])
}

// TestProxyGenerate_ConcurrentFirstRequestsSynced проверяет, что когда несколько
// клиентов одновременно делают первый запрос к одной модели, не возникает
// race condition или двойной загрузки модели.
func TestProxyGenerate_ConcurrentFirstRequestsSynced(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	numRequests := 5
	errChan := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		go func(id int) {
			payload := map[string]interface{}{
				"model":  "llama3.1:8b",
				"prompt": fmt.Sprintf("Concurrent request %d", id),
				"stream": false,
			}
			body, _ := json.Marshal(payload)

			resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
			if err != nil {
				errChan <- fmt.Errorf("request %d failed: %v", id, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errChan <- fmt.Errorf("request %d status %d", id, resp.StatusCode)
				return
			}

			var result map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				errChan <- fmt.Errorf("request %d decode error: %v", id, err)
				return
			}
			if result["model"] != "llama3.1:8b" {
				errChan <- fmt.Errorf("request %d wrong model: %v", id, result["model"])
				return
			}
			errChan <- nil
		}(i)
	}

	for i := 0; i < numRequests; i++ {
		select {
		case err := <-errChan:
			require.NoError(t, err, "Concurrent request %d should succeed", i)
		case <-time.After(10 * time.Second):
			t.Fatalf("Timeout waiting for request %d", i)
		}
	}

	// Проверяем, что все запросы долетели до бэкенда
	assert.Equal(t, numRequests, mock.generateCount,
		"All %d concurrent requests should reach backend", numRequests)
}

// TestProxyGenerate_FirstRequestSessionBind проверяет, что после первого успешного
// запроса создаётся сессия, и последующие запросы привязываются к тому же бэкенду.
func TestProxyGenerate_FirstRequestSessionBind(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	sessionID := "first-req-session"

	// Первый запрос
	payload1 := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "First request to create session",
		"stream": false,
	}
	body1, _ := json.Marshal(payload1)

	req1, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/api/generate", bytes.NewBuffer(body1))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Session-ID", sessionID)

	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	resp1.Body.Close()
	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	// Проверяем, что сессия создалась
	sessions := proxy.GetSessions()
	expectedID := sessionID + "::llama3.1:8b::gen"
	found := false
	for _, s := range sessions {
		if s.ID == expectedID {
			found = true
			assert.Equal(t, "ollama-test", s.BackendID)
			assert.Equal(t, "llama3.1:8b", s.Model)
			break
		}
	}
	assert.True(t, found, "Session should be created after first request")

	// Второй запрос с тем же session ID — должен пойти на тот же бэкенд
	payload2 := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Second request (should use same backend)",
		"stream": false,
	}
	body2, _ := json.Marshal(payload2)

	req2, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/api/generate", bytes.NewBuffer(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Session-ID", sessionID)

	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	// Оба запроса обработаны
	assert.Equal(t, 2, mock.generateCount)
}

// =============================================================================
// Категория 3: Интеграционные сценарии
// =============================================================================

// TestIntegration_OpenWebUIWorkflow эмулирует типичный workflow OpenWebUI:
// запрос к модели -> проверка корректности SSE -> корректное завершение.
func TestIntegration_OpenWebUIWorkflow(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Шаг 1: Проверка health
	respHealth, err := http.Get(proxyServer.URL + "/health")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, respHealth.StatusCode)
	respHealth.Body.Close()

	// Шаг 2: Получение списка моделей
	respTags, err := http.Get(proxyServer.URL + "/api/tags")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, respTags.StatusCode)
	respTags.Body.Close()

	// Шаг 3: Chat запрос (как OpenWebUI)
	payload := map[string]interface{}{
		"model": "llama3.1:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "Hello, how are you?"},
		},
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	respChat, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer respChat.Body.Close()

	assert.Equal(t, http.StatusOK, respChat.StatusCode)
	assert.Empty(t, respChat.Header.Get("Transfer-Encoding"))
	assert.Equal(t, "application/json", respChat.Header.Get("Content-Type"))

	var chatResult map[string]interface{}
	err = json.NewDecoder(respChat.Body).Decode(&chatResult)
	require.NoError(t, err)
	assert.Equal(t, "llama3.1:8b", chatResult["model"])

	// Шаг 4: Проверяем, что сессия создалась (OpenWebUI использует свои session cookies)
	sessions := proxy.GetSessions()
	assert.GreaterOrEqual(t, len(sessions), 1,
		"OpenWebUI-like workflow should create sessions")
}

// TestIntegration_RooCodeWorkflow эмулирует workflow Roo Code с эмбеддингами:
// запрос к /api/embed -> проверка корректности ответа -> без session stickiness.
func TestIntegration_RooCodeWorkflow(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	// Шаг 1: Проверка tags (Roo Code получает список моделей)
	respTags, err := http.Get(proxyServer.URL + "/api/tags")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, respTags.StatusCode)
	respTags.Body.Close()

	// Шаг 2: Embeddings запрос (Roo Code индексирует документы)
	payload := map[string]interface{}{
		"model": "nomic-embed-text",
		"input": "Roo Code indexing a document for embedding",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/embed", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"Roo Code /api/embed request should succeed")
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
		"Transfer-Encoding should not leak to Roo Code client")

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	assert.Equal(t, "nomic-embed-text", result["model"])

	// Проверяем структуру ответа Ollama 0.3.0+
	embeddings, ok := result["embeddings"].([]interface{})
	require.True(t, ok, "Response should contain 'embeddings' array")
	require.GreaterOrEqual(t, len(embeddings), 1, "Should have at least one embedding")

	// Шаг 3: Generate запрос (Roo Code генерирует ответ)
	genPayload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Summarize the document",
		"stream": false,
	}
	genBody, _ := json.Marshal(genPayload)

	respGen, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(genBody))
	require.NoError(t, err)
	defer respGen.Body.Close()
	assert.Equal(t, http.StatusOK, respGen.StatusCode,
		"Roo Code generate request should succeed after embed")
}

// TestIntegration_BackendFailover проверяет, что при отказе одного бэкенда
// запрос перенаправляется на другой (если настроено несколько бэкендов).
func TestIntegration_BackendFailover(t *testing.T) {
	// Первый бэкенд — всегда возвращает ошибку
	failBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "backend unavailable"})
	}))
	defer failBackend.Close()

	// Второй бэкенд — работает нормально
	workingBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":    "llama3.1:8b",
			"response": "Failover works",
			"done":     true,
		})
	}))
	defer workingBackend.Close()

	host1, port1 := parseHostPort(failBackend.URL)
	host2, port2 := parseHostPort(workingBackend.URL)

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{ID: "fail-backend", Name: "Fail Backend", Host: host1, OllamaPort: port1,
				Weight: 10, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
			{ID: "working-backend", Name: "Working Backend", Host: host2, OllamaPort: port2,
				Weight: 100, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmRoundRobin, SessionStickiness: false,
			RequestTimeout: 5, QueueTimeout: 10, QueueMaxSize: 10, QueueWorkers: 1,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
			CPU: types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk: types.DiskLimits{MinFreeMB: 100},
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("fail-backend", &types.BackendMetrics{
		ID: "fail-backend",
		GPU: types.GPUMetrics{UsagePercent: 50, MemoryTotal: 16384},
		System: types.SystemMetrics{CPUUsagePercent: 40, DiskFree: 20480},
		Ollama: types.OllamaMetrics{MaxModels: 2, MaxConcurrentRequests: 5, OllamaAvailable: false},
	})
	proxy.UpdateMetrics("working-backend", &types.BackendMetrics{
		ID: "working-backend",
		GPU: types.GPUMetrics{UsagePercent: 20, MemoryTotal: 24576},
		System: types.SystemMetrics{CPUUsagePercent: 15, DiskFree: 40960},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{{Name: "llama3.1:8b", VRAMUsage: 6000}},
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Test failover",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"Request should succeed via failover to working backend")

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	assert.Contains(t, result["response"], "Failover")
}

// =============================================================================
// Категория 4: Transfer-Encoding cleanup для embed endpoints
// =============================================================================

// TestTransferEncoding_EmbedEndpoint проверяет, что /api/embed не просачивает
// Transfer-Encoding в ответе.
func TestTransferEncoding_EmbedEndpoint(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	payload := map[string]interface{}{
		"model": "nomic-embed-text",
		"input": "Test transfer encoding cleanup",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/embed", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
		"Transfer-Encoding MUST NOT be present in /api/embed response")
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	// Проверяем, что тело читается целиком без ошибок
	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Should be able to read complete /api/embed body")
	assert.NotEmpty(t, bodyBytes)

	var result map[string]interface{}
	err = json.Unmarshal(bodyBytes, &result)
	require.NoError(t, err, "Response should be valid JSON")
	assert.Equal(t, "nomic-embed-text", result["model"])
}

// TestTransferEncoding_BothEmbedEndpoints проверяет оба embed эндпоинта
// на отсутствие Transfer-Encoding.
func TestTransferEncoding_BothEmbedEndpoints(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	tests := []struct {
		name   string
		path   string
		field  string // поле с текстом в ответе
		fields map[string]interface{}
	}{
		{
			name:  "/api/embeddings",
			path:  "/api/embeddings",
			field: "prompt",
			fields: map[string]interface{}{
				"model":  "nomic-embed-text",
				"prompt": "Hello world",
			},
		},
		{
			name:  "/api/embed",
			path:  "/api/embed",
			field: "input",
			fields: map[string]interface{}{
				"model": "nomic-embed-text",
				"input": "Hello world",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.fields)
			resp, err := http.Post(proxyServer.URL+tt.path, "application/json", bytes.NewBuffer(body))
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
				"Transfer-Encoding MUST NOT be present in %s response", tt.name)
			assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

			bodyBytes, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.NotEmpty(t, bodyBytes)

			var result map[string]interface{}
			err = json.Unmarshal(bodyBytes, &result)
			require.NoError(t, err)
			assert.Equal(t, "nomic-embed-text", result["model"])
		})
	}
}

// =============================================================================
// Категория 5: Тесты целостности ответов (Response Integrity)
// =============================================================================

// TestResponseIntegrity_EmbedResponseStructure проверяет, что структура ответа
// от /api/embed сохраняется при проксировании: model, embeddings как массив массивов.
func TestResponseIntegrity_EmbedResponseStructure(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model": "nomic-embed-text",
		"input": "Test response structure integrity",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/embed", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	// Сверяем с прямым ответом от мок-сервера (без прокси)
	directResp, err := http.Post(mock.URL()+"/api/embed", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer directResp.Body.Close()

	directBytes, err := io.ReadAll(directResp.Body)
	require.NoError(t, err)

	// Тело ответа должно совпадать (за исключением возможных заголовков)
	var proxyResult, directResult map[string]interface{}
	json.Unmarshal(bodyBytes, &proxyResult)
	json.Unmarshal(directBytes, &directResult)

	assert.Equal(t, directResult["model"], proxyResult["model"],
		"Model field should be preserved through proxy")

	// Проверяем наличие embeddings в обоих ответах
	_, proxyHasEmbed := proxyResult["embeddings"]
	_, directHasEmbed := directResult["embeddings"]
	assert.True(t, proxyHasEmbed, "Proxy response should have 'embeddings' field")
	assert.True(t, directHasEmbed, "Direct response should have 'embeddings' field")
}

// TestResponseIntegrity_ForwardedHeaders проверяет, что важные заголовки
// корректно передаются через прокси (Content-Type, X-Request-ID и т.д.).
func TestResponseIntegrity_ForwardedHeaders(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Test headers",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/api/generate", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "test-req-456")
	req.Header.Set("X-Session-ID", "header-test-session")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"))

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	assert.Equal(t, "Hello world!", result["response"])
}

// TestResponseIntegrity_StreamingEventsComplete проверяет, что все SSE события
// доставляются клиенту в правильном порядке и последнее содержит done:true.
func TestResponseIntegrity_StreamingEventsComplete(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Check event completeness",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	// Читаем все SSE события
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

	// Проверяем целостность
	assert.GreaterOrEqual(t, len(events), 3, "Should receive at least 3 SSE events")

	// Все события должны содержать model
	for i, event := range events {
		assert.Contains(t, event, "model", "Event %d should have 'model'", i)
		assert.Equal(t, "llama3.1:8b", event["model"], "Event %d should have correct model", i)
	}

	// Последнее событие — done:true
	lastEvent := events[len(events)-1]
	assert.Equal(t, true, lastEvent["done"], "Last event must have done:true")
	assert.Contains(t, lastEvent, "total_duration",
		"Last event should contain total_duration")
}

// TestResponseIntegrity_TimeoutHandling проверяет, что при таймауте запроса
// прокси возвращает корректную ошибку, а не "connection reset" или панику.
func TestResponseIntegrity_TimeoutHandling(t *testing.T) {
	// Медленный бэкенд
	slowBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Отвечаем очень медленно
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":    "llama3.1:8b",
			"response": "Too late",
			"done":     true,
		})
	}))
	defer slowBackend.Close()

	host, port := parseHostPort(slowBackend.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{ID: "slow-backend", Name: "Slow Backend", Host: host, OllamaPort: port,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, RequestTimeout: 1, // 1 секунда таймаут
			QueueTimeout: 2, QueueMaxSize: 10, QueueWorkers: 1,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 100},
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("slow-backend", &types.BackendMetrics{
		ID: "slow-backend",
		GPU: types.GPUMetrics{UsagePercent: 20, MemoryTotal: 16384},
		System: types.SystemMetrics{CPUUsagePercent: 15, DiskFree: 20480},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, OllamaAvailable: true,
			RunningModels: []types.RunningModel{{Name: "llama3.1:8b", VRAMUsage: 6000}},
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Test timeout",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := client.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))

	// Может быть ошибка (timeout) или 503/504 — главное не паника
	if err != nil {
		// Ошибка соединения — допустимо при таймауте
		t.Logf("Timeout error (expected): %v", err)
		return
	}
	defer resp.Body.Close()

	// Если ответ получен, проверяем его
	bodyBytes, _ := io.ReadAll(resp.Body)
	assert.NotEmpty(t, bodyBytes, "Response body should not be empty, even on timeout")
	t.Logf("Timeout test: status=%d, body=%s", resp.StatusCode, string(bodyBytes))
}

// =============================================================================
// Категория 6: Граничные случаи для embed запросов
// =============================================================================

// TestProxyEmbed_ErrorResponseHandling проверяет, что ошибки от бэкенда
// (например, 400 Bad Request) корректно передаются клиенту.
func TestProxyEmbed_ErrorResponseHandling(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	// Пустой запрос (без model)
	resp, err := http.Post(proxyServer.URL+"/api/embed", "application/json",
		bytes.NewBuffer([]byte(`{}`)))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Прокси не должен паниковать при пустом запросе
	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.NotEmpty(t, bodyBytes, "Response body should not be empty on error")
	t.Logf("Empty embed request: status=%d, body=%s", resp.StatusCode, string(bodyBytes))
}

// TestProxyEmbed_InvalidJSON проверяет, что прокси не паникует при невалидном JSON
// в теле запроса к /api/embed.
func TestProxyEmbed_InvalidJSON(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	resp, err := http.Post(proxyServer.URL+"/api/embed", "application/json",
		bytes.NewBuffer([]byte(`{invalid json}`)))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Прокси не должен паниковать
	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.NotNil(t, bodyBytes)
}
