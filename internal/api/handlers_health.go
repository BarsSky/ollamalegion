package api

import (
	"net/http"
	"sort"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// F.γ (2026-06-28): session F — health-check UI page backend.
//
// Контекст: существующий /api/v1/health (handlers_core.go) отдаёт только верхнеуровневый
// JSON для Docker healthcheck. Для UI страницы health.html нужна расширенная диагностика:
//   - per-backend статус (healthy/degraded/unhealthy) с latency;
//   - недавние ошибки из 3 источников: healthchecker, transport EOF, EventBus events;
//   - агрегированный HealthScore (0..100).
//
// Источники данных:
//   - s.healthChecker.GetAllStatuses() → per-backend health
//   - s.proxy.GetClusterState()        → host/port/backendType/hasAgent
//   - s.eventsHub.buf.snapshot()       → SSE ring buffer (нотификации F.α)
//   - s.proxy.GetRecentProxyLogs()     → proxy_log events (через EventBus фильтр)
//
// Endpoint: GET /api/v1/health/detailed
//   - Без аутентификации (как и /api/v1/health) — нужен для health-check UI.
//   - Rate limit применяется (через routes.go).

// healthDetailedHandler — отдаёт расширенный HealthReport для UI страницы health.html.
//
// Формат ответа — types.HealthReport (JSON):
//   {
//     "timestamp": "...",
//     "status": "healthy" | "degraded" | "unhealthy" | "empty",
//     "healthScore": 0..100,
//     "totalBackends": N,
//     "healthyBackends": M,
//     "unhealthyBackends": K,
//     "withAgent": A,
//     "recentErrors": [...],  // до 50 записей
//     "errorsBySource": {"healthcheck": X, "transport": Y, "event": Z},
//     "backends": [...]
//   }
//
// Стоимость: O(B) где B — число бэкендов + O(E) где E — размер ring buffer (≤100).
// Без блокировок на длительных операциях (никаких HTTP-вызовов).
func (s *Server) healthDetailedHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	report := s.buildHealthReport()
	logger.Get().Debugw("healthDetailedHandler: report built",
		"total", report.TotalBackends,
		"healthy", report.HealthyBackends,
		"score", report.HealthScore,
		"errors", len(report.RecentErrors),
	)
	s.writeJSON(w, http.StatusOK, report)
}

// buildHealthReport — собирает HealthReport из 3 источников данных.
//
// Шаги:
//   1. Per-backend health: HealthChecker.GetAllStatuses + ClusterState.Backends (host/port).
//   2. Health score: простая формула от процента healthy бэкендов минус штраф за ошибки.
//   3. Recent errors: eventsHub.buf.snapshot() → фильтр по severity, маппинг на ErrorSource.
//   4. Сортировка: ошибки по timestamp DESC, бэкенды по id ASC.
func (s *Server) buildHealthReport() *types.HealthReport {
	now := time.Now().UTC()
	report := &types.HealthReport{
		Timestamp:       now,
		Status:          types.HealthLevelHealthy,
		HealthScore:     100,
		RecentErrors:    []types.RecentError{},
		ErrorsBySource:  map[string]int{},
		Backends:        []types.BackendHealth{},
	}

	// 1) Per-backend data: HealthChecker (latency/fails) + ClusterState (host/port/agent).
	hcStatuses := map[string]*balancer.HealthStatus{}
	if s.healthChecker != nil {
		hcStatuses = s.healthChecker.GetAllStatuses()
	}

	// ClusterState provides host/port/backendType/hasAgent for each backend.
	cluster := s.proxy.GetClusterState()
	backendMeta := map[string]types.BackendMetrics{}
	if cluster != nil {
		for _, b := range cluster.Backends {
			backendMeta[b.ID] = b
		}
		report.TotalBackends = cluster.TotalBackends
		report.HealthyBackends = cluster.HealthyBackends
	}

	// 2) Сборка per-backend списка.
	for id, meta := range backendMeta {
		bh := types.BackendHealth{
			ID:            id,
			Host:          meta.Host,
			OllamaPort:    meta.OllamaPort,
			CppWorkerPort: meta.CppWorkerPort,
			BackendType:   string(meta.BackendType),
			Status:        string(meta.Status),
			Healthy:       meta.Status == types.StatusHealthy,
			HasAgent:      meta.HasAgent,
		}
		// Round 14 (2026-07-10): подтягиваем agent attachment данные из proxy
		// (backend.AgentID/AgentPort/LastAgentContact) — нужны для UI drill-down.
		if backend := s.proxy.GetBackend(id); backend != nil {
			bh.AgentID = backend.AgentID
			bh.AgentPort = backend.AgentPort
			bh.LastAgentContact = backend.LastAgentContact
		}
		if bh.HasAgent {
			report.WithAgent++
		}
		if hc, ok := hcStatuses[id]; ok && hc != nil {
			bh.ConsecutiveFails = hc.ConsecutiveFails
			bh.AvgLatencyMs = float64(hc.AvgLatency) / float64(time.Millisecond)
			bh.LastLatencyMs = float64(hc.LastLatency) / float64(time.Millisecond)
			bh.LastCheck = hc.LastCheck
			bh.LastSuccess = hc.LastSuccess
			bh.LastFailure = hc.LastFailure
			bh.LastError = hc.LastError
			// Если ClusterState.Status=healthy, но healthChecker знает fails → degraded.
			if hc.ConsecutiveFails > 0 && bh.Status == string(types.StatusHealthy) {
				bh.Status = "degraded"
			}
		} else {
			bh.Status = "unknown"
		}
		report.Backends = append(report.Backends, bh)
	}
	report.UnhealthyBackends = report.TotalBackends - report.HealthyBackends

	// 3) Recent errors: 3 источника.
	recentErrors := s.collectRecentErrors()
	report.RecentErrors = recentErrors
	for _, e := range recentErrors {
		report.ErrorsBySource[string(e.Source)]++
	}

	// 4) HealthScore: 100% - penalty.
	//    Penalty = % unhealthy backends + 5 points per error (cap 50).
	score := 100
	if report.TotalBackends > 0 {
		unhealthyPct := report.UnhealthyBackends * 100 / report.TotalBackends
		score -= unhealthyPct
	}
	errorPenalty := len(recentErrors) * 2
	if errorPenalty > 50 {
		errorPenalty = 50
	}
	score -= errorPenalty
	if score < 0 {
		score = 0
	}
	report.HealthScore = score

	// 5) Status badge.
	switch {
	case report.TotalBackends == 0:
		report.Status = types.HealthLevelEmpty
	case report.HealthyBackends == 0:
		report.Status = types.HealthLevelUnhealthy
	case report.HealthyBackends < report.TotalBackends || len(recentErrors) > 0:
		report.Status = types.HealthLevelDegraded
	default:
		report.Status = types.HealthLevelHealthy
	}

	// 6) Sort: backends by id ASC, errors by timestamp DESC.
	sort.Slice(report.Backends, func(i, j int) bool {
		return report.Backends[i].ID < report.Backends[j].ID
	})
	sort.Slice(report.RecentErrors, func(i, j int) bool {
		return report.RecentErrors[i].Timestamp.After(report.RecentErrors[j].Timestamp)
	})
	if len(report.RecentErrors) > 50 {
		report.RecentErrors = report.RecentErrors[:50]
	}

	return report
}

// collectRecentErrors — собирает RecentError из eventsHub + healthchecker.
//
// Источники:
//   - eventsHub.buf.snapshot() — все типы событий из SSE ring buffer (F.α).
//     Конвертируем в RecentError: severity warning/error → source=event|transport.
//   - healthChecker.GetAllStatuses() — бэкенды с ConsecutiveFails > 0 → source=healthcheck.
//
// Классификация Event → ErrorSource:
//   - Data["event_kind"] == "transport_eof" → source=transport
//   - иначе → source=event
func (s *Server) collectRecentErrors() []types.RecentError {
	var errors []types.RecentError

	// Источник 1: SSE ring buffer (notifications F.α).
	if s.eventsHub != nil {
		for _, ev := range s.eventsHub.buf.snapshot() {
			// Берём только warning/error/critical (info — обычно успешные операции).
			if ev.Severity != types.SeverityWarning &&
				ev.Severity != types.SeverityError &&
				ev.Severity != types.SeverityCritical {
				continue
			}
			source := types.ErrorSourceEvent
			if kind, ok := ev.Data["event_kind"].(string); ok && kind == "transport_eof" {
				source = types.ErrorSourceTransport
			}
			path := ""
			if p, ok := ev.Data["path"].(string); ok {
				path = p
			}
			errors = append(errors, types.RecentError{
				Source:    source,
				BackendID: ev.BackendID,
				Model:     ev.Model,
				Severity:  string(ev.Severity),
				Message:   ev.Message,
				Path:      path,
				Timestamp: ev.Timestamp,
			})
		}
	}

	// Источник 2: HealthChecker — бэкенды с ConsecutiveFails > 0.
	if s.healthChecker != nil {
		for id, hs := range s.healthChecker.GetAllStatuses() {
			if hs == nil || hs.ConsecutiveFails == 0 {
				continue
			}
			msg := hs.LastError
			if msg == "" {
				msg = "consecutive healthcheck failures"
			}
			ts := hs.LastFailure
			if ts.IsZero() {
				ts = hs.LastCheck
			}
			errors = append(errors, types.RecentError{
				Source:    types.ErrorSourceHealthcheck,
				BackendID: id,
				Severity:  "error",
				Message:   msg,
				Timestamp: ts,
			})
		}
	}

	return errors
}