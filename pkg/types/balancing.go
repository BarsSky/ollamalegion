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
	Algorithm            BalancingAlgorithm `json:"algorithm"`
	ModelAffinity        bool               `json:"modelAffinity"`
	SessionStickiness    bool               `json:"sessionStickiness"`
	HealthCheckInterval  int                `json:"healthCheckInterval"`  // секунды
	MetricsInterval      int                `json:"metricsInterval"`      // секунды
	RequestTimeout       int                `json:"requestTimeout"`       // секунды
	FirstByteTimeout     int                `json:"firstByteTimeout"`     // таймаут первого байта streaming (сек, 0=дефолт 30)
	StreamingIdleTimeout int                `json:"streamingIdleTimeout"` // таймаут простоя между чанками streaming (сек, 0=дефолт 120)
	StreamTimeout        int                `json:"streamTimeout"`        // общий таймаут streaming запроса (сек, 0=дефолт 600)
	QueueTimeout         int                `json:"queueTimeout"`         // секунды
	QueueMaxSize         int                `json:"queueMaxSize"`         // макс. размер очереди
	QueueWorkers         int                `json:"queueWorkers"`         // количество workers очереди
	SessionTTL           int                `json:"sessionTTL"`           // секунды (0 = дефолт 900)
	SessionIdleTTL       int                `json:"sessionIdleTTL"`       // секунды простоя до удаления сессии (0 = дефолт 300 / 5 мин)

	// Новые поля оптимизации балансировки
	Prewarm             PrewarmConfig             `json:"prewarm"`
	ModelInstances      ModelInstanceConfig       `json:"modelInstances"`
	Scoring             ScoringWeights            `json:"scoring"`
	SyncModelLoad       SyncModelLoadConfig       `json:"syncModelLoad"`
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

	// NCtxReload — фич-флаги для adaptive n_ctx auto-reload (cppworker).
	// При CGo-bridge ErrNCtxNeedsReload балансер может перезагрузить модель
	// на бэкенде с большим n_ctx (если VRAM позволяет), или вернуть 413
	// клиенту с подробным JSON. По умолчанию выключено (AutoReloadNCtx=false).
	NCtxReload NCtxReloadSettings `json:"nctxReload"`

	// PreflightSyncEnabled — включает синхронное ожидание reload в preflight
	// (вместо немедленного HTTP 503 + Retry-After). При включении балансер
	// ждёт завершения reload до PreflightSyncTimeoutMs (default 60s), затем
	// проксирует запрос. При таймауте fallback на async с Retry-After: 15.
	// Дефолт: true (клиент не получает EOF при reload).
	PreflightSyncEnabled bool `json:"preflightSyncEnabled"`

	// PreflightSyncTimeoutMs — таймаут синхронного ожидания reload в preflight.
	// Default 60000 (60s). Max 180000 (180s). Если reload не успел —
	// fallback на async 503 + Retry-After: 15.
	PreflightSyncTimeoutMs int `json:"preflightSyncTimeoutMs"`

	// OperatingMode — текущий вариант работы балансера для метрик и UI
	OperatingMode string `json:"operatingMode"` // "standard"|"replication"|"rpc_coordinator"|"virtual_router"|"distributed_inference"
}

// NCtxReloadSettings — фич-флаги и лимиты для n_ctx auto-reload (см.
// internal/balancer/nctx_reload.go). Все bool-флаги имеют безопасный default
// (false = выключено), чтобы фича не активировалась случайно.
//
// Поля и JSON-теги синхронизированы с balancer.NCtxReloadConfig (internal/balancer).
// Конвертация в balancer.NCtxReloadConfig — в balancer-пакете (см.
// nctx_reload_config_bridge.go), чтобы избежать циклического импорта
// (pkg/types не должен импортировать internal/*).
type NCtxReloadSettings struct {
	// AutoReloadNCtx — глобальный kill-switch. Если false, balancer
	// НЕ пытается auto-reload и сразу возвращает 413 клиенту.
	AutoReloadNCtx bool `json:"auto_reload_n_ctx" yaml:"auto_reload_n_ctx"`

	// AutoReloadMaxNCtx — верхний предел n_ctx, до которого balancer
	// имеет право auto-reload. Защита от абсурдных значений
	// (например, клиент прислал num_ctx=10000000). 0 = без лимита.
	AutoReloadMaxNCtx int `json:"auto_reload_max_n_ctx" yaml:"auto_reload_max_n_ctx"`

	// AutoReloadVRAMSafetyFactor — доля от max_vram_n_ctx, которую мы
	// готовы занять KV-cache. 0.85 = 85% от теоретического максимума.
	// 0 = использовать default 0.85.
	AutoReloadVRAMSafetyFactor float64 `json:"auto_reload_vram_safety_factor" yaml:"auto_reload_vram_safety_factor"`

	// AutoReloadTimeoutSec — таймаут на сам HTTP reload-запрос.
	// cppworker может грузить модель 30-60 секунд. 0 = default 60.
	AutoReloadTimeoutSec int `json:"auto_reload_timeout_sec" yaml:"auto_reload_timeout_sec"`
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

// LlamaCppModelProfile — per-model профиль параметров загрузки (Шаг 5).
// Позволяет задать contextLength/batchSize/gpuLayers/etc для конкретной модели,
// переопределяя per-backend default (state.Backend.CppWorkerConfig.ContextLength).
//
// Приоритет (3-tier resolver для n_ctx):
//  1. Per-request (body num_ctx / X-Cpp-Ctx header)
//  2. Per-model profile (этот struct)
//  3. Per-backend default (cppworker CppWorkerConfig.ContextLength)
//
// Таймауты (поля StreamingTimeoutSec / StreamingIdleTimeoutSec / RequestTimeoutSec):
//  - Если значение > 0 — используется для этой модели (override глобального).
//  - Если 0 — используется глобальное значение из BalancingSettings.
//  - autoAdjust=true: если таймауты не заданы (0), балансировщик вычисляет
//    их автоматически на основе NumGPULayers, SizeBytes и истории генерации
//    (через ModelLatencyTracker).
type LlamaCppModelProfile struct {
	ContextLength int    `json:"contextLength"`       // n_ctx ∈ [256, 262144]
	BatchSize     int    `json:"batchSize"`           // n_batch ∈ [1, 2048]
	NumGPULayers  int    `json:"numGpuLayers"`        // -1 = все слои, 0 = CPU, N = N слоёв на GPU
	FlashAttn     *bool  `json:"flashAttn,omitempty"` // nil = не менять, true/false = установить
	NUMA          *bool  `json:"numa,omitempty"`      // nil = не менять
	UseMmap       *bool  `json:"useMmap,omitempty"`   // nil = не менять
	Notes         string `json:"notes,omitempty"`     // человеческое описание (для WebUI/API)

	// Per-model таймауты. 0 = использовать глобальные значения из BalancingSettings.
	// Позволяют задать бóльшие таймауты для тяжёлых моделей (CPU offload) и
	// меньшие — для лёгких (GPU-only, fast).
	//
	// StreamingTimeoutSec — общий таймаут streaming-запроса (сек).
	//   Если 0 — используется BalancingSettings.StreamTimeout (default 600).
	//   Для CPU-моделей рекомендуется 1800+ (30 мин).
	StreamingTimeoutSec int `json:"streamingTimeoutSec,omitempty"`

	// StreamingIdleTimeoutSec — таймаут простоя между чанками streaming (сек).
	//   Если 0 — используется BalancingSettings.StreamingIdleTimeout (default 120).
	//   Для CPU-моделей с partial offload рекомендуется 300+.
	StreamingIdleTimeoutSec int `json:"streamingIdleTimeoutSec,omitempty"`

	// RequestTimeoutSec — таймаут non-streaming запроса (сек).
	//   Если 0 — используется BalancingSettings.RequestTimeout (default 120).
	//   Для CPU-моделей рекомендуется 300+.
	RequestTimeoutSec int `json:"requestTimeoutSec,omitempty"`

	// FirstByteTimeoutSec — таймаут ожидания первого байта ответа (сек).
	//   Если 0 — используется BalancingSettings.FirstByteTimeout (default 120).
	//   Для CPU-моделей с partial offload / RAM fallback рекомендуется 600+.
	//   Учитывает время загрузки модели в VRAM + prompt processing.
	FirstByteTimeoutSec int `json:"firstByteTimeoutSec,omitempty"`

	// SizeBytes — размер файла модели в байтах (для автоматического расчёта
	// таймаутов, если таймауты не заданы явно и autoAdjust=true).
	// Заполняется автоматически при сканировании моделей.
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// Parallel — число параллельных слотов для одновременной обработки запросов
	// (n_parallel в llama.cpp). 0 = использовать дефолт cppworker (обычно 1).
	// >0 = до 8 параллельных слотов в одном экземпляре модели. Полезно для
	// multi-user throughput без репликации модели на N бэкендов.
	//
	// Trade-off: каждый слот потребляет отдельный KV-cache (n_parallel × KV-cache per slot),
	// так что n_ctx × n_parallel должно влезать в VRAM. Например, gemma-4 8B с
	// n_ctx=65536 + parallel=2 + kv_cache_type=f16 требует ~14 GB на KV-cache
	// (было бы 7 GB при parallel=1).
	Parallel int `json:"parallel,omitempty"` // 0 = inherit, 1..8 = parallel slots

	// KVCacheType — тип квантизации KV-cache. "" (или отсутствие поля) = inherit
	// from cppworker default (обычно f16).
	//
	// Поддерживаемые значения (Session 16, 2026-06-27):
	//   "f16"  — полная точность (по умолчанию). Нет потери качества.
	//   "q8_0" — 8-bit квантизация. Экономит ~50% VRAM на KV-cache.
	//            Минимальная деградация качества (perplexity +0.1-0.3).
	//            **Рекомендуется для длинных контекстов** (gemma-4 256K).
	//   "q4_0" — 4-bit квантизация. Экономит ~75% VRAM на KV-cache.
	//            Лёгкая деградация (perplexity +1-2%). Только для очень
	//            длинных контекстов или VRAM-constrained среды.
	//
	// Пример: gemma-4 8B + n_ctx=65536 + kv_cache_type=q8_0 экономит ~3.5 GB VRAM
	// на KV-cache (7 GB → 3.5 GB) — позволяет загрузить модель с большим n_ctx
	// на 8GB GPU.
	KVCacheType string `json:"kvCacheType,omitempty"` // "" = inherit, "f16"/"q8_0"/"q4_0"

	// Round 7 (2026-07-09): per-tensor override для MoE моделей.
	// Применяется при load/reload если массивы непустые и согласованы по длине.
	// Используется для роутинга routed-expert тензоров (blk.N.ffn_*.exps.weight/bias)
	// на CPU/CUDA_Host вместо VRAM. Это освобождает VRAM для KV-cache.
	//
	// Поддерживаемые buft: "CPU" (= CUDA_Host pinned memory на CUDA-бэкендах),
	// "CUDA0"/"CUDA1" — конкретный GPU. Невалидные значения игнорируются с warn.
	//
	// Длина OverrideTensorBufts должна совпадать с длиной OverrideTensors.
	// Пример для Qwen3.6-35B-A3B / Mixtral:
	//   OverrideTensors:     ["blk\\.\\d+\\.ffn_.*_exps\\.weight", "blk\\.\\d+\\.ffn_.*_exps\\.bias"]
	//   OverrideTensorBufts: ["CPU", "CPU"]
	OverrideTensors     []string `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string `json:"overrideTensorBufts,omitempty"`
}

// AdvancedTimingConfig — конфигурируемые таймауты
type AdvancedTimingConfig struct {
	ZombieSessionThresholdSec int `json:"zombieSessionThresholdSec"` // порог зомби-сессий, сек (default: 120)
	StreamingRetryDelayMs     int `json:"streamingRetryDelayMs"`     // задержка перед retry streaming, мс (default: 500)
	MaxConcurrentWarmups      int `json:"maxConcurrentWarmups"`      // макс. одновременных warmup (default: 3)
	HeartbeatIntervalSec      int `json:"heartbeatIntervalSec"`      // интервал SSE heartbeat, сек (default: 15)
	WarmupSemaphoreTimeoutSec int `json:"warmupSemaphoreTimeoutSec"` // таймаут ожидания семафора warmup, сек (default: 30)
}
