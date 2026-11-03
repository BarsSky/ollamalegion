//go:build llama_stub

// tools_stream_reasoning_r404_test.go — R404 (2026-11-03).
//
// ЖИВОЙ ДЕФЕКТ. На tools-пути (/api/chat с tools, то есть почти каждый запрос
// Cline) старт блока размышлений включал удержание, и до ЗАКРЫТИЯ блока клиент
// не получал ни одной дельты. Если бюджет вывода заканчивался внутри
// размышлений (живой лог: thinking_len=16647, content_len=10, n_predict=4096,
// done_reason=length, 362 с), клиент минутами видел «тишину», затем получал
// пустой финальный чанк и повтор R188 ещё на столько же — и обрывал сессию.
//
// ЧТО ПРОВЕРЯЕТСЯ:
//  1. reasoning-дельта извлекается из потока токенов сразу (не в конце);
//  2. чанк thinking имеет форму Ollama-стрима (message.thinking, done=false);
//  3. обрыв по бюджету при одних размышлениях даёт done_reason=length и
//     непустое пояснение клиенту (а не молчаливый пустой content).
package main

import (
	"strings"
	"testing"
)

// collectThinkingR404 — прогнать токены через парсер, как это делает callback,
// и собрать всё, что ушло бы клиенту дельтами thinking.
func collectThinkingR404(st *ReasoningStreamState, tokens []string) string {
	var b strings.Builder
	for _, tok := range tokens {
		b.WriteString(toolsFeedThinkingR404(st, tok))
	}
	tail, _ := st.FinalizeForStream()
	b.WriteString(tail)
	return b.String()
}

// TestR404_ThinkingStreamsImmediately — thinking уходит дельтой ДО закрытия блока.
func TestR404_ThinkingStreamsImmediately(t *testing.T) {
	cases := []struct {
		name   string
		tokens []string
		want   string
	}{
		{
			name:   "channel-маркер qwen",
			tokens: []string{"<|channel>thought", "Привет", "<channel|>", "ответ"},
			want:   "Привет",
		},
		{
			name:   "think-маркер",
			tokens: []string{"<think>", "думаю", "</think>", "ответ"},
			want:   "думаю",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewReasoningStreamState()
			got := collectThinkingR404(st, c.tokens)
			if !strings.Contains(got, c.want) {
				t.Fatalf("thinking-дельты = %q, want содержит %q (размышления не стримятся)", got, c.want)
			}
			// Видимый ответ в thinking попасть не должен.
			if strings.Contains(got, "ответ") {
				t.Fatalf("в thinking попал видимый ответ: %q", got)
			}
		})
	}
}

// TestR404_ThinkingImpliedOpen — блок открыт промптом: маркера в выводе нет,
// thinking всё равно обязан стримиться (иначе клиент снова видит тишину).
func TestR404_ThinkingImpliedOpen(t *testing.T) {
	st := NewReasoningStreamState()
	st.SetImpliedOpen(true)
	got := collectThinkingR404(st, []string{"размышляю", " ещё", "<channel|>", "готово"})
	if !strings.Contains(got, "размышляю") {
		t.Fatalf("thinking при impliedOpen = %q, want содержит «размышляю»", got)
	}
}

// TestR404_ThinkingChunkShape — кадр thinking валиден для Ollama-клиентов.
func TestR404_ThinkingChunkShape(t *testing.T) {
	chunk := toolsThinkingChunkR404("qwen3.8", "2026-11-03T10:00:00Z", "думаю")
	if chunk["done"] != false {
		t.Errorf("done = %v, want false", chunk["done"])
	}
	msg, ok := chunk["message"].(map[string]string)
	if !ok {
		t.Fatalf("message = %#v, want map[string]string", chunk["message"])
	}
	if msg["thinking"] != "думаю" || msg["role"] != "assistant" {
		t.Errorf("message = %#v, want role=assistant thinking=«думаю»", msg)
	}
	if _, ok := msg["content"]; ok {
		t.Error("thinking-кадр не должен нести content")
	}
}

// TestR404_ReasoningOnlyBudgetIsHonest — обрыв по бюджету в размышлениях:
// причина length, а клиент получает непустое пояснение, а не пустой content.
func TestR404_ReasoningOnlyBudgetIsHonest(t *testing.T) {
	raw := "<|channel>thought" + strings.Repeat(" рассуждение", 200)
	reason := toolsTerminalReasonR394(raw, "", "prompt", 0, 64, 64)
	if reason != "length" {
		t.Fatalf("done_reason = %q, want length (бюджет исчерпан в размышлениях)", reason)
	}
	content := toolsTerminalContentR394("", raw)
	if strings.TrimSpace(content) == "" {
		t.Fatal("content пуст: клиент не поймёт, что ответ не сгенерирован (нужно пояснение)")
	}
}

// TestR404_Wiring — сторож проводки: thinking кормится в парсер до удержания,
// повтор R188 не запускается после уже отданного стрима, финальный чанк не
// дублирует thinking. Поведенчески это не проверить без живого bridge, поэтому
// проверяем исходник (как сторожа R394/R402).
func TestR404_Wiring(t *testing.T) {
	src := readMainSourceR402(t, "handlers_chat.go")
	for _, want := range []string{
		"toolsFeedThinkingR404(thinkingParser, token)",
		"retry skipped — stream already started (R404)",
		"if anyStreamed {",
		"reasoningOut != \"\" && !thinkingStreamed",
		"thinkingParser.FinalizeForStream()",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("в handlers_chat.go нет %q — проводка R404 потеряна", want)
		}
	}
}
