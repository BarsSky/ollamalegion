// port_free_skip_test.go — механизм «не проверять занятость порта в тестах»
// (R742, CI-флейк).
//
// ЖИВОЙ ФЛЕЙК (прогон 45329600591, шаг «Test internal/...»):
//
//	--- FAIL: TestGenerateSync_EngineQueueFull (60.09s)
//	FAIL	ollama-loadbalancer/internal/sdbackend	61.645s
//
// Тесты берут порт так: `net.Listen("127.0.0.1:0")` → номер → Close → отдают
// супервизору. Между Close и проверкой порт может занять ДРУГОЙ тестовый
// процесс (Go гоняет пакеты параллельно), и `waitPortFree` честно ждёт
// portFreeTimeout = 60 с, после чего тест краснеет не по своей теме.
//
// Ниже проверяются ОБА свойства механизма:
//  1. с `SetPortFreeSkipForTests` загрузка не зависит от занятости порта (это и
//     нужно тестам, где порт не участвует в проверяемом поведении);
//  2. БЕЗ ручки проверка работает как раньше — занятый порт даёт ошибку
//     «still busy». Иначе «лечение флейка» тихо отключило бы саму проверку для
//     прода: именно этого здесь нельзя допустить.
package sdbackend

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// heldPortR742 — занять порт на 127.0.0.1 и вернуть его номер + закрыватель.
func heldPortR742(t *testing.T) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return port, func() { _ = ln.Close() }
}

// supervisorOnPortR742 — супервизор с профилем sd15 и заданным портом движка.
func supervisorOnPortR742(t *testing.T, port int) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	reg := NewRegistry(dir)
	writeProfile(t, dir, simpleProfile("sd15"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}
	cfg := DefaultConfig()
	cfg.ModelsDir = dir
	cfg.ListenIP = "127.0.0.1"
	cfg.ServerPort = port
	cfg.StartupTimeoutSec = 5
	cfg.ImagesDir = t.TempDir()
	sup := NewSupervisor(&cfg, reg, NewMetrics())
	sup.SetReadinessPoll(2 * time.Millisecond)
	sup.SetReadinessTimeout(2 * time.Second)
	sup.SetSDServerBaseURL("http://127.0.0.1:1") // сеть тут не проверяется
	sup.SetRunner(NewFakeRunner(func([]string) (Process, error) { return NewFakeProcess(1, nil), nil }))
	return sup
}

// TestR742_PortBusyIsRefusedWithoutSkip — без ручки занятый порт по-прежнему
// отказ: проверка не «исчезла», а только отключается явно.
func TestR742_PortBusyIsRefusedWithoutSkip(t *testing.T) {
	port, release := heldPortR742(t)
	defer release()
	sup := supervisorOnPortR742(t, port)
	sup.SetPortFreeTimeout(150 * time.Millisecond) // тест не должен ждать 60 с

	_, err := sup.Load(context.Background(), "sd15", LoadOptions{})
	if err == nil {
		t.Fatal("загрузка на занятом порту прошла: проверка занятости порта исчезла")
	}
	if !strings.Contains(err.Error(), "still busy") {
		t.Fatalf("ошибка не про занятый порт: %v", err)
	}
}

// TestR742_SkipMakesPortCheckIndependent — с ручкой загрузка не смотрит на порт.
// Это ровно то, что нужно тестам, где порт не участвует в проверяемом поведении:
// чужой процесс на этом порту больше не превращает проверку 429 в 60-секундное
// ожидание.
func TestR742_SkipMakesPortCheckIndependent(t *testing.T) {
	port, release := heldPortR742(t)
	defer release()
	sup := supervisorOnPortR742(t, port)
	sup.SetPortFreeSkipForTests(true)

	// Загрузка всё равно не дойдёт до готовности: настоящего sd-server нет (мок
	// движка слушает другой адрес). Важно РОВНО одно: отказа «порт занят» нет —
	// значит проверка пропущена, и тест не потратит 60 с на чужой порт.
	_, err := sup.Load(context.Background(), "sd15", LoadOptions{})
	if err == nil {
		return // загрузка прошла целиком — тоже валидно
	}
	if strings.Contains(err.Error(), "still busy") {
		t.Fatalf("с SetPortFreeSkipForTests проверка занятости порта всё ещё выполняется: %v", err)
	}
}

var _ = types.ImageModelProfile{}
