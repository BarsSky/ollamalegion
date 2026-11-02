package modelreplication

// scale_by_load_r379_test.go — R379 (2026-11-01, P2-2): масштабирование группы
// по занятости, а не только по minInstances.
//
// Проверяем три обязательных свойства:
//  1. насыщенная группа просит ОДНУ дополнительную копию, пока не достигнут max;
//  2. ненасыщенная — не просит (иначе контроллер грузил бы копии без нагрузки);
//  3. при max == min (дефолтная конфигурация) поведение прежнее — ничего не
//     запрашивается, даже если загрузка 100%.

import (
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// r379Manager — менеджер с одной группой и одним уже загруженным инстансом.
func r379Manager(minInst, maxInst int, load float64) (*ModelGroupManager, *atomic.Int64) {
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "r379-model",
		MinInstances: minInst,
		MaxInstances: maxInst,
	})

	var warmups atomic.Int64
	mgr.SetFreeBackendFn(func(string, []string) []string {
		return []string{"backend-extra"}
	})
	mgr.SetWarmupFn(func(string, string) error {
		warmups.Add(1)
		return nil
	})
	if load >= 0 {
		mgr.SetBackendLoadFn(func(string) float64 { return load })
	}

	// Одна загруженная копия — группа исполнима, minInstances достигнут.
	mgr.mu.Lock()
	g := mgr.groups["r379-model"]
	g.mu.Lock()
	g.Instances["backend-loaded"] = &types.ModelInstanceState{
		BackendID:  "backend-loaded",
		Status:     types.ModelStateLoaded,
		LoadedAt:   time.Now(),
		LastUsedAt: time.Now(),
	}
	g.mu.Unlock()
	mgr.mu.Unlock()
	return mgr, &warmups
}

// TestR379_SaturatedGroupScalesUp — загрузка выше порога и есть запас до max:
// запрашивается ровно одна копия.
func TestR379_SaturatedGroupScalesUp(t *testing.T) {
	mgr, warmups := r379Manager(1, 3, 1.0)
	mgr.EnsureInstances()
	time.Sleep(50 * time.Millisecond)

	if got := warmups.Load(); got != 1 {
		t.Fatalf("насыщенная группа запросила копий: %d, ожидалась 1", got)
	}
}

// TestR379_IdleGroupDoesNotScaleUp — нагрузки нет: новых копий не просим.
func TestR379_IdleGroupDoesNotScaleUp(t *testing.T) {
	mgr, warmups := r379Manager(1, 3, 0.1)
	mgr.EnsureInstances()
	time.Sleep(50 * time.Millisecond)

	if got := warmups.Load(); got != 0 {
		t.Fatalf("простаивающая группа запросила %d копий, ожидалось 0", got)
	}
}

// TestR379_MaxEqualsMinKeepsOldBehaviour — оператор не разрешил запас (max==min):
// даже при 100% загрузке поведение прежнее.
func TestR379_MaxEqualsMinKeepsOldBehaviour(t *testing.T) {
	mgr, warmups := r379Manager(1, 1, 1.0)
	mgr.EnsureInstances()
	time.Sleep(50 * time.Millisecond)

	if got := warmups.Load(); got != 0 {
		t.Fatalf("при max==min запрошено %d копий, ожидалось 0 (поведение до R379)", got)
	}
}

// TestR379_NoLoadFnMeansNoScaling — балансер не дал загрузку: масштабировать
// не по чему, остаёмся на minInstances.
func TestR379_NoLoadFnMeansNoScaling(t *testing.T) {
	mgr, warmups := r379Manager(1, 3, -1) // load < 0 → callback не задаём
	mgr.EnsureInstances()
	time.Sleep(50 * time.Millisecond)

	if got := warmups.Load(); got != 0 {
		t.Fatalf("без callback'а загрузки запрошено %d копий, ожидалось 0", got)
	}
}

// TestR379_LoadingCountsTowardMax — уже запрошенная загрузка занимает место в
// max, иначе каждый тик контроллера просил бы новую копию на тот же дефицит.
func TestR379_LoadingCountsTowardMax(t *testing.T) {
	mgr, warmups := r379Manager(1, 2, 1.0)

	// Одна копия уже в пути: loaded(1)+inFlight(1) == max(2) → не масштабируемся.
	mgr.mu.Lock()
	g := mgr.groups["r379-model"]
	g.mu.Lock()
	g.Instances["backend-inflight"] = &types.ModelInstanceState{
		BackendID:  "backend-inflight",
		Status:     types.ModelStateLoading,
		LoadedAt:   time.Now(),
		LastUsedAt: time.Now(),
	}
	g.mu.Unlock()
	mgr.mu.Unlock()

	mgr.EnsureInstances()
	time.Sleep(50 * time.Millisecond)

	if got := warmups.Load(); got != 0 {
		t.Fatalf("при загрузке «в пути» до max запрошено %d копий, ожидалось 0", got)
	}
}
