// Package balancer — MetricsManager (Шаг «отображение загрузки в мониторе»).
//
// UpdateLlamaCppModelLoaded и UpdateLlamaCppLoadingModels — методы,
// которые cppworker вызывает через callback'и /api/v1/internal/* для
// синхронизации состояния загрузки моделей между cppworker'ом и балансировщиком.
package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// MetricsManager - менеджер метрик бэкендов
type MetricsManager struct {
	metrics      map[string]*types.BackendMetrics
	llamaMetrics map[string]*types.LlamaCppMetrics
	mu           sync.RWMutex
}

// NewMetricsManager - создание менеджера метрик
func NewMetricsManager() *MetricsManager {
	return &MetricsManager{
		metrics:      make(map[string]*types.BackendMetrics),
		llamaMetrics: make(map[string]*types.LlamaCppMetrics),
	}
}

// UpdateLlamaCppMetrics обновляет метрики llama.cpp бэкенда
func (mm *MetricsManager) UpdateLlamaCppMetrics(backendID string, m *types.LlamaCppMetrics) {
	if m == nil {
		return
	}
	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.llamaMetrics[backendID] = m
}

// GetLlamaCppMetrics возвращает метрики llama.cpp бэкенда
func (mm *MetricsManager) GetLlamaCppMetrics(backendID string) *types.LlamaCppMetrics {
	mm.mu.RLock()
	defer mm.mu.RUnlock()
	return mm.llamaMetrics[backendID]
}

// IsModelRunningOnBackend проверяет, запущена ли модель на бэкенде (учитывая тип бэкенда).
func (mm *MetricsManager) IsModelRunningOnBackend(backendID, modelName string, engine types.BackendEngine) bool {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	switch engine {
	case types.EngineLlamaCPP:
		lm, ok := mm.llamaMetrics[backendID]
		if !ok {
			return false
		}
		for _, m := range lm.LoadedModels {
			if m.Name == modelName {
				return true
			}
		}
		return false
	default:
		bm, ok := mm.metrics[backendID]
		if !ok {
			return false
		}
		for _, m := range bm.Ollama.RunningModels {
			if m.Name == modelName {
				return true
			}
		}
		return false
	}
}

// ListRunningModels возвращает список запущенных моделей для бэкенда (учитывая тип).
func (mm *MetricsManager) ListRunningModels(backendID string, engine types.BackendEngine) []string {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	var names []string
	switch engine {
	case types.EngineLlamaCPP:
		if lm, ok := mm.llamaMetrics[backendID]; ok {
			for _, m := range lm.LoadedModels {
				names = append(names, m.Name)
			}
		}
	default:
		if bm, ok := mm.metrics[backendID]; ok {
			for _, m := range bm.Ollama.RunningModels {
				names = append(names, m.Name)
			}
		}
	}
	return names
}

// UpdateLlamaCppModelLoaded — обработчик callback'а от cppworker'а
// (POST /api/v1/internal/llama-model-loaded). Вызывается после успешной
// загрузки модели на бэкенде.
//
// Действия:
//  1. Добавляет модель в llamaMetrics[backendID].LoadedModels (если её там ещё нет).
//  2. Удаляет модель из llamaMetrics[backendID].LoadingModels (если она там была).
//  3. Обновляет runtime params (kvCacheType/flashAttnType/useMmap) для profile
//     mismatch detection в preflight_nctx.go (Phase 2).
//
// Это позволяет UI сразу увидеть загруженную модель, не дожидаясь
// 30-секундного poll'а от llamaCppMetricsPoller.
//
// R60.4 (2026-09-04): webui meta — добавлены modelPath и quantization параметры.
// До R60.4 LoadedModels[i].Path и Quantization оставались пустыми (cppworker
// не передавал их в notify callback), и webui gguf-renderer-detail.js:232-234
// показывал карточку модели с пустыми "Размер: -" и "Квантизация: -".
// Cppworker теперь парсит quantization из path через parseQuantization (R60.3).
func (mm *MetricsManager) UpdateLlamaCppModelLoaded(
	backendID, model string,
	modelPath string,
	sizeBytes uint64,
	contextSize, gpuLayers int,
	kvCacheType string,
	flashAttnType int,
	useMmap bool,
	quantization string,
) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	lm, ok := mm.llamaMetrics[backendID]
	if !ok {
		lm = &types.LlamaCppMetrics{
			LoadedModels:    []types.LlamaCppModel{},
			AvailableModels: []types.LlamaCppModel{},
		}
		mm.llamaMetrics[backendID] = lm
	}

	// 1) Добавляем в LoadedModels (если ещё нет).
	found := false
	for i, m := range lm.LoadedModels {
		if m.Name == model {
			// Обновляем меты.
			lm.LoadedModels[i].Size = sizeBytes
			// R60.4: path и quantization (если присланы, не затираем).
			if modelPath != "" {
				lm.LoadedModels[i].Path = modelPath
			}
			if quantization != "" {
				lm.LoadedModels[i].Quantization = quantization
			}
			if contextSize > 0 {
				lm.LoadedModels[i].ContextLength = contextSize
			}
			if gpuLayers != 0 {
				lm.LoadedModels[i].NumGPULayers = gpuLayers
			}
			// Round 34 (2026-08-12): runtime params для preflight profile mismatch.
			if kvCacheType != "" {
				lm.LoadedModels[i].KvCacheType = kvCacheType
			}
			// flashAttnType: cppworker шлёт -1/0/1. -1 = auto (default). Любое != 0
			// означает что-то конкретное. -1 не перезаписываем если клиент
			// явно прислал.
			if flashAttnType != 0 {
				lm.LoadedModels[i].FlashAttnType = flashAttnType
			}
			// useMmap bool — обновляем всегда (дефолт true, явный false = signal).
			lm.LoadedModels[i].UseMmap = useMmap
			lm.LoadedModels[i].State = "loaded"
			lm.LoadedModels[i].LoadingStartedAt = nil
			lm.LoadedModels[i].LoadingError = ""
			found = true
			break
		}
	}
	if !found {
		lm.LoadedModels = append(lm.LoadedModels, types.LlamaCppModel{
			Name:           model,
			Path:           modelPath,
			Size:           sizeBytes,
			ContextLength:  contextSize,
			NumGPULayers:   gpuLayers,
			KvCacheType:    kvCacheType,
			FlashAttnType:  flashAttnType,
			UseMmap:        useMmap,
			Quantization:   quantization,
			State:          "loaded",
		})
	}

	// 2) Удаляем из LoadingModels (если была).
	filtered := lm.LoadingModels[:0]
	for _, m := range lm.LoadingModels {
		if m.Name != model {
			filtered = append(filtered, m)
		}
	}
	lm.LoadingModels = filtered
}

// UpdateLlamaCppModelUnloaded — обработчик callback'а от cppworker'а при
// выгрузке модели (POST /api/v1/internal/llama-model-unloaded). Удаляет
// модель из LoadedModels, чтобы:
//   1. UI не показывал unloaded модель как загруженную.
//   2. preflight_nctx не использовал stale lastKnownNCtx (Phase 3 fix).
//
// Round 34 (2026-08-12): без этого callback'а lastKnownNCtx остаётся
// в NCtxReloadCoordinator после `idle_unload_after` (10m) → preflight думает
// модель загружена с большим n_ctx, не триггерит reload → пользователь получает
// 502 connection refused от cppworker (модель не загружена).
func (mm *MetricsManager) UpdateLlamaCppModelUnloaded(backendID, model string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	lm, ok := mm.llamaMetrics[backendID]
	if !ok {
		return
	}
	filtered := lm.LoadedModels[:0]
	for _, m := range lm.LoadedModels {
		if m.Name != model {
			filtered = append(filtered, m)
		}
	}
	lm.LoadedModels = filtered
}

// UpdateLlamaCppLoadingModels — обновляет LoadingModels в кэше (вызывается
// из llamaCppMetricsPoller.pollLoadingProgress). Потокобезопасно.
func (mm *MetricsManager) UpdateLlamaCppLoadingModels(backendID string, models []types.LlamaCppModel) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	lm, ok := mm.llamaMetrics[backendID]
	if !ok {
		lm = &types.LlamaCppMetrics{
			LoadedModels:    []types.LlamaCppModel{},
			AvailableModels: []types.LlamaCppModel{},
		}
		mm.llamaMetrics[backendID] = lm
	}
	lm.LoadingModels = models
}

// AppendLlamaCppLoadingModel — добавляет одну модель в LoadingModels
// (вызывается из cppworker'а при старте загрузки, если будет реализован
// notifyModelLoading callback в будущем). Потокобезопасно.
func (mm *MetricsManager) AppendLlamaCppLoadingModel(backendID string, model types.LlamaCppModel) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	lm, ok := mm.llamaMetrics[backendID]
	if !ok {
		lm = &types.LlamaCppMetrics{
			LoadedModels:    []types.LlamaCppModel{},
			AvailableModels: []types.LlamaCppModel{},
		}
		mm.llamaMetrics[backendID] = lm
	}
	// Проверяем, не дубликат ли это.
	for _, m := range lm.LoadingModels {
		if m.Name == model.Name {
			return
		}
	}
	if model.LoadingStartedAt == nil {
		s := time.Now().UTC().Format(time.RFC3339Nano)
		model.LoadingStartedAt = &s
	}
	if model.State == "" {
		model.State = "loading"
	}
	lm.LoadingModels = append(lm.LoadingModels, model)
}
