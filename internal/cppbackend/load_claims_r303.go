// load_claims_r303.go — R303: «заявка на загрузку» модели с возрастом и владельцем.
//
// ЗАЧЕМ. В отчёте по двухузловому стенду (D3) зафиксировано состояние, из
// которого узел выходит только перезапуском контейнера:
//
//	POST /api/models/load  → висит 260 с без ответа и без строки «loading model»
//	GET  /api/info         → in_flight_requests {"gemma-4-E4B-it-Q4_K_M": 3}
//	POST /api/models/unload?name=… → 409 «model is currently loading»
//	(никакой загрузки при этом не идёт)
//
// Механизм: `b.loading[name]` — это канал-«заявка» (TryLockLoad/TryLockReload →
// UnlockLoad). Пока канал лежит в мапе, IsLoading()=true, unload отвечает 409, а
// второй load-запрос уходит в `WaitForLoad`, который ждёт канал БЕЗ ТАЙМАУТА.
// Если владелец заявки так и не вызвал UnlockLoad (C-мост завис на узле
// раскладки, фоновая горутина умерла вместе с соединением, ранний return), то:
//
//  1. заявка остаётся в мапе навсегда (до рестарта процесса);
//  2. все последующие load/reload этой модели висят бесконечно;
//  3. unload честно врёт «model is currently loading».
//
// ЧТО ЗДЕСЬ. Заявка получает метаданные: когда взята и КЕМ (requester:
// "load", "reload", "lazyload", "inference"). По ним:
//
//   - ожидание стало ограниченным (WaitForLoadTimeoutR303) — «повиснуть на 260 с»
//     больше нельзя, и вызывающий получает ТИПИЗИРОВАННЫЙ ответ с возрастом
//     заявки вместо тишины;
//   - появилась диагностика (LoadClaimsR303) — оператор и WebUI видят, кто держит
//     заявку и сколько времени, а не гадают;
//   - мёртвую заявку можно снять (ReclaimStaleLoadClaimR303) — но только по
//     положительному признаку: в реестре НЕТ инстанса модели в состоянии
//     StateLoading. Заявка живой загрузки не снимается никогда (это был бы второй
//     C-вызов LoadModel на тот же файл — двойная VRAM и SIGABRT в мосте).
package cppbackend

import (
	"sort"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// loadClaimMetaR303 — метаданные заявки на загрузку (под b.loadMu).
type loadClaimMetaR303 struct {
	startedAt time.Time
	requester string
}

// claimMetaLockedR303 — зафиксировать метаданные заявки. Вызывающий держит loadMu.
// Ленивая инициализация мапы: Backend может быть создан литералом (тесты),
// тогда loadClaims == nil.
func (b *Backend) claimMetaLockedR303(name, requester string) {
	if b == nil || name == "" {
		return
	}
	if b.loadClaims == nil {
		b.loadClaims = make(map[string]*loadClaimMetaR303)
	}
	if requester == "" {
		requester = "unknown"
	}
	b.loadClaims[name] = &loadClaimMetaR303{
		startedAt: time.Now(),
		requester: requester,
	}
}

// LoadClaimR303 — снимок одной заявки на загрузку.
//
// AgeSec — сколько секунд заявка уже висит (0 — только что взята).
// LoadingInstance — положительный признак ЖИВОЙ загрузки: модель есть в реестре
// и её состояние StateLoading. Именно по нему отличается «грузится» от
// «заявка осталась от умершей горутины».
type LoadClaimR303 struct {
	Model           string  `json:"model"`
	AgeSec          float64 `json:"age_sec"`
	Requester       string  `json:"requester"`
	LoadingInstance bool    `json:"loading_instance"`
	ModelLoaded     bool    `json:"model_loaded"`
}

// LoadClaimR303 — снимок заявки по модели (ok=false — заявки нет).
func (b *Backend) LoadClaimR303(name string) (LoadClaimR303, bool) {
	if b == nil || name == "" {
		return LoadClaimR303{}, false
	}
	b.loadMu.Lock()
	meta, ok := b.loadClaims[name]
	_, claimed := b.loading[name]
	b.loadMu.Unlock()
	if !ok || !claimed || meta == nil {
		return LoadClaimR303{}, false
	}
	out := LoadClaimR303{
		Model:     name,
		AgeSec:    time.Since(meta.startedAt).Seconds(),
		Requester: meta.requester,
	}
	// Состояние инстанса читаем ВНЕ loadMu: порядок блокировок в пакете —
	// loadMu → b.mu (см. TryLockLoad), но наружу мы его не выносим.
	if inst := b.lookupInstanceR303(name); inst != nil {
		inst.mu.Lock()
		state := inst.info.State
		inst.mu.Unlock()
		out.LoadingInstance = state == StateLoading
		out.ModelLoaded = state == StateLoaded
	}
	return out, true
}

// lookupInstanceR303 — инстанс модели под b.mu (nil если модели нет).
func (b *Backend) lookupInstanceR303(name string) *modelInstance {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.models[name]
}

// LoadClaimsR303 — все заявки на загрузку, по возрастанию возраста не важно:
// порядок детерминированный (по имени модели), чтобы ответы API не «мигали».
func (b *Backend) LoadClaimsR303() []LoadClaimR303 {
	if b == nil {
		return nil
	}
	b.loadMu.Lock()
	names := make([]string, 0, len(b.loading))
	for name := range b.loading {
		names = append(names, name)
	}
	b.loadMu.Unlock()
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	out := make([]LoadClaimR303, 0, len(names))
	for _, name := range names {
		if c, ok := b.LoadClaimR303(name); ok {
			out = append(out, c)
		}
	}
	return out
}

// TryLockLoadR303 — TryLockLoad с указанием владельца заявки.
func (b *Backend) TryLockLoadR303(name, requester string) (bool, error) {
	return b.tryLockLoad(name, requester)
}

// TryLockReloadR303 — TryLockReload с указанием владельца заявки.
func (b *Backend) TryLockReloadR303(name, requester string) (bool, error) {
	return b.tryLockReload(name, requester)
}

// WaitForLoadTimeoutR303 — ограниченное ожидание чужой загрузки.
//
// Возвращает (loaded, timedOut):
//   - (true,  false) — загрузка завершилась успешно (модель в реестре);
//   - (false, false) — загрузка завершилась НЕУДАЧЕЙ (канал закрыт, модели нет);
//   - (false, true)  — истёк срок ожидания, заявка всё ещё висит.
//
// d == 0 — ждать без лимита (прежнее поведение, оставлено для тестов и для
// явного «жду сколько нужно»), d < 0 — не ждать вовсе.
func (b *Backend) WaitForLoadTimeoutR303(name string, d time.Duration) (bool, bool) {
	if b == nil || name == "" {
		return false, false
	}
	if d < 0 {
		_, err := b.GetModel(name)
		return err == nil, err != nil
	}
	b.loadMu.Lock()
	ch, ok := b.loading[name]
	b.loadMu.Unlock()
	if ok {
		if d == 0 {
			<-ch
		} else {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ch:
			case <-timer.C:
				if _, err := b.GetModel(name); err == nil {
					return true, false
				}
				return false, true
			}
		}
	}
	if _, err := b.GetModel(name); err == nil {
		return true, false
	}
	return false, false
}

// ReclaimStaleLoadClaimR303 — снять МЁРТВУЮ заявку на загрузку.
//
// Снимается только при положительном доказательстве, что загрузки нет:
//   - заявка висит дольше minAge;
//   - и в реестре НЕТ инстанса модели в состоянии StateLoading.
//
// Если инстанс в StateLoading есть — это настоящая загрузка (в том числе
// зависшая в C-мосту): заявку не трогаем, иначе второй load запустит второй
// C-вызов на тот же файл (двойная VRAM, SIGABRT в мосте). Для такого случая
// есть AbortLoad (force-выгрузка) и типизированный ответ writeLoadStuckR303.
//
// Возвращает снимок снятой заявки и признак успеха.
func (b *Backend) ReclaimStaleLoadClaimR303(name string, minAge time.Duration) (LoadClaimR303, bool) {
	if b == nil || name == "" {
		return LoadClaimR303{}, false
	}
	claim, ok := b.LoadClaimR303(name)
	if !ok {
		return LoadClaimR303{}, false
	}
	if minAge > 0 && claim.AgeSec*float64(time.Second) < float64(minAge) {
		return claim, false
	}
	if claim.LoadingInstance {
		// Живая (в том числе зависшая) загрузка — не наша.
		return claim, false
	}

	b.loadMu.Lock()
	meta, exists := b.loadClaims[name]
	ch, claimed := b.loading[name]
	// Перепроверка под локом: между снимком и локом заявку могли снять/взять.
	if !exists || !claimed || meta == nil || (minAge > 0 && time.Since(meta.startedAt) < minAge) {
		b.loadMu.Unlock()
		return claim, false
	}
	close(ch)
	delete(b.loading, name)
	delete(b.loadClaims, name)
	b.loadMu.Unlock()

	logger.Get().Warnw("R303: снята мёртвая заявка на загрузку (модель не грузится, заявка осталась)",
		"name", name,
		"claim_age_sec", claim.AgeSec,
		"requester", claim.Requester,
		"loading_instance", claim.LoadingInstance)
	return claim, true
}
