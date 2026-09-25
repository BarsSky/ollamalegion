package modelreplication

// R81: хвосты P4.
//
//   - событийное подтверждение инстанса группы (cppworker сам сообщил, что
//     модель загружена) — LOADING→LOADED сразу, без ожидания тика поллера;
//   - событийная выгрузка (модель выгружена/бэкенд отвалился) — инстанс уходит
//     немедленно;
//   - строгий обход реплик: наименьшая загрузка, при равенстве — меньший
//     UseCount, затем меньший ID (раньше результат зависел от порядка обхода
//     map, и распределение уезжало: в замере P4 6:2 и 7:1 при двух равных
//     репликах).

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func r81Candidates(mgr *ModelGroupManager) []string {
	sel := NewGroupAwareSelector(mgr)
	return sel.GetGroupCandidates("r81-model")
}

func r81ManagerWithInstances(t *testing.T, backends ...string) *ModelGroupManager {
	t.Helper()
	mgr := NewModelGroupManager()
	mgr.SetEnabled(true)
	if err := mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "r81-model",
		MinInstances: 2,
		MaxInstances: 3,
	}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	mgr.mu.Lock()
	g := mgr.groups["r81-model"]
	for _, id := range backends {
		g.Instances[id] = &types.ModelInstanceState{
			BackendID:  id,
			Status:     types.ModelStateLoading,
			LoadedAt:   time.Now(),
			LastUsedAt: time.Now(),
		}
	}
	mgr.mu.Unlock()
	return mgr
}

func TestGroupR81_AdoptLoadedInstanceOnEvent(t *testing.T) {
	mgr := r81ManagerWithInstances(t, "a")

	if len(r81Candidates(mgr)) != 0 {
		t.Fatal("до события LOADING-инстанс не должен быть кандидатом")
	}
	if !mgr.AdoptLoadedInstance("r81-model", "a") {
		t.Fatal("AdoptLoadedInstance вернул false для известного инстанса")
	}
	cand := r81Candidates(mgr)
	if len(cand) != 1 || cand[0] != "a" {
		t.Fatalf("кандидаты %v, ожидалось [a]", cand)
	}
	// Идемпотентность: повторное событие ничего не ломает.
	if mgr.AdoptLoadedInstance("r81-model", "a") {
		t.Error("повторный AdoptLoadedInstance не должен считаться изменением")
	}
	// Неизвестная группа/модель — no-op.
	if mgr.AdoptLoadedInstance("other-model", "a") {
		t.Error("для чужой модели AdoptLoadedInstance должен вернуть false")
	}
}

func TestGroupR81_AdoptCreatesInstanceForUnknownBackend(t *testing.T) {
	// Событие пришло раньше, чем группа узнала про бэкенд (гонка при старте):
	// инстанс создаётся сразу как LOADED.
	mgr := r81ManagerWithInstances(t)
	if !mgr.AdoptLoadedInstance("r81-model", "b") {
		t.Fatal("AdoptLoadedInstance не создал инстанс для нового бэкенда")
	}
	if cand := r81Candidates(mgr); len(cand) != 1 || cand[0] != "b" {
		t.Fatalf("кандидаты %v, ожидалось [b]", cand)
	}
}

func TestGroupR81_DropInstanceOnUnloadEvent(t *testing.T) {
	mgr := r81ManagerWithInstances(t, "a", "b")
	mgr.AdoptLoadedInstance("r81-model", "a")
	mgr.AdoptLoadedInstance("r81-model", "b")

	if !mgr.DropInstance("r81-model", "b") {
		t.Fatal("DropInstance вернул false для существующего инстанса")
	}
	if cand := r81Candidates(mgr); len(cand) != 1 || cand[0] != "a" {
		t.Fatalf("кандидаты %v, ожидалось [a]", cand)
	}
	if mgr.DropInstance("r81-model", "b") {
		t.Error("повторный DropInstance не должен считаться изменением")
	}
}

func TestGroupR81_SelectRotatesInstances(t *testing.T) {
	mgr := r81ManagerWithInstances(t, "a", "b")
	mgr.AdoptLoadedInstance("r81-model", "a")
	mgr.AdoptLoadedInstance("r81-model", "b")
	// Обе реплики свободны (загрузка 0) — обход должен быть детерминированным.
	mgr.SetBackendLoadFn(func(string) float64 { return 0 })

	sel := NewGroupAwareSelector(mgr)
	first := sel.Select("r81-model")
	second := sel.Select("r81-model")
	if first == "" || second == "" {
		t.Fatalf("селектор вернул пустой бэкенд: %q, %q", first, second)
	}
	if first == second {
		t.Fatalf("оба запроса ушли на %q — обход не чередует реплики", first)
	}
	// Третий запрос — снова на первую (UseCount сравнялся).
	third := sel.Select("r81-model")
	if third != first {
		t.Errorf("третий выбор %q, ожидался %q (чередование a/b/a)", third, first)
	}
}

func TestGroupR81_SelectPrefersLessLoaded(t *testing.T) {
	mgr := r81ManagerWithInstances(t, "a", "b")
	mgr.AdoptLoadedInstance("r81-model", "a")
	mgr.AdoptLoadedInstance("r81-model", "b")
	// "a" занят, "b" свободен → выбор всегда "b", несмотря на UseCount.
	mgr.SetBackendLoadFn(func(id string) float64 {
		if id == "a" {
			return 1.0
		}
		return 0
	})
	sel := NewGroupAwareSelector(mgr)
	for i := 0; i < 3; i++ {
		if got := sel.Select("r81-model"); got != "b" {
			t.Fatalf("выбор %q, ожидался b (a загружен на 100%%)", got)
		}
	}
}
