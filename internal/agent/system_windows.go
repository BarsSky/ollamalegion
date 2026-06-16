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
// Основной источник — nvidia-smi; при недоступности возвращает пустые метрики.
func getGPUMetrics() types.GPUMetrics {
	output, err := executeNvidiaSmi()
	if err != nil {
		return types.GPUMetrics{}
	}
	return parseNvidiaSmiOutput(output)
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
func getCPUMetrics() types.CPUMetrics {
	metrics := types.CPUMetrics{}

	// CPU info через WMIC
	cmd := exec.Command("wmic", "cpu", "get", "Name,NumberOfCores,NumberOfLogicalProcessors", "/format:csv")
	output, err := cmd.Output()
	if err == nil {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "Node") {
				continue
			}
			parts := strings.Split(line, ",")
			if len(parts) >= 4 {
				metrics.Model = strings.TrimSpace(parts[2])
				metrics.CoreCount, _ = strconv.Atoi(strings.TrimSpace(parts[3]))
				metrics.ThreadCount, _ = strconv.Atoi(strings.TrimSpace(parts[4]))
			}
		}
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