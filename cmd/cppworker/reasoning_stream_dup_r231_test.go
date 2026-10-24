// reasoning_stream_dup_r231_test.go — R231 (2026-10-24): Cline-жалоба
// «модель иногда возвращает в основном теле ответа </think>, есть дублирование,
// и где-то затесался символ конца стрима, из-за чего Cline оборвал сессию».
//
// ЖИВОЙ ФАКТ (воспроизведено на стенде, gemma-4-E4B, /v1/chat/completions
// stream=true, max_tokens=120): все ~115 чанков ушли в reasoning_content, а
// затем Finalize отдал ТОТ ЖЕ текст ещё раз единственным чанком delta.content
// (433 символа «Thinking Process: …»). Причина — R91-фолбэк «незакрытый
// reasoning → content» применялся и к стриму.
//
// Второй дефект: SplitReasoningContent оставлял лишние закрывающие маркеры в
// content после уже закрытой пары (`<think>r</think>answer</think>` →
// `answer</think>`), и в стриме такой маркер уезжал в дельту.
package main

import (
	"strings"
	"testing"
)

// TestR231_Split_NoReasoningMarkersInVisibleContent — в видимом тексте не
// должно оставаться служебных маркеров ни при каких дублях.
func TestR231_Split_NoReasoningMarkersInVisibleContent(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		wantReasoning string
		wantContent   string
	}{
		{"дубль закрытия", "<think>r</think>answer</think>", "r", "answer"},
		{"дубль в конце строки", "<think>r</think>answer</think>\nnext", "r", "answer\nnext"},
		{"два дубля подряд", "<think>r</think>a</think></think>b", "r", "ab"},
		{"обычная пара", "<think>r</think>answer", "r", "answer"},
		{"ведущий закрывающий без открывающего", "</think>answer", "", "answer"},
		{"gemma-4 канал: дубль закрытия", "<|channel>thought\nplan<channel|>ответ<channel|>", "plan", "ответ"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reasoning, content, _ := SplitReasoningContent(c.in)
			if reasoning != c.wantReasoning {
				t.Errorf("reasoning=%q, want %q", reasoning, c.wantReasoning)
			}
			if content != c.wantContent {
				t.Errorf("content=%q, want %q", content, c.wantContent)
			}
			if strings.Contains(content, "</think>") || strings.Contains(content, "<channel|>") ||
				strings.Contains(content, "<|channel>") {
				t.Errorf("в видимом content остался маркер размышлений: %q", content)
			}
		})
	}
}

// TestR231_Stream_NoDuplicateReasoningInContent — живой случай Cline:
// блок размышлений не закрыт за max_tokens. В стриме reasoning уже отдан
// отдельным каналом, поэтому content ОБЯЗАН остаться пустым (R91-фолбэк в
// поток не применяется).
func TestR231_Stream_NoDuplicateReasoningInContent(t *testing.T) {
	st := NewReasoningStreamState()
	st.SetImpliedOpen(true) // prompt шаблона открыл канал

	var content strings.Builder
	var reasoning strings.Builder
	for _, chunk := range []string{
		"Thinking Process:\n\n",
		"1.  **Analyze the Request:** ",
		"The user wants a Go function",
		" that adds two numbers.",
	} {
		rd, cd := st.Feed(chunk)
		reasoning.WriteString(rd)
		content.WriteString(cd)
	}
	rTail, cTail := st.FinalizeForStream()
	reasoning.WriteString(rTail)
	content.WriteString(cTail)

	if content.Len() != 0 {
		t.Fatalf("content не должен дублировать размышления: %q", content.String())
	}
	if !strings.Contains(reasoning.String(), "Analyze the Request") {
		t.Fatalf("reasoning потерян: %q", reasoning.String())
	}
	if snap := st.Snapshot(); !snap.Unclosed {
		t.Fatalf("ожидался Unclosed=true (канал не закрыт), snapshot=%+v", snap)
	}
	if snap := st.Snapshot(); snap.ContentChars != 0 {
		t.Fatalf("ContentChars=%d, want 0 (иначе клиент видит дубль)", snap.ContentChars)
	}
}

// TestR231_Stream_ModelOpenedUnclosedBlock — модель САМА открыла блок (<think>)
// и оборвалась: тот же запрет на дубль в content (это и был живой случай,
// impliedOpen=false).
func TestR231_Stream_ModelOpenedUnclosedBlock(t *testing.T) {
	st := NewReasoningStreamState()
	var reasoning, content strings.Builder
	for _, chunk := range []string{"<think>blah blah", " more thoughts"} {
		rd, cd := st.Feed(chunk)
		reasoning.WriteString(rd)
		content.WriteString(cd)
	}
	rTail, cTail := st.FinalizeForStream()
	reasoning.WriteString(rTail)
	content.WriteString(cTail)
	if content.Len() != 0 {
		t.Fatalf("content=%q, want пусто (размышления уже в reasoning)", content.String())
	}
	if !strings.Contains(reasoning.String(), "more thoughts") {
		t.Fatalf("reasoning потерян: %q", reasoning.String())
	}
	if !st.Snapshot().Unclosed {
		t.Fatal("ожидался Unclosed=true")
	}
}

// TestR231_StreamDuplicateCloseTag_DoesNotLeakIntoContent — маркер, пришедший
// ПОСЛЕ закрытой пары (дубль в конце строки), не попадает в content-дельту.
func TestR231_StreamDuplicateCloseTag_DoesNotLeakIntoContent(t *testing.T) {
	st := NewReasoningStreamState()
	var content strings.Builder
	for _, chunk := range []string{"<think>r</think>", "answer", "</think>", "\nmore"} {
		_, cd := st.Feed(chunk)
		content.WriteString(cd)
	}
	_, tail := st.FinalizeForStream()
	content.WriteString(tail)

	if got := content.String(); got != "answer\nmore" {
		t.Fatalf("content=%q, want %q", got, "answer\nmore")
	}
	if strings.Contains(content.String(), "</think>") {
		t.Fatalf("маркер утёк в content: %q", content.String())
	}
}

// TestR231_StreamImpliedOpen_DuplicateCloseInTail — дубль закрывающего маркера
// в impliedOpen-режиме (канал открыт промптом).
func TestR231_StreamImpliedOpen_DuplicateCloseInTail(t *testing.T) {
	st := NewReasoningStreamState()
	st.SetImpliedOpen(true)
	var content strings.Builder
	for _, chunk := range []string{"plan", "</think>", "answer", "</think>", " tail"} {
		_, cd := st.Feed(chunk)
		content.WriteString(cd)
	}
	_, tail := st.FinalizeForStream()
	content.WriteString(tail)

	if got := content.String(); got != "answer tail" {
		t.Fatalf("content=%q, want %q", got, "answer tail")
	}
	if strings.Contains(content.String(), "</think>") {
		t.Fatalf("маркер утёк в content: %q", content.String())
	}
}

// TestR231_NonStreamFinalize_KeepsR91Fallback — R91-фолбэк для НЕ-потоковых
// путей сохранён: клиент, показывающий только content, не должен получить
// пустой ответ. В стриме этот фолбэк отключён (см. выше).
func TestR231_NonStreamFinalize_KeepsR91Fallback(t *testing.T) {
	st := NewReasoningStreamState()
	st.Feed("<think>only thoughts before cutoff")
	_, cTail := st.Finalize()
	if cTail == "" {
		t.Fatal("для не-стрима R91-фолбэк обязан отдать reasoning как content")
	}
	if !strings.Contains(cTail, "only thoughts") {
		t.Fatalf("фолбэк потерял текст: %q", cTail)
	}
}

// TestR231_Unclosed_ChannelAndThinkFormats — Snapshot().Unclosed должен быть
// честным для обоих форматов (иначе finish_reason остаётся stop при пустом
// content, и клиент считает ответ полным).
func TestR231_Unclosed_ChannelAndThinkFormats(t *testing.T) {
	cases := []struct {
		name string
		full string
		want bool
	}{
		{"think закрыт", "<think>r</think>answer", false},
		{"think не закрыт", "<think>r", true},
		{"channel закрыт", "<|channel>thought\nr\n<channel|>answer", false},
		{"channel не закрыт", "<|channel>thought\nr", true},
		{"без маркеров", "просто ответ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasOpenThink(c.full); got != c.want {
				t.Fatalf("hasOpenThink(%q)=%v, want %v", c.full, got, c.want)
			}
		})
	}
}
