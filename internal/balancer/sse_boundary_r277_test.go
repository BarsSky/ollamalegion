package balancer

// sse_boundary_r277_test.go — R277: кадры, которые пишет сам балансер (в том
// числе придержанный finish-кадр из egress-фильтра), обязаны нести границу
// SSE-события.
//
// ЖИВОЙ ДЕФЕКТ (клиент DSH, OpenAI-порт 18079): модель генерировала, но клиент
// падал с «Could not parse message into JSON» и отваливался. В сыром потоке
// придержанный finish-кадр уходил с ОДНИМ переводом строки:
//
//	data: {...,"finish_reason":"stop"}⏎data: [DONE]⏎⏎
//
// Строгий SSE-парсер читает событие до пустой строки, поэтому оба `data:`
// попадали в один блок и парсились как одно JSON-сообщение. Клиенты, которые
// это прощают (Cline), работали — из-за чего дефект и не всплывал раньше.

import (
	"bytes"
	"strings"
	"testing"
)

// TestR277_DataFrameGetsEventBoundary — data-кадр без второго перевода строки
// получает его, и вместе с [DONE] поток остаётся разделённым на события.
func TestR277_DataFrameGetsEventBoundary(t *testing.T) {
	frame := []byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n")
	got := withSSEEventBoundary(frame)

	if !bytes.HasSuffix(got, []byte("\n\n")) {
		t.Fatalf("кадр не получил границу события: %q", got)
	}
	if !bytes.HasPrefix(got, frame) {
		t.Fatalf("содержимое кадра изменено: %q", got)
	}

	// Склеиваем как это видит клиент: событие завершается пустой строкой.
	stream := string(got) + "data: [DONE]\n\n"
	events := splitSSEEvents(stream)
	if len(events) != 2 {
		t.Fatalf("ожидалось 2 SSE-события, получено %d: %q", len(events), stream)
	}
	if !strings.Contains(events[0], `"finish_reason":"stop"`) {
		t.Fatalf("первое событие потеряло finish-кадр: %q", events[0])
	}
	if !strings.Contains(events[1], "[DONE]") {
		t.Fatalf("второе событие не [DONE]: %q", events[1])
	}
}

// TestR277_AlreadyTerminatedFrameUntouched — уже размеченный кадр не удваиваем,
// иначе получим лишнюю пустую строку (безвредно для SSE, но лишний шум).
func TestR277_AlreadyTerminatedFrameUntouched(t *testing.T) {
	frame := []byte("data: {\"a\":1}\n\n")
	got := withSSEEventBoundary(frame)
	if !bytes.Equal(got, frame) {
		t.Fatalf("кадр с готовой границей изменён: %q -> %q", frame, got)
	}
}

// TestR277_CommentLineNotTouched — SSE-комментарий (keepalive) границы события
// не требует: пустая строка после комментария — не событие.
func TestR277_CommentLineNotTouched(t *testing.T) {
	frame := []byte(": keepalive\n")
	got := withSSEEventBoundary(frame)
	if !bytes.Equal(got, frame) {
		t.Fatalf("комментарий изменён: %q -> %q", frame, got)
	}
}

// TestR277_EmptyFrame — пустой кадр (терминатор chunked) не трогаем.
func TestR277_EmptyFrame(t *testing.T) {
	if got := withSSEEventBoundary(nil); len(got) != 0 {
		t.Fatalf("nil-кадр превратился в %q", got)
	}
	if got := withSSEEventBoundary([]byte{}); len(got) != 0 {
		t.Fatalf("пустой кадр превратился в %q", got)
	}
}

// splitSSEEvents — минимальный разбор SSE на события по пустой строке.
// Достаточно для проверки границ, не претендует на полный парсер.
func splitSSEEvents(stream string) []string {
	var events []string
	for _, block := range strings.Split(stream, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		events = append(events, block)
	}
	return events
}
