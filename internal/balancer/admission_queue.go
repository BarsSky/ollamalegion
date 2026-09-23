// admission_queue.go — R67b (2026-09-23): очередь допуска (admission) для
// inference-запросов llama.cpp вместо немедленного 503.
//
// ЖАЛОБА (R67, дословно): «проблема на стороне балансера, он резко отбивает
// повторный запрос от клиента выдавая 503, а не поставляя в очередь, скорей
// всего причина в том что не различает разных пользователей клиента OpenWebUI,
// что может предоставлять модель сразу нескольким пользователям одновременно»
// и «даже если от одного пользователя идет новый запрос, балансер его не ставит
// в очередь на отработку и не делает как новую сессию».
//
// ДИАГНОЗ (подтверждён чтением кода + живым стендом). Inference-хендлеры
// llama.cpp (handleChat / handleGenerate / handleOpenAIChatCompletions) выбирают
// бэкенд и сразу проксируют запрос, НЕ беря слот: `tryAcquireSlot`
// (slot_manager.go) вызывается только из общего flow Proxy.ServeHTTP и из
// QueueManager. Поэтому:
//   - `maxConcurrentReqs` бэкенда (в живом стеке 4) на llama.cpp-путь не влиял
//     вообще: `activeRequests` в /api/v1/backends оставался 0 при трёх
//     параллельных генерациях, а WebUI показывал нулевую загрузку бэкенда;
//   - всё, что не помещалось в параллелизм cppworker'а, ждало ВНУТРИ cppworker
//     (его SlotManager), а балансер при этом не имел ни очереди, ни видимости
//     ожидающих; при таймауте ожидания клиент получал 503 «model is still
//     loading» / «all backends busy» без позиции в очереди.
//
// РЕШЕНИЕ: admission-очередь на стороне балансера.
//  1. Быстрый путь: слот свободен → берём его (`tryAcquireSlot`), как раньше.
//  2. Слот занят → запрос ВСТАЁТ В ОЧЕРЕДЬ и ждёт до LB_ADMISSION_WAIT_SEC
//     (default 300 c; 0 = прежнее поведение «сразу 503»), после чего получает
//     слот и обслуживается ПЕРВЫМ же запросом (без «повторите запрос»).
//  3. Справедливость по сессиям (per-user): пока у сессии есть активный запрос,
//     её следующие ожидающие запросы УСТУПАЮТ сессиям, которые ещё не
//     обслуживались. Один пользователь OpenWebUI не может занять все слоты
//     своими повторными запросами.
//  4. Каждый НОВЫЙ запрос пользователя — новая сессия/новая позиция в очереди
//     (см. requestSessionKey: X-User-Id / X-OpenWebUI-User-Id / chat_id из тела
//     и metadata), а не «слипание» всех пользователей в одну сессию.
//  5. 503 остаётся последним рубежом: только если ожидание истекло. Клиент
//     получает Retry-After + X-Queue-Position + X-Queue-Wait-Ms.
package balancer

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

const (
	// admissionWaitDefaultSec — сколько ждать слот перед 503 (10 минут).
	// Достаточно, чтобы пережить длинную генерацию другого пользователя, и
	// меньше типичного таймаута OpenWebUI/Cline на «висит соединение».
	admissionWaitDefaultSec = 300

	// admissionPollInterval — период перепроверки слота ожидающим. Нужен как
	// страховка от потерянного пробуждения (broadcast) — основной сигнал всё же
	// мгновенный.
	admissionPollInterval = 250 * time.Millisecond

	// admissionRetryAfterSec — Retry-After для 503 при исчерпанном ожидании.
	admissionRetryAfterSec = 15

	// admissionFairnessWindow — окно «недавно обслуживался»: сессия, которая
	// получила слот в этом окне, уступает сессиям, которые ещё не обслуживались
	// (защита от «один пользователь забил очередь своими запросами»). Окно
	// конечно: через него пользователь снова считается «новым», иначе один
	// ранний запрос навсегда понижал бы его приоритет.
	admissionFairnessWindow = 30 * time.Second
)

// AdmissionWaitTimeout — R67b: сколько балансер ждёт свободный слот, прежде чем
// ответить 503.
//
//	LB_ADMISSION_WAIT_SEC=N — N секунд; 0 = не ждать (прежнее поведение);
//	отрицательное = ждать до отмены клиентом (ограничено разумным максимумом).
//	По умолчанию 300s.
func AdmissionWaitTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_ADMISSION_WAIT_SEC"))
	if v == "" {
		return admissionWaitDefaultSec * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return admissionWaitDefaultSec * time.Second
	}
	switch {
	case n == 0:
		return 0
	case n < 0:
		return 30 * time.Minute
	default:
		return time.Duration(n) * time.Second
	}
}

var (
	// errAdmissionDisabled — ожидание выключено (LB_ADMISSION_WAIT_SEC=0) и
	// свободных слотов нет: вызывающий код отвечает прежним быстрым 503.
	errAdmissionDisabled = errors.New("admission queue disabled")
	// errAdmissionTimeout — ожидание истекло: 503 + Retry-After.
	errAdmissionTimeout = errors.New("no free slot within admission wait")
)

// admissionWaiter — ожидающий слот запрос.
type admissionWaiter struct {
	session  string
	enqueued time.Time
	seq      uint64
}

// queueAdmissionStats — R67b: срез admission-очереди для /api/v1/queue/stats.
//
// Порядок полей — от «тяжёлых» к лёгким: golangci-lint включает govet
// fieldalignment, поэтому []string/map идут первыми.
type queueAdmissionStats struct {
	Sessions     []string       `json:"sessions,omitempty"`
	ByBackend    map[string]int `json:"waiting_by_backend,omitempty"`
	Served       int64          `json:"served_total"`
	Timeouts     int64          `json:"timeout_total"`
	WaitedTotal  int64          `json:"waited_total"`
	AvgWaitMs    int64          `json:"avg_wait_ms"`
	WaitSec      int            `json:"wait_max_sec"`
	Waiting      int            `json:"waiting"`
	ActiveByUser int            `json:"active_sessions"`
	Enabled      bool           `json:"enabled"`
}

// admissionQueue — очередь ожидающих слотов по бэкендам.
//
// Модель данных намеренно простая: срез ожидающих на бэкенд + счётчик активных
// запросов по сессии. Право «попробовать взять слот» выдаётся ровно одному
// ожидающему (самому приоритетному), остальные спят на broadcast-канале.
type admissionQueue struct {
	mu         sync.Mutex
	waiters    map[string][]*admissionWaiter
	inflight   map[string]int
	lastServed map[string]time.Time
	seq        uint64
	notify     chan struct{}
	// стата
	waitedTotal  int64
	timeoutTotal int64
	waitedSumMs  int64
	servedTotal  int64
}

func newAdmissionQueue() *admissionQueue {
	return &admissionQueue{
		waiters:    make(map[string][]*admissionWaiter),
		inflight:   make(map[string]int),
		lastServed: make(map[string]time.Time),
		notify:     make(chan struct{}),
	}
}

// broadcast — разбудить всех ожидающих (закрываем текущий канал и создаём новый).
func (aq *admissionQueue) broadcast() {
	if aq == nil {
		return
	}
	aq.mu.Lock()
	close(aq.notify)
	aq.notify = make(chan struct{})
	aq.mu.Unlock()
}

// waitChannel — канал текущего «поколения» пробуждений.
func (aq *admissionQueue) waitChannel() <-chan struct{} {
	aq.mu.Lock()
	defer aq.mu.Unlock()
	return aq.notify
}

// enqueue — поставить ожидающего в очередь бэкенда.
func (aq *admissionQueue) enqueue(backendID, session string) *admissionWaiter {
	aq.mu.Lock()
	defer aq.mu.Unlock()
	aq.pruneLocked()
	aq.seq++
	w := &admissionWaiter{session: session, enqueued: time.Now(), seq: aq.seq}
	aq.waiters[backendID] = append(aq.waiters[backendID], w)
	return w
}

// remove — убрать ожидающего из очереди (по таймауту/отмене/выдаче слота).
func (aq *admissionQueue) remove(backendID string, w *admissionWaiter) {
	aq.mu.Lock()
	defer aq.mu.Unlock()
	aq.removeLocked(backendID, w)
}

func (aq *admissionQueue) removeLocked(backendID string, w *admissionWaiter) {
	list := aq.waiters[backendID]
	for i, cand := range list {
		if cand == w {
			aq.waiters[backendID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(aq.waiters[backendID]) == 0 {
		delete(aq.waiters, backendID)
	}
}

// position — позиция ожидающего в очереди бэкенда (1-based, для заголовков).
func (aq *admissionQueue) position(backendID string, w *admissionWaiter) int {
	aq.mu.Lock()
	defer aq.mu.Unlock()
	for i, cand := range aq.waiters[backendID] {
		if cand == w {
			return i + 1
		}
	}
	return 0
}

// markInflight — изменить счётчик активных запросов сессии.
func (aq *admissionQueue) markInflight(session string, delta int) {
	if aq == nil || session == "" {
		return
	}
	aq.mu.Lock()
	defer aq.mu.Unlock()
	next := aq.inflight[session] + delta
	if next <= 0 {
		delete(aq.inflight, session)
		return
	}
	aq.inflight[session] = next
}

// canProceed — имеет ли ПРАВО этот ожидающий попытаться взять слот.
//
// Приоритет: (1) сессии, которые не обслуживались (нет активного запроса и не
// получали слот в admissionFairnessWindow), (2) сессии без активного запроса, но
// недавно обслуженные, (3) повторные запросы активной сессии. Внутри группы —
// FIFO. Так повторные запросы одного пользователя уступают новым пользователям.
func (aq *admissionQueue) canProceed(backendID string, w *admissionWaiter) bool {
	aq.mu.Lock()
	defer aq.mu.Unlock()
	best := aq.bestLocked(backendID)
	return best == w
}

// priorityLocked — группа приоритета ожидающего (меньше = раньше).
func (aq *admissionQueue) priorityLocked(w *admissionWaiter) int {
	if aq.inflight[w.session] > 0 {
		return 2 // сессия уже обслуживается — её повторный запрос уступает
	}
	if served, ok := aq.lastServed[w.session]; ok && time.Since(served) < admissionFairnessWindow {
		return 1 // недавно обслуживалась — уступает тем, кто ещё не обслуживался
	}
	return 0
}

// bestLocked — самый приоритетный ожидающий (mu удерживается).
func (aq *admissionQueue) bestLocked(backendID string) *admissionWaiter {
	list := aq.waiters[backendID]
	if len(list) == 0 {
		return nil
	}
	var best *admissionWaiter
	bestGroup := 0
	for _, cand := range list {
		group := aq.priorityLocked(cand)
		switch {
		case best == nil, group < bestGroup:
			best, bestGroup = cand, group
		case group == bestGroup && cand.seq < best.seq:
			best = cand
		}
	}
	return best
}

// markServed — сессия получила слот (обновляет приоритет очереди).
func (aq *admissionQueue) markServed(session string) {
	if aq == nil || session == "" {
		return
	}
	aq.mu.Lock()
	aq.lastServed[session] = time.Now()
	aq.mu.Unlock()
}

// pruneLocked — забыть сессии, не обслуживавшиеся дольше окна справедливости
// (иначе map растёт бесконечно на долгом uptime).
func (aq *admissionQueue) pruneLocked() {
	for session, at := range aq.lastServed {
		if time.Since(at) > admissionFairnessWindow {
			delete(aq.lastServed, session)
		}
	}
}

// stats — снимок состояния очереди для /api/v1/queue/stats.
func (aq *admissionQueue) stats(wait time.Duration) queueAdmissionStats {
	if aq == nil {
		return queueAdmissionStats{}
	}
	aq.mu.Lock()
	defer aq.mu.Unlock()
	out := queueAdmissionStats{
		Enabled:      wait > 0,
		WaitSec:      int(wait.Seconds()),
		ByBackend:    make(map[string]int, len(aq.waiters)),
		Served:       aq.servedTotal,
		Timeouts:     aq.timeoutTotal,
		WaitedTotal:  aq.waitedTotal,
		AvgWaitMs:    0,
		ActiveByUser: len(aq.inflight),
	}
	// Waiting — число ожидающих ЗАПРОСОВ (не бэкендов): на одном бэкенде их
	// может быть несколько.
	for backendID, list := range aq.waiters {
		out.Waiting += len(list)
		out.ByBackend[backendID] = len(list)
		for _, w := range list {
			if w.session != "" {
				out.Sessions = append(out.Sessions, w.session)
			}
		}
	}
	if aq.waitedTotal > 0 {
		out.AvgWaitMs = aq.waitedSumMs / aq.waitedTotal
	}
	return out
}

// recordServed — статистика успешного ожидания.
func (aq *admissionQueue) recordServed(waited time.Duration) {
	if aq == nil {
		return
	}
	aq.mu.Lock()
	aq.servedTotal++
	aq.waitedTotal++
	aq.waitedSumMs += waited.Milliseconds()
	aq.mu.Unlock()
}

// recordTimeout — статистика исчерпанного ожидания.
func (aq *admissionQueue) recordTimeout() {
	if aq == nil {
		return
	}
	aq.mu.Lock()
	aq.timeoutTotal++
	aq.mu.Unlock()
}

// admissionLease — выданный слот (то же, что слот tryAcquireSlot).
type admissionLease struct {
	BackendID string
	Session   string
	Waited    time.Duration
	Position  int
	release   func()
}

// Release — освободить слот. Идемпотентно (sync.Once внутри release).
func (l *admissionLease) Release() {
	if l == nil || l.release == nil {
		return
	}
	l.release()
}

// admissionEnabled — включено ли ожидание (для метрик/WebUI).
func (p *Proxy) admissionEnabled() bool {
	return p != nil && p.admission != nil && p.admissionWait > 0
}

// acquireInferenceSlot — R67b: получить слот бэкенда, при необходимости встав в
// admission-очередь. Возвращает lease (release обязателен), позицию в очереди на
// момент постановки и ошибку:
//
//	nil                   — слот получен (Waited>0, если пришлось ждать);
//	errAdmissionDisabled  — ожидание выключено и слотов нет (legacy 503);
//	errAdmissionTimeout   — ожидание истекло (503 + Retry-After);
//	ctx.Err()             — клиент отвалился/отменил запрос.
func (p *Proxy) acquireInferenceSlot(
	ctx context.Context, backendID, session string,
) (*admissionLease, int, error) {
	if p == nil || backendID == "" {
		return nil, 0, errAdmissionDisabled
	}
	wait := p.admissionWait

	if p.tryAcquireSlot(backendID) {
		return p.newAdmissionLease(backendID, session, 0, 0), 0, nil
	}
	if wait <= 0 || p.admission == nil {
		return nil, 0, errAdmissionDisabled
	}

	aq := p.admission
	w := aq.enqueue(backendID, session)
	position := aq.position(backendID, w)
	deadline := time.Now().Add(wait)
	logger.Get().Infow("admission: request queued, waiting for free slot",
		"backend", backendID, "session", session, "position", position,
		"wait_max_sec", int(wait.Seconds()))
	defer aq.remove(backendID, w)

	for {
		// Канал берём ДО попытки захвата: иначе пробуждение между попыткой и
		// select потерялось бы и запрос ждал лишний poll-интервал.
		notify := aq.waitChannel()
		if aq.canProceed(backendID, w) && p.tryAcquireSlot(backendID) {
			waited := time.Since(w.enqueued)
			aq.recordServed(waited)
			logger.Get().Infow("admission: slot acquired after waiting",
				"backend", backendID, "session", session,
				"waited_ms", waited.Milliseconds(), "position", position)
			return p.newAdmissionLease(backendID, session, waited, position), position, nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			aq.recordTimeout()
			logger.Get().Warnw("admission: wait timeout, returning 503",
				"backend", backendID, "session", session,
				"waited_ms", time.Since(w.enqueued).Milliseconds(),
				"wait_max_sec", int(wait.Seconds()))
			return nil, position, errAdmissionTimeout
		}
		nap := admissionPollInterval
		if remaining < nap {
			nap = remaining
		}
		timer := time.NewTimer(nap)
		select {
		case <-notify:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			logger.Get().Infow("admission: client canceled while waiting for slot",
				"backend", backendID, "session", session,
				"waited_ms", time.Since(w.enqueued).Milliseconds())
			return nil, position, ctx.Err()
		}
		timer.Stop()
	}
}

// newAdmissionLease — lease вокруг tryAcquireSlot/releaseSlot с учётом
// per-session inflight (нужен для справедливости очереди).
func (p *Proxy) newAdmissionLease(backendID, session string, waited time.Duration, position int) *admissionLease {
	aq := p.admission
	aq.markInflight(session, 1)
	aq.markServed(session)
	var once sync.Once
	return &admissionLease{
		BackendID: backendID,
		Session:   session,
		Waited:    waited,
		Position:  position,
		release: func() {
			once.Do(func() {
				// inflight уменьшаем ДО broadcast (внутри releaseSlot), иначе
				// ожидающие той же сессии увидят её «занятой» ещё один круг.
				aq.markInflight(session, -1)
				p.releaseSlot(backendID)
			})
		},
	}
}

// acquireInferenceAdmission — R67b: точка входа для inference-хендлеров
// llama.cpp. Берёт слот бэкенда через admission-очередь (с ожиданием) и
// возвращает release-функцию.
//
// Возвращает (release, ok):
//   - (lease.Release, true)  — слот получен, обработку можно продолжать;
//   - (nil, true)            — ожидание выключено (LB_ADMISSION_WAIT_SEC=0):
//     работаем как до R67b, без учёта слотов на этом пути;
//   - (nil, false)           — клиенту уже отправлен ответ (503 при таймауте или
//     ничего при отмене): хендлер обязан сделать return.
//
// Заголовки X-Queue-Wait-Ms / X-Queue-Position выставляются ДО того, как
// транспорт пишет свои заголовки, поэтому клиент видит факт ожидания в очереди.
func (lr *LlamaCppRouter) acquireInferenceAdmission(
	w http.ResponseWriter, r *http.Request, backendID, model string, body []byte,
) (func(), bool) {
	if lr == nil || lr.proxy == nil {
		return nil, true
	}
	session := RequestSessionKey(r, body)
	lease, position, err := lr.proxy.acquireInferenceSlot(r.Context(), backendID, session)
	switch {
	case err == nil:
		if lease.Waited > 0 {
			w.Header().Set("X-Queue-Wait-Ms", strconv.FormatInt(lease.Waited.Milliseconds(), 10))
			w.Header().Set("X-Queue-Position", strconv.Itoa(position))
		}
		return lease.Release, true
	case errors.Is(err, errAdmissionDisabled):
		// Ожидание выключено — сохраняем поведение до R67b (без гейта).
		return nil, true
	case errors.Is(err, errAdmissionTimeout):
		writeAdmissionUnavailable(w, model, session, position,
			lr.proxy.admissionWait, lr.proxy.admissionWait)
		return nil, false
	default:
		// Клиент отменил запрос, пока ждал слот: отвечать уже некому.
		logger.Get().Infow("admission: request canceled before slot was granted",
			"backend", backendID, "model", model, "session", session)
		return nil, false
	}
}

// writeAdmissionUnavailable — R67b: 503 «нет свободных слотов» с понятными
// заголовками (клиент видит, что запрос стоял в очереди, и когда повторить).
func writeAdmissionUnavailable(w http.ResponseWriter, model, session string, position int, waited, maxWait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(admissionRetryAfterSec))
	if position > 0 {
		w.Header().Set("X-Queue-Position", strconv.Itoa(position))
	}
	w.Header().Set("X-Queue-Wait-Ms", strconv.FormatInt(waited.Milliseconds(), 10))
	w.Header().Set("X-Queue-Wait-Max-Sec", strconv.Itoa(int(maxWait.Seconds())))
	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		"error": "all inference slots are busy, request waited in queue and timed out: " +
			"model '" + model + "'",
		"queued":        true,
		"queuePosition": position,
		"waitedMs":      waited.Milliseconds(),
		"waitMaxSec":    int(maxWait.Seconds()),
		"retryAfterMs":  admissionRetryAfterSec * 1000,
		"session":       session,
	})
}
