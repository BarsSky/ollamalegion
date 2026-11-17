//go:build llama_stub

// consistency_s1_r498_test.go — R498 (2026-11-19), шаг S1 аудита
// docs/plans/2026-11-19-consistency-audit.md.
//
// ЗДЕСЬ ЗАКРЫТЫ ЧЕТЫРЕ «ЛОЖНЫХ ЧИСЛА» ИЗ §4 аудита, и каждое проверяется так,
// чтобы мутация краснила:
//
//	M-1  МБ вместо байт в AutoTune → «бюджет 0.0 ГБ» на карте с 7 ГБ свободных;
//	T-1  плоские 180 с на первый байт против живой загрузки 140–485 с;
//	T-3  один `-1` даёт 30 минут на одном пути и 24 часа на другом.
//
// N-5 (единая формула KV) проверяется в autotune_geometry_r230_test.go: там
// ожидания переведены с «f16/2 и f16/4» на точные доли memfit (34/32 и 18/32).
package api

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR498_AutoTuneHardwareUnits — МБ метрик превращаются в БАЙТЫ профиля.
//
// Живой случай из аудита: MemoryTotal 8192 МБ, MemoryFree 7097 МБ. Ожидаем
// ~7.9 ГБ и ~6.9 ГБ в байтах; при подстановке МБ как есть получилось бы 8192 и
// 7097 БАЙТ, то есть «0.0 ГБ».
func TestR498_AutoTuneHardwareUnits(t *testing.T) {
	const mb = 1024 * 1024
	gpu := types.GPUMetrics{MemoryTotal: 8192, MemoryUsed: 1095, MemoryFree: 7097}
	sys := types.SystemMetrics{MemoryFree: 20480}

	freeVRAM, freeRAM, totalVRAM := autoTuneHardwareBytesR498(gpu, sys)

	if totalVRAM != 8192*mb {
		t.Errorf("totalVRAM = %d, ожидалось %d (8192 МБ в байтах)", totalVRAM, uint64(8192*mb))
	}
	if freeVRAM != 7097*mb {
		t.Errorf("freeVRAM = %d, ожидалось %d (7097 МБ в байтах): "+
			"в МБ профиль видит 7097 БАЙТ и пишет «бюджет 0.0 ГБ»", freeVRAM, uint64(7097*mb))
	}
	if freeRAM != 20480*mb {
		t.Errorf("freeRAM = %d, ожидалось %d", freeRAM, uint64(20480*mb))
	}
	if freeVRAM < 5*1024*mb {
		t.Fatalf("свободной VRAM меньше 5 ГБ (%d байт) — единицы перепутаны", freeVRAM)
	}
}

// TestR498_AutoTuneHardwareFreeFallback — движок не сообщил free: считаем
// total-used, а не ноль (иначе профиль снова видит «свободно 0»).
func TestR498_AutoTuneHardwareFreeFallback(t *testing.T) {
	const mb = 1024 * 1024
	gpu := types.GPUMetrics{MemoryTotal: 8192, MemoryUsed: 1095, MemoryFree: 0}
	freeVRAM, _, _ := autoTuneHardwareBytesR498(gpu, types.SystemMetrics{})
	if want := uint64((8192 - 1095) * mb); freeVRAM != want {
		t.Fatalf("freeVRAM = %d, ожидалось %d (total-used)", freeVRAM, want)
	}
}
