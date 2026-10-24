// llama_build_guard_r167_test.go — R167 (2026-10-17): сверка «коммит llama.cpp
// из env» ↔ «коммит, вшитый в бинарник».
//
// ЖИВОЙ СЛУЧАЙ, ради которого это появилось. На стенде env контейнера
// (CPPWORKER_LLAMA_BUILD) и реально слинкованная llama.cpp разошлись: агент донора
// отдавал «тот же коммит», потому что читал env, а бинарник был старше. Проверка
// version_mismatch у узлов раскладки сравнивала env с env — то есть всегда
// соглашалась. Здесь фиксируется, что расхождение ВИДНО, а не молчит.
package main

import "testing"

func TestR167LlamaBuildMatches(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		binary string
		want   bool
	}{
		{"совпало", "0eadefeb", "0eadefeb", true},
		{"env новее (пересобрали не тот образ)", "0eadefeb", "1d2869c6e", false},
		{"бинарь новее (забыли .env)", "1d2869c6e", "0eadefeb", false},
		{"env пуст — сравнивать нечего, не ругаемся", "", "0eadefeb", true},
		{"бинарь неизвестен (stub)", "0eadefeb", "stub", true},
		{"регистр не важен", "0EADEFEB", "0eadefeb", true},
		{"префикс в env", "0eadefeb-extra", "0eadefeb", true},
	}
	for _, c := range cases {
		if got := llamaBuildCompatible(c.env, c.binary); got != c.want {
			t.Errorf("%s: llamaBuildCompatible(%q, %q) = %v, want %v",
				c.name, c.env, c.binary, got, c.want)
		}
	}
}
