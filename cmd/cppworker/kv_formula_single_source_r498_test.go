//go:build llama_stub

// kv_formula_single_source_r498_test.go — R498 (2026-11-19), аудит N-3/N-4.
//
// ЗАЧЕМ СТАТИЧЕСКИЙ СТОРОЖ. Дефект жил в глубине функции выбора стратегии
// загрузки (adaptive_loader), где значения считаются из GGUF-метаданных и
// свободной VRAM окружения — воспроизвести это юнит-тестом можно только повторив
// всю формулу, то есть тем же способом, каким дефект и появился.
//
// Суть дефекта N-3: `bytesPerKVCacheType[f16] = 4` — это уже K+V (2+2 байта), а
// рядом стоял множитель `2`, из-за чего KV считался вдвое больше, `maxViableNCtx`
// — вдвое меньше, и окно молча урезалось.
//
// N-4: формула cpu_only не использовала ключевую длину и считала head_dim как
// n_embd/n_heads (213 у Qwen3.8 против key_length 256) — расхождение до 3.13×.
//
// Контракт: в adaptive_loader нет собственной арифметики KV — только
// memfit.KVBytesPerElement / memfit.KVBytesPerToken и
// estimateKVCacheBytesForLayers (та же формула memfit).
package main

import (
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/memfit"
)

func TestR498_AdaptiveLoaderHasNoOwnKVFormula(t *testing.T) {
	const path = "adaptive_loader.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не читается %s: %v", path, err)
	}
	src := string(raw)

	// 1. Своей арифметики KV быть не должно: умножение на `bytesPerToken` из
	// локальной таблицы — ровно тот дефект (таблица уже включала K+V).
	if strings.Contains(src, "* bytesPerToken") {
		t.Error("в adaptive_loader снова есть умножение на bytesPerToken: множитель K+V " +
			"уже входит в memfit.KVBytesPerElement (аудит N-3)")
	}

	// 2. Формула обязана приходить из memfit.
	if !strings.Contains(src, "memfit.KVBytesPerElement") && !strings.Contains(src, "memfit.KVBytesPerToken") {
		t.Error("adaptive_loader не использует memfit для KV: значит в нём опять " +
			"своя копия формулы (аудит N-3/N-4)")
	}

	// 3. Таблицы «байт на элемент» в файле больше нет: её значения (f16 4, q8_0 2,
	// q4_0 1) и были источником двойного счёта и неверного квантования.
	if strings.Contains(src, "var bytesPerKVCacheType") {
		t.Error("таблица bytesPerKVCacheType вернулась — KV снова будет считаться " +
			"по ней, а не по memfit (аудит N-3/N-5)")
	}
}

// TestR498_NoFourthKVFormulaInBalancer — четвёртая формула KV (N-5) удалена: в
// autotune_budget.go квантованные типы считаются по memfit 34/32 и 18/32,
// а не «f16/2» и «f16/4».
func TestR498_NoFourthKVFormulaInBalancer(t *testing.T) {
	const path = "../../internal/balancer/autotune_budget.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не читается %s: %v", path, err)
	}
	src := string(raw)
	if strings.Contains(src, "perTokenF16 / 2") || strings.Contains(src, "perTokenF16 / 4") {
		t.Error("в autotune_budget снова своя формула квантования KV (f16/2, f16/4): " +
			"в llama.cpp q8_0 = 34/32, q4_0 = 18/32 байта на элемент (аудит N-5)")
	}
	if !strings.Contains(src, "memfit.KVBytesPerElement") {
		t.Error("autotune_budget не берёт константы KV из memfit — это снова копия формулы")
	}
}

// TestR498_KVFormulaMatchesMemfit — единственная формула KV сверена С memfit, а
// не с собственной копией правила.
//
// ЗАЧЕМ ЧИСЛОВАЯ ПРОВЕРКА, А НЕ СТАТИЧЕСКАЯ: статический сторож ловит только
// текст («есть memfit.KVBytesPerElement»), и мутация «вернуть локальную
// константу» оставалась зелёной (проверено). Здесь результат сравнивается с
// memfit.KVBytesPerToken на тех же параметрах — любая своя арифметика краснит.
func TestR498_KVFormulaMatchesMemfit(t *testing.T) {
	spec := memfit.ModelSpec{
		Name:      "qwen3.8-27b-q4",
		NLayers:   17, // KV-слоёв у гибридной модели меньше, чем блоков
		KVLayers:  17,
		NHeads:    40,
		NKvHeads:  8,
		NEmbd:     5120,
		KVHeadDim: 256, // key_length, а не n_embd/n_heads
	}
	for _, kv := range []string{"f16", "q8_0", "q4_0"} {
		want := int64(memfit.KVBytesPerToken(spec, cppbackend.MemfitKVType(kv)))
		got := kvBytesPerTokenR498(17, 8, 256, kv)
		if got != want {
			t.Errorf("kvBytesPerTokenR498(%s) = %d, memfit = %d: формула разошлась "+
				"с единственным источником (аудит N-3/N-5)", kv, got, want)
		}
	}

	// Двойной множитель K+V — ровно тот дефект: f16 должен быть 2 байта на
	// элемент, а не 4. На 1 слое/1 голове/1 байте head_dim это видно напрямую.
	if got := kvBytesPerTokenR498(1, 1, 1, "f16"); got != 4 { // 2 (K+V) * 1 * 1 * 1 * 2
		t.Errorf("f16 на 1 слое/1 голове/1 head_dim = %d, ожидалось 4 (2 элемента × 2 байта); "+
			"8 означало бы двойной счёт K+V", got)
	}

	// Квантованные типы: q8_0 и q4_0 — не «ровно /2 и /4», а 34/32 и 18/32.
	f16 := kvBytesPerTokenR498(48, 8, 128, "f16")
	q8 := kvBytesPerTokenR498(48, 8, 128, "q8_0")
	q4 := kvBytesPerTokenR498(48, 8, 128, "q4_0")
	if want := f16 * 34 / 64; q8 != want { // (34/32) / 2 — доля от f16
		t.Errorf("q8_0/f16 = %d/%d, ожидалось 34/64 от f16 (%d)", q8, f16, want)
	}
	if want := f16 * 18 / 64; q4 != want {
		t.Errorf("q4_0/f16 = %d/%d, ожидалось 18/64 от f16 (%d)", q4, f16, want)
	}

	if kvBytesPerTokenR498(0, 8, 128, "f16") != 0 {
		t.Error("при неизвестных слоях формула обязана вернуть 0, а не выдуманное число")
	}
}
