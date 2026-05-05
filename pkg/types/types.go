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
	StatusDraining  BackendStatus = "draining"
)

// ModelState - состояние модели на бэкенде
type ModelState string

const (
	ModelStateLoaded    ModelState = "LOADED"
	ModelStateLoading   ModelState = "LOADING"
	ModelStateNotLoaded ModelState = "NOT_LOADED"
	ModelStateUnloading ModelState = "UNLOADING"
	ModelStateWarmingUp ModelState = "WARMING_UP"
)

// WarmupState - состояние превентивной загрузки модели
type WarmupState struct {
	StartedAt        time.Time `json:"startedAt"`
	EstimatedReadyAt time.Time `json:"estimatedReadyAt"`
	TriggerReason    string    `json:"triggerReason"` // "load_threshold", "queue_depth", "min_instances"
}

// BalancingAlgorithm - алгоритмы балансировки
type BalancingAlgorithm string

const (
	AlgorithmRoundRobin    BalancingAlgorithm = "roundrobin"
	AlgorithmLeastConn     BalancingAlgorithm = "leastconn"
	AlgorithmResourceAware BalancingAlgorithm = "resource-aware"
	AlgorithmModelAffinity BalancingAlgorithm = "model-affinity"
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
	LastAgentContact    time.Time     `json:"lastAgentContact"`

	// Runtime-лимиты (меняются через API без перезапуска)
	RuntimeMaxModels             int `json:"runtimeMaxModels"`
	RuntimeMaxConcurrentRequests int `json:"runtimeMaxConcurrentRequests"`
}

// BackendMetrics - метрики бэкенда в реальном времени
type BackendMetrics struct {
	ID        string        `json:"id"`
	Timestamp time.Time     `json:"timestamp"`
	Status    BackendStatus `json:"status"`
	HasAgent  bool          `json:"hasAgent"` // Флаг наличия активного агента

	// Конфигурация бэкенда (атомарно копируется из Backend)
	Host       string `json:"host"`
	OllamaPort int    `json:"ollamaPort"`

	// GPU метрики
	GPU GPUMetrics `json:"gpu"`

	// Системные метрики
	System SystemMetrics `json:"system"`

	// Ollama метрики
	Ollama OllamaMetrics `json:"ollama"`

	// Прогноз критического состояния
	Prediction Prediction `json:"prediction"`

	// --- Monitor-friendly computed fields (filled by GetClusterState) ---
	Score                 float64  `json:"score"`                 // Calculated routing score
	MaxConcurrentRequests int      `json:"maxConcurrentRequests"` // From backend config
	Models                []string `json:"models"`                // Names of running models
	VRAMUsagePercent      float64  `json:"vramUsagePercent"`      // GPU memory usage %
	VRAMTotalGB           float64  `json:"vramTotalGB"`           // Total VRAM in GB
	VRAMUsedGB            float64  `json:"vramUsedGB"`            // Used VRAM in GB
	MemoryUsagePercent    float64  `json:"memoryUsagePercent"`    // RAM usage %
}

// PlatformMode - режим работы платформы
type PlatformMode string

const (
	ModeAuto PlatformMode = "auto"
	ModeGPU  PlatformMode = "gpu"
	ModeCPU  PlatformMode = "cpu"
)

// GPUMetrics - метрики GPU
type GPUMetrics struct {
	UsagePercent float64 `json:"usagePercent"` // Загрузка GPU %
	MemoryTotal  uint64  `json:"memoryTotal"`  // Всего VRAM (MB)
	MemoryUsed   uint64  `json:"memoryUsed"`   // Использовано VRAM (MB)
	MemoryFree   uint64  `json:"memoryFree"`   // Свободно VRAM (MB)
	Temperature  int     `json:"temperature"`  // Температура (°C)
	PowerUsage   int     `json:"powerUsage"`   // Потребление (W)
	PowerLimit   int     `json:"powerLimit"`   // Лимит мощности (W)
	GPUClock     int     `json:"gpuClock"`     // Частота GPU (MHz)
	MemClock     int     `json:"memClock"`     // Частота памяти (MHz)
}

// CPUMetrics - расширенные метрики CPU
type CPUMetrics struct {
	UsagePercent  float64   `json:"usagePercent"`  // Общая загрузка CPU %
	UsagePerCore  []float64 `json:"usagePerCore"`  // Загрузка по ядрам
	CoreCount     int       `json:"coreCount"`     // Количество ядер
	ThreadCount   int       `json:"threadCount"`   // Количество потоков
	Model         string    `json:"model"`         // Модель процессора
	LoadAverage1  float64   `json:"loadAverage1"`  // Load average 1 мин
	LoadAverage5  float64   `json:"loadAverage5"`  // Load average 5 мин
	LoadAverage15 float64   `json:"loadAverage15"` // Load average 15 мин
	Temperature   int       `json:"temperature"`   // Температура CPU (°C)
	Throttled     bool      `json:"throttled"`     // CPU троттлинг
}

// SystemMetrics - системные метрики
type SystemMetrics struct {
	CPUUsagePercent float64 `json:"cpuUsagePercent"` // Загрузка CPU %

	// Расширенные CPU метрики
	CPU CPUMetrics `json:"cpu"`

	MemoryTotal uint64 `json:"memoryTotal"` // Всего RAM (MB)
	MemoryUsed  uint64 `json:"memoryUsed"`  // Использовано RAM (MB)
	MemoryFree  uint64 `json:"memoryFree"`  // Свободно RAM (MB)

	DiskTotal uint64 `json:"diskTotal"` // Всего диска (MB)
	DiskUsed  uint64 `json:"diskUsed"`  // Использовано диска (MB)
	DiskFree  uint64 `json:"diskFree"`  // Свободно диска (MB)

	NetworkRX uint64 `json:"networkRX"` // Получено байт
	NetworkTX uint64 `json:"networkTX"` // Отправлено байт

	NetworkRXRate float64 `json:"networkRXRate"` // Скорость получения (байт/с)
	NetworkTXRate float64 `json:"networkTXRate"` // Скорость отправки (байт/с)
}

// OllamaMetrics - метрики Ollama
type OllamaMetrics struct {
	RunningModels         []RunningModel    `json:"runningModels"`         // Запущенные (загруженные в память) модели
	AvailableModels       []RunningModel    `json:"availableModels"`       // Доступные модели (все, что можно загрузить)
	ActiveRequests        int               `json:"activeRequests"`        // Активные запросы (от балансировщика — точные)
	TotalRequests         int64             `json:"totalRequests"`         // Всего запросов (от балансировщика)
	AvgResponseTime       float64           `json:"avgResponseTime"`       // Среднее время ответа (ms)
	RequestsPerSecond     float64           `json:"requestsPerSecond"`     // RPS (от балансировщика)
	MaxModels             int               `json:"maxModels"`             // Максимум доступных для загрузки моделей (-1 = авто)
	MaxConcurrentRequests int               `json:"maxConcurrentRequests"` // Максимум одновременных запросов (-1 = авто)
	FreeSlots             int               `json:"freeSlots"`             // Свободные слоты для запросов (от балансера)
	AvailableSlots        int               `json:"availableSlots"`        // Реальные доступные слоты (от агента, с учётом VRAM)
	RuntimeFlags          OllamaRuntimeFlags `json:"runtimeFlags"`          // Флаги запуска Ollama
	ModelContexts         []ModelContextInfo `json:"modelContexts"`         // Информация о контексте по моделям
	BackendCapacity       BackendCapacity    `json:"backendCapacity"`       // Оценка ёмкости бэкенда
	ModelSizes            map[string]int64   `json:"modelSizes"`            // Размеры всех доступных моделей (modelName → bytes)
}

// OllamaRuntimeFlags - флаги запуска процесса Ollama
type OllamaRuntimeFlags struct {
	NumGPULayers   int    `json:"numGpuLayers"`   // Количество слоёв на GPU (-ngl, --num-gpu-layers)
	ContextLength  int    `json:"contextLength"`  // Размер контекста (-c, --ctx-size)
	NumParallel    int    `json:"numParallel"`    // Параллельных запросов (-np, --parallel)
	NumThreads     int    `json:"numThreads"`     // Потоков CPU (-t, --threads)
	BatchSize      int    `json:"batchSize"`      // Размер батча (-b, --batch-size)
	GPUSplitMode   string `json:"gpuSplitMode"`   // Режим разделения GPU (--split-mode)
	MainGPU        int    `json:"mainGpu"`        // Основной GPU (--main-gpu)
	LowVRAM        bool   `json:"lowVram"`        // Режим low VRAM (--low-vram)
	F16KV          bool   `json:"f16kv"`          // FP16 для KV cache (--no-kv-offload отключает)
	KVCacheQuant   string `json:"kvCacheQuant"`   // Квантование KV cache (--cache-type-k)
	FlashAttention bool   `json:"flashAttention"` // Flash Attention (--flash-attn)
	Source         string `json:"source"`         // Источник: process-args / env / default
}

// ModelContextInfo - информация о контексте модели
type ModelContextInfo struct {
	Name              string `json:"name"`              // Название модели
	ContextLength     int    `json:"contextLength"`     // Размер контекста (токенов)
	ContextSource     string `json:"contextSource"`     // Источник: modelfile / env / runtime / default
	EffectiveContext  int    `json:"effectiveContext"`  // Фактический контекст с учётом флагов
	ContextMemoryMB   uint64 `json:"contextMemoryMB"`   // Память контекста (MB)
	KVCacheMemoryMB   uint64 `json:"kvCacheMemoryMB"`   // Память KV cache (MB)
	ModelMemoryMB     uint64 `json:"modelMemoryMB"`     // Память самой модели (MB)
	TotalMemoryMB     uint64 `json:"totalMemoryMB"`     // Общая память модели + контекст (MB)
	NumLayers         int    `json:"numLayers"`         // Количество слоёв (для расчёта)
	HiddenSize        int    `json:"hiddenSize"`        // Размер скрытого слоя (для расчёта)
	PrecisionBits     int    `json:"precisionBits"`     // Точность KV cache (16 или 32)
}

// AvailableModel - доступная модель с оценкой загружаемости
type AvailableModel struct {
	Name           string `json:"name"`           // Название модели
	Size           uint64 `json:"size"`           // Размер модели (bytes)
	VRAMUsage      uint64 `json:"vramUsage"`      // Оценка VRAM (MB)
	CanLoad        bool   `json:"canLoad"`        // Может ли быть загружена
	LoadReason     string `json:"loadReason"`     // Причина (если не может)
	ContextLength  int    `json:"contextLength"`  // Контекст модели
	EstimatedVRAM  uint64 `json:"estimatedVram"`  // Оценка total VRAM с контекстом (MB)
	Family         string `json:"family"`         // Семейство
	ParameterSize  string `json:"parameterSize"`  // Размер параметров
	Quantization   string `json:"quantization"`   // Квантование
}

// BackendCapacity - оценка ёмкости бэкенда
type BackendCapacity struct {
	FreeVRAM           uint64           `json:"freeVram"`           // Свободно VRAM (MB)
	GuaranteedVRAM     uint64           `json:"guaranteedVram"`     // Гарантированно свободно (90% free) (MB)
	LoadedModelVRAM    uint64           `json:"loadedModelVram"`    // VRAM загруженных моделей (MB)
	ContextOverheadMB  uint64           `json:"contextOverheadMB"`  // Память контекстов (MB)
	AvailableModels    []AvailableModel `json:"availableModels"`    // Доступные модели с оценкой
	LoadableModelCount int              `json:"loadableModelCount"` // Количество моделей, которые можно загрузить
	Mode               PlatformMode     `json:"mode"`               // Режим: gpu / cpu
}

// RunningModel - информация о запущенной модели
type RunningModel struct {
	Name          string    `json:"name"`          // Название модели
	Size          uint64    `json:"size"`          // Размер модели (bytes)
	VRAMUsage     uint64    `json:"vramUsage"`     // Использование VRAM (MB)
	RAMUsage      uint64    `json:"ramUsage"`      // Использование RAM на CPU (MB)
	ExpiresAt     time.Time `json:"expiresAt"`     // Время истечения
	Digest        string    `json:"digest"`        // Хеш модели
	LoadCount     int       `json:"loadCount"`     // Количество загрузок
	Family        string    `json:"family"`        // Семейство моделей (llama, mistral, etc.)
	Format        string    `json:"format"`        // Формат модели (gguf, etc.)
	ParameterSize string    `json:"parameterSize"` // Размер параметров (7B, 13B, etc.)
	Quantization  string    `json:"quantization"`  // Квантование (Q4_0, Q8_0, etc.)
}

// ModelDetails - детали модели из /api/tags
type ModelDetails struct {
	Family        string `json:"family"`
	Format        string `json:"format"`
	ParameterSize string `json:"parameterSize"`
	Quantization  string `json:"quantization"`
}

// ResourceLimits - лимиты ресурсов для принятия решений
type ResourceLimits struct {
	GPU    GPULimits    `json:"gpu"`
	CPU    CPULimits    `json:"cpu"`
	Memory MemoryLimits `json:"memory"`
	Disk   DiskLimits   `json:"disk"`
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
	RateLimit float64 `json:"rateLimit"` // запросов в секунду
	RateBurst float64 `json:"rateBurst"` // максимальное количество токенов (burst)
}

// LoadBalancerSettings - настройки балансировщика
type LoadBalancerSettings struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	APIPort   int    `json:"apiPort"`
	TLSHost   string `json:"tlsHost"`   // хост для HTTPS (если отличается от Host)
	TLSPort   int    `json:"tlsPort"`   // порт для HTTPS
	StatePath string `json:"statePath"` // путь к файлу сохранения состояния (по умолчанию "data/state.json")
}

// PrewarmConfig - конфигурация превентивной загрузки
type PrewarmConfig struct {
	Enabled              bool    `json:"enabled"`
	TriggerLoadThreshold float64 `json:"triggerLoadThreshold"` // загрузка бэкенда для триггера (0.0-1.0)
	MaxPrewarmPerCycle   int     `json:"maxPrewarmPerCycle"`   // макс. одновременных pre-warm
	CheckIntervalSec     int     `json:"checkIntervalSec"`     // интервал проверки (сек)
}

// ModelInstanceConfig - конфигурация управления экземплярами модели
type ModelInstanceConfig struct {
	DefaultMinInstances int    `json:"defaultMinInstances"` // минимум экземпляров по умолчанию
	DefaultMaxInstances int    `json:"defaultMaxInstances"` // максимум экземпляров по умолчанию
	IdleUnloadAfter     string `json:"idleUnloadAfter"`     // выгрузить после простоя ("5m", "10m")
}

// ScoringWeights - веса для формулы скоринга
type ScoringWeights struct {
	ModelAlreadyLoaded float64 `json:"modelAlreadyLoaded"` // бонус за готовую модель
	ModelLoadingCost   float64 `json:"modelLoadingCost"`   // штраф за необходимость загрузки
	QueueDepthPenalty  float64 `json:"queueDepthPenalty"`  // штраф за глубину очереди
	ErrorRatePenalty   float64 `json:"errorRatePenalty"`   // штраф за историю ошибок
	PredictionBonus    float64 `json:"predictionBonus"`    // бонус за прогноз
}

// SyncModelLoadConfig - конфигурация синхронной загрузки модели
type SyncModelLoadConfig struct {
	Enabled bool   `json:"enabled"`
	Timeout string `json:"timeout"` // таймаут ожидания ("30s")
}

// ResourceReservationConfig - конфигурация резервирования ресурсов
type ResourceReservationConfig struct {
	GPUHeadroomPercent float64 `json:"gpuHeadroomPercent"` // резерв GPU памяти (%)
	RAMHeadroomPercent float64 `json:"ramHeadroomPercent"` // резерв RAM (%)
}

// BalancingSettings - настройки балансировки
type BalancingSettings struct {
	Algorithm            BalancingAlgorithm      `json:"algorithm"`
	ModelAffinity        bool                    `json:"modelAffinity"`
	SessionStickiness    bool                    `json:"sessionStickiness"`
	HealthCheckInterval  int                     `json:"healthCheckInterval"`  // секунды
	MetricsInterval      int                     `json:"metricsInterval"`      // секунды
	RequestTimeout       int                     `json:"requestTimeout"`       // секунды
	FirstByteTimeout     int                     `json:"firstByteTimeout"`     // таймаут первого байта streaming (сек, 0=дефолт 30)
	StreamingIdleTimeout int                     `json:"streamingIdleTimeout"` // таймаут простоя между чанками streaming (сек, 0=дефолт 120)
	QueueTimeout         int                     `json:"queueTimeout"`         // секунды
	QueueMaxSize         int                     `json:"queueMaxSize"`         // макс. размер очереди
	QueueWorkers         int                     `json:"queueWorkers"`         // количество workers очереди
	SessionTTL           int                     `json:"sessionTTL"`           // секунды (0 = дефолт 900)

	// Новые поля оптимизации балансировки
	Prewarm             PrewarmConfig            `json:"prewarm"`
	ModelInstances      ModelInstanceConfig      `json:"modelInstances"`
	Scoring             ScoringWeights           `json:"scoring"`
	SyncModelLoad       SyncModelLoadConfig      `json:"syncModelLoad"`
	ResourceReservation ResourceReservationConfig `json:"resourceReservation"`

	// Feature flags
	UseEnhancedScoring bool `json:"useEnhancedScoring"` // Расширенный скоринг v2 (полная формула)
	ModelLoadTimeout   int  `json:"modelLoadTimeout"`   // Таймаут ожидания загрузки модели (сек, default 120)
}

// LoggingSettings - настройки логирования
type LoggingSettings struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// AgentConfig - конфигурация агента
type AgentConfig struct {
	AgentID               string       `json:"agentId"`
	BalancerURL           string       `json:"balancerUrl"`
	OllamaURL             string       `json:"ollamaURL"`
	MetricsPort           int          `json:"metricsPort"`
	CollectInterval       int          `json:"collectInterval"`       // секунды
	HeartbeatInterval     int          `json:"heartbeatInterval"`     // секунды
	GPUMode               PlatformMode `json:"gpuMode"`               // auto/gpu/cpu
	NVMLEnabled           bool         `json:"nvmlEnabled"`           // включить NVML
	PublicHost            string       `json:"publicHost"`            // публичный IP/hostname, доступный балансеру
	MaxModels             int          `json:"maxModels"`             // максимум моделей (-1 = авто/не задано)
	MaxConcurrentRequests int          `json:"maxConcurrentRequests"` // максимум одновременных запросов (-1 = авто/не задано)
	Weight                int          `json:"weight"`                // приоритетный вес бэкенда (1-100, по умолчанию 1)
}

// QueuedRequest - запрос в очереди
type QueuedRequest struct {
	ID            string    `json:"id"`
	Model         string    `json:"model"`
	EnqueuedAt    time.Time `json:"enqueuedAt"`
	TargetBackend string    `json:"targetBackend"`
	Priority      int       `json:"priority"`
}

// Session - активная сессия
type Session struct {
	ID            string    `json:"id"`
	BackendID     string    `json:"backendId"`
	Model         string    `json:"model"`
	ClientName    string    `json:"clientName"`    // Имя клиента (Cline, OpenWebUI, etc.)
	ClientIP      string    `json:"clientIp"`      // IP клиента (без порта)
	UserAgent     string    `json:"userAgent"`     // Полный User-Agent для отладки
	CreatedAt     time.Time `json:"createdAt"`
	LastRequestAt time.Time `json:"lastRequestAt"`
	RequestCount  int       `json:"requestCount"`
	TotalTokens   int64     `json:"totalTokens"`   // Оценочное количество токенов
	NumCtx        int       `json:"numCtx"`        // Размер контекста запроса (токенов)
	SessionWeight float64   `json:"sessionWeight"` // Вес сессии для адаптивного переключения
}

// HealthCheckResult - результат проверки здоровья
type HealthCheckResult struct {
	BackendID string        `json:"backendId"`
	Healthy   bool          `json:"healthy"`
	Latency   time.Duration `json:"latency"`
	Error     string        `json:"error,omitempty"`
	Timestamp time.Time     `json:"timestamp"`
}

// Prediction - прогноз критического состояния бэкенда
type Prediction struct {
	SecondsToCritical float64 `json:"secondsToCritical"` // Секунд до критического состояния (-1 = нет данных, +Inf = не определено)
	CriticalReason    string  `json:"criticalReason"`    // Причина: "gpu_usage", "vram", "ram", "disk", "concurrent_requests", "models_capacity", "none"
	GPUUsageTrend     float64 `json:"gpuUsageTrend"`     // Тренд загрузки GPU (% в минуту, >0 — рост)
	VRAMUsageTrend    float64 `json:"vramUsageTrend"`    // Тренд использования VRAM (% в минуту)
	RAMUsageTrend     float64 `json:"ramUsageTrend"`     // Тренд использования RAM (% в минуту)
	FreeSlotsTrend    float64 `json:"freeSlotsTrend"`    // Тренд свободных слотов (слотов в минуту, <0 — уменьшение)
	RequestCapacity   float64 `json:"requestCapacity"`   // Текущая ёмкость запросов (0-100%, 100% = полная загрузка)
}

// MetricsSnapshot - точка истории метрик для прогнозирования
type MetricsSnapshot struct {
	Timestamp         time.Time `json:"timestamp"`
	GPUUsagePercent   float64   `json:"gpuUsagePercent"`
	VRAMUsagePercent  float64   `json:"vramUsagePercent"`
	RAMUsagePercent   float64   `json:"ramUsagePercent"`
	ActiveRequests    int       `json:"activeRequests"`
	RunningModels     int       `json:"runningModels"`
	FreeSlots         int       `json:"freeSlots"`
	RequestsPerSecond float64   `json:"requestsPerSecond"`
}

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
	Backends        []BackendMetrics `json:"backends"`
}