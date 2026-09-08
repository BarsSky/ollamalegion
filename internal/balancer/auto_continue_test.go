// auto_continue_test.go — R60.21 TDD: detect + retry for truncated LLM responses.
//
// Problem (R60.20): Qwen3-Instruct-2507-q4km emits ChatML-EOS <|im_end|>
// token mid-code generation after ~800-1000 tokens. C-bridge sees it
// as antiprompt match, stream ends. Client receives done_reason=stop
// with content like:
//
//	```python
//	import numpy as np
//	v0 = 50.0
//	... (model stops at `t = ...`)
//	← NO closing ```
//
// 50%+ failure rate on long code generation tasks. cppworker + balancer
// stream faithfully — no data loss. Issue is model-side antiprompt
// emission. Workaround: detect truncation at balancer level and
// automatically retry with "continue" prompt.
//
// Tests in this file:
//  1. DetectIncompleteResponse — pure function, no I/O
//  2. autoContinueOnTruncation (placeholder for integration test)
package balancer

import (
	"strings"
	"testing"
)

// TestDetectIncompleteResponse_UnclosedCodeBlock — primary detector.
// Triggers when content has ODD number of ``` — one code block opened
// but never closed. This is THE pattern from R60.20 reproducer.
func TestDetectIncompleteResponse_UnclosedCodeBlock(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "even backticks (closed)",
			content: "```python\nimport os\n```\nDone.",
			want:    false,
		},
		{
			name:    "odd backticks (unclosed)",
			content: "```python\nimport os\nprint('hi')",
			want:    true,
		},
		{
			name:    "three blocks, all closed",
			content: "```python\nx = 1\n```\ntext\n```js\ny = 2\n```",
			want:    false,
		},
		{
			name:    "two blocks opened, one closed",
			content: "```python\nx = 1\n```\ntext\n```js\ny = 2",
			want:    true,
		},
		{
			name:    "no backticks (plain text)",
			content: "Just plain text without any code block.",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectIncompleteResponse(tc.content)
			if got != tc.want {
				t.Errorf("DetectIncompleteResponse(%q) = %v, want %v",
					tc.content, got, tc.want)
			}
		})
	}
}

// TestDetectIncompleteResponse_MidLineCutoff — heuristic for
// code that ends mid-line. R60.20 reproducer also showed content
// ending at "t_values =" / "trajectory = " — unfinished assignment.
func TestDetectIncompleteResponse_MidLineCutoff(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "ends with = (assignment)",
			content: "```python\nt_values = ",
			want:    true,
		},
		{
			name:    "ends with ( (function call)",
			content: "```python\nprint('hi",
			want:    true,
		},
		{
			name:    "ends with { (dict/brace)",
			content: "```python\nx = {",
			want:    true,
		},
		{
			name:    "ends with [ (list/subscript)",
			content: "```python\narr = [",
			want:    true,
		},
		{
			name:    "ends with , (trailing comma — partial tuple/args)",
			content: "```python\nprint(1, 2,",
			want:    true,
		},
		{
			name:    "ends with . (but unclosed code block — still True)",
			content: "```python\nprint('hi.')",
			want:    true, // unclosed code block has higher priority
		},
		{
			name:    "ends with : (colon — block opener, could be incomplete)",
			content: "```python\nif x > 0:",
			want:    true,
		},
		{
			name:    "ends with newline (but unclosed code block — still True)",
			content: "```python\nx = 1\n",
			want:    true, // unclosed code block has higher priority
		},
		{
			name:    "ends with newline + space (mid-line in unclosed block)",
			content: "```python\nx = 1\n   ",
			want:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectIncompleteResponse(tc.content)
			if got != tc.want {
				t.Errorf("DetectIncompleteResponse(%q) = %v, want %v",
					tc.content, got, tc.want)
			}
		})
	}
}

// TestDetectIncompleteResponse_OnlyAppliesToCodeBlocks — mid-line
// detection should NOT fire on plain text responses. "Hello world"
// or "The answer is 42." should not trigger auto-continue.
func TestDetectIncompleteResponse_OnlyAppliesToCodeBlocks(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "plain text ending with period",
			content: "The answer is 42. The formula is correct.",
			want:    false,
		},
		{
			name:    "plain text ending with newline",
			content: "This is a complete sentence.\n",
			want:    false,
		},
		{
			name:    "bullet list ending cleanly",
			content: "- Item 1\n- Item 2\n- Item 3",
			want:    false,
		},
		{
			name:    "text without code, ending with = (looks like formula)",
			content: "2 + 2 =",
			want:    true, // formula-like ending → mid_line_cutoff
		},
		{
			name:    "empty content",
			content: "",
			want:    false,
		},
		{
			name:    "only whitespace",
			content: "   \n\n  ",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectIncompleteResponse(tc.content)
			if got != tc.want {
				t.Errorf("DetectIncompleteResponse(%q) = %v, want %v",
					tc.content, got, tc.want)
			}
		})
	}
}

// TestDetectIncompleteResponse_RealisticR60_20Cases — actual reproducer
// content from the diagnostic (scripts/diag_openwebui_closing_backticks.py).
// These are the EXACT strings that were cut off mid-generation.
func TestDetectIncompleteResponse_RealisticR60_20Cases(t *testing.T) {
	cases := []string{
		// From Run 1 of R60.20: ended at "times ="
		"```python\nimport numpy as np\nimport matplotlib.pyplot as plt\n" +
			"def simulate_projectile_motion(v0, theta_deg, g=9.81, ...):\n" +
			"    # Инициальные условия\n" +
			"    x, y = 0.0, 0.0\n" +
			"    vx, vy = v0 * np.cos(theta_rad), v0 * np.sin(theta_rad)\n" +
			"    \n" +
			"    # Время и массивы для хранения данных\n" +
			"    times = ",
		// From Run 2: ended at "t_values ="
		"...\n    time = 0.0\n    t_values = ",
		// From Run 3: ended at "t = "
		"...\n    # Временные данные\n    t = ",
		// From Run 4: ended at "trajectory = "
		"...\n    vx, vy = vx0, vy0\n        \n        trajectory = ",
		// User's screenshot: pip install unclosed
		"1. Установил ли ты 'scipy' и 'matplotlib'?\n```bash\npip install numpy matplotlib scipy",
	}
	for i, content := range cases {
		t.Run("r60_20_case_"+string(rune('A'+i)), func(t *testing.T) {
			if !DetectIncompleteResponse(content) {
				t.Errorf("R60.20 reproducer case %d should be detected as incomplete:\n%s",
					i, content)
			}
		})
	}
}

// TestBuildContinuePrompt — sanity check for the prompt template used
// in auto-continue. Should explicitly say "complete the code block" so
// the model knows what to do.
func TestBuildContinuePrompt(t *testing.T) {
	prompt := BuildContinuePrompt(
		"```python\nimport numpy as np\nv0 = 50.0\n",
		"unclosed_code_block",
	)
	if !strings.Contains(prompt, "```") {
		t.Error("continue prompt should mention code blocks")
	}
	if !strings.Contains(prompt, "complete") {
		t.Error("continue prompt should ask to complete")
	}
	if !strings.Contains(prompt, "```python\nimport numpy as np") {
		t.Error("continue prompt should include partial content for context")
	}
}
