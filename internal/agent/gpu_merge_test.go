package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"ollama-loadbalancer/pkg/types"
)

// R-MultiHost (2026-10-07).
//
// Оператор: «отсутствуют правильные метрики у gguf бэкендов». Текстовый бэкенд на
// RTX 3070 показывал «GPU 0.0%, TEMP 0 °C, CLOCK —», а image-бэкенд НА ТОЙ ЖЕ
// карте — «55 °C, 1755 MHz, memclock 6801».
//
// Причина: у llama.cpp-бэкенда GPU-метрики идут из cppworker (/api/gpu), который
// отдаёт ТОЛЬКО объём памяти. mapLlamaGPUMetricsWithUUIDs ставил в остальные поля
// нули, а локальный опрос nvidia-smi/NVML не выполнялся вовсе, если cppworker
// вернул хоть одну карту.
//
// Тесты фиксируют правила слияния: VRAM — от движка, загрузка/температура/частоты
// — от локального опроса (движок этих полей не отдаёт вообще, ноль там означал
// «поля нет», а не «карта холодная»).

// llamaOnly — то, что реально приходит от cppworker: только память.
func llamaOnly() types.GPUMetrics {
	return types.GPUMetrics{
		MemoryTotal: 8192,
		MemoryUsed:  1094,
		MemoryFree:  7098,
		UUIDs:       []string{"GPU-6f8d5f3b-1dde-cbcf-8c26-4a1d0dbead34"},
	}
}

// localProbe — то, что видит nvidia-smi внутри контейнера воркера.
func localProbe() types.GPUMetrics {
	return types.GPUMetrics{
		UsagePercent: 1,
		MemoryTotal:  8192,
		MemoryUsed:   1571,
		MemoryFree:   6621,
		Temperature:  55,
		PowerUsage:   51,
		PowerLimit:   220,
		GPUClock:     1755,
		MemClock:     6801,
		UUIDs:        []string{"GPU-6f8d5f3b-1dde-cbcf-8c26-4a1d0dbead34"},
	}
}

// TestMergeGPUMetrics_FillsWhatEngineDoesNotKnow — главный сценарий жалобы.
func TestMergeGPUMetrics_FillsWhatEngineDoesNotKnow(t *testing.T) {
	got := mergeGPUMetrics(llamaOnly(), localProbe())

	assert.Equal(t, 55, got.Temperature, "температура должна прийти от nvidia-smi, а не остаться нулём")
	assert.Equal(t, 1755, got.GPUClock, "частота GPU должна прийти от nvidia-smi")
	assert.Equal(t, 6801, got.MemClock, "частота памяти должна прийти от nvidia-smi")
	assert.Equal(t, float64(1), got.UsagePercent, "загрузка GPU должна прийти от nvidia-smi")
	assert.Equal(t, 51, got.PowerUsage)
	assert.Equal(t, 220, got.PowerLimit)
}

// TestMergeGPUMetrics_LocalVRAMWinsOverStaleEngine — ГЛАВНЫЙ сценарий жалобы
// «агенты показывают разные ресурсы».
//
// cppworker измеряет VRAM через cudaMemGetInfo ОДИН РАЗ на инициализации, и
// /api/gpu отдаёт этот снимок до конца жизни процесса: на живой стойке nvidia-smi
// менялся (1559 → 1662 → 1565 MB), а cppworker всё это время отдавал неизменные
// used=1094/free=7097. Поэтому два агента на ОДНОЙ карте показывали разное.
//
// Локальный опрос (он же нужен для температуры и частот) теперь даёт и память.
func TestMergeGPUMetrics_LocalVRAMWinsOverStaleEngine(t *testing.T) {
	engine := llamaOnly() // снимок cppworker: used=1094
	local := localProbe()
	local.MemoryUsed = 1662 // живое значение nvidia-smi
	local.MemoryFree = 6530

	got := mergeGPUMetrics(engine, local)

	assert.Equal(t, uint64(1662), got.MemoryUsed,
		"должно уйти живое значение nvidia-smi, а не замороженный снимок движка")
	assert.Equal(t, uint64(6530), got.MemoryFree)
	assert.Equal(t, uint64(8192), got.MemoryTotal)
}

// TestMergeGPUMetrics_KeepsEngineVRAMWhenLocalUnavailable — если локальный опрос
// недоступен, снимок движка лучше, чем ничего: не обнуляем.
func TestMergeGPUMetrics_KeepsEngineVRAMWhenLocalUnavailable(t *testing.T) {
	got := mergeGPUMetrics(llamaOnly(), types.GPUMetrics{})

	assert.Equal(t, uint64(8192), got.MemoryTotal)
	assert.Equal(t, uint64(1094), got.MemoryUsed, "снимок движка должен остаться, если заменить нечем")
	assert.Equal(t, 0, got.Temperature)
	assert.Equal(t, 0, got.GPUClock)
}

// TestMergeGPUMetrics_EngineWithoutVRAMFallsBackToLocal — движок памяти не дал
// (карта не инициализирована движком) — берём локальную.
func TestMergeGPUMetrics_EngineWithoutVRAMFallsBackToLocal(t *testing.T) {
	got := mergeGPUMetrics(types.GPUMetrics{}, localProbe())

	assert.Equal(t, uint64(8192), got.MemoryTotal)
	assert.Equal(t, uint64(1571), got.MemoryUsed)
	assert.Equal(t, 55, got.Temperature)
}

// TestMergeGPUMetrics_KeepsUUIDs — признак физической карты не должен потеряться:
// по нему балансер не удваивает VRAM на хосте с двумя бэкендами.
func TestMergeGPUMetrics_KeepsUUIDs(t *testing.T) {
	got := mergeGPUMetrics(llamaOnly(), localProbe())
	assert.Equal(t, []string{"GPU-6f8d5f3b-1dde-cbcf-8c26-4a1d0dbead34"}, got.UUIDs)

	// Если движок UUID не дал — берём локальные.
	primary := llamaOnly()
	primary.UUIDs = nil
	got = mergeGPUMetrics(primary, localProbe())
	assert.NotEmpty(t, got.UUIDs, "UUID должны взяться из локального опроса")
}

// TestMapLlamaGPUMetricsWithUUIDs_StillZerosNonMemory — фиксируем, ЧТО именно
// отдаёт cppworker-путь сам по себе: только память, остальное нули. Именно эти
// нули и попадали в UI до слияния.
func TestMapLlamaGPUMetricsWithUUIDs_StillZerosNonMemory(t *testing.T) {
	gpus := []LlamaGPUInfo{{VRAMTotalMB: 8192, VRAMFreeMB: 7098}}
	got := mapLlamaGPUMetricsWithUUIDs(gpus, []string{"GPU-abc"})

	assert.Equal(t, uint64(8192), got.MemoryTotal)
	assert.Equal(t, uint64(1094), got.MemoryUsed)
	assert.Equal(t, 0, got.Temperature)
	assert.Equal(t, 0, got.GPUClock)
	assert.Equal(t, 0, got.MemClock)
	assert.Equal(t, float64(0), got.UsagePercent)

	// И после слияния с локальным опросом — уже не нули.
	merged := mergeGPUMetrics(got, localProbe())
	assert.Equal(t, 55, merged.Temperature)
	assert.Equal(t, 1755, merged.GPUClock)
}
