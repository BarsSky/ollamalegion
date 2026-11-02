// mmap_policy_r396_test.go — R396 (2026-11-04).
//
// ЖИВОЙ СЛУЧАЙ (A10): Qwen3.8-27B-UD-Q4_K_M (16.4 GB), RAM 20 GB, запрос
// n_ctx=131072 с kvCacheType=q4_0, useMmap включён в WebUI — загрузка отклонялась:
//
//	422 n_ctx_infeasible: requested n_ctx=131072 exceeds the physically feasible
//	max 82824 (max_vram_n_ctx=0, max_ram_n_ctx=82824, gguf_max_context=262144)
//
// Причина: предикат RAM требовал, чтобы в RAM поместились И веса модели, И KV.
// С mmap веса — файловый кэш (страницы вытесняются), поэтому резидентными в RAM
// обязаны быть только KV и буферы.
package memfit

import (
	"strings"
	"testing"
)

// r396Spec — та же форма, что у 27B: 65 слоёв, 17 из них с KV (гибрид),
// head_dim 256 (attention.key_length), обучающий контекст 262144.
func r396Spec() ModelSpec {
	return ModelSpec{
		Name:      "Qwen3.8-27B-UD-Q4_K_M",
		SizeBytes: MiBOf(15696), // ~16.4 GB
		NLayers:   65,
		KVLayers:  17,
		NHeads:    40,
		NKvHeads:  8,
		NEmbd:     5120,
		KVHeadDim: 256,
		TrainCtx:  262144,
	}
}

// r396Budget — снимок A10 из живого отказа: VRAM свободно 6054 MB (резерв 2 GiB),
// RAM available 18573 MB из 20422 MB (резерв 4 GiB).
func r396Budget() Budget {
	return Budget{
		VRAMFree:    MiBOf(6054),
		VRAMTotal:   MiBOf(22502),
		VRAMKnown:   true,
		RAMAvail:    MiBOf(18573),
		RAMTotal:    MiBOf(20422),
		RAMKnown:    true,
		VRAMReserve: MiBOf(2048),
		RAMReserve:  MiBOf(4096),
		Source:      "test",
	}
}

// TestR396_MmapDecidesRAMFit — один и тот же запрос: без mmap не влезает,
// с mmap влезает. Именно это и было дефектом: режим mmap не учитывался.
func TestR396_MmapDecidesRAMFit(t *testing.T) {
	spec := r396Spec()
	budget := r396Budget()
	const ctx = 131072

	noMmap := Evaluate(spec, Request{Ctx: ctx, KVType: KVQ4},
		budget, mmapPolicyR396(false))
	if noMmap.Fits() {
		t.Fatalf("без mmap загрузка 16.4 GB при RAM 20 GB и ctx=%d не должна считаться выполнимой "+
			"(stage=%s, hard=%d) — иначе тест перестал проверять исходный дефект",
			ctx, noMmap.Stage, noMmap.MaxHardCtx)
	}
	if noMmap.Mmap {
		t.Error("без mmap вердикт не должен помечать веса как file-backed")
	}

	withMmap := Evaluate(spec, Request{Ctx: ctx, KVType: KVQ4},
		budget, mmapPolicyR396(true))
	if !withMmap.Fits() {
		t.Fatalf("с mmap веса читаются с диска, в RAM обязаны поместиться только KV и буферы — "+
			"загрузка обязана быть выполнимой: stage=%s hard=%d cpuWeights=%d cpuKV=%d usableRAM=%d",
			withMmap.Stage, withMmap.MaxHardCtx,
			withMmap.CPUWeights.MiB(), withMmap.CPUKV.MiB(), withMmap.UsableRAM.MiB())
	}
	if !withMmap.Mmap {
		t.Error("вердикт обязан пометить mmap: по этому полю диагностика объясняет, почему веса не считались")
	}
	if withMmap.Stage != StagePartial && withMmap.Stage != StageCPUOnly {
		t.Errorf("stage=%s: при mmap и весах больше VRAM ожидался partial/cpu_only", withMmap.Stage)
	}
	if withMmap.MaxHardCtx < ctx {
		t.Errorf("max_hard_n_ctx=%d < запрошенных %d при mmap", withMmap.MaxHardCtx, ctx)
	}
}

// TestR396_MmapDoesNotHelpKV — с mmap освобождаются только ВЕСА. KV по-прежнему
// обязан поместиться в RAM: иначе проверка превратилась бы в «разрешить всё».
func TestR396_MmapDoesNotHelpKV(t *testing.T) {
	spec := r396Spec()

	// Урезаем RAM: веса уже не считаются (mmap), значит отказ может дать только
	// KV — именно это и проверяем.
	tight := r396Budget()
	tight.RAMAvail = MiBOf(10000) // usable RAM = 10000 − 4096 = 5904 MB
	huge := Evaluate(spec, Request{Ctx: spec.TrainCtx, KVType: KVQ4},
		tight, mmapPolicyR396(true))
	if huge.Fits() {
		t.Fatalf("ctx=%d: KV не помещается в RAM даже без весов — отказ обязателен (stage=%s, cpuKV=%d MB, usableRAM=%d MB)",
			spec.TrainCtx, huge.Stage, huge.CPUKV.MiB(), huge.UsableRAM.MiB())
	}
}

// TestR396_MmapCeilings — потолки (Ceilings) обязаны считаться по тому же
// предикату: без mmap «доступно до X», с mmap — заметно выше, но не бесконечно.
func TestR396_MmapCeilings(t *testing.T) {
	spec := r396Spec()
	budget := r396Budget()

	_, hardNoMmap := Ceilings(spec, KVQ4, budget, mmapPolicyR396(false))
	_, hardMmap := Ceilings(spec, KVQ4, budget, mmapPolicyR396(true))

	if hardNoMmap <= 0 || hardMmap <= 0 {
		t.Fatalf("потолки не посчитаны: без mmap=%d, с mmap=%d", hardNoMmap, hardMmap)
	}
	if hardMmap <= hardNoMmap {
		t.Errorf("с mmap потолок RAM обязан вырасти: было %d, стало %d", hardNoMmap, hardMmap)
	}
	if hardMmap > spec.TrainCtx {
		t.Errorf("потолок %d выше обучающего контекста %d — проверка перестала ограничивать", hardMmap, spec.TrainCtx)
	}
}

// TestR396_VerdictStringShowsMmap — по строке вердикта оператор должен видеть
// режим весов без чтения кода.
func TestR396_VerdictStringShowsMmap(t *testing.T) {
	budget := r396Budget()
	v := Evaluate(r396Spec(), Request{Ctx: 32768, KVType: KVQ4},
		budget, mmapPolicyR396(true))
	if s := v.String(); !strings.Contains(s, "mmap_weights=true") {
		t.Errorf("в строке вердикта нет режима весов: %q", s)
	}
}

// mmapPolicyR396 — политика в том же виде, в каком её собирает cppbackend
// (MemfitPolicyForMmap): единственное отличие — режим весов.
func mmapPolicyR396(mmapWeights bool) Policy {
	p := DefaultPolicy()
	p.WeightsMmap = mmapWeights
	return p
}
