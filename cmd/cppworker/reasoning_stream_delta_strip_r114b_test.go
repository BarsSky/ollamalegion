// reasoning_stream_delta_strip_r114b_test.go — R114b, вторая часть симптома.
//
// ЖИВОЙ ДЕФЕКТ (скриншот оператора, gemma-4-E4B-it-Q4_K_M через стенд :18080):
// строка видимого ответа клиента начиналась с «Let me retry the command. </think>»
// — закрывающий маркер размышлений утёк в content ПОТОКОВОЙ дельтой.
//
// Почему: одиночный маркер режется только в НЕ-потоковых путях — через
// cleanFinalContent → stripOrphanReasoningTags (cmd/cppworker/tool_calls.go:1750).
// В стриме же content delta формируется в callback'ах
// writeOpenAIChatStream (handlers_openai.go, emit delta.content) и
// writeChatStreamResponse (handlers_chat.go, message.content) — и уходила
// клиенту сырой, минуя любую чистку маркеров.
package main

import (
	"strings"
	"testing"
)

// TestR114b_StreamingDeltas_OrphanThinkCloseDoesNotLeak — главный кейс скриншота:
// одиночный </think> приходит отдельным чанком, следом идёт видимый ответ.
// В дельтах content маркера быть не должно.
func TestR114b_StreamingDeltas_OrphanThinkCloseDoesNotLeak(t *testing.T) {
	tokens := []string{"</think>", "Let me retry the command."}
	content := streamingContentDeltas(t, tokens)
	if strings.Contains(content, "</think>") || strings.Contains(content, "<think") {
		t.Fatalf("в delta.content утёк маркер размышлений: %q", content)
	}
	if !strings.Contains(content, "Let me retry the command.") {
		t.Fatalf("ответ потерян: %q", content)
	}
}

// TestR114b_StreamingDeltas_PrefillOrphanCloseKeepsAnswer — форма prefill-канала:
// шаблон открыл канал в промпте, модель отдаёт только закрывающий маркер и ответ.
func TestR114b_StreamingDeltas_PrefillOrphanCloseKeepsAnswer(t *testing.T) {
	content := streamingContentDeltas(t, []string{"Let me retry the command. </think>", "Готово."})
	if strings.Contains(content, "</think>") {
		t.Fatalf("в delta.content утёк </think>: %q", content)
	}
	if !strings.Contains(content, "Готово.") {
		t.Fatalf("ответ потерян: %q", content)
	}
}

// TestR114b_StreamingDeltas_PairedBlockDoesNotLoseAnswer — регресс на парную
// форму. Поток с ДВУМЯ парами: первая целиком уходит в reasoning-канал, вторая
// (парсер отдаёт её как остаток после первой пары) — уже delta.content. Теги не
// должны утечь, а «c» — потеряться.
func TestR114b_StreamingDeltas_PairedBlockDoesNotLoseAnswer(t *testing.T) {
	content := streamingContentDeltas(t, []string{"<think>a</think><think>b</think>c"})
	if strings.Contains(content, "<think>") || strings.Contains(content, "</think>") {
		t.Fatalf("в delta.content утекли теги пары: %q", content)
	}
	if content != "c" {
		t.Fatalf("видимый ответ искажён: %q, ожидалось %q", content, "c")
	}
}

// TestR114b_StreamingDeltas_ReasoningFieldUnaffected — если модель отдала
// размышления ОТДЕЛЬНЫМ полем (reasoning_content), поведение не меняется:
// reasoning приходит как раньше, content маркеров не содержит.
func TestR114b_StreamingDeltas_ReasoningFieldUnaffected(t *testing.T) {
	st := NewReasoningStreamState()
	var reasoning, content strings.Builder
	for _, tok := range []string{"<think>", "думаю", "</think>", "Ответ: 4."} {
		r, c := st.Feed(tok)
		reasoning.WriteString(r)
		content.WriteString(stripReasoningTagsInStreamDelta(c))
	}
	if !strings.Contains(reasoning.String(), "думаю") {
		t.Fatalf("reasoning_content потерян: %q", reasoning.String())
	}
	if strings.Contains(content.String(), "</think>") {
		t.Fatalf("в content утёк маркер: %q", content.String())
	}
	if !strings.Contains(content.String(), "Ответ: 4.") {
		t.Fatalf("видимый ответ потерян: %q", content.String())
	}
}

// TestR114b_StreamDeltaStripCases — построчная проверка чистки дельты:
// одиночный `</think>` (в том числе без пробела) + регресс на пару
// `<think>a</think>b`, где «b» терять нельзя.
func TestR114b_StreamDeltaStripCases(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"одиночный закрывающий (кейс скриншота)", "</think>", ""},
		{"закрывающий + ответ", "</think>Let me retry the command.", "Let me retry the command."},
		{"закрывающий приклеен к концу ответа", "Let me retry the command.</think>", "Let me retry the command."},
		{"пара целиком", "<think>a</think>b", "b"},
		{"пара в окружении текста", "до <think>a</think> после", "до  после"},
		{"чужая пара", "<thinking>a</thinking> b", " b"},
		{"текст без маркеров", "обычный ответ", "обычный ответ"},
		{"только текст ответа", "Ответ: 4.", "Ответ: 4."},
	}
	for _, tc := range cases {
		if got := stripReasoningTagsInStreamDelta(tc.in); got != tc.want {
			t.Errorf("%s: stripReasoningTagsInStreamDelta(%q) = %q, ожидалось %q",
				tc.name, tc.in, got, tc.want)
		}
	}
}

// TestR114b_StreamDeltaStripKeepSpaces — дельта не должна терять пробелы на
// границах: клиент склеивает дельты, и TrimSpace склеил бы слова
// («список» + « файлов» → «списокфайлов»).
func TestR114b_StreamDeltaStripKeepSpaces(t *testing.T) {
	cases := []struct{ in, want string }{
		{"</think>", ""},
		{"</think> Ответ", " Ответ"},
		{"Ответ </think>готов", "Ответ готов"},
		{"<think>a</think>b", "b"},
		{"<thinking>a</thinking> b", " b"},
		{"обычный текст без маркеров", "обычный текст без маркеров"},
	}
	for _, tc := range cases {
		if got := stripReasoningTagsInStreamDelta(tc.in); got != tc.want {
			t.Errorf("stripReasoningTagsInStreamDelta(%q) = %q, ожидалось %q", tc.in, got, tc.want)
		}
	}
}

// streamingContentDeltas прогоняет поток токенов ровно через ту же логику, что
// и потоковые пути cppworker: Feed → (reasoning | content), и склеивает дельты
// видимого content так, как их увидел бы клиент.
func streamingContentDeltas(t *testing.T, tokens []string) string {
	t.Helper()
	st := NewReasoningStreamState()
	var content strings.Builder
	for _, tok := range tokens {
		_, c := st.Feed(tok)
		content.WriteString(stripReasoningTagsInStreamDelta(c))
	}
	return content.String()
}
