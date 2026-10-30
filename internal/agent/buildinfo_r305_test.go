//go:build llama_stub

// buildinfo_r305_test.go — R305 (2026-10-30): узел сообщает коммит ВМЕСТЕ с датой.
//
// ЗАЧЕМ. Балансер сравнивает сборку узла с последним стабильным релизом llama.cpp.
// Пока у узла только коммит, а у релиза тег и дата, сравнение честно упирается в
// «different commits, no comparable version». Дата приезжает из образа
// (CPPWORKER_LLAMA_BUILD_DATE) и уходит в buildInfo строкой «коммит ГГГГ-ММ-ДД»,
// которую балансер разбирает своим parseLlamaBuild.
package agent

import "testing"

func TestR305_BuildInfoCarriesCommitAndDate(t *testing.T) {
	t.Setenv("CPPWORKER_LLAMA_BUILD", "0eadefebd")
	t.Setenv("CPPWORKER_LLAMA_BUILD_DATE", "2026-09-01")
	if got := llamaBuildInfoR101Agent(); got != "0eadefebd 2026-09-01" {
		t.Fatalf("buildInfo = %q, ожидалось \"0eadefebd 2026-09-01\"", got)
	}
}

func TestR305_BuildInfoWithoutDateKeepsOldForm(t *testing.T) {
	t.Setenv("CPPWORKER_LLAMA_BUILD", "0eadefebd")
	t.Setenv("CPPWORKER_LLAMA_BUILD_DATE", "")
	if got := llamaBuildInfoR101Agent(); got != "0eadefebd" {
		t.Fatalf("buildInfo = %q, ожидался только коммит (старый образ без даты)", got)
	}
}

func TestR305_BuildInfoEmptyWhenCommitMissing(t *testing.T) {
	t.Setenv("CPPWORKER_LLAMA_BUILD", "")
	t.Setenv("CPPWORKER_LLAMA_BUILD_DATE", "2026-09-01")
	if got := llamaBuildInfoR101Agent(); got != "" {
		t.Fatalf("buildInfo = %q, ожидалась пустая строка: одной даты мало", got)
	}
}