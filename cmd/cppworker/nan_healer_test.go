package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Round 25: NaNHealer.RecordBreak classification tests.
//
// The bug: ctx_done_on_write with a plain "context canceled" error was
// counted as a model fault, which made Cline's 120s default timeout and
// the balancer's 120s default RequestTimeout cause false-positive auto-
// reloads of healthy models. After 2 such "breaks" in 10 min, NaNHealer
// would unload + reload the model. Real cause: client-side abort, not
// model fault.
//
// Fix: ctx_done_on_write + context.Canceled.Error() (or empty) → return
// false (don't count). Keep counting context.DeadlineExceeded / i/o
// timeout / unknown errors (those can still be real model/network
// incidents).

func newTestNaNHealer() *NaNHealer {
	return NewNaNHealer(NaNHealingConfig{
		MaxBrokenPerPeriod:    2,
		PeriodMinutes:         10,
		GPULayersReductionPct: 20.0,
		EnableAutoHeal:        true,
	}, func(string, float64) error { return nil })
}

func TestNaNHealer_RecordBreak_CtxCanceled_NotCounted(t *testing.T) {
	// The exact bug from the cppworker-gpu reload incident:
	// "context canceled" on ctx_done_on_write = client abort, NOT model fault.
	h := newTestNaNHealer()

	got := h.RecordBreak("test-model", "ctx_done_on_write", context.Canceled.Error())
	if got {
		t.Fatalf("ctx_done_on_write + context.Canceled must NOT trigger auto-heal " +
			"(client-side abort), got true")
	}

	// Even after several events, threshold must not be reached
	for i := 0; i < 5; i++ {
		h.RecordBreak("test-model", "ctx_done_on_write", context.Canceled.Error())
	}
	if got := h.RecordBreak("test-model", "ctx_done_on_write", context.Canceled.Error()); got {
		t.Fatalf("after 6 client-cancel events, auto-heal must not fire " +
			"(all are client aborts, not model faults), got true")
	}
}

func TestNaNHealer_RecordBreak_CtxCanceled_Empty_NotCounted(t *testing.T) {
	// Some callers pass empty lastWriteErr — should also be treated as
	// client abort and not counted.
	h := newTestNaNHealer()

	for i := 0; i < 5; i++ {
		if got := h.RecordBreak("test-model", "ctx_done_on_write", ""); got {
			t.Fatalf("ctx_done_on_write + empty err must NOT trigger auto-heal, got true at iter %d", i)
		}
	}
}

func TestNaNHealer_RecordBreak_BrokenPipe_NotCounted(t *testing.T) {
	// Already handled before, regression check after the patch.
	h := newTestNaNHealer()

	cases := []string{
		"write: broken pipe",
		"read: connection reset by peer",
		"write error: connection reset",
	}
	for _, msg := range cases {
		for i := 0; i < 3; i++ {
			if got := h.RecordBreak("test-model", "ctx_done_on_write", msg); got {
				t.Fatalf("ctx_done_on_write + %q must NOT trigger auto-heal, got true", msg)
			}
		}
	}
}

func TestNaNHealer_RecordBreak_DeadlineExceeded_Counted(t *testing.T) {
	// context.DeadlineExceeded is a server-side timeout (cppworker's own
	// deadline hit, or the upstream llama.cpp call timed out). It can
	// indicate a model/network issue, so we keep counting it.
	h := newTestNaNHealer()

	// First event: under threshold
	if got := h.RecordBreak("test-model", "ctx_done_on_write", context.DeadlineExceeded.Error()); got {
		t.Fatalf("first ctx_done_on_write + DeadlineExceeded must not trigger (threshold=2), got true")
	}
	// Second event: hits threshold
	if got := h.RecordBreak("test-model", "ctx_done_on_write", context.DeadlineExceeded.Error()); !got {
		t.Fatalf("second ctx_done_on_write + DeadlineExceeded in window MUST trigger auto-heal, got false")
	}
}

func TestNaNHealer_RecordBreak_IOTimeout_Counted(t *testing.T) {
	// "i/o timeout" is a real socket write timeout — possible model/network
	// issue. Keep counting.
	h := newTestNaNHealer()

	if got := h.RecordBreak("test-model", "ctx_done_on_write", "i/o timeout"); got {
		t.Fatalf("first i/o timeout must not trigger (threshold=2), got true")
	}
	if got := h.RecordBreak("test-model", "ctx_done_on_write", "i/o timeout"); !got {
		t.Fatalf("second i/o timeout in window MUST trigger auto-heal, got false")
	}
}

func TestNaNHealer_RecordBreak_UnknownError_Counted(t *testing.T) {
	// Unknown / non-cancel error — treat as potential model fault, count it.
	h := newTestNaNHealer()

	if got := h.RecordBreak("test-model", "ctx_done_on_write", "some weird FS error XYZ"); got {
		t.Fatalf("first unknown error must not trigger (threshold=2), got true")
	}
	if got := h.RecordBreak("test-model", "ctx_done_on_write", "some weird FS error XYZ"); !got {
		t.Fatalf("second unknown error in window MUST trigger auto-heal, got false")
	}
}

func TestNaNHealer_RecordBreak_WriteError_NotCountedOnBrokenPipe(t *testing.T) {
	// write_error with broken pipe / connection reset = client left, don't
	// count (existing behavior, regression check).
	h := newTestNaNHealer()

	for i := 0; i < 3; i++ {
		if got := h.RecordBreak("test-model", "write_error", "write tcp: broken pipe"); got {
			t.Fatalf("write_error + broken pipe must not trigger, got true at iter %d", i)
		}
	}
}

func TestNaNHealer_RecordBreak_DisabledConfig(t *testing.T) {
	// EnableAutoHeal = false → no counting, no trigger, ever.
	h := NewNaNHealer(NaNHealingConfig{
		MaxBrokenPerPeriod:    2,
		PeriodMinutes:         10,
		GPULayersReductionPct: 20.0,
		EnableAutoHeal:        false,
	}, func(string, float64) error { return nil })

	for i := 0; i < 5; i++ {
		if got := h.RecordBreak("test-model", "ctx_done_on_write", "i/o timeout"); got {
			t.Fatalf("auto-heal disabled, must never return true, got true at iter %d", i)
		}
	}
}

func TestNaNHealer_RecordBreak_UnknownReason_NotCounted(t *testing.T) {
	// reason we don't recognize → default branch returns false, don't count.
	h := newTestNaNHealer()

	for i := 0; i < 3; i++ {
		if got := h.RecordBreak("test-model", "something_new", "context canceled"); got {
			t.Fatalf("unknown reason must not trigger, got true at iter %d", i)
		}
	}
}

func TestNaNHealer_RecordBreak_WindowExpiry(t *testing.T) {
	// After PeriodMinutes elapses, old breaks should be forgotten and a
	// new break should not immediately trigger.
	h := newTestNaNHealer()

	// Manually inject two old breaks (older than PeriodMinutes)
	old := time.Now().Add(-time.Duration(h.config.PeriodMinutes+1) * time.Minute)
	h.breaks["test-model"] = []time.Time{old, old}

	// New break — should not trigger because old ones are out of window
	if got := h.RecordBreak("test-model", "ctx_done_on_write", "i/o timeout"); got {
		t.Fatalf("after window expiry, single break must not trigger, got true")
	}
}

func TestNaNHealer_Reset(t *testing.T) {
	// After Reset, the count for a model should be cleared.
	h := newTestNaNHealer()

	// Two breaks to hit threshold
	h.RecordBreak("test-model", "ctx_done_on_write", "i/o timeout")
	h.RecordBreak("test-model", "ctx_done_on_write", "i/o timeout")

	// Reset
	h.Reset("test-model")

	// Now one break should be under threshold
	if got := h.RecordBreak("test-model", "ctx_done_on_write", "i/o timeout"); got {
		t.Fatalf("after Reset, one break must not trigger, got true")
	}
}

func TestNaNHealer_RecordBreak_DifferentModelsIsolated(t *testing.T) {
	// Breaks on model A should not count for model B.
	h := newTestNaNHealer()

	// Two breaks on model A
	h.RecordBreak("model-a", "ctx_done_on_write", "i/o timeout")
	h.RecordBreak("model-a", "ctx_done_on_write", "i/o timeout")

	// model B should still be at zero
	if got := h.RecordBreak("model-b", "ctx_done_on_write", "i/o timeout"); got {
		t.Fatalf("breaks on model-a must not affect model-b, got true")
	}
}

// TestNaNHealer_RecordBreak_RealCppWorkerScenario is the actual incident
// reproduction: Cline sends a long-running stream, the client times out
// after 120s, the cancel propagates as ctx_done_on_write + "context canceled".
// Two such events in 10 min must NOT trigger auto-reload.
func TestNaNHealer_RecordBreak_RealCppWorkerScenario(t *testing.T) {
	h := newTestNaNHealer()

	// Simulate Cline timeout pattern: 2 long streams in 10 min, both
	// aborted by client after ~120s
	for i := 0; i < 2; i++ {
		// Stream starts, generates for a while, then client aborts
		// (e.g. Cline 120s default, or user clicks "Stop")
		got := h.RecordBreak("Qwen3-Instruct-2507-q4km", "ctx_done_on_write", "context canceled")
		if got {
			t.Fatalf("client abort #%d must not trigger auto-reload, got true (would cause false-positive model reload)", i+1)
		}
	}
}

// Sanity check: ensure context.Canceled.Error() string is what we expect.
// If Go changes the error string in some future version, the fix would
// silently break. This test fails loudly if so.
func TestContextCanceled_ErrorStringStable(t *testing.T) {
	got := context.Canceled.Error()
	if got != "context canceled" {
		t.Fatalf("context.Canceled.Error() = %q, expected %q — NaNHealer fix may be broken", got, "context canceled")
	}
	if !strings.Contains(got, "context") {
		t.Fatalf("context.Canceled.Error() = %q, expected to contain 'context'", got)
	}
}
