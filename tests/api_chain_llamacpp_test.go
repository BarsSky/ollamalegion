package tests

import (
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
	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/types"
)

// TestApiChainLlamaCppChat_NonStreaming — проверка полного цикла:
// клиент → балансер → cppworker (mock) → балансер → клиент
func TestApiChainLlamaCppChat_NonStreaming(t *testing.T) {
	// Создаём mock cppworker с Ollama-совместимым API
	mockCppWorker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Mock cppworker received: %s %s", r.Method, r.URL.Path)
		if r.URL.Path == "/api/chat" && r.Method == "POST" {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]interface{}{
				"model":     "gemma-4-E4B-it-Q4_K_M",
				"created_at": time.Now().Unix(),
				"done": true,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "Привет! Я работаю через llama.cpp. Как я могу помочь?",
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		if r.URL.Path == "/api/generate" && r.Method == "POST" {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]interface{}{
				"model":     "gemma-4-E4B-it-Q4_K_M",
				"created_at": time.Now().Unix(),
				"done": true,
				"response": "Ответ от generate endpoint через llama.cpp.",
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockCppWorker.Close()

	// Создаём конфигурацию
	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingConfig{
			Algorithm:        "resource-aware",
			StreamTimeout:    300,
			RequestTimeout:   60,
		},
		Backends: []types.Backend{
			{
				ID:           "test-llamacpp-backend",
				Type:         types.BackendTypeLlamaCpp,
				Status:       types.BackendStatusHealthy,
				Host:         "127.0.0.1",
				CppWorkerPort: parsePortFromURL(mockCppWorker.URL),
				OllamaPort:   11434,
				Weight:       1,
				MaxConcurrentRequests: 10,
			},
		},
	}

	proxy := balancer.NewProxy(cfg)
	
	// Эмулируем метрики бэкенда
	metrics := &types.BackendMetrics{
		BackendID: "test-llamacpp-backend",
		Status:    types.BackendStatusHealthy,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppLoadedModel{
				{
					Name:          "gemma-4-E4B-it-Q4_K_M",
					Status:        "loaded",
					ContextLength: 1024,
				},
			},
		},
	}
	proxy.UpdateMetrics("test-llamacpp-backend", metrics)
	time.Sleep(100 * time.Millisecond)

	// Создаём тестовый HTTP сервер с балансером
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Тест 1: /api/chat non-streaming
	t.Run("ChatNonStreaming", func(t *testing.T) {
		body := map[string]interface{}{
			"model": "gemma-4-E4B-it-Q4_K_M",
			"messages": []map[string]string{
				{"role": "user", "content": "Привет!"},
			},
			"stream": false,
		}
		bodyBytes, _ := json.Marshal(body)
		resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			t.Fatalf("POST /api/chat failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Errorf("Expected 200, got %d", resp.StatusCode)
		}

		respBytes, _ := io.ReadAll(resp.Body)
		var result map[string]interface{}
		if err := json.Unmarshal(respBytes, &result); err != nil {
			t.Fatalf("Failed to parse response: %v\nBody: %s", err, string(respBytes))
		}

		// Проверяем что ответ содержит message.content, а не "**"
		message, ok := result["message"].(map[string]interface{})
		if !ok {
			t.Fatalf("Response missing 'message' field: %s", string(respBytes))
		}
		content, ok := message["content"].(string)
		if !ok || len(content) < 3 {
			t.Fatalf("Expected meaningful content, got: '%s' (full: %s)", content, string(respBytes))
		}
		if content == "**" {
			t.Errorf("Response content is '**' — markdown bold marker, not real answer")
		}
		t.Logf("Chat response: %s", content[:min(80, len(content))])
	})

	// Тест 2: /api/chat streaming
	t.Run("ChatStreaming", func(t *testing.T) {
		body := map[string]interface{}{
			"model": "gemma-4-E4B-it-Q4_K_M",
			"messages": []map[string]string{
				{"role": "user", "content": "Привет!"},
			},
			"stream": true,
		}
		bodyBytes, _ := json.Marshal(body)
		resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			t.Fatalf("POST /api/chat (stream) failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Errorf("Expected 200, got %d", resp.StatusCode)
		}

		// Читаем NDJSON построчно
		respBytes, _ := io.ReadAll(resp.Body)
		lines := strings.Split(strings.TrimSpace(string(respBytes)), "\n")
		
		foundContent := false
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				continue
			}
			if msg, ok := chunk["message"].(map[string]interface{}); ok {
				if c, ok := msg["content"].(string); ok && c != "" {
					foundContent = true
				}
			}
		}
		if !foundContent {
			t.Errorf("No content found in streaming response: %s", string(respBytes))
		}
		t.Logf("Streaming chunks: %d, content found: %v", len(lines), foundContent)
	})

	// Тест 3: /api/generate non-streaming
	t.Run("GenerateNonStreaming", func(t *testing.T) {
		body := map[string]interface{}{
			"model":  "gemma-4-E4B-it-Q4_K_M",
			"prompt": "Привет!",
			"stream": false,
		}
		bodyBytes, _ := json.Marshal(body)
		resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			t.Fatalf("POST /api/generate failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Errorf("Expected 200, got %d", resp.StatusCode)
		}

		respBytes, _ := io.ReadAll(resp.Body)
		var result map[string]interface{}
		if err := json.Unmarshal(respBytes, &result); err != nil {
			t.Fatalf("Failed to parse response: %v\nBody: %s", err, string(respBytes))
		}

		response, ok := result["response"].(string)
		if !ok || len(response) < 3 {
			t.Fatalf("Expected meaningful response, got: '%s'", response)
		}
		t.Logf("Generate response: %s", response[:min(80, len(response))])
	})

	// Тест 4: GGUF backends API
	t.Run("GgufBackends", func(t *testing.T) {
		resp, err := http.Get(proxyServer.URL + "/api/v1/gguf/backends")
		if err != nil {
			t.Fatalf("GET /api/v1/gguf/backends failed: %v", err)
		}
		defer resp.Body.Close()

		respBytes, _ := io.ReadAll(resp.Body)
		var backends []map[string]interface{}
		if err := json.Unmarshal(respBytes, &backends); err != nil {
			t.Fatalf("Failed to parse: %v\nBody: %s", err, string(respBytes))
		}

		if len(backends) == 0 {
			t.Errorf("Expected at least 1 llama.cpp backend, got 0")
		}

		for _, b := range backends {
			backendType := b["type"]
			if backendType != "llama_cpp" {
				t.Errorf("Expected backend type 'llama_cpp', got '%v'", backendType)
			}
			if models, ok := b["models"].([]interface{}); ok {
				for _, m := range models {
					if model, ok := m.(map[string]interface{}); ok {
						if name, ok := model["name"].(string); ok {
							if name == "" {
								t.Error("Model name is empty")
							}
						}
						if status, ok := model["status"].(string); ok {
							if status != "loaded" && status != "loading" && status != "error" {
								t.Errorf("Unexpected model status: %s", status)
							}
						}
					}
				}
			}
		}
		t.Logf("GGUF backends: %d", len(backends))
	})

	// Тест 5: Content verification — ответ не должен быть "**"
	t.Run("ResponseNotMarkdownBold", func(t *testing.T) {
		testCases := []struct {
			path string
			body map[string]interface{}
		}{
			{
				path: "/api/chat",
				body: map[string]interface{}{
					"model":    "gemma-4-E4B-it-Q4_K_M",
					"messages": []map[string]string{{"role": "user", "content": "Привет!"}},
					"stream":   false,
				},
			},
			{
				path: "/api/generate",
				body: map[string]interface{}{
					"model":  "gemma-4-E4B-it-Q4_K_M",
					"prompt": "Привет!",
					"stream": false,
				},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.path, func(t *testing.T) {
				bodyBytes, _ := json.Marshal(tc.body)
				resp, err := http.Post(proxyServer.URL+tc.path, "application/json", bytes.NewReader(bodyBytes))
				if err != nil {
					t.Fatalf("POST %s failed: %v", tc.path, err)
				}
				defer resp.Body.Close()

				respBytes, _ := io.ReadAll(resp.Body)
				content := string(respBytes)

				// Проверяем что ответ не начинается с "**" и не содержит только "**"
				trimmed := strings.TrimSpace(content)
				if strings.HasPrefix(trimmed, "**") {
					t.Errorf("Response starts with '**': %s", trimmed[:min(50, len(trimmed))])
				}
				if trimmed == "**" {
					t.Errorf("Response is exactly '**' (markdown bold marker)")
				}
				// Проверяем минимальную длину ответа
				if len(trimmed) < 10 {
					t.Errorf("Response too short (%d chars): '%s'", len(trimmed), trimmed)
				}
			})
		}
	})
}

// TestApiChainLlamaCppRouting verifies routing decisions
func TestApiChainLlamaCppRouting(t *testing.T) {
	mockCppWorker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/chat" {
			resp := map[string]interface{}{
				"model":   "gemma-4-E4B-it-Q4_K_M",
				"done":    true,
				"message": map[string]interface{}{"role": "assistant", "content": "OK"},
			}
			json.NewEncoder(w).Encode(resp)
		} else {
			w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer mockCppWorker.Close()

	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingConfig{
			Algorithm: "resource-aware",
		},
		Backends: []types.Backend{
			{
				ID:            "llamacpp-1",
				Type:          types.BackendTypeLlamaCpp,
				Status:        types.BackendStatusHealthy,
				Host:          "127.0.0.1",
				CppWorkerPort: parsePortFromURL(mockCppWorker.URL),
				Weight:        1,
			},
			{
				ID:            "ollama-1",
				Type:          types.BackendTypeOllama,
				Status:        types.BackendStatusHealthy,
				Host:          "127.0.0.1",
				OllamaPort:    11434,
			},
		},
	}

	proxy := balancer.NewProxy(cfg)
	
	metrics := &types.BackendMetrics{
		BackendID: "llamacpp-1",
		Status:    types.BackendStatusHealthy,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppLoadedModel{
				{Name: "gemma-4-E4B-it-Q4_K_M", Status: "loaded", ContextLength: 1024},
			},
		},
	}
	proxy.UpdateMetrics("llamacpp-1", metrics)
	time.Sleep(100 * time.Millisecond)

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Запрос модели, которая есть только на llama.cpp бэкенде
	body := map[string]interface{}{
		"model":    "gemma-4-E4B-it-Q4_K_M",
		"messages": []map[string]string{{"role": "user", "content": "test"}},
		"stream":   false,
	}
	bodyBytes, _ := json.Marshal(body)
	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("Expected 200, got %d", resp.StatusCode)
	}

	respBytes, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err == nil {
		if msg, ok := result["message"].(map[string]interface{}); ok {
			if content, ok := msg["content"].(string); ok {
				if content == "**" || len(content) <= 2 {
					t.Errorf("Got invalid response content: '%s'", content)
				}
			}
		}
	}
}

// TestApiChainLlamaCppWithRealBackend — интеграционный тест с real cppworker
// Запускается только с тегом integration
func TestApiChainLlamaCppWithRealBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	
	// Этот тест проверяет реальное взаимодействие с запущенным cppworker через балансер
	// Предполагается что балансер запущен на localhost:18080 и cppworker на localhost:18092
	baseURL := "http://localhost:18080"
	
	t.Run("HealthCheck", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/v1/health")
		if err != nil {
			t.Skipf("Balancer not running: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Skipf("Balancer not healthy: %d", resp.StatusCode)
		}
	})

	t.Run("ChatRealModel", func(t *testing.T) {
		body := map[string]interface{}{
			"model": "gemma-4-E4B-it-Q4_K_M",
			"messages": []map[string]string{
				{"role": "user", "content": "Скажи 'Привет' по-русски"},
			},
			"stream": false,
		}
		bodyBytes, _ := json.Marshal(body)
		resp, err := http.Post(baseURL+"/api/chat", "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			t.Skipf("API not available: %v", err)
		}
		defer resp.Body.Close()

		respBytes, _ := io.ReadAll(resp.Body)
		var result map[string]interface{}
		if err := json.Unmarshal(respBytes, &result); err != nil {
			t.Fatalf("Failed to parse: %v", err)
		}

		message, ok := result["message"].(map[string]interface{})
		if !ok {
			t.Fatalf("No message in response: %s", string(respBytes))
		}
		content, ok := message["content"].(string)
		if !ok {
			t.Fatalf("No content in message: %v", message)
		}

		// Проверяем что ответ не "**"
		if content == "**" || len(strings.TrimSpace(content)) <= 2 {
			t.Errorf("Invalid response: '%s'", content)
		}
		t.Logf("Model response: %s", content[:min(200, len(content))])
	})
}

func parsePortFromURL(urlStr string) int {
	// http://127.0.0.1:XXXXX → XXXX
	parts := strings.Split(urlStr, ":")
	if len(parts) >= 3 {
		port := 0
		fmt.Sscanf(parts[2], "%d", &port)
		return port
	}
	return 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}