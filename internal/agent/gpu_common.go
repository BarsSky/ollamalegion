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

// collectGPUUUIDs — физические идентификаторы карт этой машины.
//
// ЗАЧЕМ (R-Image follow-up, 2026-10-07): балансер обязан отличать «две машины по
// одной карте» от «одна машина, два бэкенда». Имя хоста для этого не годится —
// контейнеры на одном хосте регистрируются под разными именами
// (cppworker-gpu, imageworker), поэтому балансер видел две «машины» и складывал
// память одной карты дважды. UUID карты у контейнеров одного хоста совпадает
// побайтово, а у разных физических карт — различается.
//
// Пустой результат означает «неизвестно» (нет nvidia-smi, нет прав, не-nvidia
// платформа): вызывающая сторона трактует это как «отдельная машина», то есть
// сохраняет прежнее поведение и ничего не ломает.
//
// Своя команда, а не общий запрос метрик: UUID — строковый столбец, и добавлять
// его в parseNvidiaSmiOutput значило бы переиндексировать все числовые поля
// (тесты и NVML-путь опираются на текущий порядок).
func collectGPUUUIDs() []string {
	cmd := exec.Command("nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var uuids []string
	for _, line := range strings.Split(string(out), "\n") {
		u := strings.TrimSpace(line)
		if u == "" {
			continue
		}
		uuids = append(uuids, u)
	}
	return uuids
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

		// Power draw (W).
		//
		// R88 (2026-10-08): nvidia-smi с `--format=csv,noheader,nounits` печатает
		// мощность ДРОБЬЮ («25.49»), поэтому strconv.Atoi здесь ВСЕГДА падал и в
		// панели на каждой карточке было «POWER 0W» — при том что nvidia-smi
		// внутри контейнера отдаёт реальные 22–47 W. Разбираем как float.
		if val, err := strconv.ParseFloat(strings.TrimSpace(parts[7]), 64); err == nil {
			metrics.PowerUsage = int(val + 0.5)
		}

		// Power limit (W) — та же дробная форма («100.00»).
		if val, err := strconv.ParseFloat(strings.TrimSpace(parts[8]), 64); err == nil {
			metrics.PowerLimit = int(val + 0.5)
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