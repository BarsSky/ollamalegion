// failures_r477.go — R477 (2026-11-09): последние сбои воркера.
//
// ЗАЧЕМ. Балансер теперь знает стадию, на которой умер ход (R477), но его код
// не видит, что происходило ВНУТРИ воркера: 500/503 от воркера балансер
// показывал как «апстрим ответил ошибкой», без указания, какой эндпоинт и какой
// ход (request_id) её дал. По живой жалобе «токены генерировались, ответ не
// дошёл» это ровно тот случай, где нужен ответ «сломался воркер, вот запрос».
//
// ЧТО ЗДЕСЬ. Кольцо последних ответов 5xx: время, request_id (из заголовка
// балансера), путь, код, длительность. Попадает в диагностический архив
// (failures.json) без дополнительных флагов — это несколько десятков записей.
package main

import (
	"sync"
	"time"
)

// workerFailureR477 — один сбой воркера.
type workerFailureR477 struct {
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id,omitempty"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMs int64     `json:"duration_ms"`
}

// workerFailuresMaxR477 — сколько последних сбоев держим.
const workerFailuresMaxR477 = 50

var (
	workerFailuresMu  sync.Mutex
	workerFailuresLog []workerFailureR477
)

// recordWorkerFailureR477 — запомнить ответ 5xx (вызывается из loggingMiddleware).
func recordWorkerFailureR477(f workerFailureR477) {
	workerFailuresMu.Lock()
	defer workerFailuresMu.Unlock()
	if f.Time.IsZero() {
		f.Time = time.Now()
	}
	workerFailuresLog = append(workerFailuresLog, f)
	if len(workerFailuresLog) > workerFailuresMaxR477 {
		workerFailuresLog = workerFailuresLog[len(workerFailuresLog)-workerFailuresMaxR477:]
	}
}

// RecentWorkerFailuresR477 — снимок кольца (новые в конце).
func RecentWorkerFailuresR477() []workerFailureR477 {
	workerFailuresMu.Lock()
	defer workerFailuresMu.Unlock()
	out := make([]workerFailureR477, len(workerFailuresLog))
	copy(out, workerFailuresLog)
	return out
}

// resetWorkerFailuresR477 — очистить кольцо (тесты).
func resetWorkerFailuresR477() {
	workerFailuresMu.Lock()
	workerFailuresLog = nil
	workerFailuresMu.Unlock()
}
