// throughput_ema_r598_test.go — R598 (2026-11-19): R495 S2b, EMA prefill/decode.
//
// ЗАЧЕМ. Классификатор «во что упёрлась карта» до S2b смотрел только на мгновенные
// метрики (SM busy, MemBusy, power/limit). Мгновенный снимок врёт в двух случаях:
// между запросами карта пуста, хотя предыдущий декод упирался в полосу памяти; на
// границе порога вердикт дёргается. Скорости префилла и декода — факт из
// завершённых запросов. Здесь проверяется арифметика и правила «тени»:
//
//  1. первое наблюдение становится значением (иначе первая медленная генерация
//     «прилипала» бы к нулю);
//  2. EMA считает α=0.3 (средняя помнит ~3 запроса);
//  3. нули и отрицательные НЕ попадают в статистику (ответ одним done-чанком не
//     даёт времени генерации — «0 tok/s» испортило бы среднее навсегда);
//  4. счётчики наблюдений растут независимо для префилла и декода;
//  5. ноль токенов или нулевое окно → 0 («не измерено»), а не деление на ноль;
//  6. снимок — копия: правка опубликованного не меняет состояние трекера.
//
//go:build llama_stub

package balancer

import (
	"math"
	"testing"
)

func within(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// TestThroughputEMA_R598_ПервоеНаблюдение — значение берётся как есть.
func TestThroughputEMA_R598_ПервоеНаблюдение(t *testing.T) {
	tr := newThroughputEMAR598()
	tr.observe("b1", 1200, 24)
	s := tr.snapshot()["b1"]
	within(t, s.PrefillTPS, 1200, "первый prefill")
	within(t, s.DecodeTPS, 24, "первый decode")
	if s.PrefillCount != 1 || s.DecodeCount != 1 {
		t.Errorf("счётчики = %d/%d, want 1/1", s.PrefillCount, s.DecodeCount)
	}
}

// TestThroughputEMA_R598_ШагEMA — второй замер смешивается с весом 0.3.
func TestThroughputEMA_R598_ШагEMA(t *testing.T) {
	tr := newThroughputEMAR598()
	tr.observe("b1", 1000, 20)
	tr.observe("b1", 2000, 40)
	s := tr.snapshot()["b1"]
	// 0.3*2000 + 0.7*1000 = 1300;  0.3*40 + 0.7*20 = 26
	within(t, s.PrefillTPS, 1300, "prefill после второго замера")
	within(t, s.DecodeTPS, 26, "decode после второго замера")
	if s.DecodeCount != 2 {
		t.Errorf("счётчик декода = %d, want 2", s.DecodeCount)
	}
}

// TestThroughputEMA_R598_НулиНеПортятСреднее — главная защита: «нет замера» не
// равно «медленно». Мутация «учитывать нули» ломает и это, и счётчики.
func TestThroughputEMA_R598_НулиНеПортятСреднее(t *testing.T) {
	tr := newThroughputEMAR598()
	tr.observe("b1", 900, 30)
	tr.observe("b1", 0, 0)   // нечего измерять: ответ одним done-чанком без стрима
	tr.observe("b1", -5, -1) // мусор
	s := tr.snapshot()["b1"]
	within(t, s.PrefillTPS, 900, "prefill не изменился от нулей")
	within(t, s.DecodeTPS, 30, "decode не изменился от нулей")
	if s.PrefillCount != 1 || s.DecodeCount != 1 {
		t.Errorf("счётчики = %d/%d, want 1/1: нули не должны считаться наблюдениями",
			s.PrefillCount, s.DecodeCount)
	}
}

// TestThroughputEMA_R598_СчётчикиНезависимы — префилл без декода (и наоборот) не
// должен сдвигать чужой счётчик.
func TestThroughputEMA_R598_СчётчикиНезависимы(t *testing.T) {
	tr := newThroughputEMAR598()
	tr.observe("b1", 1500, 0)
	tr.observe("b1", 0, 25)
	s := tr.snapshot()["b1"]
	if s.PrefillCount != 1 || s.DecodeCount != 1 {
		t.Errorf("счётчики = %d/%d, want 1/1", s.PrefillCount, s.DecodeCount)
	}
	within(t, s.PrefillTPS, 1500, "prefill")
	within(t, s.DecodeTPS, 25, "decode")
}

// TestThroughputEMA_R598_СнимокКопия — наружу отдаётся копия, состояние трекера
// не зависит от того, что с ней сделает вызывающий.
func TestThroughputEMA_R598_СнимокКопия(t *testing.T) {
	tr := newThroughputEMAR598()
	tr.observe("b1", 100, 10)
	snap := tr.snapshot()
	snap["b1"] = throughputSampleR598{DecodeTPS: 999}
	again := tr.snapshot()["b1"]
	within(t, again.DecodeTPS, 10, "трекер не изменился через снимок")
}

// TestTpsFromWindows_R598 — «не измерено» вместо деления на ноль.
func TestTpsFromWindows_R598(t *testing.T) {
	const sec = int64(1e9)
	within(t, tpsFromWindowsR598(100, sec), 100, "100 токенов за секунду")
	within(t, tpsFromWindowsR598(50, sec/2), 100, "50 токенов за полсекунды")
	within(t, tpsFromWindowsR598(0, sec), 0, "нет токенов → 0")
	within(t, tpsFromWindowsR598(100, 0), 0, "нет окна → 0")
	within(t, tpsFromWindowsR598(-5, sec), 0, "мусор → 0")
}

// TestThroughputSnapshot_НетДанных — без замеров снимка нет вовсе: пустой блок не
// рисуется в карточке.
func TestThroughputSnapshot_НетДанных(t *testing.T) {
	tr := newThroughputEMAR598()
	if snap := tr.snapshot(); snap != nil {
		t.Errorf("пустой трекер отдал снимок: %v", snap)
	}
	var nilTracker *throughputEMAR598
	if snap := nilTracker.snapshot(); snap != nil {
		t.Errorf("nil-трекер отдал снимок: %v", snap)
	}
	nilTracker.observe("b1", 1, 1) // не должно паниковать
}
