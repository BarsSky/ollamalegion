// cline_stream_marker_split_r243_test.go — R243 (2026-10-26).
//
// ЖИВАЯ ЖАЛОБА (Cline): «в ответе всё ещё остаётся </think>». Построчный фильтр
// R231 вырезал маркер, только если он целиком лежал в одном SSE-чанке. Когда
// маркер разрезан между чанками (`</thi` + `nk>`), ни один чанк его не содержит,
// и клиент склеивал служебный маркер в видимом content.
//
// Здесь проверяется новое поведение: хвост, который может быть началом маркера,
// удерживается (в content не уходит), а в следующем чанке маркер вырезается
// целиком. Плюс «thought»-варианты маркеров из DeepSeek-производных шаблонов.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// sseContent — извлечь choices[0].delta.content из SSE-строки фильтра.
func sseContent(t *testing.T, line []byte) string {
	t.Helper()
	s := strings.TrimSpace(string(line))
	s = strings.TrimPrefix(s, "data:")
	s = strings.TrimSpace(s)
	if s == "" || s == "[DONE]" {
		return ""
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(s), &chunk); err != nil {
		t.Fatalf("фильтр вернул невалидный JSON: %q (%v)", s, err)
	}
	if len(chunk.Choices) == 0 {
		return ""
	}
	return chunk.Choices[0].Delta.Content
}

func TestR243_SplitThinkMarkerAcrossChunks(t *testing.T) {
	st := &reasoningStreamState{}

	first := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer</thi\"}}]}\n\n")
	out1, changed := filterOpenAIStreamingLineWithState(first, st)
	if !changed {
		t.Fatal("чанк с началом маркера не изменён: клиент получил бы \"</thi\"")
	}
	got1 := sseContent(t, out1)
	if got1 != "answer" {
		t.Fatalf("content первого чанка = %q, want \"answer\" (маркер удержан)", got1)
	}
	if strings.Contains(got1, "</") || strings.Contains(got1, "<thi") {
		t.Fatalf("клиенту ушло начало маркера: %q", got1)
	}

	second := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"nk>more\"}}]}\n\n")
	out2, _ := filterOpenAIStreamingLineWithState(second, st)
	got2 := sseContent(t, out2)
	if got2 != "more" {
		t.Fatalf("content второго чанка = %q, want \"more\" (маркер вырезан целиком)", got2)
	}
	if strings.Contains(got2, "nk>") || strings.Contains(got2, "</think>") {
		t.Fatalf("маркер всё ещё виден клиенту: %q", got2)
	}
	if st.pending != "" {
		t.Fatalf("после разрешения маркера pending должен быть пуст, got %q", st.pending)
	}
}

func TestR243_SplitMarkerAtStreamEndIsDropped(t *testing.T) {
	st := &reasoningStreamState{}
	line := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"text</thi\"}}]}\n\n")
	out, _ := filterOpenAIStreamingLineWithState(line, st)
	if got := sseContent(t, out); got != "text" {
		t.Fatalf("content = %q, want \"text\" (недособранный маркер не отдаём)", got)
	}
	if st.pending != "</thi" {
		t.Fatalf("pending = %q, want \"</thi\"", st.pending)
	}
	// Поток закончился: pending просто выбрасывается (state умирает вместе со
	// стримом) — главное, что клиент не получил «<thi».
}

func TestR243_ThoughtMarkersStripped(t *testing.T) {
	cases := []string{
		"a</thought>b",
		"a<thought>b",
		"a<|thought|>b",
		"a<|/thought|>b",
		"a<|end_of_thought|>b",
	}
	for _, content := range cases {
		line := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":" +
			mustJSON(content) + "}}]}\n\n")
		out, changed := filterOpenAIStreamingLineWithState(line, &reasoningStreamState{})
		if !changed {
			t.Fatalf("маркер в %q не вырезан", content)
		}
		if got := sseContent(t, out); got != "ab" {
			t.Fatalf("content = %q, want \"ab\" для %q", got, content)
		}
	}
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
