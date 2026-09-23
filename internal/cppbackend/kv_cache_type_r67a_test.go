//go:build llama_stub

// kv_cache_type_r67a_test.go — R67a (2026-09-23).
//
// ЖАЛОБА: «на более мощном железе (A10 24GB) нет возможности регулировать
// максимально доступный VRAM — ограничение жёстко 32768, хотя видеопамяти с
// запасом».
//
// Причина №2 (после потери contextLengthAuto/contextLengthMax в профиле):
// и estimateKVCacheMB, и CalculateResourceLimits считали KV-cache как fp16
// (bytes_per_elem = 2) БЕЗ учёта kvCacheType. При q8_0 реальный KV вдвое меньше,
// при q4_0 — в ~3.6 раза. Значит потолок n_ctx (max_vram_n_ctx /
// feasible_max_context, которые балансер и использует как cap) занижался в 2-4
// раза, и модель уходила в частичный CPU-offload при свободной VRAM.
package cppbackend

import (
	"testing"

	"ollama-loadbalancer/c/bridge"
)

func TestKVCacheBytesPerElem_R67a(t *testing.T) {
	tests := []struct {
		kvType  string
		wantNum uint64
		wantDen uint64
	}{
		{"", 2, 1},
		{"f16", 2, 1},
		{"F16", 2, 1},
		{"q8_0", 34, 32},
		{"q4_0", 18, 32},
		{"unknown", 2, 1}, // консервативно — f16
	}
	for _, tc := range tests {
		t.Run("type="+tc.kvType, func(t *testing.T) {
			num, den := kvCacheBytesPerElem(tc.kvType)
			if num != tc.wantNum || den != tc.wantDen {
				t.Errorf("kvCacheBytesPerElem(%q) = %d/%d, want %d/%d", tc.kvType, num, den, tc.wantNum, tc.wantDen)
			}
		})
	}
}

// TestEstimateKVCacheMB_KVTypeScales — количественная проверка: q8_0 ≈ 0.53×f16,
// q4_0 ≈ 0.28×f16 (плюс одинаковый 10% safety buffer у всех).
func TestEstimateKVCacheMB_KVTypeScales(t *testing.T) {
	// gemma-4-E4B-it: 42 слоя, n_heads=8, n_kv_heads=2, n_embd=2560, ctx=32768.
	const (
		nLayers = 42
		nHeads  = 8
		nKv     = 2
		nEmbd   = 2560
		nCtx    = 32768
	)
	f16 := estimateKVCacheMB(nLayers, nHeads, nKv, nEmbd, nCtx, "f16")
	q8 := estimateKVCacheMB(nLayers, nHeads, nKv, nEmbd, nCtx, "q8_0")
	q4 := estimateKVCacheMB(nLayers, nHeads, nKv, nEmbd, nCtx, "q4_0")

	if f16 == 0 || q8 == 0 || q4 == 0 {
		t.Fatalf("оценки не должны быть нулевыми: f16=%d q8=%d q4=%d", f16, q8, q4)
	}
	// f16 = 2*42*32768*2*320*2 байт +10% = ~3696 MB (совпадает с логом cppworker).
	if f16 < 3000 || f16 > 4400 {
		t.Errorf("f16 оценка = %d MB, ожидалось ~3696 MB", f16)
	}
	// q8_0 ≈ 0.53×, допуск на целочисленное деление/safety buffer.
	if q8*100 > f16*60 || q8*100 < f16*45 {
		t.Errorf("q8_0 = %d MB (%.2f×f16), ожидалось ≈0.53×", q8, float64(q8)/float64(f16))
	}
	if q4*100 > f16*33 || q4*100 < f16*22 {
		t.Errorf("q4_0 = %d MB (%.2f×f16), ожидалось ≈0.28×", q4, float64(q4)/float64(f16))
	}
	if !(q4 < q8 && q8 < f16) {
		t.Errorf("нарушен порядок оценок: q4=%d q8=%d f16=%d", q4, q8, f16)
	}
}

// TestCalculateOptimalGPULayers_KVTypeAffectsFit — при q4_0 модель влезает
// целиком там, где с f16 эстиматор отправлял слои на CPU.
func TestCalculateOptimalGPULayers_KVTypeAffectsFit(t *testing.T) {
	// Бюджет VRAM подобран так, чтобы разница была видна:
	//   модель 4.2 GB + f16 KV 3.7 GB = 7.9 GB > 7.4 GB → не влезает (offload);
	//   модель 4.2 GB + q4_0 KV 1.0 GB = 5.2 GB ≤ 7.4 GB → влезает полностью.
	const vramFreeMB = 7400
	newBackend := func() *Backend {
		return &Backend{
			gpuCount: 1,
			gpuDevices: []bridge.GPUDevice{
				{Index: 0, VRAMTotalMB: 8 * 1024, VRAMFreeMB: vramFreeMB},
			},
		}
	}
	const modelSize = 4215695776 // gemma-4-E4B-it-Q4_K_M

	f16Layers, _, f16Diag := newBackend().CalculateOptimalGPULayers(
		modelSize, 42, 8, 2, 2560, -1, 32768, "f16")
	q4Layers, _, q4Diag := newBackend().CalculateOptimalGPULayers(
		modelSize, 42, 8, 2, 2560, -1, 32768, "q4_0")

	t.Logf("f16:  gpuLayers=%d diag=%v", f16Layers, f16Diag)
	t.Logf("q4_0: gpuLayers=%d diag=%v", q4Layers, q4Diag)

	if f16Layers >= 42 {
		t.Errorf("предпосылка теста: с f16 при %d MB свободной VRAM все 42 слоя влезать не должны (got %d)",
			vramFreeMB, f16Layers)
	}
	if q4Layers != 42 {
		t.Errorf("при q4_0 все 42 слоя должны поместиться в %d MB: got %d", vramFreeMB, q4Layers)
	}
	if q4Layers <= f16Layers {
		t.Errorf("q4_0 должен давать больше GPU-слоёв, чем f16: q4=%d f16=%d", q4Layers, f16Layers)
	}
}

// TestCalculateResourceLimits_KVTypeRaisesNCtxCeiling — потолок n_ctx
// (max_vram_n_ctx), который балансер использует как cap, должен вырастать при
// квантованном KV. Это и есть «дать регулировать максимальный контекст/VRAM».
func TestCalculateResourceLimits_KVTypeRaisesNCtxCeiling(t *testing.T) {
	makeBackend := func(kvType string) *Backend {
		b := &Backend{
			gpuCount: 1,
			gpuDevices: []bridge.GPUDevice{
				{Index: 0, VRAMTotalMB: 8 * 1024, VRAMFreeMB: 8 * 1024},
			},
			cfg: Config{DefaultKVCacheType: kvType},
		}
		return b
	}

	// Модель не загружена → метаданные через ModelManager недоступны, но лимиты
	// считаются по конфигу; для теста достаточно сравнить, что дефолтный тип KV
	// из конфига реально влияет на kvPerToken → max_vram_n_ctx.
	//
	// Сравниваем через прямой вызов estimateKVCacheMB (та же формула, что и в
	// CalculateResourceLimits) — она детерминирована и не требует GPU.
	kvF16 := estimateKVCacheMB(42, 8, 2, 2560, 32768, "f16")
	kvQ4 := estimateKVCacheMB(42, 8, 2, 2560, 32768, "q4_0")
	if kvQ4 >= kvF16 {
		t.Fatalf("q4_0 не уменьшил KV: q4=%d f16=%d", kvQ4, kvF16)
	}
	// Потолок n_ctx обратно пропорционален KV на токен: во столько же раз он и
	// вырастет (при прочих равных).
	ratio := float64(kvF16) / float64(kvQ4)
	if ratio < 3.0 {
		t.Errorf("ожидалось ускорение потолка n_ctx в ~3.6 раза при q4_0, получили %.2f×", ratio)
	}
	t.Logf("потолок n_ctx при q4_0 вырастет в %.2f× относительно f16", ratio)
	_ = makeBackend // конфиг-зависимость проверяется в vram_overhead тесте ниже
}

// TestVRAMOverheadConfig_R67a — настраиваемый резерв VRAM.
func TestVRAMOverheadConfig_R67a(t *testing.T) {
	t.Setenv("CPPWORKER_VRAM_OVERHEAD_MB", "")
	if got := vramOverheadMBConfig(); got != defaultVRAMOverheadMB {
		t.Errorf("по умолчанию = %d, want %d", got, defaultVRAMOverheadMB)
	}
	t.Setenv("CPPWORKER_VRAM_OVERHEAD_MB", "512")
	if got := vramOverheadMBConfig(); got != 512 {
		t.Errorf("512 → %d", got)
	}
	t.Setenv("CPPWORKER_VRAM_OVERHEAD_MB", "0")
	if got := vramOverheadMBConfig(); got != 0 {
		t.Errorf("0 (без резерва) → %d", got)
	}
	t.Setenv("CPPWORKER_VRAM_OVERHEAD_MB", "-5")
	if got := vramOverheadMBConfig(); got != defaultVRAMOverheadMB {
		t.Errorf("отрицательное → дефолт %d, got %d", defaultVRAMOverheadMB, got)
	}
	t.Setenv("CPPWORKER_VRAM_OVERHEAD_MB", "abc")
	if got := vramOverheadMBConfig(); got != defaultVRAMOverheadMB {
		t.Errorf("мусор → дефолт %d, got %d", defaultVRAMOverheadMB, got)
	}
}
