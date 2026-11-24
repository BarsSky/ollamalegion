// requestid_helpers_race_test.go — гонка в lr_recentCtx (R735, CI-дефект).
//
// ЖИВОЙ ДЕФЕКТ (прогон 45307667015, job «Test (self-hosted Windows)»):
//
//	WARNING: DATA RACE
//	Write at 0x000140edd0b0 by goroutine 10377:
//	  balancer.lr_recentCtx()  internal/balancer/requestid_helpers.go:28
//	  … queryCppWorkerModels() → isModelReadyOnBackend() → waitModelReadyForAutoLoad()
//	Previous read at 0x000140edd0b0 by goroutine 10430:
//	  balancer.lr_recentCtx()  internal/balancer/requestid_helpers.go:31
//	  … ensureModelLoadedOnBackend()
//	--- FAIL: TestOpenWebUI_Sequential_MixedRequests/step6-llamacpp-non-streaming
//	    testing.go:1617: race detected during execution of test
//
// Причина: «ротация» раз в 5 минут писала в пакетные переменные
// lr_recentCtxValue/lr_recentCtxRotatedAt БЕЗ синхронизации, а читал их каждый
// вызов, то есть два параллельных HTTP-запроса балансера. При этом ротация
// присваивала ровно то же значение ("norequest") — то есть не меняла ничего,
// кроме состояния гонки.
//
// Тест воспроизводит обращение из многих горутин: под `-race` он краснеет на
// старом коде детерминированно (несколько сотен итераций), а на исправленном
// остаётся зелёным. Проверяется и смысл: request_id в контексте — "norequest".
package balancer

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestR735_LrRecentCtxIsRaceFree — параллельные вызовы lr_recentCtx не пишут в
// общие пакетные переменные.
//
// Именно этот сценарий давал красный CI: загрузка модели в одной горутине
// (ensureModelLoadedOnBackend) и поллинг готовности в другой
// (waitModelReadyForAutoLoad) читают fallback-контекст одновременно.
func TestR735_LrRecentCtxIsRaceFree(t *testing.T) {
	const goroutines = 32
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ctx := lr_recentCtx()
				if got, _ := ctx.Value(requestIDKey{}).(string); got != "norequest" {
					t.Errorf("request_id = %q, ожидался norequest", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestR735_NoPackageMutableStateInHelper — в файле fallback-контекста НЕТ
// пакетных изменяемых переменных.
//
// ЗАЧЕМ СТАТИЧЕСКАЯ ПРОВЕРКА, ЕСЛИ ЕСТЬ ТЕСТ ВЫШЕ. Гонка из CI проявлялась
// только на границе «прошло 5 минут с прошлой ротации»: ротация писала, обычные
// вызовы читали. Без управления часами такую гонку не поймать каждым прогоном,
// а держать в тесте пакетные переменные, которых больше нет, нельзя. Запрет
// пакетного изменяемого состояния ловит регресс ДЕТЕРМИНИРОВАННО: хочешь
// кэшировать — заводи структуру с мьютексом и передавай её явно.
func TestR735_NoPackageMutableStateInHelper(t *testing.T) {
	src, err := os.ReadFile("requestid_helpers.go")
	if err != nil {
		t.Fatalf("не читается requestid_helpers.go: %v", err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		// Верхнеуровневые `var …` — единственная форма пакетного изменяемого
		// состояния, которая здесь была (lr_recentCtxValue / …RotatedAt).
		if strings.HasPrefix(trimmed, "var ") && !strings.HasPrefix(trimmed, "var (") {
			t.Errorf("requestid_helpers.go:%d: пакетная переменная %q — общий mutable state "+
				"в функции без синхронизации даёт гонку (R735: WARNING: DATA RACE в CI на "+
				"lr_recentCtx)", i+1, trimmed)
		}
	}
}

// TestR735_LrRecentCtxIsIndependent — каждый вызов возвращает собственный контекст:
// запись значения в один не влияет на другой (иначе «singleton» подменял бы
// correlation-id соседнего запроса).
func TestR735_LrRecentCtxIsIndependent(t *testing.T) {
	ctx1 := lr_recentCtx()
	ctx2 := context.WithValue(ctx1, requestIDKey{}, "req-42")
	if got, _ := ctx2.Value(requestIDKey{}).(string); got != "req-42" {
		t.Fatalf("производный контекст потерял значение: %q", got)
	}
	if got, _ := lr_recentCtx().Value(requestIDKey{}).(string); got != "norequest" {
		t.Fatalf("lr_recentCtx вернул чужой request_id: %q", got)
	}
}
