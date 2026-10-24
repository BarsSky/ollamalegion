// Package balancer — R76 (placement policy, этап P3): честный отказ вместо
// тихой подмены стратегии.
//
// План: docs/plans/2026-09-23-multi-backend-placement-policy.md §6:
//
//	«Никогда не выдавать 200 с неполным/чужим ответом и не уходить в другую
//	стратегию молча». При `placement.fallback = "error"` (default) запрос,
//	который политика не может обслужить заявленной стратегией, получает явную
//	ошибку с числами и причиной; при `fallback = "single"` — обслуживается
//	обычным путём, но деградация видна в логах, заголовках и метриках.
//
// Что считается «не обслужить»:
//   - стратегия не исполняется в этой версии (rpc — legacy-путь координатора);
//   - стратегия исполнима, но исполнителя под неё сейчас нет: sharded без
//     готового логического бэкенда (R92/S3), replicated с нехваткой копий;
//   - auto не смогла выбрать (Degraded) и правило не разрешает деградацию
//     (`auto.allowDegraded=false`).
package balancer

import (
	"net/http"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// placementRejection — нужно ли отказать в запросе согласно политике.
//
// Возвращает (текст отказа, true), если запрос обслужить заявленной стратегией
// нельзя, а политика запрещает откат на другую стратегию.
func (p *Proxy) placementRejection(d PlacementDecision) (string, bool) {
	cfg := p.PlacementSettings()
	if !cfg.Enabled {
		return "", false
	}
	if d.Executable && !d.Degraded {
		return "", false
	}
	// Деградация разрешена правилом (auto.allowDegraded=true) — обслуживаем.
	if d.Degraded {
		if rule, ok := p.matchingModelRule(d.Model); ok && rule.Auto != nil && rule.Auto.AllowDegraded {
			return "", false
		}
	}
	// Политика разрешает откат на single — не отказываем (см. placementFallbackMode).
	if p.placementFallbackMode() == types.PlacementFallbackSingle {
		return "", false
	}

	if !d.Executable {
		return i18n.T("balancer.placement_reject.strategy_not_executable",
			string(d.Strategy), d.Reason), true
	}
	// Refined=true означает, что стратегию выбирала auto (P1): тогда «не смогла
	// выбрать» — точная формулировка. Для ЯВНОГО правила (Refined=false)
	// стратегия выбрана оператором, а исполнить её сейчас нечем — это другое
	// сообщение, иначе оператор ищет проблему в auto-правиле, которого нет.
	if d.Refined {
		return i18n.T("balancer.placement_reject.auto_could_not_choose", d.Reason), true
	}
	return i18n.T("balancer.placement_reject.strategy_not_executable_now",
		string(d.Strategy), d.Reason), true
}

// placementFallbackMode — как политика предписано поступать при невозможности
// обслужить заявленную стратегию ("" → error, как в дефолте плана).
func (p *Proxy) placementFallbackMode() string {
	if fallback := p.PlacementSettings().Fallback; fallback == types.PlacementFallbackSingle {
		return types.PlacementFallbackSingle
	}
	return types.PlacementFallbackError
}

// writePlacementUnavailable — 503 с причиной и решением (клиент/оператор видит,
// что именно не так с политикой размещения).
func writePlacementUnavailable(w http.ResponseWriter, d PlacementDecision, reason string) {
	logger.Get().Warnw(i18n.T("balancer.placement_reject.rejected_by_policy_warn"),
		"model", d.Model, "strategy", d.Strategy, "source", d.Source,
		"degraded", d.Degraded, "executable", d.Executable, "reason", reason)

	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		"error": reason,
		"placement": map[string]interface{}{
			"strategy":   string(d.Strategy),
			"source":     string(d.Source),
			"executable": d.Executable,
			"degraded":   d.Degraded,
			"reason":     d.Reason,
			"model":      d.Model,
		},
		"hint": i18n.T("balancer.placement_reject.hint"),
	})
}
