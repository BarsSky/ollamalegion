package llamacpp_proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// mockCppWorker — минимальный mock cppworker с поддержкой /api/chat и /api/generate
type mockCppWorker struct {
	server   *httptest.Server
	requests int64
}

func newMockCppWorker() *mockCppWorker {
	m := &mockCppWorker{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&m.requests, 1)
		path := r.URL.Path

		switch {
		case path == "/health":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case path == "/api/tags":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "test-model", "modified_at": time.Now().UTC().Format(time.RFC3339)},
				},
			})
		case path == "/api/chat":
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(body, &req)
			stream, _ := req["stream"].(bool)

			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)

			if stream {
				resp := map[string]interface{}{
					"model":      req["model"],
					"created_at": time.Now().UTC().Format(time.RFC3339),
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "Привет! Я работающая модель llama.cpp.",
					},
					"done": true,
				}
				data, _ := json.Marshal(resp)
				fmt.Fprintf(w, "%s\n", data)
			} else {
				resp := map[string]interface{}{
					"model":      req["model"],
					"created_at": time.Now().UTC().Format(time.RFC3339),
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "Привет! Я работающая модель llama.cpp.",
					},
					"done": true,
				}
				json.NewEncoder(w).Encode(resp)
			}
		case path == "/api/generate":
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(body, &req)
			stream, _ := req["stream"].(bool)

			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)

			if stream {
				resp := map[string]interface{}{
					"model":      req["model"],
					"created_at": time.Now().UTC().Format(time.RFC3339),
					"response":   "Ответ от llama.cpp модели.",
					"done":       true,
				}
				data, _ := json.Marshal(resp)
				fmt.Fprintf(w, "%s\n", data)
			} else {
				resp := map[string]interface{}{
					"model":      req["model"],
					"created_at": time.Now().UTC().Format(time.RFC3339),
					"response":   "Ответ от llama.cpp модели.",
					"done":       true,
				}
				json.NewEncoder(w).Encode(resp)
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	return m
}

func (m *mockCppWorker) Close() {
	m.server.Close()
}

func createTestProxyForLLamaCpp(t *testing.T, cppWorkerURL string) *balancer.Proxy {
	t.Helper()

	hostPort := strings.TrimPrefix(cppWorkerURL, "http://")
	parts := strings.Split(hostPort, ":")
	host := parts[0]
	port := 8080
	if len(parts) > 1 {
		fmt.Sscanf(parts[1], "%d", &port)
	}

	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			OperatingMode:     "standard",
			SessionStickiness: true,
			SessionTTL:        60,
			SessionIdleTTL:    60,
			RequestTimeout:    30,
			QueueMaxSize:      100,
			ModelAffinity:     true,
			SyncModelLoad: types.SyncModelLoadConfig{
				Enabled: false,
			},
			Prewarm: types.PrewarmConfig{
				TriggerLoadThreshold: 0.80,
			},
			AdvancedTiming: types.AdvancedTimingConfig{
				ZombieSessionThresholdSec: 120,
				StreamingRetryDelayMs:     500,
				MaxConcurrentWarmups:      3,
				WarmupSemaphoreTimeoutSec: 30,
			},
		},
		Backends: []types.Backend{
			{
				ID:               "llamacpp-1",
				Name:             "Test llama.cpp Backend",
				Host:             host,
				OllamaPort:       port,
				CppWorkerPort:    port,
				Type:             types.BackendTypeLlamaCpp,
				Status:           types.StatusHealthy,
				MaxConcurrentReqs: 10,
				Weight:           100,
			},
		},
		BackendEngine: types.EngineAuto,
		LoadBalancer: types.LoadBalancerSettings{
			StatePath: "",
		},
	}

	proxy := balancer.NewProxy(cfg)
	return proxy
}

func registerLlamaCppModel(proxy *balancer.Proxy, backendID, modelName string) {
	proxy.UpdateMetrics(backendID, &types.BackendMetrics{
		ID:           backendID,
		BackendType:  types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: modelName, State: "loaded", ContextLength: 2048},
			},
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{},
		},
	})
}

// TestChatNonStreaming проверяет не-стриминговый /api/chat через балансер к llama.cpp
func TestChatNonStreaming(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)
	registerLlamaCppModel(proxy, "llamacpp-1", "test-model")

	reqBody := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]string{
			{"role": "user", "content": "Привет!"},
		},
		"stream": false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	message, ok := respData["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("response missing 'message' field: %v", respData)
	}
	content, ok := message["content"].(string)
	if !ok || content == "" {
		t.Fatalf("response message content is empty or missing")
	}
	if content == "**" {
		t.Errorf("response is '**' — expected meaningful text, got: %s", content)
	}
	if !strings.Contains(content, "Привет") {
		t.Errorf("expected response to contain 'Привет', got: %s", content)
	}

	t.Logf("Non-streaming chat: %s", content)
}

// TestChatStreaming проверяет стриминговый /api/chat
func TestChatStreaming(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)
	registerLlamaCppModel(proxy, "llamacpp-1", "test-model")

	reqBody := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]string{
			{"role": "user", "content": "Привет!"},
		},
		"stream": true,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	var foundDone bool
	var hasContent bool
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if done, ok := chunk["done"].(bool); ok && done {
			foundDone = true
		}
		if msg, ok := chunk["message"].(map[string]interface{}); ok {
			if content, ok := msg["content"].(string); ok && content != "" {
				hasContent = true
			}
		}
	}

	if !foundDone {
		t.Error("streaming response missing done:true")
	}
	if !hasContent {
		t.Error("streaming response missing message content")
	}

	// Проверяем отсутствие Transfer-Encoding
	transferEncoding := resp.Header.Get("Transfer-Encoding")
	if transferEncoding != "" {
		t.Errorf("Transfer-Encoding should be empty in proxy response, got: %s", transferEncoding)
	}

	t.Log("Streaming chat: done=true, has content, no Transfer-Encoding")
}

// TestGenerate проверяет /api/generate через балансер
func TestGenerate(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)
	registerLlamaCppModel(proxy, "llamacpp-1", "test-model")

	reqBody := map[string]interface{}{
		"model":  "test-model",
		"prompt": "Напиши приветствие",
		"stream": false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	response, ok := respData["response"].(string)
	if !ok || response == "" {
		t.Fatalf("response field is empty or missing")
	}
	if response == "**" {
		t.Errorf("response is '**' — expected meaningful text")
	}

	t.Logf("Generate response: %s", response)
}

// TestRouting_ModelOnLLamaCpp проверяет роутинг запросов на llama.cpp бэкенд
func TestRouting_ModelOnLLamaCpp(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)
	registerLlamaCppModel(proxy, "llamacpp-1", "llama-model")

	// Отправляем запрос
	reqBody := map[string]interface{}{
		"model": "llama-model",
		"messages": []map[string]string{
			{"role": "user", "content": "Test"},
		},
		"stream": false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	reqs := atomic.LoadInt64(&worker.requests)
	if reqs == 0 {
		t.Error("no requests reached the mock cppworker — routing failed")
	}

	t.Logf("Requests routed to llama.cpp backend: %d", reqs)
}

// TestResponseNotMarkdownBold проверяет что ответ не "**"
func TestResponseNotMarkdownBold(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)
	registerLlamaCppModel(proxy, "llamacpp-1", "test-model")

	// Non-streaming
	t.Run("non-streaming", func(t *testing.T) {
		reqBody := map[string]interface{}{
			"model": "test-model",
			"messages": []map[string]string{
				{"role": "user", "content": "Скажи привет"},
			},
			"stream": false,
		}
		bodyBytes, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)

		var respData map[string]interface{}
		json.NewDecoder(rec.Result().Body).Decode(&respData)
		msg, _ := respData["message"].(map[string]interface{})
		content, _ := msg["content"].(string)
		if content == "**" {
			t.Errorf("response is '**' — expected meaningful content")
		}
		if content == "" {
			t.Error("response content is empty")
		}
	})

	// Streaming
	t.Run("streaming", func(t *testing.T) {
		reqBody := map[string]interface{}{
			"model": "test-model",
			"messages": []map[string]string{
				{"role": "user", "content": "Скажи привет"},
			},
			"stream": true,
		}
		bodyBytes, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)

		scanner := bufio.NewScanner(rec.Result().Body)
		var allContent strings.Builder
		for scanner.Scan() {
			var chunk map[string]interface{}
			if json.Unmarshal([]byte(scanner.Text()), &chunk) == nil {
				if msg, ok := chunk["message"].(map[string]interface{}); ok {
					if c, ok := msg["content"].(string); ok {
						allContent.WriteString(c)
					}
				}
			}
		}
		result := allContent.String()
		if result == "**" {
			t.Errorf("streaming content is '**' — expected meaningful text")
		}
		if result == "" {
			t.Error("streaming content is empty")
		}
	})
}

// TestTagsEndpoint проверяет что /api/tags проксируется к llama.cpp
func TestTagsEndpoint(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)
	registerLlamaCppModel(proxy, "llamacpp-1", "test-model")

	req := httptest.NewRequest("GET", "/api/tags", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	models, ok := respData["models"].([]interface{})
	if !ok || len(models) == 0 {
		t.Error("tags response missing models")
	}

	t.Logf("Tags: %d models returned", len(models))
}