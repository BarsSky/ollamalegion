package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===== Pure helper tests (no proxy needed) =====

// TestBackendPortForInfo — выбор правильного порта по типу бэкенда.
func TestBackendPortForInfo(t *testing.T) {
	tests := []struct {
		name        string
		backendType string
		cppWorker   int
		ollama      int
		wantPort    int
	}{
		{
			name:        "llama_cpp uses cppWorkerPort",
			backendType: "llama_cpp",
			cppWorker:   18091,
			ollama:      11434,
			wantPort:    18091,
		},
		{
			name:        "ollama uses ollamaPort",
			backendType: "ollama",
			cppWorker:   18091,
			ollama:      11434,
			wantPort:    11434,
		},
		{
			name:        "llama_cpp with zero cppWorkerPort falls back to 18091",
			backendType: "llama_cpp",
			cppWorker:   0,
			ollama:      11434,
			wantPort:    18091,
		},
		{
			name:        "ollama with zero ollamaPort falls back to 11434",
			backendType: "ollama",
			cppWorker:   18091,
			ollama:      0,
			wantPort:    11434,
		},
		{
			name:        "unknown type falls back to 11434 (ollama default)",
			backendType: "unknown",
			cppWorker:   18091,
			ollama:      11434,
			wantPort:    11434,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := backendPortForInfo(types.BackendMetrics{
				BackendType:   types.BackendType(tt.backendType),
				CppWorkerPort: tt.cppWorker,
				OllamaPort:    tt.ollama,
			})
			assert.Equal(t, tt.wantPort, got)
		})
	}
}

// TestTruncateForError — утилита обрезки длинных body для error-сообщений.
func TestTruncateForError(t *testing.T) {
	assert.Equal(t, "", truncateForError(""))
	short := "model not found"
	assert.Equal(t, short, truncateForError(short))
	long := make([]byte, 1024)
	for i := range long {
		long[i] = 'a'
	}
	truncated := truncateForError(string(long))
	// Ожидаем длину ≤ 512 + префикс "...truncated (1024 bytes)" (49 символов).
	assert.True(t, len(truncated) < 1024, "truncated should be smaller than original: %d", len(truncated))
	assert.Contains(t, truncated, "truncated")
}

// TestHasSuffix — локальный хелпер для clusterModelItemDispatcher.
// Покрывает позитивные и негативные кейсы.
func TestHasSuffix(t *testing.T) {
	assert.True(t, hasSuffix("/api/v1/cluster/models/foo/info", "/info"))
	assert.True(t, hasSuffix("/api/v1/cluster/models/foo/reload", "/reload"))
	assert.False(t, hasSuffix("/api/v1/cluster/models/foo/info", "/reload"))
	assert.False(t, hasSuffix("/api/v1/cluster/models/foo", "/info"))
	assert.True(t, hasSuffix("/info", "/info"))
}

// ===== Handler tests with real proxy (no llama_cpp backend) =====

// TestClusterModelInfoHandler_MethodNotAllowed — handler возвращает 405 для не-GET.
func TestClusterModelInfoHandler_MethodNotAllowed(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)
	require.NotNil(t, srv)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/models/gemma-4-E4B-it-Q4_K_M/info", nil)

	srv.clusterModelInfoHandler(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestClusterModelInfoHandler_MissingName — handler возвращает 400, если path
// не содержит имени модели.
func TestClusterModelInfoHandler_MissingName(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)
	require.NotNil(t, srv)

	rec := httptest.NewRecorder()
	// Один слэш после /models/ → name == "".
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/models//info", nil)

	srv.clusterModelInfoHandler(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "model name")
}

// TestClusterModelInfoHandler_NoBackends — handler корректно отвечает 200, даже
// если у зарегистрированного бэкенда host недоступен (127.0.0.1:1).
// Per-backend ошибки не ломают общий ответ — запись просто получает
// status="unavailable" и попадает в массив backends.
func TestClusterModelInfoHandler_NoBackends(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)
	require.NotNil(t, srv)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/models/some-model/info", nil)

	srv.clusterModelInfoHandler(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp modelInfoResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	// createProxyTestServer регистрирует ровно один llama_cpp бэкенд.
	assert.Equal(t, 1, resp.Count)
	require.Len(t, resp.Backends, 1)
	assert.Equal(t, "unavailable", resp.Backends[0].Status)
	assert.Equal(t, 0, resp.OKCount)
}

// TestClusterModelItemDispatcher_NoMatch — dispatcher возвращает 404 для
// неизвестного sub-resource.
func TestClusterModelItemDispatcher_NoMatch(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)
	require.NotNil(t, srv)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/models/foo/unknown-action", nil)

	srv.clusterModelItemDispatcher(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "unknown cluster model sub-resource")
}

// TestClusterModelItemDispatcher_RoutesToInfo — dispatcher корректно
// перенаправляет GET /info в clusterModelInfoHandler.
func TestClusterModelItemDispatcher_RoutesToInfo(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)
	require.NotNil(t, srv)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/models/gemma-4/info", nil)

	srv.clusterModelItemDispatcher(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// TestClusterModelItemDispatcher_RoutesToReload — dispatcher корректно
// перенаправляет POST /reload в clusterReloadModelHandler (с 400, т.к. body пуст).
func TestClusterModelItemDispatcher_RoutesToReload(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)
	require.NotNil(t, srv)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/models/gemma-4/reload", nil)

	srv.clusterModelItemDispatcher(rec, req)
	// clusterReloadModelHandler вернёт 400 на невалидный JSON (пустое body) или 200,
	// в зависимости от поведения. Главное — dispatcher не отдал 404.
	assert.NotEqual(t, http.StatusNotFound, rec.Code)
}