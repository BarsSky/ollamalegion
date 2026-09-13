// auto_continue_regen_test.go — R60.55 tests for IsContinuationARegeneration.
package balancer

import (
	"strings"
	"testing"
)

// TestR6055_IsContinuationARegeneration_BasicRegeneration — chat model
// restarts with greeting: continuation looks like regeneration.
func TestR6055_IsContinuationARegeneration_BasicRegeneration(t *testing.T) {
	original := "Привет! Конечно, вот **красивый сайт на HTML** для расчёта полёта.\n\n```html\n<!DOCTYPE html>\n<html>\n<body>\n  <div class=\"form\">\n    <input id=\"speed\" />"
	continuation := "Привет! Конечно, вот **красивый сайт на HTML** для расчёта полёта.\n\n```html\n<!DOCTYPE html>\n<html>\n<body>\n  <div class=\"form\">\n    <input id=\"speed\" />\n  </div>\n</body>"

	if !IsContinuationARegeneration(original, continuation) {
		t.Errorf("expected regeneration detected, got false")
	}
}

// TestR6055_IsContinuationARegeneration_RealContinuation — true continuation
// (different content, no greeting).
func TestR6055_IsContinuationARegeneration_RealContinuation(t *testing.T) {
	original := "Привет! Вот код:\n\n```html\n<html>\n<body>\n  <div class=\"form\">\n    <input id=\"speed\" />"
	// Continuation actually continues from the anchor — different content
	continuation := "  </div>\n  <button>Calc</button>\n</body>\n</html>\n```"

	if IsContinuationARegeneration(original, continuation) {
		t.Errorf("expected real continuation (NOT regen), got true (false positive)")
	}
}

// TestR6055_IsContinuationARegeneration_Empty — guards against empty inputs.
func TestR6055_IsContinuationARegeneration_Empty(t *testing.T) {
	if IsContinuationARegeneration("", "something") {
		t.Errorf("empty original should return false")
	}
	if IsContinuationARegeneration("something", "") {
		t.Errorf("empty continuation should return false")
	}
	if IsContinuationARegeneration("", "") {
		t.Errorf("both empty should return false")
	}
}

// TestR6055_IsContinuationARegeneration_ShortContinuation — too short to detect.
func TestR6055_IsContinuationARegeneration_ShortContinuation(t *testing.T) {
	original := "Привет! Это длинный оригинальный контент с кучей текста про полёт и математику."
	continuation := "Короткий" // less than 80 chars

	if IsContinuationARegeneration(original, continuation) {
		t.Errorf("short continuation should return false (can't detect)")
	}
}

// TestR6055_IsContinuationARegeneration_EnglishChatModel — English chat model
// regenerates with similar structure.
func TestR6055_IsContinuationARegeneration_EnglishChatModel(t *testing.T) {
	original := "Sure! Here's a beautiful HTML page for you:\n\n```html\n<!DOCTYPE html>\n<html>\n<body>\n  <div class=\"form\">"
	continuation := "Sure! Here's a beautiful HTML page for you:\n\n```html\n<!DOCTYPE html>\n<html>\n<body>\n  <div class=\"form\">\n    <input />\n  </div>"

	if !IsContinuationARegeneration(original, continuation) {
		t.Errorf("expected regen detected for English chat model")
	}
}

// TestR6055_IsContinuationARegeneration_EnglishPreambleStripping — "Sure, here's the continuation:"
// preamble should be stripped before anchor check.
func TestR6055_IsContinuationARegeneration_EnglishPreambleStripping(t *testing.T) {
	original := "Sure! Here's the HTML page you requested with the form."
	continuation := "Sure, here's the continuation:\n\nHere's the HTML page you requested with the form.\n\n```html\n<input />"

	if !IsContinuationARegeneration(original, continuation) {
		t.Errorf("expected regen detected after preamble strip")
	}
}

// TestR6055_IsContinuationARegeneration_NoFalsePositiveOnCommonCode — code
// continuation that happens to share common substrings shouldn't trigger.
func TestR6055_IsContinuationARegeneration_NoFalsePositiveOnCommonCode(t *testing.T) {
	original := strings.Repeat("function calculateTrajectory() {\n  const speed = 50;\n", 100)
	continuation := "  return results;\n}"

	if IsContinuationARegeneration(original, continuation) {
		t.Errorf("real code continuation wrongly flagged as regen")
	}
}
