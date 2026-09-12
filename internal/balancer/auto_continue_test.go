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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
// in auto-continue. Should explicitly say "continue" + reference partial content.
func TestBuildContinuePrompt(t *testing.T) {
	prompt := BuildContinuePrompt(
		"```python\nimport numpy as np\nv0 = 50.0\n",
		"unclosed_code_block",
	)
	if !strings.Contains(prompt, "```") {
		t.Error("continue prompt should mention code blocks")
	}
	if !strings.Contains(prompt, "Продолжи") {
		t.Error("continue prompt should ask to continue (R60.21)")
	}
	if !strings.Contains(prompt, "```python\nimport numpy as np") {
		t.Error("continue prompt should include partial content for context")
	}
}

// TestR6050_BuildContinuePrompt_NoGreetingInstructions — R60.50 fix.
// The continuation prompt must EXPLICITLY forbid the model from greeting
// or restarting, because Qwen3-Instruct tends to restart with "Привет! Конечно..."
// when it sees the original user message in conversation history.
func TestR6050_BuildContinuePrompt_NoGreetingInstructions(t *testing.T) {
	prompt := BuildContinuePrompt(
		"```python\nimport numpy as np\nv0 = 50.0\n",
		"unclosed_code_block",
	)
	// Must explicitly tell the model NOT to greet.
	if !strings.Contains(prompt, "здоровайся") {
		t.Error("R60.50: continue prompt must forbid greeting (model tends to restart with 'Привет')")
	}
	// Must explicitly tell the model NOT to repeat.
	if !strings.Contains(prompt, "повторяй") {
		t.Error("R60.50: continue prompt must forbid repeating (avoid duplicate content)")
	}
	// Must explicitly tell the model to continue from where it stopped.
	if !strings.Contains(prompt, "Продолжи ТОЧНО") {
		t.Error("R60.50: continue prompt must explicitly say 'continue exactly from here'")
	}
}

// TestR6050_BuildContinuePrompt_IncludesAnchor — R60.50: anchor is now 400 chars (was 200).
func TestR6050_BuildContinuePrompt_IncludesAnchor(t *testing.T) {
	longPartial := strings.Repeat("a", 1000) + "let xPoints ="
	prompt := BuildContinuePrompt(longPartial, "mid_line_cutoff")
	// Must include the last 400 chars as anchor.
	expectedAnchor := longPartial[len(longPartial)-400:]
	if !strings.Contains(prompt, expectedAnchor) {
		t.Error("R60.50: continue prompt should include last 400 chars of partial content")
	}
}

// TestR6050_PerformAutoContinue_SingleUserMessage — R60.50 invariant:
// The continuation request body must contain ONLY a single user message,
// NOT the original conversation. Qwen3-Instruct tends to restart with
// "Привет! Конечно..." when it sees the original user message in history,
// which is the user-visible bug R60.50 fixes.
func TestR6050_PerformAutoContinue_SingleUserMessage(t *testing.T) {
	// Spin up a fake upstream that records the request body.
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		// Return a minimal valid OpenAI response so PerformAutoContinue succeeds.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"continuation ok"}}],"usage":{"completion_tokens":10}}`))
	}))
	defer srv.Close()

	// Original request body: user said "Привет распиши..." — MUST NOT appear in continuation.
	originalBody := []byte(`{"model":"Qwen3","messages":[{"role":"user","content":"Привет распиши красивый сайт на html css"}],"stream":true}`)

	_, _, err := PerformAutoContinue(
		srv.URL, originalBody,
		"```html\n<html><body>partial</body></html>\nlet xPoints =",
		truncateReasonUnclosedCodeBlock,
		nil, 30*time.Second,
	)
	if err != nil {
		t.Fatalf("PerformAutoContinue: %v", err)
	}

	// Verify captured body has only 1 message.
	var parsed struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("parse captured body: %v\nbody=%s", err, capturedBody)
	}
	if len(parsed.Messages) != 1 {
		t.Errorf("R60.50: continuation request must have exactly 1 message (was %d). Multiple messages caused model to restart with greeting.", len(parsed.Messages))
	}
	if parsed.Model != "Qwen3" {
		t.Errorf("model name lost: %q", parsed.Model)
	}
	if len(parsed.Messages) >= 1 {
		if parsed.Messages[0].Role != "user" {
			t.Errorf("single message must be 'user', got %q", parsed.Messages[0].Role)
		}
		// The original user prompt "Привет распиши..." MUST NOT appear in continuation.
		if strings.Contains(parsed.Messages[0].Content, "Привет распиши") {
			t.Error("R60.50: original user prompt leaked into continuation request body — this causes the model to restart with greeting")
		}
		// But the truncated content MUST be referenced (so model can resume).
		if !strings.Contains(parsed.Messages[0].Content, "let xPoints =") {
			t.Error("R60.50: continuation message must include anchor (last 400 chars of truncated content)")
		}
	}
}

// TestR6050_FixMarkdownCodeFences — R60.50: keep first ``` (for syntax
// highlighting), replace all subsequent ``` with non-fence representation.
// Qwen3-Instruct emits ``` inside JS code, breaking markdown rendering.
func TestR6050_FixMarkdownCodeFences(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no fences — unchanged",
			input: "no code blocks here",
			want:  "no code blocks here",
		},
		{
			name:  "single fence — unchanged (no subsequent fences to replace)",
			input: "before ```html\nfoo",
			want:  "before ```html\nfoo",
		},
		{
			name:  "two fences — first kept, second replaced (the closing fence becomes non-fence — code block has no closer)",
			input: "before ```html\nfoo\n``` after",
			want:  "before ```html\nfoo\n` ` after",
		},
		{
			name:  "Qwen3-Instruct antipattern (the user bug)",
			input: "```html\nconst height = value```\n) || 0;\n``` end",
			want:  "```html\nconst height = value` `\n) || 0;\n` ` end",
		},
		{
			name:  "multiple interior fences all replaced (only first kept)",
			input: "```html\n```\nfoo\n```\nbar\n``` end",
			want:  "```html\n` `\nfoo\n` `\nbar\n` ` end",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FixMarkdownCodeFences(tc.input)
			if got != tc.want {
				t.Errorf("FixMarkdownCodeFences:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
