// image_tool_load_fields_r159_test.go — R159 (2026-10-16): флейк
// TestGenerateImageForTool_LoadsNotLoadedModel.
//
// ЧТО БЫЛО: право на поля loadSeconds/loadedNow решалось значением часов
// (`if loadSeconds > 0`). Быстрая загрузка укладывается в один тик монотонных
// часов, time.Since даёт ровно 0 — оба поля молча пропадали, и tool-результат
// утверждал «загрузки не было», хотя POST load уходил и модель поднималась.
// Тест падал примерно 1 раз на 20 прогонов (в изоляции, не из-за соседей).
//
// КОНТРФАКТУАЛЬНОСТЬ: первый тест ниже НЕ ПРОХОДИТ на старом коде (старый гейт
// вернул бы пустую карту), потому что нулевая длительность при флаге загрузки
// обязана давать оба поля.
package balancer

import (
	"testing"
	"time"
)

// TestR159LoadFields_ZeroElapsedStillReportsLoad — главный контрпример: загрузка
// была, часы показали ноль — поля обязаны быть.
func TestR159LoadFields_ZeroElapsedStillReportsLoad(t *testing.T) {
	got := imageToolLoadFields(true, 0)
	if got == nil {
		t.Fatal("загрузка была, а полей нет — результат соврёт «загрузки не было»")
	}
	if got["loadedNow"] != true {
		t.Errorf("loadedNow=%v, want true", got["loadedNow"])
	}
	sec, ok := got["loadSeconds"].(float64)
	if !ok {
		t.Fatalf("loadSeconds отсутствует или не число: %+v", got)
	}
	if sec <= 0 {
		t.Errorf("loadSeconds=%v: ноль читается как «мгновенно», want > 0", sec)
	}
	if sec != 0.001 {
		t.Errorf("loadSeconds=%v, want 0.001 (нижняя граница разрешения)", sec)
	}
}

// TestR159LoadFields_SubMillisecondRoundsUp — всё быстрее миллисекунды не
// округляется в ноль.
func TestR159LoadFields_SubMillisecondRoundsUp(t *testing.T) {
	for _, d := range []time.Duration{1, 100 * time.Microsecond, 999 * time.Microsecond} {
		got := imageToolLoadFields(true, d)
		if sec, _ := got["loadSeconds"].(float64); sec != 0.001 {
			t.Errorf("%s: loadSeconds=%v, want 0.001", d, sec)
		}
	}
}

// TestR159LoadFields_MillisecondPrecision — обычные значения не теряют точности
// и не раздуваются.
func TestR159LoadFields_MillisecondPrecision(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want float64
	}{
		{time.Millisecond, 0.001},
		{1500 * time.Millisecond, 1.5},
		{12345 * time.Millisecond, 12.345},
		{90 * time.Second, 90},
	}
	for _, c := range cases {
		got := imageToolLoadFields(true, c.in)
		if sec, _ := got["loadSeconds"].(float64); sec != c.want {
			t.Errorf("%s: loadSeconds=%v, want %v", c.in, sec, c.want)
		}
	}
}

// TestR159LoadFields_NoLoadNoFields — обратная сторона: без загрузки полей нет
// даже при длинном elapsed (иначе модель объясняла бы задержку, которой не было).
func TestR159LoadFields_NoLoadNoFields(t *testing.T) {
	for _, d := range []time.Duration{0, 5 * time.Second} {
		if got := imageToolLoadFields(false, d); got != nil {
			t.Errorf("загрузки не было, а поля есть: %+v (.%s)", got, d)
		}
	}
}
