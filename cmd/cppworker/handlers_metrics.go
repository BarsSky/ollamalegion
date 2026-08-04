package main

import (
	"net/http"

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
		"requests":            totalRequests,
		"errors":              totalErrors,
		"tokens":              totalTokens,
		"error_rate":          errorRate,
		"active_requests":     m.ActiveRequests.Load(),
		"active_models":       m.ActiveModels.Load(),
		"models_loaded_total":  m.TotalModelsLoaded.Load(),
		"models_unloaded_total": m.TotalModelsUnloaded.Load(),
		"token_latency_ms":    m.TokenLatencyMs.Load(),
		"reasoning_auto_enables": m.ReasoningAutoEnables.Load(),
		"unload_timeouts":     m.TotalUnloadTimeouts.Load(),
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
