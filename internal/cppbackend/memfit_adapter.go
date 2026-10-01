// memfit_adapter.go — адаптеры между cppbackend и internal/memfit.
//
// R83, шаги 2–3 миграции. Сначала это был мост для shadow-сравнения (считаем и
// старым кодом, и memfit, решение принимает старый). После переключения решений
// (шаг 3) shadow-машинерия удалена: и потолки n_ctx, и гейт загрузки, и раскладка
// слоёв на GPU считаются memfit, поэтому сравнивать больше не с чем.
//
// Здесь остаётся только сбор ВХОДНЫХ данных: снимок ресурсов (Budget) и описание
// модели (ModelSpec). Формулы и вердикт — в internal/memfit.
//
// Подробности: plans/2026-09-25-memory-fit-subsystem.md.
package cppbackend

import (
	"os"
	"strconv"
	"strings"

	"ollama-loadbalancer/internal/memfit"
	"ollama-loadbalancer/pkg/logger"
)

// defaultRAMReserveMBForFit — системный резерв под ОС и runtime (как в
// CalculateResourceLimits). Задан здесь явно, а не четвёртым числом в коде.
const defaultRAMReserveMBForFit = 4096

// MemfitPolicy — политика решения о памяти.
//
// Одна скидка на ресурс: VRAM − 2 GiB (CUDA-контекст и буферы), RAM − 4 GiB
// (система и runtime). Дополнительная доля RAMUtil не применяется — см.
// memfit.Policy: сочетание 0.8 и резерва 4 GiB давало двойную скидку и отказывало
// в загрузке 27B на машине, где она физически грузится и работает.
func MemfitPolicy() memfit.Policy {
	return memfit.Policy{VRAMUtil: 1.0, RAMUtil: 1.0, UnknownIsFatal: false}
}

// EffectiveKVCacheType — какой тип KV-cache реально пойдёт в расчёт (per-request,
// иначе дефолт конфига). Экспортирован, чтобы решения принимались на тех же
// входных данных, что и прежний код: иначе f16 против q4_0 даёт разницу вчетверо.
func (b *Backend) EffectiveKVCacheType(perRequest string) string {
	if b == nil {
		return "f16"
	}
	return effectiveKVCacheType(perRequest, b.cfg.DefaultKVCacheType)
}

// MemfitKVType переводит строковый тип KV-cache в тип пакета memfit.
// Пустая строка и неизвестные значения → f16 (безопасный верх: наибольший KV).
func MemfitKVType(s string) memfit.KVType {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "q4_0":
		return memfit.KVQ4
	case "q8_0":
		return memfit.KVQ8
	default:
		return memfit.KVF16
	}
}

// MemfitBudget собирает снимок ресурсов из живого состояния backend'а и системы.
// VRAM обновляется из C-bridge (как раньше в CalculateResourceLimits), RAM — из
// memfit.ProbeRAM (env → /proc/meminfo → cgroup), см. ram_available.go.
func (b *Backend) MemfitBudget() memfit.Budget {
	budget := memfit.Budget{
		VRAMReserve: memfit.MiBOf(int64(vramOverheadMBConfig())),
		RAMReserve:  memfit.MiBOf(defaultRAMReserveMBForFit),
	}

	if b != nil {
		b.mu.Lock()
		// R83 (2026-09-29): НЕ зовём bridge.GetGPUInfo из горячего пути.
		//
		// ЗАЧЕМ. GetGPUInfo делает cudaMemGetInfo, а CUDA-вызовы сериализуются:
		// пока идёт генерация, вызов ждёт освобождения контекста. MemfitBudget
		// вызывается из CalculateResourceLimits, тот — из handleListModels, значит
		// GET /api/models ЗАВИСАЛ на всё время инференса.
		//
		// Живой симптом (2026-09-29, воспроизведено): во время генерации
		// /api/models и /api/info не отвечали 8+ секунд, тогда как /api/gpu и
		// /api/models/active-queries отвечали за 4-7 мс. Из-за этого балансер
		// (backendReachable с таймаутом 2 c) решал «cppworker недоступен» и на
		// ВТОРОЙ параллельный запрос отвечал 503 «model is not loaded and
		// auto-load failed: backend unreachable», хотя модель была загружена и
		// обслуживала первый запрос.
		//
		// Теперь берём УЖЕ СОБРАННЫЙ снимок b.gpuDevices (его обновляют
		// RefreshGPUDevices при загрузке/выгрузке и поллеры). Значения могут быть
		// на несколько секунд старше — для вердикта memfit это допустимо, потому
		// что решения принимаются перед загрузкой, а не во время генерации.
		var free, total uint64
		for _, dev := range b.gpuDevices {
			total += uint64(dev.VRAMTotalMB)
			free += uint64(dev.VRAMFreeMB)
		}
		b.mu.Unlock()

		if total > 0 {
			budget.VRAMTotal = memfit.MiBOf(int64(total))
			budget.VRAMFree = memfit.MiBOf(int64(free))
			budget.VRAMKnown = true
		}
	}

	pr := memfit.ProbeRAM()
	budget.RAMAvail = pr.Available
	budget.RAMTotal = pr.Total
	budget.RAMLimit = pr.Limit
	budget.RAMLimitKnown = pr.LimitKnown
	budget.RAMKnown = pr.Known
	budget.Source = pr.Source

	// R83 C1 (2026-09-27): ENV-переопределение VRAM должно быть видно и бюджету.
	//
	// ЧТО БЫЛО: budget.VRAMKnown выставлялся только из bridge/gpuDevices, поэтому
	// на стенде без видимого GPU (stub, отвалившийся NVML) CPPWORKER_VRAM_BYTES и
	// CPPWORKER_FREE_VRAM_BYTES игнорировались: вердикт считал VRAM неизвестной и
	// объявлял cpu_only, хотя оператор явно задал объём. Теперь обе переменные
	// (те же, что читает availableVRAMBytes/freeVRAMBytes в cppworker) имеют
	// приоритет: это явное намерение оператора и единственный способ проверить
	// GPU-сценарий без GPU.
	if !budget.VRAMKnown {
		if total, free, ok := vramEnvOverrideBytes(); ok {
			budget.VRAMTotal = memfit.Bytes(total)
			budget.VRAMFree = memfit.Bytes(free)
			budget.VRAMKnown = true
			budget.Source = budget.Source + "+env"
		}
	}
	return budget
}

// MemfitBudgetFor — бюджет для решения о загрузке/ПЕРЕзагрузке модели name.
//
// R83-фикс (2026-10-01): если эта модель уже загружена, её собственная VRAM
// возвращается в бюджет — при перезагрузке она освобождается. Без этого решение
// принималось по «свободно 4.7 ГБ, из них минус резерв 2 ГБ», модель
// раскладывалась на GPU лишь частично (18-20 слоёв из 43), 25 слоёв считались
// на CPU, и prefill 22.6k токенов занимал ~300 с — клиент (Cline, лимит 300 с)
// не дожидался первого токена.
//
// Живой замер (2026-10-01, gemma-4 Q4_K_M на RTX 3070 8 GB):
//   memfit: requestedGPULayers=40 → optimal=18, vramAvailableMB=2671;
//   фактически после загрузки 18 слоёв: used=3476 МБ, свободно 4715 МБ.
// То есть ~4.7 ГБ простаивало, а половина модели считалась на CPU.
func (b *Backend) MemfitBudgetFor(reclaimModel string) memfit.Budget {
	budget := b.MemfitBudget()
	if b == nil || strings.TrimSpace(reclaimModel) == "" || !budget.VRAMKnown {
		return budget
	}

	// Сколько занимает СЕЙЧАС загруженный экземпляр этой модели: веса на GPU +
	// KV-кэш. Считаем по тем же величинам, которыми оперирует memfit.
	b.mu.RLock()
	inst, ok := b.models[reclaimModel]
	b.mu.RUnlock()
	if !ok || inst == nil {
		return budget
	}
	info := inst.info
	if info.SizeBytes == 0 || info.NLayers <= 0 || info.GPULayers == 0 {
		return budget
	}

	reclaim := WeightsOnGPUBytes(int64(info.SizeBytes), info.GPULayers, info.NLayers)
	if kv, real := b.KVCacheBytesForModel(reclaimModel, info.ContextSize, info.KVCacheType,
		info.NLayers, info.NEmbd, info.NHeads, info.NKvHeads); kv > 0 && real {
		reclaim += uint64(kv)
	}
	if reclaim == 0 {
		return budget
	}

	before := budget.VRAMFree
	// Bytes — базовый целочисленный тип, сложение напрямую.
	budget.VRAMFree = budget.VRAMFree + memfit.Bytes(reclaim)
	if budget.VRAMTotal > 0 && budget.VRAMFree > budget.VRAMTotal {
		budget.VRAMFree = budget.VRAMTotal
	}
	if logger.Get() != nil {
		logger.Get().Debugw("MemfitBudgetFor: память перезагружаемой модели возвращена в бюджет",
			"model", reclaimModel,
			"reclaim_mb", reclaim/(1024*1024),
			"free_before_mb", before.MiB(),
			"free_after_mb", budget.VRAMFree.MiB(),
			"total_mb", budget.VRAMTotal.MiB())
	}
	return budget
}


// оператором). Возвращает (total, free, ok); free по умолчанию равен total.
func vramEnvOverrideBytes() (total, free int64, ok bool) {
	parse := func(name string) (int64, bool) {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			return 0, false
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return 0, false
		}
		return n, true
	}
	if t, hasTotal := parse("CPPWORKER_VRAM_BYTES"); hasTotal {
		total = t
		free = t
		if f, hasFree := parse("CPPWORKER_FREE_VRAM_BYTES"); hasFree {
			free = f
		}
		return total, free, true
	}
	if f, hasFree := parse("CPPWORKER_FREE_VRAM_BYTES"); hasFree {
		return f, f, true
	}
	return 0, 0, false
}

// WeightsOnGPUBytes — сколько байт весов модели уедет на GPU при gpuLayers слоях
// из totalLayers.
//
// R83 §9.4 (2026-09-26): это та же пропорция, которой пользуется
// memfit.computeSplit (`ModelSpec.SizeBytes.Scale(g, n_layers)`), вынесенная для
// вызывающих, которым нужна ОЦЕНКА ЧИСЛА, а не вердикт: выбор бэкенда, warmup и
// scoring сравнивают байты, чтобы принять решение о маршрутизации.
//
// Заменяет legacy EstimateGPUMemoryForModel в части весов: та формула брала
// «70% модели» (×0.7) — занижение, из-за которого на 8 GB карте планировалось
// меньше слоёв, чем помещается, и модель уходила в CPU-only.
//
// gpuLayers: 0 = CPU-only (весов на GPU нет), -1 = «все слои» (как в llama.cpp),
// >= totalLayers = тоже все. Важно: 0 и -1 — РАЗНЫЕ случаи, и legacy-функция,
// которую эта замена закрывает, различала их так же.
func WeightsOnGPUBytes(sizeBytes int64, gpuLayers, totalLayers int) uint64 {
	if sizeBytes <= 0 || totalLayers <= 0 {
		return 0
	}
	if gpuLayers == 0 {
		return 0
	}
	if gpuLayers < 0 || gpuLayers >= totalLayers {
		return uint64(sizeBytes)
	}
	return uint64(int64(sizeBytes) * int64(gpuLayers) / int64(totalLayers))
}

// MemfitSpec собирает описание модели по внешнему имени (включая «qwen3.8:latest»).
// Резолв — тем же путём, что и сама загрузка.
func (b *Backend) MemfitSpec(name string) (memfit.ModelSpec, bool) {
	if b == nil || strings.TrimSpace(name) == "" {
		return memfit.ModelSpec{}, false
	}
	mm := b.ModelManager()
	if mm == nil {
		return memfit.ModelSpec{}, false
	}
	meta, err := mm.GetModelMetaResolved(name)
	if err != nil || meta == nil {
		return memfit.ModelSpec{}, false
	}
	return MemfitSpecFromMeta(name, meta), true
}

// MemfitSpecFromMeta — сборка ModelSpec из метаданных GGUF, включая параметры
// реального KV-кэша (число слоёв с KV и head_dim KV). Без них memfit считал бы KV
// по block_count и n_embd/n_heads, то есть завышал бы его в разы (kv_layers.go).
func MemfitSpecFromMeta(name string, meta *GGUFModelMeta) memfit.ModelSpec {
	if meta == nil {
		return memfit.ModelSpec{}
	}
	plan := meta.KVPlan()
	kvLayers := plan.KVLayers()
	if kvLayers == 0 {
		kvLayers, _ = meta.KVLayers()
	}
	spec := MemfitSpecFromValues(name, meta.SizeBytes, meta.NLayers, meta.NHeads, meta.NKvHeads,
		meta.NEmbd, meta.ContextLength, kvLayers, meta.KVHeadDim())
	// R83 (2026-10-01): оконные слои (gemma4/gemma3n) держат только окно токенов,
	// поэтому их KV не растёт с n_ctx. Без этого memfit считал «все слои × n_ctx»
	// и отказывал в полном оффлоаде (замер стенда: 35/42 слоёв, 5-9 tok/s).
	spec.KVSWALayers = plan.SWALayers
	spec.SWAWindow = plan.SWAWindow
	spec.SWAHeadDim = plan.SWAHeadDim
	if plan.GlobalHeadDim > 0 {
		spec.KVHeadDim = plan.GlobalHeadDim
	}
	return spec
}

// KVLayersForModel — параметры РЕАЛЬНОГО KV-кэша модели по внешнему имени.
//
// R83 §9.4 шаг 2 (2026-09-27). Зачем отдельно от MemfitSpec: legacy-фоллбэк
// auto_offload (когда memfit не может собрать полный ModelSpec — например, в
// метаданных нет обучающего контекста) считал KV по block_count и
// n_embd/n_heads. На Qwen3.8-27B это 64 слоя × 213 = 13 632 «пары» против
// реальных 16 слоёв × 256 = 4 096, то есть завышение в 3.3 раза — ровно тот
// дефект, из-за которого исторически планировалось gpu_layers=0.
//
// Достаточно именно этих двух чисел: они не требуют TrainCtx, поэтому доступны
// и там, где полный ModelSpec ещё «неполный». ok=false — метаданных нет,
// вызывающий обязан взять консервативную верхнюю оценку и сказать об этом в лог.
func (b *Backend) KVLayersForModel(name string) (layers, headDim int, ok bool) {
	if b == nil || strings.TrimSpace(name) == "" {
		return 0, 0, false
	}
	mm := b.ModelManager()
	if mm == nil {
		return 0, 0, false
	}
	meta, err := mm.GetModelMetaResolved(name)
	if err != nil || meta == nil {
		return 0, 0, false
	}
	kvLayers, _ := meta.KVLayers()
	headDim = meta.KVHeadDim()
	if kvLayers <= 0 || headDim <= 0 {
		// Частичные метаданные: отдаём то, что знаем, но помечаем «неточно» —
		// вызывающий решит, доверять ли (см. auto_offload).
		return kvLayers, headDim, false
	}
	return kvLayers, headDim, true
}

// applyKVPlanToSpec — R83/v52 (2026-10-01): перенести раскладку KV-кэша из
// метаданных модели в спецификацию памяти.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ. checkVRAMForModel собирал spec вручную и брал из
// метаданных только «слоёв с кэшем» + head_dim. Для gemma-4 этого мало: 24 слоя
// с кэшем, но 20 из них живут в окне 512 токенов, и без оконных полей memfit
// снова считал KV как «все слои × n_ctx» (3.17 GB вместо 294 MiB на 65536) и
// оставлял 7 слоёв из 42 на CPU — генерация 5-9 tok/s вместо 30+. Ошибку ловит
// TestApplyKVPlanToSpec_Gemma4.
func applyKVPlanToSpec(spec *memfit.ModelSpec, meta *GGUFModelMeta) {
	if spec == nil || meta == nil {
		return
	}
	plan := meta.KVPlan()
	if l := plan.KVLayers(); l > 0 {
		spec.KVLayers = l
	}
	if hd := plan.GlobalHeadDim; hd > 0 {
		spec.KVHeadDim = hd
	}
	spec.KVSWALayers = plan.SWALayers
	spec.SWAWindow = plan.SWAWindow
	spec.SWAHeadDim = plan.SWAHeadDim
}

// MemfitSpecFromValues — сборка ModelSpec из уже известных чисел (например, из
// GGUF-заголовка, прочитанного в checkVRAMForModel: там файл повторно не читается).
// kvLayers/kvHeadDim = 0 означают «неизвестно» → memfit берёт верхнюю оценку.
func MemfitSpecFromValues(name string, sizeBytes int64, nLayers, nHeads, nKvHeads, nEmbd, trainCtx, kvLayers, kvHeadDim int) memfit.ModelSpec {
	return memfit.ModelSpec{
		Name:      name,
		SizeBytes: memfit.Bytes(sizeBytes),
		NLayers:   nLayers,
		KVLayers:  kvLayers,
		NHeads:    nHeads,
		NKvHeads:  nKvHeads,
		NEmbd:     nEmbd,
		KVHeadDim: kvHeadDim,
		TrainCtx:  trainCtx,
	}
}

// KVCacheBytesForModel — размер KV-кэша модели на n_ctx токенов по РЕАЛЬНЫМ
// метаданным, с явным флагом достоверности.
//
// R83 §9.4 шаг 2, продолжение (2026-09-27). Зачем отдельная функция: после того
// как раскладка слоёв стала считать KV из метаданных, в cppworker остались ещё
// пять мест со «своей» консервативной формулой (n_layers = block_count,
// head_dim = n_embd/n_heads). Из них два меняют КОНФИГУРАЦИЮ, а не только текст:
//
//   - AutoTuneNCtx — подбирает n_ctx и gpu_layers при reload на больший контекст;
//   - SelectStrategy (adaptive_loader) — свой выбор n_ctx/gpu_layers.
//
// Завышенный KV в них означает заниженный контекст: оператор просит 65536, а
// получает меньше, хотя железо позволяет (и гейт n_ctx это подтвердил).
//
// Реализация — ровно наоборот безопасная: реальные числа берутся, только если
// метаданные ЕСТЬ и полны (kvLayers>0, kvHeadDim>0). Иначе возвращается прежняя
// консервативная оценка и real=false, чтобы вызывающий мог сказать об этом в лог.
// Компромисс здесь выбран осознанно: KV, посчитанный по block_count, завышает
// (безопасно), а выдуманный «на глазок» — может занизить (опасно, переподписка).
func (b *Backend) KVCacheBytesForModel(name string, nCtx int, kvCacheType string,
	nLayers, nEmbd, nHeads, nKvHeads int) (bytes int64, real bool) {
	if b == nil || strings.TrimSpace(name) == "" {
		return 0, false
	}
	mm := b.ModelManager()
	if mm == nil {
		return 0, false
	}
	meta, err := mm.GetModelMetaResolved(name)
	if err != nil || meta == nil {
		return 0, false
	}
	plan := meta.KVPlan()
	kvLayers := plan.KVLayers()
	headDim := plan.GlobalHeadDim
	if headDim <= 0 {
		headDim = meta.KVHeadDim()
	}
	if kvLayers == 0 {
		kvLayers, _ = meta.KVLayers()
	}
	heads := meta.NKvHeads
	if heads <= 0 {
		heads = nKvHeads
	}
	if heads <= 0 {
		heads = nHeads
	}
	if kvLayers <= 0 || headDim <= 0 || heads <= 0 || nCtx <= 0 {
		return 0, false
	}
	num, den := memfit.KVBytesPerElement(MemfitKVType(kvCacheType))
	// R83 (2026-10-01): оконные слои ограничены окном, а не n_ctx.
	globalLayers := plan.GlobalLayers
	if globalLayers <= 0 && plan.SWALayers == 0 {
		globalLayers = kvLayers
	}
	perLayer := func(hd int) int64 {
		if hd <= 0 {
			hd = headDim
		}
		return int64(2) * int64(heads) * int64(hd) * num / den
	}
	total := int64(nCtx) * int64(globalLayers) * perLayer(headDim)
	if swa := plan.SWALayers; swa > 0 {
		swaCtx := nCtx
		if plan.SWAWindow > 0 && swaCtx > plan.SWAWindow {
			swaCtx = plan.SWAWindow
		}
		total += int64(swaCtx) * int64(swa) * perLayer(plan.SWAHeadDim)
	}
	return total, true
}
