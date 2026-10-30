package cppbackend

// resource_limits_r306_test.go — R306 (2026-10-30): потолок окна для ЗАГРУЖЕННОЙ
// модели.
//
// ЖИВОЙ ФАКТ (стенд, gemma-4-E4B, ctx 32768, модель работает и отвечает):
// /api/models отдавал max_vram_n_ctx=0 при vram_known=true и
// gguf_max_context=131072. По контракту поля это «веса не влезают ни при каком
// gpu_layers» — ложь: модель в памяти и обслуживает запросы.
//
// ПРИЧИНА. Ceilings считает потолок как (свободная VRAM − размер весов) / KV на
// токен, то есть предполагает, что модель ещё НЕ загружена. У загруженной модели
// веса уже вычтены из свободной VRAM, и Ceilings вычитает их второй раз:
// свободных 2.5 GB при весах 4.2 GB давали потолок 0 при любом окне.
//
// Следствие было не косметическим: отказ по окну (413) не мог сослаться на
// физический предел, и оператор не понимал, поможет ли поднять
// LB_NCTX_RELOAD_MAX_N_CTX.

import (
	"testing"

	"ollama-loadbalancer/internal/memfit"
)

func r306GemmaMeta() *GGUFModelMeta {
	return &GGUFModelMeta{
		Filename: "gemma-4-E4B-it-Q4_K_M", Path: "/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
		SizeBytes: 4215695776, NLayers: 42, NEmbd: 2560, NHeads: 8, NKvHeads: 2, ContextLength: 131072,
	}
}

// r306Budget — 8 GB карта, из которых 2.5 GB свободно: ровно состояние стенда с
// уже загруженной gemma (веса 4.2 GB занимают остальное).
func r306Budget() memfit.Budget {
	return memfit.Budget{
		VRAMFree:  memfit.MiBOf(2500),
		VRAMTotal: memfit.MiBOf(8192),
		VRAMKnown: true,
		RAMAvail:  memfit.MiBOf(32 * 1024),
		RAMTotal:  memfit.MiBOf(32 * 1024),
		RAMKnown:  true,
	}
}

// TestR306_LoadedModelCeilingIsNotZero — главный случай: модель загружена, свободной
// VRAM меньше её весов, но потолок окна обязан быть посчитан, а не обнулён.
func TestR306_LoadedModelCeilingIsNotZero(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir(), DefaultCtxSize: 4096, DefaultBatchSize: 512})
	name := "gemma-4-E4B-it-Q4_K_M"
	b.ModelManager().ggufFiles[name] = r306GemmaMeta()
	b.mu.Lock()
	b.models[name] = &modelInstance{info: ModelInfo{
		Name: name, State: StateLoaded, ContextSize: 32768, NLayers: 42, GPULayers: 42,
	}}
	b.mu.Unlock()

	limits := b.calculateResourceLimits(name, r306Budget())
	if limits.MaxVRAMNCtx <= 0 {
		t.Fatalf("max_vram_n_ctx = %d при загруженной модели: "+
			"по контракту поля это «веса не влезают ни при каком gpu_layers», а модель работает",
			limits.MaxVRAMNCtx)
	}
	// Верхняя граница — обучающий контекст модели: потолок не может быть выше.
	if limits.ModelMaxContext > 0 && limits.MaxVRAMNCtx > limits.ModelMaxContext {
		t.Errorf("max_vram_n_ctx = %d выше обучающего контекста %d",
			limits.MaxVRAMNCtx, limits.ModelMaxContext)
	}
}

// TestR306_RestoredBudgetCoversWorkingWindow — механизм на реальных параметрах KV.
//
// Синтетические метаданные в тесте выше не описывают sliding-window attention
// gemma (у неё KV растёт не по всем 42 слоям), поэтому точное число там заведомо
// пессимистичнее стенда. Здесь KV задан явно (8 KV-слоёв, head_dim 128 — как у
// gemma-3/4), и проверяется главное: с ВОЗВРАЩЁННЫМИ весами в бюджет окно 32768
// помещается целиком в VRAM. До правки бюджет вычитал веса дважды, и потолок
// получался 0 при любом окне.
func TestR306_RestoredBudgetCoversWorkingWindow(t *testing.T) {
	spec := MemfitSpecFromValues("gemma-4-E4B-it-Q4_K_M", 4215695776, 42, 8, 2, 2560, 131072, 8, 128)
	budget := r306Budget()

	// До правки: бюджет как есть (веса уже заняты моделью).
	before, _ := memfit.Ceilings(spec, memfit.KVF16, budget, MemfitPolicy())
	if before != 0 {
		t.Logf("до правки потолок = %d (на этом бюджете веса уже не влезают)", before)
	}

	// После правки: веса возвращены в свободную VRAM.
	restored := budget
	restored.VRAMFree += spec.SizeBytes
	after, _ := memfit.Ceilings(spec, memfit.KVF16, restored, MemfitPolicy())
	if after < 32768 {
		t.Fatalf("с возвращёнными весами потолок = %d < 32768: окно, на котором модель "+
			"уже работает, объявляется невместимым", after)
	}
}

// TestR306_UnloadedModelCeilingUnchanged — контроль: для НЕзагруженной модели
// расчёт прежний (веса вычитаются из свободной VRAM один раз).
func TestR306_UnloadedModelCeilingUnchanged(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir(), DefaultCtxSize: 4096, DefaultBatchSize: 512})
	name := "gemma-4-E4B-it-Q4_K_M"
	b.ModelManager().ggufFiles[name] = r306GemmaMeta()

	limits := b.calculateResourceLimits(name, r306Budget())
	if limits.MaxVRAMNCtx != 0 {
		t.Errorf("для незагруженной модели max_vram_n_ctx = %d, want 0: "+
			"свободных 2.5 GB меньше весов 4.2 GB — окно целиком в VRAM не влезает",
			limits.MaxVRAMNCtx)
	}
}

// TestR306_RestoredWeightsRespectPartialOffload — возвращаем только ту часть весов,
// что реально на GPU: при частичной выгрузке в RAM полный размер файла вернул бы в
// бюджет память, которой нет, и потолок стал бы обещанием.
func TestR306_RestoredWeightsRespectPartialOffload(t *testing.T) {
	spec := MemfitSpecFromValues("m", 4215695776, 42, 8, 2, 2560, 131072, 42, 128)

	full := &ModelInfo{Name: "m", State: StateLoaded, NLayers: 42, GPULayers: 42}
	if got := restoredWeightsBytesR306(spec, full); got != spec.SizeBytes {
		t.Errorf("полный оффлоад: вернули %s, ожидался размер весов %s", got, spec.SizeBytes)
	}

	half := &ModelInfo{Name: "m", State: StateLoaded, NLayers: 42, GPULayers: 21}
	if got := restoredWeightsBytesR306(spec, half); got != spec.SizeBytes.Scale(21, 42) {
		t.Errorf("половинный оффлоад: вернули %s, ожидалась половина весов", got)
	}

	cpu := &ModelInfo{Name: "m", State: StateLoaded, NLayers: 42, GPULayers: 0}
	if got := restoredWeightsBytesR306(spec, cpu); got != 0 {
		t.Errorf("ноль слоёв на GPU: вернули %s, ожидался 0 (веса целиком в RAM)", got)
	}

	if got := restoredWeightsBytesR306(spec, nil); got != 0 {
		t.Errorf("незагруженная модель: вернули %s, ожидался 0", got)
	}
}