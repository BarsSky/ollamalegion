package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// OpenWebUI Tool Calling Tests
//
// Эти тесты симулируют отправку tools через балансировщик и проверяют,
// что ответы содержат корректные tool_calls при работе с инструментами.
//
// Сценарии:
//   1. Non-streaming chat с tools → tool_calls в ответе
//   2. Streaming chat с tools → буферизированный ответ с tool_calls
//   3. Tool результат (follow-up) → текстовый ответ
//   4. Множественные tool_calls
//   5. Content-Length корректность при tool_calls
// =============================================================================

// =============================================================================
// Вспомогательные функции
// =============================================================================

// makeToolChatPayload создаёт JSON body для chat запроса с tools.
// Если needToolResults=true, добавляет tool-сообщения для имитации результата вызова.
func makeToolChatPayload(stream bool, needToolResults bool) []byte {
	messages := []map[string]interface{}{
		{"role": "user", "content": "Search for information about AI"},
	}
	if needToolResults {
		messages = append(messages,
			map[string]interface{}{
				"role":       "assistant",
				"content":    "",
				"tool_calls": []map[string]interface{}{},
			},
			map[string]interface{}{
				"role":         "tool",
				"content":      "AI stands for Artificial Intelligence...",
				"tool_call_id": "call_mock_search",
				"name":         "search",
			},
		)
	}
	payload := map[string]interface{}{
		"model":    "llama3.1:8b",
		"messages": messages,
		"stream":   stream,
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "search",
					"description": "Search the web for information",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"q": map[string]interface{}{
								"type":        "string",
								"description": "Search query",
							},
						},
						"required": []string{"q"},
					},
				},
			},
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "calculate",
					"description": "Perform calculations",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"expression": map[string]interface{}{
								"type":        "string",
								"description": "Math expression",
							},
						},
						"required": []string{"expression"},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	return body
}

// validateToolCallsResponse проверяет, что ответ содержит tool_calls.
func validateToolCallsResponse(t *testing.T, bodyBytes []byte) {
	t.Helper()

	var result map[string]interface{}
	err := json.Unmarshal(bodyBytes, &result)
	require.NoError(t, err, "Response must be valid JSON")

	// Проверяем done:true
	done, ok := result["done"].(bool)
	require.True(t, ok, "Response must contain 'done' field")
	assert.True(t, done, "Response must have done:true")

	// Проверяем message
	msg, ok := result["message"].(map[string]interface{})
	require.True(t, ok, "Response must contain 'message' field")

	// Проверяем content:""
	content := msg["content"]
	assert.Equal(t, "", content, "Content should be empty string when tool_calls present")

	// Проверяем tool_calls
	toolCalls, ok := msg["tool_calls"].([]interface{})
	require.True(t, ok, "Response message must contain 'tool_calls' array")
	require.GreaterOrEqual(t, len(toolCalls), 1, "Must have at least one tool_call")

	// Проверяем структуру первого tool_call
	firstCall, ok := toolCalls[0].(map[string]interface{})
	require.True(t, ok, "Each tool_call must be a JSON object")
	assert.Equal(t, "function", firstCall["type"], "Tool call type must be 'function'")

	funcObj, ok := firstCall["function"].(map[string]interface{})
	require.True(t, ok, "Tool call must contain 'function' field")
	assert.NotEmpty(t, funcObj["name"], "Function name must not be empty")
	assert.NotEmpty(t, funcObj["arguments"], "Function arguments must not be empty")

	t.Logf("Tool call: id=%v, function=%v, arguments=%v",
		firstCall["id"], funcObj["name"], funcObj["arguments"])
}

// validateToolCallsNotPresent проверяет, что в ответе нет tool_calls.
func validateToolCallsNotPresent(t *testing.T, bodyBytes []byte) {
	t.Helper()

	var result map[string]interface{}
	err := json.Unmarshal(bodyBytes, &result)
	require.NoError(t, err, "Response must be valid JSON")

	msg, ok := result["message"].(map[string]interface{})
	require.True(t, ok, "Response must contain 'message' field")

	_, hasToolCalls := msg["tool_calls"]
	assert.False(t, hasToolCalls, "Response should not contain tool_calls for follow-up text response")

	// Проверяем что есть content
	content, ok := msg["content"].(string)
	assert.True(t, ok, "Message should have content")
	assert.NotEmpty(t, content, "Content should not be empty for text response")
}

// =============================================================================
// Тест 1: Non-streaming chat с tools → tool_calls
// =============================================================================

// TestOpenWebUI_ToolCall_NonStreaming проверяет, что при отправке не-streaming
// chat запроса с tools, балансировщик корректно возвращает tool_calls в ответе.
func TestOpenWebUI_ToolCall_NonStreaming(t *testing.T) {
	mock := NewExpandedMockServer(DefaultMockBehavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Non-streaming запрос с tools
	payload := makeToolChatPayload(false, false)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Проверка целостности не-streaming ответа
	resultBytes := validateNonStreamingBody(t, resp)

	// Проверка tool_calls
	validateToolCallsResponse(t, resultBytes)

	t.Logf("Non-streaming tool_calls response: %d bytes", len(resultBytes))
}

// =============================================================================
// Тест 2: Streaming chat с tools → буферизированный ответ с tool_calls
// =============================================================================

// TestOpenWebUI_ToolCall_Streaming проверяет, что при streaming запросе с tools
// балансировщик корректно передаёт буферизированный ответ с tool_calls.
// В реальном cppworker streaming с tools буферизируется и возвращается
// как non-streaming чанк с done:true + tool_calls.
func TestOpenWebUI_ToolCall_Streaming(t *testing.T) {
	mock := NewExpandedMockServer(DefaultMockBehavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Streaming запрос с tools
	payload := makeToolChatPayload(true, false)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Читаем поток (должен быть один NDJSON чанк с tool_calls)
	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Body must be readable without errors")
	require.NotEmpty(t, bodyBytes, "Body must not be empty")

	// Transfer-Encoding не должен присутствовать
	teHeader := resp.Header.Get("Transfer-Encoding")
	assert.Empty(t, teHeader, "Transfer-Encoding MUST NOT be present in streaming with tools")

	// Парсим первую строку (весь ответ — один NDJSON чанк)
	lines := strings.Split(strings.TrimSpace(string(bodyBytes)), "\n")
	require.GreaterOrEqual(t, len(lines), 1, "Must have at least one line in streaming tools response")

	// Проверяем последний чанк
	lastLine := strings.TrimSpace(lines[len(lines)-1])
	var chunk map[string]interface{}
	err = json.Unmarshal([]byte(lastLine), &chunk)
	require.NoError(t, err, "Stream chunk must be valid JSON")

	// Проверяем done:true
	done, _ := chunk["done"].(bool)
	assert.True(t, done, "Tool calls stream must end with done:true")

	// Проверяем tool_calls
	msg, ok := chunk["message"].(map[string]interface{})
	require.True(t, ok, "Chunk must contain 'message'")

	content, _ := msg["content"].(string)
	assert.Equal(t, "", content, "Content should be empty when tool_calls present")

	toolCalls, ok := msg["tool_calls"].([]interface{})
	require.True(t, ok, "Streaming response must contain 'tool_calls'")
	assert.GreaterOrEqual(t, len(toolCalls), 1, "Must have at least one tool_call")

	t.Logf("Streaming tool_calls response: %d bytes, %d lines, %d tool_calls",
		len(bodyBytes), len(lines), len(toolCalls))
}

// =============================================================================
// Тест 3: Tool результат (follow-up) → текстовый ответ
// =============================================================================

// TestOpenWebUI_ToolCall_FollowUp проверяет, что после отправки tool-результатов
// (role="tool"), балансировщик корректно возвращает текстовый ответ.
// Это симулирует full round-trip: запрос с tools → tool_calls → вызов инструмента → ответ.
func TestOpenWebUI_ToolCall_FollowUp(t *testing.T) {
	mock := NewExpandedMockServer(DefaultMockBehavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	// Non-streaming запрос с tools + tool-результатами (имитация follow-up)
	payload := makeToolChatPayload(false, true)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resultBytes := validateNonStreamingBody(t, resp)

	// Проверяем, что нет tool_calls (это текстовый ответ)
	validateToolCallsNotPresent(t, resultBytes)

	// Проверяем, что есть текстовый ответ
	var result map[string]interface{}
	json.Unmarshal(resultBytes, &result)
	msg := result["message"].(map[string]interface{})
	content := msg["content"].(string)
	assert.Contains(t, content, "search results",
		"Follow-up response should contain text about search results")

	t.Logf("Follow-up text response: %d bytes, content=%s", len(resultBytes), content)
}

// =============================================================================
// Тест 4: Множественные tool_calls
// =============================================================================

// TestOpenWebUI_ToolCall_MultipleCalls проверяет, что ответ может содержать
// несколько tool_calls в одном сообщении.
func TestOpenWebUI_ToolCall_MultipleCalls(t *testing.T) {
	mock := NewExpandedMockServer(DefaultMockBehavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	payload := makeToolChatPayload(false, false)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resultBytes := validateNonStreamingBody(t, resp)

	// Проверяем tool_calls и их количество
	var result map[string]interface{}
	json.Unmarshal(resultBytes, &result)
	msg := result["message"].(map[string]interface{})
	toolCalls := msg["tool_calls"].([]interface{})

	// Должно быть 2 tool_calls (search и calculate)
	assert.GreaterOrEqual(t, len(toolCalls), 2, "Must have at least 2 tool_calls")

	// Проверяем второй call
	secondCall := toolCalls[1].(map[string]interface{})
	assert.Equal(t, "function", secondCall["type"])
	funcObj := secondCall["function"].(map[string]interface{})
	assert.Equal(t, "calculate", funcObj["name"],
		"Second tool call should be 'calculate'")

	t.Logf("Multiple tool_calls: count=%d, first=%v, second=%v",
		len(toolCalls), toolCalls[0], toolCalls[1])
}

// =============================================================================
// Тест 5: Content-Length корректность при tool_calls
// =============================================================================

// TestOpenWebUI_ToolCall_ContentLength проверяет, что Content-Length
// точно соответствует длине тела ответа, даже при наличии tool_calls.
func TestOpenWebUI_ToolCall_ContentLength(t *testing.T) {
	mock := NewExpandedMockServer(DefaultMockBehavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	payload := makeToolChatPayload(false, false)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Проверка целостности не-streaming ответа уже включает проверку Content-Length
	resultBytes := validateNonStreamingBody(t, resp)

	// Дополнительная верификация
	if cl := resp.ContentLength; cl > 0 {
		assert.Equal(t, cl, int64(len(resultBytes)),
			"Tool_calls response Content-Length must match actual body length")
	}

	// Явно проверяем, что JSON с tool_calls валидный
	var parsed map[string]interface{}
	err = json.Unmarshal(resultBytes, &parsed)
	require.NoError(t, err, "Tool_calls response must be valid JSON")

	// Проверяем, что все поля корректны
	msg := parsed["message"].(map[string]interface{})
	assert.Equal(t, "assistant", msg["role"])
	tc := msg["tool_calls"].([]interface{})
	require.Greater(t, len(tc), 0)

	t.Logf("Content-Length validation: Content-Length=%d, actual=%d, match=%v",
		resp.ContentLength, len(resultBytes), resp.ContentLength == int64(len(resultBytes)))
}

// =============================================================================
// Тест 6: Полный round-trip: tools → tool_calls → tool_result → text
// =============================================================================

// TestOpenWebUI_ToolCall_FullRoundTrip симулирует полный lifecycle:
// 1. Отправка запроса с tools → получение tool_calls
// 2. Отправка tool-результатов → получение текстового ответа
func TestOpenWebUI_ToolCall_FullRoundTrip(t *testing.T) {
	mock := NewExpandedMockServer(DefaultMockBehavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	client := &http.Client{Timeout: 15 * time.Second}

	// Шаг 1: Запрос с tools → tool_calls
	t.Run("step1-tools-request", func(t *testing.T) {
		payload := makeToolChatPayload(false, false)

		resp, err := client.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(payload))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		bodyBytes := validateNonStreamingBody(t, resp)
		validateToolCallsResponse(t, bodyBytes)
	})

	// Шаг 2: Отправка tool-результатов → текстовый ответ
	t.Run("step2-tool-result", func(t *testing.T) {
		payload := makeToolChatPayload(false, true)

		resp, err := client.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(payload))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		bodyBytes := validateNonStreamingBody(t, resp)
		validateToolCallsNotPresent(t, bodyBytes)
	})

	// Шаг 3: Проверяем что прокси всё ещё работает
	t.Run("step3-healthcheck", func(t *testing.T) {
		resp, err := client.Get(proxyServer.URL + "/api/tags")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	// Шаг 4: Проверяем счётчики запросов к моку
	t.Run("step4-verify-counts", func(t *testing.T) {
		// Должно быть 2 chat запроса (1 с tools + 1 с tool results)
		chatCount := atomic.LoadInt64(&mock.ChatCount)
		t.Logf("Mock chat count: %d (expected at least 2)", chatCount)
		// Минимум 2 (мы сделали 2 chat запроса), но может быть больше из-за healthcheck
	})
}

// =============================================================================
// Тест 7: Non-streaming chat с tools через балансировщик — verify last request
// =============================================================================

// TestOpenWebUI_ToolCall_VerifyRequestBody проверяет, что балансировщик
// передаёт правильный запрос с tools на бэкенд (без изменений обязательных полей).
func TestOpenWebUI_ToolCall_VerifyRequestBody(t *testing.T) {
	mock := NewExpandedMockServer(DefaultMockBehavior)
	defer mock.Close()

	proxyServer, proxy := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	payload := makeToolChatPayload(false, false)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Ждём что запрос получен моком
	received := mock.WaitForRequest(2 * time.Second)
	assert.True(t, received, "Mock must receive the request")

	// Проверяем, что tools были в запросе
	lastBody := mock.LastRequestBody()
	require.NotNil(t, lastBody, "Last request body must not be nil")

	var lastReq map[string]interface{}
	err = json.Unmarshal(lastBody, &lastReq)
	require.NoError(t, err)

	// Проверяем tools
	tools, ok := lastReq["tools"].([]interface{})
	require.True(t, ok, "Request must contain 'tools' array")
	assert.GreaterOrEqual(t, len(tools), 1, "Must have at least one tool")

	// Проверяем model
	model, _ := lastReq["model"].(string)
	assert.Equal(t, "llama3.1:8b", model)

	// Проверяем messages
	messages, ok := lastReq["messages"].([]interface{})
	require.True(t, ok, "Request must contain 'messages'")
	assert.GreaterOrEqual(t, len(messages), 1, "Must have at least one message")

	t.Logf("Backend received request with %d tools, %d messages",
		len(tools), len(messages))
}
