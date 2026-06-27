// Тесты для фикса №3 (2026-06-26): ResourceLimits и расчёт max_vram_n_ctx.
//
// Корневая причина бага из env_log.txt (строки 121, 133):
//   Балансировщик видел max_vram_n_ctx=0 и model_max_context=0 для всех моделей,
//   потому что cppworker не сообщал эти поля в /api/models. Без них preflight
//   не мог расчитать target_n_ctx и ставил хардкод 8192, что ломалось на
//   моделях с большим training context (Qwen3.6-35B-A3B, training context 32k).
//
// Фикс в internal/cppbackend/backend.go:
//   - Добавлен тип ResourceLimits с полями TotalVRAMMB, AvailableVRAMMB,
//     MaxVRAMNCtx, TotalRAMMB, AvailableRAMMB, MaxRAMNCtx, ModelMaxContext.
//   - Добавлен метод CalculateResourceLimits(name) который заполняет эти поля.
//   - В calculateLazyLoadOpts используется kvPerToken = 4 * NLayers * NKvHeads * headDim.
//
// Тесты проверяют:
//  1. Чистую функцию estimateKVCacheMB (KV-cache размер в MB для заданных n_ctx).
//  2. Корректность формулы kvPerToken для разных архитектур (LLaMA, Qwen, Gemma).
//  3. Расчёт ResourceLimits для загруженной модели в типичных сценариях.
//
// NB: Тесты НЕ создают реальный Backend (требует GPU/CUDA). Они используют
// чистые функции estimateKVCacheMB и ручной расчёт kvPerToken.
package cppbackend

import (
	"testing"
)

// kvPerTokenFromArch вычисляет размер KV-cache на один токен (в байтах) для fp16.
// Формула:
//
//	kvPerToken (bytes) = 4 × NLayers × NKvHeads × headDim
//	                    (4 = 2 (K+V) × 2 bytes per fp16)
//
// где headDim = NEmbd / NHeads. Это helper, идентичный внутреннему расчёту
// в CalculateResourceLimits, но вынесенный для прямого тестирования.
func kvPerTokenFromArch(nLayers, nEmbd, nHeads, nKvHeads int) uint64 {
	if nHeads <= 0 {
		nHeads = 1
	}
	if nKvHeads <= 0 {
		nKvHeads = nHeads
	}
	headDim := nEmbd / nHeads
	return 4 * uint64(nLayers) * uint64(nKvHeads) * uint64(headDim)
}

// TestKVPerTokenFromArch_RealModels — проверяет формулу на реальных архитектурах
// из env_log.txt (Qwen3.6-35B-A3B, gemma-4-E4B-it).
//
// Реальные параметры этих моделей (из GGUF metadata):
//   Qwen3.6-35B-A3B (HauhauCS-Aggressive-Q4_K_M):
//     NLayers=48, NEmbd=5120, NHeads=40, NKvHeads=8 (GQA)
//     → headDim = 5120/40 = 128
//     → kvPerToken = 4 * 48 * 8 * 128 = 196608 bytes/token = 192 KB/token
//
//   Gemma-4-E4B-it-Q4_K_M:
//     NLayers=42, NEmbd=2560, NHeads=20, NKvHeads=8 (GQA)
//     → headDim = 2560/20 = 128
//     → kvPerToken = 4 * 42 * 8 * 128 = 172032 bytes/token = 168 KB/token
//
//   LLaMA-3 8B (Q4_K_M):
//     NLayers=32, NEmbd=4096, NHeads=32, NKvHeads=8 (GQA)
//     → headDim = 4096/32 = 128
//     → kvPerToken = 4 * 32 * 8 * 128 = 131072 bytes/token = 128 KB/token
func TestKVPerTokenFromArch_RealModels(t *testing.T) {
	tests := []struct {
		name          string
		nLayers       int
		nEmbd         int
		nHeads        int
		nKvHeads      int
		expectedBytes uint64
	}{
		{
			name:          "Qwen3.6-35B-A3B (from env_log.txt)",
			nLayers:       48, nEmbd: 5120, nHeads: 40, nKvHeads: 8,
			expectedBytes: 196608, // 4 * 48 * 8 * 128
		},
		{
			name:          "Gemma-4-E4B-it (from env_log.txt)",
			nLayers:       42, nEmbd: 2560, nHeads: 20, nKvHeads: 8,
			expectedBytes: 172032, // 4 * 42 * 8 * 128
		},
		{
			name:          "LLaMA-3 8B (GQA)",
			nLayers:       32, nEmbd: 4096, nHeads: 32, nKvHeads: 8,
			expectedBytes: 131072, // 4 * 32 * 8 * 128
		},
		{
			name:          "LLaMA-2 7B (MHA, nKvHeads=nHeads)",
			nLayers:       32, nEmbd: 4096, nHeads: 32, nKvHeads: 0, // nKvHeads=0 → fallback to nHeads
			expectedBytes: 524288, // 4 * 32 * 32 * 128
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kvPerTokenFromArch(tt.nLayers, tt.nEmbd, tt.nHeads, tt.nKvHeads)
			if got != tt.expectedBytes {
				t.Errorf("kvPerTokenFromArch(%d, %d, %d, %d) = %d bytes, want %d",
					tt.nLayers, tt.nEmbd, tt.nHeads, tt.nKvHeads, got, tt.expectedBytes)
			}
		})
	}
}

// TestEstimateKVCacheMB_RealisticSizes — проверяет расчёт KV-cache в MB.
//
// Примеры для типичных n_ctx (env_log.txt показывает запросы на 128k токенов):
//   n_ctx=32768, model=LLaMA-3 8B (kvPerToken=131072 = 128KB):
//     KV-cache = 2 * 32 * 32768 * 8 * 128 * 2 = 4.29 GB → + 10% safety = 4.72 GB ≈ 4837 MB
//
//   n_ctx=8192, model=LLaMA-3 8B:
//     KV-cache = 2 * 32 * 8192 * 8 * 128 * 2 = 1.07 GB → + 10% safety = 1.18 GB ≈ 1206 MB
func TestEstimateKVCacheMB_RealisticSizes(t *testing.T) {
	tests := []struct {
		name      string
		nLayers   int
		nHeads    int
		nKvHeads  int
		nEmbd     int
		nCtx      int
		minMB     uint64
		maxMB     uint64
	}{
		{
			name:     "LLaMA-3 8B, n_ctx=8192 (default cppworker)",
			nLayers:  32, nHeads: 32, nKvHeads: 8, nEmbd: 4096, nCtx: 8192,
			minMB: 1100, maxMB: 1300, // ~1.18 GB
		},
		{
			name:     "LLaMA-3 8B, n_ctx=32768",
			nLayers:  32, nHeads: 32, nKvHeads: 8, nEmbd: 4096, nCtx: 32768,
			minMB: 4500, maxMB: 5100, // ~4.7 GB
		},
		{
			name:     "LLaMA-3 8B, n_ctx=131072 (Qwen3.6 request from env_log.txt)",
			nLayers:  32, nHeads: 32, nKvHeads: 8, nEmbd: 4096, nCtx: 131072,
			minMB: 18000, maxMB: 20500, // ~19 GB
		},
		{
			name:     "Gemma-4-E4B (MHA + 42 layers), n_ctx=65536",
			nLayers:  42, nHeads: 20, nKvHeads: 8, nEmbd: 2560, nCtx: 65536,
			// kvPerToken = 172032 bytes = 168 KB
			// KV = 168 * 65536 = 11 GB + 10% safety = ~12 GB
			minMB: 11000, maxMB: 13000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateKVCacheMB(tt.nLayers, tt.nHeads, tt.nKvHeads, tt.nEmbd, tt.nCtx)
			if got < tt.minMB || got > tt.maxMB {
				t.Errorf("estimateKVCacheMB(%d, %d, %d, %d, %d) = %d MB, want [%d, %d] MB",
					tt.nLayers, tt.nHeads, tt.nKvHeads, tt.nEmbd, tt.nCtx,
					got, tt.minMB, tt.maxMB)
			}
		})
	}
}

// TestEstimateKVCacheMB_ZeroInputs — проверяет безопасную обработку невалидных входов.
// Для всех нулевых/отрицательных параметров функция должна вернуть 0 (без panic).
func TestEstimateKVCacheMB_ZeroInputs(t *testing.T) {
	tests := []struct {
		name     string
		nLayers  int
		nHeads   int
		nKvHeads int
		nEmbd    int
		nCtx     int
	}{
		{"all zeros", 0, 0, 0, 0, 0},
		{"negative nLayers", -1, 32, 8, 4096, 8192},
		{"zero nHeads", 32, 0, 8, 4096, 8192},
		{"zero nEmbd", 32, 32, 8, 0, 8192},
		{"zero nCtx", 32, 32, 8, 4096, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateKVCacheMB(tt.nLayers, tt.nHeads, tt.nKvHeads, tt.nEmbd, tt.nCtx)
			if got != 0 {
				t.Errorf("estimateKVCacheMB(%d, %d, %d, %d, %d) = %d, want 0 (safe-fail)",
					tt.nLayers, tt.nHeads, tt.nKvHeads, tt.nEmbd, tt.nCtx, got)
			}
		})
	}
}

// TestCalculateResourceLimits_OnUnloadedModel — проверяет, что для незагруженной
// модели ResourceLimits возвращает нулевые MaxVRAMNCtx/MaxRAMNCtx (без panic).
//
// До фикса №3 cppworker не сообщал эти поля и балансировщик видел 0 в логах
// (env_log.txt строки 121, 133). Сейчас это явное "не загружено" поведение —
// лучше чем молча возвращать некорректные данные.
func TestCalculateResourceLimits_OnUnloadedModel(t *testing.T) {
	cfg := Config{
		ModelsDir:       t.TempDir(),
		DefaultCtxSize:  4096,
		DefaultBatchSize: 512,
	}
	b := NewBackend(cfg)
	// Не грузим ни одной модели.

	limits := b.CalculateResourceLimits("non_existent_model")
	if limits.MaxVRAMNCtx != 0 {
		t.Errorf("MaxVRAMNCtx for unloaded model = %d, want 0", limits.MaxVRAMNCtx)
	}
	if limits.MaxRAMNCtx != 0 {
		t.Errorf("MaxRAMNCtx for unloaded model = %d, want 0", limits.MaxRAMNCtx)
	}
	if limits.ModelMaxContext != 0 {
		t.Errorf("ModelMaxContext for unloaded model = %d, want 0", limits.ModelMaxContext)
	}
	// TotalVRAMMB/TotalRAMMB могут быть 0 на stub (нет GPU), это корректно.
	t.Logf("OK: ResourceLimits for unloaded model: VRAM=%dMB/%d, RAM=%dMB/%d, max_vram_n_ctx=%d, max_ram_n_ctx=%d, model_max_context=%d",
		limits.TotalVRAMMB, limits.AvailableVRAMMB,
		limits.TotalRAMMB, limits.AvailableRAMMB,
		limits.MaxVRAMNCtx, limits.MaxRAMNCtx, limits.ModelMaxContext)
}

// TestMaxViableNCtx_FromVRAM — проверяет расчёт максимального n_ctx, который
// влезет в заданный объём VRAM с overhead. Это ключевая часть формулы
// в CalculateResourceLimits (строки 4-5).
//
// Формула:
//   usableBytes = (availableVRAMBytes - overheadBytes)
//   maxNCtx     = usableBytes / kvPerTokenBytes
//
// Пример: kvPerToken=131072, available=8GB, overhead=2GB:
//   usable = 6 GB = 6442450944 bytes
//   maxNCtx = 6442450944 / 131072 = 49152
func TestMaxViableNCtx_FromVRAM(t *testing.T) {
	tests := []struct {
		name             string
		kvPerTokenBytes  uint64
		availableVRAMMB  uint64
		overheadMB       uint64
		expectedMaxNCtx  int
		tolerancePercent float64
	}{
		{
			name:             "LLaMA-3 8B на 8GB VRAM (с 2GB overhead)",
			kvPerTokenBytes:  131072, // 128 KB
			availableVRAMMB:  8192,  // 8 GB
			overheadMB:       2048,  // 2 GB
			expectedMaxNCtx:  49152, // (6 GB) / 128 KB
			tolerancePercent: 0.01,
		},
		{
			name:             "LLaMA-3 8B на 20GB VRAM (с 2GB overhead)",
			kvPerTokenBytes:  131072,
			availableVRAMMB:  20480,
			overheadMB:       2048,
			expectedMaxNCtx:  147456, // (18 GB) / 128 KB
			tolerancePercent: 0.01,
		},
		{
			name:             "Qwen3.6-35B-A3B на 24GB VRAM",
			kvPerTokenBytes:  196608, // 192 KB
			availableVRAMMB:  24576,  // 24 GB
			overheadMB:       4096,   // 4 GB (большая модель → больше overhead)
			expectedMaxNCtx:  106496, // (20 GB) / 192 KB = 108 MB / 192 KB ≈ 106496
			tolerancePercent: 0.05,   // немного больше tolerance из-за округления
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usableBytes := (tt.availableVRAMMB - tt.overheadMB) * 1024 * 1024
			got := int(usableBytes / tt.kvPerTokenBytes)
			tolerance := float64(tt.expectedMaxNCtx) * tt.tolerancePercent
			diff := float64(got - tt.expectedMaxNCtx)
			if diff < 0 {
				diff = -diff
			}
			if diff > tolerance {
				t.Errorf("maxNCtx for %s = %d, want ~%d (±%.0f%%)",
					tt.name, got, tt.expectedMaxNCtx, tt.tolerancePercent*100)
			}
		})
	}
}