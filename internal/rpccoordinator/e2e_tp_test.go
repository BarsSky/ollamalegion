package rpccoordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/rptensor"
)

// =====================================================================
// B8.5 — E2E TP pipeline: 4 mock workers × 4 layers через httptest.
// =====================================================================

// rpcWorkerTransport — adapter: WorkerClient реализует rptensor.RankTransport.
//
// WorkerID/Rank берутся из заданных полей (не из wc.WorkerID/Rank,
// потому что в тесте каждый mock обслуживает ровно один rank).
type rpcWorkerTransport struct {
	wc      *WorkerClient
	rank    int
	workerID string
}

func (t *rpcWorkerTransport) WorkerID() string { return t.workerID }
func (t *rpcWorkerTransport) Rank() int        { return t.rank }

func (t *rpcWorkerTransport) InferShard(ctx context.Context, req rptensor.TPShardInferRequest) (*rptensor.TPShardInferResponse, error) {
	// Подменяем rank в request на наш фиксированный rank (worker может
	// обслуживать только этот rank). В реальности координатор уже
	// знает rank, но наш worker-side mock тоже проверяет это.
	req.Rank = t.rank
	return t.wc.TPInferSlice(ctx, req)
}

func (t *rpcWorkerTransport) SyncKVShard(ctx context.Context, sessionID string, shard []byte) error {
	return t.wc.TPKvSync(ctx, sessionID, t.rank, shard)
}

func (t *rpcWorkerTransport) FetchKVShard(ctx context.Context, sessionID string) ([]byte, error) {
	return t.wc.TPKvFetch(ctx, sessionID, t.rank)
}

// =====================================================================
// 4 mock-worker setup
// =====================================================================

type mockWorker struct {
	mu        sync.Mutex
	latency   time.Duration
	callCount int

	// capture последнего вызова для assertions
	lastRank  int
	lastLayer int
	lastInput string

	// KV-shard map (rank → shard) для теста KV-sync
	kvShards map[string]map[int][]byte // sessionID → rank → shard
}

func newMockWorker(latency time.Duration) *mockWorker {
	return &mockWorker{
		latency:  latency,
		kvShards: make(map[string]map[int][]byte),
	}
}

func (m *mockWorker) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/rpc/tp/infer", func(w http.ResponseWriter, r *http.Request) {
		var req rptensor.TPShardInferRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.callCount++
		m.lastRank = req.Rank
		m.lastLayer = req.Layer
		m.lastInput = string(req.Input)
		m.mu.Unlock()

		// Имитация latency.
		if m.latency > 0 {
			time.Sleep(m.latency)
		}

		// Stub output: rank+1 (детерминированно).
		out := []byte{byte(req.Rank + 1)}
		resp := rptensor.TPShardInferResponse{
			Rank:        req.Rank,
			Output:      out,
			KVShard:     []byte{byte(req.Rank)},
			NeedsReduce: true,
			LatencyMs:   m.latency.Milliseconds(),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/rpc/tp/kv_sync", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"session_id"`
			Rank      int    `json:"rank"`
			Shard     []byte `json:"shard"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		if _, ok := m.kvShards[req.SessionID]; !ok {
			m.kvShards[req.SessionID] = make(map[int][]byte)
		}
		m.kvShards[req.SessionID][req.Rank] = req.Shard
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "saved"})
	})

	mux.HandleFunc("/rpc/tp/kv_fetch", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("session_id")
		rankStr := r.URL.Query().Get("rank")
		rank := parseRank(rankStr)
		m.mu.Lock()
		defer m.mu.Unlock()
		if session, ok := m.kvShards[sessionID]; ok {
			if shard, ok := session[rank]; ok {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"session_id": sessionID,
					"rank":       rank,
					"shard":      shard,
				})
				return
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
	})

	return mux
}

// parseRank — парсит rank из query string.
func parseRank(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// tpPipeline — обёртка для 4 mock-workers + 4 WorkerClient + 4 rpcWorkerTransport.
type tpPipeline struct {
	workers []*mockWorker
	clients []*WorkerClient
	transports []rptensor.RankTransport
	servers []*httptest.Server
}

func newTPPipeline(t *testing.T, latency time.Duration) *tpPipeline {
	p := &tpPipeline{
		workers: make([]*mockWorker, 4),
		clients: make([]*WorkerClient, 4),
		transports: make([]rptensor.RankTransport, 4),
		servers: make([]*httptest.Server, 4),
	}
	for i := 0; i < 4; i++ {
		p.workers[i] = newMockWorker(latency)
		p.servers[i] = httptest.NewServer(p.workers[i].handler(t))
		host, port := splitHostPort(p.servers[i].URL)
		p.clients[i] = NewWorkerClient(fmt.Sprintf("w-rank%d", i), host, port, "http", &http.Client{})
		p.transports[i] = &rpcWorkerTransport{
			wc:       p.clients[i],
			rank:     i,
			workerID: fmt.Sprintf("w-rank%d", i),
		}
	}
	return p
}

func (p *tpPipeline) Close() {
	for _, s := range p.servers {
		s.Close()
	}
}

// =====================================================================
// E2E tests
// =====================================================================

func TestE2E_TensorParallel_4Workers_4Layers(t *testing.T) {
	pipeline := newTPPipeline(t, 5*time.Millisecond)
	defer pipeline.Close()

	m, err := rptensor.NewShardedModel("llama-3-8b", 4096, 14336, 4, 32, 128256)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Partition(rptensor.MegatronPartition, 4); err != nil {
		t.Fatal(err)
	}

	coord, err := rptensor.NewTensorParallelCoordinator(m, pipeline.transports, rptensor.CoordinatorConfig{})
	if err != nil {
		t.Fatal(err)
	}

	req := rptensor.TPInferRequest{
		ModelName: "llama-3-8b",
		Prompt:    "hello world tensor parallelism",
		Input:     []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"), // 52 bytes
	}
	start := time.Now()
	resp, err := coord.Infer(context.Background(), req)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if resp.Degraded {
		t.Errorf("Degraded=true: rank_errors=%+v", resp.RankErrors)
	}
	if len(resp.RankErrors) != 0 {
		t.Errorf("RankErrors should be empty: %+v", resp.RankErrors)
	}
	if resp.WorldSize != 4 || resp.NumLayers != 4 {
		t.Errorf("ws=%d layers=%d, want 4/4", resp.WorldSize, resp.NumLayers)
	}
	if len(resp.LayerStats) != 4 {
		t.Fatalf("layer stats: got %d, want 4", len(resp.LayerStats))
	}

	// Каждый worker получил ровно 4 вызова (4 слоя × 1 rank).
	for i, w := range pipeline.workers {
		w.mu.Lock()
		got := w.callCount
		w.mu.Unlock()
		if got != 4 {
			t.Errorf("worker %d: callCount=%d, want 4", i, got)
		}
	}

	// Parallel latency: каждый слой параллельный, latency ~4*5ms=20ms
	// вместо 16*5ms=80ms sequential.
	if elapsed > 200*time.Millisecond {
		t.Errorf("parallel elapsed=%v should be < 200ms (sequential would be ~80ms)", elapsed)
	}
	t.Logf("E2E TP: %d layers × %d ranks → elapsed=%v", 4, 4, elapsed)
}

func TestE2E_TensorParallel_FailoverOnRankFailure(t *testing.T) {
	pipeline := newTPPipeline(t, 5*time.Millisecond)
	defer pipeline.Close()

	// Rank 1 (transport[1]) всегда падает.
	pipeline.workers[1].latency = 0 // быстро для ускорения теста
	pipeline.transports[1] = &failingTransport{rank: 1, err: fmt.Errorf("rank 1 simulated failure")}

	m, _ := rptensor.NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	_ = m.Partition(rptensor.MegatronPartition, 4)

	coord, _ := rptensor.NewTensorParallelCoordinator(m, pipeline.transports, rptensor.CoordinatorConfig{})

	resp, err := coord.Infer(context.Background(), rptensor.TPInferRequest{
		ModelName: "m",
		Input:     []byte("test"),
	})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if !resp.Degraded {
		t.Error("expected Degraded=true when rank 1 fails")
	}
	// На каждом слое — rank 1 падает.
	if len(resp.RankErrors) == 0 {
		t.Error("expected rank_errors to be populated")
	}
}

func TestE2E_TensorParallel_KVShardSync(t *testing.T) {
	pipeline := newTPPipeline(t, 1*time.Millisecond)
	defer pipeline.Close()

	m, _ := rptensor.NewShardedModel("m", 4096, 14336, 2, 32, 128256)
	_ = m.Partition(rptensor.MegatronPartition, 2)

	coord, _ := rptensor.NewTensorParallelCoordinator(m, pipeline.transports[:2], rptensor.CoordinatorConfig{})

	resp, err := coord.Infer(context.Background(), rptensor.TPInferRequest{
		ModelName: "m",
		SessionID: "sess-kv",
		Input:     []byte("hello"),
	})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if resp.Degraded {
		t.Errorf("Degraded=true unexpectedly: %+v", resp.RankErrors)
	}

	// После Infer'а оба worker'а должны иметь shard для sess-kv.
	for i := 0; i < 2; i++ {
		w := pipeline.workers[i]
		w.mu.Lock()
		shards, ok := w.kvShards["sess-kv"]
		count := len(shards)
		w.mu.Unlock()
		if !ok || count == 0 {
			t.Errorf("worker %d: no KV shards for sess-kv (count=%d)", i, count)
		}
	}
}

func TestE2E_TensorParallel_ConcurrentRequests(t *testing.T) {
	pipeline := newTPPipeline(t, 1*time.Millisecond)
	defer pipeline.Close()

	m, _ := rptensor.NewShardedModel("m", 4096, 14336, 2, 32, 128256)
	_ = m.Partition(rptensor.MegatronPartition, 2)

	coord, _ := rptensor.NewTensorParallelCoordinator(m, pipeline.transports[:2], rptensor.CoordinatorConfig{})

	// 10 параллельных запросов.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = coord.Infer(context.Background(), rptensor.TPInferRequest{
				ModelName: "m",
				Input:     []byte("hi"),
			})
		}()
	}
	wg.Wait()

	// Проверяем что все запросы прошли.
	stats := coord.Stats()
	if stats.TotalInfer != 10 {
		t.Errorf("TotalInfer=%d, want 10", stats.TotalInfer)
	}
}

// failingTransport — RankTransport который всегда возвращает error.
type failingTransport struct {
	rank int
	err  error
}

func (f *failingTransport) WorkerID() string { return fmt.Sprintf("failing-%d", f.rank) }
func (f *failingTransport) Rank() int       { return f.rank }
func (f *failingTransport) InferShard(ctx context.Context, req rptensor.TPShardInferRequest) (*rptensor.TPShardInferResponse, error) {
	return nil, f.err
}
func (f *failingTransport) SyncKVShard(ctx context.Context, sessionID string, shard []byte) error {
	return f.err
}
func (f *failingTransport) FetchKVShard(ctx context.Context, sessionID string) ([]byte, error) {
	return nil, f.err
}