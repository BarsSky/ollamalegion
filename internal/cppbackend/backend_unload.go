package cppbackend

import (
	"fmt"
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"time"
)

// removeOwnInstance — убрать запись модели из реестра, ТОЛЬКО если она всё ещё
// указывает на наш инстанс.
//
// R83 §9.6 D-B (2026-09-27). Зачем отдельный метод и проверка идентичности:
// провалившаяся загрузка обязана убрать за собой фантомную запись (`inst`
// регистрируется ДО C-вызова, чтобы UI видел state=loading). Но при перекрытии
// загрузок ту же модель успевает пересоздать другая горутина, и безусловный
// delete стирает уже ЕЁ — работающую — запись. Симптом снаружи: «загрузка
// завершилась в логе, а /api/models отдаёт count=0», балансер считает модель
// выгруженной и шлёт запросы в никуда.
//
// Вынесен отдельно, чтобы поведение можно было проверить тестом без реального
// C-сбоя: в stub-сборке bridge.LoadModel не падает, и добраться до этого пути
// иначе нельзя.
func (b *Backend) removeOwnInstance(name string, inst *modelInstance) {
	if b == nil || name == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, ok := b.models[name]
	switch {
	case !ok:
		// Записи уже нет (например, её снял unload) — убирать нечего.
		return
	case cur == inst:
		delete(b.models, name)
	default:
		// В реестре уже НОВЫЙ инстанс этой модели: он нашему провалу не
		// принадлежит и трогать его нельзя.
		logger.Get().Warnw(i18n.T("cppbackend.backend.load_failure_entry_replaced"),
			"name", name)
	}
}

// UnloadModel выгружает модель
func (b *Backend) UnloadModel(name string) error {
	return b.unloadModel(name, nil)
}

// unloadModel — выгрузка с проверкой идентичности инстанса (R224).
//
// ЗАЧЕМ ПАРАМЕТР expect. Авто-выгрузка после потери RPC-сессии запускается в
// горутине по факту сбоя. Между сбоем и фактической выгрузкой следующий запрос
// может успеть перезагрузить модель (lazy-load) — и тогда `UnloadModel(name)`
// освободил бы НОВЫЙ, только что загруженный инстанс: загрузка падала (живой
// прогон на паре контейнеров закончился exit code 2), а модель пропадала из
// реестра. Поэтому: если в реестре уже другой инстанс — не трогаем его.
func (b *Backend) unloadModel(name string, expect *modelInstance) error {
	b.mu.Lock()
	inst, exists := b.models[name]
	if !exists {
		b.mu.Unlock()
		return fmt.Errorf("model %s not found", name)
	}
	if expect != nil && inst != expect {
		b.mu.Unlock()
		logger.Get().Infow(i18n.T("cppbackend.backend.unload_new_instance"),
			"model", name, "reason", "instance_replaced")
		return nil
	}
	delete(b.models, name)
	b.mu.Unlock()

	// R114: снимаем наблюдение за узлами раскладки — модель уходит из памяти,
	// наблюдать нечего. Делаем ПОСЛЕ проверки идентичности: у нового инстанса
	// сторож свой, останавливать его нельзя.
	b.stopRPCWatchdog(name)

	// Round 15.1: stop BatchedScheduler ДО FreeModel, чтобы in-flight
	// sessions получили ошибку через ctx.Done() и закрыли каналы.
	// Без этого Run() goroutine может пытаться обращаться к handle
	// после FreeModel() — use-after-free в C-bridge (SIGABRT / segfault).
	if inst.batchedSchedulerCancel != nil {
		inst.batchedSchedulerCancel() // cancel ctx → Run() returns
		inst.batchedSchedulerCancel = nil
	}
	if inst.batchedScheduler != nil {
		// Round 16 code-review fix (2026-07-30): добавил timeout.
		// Round 8 BUGFIX: <inst.batchedScheduler.Done() без timeout —
		// если Run() goroutine зависнет (deadlock, infinite loop, blocked
		// channel), UnloadModel блокируется НАВСЕГДА → HTTP /api/models/unload
		// зависает, админ-операции недоступны. DOS-вектор.
		//
		// Timeout 10 сек — generous: нормальный shutdown Run() = <1 сек
		// (один tick). 10 сек = 10x headroom для медленных систем.
		// После timeout — force-detach (Nil scheduler), C-bridge handle
		// всё ещё будет FreeModel'ен ниже. Run() goroutine упадёт с nil
		// pointer dereference или завершится когда C-bridge handle free'н —
		// в обоих случаях процесс не зависнет.
		select {
		case <-inst.batchedScheduler.Done():
			// Normal shutdown
		case <-time.After(10 * time.Second):
			logger.Get().Errorw("UnloadModel: BatchedScheduler.Run() did not return within 10s, force-detaching",
				"model", name, "active_sessions", inst.batchedScheduler.ActiveSessions())
			// Record в metrics для postmortem — это указывает на баг
			// в scheduler (deadlock, infinite loop, или blocked channel).
			if b.metrics != nil {
				b.metrics.RecordUnloadTimeout(name, "batched_scheduler_stuck")
			}
		}
		inst.batchedScheduler = nil
	}

	inst.mu.Lock()
	if inst.handle != nil {
		inst.handle.FreeModel()
	}
	inst.info.State = StateUnloaded
	// Сбрасываем loading-метаданные, чтобы UI не показывал спиннер
	// для уже выгруженной модели.
	inst.info.LoadingStartedAt = time.Time{}
	inst.info.LoadingSizeBytes = 0
	inst.info.LoadingError = ""
	inst.info.BatchedParallel = false
	inst.mu.Unlock()

	// Записываем метрики
	if b.metrics != nil {
		b.metrics.RecordUnload(name)
	}

	// R83-фикс (2026-10-01): обновляем снимок VRAM СРАЗУ после выгрузки.
	//
	// Зачем. MemfitBudget читает снимок b.gpuDevices (не зовёт CUDA в горячем
	// пути). После выгрузки модели память освободилась, но снимок оставался
	// СТАРЫМ — со «занятой» моделью. Поэтому следующая загрузка (reload)
	// оценивала бюджет как «свободно ~4.7 ГБ минус резерв» и раскладывала на GPU
	// лишь ~18-20 слоёв из 43, хотя после выгрузки доступно ~8 ГБ.
	//
	// Живой замер (2026-10-01, gemma-4 Q4_K_M, RTX 3070 8 GB):
	//   memfit verdict: requestedGPULayers=40 → optimal=18,
	//   vramAvailableMB=2671 (снимок со занятой моделью), stage=partial_offload;
	//   фактически после загрузки 18 слоёв: vramUsedMB=3476, свободно 4715 МБ.
	// Итог: 25 слоёв считались на CPU, prefill 22.6k токенов занимал ~300 с, и
	// клиент (Cline, лимит 300 с) не дожидался первого токена.
	b.refreshGPUDeviceVRAM()

	logger.Get().Infow("model unloaded", "name", name)
	return nil
}

// GetModel возвращает информацию о модели
func (b *Backend) GetModel(name string) (*ModelInfo, error) {
	b.mu.RLock()
	inst, exists := b.models[name]
	b.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("model %s not found", name)
	}

	info := inst.info
	info.ActiveQueries = b.getActiveQueries(inst)
	info.LastUsedAt = b.getLastUsedAt(inst)
	return &info, nil
}

// GetModelByPath — Round 32 #8 (2026-08-10): ищет загруженную модель по file path.
// Используется handler'ом для dedup ПЕРЕД async load — чтобы вернуть
// "already_loaded" пользователю, а не 202 + background error.
//
// Раньше: handler возвращал 202 + spawn'ил background load. LoadModelWithOpts
// внутри goroutine делал dedup check (если path уже загружен) — НО response
// уже был отправлен, и dedup просто логировался как error в фоне. Пользователь
// не знал, что его запрос был dedup'нут, и видел "loading" статус на UI
// (который никогда не становился "loaded", потому что реально load не шёл).
func (b *Backend) GetModelByPath(path string) (existingName string, info *ModelInfo, found bool) {
	if path == "" {
		return "", nil, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for name, inst := range b.models {
		if inst.info.Path == path {
			infoCopy := inst.info
			infoCopy.ActiveQueries = b.getActiveQueries(inst)
			infoCopy.LastUsedAt = b.getLastUsedAt(inst)
			return name, &infoCopy, true
		}
	}
	return "", nil, false
}

// GetHandle — Round 31 #6 (2026-08-09): возвращает *bridge.ModelHandle
// для загруженной модели. Используется abort_watcher (cmd/cppworker/abort_watcher.go)
// для проброса в bridge.RequestAbort при ctx.Done.
//
// Возвращает (handle, true) если модель загружена, (nil, false) иначе.
// Thread-safe: RLock на короткое время. Callers не должны мутировать
// полученный handle (он shared с C-bridge).
func (b *Backend) GetHandle(name string) (*bridge.ModelHandle, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	inst, exists := b.models[name]
	if !exists {
		return nil, false
	}
	return inst.handle, true
}

// GetAllHandles — Round 31 #6 (2026-08-09): возвращает snapshot всех
// загруженных handles. Используется при cppworker shutdown для
// итеративного вызова bridge.RequestAbort (см. PLAN.md §2.8 DECISION Q1).
//
// Возвращает slice в произвольном порядке (Go map iteration order).
// Если backend пуст — возвращает empty slice (не nil).
func (b *Backend) GetAllHandles() []*bridge.ModelHandle {
	b.mu.RLock()
	defer b.mu.RUnlock()
	handles := make([]*bridge.ModelHandle, 0, len(b.models))
	for _, inst := range b.models {
		handles = append(handles, inst.handle)
	}
	return handles
}

// AutoEnableReasoning — Round 17 Layer 3 (2026-07-31): LAZY AUTO-DETECT.
//
// Парсер в writeOpenAIChatStream / handleV1ChatCompletions вызывает этот
// метод когда видит `<think>` в output модели, у которой reasoningEnabled=false
// (т.е. модель не в IsReasoningModel whitelist и пользователь не задал
// load-with-params enableReasoning=true). Включает routing для этой модели
// "на лету" — все последующие запросы будут split'ить <think> корректно.
//
// Идемпотентно: повторный вызов на уже-enabled модели — no-op.
// Безопасно при многократных concurrent вызовах: под b.mu.Lock.
//
// Не применяется, если модель не загружена (UnloadModel между запросами)
// — в этом случае пользователю нужно явно указать enableReasoning=true
// при следующем LoadModelWithOpts.
//
// Возвращает true если флаг реально изменился (false→true), false если
// уже был включён. Используется для логирования и метрик.
func (b *Backend) AutoEnableReasoning(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	inst, exists := b.models[name]
	if !exists {
		return false
	}
	if inst.reasoningEnabled {
		return false
	}
	inst.reasoningEnabled = true
	inst.info.ReasoningEnabled = true
	logger.Get().Warnw("reasoning auto-enabled (lazy detect)",
		"model", name,
		"hint", "model emits <think> but not in IsReasoningModel whitelist; "+
			"to pre-set, use load-with-params with enableReasoning=true, "+
			"or add model name pattern to CPPWORKER_REASONING_ARCHS env")
	if b.metrics != nil {
		// Counter-based observability для postmortem.
		// Если counter быстро растёт — есть системная проблема с whitelist
		// или с пользователями, загружающими reasoning-модели без флага.
		b.metrics.RecordReasoningAutoEnable(name)
	}
	return true
}

// ListModels возвращает список всех загруженных моделей
func (b *Backend) ListModels() []ModelInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]ModelInfo, 0, len(b.models))
	for _, inst := range b.models {
		info := inst.info
		// R83-fix (2026-09-30): счётчики берём из атомиков (lock-free).
		// inst.info читается без inst.mu намеренно: этот снимок должен
		// отвечать МГНОВЕННО даже во время генерации (его читают poller
		// балансера, WebUI и агент). Поля, меняющиеся во время инференса,
		// здесь перезаписываются атомарными значениями.
		info.ActiveQueries = inst.activeQueriesCount()
		info.TotalQueries = inst.totalQueriesCount()
		info.LastUsedAt = b.getLastUsedAt(inst)
		// Round 18 P0.1 (2026-08-03): single source of truth для capabilities.
		// Используем реальный reasoningEnabled из instance (не из whitelist),
		// чтобы auto-detect (Round 17 Layer 3) тоже отражался.
		caps := types.CapabilitiesFromModelInfo(
			info.Name,
			inst.reasoningEnabled,
			info.Architecture,
			info.ContextSize,
			info.GGUFContextLength,
		)
		info.Capabilities = &caps
		result = append(result, info)
	}
	return result
}

// getLastUsedAt возвращает момент последнего использования модели.
// Используется IdleUnloadManager (через ListModels) и монитором для
// отображения idle-времени. atomic.Value.Load lock-free, в отличие от
// чтения через inst.mu, что важно при вызовах из горячего пути ListModels.
// Если lastUsedAt не было инициализировано (например модель только в процессе
// загрузки), возвращает LoadedAt как fallback.
func (b *Backend) getLastUsedAt(inst *modelInstance) time.Time {
	if v := inst.lastUsedAt.Load(); v != nil {
		if t, ok := v.(time.Time); ok && !t.IsZero() {
			return t
		}
	}
	return inst.info.LoadedAt
}

// GetLoadingModels возвращает список моделей в состоянии Loading/Error.
// Используется UI (монитор, вкладка бэкендов, GGUF) для отображения
// спиннера и elapsed-time «Загружается model-name 25s».
//
// Потокобезопасно: захватывает mu.RLock на короткое время.
func (b *Backend) GetLoadingModels() []ModelInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]ModelInfo, 0)
	now := time.Now()
	for _, inst := range b.models {
		if inst.info.State == StateLoading || inst.info.State == StateError {
			info := inst.info
			info.ActiveQueries = b.getActiveQueries(inst)
			// Гарантируем непустой LoadingStartedAt для UI.
			if info.LoadingStartedAt.IsZero() {
				info.LoadingStartedAt = info.LoadedAt
			}
			// Заполняем elapsedMs (нс → мс) — удобно для UI, не нужно
			// делать arithmetic в JS. Вычисляем «на лету» из текущего
			// времени, так как LoadingStartedAt не меняется в процессе.
			_ = now // резерв на случай будущего счётчика в снапшоте
			result = append(result, info)
		}
	}
	return result
}

// IsModelLoading — быстрая проверка «идёт ли загрузка модели» по имени.
// Используется балансировщиком в proxyRequestLlamaCpp для решения,
// включать ли loading-fast-path с heartbeat-чанками.
func (b *Backend) IsModelLoading(name string) bool {
	b.mu.RLock()
	inst, exists := b.models[name]
	b.mu.RUnlock()
	if !exists {
		return false
	}
	inst.mu.Lock()
	state := inst.info.State
	inst.mu.Unlock()
	return state == StateLoading
}

// IsLoading сообщает, идёт ли сейчас загрузка или перезагрузка модели name.
//
// R83 §9.6 D-C (2026-09-26): нужен обработчику unload. Раньше unload во время
// загрузки 16 GB отвечал 200 мгновенно (проверялись только активные
// inference-запросы через InFlight, а загрузка там не учитывается): модель
// удалялась из реестра и тут же возвращалась туда фоновой загрузкой — то самое
// «model loaded в логе, /api/models → count=0».
func (b *Backend) IsLoading(name string) bool {
	b.loadMu.Lock()
	defer b.loadMu.Unlock()
	_, ok := b.loading[name]
	return ok
}

// AbortLoad — R215 (2026-10-21): прервать ИДУЩУЮ загрузку модели.
//
// Ставит флаг отмены в C-мосте (bridge.RequestLoadAbort); llama.cpp проверяет
// его в progress-callback и останавливает чтение весов, после чего сам
// освобождает частично загруженную модель - включая буферы на узлах раскладки.
//
// ЗАЧЕМ. До R215 единственный вызывающий RequestLoadAbort передавал
// context.Background(), то есть флаг не выставлялся НИКОГДА: «отменить загрузку»
// было невозможно, `unload?force=true` означал «дождаться конца загрузки».
// На крупной модели веса успевали лечь на GPU и держали VRAM минуты.
//
// Возвращает true, если флаг выставлен (или загрузка уже завершилась).
func (b *Backend) AbortLoad(name string) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	inst := b.models[name]
	b.mu.RUnlock()
	if inst == nil || inst.handle == nil {
		return false
	}
	b.mu.RLock()
	state := inst.info.State
	b.mu.RUnlock()
	if state != StateLoading {
		return false
	}
	if err := bridge.RequestLoadAbort(inst.handle); err != nil {
		// ptr ещё не выставлен (ранний expose не успел): загрузка не отменена.
		// Это не ошибка оператора, но и не успех - говорим честно.
		logger.Get().Warnw(i18n.T("cppbackend.backend.abort_request_not_applied"), "model", name, "error", err)
		return false
	}
	logger.Get().Infow(i18n.T("cppbackend.backend.abort_request_applied"), "model", name)
	return true
}

// TryLockLoad пытается зарезервировать эксклюзивное право на загрузку модели name.
// Возвращает:
//   - (true, nil): блокировка получена, вызывающий может начинать загрузку.
//   - (true, err): модель уже загружена (ошибка).
//   - (false, nil): другая горутина уже грузит эту модель — жди.
//   - (false, err): модель уже загружена или другая ошибка.
//
// Гарантирует, что для одной модели может быть только одна активная загрузка.
// После завершения загрузки (успех/ошибка) вызывающий ОБЯЗАН вызвать UnlockLoad.
// ИСПОЛЬЗУЕТСЯ ТОЛЬКО ДЛЯ ПЕРВИЧНОЙ ЗАГРУЗКИ. Для reload используйте TryLockReload.
func (b *Backend) TryLockLoad(name string) (bool, error) {
	return b.tryLockLoad(name, "load")
}

// tryLockLoad — тело TryLockLoad с указанием владельца заявки (R303).
func (b *Backend) tryLockLoad(name, requester string) (bool, error) {
	b.loadMu.Lock()
	defer b.loadMu.Unlock()

	// R152 (2026-10-16): СНАЧАЛА проверяем «идёт загрузка/перезагрузка», и только
	// потом «модель уже загружена». Раньше порядок был обратный, и это ломало
	// приоритет оператора.
	//
	// ЖИВОЙ ДЕФЕКТ (мастер, 2026-10-16). Оператор нажал «применить и перезагрузить»
	// (асинхронный reload, ctx=8192). Пока он шёл, прилетел load-with-params:
	// TryLockLoad увидел в b.models СТАРУЮ запись (reload ещё не успел её убрать),
	// вернул «model already loaded», и handleLoadWithParams решил «параметры
	// изменились» → UnloadModel + новая загрузка. Первая загрузка получила
	// "load cancelled: model load aborted by user (R60.57 ... BRIDGE_ERR_ABORTED)",
	// а модель поднялась с параметрами ВТОРОГО запроса (32768/q8_0/reasoning off) —
	// то есть применение оператора было молча отменено.
	//
	// Теперь занятый load/reload честно сообщается как «грузится» (false, nil):
	// вызывающий уходит в respondWhileAnotherLoadInProgress (202 + progressUrl) и
	// НЕ выгружает модель, то есть не отменяет чужую загрузку.
	if _, ok := b.loading[name]; ok {
		return false, nil
	}

	// Модель уже загружена и никакой загрузки не идёт — прежний смысл ошибки.
	b.mu.RLock()
	_, exists := b.models[name]
	b.mu.RUnlock()
	if exists {
		return true, fmt.Errorf("model %s already loaded", name)
	}

	// Создаём канал, который будет закрыт после завершения загрузки
	b.loading[name] = make(chan struct{})
	// R303: фиксируем владельца и время — по ним отличаем «грузится» от
	// «заявка осталась от умершей горутины» (load_claims.go).
	b.claimMetaLockedR303(name, requester)
	return true, nil
}

// TryLockReload — то же что TryLockLoad, но НЕ проверяет b.models.
// Нужен для /api/models/reload, когда модель уже загружена в b.models
// и мы хотим перезагрузить её с новыми параметрами.
//
// Гарантирует, что для одной модели может быть только один активный reload.
// После завершения reload (успех/ошибка) вызывающий ОБЯЗАН вызвать UnlockLoad.
func (b *Backend) TryLockReload(name string) (bool, error) {
	return b.tryLockReload(name, "reload")
}

// tryLockReload — тело TryLockReload с указанием владельца заявки (R303).
func (b *Backend) tryLockReload(name, requester string) (bool, error) {
	b.loadMu.Lock()
	defer b.loadMu.Unlock()

	// Проверяем, не reload'ит ли её другая горутина
	if _, ok := b.loading[name]; ok {
		return false, nil
	}

	// Создаём канал, который будет закрыт после завершения reload
	b.loading[name] = make(chan struct{})
	b.claimMetaLockedR303(name, requester)
	return true, nil
}

// UnlockLoad освобождает блокировку загрузки для модели name.
// Должен вызываться после TryLockLoad, когда загрузка завершена (успех или ошибка).
func (b *Backend) UnlockLoad(name string) {
	b.loadMu.Lock()
	defer b.loadMu.Unlock()
	if ch, ok := b.loading[name]; ok {
		close(ch) // оповещаем всех ожидающих
		delete(b.loading, name)
	}
	// R303: метаданные заявки живут ровно столько же, сколько сама заявка.
	delete(b.loadClaims, name)
}

// waitForLoad блокируется до завершения загрузки модели другой горутиной.
// Возвращает true если модель успешно загружена, false если произошла ошибка
// или канал закрыт по другой причине.
// WaitForLoad — блокируется до завершения загрузки модели другой горутиной.
// Используется когда tryLockLoad вернул (false, nil) — другая горутина уже грузит.
//
// Round 9 (2026-07-28) BUGFIX: если b.loading[name] уже удалён (канал
// закрыт и cleaned-up), ПРОВЕРЯЕМ b.models[name] перед возвратом false.
// До фикса был race: goroutine A загрузила модель, вызвала UnlockLoad
// (закрыла канал, удалила из map) — goroutine B видела `!ok` и сразу
// возвращала false, даже если модель УЖЕ в b.models. Caller (lazyload.go
// и 3 handler'а) интерпретировал false как "модель не загружена" и
// возвращал 503 errModelIsLoading.
//
// Корректная семантика: возвращаем true если модель загружена (любым
// способом), false только если загрузка точно провалилась.
func (b *Backend) WaitForLoad(name string) bool {
	b.loadMu.Lock()
	ch, ok := b.loading[name]
	b.loadMu.Unlock()

	if ok {
		// Другая горутина ещё грузит — ждём канал.
		<-ch
		// После пробуждения канал закрыт, b.loading[name] уже удалён
		// (UnlockLoad делает close+delete под loadMu). Fallthrough к
		// проверке b.models.
	}

	// Round 9: проверяем b.models — модель может быть уже загружена
	// (другая горутина завершила load между TryLockLoad и нашим чтением
	// канала, или мы пришли сюда после того как b.loading[name] уже
	// удалён).
	if _, err := b.GetModel(name); err == nil {
		return true
	}

	// Модель не в b.models → загрузка не удалась (или её никогда не было
	// через TryLockLoad, что не наш случай).
	return false
}

// ============================================================
// Инференс
// ============================================================
