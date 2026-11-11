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
	"net/http"
	"sync"
	"time"
)

// recordWorkerEventR483 — событие хода, которое не является 5xx, но объясняет
// «почему клиент не получил то, что ждал» (R483). Используется для снятия
// удержания вывода (R482), обрыва записи клиенту, пустого вывода после повторов
// и нераспознанной разметки вызова.
func recordWorkerEventR483(r *http.Request, path, kind, detail string) {
	if r == nil {
		return
	}
	recordWorkerFailureR477(workerFailureR477{
		RequestID: getRequestID(r),
		Method:    r.Method,
		Path:      path,
		Status:    http.StatusOK,
		Kind:      kind,
		Detail:    detail,
	})
}

// workerFailureR477 — один сбой воркера.
//
// R483: сюда попадают не только 5xx. Живой разбор «ответ пришёл одним куском /
// инструмент не применился / клиент ушёл» упирался в то, что в кольце были
// только 500-е, а настоящие события хода — снятие удержания вывода (R482),
// обрыв записи клиенту, пустой вывод после повторов, нераспознанная разметка
// вызова — жили только в тексте лога. Kind — машинный код события, Detail —
// числа/причина для человека.
type workerFailureR477 struct {
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id,omitempty"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMs int64     `json:"duration_ms"`
	Kind       string    `json:"kind,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	// Count — сколько РАЗ одно и то же событие повторилось подряд (R488).
	//
	// ЗАЧЕМ. На 27B-стенде сторож удержания снимает вывод каждые ~20 с, и каждое
	// снятие писало отдельную запись. Кольцо на 50 записей заполнялось рутиной
	// целиком: в архиве 2026-11-11 не осталось ни одного client_cancelled — то
	// есть ответ на «кто отменил запрос» приходил пустым. Теперь повторы
	// схлопываются в одну запись со счётчиком, а редкие события живут.
	Count int `json:"count,omitempty"`
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
	// R488: подряд идущие повторы того же события (kind) по той же заявке
	// (request_id) не добавляют запись, а увеличивают счётчик последней. Порядок
	// и содержимое кольца от этого только информативнее: вместо 50 строк
	// «hold_released held_ms=20034» остаётся одна с «count=37».
	if f.Kind != "" && len(workerFailuresLog) > 0 {
		last := &workerFailuresLog[len(workerFailuresLog)-1]
		if last.Kind == f.Kind && last.RequestID == f.RequestID {
			last.Count++
			last.Detail = f.Detail
			last.Time = f.Time
			return
		}
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
