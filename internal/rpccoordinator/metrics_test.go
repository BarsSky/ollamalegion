package rpccoordinator

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// =====================================================================
// Counter
// =====================================================================

func TestCounter_Basic(t *testing.T) {
	c := &Counter{}
	if c.Value() != 0 {
		t.Errorf("initial Value: got %d, want 0", c.Value())
	}
	c.Inc()
	c.Inc()
	c.Inc()
	if c.Value() != 3 {
		t.Errorf("after 3x Inc: got %d, want 3", c.Value())
	}
	c.Add(10)
	if c.Value() != 13 {
		t.Errorf("after Add(10): got %d, want 13", c.Value())
	}
}

func TestCounter_Concurrent(t *testing.T) {
	c := &Counter{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()
	if c.Value() != 100 {
		t.Errorf("after 100 concurrent Inc: got %d, want 100", c.Value())
	}
}

// =====================================================================
// Gauge
// =====================================================================

func TestGauge_Basic(t *testing.T) {
	g := &Gauge{}
	g.Set(10)
	if g.Value() != 10 {
		t.Errorf("after Set(10): got %d, want 10", g.Value())
	}
	g.Inc()
	if g.Value() != 11 {
		t.Errorf("after Inc: got %d, want 11", g.Value())
	}
	g.Dec()
	g.Dec()
	if g.Value() != 9 {
		t.Errorf("after 2x Dec: got %d, want 9", g.Value())
	}
}

// =====================================================================
// Histogram
// =====================================================================

func TestHistogram_Observe(t *testing.T) {
	h := NewHistogram([]float64{10, 100, 1000})
	// 5 observations ≤ 10
	for i := 0; i < 5; i++ {
		h.Observe(5)
	}
	// 3 observations ≤ 100
	for i := 0; i < 3; i++ {
		h.Observe(50)
	}
	// 1 observation > 1000 (infCount)
	h.Observe(5000)

	snap := h.Snapshot()
	if snap.Count != 9 {
		t.Errorf("Count: got %d, want 9", snap.Count)
	}
	if snap.Sum != 5*5+3*50+5000 {
		t.Errorf("Sum: got %f, want %f", snap.Sum, float64(5*5+3*50+5000))
	}
	// bucket ≤10: cumulative 5
	if snap.Counts[0] != 5 {
		t.Errorf("bucket ≤10: got %d, want 5", snap.Counts[0])
	}
	// bucket ≤100: cumulative 8 (5+3)
	if snap.Counts[1] != 8 {
		t.Errorf("bucket ≤100: got %d, want 8", snap.Counts[1])
	}
	// bucket ≤1000: cumulative 8
	if snap.Counts[2] != 8 {
		t.Errorf("bucket ≤1000: got %d, want 8", snap.Counts[2])
	}
	// InfCount = 1
	if snap.InfCount != 1 {
		t.Errorf("InfCount: got %d, want 1", snap.InfCount)
	}
}

func TestHistogram_BucketsSorted(t *testing.T) {
	h := NewHistogram([]float64{1000, 10, 100}) // intentionally unsorted
	snap := h.Snapshot()
	if snap.Buckets[0] != 10 || snap.Buckets[1] != 100 || snap.Buckets[2] != 1000 {
		t.Errorf("buckets not sorted: %v", snap.Buckets)
	}
}

// =====================================================================
// MetricsAggregator
// =====================================================================

func TestMetricsAggregator_New(t *testing.T) {
	m := NewMetricsAggregator()
	if m == nil {
		t.Fatal("NewMetricsAggregator: nil")
	}
	if m.InferenceTotal == nil {
		t.Error("InferenceTotal is nil")
	}
	if m.SliceLatencyMs == nil {
		t.Error("SliceLatencyMs is nil")
	}
	if len(m.SliceLatencyMs.Snapshot().Buckets) != len(DefaultLatencyBuckets) {
		t.Errorf("SliceLatencyMs buckets: got %d, want %d",
			len(m.SliceLatencyMs.Snapshot().Buckets), len(DefaultLatencyBuckets))
	}
}

func TestMetricsAggregator_Setters(t *testing.T) {
	m := NewMetricsAggregator()
	m.IncInferenceTotal()
	m.IncInferenceErrors()
	m.IncSliceInfer(false)
	m.IncSliceInfer(true)
	m.ObserveInferenceDuration(123.45)
	m.ObserveSliceLatency(67.89)
	m.SetActiveJobs(3)
	m.SetRegisteredWorkers(2)
	m.SetRegisteredModels(1)
	m.SetWorkerHealth("w1", true)
	m.SetWorkerHealth("w2", false)

	if m.InferenceTotal.Value() != 1 {
		t.Errorf("InferenceTotal: got %d, want 1", m.InferenceTotal.Value())
	}
	if m.InferenceErrorsTotal.Value() != 1 {
		t.Errorf("InferenceErrorsTotal: got %d, want 1", m.InferenceErrorsTotal.Value())
	}
	if m.SliceInferTotal.Value() != 2 {
		t.Errorf("SliceInferTotal: got %d, want 2", m.SliceInferTotal.Value())
	}
	if m.SliceInferErrors.Value() != 1 {
		t.Errorf("SliceInferErrors: got %d, want 1", m.SliceInferErrors.Value())
	}
	if m.ActiveJobs.Value() != 3 {
		t.Errorf("ActiveJobs: got %d, want 3", m.ActiveJobs.Value())
	}
	if m.RegisteredWorkers.Value() != 2 {
		t.Errorf("RegisteredWorkers: got %d, want 2", m.RegisteredWorkers.Value())
	}
	if m.RegisteredModels.Value() != 1 {
		t.Errorf("RegisteredModels: got %d, want 1", m.RegisteredModels.Value())
	}
	snap := m.SliceLatencyMs.Snapshot()
	if snap.Count != 1 {
		t.Errorf("SliceLatencyMs.Count: got %d, want 1", snap.Count)
	}
}

func TestMetricsAggregator_AllWorkerHealth(t *testing.T) {
	m := NewMetricsAggregator()
	m.SetWorkerHealth("w1", true)
	m.SetWorkerHealth("w2", true)
	m.SetWorkerHealth("w3", false)

	health := m.AllWorkerHealth()
	if len(health) != 3 {
		t.Errorf("AllWorkerHealth: got %d entries, want 3", len(health))
	}
	healthyCount := 0
	for _, h := range health {
		if h.WorkerID == "w3" && h.Healthy {
			t.Errorf("w3 should be unhealthy")
		}
		if h.WorkerID != "w3" && h.Healthy {
			healthyCount++
		}
	}
	if healthyCount != 2 {
		t.Errorf("healthy count: got %d, want 2", healthyCount)
	}
}

// =====================================================================
// WritePrometheus
// =====================================================================

func TestWritePrometheus_Format(t *testing.T) {
	m := NewMetricsAggregator()
	m.IncInferenceTotal()
	m.IncInferenceTotal()
	m.IncInferenceErrors()
	m.SetActiveJobs(5)
	m.SetRegisteredWorkers(3)
	m.SetRegisteredModels(2)
	m.ObserveInferenceDuration(50)
	m.ObserveInferenceDuration(200)
	m.ObserveSliceLatency(15)
	m.SetWorkerHealth("w1", true)
	m.SetWorkerHealth("w2", false)

	var buf bytes.Buffer
	if err := m.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()

	// Проверяем обязательные части формата.
	required := []string{
		"# HELP rpc_inference_total",
		"# TYPE rpc_inference_total counter",
		"rpc_inference_total 2",
		"# HELP rpc_inference_errors_total",
		"# TYPE rpc_inference_errors_total counter",
		"rpc_inference_errors_total 1",
		"# TYPE rpc_active_jobs gauge",
		"rpc_active_jobs 5",
		"# TYPE rpc_registered_workers gauge",
		"rpc_registered_workers 3",
		"# TYPE rpc_registered_models gauge",
		"rpc_registered_models 2",
		"# TYPE rpc_inference_duration_ms histogram",
		"rpc_inference_duration_ms_bucket",
		"rpc_inference_duration_ms_sum 250",
		"rpc_inference_duration_ms_count 2",
		"# TYPE rpc_slice_latency_ms histogram",
		"rpc_slice_latency_ms_bucket",
		"rpc_slice_latency_ms_count 1",
		"rpc_worker_health{worker_id=\"w1\"} 1",
		"rpc_worker_health{worker_id=\"w2\"} 0",
		"rpc_uptime_seconds",
		"le=\"+Inf\"",
	}
	for _, s := range required {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q\nfull output:\n%s", s, out)
		}
	}

	// Counter/gauge lines не должны заканчиваться метками.
	if strings.Contains(out, "rpc_inference_total{") {
		t.Error("counter should not have labels")
	}
}

func TestWritePrometheus_EmptyMetrics(t *testing.T) {
	m := NewMetricsAggregator()
	var buf bytes.Buffer
	if err := m.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()
	// Все счётчики = 0.
	if !strings.Contains(out, "rpc_inference_total 0\n") {
		t.Errorf("expected rpc_inference_total 0, got:\n%s", out)
	}
	// Uptime присутствует.
	if !strings.Contains(out, "rpc_uptime_seconds ") {
		t.Errorf("expected rpc_uptime_seconds, got:\n%s", out)
	}
	// Histograms присутствуют.
	if !strings.Contains(out, "rpc_inference_duration_ms_count 0\n") {
		t.Errorf("expected empty histogram count, got:\n%s", out)
	}
}

func TestWritePrometheus_HistogramBucketCumulative(t *testing.T) {
	m := NewMetricsAggregator()
	// 3 observations: 5, 50, 500.
	m.ObserveInferenceDuration(5)
	m.ObserveInferenceDuration(50)
	m.ObserveInferenceDuration(500)

	var buf bytes.Buffer
	if err := m.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()

	// DefaultLatencyBuckets = [5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000].
	// Ожидаемые cumulative counts:
	//   le="5":      1 (obs=5)
	//   le="10":     1
	//   le="25":     1
	//   le="50":     2 (+obs=50)
	//   le="100":    2
	//   le="250":    2
	//   le="500":    3 (+obs=500)
	//   le="1000":   3
	//   le="2500":   3
	//   le="5000":   3
	//   le="10000":  3
	//   le="+Inf":   3
	expected := []struct {
		bucket string
		count  string
	}{
		{`le="5"`, "1"},
		{`le="10"`, "1"},
		{`le="25"`, "1"},
		{`le="50"`, "2"},
		{`le="100"`, "2"},
		{`le="250"`, "2"},
		{`le="500"`, "3"},
		{`le="1000"`, "3"},
		{`le="+Inf"`, "3"},
	}
	for _, exp := range expected {
		line := "rpc_inference_duration_ms_bucket{" + exp.bucket + "} " + exp.count
		if !strings.Contains(out, line) {
			t.Errorf("missing %q\nfull output:\n%s", line, out)
		}
	}
}