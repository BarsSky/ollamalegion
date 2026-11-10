// unified_queue_r459_gate_r480.go — R480 (P1-7): единая очередь не выдаёт
// «холодный» (или узкий) бэкенд живой сессии мимо гейта R459.
//
// ЗАЧЕМ. Очередь (waitForInferenceBackend) ищет «любой свободный слот». Если
// сессия привязана к прогретой копии с окном 65536, а её слот занят, очередь
// могла выдать слот на другой копии — с меньшим окном или вовсе без модели.
// Снаружи это и есть «сессия внезапно уехала и ход сломался / клиент ждал
// загрузку»: ребалансировка R459 такой переезд запрещает, а очередь его делала
// молча, потому что политика жила только в селекторе с предикатом.
//
// ПРАВИЛО теперь одно на все пути: если у сессии есть привязанный бэкенд, то и
// очередь выбирает только кандидатов, сохраняющих «нужное окно» (need по
// варианту C). Нет кандидата — запрос ЖДЁТ слот своей копии (правило R139), как
// и при обычной занятости; таймаут остаётся тем же (503 + Retry-After).
//
// Новая сессия (нет привязки) ходит по-прежнему: ей нужен любой свободный слот.
package balancer

import (
	"context"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// tryAcquireAnyBackendSlotDetailedGatedR480 — отбор слота с предикатом R459.
//
// gate == nil — ровно прежнее поведение (tryAcquireAnyBackendSlotDetailed).
func (p *Proxy) tryAcquireAnyBackendSlotDetailedGatedR480(
	model string, bt types.BackendType, gate func(string) bool,
) (backendID string, noCandidate bool) {
	if gate == nil {
		return p.tryAcquireAnyBackendSlotDetailed(model, bt)
	}
	if p == nil {
		return "", true
	}
	attempted := make(map[string]bool, 4)
	// Первый кандидат тоже проходит предикат: иначе очередь схватила бы
	// «самый свободный» бэкенд и только потом обнаружила, что окно не то.
	if id := p.selectBackend(model, bt, false); id != "" {
		if gate(id) {
			if p.tryAcquireSlot(id) {
				return id, false
			}
			attempted[id] = true
		} else {
			attempted[id] = true
		}
	} else {
		noCandidate = true
	}
	const maxRetries = 10
	for retry := 0; retry < maxRetries; retry++ {
		id := p.selectBackendExcluding(model, attempted, bt)
		if id == "" {
			return "", noCandidate
		}
		if !gate(id) {
			attempted[id] = true
			continue
		}
		if p.tryAcquireSlot(id) {
			return id, false
		}
		attempted[id] = true
	}
	return "", noCandidate
}

// waitForInferenceBackendGatedR480 — waitForInferenceBackend с предикатом R459.
// gate == nil — прежнее поведение.
func (p *Proxy) waitForInferenceBackendGatedR480(
	ctx context.Context, model string, bt types.BackendType, session string, gate func(string) bool,
) (backendID string, release func(), waited time.Duration, position int, err error) {
	return p.waitForInferenceBackendR480(ctx, model, bt, session, &queueMoveR480{gate: gate})
}

// queueMoveR480 — предикат переезда и его need для списка причин R460.
// need/needSource идут в «почему в очереди» (P1-8): без них оператор видит
// «model_not_loaded» там, где настоящая причина — «окно кандидата меньше».
type queueMoveR480 struct {
	// Порядок полей — под fieldalignment (линт): строка и функция (указатели)
	// раньше числа.
	needSource string
	gate       func(string) bool
	need       int
}

// sessionQueueGateR480 — предикат очереди для сессии, привязанной к from.
//
// Возвращает (nil, 0), если окно текущей копии неизвестно: сравнивать не с чем,
// гейт не ставится (поведение прежнее), иначе можно заблокировать очередь на
// пустом месте.
func (p *Proxy) sessionQueueGateR480(from, model string) (func(string) bool, int) {
	if p == nil || from == "" {
		return nil, 0
	}
	fromWindow := p.loadedPerSeqNCtx(from, model)
	if fromWindow <= 0 {
		return nil, 0
	}
	return func(c string) bool {
		if c == from {
			return true
		}
		perSeq, known := p.loadedPerSeqNCtxKnownR480(c, model)
		if !known {
			// «Неизвестно» — не «нет модели»: допускаем, но оставляем след.
			p.logColdCandidateUnknownR480(c, from, model, fromWindow)
			return true
		}
		if perSeq <= 0 {
			return false
		}
		return perSeq >= fromWindow
	}, fromWindow
}

// waitForInferenceBackendWithMoveR480 — P1-7/P1-8: очередь с гейтом R459,
// построенным по сессии.
//
// ВАЖНО (найдено -race/таймаутом в CI на первом варианте): порог гейта здесь —
// ОКНО ТЕКУЩЕЙ КОПИИ СЕССИИ, а не «потребность запроса». Первый вариант
// сравнивал кандидата с потребностью запроса, и если она больше ВСЕХ окон в
// кластере, гейт отклонял всех — запрос висел в очереди до таймаута (900 с)
// вместо того, чтобы уйти на свою копию и дать preflight поднять окно.
//
// Правило очереди — ровно R459: «не хуже, чем у меня сейчас».
//   - кандидат с окном >= окна текущей копии — можно;
//   - известный «холодный» (свежая метрика, модели нет) — нельзя;
//   - окно неизвестно — можно (не выдумываем предел), с WARN.
func (p *Proxy) waitForInferenceBackendWithMoveR480(
	ctx context.Context, model string, bt types.BackendType, session, from string,
) (backendID string, release func(), waited time.Duration, position int, err error) {
	var move *queueMoveR480
	if from != "" {
		gate, fromWindow := p.sessionQueueGateR480(from, model)
		if gate != nil {
			move = &queueMoveR480{
				gate:       gate,
				need:       fromWindow,
				needSource: "session_window",
			}
			logger.Get().Infow("R480: очередь сессии с гейтом R459",
				"session", session, "from", from, "model", model,
				"session_window", move.need)
		}
	}
	return p.waitForInferenceBackendR480(ctx, model, bt, session, move)
}

func (p *Proxy) waitForInferenceBackendR480(
	ctx context.Context, model string, bt types.BackendType, session string, move *queueMoveR480,
) (backendID string, release func(), waited time.Duration, position int, err error) {
	gate := func(string) bool { return true }
	if move != nil && move.gate != nil {
		gate = move.gate
	}
	if move == nil {
		return p.waitForInferenceBackend(ctx, model, bt, session)
	}
	if p == nil {
		return "", nil, 0, 0, errAdmissionDisabled
	}

	// Быстрый путь: слот свободен на подходящем (по R459) бэкенде.
	id, noCandidate := p.tryAcquireAnyBackendSlotDetailedGatedR480(model, bt, gate)
	if id != "" {
		lease := p.newAdmissionLease(id, session, 0, 0)
		return id, lease.Release, 0, 0, nil
	}
	wait := p.admissionWaitBudget()
	if wait <= 0 || p.admission == nil {
		return "", nil, 0, 0, errAdmissionDisabled
	}
	if noCandidate {
		if capped, ok := p.imageLockWaitCap(model, bt); ok && capped < wait {
			wait = capped
		}
	}
	if p.admissionOverloaded() {
		return "", nil, 0, 0, errAdmissionOverloaded
	}

	aq := p.admission
	w := aq.enqueue(admissionAnyBackend, session)
	position = aq.position(admissionAnyBackend, w)
	enqueued := w.enqueued
	deadline := enqueued.Add(wait)
	SetStageR477(ctx, stageAdmissionR477)
	// R480 (P1-8): причина «в очереди» с числами и с учётом гейта окна.
	// Без этого why_queued показывал model_not_loaded там, где решала именно
	// проверка «нужного окна».
	var rejectionMove *backendRejectionMoveR480
	if move.need > 0 {
		rejectionMove = &backendRejectionMoveR480{Need: move.need, NeedSource: move.needSource}
	}
	rejections := p.backendRejectionsR460(model, bt, rejectionMove)
	logger.Get().Infow("R480: очередь с гейтом R459 — почему не взяли бэкенды",
		"model", model, "session", session, "position", position,
		"need", needForLogR480(move), "wait_max_sec", int(wait.Seconds()),
		"rejected_backends", len(rejections),
		"why_queued", rejectionsSummaryR460(rejections))
	defer aq.remove(admissionAnyBackend, w)

	for {
		notify := aq.waitChannel()
		if aq.canProceed(admissionAnyBackend, w) {
			// Повторная попытка — тем же предикатом: пока ждали, мог
			// освободиться и «правильный» слот.
			if id := p.tryAcquireAnyBackendSlotGatedR480(model, bt, gate); id != "" {
				waited = time.Since(enqueued)
				aq.recordServed(waited)
				lease := p.newAdmissionLease(id, session, waited, position)
				if p.queueMgr != nil {
					p.queueMgr.RecordUnified(model, id, enqueued)
				}
				return id, lease.Release, waited, position, nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			aq.recordTimeout()
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
			return "", nil, time.Since(enqueued), position, ctx.Err()
		}
		timer.Stop()
	}
}

// tryAcquireAnyBackendSlotGatedR480 — одиночная попытка с предикатом.
func (p *Proxy) tryAcquireAnyBackendSlotGatedR480(model string, bt types.BackendType, gate func(string) bool) string {
	id, _ := p.tryAcquireAnyBackendSlotDetailedGatedR480(model, bt, gate)
	return id
}

// needForLogR480 — need для строки лога (0, если переезда нет).
func needForLogR480(move *queueMoveR480) int {
	if move == nil {
		return 0
	}
	return move.need
}
