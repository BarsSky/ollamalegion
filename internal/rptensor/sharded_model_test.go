package rptensor

import (
	"strings"
	"testing"
)

// findShard — найти шард по (rank, matrix) для тестов.
func findShard(shards []TensorShard, rank int, mx MatrixName) (TensorShard, bool) {
	for _, s := range shards {
		if s.Rank == rank && s.MatrixName == mx {
			return s, true
		}
	}
	return TensorShard{}, false
}

// =====================================================================
// Конструктор / базовая валидация
// =====================================================================

func TestNewShardedModel_Success_8B(t *testing.T) {
	// LLaMA-3 8B: hidden=4096, layers=32, heads=32, ffn=14336 (примерно).
	m, err := NewShardedModel("llama-3-8b", 4096, 14336, 32, 32, 128256)
	if err != nil {
		t.Fatalf("NewShardedModel: %v", err)
	}
	if m.HiddenSize != 4096 || m.NumLayers != 32 || m.NumHeads != 32 {
		t.Errorf("unexpected fields: hidden=%d layers=%d heads=%d",
			m.HiddenSize, m.NumLayers, m.NumHeads)
	}
	if m.NumKVHeads != m.NumHeads {
		t.Errorf("NumKVHeads should default to NumHeads (MHA), got %d", m.NumKVHeads)
	}
}

func TestNewShardedModel_AutoFFNSize(t *testing.T) {
	// intermediateSize=0 → автодефолт roundUp((8*hidden)/3, 256).
	m, err := NewShardedModel("test", 4096, 0, 32, 32, 128256)
	if err != nil {
		t.Fatalf("NewShardedModel: %v", err)
	}
	expected := roundUpTo((8*4096)/3, 256)
	if m.IntermediateSize != expected {
		t.Errorf("auto ffn: got %d, want %d", m.IntermediateSize, expected)
	}
}

// TestNewShardedModel_Validation покрывает все ветки валидации в NewShardedModel:
// пустое имя, нулевые hidden/layers/heads.
func TestNewShardedModel_Validation(t *testing.T) {
	tests := []struct {
		name       string
		mname      string
		hidden     int
		layers     int
		heads      int
		wantErrSub string
	}{
		{"empty name", "", 4096, 32, 32, "name is required"},
		{"zero hidden", "m", 0, 32, 32, "hiddenSize"},
		{"zero layers", "m", 4096, 0, 32, "numLayers"},
		{"zero heads", "m", 4096, 32, 0, "numHeads"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewShardedModel(tt.mname, tt.hidden, 14336, tt.layers, tt.heads, 128256)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSub)
			}
		})
	}
}

func TestSetNumKVHeads_GQA(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 32, 32, 128256)
	// GQA: 32 query heads, 8 KV heads (4 query heads на каждый KV head).
	if err := m.SetNumKVHeads(8); err != nil {
		t.Fatalf("SetNumKVHeads(8): %v", err)
	}
	if m.NumKVHeads != 8 {
		t.Errorf("NumKVHeads: got %d, want 8", m.NumKVHeads)
	}

	// Negative / invalid cases.
	if err := m.SetNumKVHeads(0); err == nil {
		t.Error("expected error on kvHeads=0")
	}
	if err := m.SetNumKVHeads(64); err == nil {
		t.Error("expected error on kvHeads > numHeads")
	}
	if err := m.SetNumKVHeads(7); err == nil { // 32 % 7 != 0
		t.Error("expected error on non-divisible kvHeads")
	}
}

// =====================================================================
// Partition strategies
// =====================================================================

func TestColumnPartition_4096x4096_WorldSize4(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256) // small test
	if err := m.Partition(ColumnPartition, 4); err != nil {
		t.Fatalf("Partition: %v", err)
	}

	// Каждый rank должен получить Q [4096, 1024].
	for rank := 0; rank < 4; rank++ {
		shards, err := m.ShardForRank(0, rank)
		if err != nil {
			t.Fatalf("ShardForRank(0, %d): %v", rank, err)
		}
		q, ok := findShard(shards, rank, MatrixQProj)
		if !ok {
			t.Fatalf("rank %d: missing QProj", rank)
		}
		if q.Shape[0] != 4096 || q.Shape[1] != 1024 {
			t.Errorf("rank %d Q: shape=%v, want [4096, 1024]", rank, q.Shape)
		}
		if q.Strategy != ColumnPartition {
			t.Errorf("rank %d Q strategy: got %q, want %q", rank, q.Strategy, ColumnPartition)
		}

		// MLP_up [hidden=4096, ffn/ws=14336/4=3584].
		up, _ := findShard(shards, rank, MatrixMLPUp)
		if up.Shape[0] != 4096 || up.Shape[1] != 3584 {
			t.Errorf("rank %d MLP_up shape=%v, want [4096, 3584]", rank, up.Shape)
		}
	}
}

func TestMegatronPartition_8B_4GPUs(t *testing.T) {
	// LLaMA-3 8B parameters.
	m, _ := NewShardedModel("llama-3-8b", 4096, 14336, 32, 32, 128256)
	if err := m.Partition(MegatronPartition, 4); err != nil {
		t.Fatalf("Partition: %v", err)
	}

	// QKV: column → [4096, 1024].
	// MLP_gate, MLP_up: row → [4096, 3584].
	// MLP_down: column → [3584, 4096].
	// LM_head: column → [32064, 4096] (128256/4).
	// Embed: replicated → [128256, 4096].

	for layer := 0; layer < 32; layer++ {
		all, err := m.AllShardsForLayer(layer)
		if err != nil {
			t.Fatalf("AllShardsForLayer(%d): %v", layer, err)
		}
		// Проверяем что все 4 rank'а × 9 матриц = 36 шардов.
		expected := 4 * len(AllMatrices)
		if len(all) != expected {
			t.Errorf("layer %d: got %d shards, want %d", layer, len(all), expected)
		}

		// rank 0, Q должен быть [4096, 1024] column.
		shards, _ := m.ShardForRank(layer, 0)
		q, _ := findShard(shards, 0, MatrixQProj)
		if !q.Equal(TensorShard{
			Rank: 0, Layer: layer, MatrixName: MatrixQProj,
			Shape: []int{4096, 1024}, Strategy: ColumnPartition,
		}) {
			t.Errorf("layer %d rank 0 Q mismatch: got rank=%d layer=%d mx=%s shape=%v strategy=%s",
				layer, q.Rank, q.Layer, q.MatrixName, q.Shape, q.Strategy)
		}

		// MLP_up должен быть row split [4096, 3584].
		up, _ := findShard(shards, 0, MatrixMLPUp)
		if up.Strategy != RowPartition {
			t.Errorf("layer %d MLP_up strategy: got %q, want row", layer, up.Strategy)
		}
		if up.Shape[0] != 4096 || up.Shape[1] != 3584 {
			t.Errorf("layer %d MLP_up shape=%v, want [4096, 3584]", layer, up.Shape)
		}

		// Embed replicated.
		emb, _ := findShard(shards, 0, MatrixEmbed)
		if emb.Strategy != ReplicatedPartition {
			t.Errorf("layer %d embed strategy: got %q, want replicated", layer, emb.Strategy)
		}
		if emb.Shape[0] != 128256 || emb.Shape[1] != 4096 {
			t.Errorf("layer %d embed shape=%v, want [128256, 4096]", layer, emb.Shape)
		}
	}
}

func TestMegatron_NotDivisible(t *testing.T) {
	// hidden=4097 (prime, не делится на 4).
	m, _ := NewShardedModel("m", 4097, 14336, 32, 32, 128256)
	err := m.Partition(MegatronPartition, 4)
	if err == nil {
		t.Fatal("expected error for non-divisible hidden size")
	}
	if !strings.Contains(err.Error(), "hiddenSize") {
		t.Errorf("error should mention hiddenSize, got: %v", err)
	}
}

func TestRowPartition_Divisible(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	if err := m.Partition(RowPartition, 4); err != nil {
		t.Fatalf("Partition: %v", err)
	}

	// Q: row split → [1024, 4096].
	shards, _ := m.ShardForRank(2, 2)
	q, _ := findShard(shards, 2, MatrixQProj)
	if q.Shape[0] != 1024 || q.Shape[1] != 4096 {
		t.Errorf("rank 2 Q shape=%v, want [1024, 4096]", q.Shape)
	}
	if q.Strategy != RowPartition {
		t.Errorf("rank 2 Q strategy: got %q", q.Strategy)
	}
}

func TestReplicatedPartition(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	if err := m.Partition(ReplicatedPartition, 4); err != nil {
		t.Fatalf("Partition: %v", err)
	}

	// Все rank'ы получают одинаковые полные матрицы.
	for rank := 0; rank < 4; rank++ {
		shards, _ := m.ShardForRank(0, rank)
		q, _ := findShard(shards, rank, MatrixQProj)
		if q.Shape[0] != 4096 || q.Shape[1] != 4096 {
			t.Errorf("rank %d Q shape=%v, want [4096, 4096] (replicated)", rank, q.Shape)
		}
		if q.Strategy != ReplicatedPartition {
			t.Errorf("rank %d Q strategy: got %q", rank, q.Strategy)
		}
	}
}

func TestPartition_InvalidWorldSize(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)

	if err := m.Partition(ColumnPartition, 0); err == nil {
		t.Error("expected error on worldSize=0")
	}
	if err := m.Partition(ColumnPartition, -1); err == nil {
		t.Error("expected error on worldSize=-1")
	}
	if err := m.Partition(ColumnPartition, 100); err == nil {
		t.Error("expected error on worldSize>64")
	}
}

func TestPartition_UnknownStrategy(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	if err := m.Partition(PartitionStrategy("bogus"), 4); err == nil {
		t.Error("expected error on unknown strategy")
	}
}

// =====================================================================
// ShardForRank / Validate
// =====================================================================

func TestShardForRank_OutOfRange(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	if err := m.Partition(ColumnPartition, 4); err != nil {
		t.Fatal(err)
	}

	if _, err := m.ShardForRank(-1, 0); err == nil {
		t.Error("expected error on negative layer")
	}
	if _, err := m.ShardForRank(100, 0); err == nil {
		t.Error("expected error on layer >= numLayers")
	}
	if _, err := m.ShardForRank(0, -1); err == nil {
		t.Error("expected error on negative rank")
	}
	if _, err := m.ShardForRank(0, 100); err == nil {
		t.Error("expected error on rank >= worldSize")
	}
}

func TestValidate_NotPartitioned(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	if err := m.Validate(); err == nil {
		t.Error("expected error on not-partitioned model")
	}
}

func TestValidate_OK(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	if err := m.Partition(MegatronPartition, 4); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestTotalShards(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 32, 32, 128256)
	if err := m.Partition(MegatronPartition, 4); err != nil {
		t.Fatal(err)
	}
	// 32 layers × 4 ranks × 9 matrices = 1152.
	if got := m.TotalShards(); got != 32*4*len(AllMatrices) {
		t.Errorf("TotalShards: got %d, want %d", got, 32*4*len(AllMatrices))
	}
}

func TestShardForRank_DeterministicOrder(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	if err := m.Partition(MegatronPartition, 4); err != nil {
		t.Fatal(err)
	}

	shards1, _ := m.ShardForRank(0, 0)
	shards2, _ := m.ShardForRank(0, 0)

	if len(shards1) != len(shards2) {
		t.Fatalf("length mismatch: %d vs %d", len(shards1), len(shards2))
	}
	for i := range shards1 {
		if shards1[i].MatrixName != shards2[i].MatrixName {
			t.Errorf("position %d: %q != %q (order should be stable)",
				i, shards1[i].MatrixName, shards2[i].MatrixName)
		}
	}

	// Проверяем что имена отсортированы.
	for i := 1; i < len(shards1); i++ {
		if shards1[i].MatrixName < shards1[i-1].MatrixName {
			t.Errorf("not sorted at position %d: %q < %q",
				i, shards1[i].MatrixName, shards1[i-1].MatrixName)
		}
	}
}

// =====================================================================
// RoundUp helper
// =====================================================================

func TestRoundUpTo(t *testing.T) {
	tests := []struct {
		value, multiple, want int
	}{
		{100, 10, 100},
		{101, 10, 110},
		{99, 10, 100},
		{1024, 256, 1024},
		{1025, 256, 1280},
		{100, 0, 100}, // multiple=0 → return as-is
		{100, -1, 100},
	}
	for _, tt := range tests {
		got := roundUpTo(tt.value, tt.multiple)
		if got != tt.want {
			t.Errorf("roundUpTo(%d, %d) = %d, want %d",
				tt.value, tt.multiple, got, tt.want)
		}
	}
}