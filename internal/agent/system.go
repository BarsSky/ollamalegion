//go:build linux || darwin
// +build linux darwin

package agent

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// getCPUUsage - получение загрузки CPU
func getCPUUsage() float64 {
	stat1, err := readCPUStat()
	if err != nil {
		return 0
	}

	time.Sleep(500 * time.Millisecond)

	stat2, err := readCPUStat()
	if err != nil {
		return 0
	}

	userDiff := stat2.User - stat1.User
	niceDiff := stat2.Nice - stat1.Nice
	systemDiff := stat2.System - stat1.System
	idleDiff := stat2.Idle - stat1.Idle
	iowaitDiff := stat2.Iowait - stat1.Iowait

	totalDiff := userDiff + niceDiff + systemDiff + idleDiff + iowaitDiff
	if totalDiff == 0 {
		return 0
	}

	usedDiff := userDiff + niceDiff + systemDiff
	return float64(usedDiff) * 100.0 / float64(totalDiff)
}

// CPUStat - структура для хранения статистики CPU
type CPUStat struct {
	User    uint64
	Nice    uint64
	System  uint64
	Idle    uint64
	Iowait  uint64
	IRQ     uint64
	SoftIRQ uint64
}

// readCPUStat - чтение статистики CPU из /proc/stat (агрегированная)
func readCPUStat() (*CPUStat, error) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu ") {
			return parseCPUStatLine(line)
		}
	}

	return nil, fmt.Errorf("cpu stat not found")
}

// parseCPUStatLine - парсинг строки CPU статистики
func parseCPUStatLine(line string) (*CPUStat, error) {
	fields := strings.Fields(line)
	if len(fields) < 8 {
		return nil, fmt.Errorf("invalid cpu stat format")
	}

	stat := &CPUStat{}
	stat.User, _ = strconv.ParseUint(fields[1], 10, 64)
	stat.Nice, _ = strconv.ParseUint(fields[2], 10, 64)
	stat.System, _ = strconv.ParseUint(fields[3], 10, 64)
	stat.Idle, _ = strconv.ParseUint(fields[4], 10, 64)
	stat.Iowait, _ = strconv.ParseUint(fields[5], 10, 64)
	stat.IRQ, _ = strconv.ParseUint(fields[6], 10, 64)
	stat.SoftIRQ, _ = strconv.ParseUint(fields[7], 10, 64)

	return stat, nil
}

// getCPUMetrics - сбор расширенных CPU метрик
func getCPUMetrics() types.CPUMetrics {
	metrics := types.CPUMetrics{}

	// Load average
	load1, load5, load15 := getLoadAverage()
	metrics.LoadAverage1 = load1
	metrics.LoadAverage5 = load5
	metrics.LoadAverage15 = load15

	// CPU info
	model, cores, threads, err := getCPUInfo()
	if err == nil {
		metrics.Model = model
		metrics.CoreCount = cores
		metrics.ThreadCount = threads
	}

	// Per-core CPU usage
	metrics.UsagePerCore = getPerCoreUsage()

	// Temperature
	temps := getThermalInfo()
	for _, temp := range temps {
		if temp > metrics.Temperature {
			metrics.Temperature = temp
		}
	}

	// Throttling detection
	metrics.Throttled = detectCPUThrottling()

	return metrics
}

// getPerCoreUsage - получение загрузки по каждому ядру CPU
func getPerCoreUsage() []float64 {
	stat1, err := readAllCPUStats()
	if err != nil {
		return nil
	}

	time.Sleep(500 * time.Millisecond)

	stat2, err := readAllCPUStats()
	if err != nil {
		return nil
	}

	usage := make([]float64, 0, len(stat1))
	for cpuID, s1 := range stat1 {
		s2, ok := stat2[cpuID]
		if !ok {
			continue
		}

		userDiff := s2.User - s1.User
		niceDiff := s2.Nice - s1.Nice
		systemDiff := s2.System - s1.System
		idleDiff := s2.Idle - s1.Idle
		iowaitDiff := s2.Iowait - s1.Iowait

		totalDiff := userDiff + niceDiff + systemDiff + idleDiff + iowaitDiff
		if totalDiff == 0 {
			usage = append(usage, 0)
			continue
		}

		usedDiff := userDiff + niceDiff + systemDiff
		usage = append(usage, float64(usedDiff)*100.0/float64(totalDiff))
	}

	return usage
}

// readAllCPUStats - чтение статистики всех CPU из /proc/stat
func readAllCPUStats() (map[string]*CPUStat, error) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	stats := make(map[string]*CPUStat)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu") && len(line) > 3 && line[3] != ' ' {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				cpuID := fields[0]
				stat, err := parseCPUStatLine(line)
				if err == nil {
					stats[cpuID] = stat
				}
			}
		}
	}

	return stats, nil
}

// getMemoryInfo - получение информации о памяти
func getMemoryInfo() (total, used, free uint64) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0
	}
	defer file.Close()

	memInfo := make(map[string]uint64)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			key := strings.TrimSuffix(parts[0], ":")
			value, _ := strconv.ParseUint(parts[1], 10, 64)
			memInfo[key] = value / 1024 // kB → MB
		}
	}

	total = memInfo["MemTotal"]
	freeMem := memInfo["MemFree"] + memInfo["Buffers"] + memInfo["Cached"]
	used = total - freeMem
	free = freeMem

	return
}

// getDiskInfo - получение информации о диске
func getDiskInfo() (total, used, free uint64) {
	cmd := exec.Command("df", "-m", "/")
	output, err := cmd.Output()
	if err != nil {
		return 0, 0, 0
	}

	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return 0, 0, 0
	}

	fields := strings.Fields(lines[1])
	if len(fields) < 4 {
		return 0, 0, 0
	}

	total, _ = strconv.ParseUint(fields[1], 10, 64)
	used, _ = strconv.ParseUint(fields[2], 10, 64)
	free, _ = strconv.ParseUint(fields[3], 10, 64)

	return
}

// getNetworkIO - получение статистики сетевого трафика
func getNetworkIO() (rx, tx uint64) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "|") {
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			continue
		}

		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue
		}

		fields := strings.Fields(parts[1])
		if len(fields) >= 9 {
			rxBytes, _ := strconv.ParseUint(fields[0], 10, 64)
			txBytes, _ := strconv.ParseUint(fields[8], 10, 64)
			rx += rxBytes
			tx += txBytes
		}
	}

	return
}

// getLoadAverage - получение средней нагрузки
func getLoadAverage() (load1, load5, load15 float64) {
	file, err := os.Open("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 3 {
			load1, _ = strconv.ParseFloat(fields[0], 64)
			load5, _ = strconv.ParseFloat(fields[1], 64)
			load15, _ = strconv.ParseFloat(fields[2], 64)
		}
	}

	return
}

// getCPUInfo - получение информации о CPU
func getCPUInfo() (model string, cores int, threads int, err error) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", 0, 0, err
	}
	defer file.Close()

	uniqueCores := make(map[string]bool)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				model = strings.TrimSpace(parts[1])
			}
		}
		if strings.HasPrefix(line, "processor") {
			threads++
		}
		if strings.HasPrefix(line, "core id") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				uniqueCores[strings.TrimSpace(parts[1])] = true
			}
		}
	}

	cores = len(uniqueCores)
	if cores == 0 {
		cores = threads
	}

	return
}

// getThermalInfo - получение температур CPU
func getThermalInfo() []int {
	temps := []int{}

	zones, err := filepath.Glob("/sys/class/thermal/thermal_zone*")
	if err != nil {
		return temps
	}

	for _, zone := range zones {
		tempPath := filepath.Join(zone, "temp")
		data, err := os.ReadFile(tempPath)
		if err != nil {
			continue
		}

		temp, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if temp > 0 {
			temps = append(temps, temp/1000) // millidegrees → degrees
		}
	}

	return temps
}

// detectCPUThrottling - определение троттлинга CPU
func detectCPUThrottling() bool {
	throttleFiles := []string{
		"/sys/devices/system/cpu/cpu0/thermal_throttle/core_throttle_count",
		"/sys/devices/system/cpu/cpu0/thermal_throttle/package_throttle_count",
	}

	for _, path := range throttleFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		val, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if val > 0 {
			return true
		}
	}

	return false
}