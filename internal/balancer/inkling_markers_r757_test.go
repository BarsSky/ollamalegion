// inkling_markers_r757_test.go — R757 (2026-11-25): маркеры Inkling-2
// (architecture=inkling2_loop) на границе балансера.
//
// ЖИВОЙ ДЕФЕКТ (архив второго стенда): ответ модели приходил в виде
// `<|message_model|><|content_thinking|>…<|end_message|>…`, клиент показывал
// служебные маркеры и рассуждения в content, а `reasoning_chars` был 0.
//
// ЧТО ПРОВЕРЯЕТСЯ: даже если воркер СТАРОЙ версии не разделил ответ, балансер
// обязан вырезать маркеры конверта из видимого текста и не отдавать
// рассуждения как ответ (единый список llamaCppReasoningMarkers +
// stripReasoningTags + reasoning-egress канал).
package balancer

import (
	"strings"
	"testing"
)

const inklingEnvelopeR757 = "<|message_model|><|content_thinking|>план ответа" +
	"<|end_message|><|message_model|><|content_text|>готовый ответ<|end_message|>"

func TestInkling2Markers_ReasoningEgress_R757(t *testing.T) {
	st := &reasoningSplitState{}
	visible, reasoning := splitReasoningText(inklingEnvelopeR757, st)
	visible = strings.TrimSpace(visible)
	reasoning = strings.TrimSpace(reasoning)
	if visible != "готовый ответ" {
		t.Fatalf("visible = %q, want «готовый ответ»", visible)
	}
	if reasoning != "план ответа" {
		t.Fatalf("reasoning = %q, want «план ответа»", reasoning)
	}
}

func TestInkling2Markers_StripReasoningTags_R757(t *testing.T) {
	got := strings.TrimSpace(stripReasoningTags(inklingEnvelopeR757))
	if got != "готовый ответ" {
		t.Fatalf("stripReasoningTags = %q, want «готовый ответ»", got)
	}
	if strings.Contains(got, "<|") {
		t.Fatalf("в видимом тексте остались маркеры Inkling-2: %q", got)
	}
}

func TestInkling2Markers_ServiceTokens_R757(t *testing.T) {
	got, removed := stripLlamaCppServiceTokens(inklingEnvelopeR757)
	if removed == 0 {
		t.Fatal("служебные маркеры Inkling-2 не распознаны")
	}
	for _, marker := range []string{"<|message_model|>", "<|content_text|>", "<|end_message|>"} {
		if strings.Contains(got, marker) {
			t.Fatalf("в тексте остался маркер %q: %q", marker, got)
		}
	}
	if !strings.Contains(got, "готовый ответ") {
		t.Fatalf("полезный текст потерян: %q", got)
	}
}

func TestInkling2Markers_VisibleStrip_R757(t *testing.T) {
	got, removed := stripReasoningMarkersFromVisible(inklingEnvelopeR757)
	if removed == 0 {
		t.Fatal("markers not detected")
	}
	if strings.Contains(got, "<|content_thinking|>") || strings.Contains(got, "<|message_model|>") {
		t.Fatalf("маркеры остались в видимом тексте: %q", got)
	}
}
