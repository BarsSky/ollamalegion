//go:build llama_stub

// strutil_test.go — R507 (2026-11-19), шаг R7 плана читаемости.
//
// Тест держит ТРИ поведения firstNonEmpty, которые раньше жили в четырёх копиях
// с одинаковым именем и расходились обработкой пробелов. Рефакторинг (§3 плана)
// поведение не меняет, поэтому вариантов три, а не один «правильный»: тест
// фиксирует именно то, что делает каждый вызывающий код.
package strutil

import "testing"

// TestFirstNonEmptyTrimmed — pkg/version и internal/agent: возвращают ОБРЕЗАННОЕ
// значение, пробельные строки пропускают.
func TestFirstNonEmptyTrimmed(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"", " ", "  value  "}, "value"},
		{[]string{"first", "second"}, "first"},
		{[]string{"", "   "}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := FirstNonEmptyTrimmed(c.in...); got != c.want {
			t.Errorf("FirstNonEmptyTrimmed(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}

// TestFirstNonEmptyRaw — internal/balancer: пробельные строки пропускает, но
// возвращает ИСХОДНОЕ значение (пробелы по краям сохраняются).
func TestFirstNonEmptyRaw(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"", "  value  "}, "  value  "},
		{[]string{"first", "second"}, "first"},
		{[]string{" ", "x"}, "x"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := FirstNonEmptyRaw(c.in...); got != c.want {
			t.Errorf("FirstNonEmptyRaw(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}

// TestFirstNonEmptyAny — internal/sdbackend: строка из одних пробелов считается
// значением (обрезки нет).
func TestFirstNonEmptyAny(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"", " ", "value"}, " "},
		{[]string{"", "value"}, "value"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := FirstNonEmptyAny(c.in...); got != c.want {
			t.Errorf("FirstNonEmptyAny(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}

// TestContainsFoldMatchesBalancerBehaviour — перенос из internal/balancer не
// изменил семантику: побайтовое сравнение с понижением регистра только для ASCII.
func TestContainsFoldMatchesBalancerBehaviour(t *testing.T) {
	cases := []struct {
		haystack, needle string
		want             bool
	}{
		{"Qwen3-8B-Instruct", "qwen3-8b", true},
		{"Qwen3-8B-Instruct", "INSTRUCT", true},
		{"model", "longneedle", false},
		{"", "", true}, // пустая игла: смещение 0 в пустой строке — совпадение
	}
	for _, c := range cases {
		if got := ContainsFold(c.haystack, c.needle); got != c.want {
			t.Errorf("ContainsFold(%q, %q) = %v, ожидалось %v", c.haystack, c.needle, got, c.want)
		}
	}
}
