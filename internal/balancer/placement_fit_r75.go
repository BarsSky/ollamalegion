// Package balancer — R75 (placement policy, этап P1.5): VRAM-fit для auto.
//
// План: plans/2026-09-23-multi-backend-placement-policy.md §4 — правило выбора
// стратегии для `auto`:
//
//	need = weights + kv + overhead
//	по порядку auto.prefer: single (влезает хотя бы на один бэкенд) →
//	replicated (влезает на >= minBackends бэкендов) → sharded/rpc (P2, пока не
//	исполняется) → иначе degraded с числами.
//
// Что здесь есть и чего нет:
//   - need считается от размера GGUF (профиль модели или метрики загруженной
//     модели) с запасом placementVRAMSafetyFactor на KV-cache и накладные;
//     точный расчёт KV под конкретный n_ctx живёт в cppworker (autotune/nctx) —
//     переносить его в балансер в этом этапе не стали;
//   - «влезает» = модель уже загружена на бэкенде (заведомо влезает) ИЛИ
//     свободный VRAM бэкенда >= need;
//   - если размер модели неизвестен — не угадываем: degraded single с причиной
//     (нужны метрики/профиль с sizeBytes).
package balancer

import (
	"fmt"
	"sort"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// placementVRAMSafetyFactor — запас к размеру весов на KV-cache и накладные
// расходы (llama.cpp context, compute buffers). 1.25 ≈ 25%.
const placementVRAMSafetyFactor = 1.25

// placementFit — результат проверки «где влезает модель».
//
// Порядок полей — под fieldalignment (map, slice, затем числа/bool).
type placementFit struct {
	// FreeByBackend — свободный VRAM бэкенда в БАЙТАХ (метрики дают МБ).
	FreeByBackend   map[string]uint64
	FitBackends     []string
	NeedBytes       int64
	SizeBytes       int64
	TotalBackends   int
	UnknownBackends int
	SizeKnown       bool
}

// placementFitForModel — где может жить модель по свободному VRAM.
//
// «Влезает» = модель уже загружена на бэкенде ИЛИ (метрики известны и
// свободного VRAM ≥ need). Бэкенды без метрик VRAM (MemoryTotal=0: CPU-only,
// ollama без агента) считаем НЕИЗВЕСТНЫМИ и не блокируем по ним выбор —
// деградация объявляется только когда мы точно знаем, что не вмещается.
func (p *Proxy) placementFitForModel(model string) placementFit {
	fit := placementFit{
		FreeByBackend: map[string]uint64{},
	}
	if p == nil {
		return fit
	}
	size := p.getModelSizeBytes(model)
	fit.SizeBytes = size
	fit.SizeKnown = size > 0
	if fit.SizeKnown {
		fit.NeedBytes = int64(float64(size) * placementVRAMSafetyFactor)
	}

	p.mu.RLock()
	backends := make([]string, 0, len(p.backends))
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		backends = append(backends, id)
	}
	p.mu.RUnlock()
	sort.Strings(backends)
	fit.TotalBackends = len(backends)

	for _, id := range backends {
		// R80/R81: «загружена» проверяется по всем снапшотам метрик
		// (backendHasModelByID), а не только по context_length из llamaMetrics —
		// иначе, когда обе копии реплики уже загружены и свободного VRAM почти
		// нет, фит-проверка объявляла «подходящих бэкендов 0» и политика
		// отклоняла запросы 503-й, хотя копии на месте.
		if p.backendHasModelByID(id, model) {
			fit.FitBackends = append(fit.FitBackends, id)
			continue
		}
		metrics, ok := p.metricsMgr.SnapshotBackendMetrics(id)
		if !ok || metrics.GPU.MemoryTotal == 0 {
			// Метрик VRAM нет — судить не можем, не мешаем выбору.
			fit.UnknownBackends++
			fit.FitBackends = append(fit.FitBackends, id)
			continue
		}
		freeBytes := int64(metrics.GPU.MemoryFree) * 1024 * 1024 // МБ → байты
		fit.FreeByBackend[id] = uint64(freeBytes)
		if !fit.SizeKnown || freeBytes >= fit.NeedBytes {
			fit.FitBackends = append(fit.FitBackends, id)
		}
	}
	return fit
}

// autoStrategyAllowed — сколько бэкендов нужно для стратегии и допустима ли она
// в этой версии (sharded/rpc — этап P2, помечаем как «пропущено»).
func (p *Proxy) autoStrategyAllowed(strategy types.PlacementStrategy, rule types.PlacementModelRule) (need int, ok bool, note string) {
	switch strategy {
	case types.PlacementSingle:
		return 1, true, ""
	case types.PlacementReplicated:
		need = 2
		if rule.Auto != nil && rule.Auto.MinBackends > 0 {
			need = rule.Auto.MinBackends
		}
		return need, true, ""
	case types.PlacementPool:
		if p.virtualRouter != nil && p.virtualRouter.IsVirtualModelPath(rule.Model) {
			return 0, true, ""
		}
		return 0, false, "pool: модель не зарегистрирована как виртуальный алиас"
	case types.PlacementSharded, types.PlacementRPC:
		return 0, false, string(strategy) + ": не исполняется до этапа P2 (нужен транспорт раскладки)"
	default:
		return 0, false, "неизвестная стратегия " + string(strategy)
	}
}

// autoPreferOrder — порядок стратегий для auto (по умолчанию single).
func autoPreferOrder(rule types.PlacementModelRule) []types.PlacementStrategy {
	if rule.Auto == nil || len(rule.Auto.Prefer) == 0 {
		return []types.PlacementStrategy{types.PlacementSingle}
	}
	out := make([]types.PlacementStrategy, 0, len(rule.Auto.Prefer))
	for _, pref := range rule.Auto.Prefer {
		out = append(out, types.ParsePlacementStrategy(pref))
	}
	return out
}

// resolveAutoStrategy — R75: выбрать стратегию для auto по VRAM-fit (§4).
//
// Возвращает стратегию, признак деградации (ни одна стратегия не подошла —
// выбрана single) и причину для логов/метрик.
func (p *Proxy) resolveAutoStrategy(d PlacementDecision, rule types.PlacementModelRule, ruleFound bool) (types.PlacementStrategy, bool, string) {
	// Алиас — раньше VRAM: пул обслуживает его по registry.
	if p.virtualRouter != nil && p.virtualRouter.IsVirtualModelPath(d.Model) {
		return types.PlacementPool, false, "auto → pool: модель — виртуальный алиас (registry, alias-on-pool)"
	}

	fit := p.placementFitForModel(d.Model)
	prefer := autoPreferOrder(rule)
	var skipped []string

	// R76 (P3): однородность — если правило требует одинаковые GPU, репликацию
	// (и раскладку) считаем допустимой только внутри одной группы бэкендов.
	homogeneousNote := ""
	if rule.Auto != nil && rule.Auto.RequireHomogeneous {
		before := len(fit.FitBackends)
		fit = p.filterHomogeneousFit(fit)
		if len(fit.FitBackends) < before {
			homogeneousNote = fmt.Sprintf("однородность: подходящих %d → %d (одинаковые GPU)",
				before, len(fit.FitBackends))
		}
	}

	for _, strategy := range prefer {
		need, ok, note := p.autoStrategyAllowed(strategy, rule)
		if !ok {
			if note != "" {
				skipped = append(skipped, note)
			}
			continue
		}
		if strategy == types.PlacementPool {
			continue // алиас уже обработан выше
		}
		if len(fit.FitBackends) >= need && (need > 0 || strategy == types.PlacementPool) {
			reason := fmt.Sprintf("auto → %s: влезает на %d бэкенд(ах) из %d (need≈%s)",
				strategy, len(fit.FitBackends), fit.TotalBackends,
				humanBytesR75(fit.NeedBytes))
			if fit.UnknownBackends > 0 {
				reason += fmt.Sprintf("; без метрик VRAM: %d", fit.UnknownBackends)
			}
			if !fit.SizeKnown {
				reason += "; размер модели неизвестен — VRAM-fit не проверен"
			}
			if strategy == types.PlacementReplicated {
				reason += fmt.Sprintf(", minBackends=%d", need)
			}
			if homogeneousNote != "" {
				reason += "; " + homogeneousNote
			}
			if len(skipped) > 0 {
				// R75: сообщаем и о пропущенных вариантах (например sharded до P2),
				// иначе оператор не поймёт, почему выбран не первый prefer.
				reason += "; пропущено: " + joinR75(skipped)
			}
			return strategy, false, reason
		}
	}

	// Ни одна стратегия не подошла — деградируем на single с числами.
	reason := "auto → single (деградация): "
	if !fit.SizeKnown {
		reason += "размер модели неизвестен (нет профиля sizeBytes и метрик загруженной модели)"
	} else {
		reason += fmt.Sprintf("ни один бэкенд не вмещает: need≈%s (веса %s + запас %.0f%%), свободно максимум %s",
			humanBytesR75(fit.NeedBytes), humanBytesR75(fit.SizeBytes),
			(placementVRAMSafetyFactor-1)*100, humanBytesR75(int64(maxFreeR75(fit))))
	}
	if len(skipped) > 0 {
		reason += "; пропущено: " + joinR75(skipped)
	}
	logger.Get().Warnw("placement auto: не удалось выбрать стратегию по VRAM, деградация на single",
		"model", d.Model, "reason", reason, "fit_backends", fit.FitBackends)
	return types.PlacementSingle, true, reason
}

// backendGPUIdentity — R76 (P3): приблизительная «личность» GPU бэкенда для
// requireHomogeneous. Точного имени модели GPU в метриках балансера нет, поэтому:
//  1. метка вида `gpu:*` или `sm_*` (её проставляют cppworker/агент);
//  2. иначе — объём VRAM (`vram:<МБ>`): одинаковая память ⇒ скорее всего
//     одинаковая карта;
//  3. иначе — "" (неизвестно; такие бэкенды считаются совместимыми между собой).
func (p *Proxy) backendGPUIdentity(backendID string) string {
	p.mu.RLock()
	state, exists := p.backends[backendID]
	p.mu.RUnlock()
	if !exists || state == nil || state.Backend == nil {
		return ""
	}
	for _, label := range state.Backend.Labels {
		l := strings.ToLower(strings.TrimSpace(label))
		if strings.HasPrefix(l, "gpu:") || strings.HasPrefix(l, "sm_") || strings.HasPrefix(l, "sm") {
			return l
		}
	}
	if metrics, ok := p.metricsMgr.SnapshotBackendMetrics(backendID); ok && metrics.GPU.MemoryTotal > 0 {
		return fmt.Sprintf("vram:%d", metrics.GPU.MemoryTotal)
	}
	return ""
}

// filterHomogeneousFit — оставить только самую большую группу бэкендов с
// одинаковой «личностью» GPU (при равенстве — лексикографически первую, чтобы
// решение было детерминированным).
func (p *Proxy) filterHomogeneousFit(fit placementFit) placementFit {
	if len(fit.FitBackends) <= 1 {
		return fit
	}
	groups := map[string][]string{}
	for _, id := range fit.FitBackends {
		identity := p.backendGPUIdentity(id)
		groups[identity] = append(groups[identity], id)
	}
	bestKey := ""
	bestLen := 0
	for key, ids := range groups {
		if len(ids) > bestLen || (len(ids) == bestLen && key < bestKey) {
			bestKey, bestLen = key, len(ids)
		}
	}
	fit.FitBackends = groups[bestKey]
	sort.Strings(fit.FitBackends)
	return fit
}

func maxFreeR75(fit placementFit) uint64 {
	var max uint64
	for _, free := range fit.FreeByBackend {
		if free > max {
			max = free
		}
	}
	return max
}

func humanBytesR75(b int64) string {
	const (
		mb = 1024 * 1024
		gb = 1024 * mb
	)
	switch {
	case b <= 0:
		return "0"
	case b >= gb:
		return fmt.Sprintf("%.1f ГБ", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%d МБ", b/mb)
	default:
		return fmt.Sprintf("%d Б", b)
	}
}

func joinR75(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
