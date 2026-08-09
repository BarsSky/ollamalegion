// llamacpp_translate_resp_strip_test.go — tests for stripReasoningTags.
//
// Round 31 (2026-08-09): defensive cleanup of reasoning tag leaks in content
// from gemma-4 (cppworker's SplitReasoningContent sometimes leaves close tag
// in content when reasoning_content is set).
package balancer

import "testing"

// TestStripReasoningTags_AllPairs — все 4 типа тегов должны быть удалены.
func TestStripReasoningTags_AllPairs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"gemma4_leak_closetag",
			"\n7 * 8 = 56.\n</think>\n<start_of_turn>56",
			"7 * 8 = 56.\n<start_of_turn>56",
		},
		{
			"gemma4_just_opentag",
			"<think\nThe answer is 42.",
			"The answer is 42.",
		},
		{
			"qwen3_standard",
			"<think>r1</think>answer",
			"answer",
		},
		{
			"qwen3_instruct_reasoning_tag",
			"<reasoning>step by step</reasoning>final answer",
			"final answer",
		},
		{
			"gemma3_thinking_alt",
			"<thinking>let me think</thinking>the answer is 42",
			"the answer is 42",
		},
		{
			"o1_analysis",
			"<analysis>step by step</analysis>the answer",
			"the answer",
		},
		{
			"no_tags",
			"plain text",
			"plain text",
		},
		{
			"empty",
			"",
			"",
		},
		{
			"only_tags",
			"<think>r</think>",
			"",
		},
		{
			"leading_whitespace",
			"\n\n\nhello",
			"hello",
		},
		{
			"trailing_tags",
			"actual answer</think>",
			"actual answer",
		},
		{
			"unclosed_tag",
			"<think>opened but not closed",
			"opened but not closed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripReasoningTags(tt.input)
			if got != tt.want {
				t.Errorf("stripReasoningTags(%q) = %q, want %q",
					tt.input, got, tt.want)
			}
		})
	}
}

// TestStripReasoningTags_Gemma4RealisticOutput —
// Realistic gemma-4 output with leaked close tag.
func TestStripReasoningTags_Gemma4RealisticOutput(t *testing.T) {
	input := "\nThe answer is 42.\n</think>\n<start_of_turn>model\n42"
	want := "The answer is 42.\n<start_of_turn>model\n42"
	got := stripReasoningTags(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
