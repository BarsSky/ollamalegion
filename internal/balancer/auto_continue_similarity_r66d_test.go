//go:build llama_stub

// auto_continue_similarity_r66d_test.go — R66d (2026-09-23).
//
// ЖАЛОБА: «при запуске qwen3.8 на NVIDIA A10 без reasoning ответ клиенту
// OpenWebUI пришёл продублированным слово-в-слово».
//
// Причина: LB_AUTO_CONTINUE_ON_TRUNCATION=1 + LB_AUTO_CONTINUE_CHAT_POLICY=smart
// (default). Политика smart обещала «suppress the continuation if it's >85%
// similar to the original», но в IsContinuationARegeneration проверялись только
// приветствия/преамбулы/ChatML-утечка. Модель, которая перегенерировала ответ
// БЕЗ приветствия (сразу тем же текстом), не детектилась → continuation
// эмитился клиенту → ответ дублировался.
//
// Здесь проверяются все три признака регенерации и, что важнее, отсутствие
// ложных срабатываний на настоящем продолжении.
package balancer

import "testing"

func TestIsContinuationARegeneration_SimilarityR66d(t *testing.T) {
	// Реальный паттерн жалобы: ответ оборвался, continuation повторяет ответ с
	// начала, без приветствия/преамбулы.
	origRu := "Столица Франции — Париж. Город расположен на реке Сена, " +
		"в северной части страны. Население агломерации превышает 12 миллионов человек."
	regenRu := "Столица Франции — Париж. Город расположен на реке Сена, " +
		"в северной части страны. Население агломерации превышает 12 миллионов человек, " +
		"что делает его крупнейшим городом страны."

	// Тот же кейс: модель начала заново и продолжила дальше (A → A+B).
	// Оригинал длинный — как реальный ответ на вопрос (см.
	// minRegenerationOriginalRunes: на коротких фрагментах проверка отключена).
	origCode := "```go\nfunc main() {\n\tfmt.Println(\"start\")\n\tfor i := 0; i < 10; i++ {\n" +
		"\t\tfmt.Printf(\"iteration %d of the loop\\n\", i)\n\t\tif i%2 == 0 {\n\t\t\ttotal += i\n\t\t}"
	regenCode := origCode + "\n\t}\n\tfmt.Println(\"done\", total)\n}\n```"

	tests := []struct {
		name string
		orig string
		cont string
		want bool
	}{
		{
			name: "regeneration_word_for_word_russian",
			orig: origRu,
			cont: regenRu,
			want: true,
		},
		{
			name: "regeneration_original_is_prefix_of_continuation",
			orig: origCode,
			cont: regenCode,
			want: true,
		},
		{
			name: "regeneration_continuation_repeats_original_fragment",
			orig: "Первое предложение ответа достаточно длинное, чтобы работать как якорь. " +
				"Второе предложение тоже присутствует в оригинале и повторяется моделью.",
			cont: "Второе предложение тоже присутствует в оригинале и повторяется моделью. " +
				"И дальше модель продолжает уже новым текстом, которого раньше не было.",
			want: true,
		},
		{
			name: "genuine_continuation_mid_code",
			orig: "```python\nimport numpy as np\nv0 = 50.0\nfor i in range(10):\n    t = i * 2\n    v = v0 + t",
			cont: "\n    print(i, v)\n```\n\nФункция печатает значения скорости на каждом шаге.",
			want: false,
		},
		{
			name: "genuine_continuation_mid_sentence",
			orig: "Чтобы настроить балансировщик, откройте файл конфигурации и добавьте " +
				"секцию balancing. Затем укажите список бэкендов и их веса, после чего",
			cont: " перезапустите контейнер: docker compose up -d --force-recreate balancer.",
			want: false,
		},
		{
			name: "short_continuation_not_flagged",
			orig: "Длинный оригинальный ответ, который обрывается на середине предло",
			cont: "жении.",
			want: false,
		},
		{
			name: "empty_inputs_not_flagged",
			orig: "",
			cont: "что-то",
			want: false,
		},
		{
			name: "chatml_leak_still_flagged",
			orig: "Ответ модели, который был обрезан на середине and it is long enough to compare",
			cont: "<|im_start|>assistant\nПривет! Чем могу помочь?",
			want: true,
		},
		{
			name: "greeting_preamble_still_flagged",
			orig: "Ответ модели, который был обрезан на середине and it is long enough to compare",
			cont: "Конечно! Вот продолжение вашего ответа, начиная с того места, где он оборвался.",
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsContinuationARegeneration(tc.orig, tc.cont)
			if got != tc.want {
				t.Errorf("IsContinuationARegeneration() = %v, want %v\norig: %q\ncont: %q",
					got, tc.want, tc.orig, tc.cont)
			}
		})
	}
}

// TestCommonPrefixRunes — вспомогательная функция сравнения.
func TestCommonPrefixRunes(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"abc", "abd", 2},
		{"", "abc", 0},
		{"abc", "", 0},
		{"привет мир", "привет мир!", 10},
		{"разные", "тексты", 0},
	}
	for _, tc := range tests {
		if got := commonPrefixRunes(tc.a, tc.b); got != tc.want {
			t.Errorf("commonPrefixRunes(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestNormalizeForCompare_WhitespaceAndCase — нормализация не должна зависеть от
// переносов строк/отступов (иначе сравнение кода и markdown ломается).
func TestNormalizeForCompare_WhitespaceAndCase(t *testing.T) {
	a := normalizeForCompare("Hello,\n\tWORLD   again")
	b := normalizeForCompare("hello, world again")
	if a != b {
		t.Errorf("normalizeForCompare mismatch: %q != %q", a, b)
	}
}
