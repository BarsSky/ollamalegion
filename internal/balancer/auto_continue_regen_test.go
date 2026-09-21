// auto_continue_regen_test.go — R60.55 tests for IsContinuationARegeneration.
package balancer

import (
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
// This was the bug in v1 of R60.55 — substring-matching falsely flagged
// real continuations whose code happened to overlap with original.
func TestR6055_IsContinuationARegeneration_NoFalsePositiveOnCommonCode(t *testing.T) {
	original := "const height = parseFloat(document.getElementById('height').value);\n"
	continuation := "  const height = parseFloat(document.getElementById('height').value);"

	if IsContinuationARegeneration(original, continuation) {
		t.Errorf("real code continuation wrongly flagged as regen (substring 'const height = parseFloat(' appears in both)")
	}
}

// TestR6055_IsContinuationARegeneration_RussianGreetingStart — the canonical
// Qwen3-Instruct antipattern: continuation starts with "Привет! Конечно".
func TestR6055_IsContinuationARegeneration_RussianGreetingStart(t *testing.T) {
	original := "function foo() {"
	continuation := "Привет! Конечно, продолжаю код:\n  const x = 5;\n"

	if !IsContinuationARegeneration(original, continuation) {
		t.Errorf("expected regen detected (continuation starts with greeting)")
	}
}

// TestR6055_IsContinuationARegeneration_EnglishGreetingStart — English chat
// model regen: continuation starts with "Sure!" / "Of course".
func TestR6055_IsContinuationARegeneration_EnglishGreetingStart(t *testing.T) {
	original := "function foo() {"
	continuation := "Sure! Here's the rest of the code:\n  const x = 5;\n"

	if !IsContinuationARegeneration(original, continuation) {
		t.Errorf("expected regen detected (English chat model regen)")
	}
}

// TestR6055_IsContinuationARegeneration_RealCodeContinuation — real code
// continuation that starts with whitespace + code (not greeting).
func TestR6055_IsContinuationARegeneration_RealCodeContinuation(t *testing.T) {
	original := "  const speed = 50;\n  const angle = 45;"
	continuation := "  const gravity = 9.8;\n  return results;"

	if IsContinuationARegeneration(original, continuation) {
		t.Errorf("real code continuation wrongly flagged as regen")
	}
}
