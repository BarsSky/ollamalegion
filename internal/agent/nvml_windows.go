//go:build nvml && windows
// +build nvml,windows

package agent

import (
	"fmt"
	"sync"

	"ollama-loadbalancer/pkg/types"
)

// nvmlInitialized - флаг инициализации NVML (Windows stub)
var (
	nvmlInitialized bool
	nvmlMu          sync.Mutex
)

// initNVML - инициализация NVML библиотеки (Windows stub)
// NVML не поддерживается на Windows через go-nvml из-за зависимости от dlfcn.h
func initNVML() bool {
	fmt.Println("[NVML] Not available on Windows - requires nvidia-smi fallback")
	return false
}

// shutdownNVML - освобождение ресурсов NVML (Windows stub)
func shutdownNVML() {
	// Nothing to do on Windows
}

// nvmlAvailable - проверка доступности NVML (Windows stub)
func nvmlAvailable() bool {
	return false
}

// collectGPUInfoNVML - сбор информации через NVML (Windows stub)
func (a *Agent) collectGPUInfoNVML() GPUInfo {
	fmt.Println("[NVML] Not available on Windows - returning empty GPU info")
	return GPUInfo{Count: 0, Models: []string{}}
}

// collectGPUMetricsNVML - сбор метрик через NVML (Windows stub)
func (a *Agent) collectGPUMetricsNVML() types.GPUMetrics {
	fmt.Println("[NVML] Not available on Windows - returning empty metrics")
	return types.GPUMetrics{}
}
