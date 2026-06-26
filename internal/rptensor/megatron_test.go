package rptensor

import (
	"testing"
)

// TestApplyMegatronPartition_NilModel — nil safety.
func TestApplyMegatronPartition_NilModel(t *testing.T) {
	err := ApplyMegatronPartition(nil)
	if err == nil {
		t.Error("expected error for nil model")
	}
}

// TestMegatronLayout_8B_4GPUs — проверяет раскладку для 8B на 4 GPU.
func TestMegatronLayout_8B_4GPUs(t *testing.T) {
	// LLaMA-3 8B parameters.
	m, err := NewShardedModel("llama-3-8b", 4096, 14336, 32, 32, 128256)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Partition(MegatronPartition, 4); err != nil {
		t.Fatal(err)
	}
	// Также проверяем через ApplyMegatronPartition.
	m2, _ := NewShardedModel("llama-3-8b", 4096, 14336, 32, 32, 128256)
	m2.WorldSize = 4
	if err := ApplyMegatronPartition(m2); err != nil {
		t.Fatalf("ApplyMegatronPartition: %v", err)
	}

	// Проверяем shapes для каждой матрицы rank 0 layer 0.
	shards, _ := m.ShardForRank(0, 0)
	for _, s := range shards {
		switch s.MatrixName {
		case MatrixQProj, MatrixKProj, MatrixVProj, MatrixOProj:
			// [4096, 1024] — column split.
			if s.Shape[0] != 4096 || s.Shape[1] != 1024 {
				t.Errorf("Q/K/V/O %s: shape=%v, want [4096, 1024]", s.MatrixName, s.Shape)
			}
			if s.Strategy != ColumnPartition {
				t.Errorf("%s: strategy=%s, want column", s.MatrixName, s.Strategy)
			}
		case MatrixMLPUp, MatrixMLPGate:
			// [4096, 3584] — row split.
			if s.Shape[0] != 4096 || s.Shape[1] != 3584 {
				t.Errorf("MLP_up/gate %s: shape=%v, want [4096, 3584]", s.MatrixName, s.Shape)
			}
			if s.Strategy != RowPartition {
				t.Errorf("%s: strategy=%s, want row", s.MatrixName, s.Strategy)
			}
		case MatrixMLPDown:
			// [3584, 4096] — column split.
			if s.Shape[0] != 3584 || s.Shape[1] != 4096 {
				t.Errorf("MLP_down: shape=%v, want [3584, 4096]", s.Shape)
			}
			if s.Strategy != ColumnPartition {
				t.Errorf("MLP_down: strategy=%s, want column", s.Strategy)
			}
		case MatrixLMHead:
			// [32064, 4096] — column split.
			if s.Shape[0] != 32064 || s.Shape[1] != 4096 {
				t.Errorf("LM_head: shape=%v, want [32064, 4096]", s.Shape)
			}
			if s.Strategy != ColumnPartition {
				t.Errorf("LM_head: strategy=%s, want column", s.Strategy)
			}
		case MatrixEmbed:
			// [128256, 4096] — replicated (полная).
			if s.Shape[0] != 128256 || s.Shape[1] != 4096 {
				t.Errorf("Embed: shape=%v, want [128256, 4096]", s.Shape)
			}
			if s.Strategy != ReplicatedPartition {
				t.Errorf("Embed: strategy=%s, want replicated", s.Strategy)
			}
		}
	}
}

// TestMegatronPartition_ReconstructFullModel — concat rank'ов даёт полные матрицы.
//
// Проверяет что sum-of-shapes Q по rank'ам = Q-full, и т.д.
func TestMegatronPartition_ReconstructFullModel(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	_ = m.Partition(MegatronPartition, 4)

	// Sum Q-shape cols across ranks.
	qTotalCols := 0
	for rank := 0; rank < 4; rank++ {
		shards, _ := m.ShardForRank(0, rank)
		for _, s := range shards {
			if s.MatrixName == MatrixQProj {
				qTotalCols += s.Shape[1]
			}
		}
	}
	if qTotalCols != 4096 {
		t.Errorf("sum Q cols: got %d, want 4096", qTotalCols)
	}

	// Sum MLP_up cols across ranks (row-split по axis=0, hidden=4096 сохраняется;
	// ffn=14336 делится на 4 rank'а, каждый даёт 3584).
	upTotalCols := 0
	for rank := 0; rank < 4; rank++ {
		shards, _ := m.ShardForRank(0, rank)
		for _, s := range shards {
			if s.MatrixName == MatrixMLPUp {
				upTotalCols += s.Shape[1]
			}
		}
	}
	if upTotalCols != 14336 {
		t.Errorf("sum MLP_up cols (== ffn): got %d, want 14336", upTotalCols)
	}
}

// TestMegatron_NotDivisible — параметры не делятся → error.
func TestMegatron_NotDivisible_Vocab(t *testing.T) {
	// hidden=4096, но vocab=128257 (prime) — не делится на 4.
	m, _ := NewShardedModel("m", 4096, 14336, 32, 32, 128257)
	err := m.Partition(MegatronPartition, 4)
	if err == nil {
		t.Fatal("expected error for non-divisible vocab")
	}
}

// TestMegatron_WorldSizeMismatch — некорректный worldSize.
func TestMegatron_WorldSizeMismatch(t *testing.T) {
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	// numLayers=4, hidden=4096 → column split даст каждому rank'у
	// Q [4096, 1024]. Работает.
	if err := m.Partition(MegatronPartition, 4); err != nil {
		t.Fatal(err)
	}
	// Перевызываем с другим worldSize — должно пересоздать shards.
	if err := m.Partition(MegatronPartition, 2); err != nil {
		t.Fatalf("re-partition to 2: %v", err)
	}
	// Теперь Q [4096, 2048].
	shards, _ := m.ShardForRank(0, 0)
	for _, s := range shards {
		if s.MatrixName == MatrixQProj {
			if s.Shape[1] != 2048 {
				t.Errorf("Q re-partitioned to ws=2: shape=%v, want [4096, 2048]", s.Shape)
			}
		}
	}
}