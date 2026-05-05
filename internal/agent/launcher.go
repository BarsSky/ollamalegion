package agent

import (
	"fmt"
	"math"
	"runtime"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// LaunchConfig описывает один вариант конфигурации запуска Ollama.
type LaunchConfig struct {
	Label       string            `json:"label"`       // Человекочитаемое описание: "Оптимальный", "Скоростной", "Экономный"
	Type        string            `json:"type"`        // "optimal", "speed", "quality"
	IsOptimal   bool              `json:"isOptimal"`   // Является ли этот вариант оптимальным
	EnvVars     map[string]string `json:"envVars"`     // Переменные окружения для запуска Ollama
	Description string            `json:"description"` // Пояснение выбора параметров
	Limitations []string          `json:"limitations"` // Ограничения данного варианта (если есть)
}

// HardwareProfile содержит характеристики оборудования, на котором запускается Ollama.
type HardwareProfile struct {
	GPUCount       int     // Количество GPU
	VRAMTotalMB    uint64  // Суммарный VRAM в MB
	VRAMPerGPU     []uint64 // VRAM на каждый GPU
	RAMTotalMB     uint64  // Системная RAM в MB
	RAMFreeMB      uint64  // Свободная RAM
	CPUCores       int     // Количество ядер CPU
	CPUThreads     int     // Количество потоков CPU
	HasAVX2        bool    // Поддержка AVX2
	HasAVX512      bool    // Поддержка AVX-512
	GPUComputeCap  float64 // Compute Capability основной GPU (0 если неизвестно)
	OSType         string  // "linux", "windows", "darwin"
}

// LaunchAnalyzer анализирует оборудование и генерирует варианты конфигурации запуска Ollama.
type LaunchAnalyzer struct {
	profile HardwareProfile
	metrics *types.BackendMetrics // Текущие метрики бэкенда (если агент уже подключён)
}

// NewLaunchAnalyzer создаёт новый анализатор на основе метрик агента.
func NewLaunchAnalyzer(metrics *types.BackendMetrics) *LaunchAnalyzer {
	la := &LaunchAnalyzer{
		metrics: metrics,
	}
	la.profile = la.buildProfile(metrics)
	return la
}

// buildProfile строит профиль оборудования из метрик.
func (la *LaunchAnalyzer) buildProfile(metrics *types.BackendMetrics) HardwareProfile {
	p := HardwareProfile{
		OSType: runtime.GOOS,
	}

	// GPU
	if metrics != nil && metrics.GPU.MemoryTotal > 0 {
		p.GPUCount = 1
		// Оцениваем количество GPU по суммарной VRAM (грубая оценка)
		if metrics.GPU.MemoryTotal > 32768 {
			p.GPUCount = 2
		}
		if metrics.GPU.MemoryTotal > 65536 {
			p.GPUCount = 4
		}
		p.VRAMTotalMB = metrics.GPU.MemoryTotal
		p.VRAMPerGPU = append(p.VRAMPerGPU, metrics.GPU.MemoryTotal/uint64(p.GPUCount))

		// Compute Capability — для RTX 30xx+ это >= 8.0, для более старых < 8.0
		// Консервативно предполагаем 7.5 (поддержка Flash Attention требует >= 8.0)
		p.GPUComputeCap = 7.5

		// Системная память
		p.RAMTotalMB = metrics.System.MemoryTotal
		if metrics.System.MemoryFree > 0 {
			p.RAMFreeMB = metrics.System.MemoryFree
		}
	}

	// CPU (дополняем из runtime)
	p.CPUCores = runtime.NumCPU()
	p.CPUThreads = runtime.NumCPU() // runtime.NumCPU возвращает логические потоки

	// Определяем поддержку AVX2/AVX-512 (упрощённо, через runtime.GOARCH и OS)
	// На amd64 вероятно есть AVX2, точное определение — через CPUID в рантайме
	if runtime.GOARCH == "amd64" {
		p.HasAVX2 = true // Консервативно считаем что на amd64 AVX2 есть
	}
	if runtime.GOARCH == "amd64" && (runtime.GOOS == "linux" || runtime.GOOS == "windows") {
		// AVX-512 есть на большинстве серверных CPU начиная с Skylake-X
		// Точное определение требует CPUID — для простоты считаем что может быть
		p.HasAVX512 = false // Консервативно
	}

	// Если нет GPU, ставим разумные значения
	if p.GPUCount == 0 {
		p.GPUCount = 0
		p.VRAMTotalMB = 0
	}

	// Размер RAM если не определён из метрик
	if p.RAMTotalMB == 0 {
		p.RAMTotalMB = 8192 // Предполагаем 8GB по умолчанию
	}

	return p
}

// Analyze генерирует все варианты конфигурации запуска.
// Возвращает три конфигурации: оптимальную, скоростную и экономную.
func (la *LaunchAnalyzer) Analyze() []LaunchConfig {
	configs := make([]LaunchConfig, 0, 3)

	// Строим оптимальную конфигурацию
	optimal := la.buildOptimalConfig()
	optimal.IsOptimal = true
	configs = append(configs, optimal)

	// Строим скоростную конфигурацию
	speed := la.buildSpeedConfig()
	speed.IsOptimal = false
	configs = append(configs, speed)

	// Строим экономную конфигурацию
	quality := la.buildQualityConfig()
	quality.IsOptimal = false
	configs = append(configs, quality)

	return configs
}

// buildOptimalConfig строит оптимальную (сбалансированную) конфигурацию.
func (la *LaunchAnalyzer) buildOptimalConfig() LaunchConfig {
	p := la.profile
	env := make(map[string]string)
	descParts := []string{}
	limitations := []string{}

	// === OLLAMA_NUM_PARALLEL ===
	// Оптимально: ~2-4 параллельных запроса на GPU, но не более 8
	numParallel := 1
	if p.GPUCount > 0 && p.VRAMTotalMB >= 8192 {
		numParallel = 4
	} else if p.GPUCount > 0 && p.VRAMTotalMB >= 4096 {
		numParallel = 2
	} else if p.CPUCores >= 8 {
		numParallel = 2
	}
	env["OLLAMA_NUM_PARALLEL"] = fmt.Sprintf("%d", numParallel)
	descParts = append(descParts, fmt.Sprintf("Параллельных запросов: %d", numParallel))

	// === OLLAMA_MAX_LOADED_MODELS ===
	// Держим 1-2 модели в VRAM для быстрого переключения
	maxModels := 1
	if p.VRAMTotalMB >= 16384 {
		maxModels = 3
	} else if p.VRAMTotalMB >= 8192 {
		maxModels = 2
	}
	env["OLLAMA_MAX_LOADED_MODELS"] = fmt.Sprintf("%d", maxModels)
	descParts = append(descParts, fmt.Sprintf("Моделей в VRAM: до %d", maxModels))

	// === OLLAMA_FLASH_ATTENTION ===
	if p.GPUComputeCap >= 8.0 {
		env["OLLAMA_FLASH_ATTENTION"] = "1"
		descParts = append(descParts, "Flash Attention: включено")
	} else if p.GPUCount > 0 {
		limitations = append(limitations, "GPU не поддерживает Flash Attention (требуется CC >= 8.0)")
	}

	// === OLLAMA_KV_CACHE_TYPE ===
	if p.VRAMTotalMB >= 12288 {
		env["OLLAMA_KV_CACHE_TYPE"] = "f16"
		descParts = append(descParts, "KV Cache: f16 (точный)")
	} else if p.VRAMTotalMB >= 6144 {
		env["OLLAMA_KV_CACHE_TYPE"] = "q8_0"
		descParts = append(descParts, "KV Cache: q8_0 (баланс)")
	} else {
		env["OLLAMA_KV_CACHE_TYPE"] = "q4_0"
		descParts = append(descParts, "KV Cache: q4_0 (эконом)")
	}

	// === OLLAMA_GPU_LAYERS ===
	if p.GPUCount > 0 {
		// Автоматически: -1 означает все слои на GPU
		env["OLLAMA_GPU_LAYERS"] = "-1"
		descParts = append(descParts, "GPU Layers: все (авто)")
	} else {
		env["OLLAMA_GPU_LAYERS"] = "0"
		descParts = append(descParts, "GPU Layers: CPU-only")
		limitations = append(limitations, "Инференс только на CPU — скорость будет низкой")
	}

	// === OLLAMA_MMAP ===
	if p.RAMTotalMB >= 16384 {
		env["OLLAMA_MMAP"] = "1"
	} else if p.RAMTotalMB < 4096 {
		env["OLLAMA_MMAP"] = "0"
		limitations = append(limitations, "MMAP отключён из-за малого объёма RAM")
	}

	// === OLLAMA_NUM_THREADS ===
	// Используем 75% доступных потоков для баланса
	threads := int(math.Max(1, float64(p.CPUThreads)*0.75))
	env["OLLAMA_NUM_THREADS"] = fmt.Sprintf("%d", threads)
	descParts = append(descParts, fmt.Sprintf("Потоков CPU: %d", threads))

	// === OLLAMA_CONTEXT_LENGTH ===
	// Стандартный контекст 4096 для баланса
	env["OLLAMA_CONTEXT_LENGTH"] = "4096"

	return LaunchConfig{
		Label:       "Оптимальный (сбалансированный)",
		Type:        "optimal",
		EnvVars:     env,
		Description: "Сбалансированная конфигурация: " + strings.Join(descParts, "; "),
		Limitations: limitations,
	}
}

// buildSpeedConfig строит скоростную конфигурацию (упор на скорость).
func (la *LaunchAnalyzer) buildSpeedConfig() LaunchConfig {
	p := la.profile
	env := make(map[string]string)
	descParts := []string{}
	limitations := []string{}

	// === OLLAMA_NUM_PARALLEL ===
	numParallel := 1
	if p.GPUCount > 0 && p.VRAMTotalMB >= 16384 {
		numParallel = 8
	} else if p.GPUCount > 0 && p.VRAMTotalMB >= 8192 {
		numParallel = 6
	} else if p.GPUCount > 0 && p.VRAMTotalMB >= 4096 {
		numParallel = 4
	} else {
		numParallel = 2
		limitations = append(limitations, "Без GPU максимальный параллелизм ограничен")
	}
	env["OLLAMA_NUM_PARALLEL"] = fmt.Sprintf("%d", numParallel)
	descParts = append(descParts, fmt.Sprintf("Параллельных запросов: %d (макс.)", numParallel))

	// === OLLAMA_MAX_LOADED_MODELS ===
	maxModels := 1
	if p.VRAMTotalMB >= 24576 {
		maxModels = 4
	} else if p.VRAMTotalMB >= 16384 {
		maxModels = 3
	} else if p.VRAMTotalMB >= 8192 {
		maxModels = 2
	}
	env["OLLAMA_MAX_LOADED_MODELS"] = fmt.Sprintf("%d", maxModels)
	descParts = append(descParts, fmt.Sprintf("Моделей в VRAM: до %d", maxModels))

	// === OLLAMA_FLASH_ATTENTION ===
	if p.GPUComputeCap >= 8.0 {
		env["OLLAMA_FLASH_ATTENTION"] = "1"
		descParts = append(descParts, "Flash Attention: включено")
	} else {
		limitations = append(limitations, "Flash Attention недоступен на данном GPU")
	}

	// === OLLAMA_KV_CACHE_TYPE ===
	env["OLLAMA_KV_CACHE_TYPE"] = "f16"
	descParts = append(descParts, "KV Cache: f16 (макс. точность)")
	if p.VRAMTotalMB < 12288 {
		limitations = append(limitations, "f16 KV Cache требует много VRAM — возможен OOM на больших моделях")
	}

	// === OLLAMA_GPU_LAYERS ===
	if p.GPUCount > 0 {
		env["OLLAMA_GPU_LAYERS"] = "-1"
	} else {
		env["OLLAMA_GPU_LAYERS"] = "0"
	}

	// === OLLAMA_MMAP ===
	env["OLLAMA_MMAP"] = "1"

	// === OLLAMA_NUM_THREADS ===
	threads := p.CPUThreads // Все потоки
	if threads < 1 {
		threads = 1
	}
	env["OLLAMA_NUM_THREADS"] = fmt.Sprintf("%d", threads)
	descParts = append(descParts, fmt.Sprintf("Потоков CPU: %d (все)", threads))

	// === OLLAMA_CONTEXT_LENGTH ===
	env["OLLAMA_CONTEXT_LENGTH"] = "4096"

	return LaunchConfig{
		Label:       "Скоростной (макс. пропускная способность)",
		Type:        "speed",
		EnvVars:     env,
		Description: "Упор на скорость и параллелизм: " + strings.Join(descParts, "; "),
		Limitations: limitations,
	}
}

// buildQualityConfig строит экономную конфигурацию (упор на качество/размышления).
func (la *LaunchAnalyzer) buildQualityConfig() LaunchConfig {
	p := la.profile
	env := make(map[string]string)
	descParts := []string{}
	limitations := []string{}

	// === OLLAMA_NUM_PARALLEL ===
	// Меньше параллелизма — больше ресурсов на один запрос
	numParallel := 1
	if p.GPUCount > 0 && p.VRAMTotalMB >= 16384 {
		numParallel = 2
	}
	env["OLLAMA_NUM_PARALLEL"] = fmt.Sprintf("%d", numParallel)
	descParts = append(descParts, fmt.Sprintf("Параллельных запросов: %d (мин.)", numParallel))
	if numParallel == 1 && p.CPUCores >= 4 {
		limitations = append(limitations, "Однопоточный режим — другие клиенты будут ждать в очереди")
	}

	// === OLLAMA_MAX_LOADED_MODELS ===
	// Меньше моделей — больше контекста для активной
	maxModels := 1
	env["OLLAMA_MAX_LOADED_MODELS"] = fmt.Sprintf("%d", maxModels)
	descParts = append(descParts, "Моделей в VRAM: 1 (все ресурсы одной модели)")

	// === OLLAMA_FLASH_ATTENTION ===
	if p.GPUComputeCap >= 8.0 {
		env["OLLAMA_FLASH_ATTENTION"] = "1"
	}

	// === OLLAMA_KV_CACHE_TYPE ===
	// f16 для максимального качества при достаточном VRAM
	if p.VRAMTotalMB >= 24576 {
		env["OLLAMA_KV_CACHE_TYPE"] = "f16"
	} else {
		env["OLLAMA_KV_CACHE_TYPE"] = "q8_0"
	}
	descParts = append(descParts, fmt.Sprintf("KV Cache: %s (качество)", env["OLLAMA_KV_CACHE_TYPE"]))

	// === OLLAMA_GPU_LAYERS ===
	if p.GPUCount > 0 {
		env["OLLAMA_GPU_LAYERS"] = "-1"
	} else {
		env["OLLAMA_GPU_LAYERS"] = "0"
	}

	// === OLLAMA_MMAP ===
	env["OLLAMA_MMAP"] = "1"

	// === OLLAMA_NUM_THREADS ===
	threads := int(math.Max(1, float64(p.CPUThreads)*0.5))
	env["OLLAMA_NUM_THREADS"] = fmt.Sprintf("%d", threads)
	descParts = append(descParts, fmt.Sprintf("Потоков CPU: %d (умеренно)", threads))

	// === OLLAMA_CONTEXT_LENGTH ===
	// Увеличенный контекст для размышлений
	env["OLLAMA_CONTEXT_LENGTH"] = "8192"
	descParts = append(descParts, "Контекст: 8192 токенов (длинные размышления)")
	if p.VRAMTotalMB < 8192 && p.GPUCount > 0 {
		limitations = append(limitations, "Длинный контекст (8K) требует много VRAM — возможен OOM")
	}

	return LaunchConfig{
		Label:       "Экономный (упор на размышления/качество)",
		Type:        "quality",
		EnvVars:     env,
		Description: "Упор на качество и длинные размышления: " + strings.Join(descParts, "; "),
		Limitations: limitations,
	}
}

// GetHardwareProfile возвращает профиль оборудования.
func (la *LaunchAnalyzer) GetHardwareProfile() HardwareProfile {
	return la.profile
}

// CheckIfOptimal сравнивает текущие настройки (из переменных окружения или метрик)
// с оптимальной конфигурацией и возвращает true, если они совпадают.
func (la *LaunchAnalyzer) CheckIfOptimal(currentEnv map[string]string) bool {
	optimal := la.buildOptimalConfig()
	keysToCheck := []string{
		"OLLAMA_NUM_PARALLEL",
		"OLLAMA_MAX_LOADED_MODELS",
		"OLLAMA_KV_CACHE_TYPE",
		"OLLAMA_GPU_LAYERS",
		"OLLAMA_NUM_THREADS",
	}

	for _, key := range keysToCheck {
		if optimal.EnvVars[key] != currentEnv[key] {
			return false
		}
	}
	return true
}