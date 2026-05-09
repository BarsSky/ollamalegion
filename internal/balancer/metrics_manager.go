package balancer

import (
	"sync"

	"ollama-loadbalancer/pkg/types"
)

// MetricsManager - менеджер метрик бэкендов
type MetricsManager struct {
	metrics map[string]*types.BackendMetrics
	mu      sync.RWMutex
}

// NewMetricsManager - создание менеджера метрик
func NewMetricsManager() *MetricsManager {
	return &MetricsManager{
		metrics: make(map[string]*types.BackendMetrics),
	}
}