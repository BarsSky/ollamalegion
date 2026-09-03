// llama_collector_test.go — tests for parallel LlamaCollector.Collect()
//
// R58 (2026-09-03): tests for the parallel refactor. Verify that 3 sequential
// HTTP endpoints (gpu / models / info) are fetched concurrently, not serially.
//
// Reference: agent/llama_collector.go + docs/superpowers/specs/2026-09-03-...
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestLlamaCollector_Collect_Parallel — verifies 3 endpoints are fetched
// in parallel (total time ≈ max of 3, NOT sum of 3).
//
// Pre-R58: ~3*delay (sequential). Post-R58: ~1*delay (parallel).
func TestLlamaCollector_Collect_Parallel(t *testing.T) {
	const delay = 200 * time.Millisecond
	var hits int32
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// Each endpoint sleeps `delay` before responding.
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/gpu":
			_, _ = w.Write([]byte(`{"gpuCount":1,"devices":[{"index":0,"name":"RTX","vramTotalMB":8192,"vramFreeMB":4096}]}`))
		case "/api/models":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3-8b","architecture":"llama","sizeBytes":4500000000,"loadedAt":"2026-09-01"}],"count":1}`))
		case "/api/info":
			_, _ = w.Write([]byte(`{"uptime":"1h","version":"0.5.23"}`))
		default:
			http.NotFound(w, r)
		}
	}
	mux.HandleFunc("/api/gpu", handler)
	mux.HandleFunc("/api/models", handler)
	mux.HandleFunc("/api/info", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	lc := NewLlamaCollector(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	metrics, err := lc.Collect(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Collect() returned error: %v", err)
	}
	if metrics == nil {
		t.Fatal("Collect() returned nil metrics")
	}
	if metrics.GPUCount != 1 {
		t.Errorf("GPUCount = %d, want 1", metrics.GPUCount)
	}
	if metrics.ModelCount != 1 {
		t.Errorf("ModelCount = %d, want 1", metrics.ModelCount)
	}
	if metrics.Version != "0.5.23" {
		t.Errorf("Version = %q, want %q", metrics.Version, "0.5.23")
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("expected 3 endpoint hits, got %d", got)
	}

	// Sequential would be 3*delay = 600ms. Parallel should be ~delay = 200ms.
	// Allow 2x slack for goroutine scheduling and CI noise.
	maxAllowed := 2 * delay
	if elapsed > maxAllowed {
		t.Errorf("Collect() took %v, expected ~%v (parallel). Sequential would be ~3*%v = %v.",
			elapsed, delay, delay, 3*delay)
	}
}

// TestLlamaCollector_Collect_PartialFailure — if one endpoint fails,
// others still return. Total time still ~delay (parallel), not sum.
func TestLlamaCollector_Collect_PartialFailure(t *testing.T) {
	const delay = 150 * time.Millisecond
	mux := http.NewServeMux()
	mux.HandleFunc("/api/gpu", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[],"count":0}`))
	})
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uptime":"5m","version":"0.5.23"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	lc := NewLlamaCollector(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	metrics, err := lc.Collect(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Collect() returned error (should be nil for partial failure): %v", err)
	}
	if metrics == nil {
		t.Fatal("Collect() returned nil metrics")
	}
	// GPU failed → GPUCount = 0, GPUMetrics = nil
	if metrics.GPUCount != 0 {
		t.Errorf("GPUCount = %d, want 0 (GPU endpoint failed)", metrics.GPUCount)
	}
	if len(metrics.GPUMetrics) != 0 {
		t.Errorf("GPUMetrics should be empty, got %d entries", len(metrics.GPUMetrics))
	}
	// Models succeeded
	if metrics.ModelCount != 0 {
		t.Errorf("ModelCount = %d, want 0", metrics.ModelCount)
	}
	// Info succeeded
	if metrics.Version != "0.5.23" {
		t.Errorf("Version = %q, want %q (info should still return)", metrics.Version, "0.5.23")
	}

	// Even with partial failure, all 3 should be in parallel.
	maxAllowed := 2 * delay
	if elapsed > maxAllowed {
		t.Errorf("Collect() with partial failure took %v, expected ~%v (parallel).", elapsed, delay)
	}
}

// TestLlamaCollector_Collect_AllFailures — if all 3 endpoints fail, Collect
// still returns nil error (errors are best-effort, the collector continues).
func TestLlamaCollector_Collect_AllFailures(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/gpu", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	lc := NewLlamaCollector(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	metrics, err := lc.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect() should NOT return error for best-effort collection, got: %v", err)
	}
	if metrics == nil {
		t.Fatal("Collect() returned nil metrics")
	}
	if metrics.GPUCount != 0 || metrics.ModelCount != 0 || metrics.Version != "" {
		t.Errorf("expected zero metrics on all failures, got: GPUCount=%d ModelCount=%d Version=%q",
			metrics.GPUCount, metrics.ModelCount, metrics.Version)
	}
}

// TestLlamaCollector_Collect_ContextCancellation — context cancel halts all
// in-flight goroutines promptly.
func TestLlamaCollector_Collect_ContextCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/gpu", func(w http.ResponseWriter, r *http.Request) {
		// Sleep longer than the test timeout
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	lc := NewLlamaCollector(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := lc.Collect(ctx)
	elapsed := time.Since(start)

	// Should return within ~250ms (not 5s) due to context cancellation.
	if elapsed > 1*time.Second {
		t.Errorf("Collect() took %v, expected ~200ms (context cancellation should short-circuit)", elapsed)
	}
	if err != nil && !strings.Contains(err.Error(), "context") {
		t.Errorf("expected context error, got: %v", err)
	}
}
