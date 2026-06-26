// Package rptensor — Tensor Parallelism для rpcworker.
//
// B8 (Session 12) — каркас для tensor parallelism: domain types
// (PartitionStrategy, TensorShard, ShardedModel) без реальной
// матричной математики. Реальная llama.cpp-интеграция через ggml
// bridge отложена в post-1.0 (см. plans/b8-tensor-parallelism-plan.md).
//
// ShardedModel описывает модель, разбитую по rank'ам (TP world size).
// Для каждого слоя хранится список TensorShard'ов — по одному на каждый
// rank × матрицу. PartitionStrategy определяет, как именно матрица
// делится: column (split по cols), row (split по rows) или megatron
// (column для QKV/O + row для MLP_up + column для MLP_down).
package rptensor

import (
	"fmt"
	"sort"
	"sync"
)

// MatrixName — стандартные имена матриц трансформера, на которых
// применяется tensor parallelism. Имена согласованы с llama.cpp и
// HuggingFace (model.safetensors).
type MatrixName string

const (
	MatrixQProj    MatrixName = "q_proj"
	MatrixKProj    MatrixName = "k_proj"
	MatrixVProj    MatrixName = "v_proj"
	MatrixOProj    MatrixName = "o_proj"
	MatrixMLPUp    MatrixName = "mlp_up"
	MatrixMLPGate  MatrixName = "mlp_gate"
	MatrixMLPDown  MatrixName = "mlp_down"
	MatrixLMHead   MatrixName = "lm_head"
	MatrixEmbed    MatrixName = "embed_tokens"
)

// AllMatrices — список всех матриц, для которых строится шардирование.
// Используется в Partition() и MegatronPartition() для полного покрытия
// трансформера. Включает MatrixEmbed (replicated) для полноты покрытия.
var AllMatrices = []MatrixName{
	MatrixQProj,
	MatrixKProj,
	MatrixVProj,
	MatrixOProj,
	MatrixMLPGate,
	MatrixMLPUp,
	MatrixMLPDown,
	MatrixLMHead,
	MatrixEmbed,
}

// PartitionStrategy — стратегия шардирования матрицы.
type PartitionStrategy string

const (
	// ColumnPartition: матрица делится по столбцам (axis=1).
	// Для матрицы [rows, cols] → [rows, cols/worldSize] на каждый rank.
	ColumnPartition PartitionStrategy = "column"

	// RowPartition: матрица делится по строкам (axis=0).
	// Для матрицы [rows, cols] → [rows/worldSize, cols] на каждый rank.
	RowPartition PartitionStrategy = "row"

	// MegatronPartition: column для Q/K/V/O/lm_head, row для MLP_up/gate,
	// column для MLP_down. Требует делимости hidden_size и intermediate_size
	// на world_size.
	MegatronPartition PartitionStrategy = "megatron"

	// ReplicatedPartition: матрица целиком на каждом rank'е (no split).
	// Используется для embedding (embed_tokens) и layer norm'ов, которые
	// не шардируются.
	ReplicatedPartition PartitionStrategy = "replicated"
)

// TensorShard — описание одного шарда матрицы для конкретного rank'а.
//
// Shape — размеры [rows, cols] уже после разбиения. Например, для
// Q-матрицы [4096, 4096] (hidden=4096) с world_size=4 и ColumnPartition
// каждый rank получает shape=[4096, 1024].
type TensorShard struct {
	Rank       int               // 0..WorldSize-1
	Layer      int               // 0..NumLayers-1
	MatrixName MatrixName        // см. MatrixName constants
	Shape      []int             // [rows, cols] после шардирования
	Strategy   PartitionStrategy // column/row/megatron/replicated
}

// Equal — глубокое сравнение двух шардов (для тестов).
func (s TensorShard) Equal(other TensorShard) bool {
	if s.Rank != other.Rank || s.Layer != other.Layer ||
		s.MatrixName != other.MatrixName || s.Strategy != other.Strategy {
		return false
	}
	if len(s.Shape) != len(other.Shape) {
		return false
	}
	for i := range s.Shape {
		if s.Shape[i] != other.Shape[i] {
			return false
		}
	}
	return true
}

// ShardedModel — модель, разбитая на rank'и.
//
// WorldSize = число параллельных workers (TP degree). LayerShards[layer]
// содержит полный набор шардов для слоя (по одному на каждый rank для
// каждой матрицы).
type ShardedModel struct {
	Name        string
	HiddenSize    int // e.g., 4096 для 8B
	IntermediateSize int // FFN hidden, e.g., 11008 для 8B / 2752 для 8B-GQA
	NumLayers     int // e.g., 32 для 8B
	NumHeads      int // e.g., 32 для 8B
	NumKVHeads    int // GQA: e.g., 8 (если 0 — равно NumHeads)
	VocabSize     int // e.g., 128256 для LLaMA-3

	WorldSize  int                    // TP degree (1..32 обычно)
	Strategy   PartitionStrategy      // общая стратегия
	LayerShards map[int][]TensorShard  // layer → shards for all ranks

	mu sync.RWMutex // защищает LayerShards при concurrent reads
}

// NewShardedModel — конструктор с валидацией параметров модели.
func NewShardedModel(name string, hiddenSize, intermediateSize, numLayers, numHeads, vocabSize int) (*ShardedModel, error) {
	if name == "" {
		return nil, fmt.Errorf("rptensor: model name is required")
	}
	if hiddenSize <= 0 {
		return nil, fmt.Errorf("rptensor: hiddenSize must be > 0, got %d", hiddenSize)
	}
	if numLayers <= 0 {
		return nil, fmt.Errorf("rptensor: numLayers must be > 0, got %d", numLayers)
	}
	if numHeads <= 0 {
		return nil, fmt.Errorf("rptensor: numHeads must be > 0, got %d", numHeads)
	}
	if intermediateSize <= 0 {
		// Fallback: LLaMA-стандарт = (8/3) * hidden, round up to multiple of 256.
		intermediateSize = roundUpTo((8*hiddenSize)/3, 256)
	}
	if vocabSize <= 0 {
		vocabSize = 128256 // LLaMA-3 default
	}
	return &ShardedModel{
		Name:            name,
		HiddenSize:      hiddenSize,
		IntermediateSize: intermediateSize,
		NumLayers:       numLayers,
		NumHeads:        numHeads,
		NumKVHeads:      numHeads, // default MHA
		VocabSize:       vocabSize,
		WorldSize:       1,
		LayerShards:     make(map[int][]TensorShard),
	}, nil
}

// SetNumKVHeads — для Grouped-Query Attention (GQA).
func (m *ShardedModel) SetNumKVHeads(kvHeads int) error {
	if kvHeads <= 0 {
		return fmt.Errorf("rptensor: numKVHeads must be > 0, got %d", kvHeads)
	}
	if kvHeads > m.NumHeads {
		return fmt.Errorf("rptensor: numKVHeads (%d) cannot exceed numHeads (%d)", kvHeads, m.NumHeads)
	}
	if m.NumHeads%kvHeads != 0 {
		return fmt.Errorf("rptensor: numHeads (%d) must be divisible by numKVHeads (%d)", m.NumHeads, kvHeads)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.NumKVHeads = kvHeads
	return nil
}

// Partition — основная точка входа: запускает шардирование по стратегии.
//
// Для Column/Row/Megatron требуется делимость соответствующих размеров
// на WorldSize. Для Replicated — никаких ограничений (просто копирует
// каждый шард на все rank'ы).
func (m *ShardedModel) Partition(strategy PartitionStrategy, worldSize int) error {
	if worldSize <= 0 {
		return fmt.Errorf("rptensor: worldSize must be > 0, got %d", worldSize)
	}
	if worldSize > 64 {
		return fmt.Errorf("rptensor: worldSize too large (max 64), got %d", worldSize)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.WorldSize = worldSize
	m.Strategy = strategy
	m.LayerShards = make(map[int][]TensorShard, m.NumLayers)

	switch strategy {
	case ReplicatedPartition:
		m.partitionReplicated()
	case ColumnPartition:
		if err := m.partitionColumn(); err != nil {
			return err
		}
	case RowPartition:
		if err := m.partitionRow(); err != nil {
			return err
		}
	case MegatronPartition:
		if err := m.partitionMegatron(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("rptensor: unknown strategy %q", strategy)
	}
	return nil
}

// partitionColumn — column split для всех матриц.
func (m *ShardedModel) partitionColumn() error {
	if m.HiddenSize%m.WorldSize != 0 {
		return fmt.Errorf("rptensor: column split requires hiddenSize (%d) %% worldSize (%d) == 0",
			m.HiddenSize, m.WorldSize)
	}
	if m.IntermediateSize%m.WorldSize != 0 {
		return fmt.Errorf("rptensor: column split requires intermediateSize (%d) %% worldSize (%d) == 0",
			m.IntermediateSize, m.WorldSize)
	}
	if m.VocabSize%m.WorldSize != 0 {
		return fmt.Errorf("rptensor: column split requires vocabSize (%d) %% worldSize (%d) == 0",
			m.VocabSize, m.WorldSize)
	}

	colHidden := m.HiddenSize / m.WorldSize
	colFFN := m.IntermediateSize / m.WorldSize
	colVocab := m.VocabSize / m.WorldSize

	for layer := 0; layer < m.NumLayers; layer++ {
		shards := make([]TensorShard, 0, len(AllMatrices)*m.WorldSize)
		for rank := 0; rank < m.WorldSize; rank++ {
			for _, mx := range AllMatrices {
				shape := m.matrixShapeColumn(mx, colHidden, colFFN, colVocab)
				shards = append(shards, TensorShard{
					Rank:       rank,
					Layer:      layer,
					MatrixName: mx,
					Shape:      shape,
					Strategy:   ColumnPartition,
				})
			}
		}
		m.LayerShards[layer] = shards
	}
	return nil
}

// partitionRow — row split для всех матриц.
func (m *ShardedModel) partitionRow() error {
	if m.HiddenSize%m.WorldSize != 0 {
		return fmt.Errorf("rptensor: row split requires hiddenSize (%d) %% worldSize (%d) == 0",
			m.HiddenSize, m.WorldSize)
	}
	if m.IntermediateSize%m.WorldSize != 0 {
		return fmt.Errorf("rptensor: row split requires intermediateSize (%d) %% worldSize (%d) == 0",
			m.IntermediateSize, m.WorldSize)
	}

	rowHidden := m.HiddenSize / m.WorldSize
	rowFFN := m.IntermediateSize / m.WorldSize

	for layer := 0; layer < m.NumLayers; layer++ {
		shards := make([]TensorShard, 0, len(AllMatrices)*m.WorldSize)
		for rank := 0; rank < m.WorldSize; rank++ {
			for _, mx := range AllMatrices {
				shape := m.matrixShapeRow(mx, rowHidden, rowFFN)
				shards = append(shards, TensorShard{
					Rank:       rank,
					Layer:      layer,
					MatrixName: mx,
					Shape:      shape,
					Strategy:   RowPartition,
				})
			}
		}
		m.LayerShards[layer] = shards
	}
	return nil
}

// partitionReplicated — каждый шард целиком на каждом rank'е.
//
// Используется для embedding (matrix копируется на все GPU) или для
// тестов без реального шардирования.
func (m *ShardedModel) partitionReplicated() {
	for layer := 0; layer < m.NumLayers; layer++ {
		shards := make([]TensorShard, 0, len(AllMatrices)*m.WorldSize)
		for rank := 0; rank < m.WorldSize; rank++ {
			for _, mx := range AllMatrices {
				shape := m.matrixShapeFull(mx)
				shards = append(shards, TensorShard{
					Rank:       rank,
					Layer:      layer,
					MatrixName: mx,
					Shape:      shape,
					Strategy:   ReplicatedPartition,
				})
			}
		}
		m.LayerShards[layer] = shards
	}
}

// partitionMegatron — Megatron-LM style partitioning (column + row).
//
// Q/K/V/O: column split → [hidden, hidden/worldSize]
// MLP_gate, MLP_up: row split → [hidden, ffn/worldSize]
// MLP_down: column split → [ffn/worldSize, hidden]
// LM_head: column split → [vocab/worldSize, hidden]
// embed_tokens: replicated (full embedding table on every rank)
func (m *ShardedModel) partitionMegatron() error {
	if m.HiddenSize%m.WorldSize != 0 {
		return fmt.Errorf("rptensor: megatron requires hiddenSize (%d) %% worldSize (%d) == 0",
			m.HiddenSize, m.WorldSize)
	}
	if m.IntermediateSize%m.WorldSize != 0 {
		return fmt.Errorf("rptensor: megatron requires intermediateSize (%d) %% worldSize (%d) == 0",
			m.IntermediateSize, m.WorldSize)
	}
	if m.VocabSize%m.WorldSize != 0 {
		return fmt.Errorf("rptensor: megatron requires vocabSize (%d) %% worldSize (%d) == 0",
			m.VocabSize, m.WorldSize)
	}

	colHidden := m.HiddenSize / m.WorldSize
	colFFN := m.IntermediateSize / m.WorldSize
	colVocab := m.VocabSize / m.WorldSize

	for layer := 0; layer < m.NumLayers; layer++ {
		shards := make([]TensorShard, 0, len(AllMatrices)*m.WorldSize)
		for rank := 0; rank < m.WorldSize; rank++ {
			for _, mx := range AllMatrices {
				strategy, shape := m.megatronShardSpec(mx, colHidden, colFFN, colVocab)
				shards = append(shards, TensorShard{
					Rank:       rank,
					Layer:      layer,
					MatrixName: mx,
					Shape:      shape,
					Strategy:   strategy,
				})
			}
		}
		m.LayerShards[layer] = shards
	}
	return nil
}

// megatronShardSpec — возвращает (strategy, shape) для матрицы в Megatron-режиме.
func (m *ShardedModel) megatronShardSpec(mx MatrixName, colHidden, colFFN, colVocab int) (PartitionStrategy, []int) {
	switch mx {
	case MatrixQProj, MatrixKProj, MatrixVProj, MatrixOProj:
		// Column split: [hidden, hidden/ws]
		return ColumnPartition, []int{m.HiddenSize, colHidden}
	case MatrixMLPGate, MatrixMLPUp:
		// Row split: [hidden, ffn/ws]
		return RowPartition, []int{m.HiddenSize, colFFN}
	case MatrixMLPDown:
		// Column split: [ffn/ws, hidden]
		return ColumnPartition, []int{colFFN, m.HiddenSize}
	case MatrixLMHead:
		// Column split: [vocab/ws, hidden]
		return ColumnPartition, []int{colVocab, m.HiddenSize}
	case MatrixEmbed:
		// Replicated: full embedding on every rank.
		return ReplicatedPartition, []int{m.VocabSize, m.HiddenSize}
	}
	return ReplicatedPartition, []int{m.HiddenSize, m.HiddenSize}
}

// matrixShapeColumn — column-split shape для матрицы.
func (m *ShardedModel) matrixShapeColumn(mx MatrixName, colHidden, colFFN, colVocab int) []int {
	switch mx {
	case MatrixQProj, MatrixKProj, MatrixVProj, MatrixOProj:
		// [hidden, hidden/ws]
		return []int{m.HiddenSize, colHidden}
	case MatrixMLPGate, MatrixMLPUp:
		// [hidden, ffn/ws]
		return []int{m.HiddenSize, colFFN}
	case MatrixMLPDown:
		// [ffn/ws, hidden]
		return []int{colFFN, m.HiddenSize}
	case MatrixLMHead:
		// [vocab/ws, hidden]
		return []int{colVocab, m.HiddenSize}
	case MatrixEmbed:
		// Replicated даже в column-режиме (embedding не шардируется column-wise).
		return []int{m.VocabSize, m.HiddenSize}
	}
	return []int{m.HiddenSize, m.HiddenSize}
}

// matrixShapeRow — row-split shape для матрицы.
func (m *ShardedModel) matrixShapeRow(mx MatrixName, rowHidden, rowFFN int) []int {
	switch mx {
	case MatrixQProj, MatrixKProj, MatrixVProj, MatrixOProj:
		// [hidden/ws, hidden]
		return []int{rowHidden, m.HiddenSize}
	case MatrixMLPGate, MatrixMLPUp:
		// [hidden/ws, ffn]
		return []int{rowHidden, m.IntermediateSize}
	case MatrixMLPDown:
		// [ffn/ws, hidden]
		return []int{rowFFN, m.HiddenSize}
	case MatrixLMHead:
		// [vocab, hidden] (row-split по vocab не имеет смысла для LM head — fall back).
		return []int{m.VocabSize, m.HiddenSize}
	case MatrixEmbed:
		// [vocab, hidden] (replicated в row-режиме).
		return []int{m.VocabSize, m.HiddenSize}
	}
	return []int{m.HiddenSize, m.HiddenSize}
}

// matrixShapeFull — полная матрица (replicated).
func (m *ShardedModel) matrixShapeFull(mx MatrixName) []int {
	switch mx {
	case MatrixQProj, MatrixKProj, MatrixVProj, MatrixOProj:
		return []int{m.HiddenSize, m.HiddenSize}
	case MatrixMLPGate, MatrixMLPUp, MatrixMLPDown:
		return []int{m.HiddenSize, m.IntermediateSize}
	case MatrixLMHead, MatrixEmbed:
		return []int{m.VocabSize, m.HiddenSize}
	}
	return []int{m.HiddenSize, m.HiddenSize}
}

// ShardForRank — возвращает список шардов для конкретного rank'а на слое.
//
// Если layer или rank вне диапазона — возвращает (nil, error).
// Список отсортирован по MatrixName для детерминизма.
func (m *ShardedModel) ShardForRank(layer, rank int) ([]TensorShard, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if layer < 0 || layer >= m.NumLayers {
		return nil, fmt.Errorf("rptensor: layer %d out of range [0,%d)", layer, m.NumLayers)
	}
	if rank < 0 || rank >= m.WorldSize {
		return nil, fmt.Errorf("rptensor: rank %d out of range [0,%d)", rank, m.WorldSize)
	}

	all := m.LayerShards[layer]
	out := make([]TensorShard, 0, len(all)/maxInt(m.WorldSize, 1))
	for _, s := range all {
		if s.Rank == rank {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].MatrixName < out[j].MatrixName
	})
	return out, nil
}

// AllShardsForLayer — все шарды слоя (для тестов и отладки).
func (m *ShardedModel) AllShardsForLayer(layer int) ([]TensorShard, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if layer < 0 || layer >= m.NumLayers {
		return nil, fmt.Errorf("rptensor: layer %d out of range [0,%d)", layer, m.NumLayers)
	}
	out := make([]TensorShard, len(m.LayerShards[layer]))
	copy(out, m.LayerShards[layer])
	return out, nil
}

// TotalShards — общее число шардов (numLayers × WorldSize × len(AllMatrices)).
// Полезно для sanity-checks.
func (m *ShardedModel) TotalShards() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.WorldSize == 0 {
		return 0
	}
	return m.NumLayers * m.WorldSize * len(AllMatrices)
}

// Validate — проверяет, что модель была корректно шардирована.
//
// Возвращает error, если Partition не вызывался, или если worldSize
// выходит за допустимые пределы.
func (m *ShardedModel) Validate() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.WorldSize <= 0 {
		return fmt.Errorf("rptensor: model %q not partitioned (worldSize=0)", m.Name)
	}
	if m.WorldSize > 64 {
		return fmt.Errorf("rptensor: worldSize too large (%d)", m.WorldSize)
	}
	if len(m.LayerShards) != m.NumLayers {
		return fmt.Errorf("rptensor: model %q has %d layers in shards, expected %d",
			m.Name, len(m.LayerShards), m.NumLayers)
	}
	for layer := 0; layer < m.NumLayers; layer++ {
		shards, ok := m.LayerShards[layer]
		if !ok {
			return fmt.Errorf("rptensor: layer %d missing from LayerShards", layer)
		}
		expected := m.WorldSize * len(AllMatrices)
		if len(shards) != expected {
			return fmt.Errorf("rptensor: layer %d has %d shards, expected %d (worldSize*matrices)",
				layer, len(shards), expected)
		}
	}
	return nil
}

// roundUpTo — округление до ближайшего кратного.
// Используется для intermediate_size по умолчанию.
func roundUpTo(value, multiple int) int {
	if multiple <= 0 {
		return value
	}
	rem := value % multiple
	if rem == 0 {
		return value
	}
	return value + (multiple - rem)
}

// maxInt — максимум из двух int (для Go < 1.21 совместимости).
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}