// Package agent — GPU detection и метрики.
//
// 2026-06-30: жёсткий `gpuUnavailable` флаг (без TTL) заменён на TTL-кэш (60 сек).
// Это позволяет автоматически восстанавливаться после ситуаций, когда
// драйверы монтируются уже после старта агента (например, при compose up,
// когда NVIDIA Container Toolkit поднимает контейнеры параллельно).
package agent

import (
	"fmt"
	"os/exec"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// gpuUnavailableTTL — сколько времени помнить, что GPU недоступна, прежде чем
// попробовать снова. Слишком короткий TTL → шум в логах. Слишком длинный →
// медленное восстановление. 60 сек — компромисс: при AGENT_COLLECT_INTERVAL=15
// мы сделаем 4 неудачные попытки (одна ошибка в логе на TTL), потом — recovery.
const gpuUnavailableTTL = 60 * time.Second

// gpuAvailabilityState — состояние «GPU недоступна» с TTL.
type gpuAvailabilityState struct {
	unavailable bool
	since       time.Time
}

var (
	gpuStateMu sync.Mutex
	gpuState   gpuAvailabilityState
)

// isGPUUnavailableCached — true, если недавно была неудача и TTL ещё не истёк.
func isGPUUnavailableCached() bool {
	gpuStateMu.Lock()
	defer gpuStateMu.Unlock()
	if !gpuState.unavailable {
		return false
	}
	if time.Since(gpuState.since) > gpuUnavailableTTL {
		// TTL истёк — сбрасываем, чтобы следующий вызов попробовал снова.
		gpuState.unavailable = false
		gpuState.since = time.Time{}
		return false
	}
	return true
}

// markGPUUnavailable — пометить GPU как недоступную (с TTL).
func markGPUUnavailable() {
	gpuStateMu.Lock()
	defer gpuStateMu.Unlock()
	// Идемпотентно: не перезаписываем since, если уже помечено (чтобы TTL
	// считался от ПЕРВОЙ неудачи, а не от последней — иначе спам в логах
	// будет держать флаг вечно).
	if gpuState.unavailable {
		return
	}
	gpuState.unavailable = true
	gpuState.since = time.Now()
}

// markGPUAvailable — сбросить флаг недоступности (когда метрики успешно получены).
func markGPUAvailable() {
	gpuStateMu.Lock()
	defer gpuStateMu.Unlock()
	gpuState.unavailable = false
	gpuState.since = time.Time{}
}

// GPUInfo - информация о GPU
type GPUInfo struct {
	Count  int      `json:"count"`
	Models []string `json:"models"`
}

// collectGPUInfo - сбор информации о GPU
func (a *Agent) collectGPUInfo() GPUInfo {
	info := GPUInfo{}

	// Если недавно была неудача (с учётом TTL) — пропускаем дорогой вызов.
	if isGPUUnavailableCached() {
		return info
	}

	// Попытка получить информацию через nvidia-smi
	output, err := executeNvidiaSmi()
	if err != nil {
		// 2026-06-30: логируем тип ошибки (ErrNotFound vs ExitError) для диагностики.
		logNvidiaSmiError(err)
		// nvidia-smi не сработал — пробуем NVML.
		result := a.collectGPUInfoNVML()
		if result.Count == 0 {
			markGPUUnavailable()
		}
		return result
	}

	// Парсинг вывода nvidia-smi
	info.Count = countGPUs(output)
	info.Models = parseGPUMModels(output)
	if info.Count > 0 {
		markGPUAvailable()
	}

	return info
}

// collectGPUMetrics - сбор метрик GPU
func (a *Agent) collectGPUMetrics() types.GPUMetrics {
	metrics := types.GPUMetrics{}

	// Если недавно была неудача (с учётом TTL) — пропускаем дорогой вызов.
	if isGPUUnavailableCached() {
		return metrics
	}

	// Попытка получить метрики через nvidia-smi
	output, err := executeNvidiaSmi()
	if err != nil {
		// 2026-06-30: логируем тип ошибки для диагностики.
		logNvidiaSmiError(err)
		// Fallback на NVML: если NVML вернёт метрики — отлично; иначе пометим
		// GPU как недоступную на TTL.
		metrics = a.collectGPUMetricsNVML()
		if metrics.MemoryTotal == 0 && metrics.UsagePercent == 0 && metrics.Temperature == 0 {
			markGPUUnavailable()
			fmt.Printf("[%s] GPU metrics unavailable (nvidia-smi failed, NVML returned zeros). Retry через %s.\n",
				time.Now().Format(time.RFC3339), gpuUnavailableTTL)
		}
		return metrics
	}

	// Парсинг вывода nvidia-smi
	metrics = parseNvidiaSmiOutput(output)
	if metrics.MemoryTotal > 0 || metrics.UsagePercent > 0 || metrics.Temperature > 0 {
		markGPUAvailable()
	}

	return metrics
}

// logNvidiaSmiError — диагностический лог для executeNvidiaSmi().
func logNvidiaSmiError(err error) {
	if err == nil {
		return
	}
	now := time.Now().Format(time.RFC3339)
	// Различаем: команда не найдена (отсутствует в PATH) vs. бинарь есть, но ошибка.
	if _, ok := err.(*exec.Error); ok {
		fmt.Printf("[%s] nvidia-smi: binary not found in PATH (ожидаемо в nvidia/cuda base)\n", now)
		return
	}
	// *exec.ExitError — nvidia-smi запустился, но вернул ненулевой код (например,
	// «No devices were found» в системе без GPU).
	fmt.Printf("[%s] nvidia-smi: execution failed: %v\n", now, err)
}

// collectGPUMetricsNVML - сбор метрик через NVML
// Реализация в файле nvml_unix.go (Linux/Darwin) или nvml_windows.go (Windows stub)

// collectGPUInfoNVML - сбор информации через NVML
// Реализация в файле nvml_unix.go (Linux/Darwin) или nvml_windows.go (Windows stub)

// ResetGPUAvailabilityCache сбрасывает TTL-кэш. Используется в тестах и в
// admin-endpoint'е (если он появится) для принудительного retry.
func ResetGPUAvailabilityCache() {
	gpuStateMu.Lock()
	defer gpuStateMu.Unlock()
	gpuState.unavailable = false
	gpuState.since = time.Time{}
}
