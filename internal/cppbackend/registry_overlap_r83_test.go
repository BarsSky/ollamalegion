//go:build llama_stub

// registry_overlap_r83_test.go — R83 §9.6 D-B (2026-09-27).
//
// Симптом, который здесь воспроизводится: «загрузка модели завершилась в логе,
// а /api/models отдаёт count=0». Причина — провалившаяся загрузка убирала за
// собой фантомную запись безусловным `delete(b.models, name)`, и при перекрытии
// загрузок выбивала уже НОВУЮ (работающую) запись, созданную другой горутиной.
//
// Тесты проверяют не «delete работает», а инвариант реестра: чужая запись не
// исчезает от нашего провала. В stub-сборке bridge.LoadModel не падает, поэтому
// добраться до пути очистки иначе нельзя — отсюда прямая работа с
// removeOwnInstance и симулятор перекрытия на TryLockLoad/WaitForLoad.
package cppbackend

import (
	"testing"
	"time"
)

// TestR83DB_RemoveOwnInstanceDeletesOwn — база: свой провал убирает свою запись,
// иначе фантом «state=loading» останется в /api/models навсегда.
func TestR83DB_RemoveOwnInstanceDeletesOwn(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	const name = "db-own-instance"

	own := &modelInstance{info: ModelInfo{Name: name, State: StateLoading}}
	b.mu.Lock()
	b.models[name] = own
	b.mu.Unlock()

	b.removeOwnInstance(name, own)

	if _, err := b.GetModel(name); err == nil {
		t.Fatal("своя запись не удалена — в /api/models останется вечный state=loading")
	}
}

// TestR83DB_RemoveOwnInstanceKeepsNewer — ГЛАВНЫЙ тест D-B: если запись уже
// заменена новым инстансом, наш провал её не трогает.
//
// Без исправления (безусловный delete) здесь получалось count=0 при живой,
// успешно загруженной модели.
func TestR83DB_RemoveOwnInstanceKeepsNewer(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	const name = "db-newer-instance"

	failed := &modelInstance{info: ModelInfo{Name: name, State: StateLoading}}
	newer := &modelInstance{info: ModelInfo{Name: name, State: StateLoaded, GPULayers: 20}}

	b.mu.Lock()
	b.models[name] = newer // другая горутина уже пересоздала запись
	b.mu.Unlock()

	b.removeOwnInstance(name, failed)

	got, err := b.GetModel(name)
	if err != nil {
		t.Fatalf("новая (рабочая) запись исчезла после чужого провала: %v — "+
			"это и есть симптом D-B «loaded в логе, count=0 в /api/models»", err)
	}
	if got.State != StateLoaded {
		t.Errorf("состояние новой записи = %v, want %v", got.State, StateLoaded)
	}
	if got.GPULayers != 20 {
		t.Errorf("новая запись повреждена: GPULayers=%d, want 20", got.GPULayers)
	}
}

// TestR83DB_RemoveOwnInstanceMissingIsNoop — записи уже нет (её снял unload):
// очистка не должна ни паниковать, ни создавать запись.
func TestR83DB_RemoveOwnInstanceMissingIsNoop(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	const name = "db-missing-instance"

	b.removeOwnInstance(name, &modelInstance{info: ModelInfo{Name: name}})

	if _, err := b.GetModel(name); err == nil {
		t.Error("очистка создала запись там, где её не было")
	}
}

// TestR83DB_OverlapSimulator_RegistryStaysConsistent — симулятор перекрытия
// load/unload на настоящих примитивах лока.
//
// Сценарий (живой инцидент): пока «медленная» загрузка идёт, её запись кто-то
// заменяет (перезагрузка того же имени). Затем медленная загрузка падает и
// убирает за собой. Инвариант: в реестре остаётся ровно одна запись — свежая, и
// ни один ожидающий загрузку клиент не видит «модели нет» после успеха.
func TestR83DB_OverlapSimulator_RegistryStaysConsistent(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir()})
	const name = "db-overlap"

	// Загрузка №1 занимает лок и регистрирует свой (позже провалившийся) inst.
	taken, err := b.TryLockLoad(name)
	if err != nil || !taken {
		t.Fatalf("TryLockLoad: taken=%v err=%v", taken, err)
	}
	slow := &modelInstance{info: ModelInfo{Name: name, State: StateLoading}}
	b.mu.Lock()
	b.models[name] = slow
	b.mu.Unlock()

	// Второй клиент видит, что модель уже грузится, и ждёт (WaitForLoad).
	waitDone := make(chan bool, 1)
	go func() { waitDone <- b.WaitForLoad(name) }()

	// Перекрытие: запись заменяется свежей (так делает повторная загрузка после
	// unload), затем «медленная» загрузка проваливается и убирает за собой.
	fresh := &modelInstance{info: ModelInfo{Name: name, State: StateLoaded}}
	b.mu.Lock()
	b.models[name] = fresh
	b.mu.Unlock()
	b.removeOwnInstance(name, slow)

	// Ожидающий обязан получить успех: запись в реестре есть.
	b.UnlockLoad(name)
	select {
	case ok := <-waitDone:
		if !ok {
			t.Fatal("ожидающий клиент получил «загрузка провалилась», хотя модель в реестре есть")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitForLoad не проснулся после UnlockLoad")
	}

	// И ровно одна запись — свежая.
	models := b.ListModels()
	if len(models) != 1 {
		t.Fatalf("записей в реестре: %d (%v), want 1 — перекрытие оставило мусор",
			len(models), models)
	}
	if models[0].State != StateLoaded {
		t.Errorf("состояние = %v, want %v (должна остаться свежая запись)", models[0].State, StateLoaded)
	}
}
