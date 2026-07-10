package types

import "time"

// Session - активная сессия
type Session struct {
	ID                string    `json:"id"`
	BackendID         string    `json:"backendId"`
	Model             string    `json:"model"`
	ClientName        string    `json:"clientName"`        // Имя клиента (Cline, OpenWebUI, etc.)
	ClientIP          string    `json:"clientIp"`          // IP клиента (без порта)
	UserAgent         string    `json:"userAgent"`         // Полный User-Agent для отладки
	ClientFingerprint string    `json:"clientFingerprint"` // Хеш для различения клиентов за одним IP
	CreatedAt         time.Time `json:"createdAt"`
	LastRequestAt     time.Time `json:"lastRequestAt"`
	RequestCount      int       `json:"requestCount"`
	TotalTokens       int64     `json:"totalTokens"`     // Оценочное количество токенов
	NumCtx            int       `json:"numCtx"`          // Размер контекста запроса (токенов)
	SessionWeight     float64   `json:"sessionWeight"`   // Вес сессии для адаптивного переключения
	HasActiveStream   bool      `json:"hasActiveStream"` // Активен ли streaming-запрос (не удалять)
}

// QueuedRequest - запрос в очереди
type QueuedRequest struct {
	ID            string    `json:"id"`
	Model         string    `json:"model"`
	EnqueuedAt    time.Time `json:"enqueuedAt"`
	TargetBackend string    `json:"targetBackend"`
	Priority      int       `json:"priority"`
}

// AgentConfig - конфигурация агента
type AgentConfig struct {
	AgentID               string       `json:"agentId"`
	BalancerURL           string       `json:"balancerUrl"`
	OllamaURL             string       `json:"ollamaURL"`
	CppWorkerURL          string       `json:"cppWorkerUrl"`      // URL llama.cpp CppWorker (для BackendType=llama_cpp)
	MetricsPort           int          `json:"metricsPort"`
	CollectInterval       int          `json:"collectInterval"`   // секунды
	HeartbeatInterval     int          `json:"heartbeatInterval"` // секунды
	GPUMode               PlatformMode `json:"gpuMode"`           // auto/gpu/cpu
	NVMLEnabled           bool         `json:"nvmlEnabled"`       // включить NVML
	PublicHost            string       `json:"publicHost"`        // публичный IP/hostname agent'а, доступный балансеру
	// 2026-06-30: явный host/port физического cppworker-бэкенда, который agent обёртывает.
	// Используется в register() как host/cppWorkerPort, чтобы de-dup по (host, port)
	// в /api/v1/gguf/backends корректно склеивал cppworker-gpu и cppworker-gpu-bundled-agent.
	CppWorkerHost         string       `json:"cppWorkerHost"`     // host физического cppworker (например, "cppworker-gpu")
	CppWorkerPort         int          `json:"cppWorkerPort"`     // port физического cppworker (например, 18092)
	MaxModels             int          `json:"maxModels"`         // максимум моделей (-1 = авто/не задано)
	MaxConcurrentRequests int          `json:"maxConcurrentRequests"` // максимум одновременных запросов (-1 = авто/не задано)
	Weight                int          `json:"weight"`            // приоритетный вес бэкенда (1-100, по умолчанию 1)
	BackendType           BackendType  `json:"backendType"`       // тип бэкенда: ollama или llama_cpp (по умолчанию ollama)
	NodeLabels            string       `json:"nodeLabels"`        // метки узла (key=value через запятую)

	// Round 13 (2026-07-10): BackendID — ID бэкенда, к которому agent прикреплён.
	// Получается из ответа /api/v1/agents/register (поле backendId при action=attached).
	// Используется в heartbeat: agent отправляет heartbeat на
	// POST /api/v1/backends/{BackendID}/agent/heartbeat — это правильный ID,
	// а не agentID (который при attached режиме отличается от backendID).
	BackendID             string       `json:"backendId,omitempty"`
}
