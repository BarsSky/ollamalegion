// autotune_history_test.go — Round 55.2 (2026-08-24): tests for AutoTuneHistory.

package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func TestAutoTuneHistory_RecordAndRecent(t *testing.T) {
	h := NewAutoTuneHistory(100)

	// Record 3 events
	now := time.Now()
	for i, typ := range []types.EventType{
		types.EventAutoTuneReloadTriggered,
		types.EventAutoTuneReloadSucceeded,
		types.EventAutoTuneCircuitOpen,
	} {
		h.Record(types.Event{
			Type:      typ,
			Timestamp: now.Add(time.Duration(i) * time.Second),
			BackendID: "backend-1",
			Model:     "qwen3-4b",
			Severity:  types.SeverityInfo,
			Source:    "autotune",
			Message:   "test event " + string(typ),
		})
	}

	// Recent(0) = all
	all := h.Recent(0)
	if len(all) != 3 {
		t.Errorf("expected 3 entries, got %d", len(all))
	}
	// Recent() returns newest first
	if all[0].Type != string(types.EventAutoTuneCircuitOpen) {
		t.Errorf("expected newest first, got %s", all[0].Type)
	}
	if all[2].Type != string(types.EventAutoTuneReloadTriggered) {
		t.Errorf("expected oldest last, got %s", all[2].Type)
	}

	// Recent(2) = limit
	limited := h.Recent(2)
	if len(limited) != 2 {
		t.Errorf("expected 2 entries with limit, got %d", len(limited))
	}
}

func TestAutoTuneHistory_Overflow_DropsOldest(t *testing.T) {
	h := NewAutoTuneHistory(3) // tiny buffer for test

	for i := 0; i < 5; i++ {
		h.Record(types.Event{
			Type:      types.EventAutoTuneReloadTriggered,
			BackendID: "b1",
			Model:     "m1",
			Message:   "msg " + string(rune('0'+i)),
		})
	}

	all := h.Recent(0)
	if len(all) != 3 {
		t.Errorf("expected 3 entries (maxSize), got %d", len(all))
	}
	// Oldest dropped → "msg 0" and "msg 1" gone, "msg 4" newest
	if all[0].Message != "msg 4" {
		t.Errorf("expected newest msg 4 first, got %s", all[0].Message)
	}

	stats := h.Stats()
	if stats.DroppedCount != 2 {
		t.Errorf("expected DroppedCount=2, got %d", stats.DroppedCount)
	}
	if stats.Size != 3 {
		t.Errorf("expected Size=3, got %d", stats.Size)
	}
	if stats.MaxSize != 3 {
		t.Errorf("expected MaxSize=3, got %d", stats.MaxSize)
	}
}

func TestAutoTuneHistory_Since(t *testing.T) {
	h := NewAutoTuneHistory(100)
	base := time.Now()

	// 5 events spaced 1 second apart
	for i := 0; i < 5; i++ {
		h.Record(types.Event{
			Type:      types.EventAutoTuneReloadTriggered,
			Timestamp: base.Add(time.Duration(i) * time.Second),
			BackendID: "b1",
			Message:   "msg " + string(rune('0'+i)),
		})
	}

	// Since = base + 2s → should return msg 3, msg 4 (strict > after, so base+2s excluded)
	since := base.Add(2 * time.Second)
	sinceEntries := h.Since(since)
	if len(sinceEntries) != 2 {
		t.Errorf("expected 2 entries strictly after +2s, got %d", len(sinceEntries))
	}
	if sinceEntries[0].Message != "msg 4" {
		t.Errorf("expected newest msg 4 first, got %s", sinceEntries[0].Message)
	}

	// Since = base + 1.5s → strict > 1.5s, so msg 2, msg 3, msg 4 (3 entries)
	since15 := base.Add(1500 * time.Millisecond)
	sinceEntries15 := h.Since(since15)
	if len(sinceEntries15) != 3 {
		t.Errorf("expected 3 entries strictly after +1.5s, got %d", len(sinceEntries15))
	}

	// Since = far future → empty
	future := base.Add(time.Hour)
	emptyEntries := h.Since(future)
	if len(emptyEntries) != 0 {
		t.Errorf("expected 0 entries since future, got %d", len(emptyEntries))
	}

	// Since = far past → all 5
	past := base.Add(-time.Hour)
	allEntries := h.Since(past)
	if len(allEntries) != 5 {
		t.Errorf("expected 5 entries since past, got %d", len(allEntries))
	}
}

func TestAutoTuneHistory_NilSafe(t *testing.T) {
	var h *AutoTuneHistory
	h.Record(types.Event{Type: types.EventAutoTuneReloadTriggered}) // не паникует
	if h.Recent(0) != nil {
		t.Error("nil history should return nil for Recent")
	}
	if h.Since(time.Now()) != nil {
		t.Error("nil history should return nil for Since")
	}
	stats := h.Stats()
	if stats.Size != 0 {
		t.Error("nil history Stats should be zero")
	}
	h.Reset() // не паникует
}

func TestAutoTuneHistory_DefaultMaxSize(t *testing.T) {
	h := NewAutoTuneHistory(0) // invalid → default 500
	if h.maxSize != 500 {
		t.Errorf("expected default maxSize=500, got %d", h.maxSize)
	}
	h2 := NewAutoTuneHistory(-10) // invalid → default 500
	if h2.maxSize != 500 {
		t.Errorf("expected default maxSize=500 for negative, got %d", h2.maxSize)
	}
}

func TestAutoTuneHistory_Reset(t *testing.T) {
	h := NewAutoTuneHistory(10)
	for i := 0; i < 5; i++ {
		h.Record(types.Event{Type: types.EventAutoTuneReloadTriggered})
	}
	if h.Stats().Size != 5 {
		t.Fatal("setup: expected 5 entries")
	}
	h.Reset()
	if h.Stats().Size != 0 {
		t.Errorf("after Reset, Size should be 0, got %d", h.Stats().Size)
	}
	if h.Stats().DroppedCount != 0 {
		t.Errorf("after Reset, DroppedCount should be 0, got %d", h.Stats().DroppedCount)
	}
}

func TestProxy_AutoTuneHistory_LazyInit(t *testing.T) {
	p := &Proxy{}
	if p.AutoTuneHistory() == nil {
		t.Fatal("AutoTuneHistory() should auto-initialize")
	}
	// Second call returns same instance
	if p.AutoTuneHistory() != p.AutoTuneHistory() {
		t.Error("AutoTuneHistory() should return same instance on second call")
	}
}

func TestProxy_AutoTuneHistory_NilProxy(t *testing.T) {
	var p *Proxy
	if p.AutoTuneHistory() != nil {
		t.Error("nil proxy should return nil history")
	}
}

func TestProxy_PublishAutoTuneEvent_RecordsAndPublishes(t *testing.T) {
	proxy := newProxyWithCleanup(t, createTestConfig())

	// Subscribe to EventBus
	subID, subCh := proxy.EventBus().Subscribe()
	defer proxy.EventBus().Unsubscribe(subID)

	// Publish event via helper
	proxy.publishAutoTuneEvent(types.Event{
		Type:      types.EventAutoTuneReloadTriggered,
		Timestamp: time.Now(),
		BackendID: "backend-1",
		Model:     "qwen3-4b",
		Severity:  types.SeverityInfo,
		Source:    "autotune",
		Message:   "test",
	})

	// EventBus should have received it
	select {
	case ev := <-subCh:
		if ev.Type != types.EventAutoTuneReloadTriggered {
			t.Errorf("expected event type, got %s", ev.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for EventBus event")
	}

	// AutoTuneHistory should have recorded it
	entries := proxy.AutoTuneHistory().Recent(0)
	if len(entries) != 1 {
		t.Errorf("expected 1 entry in history, got %d", len(entries))
	}
	if len(entries) > 0 && entries[0].Message != "test" {
		t.Errorf("expected message=test, got %s", entries[0].Message)
	}
}
