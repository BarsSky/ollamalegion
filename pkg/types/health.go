package types

import "time"

// HealthLevel — общий уровень здоровья кластера для UI badge.
//
// Названо HealthLevel, чтобы не конфликтовать с balancer.HealthStatus (struct
// per-backend) и types.HealthStatus (статус backend'а в ClusterState).
type HealthLevel string

const (
	// HealthLevelHealthy — все бэкенды healthy, нет активных инцидентов.
	HealthLevelHealthy HealthLevel = "healthy"
	// HealthLevelDegraded — есть проблемы, но кластер продолжает обслуживать.
	HealthLevelDegraded HealthLevel = "degraded"
	// HealthLevelUnhealthy — нет ни одного healthy бэкенда.
	HealthLevelUnhealthy HealthLevel = "unhealthy"
	// HealthLevelEmpty — нет зарегистрированных бэкендов.
	HealthLevelEmpty HealthLevel = "empty"
)

// ErrorSource — источник недавней ошибки для агрегации в /api/v1/health/detailed.
type ErrorSource string

const (
	// ErrorSourceHealthcheck — ошибка из HealthChecker (бэкенд не отвечает / unhealthy).
	ErrorSourceHealthcheck ErrorSource = "healthcheck"
	// ErrorSourceTransport — ошибка из Proxy.publishTransportEOF (EOF при streaming).
	ErrorSourceTransport ErrorSource = "transport"
	// ErrorSourceEvent — ошибка из EventBus (notifications, status_change, proxy_log).
	ErrorSourceEvent ErrorSource = "event"
)

// RecentError — запись о недавней ошибке для UI диагностики.
//
// Содержит:
//   - источник (healthcheck/transport/event);
//   - backend ID (если известен);
//   - severity из EventBus или "error" для healthcheck;
//   - текст сообщения;
//   - RFC3339Nano timestamp.
//
// Aggregated в HealthReport.RecentErrors (capacity ≤ 50).
type RecentError struct {
	Source    ErrorSource `json:"source"`
	BackendID string      `json:"backendId,omitempty"`
	Model     string      `json:"model,omitempty"`
	Severity  string      `json:"severity,omitempty"`
	Message   string      `json:"message"`
	Path      string      `json:"path,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
}

// BackendHealth — здоровье одного бэкенда для /api/v1/health/detailed.
type BackendHealth struct {
	ID              string    `json:"id"`
	Host            string    `json:"host,omitempty"`
	OllamaPort      int       `json:"ollamaPort,omitempty"`
	CppWorkerPort   int       `json:"cppworkerPort,omitempty"`
	BackendType     string    `json:"backendType,omitempty"`
	Status          string    `json:"status"` // healthy | unhealthy | unknown
	Healthy         bool      `json:"healthy"`
	ConsecutiveFails int      `json:"consecutiveFails"`
	AvgLatencyMs    float64   `json:"avgLatencyMs"`
	LastLatencyMs   float64   `json:"lastLatencyMs"`
	LastCheck       time.Time `json:"lastCheck,omitempty"`
	LastSuccess     time.Time `json:"lastSuccess,omitempty"`
	LastFailure     time.Time `json:"lastFailure,omitempty"`
	LastError       string    `json:"lastError,omitempty"`
	HasAgent        bool      `json:"hasAgent"`
	// Round 14 (2026-07-10): AgentID + AgentPort для UI/API потребления.
	// В WebUI видно какой agent прикреплён к бэкенду и на каком порту
	// доступен (для drill-down и healthchecks).
	AgentID         string    `json:"agentId,omitempty"`
	AgentPort       int       `json:"agentPort,omitempty"`
	LastAgentContact time.Time `json:"lastAgentContact,omitempty"`
}

// HealthReport — агрегированный отчёт о здоровье для /api/v1/health/detailed.
//
// Это основной payload для F.γ (Health-check UI page) — frontend рендерит
// карточки по разделам (Backends, Errors), а верхнеуровневые поля
// (Status, HealthScore, HealthyCount) используются для status badge.
//
// Источники данных:
//   - Backends: balancer.HealthChecker.GetAllStatuses() + proxy.GetClusterState()
//   - RecentErrors: eventsHub.buf (SSE ring buffer) + новые transport EOF
//   - HealthScore: 0..100 (100 = все healthy и нет ошибок)
type HealthReport struct {
	Timestamp      time.Time      `json:"timestamp"`
	Status         HealthLevel    `json:"status"`
	HealthScore    int            `json:"healthScore"` // 0..100
	TotalBackends  int            `json:"totalBackends"`
	HealthyBackends int           `json:"healthyBackends"`
	UnhealthyBackends int         `json:"unhealthyBackends"`
	WithAgent      int            `json:"withAgent"`
	RecentErrors   []RecentError  `json:"recentErrors"`
	ErrorsBySource map[string]int `json:"errorsBySource"` // source → count
	Backends       []BackendHealth `json:"backends"`
}