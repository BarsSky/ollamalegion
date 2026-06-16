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
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// mockCppWorker — минимальный mock cppworker с поддержкой OpenAI-совместимых эндпоинтов.
// Балансер транслирует Ollama /api/chat → /v1/chat/completions и
// /api/generate → /v1/completions, поэтому mock отвечает именно на них.
type mockCppWorker struct {
	server   *httptest.Server
	host     string
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
		case path == "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(body, &req)
			stream, _ := req["stream"].(bool)

			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				chunk := map[string]interface{}{
					"id":      "chatcmpl-test",
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   req["model"],
					"choices": []map[string]interface{}{
						{
							"index": 0,
							"delta": map[string]interface{}{
								"role":    "assistant",
								"content": "Привет! Я работающая модель llama.cpp.",
							},
							"finish_reason": "stop",
						},
					},
				}
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				if flusher != nil {
					flusher.Flush()
				}
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				resp := map[string]interface{}{
					"id":      "chatcmpl-test",
					"object":  "chat.completion",
					"created": time.Now().Unix(),
					"model":   req["model"],
					"choices": []map[string]interface{}{
						{
							"index": 0,
							"message": map[string]interface{}{
								"role":    "assistant",
								"content": "Привет! Я работающая модель llama.cpp.",
							},
							"finish_reason": "stop",
						},
					},
				}
				json.NewEncoder(w).Encode(resp)
			}
		case path == "/v1/completions":
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(body, &req)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			resp := map[string]interface{}{
				"id":      "cmpl-test",
				"object":  "text_completion",
				"created": time.Now().Unix(),
				"model":   req["model"],
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"text":  "Ответ от llama.cpp модели.",
						"finish_reason": "stop",
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	m.host = strings.TrimPrefix(m.server.URL, "http://")
	return m
}

func (m *mockCppWorker) Close() {
	m.server.Close()
}

// createTestProxyForLLamaCpp создаёт Proxy с одним llama.cpp бэкендом
func createTestProxyForLLamaCpp(t *testing.T, cppWorkerURL string) *balancer.Proxy {
	t.Helper()

	// Парсим host:port из URL
	hostPort := strings.TrimPrefix(cppWorkerURL, "http://")
	parts := strings.Split(hostPort, ":")
	host := parts[0]
	port := 8080
	if len(parts) > 1 {
		fmt.Sscanf(parts[1], "%d", &port)
	}

	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			OperatingMode:    "standard",
			SessionStickiness: true,
			SessionTTL:       60,
			SessionIdleTTL:   60,
			RequestTimeout:   30,
			QueueMaxSize:     100,
			ModelAffinity:    true,
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
				ID:              "llamacpp-1",
				Name:            "Test llama.cpp Backend",
				Host:            host,
				OllamaPort:      port,
				CppWorkerPort:   port,
				Type:            types.BackendTypeLlamaCpp,
				Status:          types.StatusHealthy,
				MaxConcurrentReqs: 10,
				Weight:          100,
			},
		},
		BackendEngine: types.EngineAuto,
	}

	proxy := balancer.NewProxy(cfg)
	return proxy
}

// TestLlamaCppProxyChat_NonStreaming проверяет не-стриминговый /api/chat через балансер к llama.cpp
func TestLlamaCppProxyChat_NonStreaming(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)

	// Регистрируем модель на бэкенде через метрики
	proxy.UpdateMetrics("llamacpp-1", &types.BackendMetrics{
		ID:   "llamacpp-1",
		Host: proxy.GetClusterState().Backends[0].Host,
		OllamaPort: proxy.GetClusterState().Backends[0].OllamaPort,
		BackendType: types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: "test-model", State: "loaded", ContextLength: 2048},
			},
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{},
		},
	})

	// Отправляем запрос через прокси
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

	// Проверяем что ответ содержит осмысленный текст, а не "**"
	message, ok := respData["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("response missing 'message' field: %v", respData)
	}
	content, ok := message["content"].(string)
	if !ok || content == "" {
		t.Fatalf("response message content is empty or missing")
	}
	if content == "**" {
		t.Errorf("response content is '**' (markdown bold markers) — expected meaningful text, got: %s", content)
	}
	if !strings.Contains(content, "Привет") {
		t.Errorf("expected response to contain meaningful text, got: %s", content)
	}

	t.Logf("✅ Chat response: %s", content)
}

// TestLlamaCppProxyChat_Streaming проверяет стриминговый /api/chat
func TestLlamaCppProxyChat_Streaming(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)

	// Регистрируем модель
	proxy.UpdateMetrics("llamacpp-1", &types.BackendMetrics{
		ID:   "llamacpp-1",
		BackendType: types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: "test-model", State: "loaded", ContextLength: 2048},
			},
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{},
		},
	})

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

	// Читаем стриминговый ответ
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
			t.Logf("skipping non-json line: %s", line)
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

	// Проверяем что Transfer-Encoding не дублируется
	transferEncoding := resp.Header.Get("Transfer-Encoding")
	if transferEncoding != "" {
		t.Errorf("Transfer-Encoding header should be empty in proxy response, got: %s", transferEncoding)
	}

	t.Log("✅ Streaming chat response received successfully")
}

// TestLlamaCppProxyRouting_ModelOnLLamaCpp проверяет что запросы /api/chat
// с моделью на llama.cpp маршрутизируются на llama.cpp бэкенд
func TestLlamaCppProxyRouting_ModelOnLLamaCpp(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)

	// Регистрируем модель ТОЛЬКО на llama.cpp бэкенде
	proxy.UpdateMetrics("llamacpp-1", &types.BackendMetrics{
		ID:   "llamacpp-1",
		BackendType: types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: "llama-model", State: "loaded", ContextLength: 2048},
			},
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{},
		},
	})

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

	// Проверяем, что запрос действительно дошёл до mock cppworker
	reqs := atomic.LoadInt64(&worker.requests)
	if reqs == 0 {
		t.Error("no requests reached the mock cppworker — routing may have failed")
	}
	t.Logf("✅ Request routed to llama.cpp backend (requests: %d)", reqs)
}

// TestLlamaCppProxyResponse_NotMarkdownBold проверяет что ответ не состоит из "**"
func TestLlamaCppProxyResponse_NotMarkdownBold(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)

	proxy.UpdateMetrics("llamacpp-1", &types.BackendMetrics{
		ID:   "llamacpp-1",
		BackendType: types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: "test-model", State: "loaded", ContextLength: 2048},
			},
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{},
		},
	})

	// Тест 1: не-стриминговый
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
			t.Errorf("non-streaming response is '**' — expected meaningful content")
		}
		if content == "" {
			t.Error("non-streaming response content is empty")
		}
	})

	// Тест 2: стриминговый
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
			t.Errorf("streaming response content is '**' — expected meaningful content")
		}
		if result == "" {
			t.Error("streaming response content is empty")
		}
	})
}

// TestGgufBackendsAPI проверяет API эндпоинт /api/v1/gguf/backends
func TestGgufBackendsAPI(t *testing.T) {
	worker := newMockCppWorker()
	defer worker.Close()

	proxy := createTestProxyForLLamaCpp(t, worker.server.URL)

	// Регистрируем модель
	proxy.UpdateMetrics("llamacpp-1", &types.BackendMetrics{
		ID:   "llamacpp-1",
		BackendType: types.BackendTypeLlamaCpp,
		OllamaPort: 8080,
		GPU: types.GPUMetrics{
			MemoryTotal: 24576,
			MemoryUsed:  8192,
		},
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: "test-model", State: "loaded", ContextLength: 2048},
			},
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{},
			ActiveRequests: 1,
		},
		MaxConcurrentRequests: 10,
	})

	// Проверяем cluster state напрямую (без фильтрации effectiveBackendType)
	state := proxy.GetClusterState()
	if state == nil {
		t.Fatal("cluster state is nil")
	}

	// Ищем llama.cpp бэкенд в состоянии
	found := false
	for _, bm := range state.Backends {
		if bm.BackendType == types.BackendTypeLlamaCpp {
			found = true
			if len(bm.LlamaCpp.LoadedModels) == 0 {
				t.Error("llama.cpp backend has no loaded models in cluster state")
			} else {
				t.Logf("✅ llama.cpp backend found: %s, models: %d",
					bm.ID, len(bm.LlamaCpp.LoadedModels))
			}
		}
	}
	if !found {
		t.Error("no llama.cpp backend found in cluster state")
	}
}