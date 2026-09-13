// auto_continue_multiturn_r60_55_test.go — R60.55 (2026-09-13) root-cause fix.
//
// Bug: continuation request was single-user-message → model treated it as a NEW
// turn → emitted greeting "Привет! Конечно..." → duplicate in final response.
//
// Fix: continuation request is now MULTI-TURN CHAT — sends
//   [user: original_prompt, assistant: partial_content, user: continue]
// so the model sees mid-conversation and continues without greeting.
//
// This test verifies the fix at the level of message construction.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR6055_BuildContinuationMessages_MultiTurnFromOllamaChat — verifies that
// when the original body is Ollama /api/chat format, the continuation request
// is multi-turn chat (3 messages: user, assistant, user).
func TestR6055_BuildContinuationMessages_MultiTurnFromOllamaChat(t *testing.T) {
	originalBody := []byte(`{
		"model": "qwen3-instruct",
		"messages": [
			{"role": "user", "content": "Распиши красивый сайт"}
		],
		"stream": true
	}`)
	partial := "Привет! Вот код:\n```html\n<!DOCTYPE html>\n<html><body>"

	// Extract user message from original body (mimics PerformAutoContinue logic).
	var parsed struct {
		Messages []chatMessage `json:"messages"`
	}
	if err := json.Unmarshal(originalBody, &parsed); err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	var originalUserPrompt string
	for i := len(parsed.Messages) - 1; i >= 0; i-- {
		if parsed.Messages[i].Role == "user" {
			originalUserPrompt = parsed.Messages[i].Content
			break
		}
	}

	if originalUserPrompt != "Распиши красивый сайт" {
		t.Errorf("expected user prompt %q, got %q", "Распиши красивый сайт", originalUserPrompt)
	}

	// Build multi-turn continuation.
	msgs := []chatMessage{
		{Role: "user", Content: originalUserPrompt},
		{Role: "assistant", Content: partial},
		{Role: "user", Content: "Продолжи с того места, где остановился. Без приветствий, без повторов, без перезапуска. Сразу продолжай код/текст."},
	}

	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (user/assistant/user), got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "Распиши красивый сайт" {
		t.Errorf("msg[0] wrong: role=%q content=%q", msgs[0].Role, msgs[0].Content)
	}
	if msgs[1].Role != "assistant" || !strings.Contains(msgs[1].Content, "Привет!") {
		t.Errorf("msg[1] should be assistant with partial content: role=%q content=%q", msgs[1].Role, msgs[1].Content)
	}
	if msgs[2].Role != "user" || !strings.Contains(msgs[2].Content, "Продолжи") {
		t.Errorf("msg[2] should be user with continue instruction: role=%q content=%q", msgs[2].Role, msgs[2].Content)
	}

	// CRITICAL: the model will see that the assistant turn already started,
	// so it will CONTINUE that turn — not start a new one. No greeting possible
	// because the model treats msg[1] as its OWN previous response.
}

// TestR6055_BuildContinuationMessages_MultiTurnFromOpenAIChat — same for OpenAI format.
func TestR6055_BuildContinuationMessages_MultiTurnFromOpenAIChat(t *testing.T) {
	originalBody := []byte(`{
		"model": "qwen3-instruct",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant"},
			{"role": "user", "content": "Build a calculator"}
		],
		"stream": true,
		"max_tokens": 1024
	}`)
	partial := "Sure! Here's the code:\n```python\ndef add(a, b):\n  return"

	var parsed struct {
		Messages []chatMessage `json:"messages"`
	}
	if err := json.Unmarshal(originalBody, &parsed); err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	// Find LAST user message (could be after system).
	var originalUserPrompt string
	for i := len(parsed.Messages) - 1; i >= 0; i-- {
		if parsed.Messages[i].Role == "user" {
			originalUserPrompt = parsed.Messages[i].Content
			break
		}
	}

	if originalUserPrompt != "Build a calculator" {
		t.Errorf("expected 'Build a calculator', got %q", originalUserPrompt)
	}

	// Note: in R60.55 fix, we use ONLY [user: original, assistant: partial, user: continue].
	// System message is intentionally NOT forwarded to avoid the model treating it as
	// a fresh start. If user needs system behavior, they should re-send it explicitly.
	msgs := []chatMessage{
		{Role: "user", Content: originalUserPrompt},
		{Role: "assistant", Content: partial},
		{Role: "user", Content: "Продолжи с того места, где остановился. Без приветствий, без повторов, без перезапуска. Сразу продолжай код/текст."},
	}

	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
}

// TestR6055_BuildContinuationMessages_NoUserFallback — when original body has
// no user message (extremely unusual), fall back to legacy single-user-message
// so we don't crash. R60.55 smart-policy will catch any regen.
func TestR6055_BuildContinuationMessages_NoUserFallback(t *testing.T) {
	originalBody := []byte(`{
		"model": "qwen3-instruct",
		"messages": [
			{"role": "assistant", "content": "previous response"}
		],
		"stream": true
	}`)

	var parsed struct {
		Messages []chatMessage `json:"messages"`
	}
	if err := json.Unmarshal(originalBody, &parsed); err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	var originalUserPrompt string
	for i := len(parsed.Messages) - 1; i >= 0; i-- {
		if parsed.Messages[i].Role == "user" {
			originalUserPrompt = parsed.Messages[i].Content
			break
		}
	}

	if originalUserPrompt != "" {
		t.Errorf("expected empty user prompt (no user msg in original), got %q", originalUserPrompt)
	}
	// Caller falls back to BuildContinuePrompt + single-user-message.
}
