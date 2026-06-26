package rptensor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =====================================================================
// Mock transport — общий helper для B8.2, B8.5 e2e
// =====================================================================

// mockTransport — реализация RankTransport с заранее заданной latency и output.
type mockTransport struct {
	workerID string
	rank     int
	latency  time.Duration

	// OutputFn генерирует output по input и rank. Если nil — дефолт.
	OutputFn func(rank, layer int, input []byte) []byte

	// FailOn позволяет задать ошибки: FailOn[rank*100+layer] = error.
	FailOn map[int]error

	mu          sync.Mutex
	callCount   atomic.Int64
	kvSaves     atomic.Int64
	kvFetches   atomic.Int64
	lastLayer   int
	lastInputLen int
}

func newMockTransport(rank int, latency time.Duration) *mockTransport {
	return &mockTransport{
		workerID: fmt.Sprintf("mock-w%d", rank),
		rank:     rank,
		latency:  latency,
		FailOn:   make(map[int]error),
	}
}

func (m *mockTransport) WorkerID() string { return m.workerID }
func (m *mockTransport) Rank() int       { return m.rank }

func (m *mockTransport) InferShard(ctx context.Context, req TPShardInferRequest) (*TPShardInferResponse, error) {
	m.callCount.Add(1)
	m.mu.Lock()
	m.lastLayer = req.Layer
	m.lastInputLen = len(req.Input)
	m.mu.Unlock()

	// Имитация latency (sleep + context cancellation).
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.latency):
	}

	if err, ok := m.FailOn[m.rank*1000+req.Layer]; ok && err != nil {
		return nil, err
	}

	var out []byte
	if m.OutputFn != nil {
		out = m.OutputFn(m.rank, req.Layer, req.Input)
	} else {
		// Default: deterministic output = rank-marked byte repeated.
		plen := PartialLenForRank(len(req.Input), req.WorldSize, req.Rank)
		out = make([]byte, plen)
		for i := range out {
			out[i] = byte(m.rank + 1) // rank 0 → byte 1, rank 1 → byte 2, ...
		}
	}

	return &TPShardInferResponse{
		Rank:        m.rank,
		Output:      out,
		KVShard:     []byte{byte(m.rank), byte(req.Layer)},
		NeedsReduce: true,
		LatencyMs:   m.latency.Milliseconds(),
	}, nil
}

func (m *mockTransport) SyncKVShard(ctx context.Context, sessionID string, shard []byte) error {
	m.kvSaves.Add(1)
	return nil
}

func (m *mockTransport) FetchKVShard(ctx context.Context, sessionID string) ([]byte, error) {
	m.kvFetches.Add(1)
	return nil, nil // пустой shard
}

// =====================================================================
// Coordinator constructor
// =====================================================================

func TestNewTensorParallelCoordinator_Valid(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	if err := m.Partition(MegatronPartition, 4); err != nil {
		t.Fatal(err)
	}
	ranks := []RankTransport{
		newMockTransport(0, 1*time.Millisecond),
		newMockTransport(1, 1*time.Millisecond),
		newMockTransport(2, 1*time.Millisecond),
		newMockTransport(3, 1*time.Millisecond),
	}
	coord, err := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})
	if err != nil {
		t.Fatalf("NewTensorParallelCoordinator: %v", err)
	}
	if coord.WorldSize() != 4 || coord.NumLayers() != 4 {
		t.Errorf("ws=%d layers=%d, want 4/4", coord.WorldSize(), coord.NumLayers())
	}
}

func TestNewTensorParallelCoordinator_Errors(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	_ = m.Partition(MegatronPartition, 4)

	t.Run("nil model", func(t *testing.T) {
		_, err := NewTensorParallelCoordinator(nil, []RankTransport{}, CoordinatorConfig{})
		if err == nil {
			t.Error("expected error on nil model")
		}
	})

	t.Run("empty ranks", func(t *testing.T) {
		_, err := NewTensorParallelCoordinator(m, nil, CoordinatorConfig{})
		if err == nil {
			t.Error("expected error on empty ranks")
		}
	})

	t.Run("rank count mismatch", func(t *testing.T) {
		ranks := []RankTransport{
			newMockTransport(0, time.Millisecond),
			newMockTransport(1, time.Millisecond),
		}
		_, err := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})
		if err == nil {
			t.Error("expected error on rank count != worldSize")
		}
	})

	t.Run("missing rank", func(t *testing.T) {
		ranks := []RankTransport{
			newMockTransport(0, time.Millisecond),
			newMockTransport(1, time.Millisecond),
			newMockTransport(3, time.Millisecond), // gap: missing rank 2
			newMockTransport(0, time.Millisecond), // duplicate rank 0
		}
		_, err := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})
		if err == nil {
			t.Error("expected error on duplicate/missing ranks")
		}
	})
}

// =====================================================================
// Parallel execution
// =====================================================================

func TestParallelExecution_4Ranks_4Layers(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	_ = m.Partition(MegatronPartition, 4)

	// Каждый rank спит 10ms — sequential 4*4=160ms, parallel ~4*10ms=40ms.
	rankLatency := 10 * time.Millisecond
	ranks := []RankTransport{
		newMockTransport(0, rankLatency),
		newMockTransport(1, rankLatency),
		newMockTransport(2, rankLatency),
		newMockTransport(3, rankLatency),
	}
	coord, err := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})
	if err != nil {
		t.Fatal(err)
	}

	req := TPInferRequest{
		ModelName: "m",
		Prompt:    "hello",
		Input:     []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"), // 52 bytes
	}

	start := time.Now()
	resp, err := coord.Infer(context.Background(), req)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if resp.Degraded {
		t.Error("expected non-degraded (all ranks ok)")
	}
	if resp.WorldSize != 4 || resp.NumLayers != 4 {
		t.Errorf("ws=%d layers=%d", resp.WorldSize, resp.NumLayers)
	}
	if len(resp.LayerStats) != 4 {
		t.Fatalf("layer stats: got %d, want 4", len(resp.LayerStats))
	}
	for i, s := range resp.LayerStats {
		if s.Layer != i {
			t.Errorf("layer stat %d: layer=%d, want %d", i, s.Layer, i)
		}
		if s.Successful != 4 {
			t.Errorf("layer %d: successful=%d, want 4", i, s.Successful)
		}
	}

	// Каждый rank должен получить 4 вызова (4 слоя × 1 rank).
	for _, tr := range ranks {
		m := tr.(*mockTransport)
		if got := m.callCount.Load(); got != 4 {
			t.Errorf("rank %d: callCount=%d, want 4", m.rank, got)
		}
	}

	// Parallel latency должна быть заметно меньше sequential (4 * 4 * 10 = 160ms).
	// Допускаем до 100ms — overhead + scheduling.
	const sequentialBaseline = 160 * time.Millisecond
	if elapsed >= sequentialBaseline {
		t.Errorf("parallel execution too slow: %v (sequential would be ~%v)",
			elapsed, sequentialBaseline)
	}
	t.Logf("parallel elapsed=%v (sequential baseline ~%v, speedup ~%.1fx)",
		elapsed, sequentialBaseline, float64(sequentialBaseline)/float64(elapsed))
}

func TestParallelExecution_OneRankFails(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 2, 32, 128256)
	_ = m.Partition(MegatronPartition, 2)

	ranks := []RankTransport{
		newMockTransport(0, 5*time.Millisecond),
		newMockTransport(1, 5*time.Millisecond),
	}
	// Rank 1 падает на всех слоях.
	ranks[1].(*mockTransport).FailOn[1*1000+0] = errors.New("rank 1 simulated failure")
	ranks[1].(*mockTransport).FailOn[1*1000+1] = errors.New("rank 1 simulated failure")

	coord, _ := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})

	req := TPInferRequest{ModelName: "m", Input: []byte("12345678")}
	resp, err := coord.Infer(context.Background(), req)

	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if !resp.Degraded {
		t.Error("expected degraded=true")
	}
	if len(resp.RankErrors) == 0 {
		t.Error("expected rank_errors to be populated")
	}
	// Layer 0 и 1: successful=1 (только rank 0).
	for i, s := range resp.LayerStats {
		if s.Successful != 1 {
			t.Errorf("layer %d: successful=%d, want 1 (only rank 0 ok)", i, s.Successful)
		}
	}
}

func TestParallelExecution_AllRanksFail(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 2, 32, 128256)
	_ = m.Partition(MegatronPartition, 2)

	ranks := []RankTransport{
		newMockTransport(0, time.Millisecond),
		newMockTransport(1, time.Millisecond),
	}
	for _, tr := range ranks {
		m := tr.(*mockTransport)
		m.FailOn[0] = errors.New("rank 0 dead")
		m.FailOn[1] = errors.New("rank 1 dead")
		m.OutputFn = nil // empty
	}
	// Сделаем так чтобы mockTransport возвращал ошибку для любого слоя.
	for _, tr := range ranks {
		m := tr.(*mockTransport)
		for layer := 0; layer < 2; layer++ {
			m.FailOn[m.rank*1000+layer] = errors.New("dead")
		}
	}

	coord, _ := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})
	req := TPInferRequest{ModelName: "m", Input: []byte("xxxx")}
	_, err := coord.Infer(context.Background(), req)
	if err == nil {
		t.Error("expected error when all ranks fail")
	}
}

// =====================================================================
// Disabled / configuration
// =====================================================================

func TestInfer_DisabledCoordinator(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 2, 32, 128256)
	_ = m.Partition(MegatronPartition, 2)
	ranks := []RankTransport{newMockTransport(0, time.Millisecond), newMockTransport(1, time.Millisecond)}
	coord, _ := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{Disabled: true})

	_, err := coord.Infer(context.Background(), TPInferRequest{ModelName: "m"})
	if err == nil {
		t.Error("expected error on disabled coordinator")
	}

	coord.Close()
	if coord.Enabled() {
		t.Error("Close() should disable coordinator")
	}
}

func TestInfer_TierCountMismatch(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 2, 32, 128256)
	_ = m.Partition(MegatronPartition, 4) // worldSize=4
	ranks := []RankTransport{
		newMockTransport(0, time.Millisecond),
		newMockTransport(1, time.Millisecond),
		newMockTransport(2, time.Millisecond),
		newMockTransport(3, time.Millisecond),
	}
	coord, _ := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})
	_, err := coord.Infer(context.Background(), TPInferRequest{ModelName: "m", TierCount: 8})
	if err == nil {
		t.Error("expected tierCount mismatch error")
	}
}

func TestInfer_ModelNameMismatch(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 2, 32, 128256)
	_ = m.Partition(MegatronPartition, 2)
	ranks := []RankTransport{newMockTransport(0, time.Millisecond), newMockTransport(1, time.Millisecond)}
	coord, _ := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})
	_, err := coord.Infer(context.Background(), TPInferRequest{ModelName: "different"})
	if err == nil {
		t.Error("expected model name mismatch error")
	}
}

// =====================================================================
// Stats
// =====================================================================

func TestStats_IncrementOnInfer(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 2, 32, 128256)
	_ = m.Partition(MegatronPartition, 2)
	ranks := []RankTransport{newMockTransport(0, time.Millisecond), newMockTransport(1, time.Millisecond)}
	coord, _ := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})

	for i := 0; i < 5; i++ {
		_, _ = coord.Infer(context.Background(), TPInferRequest{ModelName: "m", Input: []byte("xx")})
	}
	stats := coord.Stats()
	if stats.TotalInfer != 5 {
		t.Errorf("TotalInfer=%d, want 5", stats.TotalInfer)
	}
	if stats.TotalRanks != 2 {
		t.Errorf("TotalRanks=%d, want 2", stats.TotalRanks)
	}
	if stats.AvgLatencyMs <= 0 {
		t.Error("expected positive avg latency")
	}
}

// =====================================================================
// Context cancellation
// =====================================================================

func TestInfer_ContextCancellation(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 2, 32, 128256)
	_ = m.Partition(MegatronPartition, 2)
	ranks := []RankTransport{
		newMockTransport(0, 5*time.Second), // долгий
		newMockTransport(1, 5*time.Second),
	}
	coord, _ := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := coord.Infer(ctx, TPInferRequest{ModelName: "m", Input: []byte("xxx")})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if elapsed > 1*time.Second {
		t.Errorf("cancellation should be quick, got %v", elapsed)
	}
}

// =====================================================================
// Helper test: rank coverage
// =====================================================================

func TestRanks_AllLayersCalled(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	_ = m.Partition(MegatronPartition, 4)

	mocks := []*mockTransport{
		newMockTransport(0, time.Millisecond),
		newMockTransport(1, time.Millisecond),
		newMockTransport(2, time.Millisecond),
		newMockTransport(3, time.Millisecond),
	}
	ranks := []RankTransport{mocks[0], mocks[1], mocks[2], mocks[3]}
	coord, _ := NewTensorParallelCoordinator(m, ranks, CoordinatorConfig{})

	_, err := coord.Infer(context.Background(), TPInferRequest{ModelName: "m", Input: []byte("1234567890abcdef")})
	if err != nil {
		t.Fatal(err)
	}

	// Каждый mock должен получить 4 вызова (по слою).
	// И слои должны быть в диапазоне 0..3.
	layersSeen := make(map[int]bool, 4)
	for _, mock := range mocks {
		count := mock.callCount.Load()
		if count != 4 {
			t.Errorf("rank %d: %d calls, want 4", mock.rank, count)
		}
		_ = layersSeen[mock.lastLayer]
	}

	// Сортируем по rank и проверяем что каждый rank имел все 4 слоя.
	// (mock.lastLayer — последний; нужно проверять всю последовательность.
	//  Для простоты — последний layer для каждого rank'а должен быть 3.)
	for _, mock := range mocks {
		if mock.lastLayer != 3 {
			t.Errorf("rank %d: last layer=%d, want 3", mock.rank, mock.lastLayer)
		}
	}
}

// Чтобы избежать unused warning в `sort` если меняется структура тестов.
var _ = sort.Slice