package agent

import (
	"fmt"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// GPUInfo - информация о GPU
type GPUInfo struct {
	Count  int      `json:"count"`
	Models []string `json:"models"`
}

// collectGPUInfo - сбор информации о GPU
func (a *Agent) collectGPUInfo() GPUInfo {
	info := GPUInfo{}

	// Если уже определено что GPU недоступна — сразу возвращаем пустой результат
	if a.gpuUnavailable {
		return info
	}

	// Попытка получить информацию через nvidia-smi
	output, err := executeNvidiaSmi()
	if err != nil {
		// Если nvidia-smi недоступен, пробуем через NVML
		return a.collectGPUInfoNVML()
	}

	// Парсинг вывода nvidia-smi
	info.Count = countGPUs(output)
	info.Models = parseGPUMModels(output)

	return info
}

// collectGPUMetrics - сбор метрик GPU
func (a *Agent) collectGPUMetrics() types.GPUMetrics {
	metrics := types.GPUMetrics{}

	// Если уже определено что GPU недоступна — сразу возвращаем пустой результат
	if a.gpuUnavailable {
		return metrics
	}

	// Попытка получить метрики через nvidia-smi
	output, err := executeNvidiaSmi()
	if err != nil {
		// Кэшируем недоступность GPU для последующих циклов
		a.gpuUnavailable = true
		fmt.Printf("[%s] nvidia-smi unavailable, GPU metrics disabled\n", time.Now().Format(time.RFC3339))
		return a.collectGPUMetricsNVML()
	}

	// Парсинг вывода nvidia-smi
	metrics = parseNvidiaSmiOutput(output)

	return metrics
}

// collectGPUMetricsNVML - сбор метрик через NVML
// Реализация в файле nvml_unix.go (Linux/Darwin) или nvml_windows.go (Windows stub)

// collectGPUInfoNVML - сбор информации через NVML
// Реализация в файле nvml_unix.go (Linux/Darwin) или nvml_windows.go (Windows stub)