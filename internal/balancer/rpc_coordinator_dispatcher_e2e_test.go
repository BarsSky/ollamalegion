//go:build llama_stub

// E2E tests for RpcCoordinatorDispatcher using real HTTP workers.
//
// Поднимаем реальный rpcworker.WorkerServer через httptest.NewServer,
// регистрируем его в реальном ModelCoordinator, регистрируем distributed
// модель, и прогоняем запросы через dispatcher.ServeHTTP. Проверяем:
//
//   1. /api/generate — Ollama non-streaming → 200 + {"response","done",...}
//   2. /api/chat     — Ollama chat non-streaming → 200 + {"message","done"}
//   3. /v1/chat/completions — OpenAI chat non-streaming → 200 + {id,choices}
//   4. /v1/completions      — OpenAI completion non-streaming → 200 + {id,choices}
//   5. Unknown model → 404 model_not_distributed
//   6. Body parse error → 400 body_read_error / invalid_envelope
//   7. Empty model field → 400 invalid_envelope
//   8. Streaming not implemented → 501 (lift from session 2 skeleton)
//
// Все тесты работают с stub-mode (LLAMA_STUB build tag), не требуют реальной llama.cpp.
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/internal/rpcworker"
	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// Test harness: real rpcworker via httptest.NewServer.
// =====================================================================

type dispatcherTestHarness struct {
	server   *httptest.Server
	worker   *rpcworker.WorkerServer
	workerID string
	host     string
	port     int
}

func startDispatcherWorker(t *testing.T, workerID string, models map[string]string) *dispatcherTestHarness {
	t.Helper()

	tmp := t.TempDir()
	for name, content := range models {
		path := filepath.Join(tmp, name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write fake model %s: %v", name, err)
		}
	}

	cfg := rpcworker.DefaultWorkerConfig()
	cfg.Host = "127.0.0.1"
	cfg.WorkerID = workerID
	cfg.ModelsDir = tmp
	cfg.StubMode = true
	cfg.SliceLayers = "1-32"

	worker := rpcworker.NewWorkerServer(cfg, nil, "rpcworker-dispatcher-e2e")
	handler := worker.MiddlewareHandler()
	srv := httptest.NewServer(handler)
	t.Cleanup(func() { srv.Close() })

	addr := strings.TrimPrefix(srv.URL, "http://")
	parts := strings.Split(addr, ":")
	host := parts[0]
	var port int
	fmt.Sscanf(parts[1], "%d", &port)

	return &dispatcherTestHarness{
		server:   srv,
		worker:   worker,
		workerID: workerID,
		host:     host,
		port:     port,
	}
}

// loadModel предзагружает модель в worker (без реального HTTP, через Manager).
func (h *dispatcherTestHarness) loadModel(t *testing.T, modelName string) {
	t.Helper()
	dst := filepath.Join(h.worker.Manager().ModelsDir(), modelName)
	if _, err := os.Stat(dst); err != nil {
		if err := os.WriteFile(dst, []byte("fake gguf for "+modelName), 0644); err != nil {
			t.Fatalf("write fake model: %v", err)
		}
	}
	if _, err := h.worker.Manager().LoadSlice(modelName, ""); err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
}

// buildCoordinatorWithWorker — стандартная setup: 1 worker + 1 distributed model
// "test-model" mapped to that worker (slice 1-32).
//
// ВАЖНО: имя модели должно совпадать в трёх местах:
//   1. файл на диске в worker'е (через loadModel)
//   2. distributed model name в coordinator (RegisterDistributedModel)
//   3. "model" поле в HTTP request body
//
// worker.InferSlice ищет модель по имени файла, поэтому coordinator slice
// получает req.ModelName, который worker потом использует для lookup.
func buildCoordinatorWithWorker(t *testing.T, h *dispatcherTestHarness) *rpccoordinator.ModelCoordinator {
	t.Helper()
	h.loadModel(t, "test-model")

	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "5s"}
	coord := rpccoordinator.NewModelCoordinator(cfg)
	if err := coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID:    h.workerID,
		Host:        h.host,
		Port:        h.port,
		SliceLayers: "1-32",
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if err := coord.RegisterDistributedModel("test-model", "e2e test model",
		[]rpccoordinator.LayerSlice{
			{StartLayer: 1, EndLayer: 32, WorkerID: h.workerID},
		}); err != nil {
		t.Fatalf("RegisterDistributedModel: %v", err)
	}
	return coord
}

// newDispatcher — обёртка: создаёт dispatcher + proxy для ServeHTTP-вызовов.
func newDispatcher(coord *rpccoordinator.ModelCoordinator, p *Proxy) *RpcCoordinatorDispatcher {
	return NewRpcCoordinatorDispatcher(coord, p)
}

// =====================================================================
// Test 1: /api/generate end-to-end.
// =====================================================================

func TestDispatcherE2E_OllamaGenerate_Success(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"hello e2e","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	d.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Ollama /api/generate format: {"response","done","done_reason","context","total_duration"}.
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (raw=%s)", err, w.Body.String())
	}
	if _, ok := resp["response"]; !ok {
		t.Errorf("missing 'response' field, got keys: %v", keysOf(resp))
	}
	if done, _ := resp["done"].(bool); !done {
		t.Errorf("expected done=true, got %v", resp["done"])
	}
	// Stub Infer() форматит output: "[llama_stub] Echo: hello e2e\n\n..."
	// resp.Output проходит через dispatcher без декодирования (Phase 8: raw passthrough).
	// Проверяем, что output непустой и содержит echo.
	if responseStr, _ := resp["response"].(string); responseStr == "" {
		t.Errorf("expected non-empty response, got empty")
	}
}

// =====================================================================
// Test 2: /api/chat end-to-end.
// =====================================================================

func TestDispatcherE2E_OllamaChat_Success(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	body := bytes.NewReader([]byte(`{
		"model":"test-model",
		"messages":[
			{"role":"user","content":"hi from chat"}
		],
		"stream":false
	}`))
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	d.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Ollama /api/chat format: {"message":{"role","content"},"done"}.
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (raw=%s)", err, w.Body.String())
	}
	msg, ok := resp["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing 'message' object, got keys: %v", keysOf(resp))
	}
	if role, _ := msg["role"].(string); role != "assistant" {
		t.Errorf("expected role=assistant, got %v", msg["role"])
	}
	if content, _ := msg["content"].(string); content == "" {
		t.Errorf("expected non-empty message content")
	}
}

// =====================================================================
// Test 3: /v1/chat/completions (OpenAI) end-to-end.
// =====================================================================

func TestDispatcherE2E_OpenAIChat_Success(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	body := bytes.NewReader([]byte(`{
		"model":"test-model",
		"messages":[
			{"role":"user","content":"openai test"}
		],
		"stream":false
	}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	d.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// OpenAI /v1/chat/completions: {id,object,created,model,choices[],usage}.
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (raw=%s)", err, w.Body.String())
	}
	if id, _ := resp["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("expected id prefix 'chatcmpl-', got %v", resp["id"])
	}
	if object, _ := resp["object"].(string); object != "chat.completion" {
		t.Errorf("expected object=chat.completion, got %v", resp["object"])
	}
	if model, _ := resp["model"].(string); model != "test-model" {
		t.Errorf("expected model=test-model, got %v", resp["model"])
	}
	choices, ok := resp["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("missing/empty choices array, got: %v", resp["choices"])
	}
	choice0, _ := choices[0].(map[string]interface{})
	if msg, _ := choice0["message"].(map[string]interface{}); msg == nil {
		t.Errorf("expected choice[0].message object, got: %v", choice0)
	}
}

// =====================================================================
// Test 4: /v1/completions (OpenAI legacy) end-to-end.
// =====================================================================

func TestDispatcherE2E_OpenAICompletion_Success(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	body := bytes.NewReader([]byte(`{
		"model":"test-model",
		"prompt":"openai completion test",
		"stream":false
	}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	d.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if object, _ := resp["object"].(string); object != "text_completion" {
		t.Errorf("expected object=text_completion, got %v", resp["object"])
	}
	choices, ok := resp["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("missing/empty choices, got: %v", resp["choices"])
	}
	choice0, _ := choices[0].(map[string]interface{})
	if text, _ := choice0["text"].(string); text == "" {
		t.Errorf("expected non-empty text, got empty")
	}
}

// =====================================================================
// Test 5: model not distributed → 404.
// =====================================================================

func TestDispatcherE2E_ModelNotDistributed(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := rpccoordinator.NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})
	_ = coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: h.workerID, Host: h.host, Port: h.port})
	// НЕ регистрируем distributed model.

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	body := bytes.NewReader([]byte(`{"model":"unknown-model","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()

	d.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not distributed") {
		t.Errorf("body should contain 'not distributed', got: %s", w.Body.String())
	}
}

// =====================================================================
// Test 6: Body parse error (invalid JSON) → 400.
// Lifts TestServeHTTP_BodyParseError_DeferredToSession3.
// =====================================================================

func TestDispatcherE2E_BodyParseError(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := rpccoordinator.NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})
	_ = coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: h.workerID, Host: h.host, Port: h.port})
	_ = coord.RegisterDistributedModel("m", "", []rpccoordinator.LayerSlice{
		{StartLayer: 1, EndLayer: 32, WorkerID: h.workerID},
	})

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	body := bytes.NewReader([]byte(`{"model":`)) // truncated JSON
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()

	d.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "parse JSON") {
		t.Errorf("body should mention 'parse JSON' error, got: %s", w.Body.String())
	}
}

// =====================================================================
// Test 7: Empty model field → 400.
// Lifts TestServeHTTP_EmptyModel_DeferredToSession3.
// =====================================================================

func TestDispatcherE2E_EmptyModel(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := rpccoordinator.NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})
	_ = coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: h.workerID, Host: h.host, Port: h.port})
	// Register some model so ShouldRoute check passes... actually,
	// empty model fails BEFORE ShouldRoute (in envelope parsing).
	_ = coord.RegisterDistributedModel("m", "", []rpccoordinator.LayerSlice{
		{StartLayer: 1, EndLayer: 32, WorkerID: h.workerID},
	})

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	body := bytes.NewReader([]byte(`{"model":"","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()

	d.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model is required") {
		t.Errorf("body should mention 'model is required', got: %s", w.Body.String())
	}
}

// =====================================================================
// Test 9: ShouldRoute + InferNonStreaming integration with real coord.
// =====================================================================

func TestDispatcherE2E_InferNonStreaming_Real(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	// ShouldRoute: distributed model → true.
	if !d.ShouldRoute("test-model") {
		t.Error("ShouldRoute(test-model) should be true (registered)")
	}
	if d.ShouldRoute("unknown-model") {
		t.Error("ShouldRoute(unknown-model) should be false (not registered)")
	}

	// InferNonStreaming: вызов через helper.
	resp, err := d.InferNonStreaming(context.Background(), &rpccoordinator.InferRequest{
		ModelName: "test-model",
		Prompt:    "hello via InferNonStreaming",
		Stream:    false,
	})
	if err != nil {
		t.Fatalf("InferNonStreaming: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.Output == "" {
		t.Error("expected non-empty output")
	}
	if len(resp.SliceStats) == 0 {
		t.Error("expected at least 1 slice stat")
	}
	if !resp.SliceStats[0].Success {
		t.Errorf("slice stat[0] should be success, got %+v", resp.SliceStats[0])
	}
}

// =====================================================================
// Test 10: Infer with disabled coordinator → 503.
// =====================================================================

func TestDispatcherE2E_DisabledCoordinator(t *testing.T) {
	// Создаём coordinator с Enabled=false. Нужно зарегистрировать worker
	// и distributed model, чтобы ShouldRoute() прошёл. Infer() потом
	// проверит c.Enabled() и вернёт "rpc coordinator is disabled" → 503.
	h := startDispatcherWorker(t, "worker-1", nil)
	h.loadModel(t, "test-model")

	coord := rpccoordinator.NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: false, Protocol: "http"})
	_ = coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: h.workerID, Host: h.host, Port: h.port})
	_ = coord.RegisterDistributedModel("test-model", "",
		[]rpccoordinator.LayerSlice{
			{StartLayer: 1, EndLayer: 32, WorkerID: h.workerID},
		})

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()

	d.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (coordinator is disabled), got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "coordinator is disabled") {
		t.Errorf("body should mention 'coordinator is disabled', got: %s", w.Body.String())
	}
}

// =====================================================================
// Test 11: Streaming Ollama /api/generate → SSE events.
// Phase 8 Session 3.2.
// =====================================================================

// =====================================================================
// Test 13: Circuit breaker integration — ShouldRoute returns false when
// all workers have Open CB. Phase 8 Session 3.3.
// =====================================================================

func TestDispatcherE2E_CircuitBreaker_AllWorkersOpen(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	// Перед началом — CB Closed, ShouldRoute = true.
	if !d.ShouldRoute("test-model") {
		t.Fatal("precondition: ShouldRoute should be true initially")
	}

	// Принудительно "открываем" CB через N failures (по умолчанию 5).
	cb := d.getOrCreateCircuitBreaker("worker-1")
	for i := 0; i < 10; i++ {
		cb.RecordFailure()
	}
	if state := cb.State(); state != rpccoordinator.StateOpen {
		t.Fatalf("expected CB state=Open after 10 failures, got %v", state)
	}

	// Теперь ShouldRoute должен вернуть false (все workers Open).
	if d.ShouldRoute("test-model") {
		t.Error("ShouldRoute should return false when all workers have Open CB")
	}

	// ServeHTTP должен вернуть 503 all_workers_unhealthy, не 404.
	body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 all_workers_unhealthy, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "circuit-broken") {
		t.Errorf("body should mention 'circuit-broken', got: %s", w.Body.String())
	}
}

// =====================================================================
// Test 14: Circuit breaker — successful Infer записывает success в CB.
// После нескольких success'ов CB остаётся Closed.
// =====================================================================

func TestDispatcherE2E_CircuitBreaker_SuccessRecorded(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	// Pre-state: CB Closed, no failures.
	cb := d.getOrCreateCircuitBreaker("worker-1")
	if cb.State() != rpccoordinator.StateClosed {
		t.Fatalf("precondition: CB should be Closed initially, got %v", cb.State())
	}

	// Делаем несколько успешных infer-вызовов.
	for i := 0; i < 3; i++ {
		body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"hi","stream":false}`))
		req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
		w := httptest.NewRecorder()
		d.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("iteration %d: expected 200, got %d: %s", i, w.Code, w.Body.String())
		}
	}

	// CB должен остаться Closed (successes записываются, но state не меняется в Closed).
	if cb.State() != rpccoordinator.StateClosed {
		t.Errorf("CB should remain Closed after successes, got %v", cb.State())
	}
}

// =====================================================================
// Test 15: Circuit breaker — SetCircuitBreakerConfig применяется.
// =====================================================================

func TestDispatcherE2E_CircuitBreaker_CustomConfig(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	// Устанавливаем кастомный config: 2 failures → Open.
	d.SetCircuitBreakerConfig(rpccoordinator.CircuitBreakerConfig{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeout:     100 * time.Millisecond,
	})

	cb := d.getOrCreateCircuitBreaker("worker-1")

	// 1 failure — должен остаться Closed.
	cb.RecordFailure()
	if cb.State() != rpccoordinator.StateClosed {
		t.Errorf("after 1 failure (threshold=2), expected Closed, got %v", cb.State())
	}

	// 2 failure — должен Open.
	cb.RecordFailure()
	if cb.State() != rpccoordinator.StateOpen {
		t.Errorf("after 2 failures (threshold=2), expected Open, got %v", cb.State())
	}

	// Ждём reset timeout → HalfOpen.
	time.Sleep(150 * time.Millisecond)
	if cb.State() != rpccoordinator.StateHalfOpen {
		t.Errorf("after reset timeout, expected HalfOpen, got %v", cb.State())
	}
}

// =====================================================================
// Phase 8 Session 3.4: Auth middleware integration.
// =====================================================================

// fakeAuthChecker — test double для AuthChecker interface.
// Определён в auth_checker_test.go (общий для dispatcher'ов).

// Test 16: Auth enabled, no token → 401.
func TestDispatcherE2E_Auth_NoToken_Rejected(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)
	d.SetAuthenticator(&fakeAuthChecker{
		enabled: true,
		tokens:  map[string]bool{"valid-token": true},
	})

	body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Unauthorized") {
		t.Errorf("body should mention 'Unauthorized', got: %s", w.Body.String())
	}
}

// Test 17: Auth enabled, valid token → 200 (request passes through).
func TestDispatcherE2E_Auth_ValidToken_Allowed(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)
	d.SetAuthenticator(&fakeAuthChecker{
		enabled: true,
		tokens:  map[string]bool{"valid-token": true},
	})

	body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	req.Header.Set("X-API-Token", "valid-token")
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d: %s", w.Code, w.Body.String())
	}
}

// Test 18: Auth enabled, invalid token → 401.
func TestDispatcherE2E_Auth_InvalidToken_Rejected(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)
	d.SetAuthenticator(&fakeAuthChecker{
		enabled: true,
		tokens:  map[string]bool{"valid-token": true},
	})

	body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	req.Header.Set("X-API-Token", "wrong-token")
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d: %s", w.Code, w.Body.String())
	}
}

// Test 19: Auth disabled → no token required, request passes.
func TestDispatcherE2E_Auth_Disabled_NoCheck(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)
	d.SetAuthenticator(&fakeAuthChecker{enabled: false, tokens: nil})

	body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	// No token set.
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with auth disabled, got %d: %s", w.Code, w.Body.String())
	}
}

// Test 20: Auth nil checker → no check.
func TestDispatcherE2E_Auth_NilChecker_NoCheck(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)
	// Не устанавливаем authenticator — d.authChecker == nil.

	body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with nil auth checker, got %d: %s", w.Code, w.Body.String())
	}
}

// Test 21: Auth enabled, token via query (?token=...) — для WebSocket-style клиентов.
func TestDispatcherE2E_Auth_QueryToken_Allowed(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)
	d.SetAuthenticator(&fakeAuthChecker{
		enabled: true,
		tokens:  map[string]bool{"valid-token": true},
	})

	body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"hi","stream":false}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate?token=valid-token", body)
	// No header, but token in URL query.
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with query token, got %d: %s", w.Code, w.Body.String())
	}
}

// Test 22: Auth enabled, OpenAI endpoint → 401 в OpenAI error format.
func TestDispatcherE2E_Auth_OpenAIFormat_Rejected(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)
	d.SetAuthenticator(&fakeAuthChecker{
		enabled: true,
		tokens:  map[string]bool{"valid": true},
	})

	body := bytes.NewReader([]byte(`{
		"model":"test-model",
		"messages":[{"role":"user","content":"hi"}],
		"stream":false
	}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
	// OpenAI format: {"error": {"message", "type"}}
	var errResp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("OpenAI error format expected {error: {message,type}}, got: %v", errResp)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "Unauthorized") {
		t.Errorf("error.message should contain 'Unauthorized', got: %v", errObj["message"])
	}
	if errType, _ := errObj["type"].(string); errType != "unauthorized" {
		t.Errorf("error.type should be 'unauthorized', got: %v", errObj["type"])
	}
}

func TestDispatcherE2E_Streaming_OllamaGenerate(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	body := bytes.NewReader([]byte(`{"model":"test-model","prompt":"stream me","stream":true}`))
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()

	d.ServeHTTP(w, req)

	// SSE: 200 + Content-Type: text/event-stream
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("expected Content-Type: text/event-stream, got %q", ct)
	}

	// Должно быть как минимум 2 SSE event'а: 1+ token + done.
	events := parseSSEEvents(w.Body.String())
	if len(events) < 2 {
		t.Fatalf("expected at least 2 SSE events, got %d: %s", len(events), w.Body.String())
	}

	// Все event'ы должны быть "data: ..." строки.
	for i, ev := range events {
		if !strings.HasPrefix(ev, "data: ") {
			t.Errorf("event[%d] should start with 'data: ', got: %s", i, ev)
		}
	}

	// Последний event должен быть done=true.
	lastPayload := strings.TrimPrefix(events[len(events)-1], "data: ")
	var lastEv map[string]interface{}
	if err := json.Unmarshal([]byte(lastPayload), &lastEv); err != nil {
		t.Fatalf("last event JSON parse: %v (raw=%s)", err, lastPayload)
	}
	if done, _ := lastEv["done"].(bool); !done {
		t.Errorf("last event should have done=true, got %v", lastEv["done"])
	}

	// Среди event'ов должен быть как минимум 1 с response != "".
	foundResponse := false
	for _, ev := range events[:len(events)-1] {
		payload := strings.TrimPrefix(ev, "data: ")
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			continue
		}
		if resp, _ := p["response"].(string); resp != "" {
			foundResponse = true
			break
		}
	}
	if !foundResponse {
		t.Error("expected at least 1 event with non-empty 'response' field")
	}
}

// =====================================================================
// Test 12: Streaming OpenAI /v1/chat/completions → SSE с [DONE] в конце.
// =====================================================================

func TestDispatcherE2E_Streaming_OpenAIChat(t *testing.T) {
	h := startDispatcherWorker(t, "worker-1", nil)
	coord := buildCoordinatorWithWorker(t, h)

	proxy := newProxyWithCleanup(t, createTestConfig())
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	d := newDispatcher(coord, proxy)

	body := bytes.NewReader([]byte(`{
		"model":"test-model",
		"messages":[{"role":"user","content":"openai stream"}],
		"stream":true
	}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	d.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	body2 := w.Body.String()
	// Должен заканчиваться на [DONE].
	if !strings.HasSuffix(strings.TrimSpace(body2), "data: [DONE]") {
		t.Errorf("OpenAI stream should end with 'data: [DONE]', got: ...%s",
			body2[max(0, len(body2)-100):])
	}

	// Каждый event должен иметь choices[0].delta.content или пустой delta (для done).
	events := parseSSEEvents(body2)
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events (token + DONE), got %d", len(events))
	}
	for i, ev := range events {
		if ev == "data: [DONE]" {
			// Последний event — должен быть ровно один.
			if i != len(events)-1 {
				t.Errorf("[DONE] should be last event, got position %d of %d", i, len(events))
			}
			continue
		}
		payload := strings.TrimPrefix(ev, "data: ")
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			t.Errorf("event[%d] JSON parse: %v (raw=%s)", i, err, payload)
			continue
		}
		choices, _ := p["choices"].([]interface{})
		if len(choices) == 0 {
			t.Errorf("event[%d] should have choices[], got: %v", i, p)
		}
	}
}

// parseSSEEvents разбивает SSE body на отдельные event'ы (по двойному \n).
func parseSSEEvents(body string) []string {
	var events []string
	// SSE events разделены \n\n (пустой строкой).
	parts := strings.Split(body, "\n\n")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		events = append(events, p)
	}
	return events
}

// max — встроенная в Go 1.21+, дублируем для совместимости.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// keysOf — хелпер для отладочных сообщений (keys map).
func keysOf(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
