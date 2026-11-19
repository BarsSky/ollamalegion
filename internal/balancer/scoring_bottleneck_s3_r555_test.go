package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// R555 (R495 S3, docs/plans/2026-11-18-gpu-bottleneck-classifier-r495.md §5.1).
//
// ЖИВОЙ ДЕФЕКТ, который закрывает S3. score считал занятость GPU по одному
// счётчику SM: `gpuFree = 100 - utilization.gpu`. На декоде каждый токен читает
// все веса и KV-cache, поэтому предел скорости — полоса памяти, а SM падает до
// 40–70 %: карта, уже насыщенная по памяти, выглядела на 30–60 % свободной и
// получала ещё запросов (§1.1 плана).
//
// После правки при `LB_GPU_BOTTLENECK=on` давление = max(SM, занятость памяти), а
// вердикт `stall` даёт штраф. По умолчанию (shadow/off) score НЕ меняется — это
// проверяется первым тестом.

// scoreMetricsR555 — метрики бэкенда с заданными SM и занятостью памяти.
func scoreMetricsR555(sm, memBusy float64, active int) *types.BackendMetrics {
	return &types.BackendMetrics{
		GPU: types.GPUMetrics{
			UsagePercent:   sm,
			MemBusyPercent: memBusy,
			MemoryTotal:    24576,
			MemoryFree:     12288,
		},
		System: types.SystemMetrics{CPUUsagePercent: 20},
		Ollama: types.OllamaMetrics{ActiveRequests: active},
	}
}

// modeR555 выставляет режим флага и сбрасывает sync.Once.
func modeR555(t *testing.T, mode string) {
	t.Helper()
	t.Setenv(bottleneckEnvR495, mode)
	resetBottleneckModeR495()
	t.Cleanup(resetBottleneckModeR495)
}

// TestR555_MemoryBoundLooksBusy — насыщенная по памяти карта теряет «свободу».
func TestR555_MemoryBoundLooksBusy(t *testing.T) {
	m := scoreMetricsR555(62, 96, 1) // SM 62 %, память 96 %

	modeR555(t, "shadow")
	shadowBase, _ := computeBaseScoreAndPenalty(m)

	modeR555(t, "on")
	onBase, _ := computeBaseScoreAndPenalty(m)

	if !(onBase < shadowBase) {
		t.Fatalf("memory-bound: score при on (%.3f) не ниже, чем при shadow (%.3f) — "+
			"карта, упершаяся в полосу памяти, снова выглядит свободной", onBase, shadowBase)
	}
	// Численная сверка: давление = 96 %, значит gpuFree = 4 %, а не 38 %.
	want := (100-96)*0.30 + 50*0.20 + 80*0.15
	if diff := onBase - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("score при on = %.3f, ожидалось %.3f (давление = занятость памяти 96 %%)", onBase, want)
	}
}

// TestR555_ShadowKeepsOldFormula — по умолчанию (shadow) score прежний: S3 не
// имеет права менять решения без явного `on`.
func TestR555_ShadowKeepsOldFormula(t *testing.T) {
	m := scoreMetricsR555(62, 96, 1)
	modeR555(t, "shadow")
	shadowBase, _ := computeBaseScoreAndPenalty(m)

	want := (100-62)*0.30 + 50*0.20 + 80*0.15 // прежняя формула: только SM
	if diff := shadowBase - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("shadow изменил score: %.3f, ожидалось %.3f (прежняя формула по SM)", shadowBase, want)
	}
}

// TestR555_StallGetsPenalty — простаивающая карта с живыми запросами получает
// штраф: она не «свободна», она ждёт (сеть/PCIe/RPC/CPU).
func TestR555_StallGetsPenalty(t *testing.T) {
	m := scoreMetricsR555(10, 10, 2) // SM и память низко, запросы есть

	modeR555(t, "shadow")
	_, shadowPenalty := computeBaseScoreAndPenalty(m)

	modeR555(t, "on")
	_, onPenalty := computeBaseScoreAndPenalty(m)

	if onPenalty < shadowPenalty+bottleneckStallPenaltyR495-0.001 {
		t.Fatalf("stall: штраф при on = %.3f, ожидался не меньше %.3f (shadow %.3f + %.0f)",
			onPenalty, shadowPenalty+bottleneckStallPenaltyR495, shadowPenalty, bottleneckStallPenaltyR495)
	}
}

// TestR555_NoDataKeepsFormula — данных R495 нет (старый агент): поведение
// прежнее даже при `on`. «Неизвестно» — не «плохо».
func TestR555_NoDataKeepsFormula(t *testing.T) {
	m := scoreMetricsR555(62, 0, 1) // MemBusyPercent = 0 → не измерено

	modeR555(t, "shadow")
	shadowBase, shadowPenalty := computeBaseScoreAndPenalty(m)

	modeR555(t, "on")
	onBase, onPenalty := computeBaseScoreAndPenalty(m)

	if onBase != shadowBase || onPenalty != shadowPenalty {
		t.Fatalf("без измеренной занятости памяти score изменился: base %.3f→%.3f, penalty %.3f→%.3f",
			shadowBase, onBase, shadowPenalty, onPenalty)
	}
}
