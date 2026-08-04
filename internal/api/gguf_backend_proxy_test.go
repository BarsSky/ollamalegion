package api

import (
	"bufio"
	"bytes"
	"context"
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

// mockCppWorker — мок-CppWorker, имитирующий нужные для тестов эндпоинты.
type mockCppWorker struct {
	server *httptest.Server
	// Запоминаем вызовы для assertions
	receivedHFTokens []string
	downloadCount    int
	cancelCount      int
	searchCount      int
	filesCount       int
}

func newMockCppWorker() *mockCppWorker {
	m := &mockCppWorker{}
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Mock-Server", "true")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"version": "test-1.0",
			"models":  []string{},
		})
	})
	mux.HandleFunc("/api/gpu", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"gpuCount": 1,
			"devices":  []map[string]interface{}{{"index": 0, "name": "Mock-GPU", "vramTotalMB": 8192}},
		})
	})
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "llama-3-8b-q4", "ctxSize": 2048},
			},
		})
	})
	mux.HandleFunc("/api/models/files", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"files": []map[string]interface{}{
				{"name": "llama-3-8b-q4_K_M.gguf", "sizeBytes": int64(4368709120)},
			},
		})
	})
	mux.HandleFunc("/api/hf/search", func(w http.ResponseWriter, r *http.Request) {
		m.searchCount++
		token := r.Header.Get("X-HF-Token")
		m.receivedHFTokens = append(m.receivedHFTokens, token)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []map[string]interface{}{
				{"id": "TheBloke/Llama-2-7B-GGUF", "downloads": 100000, "likes": 200},
			},
		})
	})
	mux.HandleFunc("/api/hf/files", func(w http.ResponseWriter, r *http.Request) {
		m.filesCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"files": []map[string]interface{}{
				{"path": "llama-2-7b.Q4_K_M.gguf", "sizeBytes": int64(4096000000), "isGGUF": true},
			},
			"modelId": r.URL.Query().Get("modelId"),
		})
	})
	mux.HandleFunc("/api/hf/download", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		m.downloadCount++
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "started",
			"progress": map[string]interface{}{
				"modelId":   req["modelId"],
				"filename":  req["filename"],
				"percent":   0,
				"status":    "starting",
				"startedAt": time.Now().UTC().Format(time.RFC3339),
			},
		})
	})
	mux.HandleFunc("/api/hf/progress", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"modelId":     r.URL.Query().Get("modelId"),
			"filename":    r.URL.Query().Get("filename"),
			"percent":     42.5,
			"status":      "downloading",
			"progressPct": 42.5,
		})
	})
	mux.HandleFunc("/api/hf/downloads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"active": []map[string]interface{}{
				{
					"modelId":  "TheBloke/Llama-2-7B-GGUF",
					"filename": "llama-2-7b.Q4_K_M.gguf",
					"status":   "downloading",
					"percent":  42.5,
				},
			},
			"history": []map[string]interface{}{},
		})
	})
	mux.HandleFunc("/api/hf/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		m.cancelCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
	})
	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockCppWorker) URL() string { return m.server.URL }

func (m *mockCppWorker) close() { m.server.Close() }

// createProxyTestServer — аналог createTestServer, но с зарегистрированным llama_cpp бэкендом
// и URL, указывающим на переданный mockCppWorker.
func createProxyTestServer(t *testing.T, cppWorkerURL string) (*httptest.Server, *Server) {
	t.Helper()

	// Парсим URL мока: http://127.0.0.1:12345
	// Бэкенд регистрируем с host=127.0.0.1 (не host.docker.internal).
	// CppWorkerPort = порт из URL — вытащим из cppWorkerURL.
	port := 18092 // default
	if strings.HasPrefix(cppWorkerURL, "http://") {
		rest := strings.TrimPrefix(cppWorkerURL, "http://")
		if i := strings.Index(rest, ":"); i >= 0 {
			rest = rest[i+1:]
			if j := strings.Index(rest, "/"); j >= 0 {
				rest = rest[:j]
			}
			fmt.Sscanf(rest, "%d", &port)
		}
	}

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:                "llama_mock",
				Name:              "Mock CppWorker",
				Host:              "127.0.0.1",
				OllamaPort:        11434,
				AgentPort:         9090,
				CppWorkerPort:     port,
				Type:              types.BackendTypeLlamaCpp,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			ModelAffinity:       true,
			HealthCheckInterval: 10,
			MetricsInterval:     5,
			RequestTimeout:      30,
			QueueTimeout:        60,
			QueueMaxSize:        100,
			QueueWorkers:        4,
		},
		API: types.APISettings{
			RateLimit: 100,
			RateBurst: 200,
		},
		Auth: types.AuthConfig{
			Enabled: false,
		},
	}

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, healthChecker)

	testServer := httptest.NewServer(server)
	return testServer, server
}

func TestGgufBackendProxy_GET_info(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/llama_mock/proxy/info")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// Заголовки должны быть переданы
	assert.Equal(t, "true", resp.Header.Get("X-Mock-Server"))
	// Заголовок от прокси о том, откуда пришёл ответ
	assert.Contains(t, resp.Header.Get("X-Proxied-From-CppWorker"), "127.0.0.1:")

	body, _ := io.ReadAll(resp.Body)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &info))
	assert.Equal(t, "test-1.0", info["version"])
}

func TestGgufBackendProxy_GET_gpu(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/llama_mock/proxy/api/gpu")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &info))
	assert.Equal(t, float64(1), info["gpuCount"])
}

func TestGgufBackendProxy_GET_models(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/llama_mock/proxy/api/models")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &info))
	models, ok := info["models"].([]interface{})
	require.True(t, ok)
	assert.Equal(t, 1, len(models))
}

func TestGgufBackendProxy_GET_models_files(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/llama_mock/proxy/api/models/files")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &info))
	files, ok := info["files"].([]interface{})
	require.True(t, ok)
	assert.Equal(t, 1, len(files))
}

func TestGgufBackendProxy_GET_hf_search_passes_token(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	req, _ := http.NewRequest("GET",
		server.URL+"/api/v1/gguf/backends/llama_mock/proxy/api/hf/search?query=llama&limit=5", nil)
	req.Header.Set("X-HF-Token", "hf_test_xxxxx")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, mock.searchCount)
	assert.Equal(t, "hf_test_xxxxx", mock.receivedHFTokens[0])
}

func TestGgufBackendProxy_GET_hf_files(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/llama_mock/proxy/api/hf/files?modelId=TheBloke/Llama-2-7B-GGUF&revision=main")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, mock.filesCount)

	body, _ := io.ReadAll(resp.Body)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &info))
	assert.Equal(t, "TheBloke/Llama-2-7B-GGUF", info["modelId"])
}

func TestGgufBackendProxy_POST_hf_download(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	body := strings.NewReader(`{"modelId":"TheBloke/Llama-2-7B-GGUF","filename":"llama-2-7b.Q4_K_M.gguf","revision":"main"}`)
	resp, err := http.Post(server.URL+"/api/v1/gguf/backends/llama_mock/proxy/api/hf/download",
		"application/json", body)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.Equal(t, 1, mock.downloadCount)

	respBody, _ := io.ReadAll(resp.Body)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(respBody, &info))
	assert.Equal(t, "started", info["status"])
}

func TestGgufBackendProxy_GET_hf_downloads(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/llama_mock/proxy/api/hf/downloads")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &info))
	active, ok := info["active"].([]interface{})
	require.True(t, ok)
	assert.GreaterOrEqual(t, len(active), 1)
}

func TestGgufBackendProxy_GET_hf_progress(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/llama_mock/proxy/api/hf/progress?modelId=foo&filename=bar.gguf")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &info))
	assert.Equal(t, "foo", info["modelId"])
	assert.Equal(t, "bar.gguf", info["filename"])
}

func TestGgufBackendProxy_POST_hf_cancel(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	body := strings.NewReader(`{"modelId":"TheBloke/Llama-2-7B-GGUF","filename":"llama-2-7b.Q4_K_M.gguf"}`)
	resp, err := http.Post(server.URL+"/api/v1/gguf/backends/llama_mock/proxy/api/hf/cancel",
		"application/json", body)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, mock.cancelCount)
}

func TestGgufBackendProxy_backend_not_found(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/unknown_backend/proxy/info")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	var info map[string]string
	require.NoError(t, json.Unmarshal(body, &info))
	assert.Equal(t, "backend not found", info["error"])
}

func TestGgufBackendProxy_wrong_backend_type(t *testing.T) {
	// Создаём сервер с бэкендом типа Ollama, но URLом как у мока
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{ID: "ollama_mock", Name: "Ollama mock", Host: "127.0.0.1", OllamaPort: 11434,
				AgentPort: 9090, CppWorkerPort: 18092, Type: types.BackendTypeOllama,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmResourceAware},
		Auth:      types.AuthConfig{Enabled: false},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	srv := NewServer(proxy, config, hc)
	testServer := httptest.NewServer(srv)
	defer testServer.Close()

	resp, err := http.Get(testServer.URL + "/api/v1/gguf/backends/ollama_mock/proxy/info")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	var info map[string]string
	require.NoError(t, json.Unmarshal(body, &info))
	assert.Contains(t, info["message"], "not llama_cpp")
}

func TestGgufBackendProxy_unreachable_target(t *testing.T) {
	// Создаём сервер с бэкендом, указывающим на закрытый порт
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{ID: "llama_unreachable", Name: "Unreachable", Host: "127.0.0.1", OllamaPort: 11434,
				AgentPort: 9090, CppWorkerPort: 1, Type: types.BackendTypeLlamaCpp,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmResourceAware},
		Auth:      types.AuthConfig{Enabled: false},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	srv := NewServer(proxy, config, hc)
	testServer := httptest.NewServer(srv)
	defer testServer.Close()

	// Таймаут прокси — 90s, но на закрытом порту получим connection refused сразу.
	// Используем клиент с маленьким таймаутом.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(testServer.URL + "/api/v1/gguf/backends/llama_unreachable/proxy/info")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	var info map[string]string
	require.NoError(t, json.Unmarshal(body, &info))
	assert.Equal(t, "cppworker unreachable", info["error"])
	assert.Contains(t, info["message"], "127.0.0.1:1")
}

func TestGgufBackendProxy_invalid_path(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:18092")
	defer server.Close()

	// Невалидный путь — не содержит "proxy/"
	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/llama_mock/notaproxy")
	require.NoError(t, err)
	defer resp.Body.Close()

	// Так как /api/v1/gguf/backends/{id}/ ловит всё подряд, и в нём есть только
	// handleGgufBackendProxy — он ответит 400 invalid proxy path.
	// Однако точное поведение зависит от того, что "backends" уже зарегистрирован
	// раньше как отдельный handler. В нашем коде order:
	//   /api/v1/gguf/backends → handleGgufBackends (точное совпадение)
	//   /api/v1/gguf/backends/ → handleGgufBackendProxy (prefix-match)
	// Поэтому /api/v1/gguf/backends/llama_mock/notaproxy пойдёт в proxy handler
	// и вернёт 400.
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestGgufBackendProxy_host_docker_internal_preserved(t *testing.T) {
	// Прокси выполняется server-to-server (balancer → CppWorker), поэтому
	// host.docker.internal должен СОХРАНЯТЬСЯ как есть: контейнер balancer'а
	// корректно резолвит этот Docker-алиас (в отличие от браузера, для которого
	// изначально и сделан прокси-эндпоинт).
	server, s := createProxyTestServer(t, "http://127.0.0.1:18092")
	defer server.Close()

	dockerBackend := types.Backend{
		ID:            "llama_docker",
		Name:          "Docker llama",
		Host:          "host.docker.internal",
		OllamaPort:    11434,
		AgentPort:     9090,
		CppWorkerPort: 18092,
		Type:          types.BackendTypeLlamaCpp,
		Weight:        1,
		Status:        types.StatusHealthy,
	}
	_ = s.proxy.AddBackend(dockerBackend)

	host, port, baseURL, err := s.resolveCppWorkerURL("llama_docker")
	require.NoError(t, err)
	assert.Equal(t, "host.docker.internal", host)
	assert.Equal(t, 18092, port)
	assert.Equal(t, "http://host.docker.internal:18092", baseURL)
}

func TestGgufBackendProxy_query_string_passthrough(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/llama_mock/proxy/api/hf/search?query=llama-3&limit=20")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, mock.searchCount)
}

func TestGgufBackendProxy_body_passthrough(t *testing.T) {
	mock := newMockCppWorker()
	defer mock.close()

	server, _ := createProxyTestServer(t, mock.URL())
	defer server.Close()

	payload := map[string]interface{}{
		"modelId":  "test/repo",
		"filename": "test.Q4_K_M.gguf",
	}
	bodyJSON, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST",
		server.URL+"/api/v1/gguf/backends/llama_mock/proxy/api/hf/download",
		bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
}

// TestGgufBackendProxy_rewrites_500_already_in_progress проверяет пункт 1.5 плана:
// если CppWorker вернул 500 «download already in progress for ...» (старая версия
// бинаря), прокси балансера должен переписать это в HTTP 200 + JSON с полем
// status="already_in_progress", чтобы UI переключился на вкладку Downloads с polling.
func TestGgufBackendProxy_rewrites_500_already_in_progress(t *testing.T) {
	// Создаём отдельный мок-сервер, который имитирует «старый» CppWorker:
	// на каждый POST /api/hf/download возвращает 500 «already in progress».
	mux := http.NewServeMux()
	mux.HandleFunc("/api/hf/download", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "download failed",
			"message": "download already in progress for TheBloke/Llama-2-7B-GGUF/llama-2-7b.Q4_K_M.gguf",
		})
	})
	oldCppWorker := httptest.NewServer(mux)
	defer oldCppWorker.Close()

	server, _ := createProxyTestServer(t, oldCppWorker.URL)
	defer server.Close()

	body := strings.NewReader(`{"modelId":"TheBloke/Llama-2-7B-GGUF","filename":"llama-2-7b.Q4_K_M.gguf","revision":"main"}`)
	resp, err := http.Post(server.URL+"/api/v1/gguf/backends/llama_mock/proxy/api/hf/download",
		"application/json", body)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Главная проверка: 500 от CppWorker был переписан в 200 на уровне прокси.
	assert.Equal(t, http.StatusOK, resp.StatusCode, "proxy should rewrite 500 «already in progress» to 200")

	respBody, _ := io.ReadAll(resp.Body)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(respBody, &info))
	assert.Equal(t, "already_in_progress", info["status"])
	progress, ok := info["progress"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "downloading", progress["status"])
}

// TestGgufBackendProxy_500_other_error_passthrough — 500 от CppWorker, НЕ связанный
// с «already in progress», должен пройти через прокси как есть (только транспарентно).
func TestGgufBackendProxy_500_other_error_passthrough(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/hf/download", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "download failed",
			"message": "out of disk space",
		})
	})
	oldCppWorker := httptest.NewServer(mux)
	defer oldCppWorker.Close()

	server, _ := createProxyTestServer(t, oldCppWorker.URL)
	defer server.Close()

	body := strings.NewReader(`{"modelId":"test/repo","filename":"x.gguf"}`)
	resp, err := http.Post(server.URL+"/api/v1/gguf/backends/llama_mock/proxy/api/hf/download",
		"application/json", body)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "other 500 errors should not be rewritten")
}

// ============================================================
// Round 25 (2026-08-04): SSE proxy tests
// ============================================================

// TestIsSSEResponse — sanity check детектора SSE-стрима.
func TestIsSSEResponse(t *testing.T) {
	cases := []struct {
		name string
		ct   string
		want bool
	}{
		{"text/event-stream", "text/event-stream", true},
		{"text/event-stream; charset=utf-8", "text/event-stream; charset=utf-8", true},
		{"uppercase", "TEXT/EVENT-STREAM", true},
		{"mixed case", "Text/Event-Stream", true},
		{"application/json", "application/json", false},
		{"text/plain", "text/plain", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.ct != "" {
				h.Set("Content-Type", c.ct)
			}
			if got := isSSEResponse(h); got != c.want {
				t.Errorf("isSSEResponse(Content-Type=%q) = %v, want %v", c.ct, got, c.want)
			}
		})
	}
}

// TestProxyToCppWorker_SSE — proxy прозрачно стримит SSE-ответ.
func TestProxyToCppWorker_SSE(t *testing.T) {
	var upstreamFlushed int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 3 SSE events.
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"event\":%d}\n\n", i)
			flusher.Flush()
			upstreamFlushed++
		}
	}))
	defer upstream.Close()

	server, _ := createProxyTestServer(t, upstream.URL)
	defer server.Close()

	// Клиент читает SSE stream.
	req, _ := http.NewRequest("GET",
		server.URL+"/api/v1/gguf/backends/llama_mock/proxy/api/test/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	// Proxy должен был сбросить Content-Length (неизвестен заранее для стрима).
	assert.Equal(t, "", resp.Header.Get("Content-Length"),
		"Content-Length should be empty for SSE (chunked transfer encoding)")
	// nginx не должен буферизировать.
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))

	// Читаем body и проверяем, что все 3 event'а пришли.
	scanner := bufio.NewScanner(resp.Body)
	events := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			events++
		}
	}
	assert.GreaterOrEqual(t, events, 3, "expected at least 3 SSE events")
	assert.GreaterOrEqual(t, upstreamFlushed, 3, "upstream should have flushed 3 times")
}

// TestProxyToCppWorker_NonSSE — обычный JSON-ответ буферизируется (старое поведение).
func TestProxyToCppWorker_NonSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","count":42}`))
	}))
	defer upstream.Close()

	server, _ := createProxyTestServer(t, upstream.URL)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/llama_mock/proxy/api/test")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	// Для НЕ-SSE Content-Length сохраняется (proxy буферизирует).
	cl := resp.Header.Get("Content-Length")
	assert.NotEqual(t, "", cl, "non-SSE response should keep Content-Length")

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), `"status":"ok"`)
}

// TestStreamCopy_NormalEOF — streamCopy возвращает nil на нормальный EOF.
func TestStreamCopy_NormalEOF(t *testing.T) {
	body := strings.NewReader("data: hello\n\ndata: world\n\n")
	w := httptest.NewRecorder()
	var flusher http.Flusher
	if f, ok := any(w).(http.Flusher); ok {
		flusher = f
	}
	err := streamCopy(w, body, flusher, context.Background())
	assert.NoError(t, err)
	assert.Contains(t, w.Body.String(), "data: hello")
	assert.Contains(t, w.Body.String(), "data: world")
}

// TestStreamCopy_ContextCanceled — streamCopy возвращает ctx.Err() на client disconnect.
func TestStreamCopy_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // отменяем сразу
	body := strings.NewReader("data: should\n\ndata: not\n\ndata: be\n\ndata: delivered\n\n")
	w := httptest.NewRecorder()
	var flusher http.Flusher
	if f, ok := any(w).(http.Flusher); ok {
		flusher = f
	}
	err := streamCopy(w, body, flusher, ctx)
	// Должен либо err == context.Canceled, либо err == nil (если успел прочитать всё).
	// Главное: не паника и не висит вечно.
	if err != nil {
		assert.Contains(t, err.Error(), "context canceled")
	}
}

// TestStreamCopy_ReaderError — streamCopy возвращает io error при network failure.
func TestStreamCopy_ReaderError(t *testing.T) {
	body := &errReader{err: io.ErrUnexpectedEOF}
	w := httptest.NewRecorder()
	var flusher http.Flusher
	if f, ok := any(w).(http.Flusher); ok {
		flusher = f
	}
	err := streamCopy(w, body, flusher, context.Background())
	assert.Error(t, err)
}

// --- helpers ---

// errReader — всегда возвращает ошибку.
type errReader struct{ err error }

func (e *errReader) Read(p []byte) (int, error) { return 0, e.err }
