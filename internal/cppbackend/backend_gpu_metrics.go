package cppbackend

import (
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
	"time"
)

// GetGPUCount возвращает количество GPU
func (b *Backend) GetGPUCount() int {
	return b.gpuCount
}

// GetGPUDevices возвращает информацию о всех GPU
func (b *Backend) GetGPUDevices() []bridge.GPUDevice {
	result := make([]bridge.GPUDevice, len(b.gpuDevices))
	copy(result, b.gpuDevices)
	return result
}

// VramFreeSourceName — R83 (2026-09-29): человекочитаемый источник значения
// VRAMFreeMB. Нужен оператору, чтобы отличить реальный ноль от «не смогли
// измерить»: раньше при ошибке замера отдавался весь объём как свободный.
func VramFreeSourceName(source int) string {
	switch source {
	case 0:
		return "cudaMemGetInfo" // runtime API — самый точный
	case 1:
		return "cuMemGetInfo" // driver API (контекст уже существовал)
	case 2:
		return "unknown" // не измерено: vramFreeMB=0 значит НЕИЗВЕСТНО, а не «ноль свободно»
	default:
		return "unknown"
	}
}

// GetGPUMetrics возвращает метрики использования GPU
func (b *Backend) GetGPUMetrics() []map[string]interface{} {
	result := make([]map[string]interface{}, 0, b.gpuCount)
	for _, dev := range b.gpuDevices {
		metrics := map[string]interface{}{
			"index":        dev.Index,
			"name":         dev.Name,
			"vramTotalMB":  dev.VRAMTotalMB,
			"vramFreeMB":   dev.VRAMFreeMB,
			"vramUsedMB":   dev.VRAMTotalMB - dev.VRAMFreeMB,
			"vramUsagePct": float64(0),
			// R83 (2026-09-29): чем измерена свободная VRAM. Оператор должен
			// отличать «свободно 0» от «не смогли спросить»: раньше при ошибке
			// замера отдавался весь объём как свободный, планировщик считал 8 ГБ
			// доступными, и загрузка падала с cudaMalloc OOM. Значение "unknown"
			// означает: VRAMFreeMB=0 — это НЕ «ноль свободно», а «неизвестно».
			"vramFreeSource": VramFreeSourceName(dev.VRAMFreeSource),
		}
		if dev.VRAMTotalMB > 0 {
			metrics["vramUsagePct"] = float64(dev.VRAMTotalMB-dev.VRAMFreeMB) / float64(dev.VRAMTotalMB) * 100
		}
		result = append(result, metrics)
	}
	return result
}

// ============================================================
// Метрики Backend
// ============================================================

// GetMetrics возвращает общие метрики backend
func (b *Backend) GetMetrics() map[string]interface{} {
	b.mu.RLock()
	defer b.mu.RUnlock()

	totalModels := len(b.models)
	loadedCount := 0
	totalQueries := int64(0)
	activeQueries := 0

	for _, inst := range b.models {
		if inst.info.State == StateLoaded {
			loadedCount++
		}
		totalQueries += inst.totalQueriesCount()
		activeQueries += b.getActiveQueries(inst)
	}

	result := map[string]interface{}{
		"version":       bridge.Version(),
		"uptimeSeconds": time.Since(b.startTime).Seconds(),
		"gpuCount":      b.gpuCount,
		"totalModels":   totalModels,
		"loadedModels":  loadedCount,
		"totalQueries":  totalQueries,
		"activeQueries": activeQueries,
	}

	// Добавляем метрики из Metrics если доступны
	if b.metrics != nil {
		ms := b.metrics.GetMetricsSnapshot()
		for k, v := range ms {
			result[k] = v
		}
	}

	return result
}

// Status возвращает статус backend
func (b *Backend) Status() map[string]interface{} {
	metrics := b.GetMetrics()
	metrics["gpuDevices"] = b.GetGPUMetrics()
	metrics["models"] = b.ListModels()
	return metrics
}

// Close завершает работу backend, выгружая все модели
func (b *Backend) Close() {
	// R65e (2026-09-20): сначала дожидаемся завершения отложенных записей
	// nameHistory. Без этого фоновый persistLoop мог дописать
	// <ModelsDir>/.name_history.json УЖЕ ПОСЛЕ Close() — и teardown каталога
	// моделей падал с "directory is not empty" (Windows), а в production
	// последняя запись могла потеряться при рестарте.
	if mm := b.modelManager; mm != nil {
		if !mm.WaitForPendingWrites(5 * time.Second) {
			logger.Get().Warnw("CppBackend.Close: pending nameHistory writes did not finish",
				"timeout", "5s")
		}
	}

	// Останавливаем idle unloader
	if b.idleUnloader != nil {
		b.idleUnloader.Stop()
	}

	// R114 (2026-10-13): останавливаем наблюдение за узлами раскладки — при
	// завершении процесса выгружать модели «по потере узла» нечего.
	b.rpcWatchMu.Lock()
	for model, wd := range b.rpcWatchdogs {
		if wd != nil {
			close(wd.stop)
		}
		delete(b.rpcWatchdogs, model)
	}
	b.rpcWatchMu.Unlock()

	// Останавливаем HuggingFace загрузчик
	if b.hfDownloader != nil {
		b.hfDownloader.Close()
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	logger.Get().Infow("shutting down CppBackend",
		"modelsToUnload", len(b.models))

	for name, inst := range b.models {
		inst.mu.Lock()
		// R714 (D2): тот же порядок, что в unloadModel — освобождение словаря
		// под tokenMu на запись (см. modelInstance.tokenMu).
		inst.tokenMu.Lock()
		if inst.handle != nil {
			inst.handle.FreeModel()
		}
		inst.info.State = StateUnloaded
		inst.tokenMu.Unlock()
		inst.mu.Unlock()
		delete(b.models, name)
		logger.Get().Infow("model unloaded on shutdown", "name", name)
	}
}

// ============================================================
// Внутренние методы
// ============================================================
