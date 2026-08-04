package cppbackend

// ============================================================
// Round 25 (2026-08-04): tests for load time history cache
// ============================================================

import (
	"testing"
	"time"
)

// TestRecordLoadTime_BasicAdd — single record → history has 1 entry.
func TestRecordLoadTime_BasicAdd(t *testing.T) {
	b := &Backend{}
	b.RecordLoadTime(LoadTimeRecord{
		SizeBytes:  1 << 30, // 1GB
		DurationMs: 10000,   // 10s
		Timestamp:  time.Now(),
	})
	hist := b.GetLoadHistory()
	if len(hist) != 1 {
		t.Fatalf("expected 1 record, got %d", len(hist))
	}
	if hist[0].SizeBytes != 1<<30 {
		t.Errorf("SizeBytes: got %d, want %d", hist[0].SizeBytes, 1<<30)
	}
	if hist[0].DurationMs != 10000 {
		t.Errorf("DurationMs: got %d, want 10000", hist[0].DurationMs)
	}
}

// TestRecordLoadTime_FIFOMaxSize — добавление больше max → FIFO eviction.
func TestRecordLoadTime_FIFOMaxSize(t *testing.T) {
	b := &Backend{}
	now := time.Now()
	// Add 25 records (max 20).
	for i := 0; i < 25; i++ {
		b.RecordLoadTime(LoadTimeRecord{
			SizeBytes:  int64(1 << 30),
			DurationMs: int64(10000 + i),
			Timestamp:  now,
		})
	}
	hist := b.GetLoadHistory()
	if len(hist) != loadHistoryMaxSize {
		t.Errorf("expected %d records (FIFO), got %d", loadHistoryMaxSize, len(hist))
	}
	// Oldest 5 должны быть evicted; newest (i=24) должен быть последним.
	// Hist[0] = i=5 (10005), Hist[last] = i=24 (10024).
	if hist[0].DurationMs != 10005 {
		t.Errorf("first.DurationMs: got %d, want 10005 (i=5)", hist[0].DurationMs)
	}
	last := hist[len(hist)-1]
	if last.DurationMs != 10024 {
		t.Errorf("last.DurationMs: got %d, want 10024 (i=24)", last.DurationMs)
	}
}

// TestEstimatedBytesPerSec_EmptyHistory — без записей → 0.
func TestEstimatedBytesPerSec_EmptyHistory(t *testing.T) {
	b := &Backend{}
	if bps := b.EstimatedBytesPerSec(); bps != 0 {
		t.Errorf("empty history: got %d, want 0", bps)
	}
}

// TestEstimatedBytesPerSec_WeightedAverage — multiple records → weighted average.
func TestEstimatedBytesPerSec_WeightedAverage(t *testing.T) {
	b := &Backend{}
	now := time.Now()
	// Note: SizeBytes — binary GiB (1<<30 = 1073741824 bytes), не decimal GB.
	// 1 GiB / 10s = 107374182.4 bytes/s = ~107.37 MB/s (binary).
	// Average 3 records: (107.37 + 107.37 + 53.69) / 3 ≈ 89.48 MB/s.
	// Проверяем что estimated bps в диапазоне [80, 100] MB/s.
	const oneGiB = int64(1 << 30)
	b.RecordLoadTime(LoadTimeRecord{SizeBytes: oneGiB, DurationMs: 10000, Timestamp: now})
	b.RecordLoadTime(LoadTimeRecord{SizeBytes: 2 * oneGiB, DurationMs: 20000, Timestamp: now})
	b.RecordLoadTime(LoadTimeRecord{SizeBytes: 5 * oneGiB, DurationMs: 100000, Timestamp: now})

	bps := b.EstimatedBytesPerSec()
	const oneMB = int64(1 << 20)
	minBps := int64(85) * oneMB
	maxBps := int64(95) * oneMB
	if bps < minBps || bps > maxBps {
		t.Errorf("avg bps: got %d (%.1f MB/s), want [%d, %d] (%.0f-%.0f MB/s)",
			bps, float64(bps)/float64(oneMB), minBps, maxBps,
			float64(minBps)/float64(oneMB), float64(maxBps)/float64(oneMB))
	}
}

// TestEstimatedBytesPerSec_StaleRecordsFilteredOut — старые записи (>7d) ignored.
func TestEstimatedBytesPerSec_StaleRecordsFilteredOut(t *testing.T) {
	b := &Backend{}
	// Старая запись: 5 GiB за 1s (= 5 GiB/s) — unrealistic, simulates pre-defrag disk.
	b.RecordLoadTime(LoadTimeRecord{
		SizeBytes:  5 * (1 << 30),
		DurationMs: 1000,
		Timestamp:  time.Now().Add(-30 * 24 * time.Hour), // 30 дней назад
	})
	// Свежая запись: 5 GiB за 100s = ~53.69 MB/s (binary).
	b.RecordLoadTime(LoadTimeRecord{
		SizeBytes:  5 * (1 << 30),
		DurationMs: 100000,
		Timestamp:  time.Now(),
	})

	bps := b.EstimatedBytesPerSec()
	const oneMB = int64(1 << 20)
	const oneGiB = int64(1 << 30)
	// Stale запись отфильтрована → только 5 GiB / 100s ≈ 53.69 MB/s.
	expected := (5 * oneGiB * 1000) / 100000 // bytes per second
	if abs(bps-expected) > oneMB {           // tolerance: 1MB
		t.Errorf("stale-filtered bps: got %d, want ~%d (diff %d)", bps, expected, abs(bps-expected))
	}
}

// TestEstimatedBytesPerSec_ZeroFiltered — zero/negative values skipped.
func TestEstimatedBytesPerSec_ZeroFiltered(t *testing.T) {
	b := &Backend{}
	now := time.Now()
	// Edge cases: 0 size, 0 duration.
	b.RecordLoadTime(LoadTimeRecord{SizeBytes: 0, DurationMs: 1000, Timestamp: now})
	b.RecordLoadTime(LoadTimeRecord{SizeBytes: 1 << 30, DurationMs: 0, Timestamp: now})
	b.RecordLoadTime(LoadTimeRecord{SizeBytes: 1 << 30, DurationMs: -1, Timestamp: now})
	// One valid record: 1 GiB / 10s = ~107.37 MB/s (binary).
	b.RecordLoadTime(LoadTimeRecord{SizeBytes: 1 << 30, DurationMs: 10000, Timestamp: now})

	bps := b.EstimatedBytesPerSec()
	const oneMB = int64(1 << 20)
	const oneGiB = int64(1 << 30)
	expected := (oneGiB * 1000) / 10000 // bytes per second
	if abs(bps-expected) > oneMB {
		t.Errorf("zero-filtered bps: got %d, want ~%d", bps, expected)
	}
}

// TestGetLoadHistory_ReturnsCopy — мутация result не должна влиять на internal state.
func TestGetLoadHistory_ReturnsCopy(t *testing.T) {
	b := &Backend{}
	b.RecordLoadTime(LoadTimeRecord{SizeBytes: 1 << 30, DurationMs: 10000, Timestamp: time.Now()})

	hist := b.GetLoadHistory()
	hist[0].SizeBytes = 999 // мутация result

	// Re-fetch — должен быть original.
	hist2 := b.GetLoadHistory()
	if hist2[0].SizeBytes != 1<<30 {
		t.Errorf("internal state mutated: got %d, want %d", hist2[0].SizeBytes, 1<<30)
	}
}

// TestResetLoadHistory — clears all records.
func TestResetLoadHistory(t *testing.T) {
	b := &Backend{}
	b.RecordLoadTime(LoadTimeRecord{SizeBytes: 1 << 30, DurationMs: 10000, Timestamp: time.Now()})
	if len(b.GetLoadHistory()) != 1 {
		t.Fatal("setup failed")
	}
	b.ResetLoadHistory()
	if len(b.GetLoadHistory()) != 0 {
		t.Errorf("after reset: got %d records, want 0", len(b.GetLoadHistory()))
	}
	if bps := b.EstimatedBytesPerSec(); bps != 0 {
		t.Errorf("after reset: bps = %d, want 0", bps)
	}
}

// TestRecordLoadTime_RealisticGemma4 — realistic gemma-4 (5GB, 90s load).
// Sanity check: history с одной realistic записью возвращает ~50-60 MB/s.
func TestRecordLoadTime_RealisticGemma4(t *testing.T) {
	b := &Backend{}
	// gemma-4 5GB, 90s load (наблюдалось в live verify v0.5.10).
	b.RecordLoadTime(LoadTimeRecord{
		SizeBytes:  5 * (1 << 30),
		DurationMs: 90000,
		Timestamp:  time.Now(),
	})
	bps := b.EstimatedBytesPerSec()
	// 5GB/90s ≈ 55.6 MB/s.
	expectedMin := int64(55 * 1024 * 1024)
	expectedMax := int64(60 * 1024 * 1024)
	if bps < expectedMin || bps > expectedMax {
		t.Errorf("gemma-4 bps: got %d, want [%d, %d]", bps, expectedMin, expectedMax)
	}
}

// --- helpers ---

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
