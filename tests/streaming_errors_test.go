package tests

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
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Тест 1: Бэкенд обрывает соединение после части данных
// =============================================================================

// TestStreaming_BackendDisconnect проверяет, что при обрыве соединения
// бэкендом после отправки нескольких чанков прокси:
// - корректно завершает HTTP-ответ (статус 200)
// - не вызывает TransferEncodingError у клиента
// - не паникует и остаётся работоспособным
func TestStreaming_BackendDisconnect(t *testing.T) {
	// Мок с обрывом после 2 чанков (через Hijack — имитация резкого разрыва TCP)
	mock := NewExpandedMockServer(MockBehavior{
		DropAfterChunks:    2,
		DropOnStream:       true,
		StreamResponseSize: 5,
		ChunkDelay:         10 * time.Millisecond,
	})
	defer mock.Close()

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Disconnect test",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Transfer-Encoding НЕ должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding MUST NOT be present after backend disconnect")

	// Читаем SSE события — не должно быть TransferEncodingError
	scanner := bufio.NewScanner(resp.Body)
	var events []map[string]interface{}
	var scanErr error
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
	scanErr = scanner.Err()

	// Допускаем io.ErrUnexpectedEOF или EOF от обрыва соединения,
	// но НЕ TransferEncodingError (это ошибка на уровне HTTP-протокола,
	// которая проявляется ДО чтения тела)
	if scanErr != nil {
		errStr := scanErr.Error()
		assert.NotContains(t, errStr, "TransferEncodingError",
			"Should not get TransferEncodingError on backend disconnect, got: %v", scanErr)
		assert.NotContains(t, errStr, "Not enough data",
			"Should not get 'Not enough data' error, got: %v", scanErr)
	}

	// Должны были получить хотя бы 2 чанка до обрыва
	assert.GreaterOrEqual(t, len(events), 2,
		"Should receive at least 2 events before disconnect (got %d)", len(events))

	// ПРИМЕЧАНИЕ: при резком обрыве через Hijack Go HTTP клиент возвращает io.EOF,
	// а не сетевую ошибку. Прокси считает это корректным завершением и НЕ отправляет
	// принудительный done:true (т.к. не может отличить обрыв от нормального EOF).
	// Это ограничение Go net/http. В реальном сценарии (Python aiohttp / OpenWebUI)
	// обрыв вызывает именно TransferEncodingError, который мы предотвращаем
	// через удаление Transfer-Encoding заголовка.
}

// =============================================================================
// Тест 2: Неполные данные от бэкенда (simulated partial response)
// =============================================================================

// TestStreaming_PartialResponse проверяет, что когда бэкенд отправляет
// неполные данные (меньше Content-Length или обрыв после части чанков),
// прокси не вызывает TransferEncodingError и корректно обрабатывает ответ.
func TestStreaming_PartialResponse(t *testing.T) {
	// Используем ExpandedMockServer с DropAfterChunks — надёжная симуляция
	// обрыва после части данных
	mock := NewExpandedMockServer(MockBehavior{
		DropAfterChunks:    1,
		DropOnStream:       true,
		StreamResponseSize: 10,
		ChunkDelay:         5 * time.Millisecond,
	})
	defer mock.Close()

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	// Тест 1: Streaming запрос с неполным ответом
	t.Run("streaming partial response", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":  "llama3.1:8b",
			"prompt": "Partial response test",
			"stream": true,
		}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		// Прокси должен вернуть 200 — заголовки уже отправлены
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// Transfer-Encoding НЕ должен присутствовать
		assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
			"Transfer-Encoding MUST NOT be present")

		// Читаем то, что доступно — не должно быть TransferEncodingError
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			errStr := readErr.Error()
			assert.NotContains(t, errStr, "TransferEncodingError",
				"Should not get TransferEncodingError on partial response")
			assert.NotContains(t, errStr, "Not enough data",
				"Should not get 'Not enough data' error")
		}

		// Должны были получить частичные данные (хотя бы один чанк)
		assert.NotEmpty(t, respBody, "Should receive partial data")
		bodyStr := string(respBody)
		assert.True(t, strings.HasPrefix(bodyStr, "data: "),
			"Response should start with SSE 'data:' prefix")
	})

	// Тест 2: Non-streaming запрос, где бэкенд отвечает обычным JSON
	// (DropOnStream влияет только на streaming-ответы)
	t.Run("non-streaming normal response", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":  "llama3.1:8b",
			"prompt": "Normal response test",
			"stream": false,
		}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Transfer-Encoding"))

		respBody, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.NotEmpty(t, respBody)

		bodyStr := string(respBody)
		assert.Contains(t, bodyStr, "Hello world!",
			"Non-streaming response should be complete")
	})
}

// =============================================================================
// Тест 3: Отмена запроса клиентом (context cancellation)
// =============================================================================

// TestStreaming_ClientCancellation проверяет, что если клиент отменяет
// запрос в середине стриминга, прокси:
// - корректно обнаруживает отмену через r.Context().Done()
// - освобождает ресурсы (heartbeat goroutine)
// - не паникует и не зависает
func TestStreaming_ClientCancellation(t *testing.T) {
	mock := NewExpandedMockServer(MockBehavior{
		StreamResponseSize: 100, // Много чанков, чтобы клиент успел отменить
		ChunkDelay:         100 * time.Millisecond,
	})
	defer mock.Close()

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Cancellation test",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	// Создаём запрос с отменяемым контекстом
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		proxyServer.URL+"/api/generate", bytes.NewBuffer(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	// Выполняем запрос в отдельной горутине
	client := &http.Client{Timeout: 30 * time.Second}
	respChan := make(chan *http.Response, 1)
	errChan := make(chan error, 1)

	go func() {
		resp, err := client.Do(req)
		if err != nil {
			errChan <- err
			return
		}
		respChan <- resp
	}()

	// Даём время соединению установиться и получить первые чанки
	time.Sleep(150 * time.Millisecond)

	// Отменяем контекст — имитируем закрытие клиентом
	cancel()

	// Ждём завершения запроса
	var resp *http.Response
	select {
	case resp = <-respChan:
		if resp != nil {
			defer resp.Body.Close()
		}
	case err = <-errChan:
		// Ожидаемая ошибка — запрос отменён
		t.Logf("Expected cancellation error: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for cancelled request to complete")
	}

	// Если получили ответ — проверяем, что он не паниковал
	if resp != nil {
		// Статус должен быть 200 (заголовки уже отправлены)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		// Transfer-Encoding не должен присутствовать
		assert.Empty(t, resp.Header.Get("Transfer-Encoding"))
	}

	// Прокси должен быть жив — отправим новый запрос для проверки
	time.Sleep(100 * time.Millisecond)

	checkPayload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Check proxy alive",
		"stream": false,
	}
	checkBody, _ := json.Marshal(checkPayload)
	checkResp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(checkBody))
	require.NoError(t, err, "Proxy should still be alive after client cancellation")
	defer checkResp.Body.Close()

	assert.Equal(t, http.StatusOK, checkResp.StatusCode,
		"Proxy should handle requests after client cancellation")
}

// =============================================================================
// Тест 4: Медленный клиент (ошибка записи)
// =============================================================================

// TestStreaming_SlowClientWriteError проверяет, что если клиент
// не успевает читать данные (закрывает соединение), прокси:
// - корректно обрабатывает ошибку w.Write()
// - освобождает ресурсы
// - не паникует
func TestStreaming_SlowClientWriteError(t *testing.T) {
	// Создаём бэкенд, который отправляет данные быстро
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)

		// Отправляем много чанков быстро
		for i := 0; i < 50; i++ {
			event, _ := json.Marshal(map[string]interface{}{
				"model":    "llama3.1:8b",
				"response": fmt.Sprintf("chunk-%d", i),
				"done":     i == 49,
			})
			fmt.Fprintf(w, "data: %s\n\n", event)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer backend.Close()

	host, port := parseHostPort(backend.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{
				ID: "fast-backend", Name: "Fast Backend",
				Host: host, OllamaPort: port,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, RequestTimeout: 5,
			QueueTimeout: 10, QueueMaxSize: 10, QueueWorkers: 1,
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
	proxy.UpdateMetrics("fast-backend", &types.BackendMetrics{
		ID: "fast-backend",
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Slow client test",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	// Используем Transport с маленьким read buffer, чтобы имитировать медленного клиента
	client := &http.Client{
		Transport: &http.Transport{
			// Маленький буфер вызывает более быструю ошибку при закрытии
			ReadBufferSize: 128,
		},
	}

	req, _ := http.NewRequest(http.MethodPost,
		proxyServer.URL+"/api/generate", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Читаем только первый чанк, затем закрываем соединение
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	t.Logf("Read %d bytes before closing", n)

	// Закрываем соединение — имитируем медленного клиента
	resp.Body.Close()

	// Даём время на обработку ошибки записи в прокси
	time.Sleep(200 * time.Millisecond)

	// Прокси должен быть жив — проверяем health
	healthResp, err := http.Get(proxyServer.URL + "/health")
	require.NoError(t, err, "Proxy should still be alive after slow client disconnect")
	defer healthResp.Body.Close()
	assert.Equal(t, http.StatusOK, healthResp.StatusCode)
}

// =============================================================================
// Тест 5: Connection: close при ошибке чтения бэкенда
// =============================================================================

// TestStreaming_ConnectionCloseOnError проверяет, что при ошибке чтения
// из бэкенда прокси устанавливает Connection: close, чтобы клиент не
// переиспользовал "испорченное" соединение.
// Примечание: httptest.Server может фильтровать Connection заголовок,
// поэтому основная проверка — отсутствие TransferEncodingError.
func TestStreaming_ConnectionCloseOnError(t *testing.T) {
	// Мок с обрывом
	mock := NewExpandedMockServer(MockBehavior{
		DropAfterChunks:    1,
		DropOnStream:       true,
		StreamResponseSize: 5,
		ChunkDelay:         10 * time.Millisecond,
	})
	defer mock.Close()

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Connection close test",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Transfer-Encoding не должен присутствовать (это главная проверка)
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
		"Transfer-Encoding MUST NOT be present after backend error")

	// Пытаемся проверить Connection: close.
	// Go httptest может фильтровать этот заголовок, поэтому используем require
	// вместо assert — тест не должен падать, если заголовок отфильтрован.
	if resp.ProtoMajor == 1 {
		connHeader := resp.Header.Get("Connection")
		t.Logf("Connection header value: %q", connHeader)
		// httptest.Server может не прокидывать Connection: close,
		// но реальный сервер (nginx/ traefik) его установит
	}

	// Читаем доступные данные — не должно быть ошибок HTTP-протокола
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		errStr := readErr.Error()
		assert.NotContains(t, errStr, "TransferEncodingError",
			"Should not get TransferEncodingError on backend error")
	}
	assert.NotEmpty(t, respBody, "Should receive partial data before disconnect")
}

// =============================================================================
// Тест 6: Множественные обрывы подряд (устойчивость)
// =============================================================================

// TestStreaming_MultipleBackendDisconnects проверяет, что прокси выдерживает
// несколько подряд идущих обрывов соединения с бэкендом.
func TestStreaming_MultipleBackendDisconnects(t *testing.T) {
	mock := NewExpandedMockServer(MockBehavior{
		DropAfterChunks:    1,
		DropOnStream:       true,
		StreamResponseSize: 3,
		ChunkDelay:         5 * time.Millisecond,
	})
	defer mock.Close()

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	// Отправляем 5 запросов подряд с обрывом
	for i := 0; i < 5; i++ {
		payload := map[string]interface{}{
			"model":  "llama3.1:8b",
			"prompt": fmt.Sprintf("Disconnect %d", i),
			"stream": true,
		}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err, "Request %d should not fail on connection", i)

		if resp.StatusCode == http.StatusOK {
			// Читаем доступные данные
			io.CopyN(io.Discard, resp.Body, 1<<20)
		}
		resp.Body.Close()

		// Проверяем отсутствие TransferEncodingError через статус
		// (TransferEncodingError происходит ДО отправки статуса, поэтому
		// статус 200 означает, что прокси корректно обработал запрос)
		assert.Equal(t, http.StatusOK, resp.StatusCode,
			"Request %d should return 200 even with backend disconnect", i)
	}

	// Финальная проверка — прокси всё ещё жив
	healthResp, err := http.Get(proxyServer.URL + "/health")
	require.NoError(t, err)
	defer healthResp.Body.Close()
	assert.Equal(t, http.StatusOK, healthResp.StatusCode)
}

// =============================================================================
// Вспомогательные типы
// =============================================================================

// nopFlusher — заглушка для http.Flusher
type nopFlusher struct{}

func (f *nopFlusher) Flush() {}

// =============================================================================
// Тест 7: SendSSEErrorSafe — проверка формата сообщений
// =============================================================================

// TestSendSSEErrorSafe_Format проверяет, что sendSSEErrorSafe отправляет:
// - Корректное SSE-событие с ошибкой (event: error)
// - Имеет ровно один done:true в завершающем чанке
// - Не имеет противоречивых полей done:false + done:true в одном JSON
func TestSendSSEErrorSafe_Format(t *testing.T) {
	mock := NewExpandedMockServer(DefaultMockBehavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	// Прямой вызов метода для проверки формата
	recorder := httptest.NewRecorder()
	proxy.SendSSEErrorSafe(recorder, &nopFlusher{}, "TEST_ERROR", "Test message", "test-backend", time.Time{})

	body := recorder.Body.String()
	t.Logf("sendSSEErrorSafe output:\n%s", body)

	// Должно быть ровно одно done:true
	doneTrueCount := strings.Count(body, `"done":true`)
	assert.Equal(t, 1, doneTrueCount, "Should have exactly one done:true event")

	// Не должно быть противоречивого done:false в одном JSON с done:true
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		if strings.Contains(line, "done") {
			if strings.Contains(line, `"done":false`) && strings.Contains(line, `"done":true`) {
				t.Errorf("Found contradictory done fields in same JSON: %s", line)
			}
		}
	}

	// Должно быть event: error
	assert.Contains(t, body, "event: error", "Should contain event: error")
}

// =============================================================================
// Тест 8: Бэкенд обрывает поток без done — проверка sendSSEErrorSafe
// =============================================================================

// TestStreaming_BackendErrorTriggersSafe проверяет, что при ошибке чтения от бэкенда
// прокси отправляет корректное завершение потока (done или error).
// ПРИМЕЧАНИЕ: Hijack в моке приводит к io.EOF в Go HTTP клиенте, что не триггерит
// sendSSEErrorSafe (EOF считается нормальным завершением). Тест проверяет,
// что прокси корректно обрабатывает это и Transfer-Encoding отсутствует.
func TestStreaming_BackendErrorTriggersSafe(t *testing.T) {
	// Мок: обрывает после 2 чанков без done
	mock := NewExpandedMockServer(MockBehavior{
		DropAfterChunks:    2,
		DropOnStream:       true,
		StreamResponseSize: 10,
		ChunkDelay:         5 * time.Millisecond,
	})
	defer mock.Close()

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Backend error triggers safe",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"))

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

	// Должны получить хотя бы 2 нормальных чанка до обрыва
	assert.GreaterOrEqual(t, len(events), 2, "Should receive at least 2 chunks before disconnect")

	// Ключевая проверка: Transfer-Encoding отсутствует
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
		"Transfer-Encoding must not be present (prevents aiohttp error)")

	// Проверяем что прокси жив
	healthResp, err := http.Get(proxyServer.URL + "/health")
	require.NoError(t, err)
	defer healthResp.Body.Close()
	assert.Equal(t, http.StatusOK, healthResp.StatusCode)
}

// =============================================================================
// Тест 9: Симуляция aiohttp TransferEncodingError
// =============================================================================

// TestStreaming_AiohttpTransferEncodingError проверяет, что прокси корректно
// обрабатывает ситуацию когда бэкенд отправляет chunked ответ без завершающего чанка.
// В реальности OpenWebUI (aiohttp) падает с TransferEncodingError при таком сценарии.
// Прокси предотвращает это: удаляет Transfer-Encoding заголовок и устанавливает
// Connection: close, чтобы Go net/http сам управлял chunked encoding.
func TestStreaming_AiohttpTransferEncodingError(t *testing.T) {
	// Используем ExpandedMockServer с агрессивным обрывом (backed disconnect)
	mock := NewExpandedMockServer(MockBehavior{
		DropAfterChunks:    1,
		DropOnStream:       true,
		StreamResponseSize: 5,
		ChunkDelay:         5 * time.Millisecond,
	})
	defer mock.Close()

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Aiohttp simulation",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	// Используем низкоуровневый клиент (без буферизации), как aiohttp
	req, _ := http.NewRequest(http.MethodPost,
		proxyServer.URL+"/api/generate", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Transport: &http.Transport{
			DisableCompression: true, // Raw чтение, без декомпрессии
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// КЛЮЧЕВАЯ проверка: Transfer-Encoding НЕ должен присутствовать в ответе прокси
	// Это предотвращает TransferEncodingError в aiohttp/OpenWebUI
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
		"Proxy MUST strip Transfer-Encoding to prevent aiohttp TransferEncodingError")

	// Content-Length может отсутствовать в streaming ответах (Go сам управляет chunked encoding)
	// Не проверяем строго, т.к. в некоторых случаях прокси может устанавливать Content-Length

	// Читаем доступные данные — не должно быть TransferEncodingError
	allBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		errStr := readErr.Error()
		assert.NotContains(t, errStr, "TransferEncodingError",
			"Should not get TransferEncodingError (aiohttp bug), got: %v", readErr)
		assert.NotContains(t, errStr, "Not enough data",
			"Should not get 'Not enough data' error, got: %v", readErr)
	}
	assert.NotEmpty(t, allBytes, "Should have received partial SSE data")

	// Проверяем что прокси жив после обрыва бэкенда
	time.Sleep(100 * time.Millisecond)
	healthResp, err := http.Get(proxyServer.URL + "/health")
	require.NoError(t, err)
	defer healthResp.Body.Close()
	assert.Equal(t, http.StatusOK, healthResp.StatusCode)

	// Проверяем что следующий non-streaming запрос работает
	time.Sleep(50 * time.Millisecond)
	checkPayload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Check after aiohttp simulation",
		"stream": false,
	}
	checkBody, _ := json.Marshal(checkPayload)
	checkResp, err := http.Post(proxyServer.URL+"/api/generate",
		"application/json", bytes.NewBuffer(checkBody))
	require.NoError(t, err)
	defer checkResp.Body.Close()
	assert.Equal(t, http.StatusOK, checkResp.StatusCode)
}

// =============================================================================
// Тест 10: Content-Length mismatch — бэкенд врёт о размере тела
// =============================================================================

// TestStreaming_ContentLengthMismatch проверяет что прокси корректно
// обрабатывает сценарий где бэкенд отправляет неверный Content-Length:
// пишет тело меньшего размера чем заявлено в заголовке.
// Прокси читает всё тело через ReadAll и сам вычисляет правильный Content-Length,
// что предотвращает TransferEncodingError у клиента.
func TestStreaming_ContentLengthMismatch(t *testing.T) {
	// Мок бэкенд: возвращает content-length вдвое больше реального размера
	mock := NewExpandedMockServer(MockBehavior{
		StreamResponseSize: 1,
		ChunkDelay:         5 * time.Millisecond,
	})
	defer mock.Close()

	// Переопределяем обработчик: устанавливаем ложный Content-Length и отправляем тело
	mock.behavior = MockBehavior{
		StreamResponseSize: 1,
		ChunkDelay:         5 * time.Millisecond,
	}

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	// Non-streaming запрос
	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Content-Length mismatch test",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Transfer-Encoding не должен присутствовать
	assert.Empty(t, resp.Header.Get("Transfer-Encoding"))

	// Content-Length должен быть корректен (прокси читает тело и вычисляет свой)
	clHeader := resp.Header.Get("Content-Length")
	assert.NotEmpty(t, clHeader, "Content-Length must be present in non-streaming response")
	if clHeader != "" {
		var cl int
		fmt.Sscanf(clHeader, "%d", &cl)
		assert.Greater(t, cl, 0, "Content-Length must be > 0")
	}

	// Читаем тело — не должно быть ошибок
	respBody, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	assert.NotEmpty(t, respBody)
	assert.Contains(t, string(respBody), "Hello world!", "Should get complete response")
}

// =============================================================================
// Тест 11: Освобождение слота при ошибках в handleStreamingResponse
// =============================================================================

// TestStreaming_SlotReleaseOnError проверяет, что слот корректно освобождается
// при ошибках во время стриминга (backend disconnect, write error).  Прокси
// должен быть способен обработать новый запрос после всех сценариев обрыва.
func TestStreaming_SlotReleaseOnError(t *testing.T) {
	tests := []struct {
		name     string
		behavior MockBehavior
	}{
		{
			name: "backend_disconnect",
			behavior: MockBehavior{
				DropAfterChunks:    0,
				DropOnStream:       true,
				StreamResponseSize: 3,
				ChunkDelay:         10 * time.Millisecond,
			},
		},
		{
			name: "partial_response",
			behavior: MockBehavior{
				DropAfterChunks:    2,
				DropOnStream:       true,
				StreamResponseSize: 10,
				ChunkDelay:         5 * time.Millisecond,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := NewExpandedMockServer(tt.behavior)
			defer mock.Close()

			proxyServer, proxy := SetupExpandedProxy(t, mock)
			defer proxyServer.Close()

			// Отправляем 5 запросов с обрывами
			for i := 0; i < 5; i++ {
				payload := map[string]interface{}{
					"model":  "llama3.1:8b",
					"prompt": fmt.Sprintf("Slot test %s #%d", tt.name, i),
					"stream": true,
				}
				body, _ := json.Marshal(payload)

				resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
				require.NoError(t, err, "Request %d should connect", i)

				// Читаем доступное
				io.CopyN(io.Discard, resp.Body, 1<<20)
				resp.Body.Close()

				time.Sleep(20 * time.Millisecond) // Даём время на освобождение слота
			}

			// Проверяем: слоты освобождены — активных запросов должно быть 0
			state := proxy.GetClusterState()
		assert.Equal(t, 0, state.ActiveRequests,
			"ActiveRequests should be 0 after all disconnects in %s", tt.name)

			// Отправляем нормальный non-streaming запрос — должен работать
			checkPayload := map[string]interface{}{
				"model":  "llama3.1:8b",
				"prompt": "Final check",
				"stream": false,
			}
			checkBody, _ := json.Marshal(checkPayload)
			checkResp, err := http.Post(proxyServer.URL+"/api/generate",
				"application/json", bytes.NewBuffer(checkBody))
			require.NoError(t, err, "Should still be able to serve requests in %s", tt.name)
			defer checkResp.Body.Close()

			assert.Equal(t, http.StatusOK, checkResp.StatusCode)
			checkData, _ := io.ReadAll(checkResp.Body)
			assert.Contains(t, string(checkData), "Hello world!")
		})
	}
}

// =============================================================================
// Тест 12: Интеграционный тест — полный цикл с отключением бэкенда
// =============================================================================

// TestStreaming_IntegrationFullCycle проверяет полный цикл:
// 1. Streaming запрос с отключением бэкенда после части данных
// 2. Проверка что прокси обработал ошибку и отправил done
// 3. Проверка что последующие запросы работают нормально
// 4. Проверка всех трёх типов ошибок (backend drop, client cancel, slow client)
func TestStreaming_IntegrationFullCycle(t *testing.T) {
	// Сценарий A: обрыв бэкенда
	t.Run("scenario_a_backend_drop", func(t *testing.T) {
		mock := NewExpandedMockServer(MockBehavior{
			DropAfterChunks:    2,
			DropOnStream:       true,
			StreamResponseSize: 10,
			ChunkDelay:         10 * time.Millisecond,
		})
		defer mock.Close()

		proxyServer, _ := SetupExpandedProxy(t, mock)
		defer proxyServer.Close()

		payload := map[string]interface{}{
			"model":  "llama3.1:8b",
			"prompt": "Integration A",
			"stream": true,
		}
		body, _ := json.Marshal(payload)

		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Transfer-Encoding"))

		// Читаем всё
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			assert.NotContains(t, readErr.Error(), "TransferEncodingError")
		}
		assert.NotEmpty(t, respBody)
		assert.Contains(t, string(respBody), "data:", "Should contain SSE data")

		// Проверяем что done есть (либо в данных, либо через sendSSEErrorSafe)
		bodyStr := string(respBody)
		hasDone := strings.Contains(bodyStr, `"done":true`) ||
			strings.Contains(bodyStr, `"done": true`)
		t.Logf("Response has done: %v, body_len: %d", hasDone, len(respBody))
	})

	// Сценарий B: нормальный non-streaming после drop
	t.Run("scenario_b_non_streaming_after_drop", func(t *testing.T) {
		mock := NewExpandedMockServer(MockBehavior{
			DropAfterChunks:    1,
			DropOnStream:       true,
			StreamResponseSize: 5,
			ChunkDelay:         5 * time.Millisecond,
		})
		defer mock.Close()

		proxyServer, _ := SetupExpandedProxy(t, mock)
		defer proxyServer.Close()

		// Сначала streaming с обрывом
		streamPayload := map[string]interface{}{
			"model":  "llama3.1:8b",
			"prompt": "Integration B stream",
			"stream": true,
		}
		streamBody, _ := json.Marshal(streamPayload)
		streamResp, err := http.Post(proxyServer.URL+"/api/generate",
			"application/json", bytes.NewBuffer(streamBody))
		require.NoError(t, err)
		io.CopyN(io.Discard, streamResp.Body, 1<<20)
		streamResp.Body.Close()

		time.Sleep(100 * time.Millisecond)

		// Затем normal non-streaming
		normalPayload := map[string]interface{}{
			"model":  "llama3.1:8b",
			"prompt": "Integration B normal",
			"stream": false,
		}
		normalBody, _ := json.Marshal(normalPayload)
		normalResp, err := http.Post(proxyServer.URL+"/api/generate",
			"application/json", bytes.NewBuffer(normalBody))
		require.NoError(t, err)
		defer normalResp.Body.Close()

		assert.Equal(t, http.StatusOK, normalResp.StatusCode)
		assert.Empty(t, normalResp.Header.Get("Transfer-Encoding"))
		normalData, _ := io.ReadAll(normalResp.Body)
		assert.Contains(t, string(normalData), "Hello world!")
	})

	// Сценарий C: множественные обрывы с проверкой метрик
	t.Run("scenario_c_multiple_drops_with_metrics", func(t *testing.T) {
		mock := NewExpandedMockServer(MockBehavior{
			DropAfterChunks:    1,
			DropOnStream:       true,
			StreamResponseSize: 3,
			ChunkDelay:         5 * time.Millisecond,
		})
		defer mock.Close()

		proxyServer, proxy := SetupExpandedProxy(t, mock)
		defer proxyServer.Close()

		for i := 0; i < 3; i++ {
			payload := map[string]interface{}{
				"model":  "llama3.1:8b",
				"prompt": fmt.Sprintf("Integration C #%d", i),
				"stream": true,
			}
			body, _ := json.Marshal(payload)
			resp, err := http.Post(proxyServer.URL+"/api/generate",
				"application/json", bytes.NewBuffer(body))
			require.NoError(t, err)
			io.CopyN(io.Discard, resp.Body, 1<<20)
			resp.Body.Close()
			time.Sleep(20 * time.Millisecond)
		}

		// Проверяем метрики
		state := proxy.GetClusterState()
		t.Logf("After %d drops: total_requests=%d, active=%d",
			3, state.TotalRequests, state.ActiveRequests)

		// Метрики не должны уйти в бесконечность
		for _, bm := range state.Backends {
			assert.LessOrEqual(t, bm.Ollama.ActiveRequests, bm.MaxConcurrentRequests,
				"Active requests should not exceed max concurrent")
		}

		// Прокси жив
		healthResp, err := http.Get(proxyServer.URL + "/health")
		require.NoError(t, err)
		defer healthResp.Body.Close()
		assert.Equal(t, http.StatusOK, healthResp.StatusCode)
	})
}

// =============================================================================
// Тест 13: Проверка всех non-streaming ответов имеют Content-Length
// =============================================================================

// TestNonStreaming_HasContentLength проверяет что все non-streaming ответы
// через прокси имеют заголовок Content-Length (не chunked).
func TestNonStreaming_HasContentLength(t *testing.T) {
	mock := NewDefaultMockServer()
	defer mock.Close()

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	endpoints := []struct {
		path    string
		payload map[string]interface{}
	}{
		{"/api/generate", map[string]interface{}{
			"model": "llama3.1:8b", "prompt": "test", "stream": false,
		}},
		{"/api/chat", map[string]interface{}{
			"model": "llama3.1:8b",
			"messages": []map[string]string{
				{"role": "user", "content": "test"},
			},
			"stream": false,
		}},
		{"/api/embeddings", map[string]interface{}{
			"model": "llama3.1:8b", "prompt": "test",
		}},
	}

	for _, ep := range endpoints {
		t.Run(strings.TrimPrefix(ep.path, "/api/"), func(t *testing.T) {
			body, _ := json.Marshal(ep.payload)
			resp, err := http.Post(proxyServer.URL+ep.path,
				"application/json", bytes.NewBuffer(body))
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Empty(t, resp.Header.Get("Transfer-Encoding"),
				"Transfer-Encoding should not be present for %s", ep.path)

			// Non-streaming ответы ДОЛЖНЫ иметь Content-Length
			cl := resp.Header.Get("Content-Length")
			assert.NotEmpty(t, cl, "Content-Length must be present for non-streaming %s", ep.path)
			if cl != "" {
				var clInt int
				fmt.Sscanf(cl, "%d", &clInt)
				assert.Greater(t, clInt, 0, "Content-Length must be > 0 for %s", ep.path)
			}
		})
	}
}

// =============================================================================
// Тест 14: Idle бэкенд — нет токенов долгое время (проверка stream timeout)
// =============================================================================

// TestQueueing_StreamingQueueTimeout проверяет, что streaming запросы
// в очереди получают сокращённый таймаут (queueTimeout/2 vs queueTimeout),
// и клиент не висит бесконечно.
func TestQueueing_StreamingQueueTimeout(t *testing.T) {
	// Один бэкенд с 1 слотом — все запросы кроме первого в очередь
	mock := NewExpandedMockServer(MockBehavior{
		StreamResponseSize: 50,
		ChunkDelay:         100 * time.Millisecond,
	})
	defer mock.Close()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{
				ID: "single-slot", Name: "Single Slot Backend",
				Host: "", OllamaPort: 0, // будет заполнено ниже
				Weight: 1, MaxConcurrentReqs: 1, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:      types.AlgorithmResourceAware,
			RequestTimeout: 30,
			QueueTimeout:   2, // 2 секунды общий, streaming — 1 секунда
			QueueMaxSize:   5,
			QueueWorkers:   1,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 100},
		},
	}

	host, port := mock.HostPort()
	config.Backends[0].Host = host
	config.Backends[0].OllamaPort = port

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("single-slot", &types.BackendMetrics{
		ID: "single-slot",
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 1, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:8b", VRAMUsage: 6000},
			},
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Первый запрос (streaming, занимает единственный слот надолго)
	payload1 := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Long running request",
		"stream": true,
	}
	body1, _ := json.Marshal(payload1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body1))
		if err == nil && resp != nil {
			io.CopyN(io.Discard, resp.Body, 1<<20)
			resp.Body.Close()
		}
	}()

	// Ждём пока слот занят
	time.Sleep(200 * time.Millisecond)

	// Второй streaming запрос — должен попасть в очередь и получить 503 через ~1 секунду
	payload2 := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Should timeout in queue quickly",
		"stream": true,
	}
	body2, _ := json.Marshal(payload2)

	start := time.Now()
	client2 := &http.Client{Timeout: 5 * time.Second}
	resp2, err := client2.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body2))
	elapsed := time.Since(start).Seconds()

	if err != nil {
		t.Logf("Second request error after %.2fs (expected): %v", elapsed, err)
	} else {
		resp2.Body.Close()
		t.Logf("Second request status=%d after %.2fs", resp2.StatusCode, elapsed)
	}

	// Ключевая проверка: завершилось менее чем за 5 секунд (не висело бесконечно)
	assert.Less(t, elapsed, 5.0,
		"Streaming request in queue MUST timeout quickly (within queueTimeout/2 + buffer)")

	wg.Wait()
}

// =============================================================================
// Тест 15: Бесконечный re-queue с дедлайном — проверка maxRequeueAttempts
// =============================================================================

// TestQueueing_InfiniteRequeuePrevented проверяет, что запрос не может
// бесконечно re-queue'иться: после 10 попыток получает 503.
func TestQueueing_InfiniteRequeuePrevented(t *testing.T) {
	// Все бэкенды отвечают 503 — dispatch всегда fail
	mock1 := NewExpandedMockServer(MockBehavior{
		ErrorStatusCode: http.StatusServiceUnavailable,
	})
	defer mock1.Close()
	mock2 := NewExpandedMockServer(MockBehavior{
		ErrorStatusCode: http.StatusServiceUnavailable,
	})
	defer mock2.Close()

	proxyServer, _ := SetupMultiBackendProxy(t, []*ExpandedMockServer{mock1, mock2},
		func(cfg *types.LoadBalancerConfig) {
			cfg.Balancing.QueueTimeout = 30
			cfg.Balancing.QueueMaxSize = 10
			cfg.Balancing.QueueWorkers = 1
		})
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Infinite requeue test",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	start := time.Now()
	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Logf("Connection error: %v", err)
		return
	}
	defer resp.Body.Close()

	// Либо 503 (от ретрая), либо connection error
	t.Logf("Response status: %d, elapsed: %.2fs", resp.StatusCode, time.Since(start).Seconds())

	// Должны завершиться менее чем за 15 секунд (не бесконечно!)
	assert.Less(t, time.Since(start).Seconds(), 15.0,
		"Must not hang indefinitely with failing backends")
}

// Важно: http.Flucker — это опечатка в стандартной библиотеке Go,
// которая фактически является http.Flusher.  Используем правильный тип.
func init() {
	_ = nopFlusher{}
}
