//go:build llama_stub

package main

import "testing"

// TestR110_ServiceTokenLeadIsHeldBack — вывод, начинающийся со служебного токена
// шаблона, не отдаётся дельтами: служебный токен — не текст ответа.
func TestR110_ServiceTokenLeadIsHeldBack(t *testing.T) {
	cases := []struct {
		buf  string
		want bool
	}{
		{"<end_of_turn>", true},
		{"  \n<end_of_turn>", true},
		{"<start_of_turn>model\n", true},
		{"<|end|>", true},
		{"The answer is 4", false},
		{"", false},
		{"<|tool_call>call:x{}", true}, // обёртка вызова — тоже служебный токен
	}
	for _, tc := range cases {
		if got := toolsStreamStartsWithServiceToken(tc.buf); got != tc.want {
			t.Errorf("toolsStreamStartsWithServiceToken(%q) = %v, ожидалось %v", tc.buf, got, tc.want)
		}
	}
}
