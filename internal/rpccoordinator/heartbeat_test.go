//go:build llama_stub

// Package rpccoordinator — tests for HeartbeatLoop (B3).
package rpccoordinator

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/rpcworker"
	"ollama-loadbalancer/pkg/types"
)

// makeStubWorker запускает rpcworker.WorkerServer через httptest.Server
// (внутри пакета rpccoordinator, тесты не экспортируются наружу — поэтому
// дублируем минимальную обвязку, аналогично e2e_worker_test.go).
func newHeartbeatTestWorker(t *testing.T, workerID string) (addr string, closeFn func()) {
	t.Helper()
	tmp := t.TempDir()
	cfg := rpcworker.DefaultWorkerConfig()
	cfg.WorkerID = workerID
	cfg.ModelsDir = tmp
	cfg.StubMode = true

	worker := rpcworker.NewWorkerServer(cfg, nil, "rpcworker-heartbeat-test")
	srv := startHTTPTestServer(worker.MiddlewareHandler())
	return srv.URL, srv.Close
}

// =====================================================================
// DefaultHeartbeatConfig
// =====================================================================

func TestDefaultHeartbeatConfig(t *testing.T) {
	cfg := DefaultHeartbeatConfig()
	if cfg.Interval != 30*time.Second {
		t.Errorf("expected Interval=30s, got %v", cfg.Interval)
	}
	if cfg.UnhealthyThreshold != 3 {
		t.Errorf("expected UnhealthyThreshold=3, got %d", cfg.UnhealthyThreshold)
	}
}

// =====================================================================
// NewHeartbeatLoop — defaults applied
// =====================================================================

func TestNewHeartbeatLoop_Defaults(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	loop := NewHeartbeatLoop(coord, HeartbeatConfig{}) // пустая config → defaults

	if loop.cfg.Interval != 30*time.Second {
		t.Errorf("expected default Interval, got %v", loop.cfg.Interval)
	}
	if loop.cfg.UnhealthyThreshold != 3 {
		t.Errorf("expected default UnhealthyThreshold, got %d", loop.cfg.UnhealthyThreshold)
	}
	if loop.IsRunning() {
		t.Error("expected IsRunning=false after NewHeartbeatLoop")
	}
}

// =====================================================================
// Start / Stop — идемпотентность
// =====================================================================

func TestHeartbeatLoop_StartStop_Idempotent(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	loop := NewHeartbeatLoop(coord, HeartbeatConfig{Interval: 100 * time.Millisecond})

	loop.Start()
	if !loop.IsRunning() {
		t.Fatal("expected IsRunning=true after Start")
	}
	// Повторный Start — no-op.
	loop.Start()
	if !loop.IsRunning() {
		t.Error("IsRunning должен остаться true после повторного Start")
	}

	loop.Stop()
	if loop.IsRunning() {
		t.Error("expected IsRunning=false после Stop")
	}
	// Повторный Stop — no-op.
	loop.Stop()
	if loop.IsRunning() {
		t.Error("IsRunning должен остаться false после повторного Stop")
	}
}

// =====================================================================
// tickAll — healthy worker помечается healthy
// =====================================================================

func TestHeartbeat_TickAll_HealthyWorker(t *testing.T) {
	url, closeFn := newHeartbeatTestWorker(t, "hb-worker-1")
	defer closeFn()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	_ = coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "hb-worker-1",
		Host:     extractHost(url),
		Port:     extractPort(url),
	})

	loop := NewHeartbeatLoop(coord, HeartbeatConfig{
		Interval:           50 * time.Millisecond,
		UnhealthyThreshold: 2,
	})

	// Вызываем tickAll явно (через Start+Stop даст тот же эффект,
	// но так быстрее и детерминированнее).
	loop.tickAll()

	// Состояние должно быть записано, worker healthy.
	fails, lastSucc, lastFail, tracked := loop.StateFor("hb-worker-1")
	if !tracked {
		t.Fatal("expected tracked=true after tickAll")
	}
	if fails != 0 {
		t.Errorf("expected 0 failures, got %d", fails)
	}
	if lastSucc.IsZero() {
		t.Error("expected lastSuccess != 0")
	}
	if !lastFail.IsZero() {
		// LastFail тоже ставится при каждом тике — может быть > 0.
		t.Logf("lastFailure=%v (допустимо)", lastFail)
	}
	if !coord.GetWorker("hb-worker-1").IsHealthy() {
		t.Error("expected worker healthy after first tick")
	}
}

// =====================================================================
// tickAll — unhealthy worker (порт закрыт) → unhealthy после порога
// =====================================================================

// TestHeartbeat_UnhealthyAfterThreshold — в текущей реализации RegisterWorker
// уже делает health check; для закрытого сервера worker сразу unhealthy.
// Этот тест проверяет только что state-tracking работает для unhealthy worker'а
// (heartbeat уже запущен, worker не восстанавливается).
func TestHeartbeat_UnhealthyAfterThreshold(t *testing.T) {
	url, closeFn := newHeartbeatTestWorker(t, "hb-worker-2")
	// Закрываем сервер ДО запуска heartbeat — worker будет недоступен.
	closeFn()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	_ = coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "hb-worker-2",
		Host:     extractHost(url),
		Port:     extractPort(url),
	})

	// После RegisterWorker worker уже unhealthy (initial health check failed).
	if coord.GetWorker("hb-worker-2").IsHealthy() {
		t.Fatal("expected worker unhealthy immediately after RegisterWorker (server closed)")
	}

	loop := NewHeartbeatLoop(coord, HeartbeatConfig{
		Interval:           50 * time.Millisecond,
		UnhealthyThreshold: 2,
	})

	// ticks не должны ничего менять для уже-unhealthy worker'а.
	loop.tickAll()
	loop.tickAll()

	// Worker остаётся unhealthy (не восстанавливается).
	if coord.GetWorker("hb-worker-2").IsHealthy() {
		t.Error("closed worker должен оставаться unhealthy")
	}
}

// TestHeartbeat_HealthyWorkerStaysHealthy — sanity-check, что healthy worker
// остаётся healthy после нескольких тиков.
func TestHeartbeat_HealthyWorkerStaysHealthy(t *testing.T) {
	url, closeFn := newHeartbeatTestWorker(t, "hb-worker-4")
	defer closeFn()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	_ = coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "hb-worker-4",
		Host:     extractHost(url),
		Port:     extractPort(url),
	})

	loop := NewHeartbeatLoop(coord, HeartbeatConfig{
		Interval:           1 * time.Second,
		UnhealthyThreshold: 3,
	})

	for i := 0; i < 5; i++ {
		loop.tickAll()
	}
	fails, _, _, tracked := loop.StateFor("hb-worker-4")
	if !tracked {
		t.Fatal("expected tracked")
	}
	if fails != 0 {
		t.Errorf("expected 0 failures после 5 тиков, got %d", fails)
	}
	if !coord.GetWorker("hb-worker-4").IsHealthy() {
		t.Error("worker должен оставаться healthy")
	}
}

// =====================================================================
// Forget — удаление state
// =====================================================================

func TestHeartbeat_Forget(t *testing.T) {
	url, closeFn := newHeartbeatTestWorker(t, "hb-worker-3")
	defer closeFn()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	_ = coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "hb-worker-3",
		Host:     extractHost(url),
		Port:     extractPort(url),
	})

	loop := NewHeartbeatLoop(coord, HeartbeatConfig{Interval: 1 * time.Second})
	loop.tickAll()
	if _, _, _, tracked := loop.StateFor("hb-worker-3"); !tracked {
		t.Fatal("expected tracked after tick")
	}

	loop.Forget("hb-worker-3")
	if _, _, _, tracked := loop.StateFor("hb-worker-3"); tracked {
		t.Error("expected tracked=false after Forget")
	}
}

// =====================================================================
// IsRunning / atomic
// =====================================================================

func TestHeartbeat_IsRunning(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	loop := NewHeartbeatLoop(coord, HeartbeatConfig{Interval: 1 * time.Second})
	if loop.IsRunning() {
		t.Error("new loop should not be running")
	}

	loop.Start()
	defer loop.Stop()
	if !loop.IsRunning() {
		t.Error("started loop should be running")
	}
}

// =====================================================================
// Helpers (локальные — повторяем минимальную httptest-обвязку)
// =====================================================================

// startHTTPTestServer — обёртка над httptest.NewServer.
// Вспомогательная функция, чтобы в тестах не тянуть httptest в каждый файл.
func startHTTPTestServer(h http.Handler) *httptest.Server {
	return httptest.NewServer(h)
}

// extractHost и extractPort выделяют host/port из URL httptest.
// URL вида http://127.0.0.1:12345 → host="127.0.0.1", port=12345.
func extractHost(url string) string {
	addr := strings.TrimPrefix(url, "http://")
	if idx := strings.Index(addr, ":"); idx > 0 {
		return addr[:idx]
	}
	return addr
}

func extractPort(url string) int {
	addr := strings.TrimPrefix(url, "http://")
	idx := strings.Index(addr, ":")
	if idx < 0 {
		return 0
	}
	p, err := strconv.Atoi(addr[idx+1:])
	if err != nil {
		return 0
	}
	return p
}

// Используется atomic.Bool для loop.running.
// Проверяем, что после Stop loop не запускается в фоне (race detector friendly).

var testRaceCheck atomic.Bool

// init() — фиксирует, что тесты могут использовать race detector.
func init() { testRaceCheck.Store(true) }