//go:build windows
// +build windows

package agent

import (
	"os/exec"
	"strconv"
	"strings"
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

// getNetworkIO - получение статистики сетевого трафика (Windows stub)
func getNetworkIO() (rx, tx uint64) {
	// Не реализовано для Windows
	return 0, 0
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