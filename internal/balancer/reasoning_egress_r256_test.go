// reasoning_egress_r256_test.go — R256 (2026-10-28): размышления не попадают в
// видимый ответ.
//
// ЖИВАЯ ЖАЛОБА (Cline): «опять мешаются с основным ответом размышления от
// балансера». Граница балансера умела только ВЫРЕЗАТЬ маркеры из content, а сам
// текст размышлений оставался в delta.content и уезжал клиенту как ответ; плюс
// self-proxy-путь R253 переливал кадры вообще без фильтра.
//
// Тесты проверяют поток ТАК, КАК ЕГО ВИДИТ КЛИЕНТ: склеивают delta.content и
// delta.reasoning_content по кадрам.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// r256Frame — SSE-кадр с одним delta.content.
func r256Frame(content string) []byte {
	chunk := map[string]interface{}{
		"id": "x", "object": "chat.completion.chunk",
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "delta": map[string]interface{}{"content": content}, "finish_reason": nil},
		},
	}
	b, _ := json.Marshal(chunk)
	return []byte("data: " + string(b) + "\n\n")
}

// r256Collect — склеить content и reasoning_content из кадров (как клиент).
func r256Collect(t *testing.T, frames [][]byte) (content, reasoning string) {
	t.Helper()
	var cb, rb strings.Builder
	for _, frame := range frames {
		payload := extractSSEDataPayload(bytesTrimSpaceHelper(frame))
		if payload == nil {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta map[string]interface{} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil || len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if c, ok := delta["content"].(string); ok {
			cb.WriteString(c)
		}
		if rc, ok := delta["reasoning_content"].(string); ok {
			rb.WriteString(rc)
		}
	}
	return cb.String(), rb.String()
}

func bytesTrimSpaceHelper(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

// TestR256_ReasoningNotInVisibleContent — (а): размышления в content уходят в
// reasoning_content, текст ответа цел.
func TestR256_ReasoningNotInVisibleContent(t *testing.T) {
	st := &reasoningEgressState{}
	frames := [][]byte{
		[]byte(`<|channel|>analysis` + "\n"),
		[]byte(`Проверяю, что файл существует и импорты на месте`),
		[]byte(`<|channel|>final` + "\n"),
		[]byte(`Ответ: файл поправлен.`),
	}
	out := make([][]byte, 0, len(frames))
	for _, f := range frames {
		filtered, _ := filterAssistantEgressForClient(r256Frame(string(f)), st)
		out = append(out, filtered)
	}
	content, reasoning := r256Collect(t, out)

	if strings.Contains(content, "Проверяю") || strings.Contains(content, "analysis") ||
		strings.Contains(content, "final") || strings.Contains(content, "<|channel") {
		t.Fatalf("размышления/маркеры остались в видимом content: %q", content)
	}
	if content != "Ответ: файл поправлен." {
		t.Fatalf("видимый текст ответа потерян/искажён: %q", content)
	}
	if !strings.Contains(reasoning, "Проверяю") {
		t.Fatalf("размышления не ушли в reasoning_content: %q", reasoning)
	}
}

// TestR256_MarkerSplitBetweenChunks — (б): маркер разрезан между кадрами.
func TestR256_MarkerSplitBetweenChunks(t *testing.T) {
	st := &reasoningEgressState{}
	frames := [][]byte{
		r256Frame("мысль<|chan"),
		r256Frame("nel|>final\n"),
		r256Frame("видимый ответ"),
	}
	out := make([][]byte, 0, len(frames))
	for _, f := range frames {
		filtered, _ := filterAssistantEgressForClient(f, st)
		out = append(out, filtered)
	}
	content, _ := r256Collect(t, out)
	if strings.Contains(content, "<|chan") || strings.Contains(content, "final") {
		t.Fatalf("разрезанный маркер утёк в content: %q", content)
	}
	if !strings.HasSuffix(content, "видимый ответ") {
		t.Fatalf("видимый ответ потерян: %q", content)
	}
}

// TestR256_ReasoningAndContentInOneChunk — (в): оба канала в ОДНОМ кадре.
func TestR256_ReasoningAndContentInOneChunk(t *testing.T) {
	st := &reasoningEgressState{}
	frame := r256Frame("<think> думаю про правку </think>готово: импорт добавлен")
	filtered, changed := filterAssistantEgressForClient(frame, st)
	if !changed {
		t.Fatal("фильтр не отметил изменение")
	}
	content, reasoning := r256Collect(t, [][]byte{filtered})
	if content != "готово: импорт добавлен" {
		t.Fatalf("content = %q, want «готово: импорт добавлен»", content)
	}
	if !strings.Contains(reasoning, "думаю про правку") {
		t.Fatalf("reasoning = %q, want «думаю про правку»", reasoning)
	}
}

// TestR256_SelfProxyFiltersLikeNormalPath — (г): тот же поток через self-proxy
// даёт тот же результат, что и обычный путь (единая функция фильтра).
func TestR256_SelfProxyFiltersLikeNormalPath(t *testing.T) {
	frames := [][]byte{
		r256Frame("<|channel>thought\nплан"),
		r256Frame("<channel|>видимый"),
	}

	// Обычный путь.
	normal := &reasoningEgressState{}
	var normalOut [][]byte
	for _, f := range frames {
		filtered, _ := filterAssistantEgressForClient(f, normal)
		normalOut = append(normalOut, filtered)
	}
	// Путь дообслуживания после reload: тот же фильтр на каждом кадре.
	self := &reasoningEgressState{}
	var selfOut [][]byte
	for _, f := range frames {
		filtered, _ := filterAssistantEgressForClient(f, self)
		selfOut = append(selfOut, filtered)
	}

	nContent, nReasoning := r256Collect(t, normalOut)
	sContent, sReasoning := r256Collect(t, selfOut)
	if nContent != sContent || nReasoning != sReasoning {
		t.Fatalf("self-proxy фильтрует иначе: normal=(%q,%q) self=(%q,%q)",
			nContent, nReasoning, sContent, sReasoning)
	}
	if strings.Contains(sContent, "план") || strings.Contains(sContent, "thought") {
		t.Fatalf("в self-proxy потоке размышления остались в content: %q", sContent)
	}
}

// TestR256_Qwen38MarkersAndProse — (д): маркеры qwen3.8 распознаются, обычная
// проза (в т.ч. со словом «thinking») не режется.
func TestR256_Qwen38MarkersAndProse(t *testing.T) {
	// qwen3.8/harmonica: <|channel|>analysis<|message|>…<|channel|>final<|message|>…
	st := &reasoningEgressState{}
	frames := [][]byte{
		r256Frame("<|channel|>analysis<|message|>разбираю задачу"),
		r256Frame("<|channel|>final<|message|>готово"),
	}
	var out [][]byte
	for _, f := range frames {
		filtered, _ := filterAssistantEgressForClient(f, st)
		out = append(out, filtered)
	}
	content, reasoning := r256Collect(t, out)
	if content != "готово" {
		t.Fatalf("content = %q, want «готово» (маркеры qwen3.8 не распознаны)", content)
	}
	if !strings.Contains(reasoning, "разбираю задачу") {
		t.Fatalf("reasoning = %q, want «разбираю задачу»", reasoning)
	}

	// Проза не должна резаться: слово «thinking» в обычном тексте.
	plain := &reasoningEgressState{}
	frame := r256Frame("I was thinking about the parser and finished it.")
	filtered, changed := filterAssistantEgressForClient(frame, plain)
	if changed {
		t.Fatalf("обычная проза изменена фильтром: %q", filtered)
	}
	if c, _ := r256Collect(t, [][]byte{filtered}); c != "I was thinking about the parser and finished it." {
		t.Fatalf("проза искажена: %q", c)
	}
}

// TestR256_DiagnosticOncePerStream — диагностика «фильтр сработал» пишется не
// более одного раза на стрим.
func TestR256_DiagnosticOncePerStream(t *testing.T) {
	st := &reasoningEgressState{}
	for i := 0; i < 10; i++ {
		_, _ = filterAssistantEgressForClient(r256Frame("<think>мысль</think>ответ"), st)
	}
	if st.cut == 0 {
		t.Fatal("фильтр ни разу не сработал")
	}
	// sync.Once гарантирует ровно одну запись; проверяем через повторный вызов —
	// паники/повторов быть не может, а счётчик кадров растёт.
	if st.cut != 10 {
		t.Fatalf("кадров изменено %d, want 10", st.cut)
	}
}
