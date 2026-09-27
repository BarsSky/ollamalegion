package memfit

import (
	"fmt"
	"strings"
)

// Stage — итог решения. Явные состояния вместо прежней перегрузки нуля:
// «VRAM неизвестна» и «веса не влезают в VRAM» — РАЗНЫЕ стадии, поэтому их можно
// по-разному показывать и по-разному на них реагировать.
type Stage string

const (
	StageExactFit   Stage = "exact_fit"       // веса + KV целиком в VRAM
	StagePartial    Stage = "partial_offload" // часть слоёв на CPU, всё влезает
	StageCPUOnly    Stage = "cpu_only"        // GPU не участвует, но RAM хватает
	StageDoesNotFit Stage = "does_not_fit"    // не хватает суммы VRAM + RAM
	StageUnknown    Stage = "unknown"         // данных нет; что делать — решает Policy
)

// ReasonCode — машинночитаемая причина. Именно на неё должны опираться HTTP-код,
// уведомление в WebUI и метрика, а не на разбор строк.
type ReasonCode string

const (
	ReasonModelMetaMissing  ReasonCode = "model_metadata_missing"
	ReasonCtxMissing        ReasonCode = "n_ctx_not_specified"
	ReasonBudgetUnknown     ReasonCode = "budget_unknown"
	ReasonVRAMUnknown       ReasonCode = "vram_unknown"
	ReasonRAMUnknown        ReasonCode = "ram_unknown"
	ReasonCtxAboveTrain     ReasonCode = "n_ctx_above_training_context"
	ReasonWeightsExceedVRAM ReasonCode = "weights_exceed_free_vram"
	ReasonRAMShort          ReasonCode = "not_enough_ram_for_cpu_part"
	ReasonRequestedSplit    ReasonCode = "requested_gpu_layers_do_not_fit"
	ReasonKVQuantLever      ReasonCode = "kv_quantization_would_make_it_fit"
)

type Reason struct {
	Code   ReasonCode
	Detail string
}

// Request — что просят загрузить.
//
// ВАЖНО про нулевые значения: Request{} обязан означать «реши сам», поэтому
// GPULayers <= 0 — это АВТО (максимум слоёв, сколько влезет), а не «ноль слоёв на
// GPU». Явный ноль не нужен: автоматический ответ и есть cpu_only, когда в VRAM не
// влезает ни один слой. Такой семантикой zero value перестаёт быть ловушкой —
// ровно та ошибка (перегруженный ноль), из-за которой в прежнем коде 0 означал
// одновременно «VRAM неизвестна» и «веса не влезают».
type Request struct {
	// Ctx — запрошенный n_ctx.
	Ctx int
	// KVType — тип KV-cache ("" = f16).
	KVType KVType
	// GPULayers > 0 — ограничить число слоёв на GPU (например, при reload с
	// явными параметрами). <= 0 — автоматически, максимум по свободной VRAM.
	GPULayers int
}

// Verdict — структурированный результат. Один и тот же объект питает HTTP-ответ,
// лог, /api/models, load_failure.diagnostics и уведомление в WebUI.
type Verdict struct {
	Stage       Stage
	KVType      KVType
	GPULayers   int
	TotalLayers int

	KVPerToken Bytes
	KVTotal    Bytes
	Weights    Bytes

	GPUWeights Bytes
	CPUWeights Bytes
	GPUKV      Bytes
	CPUKV      Bytes

	UsableVRAM Bytes
	UsableRAM  Bytes

	// MaxExactFitCtx — потолок n_ctx для загрузки ЦЕЛИКОМ в VRAM.
	// MaxHardCtx — наибольший n_ctx, который реально помещается на этом железе.
	// Оба посчитаны тем же предикатом, что и сам вердикт (см. Ceilings).
	//
	// ВАЖНО: ноль здесь означает «не влезает даже с нулевым контекстом» ИЛИ
	// «данных о ресурсах нет» — различает CeilingsKnown. Именно перегруженный ноль
	// в прежнем max_vram_n_ctx не давал починить формулу: 0 одновременно означал
	// «VRAM неизвестна» и «веса не влезают» (план, V1).
	MaxExactFitCtx int
	MaxHardCtx     int
	CeilingsKnown  bool

	Reasons    []Reason
	Suggestion string
}

func (v Verdict) Fits() bool {
	switch v.Stage {
	case StageExactFit, StagePartial, StageCPUOnly:
		return true
	default:
		return false
	}
}

func (v Verdict) HasReason(code ReasonCode) bool {
	for _, r := range v.Reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}

func (v Verdict) ReasonDetail(code ReasonCode) string {
	for _, r := range v.Reasons {
		if r.Code == code {
			return r.Detail
		}
	}
	return ""
}

// String — одна строка для лога: всё, что нужно для разбора инцидента.
func (v Verdict) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "stage=%s kv=%s weights=%s kv_total=%s", v.Stage, v.KVType, v.Weights, v.KVTotal)
	fmt.Fprintf(&b, " gpu_layers=%d/%d", v.GPULayers, v.TotalLayers)
	fmt.Fprintf(&b, " vram_used=%s/%s ram_used=%s/%s",
		v.GPUWeights+v.GPUKV, v.UsableVRAM, v.CPUWeights+v.CPUKV, v.UsableRAM)
	fmt.Fprintf(&b, " max_exact_fit_n_ctx=%d max_hard_n_ctx=%d", v.MaxExactFitCtx, v.MaxHardCtx)
	for _, r := range v.Reasons {
		fmt.Fprintf(&b, " reason=%s(%s)", r.Code, r.Detail)
	}
	return b.String()
}

// split — результат ОДНОГО предиката «помещается ли», из которого строятся и
// вердикт, и потолки. Раньше потолок и решение считались разными формулами, и
// они расходились (на A10: потолок 135 648 при фактической нехватке 393 MiB,
// потому что непрерывная формула игнорирует целочисленный сплит слоёв).
type split struct {
	kvPerToken Bytes
	kvTotal    Bytes
	total      Bytes

	g          int // слоёв на GPU
	gpuWeights Bytes
	cpuWeights Bytes
	gpuKV      Bytes
	cpuKV      Bytes

	exactFit bool
	fits     bool
}

// computeSplit — единственный предикат. Сплит пропорционален числу слоёв: так
// делит llama.cpp (веса и KV уезжают на CPU вместе со слоями).
func computeSplit(m ModelSpec, ctx int, kv KVType, b Budget, p Policy) split {
	var s split
	if !m.Complete() {
		return s
	}
	kvPT := KVBytesPerToken(m, kv)
	if kvPT <= 0 || ctx <= 0 {
		return s
	}
	if m.TrainCtx > 0 && ctx > m.TrainCtx {
		return s
	}
	s.kvPerToken = kvPT
	s.kvTotal = kvPT.Mul(int64(ctx))
	s.total = m.SizeBytes + s.kvTotal

	uv, ur := b.UsableVRAM(p), b.UsableRAM(p)

	if s.total <= uv {
		s.exactFit = true
		s.fits = true
		s.g = m.NLayers
		s.gpuWeights, s.gpuKV = m.SizeBytes, s.kvTotal
		return s
	}

	gMax := 0
	if uv > 0 {
		gMax = int(int64(m.NLayers) * int64(uv) / int64(s.total))
		if gMax > m.NLayers {
			gMax = m.NLayers
		}
	}
	s.g = gMax
	s.gpuWeights = m.SizeBytes.Scale(int64(gMax), int64(m.NLayers))
	s.cpuWeights = m.SizeBytes - s.gpuWeights
	s.gpuKV = s.kvTotal.Scale(int64(gMax), int64(m.NLayers))
	s.cpuKV = s.kvTotal - s.gpuKV
	s.fits = s.cpuWeights+s.cpuKV <= ur
	return s
}

// Ceilings — потолки n_ctx для этой модели и этого железа.
//
// exactFit — чистая формула (все слои на GPU, гранулярность не важна):
//
//	(usableVRAM − weights) / kvPerToken
//
// hard — наибольший ctx, который помещается ПО ТОМУ ЖЕ предикату, что и вердикт
// (двоичный поиск). Параллельная «непрерывная» формула здесь запрещена: она
// расходится с целочисленным сплитом слоёв, и тогда «рекомендуем ≤ X» противоречит
// «на X не помещается».
func Ceilings(m ModelSpec, kv KVType, b Budget, p Policy) (exactFitCtx, hardCtx int) {
	kvPT := KVBytesPerToken(m, kv)
	if !m.Complete() || kvPT <= 0 {
		return 0, 0
	}

	if uv := b.UsableVRAM(p); uv > m.SizeBytes {
		exactFitCtx = int(uv.Sub(m.SizeBytes) / kvPT)
	}
	if m.TrainCtx > 0 && exactFitCtx > m.TrainCtx {
		exactFitCtx = m.TrainCtx
	}

	// Верхняя граница поиска: обучающий контекст, если известен, иначе заведомо
	// недостижимое значение (память кончится гораздо раньше).
	hi := m.TrainCtx
	if hi <= 0 {
		hi = 1 << 30
	}
	if s := computeSplit(m, hi, kv, b, p); s.fits {
		return exactFitCtx, hi
	}
	lo := 0
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if computeSplit(m, mid, kv, b, p).fits {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return exactFitCtx, lo
}

// Evaluate — единственное место, где принимается решение.
//
// Функция чистая: ни окружения, ни побочных эффектов, ни логов. Ровно поэтому её
// поведение проверяется golden-тестами на реальных числах железа.
func Evaluate(m ModelSpec, req Request, b Budget, p Policy) Verdict {
	kvType := req.KVType.Normalize()
	v := Verdict{
		KVType:      kvType,
		TotalLayers: m.NLayers,
		Weights:     m.SizeBytes,
		UsableVRAM:  b.UsableVRAM(p),
		UsableRAM:   b.UsableRAM(p),
	}

	if !m.Complete() {
		v.Stage = StageUnknown
		v.Reasons = append(v.Reasons, Reason{ReasonModelMetaMissing,
			"нет метаданных модели (size/n_layers/n_embd/n_heads/n_kv_heads) — оценивать нечего"})
		v.Suggestion = "Дождитесь сканирования моделей или укажите существующий GGUF-файл."
		return v
	}
	v.KVPerToken = KVBytesPerToken(m, kvType)
	if v.KVPerToken <= 0 {
		v.Stage = StageUnknown
		v.Reasons = append(v.Reasons, Reason{ReasonModelMetaMissing, "не удалось вычислить KV-cache на токен"})
		return v
	}
	v.MaxExactFitCtx, v.MaxHardCtx = Ceilings(m, kvType, b, p)
	v.CeilingsKnown = b.VRAMKnown || b.RAMKnown

	if req.Ctx <= 0 {
		v.Stage = StageUnknown
		v.Reasons = append(v.Reasons, Reason{ReasonCtxMissing, "n_ctx не задан"})
		return v
	}

	// Неопределённость ресурсов — отдельное состояние, а не тихий «пропуск проверки».
	if !b.VRAMKnown && !b.RAMKnown {
		if p.UnknownIsFatal {
			v.Stage = StageDoesNotFit
			v.Reasons = append(v.Reasons, Reason{ReasonBudgetUnknown,
				"данные о VRAM и RAM недоступны, политика fail-closed → загрузка отклонена"})
			v.Suggestion = "Проверьте, что cppworker видит GPU (nvidia-smi/runtime) и /proc/meminfo доступен."
		} else {
			v.Stage = StageUnknown
			v.Reasons = append(v.Reasons, Reason{ReasonBudgetUnknown, "данные о VRAM и RAM недоступны"})
		}
		return v
	}
	if !b.VRAMKnown {
		v.Reasons = append(v.Reasons, Reason{ReasonVRAMUnknown, "свободная VRAM неизвестна — расчёт только по RAM"})
	}
	if !b.RAMKnown {
		v.Reasons = append(v.Reasons, Reason{ReasonRAMUnknown, "доступная RAM неизвестна — расчёт только по VRAM"})
	}

	// Обучающий контекст модели — жёсткая граница, здесь ничего не «упирается в железо».
	if m.TrainCtx > 0 && req.Ctx > m.TrainCtx {
		v.Stage = StageDoesNotFit
		v.Reasons = append(v.Reasons, Reason{ReasonCtxAboveTrain,
			fmt.Sprintf("n_ctx=%d выше обучающего контекста модели %d", req.Ctx, m.TrainCtx)})
		v.Suggestion = fmt.Sprintf("Укажите n_ctx <= %d (обучающий контекст модели).", m.TrainCtx)
		return v
	}

	s := computeSplit(m, req.Ctx, kvType, b, p)
	v.KVTotal = s.kvTotal
	v.GPULayers = s.g
	v.GPUWeights, v.CPUWeights = s.gpuWeights, s.cpuWeights
	v.GPUKV, v.CPUKV = s.gpuKV, s.cpuKV

	// Явное ограничение числа слоёв: считаем именно запрошенный вариант.
	if req.GPULayers > 0 && req.GPULayers < s.g {
		g := req.GPULayers
		v.GPULayers = g
		v.GPUWeights = m.SizeBytes.Scale(int64(g), int64(m.NLayers))
		v.CPUWeights = m.SizeBytes - v.GPUWeights
		v.GPUKV = s.kvTotal.Scale(int64(g), int64(m.NLayers))
		v.CPUKV = s.kvTotal - v.GPUKV
		s.gpuWeights, s.cpuWeights, s.gpuKV, s.cpuKV = v.GPUWeights, v.CPUWeights, v.GPUKV, v.CPUKV
		s.fits = v.CPUWeights+v.CPUKV <= v.UsableRAM
		v.Reasons = append(v.Reasons, Reason{ReasonRequestedSplit,
			fmt.Sprintf("запрошено %d слоёв на GPU (по VRAM помещается %d)", req.GPULayers, s.g)})
	}

	if s.exactFit {
		v.Stage = StageExactFit
		v.Suggestion = fmt.Sprintf("n_ctx=%d помещается целиком в VRAM (запас %s).", req.Ctx, v.UsableVRAM.Sub(s.total))
		return v
	}

	if m.SizeBytes > v.UsableVRAM {
		v.Reasons = append(v.Reasons, Reason{ReasonWeightsExceedVRAM,
			fmt.Sprintf("веса %s больше доступной VRAM %s — полный GPU-оффлоад невозможен", m.SizeBytes, v.UsableVRAM)})
	}

	if s.fits {
		// R83 C1 (2026-09-27): «CPU-only» — это утверждение о VRAM, поэтому его
		// нельзя делать, когда VRAM НЕИЗВЕСТНА.
		//
		// ЧТО БЫЛО: при неизвестной VRAM UsableVRAM(p) = 0, computeSplit давал
		// gMax=0, и вердикт объявлял cpu_only — хотя на самом деле про VRAM мы
		// ничего не знаем. Cppworker сообщал оператору честный
		// reason=vram_unknown, но РЕШЕНИЕ всё равно принимал за него: на живом
		// GPU-стенде это означало бы «модель поехала на CPU, потому что NVML
		// отвалился». Настоящая причина cpu_only — «VRAM известна и в неё не
		// влезает ни один слой», и только тогда это cpu_only.
		//
		// ТЕПЕРЬ: при неизвестной VRAM стадия — partial_offload с нулём слов,
		// то есть раскладку оставляем прежней/на усмотрение C-стороны
		// (checkVRAMForModel), а не понижаем её сами. Причина vram_unknown уже
		// добавлена выше (стр. ~295), поэтому оператор видит, почему решение
		// неполное.
		if v.GPULayers == 0 && b.VRAMKnown {
			v.Stage = StageCPUOnly
			v.Suggestion = fmt.Sprintf("n_ctx=%d обслуживается только CPU (веса %s + KV %s в RAM, доступно %s). Ответ будет медленным.",
				req.Ctx, v.CPUWeights, v.CPUKV, v.UsableRAM)
		} else if v.GPULayers == 0 {
			v.Stage = StagePartial
			v.Suggestion = fmt.Sprintf("VRAM неизвестна — раскладка не понижается до CPU-only: "+
				"n_ctx=%d обслуживается CPU-частью (веса %s + KV %s в RAM, доступно %s), "+
				"слои на GPU определит C-сторона при загрузке.",
				req.Ctx, v.CPUWeights, v.CPUKV, v.UsableRAM)
		} else {
			v.Stage = StagePartial
			v.Suggestion = fmt.Sprintf("n_ctx=%d помещается с частичным оффлоадом: %d/%d слоёв на GPU, остальное в RAM.",
				req.Ctx, v.GPULayers, m.NLayers)
		}
		return v
	}

	// Не влезает. Считаем, что именно предложить.
	v.Stage = StageDoesNotFit
	cpuPart := v.CPUWeights + v.CPUKV
	v.Reasons = append(v.Reasons, Reason{ReasonRAMShort,
		fmt.Sprintf("CPU-части нужно %s (веса %s + KV %s), доступно %s — не хватает %s",
			cpuPart, v.CPUWeights, v.CPUKV, v.UsableRAM, cpuPart.Sub(v.UsableRAM))})

	if kvType != KVQ4 {
		if s4 := computeSplit(m, req.Ctx, KVQ4, b, p); s4.fits {
			v.Reasons = append(v.Reasons, Reason{ReasonKVQuantLever,
				fmt.Sprintf("с kvCacheType=q4_0 KV снижается с %s до %s на токен — запрос становится выполнимым",
					v.KVPerToken, s4.kvPerToken)})
		}
	}

	switch {
	case v.CeilingsKnown && v.MaxHardCtx > 0 && req.Ctx > v.MaxHardCtx:
		v.Suggestion = fmt.Sprintf("Физический потолок n_ctx на этом железе — %d. Уменьшите n_ctx до %d или используйте kvCacheType=q4_0.",
			v.MaxHardCtx, v.MaxHardCtx)
	case v.CeilingsKnown && v.MaxHardCtx > 0:
		v.Suggestion = fmt.Sprintf("Суммарной памяти (VRAM+RAM) хватает лишь на n_ctx=%d.", v.MaxHardCtx)
	default:
		v.Suggestion = "Веса модели не помещаются ни в VRAM, ни в RAM — нужен меньший квант или больше памяти."
	}
	return v
}
