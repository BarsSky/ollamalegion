package types

import (
	"time"
)

// BackendStatus - статус бэкенда
type BackendStatus string

const (
	StatusHealthy   BackendStatus = "healthy"
	StatusUnhealthy BackendStatus = "unhealthy"
	StatusOffline   BackendStatus = "offline"
	StatusStarting  BackendStatus = "starting"
)

// BalancingAlgorithm - алгоритмы балансировки
type BalancingAlgorithm string

const (
	AlgorithmRoundRobin     BalancingAlgorithm = "roundrobin"
	AlgorithmLeastConn      BalancingAlgorithm = "leastconn"
	AlgorithmResourceAware  BalancingAlgorithm = "resource-aware"
	AlgorithmModelAffinity  BalancingAlgorithm = "model-affinity"
)

// Backend - конфигурация бэкенда
type Backend struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Host                string   `json:"host"`
	OllamaPort          int      `json:"ollamaPort"`
	AgentPort           int      `json:"agentPort"`
	Weight              int      `json:"weight"`
	MaxConcurrentReqs   int      `json:"maxConcurrentRequests"`
	Labels              []string `json:"labels"`
	Status              BackendStatus `json:"status"`
	LastHealthCheck     time.Time     `json:"lastHealthCheck"`
	ConsecutiveFailures int           `json:"consecutiveFailures"`
	ActiveRequests      int           `json:"activeRequests"`
}

// BackendMetrics - метрики бэкенда в реальном времени
type BackendMetrics struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	
	// GPU метрики
	GPU GPUMetrics `json:"gpu"`
	
	// Системные метрики
	System SystemMetrics `json:"system"`
	
	// Ollama метрики
	Ollama OllamaMetrics `json:"ollama"`
}

// GPUMetrics - метрики GPU
type GPUMetrics struct {
	UsagePercent    float64 `json:"usagePercent"`    // Загрузка GPU %
	MemoryTotal     uint64  `json:"memoryTotal"`     // Всего VRAM (MB)
	MemoryUsed      uint64  `json:"memoryUsed"`      // Использовано VRAM (MB)
	MemoryFree      uint64  `json:"memoryFree"`      // Свободно VRAM (MB)
	Temperature     int     `json:"temperature"`     // Температура (°C)
	PowerUsage      int     `json:"powerUsage"`      // Потребление (W)
	PowerLimit      int     `json:"powerLimit"`      // Лимит мощности (W)
	GPUClock        int     `json:"gpuClock"`        // Частота GPU (MHz)
	MemClock        int     `json:"memClock"`        // Частота памяти (MHz)
}

// SystemMetrics - системные метрики
type SystemMetrics struct {
	CPUUsagePercent float64 `json:"cpuUsagePercent"` // Загрузка CPU %
	
	MemoryTotal     uint64  `json:"memoryTotal"`     // Всего RAM (MB)
	MemoryUsed      uint64  `json:"memoryUsed"`      // Использовано RAM (MB)
	MemoryFree      uint64  `json:"memoryFree"`      // Свободно RAM (MB)
	
	DiskTotal       uint64  `json:"diskTotal"`       // Всего диска (MB)
	DiskUsed        uint64  `json:"diskUsed"`        // Использовано диска (MB)
	DiskFree        uint64  `json:"diskFree"`        // Свободно диска (MB)
	
	NetworkRX       uint64  `json:"networkRX"`       // Получено байт
	NetworkTX       uint64  `json:"networkTX"`       // Отправлено байт
}

// OllamaMetrics - метрики Ollama
type OllamaMetrics struct {
	RunningModels    []RunningModel `json:"runningModels"`    // Запущенные модели
	ActiveRequests   int            `json:"activeRequests"`   // Активные запросы
	TotalRequests    int64          `json:"totalRequests"`    // Всего запросов
	AvgResponseTime  float64        `json:"avgResponseTime"`  // Среднее время ответа (ms)
	RequestsPerSecond float64       `json:"requestsPerSecond"` // RPS
}

// RunningModel - информация о запущенной модели
type RunningModel struct {
	Name      string    `json:"name"`      // Название модели
	Size      uint64    `json:"size"`      // Размер модели (bytes)
	VRAMUsage uint64    `json:"vramUsage"` // Использование VRAM (MB)
	ExpiresAt time.Time `json:"expiresAt"` // Время истечения
	Digest    string    `json:"digest"`    // Хеш модели
}

// ResourceLimits - лимиты ресурсов для принятия решений
type ResourceLimits struct {
	GPU     GPULimits     `json:"gpu"`
	CPU     CPULimits     `json:"cpu"`
	Memory  MemoryLimits  `json:"memory"`
	Disk    DiskLimits    `json:"disk"`
}

// GPULimits - лимиты GPU
type GPULimits struct {
	MaxUsagePercent     float64 `json:"maxUsagePercent"`     // Максимальная загрузка %
	MaxVRAMUsagePercent float64 `json:"maxVramUsagePercent"` // Максимальное использование VRAM %
	MaxTemperature      int     `json:"maxTemperature"`      // Максимальная температура (°C)
}

// CPULimits - лимиты CPU
type CPULimits struct {
	MaxUsagePercent float64 `json:"maxUsagePercent"` // Максимальная загрузка %
}

// MemoryLimits - лимиты памяти
type MemoryLimits struct {
	MaxUsagePercent float64 `json:"maxUsagePercent"` // Максимальное использование %
}

// DiskLimits - лимиты диска
type DiskLimits struct {
	MinFreeMB uint64 `json:"minFreeMB"` // Минимум свободного места (MB)
}

// TLSConfig - конфигурация TLS/SSL
type TLSConfig struct {
	Enabled    bool   `json:"enabled"`    // включение TLS
	CertFile   string `json:"certFile"`   // путь к SSL сертификату
	KeyFile    string `json:"keyFile"`    // путь к SSL ключу
	MinVersion string `json:"minVersion"` // минимальная версия TLS (TLS12, TLS13)
	AutoCert   bool   `json:"autoCert"`   // автоматическая генерация self-signed сертификата
}

// AuthConfig - конфигурация аутентификации API
type AuthConfig struct {
	Enabled    bool     `json:"enabled"`    // включение аутентификации
	Tokens     []string `json:"tokens"`     // список валидных API токенов
	HeaderName string   `json:"headerName"` // имя заголовка для токена (по умолчанию "X-API-Token")
}

// LoadBalancerConfig - конфигурация балансировщика
type LoadBalancerConfig struct {
	LoadBalancer LoadBalancerSettings `json:"loadBalancer"`
	Backends     []Backend            `json:"backends"`
	Balancing    BalancingSettings    `json:"balancing"`
	Resources    ResourceLimits       `json:"resources"`
	Logging      LoggingSettings      `json:"logging"`
	API          APISettings          `json:"api"`
	TLS          TLSConfig            `json:"tls"`
	Auth         AuthConfig           `json:"auth"`
}

// APISettings - настройки API
type APISettings struct {
	RateLimit       float64 `json:"rateLimit"`       // запросов в секунду
	RateBurst       float64 `json:"rateBurst"`       // максимальное количество токенов (burst)
}

// LoadBalancerSettings - настройки балансировщика
type LoadBalancerSettings struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	APIPort int    `json:"apiPort"`
	TLSHost string `json:"tlsHost"` // хост для HTTPS (если отличается от Host)
	TLSPort int    `json:"tlsPort"` // порт для HTTPS
}

// BalancingSettings - настройки балансировки
type BalancingSettings struct {
	Algorithm           BalancingAlgorithm `json:"algorithm"`
	ModelAffinity       bool               `json:"modelAffinity"`
	SessionStickiness   bool               `json:"sessionStickiness"`
	HealthCheckInterval int                `json:"healthCheckInterval"` // секунды
	MetricsInterval     int                `json:"metricsInterval"`     // секунды
	RequestTimeout      int                `json:"requestTimeout"`      // секунды
	QueueTimeout        int                `json:"queueTimeout"`        // секунды
	QueueMaxSize        int                `json:"queueMaxSize"`        // макс. размер очереди
}

// LoggingSettings - настройки логирования
type LoggingSettings struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// AgentConfig - конфигурация агента
type AgentConfig struct {
	AgentID       string `json:"agentId"`
	BalancerURL   string `json:"balancerUrl"`
	MetricsPort   int    `json:"metricsPort"`
	CollectInterval int  `json:"collectInterval"` // секунды
	HeartbeatInterval int `json:"heartbeatInterval"` // секунды
}

// QueuedRequest - запрос в очереди
type QueuedRequest struct {
	ID          string        `json:"id"`
	Model       string        `json:"model"`
	EnqueuedAt  time.Time     `json:"enqueuedAt"`
	TargetBackend string      `json:"targetBackend"`
	Priority    int           `json:"priority"`
}

// Session - активная сессия
type Session struct {
	ID            string    `json:"id"`
	BackendID     string    `json:"backendId"`
	Model         string    `json:"model"`
	CreatedAt     time.Time `json:"createdAt"`
	LastRequestAt time.Time `json:"lastRequestAt"`
	RequestCount  int       `json:"requestCount"`
}

// HealthCheckResult - результат проверки здоровья
type HealthCheckResult struct {
	BackendID string        `json:"backendId"`
	Healthy   bool          `json:"healthy"`
	Latency   time.Duration `json:"latency"`
	Error     string        `json:"error,omitempty"`
	Timestamp time.Time     `json:"timestamp"`
}

// ClusterState - состояние кластера
type ClusterState struct {
	Timestamp      time.Time         `json:"timestamp"`
	TotalBackends  int               `json:"totalBackends"`
	HealthyBackends int              `json:"healthyBackends"`
	TotalRequests  int64             `json:"totalRequests"`
	ActiveRequests int               `json:"activeRequests"`
	QueuedRequests int               `json:"queuedRequests"`
	Backends       []BackendMetrics  `json:"backends"`
}
