package balancer

// reasoning_loop_diag_r757_test.go — R757 (2026-11-25): признак «цикл формата».
//
// ЖИВАЯ ЖАЛОБА: «есть подозрение, не обязательно по этой модели, а по остальным
// тоже, что зацикливание при генерации осталось».
//
// По длине ответа цикл от подробного рассуждения не отличить. Отличить можно по
// числу ПЕРЕОТКРЫТИЙ канала размышлений: нормальный ответ открывает его один раз,
// а модель в форматном цикле начинает «думать» снова и снова. Это счётчик
// (behavior не меняется), и по нему финализация стрима пишет WARN — то есть
// следующий разбор идёт по факту, а не по догадке.

import (
	"strings"
	"testing"
)

// TestR757_SplitCountsReasoningOpens — счётчик открытий/закрытий канала.
func TestR757_SplitCountsReasoningOpens(t *testing.T) {
	var st reasoningSplitState
	// Один нормальный ответ: размышления → ответ.
	visible, reasoning := splitReasoningText("<|message_model|><|content_thinking|>думаю<|end_message|>"+
		"<|message_model|><|content_text|>ответ<|end_message|>", &st)
	if st.startHits != 1 {
		t.Fatalf("startHits = %d, want 1: нормальный ответ открывает размышления один раз", st.startHits)
	}
	if strings.TrimSpace(visible) != "ответ" {
		t.Errorf("visible = %q, want %q", visible, "ответ")
	}
	if strings.TrimSpace(reasoning) != "думаю" {
		t.Errorf("reasoning = %q, want %q", reasoning, "думаю")
	}
}

// TestR757_SplitDetectsFormatLoop — три и более открытий = цикл формата.
//
// Ровно то, что видно в живой жалобе: модель заканчивает блок размышлений и
// начинает новый, вместо того чтобы перейти к ответу и остановиться.
func TestR757_SplitDetectsFormatLoop(t *testing.T) {
	var st reasoningSplitState
	text := ""
	for i := 0; i < 4; i++ {
		text += "<|message_model|><|content_thinking|>размышление " + string(rune('1'+i)) + "<|end_message|>"
	}
	text += "<|message_model|><|content_text|>ответ<|end_message|>"
	_, _ = splitReasoningText(text, &st)

	if st.startHits < 3 {
		t.Fatalf("startHits = %d, want >= 3: четыре блока размышлений обязаны считаться "+
			"циклом формата — иначе цикл снова останется незамеченным", st.startHits)
	}
	if st.endHits < 4 {
		t.Errorf("endHits = %d, want >= 4", st.endHits)
	}
}

// TestR757_SplitCountsAcrossChunks — счётчик живёт на весь стрим, а не на чанк.
//
// Живой стрим приходит кусками: если считать только внутри чанка, счётчик почти
// всегда будет 0 и диагностика бесполезна.
func TestR757_SplitCountsAcrossChunks(t *testing.T) {
	var st reasoningSplitState
	chunks := []string{
		"начало<|message_model|><|content_thin",
		"king|>думаю<|end_me",
		"ssage|><|message_model|><|content_thinking|>ещё",
		"<|end_message|><|message_model|><|content_text|>ответ<|end_message|>",
	}
	for _, c := range chunks {
		_, _ = splitReasoningText(c, &st)
	}
	if st.startHits != 2 {
		t.Fatalf("startHits = %d, want 2: счётчик обязан переживать разрезанные между "+
			"чанками маркеры", st.startHits)
	}
}

// TestR757_PlainTextNoReasoning — обычный ответ без маркеров не даёт ложного цикла.
func TestR757_PlainTextNoReasoning(t *testing.T) {
	var st reasoningSplitState
	visible, reasoning := splitReasoningText("Просто ответ без размышлений.", &st)
	if st.startHits != 0 || st.endHits != 0 {
		t.Fatalf("startHits=%d endHits=%d, want 0/0: ложный признак цикла на обычном тексте",
			st.startHits, st.endHits)
	}
	if visible != "Просто ответ без размышлений." || reasoning != "" {
		t.Errorf("visible=%q reasoning=%q", visible, reasoning)
	}
}
