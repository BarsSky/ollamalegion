// placement_replication_free_r379.go — R379 (2026-11-01): «свободный бэкенд»
// для репликации определяется ПАМЯТЬЮ, а не счётчиком запросов (P2-2).
//
// ЗАЧЕМ. До R379 SetFreeBackendFn (placement_replication_r74.go) считал
// свободным бэкенд с ActiveReqs == 0. Оба края этого правила жили на стенде:
//
//   - бэкенд обслуживает ОДИН короткий запрос другой модели → ActiveReqs=1 →
//     «не свободен» навсегда с точки зрения репликации, хотя VRAM пуста;
//   - бэкенд простаивает (ActiveReqs=0), но на его карте лежит чужая модель
//     почти на всю память → «свободен» → репликация грузит туда копию и
//     получает отказ/вытеснение.
//
// ПРАВИЛО. Свободен = модель ПОМЕЩАЕТСЯ: копия уже загружена ИЛИ свободного
// VRAM хватает на веса с запасом (тот же placementFitForModel, что у политики
// размещения). ActiveReqs остаётся только для ПОРЯДКА: простаивающие бэкенды
// предлагаются первыми, чтобы не ставить вторую модель поверх активной без
// необходимости.
package balancer

import (
	"sort"

	"ollama-loadbalancer/pkg/types"
)

// freeBackendsForReplicationR379 — кандидаты на копию модели для менеджера
// репликации: healthy, не заблокированные R304, из targets и такие, где модель
// помещается. Порядок: сначала простаивающие (ActiveReqs == 0), внутри группы —
// по id (детерминизм: выбор копии не должен «прыгать» между тиками контроллера).
func (p *Proxy) freeBackendsForReplicationR379(modelName string, targets []string) []string {
	if p == nil {
		return nil
	}
	type replicationCandidateR379 struct {
		id   string
		idle bool
	}
	candidates := make([]replicationCandidateR379, 0, len(p.backends))

	p.mu.RLock()
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		// R304: репликация грузит КОПИЮ модели на свободный узел, то есть ровно
		// то, что донору запрещено: его VRAM держит мастер.
		if p.cannotHostModelR304(id) {
			continue
		}
		if len(targets) > 0 && !stringInSliceR379(targets, id) {
			continue
		}
		state.mu.Lock()
		idle := state.ActiveReqs == 0
		state.mu.Unlock()
		candidates = append(candidates, replicationCandidateR379{id: id, idle: idle})
	}
	p.mu.RUnlock()

	// placementFitForModel берёт p.mu.RLock сам — вызывать только ПОСЛЕ
	// освобождения блокировки выше (RLock не реентрантен при ждущем писателе).
	fit := p.placementFitForModel(modelName)
	fits := make(map[string]bool, len(fit.FitBackends))
	for _, id := range fit.FitBackends {
		fits[id] = true
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].idle != candidates[j].idle {
			return candidates[i].idle
		}
		return candidates[i].id < candidates[j].id
	})

	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if !fits[c.id] {
			continue
		}
		out = append(out, c.id)
	}
	return out
}

// stringInSliceR379 — contains без аллокаций map (списки целей короткие).
func stringInSliceR379(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
