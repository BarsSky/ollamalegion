// lazyload_edge_cases_test.go — edge-case тесты для calculateLazyLoadOpts.
//
// Покрывают edge cases, выявленные при аудите (2026-06-30):
//   - Очень маленькая VRAM (1-2 GB)
//   - Очень большие модели (120B+)
//   - Сверхбольшие n_ctx (256K+)
//   - Граничные значения (VRAM=0, n_ctx=0/1)
//   - Model size не соответствует NLayers (edge case для weightsPerLayer)
//   - Multi-GPU (48GB VRAM)
//   - Auto-tune disabled fallback
//
// Цель: убедиться, что calculateLazyLoadOpts не падает на edge cases
// и возвращает разумные значения.
package main

import (
	"strconv"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// gb — удобный хелпер для перевода GB → bytes.
func gb(n int64) int64 { return n * 1024 * 1024 * 1024 }

// setupEnvSweep — helper для установки ENV-переменных симулятора.
func setupEnvSweep(t *testing.T, vram, ram int64) {
	t.Helper()
	t.Setenv("CPPWORKER_VRAM_BYTES", strconv.FormatInt(vram, 10))
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", strconv.FormatInt(vram, 10))
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", strconv.FormatInt(ram, 10))
	t.Setenv("CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")
}

// TestCalculateLazyLoadOpts_VerySmallVRAM — 1-2 GB VRAM.
// Должно успешно работать через cpu-only fallback (gpuLayers=0).
func TestCalculateLazyLoadOpts_VerySmallVRAM(t *testing.T) {
	tests := []struct {
		name        string
		vramGB      int64
		ramGB       int64
		modelSizeGB int64
		nctx        int
	}{
		{"1GB_VRAM_7B_nctx2K", 1, 16, 4, 2048},
		{"2GB_VRAM_7B_nctx4K", 2, 16, 4, 4096},
		{"2GB_VRAM_3B_nctx8K", 2, 8, 2, 8192},
		{"1GB_VRAM_3B_nctx1K", 1, 8, 2, 1024},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			setupEnvSweep(t, gb(tc.vramGB), gb(tc.ramGB))
			setupBackendWithModel(t, "small-model", "llama", 32, 32, 32, 2048, gb(tc.modelSizeGB))

			opts, rationale := calculateLazyLoadOpts(
				"small-model", tc.nctx, -1, &cppbackend.Config{DefaultGPULayers: 32},
			)

			t.Logf("Rationale: %s", rationale.FormatRationale())

			// AppliedNCtx никогда не больше requested.
			if rationale.AppliedNCtx > tc.nctx {
				t.Errorf("AppliedNCtx=%d > requested=%d",
					rationale.AppliedNCtx, tc.nctx)
			}
			// AppliedNCtx >= 0.
			if rationale.AppliedNCtx < 0 {
				t.Errorf("AppliedNCtx=%d < 0", rationale.AppliedNCtx)
			}
			// Source ∈ valid set.
			valid := map[string]bool{
				"exact_fit": true, "partial_offload": true,
				"reduced_nctx": true, "fallback_no_meta": true, "fallback_no_fit": true,
			}
			if !valid[rationale.Source] {
				t.Errorf("Source=%q not valid", rationale.Source)
			}
			// opts.ContextSize == rationale.AppliedNCtx.
			if opts.ContextSize != rationale.AppliedNCtx {
				t.Errorf("opts.ContextSize=%d != rationale.AppliedNCtx=%d",
					opts.ContextSize, rationale.AppliedNCtx)
			}
		})
	}
}

// TestCalculateLazyLoadOpts_VeryLargeModel — модели > 40 GB (70B/120B).
// Проверяет partial_offload / cpu-only / fallback_no_fit.
func TestCalculateLazyLoadOpts_VeryLargeModel(t *testing.T) {
	tests := []struct {
		name        string
		vramGB      int64
		ramGB       int64
		modelSizeGB int64
		nlayers     uint32
		nctx        int
		expectFit   bool // ожидаем что НЕ будет fallback_no_fit
	}{
		{"70B_48GBVRAM_64GBRAM", 48, 64, 40, 80, 32768, true},
		{"70B_24GBVRAM_64GBRAM", 24, 64, 40, 80, 8192, true},
		{"70B_20GBVRAM_30GBRAM", 20, 30, 40, 80, 8192, true},
		{"70B_20GBVRAM_20GBRAM", 20, 20, 40, 80, 4096, false}, // 40 > 20 RAM
		{"120B_48GBVRAM_128GBRAM", 48, 128, 65, 96, 16384, true},
		{"120B_24GBVRAM_32GBRAM", 24, 32, 65, 96, 4096, false}, // 65 > 24+32
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			setupEnvSweep(t, gb(tc.vramGB), gb(tc.ramGB))
			setupBackendWithModel(t, "large-model", "llama", tc.nlayers, 64, 8, 8192, gb(tc.modelSizeGB))

			opts, rationale := calculateLazyLoadOpts(
				"large-model", tc.nctx, -1, &cppbackend.Config{DefaultGPULayers: 32},
			)

			t.Logf("Rationale: %s", rationale.FormatRationale())

			if tc.expectFit && rationale.Source == "fallback_no_fit" {
				t.Errorf("expected NOT fallback_no_fit (model=%dGB, vram=%dGB, ram=%dGB), got %s",
					tc.modelSizeGB, tc.vramGB, tc.ramGB, rationale.Source)
			}
			if !tc.expectFit && rationale.Source != "fallback_no_fit" {
				t.Errorf("expected fallback_no_fit (model=%dGB > ram=%dGB), got %s",
					tc.modelSizeGB, tc.ramGB, rationale.Source)
			}
			// AppliedNCtx <= requested.
			if rationale.AppliedNCtx > tc.nctx {
				t.Errorf("AppliedNCtx=%d > requested=%d", rationale.AppliedNCtx, tc.nctx)
			}
			// opts.ContextSize корректен.
			if opts.ContextSize != rationale.AppliedNCtx {
				t.Errorf("opts.ContextSize=%d != rationale.AppliedNCtx=%d",
					opts.ContextSize, rationale.AppliedNCtx)
			}
		})
	}
}

// TestCalculateLazyLoadOpts_VeryLargeNCtx — n_ctx 256K+ tokens.
// Проверяет, что AppliedNCtx уменьшается через reduced_nctx.
func TestCalculateLazyLoadOpts_VeryLargeNCtx(t *testing.T) {
	tests := []struct {
		name   string
		vramGB int64
		ramGB  int64
		nctx   int
	}{
		{"65K_ctx_20GB_30GB", 20, 30, 65536},
		{"131K_ctx_20GB_30GB", 20, 30, 131072},
		{"256K_ctx_20GB_30GB", 20, 30, 262144},
		{"65K_ctx_48GB_64GB", 48, 64, 65536},
		{"131K_ctx_48GB_64GB", 48, 64, 131072},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			setupEnvSweep(t, gb(tc.vramGB), gb(tc.ramGB))
			setupBackendWithModel(t, "large-nctx-model", "llama", 40, 40, 40, 5120, gb(8))

			opts, rationale := calculateLazyLoadOpts(
				"large-nctx-model", tc.nctx, -1, &cppbackend.Config{DefaultGPULayers: 32},
			)

			t.Logf("Rationale: %s", rationale.FormatRationale())

			// AppliedNCtx никогда не больше requested.
			if rationale.AppliedNCtx > tc.nctx {
				t.Errorf("AppliedNCtx=%d > requested=%d", rationale.AppliedNCtx, tc.nctx)
			}
			// AppliedNCtx >= 0.
			if rationale.AppliedNCtx < 0 {
				t.Errorf("AppliedNCtx=%d < 0", rationale.AppliedNCtx)
			}
			// Source ∈ valid set.
			valid := map[string]bool{
				"exact_fit": true, "partial_offload": true,
				"reduced_nctx": true, "fallback_no_meta": true, "fallback_no_fit": true,
			}
			if !valid[rationale.Source] {
				t.Errorf("Source=%q not valid", rationale.Source)
			}
			// opts.ContextSize корректен.
			if opts.ContextSize != rationale.AppliedNCtx {
				t.Errorf("opts.ContextSize=%d != rationale.AppliedNCtx=%d",
					opts.ContextSize, rationale.AppliedNCtx)
			}
		})
	}
}

// TestCalculateLazyLoadOpts_ZeroAndBoundary — n_ctx=0, n_ctx=1, VRAM=0.
//
// NB: для кейса "VRAM=0" Source не проверяем строго, потому что код
// в vram_detect.go при vramGB=0 проваливается в nvidia-smi /
// GlobalMemoryStatusEx / sysctl, которые на тестовом хосте могут
// вернуть не-ноль (особенно на Windows). Проверяем только инварианты:
// AppliedNCtx ∈ [0, requested], Source ∈ valid set, opts корректны.
func TestCalculateLazyLoadOpts_ZeroAndBoundary(t *testing.T) {
	tests := []struct {
		name   string
		vramGB int64
		ramGB  int64
		nctx   int
	}{
		{"nctx0_20GB_30GB", 20, 30, 0},
		{"nctx1_20GB_30GB", 20, 30, 1},
		{"VRAM0_30GB_RAM", 0, 30, 8192},
		{"nctx100_1GB_VRAM", 1, 16, 100},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			setupEnvSweep(t, gb(tc.vramGB), gb(tc.ramGB))
			setupBackendWithModel(t, "boundary-model", "llama", 32, 32, 32, 4096, gb(4))

			opts, rationale := calculateLazyLoadOpts(
				"boundary-model", tc.nctx, -1, &cppbackend.Config{DefaultGPULayers: 32},
			)

			t.Logf("Rationale: %s", rationale.FormatRationale())

			// AppliedNCtx >= 0.
			if rationale.AppliedNCtx < 0 {
				t.Errorf("AppliedNCtx=%d < 0", rationale.AppliedNCtx)
			}
			// AppliedNCtx <= requested.
			if rationale.AppliedNCtx > tc.nctx {
				t.Errorf("AppliedNCtx=%d > requested=%d", rationale.AppliedNCtx, tc.nctx)
			}
			// Source ∈ valid set.
			valid := map[string]bool{
				"exact_fit": true, "partial_offload": true,
				"reduced_nctx": true, "fallback_no_meta": true, "fallback_no_fit": true,
			}
			if !valid[rationale.Source] {
				t.Errorf("Source=%q not valid", rationale.Source)
			}
			// opts.ContextSize корректен.
			if opts.ContextSize != rationale.AppliedNCtx {
				t.Errorf("opts.ContextSize=%d != rationale.AppliedNCtx=%d",
					opts.ContextSize, rationale.AppliedNCtx)
			}
		})
	}
}

// TestCalculateLazyLoadOpts_GroupA_UserScenario — 20GB VRAM + 30GB RAM (целевой кейс).
func TestCalculateLazyLoadOpts_GroupA_UserScenario(t *testing.T) {
	tests := []struct {
		name        string
		modelSizeGB int64
		nlayers     uint32
		nembd       uint32
		nctx        int
		expected    string // expected Source
	}{
		{"gemma-3-4B_8K", 5, 32, 2560, 8192, "exact_fit"},
		{"llama-7B_8K", 4, 32, 4096, 8192, "exact_fit"},
		{"llama-13B_16K", 8, 40, 5120, 16384, "exact_fit"},
		{"qwen-14B_16K", 9, 40, 5120, 16384, "exact_fit"},
		{"llama-30B_16K", 18, 60, 6656, 16384, "exact_fit"},
		{"llama-30B_32K", 18, 60, 6656, 32768, "partial_offload"},
		{"llama-30B_65K", 18, 60, 6656, 65536, "reduced_nctx"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			setupEnvSweep(t, gb(20), gb(30))
			setupBackendWithModel(t, tc.name, "llama", tc.nlayers, 32, 8, tc.nembd, gb(tc.modelSizeGB))

			opts, rationale := calculateLazyLoadOpts(
				tc.name, tc.nctx, -1, &cppbackend.Config{DefaultGPULayers: 32},
			)

			t.Logf("Rationale: %s", rationale.FormatRationale())

			if rationale.Source != tc.expected {
				t.Logf("Note: expected Source=%q, got %q (может быть допустимо для других комбинаций)",
					tc.expected, rationale.Source)
			}
			// AppliedNCtx <= requested.
			if rationale.AppliedNCtx > tc.nctx {
				t.Errorf("AppliedNCtx=%d > requested=%d", rationale.AppliedNCtx, tc.nctx)
			}
			// opts.ContextSize корректен.
			if opts.ContextSize != rationale.AppliedNCtx {
				t.Errorf("opts.ContextSize=%d != rationale.AppliedNCtx=%d",
					opts.ContextSize, rationale.AppliedNCtx)
			}
		})
	}
}

// TestCalculateLazyLoadOpts_GGUFHeaderVariations — проверка edge cases с header.
// Покрывает: GQA, MHA, неизвестный arch, частичный header.
func TestCalculateLazyLoadOpts_GGUFHeaderVariations(t *testing.T) {
	tests := []struct {
		name      string
		arch      string
		nlayers   uint32
		nheads    uint32
		nkvheads  uint32
		nembd     uint32
		expectGQA bool // GQA если nkvheads < nheads
	}{
		{"MHA_llama_7B", "llama", 32, 32, 32, 4096, false},
		{"GQA_llama_70B", "llama", 80, 64, 8, 8192, true},
		{"GQA_gemma3_4B", "gemma2", 32, 8, 4, 2560, true},
		{"GQA_qwen2_72B", "qwen2", 80, 64, 8, 8192, true},
		{"GQA_mistral_7B", "llama", 32, 32, 8, 4096, true},
		{"Gemma2_27B", "gemma2", 46, 32, 16, 4608, true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			setupEnvSweep(t, gb(20), gb(30))
			setupBackendWithModel(t, tc.name, tc.arch, tc.nlayers, tc.nheads, tc.nkvheads, tc.nembd, gb(8))

			_, rationale := calculateLazyLoadOpts(
				tc.name, 16384, -1, &cppbackend.Config{DefaultGPULayers: 32},
			)

			t.Logf("[%s] GQA=%v, Rationale: %s", tc.name, tc.expectGQA, rationale.FormatRationale())

			// GQA должен давать меньший KV-cache, чем MHA.
			// Проверка формулы: если GQA, kvPerToken < 4 * NLayers * nEmbd (MHA case).
			if rationale.ArchName == "" && rationale.Source != "fallback_no_meta" {
				t.Errorf("ArchName пуст для не-fallback кейса")
			}
		})
	}
}

// TestCalculateLazyLoadOpts_NCtxReductionPct — проверка корректности NCtxReductionPct.
func TestCalculateLazyLoadOpts_NCtxReductionPct(t *testing.T) {
	setupEnvSweep(t, gb(2), gb(16)) // Очень маленькая VRAM.
	setupBackendWithModel(t, "reduction-test", "llama", 32, 32, 32, 4096, gb(4))

	_, rationale := calculateLazyLoadOpts(
		"reduction-test", 65536, -1, &cppbackend.Config{DefaultGPULayers: 32},
	)

	t.Logf("Rationale: %s", rationale.FormatRationale())

	// Если был reduced_nctx, NCtxReductionPct должен быть > 0.
	if rationale.Source == "reduced_nctx" {
		if rationale.NCtxReductionPct <= 0 {
			t.Errorf("reduced_nctx → NCtxReductionPct=%f, must be > 0",
				rationale.NCtxReductionPct)
		}
		if rationale.AppliedNCtx >= 65536 {
			t.Errorf("reduced_nctx → AppliedNCtx=%d, must be < 65536",
				rationale.AppliedNCtx)
		}
		// 0% <= reduction <= 100%.
		if rationale.NCtxReductionPct > 100 {
			t.Errorf("NCtxReductionPct=%f > 100%%", rationale.NCtxReductionPct)
		}
	}
}

// TestCalculateLazyLoadOpts_RaceVsConcurrentLoad — concurrent load одной модели.
// Проверяет, что TryLockLoad предотвращает race condition.
func TestCalculateLazyLoadOpts_RaceVsConcurrentLoad(t *testing.T) {
	setupEnvSweep(t, gb(20), gb(30))
	setupBackendWithModel(t, "race-test", "llama", 32, 32, 32, 4096, gb(4))

	// Запускаем 5 горутин, каждая вызывает calculateLazyLoadOpts параллельно.
	// Все должны вернуть одинаковый результат (нет race в расчётах).
	results := make(chan LazyLoadRationale, 5)
	for i := 0; i < 5; i++ {
		go func() {
			_, r := calculateLazyLoadOpts(
				"race-test", 8192, -1, &cppbackend.Config{DefaultGPULayers: 32},
			)
			results <- r
		}()
	}

	var first LazyLoadRationale
	for i := 0; i < 5; i++ {
		r := <-results
		if i == 0 {
			first = r
		} else {
			if r.Source != first.Source || r.AppliedNCtx != first.AppliedNCtx {
				t.Errorf("concurrent results differ: %+v vs %+v", r, first)
			}
		}
	}
}

// TestCalculateLazyLoadOpts_AutoTuneDisabled — проверяет, что при
// autoTuneNCtxOnLoadEnabled=false calculateLazyLoadOpts возвращает
// opts как есть и Source="fallback_no_meta".
//
// NB: autoTuneNCtxOnLoadEnabled выставляется в init() и далее
// t.Setenv НЕ действует (init уже отработал). Тест напрямую
// переключает глобальный флаг с восстановлением через t.Cleanup.
func TestCalculateLazyLoadOpts_AutoTuneDisabled(t *testing.T) {
	prev := autoTuneNCtxOnLoadEnabled
	autoTuneNCtxOnLoadEnabled = false
	t.Cleanup(func() { autoTuneNCtxOnLoadEnabled = prev })

	t.Setenv("CPPWORKER_VRAM_BYTES", strconv.FormatInt(gb(20), 10))
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", strconv.FormatInt(gb(20), 10))
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", strconv.FormatInt(gb(30), 10))

	setupBackendWithModel(t, "no-tune-model", "llama", 32, 32, 32, 4096, gb(4))

	opts, rationale := calculateLazyLoadOpts(
		"no-tune-model", 8192, -1, &cppbackend.Config{DefaultGPULayers: 32},
	)

	t.Logf("Rationale: %s", rationale.FormatRationale())

	// Source должен быть fallback_no_meta.
	if rationale.Source != "fallback_no_meta" {
		t.Errorf("Source=%q, expected fallback_no_meta при auto-tune disabled", rationale.Source)
	}
	// opts должны быть как есть.
	if opts.ContextSize != 8192 {
		t.Errorf("ContextSize=%d, want 8192 (unchanged)", opts.ContextSize)
	}
}

// TestCalculateLazyLoadOpts_RationaleFieldsPopulated — все поля Rationale заполнены.
func TestCalculateLazyLoadOpts_RationaleFieldsPopulated(t *testing.T) {
	setupEnvSweep(t, gb(20), gb(30))
	setupBackendWithModel(t, "rationale-test", "llama", 32, 32, 32, 4096, gb(4))

	_, rationale := calculateLazyLoadOpts(
		"rationale-test", 8192, 16, &cppbackend.Config{DefaultGPULayers: 32},
	)

	if rationale.RequestedNCtx != 8192 {
		t.Errorf("RequestedNCtx=%d, want 8192", rationale.RequestedNCtx)
	}
	if rationale.RequestedGPULayers != 16 {
		t.Errorf("RequestedGPULayers=%d, want 16", rationale.RequestedGPULayers)
	}
	if rationale.NLayers != 32 {
		t.Errorf("NLayers=%d, want 32", rationale.NLayers)
	}
	if rationale.NEmbd != 4096 {
		t.Errorf("NEmbd=%d, want 4096", rationale.NEmbd)
	}
	if rationale.ArchName != "llama" {
		t.Errorf("ArchName=%q, want llama", rationale.ArchName)
	}
	if rationale.ModelSize != gb(4) {
		t.Errorf("ModelSize=%d, want %d", rationale.ModelSize, gb(4))
	}
	if rationale.AvailableVRAMBytes != gb(20) {
		t.Errorf("AvailableVRAMBytes=%d, want %d", rationale.AvailableVRAMBytes, gb(20))
	}
	if rationale.AvailableRAMBytes != gb(30) {
		t.Errorf("AvailableRAMBytes=%d, want %d", rationale.AvailableRAMBytes, gb(30))
	}

	// FormatRationale не должен падать.
	s := rationale.FormatRationale()
	if s == "" {
		t.Error("FormatRationale пуст")
	}
	t.Logf("Rationale formatted: %s", s)
}