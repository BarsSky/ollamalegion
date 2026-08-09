// abort_watcher.go — Round 31 #6 (2026-08-09): spawn watcher goroutine для
// ctx.Done → bridge.RequestAbort.
//
// Использование:
//
//	ctx, cancel := context.WithCancel(r.Context())
//	defer cancel()
//	if handle, ok := backend.GetHandle(modelName); ok {
//	    _ = NewAbortWatcher(ctx, handle)  // fire-and-forget
//	}
//
// При ctx.Done() watcher немедленно вызывает bridge.RequestAbort(model).
// Cancel latency: между текущим и следующим llama_decode (~50-200ms).
//
// Precondition: ctx — cancellable (handler ВСЕГДА использует r.Context()).
// Если передан context.Background() — caller получит leaked goroutine.
// Документировано, чтобы будущие разработчики не делали эту ошибку.
//
// DECISION Q4 (PLAN.md §8): НЕТ safety timer. Handler ВСЕГДА использует
// r.Context() (cancellable), значит ctx.Done() сработает в течение минут.
// 24h safety timer был бы dead code.
package main

import (
	"context"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// AbortWatcher — обёртка над goroutine, которая при ctx.Done() вызывает
// bridge.RequestAbort для указанной модели. После выполнения goroutine
// завершается естественным путём; объект AbortWatcher остаётся как
// маркер "watcher активен" (для симметрии API и future extensibility).
type AbortWatcher struct {
	done chan struct{} // closed после завершения goroutine
}

// NewAbortWatcher запускает watcher goroutine для ctx → model.
// Goroutine блокируется на <-ctx.Done(), затем вызывает bridge.RequestAbort
// и завершается. Возвращает *AbortWatcher, но в production handler может
// безопасно игнорировать (fire-and-forget) — gc соберёт объект после
// завершения goroutine.
//
// Precondition: ctx cancellable, model != nil. Если ctx == nil или
// model == nil — возвращает nil без запуска goroutine (safe no-op).
func NewAbortWatcher(ctx context.Context, model *bridge.ModelHandle) *AbortWatcher {
	if ctx == nil || model == nil {
		// Defensive: не запускаем goroutine с невалидными аргументами.
		// Это защищает от случайной передачи context.Background() или
		// ещё не загруженной модели.
		return nil
	}
	w := &AbortWatcher{done: make(chan struct{})}
	go w.run(ctx, model)
	return w
}

// run — goroutine body. Блокируется на ctx.Done, вызывает RequestAbort,
// закрывает done для синхронизации.
func (w *AbortWatcher) run(ctx context.Context, model *bridge.ModelHandle) {
	defer close(w.done)
	<-ctx.Done() // Блокируется пока ctx не отменён
	log := logger.Get()
	if err := bridge.RequestAbort(model); err != nil {
		log.Warnw("AbortWatcher: bridge.RequestAbort failed", "error", err)
		return
	}
	log.Infow("AbortWatcher: abort requested via C-bridge", "ctx_err", ctx.Err().Error())
}

// Wait блокирует до завершения watcher goroutine. В production handler
// обычно не нужен (ctx отменён при возврате handler'а, watcher уже
// завершён или вот-вот завершится). Метод оставлен для тестов и для
// синхронизации при shutdown.
func (w *AbortWatcher) Wait() {
	if w == nil {
		return
	}
	<-w.done
}
