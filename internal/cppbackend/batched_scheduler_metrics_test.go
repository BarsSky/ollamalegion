// batched_scheduler_metrics_test.go — Unit tests for Round 16 code-review fixes.
//
// Tests cover:
//   - GetSampleStats (sample OK / fallback counters)
//   - fixTrailingBraceMisorder (renamed from stripExtraBracesInStringValues) is
//     tested in cmd/cppworker/tool_calls_test.go (existing test suite).
//   - SampleStats type with TickDropCount field (HoL observability).
package cppbackend

import (
	"sync"
	"testing"
)

// TestGetSampleStats_Initial — при старте счётчики = 0.
func TestGetSampleStats_Initial(t *testing.T) {
	// Сбросить счётчики (тесты могут запускаться параллельно).
	sampleFallbackCounter.Store(0)
	sampleOKCounter.Store(0)
	tickDropCounter.Store(0)

	stats := GetSampleStats()
	if stats.FallbackCount != 0 {
		t.Errorf("expected FallbackCount=0, got %d", stats.FallbackCount)
	}
	if stats.OKCount != 0 {
		t.Errorf("expected OKCount=0, got %d", stats.OKCount)
	}
	if stats.TickDropCount != 0 {
		t.Errorf("expected TickDropCount=0, got %d", stats.TickDropCount)
	}
}

// TestGetSampleStats_Counters — увеличение счётчиков отражается в snapshot.
func TestGetSampleStats_Counters(t *testing.T) {
	// Сбросить перед тестом
	sampleFallbackCounter.Store(0)
	sampleOKCounter.Store(0)
	tickDropCounter.Store(0)

	// Симулируем работу
	sampleOKCounter.Add(100)
	sampleFallbackCounter.Add(5)
	tickDropCounter.Add(3)

	stats := GetSampleStats()
	if stats.OKCount != 100 {
		t.Errorf("expected OKCount=100, got %d", stats.OKCount)
	}
	if stats.FallbackCount != 5 {
		t.Errorf("expected FallbackCount=5, got %d", stats.FallbackCount)
	}
	if stats.TickDropCount != 3 {
		t.Errorf("expected TickDropCount=3, got %d", stats.TickDropCount)
	}
}

// TestGetSampleStats_Concurrent — атомарность: параллельные Add не теряются.
func TestGetSampleStats_Concurrent(t *testing.T) {
	sampleOKCounter.Store(0)

	const goroutines = 10
	const iters = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				sampleOKCounter.Add(1)
			}
		}()
	}
	wg.Wait()

	stats := GetSampleStats()
	expected := int64(goroutines * iters)
	if stats.OKCount != expected {
		t.Errorf("concurrent Add lost: expected %d, got %d", expected, stats.OKCount)
	}
}
