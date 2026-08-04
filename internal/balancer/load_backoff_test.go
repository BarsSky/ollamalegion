package balancer

import (
	"testing"
	"time"
)

// TestLoadBackoff_InitialState — fresh backoff не должен skip ничего.
func TestLoadBackoff_InitialState(t *testing.T) {
	lb := newLoadBackoff()
	if skip, reason, _ := lb.shouldSkip("b1", "m1"); skip {
		t.Errorf("initial state should not skip, got reason=%q", reason)
	}
}

// TestLoadBackoff_AfterFailure_UnderThreshold — 1-2 failures не открывают breaker.
func TestLoadBackoff_AfterFailure_UnderThreshold(t *testing.T) {
	lb := newLoadBackoff()
	for i := 0; i < 2; i++ {
		opened, _ := lb.recordFailure("b1", "m1", "EOF")
		if opened {
			t.Errorf("breaker should not open after %d failures", i+1)
		}
	}
	if skip, _, _ := lb.shouldSkip("b1", "m1"); skip {
		t.Errorf("should not skip under threshold (maxConsecutiveFailures=3)")
	}
}

// TestLoadBackoff_BreakerOpens — после 3 failures breaker открывается.
func TestLoadBackoff_BreakerOpens(t *testing.T) {
	lb := newLoadBackoff()
	for i := 0; i < 3; i++ {
		opened, retry := lb.recordFailure("b1", "m1", "EOF")
		_ = retry
		if i < 2 && opened {
			t.Errorf("breaker opened too early at failure #%d", i+1)
		}
		if i == 2 && !opened {
			t.Error("breaker should open after 3 failures")
		}
	}
	if skip, reason, _ := lb.shouldSkip("b1", "m1"); !skip {
		t.Errorf("should skip when breaker is open, got reason=%q", reason)
	}
}

// TestLoadBackoff_ResetOnSuccess — успешный load сбрасывает breaker.
func TestLoadBackoff_ResetOnSuccess(t *testing.T) {
	lb := newLoadBackoff()
	// Открыть breaker
	for i := 0; i < 3; i++ {
		_, _ = lb.recordFailure("b1", "m1", "EOF")
	}
	if skip, _, _ := lb.shouldSkip("b1", "m1"); !skip {
		t.Fatal("breaker should be open")
	}
	// Сбросить через success
	lb.recordSuccess("b1", "m1")
	if skip, _, _ := lb.shouldSkip("b1", "m1"); skip {
		t.Error("breaker should be reset after success")
	}
	// Snapshot должен быть пуст
	if snaps := lb.snapshot(); len(snaps) != 0 {
		t.Errorf("expected empty snapshot, got %d entries", len(snaps))
	}
}

// TestLoadBackoff_PerBackendModel — разные (backend, model) не влияют друг на друга.
func TestLoadBackoff_PerBackendModel(t *testing.T) {
	lb := newLoadBackoff()
	// Открыть breaker для b1/m1
	for i := 0; i < 3; i++ {
		_, _ = lb.recordFailure("b1", "m1", "EOF")
	}
	// b1/m2 не должен быть задет
	if skip, _, _ := lb.shouldSkip("b1", "m2"); skip {
		t.Error("b1/m2 should not be affected by b1/m1 failures")
	}
	// b2/m1 не должен быть задет
	if skip, _, _ := lb.shouldSkip("b2", "m1"); skip {
		t.Error("b2/m1 should not be affected by b1/m1 failures")
	}
}

// TestLoadBackoff_BreakerExpires — breaker автоматически expires после TTL.
func TestLoadBackoff_BreakerExpires(t *testing.T) {
	lb := newLoadBackoff()
	// Используем короткий TTL для теста
	lb.config.breakerOpenDuration = 100 * time.Millisecond

	for i := 0; i < 3; i++ {
		_, _ = lb.recordFailure("b1", "m1", "EOF")
	}
	if skip, _, _ := lb.shouldSkip("b1", "m1"); !skip {
		t.Fatal("breaker should be open")
	}
	// Ждём истечения
	time.Sleep(200 * time.Millisecond)
	// Следующий shouldSkip должен reset breaker
	if skip, _, _ := lb.shouldSkip("b1", "m1"); skip {
		t.Error("breaker should expire after TTL")
	}
	// И счётчик должен быть сброшен
	if skip, _, _ := lb.shouldSkip("b1", "m1"); skip {
		t.Error("counter should be reset after TTL expiry")
	}
}

// TestLoadBackoff_FailureWindow_Reset — старые failures не считаются.
func TestLoadBackoff_FailureWindow_Reset(t *testing.T) {
	lb := newLoadBackoff()
	lb.config.failureWindow = 100 * time.Millisecond

	// 2 failures
	_, _ = lb.recordFailure("b1", "m1", "EOF")
	_, _ = lb.recordFailure("b1", "m1", "EOF")
	// Ждём за пределы failureWindow
	time.Sleep(200 * time.Millisecond)
	// 3-й failure — breaker должен открыться, потому что старые 2 уже сброшены
	opened, _ := lb.recordFailure("b1", "m1", "EOF")
	if opened {
		t.Error("breaker opened despite failureWindow reset")
	}
}

// TestLoadBackoff_Snapshot — snapshot возвращает все backoff states.
func TestLoadBackoff_Snapshot(t *testing.T) {
	lb := newLoadBackoff()
	_, _ = lb.recordFailure("b1", "m1", "err1")
	_, _ = lb.recordFailure("b2", "m2", "err2")

	snaps := lb.snapshot()
	if len(snaps) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(snaps))
	}
	// Сортируем по ключу для стабильного теста
	if snaps[0].BackendID != "b1" || snaps[0].ModelName != "m1" {
		t.Errorf("unexpected first entry: %+v", snaps[0])
	}
	if snaps[1].BackendID != "b2" || snaps[1].ModelName != "m2" {
		t.Errorf("unexpected second entry: %+v", snaps[1])
	}
}

// TestLoadBackoff_SplitKey — key/parseKey roundtrip.
func TestLoadBackoff_SplitKey(t *testing.T) {
	tests := []struct {
		in       string
		wantBack string
		wantMod  string
	}{
		{"b1|m1", "b1", "m1"},
		{"b1|gemma-4-E4B-it-Q4_K_M", "b1", "gemma-4-E4B-it-Q4_K_M"},
		{"no-pipe", "no-pipe", ""},
		{"a|b|c", "a", "b|c"}, // splitKey берёт первый | (b содержит остаток)
	}
	for _, tt := range tests {
		parts := splitKey(tt.in)
		if len(parts) != 2 {
			t.Errorf("splitKey(%q): expected 2 parts, got %d", tt.in, len(parts))
			continue
		}
		if parts[0] != tt.wantBack {
			t.Errorf("splitKey(%q)[0]: want %q, got %q", tt.in, tt.wantBack, parts[0])
		}
		if parts[1] != tt.wantMod {
			t.Errorf("splitKey(%q)[1]: want %q, got %q", tt.in, tt.wantMod, parts[1])
		}
	}
}
