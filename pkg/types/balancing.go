package types

// BalancingAlgorithm - алгоритмы балансировки
type BalancingAlgorithm string

const (
	AlgorithmRoundRobin    BalancingAlgorithm = "roundrobin"
	AlgorithmLeastConn     BalancingAlgorithm = "leastconn"
	AlgorithmResourceAware BalancingAlgorithm = "resource-aware"
	AlgorithmModelAffinity BalancingAlgorithm = "model-affinity"
)

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
	StreamTimeout        int                     `json:"streamTimeout"`        // общий таймаут streaming запроса (сек, 0=дефолт 600)
	QueueTimeout         int                     `json:"queueTimeout"`         // секунды
	QueueMaxSize         int                     `json:"queueMaxSize"`         // макс. размер очереди
	QueueWorkers         int                     `json:"queueWorkers"`         // количество workers очереди
	SessionTTL           int                     `json:"sessionTTL"`           // секунды (0 = дефолт 900)
	SessionIdleTTL       int                     `json:"sessionIdleTTL"`       // секунды простоя до удаления сессии (0 = дефолт 300 / 5 мин)

	// Новые поля оптимизации балансировки
	Prewarm             PrewarmConfig            `json:"prewarm"`
	ModelInstances      ModelInstanceConfig      `json:"modelInstances"`
	Scoring             ScoringWeights           `json:"scoring"`
	SyncModelLoad       SyncModelLoadConfig      `json:"syncModelLoad"`
	ResourceReservation ResourceReservationConfig `json:"resourceReservation"`

	// Feature flags
	UseEnhancedScoring bool `json:"useEnhancedScoring"` // Расширенный скоринг v2 (полная формула)
	ModelLoadTimeout   int  `json:"modelLoadTimeout"`   // Таймаут ожидания загрузки модели (сек, default 120)

	// Streaming защита
	StreamingMaxDuration int `json:"streamingMaxDuration"` // Макс. длительность streaming-запроса (сек, 0=без ограничения)

	// AutoPull - автоматическая загрузка модели при запросе (Pull-on-Demand)
	AutoPull AutoPullConfig `json:"autoPull"`

	// ModelReplication (Вариант A) - репликация модели на несколько бэкендов
	ModelReplication ModelReplicationConfig `json:"modelReplication"`

	// RpcCoordinator (Вариант B) - внешний RPC координатор
	RpcCoordinator RpcCoordinatorConfig `json:"rpcCoordinator"`

	// VirtualModels (Вариант C) - виртуальные модели с pipeline slicing
	VirtualModels VirtualModelsConfig `json:"virtualModels"`

	// DistInference (Вариант D) - распределённый inference через кастомный бэкенд
	DistInference DistInferenceConfig `json:"distInference"`

	// AdvancedTiming — тонкая настройка таймаутов и интервалов.
	AdvancedTiming AdvancedTimingConfig `json:"advancedTiming"`

	// OperatingMode — текущий вариант работы балансера для метрик и UI
	OperatingMode string `json:"operatingMode"` // "standard"|"replication"|"rpc_coordinator"|"virtual_router"|"distributed_inference"
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

// AutoPullConfig - конфигурация автоматической загрузки модели по запросу
type AutoPullConfig struct {
	Enabled       bool   `json:"enabled"`       // Включить авто-загрузку модели, если её нет
	MaxConcurrent int    `json:"maxConcurrent"` // Макс. одновременных загрузок (0 = без лимита)
	PullTimeout   string `json:"pullTimeout"`   // Таймаут на загрузку модели ("5m", "10m")
	RetryCount    int    `json:"retryCount"`    // Сколько раз повторить запрос после загрузки
}

// AdvancedTimingConfig — конфигурируемые таймауты
type AdvancedTimingConfig struct {
	ZombieSessionThresholdSec int `json:"zombieSessionThresholdSec"` // порог зомби-сессий, сек (default: 120)
	StreamingRetryDelayMs     int `json:"streamingRetryDelayMs"`     // задержка перед retry streaming, мс (default: 500)
	MaxConcurrentWarmups      int `json:"maxConcurrentWarmups"`      // макс. одновременных warmup (default: 3)
	HeartbeatIntervalSec      int `json:"heartbeatIntervalSec"`      // интервал SSE heartbeat, сек (default: 15)
	WarmupSemaphoreTimeoutSec int `json:"warmupSemaphoreTimeoutSec"` // таймаут ожидания семафора warmup, сек (default: 30)
}