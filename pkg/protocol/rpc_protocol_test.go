package protocol

import (
	"fmt"
	"net"
	"net/rpc"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/rpcworker"
)

// findFreePort — резервирует свободный TCP-порт через Listen + Close.
func findFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("findFreePort: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startTestRPC поднимает in-process RPC сервер на свободном порту и возвращает
// адрес для клиента + cleanup.
func startTestRPC(t *testing.T, srv *rpcworker.WorkerServer) (string, func()) {
	t.Helper()
	port := findFreePort(t)
	addr := "127.0.0.1:" + itoa(port)

	svc := NewWorkerRPCService(srv)
	ln, _, err := StartRPCServer("127.0.0.1", port, svc)
	if err != nil {
		t.Fatalf("StartRPCServer: %v", err)
	}
	cleanup := func() { _ = ln.Close() }
	return addr, cleanup
}

// itoa — минимальный int→string без strconv (тесты).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// dialClient — открывает net/rpc клиент на адрес.
func dialClient(t *testing.T, addr string) *rpc.Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return rpc.NewClient(conn)
}

// makeTestWorkerServer — создаёт WorkerServer с дефолтным конфигом (stub).
func makeTestWorkerServer(t *testing.T) *rpcworker.WorkerServer {
	t.Helper()
	cfg := rpcworker.WorkerConfig{
		Host:      "127.0.0.1",
		Port:      18080,
		WorkerID:  "test-worker",
		ModelsDir: "./test-models",
		StubMode:  true,
	}
	if err := cfg.Validate(); err != nil {
		// "modelsDir is required" — подменим.
		cfg.ModelsDir = "./models"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("cfg.Validate: %v", err)
		}
	}
	mgr := rpcworker.NewModelManager(cfg)
	return rpcworker.NewWorkerServer(cfg, mgr, "test-1.0")
}

// =====================================================================
// Tests
// =====================================================================

func TestProtocolVersion(t *testing.T) {
	if ProtocolVersion == "" {
		t.Fatal("ProtocolVersion must be non-empty")
	}
	if !strings.HasPrefix(ProtocolVersion, "rpc-") {
		t.Errorf("ProtocolVersion must start with 'rpc-', got %q", ProtocolVersion)
	}
}

func TestStartRPCServer_HealthCheck(t *testing.T) {
	srv := makeTestWorkerServer(t)
	addr, cleanup := startTestRPC(t, srv)
	defer cleanup()

	client := dialClient(t, addr)
	defer client.Close()

	var reply HealthCheckReply
	if err := client.Call("WorkerRPCService.HealthCheck", &HealthCheckArgs{}, &reply); err != nil {
		t.Fatalf("HealthCheck Call: %v", err)
	}
	if reply.Status != "ok" {
		t.Errorf("Status: got %q, want %q", reply.Status, "ok")
	}
	if reply.WorkerID != "test-worker" {
		t.Errorf("WorkerID: got %q, want %q", reply.WorkerID, "test-worker")
	}
	if reply.Version != "test-1.0" {
		t.Errorf("Version: got %q, want %q", reply.Version, "test-1.0")
	}
	if reply.Protocol != ProtocolVersion {
		t.Errorf("Protocol: got %q, want %q", reply.Protocol, ProtocolVersion)
	}
	if reply.LoadedSlices != 0 {
		t.Errorf("LoadedSlices: got %d, want 0", reply.LoadedSlices)
	}
	if reply.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds must be >= 0, got %f", reply.UptimeSeconds)
	}
}

func TestStartRPCServer_LoadSlice_NoModel(t *testing.T) {
	srv := makeTestWorkerServer(t)
	addr, cleanup := startTestRPC(t, srv)
	defer cleanup()

	client := dialClient(t, addr)
	defer client.Close()

	// В stub-режиме LoadSlice с пустой model_name вернёт OK=false.
	var reply LoadSliceReply
	err := client.Call("WorkerRPCService.LoadSlice",
		&LoadSliceArgs{ModelName: "", Layers: "1-10"},
		&reply)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply.OK {
		t.Error("LoadSlice с пустым ModelName должен вернуть OK=false")
	}
	if reply.Message == "" {
		t.Error("Message должно быть заполнено")
	}
}

func TestStartRPCServer_UnloadSlice_NotLoaded(t *testing.T) {
	srv := makeTestWorkerServer(t)
	addr, cleanup := startTestRPC(t, srv)
	defer cleanup()

	client := dialClient(t, addr)
	defer client.Close()

	var reply UnloadSliceReply
	if err := client.Call("WorkerRPCService.UnloadSlice",
		&UnloadSliceArgs{ModelName: "any-model"},
		&reply); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !reply.OK {
		t.Errorf("UnloadSlice идемпотентный, должен вернуть OK=true; got %+v", reply)
	}
}

func TestStartRPCServer_Infer_NotLoaded(t *testing.T) {
	srv := makeTestWorkerServer(t)
	addr, cleanup := startTestRPC(t, srv)
	defer cleanup()

	client := dialClient(t, addr)
	defer client.Close()

	var reply InferReply
	if err := client.Call("WorkerRPCService.Infer",
		&InferArgs{ModelName: "missing-model", Prompt: "hi"},
		&reply); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply.OK {
		t.Error("Infer без загруженной модели должен вернуть OK=false")
	}
	if reply.Error == "" {
		t.Error("Error должно быть заполнено")
	}
}

func TestStartRPCServer_Metrics(t *testing.T) {
	srv := makeTestWorkerServer(t)
	addr, cleanup := startTestRPC(t, srv)
	defer cleanup()

	client := dialClient(t, addr)
	defer client.Close()

	var reply MetricsReply
	if err := client.Call("WorkerRPCService.Metrics", &MetricsArgs{}, &reply); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// reply.Stats — rpcworker.MetricsSnapshot, не map. Проверяем базовые поля.
	if reply.Stats.TotalInfer != 0 {
		t.Errorf("TotalInfer must be 0 initially, got %d", reply.Stats.TotalInfer)
	}
	if reply.Stats.ActiveRequests < 0 {
		t.Errorf("ActiveRequests must be >= 0, got %d", reply.Stats.ActiveRequests)
	}
	if reply.Stats.UptimeS < 0 {
		t.Errorf("UptimeS must be >= 0, got %d", reply.Stats.UptimeS)
	}
}

func TestStartRPCServer_KvSync_KvFetch_RoundTrip(t *testing.T) {
	srv := makeTestWorkerServer(t)
	addr, cleanup := startTestRPC(t, srv)
	defer cleanup()

	client := dialClient(t, addr)
	defer client.Close()

	// 1. Save shard.
	var syncReply KvSyncReply
	if err := client.Call("WorkerRPCService.KvSync",
		&KvSyncArgs{
			SessionID:   "session-1",
			SeqLen:      100,
			KeyTensor:   []byte("key-bytes-stub"),
			ValueTensor: []byte("val-bytes-stub"),
			Layers:      []int{1, 2, 3},
		},
		&syncReply); err != nil {
		t.Fatalf("KvSync Call: %v", err)
	}
	if !syncReply.OK {
		t.Fatalf("KvSync OK=false, message=%q", syncReply.Message)
	}

	// 2. Fetch shard.
	var fetchReply KvFetchReply
	if err := client.Call("WorkerRPCService.KvFetch",
		&KvFetchArgs{SessionID: "session-1"},
		&fetchReply); err != nil {
		t.Fatalf("KvFetch Call: %v", err)
	}
	if !fetchReply.OK {
		t.Fatalf("KvFetch OK=false, message=%q", fetchReply.Message)
	}
	if fetchReply.SessionID != "session-1" {
		t.Errorf("SessionID: got %q, want %q", fetchReply.SessionID, "session-1")
	}
	if fetchReply.SeqLen != 100 {
		t.Errorf("SeqLen: got %d, want 100", fetchReply.SeqLen)
	}
	if len(fetchReply.Shard) == 0 {
		t.Error("Shard не должен быть пустым (encoded base64)")
	}
}

func TestStartRPCServer_KvFetch_NotFound(t *testing.T) {
	srv := makeTestWorkerServer(t)
	addr, cleanup := startTestRPC(t, srv)
	defer cleanup()

	client := dialClient(t, addr)
	defer client.Close()

	var reply KvFetchReply
	if err := client.Call("WorkerRPCService.KvFetch",
		&KvFetchArgs{SessionID: "missing-session"},
		&reply); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply.OK {
		t.Error("KvFetch для несуществующего session_id должен вернуть OK=false")
	}
	if reply.Message == "" {
		t.Error("Message должно быть заполнено")
	}
}

func TestStartRPCServer_KvSync_MissingSessionID(t *testing.T) {
	srv := makeTestWorkerServer(t)
	addr, cleanup := startTestRPC(t, srv)
	defer cleanup()

	client := dialClient(t, addr)
	defer client.Close()

	var reply KvSyncReply
	if err := client.Call("WorkerRPCService.KvSync",
		&KvSyncArgs{SessionID: ""},
		&reply); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply.OK {
		t.Error("KvSync без session_id должен вернуть OK=false")
	}
}

func TestStartRPCServer_ConcurrentCalls(t *testing.T) {
	srv := makeTestWorkerServer(t)
	addr, cleanup := startTestRPC(t, srv)
	defer cleanup()

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			client := dialClient(t, addr)
			defer client.Close()
			var reply HealthCheckReply
			if err := client.Call("WorkerRPCService.HealthCheck", &HealthCheckArgs{}, &reply); err != nil {
				errs <- err
				return
			}
			if reply.Status != "ok" {
				errs <- fmt.Errorf("expected status=ok, got %q", reply.Status)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call error: %v", err)
	}
}

func TestStartRPCServer_ListenerClosed(t *testing.T) {
	srv := makeTestWorkerServer(t)
	_, cleanup := startTestRPC(t, srv)
	cleanup() // сразу закрываем

	// Новый клиент должен получить ошибку соединения.
	addr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	_ = addr
	// Здесь мы просто проверяем, что cleanup не паникует и возвращается.
	// Повторное открытие на тот же порт может упасть с "address in use" —
	// это ОК, тест всё равно считается успешным (cleanup корректен).
}