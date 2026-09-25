package modelreplication

// R80: группа репликации усыновляет уже загруженные копии модели.
//
// Симптом, который ловим (найден нагрузочным стендом P4): после рестарта
// балансера при двух реально загруженных копиях группа пуста (`total: 0`),
// контроллер грузит лишнюю копию, а трафик replicated уходит в одну копию.

import (
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// warmRecorder — потокобезопасный список вызовов warmup (загрузка в
// scaleUpGroup идёт асинхронно, поэтому без мьютекса тест ловит гонку).
type warmRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (w *warmRecorder) add(backendID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ids = append(w.ids, backendID)
}

func (w *warmRecorder) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.ids...)
}

// loadedState — потокобезопасный «ответ метрик» на вопрос, где модель загружена
// (в тесте его можно менять, имитируя завершение загрузки).
type loadedState struct {
	mu  sync.Mutex
	ids []string
	set bool
}

func (l *loadedState) update(ids ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ids = append([]string(nil), ids...)
	l.set = true
}

func (l *loadedState) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.set {
		return nil
	}
	return append([]string(nil), l.ids...)
}

// r80Manager — менеджер с группой min=2, записанными вызовами warmup и
// управляемым ответом «где модель реально загружена».
func r80Manager(t *testing.T, loaded []string, free []string) (*ModelGroupManager, *warmRecorder, *loadedState) {
	t.Helper()
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	if err := mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "r80-model",
		MinInstances: 2,
		MaxInstances: 3,
	}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	warmed := &warmRecorder{}
	state := &loadedState{}
	if loaded != nil {
		state.update(loaded...)
	}
	mgr.SetFreeBackendFn(func(string, []string) []string { return free })
	mgr.SetWarmupFn(func(backendID, modelName string) error {
		warmed.add(backendID)
		return nil
	})
	if loaded != nil {
		mgr.SetLoadedBackendsFn(func(string) []string { return state.snapshot() })
	}
	return mgr, warmed, state
}

func r80Candidates(mgr *ModelGroupManager) []string {
	sel := NewGroupAwareSelector(mgr)
	return sel.GetGroupCandidates("r80-model")
}

// waitWarmed — warmup запускается асинхронно (go-рутина в scaleUpGroup),
// поэтому ждём появления ожидаемого числа вызовов.
func waitWarmed(t *testing.T, warmed *warmRecorder, want int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := warmed.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return warmed.snapshot()
}

// settleWarmed — даёт шанс асинхронным warmup'ам проявиться (для проверок
// «загрузки НЕ должно быть»).
func settleWarmed() { time.Sleep(150 * time.Millisecond) }

func TestGroupR80_AdoptsAlreadyLoadedCopies(t *testing.T) {
	// Две копии уже в VRAM (загружены до старта балансера) — группа обязана
	// их усыновить и НЕ грузить третью.
	mgr, warmed, _ := r80Manager(t, []string{"a", "b"}, []string{"c"})

	mgr.EnsureInstances()
	settleWarmed()

	if len(warmed.snapshot()) != 0 {
		t.Errorf("warmup вызван для %v — копии уже загружены", warmed.snapshot())
	}
	cand := r80Candidates(mgr)
	if len(cand) != 2 {
		t.Fatalf("кандидатов %d (%v), ожидалось 2 усыновлённых", len(cand), cand)
	}
	stats := mgr.GetGroupStats("r80-model")
	if stats["loaded"] != 2 || stats["total"] != 2 {
		t.Errorf("статистика группы: loaded=%v total=%v, ожидалось 2/2", stats["loaded"], stats["total"])
	}
}

func TestGroupR80_LoadsMissingReplicaOnly(t *testing.T) {
	// Одна копия уже загружена, minInstances=2 → нужна ровно одна загрузка.
	// Пока метрики не подтвердили загрузку, инстанс LOADING (не кандидат), и
	// повторных загрузок на тот же дефицит не запускается.
	mgr, warmed, state := r80Manager(t, []string{"a"}, []string{"b"})

	mgr.EnsureInstances()
	settleWarmed()

	got := waitWarmed(t, warmed, 1)
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("warmup: %v, ожидалось ровно [b]", got)
	}
	if cand := r80Candidates(mgr); len(cand) != 1 || cand[0] != "a" {
		t.Fatalf("кандидаты %v, ожидался только [a]: b ещё грузится", cand)
	}
	stats := mgr.GetGroupStats("r80-model")
	if stats["loading"] != 1 || stats["loaded"] != 1 {
		t.Errorf("статистика: loaded=%v loading=%v, ожидалось 1/1", stats["loaded"], stats["loading"])
	}

	// Повторный тик не должен запрашивать вторую загрузку на тот же дефицит.
	mgr.EnsureInstances()
	settleWarmed()
	if n := len(warmed.snapshot()); n != 1 {
		t.Fatalf("warmup вызван %d раз (%v) — дублирующая загрузка в процессе", n, warmed.snapshot())
	}

	// Метрики подтвердили загрузку → инстанс становится LOADED и кандидатом.
	state.update("a", "b")
	mgr.EnsureInstances()
	if cand := r80Candidates(mgr); len(cand) != 2 {
		t.Errorf("кандидаты %v, ожидалось 2 после подтверждения метриками", cand)
	}
}

func TestGroupR80_DropsStaleInstance(t *testing.T) {
	// Инстанс числится LOADED, но модели на бэкенде нет и тёплое окно прошло
	// (например, cppworker перезапустили) → инстанс сбрасывается, контроллер
	// запрашивает загрузку заново.
	mgr, warmed, state := r80Manager(t, []string{}, []string{"a"})
	mgr.mu.Lock()
	g := mgr.groups["r80-model"]
	g.Instances["a"] = &types.ModelInstanceState{
		BackendID:  "a",
		Status:     types.ModelStateLoaded,
		LoadedAt:   time.Now().Add(-2 * loadedGracePeriod),
		LastUsedAt: time.Now().Add(-2 * loadedGracePeriod),
	}
	mgr.mu.Unlock()

	mgr.EnsureInstances()
	settleWarmed()

	got := waitWarmed(t, warmed, 1)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("warmup: %v, ожидалось [a] после сброса протухшего инстанса", got)
	}
	if cand := r80Candidates(mgr); len(cand) != 0 {
		t.Errorf("кандидаты %v: загрузка только запрошена, кандидатов быть не должно", cand)
	}

	state.update("a")
	mgr.EnsureInstances()
	if cand := r80Candidates(mgr); len(cand) != 1 || cand[0] != "a" {
		t.Errorf("кандидаты %v, ожидалось [a] после подтверждения метриками", cand)
	}
}

func TestGroupR80_KeepsInstanceWithinGracePeriod(t *testing.T) {
	// Загрузка асинхронная: инстанс помечен LOADED, модель ещё грузится и в
	// метриках её нет. Внутри тёплого окна повторную загрузку не запускаем.
	mgr, warmed, _ := r80Manager(t, []string{}, []string{"a"})
	mgr.mu.Lock()
	g := mgr.groups["r80-model"]
	g.Instances["a"] = &types.ModelInstanceState{
		BackendID:  "a",
		Status:     types.ModelStateLoaded,
		LoadedAt:   time.Now(),
		LastUsedAt: time.Now(),
	}
	mgr.mu.Unlock()

	mgr.EnsureInstances()
	settleWarmed()

	if len(warmed.snapshot()) != 0 {
		t.Errorf("warmup вызван (%v) внутри тёплого окна загрузки", warmed.snapshot())
	}
	if cand := r80Candidates(mgr); len(cand) != 1 || cand[0] != "a" {
		t.Errorf("кандидаты %v, ожидалось [a]", cand)
	}
}

func TestGroupR80_NoCallbackKeepsLegacyBehaviour(t *testing.T) {
	// Без callback'а реальных загрузок поведение прежнее: группа грузит копии
	// по minInstances через warmup.
	mgr, warmed, _ := r80Manager(t, nil, []string{"a", "b"})

	mgr.EnsureInstances()
	settleWarmed()

	if got := waitWarmed(t, warmed, 2); len(got) != 2 {
		t.Fatalf("warmup: %v, ожидалось 2 загрузки (обратная совместимость)", got)
	}
}
