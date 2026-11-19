package balancer

import (
	"testing"
	"time"
)

// R556 (R495 S4, docs/plans/2026-11-18-gpu-bottleneck-classifier-r495.md §5.3).
//
// ЗАЧЕМ ПОДСКАЗКИ. Вердикт «memory» оператор читает как «карта занята, надо
// разгрузить» — и уменьшает нагрузку, хотя при memory-bound главный рычаг
// ПРОТИВОПОЛОЖЕН: при декоде веса читаются один раз на N токенов, и рост
// parallel/слотов почти линейно поднимает агрегатный tok/s на одной карте.
// Без подсказки правильный вердикт приводит к неправильному действию.
//
// Подсказка — ТОЛЬКО совет: балансер ничего не применяет сам (прецедент R83-fix).

// TestR556_AdviceCoversExplainedKinds — подсказка есть у двух вердиктов, где
// действие оператора действительно меняет картину.
func TestR556_AdviceCoversExplainedKinds(t *testing.T) {
	if got := bottleneckAdviceR495(BottleneckMemoryR495); got != BottleneckAdviceIncreaseParallelR495 {
		t.Fatalf("memory → %q, ожидалось %q (главный рычаг — поднять parallel, "+
			"а не «разгрузить»)", got, BottleneckAdviceIncreaseParallelR495)
	}
	if got := bottleneckAdviceR495(BottleneckPowerR495); got != BottleneckAdviceReduceBatchR495 {
		t.Fatalf("power → %q, ожидалось %q (батч упирается в TDP)", got, BottleneckAdviceReduceBatchR495)
	}
}

// TestR556_NoAdviceWhereWeDontKnowCause — там, где причина вне карты или данных
// нет, совет не выдумывается.
func TestR556_NoAdviceWhereWeDontKnowCause(t *testing.T) {
	for _, kind := range []BottleneckKindR495{
		BottleneckComputeR495,
		BottleneckStallR495,
		BottleneckIdleR495,
		BottleneckUnknownR495,
	} {
		if got := bottleneckAdviceR495(kind); got != "" {
			t.Fatalf("%s → %q, ожидался пустой совет: причина либо вне карты (сеть/RPC/CPU), "+
				"либо данных нет — советовать наугад нельзя", kind, got)
		}
	}
}

// TestR556_AdvicePublishedInState — подсказка доезжает до опубликованного
// состояния (его читает /api/metrics и панель), а не остаётся в функции.
func TestR556_AdvicePublishedInState(t *testing.T) {
	tr := newBottleneckTrackerR495()
	now := time.Now()

	// memory-bound: SM недогружен, память на пределе, питание в норме.
	in := BottleneckInputR495{SM: 62, MemBusy: 96, MemBusyMeasured: true, ActiveRequests: 1}
	state, _ := tr.update("b1", in, false, now)
	if state.Kind != BottleneckMemoryR495 {
		t.Fatalf("условие теста не выполнено: вердикт %q, ожидался memory", state.Kind)
	}
	if state.Advice != BottleneckAdviceIncreaseParallelR495 {
		t.Fatalf("в опубликованном состоянии advice = %q, ожидалось %q — подсказка не доехала до панели",
			state.Advice, BottleneckAdviceIncreaseParallelR495)
	}
	snap := tr.snapshot()
	if snap["b1"].Advice != BottleneckAdviceIncreaseParallelR495 {
		t.Fatalf("в снимке для /api/metrics advice = %q, ожидалось %q",
			snap["b1"].Advice, BottleneckAdviceIncreaseParallelR495)
	}

	// compute-bound: совет пуст (упираемся в ядра, конфиг не при чём).
	inCompute := BottleneckInputR495{SM: 95, MemBusy: 40, MemBusyMeasured: true, ActiveRequests: 1}
	tr.update("b2", inCompute, false, now)
	if got := tr.snapshot()["b2"].Advice; got != "" {
		t.Fatalf("compute-bound получил совет %q, ожидался пустой", got)
	}
}
