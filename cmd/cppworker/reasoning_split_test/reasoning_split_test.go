// reasoning_split_standalone_test.go — STANDALONE package test (separate package)
// to validate Round 32 split logic for gemma-4 channel format without
// pulling in the cgo-dependent main package.
//
// This file declares `package reasoning_split_test` (not `main`) so it
// compiles without the C-bridge. The duplicated logic is a 1:1 copy of
// cmd/cppworker/reasoning_content.go for the new gemma-4 patterns.
package reasoning_split_test

import (
	"strings"
	"testing"
)

var round32TagPairs = []struct {
	open  string
	close string
}{
	{"<|channel>thought\n", "\n<channel|>"},
	{"<|channel>thought", "<channel|>"},
	{"<|channel>analysis\n", "\n<channel|>"},
	{"<|channel>analysis", "<channel|>"},
	{"<|think>", "<think|>"},
	{"<think>", "</think>"},
	{"<thinking>", "</thinking>"},
	{"<reasoning>", "</reasoning>"},
	{"<analysis>", "</analysis>"},
}

func openThinkTag(s string, from int) (int, int, int) {
	if from < 0 {
		from = 0
	}
	bestPos := -1
	bestKind := -1
	for i, pair := range round32TagPairs {
		idx := strings.Index(s[from:], pair.open)
		if idx < 0 {
			continue
		}
		absPos := from + idx
		if bestPos < 0 || absPos < bestPos {
			bestPos = absPos
			bestKind = i
		}
	}
	if bestPos < 0 {
		return -1, -1, -1
	}
	return bestPos, bestPos + len(round32TagPairs[bestKind].open), bestKind
}

func closeThinkTag(s string, from int, kindIdx int) int {
	if kindIdx < 0 || kindIdx >= len(round32TagPairs) {
		return -1
	}
	pair := round32TagPairs[kindIdx]
	if i := strings.Index(s[from:], pair.close); i >= 0 {
		return from + i + len(pair.close)
	}
	return -1
}

func splitReasoning(s string) (reasoning, content string, hasReasoning bool) {
	if s == "" {
		return "", "", false
	}
	var sbR, sbC strings.Builder
	pos := 0
	found := false
	for pos < len(s) {
		tagStart, tagEnd, kindIdx := openThinkTag(s, pos)
		if tagStart < 0 {
			sbC.WriteString(s[pos:])
			break
		}
		sbC.WriteString(s[pos:tagStart])
		closePos := closeThinkTag(s, tagEnd, kindIdx)
		if closePos < 0 {
			sbR.WriteString(s[tagEnd:])
			found = true
			break
		}
		closeTagLen := len(round32TagPairs[kindIdx].close)
		bodyStart := tagEnd
		bodyEnd := closePos - closeTagLen
		sbR.WriteString(s[bodyStart:bodyEnd])
		found = true
		pos = closePos
	}
	return sbR.String(), sbC.String(), found
}

func TestRound32Split_Gemma4Channel(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantR   string
		wantC   string
		wantHas bool
	}{
		{
			"gemma4_canonical_with_newlines",
			"<|channel>thought\nStep 1\nStep 2\n<channel|>The answer is 56.",
			"Step 1\nStep 2",
			"The answer is 56.",
			true,
		},
		{
			"gemma4_no_newlines_fallback",
			"<|channel>thoughtQuick<channel|>Final",
			"Quick",
			"Final",
			true,
		},
		{
			"gemma4_analysis_channel",
			"<|channel>analysis\nThink\n<channel|>Answer",
			"Think",
			"Answer",
			true,
		},
		{
			"qwen_style_bars",
			"<|think>Reasoning<think|>Answer",
			"Reasoning",
			"Answer",
			true,
		},
		{
			"unclosed_gemma4",
			// open tag "<|channel>thought\n" consumes the \n after "thought"
			// so tagEnd points at "Long thinking..." (no leading \n in reasoning).
			"<|channel>thought\nLong thinking without close",
			"Long thinking without close",
			"",
			true,
		},
		{
			"no_reasoning_plain",
			"Just plain text",
			"",
			"Just plain text",
			false,
		},
		{
			"user_example_full",
			"<|channel>thought\nHere's a thinking process that leads to the suggested HTML code:\n\nStep 1\nStep 2\n<channel|>This is a great task!",
			"Here's a thinking process that leads to the suggested HTML code:\n\nStep 1\nStep 2",
			"This is a great task!",
			true,
		},
		{
			"mixed_qwen_and_channel",
			// close tag "\n<channel|>" consumes the \n before <channel|>
			// so pos advances to "Final" (no leading \n in content).
			"<think>qwen thinking</think><|channel>thought\nchannel thinking\n<channel|>Final",
			"qwen thinkingchannel thinking",
			"Final",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotR, gotC, gotHas := splitReasoning(tt.input)
			if gotR != tt.wantR {
				t.Errorf("reasoning: got %q, want %q", gotR, tt.wantR)
			}
			if gotC != tt.wantC {
				t.Errorf("content: got %q, want %q", gotC, tt.wantC)
			}
			if gotHas != tt.wantHas {
				t.Errorf("has: got %v, want %v", gotHas, tt.wantHas)
			}
		})
	}
}
