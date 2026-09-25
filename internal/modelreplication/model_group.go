// Package modelreplication — Вариант A: Model Replication Manager
// Реализует репликацию модели на несколько бэкендов с управлением группами.
package modelreplication

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ModelGroupManager управляет группами репликации моделей.
// Каждая группа — это одна модель, загруженная на N бэкендов.
type ModelGroupManager struct {
	mu      sync.RWMutex
	groups  map[string]*ModelGroup // modelName → group
	enabled bool

	// Callbacks для взаимодействия с Balancer
	loadFn    func(backendID string) float64                    // получение загрузки бэкенда (0.0-1.0)
	backendFn func(modelName string, targets []string) []string // поиск свободных бэкендов
	warmupFn  func(backendID, modelName string) error           // запуск warmup модели
	// R80: реальный список бэкендов, на которых модель УЖЕ загружена
	// (метрики cppworker/агента). Нужен, чтобы группа усыновляла копии,
	// загруженные вне менеджера (до рестарта балансера, авто-загрузкой по
	// запросу, через WebUI) и не плодила лишние загрузки.
	loadedFn func(modelName string) []string
}

// loadedGracePeriod — сколько ждать подтверждения моделью в метриках, прежде
// чем считать инстанс протухшим. Должен превышать интервал опроса метрик
// (cppworker-поллер 30 с, агент 10 с), но быть достаточно коротким, чтобы
// восстановление реплики после падения/рестарта бэкенда не занимало минуты.
const loadedGracePeriod = 60 * time.Second

// loadingTimeout — сколько инстанс может висеть в LOADING, прежде чем его
// состояние считается протухшим (cppworker упал/загрузка не состоялась).
const loadingTimeout = 15 * time.Minute

// ModelGroup представляет группу реплик одной модели.
type ModelGroup struct {
	Config    types.ModelGroupConfig
	Instances map[string]*types.ModelInstanceState // backendID → state
	mu        sync.RWMutex
}

// NewModelGroupManager создаёт новый менеджер групп моделей.
func NewModelGroupManager() *ModelGroupManager {
	return &ModelGroupManager{
		groups:  make(map[string]*ModelGroup),
		enabled: false,
	}
}

// --- Callbacks ---

// SetBackendLoadFn устанавливает callback для получения загрузки бэкенда.
func (mgr *ModelGroupManager) SetBackendLoadFn(fn func(string) float64) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.loadFn = fn
}

// SetFreeBackendFn устанавливает callback для поиска свободных бэкендов.
func (mgr *ModelGroupManager) SetFreeBackendFn(fn func(string, []string) []string) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.backendFn = fn
}

// SetWarmupFn устанавливает callback для запуска warmup модели.
func (mgr *ModelGroupManager) SetWarmupFn(fn func(string, string) error) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.warmupFn = fn
}

// SetLoadedBackendsFn — R80: callback «на каких бэкендах модель реально
// загружена прямо сейчас» (по метрикам cppworker/агента).
//
// Без него менеджер знает только про копии, которые загрузил сам: после
// рестарта балансера группа оставалась пустой (0 инстансов) при двух реально
// загруженных копиях, а контроллер группы грузил лишнюю копию на «свободный»
// бэкенд, хотя копии уже были готовы.
func (mgr *ModelGroupManager) SetLoadedBackendsFn(fn func(string) []string) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.loadedFn = fn
}

// --- Lifecycle ---

// Start запускает менеджер (фоновые задачи).
func (mgr *ModelGroupManager) Start() error {
	logger.Get().Infow("model replication manager started")
	return nil
}

// Stop останавливает менеджер.
func (mgr *ModelGroupManager) Stop() error {
	logger.Get().Infow("model replication manager stopped")
	return nil
}

// IsEnabled возвращает статус включения.
func (mgr *ModelGroupManager) IsEnabled() bool {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	return mgr.enabled
}

// SetEnabled включает/выключает менеджер.
func (mgr *ModelGroupManager) SetEnabled(enabled bool) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.enabled = enabled
}

// --- CRUD ---

// GetGroups возвращает список конфигов всех групп.
func (mgr *ModelGroupManager) GetGroups() []types.ModelGroupConfig {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	result := make([]types.ModelGroupConfig, 0, len(mgr.groups))
	for _, g := range mgr.groups {
		g.mu.RLock()
		result = append(result, g.Config)
		g.mu.RUnlock()
	}
	return result
}

// GetGroup возвращает конфигурацию группы по имени модели.
func (mgr *ModelGroupManager) GetGroup(modelName string) *types.ModelGroupConfig {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	if g, ok := mgr.groups[modelName]; ok {
		g.mu.RLock()
		defer g.mu.RUnlock()
		cfg := g.Config
		return &cfg
	}
	return nil
}

// CreateGroup создаёт новую группу репликации с валидацией.
func (mgr *ModelGroupManager) CreateGroup(cfg types.ModelGroupConfig) error {
	if cfg.ModelName == "" {
		return errors.New("modelName is required")
	}
	if cfg.MinInstances <= 0 {
		return errors.New("minInstances must be > 0")
	}
	if cfg.MaxInstances < cfg.MinInstances {
		return errors.New("maxInstances must be >= minInstances")
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	if _, exists := mgr.groups[cfg.ModelName]; exists {
		return fmt.Errorf("group for model '%s' already exists", cfg.ModelName)
	}

	if cfg.IdleUnloadAfter == "" {
		cfg.IdleUnloadAfter = "15m"
	}

	mgr.groups[cfg.ModelName] = &ModelGroup{
		Config:    cfg,
		Instances: make(map[string]*types.ModelInstanceState),
	}

	logger.Get().Infow("model replication group created",
		"model", cfg.ModelName,
		"min", cfg.MinInstances,
		"max", cfg.MaxInstances,
		"targetBackends", cfg.TargetBackends,
		"idleUnloadAfter", cfg.IdleUnloadAfter)
	return nil
}

// DeleteGroup удаляет группу репликации.
func (mgr *ModelGroupManager) DeleteGroup(modelName string) error {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if _, exists := mgr.groups[modelName]; !exists {
		return fmt.Errorf("group for model '%s' not found", modelName)
	}
	delete(mgr.groups, modelName)
	logger.Get().Infow("model replication group deleted", "model", modelName)
	return nil
}

// UpdateGroup обновляет конфигурацию группы.
func (mgr *ModelGroupManager) UpdateGroup(cfg types.ModelGroupConfig) error {
	if cfg.ModelName == "" {
		return errors.New("modelName is required")
	}
	if cfg.MinInstances <= 0 {
		return errors.New("minInstances must be > 0")
	}
	if cfg.MaxInstances < cfg.MinInstances {
		return errors.New("maxInstances must be >= minInstances")
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	g, exists := mgr.groups[cfg.ModelName]
	if !exists {
		return fmt.Errorf("group for model '%s' not found", cfg.ModelName)
	}

	g.mu.Lock()
	g.Config = cfg
	// Очищаем инстансы не из targetBackends (если targetBackends заданы)
	if len(cfg.TargetBackends) > 0 {
		targetMap := make(map[string]bool, len(cfg.TargetBackends))
		for _, id := range cfg.TargetBackends {
			targetMap[id] = true
		}
		for id := range g.Instances {
			if !targetMap[id] {
				delete(g.Instances, id)
			}
		}
	}
	g.mu.Unlock()

	logger.Get().Infow("model replication group updated",
		"model", cfg.ModelName,
		"min", cfg.MinInstances,
		"max", cfg.MaxInstances)
	return nil
}

// --- State ---

// GetInstanceStates возвращает состояние инстансов для группы.
//
// ВНИМАНИЕ: возвращаются УКАЗАТЕЛИ на объекты группы — читать/менять их поля
// вне блокировки g.mu нельзя (гонка с warmup-горутинами scaleUpGroup). Для
// чтения «снаружи» используйте GetInstanceStatesSnapshot.
func (mgr *ModelGroupManager) GetInstanceStates(modelName string) []*types.ModelInstanceState {
	mgr.mu.RLock()
	g, ok := mgr.groups[modelName]
	mgr.mu.RUnlock()
	if !ok {
		return nil
	}

	g.mu.RLock()
	defer g.mu.RUnlock()
	result := make([]*types.ModelInstanceState, 0, len(g.Instances))
	for _, inst := range g.Instances {
		result = append(result, inst)
	}
	return result
}

// GetInstanceStatesSnapshot — R74: копии состояний инстансов под блокировкой
// группы. Возвращённые значения можно безопасно читать после возврата (гонки с
// warmup-горутинами нет): именно так читают состояние селектор и API.
func (mgr *ModelGroupManager) GetInstanceStatesSnapshot(modelName string) []types.ModelInstanceState {
	mgr.mu.RLock()
	g, ok := mgr.groups[modelName]
	mgr.mu.RUnlock()
	if !ok {
		return nil
	}

	g.mu.RLock()
	defer g.mu.RUnlock()
	result := make([]types.ModelInstanceState, 0, len(g.Instances))
	for _, inst := range g.Instances {
		if inst == nil {
			continue
		}
		result = append(result, *inst)
	}
	return result
}

// GetGroupStats возвращает расширенную статистику для группы.
//
// R74: состояния читаются через снимок (см. GetInstanceStatesSnapshot) — так
// вызовы из API/placement не гоняют с warmup-горутинами.
func (mgr *ModelGroupManager) GetGroupStats(modelName string) map[string]interface{} {
	states := mgr.GetInstanceStatesSnapshot(modelName)
	if states == nil {
		return nil
	}

	loaded, loading, idle := 0, 0, 0
	var totalUseCount int64
	for _, s := range states {
		switch s.Status {
		case types.ModelStateLoaded:
			loaded++
		case types.ModelStateLoading, types.ModelStateWarmingUp:
			loading++
		default:
			idle++
		}
		totalUseCount += s.UseCount
	}

	mgr.mu.RLock()
	g, exists := mgr.groups[modelName]
	mgr.mu.RUnlock()

	var minInst, maxInst int
	if exists {
		g.mu.RLock()
		minInst = g.Config.MinInstances
		maxInst = g.Config.MaxInstances
		g.mu.RUnlock()
	}

	return map[string]interface{}{
		"modelName":    modelName,
		"total":        len(states),
		"loaded":       loaded,
		"loading":      loading,
		"idle":         idle,
		"totalUses":    totalUseCount,
		"minInstances": minInst,
		"maxInstances": maxInst,
	}
}

// --- Reconciliation ---

// EnsureInstances проверяет и корректирует количество инстансов для всех групп.
func (mgr *ModelGroupManager) EnsureInstances() {
	if !mgr.IsEnabled() {
		return
	}

	mgr.mu.RLock()
	groups := make([]*ModelGroup, 0, len(mgr.groups))
	for _, g := range mgr.groups {
		groups = append(groups, g)
	}
	mgr.mu.RUnlock()

	for _, g := range groups {
		mgr.ensureGroupInstances(g)
	}
}

// ensureGroupInstances проверяет конкретную группу.
func (mgr *ModelGroupManager) ensureGroupInstances(g *ModelGroup) {
	// R80: сначала сверяем состав инстансов с реальностью (метрики бэкендов) —
	// усыновляем уже загруженные копии и убираем протухшие.
	mgr.syncLoadedInstances(g)

	g.mu.RLock()
	cfg := g.Config
	instanceCount := len(g.Instances)
	loaded := 0
	inFlight := 0
	now := time.Now()
	for _, inst := range g.Instances {
		switch inst.Status {
		case types.ModelStateLoaded:
			loaded++
		case types.ModelStateLoading:
			// R80: загрузка запрошена, но модель ещё не подтверждена метриками.
			// Считаем её «в пути», иначе контроллер запросит вторую загрузку на
			// тот же дефицит.
			inFlight++
		}
		// Проверка idle unload
		if inst.Status == types.ModelStateLoaded && cfg.IdleUnloadAfter != "" {
			dur, err := time.ParseDuration(cfg.IdleUnloadAfter)
			if err == nil && now.Sub(inst.LastUsedAt) > dur && instanceCount > cfg.MinInstances {
				inst.Status = types.ModelStateUnloading
			}
		}
	}
	g.mu.RUnlock()

	// Если loaded (+ уже запрошенные загрузки) < minInstances → запускаем warmup
	if loaded+inFlight < cfg.MinInstances {
		mgr.scaleUpGroup(g, cfg.MinInstances-loaded-inFlight)
	}

	// Если loaded > maxInstances → выгружаем LRU
	if loaded > cfg.MaxInstances {
		mgr.scaleDownGroup(g, loaded-cfg.MaxInstances)
	}
}

// syncLoadedInstances — R80: привести состав инстансов группы к тому, что
// реально загружено на бэкендах.
//
//   - бэкенд, на котором модель уже загружена, но которого нет в группе, —
//     усыновляется (LOADED), иначе «replicated» исполняется одной копией;
//   - инстанс, помеченный LOADED, но без модели на бэкенде, — сбрасывается,
//     чтобы контроллер перезагрузил его (например, cppworker перезапустили);
//     внутри loadedGracePeriod инстанс не трогаем: загрузка асинхронная и
//     модель появляется в метриках не сразу.
func (mgr *ModelGroupManager) syncLoadedInstances(g *ModelGroup) {
	mgr.mu.RLock()
	loadedFn := mgr.loadedFn
	mgr.mu.RUnlock()
	if loadedFn == nil {
		return
	}

	g.mu.RLock()
	modelName := g.Config.ModelName
	g.mu.RUnlock()
	if modelName == "" {
		return
	}

	loadedNow := map[string]bool{}
	for _, id := range loadedFn(modelName) {
		if id != "" {
			loadedNow[id] = true
		}
	}

	now := time.Now()
	adopted := make([]string, 0, len(loadedNow))
	dropped := make([]string, 0)

	g.mu.Lock()
	for backendID := range loadedNow {
		inst, exists := g.Instances[backendID]
		if exists && inst.Status == types.ModelStateLoaded {
			continue
		}
		if !exists {
			g.Instances[backendID] = &types.ModelInstanceState{
				BackendID:  backendID,
				Status:     types.ModelStateLoaded,
				LoadedAt:   now,
				LastUsedAt: now,
			}
		} else {
			// R80: загрузка, которую мы запросили, подтверждена метриками —
			// только теперь инстанс честно LOADED (до этого он LOADING).
			inst.Status = types.ModelStateLoaded
			inst.LoadedAt = now
			inst.LastUsedAt = now
		}
		adopted = append(adopted, backendID)
	}

	for backendID, inst := range g.Instances {
		if loadedNow[backendID] {
			continue
		}
		switch inst.Status {
		case types.ModelStateLoaded:
			if now.Sub(inst.LoadedAt) < loadedGracePeriod {
				continue // загрузка ещё идёт: тёплое окно
			}
			delete(g.Instances, backendID)
			dropped = append(dropped, backendID)
		case types.ModelStateLoading:
			if now.Sub(inst.LoadedAt) > loadingTimeout {
				delete(g.Instances, backendID)
				dropped = append(dropped, backendID)
			}
		}
	}
	g.mu.Unlock()

	if len(adopted) > 0 {
		logger.Get().Infow("group instances adopted from backends",
			"model", modelName, "backends", adopted)
	}
	if len(dropped) > 0 {
		logger.Get().Infow("group instances dropped (model not loaded on backend)",
			"model", modelName, "backends", dropped)
	}
}

// AdoptLoadedInstance — R81: событийно подтвердить инстанс группы.
//
// Вызывается, когда cppworker сам сообщил, что модель загружена
// (POST /api/v1/internal/llama-model-loaded): инстанс создаётся/повышается до
// LOADED немедленно, не дожидаясь опроса метрик (поллер ходит раз в 30 с —
// ровно столько раньше занимало «подтверждение» реплики).
func (mgr *ModelGroupManager) AdoptLoadedInstance(modelName, backendID string) bool {
	if mgr == nil || modelName == "" || backendID == "" {
		return false
	}
	mgr.mu.RLock()
	g, ok := mgr.groups[modelName]
	mgr.mu.RUnlock()
	if !ok {
		return false
	}

	now := time.Now()
	g.mu.Lock()
	inst, exists := g.Instances[backendID]
	if exists && inst.Status == types.ModelStateLoaded {
		g.mu.Unlock()
		return false
	}
	if !exists {
		g.Instances[backendID] = &types.ModelInstanceState{
			BackendID:  backendID,
			Status:     types.ModelStateLoaded,
			LoadedAt:   now,
			LastUsedAt: now,
		}
	} else {
		inst.Status = types.ModelStateLoaded
		inst.LoadedAt = now
		inst.LastUsedAt = now
	}
	g.mu.Unlock()

	logger.Get().Infow("group instance confirmed loaded by backend event",
		"model", modelName, "backend", backendID)
	return true
}

// DropInstance — R81: событийно убрать инстанс группы (cppworker сообщил о
// выгрузке модели или бэкенд отвалился). Возвращает true, если инстанс был.
func (mgr *ModelGroupManager) DropInstance(modelName, backendID string) bool {
	if mgr == nil || modelName == "" || backendID == "" {
		return false
	}
	mgr.mu.RLock()
	g, ok := mgr.groups[modelName]
	mgr.mu.RUnlock()
	if !ok {
		return false
	}
	g.mu.Lock()
	_, exists := g.Instances[backendID]
	if exists {
		delete(g.Instances, backendID)
	}
	g.mu.Unlock()
	if exists {
		logger.Get().Infow("group instance dropped by backend event",
			"model", modelName, "backend", backendID)
	}
	return exists
}

// scaleUpGroup загружает модель на свободных бэкендах.
func (mgr *ModelGroupManager) scaleUpGroup(g *ModelGroup, needed int) {
	g.mu.RLock()
	cfg := g.Config
	g.mu.RUnlock()

	mgr.mu.RLock()
	backendFn := mgr.backendFn
	warmupFn := mgr.warmupFn
	mgr.mu.RUnlock()

	if backendFn == nil || warmupFn == nil {
		logger.Get().Warnw("scaleUpGroup: callbacks not set, skipping")
		return
	}

	freeBackends := backendFn(cfg.ModelName, cfg.TargetBackends)
	loaded := 0
	for _, backendID := range freeBackends {
		if loaded >= needed {
			break
		}

		// Проверяем, не загружается ли уже
		g.mu.RLock()
		_, alreadyLoading := g.Instances[backendID]
		g.mu.RUnlock()
		if alreadyLoading {
			continue
		}

		// Обновляем состояние
		g.mu.Lock()
		g.Instances[backendID] = &types.ModelInstanceState{
			BackendID:  backendID,
			Status:     types.ModelStateLoading,
			LoadedAt:   time.Now(),
			LastUsedAt: time.Now(),
		}
		g.mu.Unlock()

		// Запускаем warmup асинхронно. R80: успешный вызов warmup = «загрузка
		// запрошена», а НЕ «модель загружена» (cppworker грузит модель минуты, а
		// callback балансера лишь принимает запрос). Инстанс переводится в
		// LOADED только когда модель подтверждена метриками бэкенда
		// (syncLoadedInstances).
		go func(bID string) {
			if err := warmupFn(bID, cfg.ModelName); err != nil {
				logger.Get().Errorw("group warmup failed",
					"backend", bID, "model", cfg.ModelName, "error", err)
				// Помечаем как ошибка
				g.mu.Lock()
				if inst, ok := g.Instances[bID]; ok {
					inst.Status = types.ModelStateNotLoaded
				}
				g.mu.Unlock()
				return
			}
			logger.Get().Infow("group instance load requested",
				"backend", bID, "model", cfg.ModelName)
		}(backendID)
		loaded++
	}
}

// scaleDownGroup выгружает избыточные инстансы (LRU).
func (mgr *ModelGroupManager) scaleDownGroup(g *ModelGroup, excess int) {
	type instanceInfo struct {
		backendID  string
		lastUsedAt time.Time
		useCount   int64
	}

	g.mu.RLock()
	cfg := g.Config
	instances := make([]instanceInfo, 0, len(g.Instances))
	for id, inst := range g.Instances {
		if inst.Status == types.ModelStateLoaded {
			instances = append(instances, instanceInfo{
				backendID:  id,
				lastUsedAt: inst.LastUsedAt,
				useCount:   inst.UseCount,
			})
		}
	}
	g.mu.RUnlock()

	// Сортируем: сначала наименее используемые (LRU)
	for i := 0; i < len(instances)-1; i++ {
		for j := i + 1; j < len(instances); j++ {
			if instances[i].lastUsedAt.After(instances[j].lastUsedAt) ||
				(instances[i].lastUsedAt.Equal(instances[j].lastUsedAt) && instances[i].useCount > instances[j].useCount) {
				instances[i], instances[j] = instances[j], instances[i]
			}
		}
	}

	unloaded := 0
	for _, inst := range instances {
		if unloaded >= excess {
			break
		}

		g.mu.Lock()
		if _, ok := g.Instances[inst.backendID]; ok {
			delete(g.Instances, inst.backendID)
		}
		g.mu.Unlock()

		// Отправляем unload signal через warmupFn(keep_alive=0)
		mgr.mu.RLock()
		if warmupFn := mgr.warmupFn; warmupFn != nil {
			go func(bID, model string) {
				_ = warmupFn(bID, model)
			}(inst.backendID, cfg.ModelName)
		}
		mgr.mu.RUnlock()

		unloaded++
		logger.Get().Infow("group instance unloaded",
			"backend", inst.backendID, "model", cfg.ModelName)
	}
}

// --- Selection ---

// selectInstance выбирает инстанс группы для следующего запроса.
//
// R81: обход строгий — сначала наименьшая загрузка (active/max), при равной
// загрузке побеждает инстанс с меньшим UseCount, при равенстве и его — меньший
// ID. Раньше при равной загрузке (частый случай: оба инстанса свободны)
// результат зависел от порядка обхода map, и распределение уезжало (в замере P4
// 6:2 и 7:1 при двух равных репликах). Выбранный инстанс сразу учитывается
// (UseCount++), поэтому следующий запрос уйдёт на другую копию.
func (mgr *ModelGroupManager) selectInstance(modelName string) string {
	mgr.mu.RLock()
	g, ok := mgr.groups[modelName]
	mgr.mu.RUnlock()
	if !ok {
		return ""
	}

	mgr.mu.RLock()
	loadFn := mgr.loadFn
	mgr.mu.RUnlock()

	if loadFn == nil {
		return ""
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	var bestID string
	var bestLoad float64 = 2.0
	var bestUse int64 = -1

	for backendID, inst := range g.Instances {
		if inst.Status != types.ModelStateLoaded {
			continue
		}
		load := loadFn(backendID)
		better := false
		switch {
		case load < bestLoad:
			better = true
		case load == bestLoad && bestID != "" && inst.UseCount < bestUse:
			better = true
		case load == bestLoad && bestID != "" && inst.UseCount == bestUse && backendID < bestID:
			better = true
		}
		if better || bestID == "" {
			bestID = backendID
			bestLoad = load
			bestUse = inst.UseCount
		}
	}
	if bestID == "" {
		return ""
	}
	g.Instances[bestID].UseCount++
	g.Instances[bestID].LastUsedAt = time.Now()
	return bestID
}

// Warmup запускает прогрев модели на указанном бэкенде для группы.
func (mgr *ModelGroupManager) Warmup(modelName, backendID string) error {
	mgr.mu.RLock()
	warmupFn := mgr.warmupFn
	mgr.mu.RUnlock()

	if warmupFn != nil {
		return warmupFn(backendID, modelName)
	}
	return nil
}
