// reasoning_hold_r190_test.go — R190 (2026-10-20).
//
// Живой дефект: вывод начинался с блока размышлений (gemma-4/Qwen3.8), после чего
// toolsHold выставлялся и БОЛЬШЕ НЕ СНИМАЛСЯ — видимый ответ (content_len=378)
// уходил клиенту одним кадром в конце хода, то есть минуты тишины.
package main

import "testing"

func TestR190_ReasoningBlockEnd(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"think закрыт", "<|channel>thought думаю <channel|>ответ", len("<|channel>thought думаю <channel|>")},
		{"think тег", "<think>размышление</think>ответ", len("<think>размышление</think>")},
		{"несколько блоков — берём последний", "<think>a</think> текст <think>b</think>ответ", len("<think>a</think> текст <think>b</think>")},
		{"блок не закрыт — держим", "<|channel>thought всё ещё думаю", -1},
		{"без разметки", "просто ответ", -1},
	}
	for _, c := range cases {
		if got := reasoningBlockEnd(c.in); got != c.want {
			t.Errorf("%s: end=%d, ожидалось %d", c.name, got, c.want)
		}
	}
}
