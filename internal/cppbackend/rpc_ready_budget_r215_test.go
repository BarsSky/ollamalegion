//go:build llama_stub

// rpc_ready_budget_r215_test.go — R215 (2026-10-21).
//
// «Узел занят» — не редкость, а штатное состояние: rpc-server обслуживает одно
// соединение за раз, и после сбоя мастера прошлая сессия освобождается до
// ~1.5-2 минут (keepalive, R214). Гейт R177 ждал 1.2 с и грузил модель без
// раскладки: слои уезжали на локальную карту, оператор видел «раскладка не
// применилась» и не понимал, что узел просто ещё не освободился.
//
// Тесты фиксируют контракт R215:
//  1. бюджет ожидания настраивается (CPPWORKER_RPC_READY_WAIT_SEC), дефолт 30 с,
//     мусор в env не превращается в бесконечное ожидание;
//  2. число проб считается по бюджету и всегда >= 1;
//  3. серия неудач гейта запоминается, и «два подряд» означает подозрение на
//     зависший узел (это уже не keepalive-случай, а ручной перезапуск).
package cppbackend

import (
	"testing"
	"time"
)

func TestR215_RPCReadyWaitBudget(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"по умолчанию 30 с", "", 30 * time.Second},
		{"явное значение", "45", 45 * time.Second},
		{"ноль = проверить один раз", "0", 0},
		{"мусор -> дефолт", "abc", 30 * time.Second},
		{"отрицательное -> дефолт", "-5", 30 * time.Second},
		{"верхняя граница 600 с", "100000", 600 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CPPWORKER_RPC_READY_WAIT_SEC", tc.env)
			if got := rpcReadyWaitBudget(); got != tc.want {
				t.Fatalf("budget = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestR215_RPCReadyWaitAttempts(t *testing.T) {
	delay := rpcReadyProbeDelay
	if got := rpcReadyWaitAttempts(0, delay); got != 1 {
		t.Errorf("budget=0: attempts = %d, want 1 (одна проба, без ожидания)", got)
	}
	if got := rpcReadyWaitAttempts(30*time.Second, delay); got != 101 {
		t.Errorf("budget=30s delay=300ms: attempts = %d, want 101", got)
	}
	if got := rpcReadyWaitAttempts(-time.Second, delay); got != 1 {
		t.Errorf("negative budget: attempts = %d, want 1", got)
	}
	// Прошлое поведение (4 x 300 мс) должно оставаться достижимым через env=1.
	t.Setenv("CPPWORKER_RPC_READY_WAIT_SEC", "1")
	if got := rpcReadyWaitAttempts(rpcReadyWaitBudget(), delay); got != 4 {
		t.Errorf("env=1s: attempts = %d, want 4 (прежнее поведение R177)", got)
	}
}

func TestR215_RPCReadyGateStreak(t *testing.T) {
	var b Backend
	if b.RPCLayoutStuckSuspected() {
		t.Fatal("свежий backend не может подозревать зависший узел")
	}

	b.noteRPCReadyGate(false)
	if got := b.RPCReadyTimeoutStreak(); got != 1 {
		t.Fatalf("streak = %d, want 1", got)
	}
	if b.RPCLayoutStuckSuspected() {
		t.Error("одного таймаута недостаточно для вывода о зависшем узле")
	}

	b.noteRPCReadyGate(false)
	if !b.RPCLayoutStuckSuspected() {
		t.Error("два таймаута подряд обязаны поднимать stuck_suspected")
	}

	// Успешный гейт сбрасывает серию: узел освободился, подозрение снято.
	b.noteRPCReadyGate(true)
	if got := b.RPCReadyTimeoutStreak(); got != 0 {
		t.Errorf("streak = %d после успешного гейта, want 0", got)
	}
	if b.RPCLayoutStuckSuspected() {
		t.Error("после успешного гейта подозрение обязано сниматься")
	}
}

// Публичные методы должны быть nil-safe: их зовут из отчётов, где backend может
// отсутствовать (stub/тесты).
func TestR215_RPCReadyGateNilSafe(t *testing.T) {
	var b *Backend
	b.noteRPCReadyGate(false)
	if got := b.RPCReadyTimeoutStreak(); got != 0 {
		t.Errorf("nil backend: streak = %d, want 0", got)
	}
	if b.RPCLayoutStuckSuspected() {
		t.Error("nil backend не может подозревать зависший узел")
	}
}
