package sdbackend

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ============================================================
// R88 (2026-10-08): доктрина таймаутов image-воркера.
//
// Та же логика, что у текстового бэкенда (internal/balancer/timeout_policy.go):
// duration-кап на РАБОТУ запрещён, пока оператор не взвёл его явно. Тесты ниже
// фиксируют именно это: «по умолчанию капа нет» — контракт, а не деталь.
// ============================================================

// TestDefaultConfig_NoWorkCaps — дефолты не содержат ни одного капа на работу.
//
// Живой повод: 2048x2048/40 шагов на RTX 3070 идёт 22m30s; кап 600 с обрывал
// генерацию ровно посередине, воркер отдавал 504, а sd-server продолжал считать.
func TestDefaultConfig_NoWorkCaps(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.GenerationTimeoutSec != 0 {
		t.Errorf("GenerationTimeoutSec = %d, want 0 (кап только opt-in: %s)",
			cfg.GenerationTimeoutSec, EnvGenerationTimeout)
	}
	if cfg.StartupTimeoutSec != 0 {
		t.Errorf("StartupTimeoutSec = %d, want 0 (ready ИЛИ смерть процесса — терминальные состояния)",
			cfg.StartupTimeoutSec)
	}
	// Дефолты при этом обязаны оставаться рабочими: очередь и выгрузка не капы.
	if cfg.MaxConcurrent <= 0 || cfg.IdleUnloadMinutes < 0 {
		t.Errorf("дефолты очереди/выгрузки сломаны: maxConcurrent=%d idleUnload=%d",
			cfg.MaxConcurrent, cfg.IdleUnloadMinutes)
	}
}

// TestProfileTimeoutsAllowed_DefaultOff — профильный кап молча НЕ применяется.
//
// profile.json описывает модель (роли файлов, дефолты генерации, плейсмент), а
// не политику таймаутов стенда: значение timeoutSec не должно обрезать работу
// без явного SDWORKER_ALLOW_PROFILE_TIMEOUTS=on (аналог LB_ALLOW_PROFILE_TIMEOUTS).
func TestProfileTimeoutsAllowed_DefaultOff(t *testing.T) {
	t.Setenv(EnvAllowProfileTimeouts, "")
	if ProfileTimeoutsAllowed() {
		t.Fatalf("по умолчанию профильные таймауты должны быть ЗАПРЕЩЕНЫ (%s не задан)", EnvAllowProfileTimeouts)
	}
	for _, v := range []string{"on", "ON", "1", "true", "yes", " on "} {
		t.Setenv(EnvAllowProfileTimeouts, v)
		if !ProfileTimeoutsAllowed() {
			t.Errorf("%q должен включать профильные таймауты", v)
		}
	}
	for _, v := range []string{"off", "0", "false", "no", "maybe", ""} {
		t.Setenv(EnvAllowProfileTimeouts, v)
		if ProfileTimeoutsAllowed() {
			t.Errorf("%q НЕ должен включать профильные таймауты", v)
		}
	}
}

// TestWithOptionalTimeout_ZeroMeansNoDeadline — 0 = «ждать терминального
// состояния», а не «истекло сразу». Ровно на этом ломается наивный
// `context.WithTimeout(ctx, 0)`.
func TestWithOptionalTimeout_ZeroMeansNoDeadline(t *testing.T) {
	ctx, cancel := WithOptionalTimeout(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Error("0 должен означать ОТСУТСТВИЕ дедлайна (без капа)")
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("контекст без капа не должен быть отменён: %v", err)
	}

	armed, cancel2 := WithOptionalTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, ok := armed.Deadline(); !ok {
		t.Error("взведённый кап обязан ставить дедлайн")
	}
}

// TestGenerationTimeout_ProfileCapIsOptIn — приоритеты капа ожидания джобы.
func TestGenerationTimeout_ProfileCapIsOptIn(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	prof := simpleProfile("qwen-image-test")
	prof.TimeoutSec = 1800
	writeProfile(t, dir, prof)
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}
	cfg := DefaultConfig()
	r := &JobRunner{cfg: &cfg, registry: reg}

	// 1) Кап из профиля игнорируется без opt-in: это и был дефект «профиль
	//    молча обрезает генерацию».
	t.Setenv(EnvAllowProfileTimeouts, "")
	if got := r.generationTimeout("qwen-image-test"); got != 0 {
		t.Errorf("без %s профильный кап применился: %v, want 0", EnvAllowProfileTimeouts, got)
	}

	// 2) Opt-in включает профильный кап.
	t.Setenv(EnvAllowProfileTimeouts, "on")
	if got := r.generationTimeout("qwen-image-test"); got != 1800*time.Second {
		t.Errorf("с %s профильный кап не применился: %v, want 30m", EnvAllowProfileTimeouts, got)
	}
	t.Setenv(EnvAllowProfileTimeouts, "")

	// 3) Явный кап из конфига/env действует и без opt-in (он и есть opt-in).
	cfg.GenerationTimeoutSec = 600
	if got := r.generationTimeout("qwen-image-test"); got != 600*time.Second {
		t.Errorf("явный %s не применён: %v, want 10m", EnvGenerationTimeout, got)
	}

	// 4) Ничего не взведено → 0 (ждать терминального состояния).
	cfg.GenerationTimeoutSec = 0
	if got := r.generationTimeout("qwen-image-test"); got != 0 {
		t.Errorf("при выключенных капах timeout = %v, want 0", got)
	}
	if got := r.generationTimeout("нет-такой-модели"); got != 0 {
		t.Errorf("неизвестная модель: timeout = %v, want 0", got)
	}
}

// TestWaitReady_ZeroTimeoutMeansNoCap — 0 не превращается в 180 с.
//
// Проверяем на терминальном состоянии, которое доступно без ожидания: процесс
// уже мёртв. Ошибка обязана быть про смерть процесса, а НЕ про таймаут
// (раньше 0 молча становился 180-секундным капом, и выключить его было нельзя).
func TestWaitReady_ZeroTimeoutMeansNoCap(t *testing.T) {
	cfg := DefaultConfig()
	dir := t.TempDir()
	reg := NewRegistry(dir)
	writeProfile(t, dir, simpleProfile("sd15"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry: %v", err)
	}
	sup := NewSupervisor(&cfg, reg, NewMetrics())
	sup.SetReadinessPoll(time.Millisecond)

	proc := NewFakeProcess(1, nil)
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill fake process: %v", err)
	}
	client := sup.newClient()
	if _, err := sup.waitReady(context.Background(), proc, client, 0); err == nil {
		t.Fatal("ожидалась ошибка про смерть процесса")
	} else if !strings.Contains(err.Error(), "process exited") {
		t.Errorf("ошибка должна говорить о смерти процесса, а не о таймауте: %v", err)
	}
}
