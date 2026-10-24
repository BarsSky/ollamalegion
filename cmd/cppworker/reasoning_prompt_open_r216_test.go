//go:build llama_stub

// reasoning_prompt_open_r216_test.go — R216 (2026-10-21).
//
// ЖИВОЙ ДЕФЕКТ (жалоба оператора). «Размышления попали в основной блок и
// закончились, а само тело ответа не пришло; с включённым флагом размышления
// просто пропали».
//
// Причина: чат-шаблон Qwen3 (и gemma-4) при включённых размышлениях дописывает
// `<think>` / `<|channel>thought` в КОНЕЦ ПРОМПТА. Модель продолжает этот блок,
// поэтому её вывод начинается ВНУТРИ размышлений и открывающего маркера не
// содержит. SplitReasoningContent такой текст считал видимым ответом — и
// reasoning уезжал в message.content без `</think>` (проверено на стенде:
// Qwen3.8-27B, max_tokens=16 → content = `The user is asking me to memorize a
// code "КРАСНЫЙ-`). Если модель успевала закрыть блок, ответ выглядел нормально;
// если нет — «тело не пришло», потому что его ещё не сгенерировали.
package main

import (
	"strings"
	"testing"
)

func TestR216_PromptOpensReasoning(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   bool
	}{
		{"Qwen3 шаблон открыл think", "<|im_start|>user\nпривет<|im_end|>\n<|im_start|>assistant\n<think>\n", true},
		{"think-блок уже закрыт в промпте", "<|im_start|>assistant\n<think>r</think>ответ", false},
		{"gemma-4 channel-префилл", "<start_of_turn>model\n<|channel>thought\n", true},
		{"обычный промпт без размышлений", "<|im_start|>user\nпривет<|im_end|>\n<|im_start|>assistant\n", false},
		{"пустой промпт", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PromptOpensReasoning(tc.prompt); got != tc.want {
				t.Fatalf("PromptOpensReasoning = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestR216_SplitFromPrompt_OpenBlockWithoutMarker(t *testing.T) {
	prompt := "<|im_start|>assistant\n<think>\n"
	out := "The user is asking me to memorize a code"
	reasoning, content, has := SplitReasoningContentFromPrompt(prompt, out)
	if !has {
		t.Fatal("hasReasoning = false: обрезанные размышления снова уедут в content")
	}
	if reasoning != out {
		t.Errorf("reasoning = %q, want %q", reasoning, out)
	}
	if content != "" {
		t.Errorf("content = %q, want пусто (ответа ещё нет)", content)
	}
}

func TestR216_SplitFromPrompt_ClosedLaterInOutput(t *testing.T) {
	prompt := "<|im_start|>assistant\n<think>\n"
	out := "думаю про 17*23</think>\n\nОтвет: 391"
	reasoning, content, has := SplitReasoningContentFromPrompt(prompt, out)
	if !has {
		t.Fatal("hasReasoning = false")
	}
	if reasoning != "думаю про 17*23" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if content != "\n\nОтвет: 391" {
		t.Errorf("content = %q", content)
	}
}

// Без префилла в промпте поведение прежнее: обычный текст — ответ, а не мысли.
func TestR216_SplitFromPrompt_NoPrefillUnchanged(t *testing.T) {
	prompt := "<|im_start|>user\nhi<|im_end|>\n<|im_start|>assistant\n"
	reasoning, content, has := SplitReasoningContentFromPrompt(prompt, "просто ответ")
	if has {
		t.Errorf("hasReasoning = true, want false")
	}
	if reasoning != "" || content != "просто ответ" {
		t.Errorf("reasoning/content = %q/%q", reasoning, content)
	}
}

// Стриминговый парсер с impliedOpen: текст до закрывающего маркера — reasoning,
// после — content; дельты не дублируются.
func TestR216_StreamImpliedOpen(t *testing.T) {
	st := NewReasoningStreamState()
	st.SetImpliedOpen(true)

	r1, c1 := st.Feed("думаю про")
	if r1 != "думаю про" || c1 != "" {
		t.Errorf("первая дельта reasoning/content = %q/%q, want %q/\"\"", r1, c1, "думаю про")
	}
	r2, c2 := st.Feed(" 17*23</think>\nОтвет: 391")
	if strings.Contains(c2, "думаю") {
		t.Errorf("reasoning утёк в content: %q", c2)
	}
	if !strings.HasPrefix(r2, " 17*23") {
		t.Errorf("reasoning-дельта = %q, want продолжение до </think>", r2)
	}
	if !strings.HasPrefix(c2, "\nОтвет: 391") {
		t.Errorf("content-дельта = %q, want видимый ответ после маркера", c2)
	}

	// Хвостовые потери: Finalize не теряет последние символы content.
	rf, cf := st.Finalize()
	if strings.Contains(rf+cf, "думаю про") {
		t.Errorf("Finalize повторил уже отданное: reasoning=%q content=%q", rf, cf)
	}
}

// R216: при impliedOpen Finalize НЕ копирует размышления в content.
//
// Живой дефект: Qwen3.8-27B, max_tokens=24, stream — одни и те же 76 символов
// пришли и в reasoning_content (19 чанков), и в content (1 чанк). Причина —
// R91-фолбэк в Finalize. Клиент видел размышления как ответ.
func TestR216_FinalizeImpliedOpenDoesNotDuplicateIntoContent(t *testing.T) {
	st := NewReasoningStreamState()
	st.SetImpliedOpen(true)
	if r, c := st.Feed("кратко размышляю и не закрываю"); r == "" || c != "" {
		t.Fatalf("Feed: reasoning=%q content=%q, want reasoning непустой и content пустой", r, c)
	}
	rTail, cTail := st.Finalize()
	if cTail != "" {
		t.Errorf("Finalize вернул reasoning как content: contentDelta=%q (будет дубль)", cTail)
	}
	if strings.TrimSpace(rTail) != "" {
		// rTail допустим только как НОВЫЙ хвост reasoning, но не как копия уже отданного.
		t.Logf("rTail=%q (хвост reasoning)", rTail)
	}
}
