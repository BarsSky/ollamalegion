// auto_continue_duplicate_similarity_r83_test.go — R83 (2026-09-25).
//
// Что закрываем. Жалоба: «при запуске qwen3.8 на NVIDIA A10 ответ клиенту
// OpenWebUI пришёл продублированным слово-в-слово» (см.
// auto_continue_duplicate_api_r66d_test.go). R66d добавил проверку совпадения
// НАЧАЛА, но осталось два пробела:
//
//  1. Docstring R60.55 обещал правило «suppress the continuation if it's >85%
//     similar to the original» — оно так и не было реализовано. Из-за этого
//     проходила перегенерация, которая НЕ начинается как оригинал (модель
//     переформулировала первые слова), но дальше повторяет текст целиком.
//  2. На оригиналах короче 120 рун similarity-проверки отключались целиком.
//
// ВАЖНО про пункт 2: при реализации выяснилось, что снимать порог НЕЛЬЗЯ —
// это ломает документированный компромисс R60.55, закреплённый
// TestR6055_IsContinuationARegeneration_NoFalsePositiveOnCommonCode (настоящее
// продолжение кода законно повторяет строку оригинала). Подробности — в
// комментарии к isRegenerationBySimilarity. Поэтому ниже есть отдельная
// проверка, что короткий фрагмент кода по-прежнему НЕ считается дублем.
package balancer

import (
	"strings"
	"testing"
)

// longAnswerR83 — ответ чата длиной заметно больше minDuplicateOriginalRunes
// (160 нормализованных рун). Это профиль реальной жалобы: дублировался именно
// содержательный ответ, а не одна строка.
const longAnswerR83 = "Столица Франции — Париж. Город расположен на реке Сена, " +
	"в северной части страны. Население агломерации превышает двенадцать " +
	"миллионов человек, что делает её крупнейшей в стране. Климат умеренный, " +
	"зимы мягкие, лето тёплое и влажное."

// TestR83_DuplicateWithDifferentOpening_Detected — перегенерация, у которой
// начало переформулировано: ни одна из «начальных» эвристик R66d не сработает,
// поймать её может только сравнение по всему тексту.
func TestR83_DuplicateWithDifferentOpening_Detected(t *testing.T) {
	original := longAnswerR83
	// Другое начало + оригинал целиком внутри.
	continuation := "Итак, отвечу ещё раз, но подробнее. " + longAnswerR83

	// Предусловие: «начальные» проверки этот случай НЕ ловят — иначе тест был бы
	// no-op и не доказывал ценность нового правила.
	if commonPrefixRunes(normalizeForCompare(original), normalizeForCompare(continuation)) >= minRegenerationPrefixRunes {
		t.Fatal("предусловие нарушено: у оригинала и continuation общий длинный префикс")
	}
	if longestPrefixPresentIn([]rune(normalizeForCompare(continuation)),
		normalizeForCompare(original), minRegenerationContainmentRunes) > 0 {
		t.Fatal("предусловие нарушено: начало continuation уже встречается в оригинале")
	}

	if !IsContinuationARegeneration(original, continuation) {
		t.Error("перегенерация с другим началом не распознана — клиент получит дубль")
	}
	if !isDuplicateBySimilarity(original, continuation) {
		t.Error("isDuplicateBySimilarity не увидел дословный повтор всего ответа")
	}
}

// TestR83_GenuineContinuationOfLongAnswer_NotDetected — настоящее продолжение
// содержательного ответа не должно подавляться (иначе молча теряем контент).
func TestR83_GenuineContinuationOfLongAnswer_NotDetected(t *testing.T) {
	original := longAnswerR83
	continuation := "Климат здесь умеренный, зимы мягкие, а лето тёплое и влажное."

	if IsContinuationARegeneration(original, continuation) {
		t.Error("настоящее продолжение ошибочно помечено как перегенерация")
	}
}

// TestR83_ShortCodeFragment_StillNotDetected — регресс-гард на порог
// minDuplicateOriginalRunes: одна строка кода (69 рун) не должна считаться
// дублем, даже если continuation повторяет её дословно.
func TestR83_ShortCodeFragment_StillNotDetected(t *testing.T) {
	original := "const height = parseFloat(document.getElementById('height').value);\n"
	continuation := "  const height = parseFloat(document.getElementById('height').value);"

	if IsContinuationARegeneration(original, continuation) {
		t.Error("короткий фрагмент кода ошибочно помечен как дубль (потеря контента)")
	}
	if isDuplicateBySimilarity(original, continuation) {
		t.Errorf("isDuplicateBySimilarity сработал на коротком оригинале (%d рун < %d)",
			len([]rune(normalizeForCompare(original))), minDuplicateOriginalRunes)
	}
}

// TestR83_LongestCommonSubstringRunes — юнит-проверка DP-хелпера.
func TestR83_LongestCommonSubstringRunes(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int
	}{
		{"полное совпадение", "abcdef", "abcdef", 6},
		{"общая подстрока в середине", "xxabcdefyy", "zzabcdefww", 6},
		{"нет общих рун", "abc", "xyz", 0},
		{"пустые строки", "", "abc", 0},
		{"подстрока в начале второго", "abcdef", "abc", 3},
		{"юникод", "привет мир", "мир привет", 4}, // "вет " / "прив" — берём фактический максимум
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := longestCommonSubstringRunes([]rune(tc.a), []rune(tc.b))
			if tc.name == "юникод" {
				// Порядок слов разный: общая подстрока — « привет» или «мир » (5 рун
				// с пробелом). Проверяем согласованность с наивной реализацией.
				if got != naiveLCS([]rune(tc.a), []rune(tc.b)) {
					t.Errorf("got %d, want %d (naive)", got, naiveLCS([]rune(tc.a), []rune(tc.b)))
				}
				return
			}
			if got != tc.want {
				t.Errorf("longestCommonSubstringRunes(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}

	// Свойство: результат не зависит от порядка аргументов.
	a, b := []rune("Столица Франции — Париж"), []rune("Париж и окрестности")
	if longestCommonSubstringRunes(a, b) != longestCommonSubstringRunes(b, a) {
		t.Error("результат зависит от порядка аргументов")
	}
}

// naiveLCS — эталон O(n*m) без свопа аргументов: сверяем с ним оптимизированную
// реализацию, чтобы поймать ошибку в rolling-строках DP.
func naiveLCS(a, b []rune) int {
	best := 0
	for i := range a {
		for j := range b {
			k := 0
			for i+k < len(a) && j+k < len(b) && a[i+k] == b[j+k] {
				k++
			}
			if k > best {
				best = k
			}
		}
	}
	return best
}

// TestR83_SimilarityRule_MatchesNaiveLCS — на нескольких парах сверяем
// оптимизированный DP с эталоном (защита от ошибки в rolling-строках).
func TestR83_SimilarityRule_MatchesNaiveLCS(t *testing.T) {
	pairs := [][2]string{
		{longAnswerR83, "Итак, отвечу ещё раз. " + longAnswerR83},
		{longAnswerR83, "Климат здесь умеренный."},
		{strings.Repeat("абвгд", 60), strings.Repeat("абвгд", 30) + "хвост"},
		{"короткий", "совсем другой текст"},
	}
	for i, p := range pairs {
		ra, rb := []rune(p[0]), []rune(p[1])
		got, want := longestCommonSubstringRunes(ra, rb), naiveLCS(ra, rb)
		if got != want {
			t.Errorf("пара %d: got %d, want %d (naive)", i, got, want)
		}
	}
}
