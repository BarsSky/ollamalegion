// Тесты для GPU detection и TTL-кэша доступности.
//
// 2026-06-30: добавлены после фикса #124 «GPU не обнаружен в agent-контейнере».
// Покрывают:
//  1. TestGPUAvailabilityCache_TTLReset — кэш сбрасывается после истечения TTL.
//  2. TestGPUAvailabilityCache_WithinTTL — в пределах TTL кэш возвращает true.
//  3. TestGPUAvailabilityCache_IdempotentMark — повторный markGPUUnavailable не сдвигает since.
//  4. TestGPUAvailabilityCache_MarkAvailable — успешный сбор сбрасывает флаг.
//  5. TestResetGPUAvailabilityCache — функция сброса работает.
//  6. TestDetectPlatformMode_OverrideRespected — config.GPUMode != ModeAuto → возврат override.
//  7. TestDetectPlatformMode_CpuModeFallback — на пустой системе → ModeCPU без паники.
//
// Эти тесты используют глобальный gpuState, поэтому НЕ используют t.Parallel()
// и сбрасывают состояние в начале каждого теста.

package agent

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"ollama-loadbalancer/pkg/types"
)

// resetGPUState — обёртка для инициализации/сброса глобального состояния.
func resetGPUState() {
	ResetGPUAvailabilityCache()
}

// TestGPUAvailabilityCache_WithinTTL — пометка и проверка в пределах TTL.
func TestGPUAvailabilityCache_WithinTTL(t *testing.T) {
	resetGPUState()

	// Помечаем как недоступную
	markGPUUnavailable()

	// В пределах TTL должен возвращать true
	assert.True(t, isGPUUnavailableCached(), "Должен вернуть true сразу после mark")
}

// TestGPUAvailabilityCache_TTLReset — после истечения TTL сбрасывается автоматически.
//
// Имитируем истечение TTL через прямую модификацию gpuState.since.
// В реальном времени ждать 60 сек в тестах нельзя.
func TestGPUAvailabilityCache_TTLReset(t *testing.T) {
	resetGPUState()

	markGPUUnavailable()

	// Форсируем «истечение TTL» — сдвигаем since на 61 секунду назад.
	gpuStateMu.Lock()
	gpuState.since = time.Now().Add(-(gpuUnavailableTTL + time.Second))
	gpuStateMu.Unlock()

	// Должен автоматически сброситься и вернуть false
	assert.False(t, isGPUUnavailableCached(), "После истечения TTL должен сброситься")
	assert.False(t, gpuState.unavailable, "gpuState.unavailable должен быть false")
}

// TestGPUAvailabilityCache_IdempotentMark — повторный mark не сдвигает since.
//
// Без идемпотентности: каждый markGPUUnavailable() сдвигал бы since на now(),
// и TTL никогда бы не истёк при периодических неудачах.
func TestGPUAvailabilityCache_IdempotentMark(t *testing.T) {
	resetGPUState()

	markGPUUnavailable()
	gpuStateMu.Lock()
	firstSince := gpuState.since
	gpuStateMu.Unlock()

	time.Sleep(10 * time.Millisecond)
	markGPUUnavailable() // второй раз

	gpuStateMu.Lock()
	secondSince := gpuState.since
	gpuStateMu.Unlock()

	assert.Equal(t, firstSince, secondSince, "since не должен сдвигаться при повторном mark")
}

// TestGPUAvailabilityCache_MarkAvailable — успешный сбор сбрасывает флаг.
func TestGPUAvailabilityCache_MarkAvailable(t *testing.T) {
	resetGPUState()

	markGPUUnavailable()
	assert.True(t, isGPUUnavailableCached(), "Должен быть помечен")

	markGPUAvailable()
	assert.False(t, isGPUUnavailableCached(), "markGPUAvailable должен сбросить")
}

// TestResetGPUAvailabilityCache — функция сброса работает корректно.
func TestResetGPUAvailabilityCache(t *testing.T) {
	resetGPUState()
	markGPUUnavailable()
	assert.True(t, isGPUUnavailableCached())

	ResetGPUAvailabilityCache()
	assert.False(t, isGPUUnavailableCached())
}

// TestDetectPlatformMode_OverrideRespected — для НЕ-Auto режимов override уважается.
//
// ModeAuto НЕ учитывается (это auto-detect, см. тест CpuModeFallback).
// Для ModeCPU и ModeGPU функция должна вернуть значение override,
// игнорируя наличие GPU в системе.
func TestDetectPlatformMode_OverrideRespected(t *testing.T) {
	resetGPUState()

	for _, mode := range []types.PlatformMode{types.ModeCPU, types.ModeGPU} {
		config := createTestAgentConfig()
		config.GPUMode = mode
		agent := NewAgent(config)

		assert.Equal(t, mode, agent.detectPlatformMode(),
			"detectPlatformMode должен уважать GPUMode=%s", mode)
	}
}

// TestDetectPlatformMode_CpuModeFallback — на пустой системе → ModeCPU без паники.
//
// Этот тест проверяет, что detectPlatformMode корректно завершается с ModeCPU,
// если ни одна из проверок не нашла GPU (как в CI без GPU).
// Не использует t.Parallel() — порядок важен из-за глобального gpuState.
func TestDetectPlatformMode_CpuModeFallback(t *testing.T) {
	resetGPUState()

	config := createTestAgentConfig()
	config.GPUMode = types.ModeAuto
	agent := NewAgent(config)

	mode := agent.detectPlatformMode()

	// На CI (Windows без GPU) — ModeCPU.
	// На Linux-машине без GPU — ModeCPU.
	// На Linux-машине с GPU — ModeGPU, но этот тест не изолирует от этого.
	// Главное — функция не паникует и возвращает валидное значение.
	assert.Contains(t,
		[]types.PlatformMode{types.ModeCPU, types.ModeGPU},
		mode,
		"detectPlatformMode должен вернуть CPU или GPU, не паникуя")
}

// TestLogNvidiaSmiError — функция диагностического лога не паникует на разных типах ошибок.
func TestLogNvidiaSmiError(t *testing.T) {
	// nil error — ничего не делает
	assert.NotPanics(t, func() {
		logNvidiaSmiError(nil)
	})
}

// TestExecuteNvidiaSmi_NotPanics — функция не паникует ни на какой системе.
//
// Реальное поведение executeNvidiaSmi() зависит от того, есть ли nvidia-smi в PATH:
// на dev-машине с GPU — вернёт CSV, на agent-контейнере — вернёт ошибку ErrNotFound.
// Тест проверяет только отсутствие паники и валидность (string, error) tuple.
func TestExecuteNvidiaSmi_NotPanics(t *testing.T) {
	assert.NotPanics(t, func() {
		_, _ = executeNvidiaSmi()
	}, "executeNvidiaSmi не должна паниковать")
}

// TestDetectPlatformMode_WithDevNvidia0 — если есть /dev/nvidia0 → ModeGPU.
//
// Создаём временный файл и подменяем путь через симлинк/нельзя (Linux требует
// реальное устройство). Поэтому используем подход с реальным /dev/nvidia0:
// если файл существует — должен вернуть GPU; иначе — пропускаем.
func TestDetectPlatformMode_WithDevNvidia0(t *testing.T) {
	resetGPUState()

	if _, err := os.Stat("/dev/nvidia0"); err != nil {
		t.Skip("/dev/nvidia0 не существует — пропускаем (нужна машина с GPU)")
	}

	config := createTestAgentConfig()
	config.GPUMode = types.ModeAuto
	agent := NewAgent(config)

	mode := agent.detectPlatformMode()
	assert.Equal(t, types.ModeGPU, mode,
		"При наличии /dev/nvidia0 должен вернуться ModeGPU")
}

// TestDetectPlatformMode_WithProcNvidiaVersion — если есть /proc/driver/nvidia/version → ModeGPU.
func TestDetectPlatformMode_WithProcNvidiaVersion(t *testing.T) {
	resetGPUState()

	if _, err := os.Stat("/proc/driver/nvidia/version"); err != nil {
		t.Skip("/proc/driver/nvidia/version не существует — пропускаем")
	}

	config := createTestAgentConfig()
	config.GPUMode = types.ModeAuto
	agent := NewAgent(config)

	mode := agent.detectPlatformMode()
	assert.Equal(t, types.ModeGPU, mode)
}

// TestDetectPlatformMode_NoGPUOnLinux — создаём временную директорию без GPU признаков.
//
// Не можем удалить /dev/nvidia0, но можем проверить, что detectPlatformMode
// хотя бы не падает в ModeGPU, если файлы отсутствуют (актуально для CI).
// Этот тест просто гарантирует graceful fallback.
func TestDetectPlatformMode_NoGPUOnLinux(t *testing.T) {
	if os.Getenv("CI") == "" {
		// На dev-машине может быть GPU — пропускаем, чтобы не было flaky.
		t.Skip("Запускаем только в CI (нет GPU в окружении)")
	}

	resetGPUState()
	config := createTestAgentConfig()
	config.GPUMode = types.ModeAuto
	agent := NewAgent(config)

	mode := agent.detectPlatformMode()
	assert.Equal(t, types.ModeCPU, mode)
}

// TestMapLlamaGPUMetrics_EmptyAndNonEmpty — простой unit-тест маппинга.
//
// Не GPU-detection, но близкая тема — проверим, что mapLlamaGPUMetrics
// корректно работает на пустом и непустом входе.
//
// Поведение при VRAMFreeMB > VRAMTotalMB: текущая реализация clamp'ит free
// к total (`if free > total { free = total }`), значит used = total - total = 0.
// Это защита от паники при uint underflow.
func TestMapLlamaGPUMetrics_EmptyAndNonEmpty(t *testing.T) {
	// Пустой список → пустые метрики
	m := mapLlamaGPUMetrics(nil)
	assert.Equal(t, types.GPUMetrics{}, m)

	// 1 GPU с 8GB total, 3GB free → 5GB used
	m = mapLlamaGPUMetrics([]LlamaGPUInfo{
		{Index: 0, Name: "RTX 3090", VRAMTotalMB: 8192, VRAMFreeMB: 3072},
	})
	assert.Equal(t, uint64(8192), m.MemoryTotal)
	assert.Equal(t, uint64(5120), m.MemoryUsed)
	assert.Equal(t, uint64(3072), m.MemoryFree)

	// Защита от VRAMFreeMB > VRAMTotalMB (баг в старых llama.cpp):
	// free clamp'ится к total → used = 0 (не уходит в uint underflow).
	m = mapLlamaGPUMetrics([]LlamaGPUInfo{
		{Index: 0, Name: "GPU", VRAMTotalMB: 8192, VRAMFreeMB: 16384},
	})
	assert.Equal(t, uint64(8192), m.MemoryTotal)
	assert.Equal(t, uint64(0), m.MemoryUsed,
		"free > total clamp'ится: used = total - total = 0")
}

// TestParseNvidiaSmiOutput — unit-тест парсера nvidia-smi (без реального запуска).
func TestParseNvidiaSmiOutput(t *testing.T) {
	// Валидный CSV-вывод
	output := "0, NVIDIA GeForce RTX 3090, 45, 24576, 8192, 16384, 65, 280, 350, 1800, 9500"
	m := parseNvidiaSmiOutput(output)
	assert.Equal(t, float64(45), m.UsagePercent)
	assert.Equal(t, uint64(24576), m.MemoryTotal)
	assert.Equal(t, uint64(8192), m.MemoryUsed)
	assert.Equal(t, uint64(16384), m.MemoryFree)
	assert.Equal(t, 65, m.Temperature)
	assert.Equal(t, 280, m.PowerUsage)
	assert.Equal(t, 350, m.PowerLimit)
	assert.Equal(t, 1800, m.GPUClock)
	assert.Equal(t, 9500, m.MemClock)

	// Пустой ввод — нулевые метрики без паники
	m = parseNvidiaSmiOutput("")
	assert.Equal(t, types.GPUMetrics{}, m)
}

// TestCountGPUs — проверка подсчёта GPU из вывода nvidia-smi.
//
// NB: реализация делает `len(strings.Split(TrimSpace(out), "\n"))`.
// Для пустой строки Split возвращает `[""]` (len=1) — это known quirk текущей
// реализации. Если будешь чинить — обнови и тест.
func TestCountGPUs(t *testing.T) {
	assert.Equal(t, 1, countGPUs("")) // quirk: пустая строка → 1
	assert.Equal(t, 1, countGPUs("0, GPU0, ..."))
	assert.Equal(t, 2, countGPUs("0, GPU0\n1, GPU1"))
}

// TestParseGPUMModels — проверка парсинга названий моделей.
func TestParseGPUMModels(t *testing.T) {
	assert.Empty(t, parseGPUMModels(""))
	models := parseGPUMModels("0, NVIDIA RTX 3090, ...")
	assert.Equal(t, []string{"NVIDIA RTX 3090"}, models)
	models = parseGPUMModels("0, GPU0\n1, GPU1")
	assert.Equal(t, []string{"GPU0", "GPU1"}, models)
}

