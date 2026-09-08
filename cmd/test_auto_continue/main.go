// Test runner for auto_continue_test.go — bypasses pre-existing
// test build issues (undefined ollamaFakeServer, cppworkerFakeServer in
// scenarios_*.go). Standalone verification of pure-function logic.
//
// Build: cd cmd/test_auto_continue && go test -v
// Or:    go run cmd/test_auto_continue/main.go
package main

import (
	"fmt"
	"os"
	"strings"

	"ollama-loadbalancer/internal/balancer"
)

func main() {
	failed := 0
	passed := 0

	// Test detectIncompleteResponse — unclosed code block
	fmt.Println("=== detectIncompleteResponse — unclosed code block ===")
	ucCases := []struct {
		name    string
		content string
		want    bool
	}{
		{"even backticks (closed)", "```python\nimport os\n```\nDone.", false},
		{"odd backticks (unclosed)", "```python\nimport os\nprint('hi')", true},
		{"three blocks all closed", "```python\nx = 1\n```\ntext\n```js\ny = 2\n```", false},
		{"two opened, one closed", "```python\nx = 1\n```\ntext\n```js\ny = 2", true},
		{"no backticks", "Just plain text without any code block.", false},
	}
	for _, tc := range ucCases {
		got := balancer.DetectIncompleteResponse(tc.content)
		status := "OK"
		if got != tc.want {
			status = "FAIL"
			failed++
		} else {
			passed++
		}
		fmt.Printf("  [%s] %s: got=%v want=%v\n", status, tc.name, got, tc.want)
	}

	// Test mid-line cutoff
	fmt.Println()
	fmt.Println("=== detectIncompleteResponse — mid-line cutoff ===")
	mlCases := []struct {
		name    string
		content string
		want    bool
	}{
		{"ends with =", "```python\nt_values = ", true},
		{"ends with (", "```python\nprint('hi", true},
		{"ends with {", "```python\nx = {", true},
		{"ends with [", "```python\narr = [", true},
		{"ends with ,", "```python\nprint(1, 2,", true},
		{"ends with .", "```python\nprint('hi.')", false},
		{"ends with :", "```python\nif x > 0:", true},
		{"ends with newline", "```python\nx = 1\n", false},
		{"ends with newline + spaces", "```python\nx = 1\n   ", true},
	}
	for _, tc := range mlCases {
		got := balancer.DetectIncompleteResponse(tc.content)
		status := "OK"
		if got != tc.want {
			status = "FAIL"
			failed++
		} else {
			passed++
		}
		fmt.Printf("  [%s] %s: got=%v want=%v\n", status, tc.name, got, tc.want)
	}

	// Test plain text behavior
	fmt.Println()
	fmt.Println("=== detectIncompleteResponse — plain text ===")
	ptCases := []struct {
		name    string
		content string
		want    bool
	}{
		{"plain text ending with period", "The answer is 42. The formula is correct.", false},
		{"plain text ending with newline", "This is a complete sentence.\n", false},
		{"bullet list ending cleanly", "- Item 1\n- Item 2\n- Item 3", false},
		{"empty content", "", false},
		{"only whitespace", "   \n\n  ", false},
	}
	for _, tc := range ptCases {
		got := balancer.DetectIncompleteResponse(tc.content)
		status := "OK"
		if got != tc.want {
			status = "FAIL"
			failed++
		} else {
			passed++
		}
		fmt.Printf("  [%s] %s: got=%v want=%v\n", status, tc.name, got, tc.want)
	}

	// Test R60.20 real cases
	fmt.Println()
	fmt.Println("=== detectIncompleteResponse — R60.20 real cases ===")
	r60Cases := []string{
		"```python\nimport numpy as np\nimport matplotlib.pyplot as plt\n" +
			"def simulate_projectile_motion(v0, theta_deg, g=9.81, ...):\n" +
			"    # Инициальные условия\n" +
			"    x, y = 0.0, 0.0\n" +
			"    vx, vy = v0 * np.cos(theta_rad), v0 * np.sin(theta_rad)\n" +
			"    \n" +
			"    # Время и массивы для хранения данных\n" +
			"    times = ",
		"...\n    time = 0.0\n    t_values = ",
		"...\n    # Временные данные\n    t = ",
		"...\n    vx, vy = vx0, vy0\n        \n        trajectory = ",
		"1. Установил ли ты 'scipy' и 'matplotlib'?\n```bash\npip install numpy matplotlib scipy",
	}
	for i, content := range r60Cases {
		got := balancer.DetectIncompleteResponse(content)
		name := fmt.Sprintf("r60_20_case_%c", 'A'+i)
		status := "OK"
		if !got {
			status = "FAIL"
			failed++
		} else {
			passed++
		}
		fmt.Printf("  [%s] %s: got=%v (expected true)\n", status, name, got)
	}

	// Test buildContinuePrompt
	fmt.Println()
	fmt.Println("=== buildContinuePrompt ===")
	promptUnclosed := balancer.BuildContinuePrompt(
		"```python\nimport numpy as np\nv0 = 50.0\n",
		"unclosed_code_block",
	)
	checks := []struct {
		name     string
		expected bool
	}{
		{"mentions ```", strings.Contains(promptUnclosed, "```")},
		{"mentions complete", strings.Contains(promptUnclosed, "Продолжи")},
		{"includes partial content", strings.Contains(promptUnclosed, "import numpy as np")},
		{"mentions closing fence", strings.Contains(promptUnclosed, "закрой")},
	}
	for _, c := range checks {
		status := "OK"
		if !c.expected {
			status = "FAIL"
			failed++
		} else {
			passed++
		}
		fmt.Printf("  [%s] %s: %v\n", status, c.name, c.expected)
	}
	fmt.Println()
	fmt.Printf("  Prompt preview: %s\n", promptUnclosed[:min(200, len(promptUnclosed))])

	// Test truncateReason
	fmt.Println()
	fmt.Println("=== truncateReason ===")
	trCases := []struct {
		content string
		want    string
	}{
		{"```python\nx = 1\n", "unclosed_code_block"},
		{"```python\nt = ", "mid_line_cutoff"},
		{"Hello world.", ""},
		{"", "empty"},
		{"```python\nx = 1\n```\nDone.", ""},
	}
	for _, tc := range trCases {
		got := balancer.TruncateReason(tc.content)
		status := "OK"
		if got != tc.want {
			status = "FAIL"
			failed++
		} else {
			passed++
		}
		fmt.Printf("  [%s] %q → %q (want %q)\n", status, tc.content, got, tc.want)
	}

	fmt.Println()
	fmt.Printf("=== RESULT: %d passed, %d failed ===\n", passed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
