package cppbackend

import (
	"context"
	"fmt"
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"os"
	"strings"
	"time"
)

// LoadModel загружает GGUF модель с параметрами по умолчанию
//
// R60.57 (2026-09-13): использует context.Background() — load нельзя отменить
// через ctx (нет родительского контекста). Для cancellable load используйте
// LoadModelWithOpts(ctx, ...) с явным context.
func (b *Backend) LoadModel(name string, path string) error {
	return b.LoadModelWithOpts(context.Background(), name, path, LoadModelOpts{
		GPULayers:     b.cfg.DefaultGPULayers,
		ContextSize:   b.cfg.DefaultCtxSize,
		BatchSize:     b.cfg.DefaultBatchSize,
		FlashAttnType: b.cfg.DefaultFlashAttnType,
		NUMA:          b.cfg.DefaultNUMA,
		UseMmap:       b.cfg.DefaultUseMmap,
		// Round 12: проброс DefaultNParallel в opts — даёт CLI/env/JSON
		// default работать через простой LoadModel (без явных opts).
		Parallel: b.cfg.DefaultNParallel,
	})
}

// LoadModelWithOpts загружает GGUF модель с указанными параметрами.
//
// R60.57 (2026-09-13): принимает context.Context для отмены загрузки.
// R60.57 follow-up (2026-09-13): использует bridge.LoadModelWithEarlyHandle
// для early-expose C-уровневого handle в inst.handle.ptr СРАЗУ после
// malloc+atomic_init (в bridge_load_model), ДО blocking llama_model_load_from_file.
// Это позволяет watcher goroutine вызвать bridge.RequestLoadAbort(handle)
// в реальном времени — handle уже валиден к моменту ctx.Done.
//
// Семантика ctx cancellation:
//   - ctx.Done() → spawn watcher goroutine вызывает bridge.RequestLoadAbort
//     на in-flight handle (читает inst.handle.ptr напрямую, без polling).
//   - Cancel возвращает ошибку с errors.Is(err, ErrAborted) == true если
//     C-bridge успел отменить load в checkpoint; иначе — обычная load error.
//   - Если ctx отменён ДО того, как C-bridge успел early-expose ptr
//     (race window в первые микросекунды load), watcher видит ptr==nil
//     и логирует warning — load продолжается до completion. Это
//     edge case (cancel до load started) — приемлемая degradation.
//
// ВНИМАНИЕ: для полноценной cancellable load требуется C-side fix
// (Approach b — out_handle parameter). R60.57 follow-up (commit a62853e)
// завершает эту работу: bridge.LoadModelWithEarlyHandle + раннее
// заполнение inst.handle.ptr в bridge_load_model.
// applyRPCServersR105 — прокинуть RPC-узлы из конфига в параметры моста (S1).
//
// Отдельная функция, а не строка в середине загрузки модели: её можно проверить
// юнит-тестом, тогда как сам путь загрузки требует реального GGUF и GPU. Копия
// среза — намеренно: параметры моста живут дольше вызова и не должны разделять
// память с конфигом (его перезаписывает reload конфигурации).
func applyRPCServersR105(cfg *bridge.ModelConfig, configured []string) []string {
	if cfg == nil || len(configured) == 0 {
		return nil
	}
	servers := make([]string, len(configured))
	copy(servers, configured)
	cfg.RPCServers = servers
	return servers
}

// LoadModelWithOpts — загрузка модели с опциями (кэш, GPU-слои, split).
func (b *Backend) LoadModelWithOpts(ctx context.Context, name string, path string, opts LoadModelOpts) error {
	// R83: single-flight — загрузка одной модели за раз (см. loadSingleFlight).
	// Без него второй запрос проходит checkVRAMForModel на устаревшем снимке
	// свободной VRAM и запускает вторую загрузку того же файла параллельно.
	// R227: захват слота загрузки с дедлайном. Раньше зависшая загрузка держала
	// loadSingleFlight, и КАЖДАЯ следующая загрузка висела молча до рестарта.
	if err := b.acquireLoadFlight(loadFlightWait()); err != nil {
		return err
	}
	defer b.loadSingleFlight.Unlock()

	// R227: сторож. По таймауту — стек горутин в лог и попытка отменить загрузку;
	// без этого клинч выглядел как «модель просто исчезла».
	loadDone := make(chan struct{})
	defer close(loadDone)
	loadStallWatchdog(name, loadStallThreshold(), loadDone, nil, func() {
		if b.AbortLoad(name) {
			logger.Get().Warnw(i18n.T("cppbackend.backend.load_abort_on_timeout"),
				"model", name)
		}
	})

	// R83 (2026-09-30): «окно на клиента» → суммарный n_ctx.
	//
	// ЕДИНСТВЕННАЯ точка пересчёта, ДО checkVRAMForModel: иначе memfit проверял
	// бы суммарное окно, а C-bridge получил бы другое (и наоборот). Семантика
	// llama.cpp в этой сборке: n_ctx_seq = n_ctx / n_seq_max (см. ContextPerSlot),
	// поэтому оператор, задав окно «на клиента», получает его на каждом слоте,
	// а суммарный n_ctx = окно × слоты.
	slotsRequested := resolveNParallel(opts.Parallel, b.cfg.DefaultNParallel)
	if opts.ContextPerSeq > 0 {
		total, per := EffectiveContextSize(opts.ContextSize, opts.ContextPerSeq, slotsRequested)
		logger.Get().Infow(i18n.T("cppbackend.backend.context_per_seq_recomputed"),
			"model", name,
			"context_per_seq", opts.ContextPerSeq,
			"slots", parallelSlotsFor(slotsRequested),
			"total_n_ctx", total,
			"context_per_seq_actual", per)
		opts.ContextSize = total
	}

	// R114 (2026-10-13): ГЕЙТ ГОТОВНОСТИ УЗЛОВ РАСКЛАДКИ.
	//
	// Стоит ДО checkVRAMForModel и до C-моста: недоступный узел должен быть
	// обнаружен, пока llama.cpp к нему ещё не подключился. Иначе загрузка
	// заканчивается `abort()` внутри моста (SIGABRT + перезапуск контейнера,
	// см. docs/architecture/layer-sharding.ru.md §4.6), а планировщик ещё и считает бюджет по
	// мёртвому узлу.
	// R229 (2026-10-22): НЕДОСТУПНЫЙ узел раскладки больше НЕ отменяет загрузку
	// целиком: исключаем его и грузим на живых узлах (или локально), если модель
	// без него влезает. Прежнее «загрузка отменена» оставляло оператора без
	// модели, хотя донор — необязательный ресурс. Планирование использует тот же
	// отфильтрованный список (PlanningRPCServers), поэтому фантомной памяти
	// мёртвого узла в плане уже нет.
	var loadRPCServers []string
	if b.cfg.RPCEnabled && len(b.cfg.RPCServers) > 0 {
		loadRPCServers = b.filterReachableRPC(b.activeRPCServers())
		if excluded := b.ExcludedRPCServers(); len(excluded) > 0 {
			if len(loadRPCServers) == 0 {
				logger.Get().Warnw(i18n.T("cppbackend.backend.all_layout_nodes_unreachable"),
					"model", name, "excluded", strings.Join(excluded, ","))
			} else {
				logger.Get().Warnw(i18n.T("cppbackend.backend.layout_nodes_excluded"),
					"model", name, "use", strings.Join(loadRPCServers, ","),
					"excluded", strings.Join(excluded, ","))
			}
		}
	}

	// Валидация VRAM перед загрузкой и авто-подбор оптимальных gpuLayers.
	// Выполняется ДО pre-allocation inst, чтобы не плодить phantom entries
	// в b.models при ошибке валидации.
	var err error
	if opts, err = b.checkVRAMForModel(name, path, opts); err != nil {
		return err
	}

	b.mu.Lock()
	if _, exists := b.models[name]; exists {
		b.mu.Unlock()
		return fmt.Errorf("model %s already loaded", name)
	}

	// Round 32 #8 (2026-08-10): dedup по path, не только по name.
	// Раньше проверяли только b.models[name] — если тот же .gguf файл уже
	// загружен под другим именем (например, "gemma-4" vs "gemma-4.gguf"),
	// LoadModelWithOpts проходил проверку и грузил модель второй раз → 2x
	// VRAM занято → балансер начинал блокировать по VRAM headroom.
	// Типичный сценарий: пользователь в OpenWebUI переключается между моделями
	// с похожими именами, или webui/balancer отправляет name с .gguf vs без.
	//
	// Теперь: если path уже загружен — возвращаем *AlreadyLoadedAsError с
	// existing_name. Handler через errors.As извлечёт existing name и вернёт
	// клиенту status=loaded (модель уже в памяти, не нужно дублировать).
	if path != "" {
		for existingName, existingInst := range b.models {
			if existingInst.info.Path == path {
				existingNameCopy := existingName
				b.mu.Unlock()
				logger.Get().Infow("LoadModelWithOpts: same path already loaded, dedup by file",
					"requested_name", name, "existing_name", existingName, "path", path)
				return &AlreadyLoadedAsError{ExistingName: existingNameCopy, Path: path}
			}
		}
	}

	gpuLayers := opts.GPULayers
	ctxSize := opts.ContextSize
	batchSize := opts.BatchSize

	// Оцениваем размер файла модели (если доступен) — для отображения прогресса в UI.
	var loadingSizeBytes int64
	if fi, statErr := os.Stat(path); statErr == nil {
		loadingSizeBytes = fi.Size()
	}

	// R60.57 follow-up: pre-allocate inst.handle ДО вызова C-bridge.
	// C-bridge (LoadModelWithEarlyHandle) использует out_handle parameter
	// для записи early-allocated C handle в inst.handle.ptr СРАЗУ после
	// malloc + atomic_init, ДО blocking llama_model_load_from_file.
	// Это позволяет watcher goroutine (ниже) вызвать bridge.RequestLoadAbort
	// на этом handle в реальном времени.
	//
	// handle.path будет установлен bridge.LoadModelWithEarlyHandle внутри
	// (earlyHandle.path = cfg.ModelPath) ДО C-вызова. handle.ptr остаётся
	// nil до момента early-expose внутри bridge_load_model.
	//
	// NOTE: ModelHandle struct fields (ptr, path) не экспортированы — мы
	// не можем pre-set path напрямую из этого пакета. bridge.go сам
	// устанавливает path первым делом в LoadModelWithEarlyHandle.
	inst := &modelInstance{
		info: ModelInfo{
			Name:             name,
			Path:             path,
			State:            StateLoading,
			GPULayers:        gpuLayers,
			BatchSize:        opts.BatchSize,
			FlashAttnType:    opts.FlashAttnType,
			NUMA:             opts.NUMA,
			UseMmap:          opts.UseMmap,
			LoadedAt:         time.Now(),
			LoadingStartedAt: time.Now(),
			LoadingSizeBytes: loadingSizeBytes,
		},
		handle: &bridge.ModelHandle{},
	}
	b.models[name] = inst
	b.mu.Unlock()

	// R493e (2026-11-17): ПОСЛЕ этой строки инстанс виден другим горутинам —
	// ListModels/GetModel/GetMetrics копируют inst.info под b.mu.RLock. Всё, что
	// загрузчик пишет в inst.info ДО `b.mu.Lock()` ниже (tensor split, состояние
	// mmproj, деградация раскладки), обязано идти в локальные переменные и
	// попадать в inst.info уже под этим замком.
	//
	// ЖИВОЙ ДЕФЕКТ, ради которого: красный CI (`cmd/...`, -race)
	//   WARNING: DATA RACE
	//   Read at ... by goroutine 1999:  cppbackend.(*Backend).ListModels()   backend.go:2611
	//   Previous write ...:             cppbackend.(*Backend).LoadModelWithOpts() backend.go:1556
	// то есть /api/models отвечал во время загрузки, пока лоадер писал
	// LayoutDegraded. Чтение под b.mu.RLock без записи под b.mu.Lock — гонка по
	// контракту, который уже соблюдался в остальных 170 строках этого метода.
	var (
		infoTensorSplit    []float32
		infoMmprojPath     string
		infoVision         bool
		infoVisionError    string
		infoLayoutDegraded bool
		infoLayoutReason   string
	)

	// R60.57 follow-up: watcher goroutine для ctx.Done() → bridge.RequestLoadAbort.
	// Запускается ПОСЛЕ pre-allocation inst.handle (нужен для direct access),
	// но ДО bridge.LoadModelWithEarlyHandle (чтобы watcher мог прервать load).
	//
	// В отличие от pre-R60.57-follow-up (Phase 3-5, commit 4682f2f), watcher
	// НЕ polling'ает b.GetHandle(name) — он читает inst.handle.ptr напрямую.
	// C-bridge экспонирует early-allocated handle в inst.handle.ptr почти
	// сразу после malloc+atomic_init (микросекунды), поэтому race window
	// минимальна. Если ctx отменён ДО early-expose — watcher видит ptr==nil
	// и логирует warning (load продолжается до completion).
	watcherDone := make(chan struct{})
	defer close(watcherDone)

	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				// ctx отменён — пробуем отменить C-side load через bridge.RequestLoadAbort.
				//
				// R60.57 follow-up: handle уже pre-allocated (inst.handle.path != ""),
				// ptr будет заполнен C-bridge'ом в первые микросекунды load.
				// bridge.RequestLoadAbort nil-safe — вернёт informative error
				// если ptr ещё nil (cancel пришёл слишком рано).
				if abortErr := bridge.RequestLoadAbort(inst.handle); abortErr != nil {
					// ptr==nil → cancel пришёл ДО early-expose (теоретически
					// возможно только при pre-cancelled ctx — race window
					// в норме <1ms). Логируем как Debug (не Warn), т.к.
					// это benign edge case.
					// Также: RequestLoadAbort уже aborted / no-op cases
					// (idempotent, atomic flag уже = 1).
					logger.Get().Debugw("LoadModelWithOpts: RequestLoadAbort no-op or ptr not yet exposed",
						"model", name, "path", path, "error", abortErr)
				} else {
					// R60.59 (2026-09-14): log successful abort — без этого оператор
					// не видит в логах что client cancel реально прервал load, и
					// при балансер-side retry cascade нельзя отличить abort от
					// generic load failure.
					logger.Get().Infow("LoadModelWithOpts: load aborted via watcher",
						"model", name, "path", path,
						"ctx_err", ctx.Err().Error())
				}
			case <-watcherDone:
				// load завершился нормально (success или error) — watcher выходит.
				return
			}
		}()
	}

	// R215 (2026-10-21): прогресс чтения весов для UI.
	//
	// ЗАЧЕМ. До R215 во время загрузки было видно только «loading» и ожидаемое
	// время: оператор не мог понять, осталось 20 секунд или две минуты, а
	// отмена (unload force) отрабатывала лишь ПОСЛЕ возврата загрузки. Теперь
	// llama.cpp сообщает процент через progress-callback (c/bridge: см.
	// bridge_load_progress_cb), и здесь он попадает в ModelInfo.
	//
	// Читаем ПО ПУТИ (bridge.LoadProgressByPath), а не по handle: на пути ошибки
	// C-мост освобождает InternalModel, и разыменование handle из этой горутины
	// было бы use-after-free.
	defer bridge.ClearLoadProgress(path) // объявлен раньше close: снимает запись ПОСЛЕ остановки опроса
	progressDone := make(chan struct{})
	defer close(progressDone)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-ticker.C:
				pct := bridge.LoadProgressByPath(path)
				if pct < 0 {
					continue
				}
				b.mu.Lock()
				// R215: процент держим МОНОТОННЫМ. llama.cpp зовёт callback
				// отдельно на каждую группу буферов (CPU_Mapped, CUDA0, ...), и
				// счётчик внутри группы начинается с нуля: без этого UI видел бы
				// «60% → 0% → 30%» на одной загрузке.
				if pct > inst.info.LoadingProgressPct {
					inst.info.LoadingProgressPct = pct
				}
				b.mu.Unlock()
			}
		}
	}()

	// Логгируем старт загрузки (UI может полагаться на наличие этой записи).
	logger.Get().Infow("model load started",
		"name", name,
		"path", path,
		"sizeBytes", loadingSizeBytes,
		"gpuLayers", gpuLayers,
		"ctxSize", ctxSize,
		"batchSize", batchSize)

	// Загружаем модель через bridge
	cfg := bridge.DefaultModelConfig(path)
	cfg.NContext = ctxSize
	cfg.NBatch = batchSize
	cfg.NGPULayers = gpuLayers
	cfg.FlashAttnType = opts.FlashAttnType
	cfg.NUMA = opts.NUMA
	cfg.UseMmap = opts.UseMmap
	cfg.UseMlock = b.cfg.DefaultUseMlock
	// R83 §3.2/§3.4: числа KV-кэша для C-оценки свободной VRAM (см.
	// checkVRAMForModel). 0 = неизвестно → C берёт консервативный верх.
	cfg.KVLayers = opts.KVLayers
	cfg.KVHeadDim = opts.KVHeadDim
	// NThreads override см. ниже — после секции KVCacheType/Parallel.
	// (Раньше cfg.NThreads = b.cfg.DefaultNThreads затирал opts.NThreads;
	//  теперь мы применяем opts > default корректно.)

	// RoPE параметры контекста
	cfg.RopeFreqBase = float32(b.cfg.DefaultRopeFreqBase)
	cfg.RopeFreqScale = float32(b.cfg.DefaultRopeFreqScale)
	cfg.RopeScalingType = b.cfg.DefaultRopeScalingType
	cfg.RopeScalingFactor = float32(b.cfg.DefaultRopeScalingFactor)

	// YaRN параметры
	cfg.YarnExtFactor = float32(b.cfg.DefaultYarnExtFactor)
	cfg.YarnAttnFactor = float32(b.cfg.DefaultYarnAttnFactor)
	cfg.YarnBetaFast = float32(b.cfg.DefaultYarnBetaFast)
	cfg.YarnBetaSlow = float32(b.cfg.DefaultYarnBetaSlow)

	// KV Cache
	cfg.NoKVOffload = b.cfg.DefaultNoKVOffload
	// Session 16 (2026-06-27): KVCacheType и Parallel — пробрасываем opts поверх дефолтов.
	// До этой правки cfg.KVCacheType жёстко затирал opts.KVCacheType значением
	// из b.cfg (DefaultKVCacheType), что делало per-model profile с q8_0/q4_0
	// бесполезным — C-bridge всегда получал default F16.
	if opts.KVCacheType != "" {
		cfg.KVCacheType = opts.KVCacheType
	} else {
		cfg.KVCacheType = b.cfg.DefaultKVCacheType
	}
	// То же для NParallel: 0 в Go = default (=1 в llama.cpp).
	// > 0 = multi-slot batched generation (требует больше VRAM).
	// Round 12 (2026-07-28): если opts.Parallel не задан (0), используем
	// b.cfg.DefaultNParallel — это даёт CLI/env/JSON-конфигурируемый default,
	// который раньше был только per-model в opts.
	//
	// R83 (2026-09-29): значение вычисляется ОДНИМ правилом (resolveNParallel),
	// потому что то же число ниже инициализирует SlotManager. Раньше здесь
	// учитывался DefaultNParallel, а в SlotManager — нет (там было просто
	// max(1, opts.Parallel)); при CPPWORKER_N_PARALLEL=2 llama.cpp получал
	// n_parallel=2 (то есть KV-cache под 2 слота), cppworker объявлял балансеру
	// вместимость 2 (effectiveNParallel), а SlotManager пропускал по одному
	// запросу — второй клиент молча ждал в Acquire и получал 503 по таймауту.
	cfg.NParallel = resolveNParallel(opts.Parallel, b.cfg.DefaultNParallel)
	// S1 (2026-10-13): режим раскладки слоёв между устройствами ОБЯЗАН доехать
	// до моста. До этой правки opts.SplitMode (его считает
	// cmd/cppworker/handlers_model.go из CPPWORKER_SPLIT_MODE/профиля/запроса)
	// нигде не присваивался в bridge.ModelConfig, поэтому мост всегда видел 0 =
	// LLAMA_SPLIT_MODE_NONE. С NONE llama.cpp (src/llama.cpp:284) оставляет из
	// списка устройств ТОЛЬКО main_gpu — и удалённый RPC-узел отбрасывался, даже
	// когда мост честно передал массив из двух устройств. Живая проба: при
	// CPPWORKER_SPLIT_MODE=1 в логе было «using device CUDA0 ... 7097 MiB free»
	// (одна строка) при «устройств в раскладке 2».
	//
	// Соглашение моста (c/bridge/bridge.h): -1 = дефолт llama.cpp (LAYER),
	// 0 = NONE (одно устройство), 1 = LAYER, 2 = ROW, 3 = TENSOR.
	cfg.SplitMode = opts.SplitMode
	// NThreads: было cfg.NThreads = b.cfg.DefaultNThreads (ВСЕГДА затирал opts).
	// Сейчас opts.NThreads > 0 — выигрывает opts, иначе — дефолт.
	if opts.NThreads > 0 {
		cfg.NThreads = opts.NThreads
	} else {
		cfg.NThreads = b.cfg.DefaultNThreads
	}

	// Round 7: override-tensors (parallel arrays, length must match).
	if len(opts.OverrideTensors) > 0 &&
		len(opts.OverrideTensors) == len(opts.OverrideTensorBufts) {
		cfg.OverrideTensors = opts.OverrideTensors
		cfg.OverrideTensorBufts = opts.OverrideTensorBufts
		logger.Get().Infow("override-tensors applied",
			"model", name,
			"count", len(opts.OverrideTensors))
	}

	// Normalization
	cfg.RMSNormEps = float32(b.cfg.DefaultRMSNormEps)

	// Прочее
	cfg.MainGPU = b.cfg.DefaultMainGPU
	cfg.NoMemoryMap = b.cfg.DefaultNoMemoryMap
	cfg.RPCBackend = b.cfg.DefaultRPCBackend
	// R105 (S1 плана 2026-10-07): узлы раскладки — в параметры моста.
	//
	// R108 (2026-10-12): предупреждение «транспорт не подключён» убрано — мост
	// теперь читает rpc_servers и сам сообщает, какие узлы подключились и сколько
	// устройств попало в раскладку. Прежний текст врал бы ровно рядом с этими
	// строками (живой прогон: «узел подключён, устройств 2» сразу после
	// «транспорт не подключён»).
	// R115: узлы берём только у activeRPCServers — при выключенном флаге
	// раскладки нет, и в мост не уходит ни одного адреса (иначе «выключено»
	// продолжало бы подключаться к узлам при загрузке модели).

	// R177 (2026-10-19): ПЕРЕД ЗАГРУЗКОЙ - ДОЖДАТЬСЯ, ПОКА УЗЕЛ СВОБОДЕН.
	//
	// Живая причина R174/R176: при reload узел ещё доигрывает прошлую сессию
	// (он обслуживает одно соединение), загрузка не регистрирует RPC-устройство,
	// и все слои уезжают на локальную карту. Раньше это выяснялось по скорости.
	// Теперь короткий гейт: пробуем РЕАЛЬНУЮ команду протокола (запрос памяти
	// устройства) несколько раз, и только потом грузим.
	// R380 (2026-11-01): результат гейта ОБЯЗАН влиять на список серверов.
	// Раньше при любом исходе адреса уходили в мост, и занятый узел ронял
	// процесс SIGSEGV'ом в cgo (negotiate_hello) вместо локальной загрузки.
	bridgeRPCServers := loadRPCServers
	if servers := b.activeRPCServers(); len(servers) > 0 {
		// R177: «узел ответил на реальную команду» фиксируем ВСЕГДА. Без этого
		// успешный гейт молчал, и по логу нельзя было отличить «узел был готов, а
		// загрузка всё равно не зарегистрировала RPC0» от «узел не был готов».
		//
		// R215: бюджет ожидания больше не 1.2 с. Узел обслуживает одно соединение,
		// и после сбоя мастера освобождается до ~1.5-2 минут (keepalive). За это
		// время лучше подождать, чем сложить все слои на локальную карту.
		budget := rpcReadyWaitBudget()
		attempts := rpcReadyWaitAttempts(budget, rpcReadyProbeDelay)
		ready, attempt := b.waitRPCNodeReadyForLoad(servers, attempts, rpcReadyProbeDelay)
		b.noteRPCReadyGate(ready)
		bridgeRPCServers = bridgeServersForLoadR380(loadRPCServers, ready)
		if ready {
			logger.Get().Infow(i18n.T("cppbackend.backend.layout_node_ready"),
				"model", name, "servers", servers, "attempt", attempt,
				"waited_ms", int64(attempt-1)*rpcReadyProbeDelay.Milliseconds())
		} else {
			// R214 (2026-10-21): узел слушает, но команду не принял — самый частый
			// случай на живом стенде: на узле висит сессия прошлого клиента (его
			// деструктор ещё не сработал), и VRAM держит веса этой сессии, а
			// новые подключения не обслуживаются. Говорим это прямо и называем
			// действие: перезапустить узел, иначе раскладка снова уедет на
			// локальную карту. Подробности — docs/operations/rpc-donor-vram-lifecycle.ru.md.
			logger.Get().Warnw(i18n.T("cppbackend.backend.layout_node_no_devices"),
				"model", name, "servers", servers, "attempts", attempt,
				"waited_ms", int64(attempt-1)*rpcReadyProbeDelay.Milliseconds(),
				"timeout_streak", b.RPCReadyTimeoutStreak(),
				"stuck_suspected", b.RPCLayoutStuckSuspected())
			logger.Get().Warnw("R380: узел не принял загрузку — в мост не передаём ни одного адреса, "+
				"грузим на локальные устройства (иначе llama.cpp падает в negotiate_hello)",
				"model", name, "skipped_servers", servers)
			if b.RPCLayoutStuckSuspected() {
				logger.Get().Errorw(i18n.T("cppbackend.backend.layout_node_stuck"),
					"model", name, "servers", servers, "timeout_streak", b.RPCReadyTimeoutStreak())
			}
		}
	}

	if servers := applyRPCServersR105(&cfg, bridgeRPCServers); len(servers) > 0 {
		logger.Get().Infow(i18n.T("cppbackend.backend.rpc_layout_handed_to_bridge"),
			"model", name, "servers", servers)
	}

	// R176 (2026-10-19): СПЛИТ ПРИМЕНЯЕТСЯ ТОЛЬКО К ТОМУ ЧИСЛУ УСТРОЙСТВ, ДЛЯ КОТОРОГО ПОСЧИТАН.
	//
	// Живой случай (reload из WebUI): сплит копировался из прошлого инстанса
	// (manual tensor split applied tensorSplit=[0.538, 0.462]), хотя RPC-узел на
	// этой загрузке не зарегистрировался, и устройство было одно. Лишние пропорции
	// llama.cpp молча игнорирует - ошибка не всплывает, слои уезжают на одну карту.
	if len(opts.TensorSplit) > 0 {
		// R178: ОДНА строка, объясняющая выбор устройств. Мост получает серверы
		// отдельно от сплита, поэтому расхождение «серверы передали, а сплит на одно
		// устройство» до сих пор было невидимым.
		if snap := b.rpcDeviceSnapshot(); true {
			var rem []string
			for _, d := range remoteDevices(snap) {
				rem = append(rem, d.Name)
			}
			logger.Get().Infow(i18n.T("cppbackend.backend.layout_devices_seen"),
				"model", name, "local_gpus", b.gpuCount, "remote_devices", rem,
				"active_servers", b.activeRPCServers(), "admitted_servers", b.admittedRPCServers(),
				"snapshot_devices", len(snap))
		}
		devices := b.gpuCount + len(remoteDevices(b.rpcDeviceSnapshot()))
		if !splitFitsDevices(opts.TensorSplit, devices) {
			logger.Get().Warnw(i18n.T("cppbackend.backend.tensor_split_not_applied"),
				"model", name, "split_len", len(opts.TensorSplit), "devices", devices,
				"split", opts.TensorSplit)
			opts.TensorSplit = nil
		}
	}

	// Пользовательский tensor split (если не авто)
	if len(opts.TensorSplit) > 0 {
		cfg.TensorSplit = opts.TensorSplit
		if b.gpuCount > 0 && len(opts.TensorSplit) > 0 {
			// R493e: в inst.info — только под b.mu (см. комментарий у публикации
			// инстанса выше).
			infoTensorSplit = append([]float32(nil), opts.TensorSplit...)
			cfg.MainGPU = 0
			logger.Get().Infow("manual tensor split applied",
				"model", name,
				"tensorSplit", opts.TensorSplit)
		}
	} else if b.cfg.AutoGPUDistribution && b.gpuManager != nil && b.gpuCount > 0 {
		// Multi-GPU: рассчитываем tensor split если включено авто-распределение
		//
		// S1 (2026-10-13): если подключены RPC-узлы, пропорции обязаны включать и
		// их — иначе llama.cpp раскладывает слои только по локальным картам, а
		// удалённое устройство из списка остаётся без весов (живой симптом:
		// «устройств в раскладке 2», а свободная память узла не используется).
		ts := b.gpuManager.CalculateTensorSplit(0) // 0 = unknown estimate
		if remote := remoteDevices(b.rpcDeviceSnapshot()); len(remote) > 0 {
			ts = b.gpuManager.CalculateTensorSplitWithRPC(remote)
		}
		if ts.GPUCount > 1 {
			cfg.TensorSplit = ts.Ratios
			cfg.MainGPU = ts.MainGPU
			// R493e: в inst.info — только под b.mu (см. комментарий у публикации).
			infoTensorSplit = append([]float32(nil), ts.Ratios...)
			logger.Get().Infow("multi-GPU tensor split applied",
				"model", name,
				"gpuCount", ts.GPUCount,
				"mainGPU", ts.MainGPU,
				"tensorSplit", ts.Ratios)
		}
	}

	// R60.57 follow-up: используем LoadModelWithEarlyHandle вместо LoadModel.
	// C-bridge (commit a62853e) теперь принимает out_handle parameter и
	// экспонирует early-allocated C handle в inst.handle.ptr СРАЗУ после
	// malloc+atomic_init. Это позволяет watcher goroutine (выше) вызвать
	// bridge.RequestLoadAbort на этом handle в реальном времени.
	//
	// inst.handle уже pre-allocated (path установлен). LoadModelWithEarlyHandle
	// заполнит ptr во время выполнения.
	// R118.16 (2026-10-14): сбрасываем план ПЕРЕД загрузкой — llama.cpp
	// наполнит его строками `load_tensors: layer N assigned to device X`, и без
	// сброса в план попали бы слои предыдущей модели.
	bridge.ResetDeviceLayers()
	bridge.ResetRecentLogLines()
	handle, err := bridge.LoadModelWithEarlyHandle(cfg, inst.handle)
	// Round 25 (2026-08-04): замеряем реальное время load'а для истории.
	// bridge.LoadModelWithEarlyHandle — blocking CGo, возвращает после
	// успешной загрузки. Эта метрика используется cppworker'ом для
	// динамической оценки estimatedLoadTimeMs (вместо хардкода 100 MB/s).
	loadStartTime := time.Now()
	if err != nil {
		// Сохраняем LoadingError на короткое время, чтобы UI мог показать
		// причину сбоя (даже после того, как inst удалён из b.models).
		// На практике inst удаляется сразу, поэтому ошибка возвращается
		// в ответе на /api/models/load и не попадает в /api/models,
		// но если клиент polling'ом проверял состояние — успеет увидеть state=loading
		// в течение ~миллисекунд до получения HTTP 500.
		//
		// R83 §9.6 D-B (2026-09-27): удаляем ТОЛЬКО СВОЙ инстанс. Раньше здесь
		// было безусловное `delete(b.models, name)`, и при перекрытии
		// load/unload это выбивало из реестра ЧУЖУЮ (уже новую) запись: другая
		// горутина успевала пересоздать inst и зарегистрировать его, а наш
		// провалившийся путь его стирал. Снаружи это выглядело как «model loaded
		// в логе, а /api/models → count=0» — тот самый симптом D-B, при котором
		// балансер не находил загруженную модель.
		b.removeOwnInstance(name, inst)
		if b.metrics != nil {
			b.metrics.RecordRequest(name, 0, 0, false)
		}
		// R697 (2026-11-24): мост на провал загрузки отвечает только «failed to load
		// model from <путь>» — настоящая причина (unknown model architecture и т.п.)
		// остаётся в перехваченном логе llama.cpp. Приклеиваем вердикт ЗДЕСЬ, один
		// раз, чтобы его увидели все потребители: ответ /api/models/load, реестр
		// провалов (load_failures) и уведомление оператору. См. load_error_tail.go.
		return LoadErrorWithTail(fmt.Errorf("load model %s: %w", name, err))
	}

	// R248/B3 (2026-10-27): mmproj passthrough — проектор грузится ПОСЛЕ
	// текстовой модели (mtmd привязывается к llama_model). Ошибка проектора
	// НЕ отменяет загрузку: текстовая модель работоспособна, vision честно
	// остаётся выключенным (415 на картинки), а причина видна в
	// /api/models.visionError и в логе — вместо тихой «модель без глаз».
	if opts.MmprojPath != "" {
		// R493e: результат проектора — в локальных переменных, публикация под b.mu.
		infoMmprojPath = opts.MmprojPath
		if verr := handle.LoadMMProj(opts.MmprojPath); verr != nil {
			infoVision = false
			infoVisionError = verr.Error()
			logger.Get().Warnw("mmproj load failed: images stay unsupported (415)",
				"name", name, "mmproj", opts.MmprojPath, "error", verr.Error())
		} else {
			// ВАЖНО: проектор загружен ≠ картинки уже обслуживаются. Пока
			// приём image_url/images и вставка медиа-маркеров в промпт не
			// подключены (R248/B3 часть 2), публиковать capabilities.vision
			// нельзя: балансер начал бы пропускать картинки, а cppworker
			// ответил бы текстом — молчаливый неверный ответ вместо 415.
			// Флаг CPPWORKER_VISION_MMPROJ=1 включает capability заранее
			// (стенд/приёмка), по умолчанию vision остаётся false.
			infoVision = handle.MMProjLoaded() && visionInferenceEnabled()
			infoVisionError = ""
			logger.Get().Infow("mmproj loaded",
				"name", name, "mmproj", opts.MmprojPath,
				"vision", infoVision, "marker", handle.MMProjMarker())
		}
	}

	// R83 §3.2 (2026-09-26): C-bridge клампит gpu_layers, если запрошенный
	// план заведомо не помещается в VRAM (см. c/bridge/bridge.c). Проверяем,
	// что фактический offload совпал с планом: молчаливое понижение до
	// CPU-only нельзя оставлять незамеченным в логах.
	if applied := bridge.GetLastGPULayers(); applied >= 0 && applied != gpuLayers {
		logger.Get().Warnw("C-bridge clamped gpu_layers (R83 §3.2): VRAM check rejected the planned offload",
			"name", name,
			"requestedGpuLayers", gpuLayers,
			"appliedGpuLayers", applied)
	}

	// R176 (2026-10-19): СВЕРКА «ПЕРЕДАЛИ / ЗАГРУЗИЛОСЬ».
	//
	// Причина, найденная R174: серверы передавались (`RPC-раскладка передана в мост`),
	// бюджет их учитывал, но llama.cpp на загрузке печатал только `using device CUDA0` -
	// узел не становился устройством модели, и 42 слоя уезжали на локальную карту.
	// Проверяем ПО ФАКТУ и говорим вслух, а не оставляем оператору догадки по скорости.
	{
		servers := b.activeRPCServers()
		layers := bridge.DeviceLayers()
		// R493e: результат — в локальные переменные, публикация под b.mu ниже.
		if degraded, reason := layoutRegistration(servers, layers); degraded {
			infoLayoutDegraded = true
			infoLayoutReason = reason
			logger.Get().Warnw(i18n.T("cppbackend.backend.layout_not_applied"),
				"name", name, "servers", servers, "layers", len(layers), "reason", reason)
		} else {
			infoLayoutDegraded = false
			infoLayoutReason = ""
		}
	}

	// Получаем метаданные
	meta, err := handle.GetMetadata()
	if err != nil {
		logger.Get().Warnw("failed to get metadata for model", "name", name, "error", err)
		meta = &bridge.ModelMetadata{
			Architecture:  "unknown",
			ContextLength: ctxSize,
		}
	}

	// Обновляем информацию
	//
	// R60.57 follow-up: НЕ переприсваиваем inst.handle — handle уже
	// pre-allocated (см. выше, перед watcher'ом) и содержит финальный
	// ptr после LoadModelWithEarlyHandle. Переприсвоение здесь создаёт
	// data race с watcher goroutine (которая читает inst.handle при
	// ctx.Done → bridge.RequestLoadAbort). Используем `handle` только
	// локально — он идентичен *inst.handle (тот же C-level ptr).
	b.mu.Lock()
	_ = handle // R60.57 follow-up: handle уже в inst.handle (pre-allocated)
	inst.metadata = meta
	inst.info.State = StateLoaded
	// R493e: публикация того, что загрузчик посчитал ДО этого замка
	// (см. комментарий у публикации инстанса выше). Здесь и только здесь эти
	// поля попадают в inst.info, который читают снимки.
	if infoTensorSplit != nil {
		inst.info.TensorSplit = infoTensorSplit
	}
	if infoMmprojPath != "" {
		inst.info.MmprojPath = infoMmprojPath
		inst.info.Vision = infoVision
		inst.info.VisionError = infoVisionError
	}
	inst.info.LayoutDegraded = infoLayoutDegraded
	inst.info.LayoutDegradedReason = infoLayoutReason
	inst.info.Architecture = meta.Architecture
	inst.info.NLayers = meta.NLayers
	// R248: фактический offload вместо запрошенного. C-bridge клампит
	// gpu_layers под свободную VRAM (R83 §3.2), и до этой правки наружу уходило
	// ЗАПРОШЕННОЕ значение (-1 = «все слои»), хотя в логе могло быть 20/66.
	// Деградация больше не молчит: offloadDegraded + offloadNote видны в
	// /api/models, а не только в логе воркера.
	if applied := bridge.GetLastGPULayers(); applied >= 0 {
		inst.info.GPULayers = applied
		if meta.NLayers > 0 && applied < meta.NLayers {
			inst.info.OffloadDegraded = true
			inst.info.OffloadNote = fmt.Sprintf("%d/%d слоёв на GPU, остальное в RAM/CPU", applied, meta.NLayers)
			logger.Get().Warnw("partial GPU offload: model does not fit VRAM",
				"name", name, "applied_gpu_layers", applied, "n_layers", meta.NLayers,
				"note", inst.info.OffloadNote)
		}
	}
	inst.info.NHeads = meta.NHeads
	inst.info.NKvHeads = meta.NKvHeads // BUG 13
	inst.info.HeadDimK = meta.HeadDimK // Phase 3 (2026-07-06): expose to /api/show
	inst.info.HeadDimV = meta.HeadDimV // Phase 3
	inst.info.NEmbd = meta.NEmbd
	inst.info.NVocab = meta.NVocab
	// R83-fix: реальное окно пишем ниже (после чтения у llama.cpp) — здесь
	// оставляем запрошенное как стартовое значение.
	inst.info.ContextSize = ctxSize
	inst.info.GGUFContextLength = meta.ContextLength
	inst.info.SizeBytes = meta.SizeTotalBytes
	inst.info.GPUCount = b.gpuCount
	// R118.16 (2026-10-14): точная раскладка из лога этой загрузки.
	//
	// Ограничение (осознанное): план хранится в мосте глобально, поэтому при
	// ДВУХ одновременных загрузках он относится к последней завершившейся.
	// От перепутывания защищает проверка суммы в потребителе (балансер
	// принимает план только если сумма слоёв = n_layers + 1, см.
	// layerPlanFromExactR118) — при несовпадении он показывает расчёт.
	inst.info.DeviceLayers = bridge.DeviceLayers()

	// Round 25: записываем реальное время load'а в историю для
	// последующей динамической оценки estimatedLoadTimeMs.
	// sizeBytes = meta.SizeTotalBytes (от llama.cpp, точнее чем os.Stat).
	b.RecordLoadTime(LoadTimeRecord{
		SizeBytes:   int64(meta.SizeTotalBytes),
		ContextSize: ctxSize,
		DurationMs:  time.Since(loadStartTime).Milliseconds(),
		Timestamp:   time.Now(),
	})

	// Session 16 (2026-06-27): Parallel + KVCacheType.
	// Сохраняем реально применённые значения opts в ModelInfo, чтобы:
	//   1. UI мог показать "parallel=4, kv=q8_0" в карточке модели;
	//   2. reload на тот же path через /api/models/reload видел, что
	//      текущая загрузка эквивалентна (sameLoadOptions);
	//   3. metrics broker экспортировал parallel/kvCacheType в /metrics.
	// opts — value type (не pointer), так что он всегда non-nil после
	// LoadModelWithOpts (даже если caller передал zero-value).
	//
	// R83-fix (2026-09-29): пишем ФАКТИЧЕСКОЕ число слотов, а не opts.Parallel.
	//
	// Было: info.Parallel = opts.Parallel, то есть 0, когда параллельность взята
	// из CPPWORKER_N_PARALLEL. При этом SlotManager инициализировался ниже
	// РЕАЛЬНЫМ числом (maxSlots=2), а в /api/models уходило `max_slots: 1`
	// (handlers_model.go берёт его из ModelInfo.Parallel). Из-за этого:
	//   - балансер НЕ мог узнать реальную вместимость воркера («capacity from
	//     model slots» не срабатывал: наблюдалось max_slots=1 при двух слотах);
	//   - WebUI и автотюн видели single-slot, хотя модель обслуживала два
	//     клиента одновременно.
	requestedParallel := resolveNParallel(opts.Parallel, b.cfg.DefaultNParallel)
	inst.info.Parallel = requestedParallel
	inst.info.KVCacheType = opts.KVCacheType
	// Окно на клиента — из фактического суммарного n_ctx и числа слотов.
	//
	// R83-fix (2026-09-30): числа берём у llama.cpp (llama_n_ctx / llama_n_ctx_seq),
	// а не из запроса. llama.cpp выравнивает их (n_ctx_seq = PAD(n_ctx/slots, 256),
	// затем n_ctx = n_ctx_seq × slots), и запрошенное отличалось от фактического:
	// на стенде 31974 против 32256. Из-за этого /api/models отдавал одно, модель
	// имела другое, и балансер, округляя вверх, просил больше реального окна.
	actualCtx, actualCtxSeq := ctxSize, 0
	if handle != nil {
		if c, s, err := bridge.GetContextSizes(handle); err == nil && c > 0 {
			actualCtx, actualCtxSeq = c, s
		} else if err != nil {
			logger.Get().Debugw(i18n.T("cppbackend.backend.ctx_sizes_unreadable"),
				"name", name, "requested_n_ctx", ctxSize, "error", err)
		}
	}
	inst.info.ContextSize = actualCtx
	if actualCtxSeq > 0 {
		inst.info.ContextPerSeq = actualCtxSeq
	} else {
		inst.info.ContextPerSeq = ContextPerSlot(actualCtx, requestedParallel)
	}
	if actualCtx != ctxSize {
		logger.Get().Infow(i18n.T("cppbackend.backend.ctx_window_differs"),
			"name", name, "requested_n_ctx", ctxSize,
			"actual_n_ctx", actualCtx, "context_per_seq", inst.info.ContextPerSeq)
	}

	// Round 13 (2026-07-28): initialize SlotManager после успешной загрузки.
	// maxSlots = resolveNParallel(opts, config) — то же правило, что ушло в
	// C-bridge выше, поэтому SlotManager и llama.cpp всегда согласованы
	// (Parallel=0/1 → single-slot, Round 8 behavior; explicit N или
	// CPPWORKER_N_PARALLEL → N слотов).
	//
	// R83 (2026-09-29): до этого здесь стояло max(1, opts.Parallel) —
	// CPPWORKER_N_PARALLEL в SlotManager не учитывался (см. комментарий у
	// cfg.NParallel выше).
	parallelSlots := parallelSlotsFor(requestedParallel)
	inst.slots = NewSlotManager(parallelSlots)
	logger.Get().Infow("slot manager initialized",
		"name", name,
		"maxSlots", parallelSlots,
		"parallel", opts.Parallel,
		"defaultNParallel", b.cfg.DefaultNParallel)

	// Round 15.1 (2026-07-30): initialize BatchedScheduler если включён
	// (per-model override или global toggle). Требует Parallel >= 2 потому
	// что batched path использует seq_id (1..N), а seq_id=0 зарезервирован
	// для legacy single-slot. NParallel в BatchedSchedulerConfig = Parallel
	// (max concurrent sessions).
	//
	// Effective flag = opts.EnableBatchedParallel (если != nil) иначе
	// b.cfg.EnableBatchedParallel (env / JSON config). Per-model wins.
	enableBatched := b.cfg.EnableBatchedParallel
	if opts.EnableBatchedParallel != nil {
		enableBatched = *opts.EnableBatchedParallel
	}
	if enableBatched && parallelSlots >= 2 {
		bs, err := NewBatchedScheduler(BatchedSchedulerConfig{
			Model:     handle,
			NParallel: int32(parallelSlots),
			WindowMs:  5,        // 5ms batching window (per design doc R3)
			ModelLock: &inst.mu, // serialize llama_decode access (Round 8 BUGFIX)
		})
		if err != nil {
			logger.Get().Warnw("BatchedScheduler init failed, falling back to Round 13 path",
				"name", name, "error", err)
		} else {
			bsCtx, bsCancel := context.WithCancel(context.Background())
			inst.batchedScheduler = bs
			inst.batchedSchedulerCancel = bsCancel
			inst.info.BatchedParallel = true
			go bs.Run(bsCtx)
			logger.Get().Infow("BatchedScheduler started (Round 15.1 batched parallel mode)",
				"name", name, "n_parallel", parallelSlots, "window_ms", 5)
		}
	} else if enableBatched && parallelSlots < 2 {
		logger.Get().Warnw("EnableBatchedParallel=true but parallel < 2; ignoring (need parallel >= 2 for batched seq_ids)",
			"name", name, "parallel", parallelSlots)
	}

	if inst.info.KVCacheType == "" {
		// Fallback на default (аналогично line 452 для cfg.KVCacheType).
		// Без этого /api/show показывает пустую строку, хотя llama.cpp использует
		// DefaultKVCacheType (f16).
		inst.info.KVCacheType = b.cfg.DefaultKVCacheType
	}
	// Round 17 (2026-07-31): resolve reasoningEnabled для этой модели ОДИН раз
	// при LoadModel. Per-model override (opts.EnableReasoning) имеет приоритет
	// над global default. Это source of truth для parser'а — handler читает
	// через inst.reasoningEnabled, чтобы IsReasoningModel(name) whitelist
	// не пропускал qwen3-instruct с SOFT prompt reasoning.
	enableReasoning := b.cfg.DefaultEnableReasoning
	if opts.EnableReasoning != nil {
		enableReasoning = *opts.EnableReasoning
	}
	inst.reasoningEnabled = enableReasoning
	inst.info.ReasoningEnabled = enableReasoning
	b.mu.Unlock()

	// Инициализируем lastUsedAt моментом загрузки, чтобы IdleUnloadManager
	// не считал только что загруженную модель сразу idle. Дальше Generate/GenerateStream
	// будут обновлять его на каждом запросе.
	inst.lastUsedAt.Store(time.Now())

	// R83 (2026-09-29): обновляем снимок свободной VRAM СРАЗУ после загрузки модели.
	//
	// Зачем. MemfitBudget больше не зовёт bridge.GetGPUInfo (см. memfit_adapter.go:
	// CUDA-вызовы сериализуются и подвешивали GET /api/models на время генерации).
	// Бюджет читает снимок b.gpuDevices, поэтому его надо освежать в точках, где
	// VRAM действительно меняется — то есть после загрузки/выгрузки модели.
	// Здесь инференса нет, CUDA свободна, вызов мгновенный.
	b.refreshGPUDeviceVRAM()

	// Записываем метрики
	if b.metrics != nil {
		b.metrics.RecordLoad(name)
	}

	// Round 22 (2026-08-03): записать name → path в ModelManager.nameHistory
	// чтобы resolveModelPath мог найти alias после idle-unload.
	// Без этого: auto-load для alias "qwen3-4b" (когда в директории 2+ .gguf)
	// падает с HTTP 500 "models/qwen3-4b.gguf not found".
	if mm := b.modelManager; mm != nil {
		mm.RecordModelLoad(name, path)
	}

	logger.Get().Infow("model loaded",
		"name", name,
		"path", path,
		"architecture", meta.Architecture,
		"layers", meta.NLayers,
		"gpuLayers", gpuLayers,
		"ctxSize", ctxSize,
		"batchSize", batchSize,
		"flashAttnType", opts.FlashAttnType,
		"gpuCount", b.gpuCount)

	// R114 (2026-10-13): модель загружена с раскладкой — включаем наблюдение за
	// узлами. Пропажа узла ПОСЛЕ загрузки иначе проявляется только на следующем
	// запросе: llama.cpp обращается к мёртвому устройству и падает с abort().
	// Watchdog выгружает модель в простое, и следующий запрос грузит её заново.
	if len(loadRPCServers) > 0 {
		b.startRPCWatchdog(name, loadRPCServers)
	}

	return nil
}
