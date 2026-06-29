// lazyload.go — Lazy model loading, loading progress response, and auto-load on startup.
package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// ensureModelLoaded — ленивая загрузка модели из файловой системы если она ещё не в памяти.
//
// Возвращаемые ошибки:
//   - nil: модель уже загружена или успешно загружена сейчас
//   - errModelIsLoading: другая горутина уже грузит эту модель.
//     Хендлеры перехватывают эту ошибку и отвечают 503 с retryAfterMs=3000
//   - прочие: ошибка загрузки (validation, FS, C-bridge)
func ensureModelLoaded(modelName string) error {
	// 0) Если модель уже загружена — сразу выходим.
	if _, err := backend.GetModel(modelName); err == nil {
		return nil
	}

	// 1) Используем TryLockLoad для постановки в очередь загрузки.
	lockOk, lockErr := backend.TryLockLoad(modelName)
	if lockErr != nil {
		// Модель уже загружена (успели загрузить между нашей проверкой и TryLockLoad).
		return nil
	}
	if !lockOk {
		// Другая горутина уже грузит эту модель — ждём завершения.
		if backend.WaitForLoad(modelName) {
			return nil
		}
		return errModelIsLoading
	}

	// lockOk == true — мы отвечаем за загрузку.
	defer backend.UnlockLoad(modelName)

	log := logger.Get()
	loadStart := time.Now()
	log.Infow("lazy-loading model from filesystem", "model", modelName)

	// Ищем модель в файловой системе
	mm := backend.ModelManager()
	if mm != nil {
		modelPath, err := mm.FindModelByPath(modelName)
		if err != nil {
			// Fallback: конструируем путь из modelsDir
			modelPath = filepath.Join(*modelsDir, modelName)
			if !strings.HasSuffix(modelPath, ".gguf") {
				matches, globErr := filepath.Glob(modelPath + "*.gguf")
				if globErr == nil && len(matches) > 0 {
					modelPath = matches[0]
				} else {
					modelPath += ".gguf"
				}
			}
		}

		// === Auto-tune на LOAD (2026-06-26 BUGFIX) ===
		//
		// До этой правки lazy-load шёл с грубыми хардкодами (estimatedLayers=80,
		// kvReserve=2GB) и НЕ пересчитывал n_ctx. Для qwen3.6 (~22GB) на 20GB VRAM
		// это приводило к OOM: weights + KV-cache для n_ctx=32768 не влезают,
		// пользователь получает либо молчаливый fallback на n_ctx=4096,
		// либо ошибку. На 8GB VRAM работало, потому что модель целиком уходила
		// в mmap (gpu_layers=0) и n_ctx оставался 32768.
		//
		// Новая логика:
		//   1. Читает реальные NLayers/NEmbd/NHeads из GGUF header
		//      (cppbackend.ReadGGUFHeader через lazy ModelManager.GetModelMeta).
		//   2. Считает maxViableNCtx по реальной архитектуре.
		//   3. Stage 1: если requestedNCtx + gpuLayers влезают → exact_fit.
		//   4. Stage 2: уменьшаем gpuLayers (partial offload) — то же n_ctx.
		//   5. Stage 3: gpu_layers=0 + n_ctx = max_viable (всё через mmap).
		//
		// Отключается через CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD=false.
		tunedOpts, rationale := calculateLazyLoadOpts(
			modelName,
			*ctxSize,
			*gpuLayers,
			currentConfig,
		)
		opts := tunedOpts
		opts.FlashAttnType = convertFlashAttn(*flashAttn)
		opts.NUMA = *numa

		logger.Get().Infow("lazy-load: load opts calculated",
			"model", modelName,
			"rationale", rationale.FormatRationale())

		// Legacy auto-offload остался для обратной совместимости: если
		// CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD=false, старая логика всё равно
		// отрабатывает (estimatedLayers=80, kvReserve=2GB).
		// Legacy auto-offload branch: срабатывает ТОЛЬКО если calculateLazyLoadOpts
		// не смог прочитать GGUF header (rationale.Source == "fallback_no_meta").
		// Используем тот же safetyFactor, что в calculateLazyLoadOpts, чтобы
		// поведение было консистентным.
		if *autoOffload && rationale.Source == "fallback_no_meta" {
			availableVRAM := availableVRAMBytes()
			availableRAM := availableRAMBytes()
			if availableVRAM > 0 {
				filename := modelName + ".gguf"
				if meta, mErr := mm.GetModelMeta(filename); mErr == nil && meta.SizeBytes > 0 {
					safeVRAM := int64(float64(availableVRAM) * nctxSafetyFactor)
					overhead := int64(1536) * 1024 * 1024 // 1.5GB CUDA + activations
					// Резерв под KV-cache: реалистичный расчёт если есть NLayers,
					// иначе грубый 2GB fallback.
					var kvReserve int64
					if meta.NLayers > 0 && meta.NEmbd > 0 {
						kvReserve = estimateKVCacheBytes(*ctxSize, meta.NLayers, meta.NEmbd, meta.NHeads, meta.NKvHeads)
					} else {
						kvReserve = int64(2) * 1024 * 1024 * 1024 // 2 GB legacy fallback
					}
					availableForWeights := safeVRAM - overhead - kvReserve
					if availableForWeights > 0 && meta.SizeBytes > availableForWeights {
						// Модель не влезает целиком — partial offload.
						// Используем РЕАЛЬНОЕ число слоёв из GGUF (если есть),
						// иначе fallback 80 (типичная крупная модель).
						estimatedLayers := 80 // legacy fallback
						if meta.NLayers > 0 {
							estimatedLayers = meta.NLayers
						}
						weightsPerLayer := meta.SizeBytes / int64(estimatedLayers)
						if weightsPerLayer > 0 {
							gpuLayers := int(availableForWeights / weightsPerLayer)
							if gpuLayers > 0 && gpuLayers < estimatedLayers {
								// Дополнительно проверяем, что веса для оставшихся слоёв
								// влезают в RAM (mmap).
								requiredRAM := int64(estimatedLayers-gpuLayers) * weightsPerLayer
								if availableRAM > 0 && requiredRAM > availableRAM {
									// RAM не хватает — пропускаем partial offload,
									// пусть AutoTuneNCtx разберётся при reload.
									logger.Get().Warnw("lazy-load: auto-offload skipped (weights don't fit in RAM)",
										"model", modelName,
										"required_ram_mb", requiredRAM/(1024*1024),
										"available_ram_mb", availableRAM/(1024*1024))
								} else {
									logger.Get().Infow("lazy-load: auto-offload (legacy, file-based estimate)",
										"model", modelName,
										"old_gpu_layers", opts.GPULayers,
										"new_gpu_layers", gpuLayers,
										"model_size_mb", meta.SizeBytes/(1024*1024),
										"available_for_weights_mb", availableForWeights/(1024*1024),
										"estimated_layers", estimatedLayers,
										"vram_total_mb", availableVRAM/(1024*1024))
									opts.GPULayers = gpuLayers
									opts.UseMmap = true
								}
							}
						}
					}
				}
			}
		}

		// Записываем попытку в диагностический буфер (для /api/diagnostics).
		attempt := LoadAttempt{
			Timestamp: loadStart,
			Model:     modelName,
			Path:      modelPath,
			Stage:     "loading",
		}

		if err := backend.LoadModelWithOpts(modelName, modelPath, opts); err != nil {
			log.Errorw("lazy-load failed", "model", modelName, "path", modelPath, "error", err)
			attempt.Stage = "load_failed"
			attempt.Success = false
			attempt.Error = err.Error()
			attempt.DurationMs = time.Since(loadStart).Milliseconds()
			attempt.Diagnostics = map[string]interface{}{
				"modelPath":   modelPath,
				"gpuLayers":   opts.GPULayers,
				"ctxSize":     opts.ContextSize,
				"batchSize":   opts.BatchSize,
				"useMmap":     opts.UseMmap,
				"modelsDir":   derefString(modelsDir),
				"vramTotalMB": backendGPUVRAMTotal(),
				"vramFreeMB":  backendGPUVRAMFree(),
				// 2026-06-29: добавлено для диагностики n_ctx auto-tune
				// (источник решения, % reduction и max_viable).
				"source":             rationale.Source,
				"max_viable_n_ctx":   rationale.MaxViableNCtx,
				"n_ctx_reduction_pct": rationale.NCtxReductionPct,
				"available_vram_mb":  rationale.AvailableVRAMBytes / (1024 * 1024),
				"available_ram_mb":   rationale.AvailableRAMBytes / (1024 * 1024),
			}
			RecordLoadAttempt(attempt)
			return fmt.Errorf("failed to load model %s: %w", modelName, err)
		}

		log.Infow("lazy-load successful", "model", modelName, "path", modelPath,
			"durationMs", time.Since(loadStart).Milliseconds())
		attempt.Stage = "complete"
		attempt.Success = true
		attempt.DurationMs = time.Since(loadStart).Milliseconds()
		attempt.Diagnostics = map[string]interface{}{
			"modelPath":          modelPath,
			"gpuLayers":          opts.GPULayers,
			"ctxSize":            opts.ContextSize,
			"useMmap":            opts.UseMmap,
			"source":             rationale.Source,
			"max_viable_n_ctx":   rationale.MaxViableNCtx,
			"n_ctx_reduction_pct": rationale.NCtxReductionPct,
			"available_vram_mb":  rationale.AvailableVRAMBytes / (1024 * 1024),
			"available_ram_mb":   rationale.AvailableRAMBytes / (1024 * 1024),
		}
		RecordLoadAttempt(attempt)
		return nil
	}

	// mm == nil — ModelManager не инициализирован.
	attempt := LoadAttempt{
		Timestamp: loadStart,
		Model:     modelName,
		Stage:     "model_manager_nil",
		Success:   false,
		Error:     "model manager not initialized",
	}
	RecordLoadAttempt(attempt)
	return fmt.Errorf("model %s not found in filesystem", modelName)
}

// backendGPUVRAMTotal возвращает суммарную VRAM по всем GPU (для диагностики).
func backendGPUVRAMTotal() uint64 {
	if backend == nil {
		return 0
	}
	var total uint64
	for _, d := range backend.GetGPUDevices() {
		total += d.VRAMTotalMB
	}
	return total
}

// backendGPUVRAMFree возвращает суммарную свободную VRAM по всем GPU.
func backendGPUVRAMFree() uint64 {
	if backend == nil {
		return 0
	}
	var free uint64
	for _, d := range backend.GetGPUDevices() {
		free += d.VRAMFreeMB
	}
	return free
}

// writeLoadingResponse — хелпер: отвечает 503 Service Unavailable с JSON
// {"error":"model is loading","loading":true,"retryAfterMs":3000,"elapsedMs":N}.
// Используется всеми хендлерами, которые вызвали ensureModelLoaded и получили
// errModelIsLoading.
func writeLoadingResponse(w http.ResponseWriter, modelName string, loadErr error) {
	elapsedMs, retryAfter := loadingInfoFor(loadErr)
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter/1000))
	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		"error":        "model is loading: " + modelName,
		"loading":      true,
		"model":        modelName,
		"elapsedMs":    elapsedMs,
		"retryAfterMs": retryAfter,
	})
}

// autoLoadModels сканирует modelsDir и загружает все найденные .gguf файлы при старте.
func autoLoadModels(cfg cppbackend.Config) {
	log := logger.Get()
	modelsDir := cfg.ModelsDir

	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		log.Warnw("auto-load: failed to read models directory", "dir", modelsDir, "error", err)
		return
	}

	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}
		modelPath := filepath.Join(modelsDir, name)
		modelName := strings.TrimSuffix(name, ".gguf")

		opts := cppbackend.LoadModelOpts{
			GPULayers:     cfg.DefaultGPULayers,
			ContextSize:   cfg.DefaultCtxSize,
			BatchSize:     cfg.DefaultBatchSize,
			FlashAttnType: cfg.DefaultFlashAttnType,
			NUMA:          cfg.DefaultNUMA,
			UseMmap:       cfg.DefaultUseMmap,
		}

		log.Infow("auto-loading model",
			"name", modelName,
			"path", modelPath,
			"gpuLayers", opts.GPULayers)

		lockOk, lockErr := backend.TryLockLoad(modelName)
		if lockErr != nil {
			log.Infow("auto-load: model already loaded", "name", modelName)
			loaded++
			continue
		}

		if !lockOk {
			log.Infow("auto-load: another goroutine is loading this model, waiting",
				"name", modelName)
			if backend.WaitForLoad(modelName) {
				loaded++
				log.Infow("auto-load: model loaded by another goroutine", "name", modelName)
			} else {
				log.Warnw("auto-load: wait for model failed, skipping", "name", modelName)
			}
			continue
		}

		if err := backend.LoadModelWithOpts(modelName, modelPath, opts); err != nil {
			backend.UnlockLoad(modelName)
			log.Errorw("auto-load: failed to load model",
				"name", modelName, "path", modelPath, "error", err)
			continue
		}
		backend.UnlockLoad(modelName)
		loaded++
		log.Infow("auto-loaded model successfully", "name", modelName)
	}

	if loaded > 0 {
		log.Infow("auto-load complete", "loaded", loaded, "dir", modelsDir)
	} else {
		log.Infow("auto-load: no .gguf files found", "dir", modelsDir)
	}
}