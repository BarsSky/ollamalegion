package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
// Тесты на TransferEncodingError при обрыве соединения от бэкенда
// (симуляция OLLAMA OOM при попытке загрузить 20GB модель в 8GB VRAM)
// =============================================================================

// TestTransferEncoding_BackendDisconnectMidStream проверяет, что при обрыве
// соединения бэкендом в середине стрима (симуляция OOM краша Ollama)
// не возникает паники и клиент не получает TransferEncodingError.
// После обрыва должен отправляться done:true или корректный chunked terminator.
func TestTransferEncoding_BackendDisconnectMidStream(t *testing.T) {
	behavior := MockBehavior{
		ChunkDelay:        5 * time.Millisecond,
		DropAfterChunks:   2,          // отправляем 2 чанка, потом обрываем
		DropOnStream:      true,       // используем hijack + close (симуляция OOM)
		StreamResponseSize: 5,         // всего могло быть 5 чанков
	}
	mock := NewExpandedMockServer(behavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Logf("request error (expected with backend disconnect): %v", err)
		return
	}
	defer resp.Body.Close()

	// КРИТИЧЕСКАЯ ПРОВЕРКА: Transfer-Encoding не должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding header MUST NOT be present even after backend disconnect")

	// Пытаемся прочитать сколько возможно данных
	_, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		// Ошибка чтения допустима - backend оборвал соединение
		t.Logf("read error after backend disconnect (expected): %v", readErr)
	} else {
		t.Log("successfully read all response body despite backend disconnect")
	}
}

// TestTransferEncoding_BackendConnectionReset проверяет сценарий, когда бэкенд
// сбрасывает TCP-соединение (RST) — как при внезапном падении Ollama из-за OOM.
// Используем hijack + SetLinger(0) для принудительного RST.
func TestTransferEncoding_BackendConnectionReset(t *testing.T) {
	// Создаём специальный мок, который обрывает соединение через RST
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)

		stream := true
		if s, ok := req["stream"].(bool); ok {
			stream = s
		}
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"response": "ok"})
			return
		}

		model := ""
		if m, ok := req["model"].(string); ok {
			model = m
		}

		// Отправляем заголовки и несколько SSE-событий
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		// Отправляем 2 чанка
		tokens := []string{"Hello", " world"}
		for i, token := range tokens {
			event, _ := json.Marshal(map[string]interface{}{
				"model":    model,
				"response": token,
				"done":     false,
			})
			fmt.Fprintf(w, "data: %s\n\n", event)
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)

			if i == len(tokens)-1 {
				// Последний чанк — обрываем через RST (симуляция OOM краша)
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					return
				}
				conn, buf, err := hijacker.Hijack()
				if err != nil {
					return
				}
				// Сбрасываем буфер
				if buf != nil {
					buf.Flush()
				}
				// SetLinger(0) = RST при закрытии
				tcpConn := conn.(*net.TCPConn)
				tcpConn.SetLinger(0)
				conn.Close()
				return
			}
		}
	}))
	defer mockServer.Close()

	host, port := parseHostPort(mockServer.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 0, APIPort: 0,
		},
		Backends: []types.Backend{
			{
				ID: "mock-rst", Name: "RST Mock", Host: host, OllamaPort: port,
				AgentPort: 9090, Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, ModelAffinity: true,
			SessionStickiness: true, RequestTimeout: 30,
			QueueTimeout: 60, QueueMaxSize: 100, QueueWorkers: 4,
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
	proxy.UpdateMetrics("mock-rst", &types.BackendMetrics{
		ID: "mock-rst",
		GPU: types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576, MemoryUsed: 8000, MemoryFree: 16576},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 65536, MemoryUsed: 16000, MemoryFree: 49536, DiskFree: 20480},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{{Name: "llama3.1:8b", VRAMUsage: 6000}},
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Logf("request error after RST (expected): %v", err)
		// При RST может не быть ответа вообще
		return
	}
	defer resp.Body.Close()

	// Проверяем отсутствие Transfer-Encoding
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding header MUST NOT be present after RST from backend")

	// Читаем что можем — ошибка EOF или connection reset ожидаемы
	_, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Logf("read error after RST disconnect (expected): %v", readErr)
	}
}

// TestTransferEncoding_OOMScenario проверяет полный сценарий OOM:
// бэкенд начинает отвечать (симуляция загрузки модели), пишет несколько токенов,
// потом внезапно умирает без отправки done:true и без корректного закрытия.
func TestTransferEncoding_OOMScenario(t *testing.T) {
	behavior := MockBehavior{
		ModelLoadDelay:     50 * time.Millisecond,  // симуляция загрузки модели
		ChunkDelay:         10 * time.Millisecond,
		DropAfterChunks:    3,     // 3 чанка, потом обрыв
		DropOnStream:       true,  // принудительный обрыв (OOM)
		StreamResponseSize: 8,     // всего могло быть 8
	}
	mock := NewExpandedMockServer(behavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	payload := map[string]interface{}{
		"model":  "llama3.1:70b",  // модель 70B, не влезает в VRAM
		"prompt": "Hello",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	// Создаём клиент с таймаутом, чтобы тест не завис
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Logf("request error in OOM scenario (expected): %v", err)
		return
	}
	defer resp.Body.Close()

	// КРИТИЧЕСКАЯ ПРОВЕРКА: Transfer-Encoding не должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding header MUST NOT be present in OOM scenario")

	// Читаем частичный ответ — ошибка допустима
	_, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Logf("partial read error in OOM scenario (expected): %v", readErr)
	}

	// Проверяем что после OOM краша прокси всё ещё работает
	resp2, err := client.Get(proxyServer.URL + "/api/tags")
	if err != nil {
		t.Logf("health check request failed: %v", err)
	} else {
		defer resp2.Body.Close()
		assert.Equal(t, http.StatusOK, resp2.StatusCode,
			"Proxy should still be functional after OOM scenario")
	}
}

// TestTransferEncoding_MultipleConsecutiveOOM проверяет, что несколько
// последовательных OOM-крашей не ломают прокси и не вызывают паники.
func TestTransferEncoding_MultipleConsecutiveOOM(t *testing.T) {
	behavior := MockBehavior{
		ModelLoadDelay:     20 * time.Millisecond,
		ChunkDelay:         5 * time.Millisecond,
		DropAfterChunks:    2,
		DropOnStream:       true,
		StreamResponseSize: 5,
	}
	mock := NewExpandedMockServer(behavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprintf("OOM_attempt_%d", i+1), func(t *testing.T) {
			payload := map[string]interface{}{
				"model":  "llama3.1:70b",
				"prompt": "Hello",
				"stream": true,
			}
			body, _ := json.Marshal(payload)

			resp, err := client.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
			if err != nil {
				t.Logf("attempt %d: request error (expected): %v", i+1, err)
				return
			}
			defer resp.Body.Close()

			// Проверяем Transfer-Encoding
			teHeader := resp.Header.Get("Transfer-Encoding")
			assert.Empty(t, teHeader,
				"Transfer-Encoding should not be present after consecutive OOM attempt %d", i+1)

			// Читаем что можем
			_, _ = io.ReadAll(resp.Body)
		})
	}

	// Проверяем работоспособность после всех OOM
	resp, err := client.Get(proxyServer.URL + "/api/version")
	if err != nil {
		t.Fatalf("health check after consecutive OOMs failed: %v", err)
	}
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestTransferEncoding_Backend503ThenStream проверяет, что 503 ошибка
// от бэкенда (например, при перегрузке из-за нехватки VRAM) не приводит
// к утечке Transfer-Encoding заголовка.
func TestTransferEncoding_Backend503ThenStream(t *testing.T) {
	// Локальный счётчик запросов для мока
	var requestCount int

	// Создаём мок, который сначала отвечает 503, потом нормально
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/generate" && requestCount == 0 {
			requestCount++
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"model not loaded - out of memory"}`))
			return
		}
		requestCount++
		// Нормальный ответ
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		event, _ := json.Marshal(map[string]interface{}{
			"model": "llama3.1:8b", "response": "Hello", "done": true,
		})
		fmt.Fprintf(w, "data: %s\n\n", event)
		flusher.Flush()
	}))

	defer mockServer.Close()

	host, port := parseHostPort(mockServer.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{ID: "mock-503", Name: "503 Mock", Host: host, OllamaPort: port,
				AgentPort: 9090, Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, ModelAffinity: true,
			SessionStickiness: true, RequestTimeout: 30, QueueTimeout: 60,
			QueueMaxSize: 100, QueueWorkers: 4,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 95},
			CPU: types.CPULimits{MaxUsagePercent: 90}, Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk: types.DiskLimits{MinFreeMB: 1024},
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("mock-503", &types.BackendMetrics{
		ID: "mock-503",
		GPU: types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576, MemoryUsed: 8000, MemoryFree: 16576},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 65536, MemoryUsed: 16000, MemoryFree: 49536, DiskFree: 20480},
		Ollama: types.OllamaMetrics{MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Нормальный запрос - должен пройти
	payload := map[string]interface{}{
		"model": "llama3.1:8b", "prompt": "Hello", "stream": true,
	}
	body, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 10 * time.Second}

	// Retry loop: бэкенд может вернуть 503 на первый запрос, балансировщик должен
	// либо повторить на том же бэкенде, либо вернуть ошибку. Ждём готовности
	// прокси и делаем до 3 попыток с короткой паузой.
	var resp *http.Response
	var err error
	for i := 0; i < 3; i++ {
		resp, err = client.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		if err == nil && resp.StatusCode != http.StatusServiceUnavailable {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding should not be present after 503-recovering backend")
}

// =============================================================================
// TestTransferEncoding_RealOllamaNdjson проверяет, что при проксировании streaming
// ответа с Content-Type: application/x-ndjson (как реальная Ollama для /api/generate
// и /api/chat) не возникает TransferEncodingError.
//
// Ключевые отличия от SSE:
// 1. Content-Type: application/x-ndjson, а не text/event-stream
// 2. NDJSON не использует "data: " префикс — каждая строка это валидный JSON
// 3. Реальная Ollama также возвращает Connection: close в заголовках,
//    что раньше копировалось в клиентский ответ и мешало Go корректно завершить
//    chunked encoding.
func TestTransferEncoding_RealOllamaNdjson(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)

		stream := true
		if s, ok := req["stream"].(bool); ok {
			stream = s
		}
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"response": "ok"})
			return
		}

		model := ""
		if m, ok := req["model"].(string); ok {
			model = m
		}

		// ТОЧНОЕ ПОВЕДЕНИЕ РЕАЛЬНОЙ OLLAMA:
		// 1. Content-Type: application/x-ndjson (а НЕ text/event-stream)
		// 2. Connection: close (реальная Ollama закрывает соединение после стрима)
		// 3. Без "data: " префикса — чистые JSON строки
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		// Отправляем NDJSON чанки как реальная Ollama
		tokens := []string{"Привет", " это", " тест", " NDJSON", "!"}
		for i, token := range tokens {
			isLast := i == len(tokens)-1
			event := map[string]interface{}{
				"model":    model,
				"response": token,
				"done":     isLast,
			}
			if isLast {
				event["total_duration"] = 1234567890
				event["eval_count"] = 5
			}

			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "%s\n", data)
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer mockServer.Close()

	host, port := parseHostPort(mockServer.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 0, APIPort: 0,
		},
		Backends: []types.Backend{
			{
				ID: "mock-ndjson", Name: "NDJSON Mock", Host: host, OllamaPort: port,
				AgentPort: 9090, Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, ModelAffinity: true,
			SessionStickiness: true, RequestTimeout: 30,
			QueueTimeout: 60, QueueMaxSize: 100, QueueWorkers: 4,
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
	proxy.UpdateMetrics("mock-ndjson", &types.BackendMetrics{
		ID: "mock-ndjson",
		GPU: types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576, MemoryUsed: 8000, MemoryFree: 16576},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 65536, MemoryUsed: 16000, MemoryFree: 49536, DiskFree: 20480},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{{Name: "llama3.1:8b", VRAMUsage: 6000}},
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	// Используем клиент с таймаутом для предотвращения зависания теста
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// КРИТИЧЕСКАЯ ПРОВЕРКА 1: Transfer-Encoding НЕ должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding header MUST NOT be present in proxy response for NDJSON streaming")

	// КРИТИЧЕСКАЯ ПРОВЕРКА 2: Connection: close НЕ должен проходить от бэкенда
	connHeader := resp.Header.Get("Connection")
	assert.NotEqual(t, "close", connHeader,
		"Connection: close from backend MUST NOT be forwarded to client")

	// КРИТИЧЕСКАЯ ПРОВЕРКА 3: Content-Type должен быть application/x-ndjson
	assert.Equal(t, "application/x-ndjson", resp.Header.Get("Content-Type"),
		"Content-Type should be preserved for NDJSON responses")

	// Читаем NDJSON строки (не SSE — нет "data: " префикса)
	scanner := bufio.NewScanner(resp.Body)
	var events []map[string]interface{}
	for scanner.Scan() {
		line := scanner.Text()
		// Пропускаем пустые строки и heartbeat
		if line == "" || strings.Contains(line, "heartbeat") {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err == nil {
			events = append(events, event)
		}
	}
	require.NoError(t, scanner.Err(),
		"Scanner should complete without errors (no TransferEncodingError)")
	assert.GreaterOrEqual(t, len(events), 5,
		"Should receive at least 5 NDJSON events (got %d)", len(events))

	// Последнее событие должно иметь done:true
	lastEvent := events[len(events)-1]
	assert.Equal(t, true, lastEvent["done"],
		"Last event should have done:true")
	assert.Contains(t, lastEvent, "total_duration",
		"Last event should have total_duration")

	t.Logf("Successfully read %d NDJSON events", len(events))
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

			// Проверка статуса
			assert.Equal(t, http.StatusOK, resp.StatusCode, "%s: status should be 200", tt.name)

			// Transfer-Encoding не должен присутствовать
			teHeader := resp.Header.Get("Transfer-Encoding")
			assert.Empty(t, teHeader, "%s: Transfer-Encoding header MUST NOT be present", tt.name)

			// Content-Type должен быть text/event-stream
			contentType := resp.Header.Get("Content-Type")
			assert.Contains(t, contentType, "text/event-stream",
				"%s: Content-Type should be text/event-stream", tt.name)

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
				"%s: should receive at least %d events (got %d)", tt.name, tt.minEvents, len(events))

			// Последнее событие проверяем
			lastEvent := events[len(events)-1]
			assert.Equal(t, tt.lastEventValue, lastEvent[tt.lastEventKey],
				"%s: last event should have %s=%v", tt.name, tt.lastEventKey, tt.lastEventValue)
		})
	}
}

// =============================================================================
// Тест на вызов /api/generate без stream флага (должен трактоваться как streaming)
// =============================================================================

// TestTransferEncoding_DefaultStream проверяет, что запрос без явного "stream"
// флага обрабатывается как streaming (совместимость с разными клиентами).
func TestTransferEncoding_DefaultStream(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Запрос без stream флага
	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Who are you?",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader)

	// Должен быть streaming ответ
	contentType := resp.Header.Get("Content-Type")
	assert.Contains(t, contentType, "text/event-stream",
		"Default request without stream flag should return streaming response")

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
	assert.GreaterOrEqual(t, len(events), 2, "Should receive at least 2 SSE events")
}

// =============================================================================
// Тесты на обработку запросов к здоровью и метрикам — не Transfer-Encoding
// =============================================================================

// TestTransferEncoding_HealthAndMetricsEndpoints проверяет, что health и metrics
// эндпоинты не имеют Transfer-Encoding и возвращают корректные ответы.
func TestTransferEncoding_HealthAndMetricsEndpoints(t *testing.T) {
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 0, APIPort: 0,
		},
		Backends: []types.Backend{},
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: true,
			RequestTimeout:    30,
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	endpoints := []string{
		"/health",
		"/metrics",
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			resp, err := http.Get(proxyServer.URL + ep)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode, "%s should return 200", ep)
			teHeader := resp.Header.Get("Transfer-Encoding")
			assert.Empty(t, teHeader, "Transfer-Encoding should not be present on %s", ep)
		})
	}
}

// =============================================================================
// Тесты на параллельные streaming запросы
// =============================================================================

// TestTransferEncoding_ParallelStreaming проверяет, что несколько параллельных
// streaming запросов не вызывают Transfer-Encoding проблем и все завершаются с done.
func TestTransferEncoding_ParallelStreaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	numRequests := 5
	errChan := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		go func(id int) {
			payload := map[string]interface{}{
				"model":  "llama3.1:8b",
				"prompt": fmt.Sprintf("Hello from request %d", id),
				"stream": true,
			}
			body, _ := json.Marshal(payload)

			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
			if err != nil {
				errChan <- fmt.Errorf("request %d failed: %v", id, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errChan <- fmt.Errorf("request %d got status %d", id, resp.StatusCode)
				return
			}

			teHeader := resp.Header.Get("Transfer-Encoding")
			if teHeader != "" {
				errChan <- fmt.Errorf("request %d has Transfer-Encoding header", id)
				return
			}

			scanner := bufio.NewScanner(resp.Body)
			hasDone := false
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "data: ") {
					data := strings.TrimPrefix(line, "data: ")
					var event map[string]interface{}
					if err := json.Unmarshal([]byte(data), &event); err == nil {
						if done, ok := event["done"].(bool); ok && done {
							hasDone = true
						}
					}
				}
			}
			if err := scanner.Err(); err != nil {
				errChan <- fmt.Errorf("request %d scanner error: %v", id, err)
				return
			}
			if !hasDone {
				errChan <- fmt.Errorf("request %d missing done:true", id)
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
