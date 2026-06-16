package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	var receivedHeaders http.Header
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
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
	assert.Equal(t, "hf_test_token_123", receivedHeaders.Get("X-HF-Token"))
	assert.Equal(t, "Bearer hf_auth_token_456", receivedHeaders.Get("Authorization"))
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