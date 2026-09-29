//go:build llama_stub

// cancel_watcher_order_r83_test.go — R83 (2026-09-29).
//
// ДЕФЕКТ, КОТОРЫЙ ЗДЕСЬ ЗАФИКСИРОВАН. В streamWithAbort (cmd/cppworker/inference.go)
// watcher отмены создавался ПОСЛЕ возврата из блокирующей генерации:
//
//	abortFlag, err := backend.GenerateStream(...)      // блокирует до конца генерации
//	if abortFlag != nil { NewAbortWatcher(ctx, abortFlag) }
//
// Флаг отмены — atomic_int на СТЕКЕ внутри bridge_infer_stream, и его адрес
// возвращался через out_abort_flag только при ВХОДЕ в C-вызов. Значит Go получал
// этот флаг, когда генерация уже завершилась, и ctx.Done() (клиент нажал Cancel /
// закрыл соединение) не мог ни на что повлиять: модель считала токены до n_predict.
// Именно это наблюдалось на живом стенде: «перестал отрабатывать cancel от клиента —
// модель продолжает генерацию судя по логам».
//
// Исправление: флаг создаётся ДО генерации (bridge.NewInferAbortFlag), watcher
// вооружается сразу, а флаг передаётся в C (bridge_infer_stream использует его
// вместо стекового). Проверяется функция beginInferCancellation — она и содержит
// весь порядок, не требуя ни загруженной модели, ни C-bridge.
//
// Сборка только под llama_stub: тесту нужен предсказуемый ответ фабрики флага.
package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// swapAbortFlagFactory подменяет фабрику abort-флага и возвращает restore.
func swapAbortFlagFactory(factory func() unsafe.Pointer) func() {
	prev := abortFlagFactory
	abortFlagFactory = factory
	return func() { abortFlagFactory = prev }
}

// TestR83_BeginInferCancellation_ArmsWatcherBeforeGeneration — главный
// regression-тест порядка.
//
// Условие «ctx уже отменён» моделирует клиента, который ушёл ДО старта генерации.
// При правильном порядке флаг выставляется сразу после вооружения watcher — то есть
// ДО того, как начнётся генерация. При старом порядке (watcher после генерации)
// в этой точке флага ещё нет.
func TestR83_BeginInferCancellation_ArmsWatcherBeforeGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // клиент уже отвалился

	flag := new(int32)
	abort := unsafe.Pointer(flag)

	restore := swapAbortFlagFactory(func() unsafe.Pointer { return abort })
	defer restore()

	got := beginInferCancellation(ctx)
	if got == nil {
		t.Fatal("beginInferCancellation вернул nil при непустой фабрике флага")
	}
	if got != abort {
		t.Fatal("beginInferCancellation вернул не тот флаг, что создала фабрика")
	}

	// Флаг обязан быть выставлен без всякой генерации: именно это означает
	// «watcher вооружён ДО старта». Раньше он создавался только после возврата
	// GenerateStream, поэтому здесь флаг оставался нулевым.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(flag) == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("abort-флаг не выставлен при отменённом ctx: watcher не вооружён до генерации "+
		"(старый порядок) — клиентский Cancel не остановит модель. Флаг=%d",
		atomic.LoadInt32(flag))
}

// TestR83_BeginInferCancellation_ResetsStaleFlag — перед новой попыткой флаг
// обязан сбрасываться, иначе отмена предыдущего запроса мгновенно убьёт следующий
// (актуально для RAM-fallback/reload, где генерация повторяется).
func TestR83_BeginInferCancellation_ResetsStaleFlag(t *testing.T) {
	flag := new(int32)
	atomic.StoreInt32(flag, 1) // «залипшая» отмена от прошлой генерации
	abort := unsafe.Pointer(flag)

	restore := swapAbortFlagFactory(func() unsafe.Pointer { return abort })
	defer restore()

	// ctx НЕ отменён: новая генерация должна получить чистый флаг.
	ctx := context.Background()
	got := beginInferCancellation(ctx)
	if got != abort {
		t.Fatal("beginInferCancellation вернул не тот флаг")
	}
	if v := atomic.LoadInt32(flag); v != 0 {
		t.Errorf("abort-флаг = %d после beginInferCancellation, want 0: залипший флаг от "+
			"предыдущей отмены прервёт новую генерацию", v)
	}

	// И он не должен «сам» выставиться: ctx живой.
	time.Sleep(50 * time.Millisecond)
	if v := atomic.LoadInt32(flag); v != 0 {
		t.Errorf("abort-флаг выставлен при живом ctx (%d) — watcher срабатывает преждевременно", v)
	}
}

// TestR83_BeginInferCancellation_NilFlagIsSafe — в stub-режиме фабрика возвращает
// nil (реального C-флага нет): функция обязана не паниковать и вернуть nil.
func TestR83_BeginInferCancellation_NilFlagIsSafe(t *testing.T) {
	restore := swapAbortFlagFactory(func() unsafe.Pointer { return nil })
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if got := beginInferCancellation(ctx); got != nil {
		t.Errorf("при nil-фабрике ожидался nil, получено %v", got)
	}
}
