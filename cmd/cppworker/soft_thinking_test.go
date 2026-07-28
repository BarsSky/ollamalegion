// soft_thinking_test.go — Round 11 (2026-07-28) tests.
//
// Soft prompt injection для EnableReasoning — universal soft-mode
// enable_thinking для instruction-tuned моделей (gemma-4-it, llama-3-it, и т.д.).
//
// Тесты проверяют:
//   1. injectThinkingInstruction: prepend с правильным форматированием
//   2. injectThinkingIntoMessages: prepend или merge с существующим system
//   3. Both обрабатывают edge cases (empty system, existing system)
package main

import (
	"strings"
	"testing"
)

// TestInjectThinkingInstruction_Empty — когда system пустой, возвращаем
// только instruction (без "\n\n" в конце).
func TestInjectThinkingInstruction_Empty(t *testing.T) {
	result := injectThinkingInstruction("")
	if !strings.Contains(result, "step-by-step thinking") {
		t.Errorf("expected thinking instruction, got %q", result)
	}
	// Не должно быть лишних whitespace в начале/конце
	if strings.HasPrefix(result, " ") || strings.HasSuffix(result, " ") {
		t.Errorf("expected no leading/trailing whitespace, got %q", result)
	}
}

// TestInjectThinkingInstruction_Existing — prepended к существующему
// system промпту с разделителем "\n\n".
func TestInjectThinkingInstruction_Existing(t *testing.T) {
	existing := "You are a helpful assistant."
	result := injectThinkingInstruction(existing)

	// Должен содержать оба: instruction И existing
	if !strings.Contains(result, "step-by-step thinking") {
		t.Errorf("expected thinking instruction, got %q", result)
	}
	if !strings.Contains(result, existing) {
		t.Errorf("expected original system in result, got %q", result)
	}
	// Должен быть разделитель
	if !strings.Contains(result, "\n\n") {
		t.Errorf("expected double-newline separator, got %q", result)
	}

	// Проверяем порядок: instruction ПЕРВЫЙ
	idxInst := strings.Index(result, "step-by-step thinking")
	idxExisting := strings.Index(result, existing)
	if idxInst >= idxExisting {
		t.Errorf("expected instruction before existing, got instruction at %d, existing at %d",
			idxInst, idxExisting)
	}
}

// TestInjectThinkingIntoMessages_Prepend — нет system message → prepended.
func TestInjectThinkingIntoMessages_Prepend(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi"},
	}
	result := injectThinkingIntoMessages(msgs)

	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
	if result[0].Role != "system" {
		t.Errorf("expected first message to be system, got role=%q", result[0].Role)
	}
	if !strings.Contains(result[0].Content, "step-by-step thinking") {
		t.Errorf("expected thinking instruction in first system message, got %q", result[0].Content)
	}
	// Original messages preserved
	if result[1].Content != "Hello" || result[2].Content != "Hi" {
		t.Errorf("expected original messages preserved, got %+v", result[1:])
	}
}

// TestInjectThinkingIntoMessages_Merge — есть system message → конкатенация.
func TestInjectThinkingIntoMessages_Merge(t *testing.T) {
	existingSystem := "You are helpful."
	msgs := []chatMessage{
		{Role: "system", Content: existingSystem},
		{Role: "user", Content: "Hello"},
	}
	result := injectThinkingIntoMessages(msgs)

	if len(result) != 2 {
		t.Fatalf("expected 2 messages (no prepend), got %d", len(result))
	}
	if result[0].Role != "system" {
		t.Errorf("expected first message to be system, got role=%q", result[0].Role)
	}
	// System должен содержать instruction + original
	if !strings.Contains(result[0].Content, "step-by-step thinking") {
		t.Errorf("expected thinking instruction, got %q", result[0].Content)
	}
	if !strings.Contains(result[0].Content, existingSystem) {
		t.Errorf("expected original system, got %q", result[0].Content)
	}
	if !strings.Contains(result[0].Content, "\n\n") {
		t.Errorf("expected double-newline separator, got %q", result[0].Content)
	}
	// User message preserved
	if result[1].Content != "Hello" {
		t.Errorf("expected user message preserved, got %q", result[1].Content)
	}
}

// TestInjectThinkingIntoMessages_OnlySystem — single system message.
func TestInjectThinkingIntoMessages_OnlySystem(t *testing.T) {
	msgs := []chatMessage{{Role: "system", Content: "Be brief."}}
	result := injectThinkingIntoMessages(msgs)

	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if !strings.Contains(result[0].Content, "step-by-step thinking") {
		t.Errorf("expected thinking instruction, got %q", result[0].Content)
	}
	if !strings.Contains(result[0].Content, "Be brief.") {
		t.Errorf("expected original system, got %q", result[0].Content)
	}
}

// TestInjectThinkingIntoMessages_Empty — пустой msgs → возвращаем [system]
// с instruction.
func TestInjectThinkingIntoMessages_Empty(t *testing.T) {
	result := injectThinkingIntoMessages([]chatMessage{})
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].Role != "system" {
		t.Errorf("expected first message to be system, got role=%q", result[0].Role)
	}
	if !strings.Contains(result[0].Content, "step-by-step thinking") {
		t.Errorf("expected thinking instruction, got %q", result[0].Content)
	}
}
