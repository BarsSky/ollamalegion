// vram_detect.go — определение доступной VRAM на текущем GPU.
//
// Используется в auto_offload для расчёта оптимального числа GPU-слоёв.
//
// Три стратегии (от лучшей к худшей):
//  1. bridge.GetGPUInfo() — если C-bridge возвращает данные о GPU.
//  2. nvidia-smi через exec — fallback для host без C-bridge (например,
//     при разработке/тестировании на Linux без CUDA toolchain).
//  3. ENV CPPWORKER_VRAM_BYTES — override для тестов/CI.
//
// Возвращает 0 если ничего не удалось (auto_offload пропускает модель).
package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// availableVRAMBytes возвращает размер доступной VRAM в байтах.
// Использует стратегии по приоритету (см. файл-комментарий).
// Возвращает 0 если не удалось определить.
func availableVRAMBytes() int64 {
	// Стратегия 3: ENV override (для тестов и CI)
	if v := os.Getenv("CPPWORKER_VRAM_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}

	// Стратегия 1: bridge.GetGPUInfo (доступна в обоих build — real и stub,
	// но в stub возвращает dummy 0)
	if v := tryBridgeGPUInfo(); v > 0 {
		return v
	}

	// Стратегия 2: nvidia-smi через exec (Linux/Windows с NVIDIA драйверами)
	if v := tryNvidiaSMI(); v > 0 {
		return v
	}

	logger.Get().Debugw("availableVRAMBytes: all strategies failed, returning 0")
	return 0
}

// tryBridgeGPUInfo — попытка получить VRAM через C-bridge.
// Возвращает 0 если функция недоступна (stub build или ошибка).
func tryBridgeGPUInfo() int64 {
	defer func() {
		if r := recover(); r != nil {
			logger.Get().Debugw("tryBridgeGPUInfo: panic (stub build?)", "panic", r)
		}
	}()
	// Получаем инфо о GPU 0. Если multi-GPU — берём первый (для auto_offload
	// это упрощение; multi-GPU требует tensor-split, который рассчитывается
	// отдельно в cppbackend).
	info, err := bridge.GetGPUInfo(0)
	if err != nil || info == nil {
		return 0
	}
	// VRAMTotalMB в bridge.GPUDevice — uint64 мегабайты, конвертируем в байты.
	if info.VRAMTotalMB > 0 {
		return int64(info.VRAMTotalMB) * 1024 * 1024
	}
	return 0
}

// tryNvidiaSMI — fallback через nvidia-smi CLI.
//
// Парсит "MiB" из вывода `nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits`.
// Возвращает байты.
func tryNvidiaSMI() int64 {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return 0
	}
	out, err := exec.Command(path, "--query-gpu=memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	mib, err := strconv.ParseInt(line, 10, 64)
	if err != nil || mib <= 0 {
		return 0
	}
	return mib * 1024 * 1024
}

// freeVRAMBytes возвращает размер СВОБОДНОЙ VRAM в байтах.
//
// В отличие от availableVRAMBytes (которая возвращает TOTAL VRAM),
// эта функция возвращает свободную VRAM — то, что осталось после загрузки
// модели. Используется AutoTuneNCtx (модель уже загружена) и
// calculateOptimalGPULayersForModel при reload.
//
// Стратегии (от лучшей к худшей):
//  1a. ENV CPPWORKER_FREE_VRAM_BYTES — override для free VRAM (тесты/CI)
//  1b. ENV CPPWORKER_VRAM_BYTES — override для total VRAM (тесты/CI)
//      Приоритет выше nvidia-smi: ENV — явная инструкция оператора/теста.
//      На хосте с загруженной моделью nvidia-smi вернёт маленькое значение
//      свободной VRAM, но тесты задают CPPWORKER_VRAM_BYTES как override.
//  2. bridge.GetGPUInfo() → VRAMFreeMB (если C-bridge возвращает free VRAM)
//  3. nvidia-smi --query-gpu=memory.free — fallback через CLI
//
// Возвращает 0 если ничего не удалось определить.
func freeVRAMBytes() int64 {
	// Стратегия 1a: ENV override для free VRAM (тесты/CI).
	if v := os.Getenv("CPPWORKER_FREE_VRAM_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}

	// Стратегия 1b: ENV override для total VRAM (CPPWORKER_VRAM_BYTES).
	// Приоритет выше nvidia-smi: ENV — явная инструкция оператора/теста.
	// На хосте с загруженной моделью nvidia-smi вернёт маленькое значение
	// свободной VRAM, но тесты задают CPPWORKER_VRAM_BYTES как override.
	if v := os.Getenv("CPPWORKER_VRAM_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}

	// Стратегия 2: bridge.GetGPUInfo → VRAMFreeMB
	if v := tryBridgeFreeVRAM(); v > 0 {
		return v
	}

	// Стратегия 3: nvidia-smi --query-gpu=memory.free
	if v := tryNvidiaSMIFree(); v > 0 {
		return v
	}

	logger.Get().Debugw("freeVRAMBytes: all strategies failed, returning 0")
	return 0
}

// tryBridgeFreeVRAM — попытка получить свободную VRAM через C-bridge.
// Возвращает 0 если функция недоступна (stub build) или VRAMFreeMB не задан.
func tryBridgeFreeVRAM() int64 {
	defer func() {
		if r := recover(); r != nil {
			logger.Get().Debugw("tryBridgeFreeVRAM: panic (stub build?)", "panic", r)
		}
	}()
	info, err := bridge.GetGPUInfo(0)
	if err != nil || info == nil {
		return 0
	}
	// VRAMFreeMB в bridge.GPUDevice — uint64 мегабайты.
	// При stub-сборке может быть 0 — в этом случае fallback на availableVRAMBytes.
	if info.VRAMFreeMB > 0 {
		return int64(info.VRAMFreeMB) * 1024 * 1024
	}
	// Если VRAMFreeMB == 0, но VRAMTotalMB > 0 — возможно, bridge
	// не поддерживает free (старая версия). Возвращаем 0, чтобы
	// caller мог fallback на availableVRAMBytes.
	return 0
}

// tryNvidiaSMIFree — fallback через nvidia-smi CLI для свободной VRAM.
// Парсит "MiB" из вывода `nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits`.
func tryNvidiaSMIFree() int64 {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return 0
	}
	out, err := exec.Command(path, "--query-gpu=memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	mib, err := strconv.ParseInt(line, 10, 64)
	if err != nil || mib <= 0 {
		return 0
	}
	return mib * 1024 * 1024
}

// availableRAMBytes возвращает размер доступной RAM в байтах.
//
// Используется AutoTuneNCtx для проверки, помещаются ли в RAM оставшиеся
// (после GPU offload) веса модели. При partial offload (gpu_layers < NLayers)
// часть тензоров остаётся в RAM через mmap — нужно убедиться, что RAM
// достаточно для всех не-GPU слоёв.
//
// Стратегии (от лучшей к худшей):
//  1. ENV CPPWORKER_AVAILABLE_RAM_BYTES (override для тестов и CI)
//  2. /proc/meminfo "MemAvailable" (Linux) — реальная доступная RAM
//  3. sysctl hw.memsize (macOS)
//  4. globalmemorystatexs (Windows) — fallback
//
// Возвращает 0 если ничего не удалось определить (AutoTuneNCtx тогда
// пропустит проверку RAM).
func availableRAMBytes() int64 {
	// Стратегия 1: ENV override (тесты, CI, non-standard окружения).
	if v := os.Getenv("CPPWORKER_AVAILABLE_RAM_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}

	// Стратегия 2: Linux /proc/meminfo MemAvailable.
	data, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemAvailable:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 {
						return kb * 1024
					}
				}
			}
		}
		// Если MemAvailable нет, попробуем MemFree + Cached.
		var memFree, cached int64
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemFree:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 {
						memFree = kb * 1024
					}
				}
			} else if strings.HasPrefix(line, "Cached:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 {
						cached = kb * 1024
					}
				}
			}
		}
		if memFree+cached > 0 {
			return memFree + cached
		}
	}

	// Стратегия 3: macOS sysctl.
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && n > 0 {
			return n
		}
	}

	// Стратегия 4: ничего не помогло — возвращаем 0 (AutoTuneNCtx пропустит RAM-проверку).
	logger.Get().Debugw("availableRAMBytes: all strategies failed, returning 0")
	return 0
}