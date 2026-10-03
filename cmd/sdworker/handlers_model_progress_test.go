package main

import (
	"testing"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
)

// TestProgressTracker_ElapsedMS — R-Image Phase 9: время загрузки в снимке.
//
// ЗАЧЕМ ТЕСТ: до этой правки ElapsedMS всегда оставался нулём, и UI честно
// показывал «Загрузка sd15-q4: ready ()» — пустые скобки вместо времени, хотя
// загрузка весов идёт десятки секунд. Проверяем три свойства отсчёта:
//  1. на стадии загрузки время растёт (а не фиксируется нулём);
//  2. на терминальном состоянии оно фиксируется как итог;
//  3. на «unloaded» сбрасывается: длительность выгрузки оператору не нужна,
//     а старое значение в снимке читалось бы как «загрузка всё ещё идёт».
func TestProgressTracker_ElapsedMS(t *testing.T) {
	p := newProgressTracker()

	// Старт: первый set в состоянии loading открывает отсчёт.
	p.set("spawn", sdbackend.StateLoading, "sd15-q4", "")
	first := p.snapshot()
	if first.State != sdbackend.StateLoading {
		t.Fatalf("state=%q, want %q", first.State, sdbackend.StateLoading)
	}

	// Имитируем, что загрузка уже идёт 2 секунды: сдвигаем начало отсчёта в
	// прошлое (в проде это делают реальные тики времени между стадиями).
	p.lock()
	p.startedAt = time.Now().Add(-2 * time.Second)
	p.unlock()

	p.set("load weights", sdbackend.StateLoading, "sd15-q4", "")
	mid := p.snapshot()
	if mid.ElapsedMS < 1900 || mid.ElapsedMS > 3000 {
		t.Fatalf("elapsed_ms=%d на стадии загрузки, ожидали ~2000", mid.ElapsedMS)
	}

	// Терминальное состояние: время фиксируется и повторный set его не сбрасывает.
	p.set("ready", sdbackend.StateLoaded, "sd15-q4", "")
	done := p.snapshot()
	if done.ElapsedMS < 1900 {
		t.Fatalf("итоговое elapsed_ms=%d, ожидали ~2000", done.ElapsedMS)
	}
	p.set("", sdbackend.StateLoaded, "sd15-q4", "")
	if again := p.snapshot(); again.ElapsedMS != done.ElapsedMS {
		t.Fatalf("повторный set изменил итоговое время: %d -> %d", done.ElapsedMS, again.ElapsedMS)
	}

	// Выгрузка: время обнуляется, отсчёт закрыт.
	p.set("unloaded", sdbackend.StateNotLoaded, "sd15-q4", "")
	if un := p.snapshot(); un.ElapsedMS != 0 {
		t.Fatalf("после выгрузки elapsed_ms=%d, ожидали 0", un.ElapsedMS)
	}
	// Новый цикл загрузки начинает отсчёт заново, а не продолжает прошлый.
	p.set("spawn", sdbackend.StateLoading, "sd15-q4", "")
	restart := p.snapshot()
	if restart.ElapsedMS > 500 {
		t.Fatalf("новый цикл загрузки продолжил старый отсчёт: elapsed_ms=%d", restart.ElapsedMS)
	}
}

// TestProgressTracker_SSESubscriberGetsElapsed — подписчик SSE получает то же
// время, что и GET /api/image/models/load/progress: два транспорта одного
// снимка не должны расходиться (UI переключается между ними при обрыве потока).
func TestProgressTracker_SSESubscriberGetsElapsed(t *testing.T) {
	p := newProgressTracker()
	p.set("spawn", sdbackend.StateLoading, "sd15-q4", "")
	ch := p.subscribe()
	defer p.unsubscribe(ch)

	p.lock()
	p.startedAt = time.Now().Add(-1500 * time.Millisecond)
	p.unlock()
	p.set("load weights", sdbackend.StateLoading, "sd15-q4", "")

	select {
	case got := <-ch:
		if got.ElapsedMS < 1400 {
			t.Fatalf("в SSE пришло elapsed_ms=%d, ожидали ~1500", got.ElapsedMS)
		}
	case <-time.After(time.Second):
		t.Fatal("подписчик не получил обновление прогресса")
	}

	if snap := p.snapshot(); snap.ElapsedMS < 1400 {
		t.Fatalf("снимок GET-ручки elapsed_ms=%d, ожидали ~1500", snap.ElapsedMS)
	}
}
