// Package balancer — R92 (S2 плана 2026-10-07): логический бэкенд в балансере.
//
// План: docs/plans/2026-10-07-model-distribution-multi-gpu-next-session.md §5:
//
//	«Логический бэкенд — новый первоклассный объект: id, members[], role
//	(master/worker), strategy, vramBudget. Все существующие счётчики (слоты,
//	active, X-Backend-ID) работают с логическим id».
//
// Этот файл — МОСТ между конфигом балансера и внутренним реестром групп
// (internal/backendgroup): он отдаёт реестру свежие сведения об узлах (статус,
// VRAM, лимит слотов) и публикует состояния групп для API и монитора.
//
// Почему реестр не читает p.backends сам: пакет backendgroup не должен знать про
// internal/balancer (иначе цикл импортов), а копия индекса здоровья внутри него
// стала бы вторым источником правды. Замыкание-провайдер решает и то, и другое.
package balancer

import (
	"strings"

	"ollama-loadbalancer/internal/backendgroup"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// initBackendGroupsR92 собирает реестр логических бэкендов из конфига.
//
// Вызывается один раз при старте и повторно при перезагрузке конфига: реестр
// неизменяем, поэтому «перечитать конфиг» = построить новый и подменить
// указатель. Гонок нет: указатель читается в горячем пути без блокировки, а
// подмена происходит на этапе reload, когда трафик уже остановлен вызывающим.
func (p *Proxy) initBackendGroupsR92(cfg types.BalancingSettings) {
	if p == nil {
		return
	}
	groups := cfg.BackendGroups
	p.backendGroups = backendgroup.NewWithModelInfo(groups, p.groupMemberInfoR92, p.groupModelInfoR104)
	// R304: тот же момент (старт и перезагрузка конфига) — правильное место для
	// снимка ролей: роли приходят из state.json и persistent-оверрайдов, и
	// горячий путь не должен успеть увидеть пустой снимок.
	p.rebuildDonorIndexR304()
	if len(groups) == 0 {
		return
	}

	workers := 0
	for _, g := range groups {
		workers += len(g.Workers())
	}
	logger.Get().Infow("backend groups initialized",
		"groups", len(groups), "workers", workers,
		"plan", "docs/plans/2026-10-07-model-distribution-multi-gpu-next-session.md")
	for _, w := range types.ValidateBackendGroups(groups) {
		logger.Get().Warnw("backend group config needs attention", "warning", w)
	}
}

// SetBackendGroupsR92 подменяет конфиг групп (reload конфига, тесты).
//
// Обновляет И конфиг, И реестр: иначе BackendGroupWarnings (он читает
// balancing.backendGroups[] для показа предупреждений оператору) рассказывал бы
// про старый набор групп, а маршрутизация шла по новому.
//
// ВНИМАНИЕ: не для конкурентного вызова из горутин обработки запросов — как и
// SetPlacementSettings, это операция перезагрузки конфига.
func (p *Proxy) SetBackendGroupsR92(cfg types.BalancingSettings) {
	if p == nil || p.config == nil {
		return
	}
	p.config.Balancing.BackendGroups = cfg.BackendGroups
	p.initBackendGroupsR92(cfg)
}

// BackendGroupsR92 возвращает реестр групп (nil, если групп нет).
func (p *Proxy) BackendGroupsR92() *backendgroup.Registry {
	if p == nil {
		return nil
	}
	return p.backendGroups
}

// BackendGroupStatuses — состояния всех групп для API и монитора.
func (p *Proxy) BackendGroupStatuses() []backendgroup.GroupState {
	if p == nil || p.backendGroups == nil {
		return nil
	}
	return p.backendGroups.States()
}

// BackendGroupStateForModel — состояние группы, обслуживающей модель.
func (p *Proxy) BackendGroupStateForModel(model string) (backendgroup.GroupState, bool) {
	if p == nil || p.backendGroups == nil {
		return backendgroup.GroupState{}, false
	}
	return p.backendGroups.StateForModel(model)
}

// BackendGroupWarnings — ошибки конфига `balancing.backendGroups[]`.
//
// Отдельно от PlacementWarnings: та валидирует только секцию placement и о
// группах ничего не знает. Обе секции видны в GET /api/v1/placement.
func (p *Proxy) BackendGroupWarnings() []string {
	if p == nil || p.config == nil {
		return nil
	}
	groups := p.config.Balancing.BackendGroups
	if len(groups) == 0 {
		return nil
	}
	return types.ValidateBackendGroups(groups)
}

// RawBackendGroupsR102 — КОНФИГ групп (не состояние), в порядке конфига.
//
// Нужен редактирующим UI (S6): состояние (`BackendGroupStatuses`) отвечает на
// вопрос «собралась ли раскладка», а для записи через PUT /api/v1/placement нужен
// ровно тот конфиг, который там уже лежит — иначе окно объединения затирало бы
// группы, которых не касается.
func (p *Proxy) RawBackendGroupsR102() []types.BackendGroupConfig {
	if p == nil || p.config == nil {
		return nil
	}
	out := make([]types.BackendGroupConfig, len(p.config.Balancing.BackendGroups))
	copy(out, p.config.Balancing.BackendGroups)
	return out
}

// isGroupWorkerR92 — узел является воркером логического бэкенда.
//
// Воркеров ОБЯЗАНО исключать из обычного выбора: они не держат модель целиком,
// и запрос, ушедший на воркер напрямую, гарантированно сломается. Реестр после
// построения неизменяем, поэтому проверка безопасна и в горячем пути, и под
// уже взятым p.mu.RLock (см. expandCandidates) — вложенных локов нет.
func (p *Proxy) isGroupWorkerR92(backendID string) bool {
	if p == nil || p.backendGroups == nil {
		return false
	}
	return p.backendGroups.IsWorker(backendID)
}

// groupModelInfoR104 — измеренная скорость декода модели группы.
//
// Источник — ModelLatencyTracker: он собирает per-model метрики генерации
// (tokens/sec) для адаптивных таймаутов, и это ЕДИНСТВЕННЫЙ источник скорости в
// балансере: в метриках бэкендов tok/s нет. Поэтому карточка группы показывает
// измеренное значение, а если измерений ещё не было — честно «н/д», а не ноль.
//
// Маска группы (`qwen3-14b*`) с именем модели в трекере не совпадёт, поэтому при
// промахе ищем первое измеренное имя под маску: иначе на стенде, где скорость уже
// измерена, карточка показывала бы «н/д» из-за формы записи правила.
func (p *Proxy) groupModelInfoR104(model string) backendgroup.ModelInfo {
	if p == nil || p.modelLatencyTracker == nil {
		return backendgroup.ModelInfo{}
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return backendgroup.ModelInfo{}
	}
	if stats := p.modelLatencyTracker.GetStats(model); stats.AvgTokensPerSec > 0 {
		return backendgroup.ModelInfo{TokensPerSecond: stats.AvgTokensPerSec, TokensKnown: true}
	}
	if !strings.ContainsAny(model, "*?") {
		return backendgroup.ModelInfo{}
	}
	for _, name := range p.modelLatencyTracker.GetModelNames() {
		if !matchModelMask(model, name) {
			continue
		}
		if stats := p.modelLatencyTracker.GetStats(name); stats.AvgTokensPerSec > 0 {
			return backendgroup.ModelInfo{TokensPerSecond: stats.AvgTokensPerSec, TokensKnown: true}
		}
	}
	return backendgroup.ModelInfo{}
}

// groupMemberInfoR92 — сведения об узле группы для реестра.
//
// Источники те же, что у VRAM-fit auto (placement_fit_r75.go): статус бэкенда и
// снапшот метрик агента. Метрик VRAM нет (CPU-only, ollama без агента) — так и
// сообщаем: VRAMKnown=false, реестр не будет выдумывать числа.
func (p *Proxy) groupMemberInfoR92(backendID string) backendgroup.MemberInfo {
	if p == nil {
		return backendgroup.MemberInfo{}
	}
	p.mu.RLock()
	state, exists := p.backends[backendID]
	p.mu.RUnlock()
	if !exists || state == nil || state.Backend == nil {
		return backendgroup.MemberInfo{}
	}

	info := backendgroup.MemberInfo{
		Registered:        true,
		Healthy:           state.Backend.Status == types.StatusHealthy,
		Status:            string(state.Backend.Status),
		MaxConcurrentReqs: state.Backend.EffectiveMaxConcurrentRequests(),
	}
	if p.metricsMgr != nil {
		if metrics, ok := p.metricsMgr.SnapshotBackendMetrics(backendID); ok {
			if metrics.GPU.MemoryTotal > 0 {
				info.VRAMKnown = true
				info.VRAMFreeMB = int(metrics.GPU.MemoryFree)
				info.VRAMTotalMB = int(metrics.GPU.MemoryTotal)
			}
			// R104: что ЗАГРУЖЕНО на узле. Нужно окну объединения: при
			// «Разъединить» эти модели будут выгружены, и оператор обязан увидеть
			// список до подтверждения, а не узнать по таймауту следующего запроса.
			//
			// Берём оба источника, как это делает expandCandidates: у llama.cpp это
			// LoadedModels, у ollama — RunningModels. Ничего не додумываем: чего в
			// метриках нет, того в списке нет.
			loaded := make([]string, 0, len(metrics.LlamaCpp.LoadedModels)+len(metrics.Ollama.RunningModels))
			for _, m := range metrics.LlamaCpp.LoadedModels {
				if name := strings.TrimSpace(m.Name); name != "" {
					loaded = append(loaded, name)
				}
			}
			for _, m := range metrics.Ollama.RunningModels {
				if name := strings.TrimSpace(m.Name); name != "" {
					loaded = append(loaded, name)
				}
			}
			if len(loaded) > 0 {
				info.LoadedModels = loaded
			}
		}
	}
	return info
}
