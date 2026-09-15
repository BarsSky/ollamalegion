// abort_watcher.go — Round 31 #6 (2026-08-09) + R63 (2026-09-15):
// spawn watcher goroutine для ctx.Done → bridge_set_infer_abort.
//
// Использование:
//
//	abortFlag := backend.GetInferAbortFlag(modelName) // *int32 (per-inference)
//	defer abortWatcher.Release(abortFlag)
//	_ = NewAbortWatcher(ctx, abortFlag)              // fire-and-forget
//
// При ctx.Done() watcher немедленно вызывает bridge_set_infer_abort(flag, 1).
// Cancel latency: между текущим и следующим llama_decode (~50-200ms).
//
// R63: per-inference abort flag вместо per-model. До R63 был
// bridge.RequestAbort(model) — per-model atomic flag, и любая отмена одной
// горутины сбрасывала ВСЕ параллельные infers для той же модели.
// Это ломало OpenWebUI streaming при многопользовательской нагрузке —
// один cancel от OpenWebUI отменял streaming для всех остальных клиентов.
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
	"sync/atomic"
	"unsafe"

	"ollama-loadbalancer/pkg/logger"
)

// AbortWatcher — обёртка над goroutine, которая при ctx.Done() вызывает
// atomic.StoreInt32 на per-inference abort flag. После выполнения goroutine
// завершается естественным путём; объект AbortWatcher остаётся как
// маркер "watcher активен" (для симметрии API и future extensibility).
type AbortWatcher struct {
	done chan struct{} // closed после завершения goroutine
}

// NewAbortWatcher запускает watcher goroutine для ctx → abortFlag.
//
// abortFlag — указатель на per-inference atomic int32 (см. bridge.SetInferAbort).
// C-bridge создаёт этот flag на стеке внутри bridge_infer / bridge_infer_stream,
// затем возвращает unsafe.Pointer через out-параметр; Go-сторона хранит его
// в local variable или структуре handler'а.
//
// Goroutine блокируется на <-ctx.Done(), затем делает atomic.StoreInt32(flag, 1)
// и завершается. Возвращает *AbortWatcher, но в production handler может
// безопасно игнорировать (fire-and-forget) — gc соберёт объект после
// завершения goroutine.
//
// Precondition: ctx cancellable, abortFlag != nil. Если ctx == nil или
// abortFlag == nil — возвращает nil без запуска goroutine (safe no-op).
func NewAbortWatcher(ctx context.Context, abortFlag unsafe.Pointer) *AbortWatcher {
	if ctx == nil || abortFlag == nil {
		// Defensive: не запускаем goroutine с невалидными аргументами.
		// Это защищает от случайной передачи context.Background() или
		// nil flag.
		return nil
	}
	w := &AbortWatcher{done: make(chan struct{})}
	go w.run(ctx, abortFlag)
	return w
}

// run — goroutine body. Блокируется на ctx.Done, ставит per-infer abort flag, и завершается.
func (w *AbortWatcher) run(ctx context.Context, abortFlag unsafe.Pointer) {
	defer close(w.done)
	<-ctx.Done() // Блокируется пока ctx не отменён
	// R63: ставим ТОЛЬКО per-inference flag. Другие параллельные infers для
	// той же модели не затрагиваются — это fix для R60.58 per-model race.
	atomic.StoreInt32((*int32)(abortFlag), 1)
	log := logger.Get()
	log.Infow("AbortWatcher: per-infer abort set via C atomic flag", "ctx_err", ctx.Err().Error())
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
