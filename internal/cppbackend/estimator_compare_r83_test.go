//go:build llama_stub

// estimator_compare_r83_test.go — R83 §9.4 (2026-09-26): эталон замены
// legacy-оценок на memfit.
//
// ИСТОРИЯ. Тест появился как «до/после» для двух живых оценок:
// Backend.CalculateOptimalGPULayers (веса ×0.7, KV «256 КБ/токен») и
// memfit.Evaluate. Он показал, что legacy ЗАНИЖАЕТ число GPU-слоёв: на живом
// стенде (RTX 3070 8 GB + Qwen3.8-27B) legacy давал 18 слоёв при q8_0 и 0 при
// f16 (CPU-only), memfit — 23 и 22.
//
// Затем legacy-функция удалена (продовых вызовов не было, раскладку считает
// memfit — см. plans/2026-09-26-estimator-unification-protocol.md). Поэтому
// колонка «legacy» здесь теперь ЗАМОРОЖЕННЫЙ ЗАМЕР: он остаётся точкой
// сравнения (что именно изменилось) и одновременно фиксирует, что после
// унификации весов (WeightsOnGPUBytes вместо ×0.7) legacy стал ещё
// консервативнее — его KV «256 КБ/токен» больше ничем не компенсируется.
//
// Живьём считается только memfit: он и есть продакшн-решение.
package cppbackend

import (
	"testing"

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
	// frozenLegacy — сколько слоёв давала удалённая legacy-оценка
	// (Backend.CalculateOptimalGPULayers) ДО удаления, на тех же входных данных.
	frozenLegacy int
}

// estimatorScenarios — живые стенды.
func estimatorScenarios() []estimatorScenario {
	return []estimatorScenario{
		{
			// RTX 3070 8 GB, Qwen3.8-27B-UD-Q4_K_M. Живые числа: веса 15 701 MB,
			// memfit выдал 20-22 слоя (partial_offload), C-bridge аллоцировал
			// KV 1088 MiB при q8_0. kvCacheType=q8_0 пришёл из профиля модели.
			name:           "3070-8GB / Qwen3.8-27B / q8_0",
			modelSizeBytes: 16464440224,
			nLayers:        64, nHeads: 24, nKvHeads: 4, nEmbd: 5120,
			kvLayers: 16, kvHeadDim: 256,
			vramFreeMB: 8191, ramAvailGB: 20, ctx: 32768, kvType: "q8_0",
			requestedGL:  -2,
			// Замер до удаления, уже с унифицированными весами.
			frozenLegacy: 12,
		},
		{
			// Тот же стенд, но KV в f16 (дефолт конфига): KV вдвое больше —
			// legacy уходил в CPU-only (0 слоёв), memfit держит partial offload.
			name:           "3070-8GB / Qwen3.8-27B / f16",
			modelSizeBytes: 16464440224,
			nLayers:        64, nHeads: 24, nKvHeads: 4, nEmbd: 5120,
			kvLayers: 16, kvHeadDim: 256,
			vramFreeMB: 8191, ramAvailGB: 20, ctx: 32768, kvType: "f16",
			requestedGL:  -2,
			frozenLegacy: 0,
		},
		{
			// A10 24 GB — модель почти целиком в VRAM. Здесь legacy исторически
			// давал exact-fit, и важно, что memfit не скажет меньше.
			name:           "A10-24GB / Qwen3.8-27B / q8_0",
			modelSizeBytes: 16464440224,
			nLayers:        64, nHeads: 24, nKvHeads: 4, nEmbd: 5120,
			kvLayers: 16, kvHeadDim: 256,
			vramFreeMB: 24000, ramAvailGB: 40, ctx: 32768, kvType: "q8_0",
			requestedGL:  -2,
			frozenLegacy: 64,
		},
		{
			// Мелкая модель: влезает целиком даже на 8 GB — оба пути «все слои».
			name:           "3070-8GB / 4B-модель / q4_0",
			modelSizeBytes: 2500000000,
			nLayers:        36, nHeads: 32, nKvHeads: 8, nEmbd: 2560,
			kvLayers: 36, kvHeadDim: 128,
			vramFreeMB: 8191, ramAvailGB: 16, ctx: 8192, kvType: "q4_0",
			requestedGL:  -2,
			frozenLegacy: 36,
		},
	}
}

// memfitGPULayers считает раскладку через memfit — так, как это делает
// checkVRAMForModel (backend.go). Бюджет собирается из чисел сценария, а не из
// системы: тест обязан быть детерминированным.
func memfitGPULayers(t *testing.T, sc estimatorScenario) (layers int, stage string, v memfit.Verdict) {
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
	v = memfit.Evaluate(spec, memfit.Request{Ctx: sc.ctx, KVType: MemfitKVType(sc.kvType)},
		budget, MemfitPolicy())
	return v.GPULayers, string(v.Stage), v
}

// TestR83_EstimatorComparison — печатает замороженный legacy и живой memfit,
// фиксируя инварианты.
func TestR83_EstimatorComparison(t *testing.T) {
	for _, sc := range estimatorScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			layers, stage, _ := memfitGPULayers(t, sc)

			t.Logf("legacy(frozen)=%d | memfit=%d (stage=%s)",
				sc.frozenLegacy, layers, stage)

			// Инвариант 1: обе оценки — в границах [0, nLayers].
			for name, v := range map[string]int{"legacy": sc.frozenLegacy, "memfit": layers} {
				if v < 0 || v > sc.nLayers {
					t.Errorf("%s: слоёв %d вне [0, %d]", name, v, sc.nLayers)
				}
			}
			// Инвариант 2: stage=cpu_only означает отсутствие слоёв на GPU.
			if stage == "cpu_only" && layers != 0 {
				t.Errorf("memfit stage=cpu_only, но layers=%d", layers)
			}
			// Инвариант 3: memfit не может планировать МЕНЬШЕ, чем давала
			// удалённая legacy-оценка, — иначе замена была бы регрессом по
			// производительности (меньше слоёв на GPU при том же железе).
			if layers < sc.frozenLegacy {
				t.Errorf("memfit=%d меньше замороженного legacy=%d — регресс по числу слоёв",
					layers, sc.frozenLegacy)
			}
		})
	}
}

// TestR83_EstimatorComparison_LiveStandDocumented — фиксирует конкретный
// результат на живом стенде (3070 + Qwen3.8-27B), чтобы после следующих шагов
// рефактора было видно, что именно изменилось.
func TestR83_EstimatorComparison_LiveStandDocumented(t *testing.T) {
	sc := estimatorScenarios()[0]
	layers, stage, v := memfitGPULayers(t, sc)

	t.Logf("живой стенд (3070 8 GB, Qwen3.8-27B, q8_0, ctx=32768): "+
		"legacy(frozen)=%d, memfit=%d (stage=%s) — в проде memfit выдал 20 слоёв, "+
		"llama.cpp аллоцировал KV 1088 MiB",
		sc.frozenLegacy, layers, stage)

	if layers <= 0 {
		t.Errorf("memfit на живом сценарии дал %d слоёв (stage=%s) — "+
			"эталон для рефактора сломан", layers, stage)
	}
	// KV на токен обязан совпасть с тем, что аллоцировал llama.cpp:
	// 34 816 Б/токен × 32 768 токенов = 1088 МиБ (K/V q8_0, 16 слоёв, head_dim 256).
	if v.KVPerToken != 34816 {
		t.Errorf("KV на токен = %d, want 34816 (llama_kv_cache: 1088 MiB на 32768 токенов)",
			v.KVPerToken)
	}
	if got := v.KVTotal.MiB(); got != 1088 {
		t.Errorf("полный KV = %d MiB, want 1088 (проверено логом llama_kv_cache)", got)
	}
}
