// last_used_at_test.go — Тесты для фикса IdleUnloadManager: использование LastUsedAt
// вместо LoadedAt при вычислении idle-time.
//
// Проблема (см. model_manager.go:checkAndUnload):
//
//	Раньше использовалось model.LoadedAt. Если модель обслуживает запросы
//	каждую секунду, idleTime всё равно рос от момента загрузки, и через
//	idleTimeout cppworker выгружал модель даже при активном использовании.
//
// Решение:
//   - Добавлено поле lastUsedAt в modelInstance (atomic.Value), обновляется
//     при каждом Generate / GenerateStream.
//   - Backend.GetModel / ListModels возвращают LastUsedAt в ModelInfo.
//   - checkAndUnload считает idle от LastUsedAt (fallback на LoadedAt).
//
// Эти тесты проверяют логику fallback-вычисления idle-time через Backend.
// (Полная интеграция с IdleUnloadManager требует реальной загрузки модели
// и не тестируется в unit-тестах без bridge stub.)
package cppbackend

import (
	"testing"
	"time"
)

// TestModelInfo_LastUsedAtField — поле LastUsedAt присутствует в ModelInfo.
// Регрессия: если кто-то удалит поле, JSON-сериализация /api/models перестанет
// возвращать lastUsedAt, монитор покажет "unknown".
func TestModelInfo_LastUsedAtField(t *testing.T) {
	now := time.Now()
	info := ModelInfo{
		Name:       "test",
		State:      StateLoaded,
		LoadedAt:   now,
		LastUsedAt: now.Add(-5 * time.Minute),
	}
	if info.LastUsedAt.IsZero() {
		t.Error("LastUsedAt should be set")
	}
}

// TestGetLastUsedAt_ReturnsLoadedAtIfZero — семантика fallback:
// если lastUsedAt не инициализирован, getLastUsedAt возвращает LoadedAt.
// Это критично для только что загруженной модели, которая ещё не использовалась:
// IdleUnloadManager не должен её выгружать сразу.
func TestGetLastUsedAt_FallbackToLoadedAt(t *testing.T) {
	// Backend нельзя создать без Config; используем минимальную конфигурацию.
	cfg := Config{
		ModelsDir:        t.TempDir(),
		DefaultCtxSize:   512,
		DefaultBatchSize: 64,
		DefaultGPULayers: 0,
	}
	backend := NewBackend(cfg)
	if backend == nil {
		t.Fatal("NewBackend returned nil")
	}
	// Создаём modelInstance вручную (без реальной загрузки).
	loadedAt := time.Now().Add(-1 * time.Hour)
	inst := &modelInstance{
		info: ModelInfo{
			Name:     "test_model",
			State:    StateLoaded,
			LoadedAt: loadedAt,
		},
		// lastUsedAt намеренно НЕ инициализирован (atomic.Value пустой).
	}
	got := backend.getLastUsedAt(inst)
	if !got.Equal(loadedAt) {
		t.Errorf("getLastUsedAt for uninitialized lastUsedAt = %v, want LoadedAt=%v", got, loadedAt)
	}
}

// TestGetLastUsedAt_ReturnsLastUsedAtWhenSet — после инициализации lastUsedAt
// должен возвращать его, а не LoadedAt.
func TestGetLastUsedAt_ReturnsLastUsedAtWhenSet(t *testing.T) {
	cfg := Config{
		ModelsDir:        t.TempDir(),
		DefaultCtxSize:   512,
		DefaultBatchSize: 64,
		DefaultGPULayers: 0,
	}
	backend := NewBackend(cfg)
	loadedAt := time.Now().Add(-1 * time.Hour)
	lastUsed := time.Now().Add(-10 * time.Second) // использована 10 сек назад

	inst := &modelInstance{
		info: ModelInfo{
			Name:     "test_model",
			State:    StateLoaded,
			LoadedAt: loadedAt,
		},
	}
	inst.lastUsedAt.Store(lastUsed)

	got := backend.getLastUsedAt(inst)
	if !got.Equal(lastUsed) {
		t.Errorf("getLastUsedAt with set lastUsedAt = %v, want %v", got, lastUsed)
	}
	// LoadedAt не должен совпадать с lastUsedAt (мы их задали разными).
	if got.Equal(loadedAt) {
		t.Error("getLastUsedAt returned LoadedAt instead of lastUsedAt — bug!")
	}
}

// TestModelInstance_LastUsedAt_AtomicUpdate — lastUsedAt.Store не паникует,
// обновление видно через Load. Это базовая проверка atomic.Value обвязки.
func TestModelInstance_LastUsedAt_AtomicUpdate(t *testing.T) {
	inst := &modelInstance{}
	first := time.Now()
	inst.lastUsedAt.Store(first)
	loaded := inst.lastUsedAt.Load()
	if loaded == nil {
		t.Fatal("lastUsedAt.Load returned nil after Store")
	}
	t1, ok := loaded.(time.Time)
	if !ok {
		t.Fatalf("lastUsedAt type = %T, want time.Time", loaded)
	}
	if !t1.Equal(first) {
		t.Errorf("lastUsedAt = %v, want %v", t1, first)
	}
	// Второе обновление.
	second := first.Add(5 * time.Second)
	inst.lastUsedAt.Store(second)
	t2, ok := inst.lastUsedAt.Load().(time.Time)
	if !ok {
		t.Fatalf("lastUsedAt type = %T after second Store, want time.Time", inst.lastUsedAt.Load())
	}
	if !t2.Equal(second) {
		t.Errorf("lastUsedAt after second Store = %v, want %v", t2, second)
	}
}

// TestIdleUnloadManager_IdleTimeoutZero — если idleTimeout <= 0, Start ничего не делает.
// Это проверка «безопасного» состояния при отключённом idle-unload.
func TestIdleUnloadManager_IdleTimeoutZero(t *testing.T) {
	cfg := Config{
		ModelsDir:        t.TempDir(),
		DefaultCtxSize:   512,
		DefaultBatchSize: 64,
		DefaultGPULayers: 0,
	}
	backend := NewBackend(cfg)
	mgr := NewIdleUnloadManager(backend, 0)
	if mgr == nil {
		t.Fatal("NewIdleUnloadManager returned nil")
	}
	// Не должно паниковать; Start просто ничего не запускает.
	mgr.Start()
	mgr.Stop()
}

// TestIdleUnloadManager_NewWithValidTimeout — проверка конструктора с валидным timeout.
func TestIdleUnloadManager_NewWithValidTimeout(t *testing.T) {
	cfg := Config{
		ModelsDir:        t.TempDir(),
		DefaultCtxSize:   512,
		DefaultBatchSize: 64,
		DefaultGPULayers: 0,
	}
	backend := NewBackend(cfg)
	mgr := NewIdleUnloadManager(backend, 60*1000000000) // 60 секунд
	if mgr == nil {
		t.Fatal("NewIdleUnloadManager returned nil")
	}
	if mgr.idleTimeout != 60*1000000000 {
		t.Errorf("idleTimeout = %v, want 60s", mgr.idleTimeout)
	}
}
