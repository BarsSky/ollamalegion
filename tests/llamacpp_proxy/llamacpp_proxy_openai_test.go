package llamacpp_proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProxyRequestOpenAIStreaming_Non2xxResponse проверяет, что при получении
// не-2xx ответа от cppworker (например, HTTP 503 — model loading state)
// прокси не начинает пустой SSE-поток, а возвращает структурированную ошибку.
//
// Эта проблема была root cause бага "Expecting value: line 2 column 1 (char 1)"
// в OpenWebUI: cppworker возвращал HTTP 503, handleOpenAIChatCompletions
// не находил n_ctx ошибку, и proxyRequestOpenAIStreaming писал HTTP 200 с
// пустым SSE-потоком (Fix #5).
func TestProxyRequestOpenAIStreaming_Non2xxResponse(t *testing.T) {
	// 1. Mock cppworker, который всегда возвращает 503 для /v1/chat/completions
	// (симуляция состояния "модель загружается").
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// health — всегда OK
		if path == "/health" {
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}

		// /api/tags — возвращаем пустой список
		if path == "/api/tags" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{},
			})
			return
		}

		// /v1/chat/completions — всегда 503 с JSON-ошибкой (model loading)
		if path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "model is loading: test-model",
				"loading": "true",
			})
			return
		}

		// Все остальные пути — 404
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mockServer.Close()

	// 2. Создаём прокси как в существующих тестах
	proxy := createTestProxyForLLamaCpp(t, mockServer.URL)
	registerLlamaCppModel(proxy, "llamacpp-1", "test-model")

	// 3. Отправляем POST /v1/chat/completions (не /api/chat!) — это прямой
	// путь к handleOpenAIChatCompletions → proxyRequestOpenAIStreaming.
	reqBody := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Hello"},
		},
		"stream": false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	// 4. Проверяем результат
	resp := rec.Result()
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Должен быть НЕ 200 OK — это ключевая проверка. Старый баг выдавал 200 с пустым SSE.
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200 status code for 503 from upstream, got 200 OK. "+
			"This means proxyRequestOpenAIStreaming is still ignoring upstream status. Body: %s",
			string(responseBody))
	}

	// Должен быть 503 (Service Unavailable) — проксируем статус cppworker
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Logf("expected 503 Service Unavailable, got %d. "+
			"This is acceptable as long as it's not 200. Body: %s",
			resp.StatusCode, string(responseBody))
	}

	// Ответ должен быть JSON, а не SSE
	contentType := resp.Header.Get("Content-Type")
	if !isJSONContentType(contentType) {
		t.Errorf("expected Content-Type application/json, got %q. "+
			"SSE Content-Type means proxy is starting a stream despite error status. Body: %s",
			contentType, string(responseBody))
	}

	// Проверяем, что тело содержит JSON с ошибкой
	var errorResp map[string]interface{}
	if err := json.Unmarshal(responseBody, &errorResp); err != nil {
		t.Fatalf("response body is not valid JSON (got %q): %v. "+
			"Empty SSE or invalid body means proxyRequestOpenAIStreaming is returning empty stream. Body: %s",
			string(responseBody), err, string(responseBody))
	}

	// Проверяем наличие поля error
	errMsg, hasError := errorResp["error"]
	if !hasError || errMsg == "" {
		t.Errorf("response missing 'error' field or it's empty: %v. Body: %s",
			errorResp, string(responseBody))
	}

	// Проверяем, что error содержит подсказку про загрузку модели
	errStr, _ := errMsg.(string)
	if !containsAny(errStr, []string{"loading", "503", "unavailable", "upstream"}) {
		t.Logf("error message doesn't mention loading/503/unavailable: %q. Body: %s",
			errStr, string(responseBody))
	}

	t.Logf("OK: upstream 503 correctly returned as %d with JSON error: %s",
		resp.StatusCode, string(responseBody))
}

// TestProxyRequestOpenAIStreaming_Non2xxWithSSEContentType проверяет случай,
// когда upstream возвращает не-2xx с Content-Type: text/event-stream (крайне
// маловероятно, но код должен это обрабатывать корректно).
func TestProxyRequestOpenAIStreaming_Non2xxWithSSEContentType(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if path == "/health" {
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		if path == "/api/tags" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{},
			})
			return
		}

		// /v1/chat/completions — 503 с SSE Content-Type
		if path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "data: {\"error\":\"model is loading\"}\n\n")
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}

		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mockServer.Close()

	proxy := createTestProxyForLLamaCpp(t, mockServer.URL)
	registerLlamaCppModel(proxy, "llamacpp-1", "test-model")

	reqBody := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Hello"},
		},
		"stream": true,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Должен быть 503 (не 200)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200, got 200. SSE error should not be masked. Body: %s",
			string(responseBody))
	}

	// Должен быть SSE content-type
	contentType := resp.Header.Get("Content-Type")
	if !containsAny(contentType, []string{"text/event-stream", "text/event-stream"}) &&
		contentType != "text/event-stream" {
		t.Logf("Content-Type is %q (expected text/event-stream for SSE error path)", contentType)
	}

	t.Logf("OK: SSE error path returned status %d, Content-Type: %s",
		resp.StatusCode, contentType)
}

// TestProxyRequestOpenAIStreaming_ConnectionError проверяет случай,
// когда upstream-сервер недоступен (502 Bad Gateway).
func TestProxyRequestOpenAIStreaming_ConnectionError(t *testing.T) {
	// Прокси с неработающим upstream (несуществующий порт).
	// createTestProxyForLLamaCpp записывает URL в конфиг бэкенда.
	// Используем URL несуществующего сервера.
	proxy := createTestProxyForLLamaCpp(t, "http://127.0.0.1:1")
	registerLlamaCppModel(proxy, "llamacpp-1", "test-model")

	reqBody := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "Hello"},
		},
		"stream": false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Должен быть error
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200 for connection error, got 200. Body: %s",
			string(responseBody))
	}

	t.Logf("OK: connection error returned status %d: %s",
		resp.StatusCode, string(responseBody))
}

// Вспомогательные функции

func isJSONContentType(contentType string) bool {
	return contentType == "application/json" ||
		contentType == "application/json; charset=utf-8" ||
		contentType == "application/json;charset=utf-8"
}

func containsAny(s string, substrs []string) bool {
	for _, sub := range substrs {
		if len(sub) > 0 && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
