package balancer

import (
	"context"
	"fmt"

	"ollama-loadbalancer/internal/modelreplication"
	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// initRpcModules - инициализация RPC Model Distribution модулей по enabled флагам.
// Выделен из proxy.go для уменьшения размера основного файла.
func (p *Proxy) initRpcModules() {
	cfg := &p.config.Balancing

	// Вариант A: Model Replication Manager
	if cfg.ModelReplication.Enabled {
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
			maxReqs := state.Backend.MaxConcurrentReqs
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

		// Создаём селектор и контроллер
		p.replicationSelector = modelreplication.NewGroupAwareSelector(p.modelReplication)
		p.replicationCtrl = modelreplication.NewGroupController(p.modelReplication)
		if err := p.replicationCtrl.Start(); err != nil {
			logger.Get().Warnw("failed to start group controller", "error", err)
		}

		// Загружаем группы из конфига
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

	// Вариант C: Virtual Model Router
	if cfg.VirtualModels.Enabled {
		p.virtualModels = virtualmodel.NewRegistry()
		p.virtualModels.SetEnabled(true)
		// Регистрируем виртуальные модели из конфига
		for _, vmCfg := range cfg.VirtualModels.Models {
			if err := p.virtualModels.Register(vmCfg); err != nil {
				logger.Get().Warnw("failed to register virtual model", "name", vmCfg.Name, "error", err)
			} else {
				logger.Get().Infow("virtual model registered", "name", vmCfg.Name,
					"slices", len(vmCfg.Slices))
			}
		}
		// Создаём Router для маршрутизации через VirtualModel pipeline
		p.virtualModelRouter = virtualmodel.NewRouter(p.virtualModels)
		logger.Get().Infow("virtual model router initialized",
			"enabled", true, "count", len(cfg.VirtualModels.Models))
	}

	// Вариант B: External RPC Coordinator
	if cfg.RpcCoordinator.Enabled {
		p.rpcCoordinator = rpccoordinator.NewModelCoordinator(cfg.RpcCoordinator)
		// Регистрируем worker'ов из конфига
		// (примечание: worker'ы обычно регистрируются динамически через API)
		logger.Get().Infow("rpc coordinator initialized",
			"enabled", true,
			"coordinatorURL", cfg.RpcCoordinator.CoordinatorURL,
			"protocol", cfg.RpcCoordinator.Protocol,
			"maxRetries", cfg.RpcCoordinator.MaxRetries)
	}
}

// GetRpcCoordinator возвращает RPC Coordinator (для API).
func (p *Proxy) GetRpcCoordinator() *rpccoordinator.ModelCoordinator {
	return p.rpcCoordinator
}

// GetRpcCoordinatorDispatcher возвращает dispatcher (Phase 8.5).
// Используется в Phase 9 main.go wiring (cmd/balancer/main.go) для
// инициализации dispatcher после создания coordinator.
func (p *Proxy) GetRpcCoordinatorDispatcher() *RpcCoordinatorDispatcher {
	return p.rpcDispatcher
}

// SetRpcCoordinatorDispatcher устанавливает dispatcher (Phase 9 main.go wiring).
// Вызывается после initRpcModules() если cfg.RpcCoordinator.Embedded=true.
func (p *Proxy) SetRpcCoordinatorDispatcher(d *RpcCoordinatorDispatcher) {
	p.rpcDispatcher = d
}

// GetVirtualRouter возвращает Phase 8 P.2 VirtualRouter (для API и main.go).
// VirtualRouter перехватывает requests с model=virtual:xxx и выбирает backend
// через Selector (round_robin / least_loaded / random).
func (p *Proxy) GetVirtualRouter() *VirtualRouter {
	return p.virtualRouter
}

// SetVirtualRouter устанавливает VirtualRouter (Phase 8 P.2 main.go wiring).
// Вызывается после initRpcModules() если cfg.Balancing.OperatingMode=virtual_router.
func (p *Proxy) SetVirtualRouter(r *VirtualRouter) {
	p.virtualRouter = r
}

// GetVirtualModelRegistry возвращает VirtualModel Registry (Phase 8 P.2).
// Используется в main.go для создания VirtualRouter поверх registry.
func (p *Proxy) GetVirtualModelRegistry() *virtualmodel.Registry {
	return p.virtualModels
}

// HasDistributedModel проверяет, доступна ли модель через RPC Coordinator.
func (p *Proxy) HasDistributedModel(modelName string) bool {
	if p.rpcCoordinator == nil {
		return false
	}
	return p.rpcCoordinator.HasDistributedModel(modelName)
}

// InferDistributed выполняет распределённый inference через RPC Coordinator.
func (p *Proxy) InferDistributed(ctx context.Context, modelName, prompt string, params map[string]string) (*rpccoordinator.InferResponse, error) {
	if p.rpcCoordinator == nil {
		return nil, fmt.Errorf("rpc coordinator not initialized")
	}
	req := rpccoordinator.InferRequest{
		ModelName: modelName,
		Prompt:    prompt,
		Params:    params,
	}
	return p.rpcCoordinator.Infer(ctx, req)
}

// GetModelReplicationManager возвращает ModelGroupManager (для API).
func (p *Proxy) GetModelReplicationManager() *modelreplication.ModelGroupManager {
	return p.modelReplication
}

// GetReplicationSelector возвращает GroupAwareSelector (для API).
func (p *Proxy) GetReplicationSelector() *modelreplication.GroupAwareSelector {
	return p.replicationSelector
}

// GetReplicationController возвращает GroupController (для API).
func (p *Proxy) GetReplicationController() *modelreplication.GroupController {
	return p.replicationCtrl
}

// GetVirtualModelRouter возвращает VirtualModel Router (для API).
func (p *Proxy) GetVirtualModelRouter() *virtualmodel.Router {
	return p.virtualModelRouter
}

// GetProxyLogger возвращает ProxyLogger для доступа из API слоя.
func (p *Proxy) GetProxyLogger() *ProxyLogger {
	return p.proxyLogger
}