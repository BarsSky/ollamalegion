// lazyload.go — Lazy model loading, loading progress response, and auto-load on startup.
package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

		opts := cppbackend.LoadModelOpts{
			GPULayers:     *gpuLayers,
			ContextSize:   *ctxSize,
			BatchSize:     *batchSize,
			FlashAttnType: convertFlashAttn(*flashAttn),
			NUMA:          *numa,
			UseMmap:       !*noMmap,
		}

		if err := backend.LoadModelWithOpts(modelName, modelPath, opts); err != nil {
			log.Errorw("lazy-load failed", "model", modelName, "path", modelPath, "error", err)
			return fmt.Errorf("failed to load model %s: %w", modelName, err)
		}

		log.Infow("lazy-load successful", "model", modelName, "path", modelPath)
		return nil
	}

	return fmt.Errorf("model %s not found in filesystem", modelName)
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
