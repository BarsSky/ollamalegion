package types

import "time"

// BackendStatus - статус бэкенда
type BackendStatus string

const (
	StatusHealthy           BackendStatus = "healthy"
	StatusUnhealthy         BackendStatus = "unhealthy"
	StatusOffline           BackendStatus = "offline"
	StatusStarting          BackendStatus = "starting"
	StatusDraining          BackendStatus = "draining"
	StatusOllamaUnavailable BackendStatus = "ollama_unavailable" // Агент жив, но Ollama недоступна
)

// PlatformMode - режим работы платформы
type PlatformMode string

const (
	ModeAuto PlatformMode = "auto"
	ModeGPU  PlatformMode = "gpu"
	ModeCPU  PlatformMode = "cpu"
)

// Backend - конфигурация бэкенда
type Backend struct {
	ID                  string        `json:"id"`
	Name                string        `json:"name"`
	Host                string        `json:"host"`
	OllamaPort          int           `json:"ollamaPort"`
	AgentPort           int           `json:"agentPort"`
	Weight              int           `json:"weight"`
	MaxConcurrentReqs   int           `json:"maxConcurrentRequests"`
	MaxModels           int           `json:"maxModels"`
	Labels              []string      `json:"labels"`
	Status              BackendStatus `json:"status"`
	LastHealthCheck     time.Time     `json:"lastHealthCheck"`
	ConsecutiveFailures int           `json:"consecutiveFailures"`
	ActiveRequests      int           `json:"activeRequests"`
	HasAgent            bool          `json:"hasAgent"`
	AgentID             string        `json:"agentId,omitempty"` // ID прикреплённого агента v2
	LastAgentContact    time.Time     `json:"lastAgentContact"`

	// Тип бэкенда (ollama / llama_cpp)
	Type BackendType `json:"type"`

	// Engine — движок инференса (ollama_api / llama_cpp / auto).
	// Если auto — определяется по Type бэкенда.
	Engine BackendEngine `json:"engine"`

	// GPU Mode (auto / gpu / cpu)
	GPUMode PlatformMode `json:"gpuMode"`

	// Runtime-лимиты (меняются через API без перезапуска)
	RuntimeMaxModels             int `json:"runtimeMaxModels"`
	RuntimeMaxConcurrentRequests int `json:"runtimeMaxConcurrentRequests"`

	// OllamaConfig — желаемые runtime-флаги Ollama, передаваемые агенту (только для ollama-типа)
	OllamaConfig *OllamaDesiredConfig `json:"ollamaConfig,omitempty"`

	// CppWorkerPort — порт cppworker для llama.cpp-бэкендов (по умолчанию 18091)
	CppWorkerPort int `json:"cppWorkerPort,omitempty"`

	// CppWorkerConfig — настройки llama.cpp (только для llama_cpp-типа)
	CppWorkerConfig *LlamaCppConfig `json:"cppWorkerConfig,omitempty"`

	// RequestTimeout — пер-бэкенд таймаут запроса (сек), 0 = использовать глобальный LB_REQUEST_TIMEOUT
	RequestTimeout int `json:"requestTimeout"`
	// RuntimeRequestTimeout — runtime-значение таймаута от балансера (меняется адаптивно, сохраняется в state.json)
	RuntimeRequestTimeout int `json:"runtimeRequestTimeout"`
}

// OllamaDesiredConfig — желаемая конфигурация Ollama, передаваемая агенту через heartbeat
type OllamaDesiredConfig struct {
	NumGPULayers    int    `json:"numGpuLayers"`    // Количество слоёв на GPU (-1 = не менять)
	ContextLength   int    `json:"contextLength"`   // Размер контекста (-1 = не менять)
	NumParallel     int    `json:"numParallel"`     // Параллельных запросов (-1 = не менять)
	NumThreads      int    `json:"numThreads"`      // Потоков CPU (-1 = не менять)
	BatchSize       int    `json:"batchSize"`       // Размер батча (-1 = не менять)
	MaxLoadedModels int    `json:"maxLoadedModels"` // Макс. загруженных моделей (-1 = не менять)
	FlashAttention  *bool  `json:"flashAttention"`  // Flash Attention (nil = не менять)
	KVCacheQuant    string `json:"kvCacheQuant"`    // Квантование KV cache ("" = не менять)
	Source          string `json:"source"`          // Источник: balancer
}

// HealthCheckResult - результат проверки здоровья
type HealthCheckResult struct {
	BackendID string        `json:"backendId"`
	Healthy   bool          `json:"healthy"`
	Latency   time.Duration `json:"latency"`
	Error     string        `json:"error,omitempty"`
	Timestamp time.Time     `json:"timestamp"`
}