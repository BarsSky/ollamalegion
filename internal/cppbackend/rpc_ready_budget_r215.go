// rpc_ready_budget_r215.go — R215 (2026-10-21).
//
// «Узел раскладки занят» — не ошибка раскладки, а состояние: rpc-server
// обслуживает ОДНО соединение за раз, поэтому сессия прошлого клиента (и тем
// более полумёртвая сессия после сбоя мастера) блокирует новую загрузку. Гейт
// R177 проверял это четырьмя пробами по 300 мс (1.2 с) и после этого грузил
// модель БЕЗ раскладки: слои уезжали на локальную карту, а оператор узнавал об
// этом только по чипу «раскладка не применилась».
//
// Здесь два изменения.
//
//  1. Бюджет ожидания настраивается (CPPWORKER_RPC_READY_WAIT_SEC, дефолт 30 с).
//     Освобождение узла после смерти клиента занимает до ~1.5-2 минут (keepalive,
//     см. docs/rpc-donor-vram-lifecycle.md), поэтому короткое ожидание почти
//     гарантированно даёт «загрузку без раскладки». 30 секунд — компромисс: за
//     них узел успевает освободиться в типичном случае, а загрузка не висит
//     непонятно сколько. 0 = не ждать (прежнее поведение), верхняя граница 600 с.
//
//  2. Серия таймаутов запоминается. Два подряд таймаута при доступном (TCP)
//     узле — признак зависшего узла, который keepalive не лечит: процесс жив,
//     сокет отвечает, но команды не обслуживаются. Это единственный случай, когда
//     нужен перезапуск rpc-server на доноре; теперь он называется в /api/models
//     полем rpc_layout.stuck_suspected, а не остаётся догадкой оператора.
package cppbackend

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// rpcReadyWaitDefaultSec — сколько ждать занятый узел по умолчанию.
	rpcReadyWaitDefaultSec = 30
	// rpcReadyWaitMaxSec — верхняя граница (защита от «повесить загрузку на час»).
	rpcReadyWaitMaxSec = 600
	// rpcReadyProbeDelay — интервал между пробами протокола.
	rpcReadyProbeDelay = 300 * time.Millisecond
)

// rpcReadyWaitBudget — бюджет ожидания узла раскладки из
// CPPWORKER_RPC_READY_WAIT_SEC (секунды). 0 = не ждать. Некорректное значение —
// дефолт (молча падать из-за опечатки в .env нельзя, но и угадывать число -
// тоже: пишем дефолт и продолжаем).
func rpcReadyWaitBudget() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CPPWORKER_RPC_READY_WAIT_SEC"))
	if raw == "" {
		return rpcReadyWaitDefaultSec * time.Second
	}
	sec, err := strconv.Atoi(raw)
	if err != nil || sec < 0 {
		return rpcReadyWaitDefaultSec * time.Second
	}
	if sec > rpcReadyWaitMaxSec {
		sec = rpcReadyWaitMaxSec
	}
	return time.Duration(sec) * time.Second
}

// rpcReadyWaitAttempts — сколько проб протокола укладывается в бюджет. Минимум
// одна проба: «0 секунд» означает «проверить один раз и не ждать», а не «не
// проверять вовсе».
func rpcReadyWaitAttempts(budget, delay time.Duration) int {
	if delay <= 0 {
		delay = rpcReadyProbeDelay
	}
	if budget <= 0 {
		return 1
	}
	n := int(budget/delay) + 1
	if n < 1 {
		n = 1
	}
	// Верхняя защита: бюджет уже ограничен, но attempts не должен стать
	// бесконечным при микроскопическом delay.
	if n > rpcReadyWaitMaxSec*1000 {
		n = rpcReadyWaitMaxSec * 1000
	}
	return n
}

// noteRPCReadyGate — запомнить результат гейта: серия таймаутов подряд.
func (b *Backend) noteRPCReadyGate(ready bool) {
	if b == nil {
		return
	}
	if ready {
		b.rpcReadyTimeoutStreak.Store(0)
		return
	}
	b.rpcReadyTimeoutStreak.Add(1)
}

// RPCReadyTimeoutStreak — сколько подряд гейтов не смогли получить устройства
// узла (0 = последний гейт был успешным или гейтов ещё не было).
func (b *Backend) RPCReadyTimeoutStreak() int32 {
	if b == nil {
		return 0
	}
	return b.rpcReadyTimeoutStreak.Load()
}

// RPCLayoutStuckSuspected — узел доступен по TCP, но не обслуживает команды
// два гейта подряд: вероятнее всего он завис и держит VRAM прошлой сессии.
// Это тот случай, где помогает только перезапуск rpc-server на доноре.
func (b *Backend) RPCLayoutStuckSuspected() bool {
	return b.RPCReadyTimeoutStreak() >= 2
}
