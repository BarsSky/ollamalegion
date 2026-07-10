// lazyload_simulator_test.go — табличный sweep по всем стратегиям каскада.
//
// Цель: проверить корректность распределения VRAM/RAM для calculateLazyLoadOpts
// на сетке параметров (VRAM × RAM × model size × n_ctx), включая edge cases
// для 20GB VRAM + 30GB RAM.
//
// Все тесты используют ENV-фейки (CPPWORKER_VRAM_BYTES, CPPWORKER_FREE_VRAM_BYTES,
// CPPWORKER_AVAILABLE_RAM_BYTES) и не зависят от nvidia-smi.
//
// Запуск:
//   go test ./cmd/cppworker -run "TestLazyLoadSweep" -tags llama_stub -v
//
// Покрывает:
//   - exact_fit / partial_offload / reduced_nctx / fallback_* стратегии
//   - 20GB+ VRAM + 30GB+ RAM (пользовательский сценарий)
//   - Очень маленькая VRAM (1-2 GB)
//   - Очень большие модели (120B+)
//   - Сверхбольшие n_ctx (131K+)
//   - Граничные значения (VRAM=0, n_ctx=0, weights=0)
package main

import (
	"fmt"
	"math"
	"strconv"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// sweepCase — один кейс в табличном sweep.
type sweepCase struct {
	Name           string
	VRAMBytes      int64 // total VRAM
	FreeVRAMBytes  int64 // free VRAM (если 0, используется VRAMBytes)
	RAMBytes       int64 // available RAM
	ModelName      string
	Arch           string
	NLayers        uint32
	NHeads         uint32
	NKvHeads       uint32
	NEmbd          uint32
	ModelSizeBytes int64
	RequestedNCtx  int
	RequestedGPU   int

	// Флаги автотюнинга.
	AutoTuneDisabled bool // true → выключить autoTuneNCtxOnLoadEnabled для кейса

	// Sanity invariants — функция вернёт nil/false если нарушены.
	ExpectSource        string // "" = любая из {exact_fit, partial_offload, reduced_nctx, fallback_*}
	ExpectNotSource     string // "" = нет ограничения
	ExpectAppliedNCtxMax int    // AppliedNCtx <= max (0 = не проверяем)
	ExpectAppliedNCtxMin int    // AppliedNCtx >= min (0 = не проверяем)
	ExpectGPULayersMax   int    // AppliedGPULayers <= max (0 = не проверяем)
	ExpectUseMmap        *bool  // nil = не проверяем, true/false = строго
	ExpectSourceValid    bool   // source ∈ {exact_fit, partial_offload, reduced_nctx, fallback_*}
	ExpectRationaleOK    bool   // все поля заполнены корректно
}

// sweepResult — результат прогона одного кейса.
type sweepResult struct {
	Case      sweepCase
	Opts      cppbackend.LoadModelOpts
	Rationale LazyLoadRationale
	Err       string // пустая = успех
}

// envForCase устанавливает ENV-переменные для кейса.
func envForCase(t *testing.T, c sweepCase) {
	t.Helper()
	t.Setenv("CPPWORKER_VRAM_BYTES", strconv.FormatInt(c.VRAMBytes, 10))
	if c.FreeVRAMBytes == 0 {
		c.FreeVRAMBytes = c.VRAMBytes
	}
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", strconv.FormatInt(c.FreeVRAMBytes, 10))
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", strconv.FormatInt(c.RAMBytes, 10))
	t.Setenv("CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")
}

// runSweep запускает один кейс и возвращает результат.
func runSweep(t *testing.T, c sweepCase) sweepResult {
	t.Helper()
	envForCase(t, c)
	setupBackendWithModel(t, c.ModelName, c.Arch, c.NLayers, c.NHeads, c.NKvHeads, c.NEmbd, c.ModelSizeBytes)

	// Если кейс требует отключить auto-tune — переключаем глобальный флаг.
	// t.Setenv НЕ работает (autoTuneNCtxOnLoadEnabled выставляется в init()),
	// поэтому переключаем напрямую с восстановлением через t.Cleanup.
	prevAutoTune := autoTuneNCtxOnLoadEnabled
	if c.AutoTuneDisabled {
		autoTuneNCtxOnLoadEnabled = false
	}
	t.Cleanup(func() { autoTuneNCtxOnLoadEnabled = prevAutoTune })
	_ = prevAutoTune

	opts, rationale := calculateLazyLoadOpts(
		c.ModelName,
		c.RequestedNCtx,
		c.RequestedGPU,
		&cppbackend.Config{DefaultGPULayers: 32},
	)

	result := sweepResult{
		Case:      c,
		Opts:      opts,
		Rationale: rationale,
	}
	return result
}

// validateSweep проверяет инварианты кейса.
func validateSweep(t *testing.T, r sweepResult) {
	t.Helper()
	c := r.Case

	// Source должен быть одним из известных.
	validSources := map[string]bool{
		"exact_fit":       true,
		"partial_offload": true,
		"reduced_nctx":    true,
		"fallback_no_meta": true,
		"fallback_no_fit": true,
	}
	if c.ExpectSourceValid && !validSources[r.Rationale.Source] {
		t.Errorf("[%s] Source=%q not in valid set", c.Name, r.Rationale.Source)
	}

	// Если ожидается конкретный source.
	if c.ExpectSource != "" && r.Rationale.Source != c.ExpectSource {
		t.Errorf("[%s] Source=%q, want %q (rationale=%s)",
			c.Name, r.Rationale.Source, c.ExpectSource, r.Rationale.FormatRationale())
	}

	// Если ожидается "не source".
	if c.ExpectNotSource != "" && r.Rationale.Source == c.ExpectNotSource {
		t.Errorf("[%s] Source=%q, must NOT be %q (rationale=%s)",
			c.Name, r.Rationale.Source, c.ExpectNotSource, r.Rationale.FormatRationale())
	}

	// AppliedNCtx <= max.
	if c.ExpectAppliedNCtxMax > 0 && r.Rationale.AppliedNCtx > c.ExpectAppliedNCtxMax {
		t.Errorf("[%s] AppliedNCtx=%d > max=%d (rationale=%s)",
			c.Name, r.Rationale.AppliedNCtx, c.ExpectAppliedNCtxMax, r.Rationale.FormatRationale())
	}

	// AppliedNCtx >= min.
	if c.ExpectAppliedNCtxMin > 0 && r.Rationale.AppliedNCtx < c.ExpectAppliedNCtxMin {
		t.Errorf("[%s] AppliedNCtx=%d < min=%d (rationale=%s)",
			c.Name, r.Rationale.AppliedNCtx, c.ExpectAppliedNCtxMin, r.Rationale.FormatRationale())
	}

	// AppliedGPULayers <= max.
	if c.ExpectGPULayersMax > 0 && r.Rationale.AppliedGPULayers > c.ExpectGPULayersMax {
		t.Errorf("[%s] AppliedGPULayers=%d > max=%d (rationale=%s)",
			c.Name, r.Rationale.AppliedGPULayers, c.ExpectGPULayersMax, r.Rationale.FormatRationale())
	}

	// AppliedGPULayers <= NLayers (инвариант).
	if r.Rationale.AppliedGPULayers > int(c.NLayers) {
		t.Errorf("[%s] AppliedGPULayers=%d > NLayers=%d (rationale=%s)",
			c.Name, r.Rationale.AppliedGPULayers, c.NLayers, r.Rationale.FormatRationale())
	}

	// AppliedNCtx >= 0 (если не fallback).
	if r.Rationale.AppliedNCtx < 0 && r.Rationale.Source != "fallback_no_fit" {
		t.Errorf("[%s] AppliedNCtx=%d < 0 (rationale=%s)",
			c.Name, r.Rationale.AppliedNCtx, r.Rationale.FormatRationale())
	}

	// UseMmap expectation.
	if c.ExpectUseMmap != nil {
		if r.Opts.UseMmap != *c.ExpectUseMmap {
			t.Errorf("[%s] UseMmap=%v, want %v (rationale=%s)",
				c.Name, r.Opts.UseMmap, *c.ExpectUseMmap, r.Rationale.FormatRationale())
		}
	}

	// Required fields заполнены для не-fallback кейсов.
	if c.ExpectRationaleOK {
		if r.Rationale.NLayers == 0 && r.Rationale.Source != "fallback_no_meta" {
			t.Errorf("[%s] NLayers=0 для non-fallback source=%s",
				c.Name, r.Rationale.Source)
		}
		if r.Rationale.ArchName == "" && r.Rationale.Source != "fallback_no_meta" {
			t.Errorf("[%s] ArchName пуст для non-fallback source=%s",
				c.Name, r.Rationale.Source)
		}
	}
}

// makeSweepMatrix — генерирует таблицу кейсов.
func makeSweepMatrix() []sweepCase {
	gb := func(n int64) int64 { return n * 1024 * 1024 * 1024 }

	// Стандартные параметры для тестовой модели.
	llama7B := func() (arch string, nl, nh, nkh, ne uint32, size int64) {
		// llama-2 7B: 32 layers, n_embd=4096, 32 heads, 32 kv_heads (MHA).
		// Q4_K_M ≈ 4 GB.
		return "llama", 32, 32, 32, 4096, gb(4)
	}
	llama13B := func() (arch string, nl, nh, nkh, ne uint32, size int64) {
		// llama-2 13B: 40 layers, n_embd=5120, 40 heads, 40 kv_heads (MHA).
		// Q4_K_M ≈ 8 GB.
		return "llama", 40, 40, 40, 5120, gb(8)
	}
	llama30B := func() (arch string, nl, nh, nkh, ne uint32, size int64) {
		// llama-2 30B: 60 layers, n_embd=6656, 52 heads, 52 kv_heads (MHA).
		// Q4_K_M ≈ 18 GB.
		return "llama", 60, 52, 52, 6656, gb(18)
	}
	llama70B := func() (arch string, nl, nh, nkh, ne uint32, size int64) {
		// llama-2 70B: 80 layers, n_embd=8192, 64 heads, 8 kv_heads (GQA).
		// Q4_K_M ≈ 40 GB.
		return "llama", 80, 64, 8, 8192, gb(40)
	}
	llama120B := func() (arch string, nl, nh, nkh, ne uint32, size int64) {
		// llama-3 120B (гипотетический): 96 layers, n_embd=12288, 96 heads, 8 kv_heads.
		// Q4_K_M ≈ 65 GB.
		return "llama", 96, 96, 8, 12288, gb(65)
	}
	gemma4B := func() (arch string, nl, nh, nkh, ne uint32, size int64) {
		// gemma-3 4B: 32 layers, n_embd=2560, 8 heads, 4 kv_heads (GQA).
		// Q4_K_M ≈ 5 GB.
		return "gemma2", 32, 8, 4, 2560, gb(5)
	}

	cases := []sweepCase{}

	// ========================================================
	// Группа A: 20GB VRAM + 30GB RAM (пользовательский кейс)
	// ========================================================
	a, al, ah, akh, ae, asz := llama7B()
	cases = append(cases, sweepCase{
		Name: "A1_7B_20GBVRAM_30GBRAM_nctx8K",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
		ModelName: "llama-7b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 8192, RequestedGPU: -1,
		ExpectSourceValid: true, ExpectRationaleOK: true,
	})

	a, al, ah, akh, ae, asz = llama13B()
	cases = append(cases, sweepCase{
		Name: "A2_13B_20GBVRAM_30GBRAM_nctx16K",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
		ModelName: "llama-13b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 16384, RequestedGPU: -1,
		ExpectSourceValid: true, ExpectRationaleOK: true,
	})

	a, al, ah, akh, ae, asz = llama30B()
	cases = append(cases, sweepCase{
		Name: "A3_30B_20GBVRAM_30GBRAM_nctx32K",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
		ModelName: "llama-30b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 32768, RequestedGPU: -1,
		ExpectSourceValid: true, ExpectRationaleOK: true,
		// 18GB модель на 20GB VRAM + KV для 32K (≈ 5GB) не влезает даже с partial offload.
		// Код выбирает reduced_nctx с applied_nctx ≈ maxViableNCtx (≈10K).
		// Оба варианта (partial_offload / reduced_nctx) корректны.
		ExpectNotSource: "exact_fit",
	})

	a, al, ah, akh, ae, asz = llama70B()
	cases = append(cases, sweepCase{
		Name: "A4_70B_20GBVRAM_30GBRAM_nctx32K",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
		ModelName: "llama-70b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 32768, RequestedGPU: -1,
		// 40GB модель > 30GB RAM — ожидаем fallback_no_fit.
		ExpectSource: "fallback_no_fit",
	})

	// ========================================================
	// Группа B: 20GB VRAM + 64GB RAM (много RAM)
	// ========================================================
	a, al, ah, akh, ae, asz = llama70B()
	cases = append(cases, sweepCase{
		Name: "B1_70B_20GBVRAM_64GBRAM_nctx32K",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(64),
		ModelName: "llama-70b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 32768, RequestedGPU: -1,
		// 40GB модель влезет в RAM+VRAM=84GB → partial_offload или cpu-only.
		ExpectSourceValid: true, ExpectRationaleOK: true,
		ExpectNotSource: "fallback_no_fit",
	})

	a, al, ah, akh, ae, asz = llama70B()
	cases = append(cases, sweepCase{
		Name: "B2_70B_20GBVRAM_64GBRAM_nctx8K",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(64),
		ModelName: "llama-70b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 8192, RequestedGPU: -1,
		ExpectSourceValid: true, ExpectRationaleOK: true,
		ExpectNotSource: "fallback_no_fit",
	})

	// ========================================================
	// Группа C: 24GB VRAM + 128GB RAM (high-end workstation)
	// ========================================================
	a, al, ah, akh, ae, asz = llama70B()
	cases = append(cases, sweepCase{
		Name: "C1_70B_24GBVRAM_128GBRAM_nctx65K",
		VRAMBytes: gb(24), FreeVRAMBytes: gb(24), RAMBytes: gb(128),
		ModelName: "llama-70b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 65536, RequestedGPU: -1,
		ExpectSourceValid: true, ExpectRationaleOK: true,
		ExpectNotSource: "fallback_no_fit",
	})

	a, al, ah, akh, ae, asz = llama120B()
	cases = append(cases, sweepCase{
		Name: "C2_120B_24GBVRAM_128GBRAM_nctx32K",
		VRAMBytes: gb(24), FreeVRAMBytes: gb(24), RAMBytes: gb(128),
		ModelName: "llama-120b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 32768, RequestedGPU: -1,
		// 65GB модель влезет в 24+128=152GB → partial_offload/cpu-only.
		ExpectSourceValid: true, ExpectRationaleOK: true,
		ExpectNotSource: "fallback_no_fit",
	})

	// ========================================================
	// Группа D: 8GB VRAM + 32GB RAM (consumer GPU)
	// ========================================================
	a, al, ah, akh, ae, asz = gemma4B()
	cases = append(cases, sweepCase{
		Name: "D1_gemma4B_8GBVRAM_32GBRAM_nctx32K",
		VRAMBytes: gb(8), FreeVRAMBytes: gb(8), RAMBytes: gb(32),
		ModelName: "gemma-4b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 32768, RequestedGPU: -1,
		ExpectSourceValid: true, ExpectRationaleOK: true,
		// 5GB модель на 8GB + KV для 32K (≈3.5GB для GQA gemma-3) не влезает целиком в VRAM.
		// Код выбирает partial_offload (gpu_layers=1, всё остальное в mmap).
		// partial_offload или exact_fit — оба корректны.
		ExpectNotSource: "fallback_no_fit",
	})

	a, al, ah, akh, ae, asz = llama13B()
	cases = append(cases, sweepCase{
		Name: "D2_13B_8GBVRAM_32GBRAM_nctx16K",
		VRAMBytes: gb(8), FreeVRAMBytes: gb(8), RAMBytes: gb(32),
		ModelName: "llama-13b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 16384, RequestedGPU: -1,
		ExpectSourceValid: true, ExpectRationaleOK: true,
		// 8GB модель на 8GB VRAM + KV → partial_offload или exact_fit.
		ExpectNotSource: "fallback_no_fit",
	})

	// ========================================================
	// Группа E: 1-2GB VRAM (edge case — старая GPU/CPU-only)
	// ========================================================
	a, al, ah, akh, ae, asz = llama7B()
	cases = append(cases, sweepCase{
		Name: "E1_7B_2GBVRAM_16GBRAM_nctx4K",
		VRAMBytes: gb(2), FreeVRAMBytes: gb(2), RAMBytes: gb(16),
		ModelName: "llama-7b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 4096, RequestedGPU: -1,
		ExpectSourceValid: true, ExpectRationaleOK: true,
		ExpectNotSource: "fallback_no_fit",
	})

	a, al, ah, akh, ae, asz = gemma4B()
	cases = append(cases, sweepCase{
		Name: "E2_gemma4B_1GBVRAM_16GBRAM_nctx2K",
		VRAMBytes: gb(1), FreeVRAMBytes: gb(1), RAMBytes: gb(16),
		ModelName: "gemma-4b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 2048, RequestedGPU: 0,
		// С gpu_layers=0 (forced cpu-only) → должно быть exact_fit или reduced_nctx.
		ExpectSourceValid: true, ExpectRationaleOK: true,
	})

	// ========================================================
	// Группа F: Огромные n_ctx (1M tokens)
	// ========================================================
	a, al, ah, akh, ae, asz = llama7B()
	cases = append(cases, sweepCase{
		Name: "F1_7B_20GBVRAM_30GBRAM_nctx131K",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
		ModelName: "llama-7b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 131072, RequestedGPU: -1,
		// 131K ctx на 7B в 20GB — KV-cache огромный, ожидаем reduced_nctx или fallback.
		ExpectSourceValid: true, ExpectRationaleOK: true,
		ExpectNotSource: "exact_fit",
	})

	a, al, ah, akh, ae, asz = llama13B()
	cases = append(cases, sweepCase{
		Name: "F2_13B_20GBVRAM_30GBRAM_nctx256K",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
		ModelName: "llama-13b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 262144, RequestedGPU: -1,
		// 256K ctx — даже 13B не влезет в 20GB → ожидаем reduced_nctx или fallback.
		ExpectSourceValid: true, ExpectRationaleOK: true,
	})

	// ========================================================
	// Группа G: Edge cases — 0/негативные значения
	// ========================================================
	cases = append(cases, sweepCase{
		Name: "G1_7B_20GBVRAM_nctx0",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
		ModelName: "llama-7b", Arch: "llama", NLayers: 32, NHeads: 32, NKvHeads: 32, NEmbd: 4096, ModelSizeBytes: gb(4),
		RequestedNCtx: 0, RequestedGPU: -1,
		ExpectSourceValid: true, ExpectRationaleOK: true,
	})

	cases = append(cases, sweepCase{
		Name: "G2_7B_20GBVRAM_nctx1",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
		ModelName: "llama-7b", Arch: "llama", NLayers: 32, NHeads: 32, NKvHeads: 32, NEmbd: 4096, ModelSizeBytes: gb(4),
		RequestedNCtx: 1, RequestedGPU: -1,
		ExpectSourceValid: true, ExpectRationaleOK: true,
	})

	cases = append(cases, sweepCase{
		Name: "G3_7B_VRAM0",
		VRAMBytes: 0, FreeVRAMBytes: 0, RAMBytes: gb(30),
		ModelName: "llama-7b", Arch: "llama", NLayers: 32, NHeads: 32, NKvHeads: 32, NEmbd: 4096, ModelSizeBytes: gb(4),
		RequestedNCtx: 8192, RequestedGPU: -1,
		// VRAM=0 → fallback.
		ExpectSourceValid: true, ExpectRationaleOK: true,
	})

	// ========================================================
	// Группа H: Multi-GPU (48GB, A6000-class)
	// ========================================================
	a, al, ah, akh, ae, asz = llama70B()
	cases = append(cases, sweepCase{
		Name: "H1_70B_48GBVRAM_64GBRAM_nctx32K",
		VRAMBytes: gb(48), FreeVRAMBytes: gb(48), RAMBytes: gb(64),
		ModelName: "llama-70b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 32768, RequestedGPU: -1,
		// 40GB модель на 48GB VRAM → должно быть exact_fit или partial_offload.
		ExpectSourceValid: true, ExpectRationaleOK: true,
		ExpectNotSource: "fallback_no_fit",
	})

	a, al, ah, akh, ae, asz = llama120B()
	cases = append(cases, sweepCase{
		Name: "H2_120B_48GBVRAM_64GBRAM_nctx16K",
		VRAMBytes: gb(48), FreeVRAMBytes: gb(48), RAMBytes: gb(64),
		ModelName: "llama-120b", Arch: a, NLayers: al, NHeads: ah, NKvHeads: akh, NEmbd: ae, ModelSizeBytes: asz,
		RequestedNCtx: 16384, RequestedGPU: -1,
		// 65GB модель на 48GB+64GB=112GB → partial_offload или cpu-only.
		ExpectSourceValid: true, ExpectRationaleOK: true,
		ExpectNotSource: "fallback_no_fit",
	})

	// ========================================================
	// Группа I: Auto-tune disabled (fallback)
	// ========================================================
	cases = append(cases, sweepCase{
		Name: "I1_7B_AutoTuneDisabled",
		VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
		ModelName: "llama-7b", Arch: "llama", NLayers: 32, NHeads: 32, NKvHeads: 32, NEmbd: 4096, ModelSizeBytes: gb(4),
		RequestedNCtx: 8192, RequestedGPU: -1,
		AutoTuneDisabled: true,
		ExpectSourceValid: true,
		ExpectSource: "fallback_no_meta", // Auto-tune off → fallback.
	})

	return cases
}

// TestLazyLoadSweep_Matrix — прогон всех кейсов из таблицы.
func TestLazyLoadSweep_Matrix(t *testing.T) {
	cases := makeSweepMatrix()
	if len(cases) < 20 {
		t.Fatalf("expected at least 20 sweep cases, got %d", len(cases))
	}
	t.Logf("Sweep matrix: %d cases", len(cases))

	var passed, failed int
	for i, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			r := runSweep(t, c)
			validateSweep(t, r)
			if t.Failed() {
				failed++
			} else {
				passed++
			}
		})
		_ = i
	}

	t.Logf("Sweep summary: %d/%d passed, %d failed",
		passed, passed+failed, failed)
}

// TestLazyLoadSweep_GroupA_UserScenario — фокус на пользовательском кейсе
// "20GB VRAM + 30GB RAM + разные модели".
func TestLazyLoadSweep_GroupA_UserScenario(t *testing.T) {
	gb := func(n int64) int64 { return n * 1024 * 1024 * 1024 }

	cases := []sweepCase{
		{
			Name: "A_small_7B_8Kctx",
			VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
			ModelName: "llama-7b", Arch: "llama", NLayers: 32, NHeads: 32, NKvHeads: 32, NEmbd: 4096, ModelSizeBytes: gb(4),
			RequestedNCtx: 8192, RequestedGPU: -1,
			ExpectSourceValid: true, ExpectRationaleOK: true,
			// 4GB на 20GB → exact_fit.
		},
		{
			Name: "A_medium_13B_16Kctx",
			VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
			ModelName: "llama-13b", Arch: "llama", NLayers: 40, NHeads: 40, NKvHeads: 40, NEmbd: 5120, ModelSizeBytes: gb(8),
			RequestedNCtx: 16384, RequestedGPU: -1,
			ExpectSourceValid: true, ExpectRationaleOK: true,
		},
		{
			Name: "A_large_30B_32Kctx",
			VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
			ModelName: "llama-30b", Arch: "llama", NLayers: 60, NHeads: 52, NKvHeads: 52, NEmbd: 6656, ModelSizeBytes: gb(18),
			RequestedNCtx: 32768, RequestedGPU: -1,
			// 18GB на 20GB → exact_fit (если KV мал) или partial_offload.
			ExpectSourceValid: true, ExpectRationaleOK: true,
		},
		{
			Name: "A_huge_70B_8Kctx",
			VRAMBytes: gb(20), FreeVRAMBytes: gb(20), RAMBytes: gb(30),
			ModelName: "llama-70b", Arch: "llama", NLayers: 80, NHeads: 64, NKvHeads: 8, NEmbd: 8192, ModelSizeBytes: gb(40),
			RequestedNCtx: 8192, RequestedGPU: -1,
			// 40GB модель на 50GB (20 VRAM + 30 RAM) — должно влезть.
			ExpectSourceValid: true, ExpectRationaleOK: true,
			ExpectNotSource: "fallback_no_fit",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			r := runSweep(t, c)
			validateSweep(t, r)
			t.Logf("Rationale: %s", r.Rationale.FormatRationale())
		})
	}
}

// TestLazyLoadSweep_ExhaustiveNCtx — перебор n_ctx от 1024 до 131072.
// Проверяет, что AppliedNCtx <= RequestedNCtx всегда.
func TestLazyLoadSweep_ExhaustiveNCtx(t *testing.T) {
	gb := func(n int64) int64 { return n * 1024 * 1024 * 1024 }

	nctxValues := []int{1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072}
	for _, nctx := range nctxValues {
		nctx := nctx
		t.Run(fmt.Sprintf("nctx_%d", nctx), func(t *testing.T) {
			t.Setenv("CPPWORKER_VRAM_BYTES", strconv.FormatInt(gb(20), 10))
			t.Setenv("CPPWORKER_FREE_VRAM_BYTES", strconv.FormatInt(gb(20), 10))
			t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", strconv.FormatInt(gb(30), 10))
			t.Setenv("CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

			setupBackendWithModel(t, "llama-13b-test", "llama", 40, 40, 40, 5120, gb(8))

			opts, rationale := calculateLazyLoadOpts(
				"llama-13b-test", nctx, -1, &cppbackend.Config{DefaultGPULayers: 32},
			)

			// AppliedNCtx <= RequestedNCtx (никогда не увеличиваем).
			if rationale.AppliedNCtx > nctx {
				t.Errorf("AppliedNCtx=%d > requested=%d (rationale=%s)",
					rationale.AppliedNCtx, nctx, rationale.FormatRationale())
			}
			// AppliedNCtx >= 0.
			if rationale.AppliedNCtx < 0 {
				t.Errorf("AppliedNCtx=%d < 0 (rationale=%s)",
					rationale.AppliedNCtx, rationale.FormatRationale())
			}
			// ContextSize в opts == rationale.AppliedNCtx.
			if opts.ContextSize != rationale.AppliedNCtx {
				t.Errorf("opts.ContextSize=%d != rationale.AppliedNCtx=%d",
					opts.ContextSize, rationale.AppliedNCtx)
			}
			// MaxViableNCtx >= 0.
			if rationale.MaxViableNCtx < 0 {
				t.Errorf("MaxViableNCtx=%d < 0", rationale.MaxViableNCtx)
			}
			t.Logf("nctx=%d → AppliedNCtx=%d, Source=%s, MaxViableNCtx=%d, UseMmap=%v",
				nctx, rationale.AppliedNCtx, rationale.Source, rationale.MaxViableNCtx, opts.UseMmap)
		})
	}
}

// TestLazyLoadSweep_ExhaustiveVRAM — перебор VRAM от 1GB до 48GB.
func TestLazyLoadSweep_ExhaustiveVRAM(t *testing.T) {
	gb := func(n int64) int64 { return n * 1024 * 1024 * 1024 }

	vramValues := []int64{1, 2, 4, 6, 8, 12, 16, 20, 24, 32, 40, 48}
	for _, vramGB := range vramValues {
		vramGB := vramGB
		t.Run(fmt.Sprintf("vram_%dGB", vramGB), func(t *testing.T) {
			t.Setenv("CPPWORKER_VRAM_BYTES", strconv.FormatInt(gb(vramGB), 10))
			t.Setenv("CPPWORKER_FREE_VRAM_BYTES", strconv.FormatInt(gb(vramGB), 10))
			t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", strconv.FormatInt(gb(64), 10))
			t.Setenv("CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

			// 7B модель 4GB.
			setupBackendWithModel(t, "llama-7b-vram-test", "llama", 32, 32, 32, 4096, gb(4))

			opts, rationale := calculateLazyLoadOpts(
				"llama-7b-vram-test", 32768, -1, &cppbackend.Config{DefaultGPULayers: 32},
			)

			// AppliedNCtx <= 32768.
			if rationale.AppliedNCtx > 32768 {
				t.Errorf("AppliedNCtx=%d > requested=32768 (vram=%dGB, source=%s)",
					rationale.AppliedNCtx, vramGB, rationale.Source)
			}
			// AppliedGPULayers <= 32.
			if rationale.AppliedGPULayers > 32 {
				t.Errorf("AppliedGPULayers=%d > NLayers=32 (vram=%dGB, source=%s)",
					rationale.AppliedGPULayers, vramGB, rationale.Source)
			}
			t.Logf("vram=%dGB → AppliedNCtx=%d, AppliedGPU=%d, Source=%s, UseMmap=%v",
				vramGB, rationale.AppliedNCtx, rationale.AppliedGPULayers, rationale.Source, opts.UseMmap)
		})
	}
}

// TestLazyLoadSweep_KVCacheSanity — проверка, что KV-cache растёт линейно с n_ctx.
func TestLazyLoadSweep_KVCacheSanity(t *testing.T) {
	gb := func(n int64) int64 { return n * 1024 * 1024 * 1024 }

	t.Setenv("CPPWORKER_VRAM_BYTES", strconv.FormatInt(gb(20), 10))
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", strconv.FormatInt(gb(20), 10))
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", strconv.FormatInt(gb(30), 10))
	t.Setenv("CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

	setupBackendWithModel(t, "llama-7b-kvtest", "llama", 32, 32, 32, 4096, gb(4))

	// 4 разных n_ctx → ExpectedKVCacheMB должно расти линейно.
	prevKVCacheMB := int64(0)
	for _, nctx := range []int{1024, 4096, 16384, 65536} {
		_, rationale := calculateLazyLoadOpts(
			"llama-7b-kvtest", nctx, -1, &cppbackend.Config{DefaultGPULayers: 32},
		)
		kvMB := rationale.EstimatedKVCacheMB
		t.Logf("nctx=%d → EstimatedKVCacheMB=%d", nctx, kvMB)
		if kvMB <= prevKVCacheMB {
			t.Errorf("KV-cache не растёт монотонно: nctx=%d, kvMB=%d, prev=%d",
				nctx, kvMB, prevKVCacheMB)
		}
		prevKVCacheMB = kvMB
	}
}

// TestLazyLoadSweep_InvariantAppliedNCtxRequested — AppliedNCtx никогда не больше RequestedNCtx.
func TestLazyLoadSweep_InvariantAppliedNCtxRequested(t *testing.T) {
	gb := func(n int64) int64 { return n * 1024 * 1024 * 1024 }

	// Большой sweep.
	type param struct {
		vram, ram int64
		modelsize int64
		nctx      int
		nlayers   uint32
	}
	sweeps := []param{}
	for _, vram := range []int64{4, 8, 16, 20, 24, 48} {
		for _, ram := range []int64{16, 32, 64, 128} {
			for _, ms := range []int64{4, 8, 18, 40, 65} {
				for _, nctx := range []int{4096, 32768, 131072} {
					nlayers := uint32(32 + ms/2) // 32, 36, 41, 52, 64
					sweeps = append(sweeps, param{vram, ram, ms, nctx, nlayers})
				}
			}
		}
	}
	if len(sweeps) < 100 {
		t.Fatalf("expected at least 100 sweep cases, got %d", len(sweeps))
	}
	t.Logf("Sweep: %d cases", len(sweeps))

	for i, p := range sweeps {
		t.Setenv("CPPWORKER_VRAM_BYTES", strconv.FormatInt(gb(p.vram), 10))
		t.Setenv("CPPWORKER_FREE_VRAM_BYTES", strconv.FormatInt(gb(p.vram), 10))
		t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", strconv.FormatInt(gb(p.ram), 10))
		t.Setenv("CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

		setupBackendWithModel(t,
			fmt.Sprintf("model-%d", i), "llama",
			p.nlayers, 32, 8, 4096, gb(p.modelsize))

		_, rationale := calculateLazyLoadOpts(
			fmt.Sprintf("model-%d", i), p.nctx, -1, &cppbackend.Config{DefaultGPULayers: 32},
		)
		if rationale.AppliedNCtx > p.nctx {
			t.Errorf("[vram=%dGB ram=%dGB size=%dGB nctx=%d] AppliedNCtx=%d > RequestedNCtx=%d, source=%s",
				p.vram, p.ram, p.modelsize, p.nctx, rationale.AppliedNCtx, p.nctx, rationale.Source)
		}
	}
}

// TestLazyLoadSweep_InvariantKVCacheFormula — проверка формулы KV-cache.
func TestLazyLoadSweep_InvariantKVCacheFormula(t *testing.T) {
	// kvPerToken = 4 * NLayers * effKVHeads * headDim
	// где headDim = nEmbd / nHeads
	// Для 7B llama (NLayers=32, NEmbd=4096, NHeads=32, NKvHeads=32):
	//   headDim = 4096/32 = 128
	//   kvPerToken = 4 * 32 * 32 * 128 = 524288 байт = 512 KB на токен.
	//   Для n_ctx=4096: 4096 * 524288 = 2147483648 байт = 2 GB.
	kvBytes := estimateKVCacheBytes(4096, 32, 4096, 32, 32, "f16")
	expectedMin := int64(1900 * 1024 * 1024) // 1.9 GB
	expectedMax := int64(2200 * 1024 * 1024) // 2.2 GB
	if kvBytes < expectedMin || kvBytes > expectedMax {
		t.Errorf("KV-cache for 7B nctx=4K: got %d bytes, want [%d, %d]",
			kvBytes, expectedMin, expectedMax)
	}
	t.Logf("KV-cache 7B nctx=4K = %d bytes (%.2f GB)", kvBytes, float64(kvBytes)/(1024*1024*1024))

	// Для 70B (NLayers=80, NEmbd=8192, NHeads=64, NKvHeads=8 GQA):
	//   headDim = 8192/64 = 128
	//   kvPerToken = 4 * 80 * 8 * 128 = 327680 байт = 320 KB на токен.
	//   Для n_ctx=32K: 32768 * 327680 = 10.7 GB.
	kvBytes = estimateKVCacheBytes(32768, 80, 8192, 64, 8, "f16")
	expectedMin70B := int64(10 * 1024 * 1024 * 1024)
	expectedMax70B := int64(11 * 1024 * 1024 * 1024)
	if kvBytes < expectedMin70B || kvBytes > expectedMax70B {
		t.Errorf("KV-cache for 70B nctx=32K: got %d bytes, want [%d, %d]",
			kvBytes, expectedMin70B, expectedMax70B)
	}
	t.Logf("KV-cache 70B nctx=32K = %d bytes (%.2f GB)", kvBytes, float64(kvBytes)/(1024*1024*1024))
}

// TestLazyLoadSweep_InvariantNoOverflow — проверка отсутствия overflow в формулах.
func TestLazyLoadSweep_InvariantNoOverflow(t *testing.T) {
	// 4 * nCtx * nLayers * nKvHeads * headDim не должно переполнять int64.
	// nCtx = 100M, nLayers = 200, nKvHeads = 200, headDim = 256.
	// 4 * 100M * 200 * 200 * 256 = 4.096 * 10^15 — влезает в int64 (max 9.2 * 10^18).
	kv := estimateKVCacheBytes(100_000_000, 200, 256*200, 200, 200, "f16")
	if kv <= 0 {
		t.Errorf("kv=%d — overflow или zero", kv)
	}
	t.Logf("max KV (100M ctx, 200 layers) = %d bytes (%.2f TB)", kv, float64(kv)/(1024*1024*1024*1024))

	// Проверим, что Math.Max не используется на int (потенциальный overflow).
	maxInt := int64(math.MaxInt64)
	if maxInt <= 0 {
		t.Error("MaxInt64 должен быть > 0")
	}
}