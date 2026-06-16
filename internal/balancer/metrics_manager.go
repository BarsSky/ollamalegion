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
//
// Это позволяет UI сразу увидеть загруженную модель, не дожидаясь
// 30-секундного poll'а от llamaCppMetricsPoller.
func (mm *MetricsManager) UpdateLlamaCppModelLoaded(backendID, model string, sizeBytes uint64, contextSize, gpuLayers int) {
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
			if contextSize > 0 {
				lm.LoadedModels[i].ContextLength = contextSize
			}
			if gpuLayers != 0 {
				lm.LoadedModels[i].NumGPULayers = gpuLayers
			}
			lm.LoadedModels[i].State = "loaded"
			lm.LoadedModels[i].LoadingStartedAt = nil
			lm.LoadedModels[i].LoadingError = ""
			found = true
			break
		}
	}
	if !found {
		lm.LoadedModels = append(lm.LoadedModels, types.LlamaCppModel{
			Name:          model,
			Size:          sizeBytes,
			ContextLength: contextSize,
			NumGPULayers:  gpuLayers,
			State:         "loaded",
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
