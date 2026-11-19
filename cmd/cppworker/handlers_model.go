package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/internal/memfit"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Model management handlers
// ============================================================

// handleLoadModel — POST /api/models/load (и /load). Загружает модель в VRAM.
func handleLoadModel(w http.ResponseWriter, r *http.Request) {
	// Round 36 Phase 3: method check per contract Section 9.2.
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	// R304 (2026-10-30): донор НЕ поднимает своих копий моделей — только делит
	// память с мастером. Проверка стоит первой содержательной (после формальной
	// проверки метода, чтобы 405 не превратился в 409): иначе обработчик успел бы
	// разобрать тело, занять заявку на загрузку и ответить 202 «принято», а копию
	// поднимать нельзя.
	if donorLoadGuard(w) {
		return
	}
	var req loadModelRequest
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

	// Если путь не задан явно, ищем через ModelManager
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
	defTensorSplit := req.TensorSplit
	// S1 (2026-10-13): -1 = «дефолт llama.cpp» = LAYER (см. c/bridge/bridge.h).
	// Раньше по умолчанию стоял 0 = LLAMA_SPLIT_MODE_NONE, и llama.cpp оставлял
	// из списка устройств только main_gpu: авто-tensor-split по локальным GPU и
	// RPC-узел не использовались вовсе (живая проба S1: «устройств в раскладке 2»,
	// а в логе — одна строка «using device CUDA0»).
	defSplitMode := -1
	if req.SplitMode != nil {
		defSplitMode = *req.SplitMode
	}
	if currentConfig != nil {
		if len(currentConfig.DefaultTensorSplit) > 0 {
			defTensorSplit = currentConfig.DefaultTensorSplit
		}
		if currentConfig.DefaultSplitMode != 0 || os.Getenv("CPPWORKER_SPLIT_MODE") != "" {
			if v := currentConfig.DefaultSplitMode; v >= -1 && v <= 3 {
				// -1 пробрасываем как есть: мост сам подставит LAYER.
				defSplitMode = v
			}
		}
	}
	// R364 (2026-10-31): профиль модели — и на legacy /api/models/load.
	//
	// До этой правки профиль применяли только handleLoadWithParams и
	// handleReloadModel (плюс ленивая загрузка через applySyncedProfileToLazyLoadOpts),
	// а здесь его не было вовсе. Замер на стенде: прямой
	// `POST /api/models/load {"name":"gemma-4-E4B-it-Q4_K_M"}` поднял модель на
	// конфиге контейнера — ctx 65536 вместо 32768, KV q8_0 вместо q4_0,
	// reasoning выключен — и выдал 2.9 tok/s против 25.9 у инстанса, поднятого
	// балансером. Приоритет тот же, что у балансера:
	// явный запрос > профиль модели > конфиг/env (см. load_profile_seed.go).
	profile := readProfileForLoadR364(modelName, modelPath)
	if profile != nil && profile.Profile.Disabled {
		// Профиль помечает модель сломанной (например, upstream GGML_ASSERT):
		// грузить её нельзя, иначе клиент уходит в crash-loop (Round 27).
		logger.Get().Warnw("handleLoadModel: profile is disabled, refusing load",
			"name", modelName, "profile_key", profile.Key)
		writeError(w, http.StatusForbidden, "model "+modelName+" is disabled by profile")
		return
	}
	defs := loadDefaultsR364{
		GPULayers:     defGPULayers,
		ContextSize:   defCtxSize,
		BatchSize:     defBatchSize,
		FlashAttnType: defFlashAttn,
	}
	seed := loadProfileSeedR364{}
	if profile != nil {
		seed = profile.Seed
	}
	params := resolveLoadParamsR364(req, seed, defs)
	if params.KVCacheInvalid {
		logger.Get().Warnw("handleLoadModel: ignoring invalid kvCacheType",
			"name", modelName, "kvCacheType", params.KVCacheRaw,
			"validValues", []string{"f16", "q8_0", "q4_0"})
	}
	opts := cppbackend.LoadModelOpts{
		GPULayers:     params.GPULayers,
		ContextSize:   params.ContextSize,
		BatchSize:     params.BatchSize,
		FlashAttnType: params.FlashAttnType,
		NUMA:          defaultBoolPtr(req.NUMA, defNUMA),
		UseMmap:       defaultBoolPtr(req.UseMmap, defUseMmap),
		TensorSplit:   defTensorSplit,
		SplitMode:     defSplitMode,
		// Round 44 (2026-08-19): kvCacheType/parallel — та же обработка, что в
		// handleLoadWithParams; значения приходят из resolveLoadParamsR364,
		// то есть с учётом профиля модели (R364).
		Parallel:    params.Parallel,
		KVCacheType: params.KVCacheType,
		// R248/B3: vision-проектор (mmproj) — только явный путь из запроса.
		MmprojPath: mmprojPathFromRequest(req.MmprojPath),
	}
	if profile != nil {
		// Печатаем РАЗРЕШЁННЫЕ значения, а не поля профиля: оператору нужно
		// видеть, что реально уедет в llama.cpp (явный запрос мог перебить
		// профиль, а какое-то поле могло остаться на конфиге).
		logger.Get().Infow("handleLoadModel: applied model profile (R364)",
			"name", modelName, "profile_key", profile.Key,
			"profile_ctx", profile.Profile.ContextLength,
			"ctxSize", opts.ContextSize,
			"gpuLayers", opts.GPULayers,
			"kvCacheType", opts.KVCacheType,
			"parallel", opts.Parallel)
	}
	// R83 (2026-09-30): окно НА КЛИЕНТА (пересчёт в суммарное — в
	// LoadModelWithOpts, там известны и слоты, и конфиг).
	if req.ContextPerSeq != nil && *req.ContextPerSeq > 0 {
		opts.ContextPerSeq = *req.ContextPerSeq
	}
	// R83/v67 (2026-10-02): per-model reasoning на legacy /api/models/load.
	// Балансер шлёт enableReasoning ВСЕГДА при загрузке по профилю модели
	// (а не только на /load-with-params), поэтому поле обязано приниматься и
	// здесь: без него строгий декодер отвечал 400 «unknown field
	// "enableReasoning"» и модель не поднималась вообще.
	//
	// R364: если запрос молчит — берём значение профиля, а не только глобальный
	// конфиг. Иначе прямой вызов поднимал модель с reasoning=false при профиле
	// «включить» (живой замер, см. шапку load_profile_seed.go).
	switch {
	case req.EnableReasoning != nil:
		opts.EnableReasoning = req.EnableReasoning
		logger.Get().Infow("handleLoadModel: per-model reasoning applied",
			"name", modelName, "enableReasoning", *req.EnableReasoning)
	case profile != nil && seed.Reasoning != nil:
		opts.EnableReasoning = seed.Reasoning
		logger.Get().Infow("handleLoadModel: reasoning from model profile (R364)",
			"name", modelName, "profile_key", profile.Key,
			"enableReasoning", *seed.Reasoning)
	}
	if req.OverrideTensor != nil && *req.OverrideTensor != "" {
		opts.OverrideTensor = *req.OverrideTensor
	}
	if len(req.OverrideTensors) > 0 && len(req.OverrideTensors) == len(req.OverrideTensorBufts) {
		opts.OverrideTensors = req.OverrideTensors
		opts.OverrideTensorBufts = req.OverrideTensorBufts
		logger.Get().Infow("handleLoadModel: override-tensors (parallel arrays) applied",
			"name", modelName, "count", len(req.OverrideTensors))
	}

	// R83 (2026-09-25): пред-загрузочная оценка n_ctx. Режим загрузки
	// (exact_fit / degraded / infeasible) пишется в лог ДО старта: раньше
	// /api/models/load рапортовал успех и при 32768, и при 65536, хотя во втором
	// случае стратегия уходила в partial_offload (часть слоёв в RAM) и ответ
	// становился настолько медленным, что клиент отваливался по таймауту.
	// Если запрошенное физически невыполнимо — 422 с числами, без старта загрузки.
	// См. cmd/cppworker/nctx_feasibility.go.
	// R83 (2026-09-25): сначала проверяем, что файл вообще есть. Раньше
	// несуществующий путь уходил прямо в llama.cpp, и оператор получал 500 с
	// сырым "failed to open GGUF file models/qwen3.8:latest.gguf" вместо
	// «такой модели нет, доступны вот эти» (см. model_not_found.go).
	if _, statErr := os.Stat(modelPath); statErr != nil {
		writeModelNotFoundResponse(w, modelName, modelPath)
		return
	}
	// R367: противоречие «flash attention выключен + квантованный KV» — отказ с причиной
	// ДО загрузки, иначе llama.cpp отдаёт невнятное "failed to create context".
	if !checkFlashAttnKVCacheR367(w, modelName, opts.FlashAttnType, opts.KVCacheType) {
		return
	}
	// R522 (§8.3.3): гейт — на ФИНАЛЬНОЕ окно (пересчёт «на клиента» → суммарное).
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

	logger.Get().Infow("loading model",
		"name", modelName, "path", modelPath,
		"gpuLayers", opts.GPULayers, "ctxSize", opts.ContextSize,
		"batchSize", opts.BatchSize, "flashAttnType", opts.FlashAttnType,
		"numa", opts.NUMA, "tensorSplit", opts.TensorSplit)

	// Запоминаем момент начала обработки запроса — используется для замера
	// времени ожидания при concurrent load (см. ниже).
	loadStart := time.Now()

	// Round 24 (2026-08-04): parse sync/async load params.
	//   ?wait=true  → блокирующий (legacy Ollama clients), max waitTimeoutMs
	//   ?wait=false → async (default), возврат 202 + Location немедленно
	// Динамическая оценка estimateLoadTimeMs() даёт клиенту реальное время,
	// а не фиксированные 60/120/180s — gemma-4 (5GB) реально загружается
	// 60-90s, что не влезает в дефолтные таймауты curl/OpenWebUI.
	waitSync, waitTimeoutMs := parseLoadWaitParams(r)
	estimatedMs := estimateLoadTimeMs(modelPath, opts.ContextSize)

	// Per-model blocking load
	//
	// R303: сначала снимаем МЁРТВУЮ заявку на загрузку (висит давно, живого
	// инстанса в состоянии loading нет). Без этого один умерший владелец заявки
	// блокировал все последующие загрузки модели до перезапуска контейнера
	// (живой стенд, D3: «load висел 260 c, unload отвечал 409 model is loading»).
	reclaimStaleClaimR303(modelName, "load")
	lockOk, lockErr := backend.TryLockLoadR303(modelName, "load")
	if lockErr != nil {
		// Модель уже загружена — проверяем, совпадают ли параметры.
		if info, getErr := backend.GetModel(modelName); getErr == nil {
			if info.Path == modelPath && sameLoadOptions(*info, opts) {
				logger.Get().Infow("model already loaded with same parameters", "name", modelName)
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status": "already_loaded",
					"model":  info,
				})
				return
			}
			// Параметры отличаются — выгружаем и перезагружаем.
			//
			// R451: печатаем ЧТО именно разошлось. oldPath/newPath у одного и того
			// же файла совпадают, поэтому прежняя строка не объясняла причину
			// перезагрузки — живой разбор (243 с посреди хода агента) шёл по
			// параметрам запроса на загрузку и арифметике окна, а не по логу.
			diffs := loadOptionDiffs(*info, opts)
			logger.Get().Infow("reloading model because parameters changed",
				"name", modelName, "oldPath", info.Path, "newPath", modelPath,
				"changed", diffs, "changed_count", len(diffs))
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

	// Round 24 (2026-08-04): динамический timeout.
	// В async-режиме (?wait=false, default) возвращаем 202 + Location сразу,
	// реальная загрузка идёт в background goroutine, не привязанной к r.Context().
	// Это решает проблему gemma-4 (5GB, 60-90s load): клиент больше не
	// отваливается по таймауту, т.к. HTTP-запрос завершается за <100ms.
	if !waitSync {
		// Round 32 #8 (2026-08-10): dedup-by-path check ПЕРЕД writeLoadAccepted.
		// Без этого async path возвращал 202 + spawn'ил background goroutine
		// которая делала dedup check — но response уже отправлен, dedup
		// логировался как error в фоне. Пользователь видел "loading" статус
		// в UI, который никогда не становился "loaded" (потому что реально
		// load не шёл). Теперь: если same path уже загружен — return 200 OK
		// с status=already_loaded сразу, без spawn'а background goroutine.
		if existingName, existingInfo, found := backend.GetModelByPath(modelPath); found {
			logger.Get().Infow("handleLoadModel: dedup by path (async path, pre-check)",
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
		// Spawn background load. runAsyncLoad handles UnlockLoad, notifyModelLoaded.
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

	// Sync mode (?wait=true): блокируем на загрузку, но не дольше waitTimeoutMs.
	// CGo-вызов не отменяется, поэтому при таймауте возвращаем 202 — load
	// продолжится в background и клиент сможет дополлить progress.
	//
	// R60.57: передаём r.Context() в LoadModelWithOpts. Если клиент отвалится
	// по timeout/close — watcher в LoadModelWithOpts попытается abort load
	// через C-bridge (полная cancellable load требует C-side fix — см. PLAN.md §3.1).
	// R454 (2026-11-06): в фоновую горутину — СНИМОК backend и трекинг.
	//
	// ЖИВОЙ СБОЙ CI (self-hosted Windows, `cmd/... -race`):
	//
	//	WARNING: DATA RACE
	//	Write at ... setupLoadLockTestBackend (пересоздаёт глобальный backend)
	//	Previous read at ... donorMarkR304 (читает глобальный backend)
	//
	// Это ровно тот класс, что R91 уже починил для async-пути: горутина
	// отложенной синхронной загрузки читала ПАКЕТНУЮ переменную `backend` и не
	// была зарегистрирована в asyncWorkWG. Поэтому (а) следующий тест успевал
	// подменить глобальную переменную, пока горутина её читала, и (б) graceful
	// shutdown не ждал эту загрузку — мог закрыть процесс посреди загрузки.
	// Теперь как в async-пути: be := backend до горутины + trackAsyncWork.
	be := backend
	loadDone := make(chan error, 1)
	done := trackAsyncWork()
	go func() {
		defer done()
		// R365 (2026-10-31): ЗАЯВКА ОСВОБОЖДАЕТСЯ ЗДЕСЬ, а не в select.
		//
		// ЖИВОЙ ДЕФЕКТ (этот же раунд, сразу после R364). Select ниже может
		// уйти по таймауту (waitTimeoutMs по умолчанию 60 с, а холодная
		// загрузка gemma-4 — 60–100 с), и тогда `UnlockLoad` в ветке
		// успеха не выполнялся НИКОГДА: заявка оставалась в b.loading, хотя
		// модель уже загружена. Следующая загрузка того же имени получала
		// «model is already being loaded by another request; waiting», ждала
		// предел ожидания (90 с) и возвращала 200 с ПАРАМЕТРАМИ ПРЕДЫДУЩЕЙ
		// загрузки — то есть просьба «перезагрузи с другими параметрами»
		// молча не выполнялась.
		//
		// Замер: `POST /api/models/load?wait=true` (таймаут 60 с) → 202 на 61-й
		// секунде, «model loaded» в логе на 8-й секунде после этого, а
		// следующая загрузка через 12 с всё ещё видела заявку и ждала 90001 мс.
		//
		// Ровно это уже было исправлено в handleLoadWithParams (Round 52), но
		// legacy-путь остался с прежним поведением. Теперь одинаково: defer в
		// горутине гарантирует освобождение, когда LoadModelWithOpts вернётся
		// (успех или ошибка), а ветка успеха ниже UnlockLoad не зовёт.
		defer be.UnlockLoad(modelName)
		// R83-fix (2026-09-29): контекст загрузки отвязан от HTTP-запроса.
		// См. loadContextForSharedLoad (lazyload.go) и комментарий в
		// handleLoadWithParams — уход клиента не должен убивать общую загрузку.
		loadCtx, cancelLoad := loadContextForSharedLoad()
		defer cancelLoad()
		loadDone <- be.LoadModelWithOpts(loadCtx, modelName, modelPath, opts)
	}()
	timeout := time.NewTimer(time.Duration(waitTimeoutMs) * time.Millisecond)
	defer timeout.Stop()
	select {
	case loadErr := <-loadDone:
		// Заявку уже снял defer в горутине (R365) — здесь не повторяем.
		if loadErr != nil {
			// Round 32 #8 (2026-08-10): dedicated dedup-by-path error → success
			// с status=already_loaded. Без этого dedup handler возвращал 500
			// и 2x VRAM был занят (gemma-4 vs gemma-4.gguf).
			var dedupErr *cppbackend.AlreadyLoadedAsError
			if errors.As(loadErr, &dedupErr) {
				logger.Get().Infow("handleLoadModel: model already loaded (dedup by path)",
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
			logger.Get().Errorw("failed to load model (sync)", "name", modelName, "error", loadErr)
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
		})
	case <-timeout.C:
		// Sync timeout: load still running in background goroutine.
		// It will UnlockLoad when done. Return 202 with current state.
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

// handleLoadWithParams ? POST /api/models/load-with-params.
//
// Расширенная версия handleLoadModel с дополнительными llama.cpp параметрами:
//   - nThreads       — CPU-потоки (0 = auto)
//   - parallel       — параллельные sequences (требует больше KV-cache VRAM)
//   - kvCacheType    — F16/Q8_0/Q4_0 (Q8_0 экономит ~50% KV-cache VRAM)
//   - splitMode      — layer/row для multi-GPU tensor split
//   - overrideTensor — regex-паттерн для переопределения dtype тензоров
//
// Совместимость: все базовые поля из loadModelRequest тоже принимаются
// (json-теги совпадают). Если все расширенные поля == nil/0 —
// поведение полностью совпадает с handleLoadModel.
//
// Логика повторяет handleLoadModel (race-condition handling, parallel
// requests wait, already_loaded detection) — отличия только в более
// широком наборе параметров для LoadModelOpts.
//
// Прокси через балансировщик: см. internal/balancer/llamacpp_handlers_load.go
// (handleLlamaCppLoad с поддержкой опциональных полей через mapstructure).
func handleUnloadModel(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}

	// Round 51.6 (2026-08-20): safety check — refuse unload если есть активные
	// inference-запросы (use-after-free в C-bridge: in-flight generate вызов
	// продолжает использовать handle который UnloadModel уже освободил).
	//
	// Без этого check (предыдущее поведение) unload посреди генерации мог
	// вызвать:
	//   - crash в C-bridge (SIGABRT) — handle double-free
	//   - 502 connection refused для клиента посреди SSE стрима
	//   - WebUI показывал "успешная выгрузка" хотя клиент получил EOF
	//
	// Теперь возвращаем 409 Conflict с деталями, либо принимаем ?force=true
	// override (логируется как WARN для последующего postmortem).
	force := r.URL.Query().Get("force") == "true"
	if !force {
		if inflight := backend.InFlight(); inflight != nil {
			if n := inflight.Get(name); n > 0 {
				// Собираем детали активных запросов для user_id list
				var userIDs []string
				if gen := backend.ActiveGenerations(); gen != nil {
					for _, e := range gen.Snapshot() {
						if e.Model == name {
							userIDs = append(userIDs, e.UserID)
						}
					}
				}
				logger.Get().Warnw("unload refused: model has active requests",
					"name", name, "active_queries", n, "user_ids", userIDs)
				writeJSON(w, http.StatusConflict, map[string]interface{}{
					"error":          "model is busy with active inference requests",
					"name":           name,
					"active_queries": n,
					"user_ids":       userIDs,
					"hint":           "wait for in-flight requests to complete, or retry with ?force=true to cancel them (will disconnect active clients)",
				})
				return
			}
		}
	} else {
		// force=true: отменяем все активные генерации для этой модели
		if gen := backend.ActiveGenerations(); gen != nil {
			cancelled := gen.CancelByModel("", name)
			if cancelled > 0 {
				logger.Get().Warnw("unload force=true: cancelled active generations",
					"name", name, "cancelled", cancelled)
			}
		}
	}

	logger.Get().Infow("unloading model", "name", name, "force", force)

	// R83 §9.6 D-C (2026-09-26): unload во время ЗАГРУЗКИ — не барьер.
	//
	// Проверка выше смотрит только активные inference-запросы (InFlight), а
	// фоновая загрузка в неё не попадает. Поэтому unload 16 GB модели,
	// которая сейчас читается с диска, отвечал 200 мгновенно: модель удалялась
	// из реестра, а через минуты возвращалась туда фоновой загрузкой —
	// «model loaded в логе, /api/models → count=0», то есть расхождение
	// состояния и 200 OK на операцию, которая фактически не произошла.
	//
	// Честный ответ — 409, как и для активной генерации: вызывающий (WebUI,
	// скрипт) должен знать, что сейчас unload невозможен. force=true
	// продавливает: загрузка не отменяется мгновенно (у неё свои checkpoints),
	// но после неё модель будет выгружена — последовательность команд
	// «unload --force» + «load» даёт предсказуемый результат.
	// R303: мёртвая заявка на загрузку не должна держать unload в 409 «model is
	// currently loading» вечно (живой стенд, D3: «лечится только перезапуском»).
	// Снимаем её, если она провably мёртвая (нет инстанса в состоянии loading).
	if backend.IsLoading(name) {
		reclaimStaleClaimR303(name, "unload")
	}
	if backend.IsLoading(name) {
		claim, haveClaim := claimForModelR303(name)
		if !force {
			logger.Get().Warnw("unload refused: model is loading",
				"name", name,
				"claim_age_sec", claim.AgeSec,
				"claim_requester", claim.Requester,
				"loading_instance", claim.LoadingInstance)
			body := map[string]interface{}{
				"error": "model is currently loading",
				"name":  name,
				"hint": "wait for the load to finish (poll /api/models until state=loaded), " +
					"or retry with ?force=true to CANCEL the load and free VRAM immediately",
			}
			if haveClaim {
				body["claimAgeSec"] = claim.AgeSec
				body["claimRequester"] = claim.Requester
				body["loadingInstance"] = claim.LoadingInstance
				body["staleAfterSec"] = loadStaleAfterR303().Seconds()
			}
			writeJSON(w, http.StatusConflict, body)
			return
		}
		// R215 (2026-10-21): force-выгрузка грузящейся модели = ОТМЕНА загрузки.
		//
		// До этого force означал «дождаться конца загрузки и выгрузить»: веса
		// успевали лечь на GPU мастера и узлов раскладки, и VRAM держалась всё
		// время чтения файла (минуты для крупных моделей). Теперь C-мост
		// получает флаг отмены, progress-callback llama.cpp возвращает false, и
		// загрузка останавливается в пределах одного тензора; частично
		// загруженная модель освобождается самим llama.cpp (включая буферы на
		// узлах).
		aborted := backend.AbortLoad(name)
		logger.Get().Warnw(i18n.T("cppworker.handlers_model.unload_force_cancels_load"),
			"name", name, "abort_requested", aborted)
		// Ждём, пока загрузка реально отпустит: C-мост проверяет флаг не чаще,
		// чем на каждом тензоре, поэтому это сотни миллисекунд, а не мгновение.
		deadline := time.Now().Add(15 * time.Second)
		for backend.IsLoading(name) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if !backend.IsLoading(name) {
			// Загрузка завершилась: либо отменена (инстанс удалён), либо успела
			// загрузиться — во втором случае обычный unload ниже её выгрузит.
			if _, err := backend.GetModel(name); err != nil {
				if balancerReg != nil {
					balancerReg.notifyModelUnloaded(name)
				}
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status":  "load_cancelled",
					"name":    name,
					"message": i18n.T("cppworker.handlers_model.unload_load_cancelled_message"),
					"aborted": aborted,
				})
				return
			}
		}
		if backend.IsLoading(name) {
			// R303: force не помог — загрузка не отпустила за 15 c. Раньше такой
			// запрос молча шёл дальше и получал 404/200 «как будто выгрузили».
			// Теперь отвечаем честно: загрузка зависла, называем её возраст и
			// что делать (её держит C-мост; снять заявку нельзя, пока жив
			// инстанс в состоянии loading — иначе второй LoadModel на тот же
			// файл даёт двойную VRAM/SIGABRT).
			writeLoadInProgressR303(w, name, 15*time.Second)
			return
		}
	}

	if err := backend.UnloadModel(name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// Round 34 (2026-08-12) Phase 3: notify balancer что модель unloaded.
	// Без этого lastKnownNCtx в NCtxReloadCoordinator остаётся прежним (после
	// `idle_unload_after` 10m), preflight думает модель загружена с большим
	// n_ctx, не триггерит reload → пользователь получает 502 connection refused
	// (модель не загружена). Cline-сессии ломаются.
	if balancerReg != nil {
		balancerReg.notifyModelUnloaded(name)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unloaded", "name": name})
}

// handleListModels ? GET /api/models.
//
// Расширен 2026-06-26: теперь возвращает top-level поля с лимитами VRAM/RAM
// и max n_ctx для балансировщика (preflight_nctx). До этой фичи балансер
// видел max_vram_n_ctx=0 и model_max_context=0 для всех моделей
// (env_log.txt строки 121, 133) и не мог расчитать целевой n_ctx.
//
// Лимиты считаются для первой загруженной модели. Если моделей несколько,
// берём минимальный MaxVRAMNCtx (т.к. лимиты VRAM делятся между моделями).
// Если моделей нет — лимиты возвращаются для дефолтной модели из ModelManager.
//
// NB: эти поля — ресурсы cppworker'а, а не конкретной модели. Для каждой
// модели балансер может запросить /api/v1/cluster/models/{name}/reload с
// явным contextSize — в этом случае он сам подберёт n_ctx.
func handleGetModel(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}
	model, err := backend.GetModel(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, model)
}

// handleDeleteModel — удаление .gguf файла с диска (DELETE /api/models/delete, /api/delete).
func handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "use POST or DELETE")
		return
	}

	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeJSONRequest(r, &req, 0); err == nil {
			name = strings.TrimSpace(req.Name)
		}
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required (JSON body or ?name= query param)")
		return
	}

	filename := name
	if !strings.HasSuffix(strings.ToLower(filename), ".gguf") {
		filename = filename + ".gguf"
	}

	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager not available")
		return
	}

	// R66d (2026-09-23): удаление алиаса (<name>.gguf.json) удаляет ТОЛЬКО сам
	// алиас, а не файл-источник. Это отдельная ветка намеренно: иначе (если
	// когда-нибудь delete начнёт ходить через resolveModelPath, который умеет
	// резолвить алиасы) удаление алиаса снесло бы сам .gguf модели.
	if alias, isAlias := mm.AliasByName(name); isAlias {
		logger.Get().Infow("delete: removing model alias (source file is kept)",
			"alias", alias.Name, "source", alias.Source, "alias_path", alias.AliasPath)
		if err := os.Remove(alias.AliasPath); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to delete alias: "+err.Error())
			return
		}
		if _, scanErr := mm.ScanModels(); scanErr != nil {
			logger.Get().Warnw("delete: rescan after alias removal failed", "error", scanErr)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "deleted", "name": alias.Name, "alias": true, "source": alias.Source,
		})
		return
	}

	modelPath, err := mm.FindModelByPath(filename)
	if err != nil {
		writeError(w, http.StatusNotFound, "model not found: "+err.Error())
		return
	}

	modelName := strings.TrimSuffix(filepath.Base(modelPath), ".gguf")
	if _, getErr := backend.GetModel(modelName); getErr == nil {
		logger.Get().Infow("delete: unloading model from memory before file removal", "name", modelName)
		if unloadErr := backend.UnloadModel(modelName); unloadErr != nil {
			logger.Get().Warnw("delete: unload before delete failed", "name", modelName, "error", unloadErr)
		}
	}

	logger.Get().Infow("deleting model file", "name", modelName, "path", modelPath)
	if err := os.Remove(modelPath); err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "model file does not exist: "+modelPath)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete model: "+err.Error())
		return
	}

	if _, scanErr := mm.ScanModels(); scanErr != nil {
		logger.Get().Warnw("delete: model rescan failed", "error", scanErr)
	}

	logger.Get().Infow("model deleted", "name", modelName, "path", modelPath)
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "deleted", "name": modelName, "filename": filepath.Base(modelPath),
	})
}

// handleReloadModel ? POST /api/models/reload.
func handleReloadModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	// R304 (2026-10-30): reload — это unload + load, то есть ТО ЖЕ поднятие
	// локальной копии. Донор, у которого копия всё-таки оказалась в памяти
	// (роль объявили после загрузки), не имеет права перезагрузить её «с другими
	// параметрами»: копии ему держать нельзя вовсе, а не «нельзя только новые».
	if donorLoadGuard(w) {
		return
	}
	var req reloadModelRequest
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

	// Round 24 (2026-08-04): async reload (?wait=false default) — Bug #1 fix.
	// Reload с новыми n_ctx/gpu_layers инициирует UnloadModel + LoadModelWithOpts.
	// Для gemma-4 (5GB) reload занимает 30-60+ сек, что превышает default
	// curl/JS timeout. Default async: возвращаем 202 + Location, реальный
	// reload идёт в background. Клиент polls /api/models/load/progress.
	waitSync, waitTimeoutMs := parseLoadWaitParams(r)

	current, err := backend.GetModel(req.Name)
	if err != nil {
		writeError(w, http.StatusNotFound, "model not currently loaded: "+err.Error()+
			" ? use /api/models/load to load it first")
		return
	}

	modelPath := current.Path
	if modelPath == "" {
		if mm := backend.ModelManager(); mm != nil {
			if foundPath, ferr := mm.FindModelByPath(req.Name); ferr == nil {
				modelPath = foundPath
			}
		}
	}
	if modelPath == "" {
		writeError(w, http.StatusInternalServerError, "could not resolve model path for "+req.Name)
		return
	}

	// Compute estimated time early so we can return it in 202 response.
	estimatedMs := estimateLoadTimeMs(modelPath, current.ContextSize)

	if req.ContextSize != nil {
		if *req.ContextSize < 256 || *req.ContextSize > 262144 {
			writeError(w, http.StatusBadRequest, "contextSize must be in [256, 262144], got "+fmt.Sprintf("%d", *req.ContextSize))
			return
		}
	}
	if req.BatchSize != nil && *req.BatchSize < 1 {
		writeError(w, http.StatusBadRequest, "batchSize must be >= 1")
		return
	}
	if req.GPULayers != nil && *req.GPULayers < -2 {
		writeError(w, http.StatusBadRequest, "gpuLayers must be >= -2 (-1 = all layers, -2 = AUTO)")
		return
	}

	opts := cppbackend.LoadModelOpts{
		GPULayers:     defaultIntPtr(req.GPULayers, current.GPULayers),
		ContextSize:   defaultIntPtr(req.ContextSize, current.ContextSize),
		BatchSize:     defaultIntPtr(req.BatchSize, current.BatchSize),
		FlashAttnType: defaultIntPtr(req.FlashAttn, current.FlashAttnType),
		NUMA:          defaultBoolPtr(req.NUMA, current.NUMA),
		UseMmap:       defaultBoolPtr(req.UseMmap, current.UseMmap),
		TensorSplit:   current.TensorSplit,
		// R179 (2026-10-19): РЕЖИМ РАСКЛАДКИ ОБЯЗАН ДОЕХАТЬ ДО МОСТА И ЗДЕСЬ.
		//
		// ЖИВАЯ ПРИЧИНА потери раскладки при «Применить профиль»: reload собирал
		// LoadModelOpts из current, но SplitMode НЕ передавал вовсе — в мост уходил
		// 0 = LLAMA_SPLIT_MODE_NONE. При NONE llama.cpp оставляет из списка устройств
		// только main_gpu и RPC-узел отбрасывает (тот же класс дефекта, что описан в
		// internal/cppbackend/backend.go рядом с cfg.SplitMode = opts.SplitMode).
		// Внешне это выглядело так: планировщик видел ДВА устройства, серверы
		// передавались в мост, а llama.cpp печатал только `using device CUDA0`.
		// Путь /api/models/load-with-params передавал режим всегда — поэтому свежая
		// загрузка раскладывала слои, а reload нет.
		SplitMode: reloadSplitMode(req.SplitMode),
		// Session 16 (2026-06-27): Parallel + KVCacheType через /api/models/reload
		// для применения Per-Model Profile (parallel + kvCacheType).
		// По умолчанию (nil от клиента) — inherit из текущей загрузки.
		Parallel:    current.Parallel,
		KVCacheType: current.KVCacheType,
		// R83/v67 (2026-10-02): reasoning НЕ сбрасывается перезагрузкой.
		// Было: opts.EnableReasoning = nil → cppbackend падал на
		// cfg.DefaultEnableReasoning (false), и любой preflight-reload молча
		// выключал уже включённые размышления. Явный req.EnableReasoning ниже
		// имеет приоритет над текущим состоянием.
		EnableReasoning: currentReasoningPtr(current),
		// R248/B3: reload НЕ должен выключать vision. Источник — явный
		// req.MmprojPath, иначе уже загруженный проектор текущей модели.
		MmprojPath: mmprojPathOrCurrent(req.MmprojPath, current.MmprojPath),
	}
	// Round 7: forward MoE override-tensors (parallel arrays) from request
	// body to LoadModelOpts so the bridge applies them to llama_model_params.
	if len(req.OverrideTensors) > 0 && len(req.OverrideTensors) == len(req.OverrideTensorBufts) {
		opts.OverrideTensors = req.OverrideTensors
		opts.OverrideTensorBufts = req.OverrideTensorBufts
	}
	// R83 §9.4 шаг 1б (2026-09-26): escape-hatch для отказа по памяти.
	//
	// Объявлено до auto-offload блока, потому что решение об отказе (ниже) уже
	// должно его видеть: оператор, который сознательно хочет попробовать,
	// ставит force. forceReloadRequest объединяет JSON-поле force и
	// ?force=true — как в handleUnloadModel, чтобы не заставлять клиента
	// переписывать тело ради одной попытки.
	forceReload := forceReloadRequest(r, req.Force)

	// R83 §9.4 шаг 1б (2026-09-26), вариант D: «загрузим, но без GPU».
	//
	// Пустая строка = обычная загрузка. Заполняется вердиктом memfit в блоке
	// auto-offload (и только если memfit реально судил — без метаданных GGUF
	// вердикта нет). Объявлено здесь, а не внутри блока, потому что заголовок
	// X-CppWorker-Degraded-Stage нужно поставить уже после решения, но до
	// первого WriteHeader — а это две разные ветки ниже (202 и 200).
	degradedStage := ""
	if req.Parallel != nil {
		opts.Parallel = *req.Parallel
	}
	// R83/v67 (2026-10-02): явный enableReasoning в теле reload перекрывает
	// унаследованное состояние (current.ReasoningEnabled, см. opts выше).
	if req.EnableReasoning != nil {
		opts.EnableReasoning = req.EnableReasoning
		logger.Get().Infow("handleReloadModel: per-model reasoning applied",
			"name", req.Name, "enableReasoning", *req.EnableReasoning)
	}
	// R83 (2026-09-30): окно на клиента при reload — та же семантика, что и при
	// загрузке (пересчёт в суммарный n_ctx делает LoadModelWithOpts).
	if req.ContextPerSeq != nil && *req.ContextPerSeq > 0 {
		opts.ContextPerSeq = *req.ContextPerSeq
	}
	if req.KVCacheType != nil && *req.KVCacheType != "" {
		// Валидируем — пропускаем незнакомые значения (cppbackend fallback'нет
		// на default F16, но лучше предупредить пользователя в логах).
		if isValidKVCacheType(*req.KVCacheType) {
			opts.KVCacheType = *req.KVCacheType
		} else {
			logger.Get().Warnw("reload: ignoring invalid kvCacheType",
				"name", req.Name, "kvCacheType", *req.KVCacheType,
				"validValues", []string{"f16", "q8_0", "q4_0"})
		}
	}

	// Round 26 (2026-08-06): pull-based profile sync from balancer.
	// При reload запрошенные параметры имеют приоритет над профилем (явный вызов),
	// но если reload идёт без параметров (пустой body) — профиль задаёт дефолты.
	if profileSyncer != nil {
		// R154: то же правило, что при загрузке — профиль владельца ФАЙЛА.
		profileName := profileLookupNameForPath(req.Name, modelPath)
		if profileName != req.Name {
			logger.Get().Infow(i18n.T("cppworker.handlers_model.reload_profile_from_file_owner"),
				"requested", req.Name, "profile_key", profileName, "path", modelPath)
		}
		if prof := profileSyncer.applyProfileOnLoad(profileName); prof != nil {
			// For reload: apply profile to fields that are still zero-value
			// (i.e. user didn't override them via body).
			if !applyProfileToLoadRequest(prof, &opts.ContextSize, &opts.BatchSize, &opts.GPULayers, &opts.KVCacheType, &opts.Parallel) {
				logger.Get().Warnw("handleReloadModel: profile is disabled, refusing reload",
					"name", req.Name, "profile", prof)
				writeError(w, http.StatusForbidden, "model "+req.Name+" is disabled by profile")
				return
			}
			// R366 (2026-10-31): flash attention из профиля. Дефолт здесь —
			// значение ТЕКУЩЕЙ загрузки (reload без параметров наследует её),
			// профиль перебивает только унаследованное, явный запрос главнее.
			if fa := profileFlashAttnToLoad(req.FlashAttn, prof.FlashAttn, opts.FlashAttnType); fa != opts.FlashAttnType {
				logger.Get().Infow("handleReloadModel: flashAttnType from model profile (R366)",
					"name", req.Name, "flashAttnType", fa, "was", opts.FlashAttnType)
				opts.FlashAttnType = fa
			}
			logger.Get().Infow("handleReloadModel: applied profile (pull-based sync from balancer)",
				"name", req.Name,
				"profileCtxSize", prof.ContextLength,
				"profileBatchSize", prof.BatchSize,
				"profileGPULayers", prof.NumGPULayers,
				"profileKVCacheType", prof.KVCacheType)
			// R83/v67 (2026-10-02): профиль может ВКЛЮЧИТЬ размышления, а
			// тело reload их не задаёт. Явное req.EnableReasoning уже
			// проставлено выше — здесь трогаем только незаданное.
			if req.EnableReasoning == nil && prof.EnableReasoning != nil {
				opts.EnableReasoning = prof.EnableReasoning
				logger.Get().Infow(i18n.T("cppworker.handlers_model.reload_enable_reasoning_from_profile"),
					"name", req.Name, "enable_reasoning", *prof.EnableReasoning)
			}
		}
	}

	// === Auto-offload: если GPULayers=-2 (AUTO) или auto_offload включён —
	// рассчитываем оптимальное число GPU-слоёв для запрошенного n_ctx.
	// Это решает проблему: большая модель (~18GB) на 20GB VRAM при
	// gpu_layers=-1 занимает всю VRAM → KV-cache не помещается →
	// max_vram_n_ctx маленький → балансировщик reject'ит запрос.
	// При auto_offload gpu_layers уменьшается, часть весов уходит в RAM
	// (mmap), освобождая VRAM для KV-cache большего размера.
	// R83 (2026-09-25): проверяем n_ctx после profile-sync — это то значение,
	// с которым пойдёт reload. Балансер сам инициирует reload на больший n_ctx
	// (preflight), и именно этот путь чаще всего просил 65536/131072 на A10.
	// Физически невыполнимое — 422 с числами, без unload/load.
	if !checkFlashAttnKVCacheR367(w, req.Name, opts.FlashAttnType, opts.KVCacheType) {
		return
	}
	// R522 (§8.3.3): reload-путь обязан проверять то же финальное окно.
	gateNCtx, gateSlots := loadWindowForGate(opts)
	if gateNCtx != opts.ContextSize {
		logger.Get().Warnw("R522: гейт n_ctx проверяет ФИНАЛЬНОЕ окно, а не исходное",
			"name", req.Name,
			"context_size_as_is", opts.ContextSize,
			"context_per_seq", opts.ContextPerSeq,
			"slots", gateSlots,
			"gate_total_n_ctx", gateNCtx)
	}
	if !checkNCtxBeforeLoad(w, req.Name, gateNCtx, opts.KVCacheType, opts.UseMmap) {
		return
	}

	if opts.GPULayers == -2 || (*autoOffload && opts.GPULayers == current.GPULayers) {
		// Создаём временный ModelInfo с запрошенным n_ctx для расчёта.
		m := *current
		m.ContextSize = opts.ContextSize
		var calculated int
		// memfitDecided — решение принято memfit (а не legacy-оценкой): только в
		// этом случае ноль слоёв означает осознанный CPU-only (R83 §9.4 шаг 1).
		memfitDecided := false
		// R83 §9.4 шаг 1б (2026-09-26): вердикт memfit — единая точка решения.
		//
		// ПОЧЕМУ ЗДЕСЬ, А НЕ ТОЛЬКО В ВЕТКЕ globalEnv == nil. До этого шага
		// решение о раскладке принимала strategy (SelectStrategy) со своей
		// формулой, а memfit попадал в игру лишь как fallback без globalEnv.
		// Из-за этого отказ по «не влезает даже в CPU-only» зависел от того,
		// инициализировано ли окружение, и на живом стенде не срабатывал
		// вовсе (см. docs/plans/2026-09-26-step1b-fit-refusal-proposal.md).
		if verdict, ok := memfitPlanForModel(m, opts.KVCacheType); ok {
			// Явный gpuLayers в запросе = «я знаю, что делаю»: не отказываем и
			// не подменяем стадию, оператор сам просил раскладку (§9.4: явное
			// намерение всегда побеждает автоматику). Именно поэтому условие
			// проверяет req.GPULayers, а не профиль: профиль — тоже автоматика.
			explicitGPULayers := req.GPULayers != nil && *req.GPULayers != -2
			if verdict.Stage == memfit.StageDoesNotFit && !explicitGPULayers {
				if forceReload {
					logger.Get().Warnw(i18n.T("cppworker.handlers_model.reload_no_fit_forced"),
						"name", req.Name,
						"requested_n_ctx", m.ContextSize,
						"max_hard_n_ctx", verdict.MaxHardCtx,
						"stage", string(verdict.Stage))
				} else {
					logger.Get().Warnw(i18n.T("cppworker.handlers_model.reload_refused_no_fit"),
						"name", req.Name,
						"requested_n_ctx", m.ContextSize,
						"max_hard_n_ctx", verdict.MaxHardCtx,
						"usable_ram_mb", verdict.UsableRAM.MiB(),
						"usable_vram_mb", verdict.UsableVRAM.MiB(),
						"verdict", verdict.String())
					loadFailures.recordDetailed(req.Name, LoadFailureInsufficientResources,
						fmt.Errorf("%s", verdict.String()), map[string]interface{}{
							"requested_n_ctx":  m.ContextSize,
							"max_viable_n_ctx": verdict.MaxHardCtx,
							"stage":            string(verdict.Stage),
							"usable_ram_mb":    verdict.UsableRAM.MiB(),
							"usable_vram_mb":   verdict.UsableVRAM.MiB(),
						})
					writeInsufficientResourcesResponse(w,
						insErrFromVerdict(req.Name, verdict, m.ContextSize))
					return
				}
			}
			if degradedStageFromVerdict(verdict) != "" {
				degradedStage = degradedStageFromVerdict(verdict)
				degraded := degradedNoticeFromVerdict(req.Name, verdict, m.ContextSize)
				if degraded != nil {
					degradedLoads.record(degraded)
				}
				logger.Get().Warnw(i18n.T("cppworker.handlers_model.reload_cpu_only_degraded"),
					"name", req.Name,
					"requested_n_ctx", m.ContextSize,
					"weights_mb", verdict.Weights.MiB(),
					"usable_ram_mb", verdict.UsableRAM.MiB(),
					"usable_vram_mb", verdict.UsableVRAM.MiB(),
					"verdict", verdict.String())
			}
		}
		// Use adaptive SelectStrategy which tries all kvCacheTypes (f16->q8_0->q4_0)
		// and handles MoE, dynamic overhead, and VRAM/RAM limits.
		if globalEnv != nil {
			env := globalEnv.Get()
			ggufMeta := cppbackend.GGUFModelMeta{
				Architecture: m.Architecture,
				NLayers:      m.NLayers,
				NEmbd:        m.NEmbd,
				NHeads:       m.NHeads,
				NKvHeads:     m.NKvHeads,
				SizeBytes:    int64(m.SizeBytes),
			}
			strategy := SelectStrategy(&env, req.Name, ggufMeta, m.ContextSize, -2, currentConfig)
			// R83 §9.3 (2026-09-26): «не влезает даже в CPU-only» — отказ, а не
			// попытка. Раньше этот случай (Stage="fallback_no_fit") проваливался в
			// ветку ниже и загрузка всё равно стартовала: unload текущей модели,
			// загрузка 16 GB в RAM, таймаут у клиента вместо причины.
			if strategy.Stage == "fallback_no_fit" {
				logger.Get().Warnw(i18n.T("cppworker.handlers_model.reload_refused_no_fit"),
					"name", req.Name,
					"model_size_mb", m.SizeBytes/(1024*1024),
					"available_ram_mb", availableRAMBytes()/(1024*1024),
					"available_vram_mb", availableVRAMBytes()/(1024*1024),
					"requested_n_ctx", m.ContextSize,
					"explanation", strategy.Explanation)
				writeInsufficientResourcesResponse(w, &InsufficientResourcesError{
					Model:              req.Name,
					RequestedNCtx:      m.ContextSize,
					MaxViableNCtx:      strategy.NCtx,
					AvailableVRAMMB:    availableVRAMBytes() / (1024 * 1024),
					AvailableRAMMB:     availableRAMBytes() / (1024 * 1024),
					ModelSizeBytes:     int64(m.SizeBytes),
					GPULayersAttempted: strategy.GPULayers,
				})
				return
			}
			if strategy.GPULayers >= 0 || strategy.Stage == "fallback_no_meta" {
				// For fallback_no_meta (GGUF header not parsed, e.g. gemma4),
				// keep gpuLayers=-2 (AUTO) — backend's checkVRAMForModel will
				// calculate proper layers with the KV-cache fallback we added.
				// Previously this fell through to calculated=0 → gpuLayers=0 (CPU-only),
				// which made inference unusably slow.
				if strategy.Stage == "fallback_no_meta" {
					calculated = -2
				} else {
					calculated = strategy.GPULayers
					// Round 40 #2 (2026-08-18): respect user-explicit kvCacheType
					// (from reload body or profile). Previously this was an
					// unconditional override `opts.KVCacheType = strategy.KVCacheType`
					// which silently discarded user q4_0/q8_0 choices. See
					// reload_kv_cache_override.go for the full precedence contract.
					applyStrategyKVCacheOverride(&opts, strategy, req.Name)
					opts.UseMmap = strategy.UseMmap
				}
				// Round 7: apply MoE override-tensors from strategy. For Qwen3-A3B
				// (and other MoE) this routes routed-expert tensors to CPU, keeping
				// attention on GPU regardless of gpu_layers.
				if len(strategy.OverrideTensors) > 0 && len(strategy.OverrideTensors) == len(strategy.OverrideTensorBufts) {
					opts.OverrideTensors = strategy.OverrideTensors
					opts.OverrideTensorBufts = strategy.OverrideTensorBufts
					logger.Get().Infow("reload: override-tensors from strategy applied",
						"name", req.Name,
						"count", len(strategy.OverrideTensors))
				}
				logger.Get().Infow("reload: adaptive SelectStrategy applied",
					"name", req.Name,
					"stage", strategy.Stage,
					"gpu_layers", strategy.GPULayers,
					"kv_cache_type", strategy.KVCacheType,
					"n_ctx", strategy.NCtx)
			}
		} else {
			calculated = calculateOptimalGPULayersForModel(m, opts.KVCacheType)
			// R83 §9.4 шаг 1 (2026-09-26): memfit может вернуть 0 осознанно —
			// это «веса не влезают в VRAM, раскладка только CPU». Прежняя
			// оценка в этом случае отдавала legacy-число, и условие
			// `calculated > 0` ниже его пропускало; для memfit-ответа ноль
			// обязан быть применён, иначе останется старое (слишком большое)
			// число слоёв и загрузка уйдёт в переподписку.
			if _, ok := memfitGPULayersForModel(m, opts.KVCacheType); ok {
				memfitDecided = true
			}
		}
		if (calculated > 0 || (memfitDecided && calculated >= 0)) && calculated != opts.GPULayers {
			source := "legacy"
			if memfitDecided {
				source = "memfit"
			}
			logger.Get().Infow("reload: auto-offload recalculated gpu_layers",
				"name", req.Name,
				"old_gpu_layers", opts.GPULayers,
				"new_gpu_layers", calculated,
				"n_ctx", opts.ContextSize,
				"model_size_mb", current.SizeBytes/(1024*1024),
				"source", source)
			opts.GPULayers = calculated
			opts.UseMmap = true // partial offload требует mmap
		}
	}

	// === AutoTuneNCtx: если auto_tune_nctx включён — подбираем оптимальные
	// n_ctx и gpu_layers с учётом реальной свободной VRAM и RAM.
	// Это позволяет при reload автоматически сделать partial offload,
	// если запрошенный n_ctx не помещается в VRAM с текущими gpu_layers.
	if *autoTuneNCtx && opts.ContextSize > current.ContextSize {
		tuned := AutoTuneNCtx(*current, opts.ContextSize, "")
		if tuned.RecommendedNCtx > 0 && tuned.Source != "fallback" {
			logger.Get().Infow("reload: AutoTuneNCtx applied",
				"name", req.Name,
				"requested_n_ctx", opts.ContextSize,
				"tuned_n_ctx", tuned.RecommendedNCtx,
				"tuned_gpu_layers", tuned.RecommendedGPULayers,
				"old_gpu_layers", opts.GPULayers,
				"source", tuned.Source,
				"max_viable_n_ctx", tuned.MaxViableNCtx)
			opts.ContextSize = tuned.RecommendedNCtx
			opts.GPULayers = tuned.RecommendedGPULayers
			opts.UseMmap = tuned.UseMmap
		}
	}

	logger.Get().Infow("reloading model with new params",
		"name", req.Name, "path", modelPath,
		"old_ctx", current.ContextSize, "new_ctx", opts.ContextSize,
		"old_batch", current.BatchSize, "new_batch", opts.BatchSize,
		"old_gpu_layers", current.GPULayers, "new_gpu_layers", opts.GPULayers,
		"use_mmap", opts.UseMmap)

	// Если force=true — пропускаем проверку "params already sufficient".
	// Это нужно, когда C-bridge не может реально использовать запрошенный n_ctx
	// (effective n_ctx < GGUF native context_length), и балансировщик шлёт force,
	// чтобы принудительно перезагрузить модель с новыми параметрами.
	// (значение forceReload вычислено выше — там же, где отказ по памяти)

	// R64 (2026-09-15): include KVCacheType in skip-check AND log diagnostics
	// on each skip miss. Pre-R64:
	//   - applyStrategyKVCacheOverride forces opts.KVCacheType = strategy.KVCacheType (f16)
	//     when strategy wants f16 but user profile says q4_0.
	//   - Skip-check compared opts.KVCacheType (f16 after override) vs current.KVCacheType
	//     (q4_0 from previous reload that honored user) — NOT equal — reload loop.
	//
	// Also normalize empty KVCacheType to current.KVCacheType so that "user didn't specify"
	// is treated as "user wants what current has" (inherit semantics).
	if opts.KVCacheType == "" {
		opts.KVCacheType = current.KVCacheType
	}

	skipReason := ""
	if !forceReload {
		switch {
		case !(opts.ContextSize <= current.ContextSize):
			skipReason = fmt.Sprintf("ctx: requested=%d current=%d", opts.ContextSize, current.ContextSize)
		case !(opts.BatchSize <= current.BatchSize):
			skipReason = fmt.Sprintf("batch: requested=%d current=%d", opts.BatchSize, current.BatchSize)
		case !(opts.GPULayers <= current.GPULayers):
			skipReason = fmt.Sprintf("gpuLayers: requested=%d current=%d", opts.GPULayers, current.GPULayers)
		case opts.FlashAttnType != current.FlashAttnType:
			skipReason = fmt.Sprintf("flashAttn: requested=%d current=%d", opts.FlashAttnType, current.FlashAttnType)
		case opts.NUMA != current.NUMA:
			skipReason = fmt.Sprintf("numa: requested=%v current=%v", opts.NUMA, current.NUMA)
		case opts.UseMmap != current.UseMmap:
			skipReason = fmt.Sprintf("useMmap: requested=%v current=%v", opts.UseMmap, current.UseMmap)
		case opts.KVCacheType != current.KVCacheType:
			skipReason = fmt.Sprintf("kvCacheType: requested=%q current=%q", opts.KVCacheType, current.KVCacheType)
		}
	}

	if !forceReload && skipReason == "" {
		logger.Get().Infow("reload: skipping - current params already sufficient",
			"name", req.Name,
			"current_ctx", current.ContextSize, "requested_ctx", opts.ContextSize,
			"current_kv", current.KVCacheType, "requested_kv", opts.KVCacheType,
			"current_useMmap", current.UseMmap, "requested_useMmap", opts.UseMmap)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":              "already_loaded",
			"model":               current,
			"estimatedLoadTimeMs": estimatedMs,
		})
		return
	}
	if !forceReload && skipReason != "" {
		logger.Get().Infow("reload: skip-check FAILED, proceeding with reload",
			"name", req.Name,
			"reason", skipReason,
			"requested_ctx", opts.ContextSize, "current_ctx", current.ContextSize,
			"requested_kv", opts.KVCacheType, "current_kv", current.KVCacheType,
			"requested_useMmap", opts.UseMmap, "current_useMmap", current.UseMmap)
	}

	// R83/v60 (2026-10-01): гейт «модель занята генерацией» ДО выбора
	// async/sync-ветки.
	//
	// ДЕФЕКТ (живая жалоба оператора). Дренаж активных запросов жил только в
	// sync-ветке (inflight.WaitZero(name, 0) — «0 = ждать вечно»), а async-путь
	// (wait=false, default!) уходил в runAsyncReload — где тот же WaitZero(0) ждал
	// бесконечно уже в фоне. Из WebUI это выглядело так: «пока модель не закончит
	// ответ, ей нельзя управлять» — применение профиля (кнопка «Применить
	// (reload)») висело минутами. Отвлечённо: тот же класс, что и у выгрузки,
	// только там cppworker отвечал 409, а здесь молча ждал.
	//
	// Теперь без force отвечаем структурно (как handleUnloadModel), а с force —
	// обрываем генерации этой модели и грузим сразу.
	inflightN := int64(0)
	if inflight := backend.InFlight(); inflight != nil {
		inflightN = inflight.Get(req.Name)
	}
	if inflightN > 0 && !forceReload {
		logger.Get().Warnw("reload refused: model has active requests",
			"name", req.Name, "in_flight", inflightN)
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error":          "model is busy with active inference requests",
			"name":           req.Name,
			"active_queries": inflightN,
			"busy":           true,
			"retryWithForce": true,
			"hint": "wait for in-flight requests to complete, or retry with \"force\": true " +
				"to cancel them and reload now (active clients will be disconnected)",
		})
		return
	}
	if forceReload {
		// Обрываем генерации по модели. Делаем это независимо от счётчика
		// in-flight: запрос мог быть уже снят со счёта, но продолжать держать
		// поток/соединение (например, на этапе финального чанка).
		if gen := backend.ActiveGenerations(); gen != nil {
			if cancelled := gen.CancelByModel("", req.Name); cancelled > 0 {
				logger.Get().Warnw("reload force=true: cancelled active generations",
					"name", req.Name, "cancelled", cancelled)
			}
		}
		if inflightN > 0 {
			logger.Get().Infow(i18n.T("cppworker.handlers_model.reload_wait_drain"),
				"name", req.Name, "in_flight", inflightN)
			if inflight := backend.InFlight(); inflight != nil {
				if !inflight.WaitZero(req.Name, 5*time.Second) {
					logger.Get().Warnw(i18n.T("cppworker.handlers_model.reload_drain_timeout"),
						"name", req.Name, "in_flight", inflight.Get(req.Name))
				}
			}
		}
	}

	// Round 24 (2026-08-04) Bug #1 fix: async reload by default.
	// Если wait=false (default), возвращаем 202 + Location и запускаем
	// реальный reload в background goroutine. WebUI settings UI больше
	// не зависает на 30-60+ сек при reload'е reasoning-модели.
	if !waitSync {
		var sizeBytes int64
		if fi, statErr := os.Stat(modelPath); statErr == nil {
			sizeBytes = fi.Size()
		}
		lm := cppbackend.ModelInfo{
			Name:             req.Name,
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
		// R83 §9.4 шаг 1б (2026-09-26), вариант D: 202 Accepted тоже несёт режим
		// загрузки — клиент узнаёт о деградации сразу, не дожидаясь метрик.
		setDegradedHeader(w, degradedStage)
		writeLoadAccepted(w, r, req.Name, modelPath, sizeBytes, estimatedMs, lm)
		// Background reload: UnloadModel + LoadModelWithOpts + notify.
		// R91: снимок backend берём здесь (в этой же горутине) — фоновая работа не
		// должна читать пакетную переменную, которую тесты подменяют и
		// восстанавливают в cleanup (иначе DATA RACE под -race).
		// Плюс регистрация в asyncWorkWG (см. async_work_tracker.go).
		be := backend
		done := trackAsyncWork()
		go func() {
			defer done()
			runAsyncReload(req.Name, modelPath, opts, current, balancerReg, degradedStage, be)
		}()
		return
	}

	lockOk, lockErr := backend.TryLockReloadR303(req.Name, "reload")
	if lockErr != nil {
		logger.Get().Errorw("reload: TryLockReload returned unexpected error",
			"name", req.Name, "error", lockErr)
		writeError(w, http.StatusInternalServerError, "reload lock error: "+lockErr.Error())
		return
	}

	if !lockOk {
		// R303: ожидание ограничено — вместо бесконечного WaitForLoad клиент
		// получает типизированный отказ с возрастом заявки (см. load_progress.go).
		logger.Get().Infow("reload: another goroutine is already loading this model, waiting",
			"name", req.Name, "wait_limit_sec", loadWaitMaxR303().Seconds())
		loaded, timedOut := waitLoadBoundedR303(req.Name)
		if timedOut {
			writeLoadInProgressR303(w, req.Name, loadWaitMaxR303())
			return
		}
		if loaded {
			// R303.1: та же проверка, что в async-ветке: «reloaded_by_other» — это
			// ТОЛЬКО StateLoaded, иначе клиент получает 200 при незавершённой
			// загрузке (живой дефект, найденный на стенде).
			if model, getErr := backend.GetModel(req.Name); getErr == nil && model != nil && model.State == cppbackend.StateLoaded {
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status": "reloaded_by_other", "model": model,
				})
				return
			}
		}
		writeLoadingResponse(w, req.Name, errModelIsLoading)
		return
	}
	defer backend.UnlockLoad(req.Name)

	// Round 24 (2026-08-04): sync reload (?wait=true) — bound by waitTimeoutMs.
	// Если не успели за waitTimeoutMs — возвращаем 202 + Location, reload продолжится
	// в background (defer UnlockLoad разблокирует после настоящего завершения).
	_ = waitTimeoutMs // explicitly noted: sync path keeps existing blocking behavior
	// для обратной совместимости. CGo не отменяется, поэтому таймаут в этой
	// ветке не режет load — он лишь говорит клиенту "я устал ждать, полли сам".
	// Future enhancement: добавить goroutine + select, как в handleLoadModel.

	// 2026-06-24: graceful reload без обрыва активных запросов.
	//
	// R83/v60 (2026-10-01): сам дренаж перенесён ВЫШЕ — до выбора async/sync-ветки
	// (см. «гейт модель занята генерацией»): иначе async-путь (default) уходил в
	// runAsyncReload и ждал освобождения модели бесконечно, а оператор в WebUI
	// видел только бесконечный прогресс. Сюда доходим уже с дренированными
	// запросами (force=true ждёт не дольше 5s).

	// Устанавливаем reload_pending heartbeat — балансировщик читает это
	// поле из /api/metrics и НЕ пытается дёргать LoadModel API, пока
	// reload не завершён. Без этого — race condition: балансировщик
	// polling'ом обнаруживает state=loading, делает LoadModel, и оба
	// потока упираются в TryLockLoad друг друга.
	backend.SetReloadPending(req.Name)
	defer backend.SetReloadPending("")

	unloadStart := time.Now()
	if err := backend.UnloadModel(req.Name); err != nil {
		writeError(w, http.StatusInternalServerError, "unload failed: "+err.Error())
		return
	}
	logger.Get().Infow("reload: model unloaded",
		"name", req.Name, "unload_ms", time.Since(unloadStart).Milliseconds())

	loadStart := time.Now()
	// R60.57: r.Context() — HTTP client disconnect → ctx.Done() → abort load.
	//
	// R83-fix (2026-09-29): reload — тоже разделяемая операция (его запускает
	// балансер по preflight/auto-reload, а не конечный клиент), поэтому его
	// контекст отвязан от HTTP-запроса: иначе таймаут балансера на ожидании
	// reload'а обрывал перезагрузку и оставлял модель выгруженной — это и
	// выглядело как «модель не загрузить». См. loadContextForSharedLoad.
	reloadCtx, cancelReload := loadContextForSharedLoad()
	defer cancelReload()
	if err := backend.LoadModelWithOpts(reloadCtx, req.Name, modelPath, opts); err != nil {
		logger.Get().Errorw("reload: load with new params failed",
			"name", req.Name, "error", err)
		oldOpts := cppbackend.LoadModelOpts{
			GPULayers:     current.GPULayers,
			ContextSize:   current.ContextSize,
			BatchSize:     current.BatchSize,
			FlashAttnType: current.FlashAttnType,
			NUMA:          current.NUMA,
			UseMmap:       current.UseMmap,
			TensorSplit:   current.TensorSplit,
		}
		// Rollback тоже не должен зависеть от ушедшего HTTP-клиента.
		if rollbackErr := backend.LoadModelWithOpts(reloadCtx, req.Name, modelPath, oldOpts); rollbackErr != nil {
			logger.Get().Errorw("reload rollback failed (model is no longer loaded!)",
				"name", req.Name, "rollback_error", rollbackErr)
		}
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	logger.Get().Infow("reload: model loaded with new params",
		"name", req.Name, "load_ms", time.Since(loadStart).Milliseconds())

	model, err := backend.GetModel(req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// R83 §9.4 шаг 1б (2026-09-26): сообщаем клиенту режим загрузки. Заголовок
	// обязан быть выставлен ДО writeJSON — после WriteHeader он теряется молча.
	setDegradedHeader(w, degradedStage)
	// Синхронный reload тоже обновляет регистр: запись о деградации должна
	// появляться и исчезать одинаково во всех путях (иначе /api/models соврёт).
	noteLoadDegradation(req.Name, opts, backend)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "reloaded", "name": req.Name,
		"contextSize": opts.ContextSize, "batchSize": opts.BatchSize,
		"gpuLayers": opts.GPULayers, "flashAttnType": opts.FlashAttnType,
		"reloadDurationMs": time.Since(unloadStart).Milliseconds(),
		"model":            model,
		"degraded":         degradedStage,
	})
}

// ============================================================
// Ollama-compatible model info & management handlers
// ============================================================