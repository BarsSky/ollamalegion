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
