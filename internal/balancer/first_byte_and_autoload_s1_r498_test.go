//go:build llama_stub

// first_byte_and_autoload_s1_r498_test.go — R498 (2026-11-19), шаг S1 аудита.
//
// T-1: граница «нет первого байта» была ПЛОСКИМИ 180 с, хотя живая холодная
// загрузка 16–27B на этом же стенде занимает 140–485 с (воркер отдаёт заголовки
// только после ленивой загрузки). Эстиматор по размеру модели
// (EstimateFirstByteTimeoutFromModelSize) был написан и не использовался.
//
// T-3: `LB_AUTO_LOAD_WAIT_SEC=-1` («ждать сколько нужно») давал 30 минут на пути
// без имени модели и 24 часа на пути с именем — один env, два поведения.
package balancer

import (
	"os"
	"testing"
	"time"
)

// TestR498_FirstByteDefaultFollowsModelSize — дефолт растёт с размером модели и
// не обрывает холодную загрузку 16 ГБ раньше 15 минут.
func TestR498_FirstByteDefaultFollowsModelSize(t *testing.T) {
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC", "")
	restore := setModelSizeBytesForTest(func(string) int64 { return 16 * 1024 * 1024 * 1024 })
	defer restore()

	p := &Proxy{}
	got := p.defaultFirstByteTimeoutForModelR498("qwen3.8-27b-q4")

	// Эстиматор на 12–24 ГБ даёт 1800 с.
	if got != 1800*time.Second {
		t.Fatalf("дефолт first-byte = %v, ожидалось 30m (оценка по 16 ГБ); "+
			"плоские 180 с обрывали холодную загрузку 140–485 с", got)
	}
	if got < 300*time.Second {
		t.Fatalf("дефолт %v меньше 300 с — загрузка не успеет начаться", got)
	}
}

// TestR498_FirstByteUnknownSizeIsSafe — размер модели неизвестен: берём оценку
// для неизвестного размера (900 с), а НЕ ноль и не 180 с.
func TestR498_FirstByteUnknownSizeIsSafe(t *testing.T) {
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC", "")
	restore := setModelSizeBytesForTest(func(string) int64 { return 0 })
	defer restore()

	p := &Proxy{}
	got := p.defaultFirstByteTimeoutForModelR498("неизвестная-модель")
	if got != EstimateFirstByteTimeoutFromModelSize(0) {
		t.Fatalf("дефолт = %v, ожидалось значение эстиматора для неизвестного размера (%v)",
			got, EstimateFirstByteTimeoutFromModelSize(0))
	}
	if got <= 0 {
		t.Fatalf("дефолт %v: при неизвестном размере граница не должна исчезать "+
			"(это и был живой разбор R189 — клиент ждал 300 с молча)", got)
	}
}

// TestR498_FirstByteExplicitWins — явная настройка сильнее оценки, включая 0
// («граница выключена осознанно»).
func TestR498_FirstByteExplicitWins(t *testing.T) {
	restore := setModelSizeBytesForTest(func(string) int64 { return 16 * 1024 * 1024 * 1024 })
	defer restore()
	p := &Proxy{}

	t.Setenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC", "0")
	if got := p.defaultFirstByteTimeoutForModelR498("m"); got != 0 {
		t.Fatalf("явный 0 → %v, ожидалось 0 (оператор выключил границу)", got)
	}
	t.Setenv("LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC", "42")
	if got := p.defaultFirstByteTimeoutForModelR498("m"); got != 42*time.Second {
		t.Fatalf("явные 42 с → %v, ожидалось 42s", got)
	}
}

// TestR498_AutoLoadUnlimitedOneMeaning — `-1` даёт ОДИН потолок на обоих путях.
func TestR498_AutoLoadUnlimitedOneMeaning(t *testing.T) {
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "-1")
	os.Unsetenv("LB_AUTO_LOAD_WAIT_SEC_FORCE") // не используется, но фиксируем чистоту

	global := AutoLoadWaitTimeout()
	p := &Proxy{}
	byModel := p.autoLoadWaitTimeoutForModel("любая-модель")

	if global != byModel {
		t.Fatalf("-1 даёт %v без имени модели и %v с именем: один env — два поведения "+
			"(аудит T-3)", global, byModel)
	}
	if global != autoLoadBudgetCeiling {
		t.Fatalf("потолок для -1 = %v, ожидалось %v (потолок БЕЗДЕЙСТВИЯ, "+
			"он не обрывает живую загрузку — продление идёт по растущему elapsedMs)",
			global, autoLoadBudgetCeiling)
	}
}
