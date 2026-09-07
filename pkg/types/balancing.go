package types

// BalancingAlgorithm - алгоритмы балансировки
type BalancingAlgorithm string

const (
	AlgorithmRoundRobin    BalancingAlgorithm = "roundrobin"
	AlgorithmLeastConn     BalancingAlgorithm = "leastconn"
	AlgorithmResourceAware BalancingAlgorithm = "resource-aware"
	AlgorithmModelAffinity BalancingAlgorithm = "model-affinity"
)

// OperatingMode — режим работы балансировщика. Определяет, как balancer
// обрабатывает inference-запросы (стандартный прокси vs coordinator pipeline
// vs virtual router и т.д.). Phase 8 (P.1): добавлены константы для
// type-safety в OperatingModeRpcCoordinator check (вместо magic string).
// Реальные значения остаются string для backward-compatible JSON config.
type OperatingMode string

const (
	// OperatingModeStandard — обычный прокси-режим (default).
	OperatingModeStandard OperatingMode = "standard"
	// OperatingModeReplication — через model replication manager.
	OperatingModeReplication OperatingMode = "replication"
	// OperatingModeRpcCoordinator — через external/embedded RPC coordinator
	// (Plan §P.1). Inference маршрутизируется через ModelCoordinator.
	OperatingModeRpcCoordinator OperatingMode = "rpc_coordinator"
	// OperatingModeVirtualRouter — через virtual model router.
	OperatingModeVirtualRouter OperatingMode = "virtual_router"
	// OperatingModeDistributedInference — через custom distributed engine.
	OperatingModeDistributedInference OperatingMode = "distributed_inference"
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
	// R54.2 (2026-08-24): AutoTune master switch. Если true (default), balancer
	// auto-detects sub-optimal state (q4_0 KV cache on small model, over-allocated
	// n_ctx) и рекомендует fix. R54.4 добавит авто-применение. Manual override:
	// per-model profile "autoTune": false отключает для конкретной модели.
	AutoTune             bool               `json:"autoTune"`             // default true
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

	// PreflightEnabled — включает preflight n_ctx check (Round 14+).
	// При true balancer ДО отправки запроса проверяет, хватает ли loaded n_ctx,
	// и при необходимости делает reload (sync или async — см. PreflightAsyncReload).
	PreflightEnabled bool `json:"preflight_enabled" yaml:"preflight_enabled"`

	// PreflightAsyncReload (Round 31 #2, 2026-08-09): async mode для reload.
	// При true balancer НЕ блокирует на reload — сразу отдаёт клиенту
	// 503 + Retry-After, а reload запускается в фоне. Клиент (Cline/OpenWebUI)
	// повторяет через Retry-After секунд и получает уже готовую модель.
	// Default false (sync reload — обратная совместимость).
	PreflightAsyncReload bool `json:"preflight_async_reload" yaml:"preflight_async_reload"`

	// PreflightAsyncRetryAfterSec (Round 31 #2): сколько секунд balancer
	// рекомендует клиенту ждать перед retry после 503. Default 5.
	PreflightAsyncRetryAfterSec int `json:"preflight_async_retry_after_sec" yaml:"preflight_async_retry_after_sec"`

	// R60.6 (2026-09-07): max clamp для PreflightAsyncRetryAfterSec.
	// Раньше было hardcoded 30s в effectiveAsyncRetryAfter balancer'а.
	// 30s мало для больших моделей (5GB+131072 n_ctx load = 60-180s),
	// client retry-ил раньше чем reload заканчивался → cascading reloads.
	// Default 120s покрывает realistic hardware. Operator может
	// переопределить через config.json или ENV.
	PreflightAsyncRetryAfterMaxSec int `json:"preflight_async_retry_after_max_sec" yaml:"preflight_async_retry_after_max_sec"`

	// PreflightMaxWaitSec (Round 35c, 2026-08-13): верхняя граница polling
	// таймаута для async load (cppworker вернул 202 Accepted, balancer
	// опрашивает /api/models пока state != "loaded"). По умолчанию 900 (15 min).
	//
	// Раньше был hardcoded cap=15min в internal/balancer/model_management.go
	// (`maxWait = 2*est + 60s`, max 15min). Для моделей с auto-offload
	// (например, Qwen3.6-35B-A3B ~22GB на 8GB VRAM 3070) фактическая
	// загрузка занимает 12-15 min из-за CUDA_Host pinned memory allocation
	// для 19GB+ весов. 2*est (3.5min) + 60s = 8min — слишком короткий.
	// Пользователь увидит `async load (202) on cppworker: model not ready
	// after 8m7s` хотя модель догружается через 1-2 минуты.
	//
	// Пример: на A10 (24GB VRAM, full GPU offload) 22GB Qwen3.6 грузится
	// ~3-5 мин, 2*est + 60s = ~5-6 min — fits в 15min cap. Поэтому
	// 900s default покрывает A10. Для 3070 или больших моделей (70B+)
	// поднимите через LB_NCTX_PREFLIGHT_MAX_WAIT_SEC=1800 (30 min) или
	// даже 3600 (1 hour). Hard cap = 3600s (1 hour) для защиты от
	// зависших загрузок.
	PreflightMaxWaitSec int `json:"preflight_max_wait_sec" yaml:"preflight_max_wait_sec"`

	// PreflightWaitMultiplier (Round 35c): множитель для оценки времени
	// загрузки (`maxWait = multiplier*est + buffer`). Default 2.
	//
	// При 2x оценка cppworker'а (est) часто слишком оптимистична для
	// моделей с auto-offload (CUDA_Host alloc занимает 5-10 min на 19GB).
	// Поднимите до 3-4 для больших моделей через LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER.
	PreflightWaitMultiplier int `json:"preflight_wait_multiplier" yaml:"preflight_wait_multiplier"`

	// PreflightWaitBufferSec (Round 35c): дополнительный буфер после
	// `multiplier*est`. Default 60s. Увеличьте для очень тяжёлых моделей
	// или медленного I/O.
	PreflightWaitBufferSec int `json:"preflight_wait_buffer_sec" yaml:"preflight_wait_buffer_sec"`
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

	// Round 37 (2026-08-18): auto-adapt n_ctx profile schema.
	//
	// ПРЕДОТВРАЩАЕТ 2026-08-18 production bug:
	//   - profile.contextLength = 32768 (консервативный)
	//   - Cline шлёт num_ctx=65536
	//   - preflight 413 "exceeds model max context=32768"
	//   - Qwen3.6-35B на 8GB VRAM реально тянет 65536 через RAM + q4_0 KV
	//
	// ContextLengthAuto — true = auto-adapt (profile = HINT, feasible = real cap).
	//   Default false для backward compat (existing profiles continue to behave as hard caps).
	//   Round 37 fix: добавь `"contextLengthAuto": true` к профилю для auto-relax.
	//
	// ContextLengthMax — soft upper bound (operator policy) для auto-adapt mode.
	//   0 = unlimited up to GGUFMax.
	//   Example: `"contextLengthMax": 131072` — "я не хочу больше 128K даже если feasible".
	//
	// Precedence (resolveModelMaxContext v2):
	//   1. profile.contextLengthAuto=false → profile.contextLength (hard cap, old behavior)
	//   2. profile.contextLengthAuto=true  → min(profile.contextLength, profile.contextLengthMax, feasible)
	//   3. fallback → metrics (cppworker ModelMaxContext)
	ContextLengthAuto bool `json:"contextLengthAuto,omitempty"` // Round 37: opt-in auto-relax
	ContextLengthMax  int  `json:"contextLengthMax,omitempty"`  // Round 37: soft cap для auto mode (0=unlimited)

	// Disabled — Round 27 follow-up: помечает модель как сломанную (например, gemma-4
	// с upstream GGML_ASSERT на любом n_ctx >= 8192). Балансер ОТКАЗЫВАЕТСЯ авто-грузить
	// такие модели — executeLlamaCppLoad возвращает ошибку с объяснением, чтобы клиент
	// (Cline/OpenWebUI) не уходил в crash-loop.
	Disabled bool `json:"disabled,omitempty"`

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

	// R54.2 (2026-08-24): AutoTune override на per-model уровне.
	//   false = отключить AutoTune для этой модели (manual params).
	//   true или nil = наследовать global BalancingSettings.AutoTune.
	// Полезно для моделей где пользователь явно знает optimal params
	// (например, после ручной настройки и тестирования) и не хочет
	// авто-reload от AutoTune. Default nil = inherit global.
	AutoTune *bool `json:"autoTune,omitempty"`
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

	// MaxTokens — верхняя граница output tokens per request (Round 26, 2026-08-06).
	// 0 = no cap. Если клиент (Cline/OpenWebUI) запрашивает max_tokens=32000,
	// а профиль говорит 8192, то cppworker ограничит n_predict до 8192
	// (даже если клиент попросил больше). Это защищает GPU от runaway
	// generations (Cline иногда генерит 30K+ токенов за раз = 10+ минут).
	//
	// Soft cap: применяется в handler'е после env defaults, до AutoTuneNCtx.
	// Явный max_tokens в запросе с меньшим значением — побеждает (не повышаем).
	MaxTokens int `json:"maxTokens,omitempty"`

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
