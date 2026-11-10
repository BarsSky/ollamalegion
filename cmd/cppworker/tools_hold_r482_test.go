// tools_hold_r482_test.go — R482 (2026-11-10): 27B-стенд, Cline.
//
// Живые жалобы:
//   1. «не использует правильно инструменты» — модель кладёт вызов внутрь
//      блока размышлений; при незакрытом блоке видимая часть пуста, и прежний
//      разбор (только по видимой части) вызов не находил;
//   2. «не отдаёт сгенерированное, пока не сделает всё одним огромным куском» —
//      удержание вывода (R190/R471) не снималось, если шаблон не закрывает
//      канал размышлений; сторож R482 снимает его по времени/объёму, после чего
//      ход снова потоковый;
//   3. финальный «сырой» текст отдавался одной дельтой — теперь режется на части.
package main

import (
	"strings"
	"testing"
	"time"
)

// TestR482_CallInsideUnclosedReasoning — Qwen3.8 кладёт вызов после
// размышлений, но закрывающего тега в выводе нет (живой формат:
// <tool_call>read_files(files: [...]) без закрывающего).
func TestR482_CallInsideUnclosedReasoning(t *testing.T) {
	output := "<think>I should inspect the TASK.md file first.\n" +
		"<tool_call>read_files(files: ['TASK.md'])"
	calls := func() int {
		_, _, c, _, _ := splitToolsOutputR482(output, nil, formatTestTools())
		return len(c)
	}()
	if calls != 1 {
		t.Fatalf("вызов внутри незакрытого reasoning не найден (calls=%d) — инструмент не применится", calls)
	}
}

// TestR482_CallInsideUnclosedReasoning_NoTools — без tools не выдумываем вызовы.
func TestR482_CallInsideUnclosedReasoning_NoTools(t *testing.T) {
	output := "<think>text <tool_call>read_files(files: ['TASK.md'])"
	_, _, calls, _, fromReasoning := splitToolsOutputR482(output, nil, nil)
	if len(calls) != 0 || fromReasoning {
		t.Fatalf("без объявленных tools вызовов быть не должно: calls=%d from_reasoning=%v", len(calls), fromReasoning)
	}
}

// TestR482_CallInClosedReasoningNotDoubleCounted — обычный вызов в видимой
// части остаётся видимым (fromReasoning=false), reasoning-скан не мешает.
func TestR482_CallInClosedReasoningNotDoubleCounted(t *testing.T) {
	output := "<think>short</think><tool_call>read_files(files: ['TASK.md'])"
	_, _, calls, _, fromReasoning := splitToolsOutputR482(output, nil, formatTestTools())
	if len(calls) != 1 {
		t.Fatalf("обычный вызов не найден: calls=%d", len(calls))
	}
	if fromReasoning {
		t.Fatal("вызов в видимой части помечен как найденный в reasoning")
	}
}

// TestR482_HoldReleaseByTime — сторож снимает удержание по времени.
func TestR482_HoldReleaseByTime(t *testing.T) {
	t.Setenv("CPPWORKER_TOOLS_HOLD_MAX_SEC", "20")
	t.Setenv("CPPWORKER_TOOLS_HOLD_MAX_BYTES", "0")
	now := time.Now()
	ok, why := toolsHoldShouldReleaseR482(now.Add(-21*time.Second), 100, now)
	if !ok || why != "hold_timeout" {
		t.Fatalf("ожидалось снятие по времени, получено ok=%v why=%q", ok, why)
	}
	if ok, _ := toolsHoldShouldReleaseR482(now.Add(-5*time.Second), 100, now); ok {
		t.Fatal("5 секунд удержания — снимать рано")
	}
}

// TestR482_HoldReleaseByBytes — сторож снимает удержание по объёму.
func TestR482_HoldReleaseByBytes(t *testing.T) {
	t.Setenv("CPPWORKER_TOOLS_HOLD_MAX_SEC", "0")
	t.Setenv("CPPWORKER_TOOLS_HOLD_MAX_BYTES", "1024")
	now := time.Now()
	ok, why := toolsHoldShouldReleaseR482(now, 2048, now)
	if !ok || why != "hold_bytes" {
		t.Fatalf("ожидалось снятие по объёму, получено ok=%v why=%q", ok, why)
	}
	if ok, _ := toolsHoldShouldReleaseR482(now, 100, now); ok {
		t.Fatal("100 байт — снимать рано")
	}
}

// TestR482_HoldWatchdogDisabled — оба предела 0: прежнее поведение (не снимаем).
func TestR482_HoldWatchdogDisabled(t *testing.T) {
	t.Setenv("CPPWORKER_TOOLS_HOLD_MAX_SEC", "0")
	t.Setenv("CPPWORKER_TOOLS_HOLD_MAX_BYTES", "0")
	now := time.Now()
	if ok, _ := toolsHoldShouldReleaseR482(now.Add(-time.Hour), 1<<20, now); ok {
		t.Fatal("сторож выключен, а удержание снято")
	}
}

// TestR482_TerminalContentSplit — финальный «сырой» текст режется на части по
// границам рун (клиент рисует прогрессивно, а не одним куском).
func TestR482_TerminalContentSplit(t *testing.T) {
	text := strings.Repeat("я", 2500) // 2 байта на руну
	parts := splitTerminalContentR482(text, 1024)
	if len(parts) != 3 {
		t.Fatalf("частей %d, ожидалось 3 (2500/1024)", len(parts))
	}
	var joined strings.Builder
	for _, p := range parts {
		joined.WriteString(p)
		if len([]rune(p)) > 1024 {
			t.Fatalf("часть длиннее 1024 рун: %d", len([]rune(p)))
		}
	}
	if joined.String() != text {
		t.Fatal("склейка частей не совпадает с исходным текстом")
	}
	if parts := splitTerminalContentR482("коротко", 1024); len(parts) != 1 {
		t.Fatalf("короткий текст разрезан на %d частей", len(parts))
	}
}
