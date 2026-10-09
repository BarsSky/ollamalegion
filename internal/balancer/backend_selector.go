package balancer

import (
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// getAllowedTypesList преобразует одиночный BackendType в список.
// Если bt пустой — возвращает типы из OperatingMode (обратная совместимость).
func (p *Proxy) getAllowedTypesList(bt types.BackendType) []types.BackendType {
	if bt != "" {
		return []types.BackendType{bt}
	}
	return p.getDefaultAllowedTypes()
}

// getDefaultAllowedTypes возвращает допустимые типы бэкендов на основе OperatingMode.
// Используется когда явный BackendType не передан (обратная совместимость).
//
// R-Image (2026-09-27): image_cpp СОЗНАТЕЛЬНО исключён из списка «по умолчанию».
//
// Инвариант: image-бэкенд достижим ТОЛЬКО по явному признаку запроса
// (endpoint /v1/images/*, /sdapi/v1/*, /api/image/* или префикс модели sd:),
// который всегда даёт bt == BackendTypeImage → getAllowedTypesList вернёт
// ровно {image_cpp}. Если же тип не определён (bt == "", типичный случай
// текстового /api/generate в смешанном режиме), в списке допустимых не должно
// быть image: иначе текстовый запрос мог бы уйти на image-бэкенд (например,
// если тот единственный здоровый или получил лучший score по ресурсам).
func (p *Proxy) getDefaultAllowedTypes() []types.BackendType {
	allowed, ok := types.ModeBackendTypes[p.config.Balancing.OperatingMode]
	if !ok || len(allowed) == 0 {
		// Неизвестный режим — разрешаем оба текстовых типа для обратной совместимости
		return []types.BackendType{types.BackendTypeOllama, types.BackendTypeLlamaCpp}
	}
	textTypes := make([]types.BackendType, 0, len(allowed))
	for _, t := range allowed {
		if t == types.BackendTypeImage {
			continue
		}
		textTypes = append(textTypes, t)
	}
	if len(textTypes) == 0 {
		// Режим, в котором вообще нет текстовых типов (на будущее) — не
		// превращаем список в пустой: пустой allowedTypes означает «разрешено всё».
		return []types.BackendType{types.BackendTypeImage}
	}
	return textTypes
}

// isBackendTypeAllowed проверяет, разрешён ли тип бэкенда в списке allowedTypes.
// Если allowedTypes пустой — разрешены все типы (обратная совместимость).
func isBackendTypeAllowed(bt types.BackendType, allowedTypes []types.BackendType) bool {
	if len(allowedTypes) == 0 {
		return true
	}
	for _, at := range allowedTypes {
		if bt == at {
			return true
		}
	}
	return false
}

// imageGateBlocksTextBackend — R-Image Phase 6 (2026-10-02): не выбирать
// ТЕКСТОВЫЙ бэкенд на GPU, где image-генерация держит эксклюзивный лок.
//
// ПОЧЕМУ ИМЕННО ЗДЕСЬ И ПОЧЕМУ ТАК. Политика exclusive (дефолт) означает «во
// время генерации карта принадлежит image». Реализовано это ДВУМЯ частями:
//  1. image-запросы берут лок на время генерации (image_resources.go);
//  2. текстовый КАНДИДАТ на занятой карте просто не выбирается — минимальный
//     хук в функциях выбора, без блокирующих примитивов.
//
// ЧЕГО ХУК НЕ ДЕЛАЕТ: не держит текстовый запрос в глубине (это опасно: лок под
// p.mu/селектором = риск дедлока и остановки всего трафика). Если подходящих
// кандидатов не осталось, запрос уходит в существующую admission-очередь
// (unified_queue_r73.go) и ждёт освобождения слота там; по истечении лимита
// клиент получает 503 + Retry-After.
//
// ЛИМИТ ЭТОГО ОЖИДАНИЯ (Phase 6 follow-up, 2026-10-02): когда единственная
// причина отсутствия кандидата — именно image-лок, ожидание ограничивается
// balancing.image.queueWaitTimeoutSec, а не LB_ADMISSION_WAIT_SEC (см.
// imageLockWaitCap). Для остальных причин (нет здоровых бэкендов, нет модели,
// обычная перегрузка слотов) поведение не меняется.
//
// image-бэкенды хук не трогает: для них всегда false (иначе exclusive
// заблокировал бы саму генерацию).
func (p *Proxy) imageGateBlocksTextBackend(state *BackendState) bool {
	if !p.imageLockBlocksTextBackend(state) {
		return false
	}
	p.imageRes.noteTextSlotWithheld()
	return true
}

// imageLockBlocksTextBackend — тот же предикат, но БЕЗ счётчика.
//
// ЗАЧЕМ РАЗДЕЛЕНИЕ: детектор «единственная причина — image-лок» (см.
// imageLockWaitCap) обязан спрашивать ровно то же условие, но он не «пропускает
// кандидата» — считать его в text_slots_withheld_total значило бы надувать
// метрику выбора несуществующими событиями.
func (p *Proxy) imageLockBlocksTextBackend(state *BackendState) bool {
	if p == nil || state == nil || state.Backend == nil {
		return false
	}
	res := p.imageRes
	if res == nil {
		return false
	}
	if normalizeBackendType(state.Backend.Type) == types.BackendTypeImage {
		return false
	}
	if state.Backend.Status == types.StatusOffline {
		return false
	}
	// GPU-индекс: (host, индекс) обеих сторон. Если индекс неизвестен ХОТЯ БЫ у
	// одной из сторон (в том числе при nil у текстового бэкенда — именно так
	// выглядит «неизвестно» после R-Image follow-up), gpuLockHeldFor блокирует
	// весь хост — консервативно, см. locksConflict.
	idx, known := state.Backend.EffectiveGPUIndex()
	return res.gpuLockHeldFor(state.Backend.Host, gpuRef{index: idx, known: known})
}

// imageLockWaitCap — предел ожидания слота для текстового запроса, когда
// единственная причина отсутствия кандидата — активный image-лок.
//
// ЧТО ЗНАЧИТ «ЕДИНСТВЕННАЯ ПРИЧИНА» (и почему проверок ровно столько):
//   - у модели есть кандидаты (expandCandidates — тот же отбор, что в
//     selectBackend: healthy, известная/загружаемая модель, живые лимиты);
//   - НИ ОДНОГО кандидата в группах P1 (модель загружена) и P2 (модель
//     загружается) — эти две группы селектор отдаёт, НЕ применяя хук Phase 6,
//     значит их наличие означало бы, что кандидат есть и без лока, то есть
//     причина не в нём;
//   - у КАЖДОГО оставшегося кандидата ЕСТЬ свободный слот (иначе причина
//     ожидания — ёмкость, то есть обычная перегрузка: лок лишь совпал с ней);
//   - КАЖДЫЙ оставшийся кандидат заблокирован image-локом.
//
// Возвращает (лимит, true), только если все условия выполнены.
func (p *Proxy) imageLockWaitCap(model string, bt types.BackendType) (time.Duration, bool) {
	if p == nil || p.imageRes == nil {
		return 0, false
	}
	candidates := p.expandCandidates(model, p.getAllowedTypesList(bt))
	if len(candidates) == 0 {
		return 0, false
	}
	blocked := 0
	for _, group := range candidates {
		if group.Priority == 1 || group.Priority == 2 {
			return 0, false
		}
		for _, backendID := range group.BackendIDs {
			state := p.backends[backendID]
			if state == nil || !p.imageLockBlocksTextBackend(state) {
				return 0, false
			}
			if !p.backendHasFreeSlot(state) {
				return 0, false
			}
			blocked++
		}
	}
	if blocked == 0 {
		return 0, false
	}
	wait := time.Duration(p.imageRes.settings().EffectiveQueueWaitTimeout()) * time.Second
	if wait <= 0 {
		return 0, false
	}
	return wait, true
}

// backendHasFreeSlot — есть ли на бэкенде свободный слот.
//
// Правило то же, что у tryAcquireSlot: EffectiveMaxConcurrentRequests против
// ActiveReqs (<= 0 = лимита нет → слот есть всегда).
func (p *Proxy) backendHasFreeSlot(state *BackendState) bool {
	if state == nil || state.Backend == nil {
		return false
	}
	state.mu.Lock()
	active := state.ActiveReqs
	state.mu.Unlock()
	maxReqs := state.Backend.EffectiveMaxConcurrentRequests()
	return maxReqs <= 0 || active < maxReqs
}

// selectBackend - выбор бэкенда для запроса (pre-step + 4 этапа = 5 шагов)
// bt — требуемый тип бэкенда (если пустой — определяется из OperatingMode)
// skipSyncLoad (variadic bool) — если true, пропускает P3 (Sync Model Load / warmup).
// Используется для read/mgmt endpoints (/api/show, /api/pull, /api/copy, ...),
// которым НЕ нужна загруженная модель — иначе balancer зависает на 10-30s
// timeout ожидая load (Round 22 BUG #1, 2026-08-03).
func (p *Proxy) selectBackend(model string, bt types.BackendType, skipSyncLoad ...bool) string {
	skipWarmup := false
	if len(skipSyncLoad) > 0 {
		skipWarmup = skipSyncLoad[0]
	}
	// RPC Module: Model Replication — если модель в группе репликации, выбираем из реплик
	if p.replicationSelector != nil {
		if selected := p.replicationSelector.Select(model); selected != "" {
			logger.Get().Infow("routed through model replication",
				"model", model, "backend", selected)
			return selected
		}
	}

	allowedTypes := p.getAllowedTypesList(bt)

	candidates := p.expandCandidates(model, allowedTypes)

	// 1. Model Affinity (LOADED) — P1
	// Выбираем лучший бэкенд по score среди loaded-кандидатов с loadRatio < threshold
	for _, group := range candidates {
		if group.Priority != 1 {
			break
		}
		var bestBackendID string
		var bestScore float64 = -1
		for _, backendID := range group.BackendIDs {
			state := p.backends[backendID]
			state.mu.Lock()
			active := state.ActiveReqs
			maxReqs := state.Backend.MaxConcurrentReqs
			state.mu.Unlock()

			loadRatio := 0.0
			if maxReqs > 0 {
				loadRatio = float64(active) / float64(maxReqs)
			}
			threshold := p.config.Balancing.Prewarm.TriggerLoadThreshold
			if threshold <= 0 {
				threshold = 0.80
			}
			if loadRatio >= threshold {
				continue
			}

			score := p.calculateScore(backendID)
			if score > bestScore {
				bestScore = score
				bestBackendID = backendID
			}
		}
		if bestBackendID != "" {
			p.queueMgr.addDispatchAffinity()
			return bestBackendID
		}
	}

	// 2. Model Warming (WARMING_UP) — P2
	for _, group := range candidates {
		if group.Priority != 2 {
			continue
		}
		for _, backendID := range group.BackendIDs {
			state := p.backends[backendID]
			state.mu.Lock()
			ws, exists := state.WarmingUpModels[model]
			state.mu.Unlock()

			if !exists {
				continue
			}
			eta := time.Until(ws.EstimatedReadyAt)
			syncTimeout := p.getModelLoadTimeout()
			if eta > 0 && eta < syncTimeout {
				start := time.Now()
				for time.Since(start) < syncTimeout {
					if p.checkModelReadyUnsafe(backendID, model) {
						p.queueMgr.addDispatchAffinity()
						return backendID
					}
					time.Sleep(500 * time.Millisecond)
				}
				logger.Get().Warnw("model warmup timeout", "backend", backendID, "model", model)
			}
		}
	}

	// 3. Sync Model Load (запуск загрузки) — P3
	// Round 22 (2026-08-03): skip для read/mgmt endpoints — они НЕ требуют
	// загруженной модели. Без skip balancer зависает на 10-30s timeout
	// (warmup инициирует POST /load к cppworker, который для /api/show
	// бесполезен — endpoint работает с файлом на диске).
	if !skipWarmup && p.config.Balancing.SyncModelLoad.Enabled {
		for _, group := range candidates {
			if group.Priority != 3 {
				continue
			}
			for _, backendID := range group.BackendIDs {
				if !p.canAcceptRequest(backendID) {
					continue
				}
				state := p.backends[backendID]
				if state == nil {
					continue
				}
				p.warmupModel(backendID, state.Backend.Host, state.Backend.OllamaPort, model)
				syncTimeout := p.getModelLoadTimeout()
				start := time.Now()
				for time.Since(start) < syncTimeout {
					if p.checkModelReadyUnsafe(backendID, model) {
						p.queueMgr.addDispatchAffinity()
						return backendID
					}
					time.Sleep(500 * time.Millisecond)
				}
				logger.Get().Warnw("sync model load timeout", "backend", backendID, "model", model)
			}
		}
	}

	// 4. selectByResources (scoring v2) — P4 (FALLBACK)
	return p.selectByResources(allowedTypes)
}

// selectBackendExcluding - выбор бэкенда, исключая указанные
func (p *Proxy) selectBackendExcluding(model string, exclude map[string]bool, bt types.BackendType) string {
	allowedTypes := p.getAllowedTypesList(bt)

	p.mu.RLock()

	if p.config.Balancing.ModelAffinity && model != "" {
		if backend := p.findBackendWithModelExcluding(model, exclude, allowedTypes); backend != "" {
			p.mu.RUnlock()
			return backend
		}
	}
	p.mu.RUnlock()

	backend := p.selectByResourcesExcluding(exclude, allowedTypes)
	if backend == "" {
		logger.Get().Warnw("no available backends with exclusions")
	}
	return backend
}

// findBackendWithModelExcluding - поиск бэкенда с моделью, исключая указанные
func (p *Proxy) findBackendWithModelExcluding(modelName string, exclude map[string]bool, allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestScore float64 = -1

	for id, state := range p.backends {
		if exclude[id] {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		if !p.checkResourceLimits(id) {
			continue
		}

		metrics, hasMetrics := p.metricsMgr.SnapshotBackendMetrics(id)
		if !hasMetrics {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		// Согласно документации: model affinity только при loadRatio < threshold
		threshold := p.config.Balancing.Prewarm.TriggerLoadThreshold
		if threshold <= 0 {
			threshold = 0.80
		}
		if maxReqs > 0 && float64(active)/float64(maxReqs) >= threshold {
			continue
		}

		pred := state.Prediction
		if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
			continue
		}

		if !p.backendHasModel(metrics, modelName) {
			continue
		}

		score := p.calculateScore(id)
		if score > bestScore {
			bestScore = score
			bestBackendID = id
		}
	}

	return bestBackendID
}

// modelIsRunningOnBackendUnsafe — проверяет, запущена ли модель на бэкенде (без блокировки)
func (p *Proxy) modelIsRunningOnBackendUnsafe(backendID, modelName string) bool {
	if modelName == "" {
		return false
	}
	metrics, ok := p.metricsMgr.SnapshotBackendMetrics(backendID)
	if !ok {
		return false
	}
	return p.backendHasModel(metrics, modelName)
}

// selectByResourcesExcluding - выбор по ресурсам с исключением бэкендов
func (p *Proxy) selectByResourcesExcluding(exclude map[string]bool, allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackend string
	var bestScore float64 = -1

	for id, state := range p.backends {
		if exclude[id] {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		// Phase 6: хост занят image-генерацией (exclusive) — текстовый кандидат
		// не подходит, ждём освобождения в admission-очереди.
		if p.imageGateBlocksTextBackend(state) {
			continue
		}

		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		if state.ActiveReqs >= state.Backend.MaxConcurrentReqs {
			state.mu.Unlock()
			continue
		}
		state.mu.Unlock()

		score := p.calculateScore(id)
		if score > bestScore {
			bestScore = score
			bestBackend = id
		}
	}

	if bestBackend != "" {
		if p.config.Balancing.UseEnhancedScoring {
			p.queueMgr.addDispatchLoad()
		} else {
			p.queueMgr.addDispatchConfig()
		}
	}

	return bestBackend
}

// findLeastLoadedBackendWithModel — наименее загруженный узел, на котором модель
// УЖЕ загружена (R91, 2026-10-08).
//
// ЧЕМ ОТЛИЧАЕТСЯ ОТ findLessLoadedBackendWithModel. Тот тоже ищет минимум
// loadRatio, но при РАВНОЙ загрузке побеждает первый, попавшийся при обходе
// КАРТЫ p.backends — а порядок обхода карты в Go случаен от вызова к вызову. Для
// переезда «от перегруженного узла» это неважно, а для выбора узла под каждый
// новый запрос — важно: клиент прыгал бы между двумя свободными узлами
// (проверено тестом: два вызова подряд дали bk-a и bk-b). Здесь tie-break
// детерминированный: выше score, затем меньший id — как в findLessLoadedBackendAny.
func (p *Proxy) findLeastLoadedBackendWithModel(modelName string, allowedTypes []types.BackendType) string {
	if p == nil || modelName == "" {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestLoadRatio float64 = 2.0
	var bestScore float64 = 0

	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if maxReqs <= 0 {
			continue
		}
		metrics, hasMetrics := p.metricsMgr.SnapshotBackendMetrics(id)
		if !hasMetrics || !p.backendHasModel(metrics, modelName) {
			continue
		}
		loadRatio := float64(active) / float64(maxReqs)
		if bestBackendID == "" || loadRatio < bestLoadRatio {
			bestBackendID, bestLoadRatio, bestScore = id, loadRatio, p.calculateScore(id)
			continue
		}
		if loadRatio == bestLoadRatio {
			score := p.calculateScore(id)
			if score > bestScore || (score == bestScore && id < bestBackendID) {
				bestBackendID, bestScore = id, score
			}
		}
	}
	return bestBackendID
}

// findLessLoadedBackendWithModel — поиск менее загруженного бэкенда с той же моделью
func (p *Proxy) findLessLoadedBackendWithModel(modelName, excludeBackendID string, allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestLoadRatio float64 = 2.0

	for id, state := range p.backends {
		if id == excludeBackendID {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		if maxReqs <= 0 {
			continue
		}

		metrics, hasMetrics := p.metricsMgr.SnapshotBackendMetrics(id)
		if !hasMetrics {
			continue
		}

		if !p.backendHasModel(metrics, modelName) {
			continue
		}

		loadRatio := float64(active) / float64(maxReqs)
		if loadRatio < bestLoadRatio {
			bestLoadRatio = loadRatio
			bestBackendID = id
		}
	}

	return bestBackendID
}

// findLessLoadedBackendAny — поиск любого менее загруженного healthy бэкенда.
// При равном loadRatio выбирает бэкенд с лучшим score (deterministic).
// Если модель не загружена на выбранном бэкенде — запускает асинхронный warmup.
func (p *Proxy) findLessLoadedBackendAny(modelName, excludeBackendID string, allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestLoadRatio float64 = 2.0
	var bestScore float64 = -1

	for id, state := range p.backends {
		if id == excludeBackendID {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		if maxReqs <= 0 {
			continue
		}

		loadRatio := float64(active) / float64(maxReqs)

		// При равном loadRatio используем score для deterministic выбора
		if loadRatio < bestLoadRatio || (loadRatio == bestLoadRatio && bestScore < 0) {
			bestLoadRatio = loadRatio
			bestBackendID = id
			bestScore = p.calculateScore(id)
		} else if loadRatio == bestLoadRatio {
			score := p.calculateScore(id)
			if score > bestScore {
				bestScore = score
				bestBackendID = id
			}
		}
	}

	// Если нашли бэкенд без модели — запускаем warmup асинхронно
	if bestBackendID != "" && modelName != "" {
		state := p.backends[bestBackendID]
		if state != nil {
			metrics, hasMetrics := p.metricsMgr.SnapshotBackendMetrics(bestBackendID)
			if hasMetrics && !p.backendHasModel(metrics, modelName) {
				p.warmupModel(bestBackendID, state.Backend.Host, state.Backend.OllamaPort, modelName)
				logger.Get().Infow("rebalance: triggering model warmup on new backend",
					"backend", bestBackendID, "model", modelName)
			}
		}
	}

	return bestBackendID
}

// backendHasModel проверяет, загружена ли модель на бэкенде (Ollama или llama.cpp)
func (p *Proxy) backendHasModel(metrics *types.BackendMetrics, modelName string) bool {
	// Проверяем Ollama модели
	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == modelName || strings.Contains(m.Name, modelName) {
			return true
		}
	}
	// Проверяем llama.cpp модели
	for _, m := range metrics.LlamaCpp.LoadedModels {
		if m.Name == modelName || strings.Contains(m.Name, modelName) {
			return true
		}
	}
	return false
}

// backendHasModelStrict — строгая проверка (без Contains), для prewarm
func (p *Proxy) backendHasModelStrict(metrics *types.BackendMetrics, modelName string) bool {
	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == modelName {
			return true
		}
	}
	for _, m := range metrics.LlamaCpp.LoadedModels {
		if m.Name == modelName {
			return true
		}
	}
	return false
}

// findBackendWithModel - поиск бэкенда с загруженной моделью
func (p *Proxy) findBackendWithModel(modelName string, allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestScore float64 = -1

	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		// Согласно документации: model affinity только при loadRatio < threshold
		threshold := p.config.Balancing.Prewarm.TriggerLoadThreshold
		if threshold <= 0 {
			threshold = 0.80
		}
		if maxReqs > 0 && float64(active)/float64(maxReqs) >= threshold {
			continue
		}

		pred := state.Prediction
		if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
			continue
		}

		metrics, hasMetrics := p.metricsMgr.SnapshotBackendMetrics(id)

		if !hasMetrics {
			continue
		}

		if !p.backendHasModel(metrics, modelName) {
			continue
		}

		score := p.calculateScore(id)
		if score > bestScore {
			bestScore = score
			bestBackendID = id
		}
	}

	return bestBackendID
}

// findWarmingBackendForModelUnsafe — поиск WARMING_UP бэкенда (без блокировки, вызывается под p.mu.RLock)
func (p *Proxy) findWarmingBackendForModelUnsafe(model string) string {
	for id, state := range p.backends {
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		state.mu.Lock()
		ws, exists := state.WarmingUpModels[model]
		state.mu.Unlock()
		if exists && ws != nil && time.Now().Before(ws.EstimatedReadyAt) {
			return id
		}
	}
	return ""
}

// findFreeBackendForModelUnsafe — свободный бэкенд с достаточным VRAM (без блокировки)
func (p *Proxy) findFreeBackendForModelUnsafe(model string, allowedTypes []types.BackendType) string {
	var best string
	var maxFree uint64
	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if active > 0 || (maxReqs > 0 && active >= maxReqs) {
			continue
		}
		metrics, ok := p.metricsMgr.SnapshotBackendMetrics(id)
		if !ok || metrics.GPU.MemoryTotal == 0 {
			continue
		}
		freeVRAM := metrics.GPU.MemoryTotal - metrics.GPU.MemoryUsed
		needed := estimateModelVRAM(model)
		if freeVRAM <= needed {
			continue
		}
		if !p.backendHasModel(metrics, model) && freeVRAM > maxFree {
			maxFree = freeVRAM
			best = id
		}
	}
	return best
}

// checkModelReadyUnsafe — готова ли модель на бэкенде (без блокировки)
func (p *Proxy) checkModelReadyUnsafe(backendID, model string) bool {
	metrics, ok := p.metricsMgr.SnapshotBackendMetrics(backendID)
	if !ok {
		return false
	}
	return p.backendHasModel(metrics, model)
}

// selectFreeBackendAny — выбор любого свободного healthy бэкенда с наименьшей загрузкой
func (p *Proxy) selectFreeBackendAny(allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackend string
	var bestLoadRatio float64 = 2.0

	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		if maxReqs >= 0 && active >= maxReqs {
			continue
		}

		// Phase 6: см. imageGateBlocksTextBackend — на хосте идёт image-генерация.
		if p.imageGateBlocksTextBackend(state) {
			continue
		}

		loadRatio := 0.0
		if maxReqs > 0 {
			loadRatio = float64(active) / float64(maxReqs)
		}

		if loadRatio < bestLoadRatio {
			bestLoadRatio = loadRatio
			bestBackend = id
		}
	}

	if bestBackend != "" {
		logger.Get().Infow("selectFreeBackendAny: selected", "backend", bestBackend, "load_ratio", bestLoadRatio)
		p.queueMgr.addDispatchLoad()
	}
	return bestBackend
}

// selectByResources - выбор бэкенда по ресурсам
func (p *Proxy) selectByResources(allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackend string
	var bestScore float64 = -1
	var bestLoadRatio float64 = 2.0
	var bestLastUsed time.Time

	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		if !p.checkResourceLimits(id) {
			continue
		}

		// Phase 6: см. imageGateBlocksTextBackend — на хосте идёт image-генерация.
		if p.imageGateBlocksTextBackend(state) {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		lastUsed := state.LastUsed
		state.mu.Unlock()

		// Не отсеиваем бэкенд если слоты заняты — пусть tryAcquireSlot в dispatchRequest
		// решает можно ли захватить. Если слот занят — dispatch вернёт ErrNoBackendAvailable
		// и queue requeue'ит запрос, дожидаясь освобождения.
		pred := state.Prediction
		if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
			continue
		}

		score := p.calculateScore(id)

		loadRatio := 0.0
		if maxReqs > 0 {
			loadRatio = float64(active) / float64(maxReqs)
		}

		if score > bestScore ||
			(score == bestScore && loadRatio < bestLoadRatio) ||
			(score == bestScore && loadRatio == bestLoadRatio && lastUsed.Before(bestLastUsed)) {
			bestScore = score
			bestBackend = id
			bestLoadRatio = loadRatio
			bestLastUsed = lastUsed
		}
	}

	if bestBackend != "" {
		if state, ok := p.backends[bestBackend]; ok {
			state.mu.Lock()
			state.LastUsed = time.Now()
			state.mu.Unlock()
		}
		if p.config.Balancing.UseEnhancedScoring {
			p.queueMgr.addDispatchLoad()
		} else {
			p.queueMgr.addDispatchConfig()
		}
	}

	return bestBackend
}
