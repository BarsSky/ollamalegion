package cppbackend

import (
	"fmt"
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/internal/memfit"
	"ollama-loadbalancer/pkg/logger"
	"os"
)

// refreshGPUDeviceVRAM — R66d (2026-09-23): обновляет свободную/полную VRAM
// каждого устройства из C-bridge (NVML).
//
// Зачем отдельный метод: b.gpuDevices заполняется один раз при инициализации и
// больше не обновляется, а от его VRAMFreeMB зависит решение
// CalculateOptimalGPULayers о числе слоёв на GPU. Устаревший снимок приводил к
// ложному «не хватает VRAM» и частичному CPU-offload сразу после reload
// (подробности — в комментарии в CalculateOptimalGPULayers).
//
// В stub-сборке (--tags llama_stub) bridge.GetGPUInfo всегда возвращает ошибку,
// поэтому инжектированные в тестах значения gpuDevices остаются нетронутыми.
func (b *Backend) refreshGPUDeviceVRAM() {
	if b == nil || b.gpuCount <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := 0; i < b.gpuCount; i++ {
		dev, err := bridge.GetGPUInfo(i)
		if err != nil || dev == nil {
			continue
		}
		if i < len(b.gpuDevices) {
			b.gpuDevices[i].VRAMFreeMB = dev.VRAMFreeMB
			b.gpuDevices[i].VRAMTotalMB = dev.VRAMTotalMB
		}
	}
}

// estimateKVCacheMB вычисляет размер KV Cache в MB для заданных параметров модели.
// Формула:
//
//	KV Cache (bytes) = (head_dim_k + head_dim_v) × n_layers × n_ctx × n_kv_heads × bytes_per_elem
//
// где head_dim_k/v — РЕАЛЬНЫЕ ширины голов K и V (R705: из llama_hparams, то есть
// <arch>.attention.key_length/value_length). Если они не переданы (0), берётся
// прежняя формула n_embd / n_heads — для вызовов без геометрии и для сверки.
// Для GQA моделей n_kv_heads < n_heads, для MHA n_kv_heads = n_heads.
//
// Добавляет 10% буфера безопасности для избежания граничных OOM.
// EstimateKVCacheMB — экспорт расчёта KV-кэша для потребителей вне пакета.
//
// ЗАЧЕМ (R313). cppworker отдаёт `size_vram` в /api/models и /api/ps — «сколько
// модель занимает в VRAM». Раньше это была ГРУБАЯ оценка `size × 3.0` (допущение
// «квантованные веса разворачиваются в fp16»), без KV и без учёта частичной
// выгрузки: на стенде gemma (4.2 ГБ, ctx 32768) показывала 12.6 ГБ VRAM на
// 8-гиговой карте. Число уезжало в балансер (poller → VRAMUsage), а оттуда в
// autotune-бюджет: `min(free + usage, total)` клипуется размером карты, поэтому
// решения не ломались, но оператор видел физически невозможное значение.
//
// R705 (2026-11-24): head_dim_k/head_dim_v — ПАРАМЕТРЫ, а не n_embd/n_heads.
// У Qwen3-4B это 128 против 80 (KV занижался на 60 %), у gemma-4-E4B — 512
// против 320, у Inkling-2-Mini — 128 против 96. size_vram — не только показ: он
// уезжает в балансер (VRAMUsage → autotune-бюджет), поэтому ширина обязана быть
// реальной. Ноль в параметрах означает «геометрии нет» и включает прежнюю
// формулу — обратная совместимость всех существующих вызовов сохранена.
//
// Считать KV здесь, а в воркере — только вызывать: второй формулы быть не должно
// (иначе «в логе одно, в решении другое», ровно как в R83).
func EstimateKVCacheMB(nLayers, nHeads, nKvHeads, nEmbd, headDimK, headDimV, nCtx int, kvType string) int {
	return int(estimateKVCacheMB(nLayers, nHeads, nKvHeads, nEmbd, headDimK, headDimV, nCtx, kvType))
}
func estimateKVCacheMB(nLayers, nHeads, nKvHeads, nEmbd, headDimK, headDimV, nCtx int, kvCacheType string) uint64 {
	if nLayers <= 0 || nCtx <= 0 {
		return 0
	}

	// Ширина головы: сначала РЕАЛЬНАЯ (R705), затем прежняя формула.
	// Safe-fail сохраняется: если ни реальной ширины, ни n_embd/n_heads нет,
	// посчитать корректно нельзя, и лучше вернуть 0, чем выдумать число
	// (caller использует другой метод оценки / консервативный fallback).
	hdK, hdV := headDimK, headDimV
	if hdK <= 0 || hdV <= 0 {
		if nHeads <= 0 || nEmbd <= 0 {
			return 0
		}
		formula := nEmbd / nHeads
		if hdK <= 0 {
			hdK = formula
		}
		if hdV <= 0 {
			hdV = hdK
		}
	}
	if hdK <= 0 || hdV <= 0 {
		return 0
	}

	if nKvHeads <= 0 {
		nKvHeads = nHeads // MHA (no GQA)
	}
	if nKvHeads <= 0 {
		return 0
	}

	// KV Cache = (K+V) × n_layers × n_ctx × n_kv_heads × bytes_per_elem
	//
	// R67a (2026-09-23): bytes_per_elem зависит от kvCacheType (f16 → 2 байта,
	// q8_0 → 34/32, q4_0 → 18/32). Раньше всегда было 2 (fp16), из-за чего при
	// q8_0/q4_0 требования к VRAM завышались в 2-4 раза и n_ctx упирался в
	// заниженный потолок даже на GPU с большим запасом VRAM.
	num, den := kvCacheBytesPerElem(kvCacheType)
	elements := uint64(hdK+hdV) * uint64(nLayers) * uint64(nCtx) * uint64(nKvHeads)
	kvBytes := elements * num / den

	// Конвертируем в MB и добавляем 10% безопасности
	kvMB := kvBytes / (1024 * 1024)
	kvMB = kvMB + kvMB/10 // +10% safety buffer

	return kvMB
}

// checkVRAMForModel проверяет, достаточно ли видеопамяти для загрузки модели
// и автоматически подбирает оптимальное количество GPU-слоёв.
// Возвращает (opts, error) — модифицированные опции с оптимальными GPU-слоями.
func (b *Backend) checkVRAMForModel(name string, path string, opts LoadModelOpts) (retOpts LoadModelOpts, retErr error) {
	// Panic recovery: readGGUFHeaderInfo может запаниковать если GGUF файл повреждён.
	// В этом случае пропускаем VRAM-проверку и загружаем модель с оригинальными опциями.
	defer func() {
		if r := recover(); r != nil {
			logger.Get().Errorw("panic in checkVRAMForModel (GGUF header read), skipping VRAM check",
				"model", name,
				"path", path,
				"recover", r)
			retOpts = opts // возвращаем оригинальные опции без изменений
			retErr = nil   // не блокируем загрузку
		}
	}()

	// Получаем размер файла модели
	fi, err := os.Stat(path)
	if err != nil {
		// Если файл не существует, пропускаем проверку (может быть ещё не скачан)
		return opts, nil
	}

	// Извлекаем метаданные из GGUF header ДО загрузки модели.
	// Сначала пытаемся получить из уже загруженной модели (если уже загружена).
	var header *GGUFHeaderInfo
	totalLayers := 0
	nHeads := 0
	nKvHeads := 0
	nEmbd := 0

	// Если модель уже загружена — берём метаданные из неё
	if info, err := b.GetModel(name); err == nil && info.NLayers > 0 {
		totalLayers = info.NLayers
		nHeads = info.NHeads
		nKvHeads = info.NKvHeads
		nEmbd = info.NEmbd
	}

	// Если модель не загружена — читаем GGUF header
	if totalLayers <= 0 {
		var readErr error
		header, readErr = readGGUFHeaderInfo(path)
		if readErr == nil && header != nil {
			totalLayers = header.NLayers
			nHeads = header.NHeads
			nKvHeads = header.NKvHeads
			nEmbd = header.NEmbd
		}
	}

	// Если GGUF header не дал результатов — fallback на эвристику по размеру файла
	if totalLayers <= 0 {
		totalLayers = estimateLayersFromFileSize(fi.Size())
		// nHeads и nEmbd остаются 0, но nKvHeads=2 чтобы estimateKVCacheMB сработал с fallback
		if nKvHeads <= 0 {
			nKvHeads = 2
		}
	}

	// R67a: в оценке VRAM учитываем фактический тип KV-cache (из запроса/профиля,
	// иначе дефолт cppworker). Без этого при q8_0/q4_0 требования завышались в
	// 2-4 раза и модель уходила в частичный CPU-offload при свободной VRAM.
	effectiveKV := effectiveKVCacheType(opts.KVCacheType, b.cfg.DefaultKVCacheType)

	// R83, шаг 3 миграции: решение «влезает ли модель и сколько слоёв на GPU»
	// принимает internal/memfit — единственный предикат computeSplit. Прежний
	// CalculateOptimalGPULayers считал веса как «размер × 0.7», KV — по всем блокам
	// с head_dim = n_embd/n_heads, а RAM сравнивал с MemTotal; из-за этого
	// llama.cpp получал gpu_layers=0 и грузил модель целиком на CPU.
	spec := MemfitSpecFromValues(name, fi.Size(), totalLayers, nHeads, nKvHeads, nEmbd, 0, 0, 0)
	if mm := b.ModelManager(); mm != nil {
		// R83 (2026-10-01): GetModelMetaResolved, а не GetModelMeta — клиент зовёт
		// модель без «.gguf», и прямой lookup молча возвращал ошибку. Тогда KV
		// считался по всем слоям (у gemma-4 3.17 GB вместо 294 MiB) и вердикт
		// уходил в partial_offload/35 слоёв.
		if meta, err := mm.GetModelMetaResolved(name); err == nil && meta != nil {
			applyKVPlanToSpec(&spec, meta)
		}
	}
	// R396 (2026-11-04): режим mmap решается ДО memfit.
	//
	// 2026-06-26 BUGFIX (сохранено): для больших моделей (model_size > 70% свободной
	// VRAM) принудительно включаем useMmap=true. Без mmap веса форсируются в VRAM,
	// что приводит к OOM на partial offload.
	//
	// ПОЧЕМУ РЕШЕНИЕ ПЕРЕЕХАЛО ВЫШЕ. memfit обязан считать раскладку в том режиме,
	// в котором загрузка реально пойдёт: с mmap веса не обязаны быть резидентными в
	// RAM. Раньше эвристика срабатывала ПОСЛЕ вердикта, поэтому большая модель
	// отвергалась по RAM («does_not_fit») ещё до того, как mmap включался, — хотя
	// именно mmap и делает такую загрузку возможной.
	totalVRAMMB := uint64(0)
	b.mu.RLock()
	for _, dev := range b.gpuDevices {
		totalVRAMMB += uint64(dev.VRAMFreeMB)
	}
	b.mu.RUnlock()
	// S1 (2026-10-13): эвристика «модель больше 70% доступной VRAM → mmap» тоже
	// обязана видеть удалённые узлы: иначе для модели, которая влезает только в
	// паре «локальная карта + узел», mmap включался/выключался по локальной карте.
	if _, remoteFree, n := b.remoteVRAMBytes(); n > 0 {
		totalVRAMMB += remoteFree / (1024 * 1024)
	}
	modelSizeMB := fi.Size() / (1024 * 1024)
	if totalVRAMMB > 0 && uint64(modelSizeMB)*100 > totalVRAMMB*70 && !opts.UseMmap {
		logger.Get().Infow("forcing useMmap=true for large model",
			"model", name,
			"modelSizeMB", modelSizeMB,
			"vramFreeMB", totalVRAMMB,
			"reason", "model > 70% of available VRAM")
		opts.UseMmap = true
	}

	verdict := memfit.Evaluate(spec, memfit.Request{
		Ctx:       opts.ContextSize,
		KVType:    MemfitKVType(effectiveKV),
		GPULayers: opts.GPULayers, // <= 0 = авто: максимум, что влезает
	}, b.MemfitBudgetFor(name), MemfitPolicyForMmap(opts.UseMmap))

	if logger.Get() != nil {
		logger.Get().Debugw(i18n.T("cppbackend.backend.memory_verdict"),
			"model", name, "kv_cache_type", effectiveKV,
			"requested_gpu_layers", opts.GPULayers, "verdict", verdict.String())
	}

	// R83 (2026-09-30): снимок памяти делается ДО решения о загрузке, и memfit
	// НЕ знает про слоты — но это и не нужно: слоты не умножают KV-cache, они
	// ДЕЛЯТ окно. Проверено на стенде (c/llama.cpp gguf-v0.19.0-1369,
	// kv_unified=false): contextSize=8192 + parallel=2 → llama_context:
	// n_ctx=8192, n_ctx_seq=4096, и свободная VRAM не меняется
	// (parallel=1 → 5197 МБ, parallel=2 → 5197 МБ, parallel=3 → 5191 МБ).
	//
	// Единственное, что важно не перепутать: если оператор задаёт окно НА КЛИЕНТА
	// (opts.ContextPerSeq), суммарный n_ctx считается выше (PAD256(per) × слотов),
	// и в memfit уходит уже он — поэтому «16K на клиента + 2 слота» проверяется
	// как 32K. Предупреждение ниже — про разницу между суммарным окном и окном
	// на клиента, а не про «KV × слотов».
	if slots := resolveNParallel(opts.Parallel, b.cfg.DefaultNParallel); slots > 1 && logger.Get() != nil {
		total, per := EffectiveContextSize(opts.ContextSize, opts.ContextPerSeq, slots)
		logger.Get().Infow(i18n.T("cppbackend.backend.parallel_slots_window_divided"),
			"model", name, "parallel_slots", slots,
			"total_n_ctx", total, "context_per_slot", per,
			"hint", i18n.T("cppbackend.backend.parallel_slots_window_hint"))
	}

	// R83 §3.2/§3.4 (2026-09-26): числа KV-кэша (слои с KV и head_dim KV) —
	// из тех же метаданных, по которым memfit считал раскладку. C-bridge
	// использует их в оценке свободной VRAM; без них он считает KV по всем
	// слоям и n_embd/n_heads, завышая его почти в 10 раз.
	//
	// R83 (2026-10-01): берём ГЛОБАЛЬНУЮ часть раскладки (KVPlan.GlobalLayers).
	// Оконные слои в оценку «KV на токен × n_ctx» не входят: их кэш ограничен
	// окном, а не контекстом (у gemma-4-E4B это 20 слоёв из 24 с окном 512).
	if header != nil {
		plan := header.KVPlan()
		if plan.GlobalLayers > 0 {
			opts.KVLayers = plan.GlobalLayers
		} else if l, ok := header.KVLayers(); ok && l > 0 {
			opts.KVLayers = l
		}
		if hd := plan.GlobalHeadDim; hd > 0 {
			opts.KVHeadDim = hd
		} else if hd := header.KVHeadDim(); hd > 0 {
			opts.KVHeadDim = hd
		}
	}

	// «Неизвестно» — НЕ отказ: если метаданных модели нет (нет файла, скан не
	// прошёл), судить не о чем, и запрошенные опции остаются как есть. Иначе
	// сервис отказывался бы загружать всё, чего нет в каталоге (и ломал бы
	// stub-режим, где моделей на диске нет вовсе).
	if verdict.Stage == memfit.StageUnknown {
		if logger.Get() != nil {
			logger.Get().Debugw(i18n.T("cppbackend.backend.no_model_metadata"),
				"model", name, "requested_gpu_layers", opts.GPULayers)
		}
		return opts, nil
	}

	if !verdict.Fits() {
		logger.Get().Errorw("cannot load model - insufficient memory (memfit)",
			"model", name,
			"path", path,
			"verdict", verdict.String(),
			"suggestion", verdict.Suggestion)
		return opts, fmt.Errorf("cannot load model %s: %s", name, verdict.Suggestion)
	}

	// Раскладка слоёв и mmap — из вердикта. Вне exact_fit часть модели живёт в RAM,
	// поэтому mmap обязателен.
	if verdict.GPULayers != opts.GPULayers {
		logger.Get().Infow("auto-adapting GPU layers for model (memfit)",
			"model", name,
			"requestedGPULayers", opts.GPULayers,
			"optimalGPULayers", verdict.GPULayers,
			"stage", string(verdict.Stage),
			"vramRequiredMB", (verdict.GPUWeights + verdict.GPUKV).MiB(),
			"vramAvailableMB", verdict.UsableVRAM.MiB(),
			"ramRequiredMB", (verdict.CPUWeights + verdict.CPUKV).MiB(),
			"ramAvailableMB", verdict.UsableRAM.MiB())
		opts.GPULayers = verdict.GPULayers
	}
	if verdict.Stage != memfit.StageExactFit {
		opts.UseMmap = true
	}

	return opts, nil
}
