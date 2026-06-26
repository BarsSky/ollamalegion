// Package api — B7: Prometheus-style metrics endpoint для RPC Coordinator.
//
// Endpoint: GET /api/v1/rpc/metrics
// Content-Type: text/plain; version=0.0.4; charset=utf-8
//
// Экспортирует все метрики из coordinator'а через MetricsAggregator.WritePrometheus.

package api

import (
	"net/http"

	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/pkg/logger"
)

// handleRpcMetrics — GET /api/v1/rpc/metrics.
//
// Возвращает Prometheus exposition format с метриками coordinator'а:
//
//   - rpc_inference_total, rpc_inference_errors_total
//   - rpc_slice_infer_total, rpc_slice_infer_errors_total
//   - rpc_load_slice_total, rpc_unload_slice_total
//   - rpc_active_jobs, rpc_registered_workers, rpc_registered_models
//   - rpc_kv_cache_size_bytes, rpc_uptime_seconds
//   - rpc_worker_health{worker_id=...} (per-worker gauge)
//   - rpc_inference_duration_ms, rpc_slice_latency_ms (histograms)
//
// Если RPC Coordinator отключён (`s.proxy.GetRpcCoordinator() == nil` или
// `c.Enabled() == false`), возвращает 503 с пустым телом.
//
// Требует X-API-Token через authMiddleware (как и остальные /rpc/* endpoints).
func (s *Server) handleRpcMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}

	coord := s.proxy.GetRpcCoordinator()
	if coord == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "rpc_disabled",
			"RPC Coordinator is not configured")
		return
	}
	if !coord.Enabled() {
		writeJSONError(w, http.StatusServiceUnavailable, "rpc_disabled",
			"RPC Coordinator is disabled (enabled=false)")
		return
	}

	// Update gauges с актуальным состоянием перед экспортом.
	m := coord.Metrics()
	s.refreshCoordinatorMetrics(coord, m)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := m.WritePrometheus(w); err != nil {
		logger.Get().Warnw("rpc metrics: write failed", "error", err)
		// Headers уже отправлены, ничего больше не сделать.
	}
}

// refreshCoordinatorMetrics обновляет gauges в metrics перед экспортом.
//
//   - RegisteredWorkers / RegisteredModels: текущее число из coordinator'а.
//   - ActiveJobs: текущее число in-flight задач.
//   - Per-worker health: IsHealthy() каждого зарегистрированного worker'а.
//
// Это вызывается на каждом /metrics запросе (не в фоне) — низкая частота,
// overhead минимальный.
func (s *Server) refreshCoordinatorMetrics(coord *rpccoordinator.ModelCoordinator, m *rpccoordinator.MetricsAggregator) {
	stats := coord.Stats()
	m.SetRegisteredWorkers(int64(len(stats.WorkersHealth)))
	m.SetRegisteredModels(int64(len(stats.Models)))
	m.SetActiveJobs(int64(stats.ActiveJobs))
	for id, healthy := range stats.WorkersHealth {
		m.SetWorkerHealth(id, healthy)
	}
}
