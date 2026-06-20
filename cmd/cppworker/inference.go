// inference.go — Inference core, token counting, RAM fallback, and reload helpers.
package main

import (
	"fmt"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// Token counting
// ============================================================

func countTokens(modelName, text string) int {
	if text == "" {
		return 0
	}
	return backend.CountTokens(modelName, text)
}

// countModelTokensByLoadedInfo — если модель ещё не загружена, не пытаться
// загружать её ради подсчёта токенов; вернуть грубую оценку.
func countModelTokensByLoadedInfo(modelName, text string) int {
	if text == "" {
		return 0
	}
	if _, err := backend.GetModel(modelName); err != nil {
		return len([]rune(text)) / 4
	}
	return backend.CountTokens(modelName, text)
}

// ============================================================
// Load options comparison
// ============================================================

// sameLoadOptions сравнивает параметры загруженной модели с запрошенными
// параметрами загрузки. Используется handleLoadModel для защиты от
// повторной загрузки модели с теми же параметрами.
func sameLoadOptions(info cppbackend.ModelInfo, opts cppbackend.LoadModelOpts) bool {
	if info.ContextSize != opts.ContextSize {
		return false
	}
	if info.BatchSize != opts.BatchSize {
		return false
	}
	if info.GPULayers != opts.GPULayers {
		return false
	}
	if info.FlashAttnType != opts.FlashAttnType {
		return false
	}
	if info.NUMA != opts.NUMA {
		return false
	}
	if info.UseMmap != opts.UseMmap {
		return false
	}
	if len(info.TensorSplit) != len(opts.TensorSplit) {
		return false
	}
	for i := range info.TensorSplit {
		if info.TensorSplit[i] != opts.TensorSplit[i] {
			return false
		}
	}
	return true
}

// ============================================================
// C-bridge error classification
// ============================================================

// isNCtxNeedsReload — true, если последняя ошибка C-bridge говорит
// "requested n_ctx exceeds model's effective n_ctx" (code 2).
func isNCtxNeedsReload() bool {
	info := bridge.GetLastErrorInfo()
	return info != nil && info.Code == bridge.ErrCodeNCtxNeedsReload
}

// isGpuOomOrNCtxNeedsReload — true, если последняя ошибка C-bridge
// требует перезагрузки модели с другими параметрами: либо n_ctx
// недостаточен (code 2), либо GPU OOM (code 4). В обоих случаях
// RAM fallback может помочь, снизив GPU-слои и/или увеличив n_ctx.
func isGpuOomOrNCtxNeedsReload() bool {
	info := bridge.GetLastErrorInfo()
	if info == nil {
		return false
	}
	return info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodeGPUOOM
}

// reloadInProgress tracks models currently being reloaded by RAM fallback.
// The value is a chan struct{} that is closed when the reload (including any
// rollback) completes. Concurrent requests that encounter "model not loaded"
// during this window wait on this channel and retry instead of returning
// connection reset to the client.
var reloadInProgress sync.Map

// waitReloadInProgress — если для модели modelName сейчас выполняется
// RAM fallback reload (reloadInProgress flag установлен), ждёт его
// завершения. Возвращает true, если дождались; false если канал не найден.
func waitReloadInProgress(modelName string) bool {
	chRaw, ok := reloadInProgress.Load(modelName)
	if !ok {
		return false
	}
	ch, ok := chRaw.(chan struct{})
	if !ok {
		return false
	}
	// Ждём завершения перезагрузки (канал закроется в defer)
	<-ch
	return true
}

// ============================================================
// RAM fallback
// ============================================================

// generateWithRamFallback пытается выполнить backend.Generate; если
// получает ErrCodeNCtxNeedsReload или ErrCodeGPUOOM и ram-fallback включён —
// перезагружает модель с запрошенным n_ctx через mmap/RAM и повторяет генерацию.
// Используется для non-streaming эндпоинтов.
// Если concurrent-запрос попадает на модель, которая сейчас перезагружается
// (reloadInProgress), он ждёт завершения reload и повторяет попытку вместо
// возврата "model not loaded" клиенту.
func generateWithRamFallback(modelName, prompt string, params bridge.GenerationParams) (*bridge.InferenceResult, error) {
	result, err := backend.Generate(modelName, prompt, params)
	if err == nil {
		return result, nil
	}
	// Если reload уже идёт — ждём и повторяем
	if waitReloadInProgress(modelName) {
		logger.Get().Debugw("RAM fallback: waiting for concurrent reload to complete before retry",
			"model", modelName)
		return backend.Generate(modelName, prompt, params)
	}
	if !isGpuOomOrNCtxNeedsReload() || params.NCtxOverride <= 0 {
		return result, err
	}
	if ok, fbErr := tryRamFallbackReload(modelName, params.NCtxOverride); !ok {
		return result, err // возвращаем исходную ошибку; fbErr только логируем
	} else if fbErr != nil {
		logger.Get().Warnw("RAM fallback declined", "model", modelName, "error", fbErr)
		return result, err
	}
	return backend.Generate(modelName, prompt, params)
}

// generateStreamWithRamFallback — аналог generateWithRamFallback для streaming.
// При ErrCodeNCtxNeedsReload или ErrCodeGPUOOM на старте (pre-flight) перезагружает
// модель и запускает стрим заново. Если стрим уже частично начался, fallback не
// применяется (вернётся текущая ошибка).
func generateStreamWithRamFallback(modelName, prompt string, params bridge.GenerationParams, callback bridge.StreamCallback) error {
	err := backend.GenerateStream(modelName, prompt, params, callback)
	if err == nil {
		return nil
	}
	// Если reload уже идёт — ждём и повторяем
	if waitReloadInProgress(modelName) {
		logger.Get().Debugw("RAM fallback: waiting for concurrent reload to complete before stream retry",
			"model", modelName)
		return backend.GenerateStream(modelName, prompt, params, callback)
	}
	if !isGpuOomOrNCtxNeedsReload() || params.NCtxOverride <= 0 {
		return err
	}
	if ok, fbErr := tryRamFallbackReload(modelName, params.NCtxOverride); !ok {
		return err
	} else if fbErr != nil {
		logger.Get().Warnw("RAM fallback declined", "model", modelName, "error", fbErr)
		return err
	}
	return backend.GenerateStream(modelName, prompt, params, callback)
}

// effectiveRamFallbackGPULayers возвращает целевое число GPU-слоёв для
// RAM fallback: если пользователь явно задал ram-fallback-gpu-layers >= 0,
// используем его; иначе сохраняем текущее значение (оставляем llama.cpp
// решать, но mmap позволит вытеснить часть в RAM при нехватке VRAM).
func effectiveRamFallbackGPULayers(current int) int {
	if *ramFallbackGpuLayers >= 0 {
		return *ramFallbackGpuLayers
	}
	return current
}

// tryRamFallbackReload пытается перезагрузить модель с запрошенным n_ctx,
// используя RAM через mmap, если VRAM недостаточна. Вызывается только
// после ErrCodeNCtxNeedsReload и только если ram-fallback-n-ctx включён.
// Возвращает (ok=true, nil) если модель успешно перезагружена.
func tryRamFallbackReload(modelName string, requestedNCtx int) (bool, error) {
	if !*ramFallbackNCtx {
		return false, nil
	}
	if requestedNCtx <= 0 {
		return false, fmt.Errorf("requested n_ctx must be > 0 for RAM fallback")
	}
	if *ramFallbackMaxNCtx > 0 && requestedNCtx > *ramFallbackMaxNCtx {
		return false, fmt.Errorf("requested n_ctx=%d exceeds ram-fallback-max-n-ctx=%d", requestedNCtx, *ramFallbackMaxNCtx)
	}

	current, err := backend.GetModel(modelName)
	if err != nil {
		return false, fmt.Errorf("cannot get current model info: %w", err)
	}
	modelPath := current.Path
	if modelPath == "" {
		mm := backend.ModelManager()
		if mm != nil {
			foundPath, ferr := mm.FindModelByPath(modelName)
			if ferr == nil {
				modelPath = foundPath
			}
		}
	}
	if modelPath == "" {
		return false, fmt.Errorf("cannot resolve model path for %s", modelName)
	}

	newGPULayers := effectiveRamFallbackGPULayers(current.GPULayers)
	opts := cppbackend.LoadModelOpts{
		GPULayers:     newGPULayers,
		ContextSize:   requestedNCtx,
		BatchSize:     current.BatchSize,
		FlashAttnType: current.FlashAttnType,
		NUMA:          current.NUMA,
		UseMmap:       true, // RAM fallback через mmap
		TensorSplit:   current.TensorSplit,
	}

	lockOk, lockErr := backend.TryLockLoad(modelName)
	if lockErr != nil {
		// модель уже загружена — RAM fallback тривиально успешен
		return true, nil
	}
	if !lockOk {
		// другая горутина грузит эту модель — ждём
		if backend.WaitForLoad(modelName) {
			return true, nil
		}
		return false, fmt.Errorf("RAM fallback: model is being loaded by another request, but wait failed")
	}
	defer backend.UnlockLoad(modelName)

	// Устанавливаем сигнал reloadInProgress, чтобы concurrent-запросы
	// ждали завершения перезагрузки вместо получения "model not loaded".
	// Канал закрывается в defer после успешной загрузки или rollback'а.
	reloadCh := make(chan struct{})
	reloadInProgress.Store(modelName, reloadCh)
	defer close(reloadCh)
	defer reloadInProgress.Delete(modelName)

	logger.Get().Infow("RAM fallback: reloading model with larger n_ctx",
		"model", modelName,
		"path", modelPath,
		"old_n_ctx", current.ContextSize,
		"new_n_ctx", requestedNCtx,
		"old_gpu_layers", current.GPULayers,
		"new_gpu_layers", newGPULayers,
		"use_mmap", true)

	unloadStart := time.Now()
	if err := backend.UnloadModel(modelName); err != nil {
		return false, fmt.Errorf("RAM fallback unload failed: %w", err)
	}
	logger.Get().Infow("RAM fallback: model unloaded",
		"model", modelName, "unload_ms", time.Since(unloadStart).Milliseconds())

	loadStart := time.Now()
	if err := backend.LoadModelWithOpts(modelName, modelPath, opts); err != nil {
		logger.Get().Errorw("RAM fallback: reload failed",
			"model", modelName, "error", err)
		// best-effort rollback к старым параметрам
		oldOpts := cppbackend.LoadModelOpts{
			GPULayers:     current.GPULayers,
			ContextSize:   current.ContextSize,
			BatchSize:     current.BatchSize,
			FlashAttnType: current.FlashAttnType,
			NUMA:          current.NUMA,
			UseMmap:       current.UseMmap,
			TensorSplit:   current.TensorSplit,
		}
		if rollbackErr := backend.LoadModelWithOpts(modelName, modelPath, oldOpts); rollbackErr != nil {
			logger.Get().Errorw("RAM fallback: rollback failed (model no longer loaded!)",
				"model", modelName, "rollback_error", rollbackErr)
		}
		return false, fmt.Errorf("RAM fallback reload failed: %w", err)
	}
	logger.Get().Infow("RAM fallback: model reloaded successfully",
		"model", modelName,
		"new_n_ctx", requestedNCtx,
		"gpu_layers", newGPULayers,
		"load_ms", time.Since(loadStart).Milliseconds())

	if balancerReg != nil {
		if info, err := backend.GetModel(modelName); err == nil {
			balancerReg.notifyModelLoaded(modelName, info.SizeBytes, info.ContextSize, info.GPULayers)
		}
	}
	return true, nil
}
