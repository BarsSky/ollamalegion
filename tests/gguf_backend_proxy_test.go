package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== Тесты прокси GGUF / CppWorker ====================

// TestGGUFBackendProxy_HFTokenPropagation проверяет, что заголовки
// X-HF-Token и Authorization прокидываются от клиента к CppWorker через балансер.
func TestGGUFBackendProxy_HFTokenPropagation(t *testing.T) {
	// Стартуем локальный CppWorker-заглушку, которая фиксирует входящие заголовки.
	//
	// R66c (2026-09-22): две правки, обе — из-за плавающего падения в Linux-CI
	// (~1 прогон из 3):
	//  1. Заголовки фиксируем ТОЛЬКО для целевого пути /api/hf/download. Раньше
	//     стаб записывал их для ЛЮБОГО запроса, а балансер сам стучится в этот
	//     же стаб health-пробой (setupTestEnvironment задаёт HealthCheckInterval,
	//     и AddBackend провоцирует проверку) — проба перезаписывала сохранённые
	//     заголовки пустыми, и тест читал не свой запрос.
	//  2. Доступ под мьютексом: обработчик httptest и тест-горутина — разные
	//     горутины, без мьютекса это ещё и гонка данных.
	var (
		receivedHeaders http.Header
		headersMu       sync.Mutex
	)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hf/download" {
			headersMu.Lock()
			receivedHeaders = r.Header.Clone()
			headersMu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer stub.Close()

	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Регистрируем llama_cpp бэкенд, указывающий на stub.
	host, port := parseStubHostPort(t, stub.URL)
	err := proxy.AddBackend(types.Backend{
		ID:            "cppworker-stub",
		Name:          "CppWorker Stub",
		Host:          host,
		CppWorkerPort: port,
		Type:          types.BackendTypeLlamaCpp,
		Status:        types.StatusHealthy,
	})
	require.NoError(t, err)

	payload := map[string]interface{}{
		"modelId":  "test/model",
		"filename": "model.gguf",
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", baseURL+"/api/v1/gguf/backends/cppworker-stub/proxy/api/hf/download", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-HF-Token", "hf_test_token_123")
	req.Header.Set("Authorization", "Bearer hf_auth_token_456")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// Снимок заголовков под мьютексом (R66c): их пишет обработчик стаба.
	headersMu.Lock()
	gotHeaders := receivedHeaders
	headersMu.Unlock()
	// R66c (2026-09-22): в сообщениях печатаем ВСЕ полученные заголовки — этот
	// тест плавал в Linux-CI (падал в ~1 прогоне из 3, проходил локально и в
	// изоляции) и по обезличенному «expected Bearer ..., actual ""» нельзя
	// было понять, дошёл ли запрос вообще до стаба и что именно пришло.
	assert.Equal(t, "hf_test_token_123", gotHeaders.Get("X-HF-Token"),
		"стаб получил заголовки: %v", gotHeaders)
	assert.Equal(t, "Bearer hf_auth_token_456", gotHeaders.Get("Authorization"),
		"стаб получил заголовки: %v", gotHeaders)
	if gotHeaders == nil {
		t.Fatal("стаб не получил НИ ОДНОГО запроса на /api/hf/download — значит запрос не дошёл до cppworker")
	}
}

// TestGGUFBackendProxy_RewritesAlreadyInProgress проверяет, что прокси
// переписывает ответ CppWorker 500 «already in progress» в 200 JSON.
func TestGGUFBackendProxy_RewritesAlreadyInProgress(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("download already in progress"))
	}))
	defer stub.Close()

	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	host, port := parseStubHostPort(t, stub.URL)
	err := proxy.AddBackend(types.Backend{
		ID:            "cppworker-stub",
		Name:          "CppWorker Stub",
		Host:          host,
		CppWorkerPort: port,
		Type:          types.BackendTypeLlamaCpp,
		Status:        types.StatusHealthy,
	})
	require.NoError(t, err)

	payload := map[string]interface{}{
		"modelId":  "test/model",
		"filename": "model.gguf",
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", baseURL+"/api/v1/gguf/backends/cppworker-stub/proxy/api/hf/download", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", string(respBody))

	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(respBody, &data))
	assert.Equal(t, "already_in_progress", data["status"])
	assert.Contains(t, data, "progress")
}

func parseStubHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	host, portStr, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := net.LookupPort("tcp", portStr)
	require.NoError(t, err)
	return host, port
}

// TestGGUFBackendProxy_NonLlamaBackendRejected проверяет, что бэкенд
// не-подходящего типа отвергается прокси.
func TestGGUFBackendProxy_NonLlamaBackendRejected(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// agent-1 из setupTestEnvironment — ollama backend (Type пустой/legacy).
	backend := proxy.GetBackend("agent-1")
	require.NotNil(t, backend)

	resp, err := http.Get(baseURL + "/api/v1/gguf/backends/agent-1/proxy/api/hf/download")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
