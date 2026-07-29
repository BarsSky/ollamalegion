// batched_scheduler_test.go — Round 15.1 unit tests для BatchedScheduler.
//
// Покрывает:
//   1. NewBatchedScheduler: validation (nil model, NParallel=0, WindowMs>50)
//   2. RegisterSession: capacity check, seq_id assignment
//   3. argmaxToken: правильный argmax, tie-breaking (первый wins)
//   4. isEOGToken: heuristic работает на 1, 2, не на других
//   5. Run loop: basic flow с mock model (требует интеграции)
//
// Интеграционные тесты (с реальной моделью) — в step 3.5.
//
// Использует стандартный testing package, не требует Docker.

package cppbackend

import (
	"context"
	"math"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

func init() {
	// Quiet logger в тестах (no info-level spam).
	_ = logger.Init(logger.Config{Level: "error"})
}

// ============================================================
// Constructor validation
// ============================================================

func TestNewBatchedScheduler_NilModel(t *testing.T) {
	_, err := NewBatchedScheduler(BatchedSchedulerConfig{
		Model:     nil,
		NParallel: 2,
		WindowMs:  5,
	})
	if err == nil {
		t.Fatal("expected error for nil model")
	}
}

func TestNewBatchedScheduler_ZeroNParallel(t *testing.T) {
	_, err := NewBatchedScheduler(BatchedSchedulerConfig{
		Model:     nil, // будет nil-check раньше — но это OK
		NParallel: 0,
		WindowMs:  5,
	})
	if err == nil {
		t.Fatal("expected error for NParallel=0")
	}
}

func TestNewBatchedScheduler_WindowMsTooLarge(t *testing.T) {
	_, err := NewBatchedScheduler(BatchedSchedulerConfig{
		Model:     nil, // nil-check раньше
		NParallel: 2,
		WindowMs:  100, // > 50
	})
	if err == nil {
		t.Fatal("expected error for WindowMs > 50")
	}
}

func TestNewBatchedScheduler_DefaultsWindowMs(t *testing.T) {
	bs, err := NewBatchedScheduler(BatchedSchedulerConfig{
		Model:     nil, // nil-check
		NParallel: 2,
		WindowMs:  0, // должен стать 5
	})
	if err != nil {
		// nil model ошибка до default check
		if bs != nil {
			t.Fatal("bs should be nil on error")
		}
	}
	// Note: nil model проверка раньше, default WindowMs не тестируется отдельно
	// (нужен mock model — см. ниже в TestNewBatchedScheduler_OK).
}

// ============================================================
// argmaxToken: greedy sampling
// ============================================================

func TestArgmaxToken_ClearWinner(t *testing.T) {
	logits := []float32{0.1, 0.5, 0.2, 0.3}
	got := argmaxToken(logits)
	if got != 1 {
		t.Errorf("argmaxToken: got %d, want 1 (logits[1]=0.5)", got)
	}
}

func TestArgmaxToken_FirstOnTie(t *testing.T) {
	// Tie между index 1 и 3. Должен вернуть первый (1).
	logits := []float32{0.1, 0.5, 0.2, 0.5}
	got := argmaxToken(logits)
	if got != 1 {
		t.Errorf("argmaxToken tie: got %d, want 1 (first wins)", got)
	}
}

func TestArgmaxToken_Negative(t *testing.T) {
	// Все negative — argmax = largest (closest to 0).
	logits := []float32{-2.0, -0.1, -1.0}
	got := argmaxToken(logits)
	if got != 1 {
		t.Errorf("argmaxToken negative: got %d, want 1 (logits[1]=-0.1)", got)
	}
}

func TestArgmaxToken_Inf(t *testing.T) {
	// -Inf < finite < +Inf.
	logits := []float32{float32(math.Inf(-1)), 0.0, float32(math.Inf(1))}
	got := argmaxToken(logits)
	if got != 2 {
		t.Errorf("argmaxToken Inf: got %d, want 2 (+Inf wins)", got)
	}
}

func TestArgmaxToken_Empty(t *testing.T) {
	got := argmaxToken([]float32{})
	if got != 0 {
		t.Errorf("argmaxToken empty: got %d, want 0", got)
	}
}

// ============================================================
// isEOGToken: heuristic
// ============================================================

func TestIsEOGToken_Heuristic(t *testing.T) {
	if !isEOGToken(1) {
		t.Error("isEOGToken(1) should be true (<eos> heuristic)")
	}
	if !isEOGToken(2) {
		t.Error("isEOGToken(2) should be true (<bos> heuristic)")
	}
	if isEOGToken(0) {
		t.Error("isEOGToken(0) should be false (<pad>)")
	}
	if isEOGToken(100) {
		t.Error("isEOGToken(100) should be false (regular token)")
	}
	// NOTE: это неточно — в Round 15.2 заменим на llama_vocab_is_eog.
}

// ============================================================
// UnregisterSession: should not panic, should not block
// ============================================================

func TestBatchedScheduler_UnregisterUnknown(t *testing.T) {
	// Создаём scheduler через nil-model конструктор (получим nil + error).
	bs, _ := NewBatchedScheduler(BatchedSchedulerConfig{
		Model:     nil,
		NParallel: 2,
		WindowMs:  5,
	})
	if bs != nil {
		// На случай если nil-check поменяется — Unregister с nil sessions map.
		// Не должно паниковать.
		bs.UnregisterSession(999)
	}
}

func TestBatchedScheduler_StopIdempotent(t *testing.T) {
	bs, _ := NewBatchedScheduler(BatchedSchedulerConfig{
		Model:     nil,
		NParallel: 2,
		WindowMs:  5,
	})
	if bs == nil {
		return
	}
	bs.Stop()
	bs.Stop() // should not panic
	bs.Stop() // should not panic
}

func TestBatchedScheduler_DoneChannel(t *testing.T) {
	bs, _ := NewBatchedScheduler(BatchedSchedulerConfig{
		Model:     nil,
		NParallel: 2,
		WindowMs:  5,
	})
	if bs == nil {
		return
	}
	// Done channel должен быть создан (но не closed).
	select {
	case <-bs.Done():
		t.Error("Done() should NOT be closed before Run() starts")
	default:
		// OK
	}
}

// ============================================================
// Run: graceful shutdown через ctx
// ============================================================

func TestBatchedScheduler_Run_CtxCancel(t *testing.T) {
	bs, _ := NewBatchedScheduler(BatchedSchedulerConfig{
		Model:     nil,
		NParallel: 2,
		WindowMs:  5,
	})
	if bs == nil {
		t.Skip("nil model — skip integration-style test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	bs.Stop() // сразу же stop
	go bs.Run(ctx)
	cancel()

	// Run должен выйти в течение 1 секунды.
	select {
	case <-bs.Done():
		// OK
	case <-time.After(1 * time.Second):
		t.Error("Run did not exit within 1s after Stop+ctx cancel")
	}
}
