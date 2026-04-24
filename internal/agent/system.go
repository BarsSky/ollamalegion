//go:build linux || darwin
// +build linux darwin

package agent

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// getCPUUsage - получение загрузки CPU
func getCPUUsage() float64 {
	// Чтение /proc/stat
	stat1, err := readCPUStat()
	if err != nil {
		return 0
	}
	
	// Ждем немного для второго замера
	time.Sleep(500 * time.Millisecond)
	
	stat2, err := readCPUStat()
	if err != nil {
		return 0
	}
	
	// Вычисление разницы
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
	User   uint64
	Nice   uint64
	System uint64
	Idle   uint64
	Iowait uint64
	IRQ    uint64
	SoftIRQ uint64
}

// readCPUStat - чтение статистики CPU из /proc/stat
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
			// Значения в kB, конвертируем в MB
			memInfo[key] = value / 1024
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
	// Используем df для получения информации о диске
	cmd := exec.Command("df", "-m", "/")
	output, err := cmd.Output()
	if err != nil {
		return 0, 0, 0
	}
	
	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return 0, 0, 0
	}
	
	// Парсим вторую строку
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
	// Чтение /proc/net/dev
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		// Пропускаем заголовки
		if strings.Contains(line, "|") && !strings.Contains(line, "Inter-") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				iface := strings.TrimSpace(parts[0])
				// Пропускаем loopback
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
		}
	}
	
	return
}

// readProcFile - чтение числового значения из proc файла
func readProcFile(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	
	value, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return value
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
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			load1, _ = strconv.ParseFloat(fields[0], 64)
			load5, _ = strconv.ParseFloat(fields[1], 64)
			load15, _ = strconv.ParseFloat(fields[2], 64)
		}
	}
	
	return
}

// getUptime - получение времени работы системы
func getUptime() (uptime, idleTime float64) {
	file, err := os.Open("/proc/uptime")
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	
	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			uptime, _ = strconv.ParseFloat(fields[0], 64)
			idleTime, _ = strconv.ParseFloat(fields[1], 64)
		}
	}
	
	return
}

// getProcessCount - получение количества процессов
func getProcessCount() int {
	files, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	
	count := 0
	pidRegex := regexp.MustCompile(`^\d+$`)
	
	for _, f := range files {
		if f.IsDir() && pidRegex.MatchString(f.Name()) {
			count++
		}
	}
	
	return count
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
		cores = threads // fallback
	}

	return
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

// detectCPUThrottling - определение троттлинга CPU
func detectCPUThrottling() bool {
	// Проверка thermal throttling через /sys
	throttleFiles := []string{
		"/sys/devices/system/cpu/cpu0/thermal_throttle/core_throttle_count",
		"/sys/devices/system/cpu/cpu0/thermal_throttle/package_throttle_count",
	}

	for _, path := range throttleFiles {
		val := readProcFile(path)
		if val > 0 {
			return true
		}
	}
	return false
}

// getDockerStats - получение статистики Docker контейнеров (если доступно)
func getDockerStats() (map[string]interface{}, error) {
	// Проверка наличия docker команды
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, err
	}
	
	cmd := exec.Command("docker", "stats", "--no-stream", "--format", 
		"{{.Container}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	
	stats := make(map[string]interface{})
	lines := strings.Split(string(output), "\n")
	
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		
		parts := strings.Split(line, "\t")
		if len(parts) >= 4 {
			stats[parts[0]] = map[string]string{
				"cpu":     parts[1],
				"memory":  parts[2],
				"network": parts[3],
			}
		}
	}
	
	return stats, nil
}

// getHostPaths - проверка путей хоста (для контейнеров)
func getHostPaths() map[string]string {
	paths := map[string]string{
		"proc":  "/host/proc",
		"sys":   "/host/sys",
		"dev":   "/host/dev",
	}
	
	// Проверка существования путей
	for key, path := range paths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			// Путь хоста не доступен, используем стандартный
			switch key {
			case "proc":
				paths[key] = "/proc"
			case "sys":
				paths[key] = "/sys"
			case "dev":
				paths[key] = "/dev"
			}
		}
	}
	
	return paths
}

// readSysfs - чтение значения из sysfs
func readSysfs(path string) string {
	data, err := os.ReadFile(filepath.Join("/sys", path))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// getThermalInfo - получение тепловой информации
func getThermalInfo() map[string]int {
	temps := make(map[string]int)
	
	// Чтение температур из /sys/class/thermal
	zones, err := filepath.Glob("/sys/class/thermal/thermal_zone*")
	if err != nil {
		return temps
	}
	
	for _, zone := range zones {
		name := readSysfs(filepath.Join(zone, "type"))
		tempStr := readSysfs(filepath.Join(zone, "temp"))
		
		if name != "" && tempStr != "" {
			temp, _ := strconv.Atoi(tempStr)
			// Температура в миллиградусах, конвертируем в градусы
			temps[name] = temp / 1000
		}
	}
	
	return temps
}

// getPowerInfo - получение информации о питании
func getPowerInfo() map[string]interface{} {
	power := make(map[string]interface{})
	
	// Для ноутбуков - информация о батарее
	batteries, _ := filepath.Glob("/sys/class/power_supply/BAT*")
	
	for _, bat := range batteries {
		capacity := readSysfs(filepath.Join(bat, "capacity"))
		status := readSysfs(filepath.Join(bat, "status"))
		
		if capacity != "" {
			power["battery_capacity"] = capacity
		}
		if status != "" {
			power["battery_status"] = status
		}
	}
	
	return power
}
