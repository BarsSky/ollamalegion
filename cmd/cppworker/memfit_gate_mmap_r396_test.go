//go:build llama_stub

// memfit_gate_mmap_r396_test.go — R396 (2026-11-04).
//
// ЖИВОЙ ОТКАЗ (A10): загрузка Qwen3.8-27B-UD-Q4_K_M с n_ctx=131072 и kv=q4_0
// отвергалась гейтом:
//
//	422 n_ctx_infeasible: requested n_ctx=131072 exceeds the physically feasible
//	max 82824 (max_vram_n_ctx=0, max_ram_n_ctx=82824, gguf_max_context=262144)
//
// при включённом в WebUI useMmap. Причина — гейт не знал про режим весов и
// требовал, чтобы в RAM поместились И веса (16.4 GB), И KV.
//
// Тест проверяет ровно то, что чинит R396: флаг mmap доходит до вердикта и
// меняет решение; KV по-прежнему обязан помещаться.
package main

import (
	"testing"

	"ollama-loadbalancer/internal/memfit"
)

// a10BudgetR396 — снимок живого отказа: VRAM 22502 MB (свободно 6054),
// RAM 20422 MB (доступно 18573), резервы как в MemfitPolicy.
func a10BudgetR396() memfit.Budget {
	return memfit.Budget{
		VRAMTotal:   memfit.MiBOf(22502),
		VRAMFree:    memfit.MiBOf(6054),
		VRAMKnown:   true,
		VRAMReserve: memfit.MiBOf(2048),
		RAMTotal:    memfit.MiBOf(20422),
		RAMAvail:    memfit.MiBOf(18573),
		RAMKnown:    true,
		RAMReserve:  memfit.MiBOf(4096),
		Source:      "test:a10-live",
	}
}

// TestR396_GateMmapFlipsDecision — без mmap отказ, с mmap — выполнимо.
//
// KV здесь q8_0 (34816 Б/токен у этого файла): на живом стенде отказ пришёл
// именно с такими числами (в memory_breakdown было ~34 КБ/токен). На q4_0
// (18432 Б/токен) 131072 на 20 ГБ RAM влезает и без mmap — там дефект не виден.
func TestR396_GateMmapFlipsDecision(t *testing.T) {
	spec := qwen38SpecR83()
	budget := a10BudgetR396()
	const requested = 131072

	fNo, _, okNo := feasibilityFromMemfit(spec, budget, "q8_0", requested, false)
	if !okNo {
		t.Fatal("вердикт без mmap не построен")
	}
	if fNo.Stage != nctxStageInfeasible {
		t.Fatalf("без mmap stage=%s (feasible=%d, hard=%d): ожидался отказ — иначе тест не проверяет исходный дефект",
			fNo.Stage, fNo.FeasibleMaxNCtx, fNo.HardMaxNCtx)
	}

	fYes, vYes, okYes := feasibilityFromMemfit(spec, budget, "q8_0", requested, true)
	if !okYes {
		t.Fatal("вердикт с mmap не построен")
	}
	if fYes.Stage == nctxStageInfeasible {
		t.Fatalf("с mmap гейт всё ещё отказывает: stage=%s feasible=%d hard=%d cpuKV=%d usableRAM=%d — "+
			"веса читаются с диска, в RAM обязаны поместиться только KV и буферы",
			fYes.Stage, fYes.FeasibleMaxNCtx, fYes.HardMaxNCtx,
			vYes.CPUKV.MiB(), vYes.UsableRAM.MiB())
	}
	if fYes.HardMaxNCtx < requested {
		t.Errorf("hard-потолок с mmap=%d меньше запрошенных %d", fYes.HardMaxNCtx, requested)
	}
	if !vYes.Mmap {
		t.Error("вердикт обязан нести признак mmap: по нему строится пояснение в 422 и логе")
	}

	// Раскладка в ответе гейта обязана объяснять режим весов: именно её читает
	// оператор в 422 (memory_breakdown.mmap_weights).
	if fYes.Breakdown == nil {
		t.Fatal("у вердикта нет раскладки памяти")
	}
	fields := fYes.Breakdown.Fields()
	if got, _ := fields["mmap_weights"].(bool); !got {
		t.Errorf("memory_breakdown.mmap_weights = %v, want true", fields["mmap_weights"])
	}
}

// TestR396_GateMmapStillRejectsKVOverflow — mmap снимает требование к весам, но
// не к KV: урезанная RAM обязана дать отказ.
func TestR396_GateMmapStillRejectsKVOverflow(t *testing.T) {
	spec := qwen38SpecR83()
	budget := a10BudgetR396()
	budget.RAMAvail = memfit.MiBOf(9000) // usable RAM = 9000 − 4096 = 4904 MB

	f, v, ok := feasibilityFromMemfit(spec, budget, "q8_0", spec.TrainCtx, true)
	if !ok {
		t.Fatal("вердикт не построен")
	}
	if f.Stage != nctxStageInfeasible {
		t.Fatalf("ctx=%d при usable RAM %d MB и cpuKV %d MB обязан быть отказом, получено stage=%s",
			spec.TrainCtx, v.UsableRAM.MiB(), v.CPUKV.MiB(), f.Stage)
	}
}
