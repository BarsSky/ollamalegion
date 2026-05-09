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
// Тесты на Transfer-Encoding Error (основная проблема от OpenWebUI)
// =============================================================================

// TestTransferEncoding_StreamingResponse проверяет, что при проксировании streaming
// ответа (SSE) не возникает двойного chunked encoding. Заголовок Transfer-Encoding
// не должен копироваться в ResponseWriter — Go управляет им автоматически.
func TestTransferEncoding_StreamingResponse(t *testing.T) {
	// Создаём мок-сервер, который возвращает SSE с Transfer-Encoding: chunked (как реальная Ollama)
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Отправляем streaming запрос
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

	// КРИТИЧЕСКАЯ ПРОВЕРКА: Transfer-Encoding не должен быть скопирован с бэкенда
	// Это вызывало ошибку "TransferEncodingError: Not enough data to satisfy transfer length header"
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding header MUST NOT be present in proxy response. "+
			"It causes double chunked encoding and TransferEncodingError in clients like OpenWebUI")

	// Content-Type должен быть передан (это не Transfer-Encoding)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream",
		"Content-Type should be preserved for SSE responses")

	// Проверяем, что SSE события читаются корректно
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

	// Последнее событие должно иметь done:true
	lastEvent := events[len(events)-1]
	assert.Equal(t, true, lastEvent["done"])
}

// TestTransferEncoding_NonStreamingResponse проверяет, что для обычных (не-streaming)
// ответов Transfer-Encoding: chunked не копируется.
func TestTransferEncoding_NonStreamingResponse(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Non-streaming запрос
	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Transfer-Encoding не должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding header MUST NOT be present in proxy response for non-streaming requests")

	// Ответ должен читаться целиком без ошибок
	resultBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Should be able to read complete response body without errors")
	assert.NotEmpty(t, resultBytes, "Response body should not be empty")

	var result map[string]interface{}
	err = json.Unmarshal(resultBytes, &result)
	require.NoError(t, err, "Response should be valid JSON")
	assert.Equal(t, "Hello world!", result["response"])
}

// TestTransferEncoding_ChatStreaming проверяет, что chat streaming тоже работает
// без Transfer-Encoding ошибок.
func TestTransferEncoding_ChatStreaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

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

	// Transfer-Encoding НЕ должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding header MUST NOT be present in proxy chat streaming response")

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
}

// TestTransferEncoding_OllamaApiEndpoints проверяет, что все Ollama API эндпоинты
// (кроме streaming) не просачивают Transfer-Encoding и возвращают корректные ответы.
func TestTransferEncoding_OllamaApiEndpoints(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Устанавливаем running models
	mock.SetRunningModels([]types.RunningModel{
		{Name: "llama3.1:8b", Size: 4928300000, Digest: "sha256:abc123", VRAMUsage: 6000},
	})

	tests := []struct {
		name     string
		method   string
		path     string
		body     interface{}
	}{
		{"GET /api/tags", "GET", "/api/tags", nil},
		{"GET /api/ps", "GET", "/api/ps", nil},
		{"GET /api/version", "GET", "/api/version", nil},
		{"POST /api/show", "POST", "/api/show", map[string]string{"name": "llama3.1:8b"}},
		{"POST /api/delete", "DELETE", "/api/delete", map[string]string{"name": "llama3.1:8b"}},
		{"POST /api/copy", "POST", "/api/copy", map[string]string{"source": "llama3.1:8b", "destination": "llama3.1:8b-custom"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			var err error

			if tt.body != nil {
				bodyBytes, _ := json.Marshal(tt.body)
				req, err = http.NewRequest(tt.method, proxyServer.URL+tt.path, bytes.NewBuffer(bodyBytes))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req, err = http.NewRequest(tt.method, proxyServer.URL+tt.path, nil)
			}
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			// Проверка статуса
			assert.Equal(t, http.StatusOK, resp.StatusCode, "%s should return 200", tt.name)

			// КРИТИЧЕСКАЯ ПРОВЕРКА: Transfer-Encoding не должен присутствовать
			teHeader := resp.Header.Get("Transfer-Encoding")
			assert.Empty(t, teHeader,
				"Transfer-Encoding header MUST NOT be present in %s response", tt.name)

			// Content-Type должен быть application/json
			assert.Equal(t, "application/json", resp.Header.Get("Content-Type"),
				"%s should return application/json", tt.name)

			// Тело читается целиком без ошибок
			bodyBytes, err := io.ReadAll(resp.Body)
			require.NoError(t, err, "%s body should be readable without errors", tt.name)
			assert.NotEmpty(t, bodyBytes, "%s body should not be empty", tt.name)

			// Ответ - валидный JSON
			var result interface{}
			err = json.Unmarshal(bodyBytes, &result)
			require.NoError(t, err, "%s response should be valid JSON: %s", tt.name, string(bodyBytes))
		})
	}
}

// =============================================================================
// Тест на полный жизненный цикл Streaming сессии
// =============================================================================

// TestStreamingSession_CompleteLifecycle проверяет полный цикл streaming:
// отправили запрос -> получили SSE события -> получили done:true -> корректное закрытие.
func TestStreamingSession_CompleteLifecycle(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Tell me a story",
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
			// Проверяем формат SSE: каждая строка data должна быть валидным JSON
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				events = append(events, event)
			} else {
				t.Logf("WARNING: Invalid JSON in SSE event: %s", data)
			}
		}
	}
	require.NoError(t, scanner.Err(), "Scanner should complete without errors")
	assert.GreaterOrEqual(t, len(events), 3,
		"Should receive at least 3 SSE events (got %d)", len(events))

	// Проверяем структуру событий
	for i, event := range events {
		assert.Contains(t, event, "model", "Event %d should have 'model' field", i)
	}

	// Проверяем последнее событие - признак завершения
	lastEvent := events[len(events)-1]
	assert.Equal(t, true, lastEvent["done"], "Last event should have done:true")
	assert.Contains(t, lastEvent, "total_duration",
		"Last event should have total_duration (final Ollama stats)")

	// Проверяем, что сессия создалась
	sessions := proxy.GetSessions()
	assert.GreaterOrEqual(t, len(sessions), 1, "Session should be created after request")
}

// =============================================================================
// Тесты на все Proxy API эндпоинты (табличный тест)
// =============================================================================

// TestProxyAllEndpoints_NonStreaming проверяет все не-streaming эндпоинты:
// правильный HTTP статус, Content-Type, валидный JSON, отсутствие Transfer-Encoding.
func TestProxyAllEndpoints_NonStreaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Устанавливаем running models
	mock.SetRunningModels([]types.RunningModel{
		{Name: "llama3.1:8b", Size: 4928300000, Digest: "sha256:abc123", VRAMUsage: 6000},
	})

	type endpointTest struct {
		name          string
		method        string
		path          string
		body          interface{}
		expectJSON    bool
		expectStream  bool
		validateFunc  func(t *testing.T, resp *http.Response, body []byte)
	}

	tests := []endpointTest{
		{
			name:        "POST /api/generate non-streaming",
			method:      "POST",
			path:        "/api/generate",
			body:        map[string]interface{}{"model": "llama3.1:8b", "prompt": "Hi", "stream": false},
			expectJSON:  true,
			expectStream: false,
			validateFunc: func(t *testing.T, resp *http.Response, body []byte) {
				var result map[string]interface{}
				json.Unmarshal(body, &result)
				assert.Equal(t, "Hello world!", result["response"])
			},
		},
		{
			name:        "POST /api/chat non-streaming",
			method:      "POST",
			path:        "/api/chat",
			body:        map[string]interface{}{"model": "llama3.1:8b", "messages": []map[string]string{{"role": "user", "content": "Hi"}}, "stream": false},
			expectJSON:  true,
			expectStream: false,
			validateFunc: func(t *testing.T, resp *http.Response, body []byte) {
				var result map[string]interface{}
				json.Unmarshal(body, &result)
				msg := result["message"].(map[string]interface{})
				assert.Equal(t, "Hi there!", msg["content"])
			},
		},
		{
			name:         "POST /api/embeddings",
			method:       "POST",
			path:         "/api/embeddings",
			body:         map[string]interface{}{"model": "nomic-embed-text", "prompt": "Hello world"},
			expectJSON:   true,
			expectStream: false,
			validateFunc: func(t *testing.T, resp *http.Response, body []byte) {
				var result map[string]interface{}
				json.Unmarshal(body, &result)
				assert.Equal(t, "nomic-embed-text", result["model"])
				embeddings := result["embeddings"].([]interface{})
				assert.Equal(t, 5, len(embeddings))
			},
		},
		{
			name:         "GET /api/tags",
			method:       "GET",
			path:         "/api/tags",
			body:         nil,
			expectJSON:   true,
			expectStream: false,
			validateFunc: func(t *testing.T, resp *http.Response, body []byte) {
				var result map[string]interface{}
				json.Unmarshal(body, &result)
				models := result["models"].([]interface{})
				assert.Equal(t, 2, len(models))
			},
		},
		{
			name:         "GET /api/version",
			method:       "GET",
			path:         "/api/version",
			body:         nil,
			expectJSON:   true,
			expectStream: false,
			validateFunc: func(t *testing.T, resp *http.Response, body []byte) {
				var result map[string]interface{}
				json.Unmarshal(body, &result)
				assert.Equal(t, "ollamalegion-1.0.0", result["version"])
			},
		},
		{
			name:         "POST /api/show",
			method:       "POST",
			path:         "/api/show",
			body:         map[string]string{"name": "llama3.1:8b"},
			expectJSON:   true,
			expectStream: false,
			validateFunc: func(t *testing.T, resp *http.Response, body []byte) {
				var result map[string]interface{}
				json.Unmarshal(body, &result)
				assert.NotEmpty(t, result["license"])
			},
		},
		{
			name:         "DELETE /api/delete",
			method:       "DELETE",
			path:         "/api/delete",
			body:         map[string]string{"name": "llama3.1:8b"},
			expectJSON:   true,
			expectStream: false,
			validateFunc: func(t *testing.T, resp *http.Response, body []byte) {
				var result map[string]interface{}
				json.Unmarshal(body, &result)
				assert.Equal(t, true, result["deleted"])
			},
		},
		{
			name:         "POST /api/copy",
			method:       "POST",
			path:         "/api/copy",
			body:         map[string]string{"source": "llama3.1:8b", "destination": "llama3.1:8b-custom"},
			expectJSON:   true,
			expectStream: false,
			validateFunc: func(t *testing.T, resp *http.Response, body []byte) {
				var result map[string]interface{}
				json.Unmarshal(body, &result)
				assert.Equal(t, true, result["copied"])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			var err error

			if tt.body != nil {
				bodyBytes, _ := json.Marshal(tt.body)
				req, err = http.NewRequest(tt.method, proxyServer.URL+tt.path, bytes.NewBuffer(bodyBytes))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req, err = http.NewRequest(tt.method, proxyServer.URL+tt.path, nil)
			}
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			// Общие проверки
			assert.Equal(t, http.StatusOK, resp.StatusCode, "%s: status should be 200", tt.name)
			assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
				"%s: Transfer-Encoding header MUST NOT be present", tt.name)

			if tt.expectJSON {
				assert.Equal(t, "application/json", resp.Header.Get("Content-Type"),
					"%s: Content-Type should be application/json", tt.name)
			}

			if !tt.expectStream {
				// Для не-streaming ответов читаем тело целиком
				bodyBytes, err := io.ReadAll(resp.Body)
				require.NoError(t, err, "%s: body should be readable without errors", tt.name)
				assert.NotEmpty(t, bodyBytes, "%s: body should not be empty", tt.name)

				if tt.expectJSON {
					var result interface{}
					err = json.Unmarshal(bodyBytes, &result)
					require.NoError(t, err, "%s: response should be valid JSON: %s", tt.name, string(bodyBytes))
				}

				if tt.validateFunc != nil {
					tt.validateFunc(t, resp, bodyBytes)
				}
			} else {
				if tt.validateFunc != nil {
					bodyBytes, _ := io.ReadAll(resp.Body)
					tt.validateFunc(t, resp, bodyBytes)
				}
			}
		})
	}
}

// =============================================================================
// Тест на Streaming эндпоинты
// =============================================================================

// TestProxyAllEndpoints_Streaming проверяет все streaming эндпоинты:
// /api/generate streaming, /api/chat streaming, /api/create, /api/pull, /api/push
func TestProxyAllEndpoints_Streaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	mock.SetRunningModels([]types.RunningModel{
		{Name: "llama3.1:8b", Size: 4928300000, Digest: "sha256:abc123", VRAMUsage: 6000},
	})

	type streamTest struct {
		name             string
		method           string
		path             string
		body             interface{}
		minEvents        int
		lastEventKey     string
		lastEventValue   interface{}
	}

	tests := []streamTest{
		{
			name:           "POST /api/generate streaming",
			method:         "POST",
			path:           "/api/generate",
			body:           map[string]interface{}{"model": "llama3.1:8b", "prompt": "Hello", "stream": true},
			minEvents:      3,
			lastEventKey:   "done",
			lastEventValue: true,
		},
		{
			name:           "POST /api/chat streaming",
			method:         "POST",
			path:           "/api/chat",
			body:           map[string]interface{}{"model": "llama3.1:8b", "messages": []map[string]string{{"role": "user", "content": "Hi"}}, "stream": true},
			minEvents:      3,
			lastEventKey:   "done",
			lastEventValue: true,
		},
		{
			name:           "POST /api/create (streaming)",
			method:         "POST",
			path:           "/api/create",
			body:           map[string]interface{}{"name": "my-model", "modelfile": "FROM llama3.1:8b"},
			minEvents:      3,
			lastEventKey:   "status",
			lastEventValue: "success",
		},
		{
			name:           "POST /api/pull (streaming)",
			method:         "POST",
			path:           "/api/pull",
			body:           map[string]interface{}{"name": "llama3.1:8b"},
			minEvents:      4,
			lastEventKey:   "status",
			lastEventValue: "success",
		},
		{
			name:           "POST /api/push (streaming)",
			method:         "POST",
			path:           "/api/push",
			body:           map[string]interface{}{"name": "llama3.1:8b"},
			minEvents:      2,
			lastEventKey:   "status",
			lastEventValue: "success",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tt.body)

			var resp *http.Response
			var err error

			switch tt.method {
			case "POST":
				resp, err = http.Post(proxyServer.URL+tt.path, "application/json", bytes.NewBuffer(bodyBytes))
			default:
				req, _ := http.NewRequest(tt.method, proxyServer.URL+tt.path, bytes.NewBuffer(bodyBytes))
				req.Header.Set("Content-Type", "application/json")
				resp, err = http.DefaultClient.Do(req)
			}
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode, "%s: status should be 200", tt.name)
			assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream",
				"%s: Content-Type should be text/event-stream", tt.name)

			// КРИТИЧЕСКАЯ ПРОВЕРКА: Transfer-Encoding не должен присутствовать
			teHeader := resp.Header.Get("Transfer-Encoding")
			assert.Empty(t, teHeader,
				"%s: Transfer-Encoding header MUST NOT be present in streaming response", tt.name)

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
			require.NoError(t, scanner.Err(), "%s: scanner should complete without errors", tt.name)
			assert.GreaterOrEqual(t, len(events), tt.minEvents,
				"%s: should receive at least %d SSE events (got %d)", tt.name, tt.minEvents, len(events))

			// Проверяем последнее событие
			lastEvent := events[len(events)-1]
			assert.Equal(t, tt.lastEventValue, lastEvent[tt.lastEventKey],
				"%s: last event should have %s=%v", tt.name, tt.lastEventKey, tt.lastEventValue)
		})
	}
}

// =============================================================================
// Тест Health-эндпоинта
// =============================================================================

func TestProxyOllama_HealthEndpoint(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	resp, err := http.Get(proxyServer.URL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
		"Health endpoint should not have Transfer-Encoding header")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result map[string]interface{}
	err = json.Unmarshal(body, &result)
	require.NoError(t, err)
	assert.Equal(t, "healthy", result["status"])
}

// =============================================================================
// Тест граничных случаев
// =============================================================================

func TestProxyOllama_EdgeCases(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	t.Run("POST /api/generate with empty body", func(t *testing.T) {
		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer([]byte(`{}`)))
		require.NoError(t, err)
		defer resp.Body.Close()

		// Ollama вернёт 400 на пустой model, но прокси не должен упасть с ошибкой кодирования
		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "Should read body without errors even with empty request")
		assert.NotEmpty(t, bodyBytes)
	})

	t.Run("POST /api/generate with no model", func(t *testing.T) {
		payload := map[string]interface{}{
			"prompt": "Hello",
			"stream": false,
		}
		body, _ := json.Marshal(payload)
		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		// Прокси не должен паниковать при отсутствии model
		assert.Empty(t, resp.Header.Get("Transfer-Encoding"))
		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.NotNil(t, bodyBytes)
	})

	t.Run("POST /api/generate with invalid JSON", func(t *testing.T) {
		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer([]byte(`{invalid json}`)))
		require.NoError(t, err)
		defer resp.Body.Close()

		// Прокси не должен паниковать при невалидном JSON
		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.NotNil(t, bodyBytes)
	})
}

// =============================================================================
// Тест на корректную передачу Content-Type без дублирования Transfer-Encoding
// для всех типов контента, которые могут вернуть бэкенды
// =============================================================================

func TestTransferEncoding_HeaderCleanup(t *testing.T) {
	// Мок, который возвращает разные заголовки (имитация разных сценариев)
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch path {
		case "/api/generate":
			// Читаем stream флаг
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(body, &req)

			stream := true
			if s, ok := req["stream"].(bool); ok {
				stream = s
			}

			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Transfer-Encoding", "chunked")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				fmt.Fprintf(w, "data: {\"done\":true}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{"response": "ok", "done": true})
			}

		case "/api/tags":
			// Имитация ответа Ollama с chunked encoding
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "llama3.1:8b"},
				},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	host, port := parseHostPort(mockServer.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{ID: "mock-backend", Name: "Mock", Host: host, OllamaPort: port, Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, RequestTimeout: 30, QueueTimeout: 60, QueueMaxSize: 100, QueueWorkers: 4,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90}, CPU: types.CPULimits{MaxUsagePercent: 90}, Memory: types.MemoryLimits{MaxUsagePercent: 90}, Disk: types.DiskLimits{MinFreeMB: 1024},
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	proxy.UpdateMetrics("mock-backend", &types.BackendMetrics{
		ID: "mock-backend",
		GPU: types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 65536, DiskFree: 20480},
		Ollama: types.OllamaMetrics{MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Тест 1: Streaming запрос
	t.Run("streaming with chunked backend header cleanup", func(t *testing.T) {
		payload := map[string]interface{}{"model": "llama3.1:8b", "prompt": "test", "stream": true}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// Проверяем что Transfer-Encoding очищен
		te := resp.Header.Get("Transfer-Encoding")
		assert.Empty(t, te, "Transfer-Encoding should not be present in streaming response")

		// Проверяем что Content-Type сохранился
		ct := resp.Header.Get("Content-Type")
		assert.Contains(t, ct, "text/event-stream",
			"Content-Type should be preserved for SSE")

		// Проверяем что X-Accel-Buffering сохранился
		xab := resp.Header.Get("X-Accel-Buffering")
		assert.Equal(t, "no", xab,
			"X-Accel-Buffering should be preserved (not a transfer-encoding header)")
	})

	// Тест 2: Non-streaming запрос
	t.Run("non-streaming with chunked backend header cleanup", func(t *testing.T) {
		payload := map[string]interface{}{"model": "llama3.1:8b", "prompt": "test", "stream": false}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
			"Transfer-Encoding should not be present in non-streaming response")

		// Читаем тело
		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		var result map[string]interface{}
		err = json.Unmarshal(bodyBytes, &result)
		require.NoError(t, err)
		assert.Equal(t, "ok", result["response"])
	})

	// Тест 3: Tags endpoint
	t.Run("tags endpoint header cleanup", func(t *testing.T) {
		resp, err := http.Get(proxyServer.URL + "/api/tags")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
			"Transfer-Encoding should not be present in /api/tags response")
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		var result map[string]interface{}
		err = json.Unmarshal(bodyBytes, &result)
		require.NoError(t, err)
		models := result["models"].([]interface{})
		assert.Equal(t, 1, len(models))
	})
}

// =============================================================================
// Тест на множественные запросы (стресс-тест стабильности)
// =============================================================================

func TestProxyOllama_MultipleStreamingRequests(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Отправляем 5 параллельных streaming запросов
	numRequests := 5
	errChan := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		go func(id int) {
			payload := map[string]interface{}{
				"model":  "llama3.1:8b",
				"prompt": fmt.Sprintf("Request %d", id),
				"stream": true,
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

			// Проверяем отсутствие Transfer-Encoding
			if te := resp.Header.Get("Transfer-Encoding"); te != "" {
				errChan <- fmt.Errorf("request %d has Transfer-Encoding: %s", id, te)
				return
			}

			// Читаем все SSE события
			scanner := bufio.NewScanner(resp.Body)
			events := 0
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "data: ") {
					events++
				}
			}
			if scanner.Err() != nil {
				errChan <- fmt.Errorf("request %d scanner error: %v", id, scanner.Err())
				return
			}
			if events < 3 {
				errChan <- fmt.Errorf("request %d only got %d events", id, events)
				return
			}

			errChan <- nil
		}(i)
	}

	// Собираем результаты
	for i := 0; i < numRequests; i++ {
		select {
		case err := <-errChan:
			require.NoError(t, err, "Parallel streaming request %d should succeed", i)
		case <-time.After(10 * time.Second):
			t.Fatalf("Timeout waiting for request %d", i)
		}
	}
}
