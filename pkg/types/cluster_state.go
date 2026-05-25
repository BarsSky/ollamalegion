package types

import "time"

// ClusterState - состояние кластера
type ClusterState struct {
	Timestamp       time.Time        `json:"timestamp"`
	TotalBackends   int              `json:"totalBackends"`
	HealthyBackends int              `json:"healthyBackends"`
	TotalRequests   int64            `json:"totalRequests"`
	ActiveRequests  int              `json:"activeRequests"`
	QueuedRequests  int              `json:"queuedRequests"`
	RPS             float64          `json:"rps"`
	TotalGPUUsage   float64          `json:"totalGpuUsage"`
	OperatingMode       string              `json:"operatingMode"`       // Текущий вариант работы балансера
	BackendEngine       BackendEngine       `json:"backendEngine"`       // Текущий движок инференса
	EffectiveBackendType BackendType        `json:"effectiveBackendType"` // Доминирующий тип бэкенда (пусто = смешанный)
	RecentClients       []RecentClient      `json:"recentClients,omitempty"` // Последние HTTP-клиенты (включая /api/tags)
	BackendTypeCounts   map[BackendType]int `json:"backendTypeCounts"`   // Количество бэкендов по типам
	Backends            []BackendMetrics    `json:"backends"`
}

// RecentClient — клиент, обратившийся к балансеру за последние N секунд
type RecentClient struct {
	ClientName     string    `json:"clientName"`
	ClientIP       string    `json:"clientIP"`
	UserAgent      string    `json:"userAgent"`
	LastRequestAt  time.Time `json:"lastRequestAt"`
	RequestCount   int       `json:"requestCount"`
	IsStreamActive bool      `json:"isStreamActive"` // Активен ли streaming-запрос сейчас
	Model          string    `json:"model,omitempty"` // Текущая модель (если известна)
}