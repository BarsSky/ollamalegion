// Round 52.4 (2026-08-24): test helper to fix leaked goroutines.
//
// Pre-R52.4: tests calling `NewProxy(conf)` напрямую не останавливали
// background controllers, которые NewProxy запускает (goroutine-leak):
//   - llamaCppMetricsPoller.loop (query llama.cpp каждые 30s)
//   - AdaptiveWeightTuner.loop (tune weights каждые 60s)
//   - SessionManager.cleanupLoop (cleanup idle sessions каждые 30s)
//   - StartAgentTimeoutChecker.func1 (timeout check каждые 60s)
//   - queueMgr workers (request queue workers)
//   - AutoPullManager (auto-pull on demand)
//
// Каждый test создавал 1-2 прокси → 8-16 leaked goroutine per test.
// 200+ тестов в internal/balancer = 1000+ leaked goroutines. После
// 60s тест-suite уходил в timeout (тесты не возвращались, и Go test
// runner ждал завершения goroutines).
//
// Fix: helper `newProxyWithCleanup(t, conf)` создаёт proxy + автоматически
// вызывает `t.Cleanup(proxy.Shutdown)`. Существующие NewProxy() вызовы
// заменены на newProxyWithCleanup() (mechanical refactor в R52.4).
//
// Имя выбрано newProxyWithCleanup (а не newTestProxy) потому что
// virtual_router_test.go уже использует имя newTestProxy с другой
// сигнатурой (t *testing.T) → newTestProxy без config — мы не
// ломаем существующий helper.
package balancer

import (
	"context"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newProxyWithCleanup — wrapper вокруг NewProxy с автоматическим cleanup.
//
// Используй в тестах ВМЕСТО `NewProxy(conf)`:
//
//	proxy := newProxyWithCleanup(t, conf)  // cleanup registered automatically
//
// Вместо:
//
//	proxy := NewProxy(conf)
//	defer proxy.queueMgr.Stop()  // stops только queueMgr, НЕ остальные
//
// newProxyWithCleanup вызывает proxy.Shutdown с 5s timeout в t.Cleanup, что
// останавливает ВСЕ background goroutines: queueMgr, unloadScheduler,
// weightTuner, agentChecker, sessionMgr, llamaCppMetricsPoller,
// profileSyncer, и т.д.
func newProxyWithCleanup(t *testing.T, config *types.LoadBalancerConfig) *Proxy {
	t.Helper()
	p := NewProxy(config)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p
}
