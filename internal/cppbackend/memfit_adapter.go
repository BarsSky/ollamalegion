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

// vramEnvOverrideBytes — VRAM из ENV-переопределений (тесты/CI и явная настройка
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
	meta, err := mm.GetModelMeta(name)
	if err != nil || meta == nil {
		if _, canonical, ok := mm.FindModelByVariants(name); ok {
			meta, err = mm.GetModelMeta(canonical)
		}
	}
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
	kvLayers, _ := meta.KVLayers()
	return MemfitSpecFromValues(name, meta.SizeBytes, meta.NLayers, meta.NHeads, meta.NKvHeads,
		meta.NEmbd, meta.ContextLength, kvLayers, meta.KVHeadDim())
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
	meta, err := mm.GetModelMeta(name)
	if err != nil || meta == nil {
		if _, canonical, ok := mm.FindModelByVariants(name); ok {
			meta, err = mm.GetModelMeta(canonical)
		}
	}
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
	meta, err := mm.GetModelMeta(name)
	if err != nil || meta == nil {
		if _, canonical, ok := mm.FindModelByVariants(name); ok {
			meta, err = mm.GetModelMeta(canonical)
		}
	}
	if err != nil || meta == nil {
		return 0, false
	}
	kvLayers, _ := meta.KVLayers()
	headDim := meta.KVHeadDim()
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
	elements := int64(kvLayers) * int64(heads) * int64(headDim) * 2
	return int64(nCtx) * elements * num / den, true
}
