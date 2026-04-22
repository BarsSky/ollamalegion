//go:build !nvml
// +build !nvml

package agent

import (
	"fmt"
	"sync"

	"ollama-loadbalancer/pkg/types"
)

// nvmlInitialized - флаг инициализации NVML (stub для сборки без NVML)
var (
	nvmlInitialized bool
	nvmlMu          sync.Mutex
)

// initNVML - инициализация NVML библиотеки (stub для сборки без NVML)
// При сборке без build tag "nvml" эта функция всегда возвращает false
func initNVML() bool {
	fmt.Println("[NVML] Disabled - build with '-tags nvml' to enable")
	return false
}

// shutdownNVML - освобождение ресурсов NVML (stub для сборки без NVML)
func shutdownNVML() {
	// Nothing to do when NVML is disabled
}

// nvmlAvailable - проверка доступности NVML (stub для сборки без NVML)
func nvmlAvailable() bool {
	return false
}

// collectGPUInfoNVML - сбор информации через NVML (stub для сборки без NVML)
func (a *Agent) collectGPUInfoNVML() GPUInfo {
	fmt.Println("[NVML] Disabled - returning empty GPU info")
	return GPUInfo{Count: 0, Models: []string{}}
}

// collectGPUMetricsNVML - сбор метрик через NVML (stub для сборки без NVML)
func (a *Agent) collectGPUMetricsNVML() types.GPUMetrics {
	fmt.Println("[NVML] Disabled - returning empty metrics")
	return types.GPUMetrics{}
}
