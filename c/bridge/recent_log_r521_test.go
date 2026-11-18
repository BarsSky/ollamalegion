// recent_log_r521_test.go — R521 (2026-11-19): кольцевой буфер последних строк
// лога llama.cpp (c/bridge/layers_plan.go: recordLogLine/RecentLogLines).
//
// Файл без build-тега: буфер — чистый Go и обязан проверяться обычным
// `go test -tags llama_stub`, без llama.cpp в окружении.
package bridge

import (
	"fmt"
	"testing"
)

func TestR521_RecentLogLinesKeepsTailAndOrder(t *testing.T) {
	ResetRecentLogLines()
	t.Cleanup(ResetRecentLogLines)

	for i := 0; i < recentLogLimit+37; i++ {
		recordLogLine(fmt.Sprintf("line-%03d", i))
	}
	got := RecentLogLines(5)
	if len(got) != 5 {
		t.Fatalf("строк: %d, want 5", len(got))
	}
	// Последние пять — это пять последних добавленных, в исходном порядке.
	wantFirst := fmt.Sprintf("line-%03d", recentLogLimit+37-5)
	if got[0] != wantFirst {
		t.Fatalf("первая строка хвоста = %q, want %q", got[0], wantFirst)
	}
	wantLast := fmt.Sprintf("line-%03d", recentLogLimit+37-1)
	if got[4] != wantLast {
		t.Fatalf("последняя строка = %q, want %q", got[4], wantLast)
	}
	// Буфер не растёт бесконечно.
	if all := RecentLogLines(0); all != nil {
		t.Fatalf("RecentLogLines(0) обязан вернуть nil, вернул %d строк", len(all))
	}
	if all := RecentLogLines(recentLogLimit * 2); len(all) > recentLogLimit {
		t.Fatalf("буфер вырос сверх предела: %d строк при пределе %d", len(all), recentLogLimit)
	}
}

func TestR521_ResetClearsTail(t *testing.T) {
	ResetRecentLogLines()
	recordLogLine("old load line")
	if len(RecentLogLines(1)) != 1 {
		t.Fatalf("строка не записалась")
	}
	ResetRecentLogLines()
	if got := RecentLogLines(10); got != nil {
		t.Fatalf("после сброса хвост не пуст: %v", got)
	}
}

func TestR521_EmptyLinesIgnored(t *testing.T) {
	ResetRecentLogLines()
	t.Cleanup(ResetRecentLogLines)
	recordLogLine("")
	recordLogLine("   ")
	// "" отбрасывается, пробельная строка остаётся: llama.cpp печатает отступы,
	// и решение «это пустая строка» принимает не буфер.
	if got := RecentLogLines(10); len(got) != 1 {
		t.Fatalf("строк: %d, want 1 (пустая отброшена, пробельная осталась)", len(got))
	}
}
