// Package balancer — R73 (2026-09-24): единая очередь.
//
// ДО R73 в балансере жили ДВЕ независимые очереди:
//
//  1. admission-очередь (`admission_queue.go`, R67b/R70) — ждёт свободный слот
//     на конкретном бэкенде; работает на llama.cpp-пути и несёт per-session
//     справедливость, `X-Queue-Position`/`X-Queue-Wait-Ms`, keepalive для
//     streaming-клиентов, статистику в `/api/v1/queue/stats` (блок admission);
//  2. legacy `QueueManager` (`queue_manager.go`) — пул worker'ов с каналом и
//     собственной историей; работал на Ollama-пути (`Proxy.ServeHTTP`): если ни
//     на одном бэкенде нет свободного слота, запрос клался в канал, а worker
//     пытался позже его обслужить (`dispatchRequest`).
//
// Из-за этого пороги и поведение расходились: у двух очередей разные лимиты
// (`queueMaxSize`/`queueTimeout` против `LB_ADMISSION_WAIT_SEC`), разная
// справедливость (у legacy её нет вообще) и разная наблюдаемость.
//
// R73 сводит Ollama-путь в admission-очередь: `waitForInferenceBackend` ждёт
// слот на ЛЮБОМ подходящем бэкенде в той же очереди (ключ `any`), с тем же
// пределом ожидания и той же статистикой. Legacy-очередь остаётся только как
// хранилище статистики/истории (`/api/v1/queue/*`), её worker'ы больше не
// участвуют в обслуживании: счётчики и история пополняются из
// `QueueManager.RecordUnified`.
package balancer

import (
	"context"
	"errors"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// admissionAnyBackend — ключ ожидания «слот на любом подходящем бэкенде».
// Отдельный ключ очереди: его ожидающие не конкурируют с ожидающими конкретного
// бэкенда (llama.cpp-путь), но используют те же приоритеты по сессиям.
const admissionAnyBackend = "any"

// errAdmissionOverloaded — legacy-backpressure: ожидающих больше, чем
// queueMaxSize × backpressure.highWatermark. Отвечаем быстрым 503 (как раньше
// делал queueRequest), чтобы не копить бесконечную очередь.
var errAdmissionOverloaded = errors.New("admission queue overloaded")

// tryAcquireAnyBackendSlot — попытка захватить слот на любом подходящем
// бэкенде (тот же выбор, что в Proxy.ServeHTTP: affinity/Load/Config).
// Возвращает ID бэкенда, на котором слот захвачен, либо "".
func (p *Proxy) tryAcquireAnyBackendSlot(model string, bt types.BackendType) string {
	if p == nil {
		return ""
	}
	attempted := make(map[string]bool, 4)
	if id := p.selectBackend(model, bt, false); id != "" {
		if p.tryAcquireSlot(id) {
			return id
		}
		attempted[id] = true
	}
	const maxRetries = 10
	for retry := 0; retry < maxRetries; retry++ {
		id := p.selectBackendExcluding(model, attempted, bt)
		if id == "" {
			return ""
		}
		if p.tryAcquireSlot(id) {
			return id
		}
		attempted[id] = true
	}
	return ""
}

// waitForInferenceBackend — R73: дождаться слота на любом подходящем бэкенде.
//
// Возвращает ID бэкенда с захваченным слотом и функцию release (обязательна к
// вызову) либо ошибку:
//
//	nil                    — слот получен (waited > 0, если пришлось ждать);
//	errAdmissionDisabled   — ожидание выключено (LB_ADMISSION_WAIT_SEC=0): 503;
//	errAdmissionOverloaded — очередь переполнена (backpressure): быстрый 503;
//	errAdmissionTimeout    — ожидание истекло: 503 + Retry-After;
//	ctx.Err()              — клиент отвалился/отменил запрос.
func (p *Proxy) waitForInferenceBackend(
	ctx context.Context, model string, bt types.BackendType, session string,
) (backendID string, release func(), waited time.Duration, position int, err error) {
	if p == nil {
		return "", nil, 0, 0, errAdmissionDisabled
	}

	// Быстрый путь: слот свободен — очередь не нужна.
	if id := p.tryAcquireAnyBackendSlot(model, bt); id != "" {
		lease := p.newAdmissionLease(id, session, 0, 0)
		return id, lease.Release, 0, 0, nil
	}

	wait := p.admissionWaitTimeout()
	if wait <= 0 || p.admission == nil {
		return "", nil, 0, 0, errAdmissionDisabled
	}
	if p.admissionOverloaded() {
		logger.Get().Warnw("unified queue: очередь переполнена, быстрый 503",
			"model", model, "waiting", p.admission.waitingCount(),
			"max_size", p.queueMaxSize())
		return "", nil, 0, 0, errAdmissionOverloaded
	}

	aq := p.admission
	w := aq.enqueue(admissionAnyBackend, session)
	position = aq.position(admissionAnyBackend, w)
	enqueued := w.enqueued
	deadline := enqueued.Add(wait)
	logger.Get().Infow("unified queue: запрос встал в admission-очередь (любой бэкенд)",
		"model", model, "session", session, "position", position,
		"wait_max_sec", int(wait.Seconds()))
	defer aq.remove(admissionAnyBackend, w)

	for {
		// Канал берём ДО попытки захвата: иначе пробуждение между попыткой и
		// select потерялось бы и запрос ждал лишний poll-интервал.
		notify := aq.waitChannel()
		if aq.canProceed(admissionAnyBackend, w) {
			if id := p.tryAcquireAnyBackendSlot(model, bt); id != "" {
				waited = time.Since(enqueued)
				aq.recordServed(waited)
				lease := p.newAdmissionLease(id, session, waited, position)
				if p.queueMgr != nil {
					p.queueMgr.RecordUnified(model, id, enqueued)
				}
				logger.Get().Infow("unified queue: слот получен после ожидания",
					"backend", id, "model", model, "session", session,
					"waited_ms", waited.Milliseconds(), "position", position)
				return id, lease.Release, waited, position, nil
			}
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			aq.recordTimeout()
			logger.Get().Warnw("unified queue: ожидание слота истекло, 503",
				"model", model, "session", session,
				"waited_ms", time.Since(enqueued).Milliseconds(),
				"wait_max_sec", int(wait.Seconds()))
			return "", nil, time.Since(enqueued), position, errAdmissionTimeout
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
			logger.Get().Infow("unified queue: клиент отменил запрос, пока ждал слот",
				"model", model, "session", session,
				"waited_ms", time.Since(enqueued).Milliseconds())
			return "", nil, time.Since(enqueued), position, ctx.Err()
		}
		timer.Stop()
	}
}

// queueMaxSize — legacy-лимит размера очереди (queueMaxSize из конфига).
func (p *Proxy) queueMaxSize() int {
	if p == nil || p.queueMgr == nil {
		return 0
	}
	return p.queueMgr.maxSize
}

// admissionOverloaded — backpressure: ожидающих в admission-очереди больше,
// чем 90% от queueMaxSize из конфига.
//
// Раньше этот порог применялся к legacy-каналу QueueManager
// (`queueRequest`): при заполнении >90% запрос получал 503 сразу. Теперь он
// применяется к единой очереди — так сохраняется защита от бесконечного
// накопления ожидающих.
func (p *Proxy) admissionOverloaded() bool {
	if p == nil || p.admission == nil {
		return false
	}
	maxSize := p.queueMaxSize()
	if maxSize <= 0 {
		return false
	}
	const highWatermark = 0.90 // как в legacy queueRequest
	return float64(p.admission.waitingCount()) >= float64(maxSize)*highWatermark
}
