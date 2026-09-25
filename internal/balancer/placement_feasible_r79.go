// Package balancer — R79 (P4-подготовка): исполнимость ЯВНОЙ стратегии.
//
// План: plans/2026-09-23-multi-backend-placement-policy.md §6 —
//
//	«Стратегия из политики недоступна (нет воркеров / мало бэкендов): при
//	 fallback: error — ошибка; при fallback: single — standard + WARN и
//	 degraded: true в метриках».
//
// R76 закрыл эту строку только для auto (Degraded) и для стратегий, которых
// нет в сборке (sharded/rpc). Явное правило `strategy: replicated` с одним
// подходящим бэкендом до R79 не проверялось вообще: группа репликации
// создавалась с minInstances=1, запрос обслуживался одной копией, а
// X-LB-Placement при этом рапортовал `replicated`. Это ровно тот «тихий уход
// в другую стратегию», который §6 запрещает.
//
// R79 закрывает дыру: для явных правил политики (model/class) считаем, сколько
// бэкендов реально подходит под стратегию (тот же VRAM-fit, что у auto), и при
// нехватке помечаем решение Degraded с числами. Дальше работает общий
// механизм §6: fallback=error → 503, fallback=single → обслуживание с
// X-LB-Placement-Fallback, auto.allowDegraded=true → обслуживание молча (явный
// операторский выбор, решение видно в /api/v1/placement и в метриках).
//
// Глобальный дефолт (operatingMode, source=global) и per-request override
// намеренно НЕ проверяются: operatingMode=replication на одном бэкенде — это
// легальная конфигурация до политики, ломать её обратной совместимостью нельзя.
package balancer

import (
	"fmt"

	"ollama-loadbalancer/pkg/types"
)

// placementExplicitNeedR79 — сколько бэкендов требует явная стратегия правила.
//
// applicable=false — для этой стратегии проверка не нужна (single/pool —
// помещаются на одном бэкенде; sharded/rpc отсекаются как «не исполняется»).
func placementExplicitNeedR79(strategy types.PlacementStrategy, rule types.PlacementModelRule) (int, bool) {
	switch strategy {
	case types.PlacementReplicated:
		// «N копий модели на N бэкендах» — минимум 2 (одна копия = single).
		// auto.minBackends у явного replicated — операторский override.
		if rule.Auto != nil && rule.Auto.MinBackends > 0 {
			return rule.Auto.MinBackends, true
		}
		return 2, true
	default:
		return 0, false
	}
}

// checkExplicitStrategyFeasibilityR79 — помечает решение Degraded, если явная
// стратегия правила не может быть исполнена полностью (мало подходящих
// бэкендов). Изменённое решение дальше обрабатывает общий §6-механизм
// (placementRejection / X-LB-Placement-Fallback).
func (p *Proxy) checkExplicitStrategyFeasibilityR79(d PlacementDecision) PlacementDecision {
	if p == nil || p.config == nil {
		return d
	}
	// Реагируем только на явные правила политики: global — это operatingMode
	// (обратная совместимость), request — осознанный override оператора.
	if d.Source != PlacementSourceModel && d.Source != PlacementSourceClass {
		return d
	}
	if !p.PlacementSettings().Enabled || !d.Executable || d.Degraded {
		return d
	}

	// Правило модели даёт auto.minBackends (если оператор его задал). Для
	// правила класса (модель не описана в models[]) auto-параметров нет —
	// работаем с дефолтом стратегии.
	var rule types.PlacementModelRule
	if matched, ok := p.matchingModelRule(d.Model); ok {
		rule = matched
	}

	need, applicable := placementExplicitNeedR79(d.Strategy, rule)
	if !applicable {
		return d
	}

	fit := p.placementFitForModel(d.Model)
	if len(fit.FitBackends) >= need {
		return d
	}

	reason := fmt.Sprintf("%s: подходящих бэкендов %d, нужно %d (копий модели)",
		d.Strategy, len(fit.FitBackends), need)
	if fit.SizeKnown {
		reason += fmt.Sprintf("; need≈%s на копию, свободно максимум %s",
			humanBytesR75(fit.NeedBytes), humanBytesR75(int64(maxFreeBytesR79(fit))))
	} else if fit.TotalBackends > 0 {
		reason += fmt.Sprintf("; размер модели неизвестен (нет профиля/метрик), healthy бэкендов %d",
			fit.TotalBackends)
	}
	reason += " — стратегия не будет исполнена полностью, запрос обслуживается одной копией"

	d.Degraded = true
	d.Reason = reason
	return d
}

// maxFreeBytesR79 — максимум свободного VRAM среди подходящих бэкендов
// (для чисел в причине деградации).
func maxFreeBytesR79(fit placementFit) uint64 {
	var maxFree uint64
	for _, free := range fit.FreeByBackend {
		if free > maxFree {
			maxFree = free
		}
	}
	return maxFree
}
