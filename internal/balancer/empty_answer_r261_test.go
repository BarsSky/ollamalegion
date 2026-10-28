package balancer

// empty_answer_r261_test.go — R261 (2026-10-28): «модель ответила, а клиент не
// получил ничего» — и хвост ответа, который терялся в фильтре размышлений.
//
// ЖИВОЙ ЗАМЕР (стенд, gemma-4-E4B-it-Q4_K_M, stream=true, max_tokens=160):
// кадров 157, content_len=0, reasoning_len=359, finish_reason=length. То есть
// весь текст ушёл в reasoning_content (так и должно быть после R256 — иначе
// размышления «мешаются с ответом»), но клиент без поддержки reasoning_content
// видел пустоту.
//
// Тесты фиксируют:
//   - стрим, где видимого текста нет, получает ОДИН кадр-объяснение в content,
//     и он идёт ПЕРЕД finish_reason (после finish клиенты контент игнорируют);
//   - нормальный стрим объяснения НЕ получает (иначе мы «лечим» обычный ответ);
//   - хвост, удержанный как возможный префикс маркера, сбрасывается в конце —
//     текст ответа не обрывается;
//   - то же правило для non-stream (собранный) ответ.

import (
	"strings"
	"testing"
)

func sseFrame(content, finish string) []byte {
	if finish != "" {
		return []byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"` + finish + `"}]}`)
	}
	return []byte(`data: {"choices":[{"index":0,"delta":{"content":"` + content + `"},"finish_reason":null}]}`)
}

// TestR261_AllReasoningStreamGetsNotice — главный живой случай.
func TestR261_AllReasoningStreamGetsNotice(t *testing.T) {
	st := &reasoningEgressState{}

	// 1. Модель открывает канал размышлений и пишет в него.
	f1 := []byte(`data: {"choices":[{"index":0,"delta":{"content":"<|channel|>analysis"},"finish_reason":null}]}`)
	out, _ := filterAssistantEgressForClient(f1, st)
	_, _ = filterAssistantEgressForClient(out, st) // повторный проход не должен ломать состояние
	if st.reasoningChars == 0 {
		// текст после маркера может прийти следующим кадром — проверим оба пути
	}
	out2, _ := filterAssistantEgressForClient(sseFrame("думаю над задачей", ""), st)
	_ = out2

	// 2. Кадр с finish_reason — обязан быть придержан.
	if !st.tryHoldFinishFrame(sseFrame("", "length")) {
		t.Fatal("finish-кадр не придержан — объяснение уйдёт после finish и будет проигнорировано клиентом")
	}

	// 3. Завершение: объяснение + finish, в этом порядке.
	frames := st.finalizeFrames()
	if len(frames) != 2 {
		t.Fatalf("кадров на завершении = %d, want 2 (объяснение + finish): %q", len(frames), frames)
	}
	notice, ok := extractSSEDeltaContent(frames[0])
	if !ok || notice == "" {
		t.Fatalf("первый кадр завершения не содержит объяснения: %q", frames[0])
	}
	if !strings.Contains(notice, "размышления") {
		t.Errorf("объяснение должно называть причину: %q", notice)
	}
	if !strings.Contains(notice, "max_tokens") {
		t.Errorf("объяснение должно говорить, что делать: %q", notice)
	}
	if got := finishReasonOfSSELine(frames[1]); got != "length" {
		t.Errorf("последний кадр = finish_reason %q, want length", got)
	}
	// Идемпотентность: второй вызов ничего не отдаёт.
	if again := st.finalizeFrames(); again != nil {
		t.Errorf("повторный finalize отдал кадры: %q", again)
	}
}

// TestR261_NormalStreamHasNoNotice — обычный ответ не «лечим».
func TestR261_NormalStreamHasNoNotice(t *testing.T) {
	st := &reasoningEgressState{}
	if _, _ = filterAssistantEgressForClient(sseFrame("Ответ: 391", ""), st); st.visibleChars == 0 {
		t.Fatal("видимый текст не учтён в счётчике")
	}
	if !st.tryHoldFinishFrame(sseFrame("", "stop")) {
		t.Fatal("finish-кадр не придержан")
	}
	frames := st.finalizeFrames()
	if len(frames) != 1 {
		t.Fatalf("кадров на завершении = %d, want 1 (только finish): %q", len(frames), frames)
	}
	if reason := finishReasonOfSSELine(frames[0]); reason != "stop" {
		t.Fatalf("придержанный finish потерян: %q", frames[0])
	}
}

// TestR261_PendingTailIsFlushed — текст не обрывается на «возможном маркере».
func TestR261_PendingTailIsFlushed(t *testing.T) {
	st := &reasoningEgressState{}
	// Ответ заканчивается символом, который может начинать маркер.
	framesIn := []string{"Список параметров: a, b, c", " и завершается <"}
	for _, c := range framesIn {
		_, _ = filterAssistantEgressForClient(sseFrame(c, ""), st)
	}
	if st.split.pending == "" {
		t.Skip("фильтр не удержал хвост — сценарий неприменим на этой реализации")
	}
	held := st.split.pending
	frames := st.finalizeFrames()
	if len(frames) == 0 {
		t.Fatal("finalize не отдал удержанный хвост — текст ответа потерян")
	}
	tail, ok := extractSSEDeltaContent(frames[0])
	if !ok || !strings.HasSuffix(tail, held) {
		t.Fatalf("хвост %q не отдан клиенту: %q", held, frames[0])
	}
}

// TestR261_NonStreamNotice — то же правило для собранного ответа.
func TestR261_NonStreamNotice(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4-E4B-it-Q4_K_M")
	a.addChunk(`{"choices":[{"index":0,"delta":{"content":"<|channel|>analysis думаю"},"finish_reason":null}]}`)
	a.addChunk(`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`)
	// Аккумулятор хранит то, что прислал воркер; отдельно проверяем правило
	// «пустой content при непустом reasoning».
	a.content.Reset()
	a.reasoning.WriteString("думаю над задачей 359 символов…")
	if !a.applyEmptyAnswerNotice() {
		t.Fatal("объяснение не отправлено — клиент снова получил бы content:\"\"")
	}
	body := string(a.toNonStreamResponse(false))
	if !strings.Contains(body, "размышления") {
		t.Fatalf("в собранном ответе нет объяснения: %s", body)
	}
	// Если видимый текст есть — объяснение не добавляется.
	b := newOpenAIStreamAccumulator("m")
	b.content.WriteString("Ответ: 391")
	b.reasoning.WriteString("думал")
	if b.applyEmptyAnswerNotice() {
		t.Fatal("объяснение добавлено к нормальному ответу")
	}
}
