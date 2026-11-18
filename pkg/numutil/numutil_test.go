//go:build llama_stub

// numutil_test.go — R507 (2026-11-19), шаг R7 плана читаемости.
//
// Тест фиксирует ДВЕ разные семантики «подставить дефолт», которые раньше жили в
// двух пакетах под ОДНИМ именем `defaultIfZero`. Свести их в одну функцию значило
// бы изменить поведение, поэтому вариантов два и они названы по смыслу.
package numutil

import "testing"

// TestDefaultIfZeroInt — cmd/balancer: дефолт только при ровно нуле.
func TestDefaultIfZeroInt(t *testing.T) {
	cases := []struct{ v, def, want int }{
		{0, 10, 10},
		{5, 10, 5},
		{-5, 10, -5}, // отрицательное сохраняется
	}
	for _, c := range cases {
		if got := DefaultIfZeroInt(c.v, c.def); got != c.want {
			t.Errorf("DefaultIfZeroInt(%d, %d) = %d, ожидалось %d", c.v, c.def, got, c.want)
		}
	}
}

// TestDefaultIfNotPositiveFloat — internal/balancer: дефолт при нуле И
// отрицательных (отрицательный вес — мусор).
func TestDefaultIfNotPositiveFloat(t *testing.T) {
	cases := []struct{ v, def, want float64 }{
		{0, 1.5, 1.5},
		{-0.1, 1.5, 1.5},
		{2.5, 1.5, 2.5},
	}
	for _, c := range cases {
		if got := DefaultIfNotPositiveFloat(c.v, c.def); got != c.want {
			t.Errorf("DefaultIfNotPositiveFloat(%v, %v) = %v, ожидалось %v", c.v, c.def, got, c.want)
		}
	}
}

// TestFloatAndIntVariantsDiffer — контракт различия: для отрицательного значения
// варианты дают РАЗНОЕ. Если кто-то «объединит» их в одну функцию, этот тест
// покажет, какое поведение он сломал.
func TestFloatAndIntVariantsDiffer(t *testing.T) {
	if got := DefaultIfZeroInt(-5, 10); got != -5 {
		t.Fatalf("int-вариант вернул %d, ожидалось -5", got)
	}
	if got := DefaultIfNotPositiveFloat(-5, 10); got != 10 {
		t.Fatalf("float-вариант вернул %v, ожидалось дефолт 10", got)
	}
}
