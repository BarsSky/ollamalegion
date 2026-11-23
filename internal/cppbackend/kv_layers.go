// kv_layers.go — R83 (2026-09-25): сколько слоёв РЕАЛЬНО хранят KV-кэш.
//
// ПРОБЛЕМА, из-за которой появился этот файл. Для Qwen3.8-27B-UD-Q4_K_M в одном
// логе соседствуют три разных числа, и каждое «правильное» в своём смысле:
//
//	qwen35.block_count = 65        → n_layer_all: все блоки, включая MTP
//	llama_model_n_layer() = 64     → n_layer() = n_layer_all − n_layer_nextn
//	KV-кэш хранят 17 слоёв         → только НЕрекуррентные (attention) слои
//
// Откуда 17 (по исходникам llama.cpp в этом репозитории):
//
//   - c/llama.cpp/src/models/qwen35.cpp:16 — n_layer_nextn = 1 (MTP-слой);
//   - c/llama.cpp/src/models/qwen35.cpp:21-27 — если ключа
//     <arch>.attention.recurrent_layers НЕТ, рекуррентными (linear attention,
//     gated delta net) считаются слои `(i+1) % full_attention_interval != 0`
//     для i < n_layer(); интервал по умолчанию 4, переопределяется ключом
//     <arch>.full_attention_interval. MTP-слой (i == n_layer()) — НЕ рекуррентный;
//   - c/llama.cpp/src/llama-memory-hybrid.cpp:34-63 — attention-кэш создаётся с
//     фильтром `!hparams.is_recr(il)`, а рекуррентное состояние живёт отдельно;
//   - c/llama.cpp/src/llama-kv-cache.cpp:163-167 — слои с has_kv(il) == false
//     пропускаются (has_kv ограничен n_layer_kv_from_start: gemma3n/gemma4).
//
// Для нашего файла: 16 attention-слоёв из 64 (MTP-слой 64 в кэш не входит).
//
// ПОЧЕМУ ЭТО ВАЖНО. Формула «2 × block_count × kv_heads × (n_embd/n_heads)»
// переоценивает KV гибридных моделей в разы: для этого файла 221 520 Б/токен
// против фактических 65 536 Б/токен (f16) — в 3.38 раза. Завышенный KV двигает
// max_vram_n_ctx, max_ram_n_ctx, гейт n_ctx, решение об оффлоаде и совет клиенту.
// Плюс head_dim KV берётся из <arch>.attention.key_length (= 256), а не из
// n_embd/n_heads (= 213): у Qwen3.5/3.8 головы K/V шире, чем «эмбеддинг / число
// голов».
//
// ЕДИНСТВЕННЫЙ ИСТОЧНИК. И число KV-слоёв, и head_dim считаются ЗДЕСЬ; и
// cppbackend, и internal/memfit берут их отсюда — чтобы «в логе одно, в решении
// другое» больше не воспроизводилось.
package cppbackend

import (
	"strings"

	"ollama-loadbalancer/internal/memfit"
)

// defaultFullAttentionInterval — значение llama.cpp, когда ключа
// <arch>.full_attention_interval в файле нет (qwen35.cpp:22).
const defaultFullAttentionInterval = 4

// kvLayersArchs — архитектуры, для которых правило «каждый N-й слой attention»
// проверено по исходникам llama.cpp. Для остальных (granite-hybrid, lfm2,
// plamo2, kimi-linear, nemotron-h…) список рекуррентных слоёв задаётся иначе, и
// угадывать нельзя: отдаём верхнюю оценку с exact=false.
var kvLayersArchs = map[string]bool{
	"qwen35":    true,
	"qwen35moe": true,
	"qwen3next": true,
}

// KVHeadDimFor — размер головы K/V (в элементах):
// <arch>.attention.key_length, иначе классическое n_embd / n_heads.
//
// Разница не косметическая: у Qwen3.8-27B key_length = 256, а n_embd/n_heads = 213.
func KVHeadDimFor(keyLength, nEmbd, nHeads int) int {
	if keyLength > 0 {
		return keyLength
	}
	if nHeads <= 0 {
		return 0
	}
	return nEmbd / nHeads
}

// KVLayersFor — сколько слоёв хранят KV-кэш.
//
// Считаются только слои [0, n_layer()), где n_layer() = n_layer_all − n_layer_nextn:
// MTP/nextn-слои в attention-кэш не попадают. Это проверено НА ЖИВОМ СТЕНДЕ:
// llama.cpp при загрузке Qwen3.8-27B (ctx=8192, q4_0, gpu_layers=0) напечатал
//
//	llama_kv_cache: size = 144.00 MiB (8192 cells, 16 layers, 1/1 seqs),
//	                K (q4_0): 72.00 MiB, V (q4_0): 72.00 MiB
//	llama_kv_cache: layer 0: filtered … layer 3: dev = CPU … layer 4: filtered …
//
// «filtered» — рекуррентные слои, «dev = CPU» — attention-слои с KV. Их 16
// (индексы 3, 7, 11, …, 63), и расчёт 2 × 16 × 4 руки × 256 × 18/32 × 8192 =
// 150 994 944 Б = 144.00 MiB совпадает с напечатанным точно. MTP-слой (64) в кэш
// не входит — поэтому 16, а не 17.
//
// exact == false означает «посчитано не по факту, а верхней оценкой» (нет данных
// об архитектуре, либо в файле есть явный список рекуррентных слоёв, который этот
// парсер не читает). Вызывающий обязан либо взять верхнюю оценку осознанно, либо
// отказаться от расчёта — но не выдавать её за факт.
//
// Возвращаемое число всегда пригодно к использованию: при exact == false это
// n_layer_all (все слои, максимальный возможный KV).
func KVLayersFor(arch string, nLayerAll, nNextn, fullAttnInterval int, hasExplicitRecurrentList bool) (layers int, exact bool) {
	if nLayerAll <= 0 {
		return 0, false
	}
	if !kvLayersArchs[strings.ToLower(arch)] || hasExplicitRecurrentList {
		// Либо архитектура не из проверенных, либо правило задано списком.
		return nLayerAll, false
	}
	interval := fullAttnInterval
	if interval <= 0 {
		interval = defaultFullAttentionInterval
	}
	nLayer := nLayerAll - nNextn
	if nLayer < 0 {
		nLayer = 0
	}
	kvLayers := 0
	for i := 0; i < nLayer; i++ {
		// qwen35.cpp:25 — (i+1) % interval != 0 ⇒ рекуррентный (KV не хранит).
		if (i+1)%interval == 0 {
			kvLayers++
		}
	}
	return kvLayers, true
}

// Источник раскладки KV (R707). Публикуется в FeasibleInfo.KVPlanSource и в
// /api/models как kv_plan_source, чтобы в API было видно, ОТКУДА цифра: послойная
// раскладка это или прежняя плоская формула.
const (
	// KVPlanSourcePlan — раскладка посчитана по слоям: какие слои держат кэш,
	// какие живут в окне, своя ширина у каждого класса.
	KVPlanSourcePlan = "plan"
	// KVPlanSourceFlat — данных по слоям нет (старый воркер, нестандартная
	// модель): KV считается прежней плоской формулой «все слои × ширина».
	KVPlanSourceFlat = "flat"
)

// KVPlan — раскладка KV-кэша модели по классам слоёв.
//
// Классов два, и KV у них устроен по-разному (llama.cpp: llama-hparams.cpp:305
// is_swa(), :336 has_kv(), llama-kv-cache.cpp:163-167):
//
//   - глобальные слои — кэш растёт с n_ctx, своя ширина K/V
//     (<arch>.attention.key_length / value_length);
//   - SWA-слои — кэш ограничен окном (<arch>.attention.sliding_window), потому
//     что attention видит только последние n_swa токенов, и своя ширина
//     (<arch>.attention.key_length_swa / value_length_swa);
//   - слои без кэша (KV-sharing) — переиспользуют кэш более раннего слоя
//     (has_kv(il) == false, кэш начинается с n_layer_all − n_kv_shared).
//
// Одно число «слоёв с KV» для оценки памяти не годится: у gemma-4-E4B из 24
// слоёв с кэшем только 4 глобальные, а 20 живут в окне 512 токенов, да ещё и
// шириной 256 против 512. Оценка «24 слоя × n_ctx × 512» завышает KV в разы и
// роняет оффлоад на CPU (R83), а «4 × n_ctx + 20 × окно» — это и есть план.
//
// R707: поля классов (ширины K/V, число KV-голов, окно) плюс источник плана.
// Формула роста — НЕ здесь: KVPlan.Shape() отдаёт memfit.KVShape, и байты
// считает memfit (одна формула на весь стек, см. internal/memfit/kv.go).
type KVPlan struct {
	// GlobalLayers — слои, чей KV пропорционален n_ctx.
	GlobalLayers int
	// GlobalHeadDim — ширина головы K глобальных слоёв (элементов).
	GlobalHeadDim int
	// GlobalHeadDimV — ширина головы V глобальных слоёв (0 → равна K).
	GlobalHeadDimV int
	// GlobalKVHeads — число KV-голов глобальных слоёв (0 → NKvHeads модели).
	GlobalKVHeads int
	// SWALayers — слои с кэшем фиксированного окна.
	SWALayers int
	// SWAHeadDim — ширина головы K SWA-слоёв (у gemma-4 это отдельный ключ
	// <arch>.attention.key_length_swa = 256 против 512 у глобальных).
	SWAHeadDim int
	// SWAHeadDimV — ширина головы V SWA-слоёв (0 → равна SWAHeadDim).
	SWAHeadDimV int
	// SWAKVHeads — число KV-голов SWA-слоёв (0 → NKvHeads модели).
	SWAKVHeads int
	// SWAWindow — окно SWA в токенах.
	SWAWindow int
	// Exact — раскладка посчитана по правилу llama.cpp, а не верхней оценкой.
	Exact bool
	// Source — KVPlanSourcePlan или KVPlanSourceFlat (см. константы выше).
	Source string
}

// KVLayers — сколько всего слоёв держат кэш (глобальные + SWA).
func (p KVPlan) KVLayers() int {
	return p.GlobalLayers + p.SWALayers
}

// Shape — та же раскладка в терминах единой формулы (memfit.KVShape).
//
// Нули в ширинах/головах классов означают «как у глобального класса» — это
// дозаполняется здесь, чтобы KVPlan можно было собирать частично (пути без
// <arch>.attention.value_length и без массива head_count_kv).
func (p KVPlan) Shape() memfit.KVShape {
	k, v := p.GlobalHeadDim, p.GlobalHeadDimV
	if v <= 0 {
		v = k
	}
	gHeads, sHeads := p.GlobalKVHeads, p.SWAKVHeads
	if sHeads <= 0 {
		sHeads = gHeads
	}
	sk, sv := p.SWAHeadDim, p.SWAHeadDimV
	if sk <= 0 {
		sk = k
	}
	if sv <= 0 {
		sv = sk
	}
	return memfit.KVShape{
		Global:    memfit.KVLayerClass{Layers: p.GlobalLayers, HeadDimK: k, HeadDimV: v, KVHeads: gHeads},
		SWA:       memfit.KVLayerClass{Layers: p.SWALayers, HeadDimK: sk, HeadDimV: sv, KVHeads: sHeads},
		SWAWindow: p.SWAWindow,
	}
}

// KVPerTokenBytes — «KV на токен» как СУММА по слоям: глобальные считаются
// своей шириной, SWA — своей, shared_kv не считаются вовсе (владелец слоя уже в
// глобальном/SWA классе). Формула — memfit.KVShape.BytesPerToken.
func (p KVPlan) KVPerTokenBytes(kvCacheType string) int64 {
	return int64(p.Shape().BytesPerToken(MemfitKVType(kvCacheType)))
}

// KVBytesAtCtx — полный KV-кэш на ctx токенов (оконные слои насыщаются окном).
func (p KVPlan) KVBytesAtCtx(kvCacheType string, ctx int) int64 {
	return int64(p.Shape().BytesAtCtx(MemfitKVType(kvCacheType), ctx))
}

// MaxCtxForBudget — максимальное окно, влезающее в budgetBytes.
//
// Это НЕ «бюджет / KV на токен»: после окна растут только глобальные слои,
// поэтому бюджет делится на прирост с учётом SWA (memfit.KVShape.MaxCtxForBudget,
// обратная к BytesAtCtx формула). ceiling = обучающий контекст модели.
func (p KVPlan) MaxCtxForBudget(kvCacheType string, budgetBytes int64, ceiling int) int {
	return p.Shape().MaxCtxForBudget(MemfitKVType(kvCacheType), memfit.Bytes(budgetBytes), ceiling)
}

// EstimateKVCacheMB — KV-кэш на nCtx токенов в MB (+10 % запаса), по раскладке.
//
// Тот же смысл, что у EstimateKVCacheMB (backend_vram.go), но ПОСЛОЙНО: оконные
// слои учитываются окном, shared_kv не считаются. Нужен там, где наружу уходит
// size_vram (cppworker → балансер → autotune-бюджет): раньше KV там считался по
// ВСЕМ слоям, и для gemma-4-E4B это было 3.5× больше реального.
func (p KVPlan) EstimateKVCacheMB(nCtx int, kvCacheType string) uint64 {
	bytes := p.KVBytesAtCtx(kvCacheType, nCtx)
	if bytes <= 0 {
		return 0
	}
	kvMB := uint64(bytes) / (1024 * 1024)
	return kvMB + kvMB/10 // +10 % safety buffer, как в EstimateKVCacheMB
}

// WireKVPlanFieldsR707 — поля раскладки KV для /api/models (snake_case).
//
// Один источник имён: и ручка воркера, и балансерный поллер, и тесты читают
// ровно этот набор, поэтому поле не может «уехать в одном месте и остаться в
// другом» (так уже случилось с n_embd/n_kv_heads в R66/R230/R660).
func (p KVPlan) WireKVPlanFieldsR707() map[string]interface{} {
	source := p.Source
	if source == "" {
		source = KVPlanSourceFlat
	}
	headDimV := p.GlobalHeadDimV
	if headDimV <= 0 {
		headDimV = p.GlobalHeadDim
	}
	swaHeadDim := p.SWAHeadDim
	swaHeadDimV := p.SWAHeadDimV
	if swaHeadDim <= 0 {
		swaHeadDim = p.GlobalHeadDim
	}
	if swaHeadDimV <= 0 {
		swaHeadDimV = swaHeadDim
	}
	swaHeads := p.SWAKVHeads
	if swaHeads <= 0 {
		swaHeads = p.GlobalKVHeads
	}
	return map[string]interface{}{
		"kv_plan_source":   source,
		"kv_plan_exact":    p.Exact,
		"kv_layers":        p.KVLayers(),
		"kv_global_layers": p.GlobalLayers,
		"kv_swa_layers":    p.SWALayers,
		"kv_swa_window":    p.SWAWindow,
		"head_dim_swa":     swaHeadDim,
		"head_dim_swa_v":   swaHeadDimV,
		"n_kv_heads_swa":   swaHeads,
	}
}

// KVPlanForModel — раскладка KV по метаданным модели (ModelManager).
//
// ok=false — метаданных нет (файл не просканирован, старый кэш): вызывающий
// обязан не выдумывать раскладку, а жить без неё (балансер тогда считает плоской
// формулой — см. kvCacheBytesForContext).
func (b *Backend) KVPlanForModel(name string) (KVPlan, bool) {
	if b == nil || strings.TrimSpace(name) == "" {
		return KVPlan{}, false
	}
	mm := b.ModelManager()
	if mm == nil {
		return KVPlan{}, false
	}
	meta, err := mm.GetModelMetaResolved(name)
	if err != nil || meta == nil {
		return KVPlan{}, false
	}
	plan := meta.KVPlan()
	if plan.Source == "" {
		return plan, false
	}
	return plan, true
}

// KVPlanFor — раскладка KV-кэша по архитектуре и ключам GGUF.
//
// swaPattern — строка из '1'/'0' (<arch>.attention.sliding_window_pattern):
// '1' = слой со скользящим окном, '0' = глобальный. Пустая строка = ключа нет.
//
// Тонкая обёртка над kvPlanFor: сохранена для вызовов, у которых нет ширин V и
// числа KV-голов (они тогда равны ширине K).
func KVPlanFor(arch string, nLayerAll, nNextn, fullAttnInterval int, hasExplicitRecurrentList bool,
	sharedKVLayers int, swaPattern string, swaWindow, swaHeadDim, kvHeadDim int) KVPlan {

	return kvPlanFor(KVPlanInput{
		Arch:                     arch,
		NLayers:                  nLayerAll,
		NextNPredictLayers:       nNextn,
		FullAttentionInterval:    fullAttnInterval,
		HasExplicitRecurrentList: hasExplicitRecurrentList,
		SharedKVLayers:           sharedKVLayers,
		SWAPattern:               swaPattern,
		SWAWindow:                swaWindow,
		KeyLength:                kvHeadDim,
		SWAKeyLength:             swaHeadDim,
	})
}

// KVPlanInput — всё, что нужно для раскладки KV.
//
// Сюда собраны ключи GGUF, от которых зависит, КАКИЕ слои держат кэш (num_loops /
// shared_kv_layers / full_attention_interval / sliding_window_pattern) и С КАКОЙ
// шириной (key_length / value_length / *_swa). Один вход — одна раскладка.
type KVPlanInput struct {
	Arch                     string
	NLayers                  int // <arch>.block_count: физические блоки ИЛИ слоты кэша (см. loop-схему)
	NextNPredictLayers       int // <arch>.nextn_predict_layers (MTP; KV не хранит)
	FullAttentionInterval    int // <arch>.full_attention_interval (qwen35: каждый N-й — attention)
	HasExplicitRecurrentList bool
	SharedKVLayers           int    // <arch>.attention.shared_kv_layers
	SWAPattern               string // <arch>.attention.sliding_window_pattern ('1' = окно)
	SWAWindow                int    // <arch>.attention.sliding_window
	KeyLength                int    // <arch>.attention.key_length
	ValueLength              int    // <arch>.attention.value_length
	SWAKeyLength             int    // <arch>.attention.key_length_swa
	SWAValueLength           int    // <arch>.attention.value_length_swa
	NKvHeads                 int    // <arch>.attention.head_count_kv (max по массиву)
	KVHeadsPerLayer          []int  // тот же ключ массивом — по слоям (R707)
	// Loop-схема (inkling2_loop): блоков NLayers, а слотов кэша (визитов) больше.
	// Числа — из <arch>.num_loops / <arch>.loop.core_start / <arch>.loop.core_end.
	LoopCount     int
	LoopCoreStart int
	LoopCoreEnd   int
}

// kvPlanFor — построение раскладки. Здесь только ПРАВИЛА llama.cpp, арифметики
// байтов нет (она в memfit.KVShape).
func kvPlanFor(in KVPlanInput) KVPlan {
	if in.NLayers <= 0 {
		return KVPlan{}
	}
	lower := strings.ToLower(in.Arch)

	// 1. Слоты кэша. Для inkling2_loop физических блоков n_phys, а слотов
	// n_phys + (n_passes−1)×(core_end−core_start): каждый визит цикла держит СВОЙ
	// KV (c/llama.cpp/src/models/inkling2-loop.cpp:16-30, models.h:2557-2565,
	// post_arch_tensors там же :65-69). Ширины и SWA-флаг берутся по ФИЗИЧЕСКОМУ
	// слою (layer_phys, inkling2-loop.cpp:3-5) — он и хранит веса.
	kvSlots := in.NLayers
	physOf := func(il int) int { return il }
	if strings.HasSuffix(lower, "_loop") &&
		in.LoopCount >= 2 && in.LoopCoreStart > 0 &&
		in.LoopCoreEnd > in.LoopCoreStart && in.LoopCoreEnd <= in.NLayers {
		core := in.LoopCoreEnd - in.LoopCoreStart
		kvSlots = in.NLayers + (in.LoopCount-1)*core
		physOf = func(il int) int {
			if il < in.NLayers {
				return il
			}
			return in.LoopCoreStart + (il-in.NLayers)%core
		}
	}

	// 2. Сколько слотов держат кэш и по какому правилу.
	if kvLayersArchs[lower] && !in.HasExplicitRecurrentList {
		// Гибридные модели (qwen35/qwen3next): KV хранят только attention-слои,
		// окна у них нет.
		layers, exact := KVLayersFor(in.Arch, in.NLayers, in.NextNPredictLayers,
			in.FullAttentionInterval, in.HasExplicitRecurrentList)
		return KVPlan{
			GlobalLayers:   layers,
			GlobalHeadDim:  in.KeyLength,
			GlobalHeadDimV: in.ValueLength,
			GlobalKVHeads:  in.NKvHeads,
			Exact:          exact,
			Source:         KVPlanSourcePlan,
		}
	}

	kvSlotsWithCache := kvSlots
	planSource := KVPlanSourceFlat
	exact := false
	loopExpanded := kvSlots != in.NLayers

	switch lower {
	case "gemma4":
		// llama.cpp: кэш начинается с n_layer_all − n_kv_shared_layers
		// (c/llama.cpp/src/models/gemma4.cpp, n_layer_kv_from_start).
		if in.SharedKVLayers <= 0 {
			break // ключа нет — прежняя верхняя оценка (все слои), ниже
		}
		kvSlotsWithCache = kvSlots - in.SharedKVLayers
		planSource = KVPlanSourcePlan
	case "gemma3n":
		// Константа llama.cpp: кэш держат первые 20 слоёв (gemma3n.cpp).
		kvSlotsWithCache = 20
		if kvSlotsWithCache > kvSlots {
			kvSlotsWithCache = kvSlots
		}
		planSource = KVPlanSourcePlan
	default:
		if loopExpanded {
			// Визиты цикла — факт из llama.cpp, а не оценка.
			planSource = KVPlanSourcePlan
		}
	}
	if kvSlotsWithCache <= 0 {
		return KVPlan{}
	}

	// 3. Классы: глобальные и оконные. Без ключа паттерна делить нечем — все
	// слои с кэшем считаются глобальными (верхняя оценка, exact=false).
	isSWA := func(il int) bool {
		if in.SWAPattern == "" || in.SWAWindow <= 0 {
			return false
		}
		p := physOf(il)
		if p < 0 || p >= len(in.SWAPattern) {
			return false
		}
		return in.SWAPattern[p] == '1'
	}
	swa, global := 0, 0
	for il := 0; il < kvSlotsWithCache; il++ {
		if isSWA(il) {
			swa++
		} else {
			global++
		}
	}
	// Явный массив sliding_window_pattern — это ТА ЖЕ строка, из которой llama.cpp
	// строит is_swa (llama-hparams.cpp:8-22 читает ключ в is_swa_impl), поэтому
	// раскладка по ней — факт, а не оценка, и источник можно объявлять "plan"
	// даже для архитектуры, которой нет в списке правил (inkling2: 48 слотов,
	// 8 глобальных, 40 оконных с окном 1024).
	if in.SWAPattern != "" && in.SWAWindow > 0 && swa > 0 {
		planSource = KVPlanSourcePlan
		exact = true
	}
	if planSource == KVPlanSourcePlan && loopExpanded && in.SWAPattern == "" {
		exact = true // число слотов известно по правилу, окна нет
	}

	plan := KVPlan{
		GlobalLayers:   global,
		GlobalHeadDim:  in.KeyLength,
		GlobalHeadDimV: in.ValueLength,
		GlobalKVHeads:  in.NKvHeads,
		SWALayers:      swa,
		SWAWindow:      in.SWAWindow,
		Exact:          exact,
		Source:         planSource,
	}
	if swa > 0 {
		plan.SWAHeadDim = in.SWAKeyLength
		plan.SWAHeadDimV = in.SWAValueLength
		plan.SWAKVHeads = in.NKvHeads
	}
	// 4. Число KV-голов по классам. Ключ head_count_kv у новых архитектур задан
	// массивом (Inkling-2: 48 значений) — берём МАКСИМУМ класса: это верхняя
	// оценка KV, то есть безопасная (окно рекомендуем меньше, а не больше).
	if len(in.KVHeadsPerLayer) > 0 {
		gMax, sMax := 0, 0
		for il := 0; il < kvSlotsWithCache; il++ {
			p := physOf(il)
			if p < 0 || p >= len(in.KVHeadsPerLayer) {
				continue
			}
			h := in.KVHeadsPerLayer[p]
			if isSWA(il) {
				if h > sMax {
					sMax = h
				}
			} else if h > gMax {
				gMax = h
			}
		}
		if gMax > 0 {
			plan.GlobalKVHeads = gMax
		}
		if swa > 0 && sMax > 0 {
			plan.SWAKVHeads = sMax
		}
	}
	return plan
}

// KVPlan — раскладка KV для метаданных, прочитанных из GGUF.
func (m *GGUFModelMeta) KVPlan() KVPlan {
	if m == nil {
		return KVPlan{}
	}
	return kvPlanFor(m.kvPlanInputR707())
}

// kvPlanInputR707 — вход раскладки из кэша метаданных ModelManager.
func (m *GGUFModelMeta) kvPlanInputR707() KVPlanInput {
	return KVPlanInput{
		Arch:                     m.Architecture,
		NLayers:                  m.NLayers,
		NextNPredictLayers:       m.NextNPredictLayers,
		FullAttentionInterval:    m.FullAttentionInterval,
		HasExplicitRecurrentList: m.HasRecurrentLayersKey,
		SharedKVLayers:           m.SharedKVLayers,
		SWAPattern:               m.SWAPattern,
		SWAWindow:                m.SlidingWindow,
		KeyLength:                m.KeyLength,
		ValueLength:              m.ValueLength,
		SWAKeyLength:             m.SWAKeyLength,
		SWAValueLength:           m.SWAValueLength,
		NKvHeads:                 m.NKvHeads,
		KVHeadsPerLayer:          m.KVHeadsPerLayer,
		LoopCount:                m.LoopCount,
		LoopCoreStart:            m.LoopCoreStart,
		LoopCoreEnd:              m.LoopCoreEnd,
	}
}

// KVLayers — число слоёв с KV-кэшем для метаданных, прочитанных из GGUF.
func (m *GGUFModelMeta) KVLayers() (int, bool) {
	if m == nil {
		return 0, false
	}
	if p := m.KVPlan(); p.KVLayers() > 0 {
		return p.KVLayers(), p.Exact
	}
	return KVLayersFor(m.Architecture, m.NLayers, m.NextNPredictLayers, m.FullAttentionInterval, m.HasRecurrentLayersKey)
}

// KVPlan — раскладка KV для GGUFHeaderInfo (путь без кэша ModelManager).
func (h *GGUFHeaderInfo) KVPlan() KVPlan {
	if h == nil {
		return KVPlan{}
	}
	return kvPlanFor(KVPlanInput{
		Arch:                     h.Architecture,
		NLayers:                  h.NLayers,
		NextNPredictLayers:       h.NextNPredictLayers,
		FullAttentionInterval:    h.FullAttentionInterval,
		HasExplicitRecurrentList: h.HasRecurrentLayersKey,
		SharedKVLayers:           h.SharedKVLayers,
		SWAPattern:               h.SWAPattern,
		SWAWindow:                h.SlidingWindow,
		KeyLength:                h.KeyLength,
		ValueLength:              h.ValueLength,
		SWAKeyLength:             h.SWAKeyLength,
		SWAValueLength:           h.SWAValueLength,
		NKvHeads:                 h.NKvHeads,
		KVHeadsPerLayer:          h.KVHeadsPerLayer,
		LoopCount:                h.LoopCount,
		LoopCoreStart:            h.LoopCoreStart,
		LoopCoreEnd:              h.LoopCoreEnd,
	})
}

// KVHeadDim — размер головы K/V для метаданных из GGUF.
func (m *GGUFModelMeta) KVHeadDim() int {
	if m == nil {
		return 0
	}
	return KVHeadDimFor(m.KeyLength, m.NEmbd, m.NHeads)
}

// KVLayers — то же для GGUFHeaderInfo (путь без кэша ModelManager).
func (h *GGUFHeaderInfo) KVLayers() (int, bool) {
	if h == nil {
		return 0, false
	}
	if p := h.KVPlan(); p.KVLayers() > 0 {
		return p.KVLayers(), p.Exact
	}
	return KVLayersFor(h.Architecture, h.NLayers, h.NextNPredictLayers, h.FullAttentionInterval, h.HasRecurrentLayersKey)
}

// KVHeadDim — то же для GGUFHeaderInfo.
func (h *GGUFHeaderInfo) KVHeadDim() int {
	if h == nil {
		return 0
	}
	return KVHeadDimFor(h.KeyLength, h.NEmbd, h.NHeads)
}

// KVHeadVDim — ширина головы V (в элементах): <arch>.attention.value_length,
// иначе равна ширине K — ровно как у llama.cpp (value_length опционален,
// src/llama-model.cpp:1662-1663).
//
// R705 (2026-11-24): появилась вместе с публикацией head_dim_v. У gemma-4 K и V
// совпадают (512/512), но в общем случае это разные ключи, и считать V «равной K»
// без проверки файла — догадка, которую теперь видно (value_length читается).
func (m *GGUFModelMeta) KVHeadVDim() int {
	if m == nil {
		return 0
	}
	return headDimVForR705(m.ValueLength, m.KVHeadDim())
}

// KVHeadVDim — то же для GGUFHeaderInfo.
func (h *GGUFHeaderInfo) KVHeadVDim() int {
	if h == nil {
		return 0
	}
	return headDimVForR705(h.ValueLength, h.KVHeadDim())
}

// KVHeadDimSource — R705: откуда KVHeadDim(): bridge.HeadDimSourceGGUF, если в
// файле есть <arch>.attention.key_length, иначе HeadDimSourceComputed (прежняя
// формула n_embd/n_heads). Пусто — данных нет вовсе.
func (m *GGUFModelMeta) KVHeadDimSource() string {
	if m == nil {
		return ""
	}
	return headDimSourceForKeyLengthR705(m.KeyLength, m.KVHeadDim())
}

// KVHeadDimSource — то же для GGUFHeaderInfo.
func (h *GGUFHeaderInfo) KVHeadDimSource() string {
	if h == nil {
		return ""
	}
	return headDimSourceForKeyLengthR705(h.KeyLength, h.KVHeadDim())
}
