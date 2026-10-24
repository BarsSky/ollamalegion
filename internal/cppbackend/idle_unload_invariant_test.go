//go:build llama_stub

// idle_unload_invariant_test.go — R119 (2026-10-15), пункт 1 плана.
//
// ПОДОЗРЕНИЕ ОПЕРАТОРА: «idle-unload выгружает модель, пока она генерирует, и
// после этого клиент получает Internal Server Error».
//
// Разбор показал, что 500-е приходили от ДРУГОЙ причины (запрос к несуществующей
// модели: cppworker отвечал 500 без тела, балансер игнорировал терминальный 404 и
// крутил retry — исправлено в 0.7.111). Но у подозрения есть и вторая половина:
// защита «не выгружать занятую модель» должна быть доказана, а не выведена из
// чтения кода. Живьём нужную длительность получить трудно (стенд отдаёт 300
// токенов за 10 c, а минимальный таймаут простоя — 1 минута), поэтому инвариант
// проверяется детерминированно: менеджер зависит от интерфейса idleUnloadBackend,
// и тест подаёт ему ровно те состояния моделей, которые важны.
package cppbackend

import (
	"testing"
	"time"
)

// fakeIdleBackend — источник моделей для менеджера автовыгрузки.
type fakeIdleBackend struct {
	models  []ModelInfo
	unloads []string
}

func (f *fakeIdleBackend) ListModels() []ModelInfo { return f.models }
func (f *fakeIdleBackend) UnloadModel(name string) error {
	f.unloads = append(f.unloads, name)
	return nil
}

func idleModel(name string, state LoadState, activeQueries int, lastUsedAgo time.Duration) ModelInfo {
	return ModelInfo{
		Name:          name,
		State:         state,
		ActiveQueries: activeQueries,
		LastUsedAt:    time.Now().Add(-lastUsedAgo),
		LoadedAt:      time.Now().Add(-lastUsedAgo),
	}
}

// TestR119_IdleUnload_SkipsModelDuringGeneration — ГЛАВНЫЙ инвариант: модель с
// активными запросами не выгружается, даже если её простой давно перекрыл таймаут.
//
// Тест падает, если убрать проверку ActiveQueries > 0 в checkAndUnload: тогда
// выгрузка произойдёт прямо во время генерации и клиент получит обрыв.
func TestR119_IdleUnload_SkipsModelDuringGeneration(t *testing.T) {
	fake := &fakeIdleBackend{
		models: []ModelInfo{
			// Простой 2 часа, таймаут 1 минута — но модель СЕЙЧАС генерирует.
			idleModel("busy", StateLoaded, 1, 2*time.Hour),
		},
	}
	m := newIdleUnloadManagerFor(fake, time.Minute)
	m.checkAndUnload()

	if len(fake.unloads) != 0 {
		t.Fatalf("выгружено %v: модель с активным запросом (active_queries=1) выгружать нельзя — "+
			"клиент получит обрыв на середине ответа", fake.unloads)
	}
}

// TestR119_IdleUnload_UnloadsAfterTimeout — контроль: та же модель БЕЗ активных
// запросов и с истёкшим простоем выгружается (иначе автовыгрузка сломана вовсе).
func TestR119_IdleUnload_UnloadsAfterTimeout(t *testing.T) {
	fake := &fakeIdleBackend{
		models: []ModelInfo{
			idleModel("idle-too-long", StateLoaded, 0, 2*time.Hour),
		},
	}
	m := newIdleUnloadManagerFor(fake, time.Minute)
	m.checkAndUnload()

	if len(fake.unloads) != 1 || fake.unloads[0] != "idle-too-long" {
		t.Fatalf("выгрузки = %v, want [idle-too-long] (простой 2 часа при таймауте 1 минута)", fake.unloads)
	}
}

// TestR119_IdleUnload_KeepsFreshModel — модель, использованная только что, не
// выгружается (idle считается от LastUsedAt, а не от LoadedAt).
func TestR119_IdleUnload_KeepsFreshModel(t *testing.T) {
	fake := &fakeIdleBackend{
		models: []ModelInfo{
			idleModel("fresh", StateLoaded, 0, 5*time.Second),
		},
	}
	m := newIdleUnloadManagerFor(fake, time.Minute)
	m.checkAndUnload()
	if len(fake.unloads) != 0 {
		t.Errorf("выгружена свежая модель (простой 5 c при таймауте 1 мин): %v", fake.unloads)
	}
}

// TestR119_IdleUnload_SkipsNotLoadedStates — модель в состоянии loading/error не
// выгружается (иначе выгрузка ломает идущую загрузку).
func TestR119_IdleUnload_SkipsNotLoadedStates(t *testing.T) {
	for _, state := range []LoadState{StateLoading, StateError} {
		fake := &fakeIdleBackend{
			models: []ModelInfo{idleModel("m-"+string(state), state, 0, 2*time.Hour)},
		}
		m := newIdleUnloadManagerFor(fake, time.Minute)
		m.checkAndUnload()
		if len(fake.unloads) != 0 {
			t.Errorf("state=%s: выгрузка %v, want ничего (выгружаем только loaded)", state, fake.unloads)
		}
	}
}

// TestR119_IdleUnload_NeverUnloadsWithoutTimeout — при таймауте 0 (автовыгрузка
// выключена) менеджер не выгружает даже давно простаивающую модель.
func TestR119_IdleUnload_NeverUnloadsWithoutTimeout(t *testing.T) {
	fake := &fakeIdleBackend{
		models: []ModelInfo{idleModel("ancient", StateLoaded, 0, 24*time.Hour)},
	}
	m := newIdleUnloadManagerFor(fake, 0)
	m.checkAndUnload()
	if len(fake.unloads) != 0 {
		t.Errorf("при выключенной автовыгрузке (timeout=0) выгружено %v", fake.unloads)
	}
}

// TestR119_IdleUnload_BusyAmongIdle — смешанный случай: занятая модель остаётся,
// простаивающая выгружается. Именно этот сценарий оператор и описывал («одна
// сессия работает, вторая потерялась»).
func TestR119_IdleUnload_BusyAmongIdle(t *testing.T) {
	fake := &fakeIdleBackend{
		models: []ModelInfo{
			idleModel("generating", StateLoaded, 3, 3*time.Hour),
			idleModel("abandoned", StateLoaded, 0, 3*time.Hour),
		},
	}
	m := newIdleUnloadManagerFor(fake, time.Minute)
	m.checkAndUnload()

	if len(fake.unloads) != 1 || fake.unloads[0] != "abandoned" {
		t.Fatalf("выгрузки = %v, want ровно [abandoned]: работающая модель обязана остаться", fake.unloads)
	}
}
