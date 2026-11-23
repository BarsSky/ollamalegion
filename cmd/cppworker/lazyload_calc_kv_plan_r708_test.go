// lazyload_calc_kv_plan_r708_test.go — R708 (2026-11-23): CPU-ветка
// calculateLazyLoadOpts считает KV по РАСКЛАДКЕ (cppbackend.KVPlan), а не
// плоской формулой «все слои × одна ширина × f16».
//
// ПОЧЕМУ ЭТО НЕ КОСМЕТИКА. На gemma-4-E4B плоская формула даёт на n_ctx=32768
// 3072 MiB KV (24 слоя × 512), тогда как по раскладке их 532 MiB: 4 глобальных
// слоя шириной 512 растут с окном, а 20 оконных шириной 256 живут в окне 512
// токенов и после него НЕ растут. Завышенный KV двигал потолок CPU-only с
// ~67532 токенов до ~22937, то есть окно 32768 молча урезалось, а на тесной
// карте модель уходила в CPU-only — ровно тот класс дефекта, который R707
// исправил в остальных путях (feasible, memfit, /api/models).
//
// ЧТО ПРОВЕРЯЕТСЯ:
//  1. CPU-ветка использует план: KV заметно меньше плоского, потолок окна
//     выше, «источник» — plan (виден в rationale);
//  2. без плана (старый воркер/битые метаданные) — ПРЕЖНЯЯ плоская формула и
//     «источник» flat: занижать требование к памяти нельзя;
//  3. направление консервативное: плоский план (архитектура без классов слоёв)
//     даёт ровно прежнее число, а неполный план отвергается, а не досчитывается
//     «на глазок».
package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// writeGGUFKVBlockR708 — словарь GGUF из map[string]interface{}.
//
// Типы: string (GGUF 8), uint32/int (GGUF 4), []bool (GGUF 9/7 — так llama.cpp
// хранит <arch>.attention.sliding_window_pattern), []uint32 (GGUF 9/4 — массив
// <arch>.attention.head_count_kv). Формат тот же, что в internal/cppbackend
// (kv_plan_feasible_r707_test.go:writeGGUFR707) — две копий писать не хочется,
// но тестовые помощники не экспортируются между пакетами.
func writeGGUFKVBlockR708(t *testing.T, path string, kv map[string]interface{}) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fake gguf: %v", err)
	}
	defer f.Close()
	w := binary.LittleEndian
	must := func(err error) {
		if err != nil {
			t.Fatalf("write fake gguf: %v", err)
		}
	}
	str := func(s string) {
		must(binary.Write(f, w, uint64(len(s))))
		_, err := f.Write([]byte(s))
		must(err)
	}
	keys := make([]string, 0, len(kv))
	if _, ok := kv["general.architecture"]; ok {
		keys = append(keys, "general.architecture")
	}
	for k := range kv {
		if k != "general.architecture" {
			keys = append(keys, k)
		}
	}
	must(binary.Write(f, w, []byte("GGUF")))
	must(binary.Write(f, w, uint32(3)))
	must(binary.Write(f, w, uint64(0)))
	must(binary.Write(f, w, uint64(len(keys))))
	for _, k := range keys {
		str(k)
		switch v := kv[k].(type) {
		case string:
			must(binary.Write(f, w, uint32(8)))
			str(v)
		case uint32:
			must(binary.Write(f, w, uint32(4)))
			must(binary.Write(f, w, v))
		case int:
			must(binary.Write(f, w, uint32(4)))
			must(binary.Write(f, w, uint32(v)))
		case []bool:
			must(binary.Write(f, w, uint32(9)))
			must(binary.Write(f, w, uint32(7)))
			must(binary.Write(f, w, uint64(len(v))))
			for _, b := range v {
				var x uint8
				if b {
					x = 1
				}
				must(binary.Write(f, w, x))
			}
		case []uint32:
			must(binary.Write(f, w, uint32(9)))
			must(binary.Write(f, w, uint32(4)))
			must(binary.Write(f, w, uint64(len(v))))
			for _, x := range v {
				must(binary.Write(f, w, x))
			}
		default:
			t.Fatalf("writeGGUFKVBlockR708: тип %T для ключа %q не поддержан", v, k)
		}
	}
}

// gemma4KeysR708 — ключи gemma-4-E4B, снятые с реального файла стенда
// (D:\ollama-legion-models\gemma-4-E4B-it-Q4_K_M.gguf, `-feasible`):
//
//	plan: global 4 × (512+512) × 2 kv-heads + swa 20 × (256+256) × 2 kv-heads,
//	      окно 512; всего слоёв с кэшем 24 из 42 (shared_kv_layers = 18).
func gemma4KeysR708() map[string]interface{} {
	// sliding_window_pattern: '1' = окно. У gemma-4 глобальные — каждый 6-й
	// слой (индексы 5, 11, 17, 23, …), поэтому 4 глобальных из 24 с кэшем.
	pattern := make([]bool, 0, 42)
	for i := 0; i < 42; i++ {
		pattern = append(pattern, (i+1)%6 != 0)
	}
	return map[string]interface{}{
		"general.architecture":                    "gemma4",
		"gemma4.block_count":                      uint32(42),
		"gemma4.embedding_length":                 uint32(2560),
		"gemma4.attention.head_count":             uint32(8),
		"gemma4.attention.head_count_kv":          uint32(2),
		"gemma4.attention.key_length":             uint32(512),
		"gemma4.attention.value_length":           uint32(512),
		"gemma4.attention.key_length_swa":         uint32(256),
		"gemma4.attention.value_length_swa":       uint32(256),
		"gemma4.attention.sliding_window":         uint32(512),
		"gemma4.attention.sliding_window_pattern": pattern,
		"gemma4.attention.shared_kv_layers":       uint32(18),
		"gemma4.context_length":                   uint32(131072),
	}
}

// setupBackendWithKeysR708 — backend над каталогом с одним fake GGUF и
// подменённым размером файла (веса нужны большими, а сам header — маленький).
func setupBackendWithKeysR708(t *testing.T, modelName string, kv map[string]interface{}, sizeBytes int64) *cppbackend.Backend {
	t.Helper()
	dir := t.TempDir()
	writeGGUFKVBlockR708(t, filepath.Join(dir, modelName+".gguf"), kv)

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
	mm := be.ModelManager()
	if mm == nil {
		t.Fatalf("ModelManager вернул nil")
	}
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	// Ленивое чтение header + подмена размера: размер весов влияет на выбор
	// стадии (веса не влезают в VRAM), но в файле его держать незачем.
	meta, err := mm.GetModelMeta(modelName + ".gguf")
	if err != nil || meta == nil {
		t.Fatalf("GetModelMeta: %v", err)
	}
	meta.SizeBytes = sizeBytes

	backend = be
	t.Cleanup(func() { backend = nil })
	return be
}

// cpuOnlyBudgetR708 — тот же бюджет, который считает Stage 3: safeVRAM
// (0.85 от свободной VRAM) минус накладные 1.5 GiB.
func cpuOnlyBudgetR708(availableVRAM int64) int64 {
	safeVRAM := int64(float64(availableVRAM) * 0.85)
	return safeVRAM - int64(1536)*1024*1024
}

// TestR708_CPUOnlyUsesKVPlan_Gemma4 — главный тест пункта A:
// «CPU-ветка использует план» (gemma-4: KV заметно меньше плоской).
func TestR708_CPUOnlyUsesKVPlan_Gemma4(t *testing.T) {
	const vram = int64(3) * 1024 * 1024 * 1024 // тесная карта: веса 4.2 GB не влезают
	setEnvLazy(t, "CPPWORKER_VRAM_BYTES", "3221225472")
	setEnvLazy(t, "CPPWORKER_FREE_VRAM_BYTES", "3221225472")
	setEnvLazy(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "34359738368")
	setEnvLazy(t, "CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

	be := setupBackendWithKeysR708(t, "gemma-4-E4B-it-Q4_K_M", gemma4KeysR708(),
		4*1024*1024*1024+200*1024*1024)

	// Ожидаемые числа — из плана, посчитанного тем же KVPlan, что отдаёт
	// /api/models: 4 глобальных слоя × (512+512) × 2 kv-heads + 20 оконных
	// × (256+256) × 2 kv-heads, окно 512.
	plan, ok := be.KVPlanForModel("gemma-4-E4B-it-Q4_K_M")
	if !ok {
		t.Fatal("KVPlanForModel не нашёл раскладку gemma-4 по метаданным")
	}
	if plan.Source != cppbackend.KVPlanSourcePlan {
		t.Fatalf("источник плана = %q, want %q", plan.Source, cppbackend.KVPlanSourcePlan)
	}
	if plan.KVLayers() != 24 || plan.GlobalLayers != 4 || plan.SWALayers != 20 || plan.SWAWindow != 512 {
		t.Fatalf("раскладка gemma-4: всего %d g=%d swa=%d окно=%d, want 24/4/20/512",
			plan.KVLayers(), plan.GlobalLayers, plan.SWALayers, plan.SWAWindow)
	}
	// Абсолютные числа (f16, K=V): global 4·2·(512+512)·2 = 16384 Б/токен,
	// swa 20·2·(256+256)·2 = 40960 Б/токен, итого 57344 Б/токен.
	wantKVPerToken := int64(57344)
	if got := plan.KVPerTokenBytes(""); got != wantKVPerToken {
		t.Fatalf("KV на токен по плану = %d, want %d", got, wantKVPerToken)
	}

	opts, rationale := calculateLazyLoadOpts(
		"gemma-4-E4B-it-Q4_K_M", 32768, -1, &cppbackend.Config{DefaultGPULayers: 32})

	t.Logf("rationale: %s", rationale.FormatRationale())

	if rationale.Source != "reduced_nctx" {
		t.Fatalf("стадия = %q, want reduced_nctx (веса не влезают в VRAM): %s",
			rationale.Source, rationale.FormatRationale())
	}
	// 1. ЧЕМ посчитан KV — сказано в rationale (plan/flat публикуются).
	if rationale.KVPlanSource != cppbackend.KVPlanSourcePlan {
		t.Fatalf("kv_source = %q, want plan: CPU-ветка снова считает плоской формулой (%s)",
			rationale.KVPlanSource, rationale.FormatRationale())
	}
	if rationale.KVPerTokenBytes != wantKVPerToken {
		t.Fatalf("kv_per_token = %d, want %d (по раскладке)", rationale.KVPerTokenBytes, wantKVPerToken)
	}
	if rationale.KVLayers != 24 || rationale.KVGlobalLayers != 4 ||
		rationale.KVSWALayers != 20 || rationale.KVSWAWindow != 512 {
		t.Fatalf("раскладка в rationale: layers=%d g=%d swa=%d окно=%d, want 24/4/20/512",
			rationale.KVLayers, rationale.KVGlobalLayers, rationale.KVSWALayers, rationale.KVSWAWindow)
	}

	// 2. KV на запрошенном окне по плану — заметно меньше плоского.
	flatKVBytes := estimateKVCacheBytesForLayers(32768, 24, 2, 512, "")
	planKVBytes := plan.KVBytesAtCtx("", 32768)
	if planKVBytes <= 0 {
		t.Fatal("KVBytesAtCtx = 0")
	}
	if want := planKVBytes / (1024 * 1024); rationale.EstimatedKVCacheMB != want {
		t.Fatalf("EstimatedKVCacheMB = %d, want %d (KV на 32768 по плану)", rationale.EstimatedKVCacheMB, want)
	}
	if planKVBytes*3 > flatKVBytes {
		t.Fatalf("KV по плану %d Б не «заметно меньше» плоского %d Б — раскладка не применилась",
			planKVBytes, flatKVBytes)
	}
	t.Logf("KV на 32768: план %d MiB против плоского %d MiB (в %d раз меньше)",
		planKVBytes/(1024*1024), flatKVBytes/(1024*1024), flatKVBytes/planKVBytes)

	// 3. Потолок окна выше плоского — и запрошенные 32768 сохраняются.
	budget := cpuOnlyBudgetR708(vram)
	wantMax := plan.MaxCtxForBudget("", budget, 0)
	if rationale.MaxViableNCtx != wantMax {
		t.Fatalf("MaxViableNCtx = %d, want %d (обратная формула memfit.KVShape.MaxCtxForBudget)",
			rationale.MaxViableNCtx, wantMax)
	}
	flatMax := int(budget / (flatKVBytes / 32768))
	if wantMax < flatMax*2 {
		t.Fatalf("потолок по плану %d не вырос относительно плоского %d — окно снова режется зря",
			wantMax, flatMax)
	}
	t.Logf("потолок n_ctx: план %d против плоского %d", wantMax, flatMax)

	if opts.ContextSize != 32768 {
		t.Fatalf("ContextSize = %d, want 32768: по раскладке окно влезает, урезать не за что (%s)",
			opts.ContextSize, rationale.FormatRationale())
	}
	if opts.GPULayers != 0 || !opts.UseMmap {
		t.Fatalf("CPU-ветка: gpu_layers=%d mmap=%v, want 0/true", opts.GPULayers, opts.UseMmap)
	}
}

// TestR708_CPUOnlyWithoutPlanFallsBackToFlat — «без плана — прежняя формула»:
// KV-раскладки нет (старый воркер/битый header), поэтому KV и потолок окна
// считаются ровно как до R708, и это НАЗВАНО (flat).
func TestR708_CPUOnlyWithoutPlanFallsBackToFlat(t *testing.T) {
	const vram = int64(3) * 1024 * 1024 * 1024
	setEnvLazy(t, "CPPWORKER_VRAM_BYTES", "3221225472")
	setEnvLazy(t, "CPPWORKER_FREE_VRAM_BYTES", "3221225472")
	setEnvLazy(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "34359738368")
	setEnvLazy(t, "CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

	setupBackendWithKeysR708(t, "gemma-4-noplan", gemma4KeysR708(),
		4*1024*1024*1024+200*1024*1024)

	// Подменяем источник раскладки: «плана нет». На живом стенде это состояние
	// старых воркеров; без подмены запасной путь нечем проверить.
	original := kvPlanForLazyLoad
	kvPlanForLazyLoad = func(string) (cppbackend.KVPlan, bool) { return cppbackend.KVPlan{}, false }
	t.Cleanup(func() { kvPlanForLazyLoad = original })

	opts, rationale := calculateLazyLoadOpts(
		"gemma-4-noplan", 32768, -1, &cppbackend.Config{DefaultGPULayers: 32})
	t.Logf("rationale: %s", rationale.FormatRationale())

	if rationale.KVPlanSource != cppbackend.KVPlanSourceFlat {
		t.Fatalf("kv_source = %q, want flat (плана нет): %s", rationale.KVPlanSource, rationale.FormatRationale())
	}
	if rationale.Source != "reduced_nctx" {
		t.Fatalf("стадия = %q, want reduced_nctx", rationale.Source)
	}

	// Прежняя формула: плоский KV на 32768 = 2·24·2·512 байт/токен.
	flatKVBytes := estimateKVCacheBytesForLayers(32768, 24, 2, 512, "")
	if want := flatKVBytes / (1024 * 1024); rationale.EstimatedKVCacheMB != want {
		t.Fatalf("EstimatedKVCacheMB = %d, want %d (прежняя плоская формула)",
			rationale.EstimatedKVCacheMB, want)
	}
	flatPerToken := flatKVBytes / 32768
	if rationale.KVPerTokenBytes != flatPerToken {
		t.Fatalf("kv_per_token = %d, want %d (плоская формула, все 24 слоя шириной 512)",
			rationale.KVPerTokenBytes, flatPerToken)
	}
	budget := cpuOnlyBudgetR708(vram)
	wantMax := int(budget / flatPerToken)
	if rationale.MaxViableNCtx != wantMax {
		t.Fatalf("MaxViableNCtx = %d, want %d (бюджет / KV на токен — прежнее поведение)",
			rationale.MaxViableNCtx, wantMax)
	}
	if opts.ContextSize != wantMax {
		t.Fatalf("ContextSize = %d, want %d (окно урезано по плоской оценке)", opts.ContextSize, wantMax)
	}
	// Не-вакуумность: без плана окно РЕЖЕТСЯ, с планом — нет. Если бы правка
	// ничего не меняла, эти два теста совпадали бы.
	if wantMax >= 32768 {
		t.Fatalf("плоский потолок %d ≥ 32768 — тест не различает plan/flat", wantMax)
	}
}

// TestR708_FlatPlanKeepsFlatNumbers — архитектура без раскладки по классам
// (llama: все слои глобальные) даёт ПРЕЖНЕЕ число: план есть, но он плоский.
// Это защищает от соблазна «улучшить» и плотные модели тоже.
func TestR708_FlatPlanKeepsFlatNumbers(t *testing.T) {
	setEnvLazy(t, "CPPWORKER_VRAM_BYTES", "21474836480")
	setEnvLazy(t, "CPPWORKER_FREE_VRAM_BYTES", "21474836480")
	setEnvLazy(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "34359738368")
	setEnvLazy(t, "CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

	be := setupBackendWithKeysR708(t, "llama-13b-Q4_K_M", map[string]interface{}{
		"general.architecture":          "llama",
		"llama.block_count":             uint32(40),
		"llama.embedding_length":        uint32(5120),
		"llama.attention.head_count":    uint32(40),
		"llama.attention.head_count_kv": uint32(40),
		"llama.attention.key_length":    uint32(128),
		"llama.attention.value_length":  uint32(128),
		"llama.context_length":          uint32(4096),
	}, 8*1024*1024*1024)

	plan, ok := be.KVPlanForModel("llama-13b-Q4_K_M")
	if !ok {
		t.Fatal("KVPlanForModel: план должен существовать (плоский)")
	}
	if plan.Source != cppbackend.KVPlanSourceFlat {
		t.Fatalf("источник = %q, want flat для llama без классов слоёв", plan.Source)
	}
	if plan.KVLayers() != 40 || plan.SWALayers != 0 {
		t.Fatalf("плоский план: layers=%d swa=%d, want 40/0", plan.KVLayers(), plan.SWALayers)
	}

	// Прежняя формула и план обязаны совпасть: 2 (K+V) × 40 слоёв × 40 рук × 128.
	flat := estimateKVCacheBytesForLayers(8192, 40, 40, 128, "")
	fromPlan := plan.KVBytesAtCtx("", 8192)
	if flat != fromPlan {
		t.Fatalf("плоский план %d Б ≠ прежняя формула %d Б — для плотной модели числа разъехались",
			fromPlan, flat)
	}

	kvPerToken, maxCtx, source := cpuOnlyKVBudgetR708(plan, true, nil,
		cpuOnlyBudgetR708(21474836480), flat, 8192)
	if source != cppbackend.KVPlanSourceFlat {
		t.Fatalf("источник бюджета = %q, want flat", source)
	}
	if want := flat / 8192; kvPerToken != want {
		t.Fatalf("KV на токен = %d, want %d (прежняя формула)", kvPerToken, want)
	}
	if want := int(cpuOnlyBudgetR708(21474836480) / kvPerToken); maxCtx != want {
		t.Fatalf("потолок = %d, want %d", maxCtx, want)
	}
}

// TestR708_IncompletePlanRejectedConservatively — неполная раскладка (нет
// ширин/слоёв) не используется: лучше плоская формула, чем заниженный KV.
func TestR708_IncompletePlanRejectedConservatively(t *testing.T) {
	setEnvLazy(t, "CPPWORKER_VRAM_BYTES", "3221225472")
	setEnvLazy(t, "CPPWORKER_FREE_VRAM_BYTES", "3221225472")
	setEnvLazy(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "34359738368")
	setEnvLazy(t, "CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD", "true")

	be := setupBackendWithKeysR708(t, "gemma-4-noplan", gemma4KeysR708(),
		4*1024*1024*1024+200*1024*1024)
	meta, err := be.ModelManager().GetModelMeta("gemma-4-noplan.gguf")
	if err != nil || meta == nil {
		t.Fatalf("GetModelMeta: %v", err)
	}

	original := kvPlanForLazyLoad
	t.Cleanup(func() { kvPlanForLazyLoad = original })

	// План без единого слоя с кэшем — использовать нельзя.
	kvPlanForLazyLoad = func(string) (cppbackend.KVPlan, bool) {
		return cppbackend.KVPlan{Source: cppbackend.KVPlanSourcePlan}, true
	}
	if _, ok := resolveLazyLoadKVPlan("gemma-4-noplan", meta); ok {
		t.Fatal("план без слоёв с кэшем принят — KV занизился бы до нуля")
	}

	// План со слоями, но без ширин: их подставляет meta (n_embd/n_heads), и это
	// по-прежнему ВЕРХНЯЯ оценка, а не ноль.
	kvPlanForLazyLoad = func(string) (cppbackend.KVPlan, bool) {
		return cppbackend.KVPlan{GlobalLayers: 24, Source: cppbackend.KVPlanSourcePlan}, true
	}
	plan, ok := resolveLazyLoadKVPlan("gemma-4-noplan", meta)
	if !ok {
		t.Fatal("план без ширин отвергнут — плоская формула тоже не знает раскладку")
	}
	if plan.GlobalHeadDim <= 0 || plan.GlobalHeadDimV <= 0 || plan.GlobalKVHeads <= 0 {
		t.Fatalf("ширины не дозаполнены: K=%d V=%d heads=%d",
			plan.GlobalHeadDim, plan.GlobalHeadDimV, plan.GlobalKVHeads)
	}
	if plan.KVPerTokenBytes("") <= 0 {
		t.Fatal("KV на токен = 0 после дозаполнения — занижение требования к памяти")
	}
}
