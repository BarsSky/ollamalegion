package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"testing"
)

// mockOllamaServer - мок-сервер Ollama API для тестирования проксирования
type mockOllamaServer struct {
	server        *httptest.Server
	generateCount int64
	chatCount     int64
	embedCount    int64
	embed2Count   int64
	tagsCount     int64
	psCount       int64
	versionCount  int64
	showCount     int64
	createCount   int64
	pullCount     int64
	deleteCount   int64
	copyCount     int64
	pushCount     int64
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
		atomic.AddInt64(&m.generateCount, 1)
		m.handleGenerate(w, r)
	case "/api/chat":
		atomic.AddInt64(&m.chatCount, 1)
		m.handleChat(w, r)
	case "/api/embeddings":
		atomic.AddInt64(&m.embedCount, 1)
		m.handleEmbeddings(w, r)
	case "/api/embed":
		atomic.AddInt64(&m.embed2Count, 1)
		m.handleEmbed(w, r)
	case "/api/tags":
		atomic.AddInt64(&m.tagsCount, 1)
		m.handleTags(w, r)
	case "/api/ps":
		atomic.AddInt64(&m.psCount, 1)
		m.handlePs(w, r)
	case "/api/version":
		atomic.AddInt64(&m.versionCount, 1)
		m.handleVersion(w, r)
	case "/api/show":
		atomic.AddInt64(&m.showCount, 1)
		m.handleShow(w, r)
	case "/api/create":
		atomic.AddInt64(&m.createCount, 1)
		m.handleCreate(w, r)
	case "/api/pull":
		atomic.AddInt64(&m.pullCount, 1)
		m.handlePull(w, r)
	case "/api/delete":
		atomic.AddInt64(&m.deleteCount, 1)
		m.handleDelete(w, r)
	case "/api/copy":
		atomic.AddInt64(&m.copyCount, 1)
		m.handleCopy(w, r)
	case "/api/push":
		atomic.AddInt64(&m.pushCount, 1)
		m.handlePush(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}
}

func (m *mockOllamaServer) handleGenerate(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
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
			time.Sleep(10 * time.Millisecond)
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":          model,
			"response":       "Hello world!",
			"done":           true,
			"total_duration": 1234567890,
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

func (m *mockOllamaServer) handleEmbed(w http.ResponseWriter, r *http.Request) {
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
		"embeddings": [][]float64{{0.1, 0.2, 0.3, 0.4, 0.5}},
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

func (m *mockOllamaServer) handleShow(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	model := ""
	if mod, ok := req["name"].(string); ok {
		model = mod
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"license":    "MIT",
		"modelfile":  "# Modelfile for " + model,
		"parameters": "num_ctx 4096",
		"template":   "[INST] {{ .Prompt }} [/INST]",
		"details": map[string]interface{}{
			"parent_model":       "",
			"format":             "gguf",
			"family":             "llama",
			"families":           []string{"llama"},
			"parameter_size":     "8B",
			"quantization_level": "Q4_0",
		},
		"model_info": map[string]interface{}{
			"general.architecture":    "llama",
			"general.parameter_count": 8000000000,
			"llama.context_length":    4096,
			"llama.embedding_length":  4096,
		},
	})
}

func (m *mockOllamaServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	model := ""
	if mod, ok := req["name"].(string); ok {
		model = mod
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	responses := []map[string]interface{}{
		{"status": "reading manifest"},
		{"status": "pulling base model"},
		{"status": "creating model " + model},
		{"status": "writing layer"},
		{"status": "success"},
	}

	for _, resp := range responses {
		data, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		time.Sleep(5 * time.Millisecond)
	}
}

func (m *mockOllamaServer) handlePull(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

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
		{"status": "downloading base layer", "completed": 1024, "total": 4096},
		{"status": "downloading base layer", "completed": 2048, "total": 4096},
		{"status": "downloading base layer", "completed": 4096, "total": 4096},
		{"status": "verifying sha256 digest"},
		{"status": "writing manifest"},
		{"status": "success"},
	}

	for _, resp := range responses {
		data, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		time.Sleep(5 * time.Millisecond)
	}
}

func (m *mockOllamaServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	model := ""
	if mod, ok := req["name"].(string); ok {
		model = mod
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"deleted": true,
		"model":   model,
	})
}

func (m *mockOllamaServer) handleCopy(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	source := ""
	if s, ok := req["source"].(string); ok {
		source = s
	}
	dest := ""
	if d, ok := req["destination"].(string); ok {
		dest = d
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"copied":      true,
		"source":      source,
		"destination": dest,
	})
}

func (m *mockOllamaServer) handlePush(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	responses := []map[string]interface{}{
		{"status": "pushing manifest"},
		{"status": "uploading layer", "completed": 1024, "total": 4096},
		{"status": "uploading layer", "completed": 4096, "total": 4096},
		{"status": "success"},
	}

	for _, resp := range responses {
		data, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		time.Sleep(5 * time.Millisecond)
	}
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
			Port:    0,
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
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: true,
			RequestTimeout:    30,
			QueueTimeout:      60,
			QueueMaxSize:      100,
			QueueWorkers:      4,
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
			DiskFree:        20480,
		},
		Ollama: types.OllamaMetrics{
			MaxModels:             5,
			MaxConcurrentRequests: 10,
			ActiveRequests:        0,
			OllamaAvailable:       true,
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:8b", VRAMUsage: 6000},
			},
		},
	})

	// Создаем HTTP сервер для прокси
	proxyServer := httptest.NewServer(proxy)

	return proxyServer, proxy
}

// parseHostPort - парсинг URL в хост:порт.
// Использует net.SplitHostPort, чтобы корректно обрабатывать IPv6-адреса
// (httptest может слушать на [::1]:port).
func parseHostPort(urlStr string) (string, int) {
	urlStr = strings.TrimPrefix(urlStr, "http://")
	host, portStr, err := net.SplitHostPort(urlStr)
	if err != nil {
		return "localhost", 11434
	}
	port := 0
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

// setupProxyWithMock - вариант setupProxyWithMockOllama для удобства
func setupProxyWithMock(t *testing.T, mock *mockOllamaServer) (*httptest.Server, *balancer.Proxy) {
	return setupProxyWithMockOllama(t, mock)
}
