//go:build llama_stub

// empty_answer_r461_test.go — R461 (2026-11-06): пояснение «только размышления»
// не утверждает исчерпанный бюджет, которого не было.
//
// ЖИВОЙ ДЕФЕКТ: клиент постоянно показывал «модель сгенерировала только
// размышления, увеличьте max_tokens» при окне 131072 и невыбранном бюджете.
// R314 писал «(бюджет ответа исчерпан)» ВСЕГДА, потому что признак «видимого
// текста нет, а размышления были» этого не доказывает.
package main

import (
	"strings"
	"testing"
)

// TestR461_NeutralTextDoesNotClaimExhaustion — базовый текст (числа бюджета
// неизвестны) не должен содержать утверждения об исчерпании.
func TestR461_NeutralTextDoesNotClaimExhaustion(t *testing.T) {
	got := emptyAnswerNoticeForNativeR314("", 592)
	if got == "" {
		t.Fatal("пусто при размышлениях — пояснение обязано быть")
	}
	for _, lie := range []string{"исчерпан", "used up"} {
		if strings.Contains(got, lie) {
			t.Errorf("пояснение утверждает исчерпание бюджета без доказательства (%q): %s", lie, got)
		}
	}
	if !strings.Contains(got, "592") {
		t.Errorf("в пояснении нет числа символов размышлений: %s", got)
	}
}

// TestR461_BudgetHitSaysExhausted — бюджет выбран: можно утверждать.
func TestR461_BudgetHitSaysExhausted(t *testing.T) {
	got := emptyAnswerNoticeWithBudgetR461("", 300, 100, 100)
	if got == "" {
		t.Fatal("пояснение обязано быть")
	}
	if !strings.Contains(got, "исчерпан") && !strings.Contains(got, "used up") {
		t.Errorf("бюджет 100/100 выбран, а текст этого не говорит: %s", got)
	}
	if !strings.Contains(got, "100") {
		t.Errorf("нет чисел бюджета: %s", got)
	}
}

// TestR461_BudgetLeftSaysModelStopped — бюджет НЕ выбран: текст обязан это
// сказать (ровно живой случай, где max_tokens увеличивать было нечего).
func TestR461_BudgetLeftSaysModelStopped(t *testing.T) {
	got := emptyAnswerNoticeWithBudgetR461("", 300, 8192, 1200)
	if got == "" {
		t.Fatal("пояснение обязано быть")
	}
	lower := strings.ToLower(got)
	if !strings.Contains(lower, "не исчерпан") && !strings.Contains(lower, "not used up") {
		t.Errorf("бюджет 1200 из 8192 НЕ выбран, а текст молчит об этом: %s", got)
	}
	if !strings.Contains(got, "1200") || !strings.Contains(got, "8192") {
		t.Errorf("нет чисел «сгенерировано из бюджета»: %s", got)
	}
}

// TestR461_UnknownBudgetFallsBackToNeutral — бюджет неизвестен: не выдумываем.
func TestR461_UnknownBudgetFallsBackToNeutral(t *testing.T) {
	got := emptyAnswerNoticeWithBudgetR461("", 300, 0, 300)
	if got == "" {
		t.Fatal("пояснение обязано быть")
	}
	if strings.Contains(got, "исчерпан") || strings.Contains(got, "used up") {
		t.Errorf("бюджет неизвестен — утверждать исчерпание нельзя: %s", got)
	}
}

// TestR461_DoesNotTouchRealAnswers — правила R314/R394/R417 сохранены: непустой
// видимый ответ и случай «размышлений не было» не подменяются.
func TestR461_DoesNotTouchRealAnswers(t *testing.T) {
	if got := emptyAnswerNoticeWithBudgetR461("Готово: 15 яблок.", 300, 8192, 1200); got != "" {
		t.Errorf("непустой ответ подменён: %q", got)
	}
	if got := emptyAnswerNoticeWithBudgetR461("", 0, 8192, 1200); got != "" {
		t.Errorf("размышлений не было — пояснение не нужно: %q", got)
	}
}
