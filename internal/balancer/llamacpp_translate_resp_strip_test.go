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

// TestStripReasoningTags_Gemma4ChannelFormat — Round 32 (2026-08-09).
// Gemma-4 native chat-template format: `<|channel>thought\n[reasoning]\n<channel|>`.
// Если cppworker не разделил reasoning (старая версия или tag разорван
// между чанками), defensive strip в balancer должен почистить content.
func TestStripReasoningTags_Gemma4ChannelFormat(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"gemma4_canonical_with_newlines",
			"<|channel>thought\nStep 1: compute 7*8\nStep 2: get 56\n<channel|>The answer is 56.",
			"The answer is 56.",
		},
		{
			"gemma4_no_newlines_fallback",
			"<|channel>thoughtQuick analysis<channel|>Final answer: 42",
			"Final answer: 42",
		},
		{
			"gemma4_analysis_channel",
			"<|channel>analysis\nThinking here\n<channel|>Answer here",
			"Answer here",
		},
		{
			"gemma4_with_message_separator",
			"<|channel>thought<|message|>\nMy reasoning\n<channel|>My answer",
			"My answer",
		},
		{
			"gemma4_only_opening_no_close",
			// open tag `<|channel>thought\n` (19 chars) consumes \n after thought.
			// stripWithBoundary strips the full 19-char open, content starts
			// directly with "Incomplete thinking..." (no leading \n).
			"<|channel>thought\nIncomplete thinking, no closing tag",
			"Incomplete thinking, no closing tag",
		},
		{
			"gemma4_only_closing_no_open",
			// Round 32 design decision: orphan <channel|> is NOT stripped
			// because close is shared между thought/analysis парами —
			// aggressive strip сломал бы legitimate pair matching.
			// Лучше оставить косметический артефакт, чем сломать split.
			"Incomplete opening<channel|>actual content",
			"Incomplete opening<channel|>actual content",
		},
		{
			"gemma4_message_only",
			"Some content<|message|>more content",
			"Some contentmore content",
		},
		{
			"gemma4_multiple_think_blocks",
			"<|channel>thought\nFirst thinking\n<channel|>middle<|channel>thought\nSecond thinking\n<channel|>final",
			"middlefinal",
		},
		{
			"gemma4_empty_input",
			"",
			"",
		},
		{
			"gemma4_realistic_user_example",
			// Точная репродукция бага из user report (2026-08-09):
			// <|channel>thought\n...long thinking...\n<channel|>actual response
			"<|channel>thought\nHere's a thinking process that leads to the suggested HTML code:\n\nStep 1\nStep 2\n<channel|>This is a great task!",
			"This is a great task!",
		},
		{
			"qwen_style_bars",
			// Qwen-style с обёрткой |...| — Round 32
			"<|think>Step 1: think\nStep 2: more<think|>The answer is 56.",
			"The answer is 56.",
		},
		{
			"qwen_style_bars_unclosed",
			"<|think>Long thinking without close",
			"Long thinking without close",
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
