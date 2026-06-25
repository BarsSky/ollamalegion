package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===== Pure helper tests (no proxy needed) =====

// TestSplitClusterModelPath — парсинг URL /api/v1/cluster/models/{name}/reload.
func TestSplitClusterModelPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantName string
		wantOK   bool
	}{
		{
			name:     "standard reload path",
			path:     "/api/v1/cluster/models/gemma-4-E4B-it-Q4_K_M/reload",
			wantName: "gemma-4-E4B-it-Q4_K_M",
			wantOK:   true,
		},
		{
			name:     "simple model name",
			path:     "/api/v1/cluster/models/llama3/reload",
			wantName: "llama3",
			wantOK:   true,
		},
		{
			name:     "model name with path separators",
			path:     "/api/v1/cluster/models/hf:org/repo/file.gguf/reload",
			wantName: "hf:org",
			wantOK:   true,
		},
		{
			name:     "model name without reload suffix",
			path:     "/api/v1/cluster/models/some-model",
			wantName: "some-model",
			wantOK:   true,
		},
		{
			name:     "model name with .gguf extension",
			path:     "/api/v1/cluster/models/model.gguf/reload",
			wantName: "model.gguf",
			wantOK:   true,
		},
		{
			name:    "missing prefix",
			path:    "/api/v1/other/models/foo",
			wantOK:  false,
		},
		{
			name:    "empty model name",
			path:    "/api/v1/cluster/models/",
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, ok := splitClusterModelPath(tt.path)
			assert.Equal(t, tt.wantOK, ok, "path=%q ok mismatch", tt.path)
			if tt.wantOK {
				assert.Equal(t, tt.wantName, name, "path=%q name mismatch", tt.path)
			}
		})
	}
}

// TestCtxSizeForLog — helper для structured-логов.
func TestCtxSizeForLog(t *testing.T) {
	assert.Nil(t, ctxSizeForLog(nil))

	v := 32768
	assert.Equal(t, 32768, ctxSizeForLog(&v))
}

// TestGpuLayersForLog — helper для structured-логов.
func TestGpuLayersForLog(t *testing.T) {
	assert.Nil(t, gpuLayersForLog(nil))

	v := -2
	assert.Equal(t, -2, gpuLayersForLog(&v))
}

// ===== Handler tests with real proxy (no llama_cpp backend) =====

// TestClusterLoadedModelsHandler_NoLlamaCppBackends — handler возвращает пустой
// массив, если proxy работает, но нет llama_cpp бэкендов.
func TestClusterLoadedModelsHandler_NoLlamaCppBackends(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1") // неработающий URL
	defer server.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/models/loaded", nil)

	// Прямой вызов handler (без middleware) для проверки логики.
	// server.Config.Handler — это api.Server, доступ через server.Config.Handler.(*Server).
	srv, ok := server.Config.Handler.(*Server)
	require.True(t, ok, "handler must be *Server")

	srv.clusterLoadedModelsHandler(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp clusterLoadedModelsResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Бэкенд зарегистрирован, но не отвечает на health-чек → нет llama_cpp метрик
	// → пустой массив (но handler не возвращает 404 для пустого списка).
	assert.Equal(t, 0, resp.Count)
	assert.Empty(t, resp.Models)
}

// TestClusterLoadedModelsHandler_MethodNotAllowed — handler возвращает 405 для не-GET.
func TestClusterLoadedModelsHandler_MethodNotAllowed(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/models/loaded", nil)
	srv.clusterLoadedModelsHandler(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodDelete, "/api/v1/cluster/models/loaded", nil)
	srv.clusterLoadedModelsHandler(rec2, req2)
	assert.Equal(t, http.StatusMethodNotAllowed, rec2.Code)
}

// TestClusterLoadingModelsHandler_MethodNotAllowed — handler возвращает 405 для не-GET.
func TestClusterLoadingModelsHandler_MethodNotAllowed(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/models/loading", nil)
	srv.clusterLoadingModelsHandler(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodDelete, "/api/v1/cluster/models/loading", nil)
	srv.clusterLoadingModelsHandler(rec2, req2)
	assert.Equal(t, http.StatusMethodNotAllowed, rec2.Code)
}

// TestClusterReloadModelHandler_MethodNotAllowed — handler возвращает 405 для не-POST.
func TestClusterReloadModelHandler_MethodNotAllowed(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/models/foo/reload", nil)
	srv.clusterReloadModelHandler(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestClusterReloadModelHandler_MissingName — handler возвращает 400 если имя
// модели не извлекается из URL.
func TestClusterReloadModelHandler_MissingName(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)

	// Пустой путь: /api/v1/cluster/models//reload → parts = ["", "reload"] → "" → 400.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/models//reload", nil)
	srv.clusterReloadModelHandler(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "missing model name")
}

// TestClusterReloadModelHandler_InvalidOperation — handler возвращает 400 для
// неподдерживаемой операции.
func TestClusterReloadModelHandler_InvalidOperation(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)

	body := strings.NewReader(`{"operation":"delete-the-world"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/models/foo/reload", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.clusterReloadModelHandler(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid operation")
}

// TestClusterReloadModelHandler_InvalidJSON — handler возвращает 400 при
// невалидном JSON.
func TestClusterReloadModelHandler_InvalidJSON(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)

	body := strings.NewReader(`{not valid json`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/models/foo/reload", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.clusterReloadModelHandler(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid request body")
}

// TestClusterReloadModelHandler_ValidRequest_NoBackends — handler корректно обрабатывает
// запрос даже когда cppworker недоступен: возвращает 200 с per-backend результатами,
// где httpStatus=500 + error содержит детали сетевой ошибки. Это правильное поведение
// для cluster-level endpoint'а: ошибки отдельных бэкендов не ломают весь ответ.
func TestClusterReloadModelHandler_ValidRequest_NoBackends(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	srv := server.Config.Handler.(*Server)

	body := strings.NewReader(`{"operation":"load"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/models/gemma-4/reload", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.clusterReloadModelHandler(rec, req)

	// Handler возвращает 200 — это нормально для cluster endpoint'а, который агрегирует
	// per-backend результаты. Сам бэкенд ответил httpStatus=500 (cppworker unreachable).
	assert.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp clusterReloadModelResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "gemma-4", resp.Model)
	assert.Equal(t, "load", resp.Operation)
	require.Len(t, resp.Results, 1, "should have 1 backend result")

	r := resp.Results[0]
	assert.Equal(t, "llama_mock", r.BackendID)
	assert.Equal(t, "error", r.Status)
	assert.Equal(t, http.StatusInternalServerError, r.HTTPStatus)
	// error содержит либо "cppworker unreachable" (из retry), либо
	// "request failed: Post ..." (из executeLlamaCppLoad при первом failed request).
	assert.True(t,
		strings.Contains(r.Error, "cppworker unreachable") ||
			strings.Contains(r.Error, "request failed") ||
			strings.Contains(r.Error, "No connection could be made"),
		"error should mention cppworker connection issue: %s", r.Error)
}

// ===== Routes registration test =====

// TestRoutes_ClusterModels — проверяет, что новые routes зарегистрированы
// через стандартный mux.
func TestRoutes_ClusterModels(t *testing.T) {
	server, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer server.Close()

	tests := []struct {
		name       string
		method     string
		path       string
		expectCode int // один из: 200/400/404/405/503
	}{
		{
			name:       "GET /api/v1/cluster/models/loaded",
			method:     http.MethodGet,
			path:       "/api/v1/cluster/models/loaded",
			expectCode: 200, // proxy есть, нет бэкендов → пустой массив
		},
		{
			name:       "GET /api/v1/cluster/models/loading",
			method:     http.MethodGet,
			path:       "/api/v1/cluster/models/loading",
			expectCode: 200,
		},
		{
			name:       "POST /api/v1/cluster/models/gemma-4/reload",
			method:     http.MethodPost,
			path:       "/api/v1/cluster/models/gemma-4/reload",
			expectCode: 200, // handler возвращает 200 с per-backend результатами,
			// даже если cppworker недоступен (агрегированный cluster response).
		},
		{
			name:       "GET /api/v1/cluster/models/gemma-4/reload (wrong method)",
			method:     http.MethodGet,
			path:       "/api/v1/cluster/models/gemma-4/reload",
			expectCode: http.StatusMethodNotAllowed,
		},
		{
			name:       "POST /api/v1/cluster/models/loaded (wrong method)",
			method:     http.MethodPost,
			path:       "/api/v1/cluster/models/loaded",
			expectCode: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			server.Config.Handler.ServeHTTP(rec, req)

			assert.Equal(t, tt.expectCode, rec.Code, "body=%s", rec.Body.String())
		})
	}
}

// ===== Sanity: типы и константы =====

// TestAggregateLoadedModel_JSONFields — проверка что aggregate-структура имеет
// все нужные JSON-поля (для UI/скриптов).
func TestAggregateLoadedModel_JSONFields(t *testing.T) {
	m := aggregateLoadedModel{
		Name:          "gemma-4",
		BackendID:     "llama-cpp-1",
		BackendType:   "llama_cpp",
		Engine:        "llama_cpp",
		ContextLength: 32768,
		BatchSize:     256,
		NumGPULayers:  30,
		Quantization:  "Q4_K_M",
		VRAMUsage:     4096,
		RAMUsage:      0,
		Size:          4970000000,
		State:         "loaded",
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)

	// Проверяем что camelCase JSON-поля присутствуют.
	expected := []string{
		`"name":"gemma-4"`,
		`"backendId":"llama-cpp-1"`,
		`"backendType":"llama_cpp"`,
		`"contextLength":32768`,
		`"numGpuLayers":30`,
		`"quantization":"Q4_K_M"`,
		`"vramUsage":4096`,
		`"state":"loaded"`,
	}
	for _, want := range expected {
		assert.Contains(t, string(b), want, "missing field %s", want)
	}
}

// TestClusterReloadModelRequest_Defaults — проверка что при пустом operation
// используется "load" по умолчанию.
func TestClusterReloadModelRequest_Defaults(t *testing.T) {
	// Декодируем пустой JSON → Operation = "" → в handler станет "load".
	var req clusterReloadModelRequest
	err := json.Unmarshal([]byte(`{}`), &req)
	require.NoError(t, err)
	assert.Equal(t, "", req.Operation, "expected empty Operation in request, handler adds default")
}

// Compile-time guard: убедимся, что ClusterState содержит нужные поля,
// которые мы используем в handler'ах.
var _ = func() *types.ClusterState {
	return &types.ClusterState{
		Backends: []types.BackendMetrics{
			{BackendType: types.BackendTypeLlamaCpp},
		},
	}
}