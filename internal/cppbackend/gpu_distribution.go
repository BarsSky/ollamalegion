// Package cppbackend — multi-GPU распределение для llama.cpp
//
// GPUManager отвечает за:
// - Автоматическое распределение моделей по GPU
// - Расчёт tensor split пропорций на основе VRAM
// - Выбор оптимального GPU для новой модели
// - Мониторинг занятой VRAM с учётом загруженных моделей
package cppbackend

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// GPUManager управляет multi-GPU распределением
type GPUManager struct {
	mu         sync.RWMutex
	devices    []bridge.GPUDevice
	strategy   string // "vram-ratio", "manual", "round-robin"

	// Отслеживание загрузки GPU
	gpuUsage map[int]uint64 // gpuIndex → estimatedUsedMB (включая загруженные модели)
}

// NewGPUManager создаёт новый GPU менеджер
func NewGPUManager(strategy string) *GPUManager {
	return &GPUManager{
		devices:  make([]bridge.GPUDevice, 0),
		strategy: strategy,
		gpuUsage: make(map[int]uint64),
	}
}

// InitGPU инициализирует GPU менеджер с информацией о GPU
func (m *GPUManager) InitGPU(devices []bridge.GPUDevice) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.devices = make([]bridge.GPUDevice, len(devices))
	copy(m.devices, devices)

	// Инициализируем usage нулями
	for _, dev := range devices {
		m.gpuUsage[dev.Index] = 0
	}

	log := logger.Get()
	log.Infow("GPUManager initialized",
		"strategy", m.strategy,
		"gpuCount", len(devices))
	for _, dev := range devices {
		log.Infow("GPU available",
			"index", dev.Index,
			"name", dev.Name,
			"vramTotalMB", dev.VRAMTotalMB,
			"vramFreeMB", dev.VRAMFreeMB)
	}
}

// GetDeviceCount возвращает количество GPU
func (m *GPUManager) GetDeviceCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.devices)
}

// GetDevices возвращает список GPU
func (m *GPUManager) GetDevices() []bridge.GPUDevice {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]bridge.GPUDevice, len(m.devices))
	copy(result, m.devices)
	return result
}

// ============================================================
// Tensor Split Calculation
// ============================================================

// TensorSplit представляет рассчитанные пропорции для multi-GPU
type TensorSplit struct {
	Ratios    []float32 // пропорции для каждого GPU (сумма = 1.0)
	MainGPU   int       // индекс главного GPU
	GPUCount  int       // количество используемых GPU
	NGPULayers int      // количество слоёв на GPU
}

// CalculateTensorSplit рассчитывает tensor split пропорции на основе стратегии
func (m *GPUManager) CalculateTensorSplit(totalVRAMEstimateMB uint64) TensorSplit {
	m.mu.RLock()
	defer m.mu.RUnlock()

	deviceCount := len(m.devices)
	if deviceCount == 0 {
		return TensorSplit{
			Ratios:      []float32{1.0},
			MainGPU:     0,
			GPUCount:    1,
			NGPULayers: -1, // все на CPU (нет GPU)
		}
	}

	switch m.strategy {
	case "manual":
		return m.calculateManual()
	case "round-robin":
		return m.calculateRoundRobin()
	default: // "vram-ratio" (default)
		return m.calculateVRAMRatio(totalVRAMEstimateMB)
	}
}

// calculateVRAMRatio рассчитывает пропорции на основе VRAM
func (m *GPUManager) calculateVRAMRatio(totalVRAMEstimateMB uint64) TensorSplit {
	if len(m.devices) == 0 {
		return TensorSplit{Ratios: []float32{1.0}, MainGPU: 0, GPUCount: 1, NGPULayers: 0}
	}

	// Сортируем GPU по свободной VRAM (с учётом уже занятой)
	type gpuInfo struct {
		index   int
		freeMB  uint64
	}
	gpus := make([]gpuInfo, 0, len(m.devices))
	totalFree := uint64(0)

	for _, dev := range m.devices {
		used := m.gpuUsage[dev.Index]
		free := uint64(0)
		if dev.VRAMTotalMB > used {
			free = dev.VRAMTotalMB - used
		}
		gpus = append(gpus, gpuInfo{index: dev.Index, freeMB: free})
		totalFree += free
	}

	if totalFree == 0 {
		// Все GPU заполнены — используем равномерное распределение
		ratio := 1.0 / float64(len(m.devices))
		ratios := make([]float32, len(m.devices))
		for i := range ratios {
			ratios[i] = float32(ratio)
		}
		return TensorSplit{
			Ratios:      ratios,
			MainGPU:     m.devices[0].Index,
			GPUCount:    len(m.devices),
			NGPULayers: -1,
		}
	}

	// Сортируем по убыванию свободной VRAM
	sort.Slice(gpus, func(i, j int) bool {
		return gpus[i].freeMB > gpus[j].freeMB
	})

	// Рассчитываем пропорции как долю свободной VRAM каждого GPU
	ratios := make([]float32, len(m.devices))
	totalFreeFloat := float64(totalFree)
	for i, gpu := range gpus {
		ratio := float64(gpu.freeMB) / totalFreeFloat
		if ratio < 0.05 {
			ratio = 0.05 // минимум 5% для edge cases
		}
		ratios[i] = float32(ratio)
	}

	// Нормализуем до суммы = 1.0
	sum := float32(0)
	for _, r := range ratios {
		sum += r
	}
	for i := range ratios {
		ratios[i] /= sum
	}

	logger.Get().Infow("tensor split calculated (vram-ratio)",
		"gpuCount", len(gpus),
		"ratios", ratios,
		"totalFreeMB", totalFree,
		"mainGPU", gpus[0].index)

	return TensorSplit{
		Ratios:      ratios,
		MainGPU:     gpus[0].index,
		GPUCount:    len(m.devices),
		NGPULayers: -1,
	}
}

// calculateManual возвращает ручное распределение (равномерное как fallback)
func (m *GPUManager) calculateManual() TensorSplit {
	n := len(m.devices)
	if n == 0 {
		return TensorSplit{Ratios: []float32{1.0}, MainGPU: 0, GPUCount: 1, NGPULayers: 0}
	}

	ratio := 1.0 / float64(n)
	ratios := make([]float32, n)
	for i := range ratios {
		ratios[i] = float32(ratio)
	}

	return TensorSplit{
		Ratios:      ratios,
		MainGPU:     0,
		GPUCount:    n,
		NGPULayers: -1,
	}
}

// calculateRoundRobin распределяет по кругу
func (m *GPUManager) calculateRoundRobin() TensorSplit {
	n := len(m.devices)
	if n == 0 {
		return TensorSplit{Ratios: []float32{1.0}, MainGPU: 0, GPUCount: 1, NGPULayers: 0}
	}

	// Выбираем GPU с наименьшей загрузкой как main
	mainIdx := 0
	minUsage := uint64(math.MaxUint64)
	for i, dev := range m.devices {
		usage := m.gpuUsage[dev.Index]
		if usage < minUsage {
			minUsage = usage
			mainIdx = i
		}
	}

	// Равномерное распределение по всем GPU
	ratio := 1.0 / float64(n)
	ratios := make([]float32, n)
	for i := range ratios {
		ratios[i] = float32(ratio)
	}

	return TensorSplit{
		Ratios:      ratios,
		MainGPU:     mainIdx,
		GPUCount:    n,
		NGPULayers: -1,
	}
}

// ============================================================
// GPU Selection
// ============================================================

// SelectGPUForModel выбирает лучший GPU для загрузки модели
// Возвращает индекс GPU
func (m *GPUManager) SelectGPUForModel(estimatedVRAMMB uint64) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.devices) == 0 {
		return 0 // CPU mode
	}

	type gpuScore struct {
		index int
		score float64
	}

	scores := make([]gpuScore, 0, len(m.devices))
	for _, dev := range m.devices {
		used := m.gpuUsage[dev.Index]
		available := uint64(0)
		if dev.VRAMTotalMB > used {
			available = dev.VRAMTotalMB - used
		}

		// Score: чем больше свободной VRAM, тем выше score
		// Если модель не влезает — штрафуем
		score := float64(available)
		if available < estimatedVRAMMB && estimatedVRAMMB > 0 {
			// Модель не влезает целиком — сильно штрафуем
			score *= 0.1
		}
		scores = append(scores, gpuScore{index: dev.Index, score: score})
	}

	// Выбираем GPU с максимальным score
	bestIdx := scores[0].index
	bestScore := scores[0].score
	for _, s := range scores[1:] {
		if s.score > bestScore {
			bestScore = s.score
			bestIdx = s.index
		}
	}

	return bestIdx
}

// ============================================================
// GPU Usage Tracking
// ============================================================

// ReserveGPUMemory резервирует VRAM для модели на указанном GPU
func (m *GPUManager) ReserveGPUMemory(gpuIndex int, sizeMB uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gpuUsage[gpuIndex] += sizeMB
}

// ReleaseGPUMemory освобождает VRAM
func (m *GPUManager) ReleaseGPUMemory(gpuIndex int, sizeMB uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	usage := m.gpuUsage[gpuIndex]
	if usage >= sizeMB {
		m.gpuUsage[gpuIndex] = usage - sizeMB
	} else {
		m.gpuUsage[gpuIndex] = 0
	}
}

// GetGPUUsage возвращает текущую загрузку VRAM по GPU
func (m *GPUManager) GetGPUUsage() map[int]uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[int]uint64, len(m.gpuUsage))
	for k, v := range m.gpuUsage {
		result[k] = v
	}
	return result
}

// GetFreeVRAM возвращает свободную VRAM для каждого GPU
func (m *GPUManager) GetFreeVRAM() map[int]uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[int]uint64, len(m.devices))
	for _, dev := range m.devices {
		used := m.gpuUsage[dev.Index]
		free := dev.VRAMTotalMB
		if free > used {
			free -= used
		} else {
			free = 0
		}
		result[dev.Index] = free
	}
	return result
}

// GetTotalVRAM возвращает общую VRAM
func (m *GPUManager) GetTotalVRAM() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var total uint64
	for _, dev := range m.devices {
		total += dev.VRAMTotalMB
	}
	return total
}

// GetVRAMSummary возвращает сводку по VRAM
func (m *GPUManager) GetVRAMSummary() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	devices := make([]map[string]interface{}, 0, len(m.devices))
	var total uint64
	var used uint64

	for _, dev := range m.devices {
		usage := m.gpuUsage[dev.Index]
		total += dev.VRAMTotalMB
		used += usage

		devices = append(devices, map[string]interface{}{
			"index":      dev.Index,
			"name":       dev.Name,
			"vramTotalMB": dev.VRAMTotalMB,
			"vramUsedMB": usage,
			"vramFreeMB": func() uint64 {
				if dev.VRAMTotalMB > usage {
					return dev.VRAMTotalMB - usage
				}
				return 0
			}(),
		})
	}

	return map[string]interface{}{
		"gpuCount":  len(m.devices),
		"vramTotalMB": total,
		"vramUsedMB":  used,
		"vramFreeMB":  total - used,
		"devices":   devices,
		"strategy":  m.strategy,
	}
}

// EstimateModelVRAM оценивает VRAM для модели на одном GPU
// Использует ту же эвристику что EstimateGPUMemoryForModel (теперь с ctxSize + compute buffer)
func EstimateModelVRAM(sizeBytes int64, gpuLayers int, totalLayers int, ctxSize int) uint64 {
	// 2026-06-26: новая сигнатура EstimateGPUMemoryForModel уже учитывает ctxSize
	// через ctxMemoryMB (KV-cache) и computeBufferMB (1 GB overhead).
	// Дополнительной поправки больше не требуется.
	base := EstimateGPUMemoryForModel(sizeBytes, gpuLayers, totalLayers, ctxSize)
	if base == 0 {
		return 0
	}

	// ctxOverhead оставлен для обратной совместимости с legacy-вызовами
	// (если базовая оценка когда-то изменится обратно на hardcoded ctx=512MB).
	// Сейчас ctxOverhead = 0, потому что база уже включает ctxMemoryMB.
	ctxOverhead := uint64(0)

	return base + ctxOverhead
}

// ============================================================
// String representation
// ============================================================

func (ts TensorSplit) String() string {
	return fmt.Sprintf("TensorSplit{GPUs=%d, MainGPU=%d, Ratios=%v, GPULayers=%d}",
		ts.GPUCount, ts.MainGPU, ts.Ratios, ts.NGPULayers)
}
