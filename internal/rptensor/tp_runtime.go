package rptensor

import (
	"context"
	"errors"
)

// TPRuntime — абстракция над реальной llama.cpp / ggml tensor-parallel execution.
//
// В Session 12 (B8.7) предоставляется только stub-реализация StubTPRuntime,
// которая возвращает deterministic partial output для тестов. Реальная
// интеграция с ggml + NCCL all-reduce отложена в post-1.0 фазу.
//
// Контракт интерфейса:
//
//   - InferShard: запустить одну матрицу матрицу на rank'е (partial computation).
//   - AllReduce: агрегировать partial outputs от всех rank'ов.
//
// StubTPRuntime использует rptensor.PartialLenForRank для расчёта длины
// partial output и заполняет его rank-маркированными байтами. Это даёт
// детерминированный вывод для e2e тестов без реальной математики.
type TPRuntime interface {
	// InferShard — запуск partial computation на одном rank'е.
	//
	// В stub: возвращает partial output длиной PartialLenForRank(len(input), worldSize, rank)
	// с rank-маркированными байтами.
	//
	// В production: вызывает ggml/tensor parallel kernel для шарда матрицы.
	InferShard(ctx context.Context, model *ShardedModel, rank int, input []byte) ([]byte, error)

	// AllReduce — агрегация partial outputs (вызывается после каждого слоя).
	//
	// В stub: concat bytes по rank order (с degraded mode для отсутствующих ranks).
	//
	// В production: NCCL AllReduce поверх GPU tensors (FP16/BF16 sum).
	AllReduce(partials map[int][]byte, worldSize int) ([]byte, error)

	// Name — идентификатор runtime (для логирования и метрик).
	Name() string

	// Close — освобождение ресурсов (для real runtime — освобождение GPU memory).
	Close() error
}

// StubTPRuntime — дефолтная реализация TPRuntime для тестов и dev-режима.
//
// Алгоритм:
//   - InferShard: возвращает partial output длиной tpPartialLen с rank-маркированными байтами.
//   - AllReduce: конкатенирует partial outputs в порядке rank 0..worldSize-1.
//                 Пропускает отсутствующие ranks (degraded mode).
//
// Не выполняет реальной матричной математики — только детерминированный
// I/O для проверки инфраструктуры tensor parallelism.
type StubTPRuntime struct {
	closed bool
}

// NewStubTPRuntime — конструктор.
func NewStubTPRuntime() *StubTPRuntime {
	return &StubTPRuntime{}
}

// InferShard — stub-реализация.
func (s *StubTPRuntime) InferShard(ctx context.Context, model *ShardedModel, rank int, input []byte) ([]byte, error) {
	if s.closed {
		return nil, errors.New("rptensor: StubTPRuntime is closed")
	}
	if model == nil {
		return nil, errors.New("rptensor: model is nil")
	}
	if rank < 0 || rank >= model.WorldSize {
		return nil, errors.New("rptensor: rank out of range")
	}

	// Проверяем контекст (для cooperative cancellation).
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Deterministic partial output.
	plen := PartialLenForRank(len(input), model.WorldSize, rank)
	out := make([]byte, plen)
	for i := range out {
		out[i] = byte(rank + 1) // rank 0 → 1, rank 1 → 2, ...
	}
	return out, nil
}

// AllReduce — stub-реализация (concat в порядке rank).
//
// Аналог AllReduceConcat, но встроен в runtime для симметрии с
// production-реализацией (где AllReduce будет NCCL-вызовом).
func (s *StubTPRuntime) AllReduce(partials map[int][]byte, worldSize int) ([]byte, error) {
	if s.closed {
		return nil, errors.New("rptensor: StubTPRuntime is closed")
	}
	if worldSize <= 0 {
		return nil, errors.New("rptensor: worldSize must be > 0")
	}
	if len(partials) == 0 {
		return nil, errors.New("rptensor: no partials to reduce")
	}

	totalLen := 0
	for rank := 0; rank < worldSize; rank++ {
		if out, ok := partials[rank]; ok {
			totalLen += len(out)
		}
	}
	out := make([]byte, 0, totalLen)
	for rank := 0; rank < worldSize; rank++ {
		if p, ok := partials[rank]; ok {
			out = append(out, p...)
		}
	}
	return out, nil
}

// Name — "stub".
func (s *StubTPRuntime) Name() string { return "stub" }

// Close — no-op для stub.
func (s *StubTPRuntime) Close() error {
	s.closed = true
	return nil
}