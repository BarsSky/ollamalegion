//go:build nvml && windows
// +build nvml,windows

package agent

import (
	"fmt"
	"sync"

	"ollama-loadbalancer/pkg/types"
)

// nvmlInitialized - флаг инициализации NVML (Windows fallback)
var (
	nvmlInitialized bool
	nvmlMu          sync.Mutex
)

// initNVML - инициализация NVML библиотеки (Windows fallback)
// go-nvml не поддерживается на Windows из-за зависимости от dlfcn.h,
// поэтому используем nvidia-smi / WMI fallback через getGPUInfo/getGPUMetrics.
func initNVML() bool {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	if nvmlInitialized {
		return true
	}
	fmt.Println("[NVML] go-nvml not available on Windows - using nvidia-smi/WMI fallback")
	nvmlInitialized = true
	return true
}

// shutdownNVML - освобождение ресурсов NVML (Windows fallback)
func shutdownNVML() {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	nvmlInitialized = false
}

// nvmlAvailable - проверка доступности NVML (Windows fallback)
func nvmlAvailable() bool {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	return nvmlInitialized
}

// collectGPUInfoNVML - сбор информации через NVML (Windows fallback)
// Использует nvidia-smi и WMI Win32_VideoController.
func (a *Agent) collectGPUInfoNVML() GPUInfo {
	return getGPUInfo()
}

// collectGPUMetricsNVML - сбор метрик через NVML (Windows fallback)
// Использует nvidia-smi; при недоступности возвращает пустые метрики.
func (a *Agent) collectGPUMetricsNVML() types.GPUMetrics {
	return getGPUMetrics()
}
