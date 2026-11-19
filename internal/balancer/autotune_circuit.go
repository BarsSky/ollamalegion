// autotune_circuit.go — R636 (2026-11-19): предохранитель: AutoTuneCircuit и его конфигурация.
//
// ЗАЧЕМ ВЫНЕСЕНО. `autotune.go` держал 1666 строк: типы профиля, правила
// выбора KV-типа и числа слоёв, отчёт, предохранитель, трекер, две перезагрузки
// и планирование применения. Здесь — один из слоёв; поведение не менялось.
package balancer

import "ollama-loadbalancer/pkg/types"

// AutoTuneTracker accessor для API handlers (R54.6).
func (p *Proxy) AutoTuneTracker() *AutoTuneTracker {
	if p == nil {
		return nil
	}
	return p.autoTuneTracker
}

// WorkloadTracker accessor для API handlers (R54.9).
// Lazy init с default config — same pattern as AutoTuneTracker.
// R245: инициализация под autoTuneLazyMu — getter вызывается конкурентно
// из proxyRequestLlamaCpp (без блокировки это data race, ловится -race в CI).
func (p *Proxy) WorkloadTracker() *WorkloadTracker {
	if p == nil {
		return nil
	}
	p.autoTuneLazyMu.Lock()
	defer p.autoTuneLazyMu.Unlock()
	if p.workloadTracker == nil {
		p.workloadTracker = NewWorkloadTracker(DefaultWorkloadTrackerConfig())
	}
	return p.workloadTracker
}

// AutoTuneHistory accessor для API handlers (R55.2).
// Lazy init с default config (500 entries max).
// Используется для ring buffer последних AutoTune events.
// R245: та же защита, что и в WorkloadTracker() — вызовы идут одновременно
// из goroutine публикации событий и из HTTP handlers.
func (p *Proxy) AutoTuneHistory() *AutoTuneHistory {
	if p == nil {
		return nil
	}
	p.autoTuneLazyMu.Lock()
	defer p.autoTuneLazyMu.Unlock()
	if p.autoTuneHistory == nil {
		p.autoTuneHistory = NewAutoTuneHistory(500) // R55.2 default
	}
	return p.autoTuneHistory
}

// publishAutoTuneEvent — R55.2: helper для двойной записи event
// (EventBus для live WebSocket + AutoTuneHistory ring buffer для HTTP poll).
// Убирает boilerplate в 4 publish call-sites triggerAutoTuneReload.
func (p *Proxy) publishAutoTuneEvent(ev types.Event) {
	if p == nil {
		return
	}
	if p.EventBus() != nil {
		p.EventBus().Publish(ev)
	}
	if h := p.AutoTuneHistory(); h != nil {
		h.Record(ev)
	}
}
