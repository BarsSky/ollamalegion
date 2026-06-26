//go:build llama_stub

// Package rpccoordinator — B6: e2e тест failover через selector.
//
// Поднимаем 2 worker'а (httptest.Server), регистрируем distributed model
// с WorkerCandidates=[primary, secondary], и проверяем:
//  1. Infer выбирает наименее загруженного.
//  2. Если primary возвращает 500 — failover на secondary.

package rpccoordinator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// failOnNthWorker — worker-стаб, который возвращает 500 первые failTimes вызовов,
// затем начинает работать нормально.
type failOnNthWorker struct {
	server    *httptest.Server
	failTimes int32 // remaining failures
	calls     int32
}

func newFailOnNthWorker(t *testing.T, failTimes int32) *failOnNthWorker {
	t.Helper()
	fw := &failOnNthWorker{failTimes: failTimes}

	mux := http.NewServeMux()
	mux.HandleFunc("/rpc/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","worker_id":"stub"}`))
	})
	mux.HandleFunc("/rpc/load", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"loaded"}`))
	})
	mux.HandleFunc("/rpc/unload", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/rpc/infer", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fw.calls, 1)
		remaining := atomic.LoadInt32(&fw.failTimes)
		if remaining > 0 {
			atomic.AddInt32(&fw.failTimes, -1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"transient","message":"simulated failure"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"worker_id":"stub","slice_id":"1-32","output":"OK","tokens_used":1,"latency_ms":10}`))
	})
	mux.HandleFunc("/rpc/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"active_requests":0,"loaded_slices":1,"total_infer":0,"total_errors":0,"total_load_ok":0,"total_load_err":0,"avg_latency_ms":0,"uptime_s":1}`))
	})

	srv := httptest.NewServer(mux)
	fw.server = srv
	t.Cleanup(func() { srv.Close() })
	return fw
}

// hostPortOf — разбирает "127.0.0.1:12345" на host и port.
func hostPortOf(url string) (string, int) {
	addr := strings.TrimPrefix(url, "http://")
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, 0
	}
	host := addr[:idx]
	port, _ := strconv.Atoi(addr[idx+1:])
	return host, port
}

func TestE2E_Selector_FailoverToSecondary(t *testing.T) {
	// Primary worker всегда 500.
	primary := newFailOnNthWorker(t, 1000)
	// Secondary worker успешный.
	secondary := newFailOnNthWorker(t, 0)

	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "5s"}
	c := NewModelCoordinator(cfg)
	defer c.Close()

	primaryHost, primaryPort := hostPortOf(primary.server.URL)
	secondaryHost, secondaryPort := hostPortOf(secondary.server.URL)

	c.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "primary", Host: primaryHost, Port: primaryPort,
	})
	c.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "secondary", Host: secondaryHost, Port: secondaryPort,
	})

	err := c.RegisterDistributedModel("dist-model", "test",
		[]LayerSlice{{
			StartLayer: 1, EndLayer: 32,
			WorkerID:         "primary",
			WorkerCandidates: []string{"primary", "secondary"},
		}})
	if err != nil {
		t.Fatalf("RegisterDistributedModel: %v", err)
	}

	c.SetSelector(NewLeastLoadedSelector())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.Infer(ctx, InferRequest{
		ModelName: "dist-model",
		Prompt:    "hello",
	})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if !strings.Contains(resp.Output, "OK") {
		t.Errorf("Output: got %q, want contains OK", resp.Output)
	}
	// При failover в SliceResults будет 2 записи: неудачная primary и успешная secondary.
	// Ищем successful запись и проверяем что WorkerID — secondary.
	var succeededOn string
	primaryFailed := false
	for _, s := range resp.SliceStats {
		if s.WorkerID == "primary" && !s.Success {
			primaryFailed = true
		}
		if s.WorkerID == "secondary" && s.Success {
			succeededOn = "secondary"
		}
	}
	if !primaryFailed {
		t.Errorf("ожидалась неудачная попытка на primary, SliceStats=%+v", resp.SliceStats)
	}
	if succeededOn != "secondary" {
		t.Errorf("Success.WorkerID: got %q, want secondary (failover); SliceStats=%+v",
			succeededOn, resp.SliceStats)
	}

	if atomic.LoadInt32(&primary.calls) < 1 {
		t.Errorf("primary.calls=%d, want >= 1", atomic.LoadInt32(&primary.calls))
	}
	if atomic.LoadInt32(&secondary.calls) != 1 {
		t.Errorf("secondary.calls=%d, want 1", atomic.LoadInt32(&secondary.calls))
	}
}

func TestE2E_Selector_PrimaryHealthy(t *testing.T) {
	primary := newFailOnNthWorker(t, 0)
	secondary := newFailOnNthWorker(t, 0)

	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "5s"}
	c := NewModelCoordinator(cfg)
	defer c.Close()

	primaryHost, primaryPort := hostPortOf(primary.server.URL)
	secondaryHost, secondaryPort := hostPortOf(secondary.server.URL)

	c.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "primary", Host: primaryHost, Port: primaryPort,
	})
	c.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "secondary", Host: secondaryHost, Port: secondaryPort,
	})
	err := c.RegisterDistributedModel("m1", "test",
		[]LayerSlice{{
			StartLayer: 1, EndLayer: 32,
			WorkerID:         "primary",
			WorkerCandidates: []string{"primary", "secondary"},
		}})
	if err != nil {
		t.Fatalf("RegisterDistributedModel: %v", err)
	}

	c.SetSelector(NewLeastLoadedSelector())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.Infer(ctx, InferRequest{ModelName: "m1", Prompt: "hello"})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if resp.SliceStats[0].WorkerID != "primary" {
		t.Errorf("WorkerID: got %s, want primary (no failover)", resp.SliceStats[0].WorkerID)
	}
	if atomic.LoadInt32(&secondary.calls) != 0 {
		t.Errorf("secondary.calls=%d, want 0", atomic.LoadInt32(&secondary.calls))
	}
}

func TestE2E_Selector_AllCandidatesFail(t *testing.T) {
	primary := newFailOnNthWorker(t, 1000)
	secondary := newFailOnNthWorker(t, 1000)

	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "5s"}
	c := NewModelCoordinator(cfg)
	defer c.Close()

	primaryHost, primaryPort := hostPortOf(primary.server.URL)
	secondaryHost, secondaryPort := hostPortOf(secondary.server.URL)

	c.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "primary", Host: primaryHost, Port: primaryPort,
	})
	c.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "secondary", Host: secondaryHost, Port: secondaryPort,
	})
	err := c.RegisterDistributedModel("m-allfail", "test",
		[]LayerSlice{{
			StartLayer: 1, EndLayer: 32,
			WorkerID:         "primary",
			WorkerCandidates: []string{"primary", "secondary"},
		}})
	if err != nil {
		t.Fatalf("RegisterDistributedModel: %v", err)
	}

	c.SetSelector(NewLeastLoadedSelector())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = c.Infer(ctx, InferRequest{ModelName: "m-allfail", Prompt: "hello"})
	if err == nil {
		t.Fatal("Infer: want error when both candidates fail, got nil")
	}
	if !strings.Contains(err.Error(), "failed on all candidates") {
		t.Errorf("Infer err: got %q, want 'failed on all candidates'", err.Error())
	}
	if atomic.LoadInt32(&primary.calls) < 1 {
		t.Errorf("primary.calls=%d, want >= 1", atomic.LoadInt32(&primary.calls))
	}
	if atomic.LoadInt32(&secondary.calls) < 1 {
		t.Errorf("secondary.calls=%d, want >= 1", atomic.LoadInt32(&secondary.calls))
	}
}

func TestE2E_Selector_NoCandidatesConfigured(t *testing.T) {
	// WorkerID без WorkerCandidates — fallback на WorkerID (только primary).
	primary := newFailOnNthWorker(t, 0)

	cfg := types.RpcCoordinatorConfig{Enabled: true, Protocol: "http", Timeout: "5s"}
	c := NewModelCoordinator(cfg)
	defer c.Close()

	primaryHost, primaryPort := hostPortOf(primary.server.URL)
	c.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "primary", Host: primaryHost, Port: primaryPort,
	})
	err := c.RegisterDistributedModel("m-nofailover", "test",
		[]LayerSlice{{
			StartLayer: 1, EndLayer: 32,
			WorkerID: "primary",
			// WorkerCandidates пусто — fallback на [primary].
		}})
	if err != nil {
		t.Fatalf("RegisterDistributedModel: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.Infer(ctx, InferRequest{ModelName: "m-nofailover", Prompt: "hello"})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if resp.SliceStats[0].WorkerID != "primary" {
		t.Errorf("WorkerID: got %s, want primary", resp.SliceStats[0].WorkerID)
	}
}