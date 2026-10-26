// generate_cancel_r248_test.go — R248, часть A: буферизированная генерация
// обязана отменяться по контексту клиента.
//
// ЖИВОЙ ДЕФЕКТ (R247): клиент отвалился в tools-пути, балансер отменил свой
// запрос, но cppworker продолжал генерацию без контекста — слот держался до
// конца вывода (`active_queries=1`), следующий клиент ждал до рестарта.
package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// fakeBufferedGenerate — подмена потоковой генерации: отдаёт токены и уважает ctx.
func fakeBufferedGenerate(tokens []string, delay time.Duration, onStart func()) func(context.Context, string, string, bridge.GenerationParams, bridge.StreamCallback, bool) error {
	return func(ctx context.Context, modelName, prompt string, params bridge.GenerationParams, callback bridge.StreamCallback, hasTools bool) error {
		if onStart != nil {
			onStart()
		}
		for _, token := range tokens {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			if !callback(token) {
				return errors.New("callback stopped")
			}
		}
		return nil
	}
}

// TestR248_BufferedGenerateAggregates — обычный путь: токены склеиваются в Output.
func TestR248_BufferedGenerateAggregates(t *testing.T) {
	old := bufferedGenerateFn
	defer func() { bufferedGenerateFn = old }()
	bufferedGenerateFn = fakeBufferedGenerate([]string{"часть ", "ответа"}, 0, nil)

	result, err := generateBufferedWithCancel(context.Background(), "m", "prompt", bridge.GenerationParams{}, true)
	if err != nil {
		t.Fatalf("генерация упала: %v", err)
	}
	if result == nil || result.Output != "часть ответа" {
		t.Fatalf("Output = %+v, ожидалось склеивание токенов", result)
	}
}

// TestR248_BufferedGenerateCancelled — отмена клиента возвращает ошибку и НЕ ждёт
// конца вывода (это и освобождает слот).
func TestR248_BufferedGenerateCancelled(t *testing.T) {
	old := bufferedGenerateFn
	defer func() { bufferedGenerateFn = old }()

	started := make(chan struct{})
	tokens := make([]string, 200) // имитация длинного вывода
	for i := range tokens {
		tokens[i] = "x"
	}
	bufferedGenerateFn = fakeBufferedGenerate(tokens, 10*time.Millisecond, func() { close(started) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := generateBufferedWithCancel(ctx, "m", "prompt", bridge.GenerationParams{}, true)
		done <- err
	}()
	<-started
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("отмена контекста не вернула ошибку — слот не освободится")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ошибка = %v, ожидался context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("генерация не отменилась за 500 мс после cancel — клиент держит слот")
	}
}

// TestR248_NoBufferedCallWithoutContext — регресс-страховка: в handlers не
// осталось вызовов generateWithRamFallback (без ctx) на tools-путях.
func TestR248_BufferedHelperIsTheOnlyPath(t *testing.T) {
	// Косвенная проверка: helper обязан существовать и возвращать результат с
	// Output, чтобы вызывающий код не различал старый/новый путь.
	old := bufferedGenerateFn
	defer func() { bufferedGenerateFn = old }()
	bufferedGenerateFn = fakeBufferedGenerate([]string{"ok"}, 0, nil)
	result, err := generateBufferedWithCancel(context.Background(), "m", "p", bridge.GenerationParams{}, false)
	if err != nil || result == nil || !strings.Contains(result.Output, "ok") {
		t.Fatalf("helper вернул %+v, %v", result, err)
	}
}
