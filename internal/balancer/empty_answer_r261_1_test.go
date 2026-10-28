package balancer

// empty_answer_r261_1_test.go — R261.1 (2026-10-28): счётчик размышлений
// обязан видеть канал reasoning_content.
//
// ЖИВОЙ ПРОВАЛ (стенд r261, основной hijack-путь Cline/DSH): запрос gemma,
// stream=true, max_tokens=160 → видимого content нет, 446 символов пришли в
// delta.reasoning_content, finish=length, но кадра-объяснения НЕТ и клиент снова
// видит пустоту. Лог это подтверждал: `stream egress finalize: visible_chars:0,
// reasoning_chars:0, notice_sent:false` при 446/572 символах reasoning.
//
// ПРИЧИНА: в filterAssistantEgressForClient кадр БЕЗ delta.content выходил
// ранним return ДО счётчиков, а reasoning_content воркер шлёт именно таким
// кадром. Условие empty_answer_r261.go:170 (visibleChars==0 &&
// reasoningChars>0) не могло стать истинным НИКОГДА для такого стрима.
//
// Тесты фиксируют:
//   - поток, целиком из reasoning_content → счётчик растёт, клиент получает
//     ровно один кадр-объяснение СТРОГО перед finish;
//   - кадр, где content и reasoning в ОДНОМ сообщении → счётчики не удваиваются
//     и уведомления нет (иначе «лечим» нормальные ответы);
//   - кадры reasoning_content не переписываются (чужой канал, R231).

import (
	"strings"
	"testing"
)

// TestR261_1_ReasoningContentOnlyGetsNotice — главный живой случай: воркер не
// присылает content вовсе, только reasoning_content.
func TestR261_1_ReasoningContentOnlyGetsNotice(t *testing.T) {
	st := &reasoningEgressState{}

	var total int
	for _, part := range []string{"думаю над задачей", " проверяю импорты", " готовлю ответ"} {
		line := sseDeltaFrame("", part)
		out, changed := filterAssistantEgressForClient(line, st)
		if changed {
			t.Fatalf("кадр reasoning_content переписан (чужой канал, R231): %q", out)
		}
		total += len(part)
	}

	if st.visibleChars != 0 {
		t.Errorf("visibleChars = %d, want 0 (видимого текста не было)", st.visibleChars)
	}
	if st.reasoningChars != total {
		t.Fatalf("reasoningChars = %d, want %d: счётчик не видит reasoning_content, "+
			"объяснение пустого ответа не сработает (это и был живой дефект)",
			st.reasoningChars, total)
	}

	if !st.tryHoldFinishFrame(sseFrame("", "length")) {
		t.Fatal("finish-кадр не придержан: объяснение уйдёт после finish и будет проигнорировано клиентом")
	}
	frames := st.finalizeFrames()
	if len(frames) != 2 {
		t.Fatalf("кадров на завершении = %d, want 2 (объяснение + finish)", len(frames))
	}
	notice, ok := extractSSEDeltaContent(frames[0])
	if !ok || notice == "" {
		t.Fatalf("первый кадр завершения пуст: %q", frames[0])
	}
	if !strings.Contains(notice, "размышления") {
		t.Errorf("объяснение не называет причину: %q", notice)
	}
	if fr := finishReasonOfSSELine(frames[1]); fr != "length" {
		t.Errorf("второй кадр должен быть придержанный finish(length), got %q (%q)", fr, frames[1])
	}
	// Идемпотентность: повторный finalize ничего не добавляет (иначе второй
	// конец потока — ровно то, что ловит R258-тест).
	if again := st.finalizeFrames(); len(again) != 0 {
		t.Errorf("повторный finalize вернул %d кадров, want 0", len(again))
	}
}

// TestR261_1_ContentAndReasoningInOneFrameNoDoubleCount — кадр с обоими каналами
// считается один раз, и нормальный ответ уведомления не получает.
func TestR261_1_ContentAndReasoningInOneFrameNoDoubleCount(t *testing.T) {
	st := &reasoningEgressState{}

	// Один кадр: и видимый текст, и размышления.
	out, _ := filterAssistantEgressForClient(sseDeltaFrame("Ответ: 391", "подумал"), st)
	if len(out) == 0 {
		t.Fatal("кадр потерян")
	}
	if st.reasoningChars != len("подумал") {
		t.Errorf("reasoningChars = %d, want %d (двойной счёт по одному кадру)",
			st.reasoningChars, len("подумал"))
	}
	if st.visibleChars == 0 {
		t.Error("visibleChars = 0 при непустом content: счётчик видимого канала сломан")
	}

	// Завершение: объяснения быть НЕ должно — видимый ответ есть.
	_ = st.tryHoldFinishFrame(sseFrame("", "stop"))
	frames := st.finalizeFrames()
	for _, f := range frames {
		if c, ok := extractSSEDeltaContent(f); ok && strings.Contains(c, "размышления") {
			t.Fatalf("нормальный ответ получил объяснение пустого ответа: %q", c)
		}
	}
}

// TestR261_1_ReasoningFramesNotRewritten — кадры reasoning_content уходят
// байт-в-байт: их содержимое принадлежит воркеру (R231), а балансер только
// считает объём.
func TestR261_1_ReasoningFramesNotRewritten(t *testing.T) {
	st := &reasoningEgressState{}
	line := sseDeltaFrame("", "<|channel|>analysis думаю")
	out, changed := filterAssistantEgressForClient(line, st)
	if changed {
		t.Error("кадр reasoning_content переписан — балансер не должен чистить чужой канал")
	}
	if string(out) != string(line) {
		t.Errorf("кадр изменён байт-в-байт:\n  было: %q\n  стало: %q", line, out)
	}
	if st.reasoningChars == 0 {
		t.Error("объём размышлений не учтён")
	}
}
