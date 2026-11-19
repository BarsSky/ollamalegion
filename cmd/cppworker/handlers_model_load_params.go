// handlers_model_load_params.go — R623 (2026-11-19): загрузка модели с параметрами
// и прогресс загрузки.
//
// ЗАЧЕМ ВЫНЕСЕНО. `handlers_model.go` держал 2562 строки и девять обработчиков;
// найти в нём одну ручку можно было только чтением целиком. Здесь — отдельная
// поверхность: «загрузить модель с явными параметрами» (`handleLoadWithParams`) и
// «показать прогресс загрузки» (`handleLoadProgress`). Оба обработчика ходят в слоты
// и `cppbackend`, но не делят состояния с `handleLoadModel` — перенос дословный.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть; тесты `cmd/cppworker` зелёные до и
// после (включая наборы про lazy-load и n_ctx-гейты, которые дергают эти ручки).
package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)
func handleLoadWithParams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	// R304 (2026-10-30): вторая дверь к той же копии — загрузка с явными
	// параметрами. Гард тот же, что в handleLoadModel: это ОДНА операция «поднять
	// локальную копию», просто с другим набором полей в теле.
	if donorLoadGuard(w) {
		return
	}
	var req loadWithParamsRequest
	// Round 36 Phase 2: strict JSON decoder.
	if err := types.DecodeJSONRequest(r.Body, types.MaxRequestBodyBytes, &req); err != nil {
		switch {
		case errors.Is(err, types.ErrBodyEmpty):
			writeError(w, http.StatusBadRequest, "empty request body")
		case errors.Is(err, types.ErrBodyTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	modelPath := req.Path
	modelName := req.Name

	// Round 32 #14 (2026-08-11): strip .gguf extension from model name.
	// User screenshot showed две одинаковых модели в webui:
	//   gemma-4-E4B-it-Q4_K_M
	//   gemma-4-E4B-it-Q4_K_M.gguf
	// Root cause: balancer (or webui) отправляет name с .gguf, cppworker
	// сохраняет as-is. Fix: TrimSuffix ВСЕГДА — basename без .gguf
	// это canonical model name для inference / dedup / metrics.
	modelName = strings.TrimSuffix(modelName, ".gguf")

	// R91 (2026-10-08): явная загрузка модели — штатный способ оператора вылечить
	// узел после cgo SIGSEGV в chat template (см. chat_template_guard.go). Снимаем
	// пометку: следующий запрос снова попробует родной шаблон из GGUF.
	clearChatTemplateBroken(modelName)

	// Резолвим путь для HF-скачанных моделей (префикс hf:)
	if strings.HasPrefix(modelName, "hf:") && backend.HFDownloader() != nil {
		parts := strings.TrimPrefix(modelName, "hf:")
		if localPath, err := backend.HFDownloader().GetLocalPath(parts); err == nil && localPath != "" {
			modelPath = localPath
		}
	}
	if modelPath == "" {
		modelPath = resolveModelPath(modelName)
	}

	// R157 (2026-10-16): КАНОНИЗАЦИЯ ИМЕНИ по владельцу файла.
	//
	// ЖИВОЙ ДЕФЕКТ (оба бэкенда стенда). Модель поднимали под коротким именем-алиасом
	// (`gemma-4`), файл тот же `gemma-4-E4B-it-Q4_K_M.gguf`. Инстанс регистрировался
	// под алиасом, и запрос по КАНОНИЧЕСКОМУ имени падал на воркере:
	//   "chat generate failed: model gemma-4-E4B-it-Q4_K_M not found"
	// а балансер отдавал клиенту 502. Ни параметры (R154 их уже починил), ни видимость
	// в карточке (R156) этого не лечат: реестр моделей знает только алиас.
	//
	// Поэтому грузим/регистрируем под именем ВЛАДЕЛЬЦА ФАЙЛА, а клиенту сообщаем
	// заголовками, что имя заменено (он вправе ожидать своё).
	if owner := canonicalModelName(modelName, modelPath); owner != modelName {
		logger.Get().Infow(i18n.T("cppworker.handlers_model.load_canonical_name"),
			"requested", modelName, "canonical", owner, "path", modelPath)
		w.Header().Set("X-Model-Canonical", owner)
		w.Header().Set("X-Model-Requested", modelName)
		modelName = owner
	}

	// Round 19 (2026-07-10): fallbacks from currentConfig.Default* so that
	// CPPWORKER_GPU_LAYERS / CPPWORKER_CTX_SIZE / ... env vars are honored
	// by load handlers. Without this, *gpuLayers flag default (-1 = "all layers
	// on GPU") wins over the env value, and 21GB Q4_K_M models OOM on 24GB A10.
	defGPULayers := *gpuLayers
	defCtxSize := *ctxSize
	defBatchSize := *batchSize
	defFlashAttn := *flashAttn
	defNUMA := *numa
	defUseMmap := !*noMmap
	if currentConfig != nil {
		if currentConfig.DefaultGPULayers != 0 || os.Getenv("CPPWORKER_GPU_LAYERS") != "" {
			defGPULayers = currentConfig.DefaultGPULayers
		}
		if currentConfig.DefaultCtxSize != 0 || os.Getenv("CPPWORKER_CTX_SIZE") != "" {
			defCtxSize = currentConfig.DefaultCtxSize
		}
		if currentConfig.DefaultBatchSize != 0 || os.Getenv("CPPWORKER_BATCH_SIZE") != "" {
			defBatchSize = currentConfig.DefaultBatchSize
		}
		if currentConfig.DefaultFlashAttnType != 0 || os.Getenv("CPPWORKER_FLASH_ATTN_TYPE") != "" {
			defFlashAttn = currentConfig.DefaultFlashAttnType
		}
		if currentConfig.DefaultNUMA || os.Getenv("CPPWORKER_NUMA") != "" {
			defNUMA = currentConfig.DefaultNUMA
		}
		if currentConfig.DefaultUseMmap || os.Getenv("CPPWORKER_USE_MMAP") != "" {
			defUseMmap = currentConfig.DefaultUseMmap
		}
	}

	// Build load options (Round 19: env-driven defaults).
	// Phase 8 P.4: TensorSplit + SplitMode defaults from currentConfig
	// (CPPWORKER_TENSOR_SPLIT / CPPWORKER_SPLIT_MODE env vars or config file).
	// Falls back to request value if set explicitly.
	defTensorSplit2 := req.TensorSplit
	// S1 (2026-10-13): см. комментарий в handleLoadModel — -1 = LAYER (дефолт).
	defSplitMode2 := -1
	if req.SplitMode != nil {
		defSplitMode2 = *req.SplitMode
	}
	if currentConfig != nil {
		if len(currentConfig.DefaultTensorSplit) > 0 {
			defTensorSplit2 = currentConfig.DefaultTensorSplit
		}
		if currentConfig.DefaultSplitMode != 0 || os.Getenv("CPPWORKER_SPLIT_MODE") != "" {
			if v := currentConfig.DefaultSplitMode; v >= -1 && v <= 3 {
				defSplitMode2 = v
			}
		}
	}
	opts := cppbackend.LoadModelOpts{
		GPULayers:     defaultIntPtr(req.GPULayers, defGPULayers),
		ContextSize:   defaultIntPtr(req.ContextSize, defCtxSize),
		BatchSize:     defaultIntPtr(req.BatchSize, defBatchSize),
		FlashAttnType: defaultIntPtr(req.FlashAttnType, defFlashAttn),
		NUMA:          defaultBoolPtr(req.NUMA, defNUMA),
		UseMmap:       defaultBoolPtr(req.UseMmap, defUseMmap),
		TensorSplit:   defTensorSplit2,
		SplitMode:     defSplitMode2,
		// Round 15.1: per-model override для batched parallel inference.
		// nil = inherit global cfg.EnableBatchedParallel. non-nil = explicit choice.
		EnableBatchedParallel: req.EnableBatchedParallel,
		// Round 17 (2026-07-31): per-model override для reasoning parser routing.
		// nil = inherit global cfg.DefaultEnableReasoning. non-nil = explicit choice.
		// Решает bug docs/plans/bug-2026-07-31-reasoning-not-routed.md.
		EnableReasoning: req.EnableReasoning,
		// R248/B3: vision-проектор (mmproj) — только явный путь из запроса.
		MmprojPath: mmprojPathFromRequest(req.MmprojPath),
	}
	if req.NThreads != nil && *req.NThreads > 0 {
		opts.NThreads = *req.NThreads
	}
	if req.Parallel != nil && *req.Parallel > 0 {
		opts.Parallel = *req.Parallel
	}
	// R83 (2026-09-30): окно НА КЛИЕНТА — пересчёт в суммарный n_ctx делает
	// LoadModelWithOpts (там известны слоты и env-дефолт CPPWORKER_N_PARALLEL).
	if req.ContextPerSeq != nil && *req.ContextPerSeq > 0 {
		opts.ContextPerSeq = *req.ContextPerSeq
	}
	if req.KVCacheType != nil && *req.KVCacheType != "" {
		// Session 16: KVCacheType — строковое значение "f16"/"q8_0"/"q4_0".
		// Невалидные значения отбрасываются (cppbackend.LoadModelWithOpts
		// fallback'нет на default F16 если строка не пустая, но неизвестная).
		if isValidKVCacheType(*req.KVCacheType) {
			opts.KVCacheType = *req.KVCacheType
		} else {
			logger.Get().Warnw("load-with-params: ignoring invalid kvCacheType",
				"name", modelName, "kvCacheType", *req.KVCacheType,
				"validValues", []string{"f16", "q8_0", "q4_0"})
		}
	}
	if req.SplitMode != nil && *req.SplitMode >= 0 {
		opts.SplitMode = *req.SplitMode
	}
	if req.OverrideTensor != nil && *req.OverrideTensor != "" {
		opts.OverrideTensor = *req.OverrideTensor
	}
	// Round 7: prefer parallel-array OverrideTensors if present and matching length.
	if len(req.OverrideTensors) > 0 && len(req.OverrideTensors) == len(req.OverrideTensorBufts) {
		opts.OverrideTensors = req.OverrideTensors
		opts.OverrideTensorBufts = req.OverrideTensorBufts
		logger.Get().Infow("override-tensors (parallel arrays) applied",
			"name", modelName, "count", len(req.OverrideTensors))
	}

	// Round 26 (2026-08-06): pull-based profile sync from balancer.
	// Применяется ПОСЛЕ env defaults но ДО AutoTuneNCtx — чтобы AutoTuneNCtx
	// мог ещё уменьшить n_ctx/gpu_layers, если профиль задал слишком много
	// (partial offload fallback).
	if profileSyncer != nil {
		// R154: профиль ищем у ВЛАДЕЛЬЦА ФАЙЛА, а не у имени запроса. Загрузка под
		// коротким именем-алиасом поднимает тот же файл, и профиль алиаса/env-дефолты
		// молча подменяли параметры оператора (живой случай: gemma-4 вместо
		// gemma-4-E4B-it-Q4_K_M → ctx=65536, q8_0, reasoning=off).
		profileName := profileLookupNameForPath(modelName, modelPath)
		if profileName != modelName {
			logger.Get().Infow(i18n.T("cppworker.handlers_model.load_profile_from_file_owner"),
				"requested", modelName, "profile_key", profileName, "path", modelPath)
		}
		if prof := profileSyncer.applyProfileOnLoad(profileName); prof != nil {
			if !applyProfileToLoadRequest(prof, &opts.ContextSize, &opts.BatchSize, &opts.GPULayers, &opts.KVCacheType, &opts.Parallel) {
				logger.Get().Warnw("handleLoadWithParams: profile is disabled, refusing load",
					"name", modelName, "profile", prof)
				writeError(w, http.StatusForbidden, "model "+modelName+" is disabled by profile")
				return
			}
			// R366 (2026-10-31): flash attention из профиля — отдельным правилом.
			// Прежняя строка внутри помощника отдавала false → -1 (auto, то есть
			// «включить по усмотрению llama.cpp») и при дефолте контейнера -1 не
			// применялась вовсе. Подробности — profile_flash_attn.go.
			if fa := profileFlashAttnToLoad(req.FlashAttnType, prof.FlashAttn, opts.FlashAttnType); fa != opts.FlashAttnType {
				logger.Get().Infow("handleLoadWithParams: flashAttnType from model profile (R366)",
					"name", modelName, "flashAttnType", fa, "was", opts.FlashAttnType)
				opts.FlashAttnType = fa
			}
			logger.Get().Infow("handleLoadWithParams: applied profile (pull-based sync from balancer)",
				"name", modelName,
				"profileCtxSize", prof.ContextLength,
				"profileBatchSize", prof.BatchSize,
				"profileGPULayers", prof.NumGPULayers,
				"profileKVCacheType", prof.KVCacheType)
			// R83/v67 (2026-10-02): reasoning из профиля применяется тем же
			// правилом «заполняем только пустое». Раньше профильная галочка
			// «включить размышления» при загрузке через балансер терялась:
			// opts.EnableReasoning оставался nil → cfg.DefaultEnableReasoning.
			if opts.EnableReasoning == nil && prof.EnableReasoning != nil {
				opts.EnableReasoning = prof.EnableReasoning
				logger.Get().Infow(i18n.T("cppworker.handlers_model.load_enable_reasoning_from_profile"),
					"name", modelName, "enable_reasoning", *prof.EnableReasoning)
			}
		}
	}

	// Round 27 follow-up (v0.5.14 follow-up #3): apply SelectStrategy (memory auto-tune)
	// BEFORE the actual load. До этого момента AutoTuneNCtx вызывался только в
	// handleReloadModel — при auto-load через handleLoadWithParams (которую
	// использует балансер для executeLlamaCppLoad) AutoTuneNCtx не срабатывал.
	// Это приводило к OOM/crash при загрузке больших моделей с высокими n_ctx.
	//
	// Стратегия:
	//   1. Читаем GGUF header (lazy, уже кэширован в ModelManager).
	//   2. SelectStrategy подбирает n_ctx/gpu_layers под доступную VRAM + RAM.
	//   3. Если клиент явно передал n_ctx и оно влезает → не меняем.
	//   4. Если не влезает → уменьшаем n_ctx (partial offload / cpu-only fallback).
	//
	// Skip если клиент явно задал OverrideTensors (per-tensor routing сложнее
	// пересчитывать, не ломаем opt-in advanced flow).
	if len(req.OverrideTensors) == 0 && autoTuneNCtxOnLoadEnabled && req.ContextSize != nil {
		tunedGPULayers := 0
		if req.GPULayers != nil {
			tunedGPULayers = *req.GPULayers
		}
		tunedOpts, tunedRationale := calculateLazyLoadOpts(modelName, *req.ContextSize, tunedGPULayers, currentConfig)
		// R83 §9.3 (2026-09-26): «не влезает даже в CPU-only» — отказ, а не
		// «попробуем всё равно». Отдача плана в llama.cpp означала загрузку
		// 16.4 GB в RAM (вытесняя всё) и OOM/таймаут у клиента вместо причины.
		if fitErr := insufficientResourcesFromFallbackNoFit(modelName, tunedRationale); fitErr != nil {
			logger.Get().Warnw(i18n.T("cppworker.handlers_model.load_refused_no_fit"),
				"name", modelName,
				"model_size_mb", tunedRationale.ModelSize/(1024*1024),
				"available_ram_mb", tunedRationale.AvailableRAMBytes/(1024*1024),
				"requested_n_ctx", tunedRationale.RequestedNCtx,
				"max_viable_n_ctx", tunedRationale.MaxViableNCtx)
			writeInsufficientResourcesResponse(w, fitErr.(*InsufficientResourcesError))
			return
		}
		if tunedOpts.ContextSize > 0 && tunedOpts.ContextSize != *req.ContextSize {
			logger.Get().Infow("handleLoadWithParams: applying AutoTuneNCtx from GGUF meta",
				"name", modelName,
				"requested_n_ctx", *req.ContextSize,
				"tuned_n_ctx", tunedOpts.ContextSize,
				"requested_gpu_layers", tunedGPULayers,
				"tuned_gpu_layers", tunedOpts.GPULayers)
			*req.ContextSize = tunedOpts.ContextSize
			opts.ContextSize = tunedOpts.ContextSize
		}
		if tunedOpts.GPULayers != 0 && tunedOpts.GPULayers != tunedGPULayers {
			if req.GPULayers != nil {
				*req.GPULayers = tunedOpts.GPULayers
			}
			opts.GPULayers = tunedOpts.GPULayers
		}
		if tunedOpts.UseMmap {
			opts.UseMmap = true
		}
	}

	// R83 (2026-09-25): файла нет — 404 со списком доступных (см. model_not_found.go),
	// а не 500 от llama.cpp с несуществующим путём.
	if _, statErr := os.Stat(modelPath); statErr != nil {
		writeModelNotFoundResponse(w, modelName, modelPath)
		return
	}
	// R83 (2026-09-25): проверяем ИТОГОВЫЙ n_ctx — после profile-sync и после
	// AutoTuneNCtx выше, то есть то значение, с которым модель реально пойдёт
	// в llama.cpp. Если оно физически невыполнимо — 422 с числами и без старта
	// загрузки (иначе получим OOM/обрыв стрима вместо внятной причины).
	// R367: противоречие «flash attention выключен + квантованный KV» — отказ с причиной
	// ДО загрузки, иначе llama.cpp отдаёт невнятное "failed to create context".
	if !checkFlashAttnKVCacheR367(w, modelName, opts.FlashAttnType, opts.KVCacheType) {
		return
	}
	// R522 (§8.3.3): та же проверка финального окна, что и в handleLoadModel.
	gateNCtx, gateSlots := loadWindowForGate(opts)
	if gateNCtx != opts.ContextSize {
		logger.Get().Warnw("R522: гейт n_ctx проверяет ФИНАЛЬНОЕ окно, а не исходное",
			"name", modelName,
			"context_size_as_is", opts.ContextSize,
			"context_per_seq", opts.ContextPerSeq,
			"slots", gateSlots,
			"gate_total_n_ctx", gateNCtx)
	}
	if !checkNCtxBeforeLoad(w, modelName, gateNCtx, opts.KVCacheType, opts.UseMmap) {
		return
	}

	logger.Get().Infow("loading model with extended params",
		"name", modelName, "path", modelPath,
		"gpuLayers", opts.GPULayers, "ctxSize", opts.ContextSize,
		"batchSize", opts.BatchSize, "flashAttnType", opts.FlashAttnType,
		"numa", opts.NUMA, "tensorSplit", opts.TensorSplit,
		"nThreads", opts.NThreads, "parallel", opts.Parallel,
		"kvCacheType", opts.KVCacheType, "splitMode", opts.SplitMode,
		"overrideTensor", opts.OverrideTensor)

	loadStart := time.Now()

	// Round 24 (2026-08-04): parse sync/async load params (same as handleLoadModel).
	//   ?wait=true  → блокирующий (legacy Ollama clients), max waitTimeoutMs
	//   ?wait=false → async (default), возврат 202 + Location немедленно
	waitSync, waitTimeoutMs := parseLoadWaitParams(r)
	estimatedMs := estimateLoadTimeMs(modelPath, opts.ContextSize)

	// R303: мёртвую заявку на загрузку снимаем сами (см. load_progress.go).
	reclaimStaleClaimR303(modelName, "load-with-params")
	lockOk, lockErr := backend.TryLockLoadR303(modelName, "load-with-params")
	if lockErr != nil {
		if info, getErr := backend.GetModel(modelName); getErr == nil {
			if info.Path == modelPath && sameLoadOptions(*info, opts) {
				logger.Get().Infow("model already loaded with same parameters", "name", modelName)
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status": "already_loaded",
					"model":  info,
				})
				return
			}
			logger.Get().Infow("reloading model because parameters changed (load-with-params)",
				"name", modelName, "oldPath", info.Path, "newPath", modelPath)
			if unloadErr := backend.UnloadModel(modelName); unloadErr != nil {
				logger.Get().Errorw("failed to unload model before reload", "name", modelName, "error", unloadErr)
				writeError(w, http.StatusInternalServerError, "unload before reload failed: "+unloadErr.Error())
				return
			}
			lockOk, lockErr = backend.TryLockLoadR303(modelName, "load")
			if lockErr != nil {
				logger.Get().Errorw("race: model appeared after unload", "name", modelName)
				writeError(w, http.StatusInternalServerError, "concurrent load race after unload")
				return
			}
		} else {
			lockOk = true
		}
	}

	if !lockOk {
		respondWhileAnotherLoadInProgress(w, r, modelName, modelPath,
			estimatedMs, loadStart, waitSync)
		return
	}

	// Round 24 (2026-08-04): динамический timeout (async по умолчанию).
	if !waitSync {
		// Round 32 #8 (2026-08-10): dedup-by-path check ПЕРЕД writeLoadAccepted.
		// См. handleLoadModel для деталей.
		if existingName, existingInfo, found := backend.GetModelByPath(modelPath); found {
			logger.Get().Infow("handleLoadWithParams: dedup by path (async path, pre-check)",
				"requested_name", modelName, "existing_name", existingName, "path", modelPath)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"status":  "already_loaded",
				"model":   existingInfo,
				"dedup":   true,
				"message": fmt.Sprintf("model already loaded as %q (same file path)", existingName),
			})
			return
		}
		var sizeBytes int64
		if fi, statErr := os.Stat(modelPath); statErr == nil {
			sizeBytes = fi.Size()
		}
		lm := cppbackend.ModelInfo{
			Name:             modelName,
			Path:             modelPath,
			State:            cppbackend.StateLoading,
			GPULayers:        opts.GPULayers,
			BatchSize:        opts.BatchSize,
			FlashAttnType:    opts.FlashAttnType,
			NUMA:             opts.NUMA,
			UseMmap:          opts.UseMmap,
			LoadingStartedAt: time.Now(),
			LoadingSizeBytes: sizeBytes,
		}
		writeLoadAccepted(w, r, modelName, modelPath, sizeBytes, estimatedMs, lm)
		// R91: снимок backend берём здесь (в этой же горутине) — фоновая работа не
		// должна читать пакетную переменную, которую тесты подменяют и
		// восстанавливают в cleanup (иначе DATA RACE под -race).
		// Плюс регистрация в asyncWorkWG (см. async_work_tracker.go).
		be := backend
		done := trackAsyncWork()
		go func() {
			defer done()
			runAsyncLoad(modelName, modelPath, opts, balancerReg, be)
		}()
		return
	}

	// Sync mode: блокируем до waitTimeoutMs, потом 202.
	//
	// Round 52 (2026-08-24) BUGFIX: inline goroutine ОБЯЗАНА вызвать
	// UnlockLoad через defer после LoadModelWithOpts (success OR error).
	// До этого фикса в timeout-ветке lock утекал — request timeout'ился,
	// background goroutine продолжал грузить, channel в b.loading[name]
	// оставался незакрытым. Следующие load-запросы с тем же именем видели
	// `lockOk=false` от TryLockLoad и уходили в "model is already being
	// loaded; waiting" — до полного завершения background goroutine (60+ сек).
	//
	// Async path (выше, `go runAsyncLoad(...)`) не затрагивается —
	// runAsyncLoad сам вызывает UnlockLoad в конце.
	// R454 (2026-11-06): то же, что в handleLoadModel — снимок backend и
	// трекинг фоновой работы. Горутина отложенной синхронной загрузки читала
	// пакетную переменную `backend` и не была зарегистрирована в asyncWorkWG:
	// следующий тест успевал её подменить (DATA RACE под -race), а graceful
	// shutdown не ждал загрузку.
	be := backend
	loadDone := make(chan error, 1)
	done := trackAsyncWork()
	go func() {
		defer done()
		// defer UnlockLoad здесь, не в select: select может не
		// выполниться полностью (timeout wins), loadErr не придёт
		// в основной goroutine. defer гарантирует освобождение
		// lock когда LoadModelWithOpts ВЕРНЁТСЯ (успех или ошибка).
		defer be.UnlockLoad(modelName)
		// R60.57: r.Context() — client disconnect → ctx.Done() → watcher в
		// LoadModelWithOpts попытается abort load. C-side fix нужен для
		// full cancel; см. PLAN.md §3.1.
		//
		// R83-fix (2026-09-29): БОЛЬШЕ НЕ r.Context(). Загрузка — разделяемый
		// ресурс (одну модель грузит одна горутина, остальные ждут), поэтому
		// уход клиента, ПОПРОСИВШЕГО загрузку, обрывал её для всех: в логах
		// «model load aborted by user ... BRIDGE_ERR_ABORTED», а остальные
		// клиенты получали 503 «model is not loaded and auto-load failed».
		// Именно так балансер (его auto-load/reload — отдельный HTTP-клиент)
		// терял модель, когда исходный клиент отваливался по таймауту.
		// Правило и подробности — loadContextForSharedLoad (lazyload.go).
		loadCtx, cancelLoad := loadContextForSharedLoad()
		defer cancelLoad()
		loadDone <- be.LoadModelWithOpts(loadCtx, modelName, modelPath, opts)
	}()
	timeout := time.NewTimer(time.Duration(waitTimeoutMs) * time.Millisecond)
	defer timeout.Stop()
	select {
	case loadErr := <-loadDone:
		// background goroutine уже вызвал UnlockLoad через defer
		// (см. R52 fix выше). Дополнительный вызов здесь был бы
		// идемпотентен (delete-if-exists) — оставляем однократный вызов.
		if loadErr != nil {
			// Round 32 #8 (2026-08-10): dedup-by-path → success с status=already_loaded.
			var dedupErr *cppbackend.AlreadyLoadedAsError
			if errors.As(loadErr, &dedupErr) {
				logger.Get().Infow("handleLoadWithParams: model already loaded (dedup by path)",
					"requested_name", modelName, "existing_name", dedupErr.ExistingName, "path", dedupErr.Path)
				model, gErr := backend.GetModel(dedupErr.ExistingName)
				if gErr != nil {
					writeError(w, http.StatusInternalServerError, "dedup: "+gErr.Error())
					return
				}
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status":  "already_loaded",
					"model":   model,
					"dedup":   true,
					"message": fmt.Sprintf("model already loaded as %q (same file path)", dedupErr.ExistingName),
				})
				return
			}
			logger.Get().Errorw("failed to load model (load-with-params sync)", "name", modelName, "error", loadErr)
			writeError(w, http.StatusInternalServerError, "load failed: "+loadErr.Error())
			return
		}
		loadDuration := time.Since(loadStart)

		model, err := backend.GetModel(modelName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		if balancerReg != nil {
			// Round 34 (2026-08-12): добавили runtime params (kvCacheType,
			// flashAttnType, useMmap) для profile mismatch detection в balancer.
			balancerReg.notifyModelLoaded(modelName, model.Path, model.SizeBytes, model.ContextSize, model.GPULayers,
				model.KVCacheType, model.FlashAttnType, model.UseMmap)
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":              "loaded",
			"model":               model,
			"loadDurationMs":      loadDuration.Milliseconds(),
			"estimatedLoadTimeMs": estimatedMs,
			"appliedOpts":         opts,
		})
	case <-timeout.C:
		// Sync timeout: load still running in background goroutine.
		waitedMs := time.Since(loadStart).Milliseconds()
		var sizeBytes int64
		if fi, statErr := os.Stat(modelPath); statErr == nil {
			sizeBytes = fi.Size()
		}
		lm := cppbackend.ModelInfo{
			Name:             modelName,
			Path:             modelPath,
			State:            cppbackend.StateLoading,
			LoadingStartedAt: loadStart,
			LoadingSizeBytes: sizeBytes,
		}
		writeLoadWaitTimeout(w, r, modelName, modelPath, sizeBytes, waitedMs, estimatedMs, lm)
	}
}

// handleLoadProgress ? GET /api/models/load/progress?model=<name>
func handleLoadProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("model"))
	if name == "" || name == "*" {
		all := backend.GetLoadingModels()
		out := make([]map[string]interface{}, 0, len(all))
		now := time.Now()
		for _, m := range all {
			elapsed := int64(0)
			if !m.LoadingStartedAt.IsZero() {
				elapsed = now.Sub(m.LoadingStartedAt).Milliseconds()
			}
			out = append(out, map[string]interface{}{
				"name":               m.Name,
				"state":              m.State,
				"loadingStartedAt":   m.LoadingStartedAt.UTC().Format(time.RFC3339Nano),
				"loadingSizeBytes":   m.LoadingSizeBytes,
				"elapsedMs":          elapsed,
				"loadingProgressPct": m.LoadingProgressPct,
				"error":              m.LoadingError,
			})
		}
		// R66d: добавляем провалившиеся загрузки. Без этого «failed» был
		// неотличим от «ничего не происходит» (см. load_failures.go).
		for failedName, fe := range loadFailures.list() {
			out = append(out, map[string]interface{}{
				"name":     failedName,
				"state":    "failed",
				"error":    fe.Err,
				"failedAt": fe.At.UTC().Format(time.RFC3339Nano),
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"models": out, "count": len(out)})
		return
	}

	m, err := backend.GetModel(name)
	if err != nil {
		for _, lm := range backend.GetLoadingModels() {
			if lm.Name == name {
				elapsed := int64(0)
				if !lm.LoadingStartedAt.IsZero() {
					elapsed = time.Since(lm.LoadingStartedAt).Milliseconds()
				}
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"name": lm.Name, "state": lm.State,
					"loadingStartedAt": lm.LoadingStartedAt.UTC().Format(time.RFC3339Nano),
					"loadingSizeBytes": lm.LoadingSizeBytes, "elapsedMs": elapsed,
					"loadingProgressPct": lm.LoadingProgressPct,
					"error":              lm.LoadingError,
				})
				return
			}
		}
		// R66d: модель не загружена и не грузится. Если есть запись о провале —
		// отдаём 200 со state="failed" и настоящей причиной (раньше здесь был
		// 404 «model not found and not loading», из-за чего балансер продолжал
		// поллинг до maxWait и кормил клиента «подожди 90 секунд»).
		if fe, ok := loadFailures.get(name); ok {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"name":     name,
				"state":    "failed",
				"error":    fe.Err,
				"failedAt": fe.At.UTC().Format(time.RFC3339Nano),
			})
			return
		}
		writeError(w, http.StatusNotFound, "model not found and not loading: "+name)
		return
	}

	elapsed := int64(0)
	if !m.LoadedAt.IsZero() {
		elapsed = time.Since(m.LoadedAt).Milliseconds()
	}
	resp := map[string]interface{}{
		"name": m.Name, "state": m.State,
		"loadedAt":  m.LoadedAt.UTC().Format(time.RFC3339Nano),
		"elapsedMs": elapsed, "sizeBytes": m.SizeBytes, "contextSize": m.ContextSize,
	}
	// R215 (2026-10-21): прогресс весов нужен и на этом пути. Пока модель
	// грузится, она уже есть в реестре, и ответ выше - единственный, который
	// видит поллер: прежний вариант отдавал «loading» без процента, и новый
	// loadingProgressPct до UI не доезжал.
	if m.LoadingProgressPct > 0 {
		resp["loadingProgressPct"] = m.LoadingProgressPct
	}
	if !m.LoadingStartedAt.IsZero() {
		resp["loadingStartedAt"] = m.LoadingStartedAt.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}
