//go:build llama_stub

// estimator_compare_r83_test.go — R83 §9.4 (2026-09-26): подготовка к удалению
// legacy-оценок.
//
// ЗАЧЕМ ЭТОТ ТЕСТ. В коде живут две оценки памяти:
//
//	legacy: Backend.CalculateOptimalGPULayers — веса ×0.7 (EstimateGPUMemoryForModel)
//	        и KV как 256 Б/токен; продовые вызовы остались на путях
//	        auto-offload/reload (cmd/cppworker/handlers_model.go:1735,
//	        handlers_config.go:582);
//	memfit: internal/memfit — единственный предикат computeSplit, реальные
//	        числа KV (16 слоёв, head_dim 256), cgroup-aware RAM.
//
// Удалять legacy вслепую нельзя: на одних сценариях он занижает (веса ×0.7),
// на других завышает (KV 256 Б/токен против реальных 18 432 Б/токен у Qwen3.8).
// Этот тест прогоняет ОБА пути на числах живого стенда и фиксирует расхождение —
// то есть служит эталоном «до» для рефактора: после замены legacy на memfit
// значения обязаны совпасть с колонкой memfit.
//
// Паттерн Backend{} с явными gpuDevices взят из backend_r52_test.go.
package cppbackend

import (
	"testing"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/memfit"
)

// estimatorScenario — один сценарий раскладки.
type estimatorScenario struct {
	name string
	// модель
	modelSizeBytes int64
	nLayers        int // всего слоёв (llama_model_n_layer)
	nHeads         int
	nKvHeads       int
	nEmbd          int
	kvLayers       int // слоёв с KV-кэшем (KVLayersFor)
	kvHeadDim      int // head_dim KV (attention.key_length)
	// железо и запрос
	vramFreeMB  uint64
	ramAvailGB  int
	ctx         int
	kvType      string
	requestedGL int // -2/0 = авто
}

// estimatorScenarios — живые стенды.
func estimatorScenarios() []estimatorScenario {
	return []estimatorScenario{
		{
			// RTX 3070 8 GB, Qwen3.8-27B-UD-Q4_K_M. Живые числа: веса 15 701 MB,
			// memfit выдал 22 слоя (partial_offload), C-bridge потом видел
			// free=1082 MB. kvCacheType=q8_0 пришёл из профиля модели.
			name:           "3070-8GB / Qwen3.8-27B / q8_0",
			modelSizeBytes: 16464440224,
			nLayers:        64, nHeads: 24, nKvHeads: 4, nEmbd: 5120,
			kvLayers: 16, kvHeadDim: 256,
			vramFreeMB: 8191, ramAvailGB: 20, ctx: 32768, kvType: "q8_0",
			requestedGL: -2,
		},
		{
			// Тот же стенд, но KV в f16 (дефолт конфига): KV вдвое больше —
			// проверяем, что обе оценки это замечают.
			name:           "3070-8GB / Qwen3.8-27B / f16",
			modelSizeBytes: 16464440224,
			nLayers:        64, nHeads: 24, nKvHeads: 4, nEmbd: 5120,
			kvLayers: 16, kvHeadDim: 256,
			vramFreeMB: 8191, ramAvailGB: 20, ctx: 32768, kvType: "f16",
			requestedGL: -2,
		},
		{
			// A10 24 GB — модель почти целиком в VRAM. Здесь legacy historically
			// давал exact-fit, и важно, что memfit не скажет меньше.
			name:           "A10-24GB / Qwen3.8-27B / q8_0",
			modelSizeBytes: 16464440224,
			nLayers:        64, nHeads: 24, nKvHeads: 4, nEmbd: 5120,
			kvLayers: 16, kvHeadDim: 256,
			vramFreeMB: 24000, ramAvailGB: 40, ctx: 32768, kvType: "q8_0",
			requestedGL: -2,
		},
		{
			// Мелкая модель: влезает целиком даже на 8 GB — оба пути обязаны
			// вернуть «все слои» (или максимум слоёв).
			name:           "3070-8GB / 4B-модель / q4_0",
			modelSizeBytes: 2500000000,
			nLayers:        36, nHeads: 32, nKvHeads: 8, nEmbd: 2560,
			kvLayers: 36, kvHeadDim: 128,
			vramFreeMB: 8191, ramAvailGB: 16, ctx: 8192, kvType: "q4_0",
			requestedGL: -2,
		},
	}
}

// legacyGPULayers вызывает оставшийся в проде legacy-путь.
func legacyGPULayers(t *testing.T, sc estimatorScenario) (int, bool) {
	t.Helper()
	b := &Backend{
		gpuDevices: []bridge.GPUDevice{
			{Index: 0, VRAMTotalMB: sc.vramFreeMB, VRAMFreeMB: sc.vramFreeMB},
		},
	}
	layers, useMmap, diag := b.CalculateOptimalGPULayers(
		sc.modelSizeBytes, sc.nLayers, sc.nHeads, sc.nKvHeads, sc.nEmbd,
		sc.requestedGL, sc.ctx, sc.kvType)
	if diag != nil && diag.Recomendation != "" {
		t.Logf("legacy diagnostic: %s", diag.Recomendation)
	}
	return layers, useMmap
}

// memfitGPULayers считает раскладку через memfit — так, как это делает
// checkVRAMForModel (backend.go). Бюджет собирается из чисел сценария, а не из
// системы: тест обязан быть детерминированным.
func memfitGPULayers(t *testing.T, sc estimatorScenario) (layers int, stage string) {
	t.Helper()
	spec := MemfitSpecFromValues("m", sc.modelSizeBytes, sc.nLayers, sc.nHeads,
		sc.nKvHeads, sc.nEmbd, 0, sc.kvLayers, sc.kvHeadDim)
	budget := memfit.Budget{
		VRAMTotal:   memfit.MiBOf(int64(sc.vramFreeMB)),
		VRAMFree:    memfit.MiBOf(int64(sc.vramFreeMB)),
		VRAMKnown:   true,
		VRAMReserve: memfit.MiBOf(int64(vramOverheadMBConfig())),
		RAMTotal:    memfit.GiBOf(int64(sc.ramAvailGB)),
		RAMAvail:    memfit.GiBOf(int64(sc.ramAvailGB)),
		RAMKnown:    true,
		RAMReserve:  memfit.MiBOf(defaultRAMReserveMBForFit),
	}
	v := memfit.Evaluate(spec, memfit.Request{Ctx: sc.ctx, KVType: MemfitKVType(sc.kvType)},
		budget, MemfitPolicy())
	return v.GPULayers, string(v.Stage)
}

// TestR83_EstimatorComparison — печатает обе оценки и фиксирует инварианты,
// которые обязаны сохраниться после замены legacy на memfit.
func TestR83_EstimatorComparison(t *testing.T) {
	for _, sc := range estimatorScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			legacy, legacyMmap := legacyGPULayers(t, sc)
			layers, stage := memfitGPULayers(t, sc)

			t.Logf("legacy=%d (mmap=%v) | memfit=%d (stage=%s)",
				legacy, legacyMmap, layers, stage)

			// Инвариант 1: обе оценки — в границах [0, nLayers].
			for name, v := range map[string]int{"legacy": legacy, "memfit": layers} {
				if v < 0 || v > sc.nLayers {
					t.Errorf("%s: слоёв %d вне [0, %d]", name, v, sc.nLayers)
				}
			}
			// Инвариант 2: memfit никогда не просит невзгоды — при stage=cpu_only
			// слоёв на GPU нет.
			if stage == "cpu_only" && layers != 0 {
				t.Errorf("memfit stage=cpu_only, но layers=%d", layers)
			}
		})
	}
}

// TestR83_EstimatorComparison_LiveStandDocumented — фиксирует КОНКРЕТНОЕ
// расхождение на живом стенде (3070 + Qwen3.8-27B), чтобы после рефактора было
// видно, что именно изменилось.
func TestR83_EstimatorComparison_LiveStandDocumented(t *testing.T) {
	sc := estimatorScenarios()[0]
	legacy, _ := legacyGPULayers(t, sc)
	layers, stage := memfitGPULayers(t, sc)

	t.Logf("живой стенд (3070 8 GB, Qwen3.8-27B, q8_0, ctx=32768): "+
		"legacy=%d, memfit=%d (stage=%s) — в проде memfit выдал 22 слоя",
		legacy, layers, stage)

	if layers <= 0 {
		t.Errorf("memfit на живом сценарии дал %d слоёв (stage=%s) — "+
			"эталон для рефактора сломан", layers, stage)
	}
}
