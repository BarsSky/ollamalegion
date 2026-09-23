// kv_cache_type.go — R67a (2026-09-23): учёт типа квантования KV-cache в оценках
// VRAM/feasible n_ctx.
//
// ЗАЧЕМ. Все оценки (estimateKVCacheMB для выбора числа GPU-слоёв и
// CalculateResourceLimits для max_vram_n_ctx/feasible_max_context, которые
// балансер использует как потолок n_ctx) считали KV-cache как fp16 —
// bytes_per_elem = 2, БЕЗ учёта kvCacheType. При q8_0 реальный KV вдвое меньше,
// при q4_0 — почти вчетверо. На мощном GPU (A10 24GB) это давало жалобу
// «n_ctx жёстко упирается в 32768, хотя VRAM с запасом»: эстиматор резервировал
// в 2-4 раза больше памяти, чем модель реально занимает, и «feasible» потолок
// оказывался заниженным, а частичный CPU-offload включался без необходимости.
//
// Размеры блоков (llama.cpp block_*):
//
//	f16  — 2 байта на элемент (базовый, backward compat)
//	q8_0 — 32 значения в блоке: 32 байта (8-бит) + 2 байта fp16-масштаб = 34/32
//	q4_0 — 32 значения в блоке: 16 байт (4-бит) + 2 байта fp16-масштаб = 18/32
//
// Значения намеренно считаются в целочисленной арифметике (num/den), чтобы
// результат не зависел от float-округлений на разных платформах.
package cppbackend

import (
	"os"
	"strconv"
	"strings"
)

// kvCacheBytesPerElem — байт на элемент KV-cache в виде дроби num/den.
// Неизвестный/пустой тип трактуется как f16 (консервативно, backward compat).
func kvCacheBytesPerElem(kvCacheType string) (num, den uint64) {
	switch strings.ToLower(strings.TrimSpace(kvCacheType)) {
	case "q8_0":
		return 34, 32
	case "q4_0":
		return 18, 32
	case "f16", "fp16", "":
		return 2, 1
	default:
		// Неизвестный тип (например, f32) — не занижаем требования: f16.
		return 2, 1
	}
}

// effectiveKVCacheType — какой тип KV использовать в оценке: явный из запроса/
// профиля, иначе дефолт конфига cppworker, иначе f16.
func effectiveKVCacheType(explicit, fallback string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	return "f16"
}

// defaultVRAMOverheadMB — сколько VRAM резервировать на runtime/фрагментацию при
// расчёте потолка n_ctx (было жёстко 2 GB).
const defaultVRAMOverheadMB = uint64(2048)

// vramOverheadMBConfig — R67a: настраиваемый резерв VRAM.
//
// CPPWORKER_VRAM_OVERHEAD_MB=N (0 = без резерва, отрицательные/мусор → дефолт).
// Зачем: на картах с большим объёмом VRAM фиксированные 2 GB заметно занижали
// max_vram_n_ctx; оператор может уменьшить резерв (например, 512 MB на A10 24GB)
// и тем самым поднять разрешённый n_ctx.
func vramOverheadMBConfig() uint64 {
	v := strings.TrimSpace(os.Getenv("CPPWORKER_VRAM_OVERHEAD_MB"))
	if v == "" {
		return defaultVRAMOverheadMB
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return defaultVRAMOverheadMB
	}
	return uint64(n)
}
