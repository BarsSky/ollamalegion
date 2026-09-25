// Package balancer — R74 (P1, placement policy): исполнение replicated/pool.
//
// План: plans/2026-09-23-multi-backend-placement-policy.md, этап P1.
//
// Что делает этот файл:
//  1. `setupReplicationManager` — wiring менеджера репликации (вынесен из
//     initRpcModules, чтобы его можно было поднять по требованию политики:
//     раньше менеджер существовал только при `modelReplication.enabled=true`);
//  2. `ensureReplicationManager` — идемпотентный «поднять, если нужно»;
//  3. `syncPlacementReplicationGroups` — привести группы репликации в
//     соответствие с политикой (`strategy=replicated`): создать группу, если её
//     нет. Дальше работает штатный `replicationSelector` в selectBackend, то
//     есть replicated-модель обслуживается репликами БЕЗ глобального
//     operatingMode=replication;
//  4. `PlacementReplicationStatus` — наблюдаемость для GET /api/v1/placement.
package balancer

import (
	"fmt"

	"ollama-loadbalancer/internal/modelreplication"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// setupReplicationManager — инициализация менеджера репликации + callbacks.
//
// Idempotent: повторный вызов ничего не делает (для policy-driven запуска).
func (p *Proxy) setupReplicationManager() {
	if p.modelReplication != nil && p.replicationSelector != nil {
		return
	}
	cfg := &p.config.Balancing

	p.modelReplication = modelreplication.NewModelGroupManager()
	p.modelReplication.SetEnabled(true)

	// Настраиваем callbacks для интеграции с Balancer
	p.modelReplication.SetBackendLoadFn(func(backendID string) float64 {
		p.mu.RLock()
		state, exists := p.backends[backendID]
		p.mu.RUnlock()
		if !exists {
			return 1.0
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.EffectiveMaxConcurrentRequests()
		state.mu.Unlock()
		if maxReqs <= 0 {
			return 0.0
		}
		return float64(active) / float64(maxReqs)
	})

	p.modelReplication.SetFreeBackendFn(func(modelName string, targets []string) []string {
		p.mu.RLock()
		defer p.mu.RUnlock()
		var free []string
		for id, state := range p.backends {
			if state.Backend.Status != types.StatusHealthy {
				continue
			}
			if len(targets) > 0 {
				found := false
				for _, t := range targets {
					if id == t {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			state.mu.Lock()
			active := state.ActiveReqs
			state.mu.Unlock()
			if active == 0 {
				free = append(free, id)
			}
		}
		return free
	})

	p.modelReplication.SetWarmupFn(func(backendID, modelName string) error {
		p.mu.RLock()
		state, exists := p.backends[backendID]
		p.mu.RUnlock()
		if !exists {
			return fmt.Errorf("backend %s not found", backendID)
		}
		p.warmupModel(backendID, state.Backend.Host, state.Backend.OllamaPort, modelName)
		return nil
	})

	p.replicationSelector = modelreplication.NewGroupAwareSelector(p.modelReplication)
	p.replicationCtrl = modelreplication.NewGroupController(p.modelReplication)
	if err := p.replicationCtrl.Start(); err != nil {
		logger.Get().Warnw("failed to start group controller", "error", err)
	}

	for _, groupCfg := range cfg.ModelReplication.Groups {
		if err := p.modelReplication.CreateGroup(groupCfg); err != nil {
			logger.Get().Warnw("failed to create model group from config",
				"model", groupCfg.ModelName, "error", err)
		}
	}

	logger.Get().Infow("model replication manager initialized",
		"defaultMinInstances", cfg.ModelReplication.DefaultMinInstances,
		"defaultMaxInstances", cfg.ModelReplication.DefaultMaxInstances,
		"idleUnloadAfter", cfg.ModelReplication.IdleUnloadAfter,
		"groups", len(cfg.ModelReplication.Groups))
}

// ensureReplicationManager — поднять менеджер репликации, если политика
// требует replicated, а менеджер ещё не создан (modelReplication.enabled=false).
func (p *Proxy) ensureReplicationManager() bool {
	if p == nil {
		return false
	}
	if p.replicationSelector != nil && p.modelReplication != nil {
		return true
	}
	p.setupReplicationManager()
	if p.replicationSelector == nil || p.modelReplication == nil {
		logger.Get().Warnw("placement: не удалось поднять менеджер репликации для strategy=replicated")
		return false
	}
	logger.Get().Infow("placement: менеджер репликации поднят по требованию политики")
	return true
}

// syncPlacementReplicationGroups — R74 (P1): создать группы репликации для
// моделей, у которых политика требует strategy=replicated, а также для
// strategy=auto с `auto.prefer`, содержащим replicated (R75: чтобы выбранная
// авто-стратегия replicated была подкреплена реальными репликами).
//
// Идемпотентно: существующие группы не трогаем (их состояние ведёт
// GroupController). Маски имён (`Qwen3*`) пропускаем: группе нужно конкретное
// имя модели — такие правила попадают в возвращённый список как пропущенные.
func (p *Proxy) syncPlacementReplicationGroups() []string {
	cfg := p.PlacementSettings()
	if !cfg.Enabled || len(cfg.Models) == 0 {
		return nil
	}

	var actions []string
	for _, rule := range cfg.Models {
		strategy := types.ParsePlacementStrategy(rule.Strategy)
		if strategy != types.PlacementReplicated && !autoPrefersReplicatedR75(rule) {
			continue
		}
		name := rule.Model
		if name == "" {
			continue
		}
		if _, isMask := maskOrName(name); isMask {
			actions = append(actions, "пропущено (маска, нужен точный model): "+name)
			continue
		}
		if !p.ensureReplicationManager() {
			actions = append(actions, "менеджер репликации недоступен: "+name)
			continue
		}
		if group := p.modelReplication.GetGroup(name); group != nil {
			continue // уже есть (из конфига или создана ранее)
		}
		minInstances := 1
		maxInstances := 0
		if rule.Auto != nil {
			if rule.Auto.MinBackends > 0 {
				minInstances = rule.Auto.MinBackends
			}
			if rule.Auto.MaxShardCount > 0 {
				maxInstances = rule.Auto.MaxShardCount
			}
		}
		// Для auto-правил, предпочитающих replicated, репликация осмысленна
		// минимум с двух инстансов (как и в autoStrategyAllowed).
		if strategy == types.PlacementAuto && minInstances < 2 {
			minInstances = 2
		}
		// maxInstances выводим из контекста: список бэкендов правила, дефолт
		// из конфига репликации, иначе min+1. CreateGroup требует
		// maxInstances >= minInstances.
		if maxInstances <= 0 && len(rule.Pool) > 0 {
			maxInstances = len(rule.Pool)
		}
		if maxInstances <= 0 && p.config.Balancing.ModelReplication.DefaultMaxInstances > 0 {
			maxInstances = p.config.Balancing.ModelReplication.DefaultMaxInstances
		}
		if maxInstances < minInstances {
			maxInstances = minInstances
		}
		groupCfg := types.ModelGroupConfig{
			ModelName:      name,
			MinInstances:   minInstances,
			MaxInstances:   maxInstances,
			TargetBackends: rule.Pool,
		}
		if err := p.modelReplication.CreateGroup(groupCfg); err != nil {
			actions = append(actions, fmt.Sprintf("ошибка создания группы %s: %v", name, err))
			continue
		}
		logger.Get().Infow("placement: группа репликации создана по политике",
			"model", name, "min_instances", minInstances,
			"max_instances", maxInstances, "targets", rule.Pool)
		actions = append(actions, fmt.Sprintf("группа создана: %s (min=%d max=%d targets=%d)",
			name, minInstances, maxInstances, len(rule.Pool)))
	}
	// Сразу поднимаем инстансы по minInstances (не ждём тика GroupController):
	// иначе политика «включилась», а реплик ещё нет и модель обслуживается
	// обычным путём до следующей итерации контроллера.
	if len(actions) > 0 {
		p.ensurePlacementInstances()
	}
	return actions
}

// ensurePlacementInstances — поднять инстансы групп по minInstances
// (idempotent; менеджер репликации сам решает, сколько экземпляров не хватает).
func (p *Proxy) ensurePlacementInstances() {
	if p == nil || p.modelReplication == nil {
		return
	}
	p.modelReplication.EnsureInstances()
}

// schedulePlacementResync — R78 (P3): пересобрать раскладку по политике после
// изменения состава бэкендов (добавление/удаление).
//
// Асинхронно и только когда это вообще нужно: синхронизация вызывает callbacks
// менеджера репликации, которые берут p.mu.RLock — из AddBackend/RemoveBackend
// (там p.mu уже удерживается) прямой вызов дал бы самоблокировку.
func (p *Proxy) schedulePlacementResync(reason string) {
	if p == nil || !p.placementResyncRelevant() {
		return
	}
	go func() {
		actions := p.syncPlacementReplicationGroups()
		// R78: главное здесь — дотянуть/пересобрать инстансы под новый состав
		// бэкендов (новый бэкенд должен получить свою реплику сразу, не ожидая
		// тика GroupController).
		p.ensurePlacementInstances()
		if len(actions) == 0 {
			logger.Get().Infow("placement: раскладка пересобрана после изменения состава бэкендов",
				"reason", reason)
			return
		}
		logger.Get().Infow("placement: раскладка пересобрана после изменения состава бэкендов",
			"reason", reason, "actions", actions)
	}()
}

// placementResyncRelevant — есть ли в политике правила, требующие групп
// репликации (strategy=replicated или auto с prefer=replicated).
func (p *Proxy) placementResyncRelevant() bool {
	cfg := p.PlacementSettings()
	if !cfg.Enabled {
		return false
	}
	for _, rule := range cfg.Models {
		if types.ParsePlacementStrategy(rule.Strategy) == types.PlacementReplicated {
			return true
		}
		if autoPrefersReplicatedR75(rule) {
			return true
		}
	}
	return false
}

// maskOrName — true, если в имени есть wildcard-символы маски.
func maskOrName(name string) (string, bool) {
	for _, ch := range name {
		if ch == '*' || ch == '?' {
			return name, true
		}
	}
	return name, false
}

// autoPrefersReplicatedR75 — правило auto, у которого в prefer есть replicated
// (для таких правил R75 заранее создаёт группу репликации).
func autoPrefersReplicatedR75(rule types.PlacementModelRule) bool {
	if types.ParsePlacementStrategy(rule.Strategy) != types.PlacementAuto || rule.Auto == nil {
		return false
	}
	for _, pref := range rule.Auto.Prefer {
		if types.ParsePlacementStrategy(pref) == types.PlacementReplicated {
			return true
		}
	}
	return false
}

// PlacementReplicationStatus — состояние репликации для отчёта
// /api/v1/placement: по каждой паре «модель → стратегия replicated/auto»
// показываем, есть ли группа и её кандидаты.
func (p *Proxy) PlacementReplicationStatus() map[string]interface{} {
	cfg := p.PlacementSettings()
	out := map[string]interface{}{
		"managerReady": p.replicationSelector != nil,
		"groups":       map[string]interface{}{},
	}
	groups := map[string]interface{}{}
	for _, rule := range cfg.Models {
		strategy := types.ParsePlacementStrategy(rule.Strategy)
		if strategy != types.PlacementReplicated && strategy != types.PlacementAuto {
			continue
		}
		entry := map[string]interface{}{
			"strategy": string(strategy),
			"declared": true,
		}
		if p.modelReplication != nil {
			if group := p.modelReplication.GetGroup(rule.Model); group != nil {
				entry["hasGroup"] = true
				entry["candidates"] = p.replicationSelector.GetGroupCandidates(rule.Model)
			} else {
				entry["hasGroup"] = false
			}
		} else {
			entry["hasGroup"] = false
		}
		groups[rule.Model] = entry
	}
	out["groups"] = groups
	return out
}
