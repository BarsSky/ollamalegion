// backend_wait_queue_r403.go — R403 (2026-11-05): отдельная очередь ожидания
// СВОБОДНОГО БЭКЕНДА (не слота).
//
// ЖИВОЙ ДЕФЕКТ. Когда healthy llama.cpp-бэкендов не было вовсе (все offline /
// disabled / доноры раскладки), inference-обработчики отвечали
// 503 {"error":"no llama.cpp backend available"} НЕМЕДЛЕННО. Клиент
// (Cline/OpenWebUI) уходил в свой retry или показывал ошибку, хотя через
// секунды бэкенд мог вернуться (рестарт контейнера, завершение heal-раскладки,
// снятие disabled). Это НЕ то же самое, что admission-очередь R67b: там запрос
// уже выбрал бэкенд и ждёт свободный СЛОТ; здесь ещё НЕКОГО выбирать.
//
// ПРАВИЛО. Ждём появления подходящего бэкенда до LB_BACKEND_WAIT_SEC
// (default 120 c; 0 = прежнее поведение «сразу 503»). Очередь FIFO по модели:
// пока первый ожидающий не получил бэкенд, остальные не «перехватывают» его
// повторами — иначе один клиент с быстрым retry-starve'ил бы остальных.
// После таймаута — 503 с числами и позицией (I7): Retry-After,
// X-Backend-Queue-Position, X-Backend-Queue-Wait-Ms.
package balancer

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
)

const (
	backendWaitDefaultSec = 120
	backendWaitPoll       = 250 * time.Millisecond
	backendWaitRetryAfter = 15
)

// backendWaitTimeoutR403 — сколько ждать бэкенд. Горячая настройка
// LB_BACKEND_WAIT_SEC перекрывает env (как LB_ADMISSION_WAIT_SEC).
func backendWaitTimeoutR403() time.Duration {
	sec := int64(backendWaitDefaultSec)
	if v, ok := settings.HotInt("LB_BACKEND_WAIT_SEC"); ok {
		sec = v
	} else if raw := strings.TrimSpace(os.Getenv("LB_BACKEND_WAIT_SEC")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			sec = n
		}
	}
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

// backendWaitQueue — FIFO ожидающих по модели. Отдельный тип (а не срез в
// Proxy), чтобы ожидание можно было тестировать без живого прокси.
type backendWaitQueue struct {
	mu      sync.Mutex
	nextID  uint64
	waiters map[string][]uint64 // model -> очередь id
}

var llmBackendWaitQueue = &backendWaitQueue{waiters: map[string][]uint64{}}

// enqueue ставит ожидающего в очередь модели и возвращает его id и позицию (1-based).
func (q *backendWaitQueue) enqueue(model string) (uint64, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nextID++
	id := q.nextID
	q.waiters[model] = append(q.waiters[model], id)
	return id, len(q.waiters[model])
}

// dequeue убирает ожидающего из очереди модели.
func (q *backendWaitQueue) dequeue(model string, id uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	list := q.waiters[model]
	for i, v := range list {
		if v == id {
			q.waiters[model] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(q.waiters[model]) == 0 {
		delete(q.waiters, model)
	}
}

// isFirst — ожидающий первый в очереди модели (FIFO: только он пробует взять
// бэкенд, остальные ждут, чтобы не перехватывать его повторами).
func (q *backendWaitQueue) isFirst(model string, id uint64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	list := q.waiters[model]
	return len(list) > 0 && list[0] == id
}

// position — текущая позиция ожидающего (1-based; 0 = уже не в очереди).
func (q *backendWaitQueue) position(model string, id uint64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, v := range q.waiters[model] {
		if v == id {
			return i + 1
		}
	}
	return 0
}

// waitBackendR403 — ждёт, пока probe вернёт непустой id бэкенда.
//
// Возвращает:
//   - (id, waited, 0, true)  — бэкенд появился;
//   - ("", waited, pos, false) — таймаут/отмена; pos — позиция на момент выхода
//     (0, если очередь уже покинута).
//
// timeout <= 0 означает «не ждать»: один быстрый probe и выход (прежнее
// поведение, совместимость).
func waitBackendR403(ctx context.Context, q *backendWaitQueue, model string,
	timeout time.Duration, probe func() string) (string, time.Duration, int, bool) {

	start := time.Now()
	if timeout <= 0 {
		id := probe()
		return id, 0, 0, id != ""
	}
	id, pos := q.enqueue(model)
	defer func() {
		q.dequeue(model, id)
	}()

	ticker := time.NewTicker(backendWaitPoll)
	defer ticker.Stop()

	for {
		// Проверяем сразу, без ожидания тика: бэкенд мог появиться, пока мы
		// вставали в очередь.
		if q.isFirst(model, id) {
			if b := probe(); b != "" {
				return b, time.Since(start), pos, true
			}
		}
		select {
		case <-ctx.Done():
			return "", time.Since(start), q.position(model, id), false
		case <-ticker.C:
		}
		if time.Since(start) >= timeout {
			return "", time.Since(start), q.position(model, id), false
		}
	}
}

// selectForInferenceR403 — выбор бэкенда для inference с учётом тумблера
// «не использовать бэкенд» (R250).
//
// ЖИВОЙ ДЕФЕКТ (нашёлся при живой проверке очереди R403). Llama.cpp-путь
// выбирает бэкенд своими хелперами (selectLlamaCppBackendForModelWithNCtx →
// findLeastLoadedBackendWithModel / selectAnyLlamaCppHealthy), и они НЕ
// проверяли operator-disabled: выключенный узел продолжал обслуживать запросы,
// хотя общий selectBackend (backend_selector.go) его уже пропускал. Из-за этого
// «не использовать бэкенд» на llama.cpp-пути не работал, а очередь ожидания
// бэкенда физически не могла включиться: селектор всегда что-то возвращал.
func (lr *LlamaCppRouter) selectForInferenceR403(model string, requestedNCtx int) string {
	if lr == nil || lr.proxy == nil {
		return ""
	}
	id := lr.selectLlamaCppBackendForModelWithNCtx(model, requestedNCtx)
	if id != "" && lr.proxy.backendDisabled(id) {
		return ""
	}
	return id
}

// waitForLlamaCppBackendR403 — обёртка для inference-обработчиков: ждёт
// свободный бэкенд и, если не дождались, сама пишет 503 с числами.
//
// probe должен быть тем же выбором, что и в обработчике (модель + окно), иначе
// очередь «увидит» не тот бэкенд, который выберет запрос.
func (lr *LlamaCppRouter) waitForLlamaCppBackendR403(ctx context.Context, w http.ResponseWriter,
	model string, requestedNCtx int) string {

	timeout := backendWaitTimeoutR403()
	probe := func() string {
		return lr.selectForInferenceR403(model, requestedNCtx)
	}
	if timeout <= 0 {
		if id := probe(); id != "" {
			return id
		}
		writeBackendUnavailableR403(w, model, 0, 0, false)
		return ""
	}
	id, waited, pos, ok := waitBackendR403(ctx, llmBackendWaitQueue, model, timeout, probe)
	if ok {
		if waited > 0 {
			logBackendWaitR403(model, waited, pos)
		}
		return id
	}
	writeBackendUnavailableR403(w, model, waited, pos, true)
	return ""
}

// writeBackendUnavailableR403 — 503 с числами и позицией в очереди.
//
// Прежний ответ был {"error":"no llama.cpp backend available"} без чисел и без
// указания, что запрос вообще стоял в очереди; клиент не мог отличить «нет
// бэкенда» от «подожди и повтори».
func writeBackendUnavailableR403(w http.ResponseWriter, model string, waited time.Duration, position int, queued bool) {
	if w == nil {
		return
	}
	if position > 0 {
		w.Header().Set("X-Backend-Queue-Position", strconv.Itoa(position))
	}
	w.Header().Set("X-Backend-Queue-Wait-Ms", strconv.FormatInt(waited.Milliseconds(), 10))
	w.Header().Set("Retry-After", strconv.Itoa(backendWaitRetryAfter))
	code := "no_backend"
	reason := "no llama.cpp backend available: all backends are offline, disabled or serving a layout role"
	if queued {
		reason = "no llama.cpp backend available within wait: all backends are offline, disabled or serving a layout role"
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		// error — прежний текст (контракт клиентов/тестов не ломаем),
		// code — машиночитаемая причина, числа и позиция — для диагностики.
		"error":     "no llama.cpp backend available",
		"code":      code,
		"reason":    reason,
		"model":     model,
		"waited_ms": waited.Milliseconds(),
		"position":  position,
	})
}

// logBackendWaitR403 — одна строка в лог, когда ожидание закончилось успехом.
// Нужна, чтобы оператор видел: запрос не «висел», а ждал появления бэкенда.
func logBackendWaitR403(model string, waited time.Duration, position int) {
	if logger.Get() == nil {
		return
	}
	logger.Get().Infow("R403: запрос дождался свободного бэкенда",
		"model", model,
		"waited_ms", waited.Milliseconds(),
		"queue_position", position,
		"why", "все бэкенды были offline/disabled/донорами — запрос ждал в очереди, а не получал 503 сразу")
}
