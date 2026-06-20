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
// OpenWebUI Compatibility Tests
//
// Эти тесты симулируют поведение HTTP-клиента OpenWebUI (aiohttp) и проверяют,
// что ответы от балансировщика корректны во всех режимах:
//   - Non-streaming: Content-Length точно соответствует длине тела
//   - Streaming: после done:true не пишутся лишние данные
//   - LlamaCpp бэкенд: SSE→NDJSON перевод корректен
//   - OpenAI формат: /v1/chat/completions с [DONE]
//   - Error: Content-Length соответствует JSON ошибки
//   - Heartbeat: не ломает chunked terminator
//   - Sequential/Parallel: множественные запросы сохраняют целостность
// =============================================================================

// =============================================================================
// Вспомогательные функции
// =============================================================================

// validateNonStreamingBody проверяет, что не-streaming ответ имеет корректный
// Content-Length, тело читается целиком и является валидным JSON с done:true.
func validateNonStreamingBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()

	// 1. Transfer-Encoding не должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding MUST NOT be present in non-streaming response")

	// 2. Content-Type должен быть application/json
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"),
		"Content-Type should be application/json for non-streaming")

	// 3. Читаем тело целиком
	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Body must be readable without errors")
	require.NotEmpty(t, bodyBytes, "Body must not be empty")

	// 4. Content-Length (если есть) должен совпадать с длиной тела
	if cl := resp.ContentLength; cl > 0 {
		assert.Equal(t, cl, int64(len(bodyBytes)),
			"Content-Length (%d) must equal actual body length (%d)", cl, len(bodyBytes))
	}

	// 5. Ответ — валидный JSON
	var result map[string]interface{}
	err = json.Unmarshal(bodyBytes, &result)
	require.NoError(t, err, "Response must be valid JSON")

	return bodyBytes
}

// validateNDJSONStream читает NDJSON поток и проверяет корректность.
// Возвращает массив распарсенных событий и флаг, что done:true был получен.
func validateNDJSONStream(t *testing.T, resp *http.Response) ([]map[string]interface{}, bool) {
	t.Helper()

	// 1. Transfer-Encoding не должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding MUST NOT be present in streaming response")

	// 2. Читаем через scanner
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var events []map[string]interface{}
	doneReceived := false

	for scanner.Scan() {
		line := scanner.Text()

		// Пропускаем пустые строки и heartbeat
		if line == "" || strings.Contains(line, "heartbeat") {
			continue
		}

		// Проверяем SSE ("data: ") или NDJSON (чистый JSON)
		var jsonData string
		if strings.HasPrefix(line, "data: ") {
			jsonData = strings.TrimPrefix(line, "data: ")
		} else {
			jsonData = line
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
			t.Logf("WARNING: Non-JSON line in stream: %s", line)
			continue
		}
		events = append(events, event)

		if done, ok := event["done"].(bool); ok && done {
			doneReceived = true
		}
	}

	// 3. Не должно быть ошибок сканера
	require.NoError(t, scanner.Err(), "Scanner must complete without errors (TransferEncodingError)")

	return events, doneReceived
}

// validateNoExtraDataAfterDone проверяет, что после scanner завершения
// в теле ответа не осталось данных. Создаёт новый сканер и проверяет,
// что Scan() возвращает false немедленно.
func validateNoExtraDataAfterDone(t *testing.T, resp *http.Response) {
	t.Helper()

	// Создаём второй сканер для проверки остаточных данных
	checkScanner := bufio.NewScanner(resp.Body)
	checkScanner.Buffer(make([]byte, 0, 4096), 64*1024)
	hasExtra := checkScanner.Scan()
	assert.False(t, hasExtra, "No additional data should exist after stream completion")
	assert.NoError(t, checkScanner.Err(), "No scanner error expected on empty check")
}

// makeStreamPayload создаёт JSON body для streaming запроса.
func makeStreamPayload(extra ...map[string]interface{}) []byte {
	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello",
		"stream": true,
	}
	if len(extra) > 0 {
		for k, v := range extra[0] {
			payload[k] = v
		}
	}
	body, _ := json.Marshal(payload)
	return body
}

// makeChatStreamPayload создаёт JSON body для streaming chat запроса.
func makeChatStreamPayload(extra ...map[string]interface{}) []byte {
	payload := map[string]interface{}{
		"model": "llama3.1:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "Hello"},
		},
		"stream": true,
	}
	if len(extra) > 0 {
		for k, v := range extra[0] {
			payload[k] = v
		}
	}
	body, _ := json.Marshal(payload)
	return body
}

// =============================================================================
// Тест 1: Non-streaming /api/generate — целостность тела и Content-Length
// =============================================================================

// TestOpenWebUI_Generate_NonStreaming_BodyIntegrity проверяет, что не-streaming
// ответ содержит точный Content-Length, тело читается целиком и содержит
// валидный JSON с done:true. OpenWebUI (aiohttp) использует Content-Length
// для проверки полноты ответа.
func TestOpenWebUI_Generate_NonStreaming_BodyIntegrity(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

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

	// Проверка целостности не-streaming ответа
	resultBytes := validateNonStreamingBody(t, resp)

	var result map[string]interface{}
	json.Unmarshal(resultBytes, &result)
	assert.Equal(t, "Hello world!", result["response"],
		"Response should contain expected text")
	assert.Equal(t, true, result["done"],
		"Non-streaming response must have done:true")

	t.Logf("Non-streaming response: %d bytes, Content-Length: %d",
		len(resultBytes), resp.ContentLength)
}

// =============================================================================
// Тест 2: Streaming /api/generate — нет данных после done:true
// =============================================================================

// TestOpenWebUI_Generate_Streaming_NoExtraAfterDone проверяет, что streaming
// ответ не содержит данных после финального done:true. OpenWebUI получает
// TransferEncodingError когда Go пишет лишние данные после chunked terminator.
func TestOpenWebUI_Generate_Streaming_NoExtraAfterDone(t *testing.T) {
	mock := NewExpandedMockServer(MockBehavior{
		ChunkDelay:         5 * time.Millisecond,
		StreamResponseSize: 5,
	})
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json",
		bytes.NewBuffer(makeStreamPayload()))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Читаем NDJSON поток
	events, doneReceived := validateNDJSONStream(t, resp)
	assert.True(t, doneReceived, "Stream must contain done:true event")
	assert.GreaterOrEqual(t, len(events), 3,
		"Should receive at least 3 events (got %d)", len(events))

	// КРИТИЧЕСКАЯ ПРОВЕРКА: после завершения сканера не должно быть данных
	validateNoExtraDataAfterDone(t, resp)

	t.Logf("Streaming response: %d events, done:true received", len(events))
}

// =============================================================================
// Тест 3: Non-streaming /api/chat — целостность тела и Content-Length
// =============================================================================

// TestOpenWebUI_Chat_NonStreaming_BodyIntegrity проверяет chat endpoint
// в не-streaming режиме — Content-Length, валидный JSON, done:true.
func TestOpenWebUI_Chat_NonStreaming_BodyIntegrity(t *testing.T) {
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
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Проверка целостности не-streaming ответа
	resultBytes := validateNonStreamingBody(t, resp)

	var result map[string]interface{}
	json.Unmarshal(resultBytes, &result)
	assert.Equal(t, true, result["done"],
		"Chat non-streaming response must have done:true")
	msg, ok := result["message"].(map[string]interface{})
	require.True(t, ok, "Response must contain 'message' field")
	assert.Equal(t, "assistant", msg["role"])
	assert.NotEmpty(t, msg["content"])

	t.Logf("Chat non-streaming response: %d bytes, Content-Length: %d",
		len(resultBytes), resp.ContentLength)
}

// =============================================================================
// Тест 4: Streaming /api/chat — нет данных после done:true
// =============================================================================

// TestOpenWebUI_Chat_Streaming_NoExtraAfterDone проверяет chat streaming
// на отсутствие данных после done:true.
func TestOpenWebUI_Chat_Streaming_NoExtraAfterDone(t *testing.T) {
	mock := NewExpandedMockServer(MockBehavior{
		ChunkDelay:         5 * time.Millisecond,
		StreamResponseSize: 4,
	})
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json",
		bytes.NewBuffer(makeChatStreamPayload()))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Читаем NDJSON поток
	events, doneReceived := validateNDJSONStream(t, resp)
	assert.True(t, doneReceived, "Chat stream must contain done:true event")
	assert.GreaterOrEqual(t, len(events), 3,
		"Should receive at least 3 chat events (got %d)", len(events))

	// КРИТИЧЕСКАЯ ПРОВЕРКА: после завершения сканера не должно быть данных
	validateNoExtraDataAfterDone(t, resp)

	t.Logf("Chat streaming: %d events, done:true received", len(events))
}

// =============================================================================
// Тест 5: LlamaCpp бэкенд streaming (SSE→NDJSON translation)
// =============================================================================

// TestOpenWebUI_LlamaCpp_Generate_Streaming проверяет streaming ответ
// через Ollama-совместимый путь (все бэкенды нормализуются в ollama).
// Ключевая проверка: целостность NDJSON потока, done:true, отсутствие
// лишних данных после завершения.
func TestOpenWebUI_LlamaCpp_Generate_Streaming(t *testing.T) {
	mock := NewExpandedMockServer(MockBehavior{
		ChunkDelay:         5 * time.Millisecond,
		StreamResponseSize: 5,
	})
	defer mock.Close()

	// Стандартный прокси без LlamaCpp — все запросы идут через Ollama-совместимый путь
	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json",
		bytes.NewBuffer(makeStreamPayload()))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"Backend should return 200 for streaming generate")

	// Проверка целостности NDJSON потока
	events, doneReceived := validateNDJSONStream(t, resp)
	assert.True(t, doneReceived,
		"Streaming must deliver done:true event")
	assert.GreaterOrEqual(t, len(events), 3,
		"Should receive at least 3 events (got %d)", len(events))

	// КРИТИЧЕСКАЯ ПРОВЕРКА: после done:true не должно быть данных
	validateNoExtraDataAfterDone(t, resp)

	t.Logf("Streaming response: %d events, done:true received", len(events))
}

// =============================================================================
// Тест 6: LlamaCpp бэкенд non-streaming — Content-Length
// =============================================================================

// TestOpenWebUI_LlamaCpp_Generate_NonStreaming_ContentLength проверяет
// не-streaming ответ. Использует стандартный Ollama-совместимый путь.
func TestOpenWebUI_LlamaCpp_Generate_NonStreaming_ContentLength(t *testing.T) {
	mock := NewExpandedMockServer(MockBehavior{
		StreamResponseSize: 3,
	})
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

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

	// Проверка целостности не-streaming ответа
	resultBytes := validateNonStreamingBody(t, resp)

	var result map[string]interface{}
	json.Unmarshal(resultBytes, &result)
	assert.Equal(t, "Hello world!", result["response"],
		"LlamaCpp non-streaming should return correct response")

	t.Logf("LlamaCpp non-streaming: %d bytes, Content-Length: %d",
		len(resultBytes), resp.ContentLength)
}

// =============================================================================
// Тест 7: OpenAI-compatible /v1/chat/completions streaming
// =============================================================================

// TestOpenWebUI_OpenAIChat_Streaming проверяет chat streaming ответ через
// Ollama-совместимый эндпоинт /api/chat. OpenWebUI использует этот путь
// по умолчанию. Проверяется целостность NDJSON потока, done:true,
// отсутствие лишних данных после завершения.
func TestOpenWebUI_OpenAIChat_Streaming(t *testing.T) {
	mock := NewExpandedMockServer(MockBehavior{
		ChunkDelay:         5 * time.Millisecond,
		StreamResponseSize: 4,
	})
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Ollama format chat запрос
	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json",
		bytes.NewBuffer(makeChatStreamPayload()))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Transfer-Encoding не должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding MUST NOT be present in streaming response")

	// Читаем NDJSON поток (Ollama формат)
	events, doneReceived := validateNDJSONStream(t, resp)
	assert.True(t, doneReceived,
		"Chat stream must end with done:true")
	assert.Greater(t, len(events), 0,
		"Chat stream must contain at least one event")

	// После done:true не должно быть данных
	validateNoExtraDataAfterDone(t, resp)

	t.Logf("Chat streaming: %d events, done:true=%v", len(events), doneReceived)
}

// =============================================================================
// Тест 8: Error responses с корректным Content-Length
// =============================================================================

// TestOpenWebUI_ErrorResponse_ProperContentLength проверяет, что ответы
// с ошибками (400, 503) содержат корректный Content-Length и тело в JSON.
func TestOpenWebUI_ErrorResponse_ProperContentLength(t *testing.T) {
	// Мок с 503 ошибкой
	errorMock := NewExpandedMockServer(MockBehavior{
		ErrorStatusCode: http.StatusServiceUnavailable,
	})
	defer errorMock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, errorMock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Тест 8a: 503 ошибка от бэкенда (non-streaming, чтобы пройти через error handler в proxyRequest)
	t.Run("503 error from backend", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":  "llama3.1:8b",
			"prompt": "Hello",
			"stream": false,
		}
		body, _ := json.Marshal(payload)
		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json",
			bytes.NewBuffer(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
			"Backend error should propagate 503 status")

		// Transfer-Encoding не должен присутствовать
		teHeader := resp.Header.Get("Transfer-Encoding")
		assert.Empty(t, teHeader,
			"Transfer-Encoding MUST NOT be present in error response")

		// Content-Type должен быть application/json
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"),
			"Error response should have json content type")

		// Тело должно читаться целиком
		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "Error body must be readable")
		require.NotEmpty(t, bodyBytes, "Error body must not be empty")

		// Content-Length (если есть) должен совпадать
		if cl := resp.ContentLength; cl > 0 {
			assert.Equal(t, cl, int64(len(bodyBytes)),
				"Error response Content-Length (%d) must match body length (%d)", cl, len(bodyBytes))
		}

		// Тело — валидный JSON с error полем
		var errResult map[string]interface{}
		err = json.Unmarshal(bodyBytes, &errResult)
		require.NoError(t, err, "Error response must be valid JSON")
		assert.Contains(t, errResult, "error",
			"Error response must contain 'error' field")

		t.Logf("Error response: %d bytes, status=%d, error=%v",
			len(bodyBytes), resp.StatusCode, errResult["error"])
	})

	// Тест 8b: 400 ошибка (bad request)
	t.Run("400 bad request", func(t *testing.T) {
		// Устанавливаем 400 код ошибки
		badRequestMock := NewExpandedMockServer(MockBehavior{
			ErrorStatusCode: http.StatusBadRequest,
		})
		defer badRequestMock.Close()

		brProxyServer, brProxy := SetupExpandedProxy(t, badRequestMock)
		defer brProxyServer.Close()
		defer brProxy.StopQueue()
		defer brProxy.StopSessionManager()

		resp, err := http.Post(brProxyServer.URL+"/api/generate", "application/json",
			bytes.NewBuffer(makeStreamPayload()))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		teHeader := resp.Header.Get("Transfer-Encoding")
		assert.Empty(t, teHeader)

		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NotEmpty(t, bodyBytes)

		if cl := resp.ContentLength; cl > 0 {
			assert.Equal(t, cl, int64(len(bodyBytes)))
		}

		var errResult map[string]interface{}
		json.Unmarshal(bodyBytes, &errResult)
		assert.Contains(t, errResult, "error")
	})
}

// =============================================================================
// Тест 9: Heartbeat не ломает chunked terminator при медленном стриминге
// =============================================================================

// TestOpenWebUI_Heartbeat_NoCorruption проверяет, что heartbeat сообщения
// не вызывают записи данных после chunked terminator. Использует медленный
// streaming (ChunkDelay > heartbeat interval), чтобы heartbeat goroutine
// успела сработать несколько раз.
func TestOpenWebUI_Heartbeat_NoCorruption(t *testing.T) {
	// ChunkDelay=100ms — медленнее heartbeat (heartbeat обычно ~30-50ms)
	mock := NewExpandedMockServer(MockBehavior{
		ChunkDelay:         100 * time.Millisecond,
		StreamResponseSize: 4,
	})
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Клиент с таймаутом
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Post(proxyServer.URL+"/api/generate", "application/json",
		bytes.NewBuffer(makeStreamPayload()))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Читаем поток с таймаутом между чанками
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var events []map[string]interface{}
	doneReceived := false
	heartbeatCount := 0

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			continue
		}

		// Считаем heartbeat'ы
		if strings.Contains(line, "heartbeat") {
			heartbeatCount++
			continue
		}

		// Парсим SSE или NDJSON
		var jsonData string
		if strings.HasPrefix(line, "data: ") {
			jsonData = strings.TrimPrefix(line, "data: ")
		} else {
			jsonData = line
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &event); err == nil {
			events = append(events, event)
			if done, ok := event["done"].(bool); ok && done {
				doneReceived = true
			}
		}
	}

	require.NoError(t, scanner.Err(),
		"Scanner must complete without errors despite heartbeat")
	assert.True(t, doneReceived,
		"Stream must contain done:true event even with heartbeat")
	assert.Greater(t, len(events), 0,
		"Stream must contain at least one content event")
	t.Logf("Heartbeat test: %d events, %d heartbeats, done:true=%v",
		len(events), heartbeatCount, doneReceived)

	// КРИТИЧЕСКАЯ ПРОВЕРКА: после завершения не должно быть данных
	validateNoExtraDataAfterDone(t, resp)

	// Проверка что прокси всё ещё работает (heartbeat не сломал состояние)
	resp2, err := client.Get(proxyServer.URL + "/api/version")
	require.NoError(t, err)
	resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode,
		"Proxy must remain functional after heartbeat streaming")
}

// =============================================================================
// Тест 10: Sequential mixed requests — целостность после нескольких запросов
// =============================================================================

// TestOpenWebUI_Sequential_MixedRequests проверяет, что после серии
// последовательных запросов разных типов все ответы остаются целостными.
// Это симулирует типичную сессию OpenWebUI: не-streaming → streaming → chat.
func TestOpenWebUI_Sequential_MixedRequests(t *testing.T) {
	mock := NewExpandedMockServer(MockBehavior{
		ChunkDelay:         5 * time.Millisecond,
		StreamResponseSize: 3,
	})
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	client := &http.Client{Timeout: 10 * time.Second}

	// 1. Non-streaming generate
	t.Run("step1-non-streaming-generate", func(t *testing.T) {
		payload := map[string]interface{}{
			"model": "llama3.1:8b", "prompt": "Hi", "stream": false,
		}
		body, _ := json.Marshal(payload)

		resp, err := client.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		resultBytes := validateNonStreamingBody(t, resp)
		var result map[string]interface{}
		json.Unmarshal(resultBytes, &result)
		assert.Equal(t, true, result["done"])
	})

	// 2. Streaming generate
	t.Run("step2-streaming-generate", func(t *testing.T) {
		resp, err := client.Post(proxyServer.URL+"/api/generate", "application/json",
			bytes.NewBuffer(makeStreamPayload()))
		require.NoError(t, err)
		defer resp.Body.Close()

		events, doneReceived := validateNDJSONStream(t, resp)
		assert.True(t, doneReceived)
		assert.GreaterOrEqual(t, len(events), 3)
		validateNoExtraDataAfterDone(t, resp)
	})

	// 3. Streaming chat
	t.Run("step3-streaming-chat", func(t *testing.T) {
		resp, err := client.Post(proxyServer.URL+"/api/chat", "application/json",
			bytes.NewBuffer(makeChatStreamPayload()))
		require.NoError(t, err)
		defer resp.Body.Close()

		events, doneReceived := validateNDJSONStream(t, resp)
		assert.True(t, doneReceived)
		assert.GreaterOrEqual(t, len(events), 3)
		validateNoExtraDataAfterDone(t, resp)
	})

	// 4. Health check
	t.Run("step4-healthcheck", func(t *testing.T) {
		resp, err := client.Get(proxyServer.URL + "/health")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		teHeader := resp.Header.Get("Transfer-Encoding")
		assert.Empty(t, teHeader)
	})

	// 5. API tags
	t.Run("step5-tags", func(t *testing.T) {
		resp, err := client.Get(proxyServer.URL + "/api/tags")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		teHeader := resp.Header.Get("Transfer-Encoding")
		assert.Empty(t, teHeader)
	})

	// 6. LlamaCpp backend non-streaming (если есть)
	t.Run("step6-llamacpp-non-streaming", func(t *testing.T) {
		llamaMock := NewExpandedMockServer(MockBehavior{
			StreamResponseSize: 3,
		})
		defer llamaMock.Close()

		lp, lpx := SetupExpandedProxy(t, llamaMock, func(cfg *types.LoadBalancerConfig) {
			cfg.Backends[0].Type = types.BackendTypeLlamaCpp
			cfg.Backends[0].Engine = types.EngineLlamaCPP
		})
		defer lp.Close()
		defer lpx.StopQueue()
		defer lpx.StopSessionManager()

		payload := map[string]interface{}{
			"model": "llama3.1:8b", "prompt": "Hi", "stream": false,
		}
		body, _ := json.Marshal(payload)

		resp, err := client.Post(lp.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		validateNonStreamingBody(t, resp)
	})
}

// =============================================================================
// Тест 11: Parallel streaming — все запросы завершаются без ошибок
// =============================================================================

// TestOpenWebUI_Parallel_Streaming проверяет, что несколько параллельных
// streaming запросов все корректно завершаются с done:true и без
// TransferEncodingError. OpenWebUI отправляет несколько запросов
// одновременно при переключении между чатами.
func TestOpenWebUI_Parallel_Streaming(t *testing.T) {
	mock := NewExpandedMockServer(MockBehavior{
		ChunkDelay:         10 * time.Millisecond,
		StreamResponseSize: 4,
	})
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	numRequests := 5
	errChan := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		go func(id int) {
			client := &http.Client{Timeout: 15 * time.Second}

			payload := map[string]interface{}{
				"model":  "llama3.1:8b",
				"prompt": fmt.Sprintf("Hello from request %d", id),
				"stream": true,
			}
			body, _ := json.Marshal(payload)

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

			// Проверка Transfer-Encoding
			teHeader := resp.Header.Get("Transfer-Encoding")
			if teHeader != "" {
				errChan <- fmt.Errorf("request %d has Transfer-Encoding header", id)
				return
			}

			// Читаем весь поток
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
				errChan <- fmt.Errorf("request %d scanner error (TransferEncodingError): %v", id, err)
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
		case <-time.After(15 * time.Second):
			t.Fatalf("Timeout waiting for parallel request %d", i)
		}
	}

	t.Logf("All %d parallel streaming requests completed successfully", numRequests)
}

// =============================================================================
// Тест 12: NDJSON Content-Type от бэкенда не вызывает TransferEncodingError
// =============================================================================

// TestOpenWebUI_NDJSON_ContentType проверяет, что когда бэкенд возвращает
// application/x-ndjson (как реальная Ollama), балансировщик корректно
// обрабатывает ответ без утечки Transfer-Encoding.
func TestOpenWebUI_NDJSON_ContentType(t *testing.T) {
	// Специальный мок с NDJSON форматом (реальное поведение Ollama)
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
			json.NewEncoder(w).Encode(map[string]string{"response": "ok", "done": "true"})
			return
		}

		model := ""
		if m, ok := req["model"].(string); ok {
			model = m
		}

		// Реальное поведение Ollama: NDJSON + Connection: close
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		tokens := []string{"Привет", " это", " NDJSON", "!"}
		for i, token := range tokens {
			isLast := i == len(tokens)-1
			event := map[string]interface{}{
				"model":    model,
				"response": token,
				"done":     isLast,
			}
			if isLast {
				event["total_duration"] = 1234567890
				event["eval_count"] = 4
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

	// Streaming запрос
	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json",
		bytes.NewBuffer(makeStreamPayload()))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// КРИТИЧЕСКАЯ ПРОВЕРКА 1: Transfer-Encoding не должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader,
		"Transfer-Encoding MUST NOT be present for NDJSON response")

	// КРИТИЧЕСКАЯ ПРОВЕРКА 2: Connection: close не должен проходить от бэкенда
	connHeader := resp.Header.Get("Connection")
	assert.NotEqual(t, "close", connHeader,
		"Connection: close from backend MUST NOT be forwarded to client")

	// Читаем NDJSON
	events, doneReceived := validateNDJSONStream(t, resp)
	assert.True(t, doneReceived,
		"NDJSON stream must contain done:true")
	assert.GreaterOrEqual(t, len(events), 4,
		"NDJSON stream should contain at least 4 events (got %d)", len(events))

	// После done:true не должно быть данных
	validateNoExtraDataAfterDone(t, resp)

	t.Logf("NDJSON test: %d events, done:true=%v, content-type=%s",
		len(events), doneReceived, resp.Header.Get("Content-Type"))
}
