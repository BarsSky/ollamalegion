// Package rptensor — AllReduce функции для tensor parallelism (B8.2).
//
// AllReduce в tensor parallelism — это агрегация partial outputs от
// всех rank'ов (worldSize workers) в один финальный output. В
// production (real llama.cpp / NCCL) это была бы настоящая сумма по
// FP16/BF16-тензорам. На данном этапе (Session 12) мы реализуем
// три простые стратегии, работающие на []byte:
//
//   - AllReduceConcat: concat в порядке rank 0..N-1.
//   - AllReduceSumBytes: поэлементная сумма байт (по модулю 256).
//   - AllReduceXorBytes: XOR по байтам.
//
// SumBytes и XorBytes используются для round-trip проверки детерминизма
// (например, чтобы ранг-выход отличался и был агрегирован правильно).
// Реальная FP16/BF16-агрегация отложена в B8.7 (post-1.0, ggml bridge).
package rptensor

import (
	"errors"
	"fmt"
)

// AllReduceConcat — конкатенирует partial outputs в порядке rank 0..worldSize-1.
//
// Поведение:
//   - all ranks present → строгий concat (full output).
//   - any rank missing → degraded concat (только присутствующие ranks, в порядке rank).
//   - все ranks missing → error (невозможно агрегировать пустой вход).
//
// Degraded mode включается автоматически: если хоть один rank отсутствует,
// AllReduceConcat пропускает его (не error), и coordinator помечает
// ответ как Degraded=true. Это позволяет TP inference продолжаться
// при отказе отдельных worker'ов.
func AllReduceConcat(partials map[int][]byte, worldSize int) ([]byte, error) {
	if worldSize <= 0 {
		return nil, errors.New("rptensor: worldSize must be > 0")
	}
	if len(partials) == 0 {
		return nil, errors.New("rptensor: all-reduce has no partials (all ranks failed)")
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

// AllReduceSumBytes — поэлементная сумма байт по модулю 256.
//
// Все partial outputs должны иметь одинаковую длину (последний шард
// может быть короче — см. PartialLenForRank). Несовпадение длин → error.
func AllReduceSumBytes(partials map[int][]byte, worldSize int) ([]byte, error) {
	if worldSize <= 0 {
		return nil, errors.New("rptensor: worldSize must be > 0")
	}
	// Определяем общую длину (= длине первого непустого partial).
	var length int
	for rank := 0; rank < worldSize; rank++ {
		out, ok := partials[rank]
		if !ok {
			return nil, fmt.Errorf("rptensor: all-reduce missing rank %d", rank)
		}
		if length == 0 {
			length = len(out)
			continue
		}
		if len(out) != length {
			return nil, fmt.Errorf("rptensor: all-reduce rank %d has length %d, expected %d",
				rank, len(out), length)
		}
	}
	if length == 0 {
		return []byte{}, nil
	}

	out := make([]byte, length)
	for rank := 0; rank < worldSize; rank++ {
		p := partials[rank]
		for i := 0; i < length; i++ {
			out[i] = byte(int(out[i]) + int(p[i]))
		}
	}
	return out, nil
}

// AllReduceXorBytes — XOR по байтам.
//
// Требования к длинам — те же что и для SumBytes.
func AllReduceXorBytes(partials map[int][]byte, worldSize int) ([]byte, error) {
	if worldSize <= 0 {
		return nil, errors.New("rptensor: worldSize must be > 0")
	}
	var length int
	for rank := 0; rank < worldSize; rank++ {
		out, ok := partials[rank]
		if !ok {
			return nil, fmt.Errorf("rptensor: all-reduce missing rank %d", rank)
		}
		if length == 0 {
			length = len(out)
			continue
		}
		if len(out) != length {
			return nil, fmt.Errorf("rptensor: all-reduce rank %d has length %d, expected %d",
				rank, len(out), length)
		}
	}
	if length == 0 {
		return []byte{}, nil
	}

	out := make([]byte, length)
	for rank := 0; rank < worldSize; rank++ {
		p := partials[rank]
		for i := 0; i < length; i++ {
			out[i] ^= p[i]
		}
	}
	return out, nil
}

// AllReduceMeanBytes — среднее арифметическое (для fp16/bf16 в production).
//
// В stub-режиме используем обычную integer division на worldSize.
func AllReduceMeanBytes(partials map[int][]byte, worldSize int) ([]byte, error) {
	sum, err := AllReduceSumBytes(partials, worldSize)
	if err != nil {
		return nil, err
	}
	if worldSize == 0 {
		return nil, errors.New("rptensor: worldSize must be > 0")
	}
	for i := range sum {
		sum[i] = byte(int(sum[i]) / worldSize)
	}
	return sum, nil
}

// PartialLenForRank — длина partial output для rank'а при равномерном split'е.
//
// Используется в stub-режиме для генерации детерминированных partial outputs.
// Возвращает ceil(inputLen / worldSize) для всех rank'ов кроме последнего,
// и remainder для последнего.
func PartialLenForRank(inputLen, worldSize, rank int) int {
	if worldSize <= 0 || rank < 0 || rank >= worldSize {
		return 0
	}
	if inputLen == 0 {
		return 0
	}
	base := inputLen / worldSize
	rem := inputLen % worldSize
	if rank < rem {
		return base + 1
	}
	return base
}