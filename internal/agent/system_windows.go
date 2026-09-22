//go:build windows
// +build windows

package agent

import (
	"os/exec"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// getCPUUsage - получение загрузки CPU через WMIC
func getCPUUsage() float64 {
	cmd := exec.Command("wmic", "cpu", "get", "loadpercentage", "/value")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	return parseWMICValueFloat(string(output), "LoadPercentage")
}

// getMemoryInfo - получение информации о памяти через WMIC
func getMemoryInfo() (total, used, free uint64) {
	// TotalPhysicalMemory (bytes)
	cmd := exec.Command("wmic", "ComputerSystem", "get", "TotalPhysicalMemory", "/value")
	output, err := cmd.Output()
	if err != nil {
		return 0, 0, 0
	}
	totalBytes := parseWMICValueUint64(string(output), "TotalPhysicalMemory")
	total = totalBytes / 1024 / 1024 // MB

	// FreePhysicalMemory (KB)
	cmd = exec.Command("wmic", "OS", "get", "FreePhysicalMemory", "/value")
	output, err = cmd.Output()
	if err != nil {
		return total, 0, 0
	}
	freeKB := parseWMICValueUint64(string(output), "FreePhysicalMemory")
	free = freeKB / 1024 // MB

	used = total - free
	return
}

// getDiskInfo - получение информации о диске C: через WMIC
func getDiskInfo() (total, used, free uint64) {
	cmd := exec.Command("wmic", "logicaldisk", "where", "DeviceID='C:'", "get", "size,freespace", "/value")
	output, err := cmd.Output()
	if err != nil {
		return 0, 0, 0
	}
	total = parseWMICValueUint64(string(output), "Size") / 1024 / 1024
	free = parseWMICValueUint64(string(output), "FreeSpace") / 1024 / 1024
	used = total - free
	return
}

// getNetworkIO - получение статистики сетевого трафика (Windows)
// Использует netstat -e для получения кумулятивных байт
func getNetworkIO() (rx, tx uint64) {
	cmd := exec.Command("netstat", "-e")
	output, err := cmd.Output()
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Bytes") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				rx, _ = strconv.ParseUint(parts[1], 10, 64)
				tx, _ = strconv.ParseUint(parts[2], 10, 64)
			}
			return
		}
	}
	return 0, 0
}

// getGPUInfo - получение информации о GPU через WMI (Windows fallback)
// Используется, когда nvidia-smi недоступен или NVML не собран.
func getGPUInfo() GPUInfo {
	info := GPUInfo{}

	// Первый fallback: nvidia-smi (если установлен)
	output, err := executeNvidiaSmi()
	if err == nil {
		info.Count = countGPUs(output)
		info.Models = parseGPUMModels(output)
		return info
	}

	// Второй fallback: WMI Win32_VideoController
	cmd := exec.Command("wmic", "path", "Win32_VideoController", "get", "Name,AdapterRAM", "/format:csv")
	wmiOutput, err := cmd.Output()
	if err != nil {
		return info
	}

	lines := strings.Split(string(wmiOutput), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Node") {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) >= 3 {
			name := strings.TrimSpace(parts[2])
			if name != "" && !strings.Contains(strings.ToLower(name), "basic display") {
				info.Count++
				info.Models = append(info.Models, name)
			}
		}
	}

	return info
}

// getGPUMetrics - получение метрик GPU на Windows.
//
// Источники данных (по убыванию приоритета):
//  1. nvidia-smi (полные метрики: usage%, VRAM used/free, temperature, power, clocks).
//  2. WMI Win32_VideoController (fallback при отсутствии nvidia-smi —
//     возвращает только VRAM Total через поле AdapterRAM; usage/temperature
//     недоступны без проприетарных драйверов).
//  3. Пустая структура (все нули) — если оба источника недоступны.
//
// Ограничения WMI fallback:
//   - AdapterRAM возвращает 0 для GPU с разделяемой памятью (iGPU) — тогда
//     MemoryTotal останется 0.
//   - AdapterRAM на дискретных GPU обычно показывает полный объём VRAM (напр.,
//     RTX 3070 8GB → 8589934592 bytes = 8192 MB).
//   - MemoryUsed/MemoryFree/Temperature/PowerUsage/Clocks через WMI
//     Win32_VideoController НЕ доступны — требуется NVML (Linux) или
//     nvidia-smi (Windows).
func getGPUMetrics() types.GPUMetrics {
	// Основной источник — nvidia-smi.
	output, err := executeNvidiaSmi()
	if err == nil {
		return parseNvidiaSmiOutput(output)
	}

	// Fallback #1: WMI Win32_VideoController (даёт хотя бы AdapterRAM).
	metrics := getGPUMetricsWMIFallback()
	if metrics.MemoryTotal > 0 {
		return metrics
	}

	// Fallback #2: ничего не доступно — возвращаем пустую структуру
	// (UI покажет «GPU: unknown» / N/A).
	return types.GPUMetrics{}
}

// getGPUMetricsWMIFallback - получение VRAM Total через WMI Win32_VideoController.
// Используется при отсутствии nvidia-smi (напр., в WSL или на сервере без NVIDIA
// драйверов).
//
// WMI-класс Win32_VideoController предоставляет только AdapterRAM (bytes),
// Name и DriverVersion. Остальные поля (usage/temperature/power) недоступны.
//
// Если машина имеет несколько GPU, суммируем AdapterRAM всех дискретных
// карт (исключая «Basic Display Adapter» — это всегда встроенная графика
// Windows без VRAM).
func getGPUMetricsWMIFallback() types.GPUMetrics {
	metrics := types.GPUMetrics{}

	cmd := exec.Command("wmic", "path", "Win32_VideoController", "get", "Name,AdapterRAM", "/format:csv")
	output, err := cmd.Output()
	if err != nil {
		return metrics
	}

	// Парсинг делегирован в платформонезависимую функцию parseWmiVideoControllerCSV.
	// bytes → MB.
	totalBytes := parseWmiVideoControllerVRAMBytes(string(output))
	if totalBytes > 0 {
		metrics.MemoryTotal = totalBytes / 1024 / 1024
	}
	return metrics
}

// parseWMICValueFloat - парсинг float из WMIC вывода
func parseWMICValueFloat(output, key string) float64 {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+"=") {
			val, _ := strconv.ParseFloat(strings.TrimPrefix(line, key+"="), 64)
			return val
		}
	}
	return 0
}

// parseWMICValueUint64 - парсинг uint64 из WMIC вывода
func parseWMICValueUint64(output, key string) uint64 {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+"=") {
			val, _ := strconv.ParseUint(strings.TrimPrefix(line, key+"="), 10, 64)
			return val
		}
	}
	return 0
}

// getCPUMetrics - сбор расширенных CPU метрик (Windows)
// parseWMICCPUCSV — разбор вывода
// `wmic cpu get Name,NumberOfCores,NumberOfLogicalProcessors /format:csv`.
//
// R66c (2026-09-22): переписано ради двух реальных дефектов.
//
//  1. ПАНИКА. Было `if len(parts) >= 4 { ... parts[4] }` — чтение за границей
//     среза: "index out of range [4] with length 4". Паника в сборе метрик
//     роняет ВЕСЬ процесс агента, а значит и метрики, которые он отправляет в
//     балансер и WebUI. Именно так падал job test-self-hosted
//     (TestCollectSystemMetrics) — под LocalSystem `wmic /format:csv` работает,
//     а у интерактивного пользователя он падает с "Invalid XSL format (or) file
//     name", поэтому локально дефект не воспроизводился.
//
//  2. НЕВЕРНЫЕ ИНДЕКСЫ. `/format:csv` печатает колонки в алфавитном порядке и
//     БЕЗ лишнего поля: Node,Name,NumberOfCores,NumberOfLogicalProcessors — это
//     4 колонки, то есть Model=parts[1], CoreCount=parts[2], ThreadCount=parts[3].
//     Прежний код читал parts[2]/[3]/[4], то есть отдавал в метрики
//     NumberOfCores как имя модели и т.д.
//
// Теперь индексы берутся из строки заголовка (порядок колонок не важен), а
// каждое чтение проверяется на границы. Если заголовка нет — используется
// документированный позиционный порядок.
func parseWMICCPUCSV(output string) types.CPUMetrics {
	metrics := types.CPUMetrics{}

	idxName, idxCores, idxThreads := 1, 2, 3
	headerSeen := false

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		raw := strings.Split(line, ",")
		parts := make([]string, len(raw))
		for i, p := range raw {
			parts[i] = strings.TrimSpace(p)
		}

		if !headerSeen && len(parts) > 0 && strings.EqualFold(parts[0], "Node") {
			headerSeen = true
			idxName, idxCores, idxThreads = -1, -1, -1
			for i, p := range parts {
				switch p {
				case "Name":
					idxName = i
				case "NumberOfCores":
					idxCores = i
				case "NumberOfLogicalProcessors":
					idxThreads = i
				}
			}
			if idxName < 0 || idxCores < 0 || idxThreads < 0 {
				// Неожиданный набор колонок — возвращаемся к позиционному разбору.
				idxName, idxCores, idxThreads = 1, 2, 3
			}
			continue
		}

		at := func(i int) (string, bool) {
			if i < 0 || i >= len(parts) {
				return "", false
			}
			return parts[i], true
		}
		if v, ok := at(idxName); ok {
			metrics.Model = v
		}
		if v, ok := at(idxCores); ok {
			metrics.CoreCount, _ = strconv.Atoi(v)
		}
		if v, ok := at(idxThreads); ok {
			metrics.ThreadCount, _ = strconv.Atoi(v)
		}
	}

	return metrics
}

func getCPUMetrics() types.CPUMetrics {
	metrics := types.CPUMetrics{}

	// CPU info через WMIC
	cmd := exec.Command("wmic", "cpu", "get", "Name,NumberOfCores,NumberOfLogicalProcessors", "/format:csv")
	output, err := cmd.Output()
	if err == nil {
		metrics = parseWMICCPUCSV(string(output))
	}

	// Load average через typeperf (performance counter)
	cmd = exec.Command("typeperf", "\\System\\Processor Queue Length", "-sc", "1")
	output, err = cmd.Output()
	if err == nil {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "\"") && !strings.Contains(line, "Time") {
				parts := strings.Split(line, "\",\"")
				if len(parts) >= 2 {
					valStr := strings.Trim(parts[1], "\"")
					val, _ := strconv.ParseFloat(valStr, 64)
					metrics.LoadAverage1 = val
				}
			}
		}
	}

	// CPU temperature через WMIC (если доступно)
	cmd = exec.Command("wmic", "/namespace:\\\\root\\wmi", "PATH", "MSAcpi_ThermalZoneTemperature", "GET", "CurrentTemperature")
	output, err = cmd.Output()
	if err == nil {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "CurrentTemperature") {
				tempK, _ := strconv.Atoi(line)
				if tempK > 0 {
					// Температура в десятых долях Кельвина
					tempC := (tempK / 10) - 273
					if tempC > metrics.Temperature {
						metrics.Temperature = tempC
					}
				}
			}
		}
	}

	return metrics
}