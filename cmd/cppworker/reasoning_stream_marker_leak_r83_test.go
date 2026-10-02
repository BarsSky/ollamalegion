// reasoning_stream_marker_leak_r83_test.go — R83/v67 (2026-10-02).
//
// ЖИВОЙ ДЕФЕКТ (воспроизведён на стенде, gemma-4 + reasoning через балансер).
// Тело ответа, которое увидел клиент:
//
//	reasoning: "though a thinking process to calculate ..."   (810 символов)
//	content:   "|chann" ← ОБРЫВОК ОТКРЫВАЮЩЕГО МАРКЕРА
//
// Прямой запрос к cppworker (stream:false) давал корректное разделение, поэтому
// причина — в ПОТОКОВОМ парсере: SSE-дельты выглядели так
//
//	content:  "<|c"   content: "h"   content: "a"   content: "nn"
//	reasoning_content: "though" ...
//
// то есть первые 7 символов маркера `<|channel>thought` были отправлены в
// ВИДИМЫЙ ответ до того, как парсер понял, что это тег. Парсер пересчитывал
// разбиение на каждом чанке и не мог отозвать уже отправленное.
//
// Тесты фиксируют: неполный маркер наружу не уходит, а хвост, который так и не
// стал тегом, выпускается ровно один раз в Finalize.
package main

import (
	"strings"
	"testing"
)

// feedAllR83 прогоняет чанки через парсер и собирает всё, что было отправлено
// клиенту (включая финальный сброс — как это обязан делать потоковый путь).
func feedAllR83(chunks []string) (reasoning, content string) {
	st := NewReasoningStreamState()
	for _, c := range chunks {
		r, ct := st.Feed(c)
		reasoning += r
		content += ct
	}
	r, ct := st.Finalize()
	return reasoning + r, content + ct
}

// TestReasoningStream_NoMarkerLeakR83 — главный регресс: маркер приходит по
// символам, но в visible content не попадает ни один его фрагмент.
func TestReasoningStream_NoMarkerLeakR83(t *testing.T) {
	chunks := []string{
		"<|c", "h", "a", "nn", "el>thought", "\n",
		"Here is my reasoning", "\n", "<channel|>", "\nFinal answer: 391",
	}
	reasoning, content := feedAllR83(chunks)

	if strings.Contains(content, "<|") || strings.Contains(content, "chann") {
		t.Fatalf("фрагмент маркера утёк в видимый ответ: content=%q", content)
	}
	if strings.Contains(reasoning, "<|") || strings.Contains(reasoning, "channel") {
		t.Fatalf("маркер утёк в reasoning: reasoning=%q", reasoning)
	}
	if !strings.Contains(reasoning, "Here is my reasoning") {
		t.Errorf("reasoning потерян: %q", reasoning)
	}
	if !strings.Contains(content, "Final answer: 391") {
		t.Errorf("видимый ответ потерян: %q", content)
	}
}

// TestReasoningStream_CanonicalSingleChunkR83 — тот же текст одним чанком
// (непотоковый путь): результат обязан совпасть с потоковым.
func TestReasoningStream_CanonicalSingleChunkR83(t *testing.T) {
	one := []string{"<|channel>thought\nHere is my reasoning\n<channel|>\nFinal answer: 391"}
	reasoning, content := feedAllR83(one)
	if strings.Contains(content, "channel") {
		t.Fatalf("в видимом ответе остался маркер: %q", content)
	}
	if !strings.Contains(reasoning, "Here is my reasoning") {
		t.Fatalf("reasoning не отделён: %q", reasoning)
	}
	if !strings.Contains(content, "Final answer: 391") {
		t.Fatalf("видимый ответ потерян: %q", content)
	}
}

// TestReasoningStream_TrailingPartialMarkerFlushed — если поток ОБОРВАЛСЯ на
// неполном маркере, Finalize обязан выпустить удержанный хвост как обычный
// текст, иначе последние символы ответа исчезнут.
func TestReasoningStream_TrailingPartialMarkerFlushed(t *testing.T) {
	chunks := []string{"Hello world", "<|ch"}
	_, content := feedAllR83(chunks)
	if !strings.Contains(content, "Hello world") {
		t.Fatalf("обычный текст потерян: %q", content)
	}
	if !strings.Contains(content, "<|ch") {
		t.Fatalf("удержанный хвост не выпущен в Finalize: content=%q — последние "+
			"символы ответа пропали бы", content)
	}
}

// TestReasoningStream_NoHoldBackForPlainText — обычный текст не задерживается:
// хвост, который не может стать тегом, уходит сразу (никаких «залипаний»).
func TestReasoningStream_NoHoldBackForPlainText(t *testing.T) {
	st := NewReasoningStreamState()
	r1, c1 := st.Feed("Hello ")
	if r1 != "" || c1 != "Hello " {
		t.Fatalf("обычный текст задержан: reasoning=%q content=%q", r1, c1)
	}
	r2, c2 := st.Feed("world")
	if r2 != "" || c2 != "world" {
		t.Fatalf("обычный текст задержан: reasoning=%q content=%q", r2, c2)
	}
}

// TestReasoningStream_LessThanIsNotSwallowed — одиночный '<' в тексте (начало
// любого тега) удерживается ровно до следующего чанка и не теряется.
func TestReasoningStream_LessThanIsNotSwallowed(t *testing.T) {
	chunks := []string{"a <", "b\n", "продолжение"}
	_, content := feedAllR83(chunks)
	if !strings.Contains(content, "a <b") {
		t.Fatalf("текст с '<' потерян или переставлен: %q", content)
	}
	if !strings.Contains(content, "продолжение") {
		t.Fatalf("текст после '<' потерян: %q", content)
	}
}

// TestReasoningStream_FinalizeIsIdempotent — повторный Finalize не должен
// повторно отдавать тот же хвост (иначе в ответе появится дубль).
func TestReasoningStream_FinalizeIsIdempotent(t *testing.T) {
	st := NewReasoningStreamState()
	st.Feed("answer <|ch")
	r1, c1 := st.Finalize()
	if r1 == "" && c1 == "" {
		t.Fatal("Finalize ничего не выпустил, хотя удерживался неполный маркер")
	}
	r2, c2 := st.Finalize()
	if r2 != "" || c2 != "" {
		t.Fatalf("повторный Finalize продублировал данные: reasoning=%q content=%q", r2, c2)
	}
}
