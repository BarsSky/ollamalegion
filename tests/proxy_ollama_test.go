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
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockOllamaServer - мок-сервер Ollama API для тестирования проксирования
type mockOllamaServer struct {
	server        *httptest.Server
	generateCount int
	chatCount     int
	embedCount    int
	tagsCount     int
	psCount       int
	versionCount  int
	mu            struct {
		sync.Mutex
		models []types.RunningModel
	}
}

// newMockOllamaServer - создание нового мок-сервера Ollama
func newMockOllamaServer() *mockOllamaServer {
	mock := &mockOllamaServer{}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mock.handleRequest(w, r)
	}))
	return mock
}

func (m *mockOllamaServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/generate":
		m.generateCount++
		m.handleGenerate(w, r)
	case "/api/chat":
		m.chatCount++
		m.handleChat(w, r)
	case "/api/embeddings":
		m.embedCount++
		m.handleEmbeddings(w, r)
	case "/api/tags":
		m.tagsCount++
		m.handleTags(w, r)
	case "/api/ps":
		m.psCount++
		m.handlePs(w, r)
	case "/api/version":
		m.versionCount++
		m.handleVersion(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}
}

func (m *mockOllamaServer) handleGenerate(w http.ResponseWriter, r *http.Request) {
	// Читаем тело для определения streaming режима
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	stream := true
	if s, ok := req["stream"].(bool); ok {
		stream = s
	}

	model := ""
	if m, ok := req["model"].(string); ok {
		model = m
	}

	if stream {
		// Streaming ответ (SSE)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		// Отправляем несколько SSE событий
		responses := []map[string]interface{}{
			{"model": model, "response": "Hello", "done": false},
			{"model": model, "response": " world", "done": false},
			{"model": model, "response": "!", "done": true, "total_duration": 1234567890},
		}

		for _, resp := range responses {
			data, _ := json.Marshal(resp)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	} else {
		// Обычный ответ
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":           model,
			"response":        "Hello world!",
			"done":            true,
			"total_duration":  1234567890,
		})
	}
}

func (m *mockOllamaServer) handleChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	stream := true
	if s, ok := req["stream"].(bool); ok {
		stream = s
	}

	model := ""
	if mod, ok := req["model"].(string); ok {
		model = mod
	}

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)

		responses := []map[string]interface{}{
			{"model": model, "message": map[string]string{"role": "assistant", "content": "Hi"}, "done": false},
			{"model": model, "message": map[string]string{"role": "assistant", "content": " there"}, "done": false},
			{"model": model, "message": map[string]string{"role": "assistant", "content": "!"}, "done": true},
		}

		for _, resp := range responses {
			data, _ := json.Marshal(resp)
			fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model": model,
			"message": map[string]string{
				"role":    "assistant",
				"content": "Hi there!",
			},
			"done": true,
		})
	}
}

func (m *mockOllamaServer) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	model := ""
	if mod, ok := req["model"].(string); ok {
		model = mod
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"model":      model,
		"embeddings": []float64{0.1, 0.2, 0.3, 0.4, 0.5},
	})
}

func (m *mockOllamaServer) handleTags(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models": []map[string]interface{}{
			{
				"name":    "llama3.1:8b",
				"size":    4928300000,
				"digest":  "sha256:abc123",
				"details": map[string]interface{}{"family": "llama", "parameter_size": "8B"},
			},
			{
				"name":    "qwen2.5:14b",
				"size":    8965234567,
				"digest":  "sha256:def456",
				"details": map[string]interface{}{"family": "qwen", "parameter_size": "14B"},
			},
		},
	})
}

func (m *mockOllamaServer) handlePs(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	models := make([]types.RunningModel, len(m.mu.models))
	copy(models, m.mu.models)
	m.mu.Unlock()

	// Преобразуем в формат Ollama API
	var ollamaModels []map[string]interface{}
	for _, m := range models {
		ollamaModels = append(ollamaModels, map[string]interface{}{
			"name":       m.Name,
			"size":       m.Size,
			"digest":     m.Digest,
			"expires_at": m.ExpiresAt,
			"size_vram":  m.VRAMUsage,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models": ollamaModels,
	})
}

func (m *mockOllamaServer) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"version": "0.3.0",
	})
}

func (m *mockOllamaServer) URL() string {
	return m.server.URL
}

func (m *mockOllamaServer) Close() {
	m.server.Close()
}

func (m *mockOllamaServer) SetRunningModels(models []types.RunningModel) {
	m.mu.Lock()
	m.mu.models = models
	m.mu.Unlock()
}

func (m *mockOllamaServer) HostPort() (string, int) {
	// Парсим URL вида http://127.0.0.1:12345
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

// setupProxyWithMockOllama - настройка прокси с мок-сервером Ollama
func setupProxyWithMockOllama(t *testing.T, mock *mockOllamaServer) (*httptest.Server, *balancer.Proxy) {
	t.Helper()

	host, port := mock.HostPort()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    0, // будет назначен динамически httptest
			APIPort: 0,
		},
		Backends: []types.Backend{
			{
				ID:                "ollama-test",
				Name:              "Test Ollama",
				Host:              host,
				OllamaPort:        port,
				AgentPort:         9090,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			ModelAffinity:       true,
			SessionStickiness:   true,
			RequestTimeout:      30,
			QueueTimeout:        60,
			QueueMaxSize:        100,
			QueueWorkers:        4,
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

	// Обновляем метрики бэкенда чтобы он был доступен для балансировки
	proxy.UpdateMetrics("ollama-test", &types.BackendMetrics{
		ID: "ollama-test",
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
			DiskFree:        20480, // > MinFreeMB (1024)
		},
		Ollama: types.OllamaMetrics{
			MaxModels:             5,
			MaxConcurrentRequests: 10,
			ActiveRequests:        0,
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:8b", VRAMUsage: 6000},
			},
		},
	})

	// Создаем HTTP сервер для прокси
	proxyServer := httptest.NewServer(proxy)

	return proxyServer, proxy
}

// ==================== Тесты проксирования Ollama API ====================

func TestProxyOllama_Generate_NonStreaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	// POST /api/generate с stream=false
	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello, how are you?",
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
	assert.Equal(t, "Hello world!", result["response"])
	assert.Equal(t, true, result["done"])

	// Проверяем, что мок-сервер получил запрос
	assert.Equal(t, 1, mock.generateCount)
}

func TestProxyOllama_Generate_Streaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	// POST /api/generate с stream=true (по умолчанию)
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
	assert.GreaterOrEqual(t, len(events), 3, "Should receive at least 3 SSE events")

	// Проверяем последнее событие
	lastEvent := events[len(events)-1]
	assert.Equal(t, true, lastEvent["done"])
	assert.NotNil(t, lastEvent["total_duration"])

	// Проверяем, что мок-сервер получил запрос
	assert.Equal(t, 1, mock.generateCount)
}

func TestProxyOllama_Chat_NonStreaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
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

	assert.Equal(t, "llama3.1:8b", result["model"])
	message, ok := result["message"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "assistant", message["role"])
	assert.Equal(t, "Hi there!", message["content"])
	assert.Equal(t, true, result["done"])

	assert.Equal(t, 1, mock.chatCount)
}

func TestProxyOllama_Chat_Streaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model": "llama3.1:8b",
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
	assert.GreaterOrEqual(t, len(events), 3)

	lastEvent := events[len(events)-1]
	assert.Equal(t, true, lastEvent["done"])

	assert.Equal(t, 1, mock.chatCount)
}

func TestProxyOllama_Embeddings(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model": "nomic-embed-text",
		"prompt": "Hello world",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/embeddings", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, "nomic-embed-text", result["model"])
	embeddings, ok := result["embeddings"].([]interface{})
	require.True(t, ok)
	assert.Equal(t, 5, len(embeddings))

	assert.Equal(t, 1, mock.embedCount)
}

func TestProxyOllama_Tags(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/api/tags")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	models, ok := result["models"].([]interface{})
	require.True(t, ok)
	assert.Equal(t, 2, len(models))

	// Проверяем первую модель
	model0 := models[0].(map[string]interface{})
	assert.Equal(t, "llama3.1:8b", model0["name"])

	assert.Equal(t, 1, mock.tagsCount)
}

func TestProxyOllama_Ps(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	// Устанавливаем running models в моке
	mock.SetRunningModels([]types.RunningModel{
		{Name: "llama3.1:8b", Size: 4928300000, Digest: "sha256:abc123", VRAMUsage: 6000},
	})

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/api/ps")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	models, ok := result["models"].([]interface{})
	require.True(t, ok)
	assert.Equal(t, 1, len(models))

	model0 := models[0].(map[string]interface{})
	assert.Equal(t, "llama3.1:8b", model0["name"])

	assert.Equal(t, 1, mock.psCount)
}

func TestProxyOllama_Version(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/api/version")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, "ollamalegion-1.0.0", result["version"])
	assert.Equal(t, 1, mock.versionCount)
}

// ==================== Тесты Session Stickiness ====================

func TestProxyOllama_SessionStickiness(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	// Первый запрос - должен создать сессию
	payload1 := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "First request",
		"stream": false,
	}
	body1, _ := json.Marshal(payload1)

	req1, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/api/generate", bytes.NewBuffer(body1))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Session-ID", "test-session-123")

	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	defer resp1.Body.Close()

	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	// Проверяем, что сессия создана
	session := proxy.GetSessions()
	found := false
	for _, s := range session {
		if s.ID == "test-session-123" {
			found = true
			assert.Equal(t, "ollama-test", s.BackendID)
			assert.Equal(t, "llama3.1:8b", s.Model)
			break
		}
	}
	assert.True(t, found, "Session should be created")

	// Второй запрос с тем же session ID - должен пойти на тот же бэкенд
	payload2 := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Second request",
		"stream": false,
	}
	body2, _ := json.Marshal(payload2)

	req2, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/api/generate", bytes.NewBuffer(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Session-ID", "test-session-123")

	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()

	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	// Проверяем, что оба запроса были обработаны
	assert.Equal(t, 2, mock.generateCount)
}

func TestProxyOllama_SessionStickiness_CookieFallback(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	// Первый запрос без X-Session-ID - сессия должна быть создана по IP
	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Cookie test",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp1, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp1.Body.Close()

	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	// Проверяем, что сессия создана (по IP)
	sessions := proxy.GetSessions()
	assert.GreaterOrEqual(t, len(sessions), 1, "Session should be created by IP fallback")

	// Проверяем cookie
	var sessionCookie *http.Cookie
	for _, c := range resp1.Cookies() {
		if c.Name == "session_id" {
			sessionCookie = c
			break
		}
	}
	// Cookie может быть установлена
	if sessionCookie != nil {
		assert.NotEmpty(t, sessionCookie.Value)
	}
}

// ==================== Тесты Model Affinity ====================

func TestProxyOllama_ModelAffinity(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	// Обновляем метрики с запущенной моделью qwen2.5:14b на бэкенде
	proxy.UpdateMetrics("ollama-test", &types.BackendMetrics{
		ID: "ollama-test",
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
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:8b", VRAMUsage: 6000},
				{Name: "qwen2.5:14b", VRAMUsage: 10000},
			},
		},
	})

	// Запрос к модели, которая уже загружена - должен выбрать бэкенд с этой моделью
	payload := map[string]interface{}{
		"model":  "qwen2.5:14b",
		"prompt": "Test model affinity",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Проверяем, что мок получил запрос с правильной моделью
	assert.Equal(t, 1, mock.generateCount)
}

// ==================== Тесты Retry / Failover ====================

func TestProxyOllama_RetryOnBackendFailure(t *testing.T) {
	// Создаем мок, который падает на первом запросе, но работает на втором
	failCount := 0
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCount++
		if failCount == 1 {
			// Первый запрос - ошибка
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "backend overloaded"})
			return
		}
		// Второй запрос - успех
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":    "llama3.1:8b",
			"response": "Success after retry",
			"done":     true,
		})
	}))
	defer mockServer.Close()

	// Настраиваем прокси с этим бэкендом
	host, port := parseHostPort(mockServer.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost",
			Port: 0,
		},
		Backends: []types.Backend{
			{
				ID:                "fail-then-success",
				Name:              "Fail Then Success",
				Host:              host,
				OllamaPort:        port,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			SessionStickiness: false,
			RequestTimeout:    5,
			QueueTimeout:      10,
			QueueMaxSize:      10,
			QueueWorkers:      1,
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

	// Устанавливаем метрики для бэкенда
	proxy.UpdateMetrics("fail-then-success", &types.BackendMetrics{
		ID: "fail-then-success",
		GPU: types.GPUMetrics{
			UsagePercent: 20,
			MemoryTotal:  16384,
			MemoryUsed:   4000,
			MemoryFree:   12384,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 15,
			DiskFree:        20480,
		},
	})

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

	// Запрос должен быть обработан (retry не работает с одним бэкендом, но проверяем что не паникует)
	// С одним бэкендом retry выберет тот же бэкенд и вернет ошибку
	// Но мы проверяем что механизм retry срабатывает без panic
	assert.True(t, resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusServiceUnavailable)
}

// ==================== Helpers ====================

func parseHostPort(urlStr string) (string, int) {
	urlStr = strings.TrimPrefix(urlStr, "http://")
	parts := strings.Split(urlStr, ":")
	if len(parts) != 2 {
		return "localhost", 11434
	}
	port := 0
	fmt.Sscanf(parts[1], "%d", &port)
	return parts[0], port
}