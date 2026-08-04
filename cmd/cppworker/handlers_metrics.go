package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// handleInferMetrics — Round 18 P1.4 (2026-08-04): per-model stats endpoint.
//
// GET /api/infer/metrics
// → {
//     "uptime_seconds": 12345,
//     "totals": {
//       "requests": 1234, "errors": 12, "tokens": 567890,
//       "error_rate": 0.00973,
//       "active_requests": 0,
//       "active_models": 2,
//       "models_loaded_total": 5, "models_unloaded_total": 3,
//       "token_latency_ms": 23,
//       "gpu_memory_used_mb": 4200, "gpu_memory_total_mb": 8192
//     },
//     "models": {
//       "qwen3-4b": {
//         "requests": 1000, "errors": 10, "error_rate": 0.01,
//         "tokens": 500000, "avg_ms": 1234,
//         "p50_ms": 1100, "p95_ms": 2500, "p99_ms": 5000,
//         "last_used_at": "2026-08-04T07:30:00Z"
//       },
//       ...
//     }
//   }
//
// Auth: X-API-Token обязателен.
//
// Use case: admin мониторинг. Round 22 deferred: /metrics exposure (Prometheus
// format) — отдельный P2 task. Этот endpoint — JSON-only для WebUI / скриптов.
func handleInferMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	m := backend.Metrics()
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics not initialized")
		return
	}

	// Per-model stats (with p50/p95/p99 percentiles)
	models := m.GetModelMetricsSnapshot()

	// Totals (snapshot of atomic counters)
	totalRequests := m.TotalRequests.Load()
	totalErrors := m.TotalErrors.Load()
	totalTokens := m.TotalTokens.Load()
	var errorRate float64
	if totalRequests > 0 {
		errorRate = float64(totalErrors) / float64(totalRequests)
	}

	totals := map[string]interface{}{
		"requests":              totalRequests,
		"errors":                totalErrors,
		"tokens":                totalTokens,
		"error_rate":            errorRate,
		"active_requests":       m.ActiveRequests.Load(),
		"active_models":         m.ActiveModels.Load(),
		"models_loaded_total":   m.TotalModelsLoaded.Load(),
		"models_unloaded_total": m.TotalModelsUnloaded.Load(),
		"token_latency_ms":      m.TokenLatencyMs.Load(),
		"reasoning_auto_enables": m.ReasoningAutoEnables.Load(),
		"unload_timeouts":       m.TotalUnloadTimeouts.Load(),
	}

	snap := m.GetMetricsSnapshot()
	totals["uptime_seconds"] = snap["uptimeSeconds"]
	totals["avg_duration_ms_global"] = snap["avgDurationMs"]
	totals["gpu_memory_used_mb"] = snap["gpuMemoryUsedMb"]
	totals["gpu_memory_total_mb"] = snap["gpuMemoryTotalMb"]

	logger.Get().Debugw("handleInferMetrics: snapshot served",
		"models_count", len(models), "total_requests", totalRequests)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"uptime_seconds": snap["uptimeSeconds"],
		"totals":         totals,
		"models":         models,
	})
}

// handlePrometheusMetrics — Round 22 deferred (2026-08-04): Prometheus
// text format endpoint для scrape.
//
// GET /metrics
// → text/plain (Prometheus exposition format):
//
//	# HELP cppworker_requests_total Total inference requests
//	# TYPE cppworker_requests_total counter
//	cppworker_requests_total{model="qwen3-4b"} 1234
//	# HELP cppworker_errors_total Total errors
//	# TYPE cppworker_errors_total counter
//	cppworker_errors_total{model="qwen3-4b"} 12
//	# HELP cppworker_request_duration_ms Request duration in milliseconds (p50/p95/p99)
//	# TYPE cppworker_request_duration_ms summary
//	cppworker_request_duration_ms{model="qwen3-4b",quantile="0.5"} 1100
//	cppworker_request_duration_ms{model="qwen3-4b",quantile="0.95"} 2500
//	cppworker_request_duration_ms{model="qwen3-4b",quantile="0.99"} 5000
//	cppworker_request_duration_ms_sum{model="qwen3-4b"} 1234000
//	cppworker_request_duration_ms_count{model="qwen3-4b"} 1234
//	# HELP cppworker_tokens_total Total tokens generated
//	# TYPE cppworker_tokens_total counter
//	cppworker_tokens_total{model="qwen3-4b"} 567890
//	# HELP cppworker_active_requests Current in-flight requests
//	# TYPE cppworker_active_requests gauge
//	cppworker_active_requests 0
//	# HELP cppworker_active_models Currently loaded models
//	# TYPE cppworker_active_models gauge
//	cppworker_active_models 2
//	# HELP cppworker_models_loaded_total Total LoadModel calls
//	# TYPE cppworker_models_loaded_total counter
//	cppworker_models_loaded_total 5
//	# HELP cppworker_uptime_seconds Process uptime
//	# TYPE cppworker_uptime_seconds gauge
//	cppworker_uptime_seconds 12345
//	# HELP cppworker_gpu_memory_used_mb GPU memory used
//	# TYPE cppworker_gpu_memory_used_mb gauge
//	cppworker_gpu_memory_used_mb 4200
//	# HELP cppworker_gpu_memory_total_mb GPU memory total
//	# TYPE cppworker_gpu_memory_total_mb gauge
//	cppworker_gpu_memory_total_mb 8192
//
// Auth: НЕТ (Prometheus обычно scrape'ит без auth, через internal network).
// Если нужна auth — обернуть в authMiddleware (но scrape'ы тогда нужно
// конфигурить с X-API-Token). По умолчанию открыт — security model предполагает
// что /metrics НЕ доступен снаружи.
//
// Use case: Prometheus + Grafana для dashboards и alert'ов (P99 latency > 5s
// → page on-call, error_rate > 5% → investigate, etc.).
func handlePrometheusMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	m := backend.Metrics()
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics not initialized")
		return
	}

	snap := m.GetModelMetricsSnapshot()
	snapGlobal := m.GetMetricsSnapshot()

	var b strings.Builder
	writeProm := func(name, help, kind string, samples []promSample) {
		fmt.Fprintf(&b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, kind)
		for _, s := range samples {
			fmt.Fprintf(&b, "%s%s %s\n", name, s.Labels, formatPromValue(s.Value))
		}
	}

	// Per-model metrics (sort by model name for stable output)
	modelNames := make([]string, 0, len(snap))
	for name := range snap {
		modelNames = append(modelNames, name)
	}
	sort.Strings(modelNames)

	// Counters
	requestsSamples := make([]promSample, 0, len(modelNames))
	errorsSamples := make([]promSample, 0, len(modelNames))
	tokensSamples := make([]promSample, 0, len(modelNames))
	durationSummarySamples := make([]promSample, 0, len(modelNames)*3)
	durationSumSamples := make([]promSample, 0, len(modelNames))
	durationCountSamples := make([]promSample, 0, len(modelNames))

	for _, name := range modelNames {
		m := snap[name]
		label := fmt.Sprintf(`{model="%s"}`, name)
		requestsSamples = append(requestsSamples, promSample{Labels: label, Value: m.Requests})
		errorsSamples = append(errorsSamples, promSample{Labels: label, Value: m.Errors})
		tokensSamples = append(tokensSamples, promSample{Labels: label, Value: m.Tokens})
		durationSummarySamples = append(durationSummarySamples,
			promSample{Labels: fmt.Sprintf(`{model="%s",quantile="0.5"}`, name), Value: m.P50Ms},
			promSample{Labels: fmt.Sprintf(`{model="%s",quantile="0.95"}`, name), Value: m.P95Ms},
			promSample{Labels: fmt.Sprintf(`{model="%s",quantile="0.99"}`, name), Value: m.P99Ms},
		)
		// _sum = totalDuration / requests. Избегаем деления на 0.
		var sumVal int64
		if m.Requests > 0 {
			sumVal = m.AvgMs * m.Requests
		}
		durationSumSamples = append(durationSumSamples, promSample{Labels: label, Value: sumVal})
		durationCountSamples = append(durationCountSamples, promSample{Labels: label, Value: m.Requests})
	}
	writeProm("cppworker_requests_total", "Total inference requests", "counter", requestsSamples)
	writeProm("cppworker_errors_total", "Total inference errors", "counter", errorsSamples)
	writeProm("cppworker_tokens_total", "Total tokens generated", "counter", tokensSamples)
	writeProm("cppworker_request_duration_ms", "Request duration in ms (p50/p95/p99 over last 256 samples)", "summary", durationSummarySamples)
	writeProm("cppworker_request_duration_ms_sum", "Request duration sum in ms", "counter", durationSumSamples)
	writeProm("cppworker_request_duration_ms_count", "Request duration count", "counter", durationCountSamples)

	// Gauges (single sample, no labels)
	gauges := []struct {
		name, help string
		value      int64
	}{
		{"cppworker_active_requests", "Currently in-flight requests", m.ActiveRequests.Load()},
		{"cppworker_active_models", "Currently loaded models", m.ActiveModels.Load()},
		{"cppworker_models_loaded_total", "Total LoadModel calls", m.TotalModelsLoaded.Load()},
		{"cppworker_models_unloaded_total", "Total UnloadModel calls", m.TotalModelsUnloaded.Load()},
		{"cppworker_unload_timeouts", "Total UnloadModel timeouts (likely BatchedScheduler stuck)", m.TotalUnloadTimeouts.Load()},
		{"cppworker_reasoning_auto_enables", "Total reasoning auto-enables (model outside whitelist)", m.ReasoningAutoEnables.Load()},
		{"cppworker_uptime_seconds", "Process uptime in seconds", int64(snapGlobal["uptimeSeconds"].(float64))},
		{"cppworker_gpu_memory_used_mb", "GPU memory used in MB", m.GPUMemoryUsedMB.Load()},
		{"cppworker_gpu_memory_total_mb", "GPU memory total in MB", m.GPUMemoryTotalMB.Load()},
		{"cppworker_token_latency_ms", "Average per-token latency in ms", m.TokenLatencyMs.Load()},
	}
	for _, g := range gauges {
		fmt.Fprintf(&b, "# HELP %s %s\n", g.name, g.help)
		fmt.Fprintf(&b, "# TYPE %s gauge\n", g.name)
		fmt.Fprintf(&b, "%s %s\n", g.name, formatPromValue(g.value))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// promSample — пара (labels, value) для Prometheus exposition.
type promSample struct {
	Labels string // например `{model="qwen3-4b"}` или `{model="x",quantile="0.5"}`
	Value  int64
}

// formatPromValue форматирует int64 для Prometheus text format.
// Prometheus принимает только float-совместимые значения (1.0, 2.5e3, etc.).
// int64 → "1234".
func formatPromValue(v int64) string {
	return strconv.FormatInt(v, 10)
}
