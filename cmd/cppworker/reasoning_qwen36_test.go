// reasoning_qwen36_test.go — tests for qwen3.6 specific behavior.
//
// Bug context (2026-08-09):
//   qwen3.6 35B-A3B-UD-Q4_K_M (22GB) — MoE архитектура с reasoning.
//   Balancer не работает корректно с этой моделью: ошибки и в OpenWebUI и в Cline.
//
//   Гипотезы:
//     1. cppworker не распознаёт qwen3.6 как reasoning (IsReasoningModel)
//     2. SplitReasoningContent неправильно split'ит вывод qwen3.6
//     3. Balancer firstByteTimeout слишком мал для 22GB модели
//
// Покрывает:
//   - IsReasoningModel для qwen3.6 вариантов имени
//   - SplitReasoningContent для qwen3.6-стиля (длинный thinking, moe-теги)
//   - ReasoningArchPrefixes contains qwen3.6
package main

import (
	"testing"
)

// ============================================================
// IsReasoningModel для qwen3.6
// ============================================================

func TestIsReasoningModel_Qwen36(t *testing.T) {
	tests := []struct {
		name      string
		modelName string
		want      bool
	}{
		// Should be true — reasoning model
		{"qwen3.6_standard", "Qwen3.6-35B-A3B-UD-Q4_K_M", true},
		{"qwen3.6_lowercase", "qwen3.6-35b-a3b-ud-q4_k_m", true},
		{"qwen3.6_with_gguf", "Qwen3.6-35B-A3B-UD-Q4_K_M.gguf", true},
		{"qwen3.5moe", "qwen3.5moe-72b", true},
		{"qwen35moe", "qwen35moe-72b", true},
		{"qwen3moe", "qwen3moe-30b", true},

		// Should be false — non-reasoning variants
		{"qwen3_instruct", "qwen3-instruct-7b", false},
		{"qwen3_chat", "qwen3-chat-14b", false},
		{"qwen2", "qwen2-7b", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReasoningModel(tt.modelName); got != tt.want {
				t.Errorf("IsReasoningModel(%q) = %v, want %v", tt.modelName, got, tt.want)
			}
		})
	}
}

// TestReasoningArchPrefixes_Contains_Qwen36 —
// Sanity test: префикс qwen3.6 должен быть в whitelist ReasoningArchPrefixes.
func TestReasoningArchPrefixes_Contains_Qwen36(t *testing.T) {
	found := false
	for _, prefix := range ReasoningArchPrefixes {
		if prefix == "qwen3.6" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ReasoningArchPrefixes does NOT contain 'qwen3.6'. Current list: %v", ReasoningArchPrefixes)
	}
}

// ============================================================
// SplitReasoningContent для qwen3.6
// ============================================================

func TestSplitReasoningContent_Qwen36(t *testing.T) {
	// qwen3.6 может использовать <think> или альтернативные теги.
	// Soft prompt от Cline/OpenWebUI может также влиять.
	tests := []struct {
		name        string
		input       string
		wantReason  string
		wantContent string
		wantHas     bool
	}{
		{
			name:        "qwen3.6_standard_think",
			input:       "<think>Step 1: compute 7*8. Step 2: get 56.</think>The answer is 56.",
			wantReason:  "Step 1: compute 7*8. Step 2: get 56.",
			wantContent: "The answer is 56.",
			wantHas:     true,
		},
		{
			name:        "qwen3.6_with_prefix_thinking",
			input:       "Let me think.<thinking>Multi-step reasoning here.</thinking>Final answer: 42.",
			wantReason:  "Multi-step reasoning here.",
			wantContent: "Let me think.Final answer: 42.",
			wantHas:     true,
		},
		{
			name:        "qwen3.6_multiple_think_blocks",
			input:       "<think>r1</think>pre<think>r2</think>final",
			wantReason:  "r1r2",
			wantContent: "prefinal",
			wantHas:     true,
		},
		{
			name:        "qwen3.6_unclosed_think",
			input:       "<think>this is a long thinking that gets cut off",
			wantReason:  "this is a long thinking that gets cut off",
			wantContent: "",
			wantHas:     true,
		},
		{
			name:        "qwen3.6_no_think",
			input:       "Just a normal answer without thinking",
			wantReason:  "",
			wantContent: "Just a normal answer without thinking",
			wantHas:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotR, gotC, gotHas := SplitReasoningContent(tt.input)
			if gotR != tt.wantReason {
				t.Errorf("reasoning: got %q, want %q", gotR, tt.wantReason)
			}
			if gotC != tt.wantContent {
				t.Errorf("content: got %q, want %q", gotC, tt.wantContent)
			}
			if gotHas != tt.wantHas {
				t.Errorf("has: got %v, want %v", gotHas, tt.wantHas)
			}
		})
	}
}
