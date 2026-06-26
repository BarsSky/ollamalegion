//go:build llama_stub

// Package rpccoordinator — e2e-тесты Worker HTTP Server.
//
// Запускаем реальные WorkerServer через httptest.NewServer, регистрируем
// их в реальном ModelCoordinator, шлём InferRequest → проверяем SliceStats.
//
// B1: тесты работают с stub-mode (LLAMA_STUB build tag), не требуют реальной llama.cpp.
package rpccoordinator

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
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/rpcworker"
	"ollama-loadbalancer/pkg/types"
)

// rpcWorkerTestHarness — обёртка для запуска rpcworker в тестах.
type rpcWorkerTestHarness struct {
	server   *httptest.Server
	worker   *rpcworker.WorkerServer
	workerID string
	host     string
	port     int
}

// startTestWorker поднимает WorkerServer через httptest.Server.
// Возвращает URL + cleanup-функцию.
func startTestWorker(t *testing.T, workerID string, models map[string]string) *rpcWorkerTestHarness {
	t.Helper()

	// tmp models dir
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

	worker := rpcworker.NewWorkerServer(cfg, nil, "rpcworker-e2e-test")

	// Обёртка: middleware + mux (как в NewWorkerServer, но без ListenAndServe).
	handler := worker.MiddlewareHandler()

	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		srv.Close()
	})

	// Парсим host:port из URL.
	addr := strings.TrimPrefix(srv.URL, "http://")
	parts := strings.Split(addr, ":")
	host := parts[0]
	var port int
	fmt.Sscanf(parts[1], "%d", &port)

	return &rpcWorkerTestHarness{
		server:   srv,
		worker:   worker,
		workerID: workerID,
		host:     host,
		port:     port,
	}
}

// loadModel хелпер для предзагрузки модели в worker (без реального HTTP).
func (h *rpcWorkerTestHarness) loadModel(t *testing.T, modelName string) {
	t.Helper()
	modelsDir := h.worker.Manager().ModelsDir()
	// Создаём пустой .gguf если ещё нет.
	dst := filepath.Join(modelsDir, modelName)
	if _, err := os.Stat(dst); err != nil {
		if err := os.WriteFile(dst, []byte("fake gguf for "+modelName), 0644); err != nil {
			t.Fatalf("write fake model: %v", err)
		}
	}
	if _, err := h.worker.Manager().LoadSlice(modelName, ""); err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
}

// infer хелпер — POST /rpc/infer через HTTP.
func (h *rpcWorkerTestHarness) infer(t *testing.T, prompt string) *SliceInferResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"model_name": "test-model.gguf",
		"prompt":     prompt,
	})
	resp, err := http.Post(h.server.URL+"/rpc/infer", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /rpc/infer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /rpc/infer: status %d", resp.StatusCode)
	}
	var out SliceInferResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode infer response: %v", err)
	}
	return &out
}

// =====================================================================
// E2E: Coordinator регистрирует worker и шлёт ему /rpc/infer
// =====================================================================

func TestE2E_RegisterWorker_HealthCheck(t *testing.T) {
	h := startTestWorker(t, "worker-1", nil)

	// Регистрируем в координаторе.
	cfg := types.RpcCoordinatorConfig{
		Enabled:     true,
		Protocol:    "http",
		Timeout:     "5s",
		MaxRetries:  1,
	}
	coord := NewModelCoordinator(cfg)

	if err := coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID:    h.workerID,
		Host:        h.host,
		Port:        h.port,
		SliceLayers: "1-32",
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}

	// Проверяем, что worker зарегистрирован.
	wc := coord.GetWorker(h.workerID)
	if wc == nil {
		t.Fatalf("worker not found after register")
	}
	if wc.WorkerID != h.workerID {
		t.Errorf("expected WorkerID=%s, got %s", h.workerID, wc.WorkerID)
	}

	// Health check через WorkerClient.
	ok, err := wc.HealthCheck()
	if err != nil {
		t.Fatalf("HealthCheck err: %v", err)
	}
	if !ok {
		t.Errorf("HealthCheck: expected ok=true")
	}
}

func TestE2E_LoadAndInfer_OneWorker(t *testing.T) {
	h := startTestWorker(t, "worker-1", map[string]string{
		"test-model.gguf": "fake gguf content",
	})
	h.loadModel(t, "test-model.gguf")

	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "5s"}
	coord := NewModelCoordinator(cfg)

	_ = coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID:    h.workerID,
		Host:        h.host,
		Port:        h.port,
		SliceLayers: "1-32",
	})

	wc := coord.GetWorker(h.workerID)
	if wc == nil {
		t.Fatal("worker not found")
	}

	// 1. LoadSlice через coordinator-клиент.
	if err := wc.LoadSlice(context.Background(), "test-model.gguf", "1-32"); err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	if wc.SliceLayers != "1-32" {
		t.Errorf("expected SliceLayers=1-32, got %s", wc.SliceLayers)
	}

	// 2. InferSlice (используем реальные поля rpccoordinator.SliceInferRequest).
	out, err := wc.InferSlice(context.Background(), SliceInferRequest{
		ModelName:  "test-model.gguf",
		StartLayer: 1,
		EndLayer:   32,
		Input:      []byte("hello e2e"),
	})
	if err != nil {
		t.Fatalf("InferSlice: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("empty infer response")
	}

	// Body — JSON rpcworker.SliceInferResponse.
	var resp rpcworker.SliceInferResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, string(out))
	}
	if resp.WorkerID != h.workerID {
		t.Errorf("expected worker_id=%s, got %s", h.workerID, resp.WorkerID)
	}
	if resp.Output == "" {
		t.Error("expected non-empty output")
	}
}

func TestE2E_TwoWorkers_PipelineInfer(t *testing.T) {
	h1 := startTestWorker(t, "worker-A", map[string]string{
		"shared-model.gguf": "fake gguf A",
	})
	h1.loadModel(t, "shared-model.gguf")

	h2 := startTestWorker(t, "worker-B", map[string]string{
		"shared-model.gguf": "fake gguf B",
	})
	h2.loadModel(t, "shared-model.gguf")

	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "5s"}
	coord := NewModelCoordinator(cfg)

	_ = coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: h1.workerID, Host: h1.host, Port: h1.port, SliceLayers: "1-16",
	})
	_ = coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: h2.workerID, Host: h2.host, Port: h2.port, SliceLayers: "17-32",
	})

	// Inference на каждом.
	for _, h := range []*rpcWorkerTestHarness{h1, h2} {
		wc := coord.GetWorker(h.workerID)
		if wc == nil {
			t.Fatalf("worker %s not registered", h.workerID)
		}
		out, err := wc.InferSlice(context.Background(), SliceInferRequest{
			ModelName: "shared-model.gguf",
			Input:     []byte("hello from " + h.workerID),
		})
		if err != nil {
			t.Fatalf("InferSlice on %s: %v", h.workerID, err)
		}
		var resp rpcworker.SliceInferResponse
		_ = json.Unmarshal(out, &resp)
		if resp.Output == "" {
			t.Errorf("worker %s returned empty output", h.workerID)
		}
	}
}

func TestE2E_WorkerHealthCheck_UnhealthyAfterStop(t *testing.T) {
	h := startTestWorker(t, "worker-1", nil)

	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "5s"}
	coord := NewModelCoordinator(cfg)
	_ = coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: h.workerID, Host: h.host, Port: h.port,
	})

	// Закрываем worker-сервер → health check должен fail.
	h.server.Close()

	wc := coord.GetWorker(h.workerID)
	ok, err := wc.HealthCheck()
	if err == nil {
		t.Errorf("expected error after server closed, got nil")
	}
	if ok {
		t.Errorf("expected ok=false after server closed, got true")
	}
	if wc.IsHealthy() {
		t.Errorf("expected wc.IsHealthy()=false after failed check")
	}
}

// TestE2E_InferTimeout — проверяем, что InferSlice с уже отменённым
// контекстом возвращает ошибку. Используем уже отменённый контекст
// (без ожидания) — это надёжнее, чем гонка с 1ms timeout, потому что
// stub-Infer в llama_stub слишком быстрый.
func TestE2E_InferTimeout(t *testing.T) {
	h := startTestWorker(t, "worker-1", map[string]string{
		"m.gguf": "fake",
	})
	h.loadModel(t, "m.gguf")

	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "5s"}
	coord := NewModelCoordinator(cfg)
	_ = coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: h.workerID, Host: h.host, Port: h.port,
	})

	wc := coord.GetWorker(h.workerID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // отменяем сразу

	_, err := wc.InferSlice(ctx, SliceInferRequest{
		ModelName: "m.gguf",
		Input:     []byte("hi"),
	})
	if err == nil {
		t.Error("expected context-cancelled error, got nil")
	}
}

func TestE2E_RegisterWorker_InvalidHost(t *testing.T) {
	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "1s"}
	coord := NewModelCoordinator(cfg)
	err := coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "invalid",
		Host:     "",
		Port:     18080,
	})
	if err == nil {
		t.Error("expected error for empty host")
	}
}

func TestE2E_MetricsEndpoint(t *testing.T) {
	h := startTestWorker(t, "worker-1", map[string]string{
		"m.gguf": "fake",
	})
	h.loadModel(t, "m.gguf")

	// Coordinator-метрика через WorkerClient.
	wc := &WorkerClient{
		WorkerID: h.workerID,
		Host:     h.host,
		Port:     h.port,
		Protocol: "http",
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		healthy: true,
	}
	// Делаем 3 infer-запроса через WorkerClient (правильный контракт).
	for i := 0; i < 3; i++ {
		out, err := wc.InferSlice(context.Background(), SliceInferRequest{
			ModelName: "m.gguf",
			Input:     []byte("hello"),
		})
		if err != nil {
			t.Fatalf("InferSlice #%d: %v", i, err)
		}
		if len(out) == 0 {
			t.Fatalf("empty response #%d", i)
		}
	}
	m, err := wc.GetMetrics()
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if m == nil {
		t.Fatal("nil metrics")
	}
	// Может быть total_infer int или float (JSON unmarshal).
	if v, ok := m["total_infer"]; !ok {
		t.Errorf("expected total_infer in metrics")
	} else {
		t.Logf("total_infer = %v", v)
	}
}

// =====================================================================
// Concurrent: 10 параллельных InferSlice от coordinator
// =====================================================================

func TestE2E_ConcurrentInferRequests(t *testing.T) {
	h := startTestWorker(t, "worker-c", map[string]string{
		"c.gguf": "fake",
	})
	h.loadModel(t, "c.gguf")

	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "10s"}
	coord := NewModelCoordinator(cfg)
	_ = coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: h.workerID, Host: h.host, Port: h.port,
	})
	wc := coord.GetWorker(h.workerID)

	const N = 10
	var ok atomic.Int64
	done := make(chan struct{}, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			out, err := wc.InferSlice(context.Background(), SliceInferRequest{
				ModelName: "c.gguf",
				Input:     []byte("req"),
			})
			if err == nil && len(out) > 0 {
				ok.Add(1)
			}
		}(i)
	}
	for i := 0; i < N; i++ {
		<-done
	}
	if ok.Load() != N {
		t.Errorf("expected all %d concurrent infers to succeed, got %d", N, ok.Load())
	}
}
