package balancer

// Regression tests for /api/chat and /api/generate routing fix.
//
// До фикса (multi_client_e2e_report.json):
//   - /api/chat:      0/22 (0%)  — selectBackend() возвращал "" → 503
//   - /api/generate:  0/7  (0%)  — selectBackend() возвращал "" → 503
//   - /v1/chat/completions (stream):  5/5  (100%) — работал, т.к. использовал
//     findModelOnLlamaCppBackend + selectAnyLlamaCppHealthy
//
// После фикса handleChat и handleGenerate используют тот же простой алгоритм,
// что и handleOpenAIChatCompletions (findModel + fallback).
//
// Эти тесты регрессионные: они ДОЛЖНЫ проходить после нашего изменения и
// ДОЛЖНЫ были провалиться до него (если бы multi-model + auto-load настроены).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// makeUpstreamForOllamaTest создаёт mock cppworker, который:
//  1. Отвечает на /api/models (для ensureModelLoadedOnBackend)
//  2. Отвечает на /api/models/load
//  3. Записывает полученный body в receivedBody (с мьютексом)
//  4. Возвращает валидный OpenAI streaming ответ (sse)
func makeUpstreamForOllamaTest(t *testing.T, receivedBody *[]byte, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"qwen2.5-coder","path":"qwen2.5-coder.gguf","state":"loaded"}]}`))
			return
		}
		if r.URL.Path == "/api/models/load" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true,"message":"loaded"}`))
			return
		}
		// /v1/chat/completions и /v1/completions — основные inference endpoint'ы.
		// Записываем тело и возвращаем OpenAI-style streaming ответ.
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*receivedBody = body
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Минимальный валидный OpenAI SSE ответ с одной delta и финальным done.
		_, _ = w.Write([]byte("data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

// makeTestLlamaProxy создаёт Proxy + LlamaCppRouter, настроенные на upstream (mock cppworker).
// Имя с префиксом Llama, чтобы не конфликтовать с makeTestProxy() в proxy_request_openai_test.go.
func makeTestLlamaProxy(t *testing.T, upstreamURL string) (*Proxy, *LlamaCppRouter, func()) {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{
				ID:            "llama_test",
				Name:          "test llama.cpp",
				Host:          "127.0.0.1",
				OllamaPort:    11434,
				AgentPort:     9090,
				CppWorkerPort: extractPort(upstreamURL),
				Type:          types.BackendTypeLlamaCpp,
				Weight:        1,
				Status:        types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:            types.AlgorithmResourceAware,
			HealthCheckInterval:  60,
			MetricsInterval:      60,
			StreamingIdleTimeout: 30,
			AdvancedTiming:       types.AdvancedTimingConfig{HeartbeatIntervalSec: 5},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false},
	}
	p := NewProxy(cfg)
	if p.GetBackend("llama_test") == nil {
		t.Fatal("backend not registered in proxy")
	}
	router := NewLlamaCppRouter(p)
	return p, router, func() {}
}

// TestHandleChat_DispatchesToHealthyBackend — главный регрессионный тест:
// handleChat должен корректно выбрать healthy llama.cpp backend и НЕ вернуть 503
// (раньше selectBackend() возвращал "" и клиент получал 503 "no llama.cpp backend
// available" вместо реального ответа).
func TestHandleChat_DispatchesToHealthyBackend(t *testing.T) {
	var receivedBody []byte
	var mu sync.Mutex
	upstream := makeUpstreamForOllamaTest(t, &receivedBody, &mu)
	defer upstream.Close()

	_, router, _ := makeTestLlamaProxy(t, upstream.URL)

	// Ollama-формат /api/chat (как шлёт OpenWebUI).
	ollamaPayload := `{
		"model": "qwen2.5-coder",
		"stream": true,
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`

	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(ollamaPayload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handled := router.Route(rec, req)
	if !handled {
		t.Fatalf("router did not handle /api/chat (status=%d body=%s)", rec.Code, rec.Body.String())
	}

	// Главная проверка: НЕ ДОЛЖНО быть 503 "no llama.cpp backend available"
	// (это был симптом исходной проблемы).
	bodyStr := rec.Body.String()
	if rec.Code == http.StatusServiceUnavailable && strings.Contains(bodyStr, "no llama.cpp backend available") {
		t.Fatalf("REGRESSION: got 503 'no llama.cpp backend available' — backend selection failed. body=%s", bodyStr)
	}

	// Upstream должен был получить body (модель выбрана, прокси сработал).
	mu.Lock()
	got := append([]byte{}, receivedBody...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatalf("upstream received empty body — proxy did not dispatch the request. recorder body: %s", bodyStr)
	}

	// Проверяем, что upstream получил корректный запрос (модель та же).
	var gotReq struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(got, &gotReq); err != nil {
		t.Fatalf("upstream body is not valid JSON: %v\nbody=%s", err, got)
	}
	if gotReq.Model != "qwen2.5-coder" {
		t.Errorf("upstream model=%q, want qwen2.5-coder", gotReq.Model)
	}
}

// TestHandleChat_NormalizesMultimodalContent — handleChat должен нормализовать
// multi-modal content[] (как и handleOpenAIChatCompletions), иначе cppworker
// вернёт 400 "cannot unmarshal array into string".
func TestHandleChat_NormalizesMultimodalContent(t *testing.T) {
	var receivedBody []byte
	var mu sync.Mutex

	// Строгий upstream: проверяет, что content — строка.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"qwen","path":"qwen.gguf","state":"loaded"}]}`))
			return
		}
		if r.URL.Path == "/api/models/load" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		receivedBody = body
		mu.Unlock()

		// Строгая проверка: content должен быть строкой.
		var req struct {
			Messages []struct {
				Role    string      `json:"role"`
				Content interface{} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"` + err.Error() + `"}`))
			return
		}
		for i, m := range req.Messages {
			if _, isArr := m.Content.([]interface{}); isArr {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"content is array at ` + string(rune('0'+i)) + `"}`))
				return
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	_, router, _ := makeTestLlamaProxy(t, upstream.URL)

	// Cline-style payload: content как массив (multi-modal).
	clineStyle := `{
		"model": "qwen",
		"stream": true,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "Hello"}
			]}
		]
	}`

	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(clineStyle))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.Route(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("got 400 (multi-modal content not normalized): body=%s", rec.Body.String())
	}

	mu.Lock()
	got := append([]byte{}, receivedBody...)
	mu.Unlock()
	var gotReq struct {
		Messages []struct {
			Role    string      `json:"role"`
			Content interface{} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &gotReq); err != nil {
		t.Fatalf("upstream body is not valid JSON: %v", err)
	}
	if len(gotReq.Messages) == 0 {
		t.Fatalf("no messages in upstream body: %s", got)
	}
	if _, isArr := gotReq.Messages[0].Content.([]interface{}); isArr {
		t.Errorf("upstream received content as ARRAY (normalization failed): %v", gotReq.Messages[0].Content)
	}
	if s, isStr := gotReq.Messages[0].Content.(string); !isStr || s != "Hello" {
		t.Errorf("upstream content=%v (%T), want string \"Hello\"", gotReq.Messages[0].Content, gotReq.Messages[0].Content)
	}
}

// TestHandleChat_503WhenNoBackends — если в кластере нет ни одного llama.cpp
// backend'а, handleChat ДОЛЖЕН вернуть 503 (а не падать или возвращать 200).
func TestHandleChat_503WhenNoBackends(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends:     []types.Backend{}, // пустой список
		Balancing: types.BalancingSettings{
			Algorithm:            types.AlgorithmResourceAware,
			StreamingIdleTimeout: 30,
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false},
	}
	p := NewProxy(cfg)
	router := NewLlamaCppRouter(p)

	req := httptest.NewRequest("POST", "/api/chat",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.Route(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503 (no backends). body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no llama.cpp backend available") {
		t.Errorf("body=%q should contain 'no llama.cpp backend available'", rec.Body.String())
	}
}

// TestHandleGenerate_DispatchesToHealthyBackend — симметричный тест для
// /api/generate (раньше тоже возвращал 0/7 в baseline).
func TestHandleGenerate_DispatchesToHealthyBackend(t *testing.T) {
	var receivedBody []byte
	var mu sync.Mutex
	upstream := makeUpstreamForOllamaTest(t, &receivedBody, &mu)
	defer upstream.Close()

	_, router, _ := makeTestLlamaProxy(t, upstream.URL)

	// Ollama-формат /api/generate.
	ollamaPayload := `{
		"model": "qwen2.5-coder",
		"prompt": "Tell me a joke",
		"stream": false
	}`

	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(ollamaPayload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handled := router.Route(rec, req)
	if !handled {
		t.Fatalf("router did not handle /api/generate (status=%d body=%s)", rec.Code, rec.Body.String())
	}

	bodyStr := rec.Body.String()
	if rec.Code == http.StatusServiceUnavailable && strings.Contains(bodyStr, "no llama.cpp backend available") {
		t.Fatalf("REGRESSION: got 503 'no llama.cpp backend available' for /api/generate. body=%s", bodyStr)
	}

	mu.Lock()
	got := append([]byte{}, receivedBody...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatalf("upstream received empty body for /api/generate. recorder body: %s", bodyStr)
	}
}

// Заглушка для использования bytes.NewReader в тестах (чтобы избежать import warnings).
var _ = bytes.NewReader
var _ = time.Now
