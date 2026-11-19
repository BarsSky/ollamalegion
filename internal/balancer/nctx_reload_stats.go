// nctx_reload_stats.go — R634 (2026-11-19): метрики координатора: решения, длительности, ошибки, снимок, выключение.
//
// ЗАЧЕМ ВЫНЕСЕНО. `nctx_reload.go` держал 1865 строк: типы и конфигурацию,
// состояние координатора, решения о перезагрузке, HTTP-клиент, длинный DoReload,
// метрики и опрос прогресса. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"ollama-loadbalancer/pkg/types"
	"sync"
	"time"
)

type perBackendMetrics struct {
	mu            sync.Mutex
	lastReloadAt  time.Time
	lastError     string
	reloadsTotal  int64
	rejectsTotal  int64
	errorsTotal   int64
	durationSumMs int64
	durationCount int64
}

func (m *perBackendMetrics) Snapshot() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	avg := int64(0)
	if m.durationCount > 0 {
		avg = m.durationSumMs / m.durationCount
	}
	return map[string]interface{}{
		"last_reload_at":         m.lastReloadAt.UTC().Format(time.RFC3339),
		"last_error":             m.lastError,
		"reloads_total":          m.reloadsTotal,
		"rejects_total":          m.rejectsTotal,
		"errors_total":           m.errorsTotal,
		"reload_duration_ms_avg": avg,
		"reload_duration_ms_sum": m.durationSumMs,
		"reload_duration_count":  m.durationCount,
	}
}

// decision: "noop" / "reject" / "reload-start" / "reloaded" / "reload-failed".
func (c *NCtxReloadCoordinator) RecordDecision(backendID, decision, reason string) {
	if c == nil {
		return
	}
	s := c.perBackendState(backendID)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch decision {
	case "reject":
		s.rejectsTotal++
	case "reload-start":
		s.reloadsTotal++
		s.lastReloadAt = time.Now()
	}
}

func (c *NCtxReloadCoordinator) RecordReloadDuration(backendID string, d time.Duration, err error) {
	if c == nil {
		return
	}
	s := c.perBackendState(backendID)
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := d.Milliseconds()
	s.durationSumMs += ms
	s.durationCount++
	if err != nil {
		s.errorsTotal++
		s.lastError = err.Error()
	}
}

func (c *NCtxReloadCoordinator) RecordError(backendID, errMsg string) {
	if c == nil {
		return
	}
	s := c.perBackendState(backendID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errorsTotal++
	if errMsg != "" {
		s.lastError = errMsg
	}
}

func (c *NCtxReloadCoordinator) perBackendState(backendID string) *perBackendMetrics {
	if existing, ok := c.metricsByBackend.Load(backendID); ok {
		return existing.(*perBackendMetrics)
	}
	actual, _ := c.metricsByBackend.LoadOrStore(backendID, &perBackendMetrics{})
	return actual.(*perBackendMetrics)
}

//

func (c *NCtxReloadCoordinator) Snapshot() map[string]interface{} {
	if c == nil {
		return map[string]interface{}{
			"nctx_reloads_total":          int64(0),
			"nctx_rejects_total":          int64(0),
			"nctx_errors_total":           int64(0),
			"nctx_reload_duration_ms_avg": int64(0),
			"nctx_reload_duration_ms_sum": int64(0),
			"nctx_reload_duration_count":  int64(0),
			"nctx_per_backend":            map[string]interface{}{},
		}
	}
	var totalReloads, totalRejects, totalErrors, totalSumMs, totalCount int64
	perBackend := make(map[string]interface{})
	c.metricsByBackend.Range(func(key, value interface{}) bool {
		id := key.(string)
		m := value.(*perBackendMetrics)
		m.mu.Lock()
		totalReloads += m.reloadsTotal
		totalRejects += m.rejectsTotal
		totalErrors += m.errorsTotal
		totalSumMs += m.durationSumMs
		totalCount += m.durationCount
		m.mu.Unlock()
		perBackend[id] = m.Snapshot()
		return true
	})
	avg := int64(0)
	if totalCount > 0 {
		avg = totalSumMs / totalCount
	}
	return map[string]interface{}{
		"nctx_reloads_total":          totalReloads,
		"nctx_rejects_total":          totalRejects,
		"nctx_errors_total":           totalErrors,
		"nctx_reload_duration_ms_avg": avg,
		"nctx_reload_duration_ms_sum": totalSumMs,
		"nctx_reload_duration_count":  totalCount,
		"nctx_per_backend":            perBackend,
	}
}

//

//

func (c *NCtxReloadCoordinator) BackendMetricsFromState(backendID string) *types.BackendMetrics {
	if c == nil {
		return nil
	}
	return &types.BackendMetrics{
		ID:              backendID,
		Timestamp:       time.Now(),
		WarmingUpModels: []string{},
	}
}

//

func (c *NCtxReloadCoordinator) Shutdown() {
	if c == nil {
		return
	}

}

// ============================================================
// Round 33: HTTP 202 handling для cppworker async load
// ============================================================
