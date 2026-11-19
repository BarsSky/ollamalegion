package balancer

import "ollama-loadbalancer/pkg/logger"

// logForceRebalanceNoAltR493 — «свободной альтернативы нет» при force-rebalance.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ (долг 12m). Раньше оба места (proxy.go и
// session_handler.go) писали Warnw «force rebalance failed, queueing request» на
// каждый запрос, который уходил в очередь из-за занятого слота. При включённой
// единой admission-очереди это ШТАТНЫЙ исход: запрос обязан встать в очередь и
// обслуживается ею. Живой факт (второй стенд, 2026-11-16, соук R493):
// 10 таких warn за 46 с при трёх параллельных клиентах (load_ratio=1), всего
// 20 warn из 507 строк — настоящие предупреждения в этом шуме не видны.
//
// Правило простое и проверяется сторожем: единая очередь включена → Info
// (штатный уход в очередь); выключена → Warn (запрос уйдёт в legacy-очередь или
// получит 503 — это действительно стоит внимания оператора).
func logForceRebalanceNoAltR493(session, backend string, loadRatio float64) {
	if admissionUnifiedR493() {
		logger.Get().Infow("force rebalance: свободного узла с запасом нет — решает единая очередь допуска",
			"session", session, "backend", backend, "load_ratio", loadRatio,
			"queue", "unified")
		return
	}
	logger.Get().Warnw("force rebalance failed, queueing request",
		"session", session, "backend", backend, "load_ratio", loadRatio,
		"queue", "legacy")
}
