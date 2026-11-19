// nctx_reload_coordinator.go — R634 (2026-11-19): состояние координатора: подсказки, гейт перед перезагрузкой, заявки, желаемое окно, счётчик циклов.
//
// ЗАЧЕМ ВЫНЕСЕНО. `nctx_reload.go` держал 1865 строк: типы и конфигурацию,
// состояние координатора, решения о перезагрузке, HTTP-клиент, длинный DoReload,
// метрики и опрос прогресса. Здесь — один из слоёв; поведение не менялось.
package balancer

import "time"

// ReloadHints — R70 (2026-09-24): подсказки для reload'а, которые знает только
// Proxy (профиль модели, фактическое состояние загруженной модели).
type ReloadHints struct {
	// KVCacheType — рабочий тип KV-cache ("f16"/"q8_0"/"q4_0"), "" = неизвестен.
	// Передаётся cppworker'у как ?kv_cache_type=…, чтобы адаптивная стратегия
	// считала по реальному типу, а не по f16 (иначе для 65K на 8 GB она выбирает
	// cpu_only/gpu_layers=0, хотя с q4_0 модель влезает на GPU).
	KVCacheType string
	// GPULayers — R83-fix (2026-09-30): сколько слоёв СЕЙЧАС на GPU у загруженной	// модели (`gpu_layers` в /api/models cppworker). 0 = неизвестно.
	//
	// ЗАЧЕМ. Reload не имеет права УХУДШАТЬ раскладку: если адаптивная стратегия
	// предлагает меньше слоёв, чем уже работает (типичный случай — stage=cpu_only
	// при неизвестном бюджете), то перезагрузка ради чуть большего окна выкинет
	// модель из VRAM, ускорить ничего не ускорит и превратит нормальный запрос в
	// минуты ожидания. Живой лог 2026-09-30: gemma-4 работала с 19 слоями на GPU,
	// reload «на 131072» вернул gpuLayers=0 и contextSize=65536 (то же окно).
	GPULayers int
	// WindowNCtx — R83-политика (2026-10-01): СУММАРНОЕ окно, с которым модель
	// загружена сейчас (`context_size` из /api/models). 0 = неизвестно.
	//
	// Зачем. DoReload обязан уметь сказать «перезагрузка не изменит окно»:
	// адаптивная стратегия может срезать цель (131072 → 65536), и тогда reload
	// стоит 1–3 минуты простоя, не давая клиенту ничего.
	WindowNCtx int
	// Parallel — R119 (2026-10-15): число слотов модели (профиль оператора, иначе
	// фактическое из /api/models).
	//
	// ЗАЧЕМ. Тело reload собиралось с нуля и `parallel` в него не попадало:
	// cppworker брал CPPWORKER_N_PARALLEL, и модель, поднятая профилем с
	// parallel=3, после preflight-перезагрузки молча возвращалась к 2 слотам.
	Parallel int
	// EnableReasoning — R119 (2026-10-15): per-model переключатель размышлений из
	// профиля. nil = «не задан» (наследовать политику cppworker).
	//
	// ЗАЧЕМ. На reload это поле не отправлялось, и `opts.EnableReasoning`
	// оставался nil → cppbackend падал на cfg.DefaultEnableReasoning (false):
	// любая перезагрузка тихо выключала уже включённые размышления.
	EnableReasoning *bool
}

// SetReloadHintsProvider — установка поставщика хинтов (вызывается Proxy'ем).
func (c *NCtxReloadCoordinator) SetReloadHintsProvider(fn func(backendID, modelName string) ReloadHints) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.reloadHints = fn
	c.mu.Unlock()
}

// ReloadHintsFor — хинты для reload'а конкретной модели (nil-safe).
func (c *NCtxReloadCoordinator) ReloadHintsFor(backendID, modelName string) ReloadHints {
	if c == nil {
		return ReloadHints{}
	}
	c.mu.RLock()
	fn := c.reloadHints
	c.mu.RUnlock()
	if fn == nil {
		return ReloadHints{}
	}
	return fn(backendID, modelName)
}

// SetBeforeReloadGuard — R493 (2026-11-17): последняя проверка ПЕРЕД тем, как
// перезагрузка реально начнётся.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ХУК, ЕСЛИ ПРОВЕРКИ ЗАНЯТОСТИ УЖЕ ЕСТЬ. Проверки в решателях
// (`growthDeferR299`, `contextGrowthBlocker`, `decideWindowR493`) стоят ДО
// решения, а между решением и фактическим unload+load проходит время: клиент
// успевает начать новый ход, а перезагрузка всё равно стартует и рвёт его.
// Живой факт (архив второго стенда): 06:24:53 запуск reload на 194296,
// 06:25:02 узел уже грузится с `partial offload 62/64`, 06:25:43 — следующая
// перезагрузка; ходы Cline при этом шли.
//
// Координатор не знает о стримах (это состояние Proxy), поэтому Proxy ставит
// сюда функцию: (false, причина) — перезагрузку НЕ начинать, клиент получит
// «занято, повторите» вместо оборванного чужого ответа.
func (c *NCtxReloadCoordinator) SetBeforeReloadGuard(fn func(backendID, modelName string) (bool, string)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.beforeReload = fn
	c.mu.Unlock()
}

// reloadAllowed — можно ли начинать перезагрузку прямо сейчас (nil-safe).
func (c *NCtxReloadCoordinator) reloadAllowed(backendID, modelName string) (bool, string) {
	if c == nil {
		return true, ""
	}
	c.mu.RLock()
	fn := c.beforeReload
	c.mu.RUnlock()
	if fn == nil {
		return true, ""
	}
	return fn(backendID, modelName)
}

// StartModelReloadIfNotPending — R69 (2026-09-23): ЕДИНЫЙ gate для всех
// триггеров reload (preflight, R60.47-обработчик 413, R60.35/44
// executeAsyncReload). Ключ — только (backend, model), целевой n_ctx не важен:
// cppworker не умеет вести две загрузки одной модели, вторую он отменяет
// («load cancelled: model load aborted by user»), после чего rollback падает
// («reload rollback failed: model is no longer loaded») и модель остаётся
// выгруженной.
//
// Возвращает true, если загрузка запущена этим вызовом; false — если для этой
// модели reload уже идёт (fn НЕ вызывается).
func (c *NCtxReloadCoordinator) StartModelReloadIfNotPending(
	backendID, modelName string, fn func(entry *reloadEntry),
) bool {
	if c == nil || c.reloadDedup == nil {
		go fn(nil)
		return true
	}
	// Проверяем ЛЮБОЙ уже идущий reload этой модели (в т.ч. с другим target).
	if c.reloadDedup.IsReloadPending(backendID, modelName) {
		return false
	}
	_, started := c.reloadDedup.StartReloadIfNotPending(backendID, modelName, 0, fn)
	return started
}

// RecordRequestedNCtx — R69: запомнить n_ctx, который запросил клиент
// (num_ctx из тела запроса). Хранится максимум за desiredNCtxTTL.
func (c *NCtxReloadCoordinator) RecordRequestedNCtx(backendID, modelName string, nCtx int) {
	if c == nil || nCtx <= 0 || backendID == "" || modelName == "" {
		return
	}
	c.desiredMu.Lock()
	defer c.desiredMu.Unlock()
	if c.desired == nil {
		c.desired = make(map[string]desiredNCtxEntry)
	}
	key := backendID + "\x00" + modelName
	cur, ok := c.desired[key]
	if !ok || nCtx > cur.nCtx || time.Since(cur.at) > desiredNCtxTTL {
		c.desired[key] = desiredNCtxEntry{nCtx: nCtx, at: time.Now()}
		return
	}
	// Освежаем отметку времени, значение оставляем максимальным.
	cur.at = time.Now()
	c.desired[key] = cur
}

// DesiredNCtx — R69: максимальный n_ctx, который клиент просил за последние
// desiredNCtxTTL. 0 = неизвестно (AutoTune не ограничивается).
func (c *NCtxReloadCoordinator) DesiredNCtx(backendID, modelName string) int {
	if c == nil || backendID == "" || modelName == "" {
		return 0
	}
	c.desiredMu.Lock()
	defer c.desiredMu.Unlock()
	key := backendID + "\x00" + modelName
	entry, ok := c.desired[key]
	if !ok {
		return 0
	}
	if time.Since(entry.at) > desiredNCtxTTL {
		delete(c.desired, key)
		return 0
	}
	return entry.nCtx
}

func (c *NCtxReloadCoordinator) IsReloadPending(backendID, modelName string) bool {
	if c == nil || c.reloadDedup == nil {
		return false
	}
	return c.reloadDedup.IsReloadPending(backendID, modelName)
}

// BeginExternalReload — R230: пометить reload идущим для путей, которые сами
// выполняют unload+load (AutoTune apply). Пока маркер жив, ensureModelLoaded-
// OnBackend не запускает параллельную ленивую загрузку с профильными params
// (это перехватывало reload AutoTune и оставляло конфигурацию неизменной).
// Возвращает finish (идемпотентный) и ok=false, если reload уже идёт.
func (c *NCtxReloadCoordinator) BeginExternalReload(backendID, modelName string) (func(), bool) {
	if c == nil || c.reloadDedup == nil {
		return func() {}, true
	}
	return c.reloadDedup.BeginExternalReload(backendID, modelName)
}

func (c *NCtxReloadCoordinator) WaitReloadDone(backendID, modelName string, timeout time.Duration) error {
	if c == nil || c.reloadDedup == nil {
		return nil
	}
	return c.reloadDedup.WaitReloadDone(backendID, modelName, timeout)
}

func (c *NCtxReloadCoordinator) Config() NCtxReloadConfig {
	return c.config
}

func (c *NCtxReloadCoordinator) SetConfig(cfg NCtxReloadConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.config = cfg
}

func (c *NCtxReloadCoordinator) state(backendID string) *backendReloadState {
	c.mu.RLock()
	s, ok := c.perBackend[backendID]
	c.mu.RUnlock()
	if ok {
		return s
	}
	c.mu.Lock()
	s, ok = c.perBackend[backendID]
	if !ok {
		s = &backendReloadState{}
		c.perBackend[backendID] = s
	}
	c.mu.Unlock()
	return s
}

// SetLastKnownNCtx — устанавливает lastKnownNCtx для бэкенда.
//
// R60.32 (2026-09-10): ранний возврат на nCtx <= 0 был БАГОМ —
// R60.31 stickiness check (preflight_nctx.go:335-360) опирается на
// state.CurrentNCtx, и при unload (nCtx=0) state оставался stale
// (предыдущее loaded значение) → preflight думал "модель загружена"
// → NoOp → следующий запрос получал 502 от cppworker (модель
// реально не загружена). Теперь nCtx=0 сбрасывает state явно.
func (c *NCtxReloadCoordinator) SetLastKnownNCtx(backendID string, nCtx int) {
	s := c.state(backendID)
	s.inflightMu.Lock()
	s.lastKnownNCtx = nCtx
	s.inflightMu.Unlock()
}

func (c *NCtxReloadCoordinator) LastKnownNCtx(backendID string) int {
	s := c.state(backendID)
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.lastKnownNCtx
}

func (c *NCtxReloadCoordinator) RecordCycleAttempt(backendID string) {
	s := c.state(backendID)
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	s.consecutiveCycleFailures++
	s.lastCycleReset = time.Now()
}

func (c *NCtxReloadCoordinator) IsCycleDetected(backendID string, maxAttempts int) bool {
	s := c.state(backendID)
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()

	const cycleResetInterval = 5 * time.Minute
	if !s.lastCycleReset.IsZero() && time.Since(s.lastCycleReset) > cycleResetInterval {
		s.consecutiveCycleFailures = 0
		return false
	}
	return s.consecutiveCycleFailures >= maxAttempts
}

func (c *NCtxReloadCoordinator) ResetCycleCounter(backendID string) {
	s := c.state(backendID)
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	s.consecutiveCycleFailures = 0
	s.lastCycleReset = time.Now()
}

// ============================================================

// ============================================================

//

//
