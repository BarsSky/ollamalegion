//go:build llama_stub

// idle_unload_runtime_r119_test.go — R119 (2026-10-15), пункт 3 плана.
//
// ЖИВОЙ ДЕФЕКТ (стенд). Оператор меняет idleUnloadMinutes в WebUI
// (PUT /api/v1/cppworker/config/update — поле ggufOptIdleUnloadMinutes):
//
//	PUT {"idleUnloadMinutes":3} → 200 {"applied":["idleUnloadMinutes"],"status":"updated"}
//	GET /api/v1/cppworker/config → idleUnloadMinutes = 3
//	…а живой IdleUnloadManager продолжает считать по 10 минутам,
//
// потому что таймаут и интервал проверки записывались ОДИН РАЗ в конструкторе
// (backend.go: NewIdleUnloadManager(b, minutes*time.Minute)), сеттера не было, а
// тикер продолжал идти по старому расписанию. То есть настройка «сохранялась», но
// не действовала — ровно та жалоба «непонятно, применяются ли настройки».
//
// Вторая половина дефекта: значение не переживало пересоздание контейнера, так как
// порядок загрузки конфигурации — JSON-дефолты → env, и
// CPPWORKER_IDLE_UNLOAD_MINUTES=10 затирал сохранённое.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestR119_SetIdleTimeout_AppliesToLiveManager — сеттер меняет таймаут и интервал
// ЖИВОГО менеджера (до правки метода не было вовсе).
func TestR119_SetIdleTimeout_AppliesToLiveManager(t *testing.T) {
	// Нулевого Backend достаточно: сеттер трогает только поле менеджера и мьютекс,
	// а фоновый цикл на пустом бэкенде (нет моделей) ничего не выгружает.
	b := &cppbackend.Backend{}
	defer b.SetIdleUnloadTimeout(0)

	if got := b.SetIdleUnloadTimeout(10 * time.Minute); got != 10*time.Minute {
		t.Fatalf("SetIdleUnloadTimeout(10m) = %v, want 10m", got)
	}
	d, running := b.IdleUnloadStatus()
	if d != 10*time.Minute || !running {
		t.Fatalf("после включения: timeout=%v running=%v, want 10m/true", d, running)
	}

	// Оператор выставил 3 минуты — менеджер обязан увидеть именно 3.
	if got := b.SetIdleUnloadTimeout(3 * time.Minute); got != 3*time.Minute {
		t.Fatalf("SetIdleUnloadTimeout(3m) = %v, want 3m (живой менеджер не обновился)", got)
	}
	d, running = b.IdleUnloadStatus()
	if d != 3*time.Minute {
		t.Errorf("действующий timeout = %v, want 3m — правка из WebUI не дошла до живого менеджера", d)
	}
	if !running {
		t.Error("после смены таймаута цикл должен остаться запущенным")
	}

	// 0 = выключено.
	if got := b.SetIdleUnloadTimeout(0); got != 0 {
		t.Errorf("SetIdleUnloadTimeout(0) = %v, want 0", got)
	}
	if d, running := b.IdleUnloadStatus(); d != 0 || running {
		t.Errorf("после выключения: timeout=%v running=%v, want 0/false", d, running)
	}
}

// TestR119_SetIdleTimeout_IdempotentStart — повторные вызовы не плодят циклы
// (иначе две горутины выгружали бы модели параллельно).
func TestR119_SetIdleTimeout_IdempotentStart(t *testing.T) {
	b := &cppbackend.Backend{}
	defer b.SetIdleUnloadTimeout(0)
	for i := 0; i < 5; i++ {
		b.SetIdleUnloadTimeout(time.Duration(5+i) * time.Minute)
	}
	d, running := b.IdleUnloadStatus()
	if !running || d != 9*time.Minute {
		t.Errorf("после пяти смен: timeout=%v running=%v, want 9m/true", d, running)
	}
	b.SetIdleUnloadTimeout(0)
}

// --- операторские переопределения: приоритет над env и переживание рестарта ----

// TestR119_RuntimeOverride_WinsOverEnv — файл переопределений читается ПОСЛЕ env
// и потому побеждает.
func TestR119_RuntimeOverride_WinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)

	// Оператор в WebUI выставил 3 минуты.
	recordRuntimeOverride("idleUnloadMinutes", 3)

	// Так выглядит конфиг ПОСЛЕ окружения (compose ставит 10).
	cfg := &cppbackend.Config{IdleUnloadMinutes: 10}
	applied := applyRuntimeOverrides(cfg)

	if cfg.IdleUnloadMinutes != 3 {
		t.Errorf("idleUnloadMinutes = %d, want 3: файл переопределений обязан побеждать env "+
			"(иначе правка из WebUI живёт до первого пересоздания контейнера)", cfg.IdleUnloadMinutes)
	}
	if len(applied) != 1 || applied[0] != "idleUnloadMinutes" {
		t.Errorf("applied = %v, want [idleUnloadMinutes]", applied)
	}

	// Файл лежит там, где его ждут (проверяем, что путь не «в никуда»).
	path := filepath.Join(dir, runtimeOverridesFileName)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("файл переопределений не создан: %v", err)
	}
}

// TestR119_RuntimeOverride_NoFileKeepsEnv — без файла значение окружения не
// трогается (обратная совместимость).
func TestR119_RuntimeOverride_NoFileKeepsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)

	cfg := &cppbackend.Config{IdleUnloadMinutes: 10}
	if applied := applyRuntimeOverrides(cfg); len(applied) != 0 {
		t.Errorf("applied = %v, want пусто (файла нет)", applied)
	}
	if cfg.IdleUnloadMinutes != 10 {
		t.Errorf("idleUnloadMinutes = %d, want 10 (значение env не тронуто)", cfg.IdleUnloadMinutes)
	}
}

// TestR119_RuntimeOverride_ZeroMeansOff — явный 0 из WebUI обязан побеждать
// env=10: «выключить автовыгрузку» — самый частый сценарий оператора.
func TestR119_RuntimeOverride_ZeroMeansOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)

	recordRuntimeOverride("idleUnloadMinutes", 0)
	cfg := &cppbackend.Config{IdleUnloadMinutes: 10}
	if applied := applyRuntimeOverrides(cfg); len(applied) != 1 {
		t.Fatalf("applied = %v, want [idleUnloadMinutes]", applied)
	}
	if cfg.IdleUnloadMinutes != 0 {
		t.Errorf("idleUnloadMinutes = %d, want 0 (оператор выключил автовыгрузку)", cfg.IdleUnloadMinutes)
	}
}

// TestR119_RuntimeOverride_SurvivesRecreate — эмуляция пересоздания контейнера:
// конфиг читается заново (значения из env), файл на «томе» уже есть.
func TestR119_RuntimeOverride_SurvivesRecreate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)
	recordRuntimeOverride("idleUnloadMinutes", 7)

	// «Новый контейнер»: свежий Config из окружения (10) + файл на томе.
	fresh := &cppbackend.Config{IdleUnloadMinutes: 10}
	applyRuntimeOverrides(fresh)
	if fresh.IdleUnloadMinutes != 7 {
		t.Errorf("после «пересоздания» idleUnloadMinutes = %d, want 7 — значение из WebUI "+
			"должно переживать рестарт", fresh.IdleUnloadMinutes)
	}
}

// TestR119_RuntimeOverride_BrokenFileIsIgnored — битый файл не должен ломать
// старт: значения окружения остаются в силе.
func TestR119_RuntimeOverride_BrokenFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, runtimeOverridesFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &cppbackend.Config{IdleUnloadMinutes: 10}
	if applied := applyRuntimeOverrides(cfg); len(applied) != 0 {
		t.Errorf("applied = %v, want пусто на битом файле", applied)
	}
	if cfg.IdleUnloadMinutes != 10 {
		t.Errorf("idleUnloadMinutes = %d, want 10", cfg.IdleUnloadMinutes)
	}
}

// TestR119_RuntimeOverride_UnknownKeysNotApplied — чужие ключи сохраняются, но не
// применяются: env обязан оставаться источником истины для остальных параметров.
func TestR119_RuntimeOverride_UnknownKeysNotApplied(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)

	raw := map[string]json.RawMessage{"defaultCtxSize": json.RawMessage("4096")}
	if err := saveRuntimeOverrides(raw); err != nil {
		t.Fatal(err)
	}
	cfg := &cppbackend.Config{DefaultCtxSize: 65536, IdleUnloadMinutes: 10}
	if applied := applyRuntimeOverrides(cfg); len(applied) != 0 {
		t.Errorf("applied = %v, want пусто (ключ не поддерживается)", applied)
	}
	if cfg.DefaultCtxSize != 65536 {
		t.Errorf("defaultCtxSize = %d, want 65536 (env важнее)", cfg.DefaultCtxSize)
	}
}

// TestR119_AppliedListContains — helper для обработчика.
func TestR119_AppliedListContains(t *testing.T) {
	if !appliedListContains([]string{"a", "idleUnloadMinutes"}, "idleUnloadMinutes") {
		t.Error("appliedContains не нашёл ключ")
	}
	if appliedListContains([]string{"a"}, "idleUnloadMinutes") {
		t.Error("appliedContains нашёл отсутствующий ключ")
	}
}
