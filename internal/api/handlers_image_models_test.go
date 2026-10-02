package api

import (
	"bufio"
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

// ============================================================
// Мок image-воркера (sd-server, Phase 3)
// ============================================================

// mockImageWorker — мок image-воркера. Схему ответов НЕ подгоняем под
// балансер: наоборот, балансер обязан передать их как есть (это и проверяем).
type mockImageWorker struct {
	server *httptest.Server

	mu        sync.Mutex
	hitsByKey map[string]int
	lastBody  string
	lastToken string
	lastAuth  string
}

func newMockImageWorker() *mockImageWorker {
	m := &mockImageWorker{hitsByKey: map[string]int{}}
	mux := http.NewServeMux()

	record := func(w http.ResponseWriter, r *http.Request, key string) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.hitsByKey[key]++
		m.lastBody = string(body)
		m.lastToken = r.Header.Get("X-API-Token")
		m.lastAuth = r.Header.Get("Authorization")
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
	}

	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, "/info")
		w.Header().Set("X-Mock-Image", "true")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"version": "test-image-1.0"})
	})
	mux.HandleFunc("/api/image/models", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, "/api/image/models")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "z-image-turbo-q3-k", "family": "z_image", "loaded": true},
			},
		})
	})
	mux.HandleFunc("/api/image/capabilities", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, "/api/image/capabilities")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"families": []string{"sd15", "z_image"},
			"maxSize":  2048,
		})
	})
	mux.HandleFunc("/api/image/models/load", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		record(w, r, "/api/image/models/load")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "loading", "echo": m.lastBody,
		})
	})
	mux.HandleFunc("/api/image/models/unload", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, "/api/image/models/unload")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unloaded"})
	})
	mux.HandleFunc("/api/image/models/load/progress", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, "/api/image/models/load/progress")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"stage\":%d}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/api/image/generate", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, "/api/image/generate")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"jobId": "job-42", "status": "queued"})
	})
	mux.HandleFunc("/api/image/jobs/job-42", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, "/api/image/jobs/job-42")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "completed", "format": "png"})
	})
	mux.HandleFunc("/api/hf/bundle", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		record(w, r, "/api/hf/bundle")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "started", "bundleId": "z-image-turbo-q3-k"})
	})
	mux.HandleFunc("/api/hf/download", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, "/api/hf/download")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "started"})
	})

	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockImageWorker) hits(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hitsByKey[key]
}

func (m *mockImageWorker) body() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastBody
}

func (m *mockImageWorker) token() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastToken
}

func (m *mockImageWorker) close() { m.server.Close() }

// portFromURL — вытащить порт из http://127.0.0.1:NNNN.
func portFromURL(t *testing.T, raw string) int {
	t.Helper()
	rest := strings.TrimPrefix(raw, "http://")
	idx := strings.LastIndex(rest, ":")
	require.GreaterOrEqual(t, idx, 0, "unexpected URL %q", raw)
	port := 0
	_, err := fmt.Sscanf(rest[idx+1:], "%d", &port)
	require.NoError(t, err)
	return port
}

// createImageTestServer — сервер с одним image_cpp бэкендом, указывающим на мок.
func createImageTestServer(t *testing.T, workerURL string) (*httptest.Server, *Server) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{
				ID:                "img_mock",
				Name:              "Mock Image Worker",
				Host:              "127.0.0.1",
				ImagePort:         portFromURL(t, workerURL),
				Type:              types.BackendTypeImage,
				Weight:            1,
				MaxConcurrentReqs: 2,
				Status:            types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			HealthCheckInterval: 10,
			MetricsInterval:     5,
			RequestTimeout:      30,
			QueueTimeout:        60,
			QueueMaxSize:        100,
			QueueWorkers:        4,
		},
		API:  types.APISettings{RateLimit: 1000, RateBurst: 2000},
		Auth: types.AuthConfig{Enabled: false},
	}

	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	srv := NewServer(proxy, config, hc)
	return httptest.NewServer(srv), srv
}

// ============================================================
// Список image-бэкендов и агрегаты
// ============================================================

func TestImageBackends_List(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/backends")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload struct {
		Backends []struct {
			ID        string `json:"id"`
			ImagePort int    `json:"imagePort"`
			URL       string `json:"url"`
			Type      string `json:"type"`
		} `json:"backends"`
		Total       int    `json:"total"`
		BackendType string `json:"backendType"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))

	require.Equal(t, 1, payload.Total)
	require.Len(t, payload.Backends, 1)
	assert.Equal(t, "img_mock", payload.Backends[0].ID)
	assert.Equal(t, "image_cpp", payload.Backends[0].Type)
	assert.Equal(t, portFromURL(t, mock.server.URL), payload.Backends[0].ImagePort,
		"imagePort must come from EffectiveImagePort()")
	assert.Contains(t, payload.Backends[0].URL, fmt.Sprintf("127.0.0.1:%d", portFromURL(t, mock.server.URL)))
	assert.Equal(t, "image_cpp", payload.BackendType)
}

func TestImageModelsAggregate_CollectsRawPayloads(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, s := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	// Второй image-бэкенд на тот же мок (проверяем, что агрегат собирает все).
	second := types.Backend{
		ID: "img_mock2", Name: "Mock Image Worker 2", Host: "127.0.0.1",
		ImagePort: portFromURL(t, mock.server.URL), Type: types.BackendTypeImage,
		Weight: 1, Status: types.StatusHealthy,
	}
	require.NoError(t, s.proxy.AddBackend(second))

	resp, err := http.Get(server.URL + "/api/v1/image/models")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload struct {
		Backends []struct {
			BackendID string          `json:"backendId"`
			OK        bool            `json:"ok"`
			Payload   json.RawMessage `json:"payload"`
		} `json:"backends"`
		Total int `json:"total"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	require.Equal(t, 2, payload.Total)
	for _, b := range payload.Backends {
		assert.True(t, b.OK, "backend %s must be reachable", b.BackendID)
		assert.Contains(t, string(b.Payload), "z-image-turbo-q3-k",
			"payload must be passed through verbatim")
	}
}

// ============================================================
// Прокси к image-воркеру
// ============================================================

func TestImageBackendProxy_TransparentPassthrough(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/backends/img_mock/proxy/info")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "true", resp.Header.Get("X-Mock-Image"), "worker headers must pass through")
	assert.Contains(t, resp.Header.Get("X-Proxied-From-ImageWorker"), "127.0.0.1:")
}

func TestImageBackendProxy_ModelsAlias(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/backends/img_mock/models")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, mock.hits("/api/image/models"))

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "z-image-turbo-q3-k")
}

func TestImageBackendProxy_LoadUnloadAliases(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/image/backends/img_mock/models/load",
		"application/json", strings.NewReader(`{"name":"z-image-turbo-q3-k"}`))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.Equal(t, 1, mock.hits("/api/image/models/load"))
	assert.Contains(t, mock.body(), "z-image-turbo-q3-k", "request body must be forwarded")

	resp, err = http.Post(server.URL+"/api/v1/image/backends/img_mock/models/unload",
		"application/json", strings.NewReader(`{"name":"z-image-turbo-q3-k"}`))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, mock.hits("/api/image/models/unload"))
}

func TestImageBackendProxy_LoadProgressSSE(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/backends/img_mock/models/load/progress")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"), "SSE must not be buffered by nginx")
	assert.Empty(t, resp.Header.Get("Content-Length"), "Content-Length must be dropped for SSE")

	scanner := bufio.NewScanner(resp.Body)
	events := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			events++
		}
	}
	assert.GreaterOrEqual(t, events, 3, "all SSE events must be streamed")
}

func TestImageBackendProxy_GenerateAndJobs(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/image/backends/img_mock/generate",
		"application/json", strings.NewReader(`{"prompt":"a cat","steps":8}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)

	var gen map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&gen))
	assert.Equal(t, "job-42", gen["jobId"], "worker response must not be rewritten")

	resp2, err := http.Get(server.URL + "/api/v1/image/backends/img_mock/jobs/job-42")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	body, _ := io.ReadAll(resp2.Body)
	assert.Contains(t, string(body), "completed")
}

func TestImageBackendProxy_PullAliasGoesToHFBundle(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/image/backends/img_mock/pull",
		"application/json", strings.NewReader(`{"bundleId":"z-image-turbo-q3-k"}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.Equal(t, 1, mock.hits("/api/hf/bundle"),
		"pull must be proxied to the worker's HF bundle endpoint (files belong next to sd-server)")
}

func TestImageBackendProxy_HFDownloadPassthrough(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/v1/image/backends/img_mock/hf/download",
		"application/json", strings.NewReader(`{"modelId":"leejet/Z-Image-Turbo-GGUF","filename":"x.gguf"}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.Equal(t, 1, mock.hits("/api/hf/download"))
}

func TestImageBackendProxy_WrongBackendType(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	// llama_cpp бэкенд: image-путь должен его отвергнуть (иначе запрос генерации
	// ушёл бы в llama.cpp-воркер).
	server, _ := createProxyTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/backends/llama_mock/models")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "not image_cpp")
}

func TestImageBackendProxy_BackendNotFound(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/backends/unknown/models")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "not found")
}

func TestImageBackendRoutes_UnknownPath(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/backends/img_mock/nonsense")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestImageBackendRoutes_SingleBackendInfo(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/image/backends/img_mock")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var info imageBgInfo
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&info))
	assert.Equal(t, "img_mock", info.ID)
	assert.Equal(t, portFromURL(t, mock.server.URL), info.ImagePort)
}

// TestImageBackendProxy_GgufPathAlsoAcceptsImageBackend — resolveCppWorkerURL
// теперь пускает image_cpp и берёт порт через EffectiveImagePort(): общий
// gguf-префикс прокси остаётся рабочим для image-воркера (обратная совместимость
// с уже написанным WebUI-кодом), но резолвится в порт image-воркера.
func TestImageBackendProxy_GgufPathAlsoAcceptsImageBackend(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, _ := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/gguf/backends/img_mock/proxy/info")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "true", resp.Header.Get("X-Mock-Image"))
	assert.Contains(t, resp.Header.Get("X-Proxied-From-CppWorker"),
		fmt.Sprintf("127.0.0.1:%d", portFromURL(t, mock.server.URL)),
		"image backend must resolve to EffectiveImagePort()")
}

// TestImageWorkerAPIToken_Precedence — токен image-воркера: бэкенд → env-фолбэк.
func TestImageWorkerAPIToken_Precedence(t *testing.T) {
	mock := newMockImageWorker()
	defer mock.close()

	server, s := createImageTestServer(t, mock.server.URL)
	defer server.Close()

	// 1) Токен из записи бэкенда.
	b := s.proxy.GetBackend("img_mock")
	require.NotNil(t, b)
	b.CppWorkerApiToken = "backend-token"
	assert.Equal(t, "backend-token", s.imageWorkerAPIToken("img_mock"))

	// 2) Пусто в бэкенде → env-фолбэк (bundled-стек: один LB_API_TOKEN на все сервисы).
	b.CppWorkerApiToken = ""
	t.Setenv("LB_API_TOKEN", "stack-token")
	t.Setenv("CPPWORKER_API_TOKEN", "")
	t.Setenv("IMAGEWORKER_API_TOKEN", "")
	assert.Equal(t, "stack-token", s.imageWorkerAPIToken("img_mock"))

	// 3) И токен реально уходит воркеру.
	resp, err := http.Get(server.URL + "/api/v1/image/backends/img_mock/proxy/info")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, "stack-token", mock.token())
}
