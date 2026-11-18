// Package strutil — R507 (2026-11-19), шаг R7 плана
// docs/plans/2026-11-19-readability-refactor.md: одна реализация строковых
// хелперов.
//
// ПОЧЕМУ ТРИ ФУНКЦИИ, А НЕ ОДНА `firstNonEmpty`. В проекте было ЧЕТЫРЕ копии
// `firstNonEmpty`, и они расходились поведением:
//
//	pkg/version, internal/agent   — возвращает ОБРЕЗАННОЕ значение (TrimSpace);
//	internal/balancer             — пропускает пробельные строки, но возвращает
//	                                ИСХОДНОЕ значение (с пробелами);
//	internal/sdbackend            — берёт первую НЕПУСТУЮ строку без обрезки,
//	                                то есть " " для него — значение.
//
// Рефакторинг (§3 плана) не меняет поведение, поэтому здесь три реализации с
// ПОНЯТНЫМИ именами, а не одна «правильная». Разница, которая раньше была скрыта
// в четырёх копиях с одинаковым именем, теперь видна в имени функции, и её можно
// решать осознанно — отдельной задачей, а не «попутно».
package strutil

import "strings"

// FirstNonEmptyTrimmed — первое значение, у которого после TrimSpace есть
// содержимое; возвращается ОБРЕЗАННЫМ.
func FirstNonEmptyTrimmed(values ...string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// FirstNonEmptyRaw — первое значение, которое не пусто после TrimSpace;
// возвращается КАК ЕСТЬ (пробелы по краям сохраняются).
func FirstNonEmptyRaw(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// FirstNonEmptyAny — первая строка, которая не равна "", БЕЗ обрезки: строка из
// одних пробелов считается значением. Это поведение internal/sdbackend.
func FirstNonEmptyAny(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ContainsFold — содержит ли haystack needle без учёта регистра.
//
// РЕАЛИЗАЦИЯ СКОПИРОВАНА ИЗ internal/balancer БЕЗ ИЗМЕНЕНИЙ (R507): это побайтовое
// сравнение с понижением регистра ТОЛЬКО для ASCII. Написать здесь
// `strings.Contains(strings.ToLower(...))` было бы «чище», но это ДРУГОЕ поведение
// для не-ASCII (Unicode-свёртка регистра), а рефакторинг поведение не меняет.
// Если понадобится Unicode-вариант — это отдельная задача с отдельным тестом, а не
// «улучшение» при переносе.
func ContainsFold(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	hl := len(haystack)
	nl := len(needle)
	for i := 0; i+nl <= hl; i++ {
		match := true
		for j := 0; j < nl; j++ {
			h := haystack[i+j]
			n := needle[j]
			if h >= 'A' && h <= 'Z' {
				h += 'a' - 'A'
			}
			if n >= 'A' && n <= 'Z' {
				n += 'a' - 'A'
			}
			if h != n {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
