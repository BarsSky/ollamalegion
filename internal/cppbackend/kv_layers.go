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

import "strings"

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

// KVPlan — раскладка KV-кэша модели по классам слоёв.
//
// Классов три, и KV у них устроен по-разному (llama.cpp: llama-model.cpp:2342
// has_kv(), llama-hparams.cpp:268):
//
//   - глобальные слои — кэш растёт с n_ctx;
//   - SWA-слои — кэш ограничен окном (<arch>.attention.sliding_window), потому
//     что attention видит только последние SWAWindow токенов;
//   - слои без кэша (KV-sharing) — переиспользуют кэш более раннего слоя.
//
// Одно число «слоёв с KV» для оценки памяти не годится: у gemma-4-E4B из 24
// слоёв с кэшем только 4 глобальные, а 20 живут в окне 512 токенов. Оценка
// «24 слоя × n_ctx» завышает KV в разы и роняет оффлоад на CPU.
type KVPlan struct {
	// GlobalLayers — слои, чей KV пропорционален n_ctx.
	GlobalLayers int
	// GlobalHeadDim — размер головы K/V глобальных слоёв (элементов).
	GlobalHeadDim int
	// SWALayers — слои с кэшем фиксированного окна.
	SWALayers int
	// SWAHeadDim — размер головы K/V SWA-слоёв (у gemma-4 это отдельный ключ
	// <arch>.attention.key_length_swa = 256 против 512 у глобальных).
	SWAHeadDim int
	// SWAWindow — окно SWA в токенах.
	SWAWindow int
	// Exact — раскладка посчитана по правилу llama.cpp, а не верхней оценкой.
	Exact bool
}

// KVLayers — сколько всего слоёв держат кэш (глобальные + SWA).
func (p KVPlan) KVLayers() int {
	return p.GlobalLayers + p.SWALayers
}

// KVPlanFor — раскладка KV-кэша по архитектуре и ключам GGUF.
//
// swaPattern — строка из '1'/'0' (<arch>.attention.sliding_window_pattern):
// '1' = слой со скользящим окном, '0' = глобальный. Пустая строка = ключа нет.
func KVPlanFor(arch string, nLayerAll, nNextn, fullAttnInterval int, hasExplicitRecurrentList bool,
	sharedKVLayers int, swaPattern string, swaWindow, swaHeadDim, kvHeadDim int) KVPlan {

	if nLayerAll <= 0 {
		return KVPlan{}
	}

	lower := strings.ToLower(arch)

	// Гибридные модели с рекуррентными слоями (qwen35/qwen3next): KV хранят
	// только attention-слои, окна у них нет.
	if kvLayersArchs[lower] && !hasExplicitRecurrentList {
		layers, exact := KVLayersFor(arch, nLayerAll, nNextn, fullAttnInterval, hasExplicitRecurrentList)
		return KVPlan{GlobalLayers: layers, GlobalHeadDim: kvHeadDim, Exact: exact}
	}

	// gemma4/gemma3n: KV-sharing. llama.cpp задаёт начало кэша как
	// n_layer_all − n_kv_shared_layers (gemma4.cpp:10); у gemma3n это
	// константа 20 (gemma3n.cpp:9).
	kvLayers := 0
	switch lower {
	case "gemma4":
		if sharedKVLayers <= 0 {
			// Ключа нет — считаем верхней оценкой, как раньше.
			return KVPlan{GlobalLayers: nLayerAll, GlobalHeadDim: kvHeadDim}
		}
		kvLayers = nLayerAll - sharedKVLayers
	case "gemma3n":
		kvLayers = 20
		if kvLayers > nLayerAll {
			kvLayers = nLayerAll
		}
	default:
		// Архитектура не из проверенных: верхняя оценка (все слои, без окна).
		return KVPlan{GlobalLayers: nLayerAll, GlobalHeadDim: kvHeadDim}
	}
	if kvLayers <= 0 {
		return KVPlan{}
	}

	plan := KVPlan{GlobalLayers: kvLayers, GlobalHeadDim: kvHeadDim, SWAWindow: swaWindow}
	if swaPattern == "" {
		// Паттерн окна неизвестен: слои с кэшем считаем глобальными — это
		// верхняя оценка (KV больше реального), exact=false. Число слоёв с
		// кэшем при этом известно точно и берётся из KV-sharing.
		return plan
	}

	swa := 0
	for i := 0; i < kvLayers && i < len(swaPattern); i++ {
		if swaPattern[i] == '1' {
			swa++
		}
	}
	plan.SWALayers = swa
	plan.GlobalLayers = kvLayers - swa
	plan.SWAHeadDim = swaHeadDim
	plan.Exact = true
	return plan
}

// KVPlan — раскладка KV для метаданных, прочитанных из GGUF.
func (m *GGUFModelMeta) KVPlan() KVPlan {
	if m == nil {
		return KVPlan{}
	}
	return KVPlanFor(m.Architecture, m.NLayers, m.NextNPredictLayers, m.FullAttentionInterval,
		m.HasRecurrentLayersKey, m.SharedKVLayers, m.SWAPattern, m.SlidingWindow,
		m.SWAKeyLength, m.KVHeadDim())
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
	return KVPlanFor(h.Architecture, h.NLayers, h.NextNPredictLayers, h.FullAttentionInterval,
		h.HasRecurrentLayersKey, h.SharedKVLayers, h.SWAPattern, h.SlidingWindow,
		h.SWAKeyLength, h.KVHeadDim())
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
