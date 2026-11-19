// bottleneck_observe.go — R495 (2026-11-18), S2: применение классификатора.
//
// Здесь вердикт СЧИТАЕТСЯ и ПУБЛИКУЕТСЯ. Решений он не меняет: это shadow по
// умолчанию (§4.2/§7 плана). Единственный побочный эффект — одна Info-строка на
// СМЕНУ вердикта (не на каждый замер: иначе лог превращается в шум, и смену
// вердикта в нём уже не найти).
package balancer

import (
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// observeBottlenecksR495 — пройти по бэкендам, посчитать вердикты, опубликовать
// и залогировать смены. Вызывается из сборщика метрик (там же, где обновляются
// снимки GPU), поэтому «живость» вердикта равна живости метрик.
func (p *Proxy) observeBottlenecksR495() {
	if p == nil || p.metricsMgr == nil || p.bottleneckTrackerR495Get() == nil {
		return
	}
	// off — прежнее поведение: ни счёта, ни логов, ни публикации.
	if bottleneckModeFromEnvR495() == BottleneckModeOffR495 {
		return
	}

	p.metricsMgr.mu.RLock()

	snapshot := make(map[string]BottleneckInputR495, len(p.metricsMgr.metrics))
	for id, m := range p.metricsMgr.metrics {
		if m == nil {
			continue
		}
		snapshot[id] = bottleneckInputFromMetricsR495(m.GPU, p.activeStreamsOnBackendR495(id))
	}
	p.metricsMgr.mu.RUnlock()

	now := time.Now()
	for id, in := range snapshot {
		// Пока карта полностью неизвестна (старый агент), вердикт не публикуем
		// вовсе: «unknown» в панели без чисел выглядел бы как факт.
		if in.SM <= 0 && !in.MemBusyMeasured {
			continue
		}
		state, changed := p.bottleneckTrackerR495Get().update(id, in, false, now)
		if !changed {
			continue
		}
		logger.Get().Infow("R495 bottleneck: вердикт сменился",
			"backend", id,
			"kind", state.Kind,
			"sm", in.SM,
			"mem_busy", in.MemBusy,
			"active_requests", in.ActiveRequests,
			"mode", bottleneckModeFromEnvR495().String(),
			"since", state.Since,
		)
	}
}

// bottleneckSnapshotR495 — опубликованные вердикты (для /api/metrics).
func (p *Proxy) bottleneckSnapshotR495() map[string]BottleneckStateR495 {
	if p == nil || p.bottleneckTrackerR495Get() == nil {
		return nil
	}
	if bottleneckModeFromEnvR495() == BottleneckModeOffR495 {
		return nil
	}
	snap := p.bottleneckTrackerR495Get().snapshot()
	if len(snap) == 0 {
		return nil
	}
	return snap
}

// activeStreamsOnBackendR495 — сколько ответов сейчас идёт на бэкенде.
// Nil-safe: у тестовых прокси счётчика может не быть.
func (p *Proxy) activeStreamsOnBackendR495(backendID string) int {
	if p == nil {
		return 0
	}
	return p.ActiveStreamsOnBackend(backendID)
}

// bottleneckTrackerR495Get — ленивая инициализация (как requestIndexR487): прокси
// собирается в тестах десятками способов, и требовать конструктор значило бы
// ломать каждый из них.
func (p *Proxy) bottleneckTrackerR495Get() *bottleneckTrackerR495 {
	if p == nil {
		return nil
	}
	p.bottleneckMuR495.Lock()
	defer p.bottleneckMuR495.Unlock()
	if p.bottleneckTracker == nil {
		p.bottleneckTracker = newBottleneckTrackerR495()
	}
	return p.bottleneckTracker
}
