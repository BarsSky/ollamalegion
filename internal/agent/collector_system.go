package agent

import (
	"ollama-loadbalancer/pkg/types"
)

// collectSystemMetrics - сбор системных метрик
func (a *Agent) collectSystemMetrics() types.SystemMetrics {
	metrics := types.SystemMetrics{}

	// CPU usage
	metrics.CPUUsagePercent = getCPUUsage()

	// Расширенные CPU метрики
	metrics.CPU = getCPUMetrics()

	// Memory
	total, used, free := getMemoryInfo()
	metrics.MemoryTotal = total
	metrics.MemoryUsed = used
	metrics.MemoryFree = free

	// Disk
	diskTotal, diskUsed, diskFree := getDiskInfo()
	metrics.DiskTotal = diskTotal
	metrics.DiskUsed = diskUsed
	metrics.DiskFree = diskFree

	// Network
	rx, tx := getNetworkIO()
	metrics.NetworkRX = rx
	metrics.NetworkTX = tx

	return metrics
}