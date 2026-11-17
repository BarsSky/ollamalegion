// autotune_units_r498.go — R498 (2026-11-19), аудит M-1: единицы измерения в
// AutoTune.
//
// ЗАЧЕМ. `types.GPUMetrics.MemoryFree/MemoryTotal` и `types.System.MemoryFree`
// отдаются в МЕГАБАЙТАХ, а поля `ModelProfileInfo.FreeVRAMBytes/FreeRAMBytes/
// TotalVRAMBytes` — в БАЙТАХ. В `buildPlanFromReport` МБ подставлялись как есть:
// при 7097 МБ свободной VRAM профиль видел 7097 БАЙТ, `autotuneReserveBytes`
// считал резерв больше свободного, и оператор получал «бюджет 0.0 ГБ — модель не
// влезает» на карте, где реально было ~7 ГБ.
//
// Один и тот же класс уже был закрыт в `handlers_backends.go` (MBToBytes) — но не
// здесь. Поэтому конверсия вынесена в ОДНУ функцию, и её проверяет тест.
package api

import (
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// autoTuneHardwareBytesR498 — переводит метрики бэкенда (МБ) в байты для
// профиля AutoTune.
//
// Правила (те же, что в handlers_backends.go:947-959):
//   - total/free VRAM — МБ → байты;
//   - free VRAM: если движок свободное не сообщил (0), считаем free = total -
//     used (иначе профиль увидит «свободно 0 байт»);
//   - free RAM — МБ → байты.
func autoTuneHardwareBytesR498(gpu types.GPUMetrics, sys types.SystemMetrics) (freeVRAM, freeRAM, totalVRAM uint64) {
	totalVRAM = balancer.MBToBytes(gpu.MemoryTotal)
	if gpu.MemoryFree > 0 {
		freeVRAM = balancer.MBToBytes(gpu.MemoryFree)
	} else if gpu.MemoryTotal > gpu.MemoryUsed {
		freeVRAM = balancer.MBToBytes(gpu.MemoryTotal - gpu.MemoryUsed)
	}
	freeRAM = balancer.MBToBytes(sys.MemoryFree)
	return freeVRAM, freeRAM, totalVRAM
}
