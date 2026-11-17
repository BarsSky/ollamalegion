// kv_formula_r498.go — R498 (2026-11-19), аудит N-3: ОДНА формула KV для
// cppworker.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ. Дефект жил в двух местах adaptive_loader, где KV
// считался «на месте»: `bytesPerKVCacheType[f16] = 4` (это уже K+V) плюс ещё
// множитель 2 → KV вдвое больше реального, maxViableNCtx вдвое меньше, окно
// молча урезалось. Для q8_0/q4_0 там же стояли 2 и 1, тогда как llama.cpp держит
// 34/32 и 18/32 (значение + масштаб на блок) — ещё и это расходилось с memfit.
//
// Пока формула живёт внутри большой функции выбора стратегии, её нельзя ни
// проверить числом, ни удержать от повторного появления (статический сторож
// ловит только текст). Здесь она вынесена в чистую функцию и покрыта тестом,
// который сверяет результат С memfit, а не с собственной копией правила.
package main

import (
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/memfit"
)

// kvBytesPerTokenR498 — байт KV-кэша на один токен.
//
// Формула ровно как в memfit.KVBytesPerToken: 2 (K и V) × слоёв с кэшем ×
// голов KV × head_dim × байт-на-элемент (f16 2/1, q8_0 34/32, q4_0 18/32).
// Умножение на числитель идёт ДО деления, чтобы не терять точность на целых.
//
// Возвращает 0 при неизвестных размерах: вызывающий сам решает, что делать
// (в adaptive_loader есть страховка на такой случай).
func kvBytesPerTokenR498(layers, kvHeads, headDim int, kvType string) int64 {
	if layers <= 0 || kvHeads <= 0 || headDim <= 0 {
		return 0
	}
	num, den := memfit.KVBytesPerElement(cppbackend.MemfitKVType(kvType))
	if num <= 0 || den <= 0 {
		return 0
	}
	return int64(2) * int64(layers) * int64(kvHeads) * int64(headDim) * num / den
}
