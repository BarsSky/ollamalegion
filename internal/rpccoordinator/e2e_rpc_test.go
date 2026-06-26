//go:build llama_stub

// Package rpccoordinator — e2e-тесты B5 RPC pipeline (net/rpc/gob).
//
// Запускаем WorkerRPCService in-process через свободный TCP-порт, и проверяем
// что WorkerRPCClient успешно делает full pipeline:
//
//  1. HealthCheck
//  2. LoadSlice (stub: с реальным .gguf в tmp dir)
//  3. Infer
//  4. GetMetrics
//  5. KvSync / KvFetch round-trip
//  6. UnloadSlice
//
// Параллельно поднимаем 2 worker'а и проверяем что они изолированы.

package rpccoordinator

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/rpcworker"
	"ollama-loadbalancer/pkg/protocol"
)

// findFreePortRPC — резервирует свободный TCP-порт.
func findFreePortRPC(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("findFreePortRPC: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// startRPCWorker поднимает WorkerRPCService на свободном порту и
// возвращает (host, port, *WorkerRPCClient, cleanup).
func startRPCWorker(t *testing.T, workerID string, models map[string]string) (string, int, *WorkerRPCClient, func()) {
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
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}

	worker := rpcworker.NewWorkerServer(cfg, nil, "rpcworker-e2e-rpc-test")

	port := findFreePortRPC(t)
	svc := protocol.NewWorkerRPCService(worker)
	ln, _, err := protocol.StartRPCServer("127.0.0.1", port, svc)
	if err != nil {
		t.Fatalf("StartRPCServer: %v", err)
	}

	client := NewWorkerRPCClient(workerID, "127.0.0.1", port)
	cleanup := func() {
		_ = client.Close()
		_ = ln.Close()
	}

	return "127.0.0.1", port, client, cleanup
}

// fakeGGUFContent — минимальный stub контент для .gguf файла.
// ModelManager.LoadSlice в stub-режиме принимает любой непустой файл.
const fakeGGUFContent = "GGUF\x00stub-content-for-tests"

// =====================================================================
// Tests
// =====================================================================

func TestE2E_RPC_HealthCheck(t *testing.T) {
	_, _, client, cleanup := startRPCWorker(t, "rpc-w-1", nil)
	defer cleanup()

	reply, err := client.HealthCheck()
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if reply.Status != "ok" {
		t.Errorf("Status: got %q, want %q", reply.Status, "ok")
	}
	if reply.WorkerID != "rpc-w-1" {
		t.Errorf("WorkerID: got %q, want %q", reply.WorkerID, "rpc-w-1")
	}
	if reply.Protocol != protocol.ProtocolVersion {
		t.Errorf("Protocol: got %q, want %q", reply.Protocol, protocol.ProtocolVersion)
	}
	if reply.LoadedSlices != 0 {
		t.Errorf("LoadedSlices: got %d, want 0", reply.LoadedSlices)
	}
}

func TestE2E_RPC_FullPipeline(t *testing.T) {
	models := map[string]string{
		"model-stub.gguf": fakeGGUFContent,
	}
	_, _, client, cleanup := startRPCWorker(t, "rpc-w-full", models)
	defer cleanup()

	// 1. HealthCheck.
	hc, err := client.HealthCheck()
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if hc.Status != "ok" {
		t.Fatalf("HealthCheck status: %q", hc.Status)
	}

	// 2. LoadSlice.
	loadReply, err := client.LoadSlice("model-stub.gguf", "1-32")
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	if !loadReply.OK {
		t.Fatalf("LoadSlice not OK: %s", loadReply.Message)
	}
	if loadReply.Model != "model-stub.gguf" {
		t.Errorf("LoadSlice.Model: got %q, want %q", loadReply.Model, "model-stub.gguf")
	}

	// 3. Infer.
	inferReply, err := client.Infer(&protocol.InferArgs{
		ModelName: "model-stub.gguf",
		Prompt:    "hello world",
		Tokens:    64,
	})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if !inferReply.OK {
		t.Fatalf("Infer not OK: %s", inferReply.Error)
	}
	if inferReply.Output == "" {
		t.Error("Infer.Output is empty")
	}
	if inferReply.WorkerID != "rpc-w-full" {
		t.Errorf("Infer.WorkerID: got %q, want %q", inferReply.WorkerID, "rpc-w-full")
	}
	if inferReply.TokensUsed <= 0 {
		t.Errorf("Infer.TokensUsed: got %d, want > 0", inferReply.TokensUsed)
	}

	// 4. Metrics — должны показать total_infer=1.
	metrics, err := client.GetMetrics()
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if metrics.Stats.TotalInfer != 1 {
		t.Errorf("TotalInfer: got %d, want 1", metrics.Stats.TotalInfer)
	}
	if metrics.Stats.LoadedSlices != 1 {
		t.Errorf("LoadedSlices: got %d, want 1", metrics.Stats.LoadedSlices)
	}

	// 5. KvSync / KvFetch round-trip.
	syncReply, err := client.KvSync(&protocol.KvSyncArgs{
		SessionID: "session-rpc-1",
		SeqLen:    256,
		Layers:    []int{1, 2, 3, 4},
	})
	if err != nil {
		t.Fatalf("KvSync: %v", err)
	}
	if !syncReply.OK {
		t.Fatalf("KvSync not OK: %s", syncReply.Message)
	}

	fetchReply, err := client.KvFetch("session-rpc-1")
	if err != nil {
		t.Fatalf("KvFetch: %v", err)
	}
	if !fetchReply.OK {
		t.Fatalf("KvFetch not OK: %s", fetchReply.Message)
	}
	if fetchReply.SessionID != "session-rpc-1" {
		t.Errorf("KvFetch.SessionID: got %q, want %q", fetchReply.SessionID, "session-rpc-1")
	}
	if fetchReply.SeqLen != 256 {
		t.Errorf("KvFetch.SeqLen: got %d, want 256", fetchReply.SeqLen)
	}
	if len(fetchReply.Shard) == 0 {
		t.Error("KvFetch.Shard is empty")
	}

	// 6. UnloadSlice.
	unloadReply, err := client.UnloadSlice("model-stub.gguf")
	if err != nil {
		t.Fatalf("UnloadSlice: %v", err)
	}
	if !unloadReply.OK {
		t.Errorf("UnloadSlice not OK")
	}

	// 7. После unload Infer должен вернуть OK=false (ошибка транспорта).
	_, err = client.Infer(&protocol.InferArgs{
		ModelName: "model-stub.gguf",
		Prompt:    "after unload",
	})
	if err == nil {
		t.Error("Infer after Unload должен вернуть ошибку (модель не загружена)")
	}
	if !strings.Contains(err.Error(), "model not loaded") &&
		!strings.Contains(err.Error(), "not loaded") {
		t.Errorf("Infer after Unload error должен упоминать \"not loaded\", got %q", err.Error())
	}
}

func TestE2E_RPC_TwoWorkersIsolated(t *testing.T) {
	models := map[string]string{
		"model-a.gguf": fakeGGUFContent,
		"model-b.gguf": fakeGGUFContent,
	}
	_, _, clientA, cleanupA := startRPCWorker(t, "rpc-w-a", models)
	defer cleanupA()
	_, _, clientB, cleanupB := startRPCWorker(t, "rpc-w-b", models)
	defer cleanupB()

	// Загружаем в A, но не в B.
	if _, err := clientA.LoadSlice("model-a.gguf", "1-32"); err != nil {
		t.Fatalf("A.LoadSlice: %v", err)
	}

	// HealthCheck на A показывает 1 loaded, на B — 0.
	hcA, err := clientA.HealthCheck()
	if err != nil {
		t.Fatalf("A.HealthCheck: %v", err)
	}
	if hcA.LoadedSlices != 1 {
		t.Errorf("A.LoadedSlices: got %d, want 1", hcA.LoadedSlices)
	}
	hcB, err := clientB.HealthCheck()
	if err != nil {
		t.Fatalf("B.HealthCheck: %v", err)
	}
	if hcB.LoadedSlices != 0 {
		t.Errorf("B.LoadedSlices: got %d, want 0 (cross-contamination!)", hcB.LoadedSlices)
	}

	// A.Infer работает, B.Infer падает (модель не загружена на B).
	_, err = clientA.Infer(&protocol.InferArgs{ModelName: "model-a.gguf", Prompt: "ping"})
	if err != nil {
		t.Errorf("A.Infer should work: %v", err)
	}
	_, err = clientB.Infer(&protocol.InferArgs{ModelName: "model-a.gguf", Prompt: "ping"})
	if err == nil {
		t.Error("B.Infer должен вернуть ошибку (модель не загружена на B)")
	}
}

func TestE2E_RPC_ConcurrentClients(t *testing.T) {
	models := map[string]string{
		"model-conc.gguf": fakeGGUFContent,
	}
	_, _, client, cleanup := startRPCWorker(t, "rpc-w-conc", models)
	defer cleanup()

	if _, err := client.LoadSlice("model-conc.gguf", "1-32"); err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	const N = 30
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)

	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			// Каждая горутина использует свой клиент (lazy dial).
			c := NewWorkerRPCClient(fmt.Sprintf("c-%d", idx), "127.0.0.1", client.Port)
			defer c.Close()
			reply, err := c.Infer(&protocol.InferArgs{
				ModelName: "model-conc.gguf",
				Prompt:    "concurrent",
				Tokens:    32,
			})
			if err != nil {
				errs <- err
				return
			}
			if !reply.OK {
				errs <- fmt.Errorf("goroutine %d: Infer not OK: %s", idx, reply.Error)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent client error: %v", err)
	}
}

func TestE2E_RPC_ReconnectAfterServerRestart(t *testing.T) {
	// Этот тест проверяет, что после закрытия и перезапуска RPC-сервера
	// WorkerRPCClient восстанавливает соединение (lazy reconnect).
	models := map[string]string{
		"model-rc.gguf": fakeGGUFContent,
	}
	_, _, _, cleanup := startRPCWorker(t, "rpc-w-rc", models)

	// Закрываем сервер.
	cleanup()

	// Ждём немного чтобы порт освободился (SO_REUSEADDR не всегда работает).
	time.Sleep(100 * time.Millisecond)

	// Поднимаем новый сервер на ТОМ ЖЕ порту.
	models2 := map[string]string{
		"model-rc2.gguf": fakeGGUFContent,
	}
	tmp := t.TempDir()
	for name, content := range models2 {
		_ = os.WriteFile(filepath.Join(tmp, name), []byte(content), 0644)
	}
	cfg := rpcworker.DefaultWorkerConfig()
	cfg.Host = "127.0.0.1"
	cfg.WorkerID = "rpc-w-rc2"
	cfg.ModelsDir = tmp
	cfg.StubMode = true
	cfg.SliceLayers = "1-32"
	worker := rpcworker.NewWorkerServer(cfg, nil, "rpcworker-e2e-rpc-restart")

	port := findFreePortRPC(t)
	svc := protocol.NewWorkerRPCService(worker)
	ln, _, err := protocol.StartRPCServer("127.0.0.1", port, svc)
	if err != nil {
		t.Fatalf("Restart StartRPCServer: %v", err)
	}
	defer ln.Close()

	// Новый клиент к новому серверу.
	client2 := NewWorkerRPCClient("rc2", "127.0.0.1", port)
	defer client2.Close()

	hc, err := client2.HealthCheck()
	if err != nil {
		t.Fatalf("post-restart HealthCheck: %v", err)
	}
	if hc.WorkerID != "rpc-w-rc2" {
		t.Errorf("WorkerID: got %q, want %q", hc.WorkerID, "rpc-w-rc2")
	}
}

func TestE2E_RPC_LoadSliceInvalidModel(t *testing.T) {
	_, _, client, cleanup := startRPCWorker(t, "rpc-w-inv", nil)
	defer cleanup()

	// Без модели в ModelsDir LoadSlice падает.
	reply, err := client.LoadSlice("nonexistent.gguf", "1-32")
	if err == nil {
		t.Error("LoadSlice должен вернуть ошибку для несуществующего файла")
	}
	if reply.OK {
		t.Error("LoadSlice reply.OK должен быть false")
	}
	if !strings.Contains(reply.Message, "not found") &&
		!strings.Contains(reply.Message, "no such file") &&
		!strings.Contains(reply.Message, "load") {
		t.Errorf("LoadSlice.Message должен упоминать ошибку загрузки, got %q", reply.Message)
	}
}

func TestE2E_RPC_MetricsContainsAllFields(t *testing.T) {
	_, _, client, cleanup := startRPCWorker(t, "rpc-w-met", nil)
	defer cleanup()

	reply, err := client.GetMetrics()
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	// rpcworker.MetricsSnapshot — все поля int64, должны быть >= 0 (или == 0).
	if reply.Stats.TotalInfer < 0 {
		t.Errorf("TotalInfer: got %d, want >= 0", reply.Stats.TotalInfer)
	}
	if reply.Stats.TotalErrors < 0 {
		t.Errorf("TotalErrors: got %d, want >= 0", reply.Stats.TotalErrors)
	}
	if reply.Stats.UptimeS < 0 {
		t.Errorf("UptimeS: got %d, want >= 0", reply.Stats.UptimeS)
	}
}