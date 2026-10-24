// Package balancer — R92 (S3 плана 2026-10-07): маршрутизация раскладки.
//
// План §5: «Одна модель = один логический бэкенд при sharded»; «cppworker знает
// только про свой GPU и про список RPC-серверов; он не знает, „какой режим“ в
// балансере».
//
// Роль этого файла — ответить на один вопрос: должен ли ЭТОТ запрос уйти на
// мастера логического бэкенда. Ответ даёт политика размещения (а не «режим»):
// группа — исполнитель стратегии `sharded`, поэтому при strategy=single/pool/
// replicated группа в маршрутизации не участвует вообще. Если политика говорит
// sharded, но группа не готова (выпал воркер, нет метрик, не зарегистрирован
// узел), функция возвращает "" — запрос идёт обычным путём, а честность
// обеспечивает решение политики: при fallback=error он до маршрутизации не
// дойдёт вовсе (R76), при fallback=single — обслужен как single с явным
// X-LB-Placement-Fallback.
package balancer

import (
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// shardedMasterForRequestR92 — backend id мастера группы, если запрос должен
// обслуживаться раскладкой; иначе "".
func (p *Proxy) shardedMasterForRequestR92(model string, bt types.BackendType) string {
	if p == nil || p.backendGroups == nil || model == "" || !p.backendGroups.Configured() {
		return ""
	}
	sizeGB := float64(p.getModelSizeBytes(model)) / (1024 * 1024 * 1024)
	decision := p.ResolvePlacement(model, sizeGB, "")
	if decision.Strategy != types.PlacementSharded {
		return ""
	}
	master := p.backendGroups.UsableMaster(model)
	if master == "" {
		// Группа не готова. Дожимать запрос на мастера без части слоёв нельзя:
		// это гарантированная ошибка инференса, а не деградация.
		return ""
	}
	if !p.backendMatchesAllowedTypesR92(master, bt) {
		// Запрос просит другой тип бэкенда (например image) — раскладка живёт
		// на llama.cpp-узлах и сюда не относится.
		return ""
	}
	logger.Get().Infow("sharded: запрос направлен на мастер логического бэкенда",
		"model", model, "group", decision.GroupID, "master", master,
		"members", decision.GroupMembers,
		"strategy", decision.Strategy, "source", decision.Source)
	return master
}

// backendMatchesAllowedTypesR92 — подходит ли узел под допустимые типы запроса.
func (p *Proxy) backendMatchesAllowedTypesR92(backendID string, bt types.BackendType) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok || state == nil || state.Backend == nil {
		return false
	}
	return isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), p.getAllowedTypesList(bt))
}
