//go:build llama_stub

// load_stall_r227_test.go — R227 (2026-10-22).
//
// ЖИВОЙ ДЕФЕКТ. «Применить и перезагрузить» с недоступным узлом/невалидными
// параметрами: воркер выгружал модель и дальше не грузил её — ни ошибки, ни
// отката в логе, а все последующие загрузки висели до перезапуска контейнера
// (держится loadSingleFlight). Без стека горутин причину не видно.
//
// Контракт R227:
//  1. пока загрузка идёт — сторож молчит;
//  2. по истечении порога — один ERROR со стеком горутин;
//  3. завершённая загрузка (done закрыт) сторож не будит;
//  4. порог читается из CPPWORKER_LOAD_STALL_SEC, значения меньше 30 с поднимаются.
package cppbackend

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestR227_StallThresholdFromEnv(t *testing.T) {
	t.Setenv("CPPWORKER_LOAD_STALL_SEC", "")
	if got := loadStallThreshold(); got != 300*time.Second {
		t.Errorf("по умолчанию ожидалось 300s, получено %v", got)
	}
	t.Setenv("CPPWORKER_LOAD_STALL_SEC", "45")
	if got := loadStallThreshold(); got != 45*time.Second {
		t.Errorf("ожидалось 45s, получено %v", got)
	}
	t.Setenv("CPPWORKER_LOAD_STALL_SEC", "5") // меньше минимума
	if got := loadStallThreshold(); got != 30*time.Second {
		t.Errorf("значение меньше 30s должно подниматься до 30s, получено %v", got)
	}
	t.Setenv("CPPWORKER_LOAD_STALL_SEC", "мусор")
	if got := loadStallThreshold(); got != 300*time.Second {
		t.Errorf("мусор в env должен давать дефолт, получено %v", got)
	}
}

func TestR227_StallWatchdogFiresOnTimeout(t *testing.T) {
	var fired atomic.Bool
	dump := func() string { fired.Store(true); return "goroutine stack" }
	done := make(chan struct{})
	loadStallWatchdog("gemma", 20*time.Millisecond, done, dump, nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !fired.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	if !fired.Load() {
		t.Fatal("сторож не сработал за 2 с при пороге 20 мс — клинч снова будет невидимым")
	}
}

// Живой дефект: зависшая загрузка держала слот, и все следующие висели молча.
func TestR227_LoadFlightHasDeadline(t *testing.T) {
	b := &Backend{}
	b.loadSingleFlight.Lock() // «зависла» предыдущая загрузка
	defer b.loadSingleFlight.Unlock()

	start := time.Now()
	err := b.acquireLoadFlight(300 * time.Millisecond)
	if err == nil {
		t.Fatal("захват слота удался, хотя он занят — дедлайн не работает")
	}
	if !strings.Contains(err.Error(), "заблокирована") {
		t.Errorf("непонятная ошибка: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("дедлайн не соблюдён: %v", elapsed)
	}

	// Свободный слот захватывается сразу.
	b2 := &Backend{}
	if err := b2.acquireLoadFlight(50 * time.Millisecond); err != nil {
		t.Fatalf("свободный слот не захвачен: %v", err)
	}
	b2.loadSingleFlight.Unlock()
}

// Сторож обязан не только печатать стек, но и просить отмену загрузки.
func TestR227_StallWatchdogRequestsAbort(t *testing.T) {
	var aborted atomic.Bool
	done := make(chan struct{})
	loadStallWatchdog("gemma", 20*time.Millisecond, done, func() string { return "stack" },
		func() { aborted.Store(true) })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !aborted.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	if !aborted.Load() {
		t.Fatal("по таймауту загрузка не была отменена — она продолжит держать слот")
	}
}
func TestR227_StallWatchdogSilentWhenLoadFinishes(t *testing.T) {
	var fired atomic.Bool
	dump := func() string { fired.Store(true); return "stack" }
	done := make(chan struct{})
	loadStallWatchdog("gemma", 50*time.Millisecond, done, dump, nil)
	close(done) // загрузка завершилась штатно
	time.Sleep(150 * time.Millisecond)
	if fired.Load() {
		t.Fatal("сторож сработал после завершения загрузки — ложный ERROR в логе")
	}
}
