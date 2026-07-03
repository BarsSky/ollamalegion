//go:build (linux || darwin) && nvml

package agent

import (
	"fmt"
	"os"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"

	"ollama-loadbalancer/pkg/types"
)

// nvmlInitialized - флаг инициализации NVML
var (
	nvmlInitialized bool
	nvmlMu          sync.Mutex
)

// nvmlLibraryPaths - типичные пути поиска libnvidia-ml.so.1.
//
// 2026-06-30: до добавления этого списка вызов nvml.Init() мог вызвать SIGSEGV
// (PC=0x0) на системах без NVIDIA-драйвера / без NVIDIA Container Toolkit.
// Причина: cgo-обёртка над libnvidia-ml.so.1 делает dlopen() и затем
// dereference указателя на функцию. Если библиотека не найдена, dlopen()
// возвращает NULL, и попытка вызвать функцию по NULL-указателю приводит
// к segmentation fault, который НЕЛЬЗЯ перехватить через recover().
//
// Безопасная стратегия: сначала проверить наличие библиотеки через os.Stat
// (или эквивалент), и только потом вызывать nvml.Init().
var nvmlLibraryPaths = []string{
	"/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
	"/usr/lib64/libnvidia-ml.so.1",
	"/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
	"/usr/lib/aarch64-linux-gnu/libnvidia-ml.so.1",
	"/usr/lib64/libnvidia-ml.so.1",
	"/usr/local/lib/libnvidia-ml.so.1",
	"/usr/lib/libnvidia-ml.so.1",
	"/lib64/libnvidia-ml.so.1",
}

// nvmlLibraryPresent - проверяет наличие libnvidia-ml.so.1 по известным путям.
//
// Возвращает (path, true) если найден хотя бы один файл, иначе ("", false).
// Используется как «дешёвый» pre-check перед nvml.Init(), чтобы избежать
// SIGSEGV в окружениях без NVIDIA-драйвера (например, в nvidia/cuda:*-base
// образах, которые НЕ содержат userspace-часть драйвера).
func nvmlLibraryPresent() (string, bool) {
	for _, p := range nvmlLibraryPaths {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// initNVML - инициализация NVML библиотеки.
//
// 2026-06-30: добавлена pre-check через nvmlLibraryPresent(), чтобы не вызывать
// nvml.Init() в окружениях без libnvidia-ml.so.1. Без этого check'а процесс
// падает с SIGSEGV (PC=0x0) на стадии dlopen внутри CGo-обёртки.
func initNVML() bool {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()

	if nvmlInitialized {
		return true
	}

	// Pre-check: убеждаемся, что библиотека доступна в файловой системе,
	// прежде чем вызывать nvml.Init() (который может SIGSEGV при NULL dlopen).
	libPath, ok := nvmlLibraryPresent()
	if !ok {
		fmt.Println("[NVML] libnvidia-ml.so.1 not found в известных путях — NVML недоступен (безопасно пропускаем)")
		return false
	}

	// Defense-in-depth: recover() не ловит SIGSEGV из C-кода (POSIX сигналы не идут через
	// Go runtime), но ловит panic'и, которые cgo-обёртка может бросить при невалидной
	// библиотеке или ошибке драйвера. В случае panic'а помечаем NVML как «не инициализирован»
	// и возвращаем false — collectGPUInfoNVML() вернёт пустой результат.
	initOK := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[NVML] Panic during Init (lib=%s): %v — NVML помечен как недоступный\n", libPath, r)
			}
		}()
		ret := nvml.Init()
		if ret != nvml.SUCCESS {
			fmt.Printf("[NVML] Init failed (lib=%s): %v\n", libPath, ret)
			return
		}
		initOK = true
	}()

	if !initOK {
		return false
	}

	nvmlInitialized = true
	fmt.Printf("[NVML] Successfully initialized (lib=%s)\n", libPath)
	return true
}

// shutdownNVML - освобождение ресурсов NVML
func shutdownNVML() {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()

	if !nvmlInitialized {
		return
	}

	ret := nvml.Shutdown()
	if ret != nvml.SUCCESS {
		fmt.Printf("[NVML] Shutdown failed: %v\n", ret)
	} else {
		nvmlInitialized = false
		fmt.Println("[NVML] Successfully shutdown")
	}
}

// nvmlAvailable - проверка доступности NVML
func nvmlAvailable() bool {
	if nvmlInitialized {
		return true
	}
	return initNVML()
}

// collectGPUInfoNVML - сбор информации через NVML (Unix/Linux)
func (a *Agent) collectGPUInfoNVML() GPUInfo {
	info := GPUInfo{
		Count:  0,
		Models: []string{},
	}

	// Проверка доступности NVML
	if !nvmlAvailable() {
		fmt.Println("[NVML] Not available for GPU info collection")
		return info
	}

	// Получение количества GPU
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		fmt.Printf("[NVML] Failed to get device count: %v\n", ret)
		return info
	}

	info.Count = count
	info.Models = make([]string, 0, count)

	// Получение driver version
	driverVersion, ret := nvml.SystemGetDriverVersion()
	if ret == nvml.SUCCESS {
		fmt.Printf("[NVML] Driver version: %s\n", driverVersion)
	}

	// Сбор информации по каждому GPU
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			fmt.Printf("[NVML] Failed to get device handle for index %d: %v\n", i, ret)
			continue
		}

		// Название модели
		name, ret := device.GetName()
		if ret != nvml.SUCCESS {
			fmt.Printf("[NVML] Failed to get name for device %d: %v\n", i, ret)
			name = "Unknown"
		}

		// UUID
		uuid, ret := device.GetUUID()
		if ret != nvml.SUCCESS {
			fmt.Printf("[NVML] Failed to get UUID for device %d: %v\n", i, ret)
			uuid = "Unknown"
		}

		// Информация о памяти
		memory, ret := device.GetMemoryInfo()
		var totalMemoryMB uint64
		if ret == nvml.SUCCESS {
			totalMemoryMB = memory.Total / 1024 / 1024
		} else {
			fmt.Printf("[NVML] Failed to get memory info for device %d: %v\n", i, ret)
			totalMemoryMB = 0
		}

		modelInfo := fmt.Sprintf("%s (UUID: %s, VRAM: %d MB)", name, uuid, totalMemoryMB)
		info.Models = append(info.Models, modelInfo)

		fmt.Printf("[NVML] GPU %d: %s\n", i, modelInfo)
	}

	return info
}

// collectGPUMetricsNVML - сбор метрик через NVML (Unix/Linux)
func (a *Agent) collectGPUMetricsNVML() types.GPUMetrics {
	metrics := types.GPUMetrics{}

	// Проверка доступности NVML
	if !nvmlAvailable() {
		return metrics
	}

	// Получение количества GPU
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS || count == 0 {
		return metrics
	}

	// Собираем метрики для первого GPU (или можно агрегировать все)
	device, ret := nvml.DeviceGetHandleByIndex(0)
	if ret != nvml.SUCCESS {
		fmt.Printf("[NVML] Failed to get device handle: %v\n", ret)
		return metrics
	}

	// Utilization GPU (%)
	util, ret := device.GetUtilizationRates()
	if ret == nvml.SUCCESS {
		metrics.UsagePercent = float64(util.Gpu)
	} else {
		fmt.Printf("[NVML] Failed to get utilization: %v\n", ret)
	}

	// Информация о памяти
	memory, ret := device.GetMemoryInfo()
	if ret == nvml.SUCCESS {
		metrics.MemoryTotal = memory.Total / 1024 / 1024 // MB
		metrics.MemoryUsed = memory.Used / 1024 / 1024   // MB
		metrics.MemoryFree = memory.Free / 1024 / 1024   // MB
	} else {
		fmt.Printf("[NVML] Failed to get memory info: %v\n", ret)
	}

	// Температура
	thermals, ret := device.GetTemperature(nvml.TEMPERATURE_GPU)
	if ret == nvml.SUCCESS {
		metrics.Temperature = int(thermals)
	} else {
		fmt.Printf("[NVML] Failed to get temperature: %v\n", ret)
	}

	// Потребление мощности
	power, ret := device.GetPowerUsage()
	if ret == nvml.SUCCESS {
		metrics.PowerUsage = int(power / 1000) // mW -> W
	} else {
		fmt.Printf("[NVML] Failed to get power usage: %v\n", ret)
	}

	// Лимит мощности
	powerLimit, ret := device.GetPowerManagementLimit()
	if ret == nvml.SUCCESS {
		metrics.PowerLimit = int(powerLimit / 1000) // mW -> W
	} else {
		fmt.Printf("[NVML] Failed to get power limit: %v\n", ret)
	}

	// Частота GPU
	gpuClock, ret := device.GetClockInfo(nvml.CLOCK_GRAPHICS)
	if ret == nvml.SUCCESS {
		metrics.GPUClock = int(gpuClock)
	} else {
		fmt.Printf("[NVML] Failed to get GPU clock: %v\n", ret)
	}

	// Частота памяти
	memClock, ret := device.GetClockInfo(nvml.CLOCK_MEM)
	if ret == nvml.SUCCESS {
		metrics.MemClock = int(memClock)
	} else {
		fmt.Printf("[NVML] Failed to get memory clock: %v\n", ret)
	}

	return metrics
}
