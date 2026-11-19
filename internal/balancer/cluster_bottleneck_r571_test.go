package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// R571 (R495 S4-хвост, docs/plans/2026-11-18-gpu-bottleneck-classifier-r495.md §6).
//
// ЧТО БЫЛО. Подсказка «что делать» публиковалась только в `/api/metrics`
// (`gpu_bottleneck`), а карточка бэкенда читает снапшот кластера — то есть оператор
// в интерфейсе видел голые проценты и вердикта не видел вовсе.
//
// ЧТО СДЕЛАНО. Вердикт и подсказка едут в снапшоте кластера: `BackendMetrics`
// получил `BottleneckKind`/`BottleneckAdvice` (строки, а не balancer-структура —
// `pkg/types` не может импортировать балансер), заполняются из ТОГО ЖЕ трекера,
// что публикует `/api/metrics` (один источник, не два).

// TestR571_ClusterStateCarriesBottleneck — вердикт и подсказка доезжают до снапшота.
func TestR571_ClusterStateCarriesBottleneck(t *testing.T) {
	p := newProxyWithCleanup(t, createTestConfig())
	t.Setenv(bottleneckEnvR495, "shadow")
	resetBottleneckModeR495()
	t.Cleanup(resetBottleneckModeR495)

	// memory-bound: SM недогружен, память на пределе.
	in := BottleneckInputR495{SM: 62, MemBusy: 96, MemBusyMeasured: true, ActiveRequests: 1}
	p.bottleneckTrackerR495Get().update("backend-1", in, false, time.Now())

	st := p.GetClusterState()

	var found *types.BackendMetrics
	for i := range st.Backends {
		if st.Backends[i].ID == "backend-1" {
			found = &st.Backends[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("в снапшоте кластера нет бэкенда backend-1 (бэкендов: %d)", len(st.Backends))
	}
	if found.BottleneckKind != string(BottleneckMemoryR495) {
		t.Fatalf("BottleneckKind = %q, ожидалось %q — карточка бэкенда показывает проценты без причины",
			found.BottleneckKind, BottleneckMemoryR495)
	}
	if found.BottleneckAdvice != BottleneckAdviceIncreaseParallelR495 {
		t.Fatalf("BottleneckAdvice = %q, ожидалось %q — подсказка «что делать» не доехала до UI",
			found.BottleneckAdvice, BottleneckAdviceIncreaseParallelR495)
	}
}

// TestR571_OffModeCarriesNothing — в режиме off вердикта нет, и пустые строки не
// выдумываются (omitempty: поля просто отсутствуют в JSON).
func TestR571_OffModeCarriesNothing(t *testing.T) {
	p := newProxyWithCleanup(t, createTestConfig())
	t.Setenv(bottleneckEnvR495, "off")
	resetBottleneckModeR495()
	t.Cleanup(resetBottleneckModeR495)

	st := p.GetClusterState()
	for _, b := range st.Backends {
		if b.BottleneckKind != "" || b.BottleneckAdvice != "" {
			t.Fatalf("в режиме off у %s появился вердикт %q / подсказка %q — числа выдуманы",
				b.ID, b.BottleneckKind, b.BottleneckAdvice)
		}
	}
}
