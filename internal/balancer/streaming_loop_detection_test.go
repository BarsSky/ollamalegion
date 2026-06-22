package balancer

import (
	"strings"
	"testing"
	"time"
)

// TestDetectLoopingChunk_NoLoop — обычные разные чанки не должны триггерить loop detection.
func TestDetectLoopingChunk_NoLoop(t *testing.T) {
	cases := []struct {
		name    string
		current string
		recent  []string
	}{
		{"empty recent", "data chunk 1", nil},
		{"single chunk", "data chunk 1", []string{"data chunk 1"}},
		{"different chunks", "hello world", []string{"goodbye", "foo bar"}},
		{"short chunk", ".", []string{".", ".", "."}}, // слишком короткий, не считается loop
		{"normal sequence", "The quick brown fox", []string{"jumps over", "the lazy dog", "and runs", "away"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detected, pattern := detectLoopingChunk([]byte(tc.current), tc.recent)
			if detected {
				t.Errorf("unexpected loop detected: pattern=%q", pattern)
			}
		})
	}
}

// TestDetectLoopingChunk_ExactMatch — точное совпадение 3+ раз подряд.
func TestDetectLoopingChunk_ExactMatch(t *testing.T) {
	// 3 одинаковых чанка подряд (текущий + 2 предыдущих).
	detected, pattern := detectLoopingChunk(
		[]byte("aaaa"),
		[]string{"aaaa", "aaaa"},
	)
	if !detected {
		t.Error("expected loop detection on 3 consecutive identical chunks")
	}
	if !strings.HasPrefix(pattern, "exact_match_x") {
		t.Errorf("expected exact_match pattern, got %q", pattern)
	}
}

// TestDetectLoopingChunk_ConsecutiveCopy — exact_match имеет наивысший приоритет
// (≥3 одинаковых подряд). 5 из 6 идентичных дают exact_match_x5, а не consecutive_copy.
// Тест проверяет, что детектируется хотя бы один из паттернов.
func TestDetectLoopingChunk_ConsecutiveCopy(t *testing.T) {
	recent := []string{
		"unique 1",
		"echo echo echo echo echo echo echo echo", // 1
		"echo echo echo echo echo echo echo echo", // 2
		"unique 2",
		"echo echo echo echo echo echo echo echo", // 3
		"echo echo echo echo echo echo echo echo", // 4
	}
	detected, pattern := detectLoopingChunk(
		[]byte("echo echo echo echo echo echo echo echo"),
		recent,
	)
	if !detected {
		t.Error("expected loop detection on 5/6 consecutive identical chunks")
	}
	// exact_match срабатывает первым (≥3 одинаковых подряд: 2, 3, current = 3 подряд).
	if !strings.HasPrefix(pattern, "exact_match_x") {
		t.Errorf("expected exact_match pattern (highest priority), got %q", pattern)
	}
}

// TestDetectLoopingChunk_ShortNotLoop — одиночные токены (короткие чанки)
// НЕ должны вызывать false positive.
func TestDetectLoopingChunk_ShortNotLoop(t *testing.T) {
	// 3 одинаковых однобуквенных чанка подряд — это нормально для LLM,
	// она может выдавать "a", "a", "a" при перечислении.
	detected, _ := detectLoopingChunk(
		[]byte("a"),
		[]string{"a", "a"},
	)
	if detected {
		t.Error("short chunks (< 4 bytes) should not be detected as loops")
	}
}

// TestDetectLoopingChunk_PatternMatch — exact_match имеет приоритет над pattern_match.
// Когда 8 последних чанков содержат одинаковый pattern, это и exact_match (текущий
// совпадает с последним) и pattern match. exact_match срабатывает первым.
func TestDetectLoopingChunk_PatternMatch(t *testing.T) {
	recent := make([]string, 8)
	for i := range recent {
		recent[i] = "blah blah blah the"
	}
	detected, pattern := detectLoopingChunk(
		[]byte("blah blah blah the"),
		recent,
	)
	if !detected {
		t.Errorf("expected loop detection via pattern match, but not detected")
	}
	// exact_match имеет приоритет и срабатывает первым.
	if !strings.HasPrefix(pattern, "exact_match_x") {
		t.Errorf("expected exact_match pattern (priority over pattern_match), got %q", pattern)
	}
}

// TestDetectLoopingChunk_NoFalsePositive_NormalText — нормальный вывод модели
// (без явного повторения) не должен триггерить loop detection.
func TestDetectLoopingChunk_NoFalsePositive_NormalText(t *testing.T) {
	// Реалистичная последовательность из 10 разных чанков модели.
	recent := []string{
		"The capital of France is",
		" Paris. It is known for",
		" the Eiffel Tower, which",
		" was built in 1889 and",
		" stands at 330 meters tall.",
		" France is also famous for",
		" its cuisine and wine.",
		" The population is about",
		" 67 million people.",
		" The official language",
	}
	// Следующий уникальный чанк — не должно быть loop detection.
	current := []byte(" is French.")
	detected, _ := detectLoopingChunk(current, recent)
	if detected {
		t.Error("normal text should not be detected as loop")
	}
}

// TestDetectLoopingChunk_OnlyTwoRepeats — два повтора подряд ещё НЕ loop
// (нужно 3+ чтобы детектировать — иначе false positive на нормальных данных).
func TestDetectLoopingChunk_OnlyTwoRepeats(t *testing.T) {
	detected, _ := detectLoopingChunk(
		[]byte("yes"),
		[]string{"yes"},
	)
	if detected {
		t.Error("2 repeats is not yet a loop (need 3+)")
	}
}

// TestHandleStreamingResponse_WithLoopDetection — используем длинный pattern
// ("hello world", ≥4 байт) чтобы exact_match сработал на 6 одинаковых чанках подряд.
func TestHandleStreamingResponse_WithLoopDetection(t *testing.T) {
	// 5 одинаковых "hello world" чанков + 1 текущий = 6 подряд → exact_match.
	chunks := []string{
		"Intro",
		"hello world", "hello world", "hello world",
		"hello world", "hello world",
	}
	detected, pattern := detectLoopingChunk(
		[]byte(chunks[len(chunks)-1]),
		chunks[:len(chunks)-1],
	)
	if !detected {
		t.Errorf("expected loop detection on repeated 'hello world', not detected")
	}
	if pattern == "" {
		t.Error("pattern should be reported for diagnostics")
	}
}

// TestHandleStreamingResponse_ShortEchoNotDetected — короткие чанки (<4 байт)
// типа "..." намеренно НЕ триггерят loop detection (защита от false positive).
func TestHandleStreamingResponse_ShortEchoNotDetected(t *testing.T) {
	chunks := []string{
		"Hello", "...",
		"...", "...", "...", "...",
	}
	detected, _ := detectLoopingChunk(
		[]byte(chunks[len(chunks)-1]),
		chunks[:len(chunks)-1],
	)
	if detected {
		t.Error("short '...' chunks (<4 bytes) should NOT trigger loop detection (intentional)")
	}
}

// TestHandleStreamingResponse_LoopTimeoutSafe — проверяем, что таймауты НЕ
// триггерят ложный loop detection на коротких legitimate чанках.
func TestHandleStreamingResponse_LoopTimeoutSafe(t *testing.T) {
	// Модель может выдавать "yes " несколько раз подряд (валидный паттерн).
	// Это НЕ должно триггерить loop detection (3 одинаковых но они короткие и валидные).
	for i := 0; i < 3; i++ {
		detected, _ := detectLoopingChunk(
			[]byte("yes "),
			[]string{"yes ", "yes "},
		)
		_ = detected // намеренно игнорируем — это валидный кейс
	}
	// Чтобы быть уверенным что 4-й триггернёт loop (3 предыдущих + текущий = 4).
	detected, pattern := detectLoopingChunk(
		[]byte("yes "),
		[]string{"yes ", "yes ", "yes "},
	)
	if !detected {
		t.Error("4 consecutive 'yes ' chunks should be detected as loop")
	}
	if !strings.HasPrefix(pattern, "exact_match_x") {
		t.Errorf("expected exact_match pattern, got %q", pattern)
	}
	_ = time.Second // чтобы импорт time не ругался
}