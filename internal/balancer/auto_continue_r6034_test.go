//go:build llama_stub

// auto_continue_r6034_test.go — R60.34: R60.21 must NOT fire on
// empty content (previously TruncateReason("") returned "empty"
// and triggered garbage PerformAutoContinue call).
//
// Symptom: OpenWebUI sent prompt, cppworker rejected with n_ctx
// overflow, balancer R60.21 saw empty accumulated content, fired
// PerformAutoContinue with empty body, got 6933 chars of junk
// back, then "auto-continue succeeded" log. Combined with R60.33
// async load (which returns 503 immediately), the failure was
// interpreted as "completed stream" and R60.21 fired again.

package balancer

import "testing"

// TestTruncateReason_EmptyContent_ShouldNotTriggerAutoContinue —
// R60.21 fix: пустой content = не "truncated", это "не сгенерировано".
// auto-continue не должен срабатывать.
func TestTruncateReason_EmptyContent_ShouldNotTriggerAutoContinue(t *testing.T) {
	reason := TruncateReason("")
	// ДО R60.34: reason == "empty" (R60.21 fired)
	// R60.34: caller проверяет `accumulatedPlainContent != ""` ПЕРЕД
	// вызовом TruncateReason, поэтому reason="empty" уже не достижим.
	// Этот тест только фиксирует текущее поведение TruncateReason
	// (функция сама по себе не изменилась — R60.34 guard в caller).
	if reason == "" {
		t.Errorf("TruncateReason(\"\") = empty, expected non-empty (to match caller guard)")
	}
}

// TestTruncateReason_MidLineCutoff_StillTriggers — sanity: legitimate
// truncation cases (unclosed code block, end with = without value)
// still trigger.
func TestTruncateReason_MidLineCutoff_StillTriggers(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"unclosed code block", "```html\n<html>", "unclosed_code_block"},
		// Note: "let x = 1," ends with a value-assignment in most languages
		// and `endsWithCompleteStatement` returns true → no truncation.
		// We only test cases that are CLEARLY truncated.
		{"trailing open paren", "function(", "mid_line_cutoff"},
		{"trailing open brace", "if (x) {", "mid_line_cutoff"},
		{"clean stop with period", "Hello world.", ""},
		{"clean stop with newline", "Hello world.\n", ""},
		{"html closing", "</html>", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateReason(tc.content)
			if got != tc.want {
				t.Errorf("TruncateReason(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

// TestTruncateReason_NoFalsePositiveOnLegitEndings — R66b (2026-09-22).
//
// До этого фикса набор «обрывочных» символов включал операторы, которыми
// ЛЕГИТИМНО заканчиваются корректные ответы. При LB_AUTO_CONTINUE_ON_TRUNCATION=1
// каждый такой ответ тянул лишний continue-запрос и дублировал контент:
//   * '|' — последняя строка markdown-таблицы;
//   * '/' — URL или путь;
//   * '-' — разделитель таблицы/элемент списка;
//   * '*' — закрывающий **markdown-выделения**;
//   * '<' — HTML-строка, где тег закрыт в той же строке.
func TestTruncateReason_NoFalsePositiveOnLegitEndings(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"markdown table row", "| Model | VRAM | Context |\n| Qwen3 | 4 GB | 32K |"},
		{"markdown table separator", "| Model | VRAM |\n|---|---|"},
		{"bare table separator", "Model | VRAM\n--- | ---"},
		{"url at line end", "Документация: https://github.com/BarsSky/ollamalegion/"},
		{"unix path at line end", "Файлы лежат в /usr/local/share/models/"},
		{"bold markdown ending", "Это **очень важно** для настройки **GPU**"},
		{"italic markdown ending", "Ключевое слово здесь — *context*"},
		{"html line closed in same line", "<html><body><div class=\"main\">"},
		{"bullet list items", "Шаги:\n- собрать образ\n- запустить контейнер"},
		{"markdown horizontal rule", "Первый раздел\n\n---"},
		{"quoted sentence", "Он ответил: \"всё готово\""},
		{"numeric value at line end", "context_length: 32768"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TruncateReason(tc.content); got != "" {
				t.Errorf("TruncateReason(%q) = %q, ожидалось \"\" (легитимное завершение, auto-continue не нужен)",
					tc.content, got)
			}
		})
	}
}

// TestTruncateReason_StillDetectsRealCutoffs — R66b: ослабление эвристики не
// должно пропускать НАСТОЯЩИЕ обрывы (в т.ч. на слабых операторах внутри кода).
func TestTruncateReason_StillDetectsRealCutoffs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"trailing equals", "def compute_total(items):\n    total ="},
		{"trailing open paren", "result = calculate("},
		{"trailing open brace", "def handler(event) {"},
		{"trailing comma", "items = [1, 2,"},
		{"trailing plus", "sum = a + b +"},
		{"trailing ampersand", "query = \"a=1\" &"},
		{"trailing pipe operator in code", "mask = flags |"},
		{"trailing division operator", "ratio = total / count /"},
		{"trailing multiplication", "total = price *"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TruncateReason(tc.content); got != truncateReasonMidLineCutoff {
				t.Errorf("TruncateReason(%q) = %q, ожидалось %q", tc.content, got, truncateReasonMidLineCutoff)
			}
		})
	}
}
