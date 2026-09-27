package memfit

// ModelSpec — всё, что нужно для оценки памяти, берётся из метаданных GGUF
// (или из уже загруженной модели). Никаких «магических» полей: если данных нет,
// поле равно нулю, и Evaluate вернёт unknown, а не посчитает по догадке.
type ModelSpec struct {
	// Name — внешнее имя модели (для сообщений и трассировки), не участвует в расчёте.
	Name string
	// SizeBytes — размер .gguf = все веса модели (сумма тензоров).
	SizeBytes Bytes
	// NLayers — все блоки модели (n_layer_all из <arch>.block_count).
	NLayers int
	// KVLayers — сколько слоёв РЕАЛЬНО хранят KV-кэш. У гибридных моделей
	// (qwen35/qwen3next) рекуррентные слои KV не имеют: у Qwen3.8-27B это 17 из 65.
	// 0 = неизвестно → используется NLayers как ВЕРХНЯЯ оценка (KV больше реального).
	KVLayers int
	// NHeads / NKvHeads / NEmbd — для GQA-расчёта KV.
	NHeads   int
	NKvHeads int
	NEmbd    int
	// KVHeadDim — размер головы K/V (из <arch>.attention.key_length). У Qwen3.8-27B
	// это 256, тогда как NEmbd/NHeads = 213. 0 = неизвестно → NEmbd/NHeads.
	KVHeadDim int
	// TrainCtx — обучающий контекст из GGUF (*.context_length). 0 = неизвестен.
	TrainCtx int
}

// EffectiveKVLayers — сколько слоёв считать хранящими KV (0 → верхняя оценка).
func (m ModelSpec) EffectiveKVLayers() int {
	if m.KVLayers > 0 {
		return m.KVLayers
	}
	return m.NLayers
}

// EffectiveKVHeadDim — размер головы KV (0 → NEmbd/NHeads).
func (m ModelSpec) EffectiveKVHeadDim() int {
	if m.KVHeadDim > 0 {
		return m.KVHeadDim
	}
	return m.HeadDim()
}

// HeadDim — размер головы attention. Для нестандартных архитектур (Qwen3.8:
// n_embd=5120, n_heads=24) целочисленное деление даёт 213 вместо 213.33 —
// осознанная аппроксимация. Для KV-cache она НЕ используется, если в GGUF есть
// attention.key_length (см. EffectiveKVHeadDim).
func (m ModelSpec) HeadDim() int {
	if m.NHeads <= 0 {
		return 0
	}
	return m.NEmbd / m.NHeads
}

// Complete — достаточно ли данных, чтобы вообще считать.
func (m ModelSpec) Complete() bool {
	return m.SizeBytes > 0 && m.EffectiveKVLayers() > 0 && m.EffectiveKVHeadDim() > 0 && m.NKvHeads > 0
}

// KVType — тип квантования KV-cache.
type KVType string

const (
	KVF16 KVType = "f16"
	KVQ8  KVType = "q8_0"
	KVQ4  KVType = "q4_0"
)

// Normalize принимает пустое значение как f16 — единственное безопасное
// (наибольший KV). Явное «не знаю» живёт в Budget/Stage, а не в типе KV.
func (t KVType) Normalize() KVType {
	switch t {
	case KVQ4:
		return KVQ4
	case KVQ8:
		return KVQ8
	default:
		return KVF16
	}
}

// KVBytesPerToken — ЕДИНСТВЕННАЯ формула KV-cache на весь проект.
//
//	bytes/token = 2 (K и V) × kv_layers × n_kv_heads × kv_head_dim × bytes_per_elem
//
// ГДЕ kv_layers — НЕ число блоков модели, а число слоёв, которые реально хранят
// KV-кэш. У гибридных моделей (Qwen3.8: qwen35) рекуррентные слои linear attention
// KV не хранят вовсе: у Qwen3.8-27B KV считают 17 слоёв из 65, а head_dim равен
// 256 (<arch>.attention.key_length), а не 213 (n_embd/n_heads). Наивная формула
// «2 × block_count × kv_heads × (n_embd/n_heads)» завышала KV в 3.13 раза
// (218 112 против 69 632 Б/токен при f16) — и вместе с ним все потолки n_ctx.
// Разбор правила и ссылки на llama.cpp: internal/cppbackend/kv_layers.go.
//
// bytes_per_elem берётся из KVBytesPerElement — одной константы на весь стек
// (её же использует legacy-оценка в cmd/cppworker/auto_offload.go). Раньше
// константа дублировалась, и legacy считал q8_0 как ровно 2 байта вместо 34/32
// (R83 §9.4 шаг 2, 2026-09-27).
func KVBytesPerToken(m ModelSpec, t KVType) Bytes {
	hd := m.EffectiveKVHeadDim()
	layers := m.EffectiveKVLayers()
	if hd <= 0 || layers <= 0 || m.NKvHeads <= 0 {
		return 0
	}
	elements := int64(2) * int64(layers) * int64(m.NKvHeads) * int64(hd)
	num, den := KVBytesPerElement(t)
	return Bytes(elements * num / den)
}

// KVBytesPerElement — байт на элемент KV-кэша с учётом типа (числитель и
// знаменатель, потому что для квантованных типов значение дробное).
//
// Как в llama.cpp: значение + масштаб на блок.
//
//	f16  → 2/1   (2 байта: fp16)
//	q8_0 → 34/32 (1 байт + 1/32 на масштаб)
//	q4_0 → 18/32 (0.5 байта + 1/32 на масштаб)
//
// Возвращает (num, den): вызывающий делит СНАЧАЛА умножив на num, чтобы не
// терять точность на целочисленном делении.
func KVBytesPerElement(t KVType) (num, den int64) {
	switch t.Normalize() {
	case KVQ8:
		return 34, 32
	case KVQ4:
		return 18, 32
	}
	return 2, 1
}
