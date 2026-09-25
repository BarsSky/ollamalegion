// load_singleflight_r83_test.go — R83: сериализация загрузок.
//
// ИНЦИДЕНТ, который закрывает этот тест. Балансер ждал авто-загрузку 180 с
// (LB_AUTO_LOAD_WAIT_SEC), отдавал клиенту «load started, waiting for cppworker
// to finish», загрузка продолжалась, клиент/автотюн повторяли запрос — и в
// cppworker уходили ДВА плана подряд (gpu_layers=33→29 и -2→18), пока первый ещё
// держал память:
//
//	[bridge] WARNING: VRAM insufficient for KV-cache with current gpu_layers=29
//	         (free=0 MB, gpu_model=15691 MB)
//	malloc(): unaligned tcache chunk detected   → SIGABRT → рестарт контейнера
//
// Причина: checkVRAMForModel считает бюджет VRAM ДО распределения, а guard-а
// «загрузка уже идёт» вокруг этой пары не было. Single-flight закрывает дыру.
package cppbackend

import (
	"context"
	"testing"
	"time"
)

// TestR83_LoadModelWithOpts_WaitsForInFlightLoad — пока загрузка идёт, вторая
// обязана ждать, а не стартовать параллельно на устаревшем снимке VRAM.
//
// Без single-flight тест падает: LoadModelWithOpts завершается сразу, потому что
// мьютекс не участвует в пути загрузки.
func TestR83_LoadModelWithOpts_WaitsForInFlightLoad(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir(), DefaultCtxSize: 32768, DefaultBatchSize: 512})

	// Эмулируем «загрузка уже идёт»: держим single-flight сами.
	b.loadSingleFlight.Lock()

	done := make(chan error, 1)
	go func() {
		done <- b.LoadModelWithOpts(context.Background(), "r83_singleflight_model", "", LoadModelOpts{
			ContextSize: 32768,
			BatchSize:   512,
			GPULayers:   -1,
		})
	}()

	select {
	case err := <-done:
		t.Fatalf("загрузка НЕ ждала in-flight загрузку (single-flight не работает), вернула: %v", err)
	case <-time.After(300 * time.Millisecond):
		// Ожидаемо: вторая загрузка ждёт освобождения.
	}

	// Освобождаем — вторая загрузка обязана продолжиться и завершиться.
	b.loadSingleFlight.Unlock()

	select {
	case err := <-done:
		if err != nil {
			// В stub-режиме загрузка может не найти файл — это не ошибка теста:
			// важно, что она ДОЖДАЛАСЬ и выполнилась, а не стартовала параллельно.
			t.Logf("после освобождения загрузка вернула: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("загрузка не завершилась после освобождения single-flight — возможен дедлок")
	}
}
