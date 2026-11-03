// autoload_terminal_r421_test.go — R421 (2026-11-03): конкретный отказ загрузки
// терминален и не ждёт бюджет автозагрузки.
//
// ЖИВОЙ ФАКТ (CI, tests/cppworker_lazy_load): мок отвечал 500 «model file
// corrupted: bad magic number», а балансер ждал полный бюджет (180 с) и только
// потом отдавал 503 с той же причиной. Клиент висел без пользы.
package balancer

import (
	"errors"
	"testing"
)

func TestR421_ConcreteLoadFailureIsTerminal(t *testing.T) {
	cases := []string{
		"auto-load failed: cppworker error (HTTP 500): model file corrupted: bad magic number",
		"failed to load model file: bad magic",
		"unsupported model architecture: qwen9",
		"invalid model header",
	}
	for _, msg := range cases {
		if !isModelLoadFailedError(errors.New(msg)) {
			t.Fatalf("терминальный отказ не распознан: %q", msg)
		}
	}
}

func TestR421_EmptyLoadFailureStillWaits(t *testing.T) {
	// R325.1: «model load failed: » с ПУСТОЙ причиной — таймаут ожидания,
	// модель может ещё грузиться; ждать обязательно.
	if isModelLoadFailedError(errors.New("model load failed: ")) {
		t.Fatal("пустая причина ошибочно признана терминальной — R325.1 сломан")
	}
	if isModelLoadFailedError(errors.New("context canceled")) {
		t.Fatal("отмена клиента ошибочно признана терминальной загрузкой")
	}
	if isModelLoadFailedError(nil) {
		t.Fatal("nil-ошибка признана терминальной")
	}
}
