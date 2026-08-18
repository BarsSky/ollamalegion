// feasible_sync_test.go — Round 37 (2026-08-18) tests.
//
// Эти тесты — защита от regression: если кто-то удалит/сломает background
// warning для conservative profile, production bug 2026-08-18 вернётся.
//go:build llama_stub

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// testLogger — captures Warnw/Infow calls for assertions.
type testLogger struct {
	mu      sync.Mutex
	warnings []loggedMessage
	infos    []loggedMessage
	debugs   []loggedMessage
}

type loggedMessage struct {
	Msg    string
	Fields map[string]interface{}
}

func (l *testLogger) Warnw(msg string, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, loggedMessage{Msg: msg, Fields: flatten(kv)})
}

func (l *testLogger) Infow(msg string, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, loggedMessage{Msg: msg, Fields: flatten(kv)})
}

func (l *testLogger) Debugw(msg string, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debugs = append(l.debugs, loggedMessage{Msg: msg, Fields: flatten(kv)})
}

func (l *testLogger) Warnings() []loggedMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]loggedMessage{}, l.warnings...)
}

func flatten(kv []interface{}) map[string]interface{} {
	m := make(map[string]interface{})
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}

// TestFeasibleSync_DetectsConservativeProfile — главный тест: profile=4096,
// feasible=131072, должен быть warning с ratio ~32.
func TestFeasibleSync_DetectsConservativeProfile(t *testing.T) {
	fs := newFeasibleSync(0, 0, "")
	fs.logger = &testLogger{}

	// Mock profile = 4096 (very conservative)
	fs.getProfileCtx = func(modelName string) int { return 4096 }
	// Mock feasible = 131072 (hardware allows much more)
	fs.computeFeasCtx = func(modelName string) (*cppbackend.FeasibleInfo, error) {
		return &cppbackend.FeasibleInfo{
			GGUFMax:     262144,
			MaxVRAMCtx:  0,
			MaxRAMCtx:   131072,
			KVCacheType: "q4_0",
		}, nil
	}

	// Trigger single check (skip background loop for test)
	fs.checkModel(context.Background(), "test-model")

	warns := fs.logger.(*testLogger).Warnings()
	if len(warns) != 1 {
		t.Fatalf("expected 1 warning, got %d: %+v", len(warns), warns)
	}
	w := warns[0]
	if w.Msg != "conservative profile detected (Round 37: auto-adapt recommends larger n_ctx)" {
		t.Errorf("unexpected message: %q", w.Msg)
	}
	if w.Fields["profile_n_ctx"] != 4096 {
		t.Errorf("profile_n_ctx = %v, want 4096", w.Fields["profile_n_ctx"])
	}
	if w.Fields["feasible_n_ctx"] != 131072 {
		t.Errorf("feasible_n_ctx = %v, want 131072", w.Fields["feasible_n_ctx"])
	}
	if ratio, _ := w.Fields["headroom_ratio"].(float64); ratio < 30 || ratio > 33 {
		t.Errorf("headroom_ratio = %v, want ~32", w.Fields["headroom_ratio"])
	}
}

// TestFeasibleSync_HealthyProfile_NoWarning — profile is within 10% of feasible.
func TestFeasibleSync_HealthyProfile_NoWarning(t *testing.T) {
	fs := newFeasibleSync(0, 0, "")
	fs.logger = &testLogger{}

	fs.getProfileCtx = func(modelName string) int { return 30000 }
	fs.computeFeasCtx = func(modelName string) (*cppbackend.FeasibleInfo, error) {
		return &cppbackend.FeasibleInfo{
			GGUFMax:    32768,
			MaxRAMCtx:  30000, // within 10% of profile
			KVCacheType: "f16",
		}, nil
	}

	fs.checkModel(context.Background(), "test-model")

	if warns := fs.logger.(*testLogger).Warnings(); len(warns) != 0 {
		t.Errorf("expected 0 warnings for healthy profile, got %d: %+v", len(warns), warns)
	}
}

// TestFeasibleSync_ThrottlesDuplicateWarnings — не спамит warning чаще чем warnThrottle.
func TestFeasibleSync_ThrottlesDuplicateWarnings(t *testing.T) {
	fs := newFeasibleSync(0, time.Hour, "") // warnThrottle = 1h
	fs.logger = &testLogger{}

	fs.getProfileCtx = func(modelName string) int { return 4096 }
	fs.computeFeasCtx = func(modelName string) (*cppbackend.FeasibleInfo, error) {
		return &cppbackend.FeasibleInfo{MaxRAMCtx: 131072, GGUFMax: 262144, KVCacheType: "q4_0"}, nil
	}

	fs.checkModel(context.Background(), "test-model")
	fs.checkModel(context.Background(), "test-model")
	fs.checkModel(context.Background(), "test-model")

	if warns := fs.logger.(*testLogger).Warnings(); len(warns) != 1 {
		t.Errorf("expected 1 warning (throttled), got %d", len(warns))
	}
}

// TestFeasibleSync_NoProfile_NoWarning — если profile не synced, нечего сравнивать.
func TestFeasibleSync_NoProfile_NoWarning(t *testing.T) {
	fs := newFeasibleSync(0, 0, "")
	fs.logger = &testLogger{}

	fs.getProfileCtx = func(modelName string) int { return 0 } // no profile
	fs.computeFeasCtx = func(modelName string) (*cppbackend.FeasibleInfo, error) {
		return &cppbackend.FeasibleInfo{MaxRAMCtx: 131072, GGUFMax: 262144}, nil
	}

	fs.checkModel(context.Background(), "test-model")

	if warns := fs.logger.(*testLogger).Warnings(); len(warns) != 0 {
		t.Errorf("expected 0 warnings (no profile), got %d", len(warns))
	}
}

// TestFeasibleSync_ProfileImprovement_ClearsWarning — если operator поднял
// profile до healthy, throttle-state очищается (следующее ухудшение снова warning).
func TestFeasibleSync_ProfileImprovement_ClearsWarning(t *testing.T) {
	fs := newFeasibleSync(0, time.Hour, "")
	fs.logger = &testLogger{}

	// First: conservative → warn
	fs.getProfileCtx = func(modelName string) int { return 4096 }
	fs.computeFeasCtx = func(modelName string) (*cppbackend.FeasibleInfo, error) {
		return &cppbackend.FeasibleInfo{MaxRAMCtx: 131072, GGUFMax: 262144}, nil
	}
	fs.checkModel(context.Background(), "m1")
	if len(fs.logger.(*testLogger).Warnings()) != 1 {
		t.Fatal("setup: expected 1 warning")
	}

	// Then: profile improved → no warning, throttle cleared
	fs.getProfileCtx = func(modelName string) int { return 131072 }
	fs.checkModel(context.Background(), "m1")
	if len(fs.logger.(*testLogger).Warnings()) != 1 {
		t.Error("improved profile should not re-warn (still 1 total)")
	}

	// Then: profile degraded again → SHOULD warn (throttle was cleared)
	fs.getProfileCtx = func(modelName string) int { return 4096 }
	fs.checkModel(context.Background(), "m1")
	if len(fs.logger.(*testLogger).Warnings()) != 2 {
		t.Errorf("expected 2 warnings total (initial + after-regression), got %d",
			len(fs.logger.(*testLogger).Warnings()))
	}
}

// TestItoa — pure-function test.
func TestItoa(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{4096, "4096"},
		{131072, "131072"},
		{-1, "-1"},
		{-100, "-100"},
	}
	for _, tt := range tests {
		if got := itoa(tt.in); got != tt.want {
			t.Errorf("itoa(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
