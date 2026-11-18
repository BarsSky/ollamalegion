//go:build llama_stub

// feasible_memory_r521_test.go — R521 (2026-11-19, §8.3.1): потолок окна по
// ПАМЯТИ, а не «половина свободной VRAM».
//
// ЖИВОЙ ФАКТ (архив 2026-11-18, A10 24 ГБ, 27B Q4_K_M, kv q8_0): прежняя формула
// `(freeVRAM/2)/kvPerToken` разрешила окно 131072; после загрузки осталось
// 2508 МБ, и первый же llama_decode на 80k-промпте упал с CUDA OOM. Она не
// вычитала ни веса модели, ни compute buffer.
package cppbackend

import "testing"

func TestR521_WeightsAreSubtractedFromFreeVRAM(t *testing.T) {
	const kvPerToken = 73728 // 4B, q8_0
	// Та же свободная VRAM: без вычета весов формула дала бы (8000/2)/72KB ≈ 56k,
	// с вычетом весов и буферов — заметно меньше.
	got := maxVRAMCtxFromMemory(8000, 2381, 1536, kvPerToken, 262144)
	// 8000 − 2381 − 1536 − 119 (5 % фрагментация) = 3964 МБ
	want := int(uint64(3964)*1024*1024) / kvPerToken
	if got != want {
		t.Fatalf("maxVRAMCtx = %d, want %d (свободная − веса − compute buffer − фрагментация)", got, want)
	}
	halfOfFree := int((uint64(8000/2))*1024*1024) / kvPerToken
	if got >= halfOfFree {
		t.Fatalf("значение не уменьшилось против прежней формулы «половина свободной»: %d >= %d", got, halfOfFree)
	}
}

func TestR521_NoRoomMeansZeroNotAGuess(t *testing.T) {
	// Веса 16.46 ГБ на 1 ГБ свободной VRAM: места нет, и это 0, а не «неизвестно».
	if got := maxVRAMCtxFromMemory(1000, 16855, 2048, 33792, 262144); got != 0 {
		t.Fatalf("при нехватке памяти maxVRAMCtx = %d, want 0", got)
	}
	// Ровно сходится в ноль тоже 0, а не минимальное окно.
	if got := maxVRAMCtxFromMemory(16855+2048+512, 16855, 2048, 33792, 262144); got != 0 {
		t.Fatalf("на границе maxVRAMCtx = %d, want 0", got)
	}
}

func TestR521_GGUFClampStillApplies(t *testing.T) {
	got := maxVRAMCtxFromMemory(100000, 1000, 1536, 1024, 32768)
	if got != 32768 {
		t.Fatalf("окно не ограничено GGUF: %d, want 32768", got)
	}
}

func TestR521_HeavyComputeBufferForBigWeights(t *testing.T) {
	if v := computeBufferReserveMB(4096); v != computeBufferFloorMB {
		t.Fatalf("для 4 ГБ весов резерв = %d, want %d", v, computeBufferFloorMB)
	}
	if v := computeBufferReserveMB(16855); v != computeBufferHeavyMB {
		t.Fatalf("для 16.46 ГБ весов резерв = %d, want %d", v, computeBufferHeavyMB)
	}
}

func TestR521_ComputeBufferEnvOverride(t *testing.T) {
	t.Setenv("CPPWORKER_COMPUTE_BUFFER_MB", "4096")
	if v := computeBufferReserveMB(16855); v != 4096 {
		t.Fatalf("env-override не применён: резерв = %d, want 4096", v)
	}
	t.Setenv("CPPWORKER_COMPUTE_BUFFER_MB", "мусор")
	if v := computeBufferReserveMB(4096); v != computeBufferFloorMB {
		t.Fatalf("мусор в env обязан откатываться к дефолту: %d", v)
	}
}

func TestR521_UnknownKVMeansUnknownCeiling(t *testing.T) {
	if got := maxVRAMCtxFromMemory(8000, 2381, 1536, 0, 262144); got != 0 {
		t.Fatalf("без kvPerToken окно = %d, want 0 (считать нечем)", got)
	}
}
