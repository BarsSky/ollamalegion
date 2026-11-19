// image_resources_locks.go — R633 (2026-11-19): замки GPU по хостам: acquireLock, конфликты, освобождение, счётчики ожидания.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_resources.go` держал 2001 строку: типы и разбор
// метрик воркера, жизненный цикл опроса, гейт перед генерацией, оценка VRAM
// и замки GPU по хостам. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"time"
)

// acquireLock — взять лок GPU (host + индекс, когда индекс известен), ожидая
// освобождения не дольше queueWait. Возвращает (holder, ok, waited).
func (r *imageResources) acquireLock(ctx context.Context, backendID, host string, gpu gpuRef, model string) (*imageLockHolder, bool, time.Duration) {
	if r == nil {
		return nil, true, 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := imageLockKey(host, gpu)
	wait := r.queueWait()
	start := time.Now()
	deadline := start.Add(wait)

	for {
		r.mu.Lock()
		lk := r.locks[key]
		if lk == nil {
			lk = &imageHostLock{host: host, gpu: gpu}
			r.locks[key] = lk
		}
		// Конфликт ищем по ВСЕМ локам хоста, а не только по своему ключу:
		// хостовый лок (индекс неизвестен) и лок другой карты — разные ключи,
		// но первый обязан блокировать всех (см. locksConflict).
		conflict := r.conflictLocked(lk)
		if conflict == nil {
			h := &imageLockHolder{
				res:        r,
				done:       make(chan struct{}),
				key:        key,
				host:       host,
				gpu:        gpu,
				backendID:  backendID,
				model:      model,
				acquiredAt: time.Now(),
			}
			lk.held = true
			lk.holder = h
			lk.releaseCh = make(chan struct{})
			r.mu.Unlock()

			fuse := r.lockFuse()
			if fuse > 0 {
				h.fuse = time.AfterFunc(fuse, func() { r.forceRelease(h) })
			}
			r.lockAcquired.Add(1)
			waited := time.Since(start)
			if waited > 0 {
				r.lockWaits.Add(1)
				r.lockWaitSumMS.Add(waited.Milliseconds())
			}
			logger.Get().Infow("image GPU lock acquired",
				"host", host,
				"gpu_index", gpu.index,
				"gpu_known", gpu.known,
				"lock_key", key,
				"backend", backendID,
				"model", model,
				"waited_ms", waited.Milliseconds(),
				"fuse_sec", int(fuse.Seconds()))
			return h, true, waited
		}
		holder := conflict.holder
		releaseCh := conflict.releaseCh
		r.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			r.lockRejected.Add(1)
			return nil, false, time.Since(start)
		}
		if holder != nil {
			logger.Get().Debugw("image GPU lock busy, waiting",
				"host", host,
				"gpu_index", gpu.index,
				"gpu_known", gpu.known,
				"held_by_backend", holder.backendID,
				"held_gpu_index", holder.gpu.index,
				"held_gpu_known", holder.gpu.known,
				"held_ms", time.Since(holder.acquiredAt).Milliseconds(),
				"remaining_ms", remaining.Milliseconds())
		}

		nap := remaining
		if nap > imageLockWaitPoll {
			nap = imageLockWaitPoll
		}
		timer := time.NewTimer(nap)
		select {
		case <-releaseCh:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			r.lockRejected.Add(1)
			return nil, false, time.Since(start)
		}
		timer.Stop()
	}
}

// conflictLocked — удерживаемый лок, конфликтующий с lk (mu удерживается).
// Выбор детерминирован (см. lockOrderLess).
func (r *imageResources) conflictLocked(lk *imageHostLock) *imageHostLock {
	if lk == nil {
		return nil
	}
	// СВОЙ ключ занят — это и есть конфликт (другой запрос держит ту же карту).
	// Отдельная ветка обязательна: в общем цикле свой лок пропускается как
	// «сам себя», и без неё повторный захват того же (host, gpu) проходил бы
	// молча — воспроизведено тестом TestImageLock_ExclusiveSecondRequestRejected.
	if lk.held {
		return lk
	}
	var best *imageHostLock
	for _, other := range r.locks {
		if other == lk || !other.held {
			continue
		}
		if !locksConflict(lk, other) {
			continue
		}
		if best == nil || lockOrderLess(other, best) {
			best = other
		}
	}
	return best
}

// unlock — снять лок (вызывается из imageLockHolder.Release под sync.Once).
func (r *imageResources) unlock(h *imageLockHolder) {
	if r == nil || h == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lk := r.locks[h.key]
	if lk == nil || !lk.held || lk.holder != h {
		return
	}
	lk.held = false
	lk.holder = nil
	if lk.releaseCh != nil {
		close(lk.releaseCh)
		lk.releaseCh = nil
	}
}

// forceRelease — предохранитель: лок держат дольше ExclusiveLockTimeoutSec.
//
// Зачем: зависшая генерация (или потерянный обработчик) иначе заблокировала бы
// текстовый трафик на хосте навсегда. Освобождаем принудительно и пишем WARN —
// это операторское событие, а не рутина.
func (r *imageResources) forceRelease(h *imageLockHolder) {
	if r == nil || h == nil {
		return
	}
	r.mu.Lock()
	lk := r.locks[h.key]
	stillHeld := lk != nil && lk.held && lk.holder == h
	r.mu.Unlock()
	if !stillHeld {
		return // уже освобождён штатно — предохранитель не нужен
	}
	r.lockForced.Add(1)
	logger.Get().Warnw(i18n.T("balancer.image_resources.lock_forced_release_warn"),
		"host", h.host,
		"gpu_index", h.gpu.index,
		"gpu_known", h.gpu.known,
		"backend", h.backendID,
		"model", h.model,
		"held_sec", int(time.Since(h.acquiredAt).Seconds()),
		"limit_sec", int(r.lockFuse().Seconds()))
	h.Release("fuse_timeout")
}

// releaseFromRequest — освобождение из HTTP-обработчика: no-op, если лок уже
// передан сторожу асинхронной генерации.
func (r *imageResources) releaseFromRequest(h *imageLockHolder) {
	if h == nil {
		return
	}
	if h.handedOff.Load() {
		logger.Get().Debugw("image GPU lock release skipped: ownership handed to async watcher",
			"host", h.host, "gpu_index", h.gpu.index, "gpu_known", h.gpu.known,
			"backend", h.backendID, "model", h.model)
		return
	}
	h.Release("request_done")
}

// gpuLockHeld — занят ли хост ЛЮБЫМ локом (без учёта индекса GPU): «есть ли на
// хосте активная генерация вообще». Используется диагностикой/тестами; боевой
// путь текстовой стороны — gpuLockHeldFor с индексом.
func (r *imageResources) gpuLockHeld(host string) bool {
	if r == nil || host == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, lk := range r.locks {
		if lk != nil && lk.held && lk.host == host {
			return true
		}
	}
	return false
}

// gpuLockHeldFor — блокирует ли удерживаемый лок эту (host, gpu) пару.
//
// Правило то же, что и у взятия лока (locksConflict): лок другой карты того же
// хоста не блокирует, а лок с неизвестным индексом (или проверка без индекса)
// блокирует весь хост — консервативно. gpu.known == false здесь означает «у
// проверяемой стороны индекс неизвестен»: тогда она считается занимающей весь
// хост, и любой удерживаемый лок на этом хосте её блокирует.
func (r *imageResources) gpuLockHeldFor(host string, gpu gpuRef) bool {
	if r == nil || host == "" {
		return false
	}
	probe := &imageHostLock{host: host, gpu: gpu}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, lk := range r.locks {
		if lk != nil && lk.held && locksConflict(probe, lk) {
			return true
		}
	}
	return false
}

// noteTextSlotWithheld — счётчик «текстовый кандидат пропущен из-за image-лока».
func (r *imageResources) noteTextSlotWithheld() {
	if r != nil {
		r.textSlotsWithheld.Add(1)
	}
}

// noteTextWaitCapped — счётчик «ожидание текстового запроса урезано до
// balancing.image.queueWaitTimeoutSec, потому что единственная причина
// отсутствия кандидата — активный image-лок» (см. imageLockWaitCap).
func (r *imageResources) noteTextWaitCapped() {
	if r != nil {
		r.textWaitCapped.Add(1)
	}
}
