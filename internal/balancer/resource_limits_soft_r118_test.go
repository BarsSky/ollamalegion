// resource_limits_soft_r118_test.go — R118 (2026-10-14): занятый работой бэкенд
// не исключается из кандидатов; жёсткими остаются только память и диск.
//
// ЖИВОЙ ДЕФЕКТ. checkResourceLimits вызывается из 13 точек выбора бэкенда, и
// блокировка по мгновенной утилизации возвращала false — бэкенд выпадал из
// кандидатов, запрос не вставал в admission-очередь, а получал «all backends
// busy» (503). На стенде модель загружена частично на GPU, частично на CPU
// (16 слоёв), поэтому во время ЛЮБОЙ генерации CPU ~96% и GPU ~100% — занятый
// бэкенд блокировался сразу и для всех: второй экземпляр Cline, OpenWebUI,
// вторая модель.
//
// Лог стенда до правки (плотным циклом):
//
//	{"msg":"backend CPU usage critical, blocking","cpu_usage":96.35}
//
// Теперь утилизация только логируется: параллелизм ограничен слотами
// (EffectiveMaxConcurrentRequests), а лишний запрос ЖДЁТ в очереди.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// r118ProxyWithMetrics — Proxy с одним llama.cpp-бэкендом и заданными метриками.
func r118ProxyWithMetrics(t *testing.T, metrics *types.BackendMetrics) *Proxy {
	t.Helper()
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Resources: types.ResourceLimits{
				Memory: types.MemoryLimits{MaxUsagePercent: 90},
				Disk:   types.DiskLimits{MinFreeMB: 100},
			},
			Balancing: types.BalancingSettings{
				ResourceReservation: types.ResourceReservationConfig{GPUHeadroomPercent: 5},
			},
		},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["bk"] = &BackendState{
		Backend: &types.Backend{
			ID: "bk", Type: types.BackendTypeLlamaCpp,
			Status: types.StatusHealthy, MaxConcurrentReqs: 4,
		},
	}
	if metrics != nil {
		p.metricsMgr.mu.Lock()
		p.metricsMgr.metrics["bk"] = metrics
		p.metricsMgr.mu.Unlock()
	}
	return p
}

// busyMetrics — картина «бэкенд занят собственной генерацией»: GPU 100%, CPU 96%,
// VRAM занята загруженной моделью на 97% (при резерве headroom 5%).
func busyMetrics() *types.BackendMetrics {
	return &types.BackendMetrics{
		GPU: types.GPUMetrics{UsagePercent: 100, MemoryUsed: 9700, MemoryTotal: 10000},
		System: types.SystemMetrics{
			CPUUsagePercent: 96.4,
			MemoryTotal:     25000,
			MemoryUsed:      6000, // RAM в норме — иначе сработает жёсткий лимит
			DiskTotal:       100000,
			DiskFree:        50000,
		},
	}
}

// TestR118_BusyBackendStaysCandidate — главное: занятый бэкенд НЕ выпадает из
// кандидатов, поэтому запрос попадёт в admission-очередь, а не получит 503.
func TestR118_BusyBackendStaysCandidate(t *testing.T) {
	p := r118ProxyWithMetrics(t, busyMetrics())

	if !p.checkResourceLimits("bk") {
		t.Error("checkResourceLimits(занятый бэкенд) = false: бэкенд выпадает из кандидатов, " +
			"и второй запрос (второй Cline, OpenWebUI, вторая модель) получает 503 вместо очереди")
	}
}

// TestR118_CriticalRAMStillBlocks — критическая RAM остаётся жёстким лимитом:
// при ней обслуживание физически невозможно (своп/OOM).
func TestR118_CriticalRAMStillBlocks(t *testing.T) {
	m := busyMetrics()
	m.System.MemoryUsed = 24800 // 99.2% > 98%
	p := r118ProxyWithMetrics(t, m)

	if p.checkResourceLimits("bk") {
		t.Error("checkResourceLimits(critical RAM) = true, want false: RAM > 98% — жёсткий лимит")
	}
}

// TestR118_DiskStillBlocks — нехватка места на диске остаётся жёстким лимитом.
func TestR118_DiskStillBlocks(t *testing.T) {
	m := busyMetrics()
	m.System.DiskFree = 10 // < MinFreeMB (100)
	p := r118ProxyWithMetrics(t, m)

	if p.checkResourceLimits("bk") {
		t.Error("checkResourceLimits(disk below minimum) = true, want false: диск — жёсткий лимит")
	}
}

// TestR118_HardBlockFlagRestoresOldBehaviour — прежнее поведение возвращается
// флагом: LB_RESOURCE_LIMITS_HARD_BLOCK=true.
func TestR118_HardBlockFlagRestoresOldBehaviour(t *testing.T) {
	t.Setenv("LB_RESOURCE_LIMITS_HARD_BLOCK", "true")
	p := r118ProxyWithMetrics(t, busyMetrics())

	if p.checkResourceLimits("bk") {
		t.Error("с LB_RESOURCE_LIMITS_HARD_BLOCK=true занятый бэкенд должен блокироваться " +
			"(прежнее поведение), а он остался кандидатом")
	}
}
