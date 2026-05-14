package types

import "time"

// EventType — тип события
type EventType string

const (
	EventMetrics       EventType = "metrics"       // обновление метрик бэкенда
	EventBackendAdd    EventType = "backend_add"   // добавлен бэкенд
	EventBackendRemove EventType = "backend_remove" // удалён бэкенд
	EventStatusChange  EventType = "status_change"  // изменение статуса бэкенда
	EventLimitsChange  EventType = "limits_change"  // изменение runtime-лимитов
	EventReconfigure   EventType = "reconfigure_request"  // запрос на переформирование бэкенда
	EventProxyLog      EventType = "proxy_log"      // запись лога прокси-запроса
)


// Event — событие для WebSocket/внутренней шины
type Event struct {
	Type      EventType              `json:"type"`
	Timestamp time.Time              `json:"timestamp"`
	BackendID string                 `json:"backendId,omitempty"`
	Data      map[string]interface{} `json:"data"`
}