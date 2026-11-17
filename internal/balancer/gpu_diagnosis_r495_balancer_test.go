//go:build llama_stub

// gpu_diagnosis_r495_balancer_test.go — R495 шаг S1 (2026-11-18).
//
// Здесь проверяется правило «числа только измеренные» на выходе в /api/metrics:
// старый агент (без R495-полей) обязан давать НЕ нули, а отсутствие полей —
// иначе `mem_busy_percent: 0` читается как «память свободна», то есть повторяет
// ровно тот дефект, который задача исправляет (memory-bound карта выглядит
// свободной).
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func proxyWithGPU(t *testing.T, gpu types.GPUMetrics) *Proxy {
	t.Helper()
	p := &Proxy{metricsMgr: NewMetricsManager()}
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{GPU: gpu}
	return p
}

// TestR495_SnapshotOmitsUnmeasured — старый агент: измеренных признаков нет,
// значит в снимке НЕТ нулей и нет записи о бэкенде вообще.
func TestR495_SnapshotOmitsUnmeasured(t *testing.T) {
	p := proxyWithGPU(t, types.GPUMetrics{UsagePercent: 0, PState: -1})
	snap := p.gpuDiagnosisSnapshot()
	if len(snap) != 0 {
		t.Fatalf("снимок непустой при отсутствии измерений: %+v\n"+
			"нули выдаются за измерения — это исходный дефект R495", snap)
	}
}

// TestR495_SnapshotKeepsMeasured — измеренные признаки попадают, PState 0 (P0)
// не теряется, power_ratio считается из измеренных чисел.
func TestR495_SnapshotKeepsMeasured(t *testing.T) {
	p := proxyWithGPU(t, types.GPUMetrics{
		UsagePercent:        62,
		MemBusyPercent:      96,
		PowerUsage:          141,
		PowerLimit:          150,
		PState:              0,
		ThrottleKnown:       true,
		ThrottleReasons:     4,
		ThrottleReasonsList: []string{"sw_power_cap"},
		PCIELinkGen:         4,
		PCIELinkWidth:       16,
	})

	snap := p.gpuDiagnosisSnapshot()
	entry, ok := snap["b1"]
	if !ok {
		t.Fatalf("нет записи b1 в снимке: %+v", snap)
	}

	if got, _ := entry["mem_busy_percent"].(float64); got != 96 {
		t.Errorf("mem_busy_percent = %v, ожидалось 96", entry["mem_busy_percent"])
	}
	if got, _ := entry["mem_busy_measured"].(bool); !got {
		t.Error("mem_busy_measured = false при измеренных 96 %")
	}
	if got, _ := entry["pstate"].(int); got != 0 {
		t.Errorf("pstate = %v, ожидалось 0 (P0 — законное значение)", entry["pstate"])
	}
	if got, _ := entry["power_ratio"].(float64); got < 0.93 || got > 0.95 {
		t.Errorf("power_ratio = %v, ожидалось ≈0.94 (141/150)", entry["power_ratio"])
	}
	if got, _ := entry["throttle_reasons"].(uint64); got != 4 {
		t.Errorf("throttle_reasons = %v, ожидалось 4", entry["throttle_reasons"])
	}
}

// TestR495_SnapshotMemBusyOnlyOldFieldsZero — карта отдала только память-полосу
// (temperature/clock нулевые): снимок обязан её показать, иначе диагноза нет.
func TestR495_SnapshotMemBusyOnlyOldFieldsZero(t *testing.T) {
	p := proxyWithGPU(t, types.GPUMetrics{PState: -1, MemBusyPercent: 88})
	snap := p.gpuDiagnosisSnapshot()
	entry := snap["b1"]
	if entry == nil {
		t.Fatal("снимок пуст, хотя занятость памяти измерена (88 %)")
	}
	if got, _ := entry["mem_busy_percent"].(float64); got != 88 {
		t.Errorf("mem_busy_percent = %v, ожидалось 88", entry["mem_busy_percent"])
	}
	// Неизмеренные поля не публикуются.
	if _, has := entry["throttle_reasons"]; has {
		t.Error("throttle_reasons опубликован без измерения (ThrottleKnown=false)")
	}
	if _, has := entry["pstate"]; has {
		t.Error("pstate опубликован при -1 («неизвестно»)")
	}
}
