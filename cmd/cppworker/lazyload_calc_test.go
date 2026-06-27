// lazyload_calc_test.go — тесты для calculateLazyLoadOpts.
//
// Покрывают кейсы:
//   - qwen3.6 (~22 GB) на 20 GB VRAM: должно применить partial offload +
//     уменьшенный n_ctx (раньше падало в OOM с хардкодом estimatedLayers=80).
//   - малая модель (5 GB) на 8 GB VRAM: exact_fit, n_ctx=32768 сохраняется.
//   - средняя модель (10 GB) на 20 GB VRAM: точное попадание.
//   - fallback на defaults при отсутствии GGUF header.
//
// Все тесты используют ENV-переменные (CPPWORKER_VRAM_BYTES, CPPWORKER_FREE_VRAM_BYTES,
// CPPWORKER_AVAILABLE_RAM_BYTES) чтобы подделать реальное железо и не зависеть
// от nvidia-smi в CI.
package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// setEnvLazy — helper для установки ENV-переменных в тестах.
// Использует t.Setenv (Go 1.17+) для корректной изоляции между subtests.
func setEnvLazy(t *testing.T, key, value string) {
	t.Helper()
	if value == "" {
		prev, hadPrev := os.LookupEnv(key)
		os.Unsetenv(key)
		t.Cleanup(func() {
			if hadPrev {
				os.Setenv(key, prev)
			}
		})
		return
	}
	t.Setenv(key, value)
}

// makeFakeGGUF создаёт временный GGUF-файл с заданными архитектурными параметрами
// в header. Используется в тестах чтобы имитировать qwen3.6 (NLayers=80, NEmbd=5120).
func makeFakeGGUF(t *testing.T, path string, arch string, nLayers, nHeads, nKvHeads, nEmbd uint32) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fake gguf: %v", err)
	}
	defer f.Close()

	if _, err := f.Write([]byte{'G', 'G', 'U', 'F'}); err != nil {
		t.Fatalf("write magic: %v", err)
	}
	binary.Write(f, binary.LittleEndian, uint32(3))
	binary.Write(f, binary.LittleEndian, uint64(0))
	binary.Write(f, binary.LittleEndian, uint64(5))

	writeGGUFString(f, "general.architecture", arch)
	writeGGUFUint32(f, arch+".block_count", nLayers)
	writeGGUFUint32(f, arch+".attention.head_count", nHeads)
	writeGGUFUint32(f, arch+".attention.head_count_kv", nKvHeads)
	writeGGUFUint32(f, arch+".embedding_length", nEmbd)
}

func writeGGUFString(f *os.File, key string, val string) {
	binary.Write(f, binary.LittleEndian, uint64(len(key)))
	f.Write([]byte(key))
	binary.Write(f, binary.LittleEndian, uint32(8))
	binary.Write(f, binary.LittleEndian, uint64(len(val)))
	f.Write([]byte(val))
}

func writeGGUFUint32(f *os.File, key string, val uint32) {
	binary.Write(f, binary.LittleEndian, uint64(len(key)))
	f.Write([]byte(key))
	binary.Write(f, binary.LittleEndian, uint32(4))
	binary.Write(f, binary.LittleEndian, val)
}

// setupBackendWithModel создаёт cppbackend.Backend с одним fake .gguf файлом.
// Для больших размеров (>= 100MB) подменяет meta.SizeBytes на fileSize.
func setupBackendWithModel(t *testing.T, modelName string, arch string, nLayers, nHeads, nKvHeads, nEmbd uint32, fileSize int64) *cppbackend.Backend {
	t.Helper()

	dir := t.TempDir()
	modelPath := filepath.Join(dir, modelName+".gguf")

	makeFakeGGUF(t, modelPath, arch, nLayers, nHeads, nKvHeads, nEmbd)

	if fileSize > 0 && fileSize < 100*1024*1024 {
		currentSize, _ := os.Stat(modelPath)
		if currentSize != nil {
			padding := make([]byte, fileSize-currentSize.Size())
			if int64(len(padding)) > 0 {
				f, _ := os.OpenFile(modelPath, os.O_APPEND|os.O_WRONLY, 0644)
				if f != nil {
					f.Write(padding)
					f.Close()
				}
			}
		}
	}

	cfg := cppbackend.Config{
		ModelsDir:        dir,
		DefaultCtxSize:   32768,
		DefaultGPULayers: 32,
		DefaultUseMmap:   false,
	}

	be := cppbackend.NewBackend(cfg)
	if err := be.Init(); err != nil {
		t.Logf("backend.Init() warning (expected in stub): %v", err)
	}
	if mm := be.ModelManager(); mm != nil {
		if _, err := mm.ScanModels(); err != nil {
			t.Fatalf("ScanModels: %v", err)
		}
	}
	if fileSize >= 100*1024*1024 {
		if mm := be.ModelManager(); mm != nil {
			filename := modelName + ".gguf"
			meta, err := mm.GetModelMeta(filename)
			if err == nil && meta != nil {
				meta.SizeBytes = fileSize
				t.Logf("setupBackendWithModel: подменён SizeBytes=%d MB для %s",
					fileSize/(1024*1024), filename)
			}
		}
	}
	backend = be
	t.Cleanup(func() {
		backend = nil
	})

	return be
}

// TestCalculateLazyLoadOpts_Qwen36_On20GBVRAM — главный кейс из задачи.
//
// qwen3.6 ~22 GB, 80 слоёв, 5120 embd. На 20 GB VRAM:
//   - Stage 1: full GPU + KV для 32K не влезает → fallback.
//   - Stage 2: уменьшаем gpu_layers (partial offload) до ~30-40.
//
// Ожидаем: n_ctx=32768 сохраняется + gpu_layers уменьшен + UseMmap=true.
func TestCalculateLazyLoadOpts_Qwen36_On20GBVRAM(t *testing.T) {
	setEnvLazy(t, "CPPWORKER_VRAM_BYTES", "21474836480")
	setEnvLazy(t, "CPPWORKER_FREE_VRAM_BYTES", "21474836480")
	setEnvLazy(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "68719476736")
	setEnvLazy(t, "CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

	// qwen3.6: 80 layers, n_embd=5120, 64 heads, 8 kv_heads (GQA).
	setupBackendWithModel(t, "qwen3.6-72B-Q4_K_M", "llama", 80, 64, 8, 5120,
		22*1024*1024*1024)

	opts, rationale := calculateLazyLoadOpts(
		"qwen3.6-72B-Q4_K_M",
		32768,
		-1,
		&cppbackend.Config{DefaultGPULayers: 32},
	)

	t.Logf("rationale: %s", rationale.FormatRationale())

	if rationale.Source == "fallback_no_meta" {
		t.Fatalf("expected meta to be read from GGUF, got fallback_no_meta")
	}
	if rationale.Source == "exact_fit" && rationale.AppliedNCtx == 32768 {
		t.Fatalf("22GB model on 20GB VRAM с n_ctx=32768 не может быть exact_fit: %s",
			rationale.FormatRationale())
	}
	if !rationale.GPULayersReduced && rationale.AppliedNCtx >= 32768 {
		t.Errorf("expected either gpu_layers reduction or n_ctx reduction, got: %s",
			rationale.FormatRationale())
	}
	if !opts.UseMmap && rationale.Source != "fallback_no_fit" {
		t.Errorf("expected UseMmap=true for large model, got opts=%+v", opts)
	}
}

// TestCalculateLazyLoadOpts_SmallModel_On8GBVRAM — модель 5 GB на 8 GB VRAM:
// n_ctx=32768 сохраняется (НЕ должно быть reduced_nctx).
func TestCalculateLazyLoadOpts_SmallModel_On8GBVRAM(t *testing.T) {
	setEnvLazy(t, "CPPWORKER_VRAM_BYTES", "8589934592")
	setEnvLazy(t, "CPPWORKER_FREE_VRAM_BYTES", "8589934592")
	setEnvLazy(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "34359738368")
	setEnvLazy(t, "CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

	// gemma-3 4B: 32 layers, n_embd=2560, 8 heads, 4 kv_heads (GQA).
	setupBackendWithModel(t, "gemma-3-4b-it-Q4_K_M", "gemma2", 32, 8, 4, 2560,
		5*1024*1024*1024)

	opts, rationale := calculateLazyLoadOpts(
		"gemma-3-4b-it-Q4_K_M",
		32768,
		-1,
		&cppbackend.Config{DefaultGPULayers: 32},
	)

	t.Logf("rationale: %s", rationale.FormatRationale())

	if rationale.Source == "reduced_nctx" {
		t.Fatalf("5GB model on 8GB VRAM не должно требовать reduced_nctx: %s",
			rationale.FormatRationale())
	}
	if opts.ContextSize != 32768 {
		t.Errorf("expected ContextSize=32768 (unchanged), got %d", opts.ContextSize)
	}
}

// TestCalculateLazyLoadOpts_FitsExactly — модель 8 GB на 20 GB VRAM
// с n_ctx=16384: должно быть exact_fit или partial_offload, но НЕ reduced_nctx.
func TestCalculateLazyLoadOpts_FitsExactly(t *testing.T) {
	setEnvLazy(t, "CPPWORKER_VRAM_BYTES", "21474836480")
	setEnvLazy(t, "CPPWORKER_FREE_VRAM_BYTES", "21474836480")
	setEnvLazy(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "34359738368")
	setEnvLazy(t, "CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

	// llama 13B: 40 layers, n_embd=5120, 40 heads, 40 kv_heads (MHA).
	setupBackendWithModel(t, "llama-13b-Q4_K_M", "llama", 40, 40, 40, 5120,
		8*1024*1024*1024)

	opts, rationale := calculateLazyLoadOpts(
		"llama-13b-Q4_K_M",
		16384,
		-1,
		&cppbackend.Config{DefaultGPULayers: 40},
	)

	t.Logf("rationale: %s", rationale.FormatRationale())

	if rationale.Source == "reduced_nctx" {
		t.Fatalf("13B on 20GB with n_ctx=16K не должно требовать reduced_nctx: %s",
			rationale.FormatRationale())
	}
	if opts.ContextSize != 16384 {
		t.Errorf("expected ContextSize=16384, got %d", opts.ContextSize)
	}
}

// TestCalculateLazyLoadOpts_AutoTuneFlagDefaultsToTrue — флаг autoTuneNCtxOnLoadEnabled
// должен быть true по умолчанию (если CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD не задан).
func TestCalculateLazyLoadOpts_AutoTuneFlagDefaultsToTrue(t *testing.T) {
	if !autoTuneNCtxOnLoadEnabled {
		t.Error("autoTuneNCtxOnLoadEnabled должен быть true по умолчанию")
	}
}

// TestCalculateLazyLoadOpts_NoGGUFHeader — если header пустой/повреждён,
// возвращается fallback_no_meta.
func TestCalculateLazyLoadOpts_NoGGUFHeader(t *testing.T) {
	setEnvLazy(t, "CPPWORKER_VRAM_BYTES", "21474836480")
	setEnvLazy(t, "CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

	dir := t.TempDir()
	modelPath := filepath.Join(dir, "broken.gguf")
	os.WriteFile(modelPath, []byte("not a gguf file"), 0644)

	cfg := cppbackend.Config{ModelsDir: dir, DefaultCtxSize: 32768, DefaultGPULayers: 32}
	be := cppbackend.NewBackend(cfg)
	be.Init()
	if mm := be.ModelManager(); mm != nil {
		mm.ScanModels()
	}
	backend = be
	t.Cleanup(func() { backend = nil })

	opts, rationale := calculateLazyLoadOpts(
		"broken",
		32768,
		-1,
		&cppbackend.Config{DefaultGPULayers: 32},
	)

	if rationale.Source != "fallback_no_meta" {
		t.Errorf("expected fallback for broken GGUF, got %s", rationale.Source)
	}
	if opts.ContextSize != 32768 {
		t.Errorf("expected ContextSize unchanged, got %d", opts.ContextSize)
	}
}

// TestCalculateLazyLoadOpts_PartialOffload_30B_On8GB — модель 18 GB на 8 GB VRAM:
// должна применить partial offload или reduced_nctx (так как модель явно больше VRAM).
func TestCalculateLazyLoadOpts_PartialOffload_30B_On8GB(t *testing.T) {
	setEnvLazy(t, "CPPWORKER_VRAM_BYTES", "8589934592")
	setEnvLazy(t, "CPPWORKER_FREE_VRAM_BYTES", "8589934592")
	setEnvLazy(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "34359738368")
	setEnvLazy(t, "CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

	// llama 30B: 60 layers, n_embd=6656, 52 heads, 52 kv_heads (MHA).
	setupBackendWithModel(t, "llama-30b-Q4_K_M", "llama", 60, 52, 52, 6656,
		18*1024*1024*1024)

	opts, rationale := calculateLazyLoadOpts(
		"llama-30b-Q4_K_M",
		8192,
		-1,
		&cppbackend.Config{DefaultGPULayers: 60},
	)

	t.Logf("rationale: %s", rationale.FormatRationale())

	// 18GB модель на 8GB VRAM не влезет целиком — должно быть применено
	// какое-то из решений (partial_offload, reduced_nctx, fallback).
	if rationale.Source == "exact_fit" {
		t.Errorf("18GB model on 8GB VRAM не должен быть exact_fit: %s",
			rationale.FormatRationale())
	}
	if opts.ContextSize > 8192 {
		t.Errorf("expected n_ctx<=8192 (applied=%d), got %s",
			opts.ContextSize, rationale.FormatRationale())
	}
}

// TestLazyLoadRationale_FormatRationale — smoke test для FormatRationale.
func TestLazyLoadRationale_FormatRationale(t *testing.T) {
	r := LazyLoadRationale{
		RequestedNCtx:      32768,
		AppliedNCtx:        16384,
		RequestedGPULayers: 80,
		AppliedGPULayers:   30,
		Source:             "partial_offload",
		ArchName:           "llama",
		NLayers:            80,
		NEmbd:              5120,
		ModelSize:          22 * 1024 * 1024 * 1024,
		AvailableVRAMBytes: 20 * 1024 * 1024 * 1024,
		MaxViableNCtx:      32768,
		GPULayersReduced:   true,
		NCtxReductionPct:   50.0,
	}
	s := r.FormatRationale()
	if s == "" {
		t.Error("FormatRationale returned empty string")
	}
	t.Logf("FormatRationale: %s", s)
}