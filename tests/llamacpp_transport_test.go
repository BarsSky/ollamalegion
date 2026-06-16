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

// mockCppWorkerOpenAI — cppworker mock возвращающий OpenAI-формат ответов
// (как реальный llama.cpp сервер). Проверяет работу транслятора llamacpp_transport.go.
type mockCppWorkerOpenAI struct {
	server   *httptest.Server
	host     string
	requests int64
}

func newMockCppWorkerOpenAI() *mockCppWorkerOpenAI {
	m := &mockCppWorkerOpenAI{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&m.requests, 1)
		path := r.URL.Path

		switch {
		case path == "/health":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

		case path == "/v1/models":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"object": "list",
				"data": []map[string]interface{}{
					{"id": "test-model", "object": "model"},
				},
			})

		case path == "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(body, &req)
			stream, _ := req["stream"].(bool)

			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Transfer-Encoding", "chunked")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)

				chunk1 := map[string]interface{}{
					"choices": []map[string]interface{}{
						{"index": 0, "delta": map[string]interface{}{"role": "assistant", "content": "Привет! Я "}},
					},
				}
				data1, _ := json.Marshal(chunk1)
				fmt.Fprintf(w, "data: %s\n\n", data1)
				if flusher != nil { flusher.Flush() }

				chunk2 := map[string]interface{}{
					"choices": []map[string]interface{}{
						{"index": 0, "delta": map[string]interface{}{"content": "работающая модель"}},
					},
				}
				data2, _ := json.Marshal(chunk2)
				fmt.Fprintf(w, "data: %s\n\n", data2)
				if flusher != nil { flusher.Flush() }

				chunk3 := map[string]interface{}{
					"choices": []map[string]interface{}{
						{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"},
					},
				}
				data3, _ := json.Marshal(chunk3)
				fmt.Fprintf(w, "data: %s\n\n", data3)
				if flusher != nil { flusher.Flush() }

				fmt.Fprintf(w, "data: [DONE]\n\n")
				if flusher != nil { flusher.Flush() }
			} else {
				resp := map[string]interface{}{
					"id":      "chatcmpl-xxx",
					"object":  "chat.completion",
					"created": time.Now().Unix(),
					"model":   req["model"],
					"choices": []map[string]interface{}{
						{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": "Привет! Я работающая модель llama.cpp."}, "finish_reason": "stop"},
					},
					"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 15, "total_tokens": 25},
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
			}

		case path == "/v1/completions":
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(body, &req)
			stream, _ := req["stream"].(bool)

			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Transfer-Encoding", "chunked")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)

				data1, _ := json.Marshal(map[string]interface{}{"choices": []map[string]interface{}{{"index": 0, "text": "Ответ от "}}})
				fmt.Fprintf(w, "data: %s\n\n", data1)
				if flusher != nil { flusher.Flush() }

				data2, _ := json.Marshal(map[string]interface{}{"choices": []map[string]interface{}{{"index": 0, "text": "llama.cpp модели.", "finish_reason": "stop"}}})
				fmt.Fprintf(w, "data: %s\n\n", data2)
				if flusher != nil { flusher.Flush() }

				fmt.Fprintf(w, "data: [DONE]\n\n")
				if flusher != nil { flusher.Flush() }
			} else {
				resp := map[string]interface{}{
					"id":      "cmpl-xxx",
					"object":  "text_completion",
					"created": time.Now().Unix(),
					"model":   req["model"],
					"choices": []map[string]interface{}{{"index": 0, "text": "Ответ от llama.cpp модели.", "finish_reason": "stop"}},
					"usage":   map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18},
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	m.host = strings.TrimPrefix(m.server.URL, "http://")
	return m
}

func (m *mockCppWorkerOpenAI) Close() { m.server.Close() }

func createTestProxyForLlamaCppOpenAI(t *testing.T, cppWorkerURL string) *balancer.Proxy {
	t.Helper()
	hostPort := strings.TrimPrefix(cppWorkerURL, "http://")
	parts := strings.Split(hostPort, ":")
	host := parts[0]
	port := 8080
	if len(parts) > 1 { fmt.Sscanf(parts[1], "%d", &port) }

	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			OperatingMode: "standard", SessionStickiness: true, SessionTTL: 60, SessionIdleTTL: 60,
			RequestTimeout: 30, QueueMaxSize: 100, ModelAffinity: true,
			SyncModelLoad: types.SyncModelLoadConfig{Enabled: false},
			Prewarm:       types.PrewarmConfig{TriggerLoadThreshold: 0.80},
			AdvancedTiming: types.AdvancedTimingConfig{
				ZombieSessionThresholdSec: 120, StreamingRetryDelayMs: 500,
				MaxConcurrentWarmups: 3, WarmupSemaphoreTimeoutSec: 30,
			},
		},
		Backends: []types.Backend{
			{ID: "llamacpp-1", Name: "Test", Host: host, OllamaPort: port, CppWorkerPort: port,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 10, Weight: 100},
		},
		BackendEngine: types.EngineAuto,
	}
	return balancer.NewProxy(cfg)
}

func registerModel(proxy *balancer.Proxy, backendID, modelName string) {
	proxy.UpdateMetrics(backendID, &types.BackendMetrics{
		ID: backendID, BackendType: types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{Name: modelName, State: "loaded", ContextLength: 2048}}},
		Ollama:   types.OllamaMetrics{RunningModels: []types.RunningModel{}},
	})
}

// ---- ТЕСТЫ ----

func TestLlamaCppTransport_ChatNonStreaming(t *testing.T) {
	worker := newMockCppWorkerOpenAI()
	defer worker.Close()
	proxy := createTestProxyForLlamaCppOpenAI(t, worker.server.URL)
	registerModel(proxy, "llamacpp-1", "test-model")

	body, _ := json.Marshal(map[string]interface{}{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "Hi!"}}, "stream": false})
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != 200 { t.Fatalf("expected 200, got %d", rec.Code) }

	var resp map[string]interface{}
	json.NewDecoder(rec.Result().Body).Decode(&resp)
	msg, _ := resp["message"].(map[string]interface{})
	content, _ := msg["content"].(string)
	role, _ := msg["role"].(string)
	if content == "" { t.Fatal("empty content") }
	if role != "assistant" { t.Errorf("expected role=assistant, got %s", role) }
	if content == "**" { t.Errorf("response is '**'") }
	if !strings.Contains(content, "Привет") { t.Errorf("expected Привет, got %s", content) }
	done, _ := resp["done"].(bool)
	if !done { t.Error("done != true") }
	t.Logf("✅ Chat non-streaming: %q role=%s done=%v", content, role, done)
}

func TestLlamaCppTransport_ChatStreaming(t *testing.T) {
	worker := newMockCppWorkerOpenAI()
	defer worker.Close()
	proxy := createTestProxyForLlamaCppOpenAI(t, worker.server.URL)
	registerModel(proxy, "llamacpp-1", "test-model")

	body, _ := json.Marshal(map[string]interface{}{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "Hi!"}}, "stream": true})
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != 200 { t.Fatalf("expected 200, got %d", rec.Code) }

	scanner := bufio.NewScanner(rec.Result().Body)
	var doneFound bool
	var content strings.Builder
	for scanner.Scan() {
		var chunk map[string]interface{}
		if json.Unmarshal([]byte(scanner.Text()), &chunk) == nil {
			if d, _ := chunk["done"].(bool); d { doneFound = true }
			if msg, ok := chunk["message"].(map[string]interface{}); ok {
				if c, _ := msg["content"].(string); c != "" { content.WriteString(c) }
			}
		}
	}
	if !doneFound { t.Error("missing done:true") }
	if content.Len() == 0 { t.Error("no content") }
	if content.String() == "**" { t.Errorf("streaming is '**'") }
	t.Logf("✅ Chat streaming: %q", content.String())
}

func TestLlamaCppTransport_GenerateNonStreaming(t *testing.T) {
	worker := newMockCppWorkerOpenAI()
	defer worker.Close()
	proxy := createTestProxyForLlamaCppOpenAI(t, worker.server.URL)
	registerModel(proxy, "llamacpp-1", "test-model")

	body, _ := json.Marshal(map[string]interface{}{"model": "test-model", "prompt": "Hello", "stream": false})
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != 200 { t.Fatalf("expected 200, got %d", rec.Code) }
	var resp map[string]interface{}
	json.NewDecoder(rec.Result().Body).Decode(&resp)
	response, _ := resp["response"].(string)
	if response == "" || response == "**" { t.Errorf("bad generate response: %q", response) }
	t.Logf("✅ Generate: %q", response)
}

func TestLlamaCppTransport_TagsWithFallback(t *testing.T) {
	worker := newMockCppWorkerOpenAI()
	defer worker.Close()
	proxy := createTestProxyForLlamaCppOpenAI(t, worker.server.URL)
	// No metrics — fallback to /v1/models

	req := httptest.NewRequest("GET", "/api/tags", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != 200 { t.Fatalf("expected 200, got %d", rec.Code) }
	var resp map[string]interface{}
	json.NewDecoder(rec.Result().Body).Decode(&resp)
	models, _ := resp["models"].([]interface{})
	if len(models) == 0 { t.Error("fallback returned 0 models") }
	t.Logf("✅ Tags fallback: %d models", len(models))
}

func TestLlamaCppTransport_NoStarsInResponse(t *testing.T) {
	worker := newMockCppWorkerOpenAI()
	defer worker.Close()
	proxy := createTestProxyForLlamaCppOpenAI(t, worker.server.URL)
	registerModel(proxy, "llamacpp-1", "test-model")

	check := func(t *testing.T, stream bool) {
		t.Helper()
		body, _ := json.Marshal(map[string]interface{}{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "Hi!"}}, "stream": stream})
		req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)

		var allText strings.Builder
		if stream {
			scanner := bufio.NewScanner(rec.Result().Body)
			for scanner.Scan() {
				var chunk map[string]interface{}
				if json.Unmarshal([]byte(scanner.Text()), &chunk) == nil {
					if msg, ok := chunk["message"].(map[string]interface{}); ok {
						if c, _ := msg["content"].(string); c != "" { allText.WriteString(c) }
					}
				}
			}
		} else {
			var resp map[string]interface{}
			json.NewDecoder(rec.Result().Body).Decode(&resp)
			if msg, ok := resp["message"].(map[string]interface{}); ok {
				if c, _ := msg["content"].(string); c != "" { allText.WriteString(c) }
			}
		}
		if allText.String() == "**" { t.Errorf("response is '**' (stream=%v)", stream) }
	}
	t.Run("non-streaming", func(t *testing.T) { check(t, false) })
	t.Run("streaming", func(t *testing.T) { check(t, true) })
	t.Log("✅ No '**' in responses")
}

func TestLlamaCppTransport_APIChain(t *testing.T) {
	worker := newMockCppWorkerOpenAI()
	defer worker.Close()
	proxy := createTestProxyForLlamaCppOpenAI(t, worker.server.URL)
	registerModel(proxy, "llamacpp-1", "test-model")

	t.Run("01_tags", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/tags", nil)
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if rec.Code != 200 { t.Fatalf("tags: %d", rec.Code) }
		var resp map[string]interface{}
		json.NewDecoder(rec.Result().Body).Decode(&resp)
		t.Logf("✓ tags: %v models", len(resp["models"].([]interface{})))
	})

	t.Run("02_chat_nonstream", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "Hi!"}}, "stream": false})
		req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if rec.Code != 200 { t.Fatalf("chat: %d", rec.Code) }
		var resp map[string]interface{}
		json.NewDecoder(rec.Result().Body).Decode(&resp)
		msg, _ := resp["message"].(map[string]interface{})
		t.Logf("✓ chat non-stream: %q", msg["content"])
	})

	t.Run("03_chat_stream", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "Hi!"}}, "stream": true})
		req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if rec.Code != 200 { t.Fatalf("chat stream: %d", rec.Code) }
		var collected strings.Builder
		scanner := bufio.NewScanner(rec.Result().Body)
		for scanner.Scan() {
			var chunk map[string]interface{}
			if json.Unmarshal([]byte(scanner.Text()), &chunk) == nil {
				if msg, ok := chunk["message"].(map[string]interface{}); ok {
					if c, _ := msg["content"].(string); c != "" { collected.WriteString(c) }
				}
			}
		}
		t.Logf("✓ chat stream: %q", collected.String())
	})

	t.Run("04_generate", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{"model": "test-model", "prompt": "Hi", "stream": false})
		req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if rec.Code != 200 { t.Fatalf("generate: %d", rec.Code) }
		var resp map[string]interface{}
		json.NewDecoder(rec.Result().Body).Decode(&resp)
		t.Logf("✓ generate: %q", resp["response"])
	})

	reqs := atomic.LoadInt64(&worker.requests)
	t.Logf("✅ Full API chain OK: %d requests to cppworker", reqs)
}