// fit_test.go — golden-тесты на РЕАЛЬНЫХ числах живого стенда (2026-09-25).
//
// Смысл этих тестов не в покрытии строк, а в том, что они ловят конкретные
// ошибки, которые жили в проекте годами и не ловились ни одним тестом, потому
// что прежние тесты проверяли синтетические фикстуры и «разницу между двумя
// вызовами», а не абсолютные числа на реальном железе:
//
//	V1  max_vram_n_ctx без весов          → на 8 GiB карте давал 103 389
//	V3  веса × 0.7                        → потолок завышался
//	V4  KV-cache как 256 БАЙТ на токен    → в 1000 раз меньше заявленного в комментарии
//	R3  MemTotal вместо MemAvailable      → одобрение загрузки с запасом 105 MiB
//	R5  лимит cgroup не читался
package memfit

import "testing"

// Реальные модели (метаданные из GGUF, размеры — фактические файлы на стенде).
//
// Qwen3.8-27B-UD-Q4_K_M: qwen35.block_count=65, nextn_predict_layers=1,
// attention.key_length=256, head_count=24, head_count_kv=4. Ключа
// attention.recurrent_layers в файле НЕТ, поэтому llama.cpp берёт правило по
// умолчанию (qwen35.cpp:21-27): рекуррентные — каждый слой, кроме каждого 4-го,
// то есть 48 из 64. MTP-слой (индекс 64) в attention-кэш не попадает — измерено на живом стенде: llama_kv_cache напечатал «16 layers».
var (
	qwen38 = ModelSpec{
		Name:      "qwen3.8:latest",
		SizeBytes: Bytes(16464440224), // Qwen3.8-27B-UD-Q4_K_M.gguf
		NLayers:   65,                 // block_count (n_layer_all)
		KVLayers:  16,                 // 64 − 48 рекуррентных (MTP-слой KV не хранит)
		NHeads:    24,
		NKvHeads:  4,
		NEmbd:     5120,
		KVHeadDim: 256, // attention.key_length, НЕ n_embd/n_heads = 213
		TrainCtx:  262144,
	}
	gemma4 = ModelSpec{
		Name:      "gemma-4-E4B-it-Q4_K_M",
		SizeBytes: Bytes(4215695776), // gemma-4-E4B-it-Q4_K_M.gguf
		NLayers:   42,
		NHeads:    8,
		NKvHeads:  2,
		NEmbd:     2560,
		TrainCtx:  131072,
		// KVLayers не задан: у gemma-4 KV общий для части слоёв
		// (n_layer_kv_from_start), правило kv_layers.go его не покрывает и
		// возвращает верхнюю оценку — осознанный консерватизм.
	}
)

// budget3070 — измерения живого контейнера: MemTotal 25 042 MiB,
// MemAvailable 21 651 MiB (/proc/meminfo), VRAM 8 192 MiB (NVML).
func budget3070() Budget {
	return Budget{
		VRAMFree:    MiBOf(8192),
		VRAMTotal:   MiBOf(8192),
		VRAMKnown:   true,
		RAMAvail:    MiBOf(21651),
		RAMTotal:    MiBOf(25042),
		RAMKnown:    true,
		VRAMReserve: MiBOf(2048), // defaultVRAMOverheadMB
		RAMReserve:  MiBOf(4096),
	}
}

// budgetA10 — NVIDIA A10 24 GB. Память хоста взята 32 GiB как параметр стенда
// (точное значение надо измерить на месте: cppworker -feasible на A10).
func budgetA10() Budget {
	return Budget{
		VRAMFree:    MiBOf(23028),
		VRAMTotal:   MiBOf(24576),
		VRAMKnown:   true,
		RAMAvail:    MiBOf(32768),
		RAMTotal:    MiBOf(32768),
		RAMKnown:    true,
		VRAMReserve: MiBOf(2048),
		RAMReserve:  MiBOf(4096),
	}
}

// TestKVBytesPerToken_RealModels — абсолютные числа. KV = 2 × kv_layers ×
// kv_heads × kv_head_dim × bytes/elem:
//
//	27B : 2×16×4×256 = 32 768 элементов → f16 ×2 = 65 536; q8_0 ×34/32 = 34 816; q4_0 ×18/32 = 18 432
//	gemma4 (верхняя оценка, все 42 слоя): 2×42×2×320 = 53 760 → f16 = 107 520; q4_0 = 30 240
func TestKVBytesPerToken_RealModels(t *testing.T) {
	cases := []struct {
		name string
		spec ModelSpec
		kv   KVType
		want int64
	}{
		{"27B_f16", qwen38, KVF16, 65536},
		{"27B_q8_0", qwen38, KVQ8, 34816},
		{"27B_q4_0", qwen38, KVQ4, 18432},
		{"27B_empty_is_f16", qwen38, "", 65536},
		{"gemma4_f16", gemma4, KVF16, 107520},
		{"gemma4_q4_0", gemma4, KVQ4, 30240},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := int64(KVBytesPerToken(tc.spec, tc.kv))
			if got != tc.want {
				t.Errorf("KVBytesPerToken(%s, %q) = %d Б/токен, want %d", tc.spec.Name, tc.kv, got, tc.want)
			}
		})
	}
}

// TestRegression_KVUsesAttentionLayersNotBlockCount — ключевой регресс R83.
//
// Наивная формула «2 × block_count × kv_heads × (n_embd/n_heads)» давала для этого
// файла 221 520 Б/токен (f16) — в 3.38 раза больше факта, потому что у гибридной
// модели KV хранят 16 слоёв из 64 (MTP-слой в кэш не входит), а head_dim KV равен
// 256, а не 213. Из этого завышения росли все потолки n_ctx и режим загрузки
// (gpu_layers=0 → модель грузилась целиком на CPU и была практически непригодна).
//
// Эталон — измерение: llama.cpp напечатал «llama_kv_cache: size = 144.00 MiB
// (8192 cells, 16 layers, …)», и 2×16×4×256×18/32×8192 = 150 994 944 Б = 144.00 MiB.
func TestRegression_KVUsesAttentionLayersNotBlockCount(t *testing.T) {
	if got := int64(KVBytesPerToken(qwen38, KVF16)); got != 65536 {
		t.Fatalf("KV(f16) = %d, want 65 536 (16 слоёв × 4 головы × 256)", got)
	}
	if got := int64(KVBytesPerToken(qwen38, KVQ4)); got != 18432 {
		t.Fatalf("KV(q4_0) = %d, want 18 432 (то же, ×18/32)", got)
	}

	// Верхняя оценка (нет данных о KV-слоях) обязана быть больше — именно так
	// выглядела прежняя формула: все блоки и n_embd/n_heads.
	naive := qwen38
	naive.KVLayers = 0
	naive.KVHeadDim = 0
	naiveKV := int64(KVBytesPerToken(naive, KVF16))
	if naiveKV != 221520 {
		t.Errorf("верхняя оценка KV(f16) = %d, want 221 520 (65 слоёв × 4 × 213)", naiveKV)
	}
	if naiveKV*100 < int64(KVBytesPerToken(qwen38, KVF16))*335 {
		t.Errorf("завышение наивной формулы должно быть ~3.38×: naive=%d correct=%d",
			naiveKV, KVBytesPerToken(qwen38, KVF16))
	}
}

// TestEvaluate_Golden_Qwen38_On3070 — вердикты на реальном железе.
func TestEvaluate_Golden_Qwen38_On3070(t *testing.T) {
	b := budget3070()
	p := DefaultPolicy()

	cases := []struct {
		name       string
		ctx        int
		kv         KVType
		wantStage  Stage
		wantLayers int
		wantReason ReasonCode
		notReason  ReasonCode
	}{
		// KV(q4_0) = 18 432 Б/токен × 32 768 = 612 MiB; веса 15 701 MiB.
		// Итого 16 313 MiB → 24/65 слоя на GPU (5 881 MiB), CPU-часть 10 290 MiB
		// влезает в доступные 14 044 MiB.
		{"32768_q4_0_partial", 32768, KVQ4, StagePartial, 24, "", ""},
		// KV = 2 448 MiB → 18 149 MiB: всё ещё помещается (22/65 на GPU), но
		// медленно. До исправления KV этот же запрос считался невыполнимым — и
		// именно поэтому llama.cpp грузил модель с gpu_layers=0.
		{"131072_q4_0_partial_but_slow", 131072, KVQ4, StagePartial, 22, "", ""},
		// f16 при 140 000: KV = 8 750 MiB, CPU-часть 18 432 MiB > 17 555 MiB
		// (свободная RAM 21 651 минус резерв 4 GiB) → отказ, но с q4_0 тот же
		// запрос проходит (12 295 MiB) → обязана быть подсказка про квантизацию KV.
		{"140000_f16_fails_with_kv_lever", 140000, KVF16, StageDoesNotFit, 16, ReasonKVQuantLever, ""},
		// Выше обучающего контекста модели — жёсткая граница, не «железо».
		{"above_training_context", 999999, KVQ4, StageDoesNotFit, 0, ReasonCtxAboveTrain, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(qwen38, Request{Ctx: tc.ctx, KVType: tc.kv}, b, p)
			if v.Stage != tc.wantStage {
				t.Errorf("stage = %s, want %s\n  %s", v.Stage, tc.wantStage, v.String())
			}
			if v.GPULayers != tc.wantLayers {
				t.Errorf("gpu_layers = %d, want %d\n  %s", v.GPULayers, tc.wantLayers, v.String())
			}
			if tc.wantReason != "" && !v.HasReason(tc.wantReason) {
				t.Errorf("нет причины %s\n  %s", tc.wantReason, v.String())
			}
			if tc.notReason != "" && v.HasReason(tc.notReason) {
				t.Errorf("причина %s не должна появляться\n  %s", tc.notReason, v.String())
			}
			if !v.CeilingsKnown {
				t.Errorf("CeilingsKnown = false при известном бюджете")
			}
		})
	}
}

// TestEvaluate_Golden_Gemma4_On3070 — gemma-4 реально работает на этой карте:
// 34/42 слоя на GPU, CPU-часть 1 406 MiB.
func TestEvaluate_Golden_Gemma4_On3070(t *testing.T) {
	v := Evaluate(gemma4, Request{Ctx: 32768, KVType: KVF16}, budget3070(), DefaultPolicy())
	if v.Stage != StagePartial || v.GPULayers != 34 {
		t.Errorf("gemma-4 на 3070: stage=%s layers=%d, want partial_offload/34\n  %s", v.Stage, v.GPULayers, v.String())
	}
}

// TestEvaluate_Golden_Qwen38_OnA10 — ключевой ответ на исходный вопрос.
//
// С ИСПРАВЛЕННЫМ KV (17 слоёв × 256) картина на A10 меняется принципиально:
//
//	q4_0 @ 131072: KV = 2 448 MiB, веса 15 701 → 18 149 MiB ≤ 20 980 MiB доступной
//	  VRAM ⇒ exact_fit, ВСЕ 65 слоёв на GPU. Раньше (KV 8 565 MiB) тот же запрос
//	  давал gpu_layers=0 и грузился целиком на CPU — отсюда «65536/128000 не
//	  работают, а 32768 работает».
//	f16 @ 131072: KV = 8 704 MiB → 24 405 MiB, не влезает целиком ⇒ частичный
//	  оффлоад 55/65.
//	f16 @ 32768: 17 876 MiB ⇒ exact_fit (это и есть наблюдаемое «32768 работает»).
func TestEvaluate_Golden_Qwen38_OnA10(t *testing.T) {
	b := budgetA10()
	p := DefaultPolicy()

	q4 := Evaluate(qwen38, Request{Ctx: 131072, KVType: KVQ4}, b, p)
	if q4.Stage != StageExactFit || q4.GPULayers != qwen38.NLayers {
		t.Errorf("A10 131072 q4_0: stage=%s layers=%d, want exact_fit/%d\n  %s",
			q4.Stage, q4.GPULayers, qwen38.NLayers, q4.String())
	}

	f16 := Evaluate(qwen38, Request{Ctx: 131072, KVType: KVF16}, b, p)
	// KV = 65 536 × 131 072 = 8 192 MiB; итого 23 893 MiB при 20 980 MiB доступной
	// VRAM → floor(65 × 20 980/23 893) = 57 слоёв на GPU, CPU-часть 2 941 MiB.
	if f16.Stage != StagePartial || f16.GPULayers != 57 {
		t.Errorf("A10 131072 f16: stage=%s layers=%d, want partial_offload/57\n  %s",
			f16.Stage, f16.GPULayers, f16.String())
	}
	if f16.GPULayers >= q4.GPULayers {
		t.Errorf("на f16 слоёв на GPU должно быть меньше, чем на q4_0: f16=%d q4_0=%d",
			f16.GPULayers, q4.GPULayers)
	}

	small := Evaluate(qwen38, Request{Ctx: 32768, KVType: KVF16}, b, p)
	if small.Stage != StageExactFit {
		t.Errorf("32768 f16 на A10 обязан влезать целиком (подтверждено на живом стенде): %s", small.String())
	}
}

// TestEvaluate_ExactFit_LargeVRAM — при достаточной VRAM всё уходит на GPU.
func TestEvaluate_ExactFit_LargeVRAM(t *testing.T) {
	b := budget3070()
	b.VRAMFree = MiBOf(49152) // 48 GiB
	v := Evaluate(qwen38, Request{Ctx: 32768, KVType: KVF16}, b, DefaultPolicy())
	if v.Stage != StageExactFit || v.GPULayers != qwen38.NLayers {
		t.Errorf("stage=%s layers=%d, want exact_fit/%d\n  %s", v.Stage, v.GPULayers, qwen38.NLayers, v.String())
	}
	if v.CPUWeights != 0 || v.CPUKV != 0 {
		t.Errorf("при exact_fit CPU-часть обязана быть нулевой: weights=%s kv=%s", v.CPUWeights, v.CPUKV)
	}
}

// TestEvaluate_UnknownBudget_NeverClaimsFit — структурный инвариант: из отсутствия
// данных нельзя вывести «поместится». Прежний код в этой ситуации пропускал
// проверку (fail-open) и грузил модель 9 минут целиком на CPU.
func TestEvaluate_UnknownBudget_NeverClaimsFit(t *testing.T) {
	v := Evaluate(qwen38, Request{Ctx: 32768, KVType: KVQ4}, Budget{}, DefaultPolicy())
	if v.Stage != StageUnknown {
		t.Errorf("stage = %s, want unknown", v.Stage)
	}
	if v.Fits() {
		t.Errorf("при неизвестном бюджете Fits() обязан быть false")
	}
	if v.CeilingsKnown {
		t.Errorf("CeilingsKnown = true без данных о ресурсах")
	}
	if !v.HasReason(ReasonBudgetUnknown) {
		t.Errorf("нет причины budget_unknown: %s", v.String())
	}

	fatal := DefaultPolicy()
	fatal.UnknownIsFatal = true
	vf := Evaluate(qwen38, Request{Ctx: 32768, KVType: KVQ4}, Budget{}, fatal)
	if vf.Stage != StageDoesNotFit || vf.Fits() {
		t.Errorf("при UnknownIsFatal ожидался does_not_fit, получено %s", vf.Stage)
	}
}

// TestRegression_WeightsCountedInVRAMCeiling — V1: веса обязаны участвовать в
// потолке VRAM. Прежняя формула (freeVRAM − overhead)/kvPerToken давала на этой
// карте max_vram_n_ctx = 103 389 при весах 15.7 GiB, которые в 8 GiB не влезают.
func TestRegression_WeightsCountedInVRAMCeiling(t *testing.T) {
	exact, hard := Ceilings(qwen38, KVQ4, budget3070(), DefaultPolicy())
	if exact != 0 {
		t.Errorf("MaxExactFitCtx = %d, want 0: веса 15.7 GiB не влезают в 6 GiB доступной VRAM", exact)
	}
	if hard <= 131072 || (qwen38.TrainCtx > 0 && hard > qwen38.TrainCtx) {
		t.Errorf("MaxHardCtx = %d, ожидалось больше 131072 и не больше обучающего контекста %d "+
			"(после исправления KV 131072 на этой карте ПОМЕЩАЕТСЯ, поэтому прежняя граница «между 32768 и 131072» неверна)", hard, qwen38.TrainCtx)
	}
	if v := Evaluate(qwen38, Request{Ctx: 32768, KVType: KVF16}, budget3070(), DefaultPolicy()); v.Stage == StageExactFit {
		t.Errorf("exact_fit на 8 GiB карте для 27B невозможен, получено %s", v.Stage)
	}
}

// TestRegression_NoArbitraryWeightFactor — V3: веса не должны масштабироваться
// коэффициентами вида ×0.7. При бюджете «веса + ровно 1 GiB» потолок обязан быть
// 1 GiB / kvPerToken; коэффициент 0.7 дал бы ~4.3× больше.
func TestRegression_NoArbitraryWeightFactor(t *testing.T) {
	b := budget3070()
	b.VRAMReserve = 0
	b.VRAMFree = qwen38.SizeBytes + Bytes(1)<<30 // вес + 1 GiB
	exact, _ := Ceilings(qwen38, KVQ4, b, DefaultPolicy())

	want := int((Bytes(1) << 30) / KVBytesPerToken(qwen38, KVQ4))
	if exact != want {
		t.Errorf("MaxExactFitCtx = %d, want %d (ровно 1 GiB под KV: вес учтён полностью)", exact, want)
	}
}

// TestRegression_CgroupLimitRespected — R5: при заданном лимите контейнера бюджет
// обязан ограничиваться им, а не памятью всей VM.
func TestRegression_CgroupLimitRespected(t *testing.T) {
	p := DefaultPolicy()

	noLimit := budget3070()
	v1 := Evaluate(qwen38, Request{Ctx: 32768, KVType: KVQ4}, noLimit, p)
	if !v1.Fits() {
		t.Fatalf("предпосылка теста: без лимита запрос должен проходить: %s", v1.String())
	}

	limited := budget3070()
	limited.RAMAvail = MiBOf(65536) // VM видит много памяти…
	limited.RAMLimit = MiBOf(8192)  // …но контейнеру разрешено 8 GiB
	limited.RAMLimitKnown = true
	v2 := Evaluate(qwen38, Request{Ctx: 32768, KVType: KVQ4}, limited, p)
	if v2.Stage != StageDoesNotFit {
		t.Errorf("при лимите cgroup 8 GiB ожидался does_not_fit, получено %s\n  %s", v2.Stage, v2.String())
	}
}

// TestPolicy_MakesTradeoffExplicit — политика (резерв и доли) задана явно и
// проверяема. Раньше в трёх местах одновременно жили «−4 GiB», «×0.8» и
// «MemTotal», из-за чего одна и та же загрузка оценивалась по-разному.
//
// Проверяем на модели без обучающего контекста: иначе потолок упирается в
// TrainCtx и рычаги политики не видны.
func TestPolicy_MakesTradeoffExplicit(t *testing.T) {
	spec := qwen38
	spec.TrainCtx = 0

	_, hardBase := Ceilings(spec, KVQ4, budget3070(), DefaultPolicy())

	// Дополнительная доля RAM уменьшает потолок.
	strict := DefaultPolicy()
	strict.RAMUtil = 0.5
	_, hardStrict := Ceilings(spec, KVQ4, budget3070(), strict)
	if hardStrict >= hardBase {
		t.Errorf("RAMUtil=0.5 обязан уменьшать потолок: strict=%d base=%d", hardStrict, hardBase)
	}

	// Меньший системный резерв — увеличивает.
	loose := budget3070()
	loose.RAMReserve = MiBOf(1024)
	_, hardLoose := Ceilings(spec, KVQ4, loose, DefaultPolicy())
	if hardLoose <= hardBase {
		t.Errorf("меньший резерв обязан давать больший потолок: loose=%d base=%d", hardLoose, hardBase)
	}

	// Больший резерв — уменьшает.
	tight := budget3070()
	tight.RAMReserve = MiBOf(8192)
	_, hardTight := Ceilings(spec, KVQ4, tight, DefaultPolicy())
	if hardTight >= hardBase {
		t.Errorf("больший резерв обязан уменьшать потолок: tight=%d base=%d", hardTight, hardBase)
	}
}
