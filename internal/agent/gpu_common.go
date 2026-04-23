package agent

import (
	"os/exec"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// executeNvidiaSmi - выполнение nvidia-smi и получение вывода
func executeNvidiaSmi() (string, error) {
	// Попытка выполнить nvidia-smi с нужными параметрами
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=index,name,utilization.gpu,memory.total,memory.used,memory.free,temperature.gpu,power.draw,power.limit,clocks.gr,clocks.mem",
		"--format=csv,noheader,nounits")

	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// countGPUs - подсчет количества GPU из вывода nvidia-smi
func countGPUs(output string) int {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return len(lines)
}

// parseGPUMModels - парсинг названий GPU моделей
func parseGPUMModels(output string) []string {
	var models []string

	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		parts := strings.Split(line, ",")
		if len(parts) >= 2 {
			modelName := strings.TrimSpace(parts[1])
			if modelName != "" {
				models = append(models, modelName)
			}
		}
	}

	return models
}

// parseNvidiaSmiOutput - парсинг вывода nvidia-smi в метрики
func parseNvidiaSmiOutput(output string) types.GPUMetrics {
	metrics := types.GPUMetrics{}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return metrics
	}

	// Парсим первую строку (первый GPU)
	// Формат: index, name, utilization.gpu, memory.total, memory.used, memory.free, temperature.gpu, power.draw, power.limit, clocks.gr, clocks.mem
	line := lines[0]
	parts := strings.Split(line, ",")

	if len(parts) >= 11 {
		// Usage percent
		if val, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64); err == nil {
			metrics.UsagePercent = val
		}

		// Memory total (MB)
		if val, err := strconv.ParseUint(strings.TrimSpace(parts[3]), 10, 64); err == nil {
			metrics.MemoryTotal = val
		}

		// Memory used (MB)
		if val, err := strconv.ParseUint(strings.TrimSpace(parts[4]), 10, 64); err == nil {
			metrics.MemoryUsed = val
		}

		// Memory free (MB)
		if val, err := strconv.ParseUint(strings.TrimSpace(parts[5]), 10, 64); err == nil {
			metrics.MemoryFree = val
		}

		// Temperature
		if val, err := strconv.Atoi(strings.TrimSpace(parts[6])); err == nil {
			metrics.Temperature = val
		}

		// Power draw (W)
		if val, err := strconv.Atoi(strings.TrimSpace(parts[7])); err == nil {
			metrics.PowerUsage = val
		}

		// Power limit (W)
		if val, err := strconv.Atoi(strings.TrimSpace(parts[8])); err == nil {
			metrics.PowerLimit = val
		}

		// GPU Clock (MHz)
		if val, err := strconv.Atoi(strings.TrimSpace(parts[9])); err == nil {
			metrics.GPUClock = val
		}

		// Memory Clock (MHz)
		if val, err := strconv.Atoi(strings.TrimSpace(parts[10])); err == nil {
			metrics.MemClock = val
		}
	}

	return metrics
}