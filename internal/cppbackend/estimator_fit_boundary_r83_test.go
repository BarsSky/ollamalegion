//go:build llama_stub

// estimator_fit_boundary_r83_test.go — R83 §9.4 шаг 1б (2026-09-26):
// ГДЕ ПРОХОДИТ ГРАНИЦА между «влезает» и «не влезает».
//
// Зачем отдельный тест-калькулятор. Решение «отказывать при !Fits() или
// применять CPU-only раскладку» невозможно принять без чисел: на живом стенде
// cppworker уже грузит модель с CPU-only и она работает (медленно). Тест печатает
// таблицу по живым числам (3070 8 GB + Qwen3.8-27B) и фиксирует инварианты,
// на которые опирается предложение:
//
//  1. `StageDoesNotFit` при живом n_ctx=32768 НЕ наступает — то есть нынешняя
//     раскладка не «не влезает», а «влезает частично»: отказывать не за что.
//  2. `StageDoesNotFit` наступает, когда сумма VRAM+RAM не покрывает даже
//     cpu_only (RAM-дефицит) — вот ТАМ отказ уместен, потому что загрузка
//     заведомо провалится (llama.cpp упадёт с OOM/abort).
//  3. `MaxHardCtx` называет максимальный n_ctx, который ещё помещается, — это
//     число и должно уходить в suggestion отказа.
package cppbackend

import (
	"testing"

	"ollama-loadbalancer/internal/memfit"
)

// fitBoundarySpec — живой файл Qwen3.8-27B-UD-Q4_K_M (16 464 440 224 байт,
// 64 слоя, 16 из них с KV, head_dim KV 256).
func fitBoundarySpec() memfit.ModelSpec {
	return MemfitSpecFromValues("qwen3.8:latest", 16464440224, 64, 24, 4, 5120, 262144, 16, 256)
}

// fitBoundaryBudget — бюджет стенда: 8 GB VRAM (2048 MiB резерв) и заданная RAM
// (4096 MiB резерв). Числа совпадают с тем, что cppworker видит в контейнере
// (24.46 GiB лимит WSL минус занятое → ~18.8-20 GiB доступно).
func fitBoundaryBudget(ramGB int64) memfit.Budget {
	return memfit.Budget{
		VRAMTotal:   memfit.MiBOf(8192),
		VRAMFree:    memfit.MiBOf(8192),
		VRAMKnown:   true,
		VRAMReserve: memfit.MiBOf(2048),
		RAMTotal:    memfit.GiBOf(ramGB),
		RAMAvail:    memfit.GiBOf(ramGB),
		RAMKnown:    true,
		RAMReserve:  memfit.MiBOf(4096),
	}
}

func fitBoundaryEval(ctx int, kvType string, ramGB int64) memfit.Verdict {
	return memfit.Evaluate(fitBoundarySpec(),
		memfit.Request{Ctx: ctx, KVType: MemfitKVType(kvType)},
		fitBoundaryBudget(ramGB), MemfitPolicy())
}

// TestR83_FitBoundary_LiveStand — печатает таблицу и проверяет три инварианта.
func TestR83_FitBoundary_LiveStand(t *testing.T) {
	type row struct {
		ctx    int
		kvType string
		ramGB  int64
	}
	grid := []row{
		{32768, "q8_0", 20},  // текущая рабочая конфигурация стенда
		{32768, "f16", 20},   // KV вдвое больше
		{65536, "q4_0", 20},  // q4_0 экономит KV вдвое против q8_0
		{131072, "q4_0", 20}, // верхняя граница «ещё влезает»
		{262144, "q4_0", 20}, // обучающий контекст модели
		{32768, "q8_0", 12},  // RAM зажата (другие контейнеры)
		{32768, "q8_0", 8},   // RAM зажата сильнее: cpu_only уже не влезает
		{32768, "q8_0", 6},   // гарантированный дефицит RAM
	}

	for _, g := range grid {
		v := fitBoundaryEval(g.ctx, g.kvType, g.ramGB)
		t.Logf("ctx=%-7d kv=%-5s ram=%-3dGB → stage=%-15s fits=%-5v gpu_layers=%-3d "+
			"max_hard_n_ctx=%-7d kv/token=%-6d cpu_needed=%s avail_ram=%s",
			g.ctx, g.kvType, g.ramGB, v.Stage, v.Fits(), v.GPULayers,
			v.MaxHardCtx, v.KVPerToken, v.CPUWeights+v.CPUKV, v.UsableRAM)
	}

	// Инвариант 1: живая рабочая конфигурация (32768/q8_0/20 GB) обязана
	// ВЛЕЗАТЬ. Если это перестанет выполняться — предложение об отказе
	// немедленно превратилось бы в регресс «модель больше не грузится».
	live := fitBoundaryEval(32768, "q8_0", 20)
	if !live.Fits() {
		t.Fatalf("живая конфигурация стенда перестала влезать: %s", live.String())
	}
	if live.Stage != memfit.StagePartial {
		t.Errorf("живая конфигурация: stage=%s, want partial_offload "+
			"(модель 16.4 GB не влезает в 6 GB VRAM целиком)", live.Stage)
	}

	// Инвариант 2: при жёстком дефиците RAM наступает does_not_fit — вот случай,
	// где отказ обоснован: llama.cpp всё равно не сможет аллоцировать CPU-часть.
	tight := fitBoundaryEval(32768, "q8_0", 6)
	if tight.Fits() {
		t.Errorf("при 6 GB RAM 16.4 GB модель обязана не влезать: %s", tight.String())
	}

	// Инвариант 3: does_not_fit обязан называть потолок (иначе suggestion
	// отказа будет без числа) и причину RAM.
	if tight.MaxHardCtx != 0 {
		t.Logf("при 6 GB RAM max_hard_n_ctx=%d", tight.MaxHardCtx)
	}
	if !tight.HasReason(memfit.ReasonRAMShort) {
		t.Errorf("нет причины RAM-дефицита: %s", tight.String())
	}
}

// TestR83_FitBoundary_MaxHardCtxIsUsableCeiling — MaxHardCtx — это именно тот
// n_ctx, который ЕЩЁ влезает, и на нём вердикт обязан быть fits (иначе
// suggestion отказал бы в выполнимом запросе).
func TestR83_FitBoundary_MaxHardCtxIsUsableCeiling(t *testing.T) {
	for _, kvType := range []string{"q4_0", "q8_0"} {
		v := fitBoundaryEval(262144, kvType, 20)
		if v.MaxHardCtx <= 0 {
			t.Errorf("%s: MaxHardCtx=%d — потолок неизвестен", kvType, v.MaxHardCtx)
			continue
		}
		atCeiling := fitBoundaryEval(v.MaxHardCtx, kvType, 20)
		if !atCeiling.Fits() {
			t.Errorf("%s: на самом потолке MaxHardCtx=%d вердикт не fits (%s) — "+
				"suggestion отказал бы в выполнимом n_ctx",
				kvType, v.MaxHardCtx, atCeiling.Stage)
		}
		t.Logf("%s: потолок n_ctx на живом стенде = %d (при ctx=262144 → %s, gpu_layers=%d)",
			kvType, v.MaxHardCtx, v.Stage, v.GPULayers)
	}
}
