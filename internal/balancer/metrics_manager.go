package balancer

import (
	"sync"

	"ollama-loadbalancer/pkg/types"
)

// MetricsManager - менеджер метрик бэкендов
type MetricsManager struct {
	metrics    map[string]*types.BackendMetrics
	llamaMetrics map[string]*types.LlamaCppMetrics
	mu         sync.RWMutex
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
