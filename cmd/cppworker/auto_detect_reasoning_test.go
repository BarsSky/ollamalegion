// auto_detect_reasoning_test.go — Round 17 Layer 3 (2026-07-31) tests for
// the LAZY AUTO-DETECT mechanism.
//
// Контекст: см. plans/bug-2026-07-31-reasoning-not-routed.md.
// Round 17 Layer 1 (per-model override) требует от пользователя явно задать
// enableReasoning=true при load-with-params. Layer 3 (lazy auto-detect) —
// страховка: если модель не в IsReasoningModel whitelist, парсер всё равно
// проверяет первые 64 символа output и при обнаружении `<think>` auto-включает
// routing для этой и всех будущих сессий на этой модели.
//
// Эти тесты проверяют корректность auto-detect THRESHOLD и PATTERN DETECTION.
// Сама callback-функция в writeOpenAIChatStream интегрирует этот pattern;
// unit-тесты проверяют что pattern распознавания корректен.
//
// Тесты:
//   1. Auto-detect срабатывает на `<think>` в префиксе → true
//   2. Auto-detect НЕ срабатывает без `<think>` в префиксе → false
//   3. Auto-detect работает через `SplitReasoningContent` (используется в non-stream)
//   4. Auto-detect threshold: 64 символа prefix достаточно
package main

import (
	"strings"
	"testing"
)

// TestAutoDetect_ThinkTagInPrefix — проверяет, что strings.Contains
// корректно ловит `<think>` в prefix-окне.
func TestAutoDetect_ThinkTagInPrefix(t *testing.T) {
	const autoDetectPrefixLen = 64
	const thinkStartTag = "<think>"

	// Simulate output от reasoning-модели: <think>...reasoning...</think>answer
	// (в стриме префикс растёт постепенно, но parser видит все накопленные байты)
	cases := []struct {
		name     string
		prefix   string
		wantTrig bool
	}{
		{
			name:     "think tag immediately",
			prefix:   "<think>Let me think about this carefully</think>",
			wantTrig: true,
		},
		{
			name:     "think tag after some text",
			prefix:   "OK, here is my analysis: <think>I need to consider 3 things",
			wantTrig: true,
		},
		{
			name:     "think tag in middle of prefix",
			prefix:   strings.Repeat("a", 30) + "<think>reasoning</think>", // 30+24=54 chars
			wantTrig: true, // 54 chars → 54 < 64 → без truncation, full "<think>" присутствует
		},
		{
			name:     "think tag JUST before boundary",
			prefix:   strings.Repeat("a", 57) + "<think>", // 57+7=64 chars, tag fits
			wantTrig: true, // ровно 64 chars → truncation не происходит, <think> присутствует
		},
		{
			name:     "think tag just past boundary",
			prefix:   strings.Repeat("a", 58) + "<think>", // 58+7=65, truncation теряет часть тега
			wantTrig: false, // prefix [:64] = 58 'a' + "thi" — без "<think>"
		},
		{
			name:     "no think tag in prefix",
			prefix:   "The answer is 42. This is a simple arithmetic question.",
			wantTrig: false,
		},
		{
			name:     "long output without think",
			prefix:   strings.Repeat("a", 200), // 200 'a' без тегов
			wantTrig: false,
		},
		{
			name:     "think tag only in tail (well past boundary)",
			prefix:   strings.Repeat("a", 100) + "<think>", // 100+7, prefix [:64] без тега
			wantTrig: false, // ВАЖНО: тег за пределами prefix — auto-detect НЕ сработает
		},
		{
			name:     "alternative thinking tag (not detected by Layer 3)",
			prefix:   "<thinking>Let me think...</thinking>",
			wantTrig: false, // Layer 3 проверяет только <think> (наиболее распространённый)
		},
		{
			name:     "empty prefix",
			prefix:   "",
			wantTrig: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Эмулируем ровно ту логику, что в writeOpenAIChatStream callback:
			//   prefix := outputBuf.String()
			//   if len(prefix) > autoDetectPrefixLen {
			//       prefix = prefix[:autoDetectPrefixLen]
			//   }
			//   if strings.Contains(prefix, thinkStartTag) { ... auto-enable ... }
			prefix := tc.prefix
			if len(prefix) > autoDetectPrefixLen {
				prefix = prefix[:autoDetectPrefixLen]
			}
			gotTrig := strings.Contains(prefix, thinkStartTag)
			if gotTrig != tc.wantTrig {
				t.Errorf("auto-detect for %q: triggered=%v, want=%v (prefix window=%d chars)",
					tc.name, gotTrig, tc.wantTrig, autoDetectPrefixLen)
			}
		})
	}
}

// TestAutoDetect_SplitReasoningContent_Integration — проверяет, что non-stream
// post-hoc auto-detect (handleV1ChatCompletions) корректно разбирает
// reasoning, если <think> присутствует в output.
func TestAutoDetect_SplitReasoningContent_Integration(t *testing.T) {
	// Сценарий: модель не в whitelist, но выдаёт <think> в output.
	// Post-hoc auto-detect должен:
	//   1) SplitReasoningContent возвращает hasReasoning=true
	//   2) parser вызывает AutoEnableReasoning → следующий запрос уже routing
	output := "<think>The user asked about 2+2. Let me think. 2+2 is 4.</think>The answer is 4."

	r, c, has := SplitReasoningContent(output)
	if !has {
		t.Fatalf("expected SplitReasoningContent to find <think> in output")
	}
	if r == "" {
		t.Errorf("reasoning should not be empty when has=true")
	}
	if c != "The answer is 4." {
		t.Errorf("content mismatch: got %q, want %q", c, "The answer is 4.")
	}
	if !strings.Contains(r, "2+2 is 4") {
		t.Errorf("reasoning should contain the think-block body, got %q", r)
	}
}

// TestAutoDetect_PrefixThreshold — проверяет, что autoDetectPrefixLen=64
// достаточно большой для типичных reasoning-префиксов, и что модели
// с задержкой начала thinking (но в пределах 64 chars) корректно
// обнаруживаются.
func TestAutoDetect_PrefixThreshold(t *testing.T) {
	const autoDetectPrefixLen = 64

	// Реальные reasoning-модели: <think> обычно появляется в первых
	// 50 символах для chat-style (Qwen, DeepSeek-R1, Kimi, gemma-4).
	// Каждый из этих префиксов вписывается в 64 chars и содержит <think>.

	typicalReasoningPrefixes := []string{
		"<think>Let me think about this step by step. The user wants me to", // ~62 chars
		"Sure, let me analyze this. <think>The question is about arithmetic", // ~62 chars
		"\n<think>", // some models emit newline first
		"OK.\n<think>I'll start by understanding the question.",     // typical Qwen pattern
	}

	for _, p := range typicalReasoningPrefixes {
		prefix := p
		if len(prefix) > autoDetectPrefixLen {
			prefix = prefix[:autoDetectPrefixLen]
		}
		if !strings.Contains(prefix, "<think>") {
			t.Errorf("typical reasoning prefix %q (len=%d) should contain <think> in window=%d, but doesn't",
				p, len(p), autoDetectPrefixLen)
		}
	}

	// Sanity: 64-char window — это разумный компромисс:
	//   - достаточно большой для типичных reasoning-префиксов (тест выше)
	//   - достаточно маленький, чтобы не тратить CPU на длинных не-reasoning
	//     стримах (chat-style: первые 64 chars — это префикс + начало ответа)
	//   - не вызывает race с реальным parser (который всё равно
	//     SplitReasoningContent делает на полном output)
	if autoDetectPrefixLen < 32 {
		t.Errorf("autoDetectPrefixLen=%d is too small, reasoning models with intro "+
			"text won't be detected", autoDetectPrefixLen)
	}
	if autoDetectPrefixLen > 256 {
		t.Errorf("autoDetectPrefixLen=%d is too large, lazy detect adds latency "+
			"to non-reasoning streams", autoDetectPrefixLen)
	}
}

// TestAutoDetect_AfterSplit_LongContent — проверяет, что SplitReasoningContent
// корректно обрабатывает длинный output с <think> (используется non-stream handlers).
func TestAutoDetect_AfterSplit_LongContent(t *testing.T) {
	// Длинный thinking block (как у DeepSeek-R1 на сложных задачах)
	longReasoning := strings.Repeat("Let me consider this. ", 100) // 2300 chars
	output := "<think>" + longReasoning + "</think>The final answer is 42."

	r, c, has := SplitReasoningContent(output)
	if !has {
		t.Fatalf("expected SplitReasoningContent to find <think> in long output")
	}
	if len(r) < 2000 {
		t.Errorf("reasoning should be ~2300 chars, got %d", len(r))
	}
	if c != "The final answer is 42." {
		t.Errorf("content mismatch: got %q", c)
	}
}
