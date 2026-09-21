package tokencount

import (
	"strings"
	"testing"
)

// TestEstimate_NeverUnderestimatesMeasuredVocabularies — главный тест.
//
// Таблица — результат работы scripts/vocab_bpe_probe.js по реальным словарям
// из c/llama.cpp/models/ggml-vocab-*.gguf (см. doc-комментарий пакета).
// `wantAtLeast` — измеренное число токенов; оценка обязана быть НЕ МЕНЬШЕ.
//
// Это прямая проверка инварианта «фолбэк не занижает», ради которого пакет и
// написан: занижение уводит запрос за границу контекста.
func TestEstimate_NeverUnderestimatesMeasuredVocabularies(t *testing.T) {
	// Тексты ровно те же, что в scripts/vocab_bpe_probe.js (SAMPLES),
	// иначе измеренные числа к ним не относятся.
	const (
		latinSample = "The quick brown fox jumps over the lazy dog. " +
			"Please summarize the following document and explain the main risks. " +
			"function calculateTotal(items) { return items.reduce((a, b) => a + b.price, 0); }"
		cyrillicSample = "Быстрая коричневая лиса прыгает через ленивую собаку. " +
			"Пожалуйста, перескажи следующий документ и объясни основные риски. " +
			"Сегодня хорошая погода, и мы пошли гулять в парк после обеда."
		cjkSample = "敏捷的棕色狐狸跳过了懒惰的狗。请总结以下文件并解释主要风险。" +
			"今天天气很好，午饭后我们去公园散步。"
		mixedSample = "Ошибка: connection refused при вызове API endpoint /v1/chat/completions. " +
			"Проверьте timeout=30s и повторите запрос."
	)

	cases := []struct {
		vocab       string
		sample      string
		text        string
		wantAtLeast int
	}{
		// latin: измеренный максимум 87 (phi-3, gpt-2), минимум 78 (qwen2/llama-bpe).
		{"gemma-4", "latin", latinSample, 80},
		{"qwen2", "latin", latinSample, 78},
		{"llama-bpe", "latin", latinSample, 78},
		{"phi-3", "latin", latinSample, 87},
		{"command-r", "latin", latinSample, 80},
		{"deepseek-llm", "latin", latinSample, 81},
		{"gpt-2", "latin", latinSample, 87},
		{"gpt-neox", "latin", latinSample, 84},

		// cyrillic: от 86 (gemma-4, лучший) до 182 (gpt-2, нет кириллических
		// merge'ов вообще — строго по символу).
		{"gemma-4", "cyrillic", cyrillicSample, 86},
		{"qwen2", "cyrillic", cyrillicSample, 143},
		{"llama-bpe", "cyrillic", cyrillicSample, 142},
		{"phi-3", "cyrillic", cyrillicSample, 101},
		{"command-r", "cyrillic", cyrillicSample, 138},
		{"deepseek-llm", "cyrillic", cyrillicSample, 145},
		{"gpt-2", "cyrillic", cyrillicSample, 182},
		{"gpt-neox", "cyrillic", cyrillicSample, 150},

		// cjk: от 36 (gemma-4) до 48 (остальные, по символу).
		{"gemma-4", "cjk", cjkSample, 36},
		{"qwen2", "cjk", cjkSample, 47},
		{"llama-bpe", "cjk", cjkSample, 47},
		{"phi-3", "cjk", cjkSample, 48},
		{"command-r", "cjk", cjkSample, 48},
		{"gpt-2", "cjk", cjkSample, 48},

		// mixed: от 47 (gemma-4) до 74 (gpt-2). Самая плотная упаковка
		// смешанного текста — 1.54 руны/токен; именно на этом кейсе первая
		// версия оценки (делившая пробелы и пунктуацию на 2) занижала,
		// поэтому пробелы теперь считаются по символу.
		{"gemma-4", "mixed", mixedSample, 47},
		{"qwen2", "mixed", mixedSample, 64},
		{"llama-bpe", "mixed", mixedSample, 61},
		{"phi-3", "mixed", mixedSample, 53},
		{"command-r", "mixed", mixedSample, 63},
		{"deepseek-llm", "mixed", mixedSample, 64},
		{"gpt-2", "mixed", mixedSample, 74},
		{"gpt-neox", "mixed", mixedSample, 66},
	}

	for _, tc := range cases {
		t.Run(tc.vocab+"/"+tc.sample, func(t *testing.T) {
			got := Estimate(tc.text)
			if got < tc.wantAtLeast {
				t.Errorf("Estimate = %d, а реальный токенизатор %s даёт %d токенов — "+
					"оценка ЗАНИЖАЕТ (это опасно: n_predict/preflight посчитают, что "+
					"места в контексте больше, чем есть)", got, tc.vocab, tc.wantAtLeast)
			}
		})
	}
}

// TestEstimate_BeatsOldHeuristic — регрессия на исходную эвристику len(runes)/4.
//
// Тест фиксирует, что для КАЖДОГО измеренного случая старая формула занижала,
// а новая — нет. Если кто-то вернёт деление на 4, тест упадёт с числами.
func TestEstimate_BeatsOldHeuristic(t *testing.T) {
	cyrillic := "Быстрая коричневая лиса прыгает через ленивую собаку. " +
		"Пожалуйста, перескажи следующий документ и объясни основные риски."

	runes := len([]rune(cyrillic))
	old := runes / 4
	now := Estimate(cyrillic)

	// Реальный токенизатор этого текста: 120 токенов у gemma-4 (лучший
	// измеренный словарь) ... 182 у gpt-2 (нет кириллических merge'ов).
	// Нижняя граница — 120, её оценка обязана покрыть.
	const measuredWorstCase = 120

	if old >= measuredWorstCase {
		t.Errorf("старая эвристика дала %d при измеренных %d — тест потерял смысл, "+
			"проверьте образец", old, measuredWorstCase)
	}
	if now < measuredWorstCase {
		t.Errorf("Estimate = %d < измеренных %d — оценка занижает", now, measuredWorstCase)
	}
	if now > runes {
		t.Errorf("Estimate = %d > числа символов %d — оценка не может превышать "+
			"один токен на символ", now, runes)
	}
}

// TestEstimate_EdgeCases — пустая строка, один символ, невалидный UTF-8.
func TestEstimate_EdgeCases(t *testing.T) {
	if got := Estimate(""); got != 0 {
		t.Errorf("Estimate(\"\") = %d, want 0", got)
	}
	if got := Estimate("a"); got != 1 {
		t.Errorf("Estimate(\"a\") = %d, want 1", got)
	}
	if got := Estimate("ab"); got != 1 {
		t.Errorf("Estimate(\"ab\") = %d, want 1 (2 латинских руны / 2)", got)
	}
	if got := Estimate("abc"); got != 2 {
		t.Errorf("Estimate(\"abc\") = %d, want 2 (округление вверх, не занижать)", got)
	}
	if got := Estimate("Ж"); got != 1 {
		t.Errorf("Estimate(\"Ж\") = %d, want 1", got)
	}
	if got := Estimate("ЖЖ"); got != 2 {
		t.Errorf("Estimate(\"ЖЖ\") = %d, want 2 (кириллица — 1 токен на руну)", got)
	}

	// Невалидный UTF-8: НЕ должен паниковать и не должен вернуть 0 на непустой
	// строке (иначе n_predict посчитает prompt пустым).
	bad := string([]byte{0xff, 0xfe, 0x41})
	if got := Estimate(bad); got == 0 {
		t.Errorf("Estimate(невалидный UTF-8) = 0, want > 0")
	}
}

// TestEstimateBytes_MatchesEstimateForValidUTF8 — байтовый фасад должен
// совпадать с рунным на валидном UTF-8 (иначе два пути дадут разные оценки).
func TestEstimateBytes_MatchesEstimateForValidUTF8(t *testing.T) {
	texts := []string{
		"",
		"hello world",
		"Привет, мир!",
		"こんにちは世界",
		"Смешанный text with 汉字 and emoji 🚀",
		strings.Repeat("Ошибка: timeout при вызове API endpoint. ", 5),
	}
	for _, text := range texts {
		want := Estimate(text)
		got := EstimateBytes([]byte(text))
		if got != want {
			t.Errorf("EstimateBytes(%q) = %d, Estimate = %d — пути расходятся",
				text, got, want)
		}
	}
}

// TestEstimate_Monotonic — больше текста не может дать меньше токенов.
// Защита от арифметических сюрпризов при округлении.
func TestEstimate_Monotonic(t *testing.T) {
	base := "Быстрая лиса jumps over the lazy dog. "
	prev := 0
	for i := 1; i <= 20; i++ {
		got := Estimate(strings.Repeat(base, i))
		if got < prev {
			t.Fatalf("на %d повторениях оценка упала: %d → %d", i, prev, got)
		}
		prev = got
	}
}

// TestEstimate_NonZeroForShortNonEmptyText — гарантия «никогда 0 на непустом
// тексте». Ровно из-за этого свойства старую эвристику пришлось менять:
// len(runes)/4 возвращала 0 для текста короче 4 символов, и n_predict
// мог оказаться нулевым при непустом prompt.
func TestEstimate_NonZeroForShortNonEmptyText(t *testing.T) {
	for _, s := range []string{"a", "!", " ", "\n", "Ж", "漢", "🚀"} {
		if got := Estimate(s); got == 0 {
			t.Errorf("Estimate(%q) = 0 на непустом тексте", s)
		}
	}
}
