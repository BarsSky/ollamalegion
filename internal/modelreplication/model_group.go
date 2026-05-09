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
	mu       sync.RWMutex
	groups   map[string]*ModelGroup // modelName → group
	enabled  bool

	// Callbacks для взаимодействия с Balancer
	loadFn    func(backendID string) float64     // получение загрузки бэкенда (0.0-1.0)
	backendFn func(modelName string, targets []string) []string // поиск свободных бэкендов
	warmupFn  func(backendID, modelName string) error // запуск warmup модели
}

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

// GetGroupStats возвращает расширенную статистику для группы.
func (mgr *ModelGroupManager) GetGroupStats(modelName string) map[string]interface{} {
	states := mgr.GetInstanceStates(modelName)
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
		"modelName":  modelName,
		"total":      len(states),
		"loaded":     loaded,
		"loading":    loading,
		"idle":       idle,
		"totalUses":  totalUseCount,
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
	g.mu.RLock()
	cfg := g.Config
	instanceCount := len(g.Instances)
	loaded := 0
	now := time.Now()
	for _, inst := range g.Instances {
		if inst.Status == types.ModelStateLoaded {
			loaded++
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

	// Если loaded < minInstances → запускаем warmup
	if loaded < cfg.MinInstances {
		mgr.scaleUpGroup(g, cfg.MinInstances-loaded)
	}

	// Если loaded > maxInstances → выгружаем LRU
	if loaded > cfg.MaxInstances {
		mgr.scaleDownGroup(g, loaded-cfg.MaxInstances)
	}
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

		// Запускаем warmup асинхронно
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
			// Успешно загружено
			g.mu.Lock()
			if inst, ok := g.Instances[bID]; ok {
				inst.Status = types.ModelStateLoaded
				inst.LoadedAt = time.Now()
			}
			g.mu.Unlock()
			logger.Get().Infow("group instance loaded",
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

// selectInstance выбирает наименее загруженный инстанс из группы.
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

	g.mu.RLock()
	defer g.mu.RUnlock()

	var bestID string
	var bestLoad float64 = 2.0

	for backendID, inst := range g.Instances {
		if inst.Status != types.ModelStateLoaded {
			continue
		}
		load := loadFn(backendID)
		if load < bestLoad {
			bestLoad = load
			bestID = backendID
		}
	}
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

