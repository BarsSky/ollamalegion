// generate_cancel_r248.go — R248, часть A плана
// (docs/plans/2026-10-27-mmproj-vision-and-tools-cancel-r248.md).
//
// ЖИВОЙ ДЕФЕКТ (найден на R247). Клиент отвалился во время буферизированной
// генерации tools-пути: балансер отменяет свой запрос (r.Context()), но cppworker
// продолжал генерацию, потому что вызывал `generateWithRamFallback`, у которого
// НЕТ контекста и который идёт в `backend.Generate` (слот держится до конца
// вывода). Итог: `active_queries=1`, следующий клиент ждёт до рестарта воркера.
//
// РЕШЕНИЕ. Буферизированная генерация идёт через ТОТ ЖЕ потоковый путь, который
// уже умеет отмену (`generateStreamWithRamFallback` → abort-флаг по ctx.Done),
// а токены складываются в буфер. Семантика для вызывающего кода не меняется:
// на выходе те же Output/Status, что у `generateWithRamFallback`.
package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// bufferedGenerateFn — точка подмены для тестов.
var bufferedGenerateFn = generateStreamWithRamFallback

// generateBufferedWithCancel — буферизированная генерация, отменяемая по ctx.
//
// Клиент ушёл → ctx отменён → потоковый путь взводит abort-флаг → llama_decode
// останавливается на следующем токене → слот освобождается в defer, и следующий
// запрос не ждёт.
func generateBufferedWithCancel(ctx context.Context, modelName, prompt string, params bridge.GenerationParams, hasTools bool) (*bridge.InferenceResult, error) {
	var mu sync.Mutex
	var buf strings.Builder
	callback := func(token string) bool {
		mu.Lock()
		buf.WriteString(token)
		mu.Unlock()
		return true
	}
	started := time.Now()
	err := bufferedGenerateFn(ctx, modelName, prompt, params, callback, hasTools)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	output := buf.String()
	mu.Unlock()
	// Информация о времени нужна диагностике/метрикам; в InferenceResult других
	// полей, которые заполнял бы backend.Generate, вызывающие не используют.
	_ = started
	return &bridge.InferenceResult{Output: output}, nil
}
