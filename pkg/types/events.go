package types

import "time"

// EventType — тип события
type EventType string

const (
	EventMetrics       EventType = "metrics"               // обновление метрик бэкенда
	EventBackendAdd    EventType = "backend_add"           // добавлен бэкенд
	EventBackendRemove EventType = "backend_remove"        // удалён бэкенд
	EventStatusChange  EventType = "status_change"         // изменение статуса бэкенда
	EventLimitsChange  EventType = "limits_change"         // изменение runtime-лимитов
	EventReconfigure   EventType = "reconfigure_request"   // запрос на переформирование бэкенда
	EventProxyLog      EventType = "proxy_log"             // запись лога прокси-запроса
	EventNotification  EventType = "notification"          // F.α: нотификация для WebUI bell icon
)

// EventSeverity — severity уровень нотификации (для WebUI badge color).
type EventSeverity string

const (
	SeverityInfo     EventSeverity = "info"
	SeverityWarning  EventSeverity = "warning"
	SeverityError    EventSeverity = "error"
	SeverityCritical EventSeverity = "critical"
)

// Event — событие для WebSocket/внутренней шины
type Event struct {
	Type      EventType              `json:"type"`
	Timestamp time.Time              `json:"timestamp"`
	BackendID string                 `json:"backendId,omitempty"`
	Model     string                 `json:"model,omitempty"`
	Severity  EventSeverity          `json:"severity,omitempty"`
	Source    string                 `json:"source,omitempty"`
	Message   string                 `json:"message,omitempty"`
	Data      map[string]interface{} `json:"data"`
}