//go:build llama_stub

// reasoning_start_hold_r471_test.go — R471 (2026-11-06): начало вывода не уходит
// клиенту как ответ, если размышления включены, а клиент их не просил.
//
// ЖИВОЙ ДЕФЕКТ. Сводка воркера: streamed_content_bytes=5363 при content_len=3323 —
// 2040 символов английских размышлений уехали в content вместе с русским ответом.
// На уровне парсера воспроизводится так: закрывающий маркер есть, открывающего
// нет (канал открыт шаблоном) → поток отдаёт всё как content, финальный разбор
// считает начало reasoning. Клиент видит склейку «рассуждение + ответ».
package main

import (
	"strings"
	"testing"
)

// TestR471_HoldDecision — когда держим начало.
func TestR471_HoldDecision(t *testing.T) {
	promptWithThinking := "<|turn>system\n<|think|>\nYou are Cline…<|turn>model\n"
	promptNoThinking := "<|turn>system\nYou are Cline…<|turn>model\n"

	cases := []struct {
		name   string
		prompt string
		model  string
		echo   bool
		want   bool
	}{
		{"gemma + thinking + клиент не просил", promptWithThinking, "gemma-4-E4B-it-Q4_K_M", false, true},
		{"gemma + thinking + клиент просил", promptWithThinking, "gemma-4-E4B-it-Q4_K_M", true, false},
		{"gemma без thinking", promptNoThinking, "gemma-4-E4B-it-Q4_K_M", false, false},
		{"не reasoning-модель", promptWithThinking, "llama-3.1-8b-instruct", false, false},
		{"пустой промпт", "", "gemma-4-E4B-it-Q4_K_M", false, false},
	}
	for _, c := range cases {
		if got := reasoningStartHoldR471(c.prompt, c.model, c.echo); got != c.want {
			t.Errorf("%s: hold=%v, want %v", c.name, got, c.want)
		}
	}
}

// TestR471_HoldPreventsReasoningInContent — воспроизведение живой склейки на
// уровне парсера: без удержания английский уезжает в content, с удержанием —
// нет, а русский ответ доходит.
func TestR471_HoldPreventsReasoningInContent(t *testing.T) {
	english := "The user is asking to continue the work. I have gathered the files and checked them. "
	russian := "Я проанализировал содержимое файлов и подтверждаю готовность."
	// Ключевая форма: закрывающий маркер есть, открывающего нет.
	output := english + "<channel|>" + russian

	// simulate — повторяет логику writer'а: держим, пока не увидим конец
	// reasoning-блока; затем отдаём всё, что после него.
	simulate := func(hold bool) (content string, held bool) {
		st := NewReasoningStreamState()
		emittedLen := 0
		reasoningHold := false
		var out strings.Builder
		for i := 0; i < len(output); i += 8 {
			end := i + 8
			if end > len(output) {
				end = len(output)
			}
			token := output[i:end]
			buf := output[:end]
			if emittedLen == 0 && !reasoningHold && startsWithReasoningMarker(buf) {
				reasoningHold = true
			}
			if emittedLen == 0 && !reasoningHold && hold {
				reasoningHold = true
				held = true
			}
			_, _ = st.Feed(token)
			if reasoningHold {
				if pos := reasoningBlockEnd(buf); pos > 0 {
					reasoningHold = false
					emittedLen = pos
					out.WriteString(output[emittedLen:end])
				}
				continue
			}
			if end > emittedLen {
				out.WriteString(token)
				emittedLen = end
			}
		}
		return out.String(), held
	}

	withoutHold, _ := simulate(false)
	if !strings.Contains(withoutHold, "The user is asking") {
		t.Fatal("подготовка теста: без удержания английский обязан попадать в content (живая склейка)")
	}

	withHold, held := simulate(true)
	if !held {
		t.Fatal("удержание не сработало — тест не проверяет R471")
	}
	if strings.Contains(withHold, "The user is asking") {
		t.Errorf("с удержанием размышления всё равно ушли в content: %.120s", withHold)
	}
	if !strings.Contains(withHold, "Я проанализировал") {
		t.Errorf("с удержанием потерялся видимый ответ: %.120s", withHold)
	}
}
