package types

import "time"

// ============================================================
// Вариант A: Model Replication Manager
// ============================================================

// ModelReplicationConfig - конфигурация репликации моделей
type ModelReplicationConfig struct {
	Enabled             bool                `json:"enabled"`
	DefaultMinInstances int                 `json:"defaultMinInstances"`
	DefaultMaxInstances int                 `json:"defaultMaxInstances"`
	IdleUnloadAfter     string              `json:"idleUnloadAfter"` // "10m"
	Groups              []ModelGroupConfig  `json:"groups"`
}

// ModelGroupConfig - конфигурация группы моделей
type ModelGroupConfig struct {
	ModelName       string   `json:"modelName"`
	MinInstances    int      `json:"minInstances"`
	MaxInstances    int      `json:"maxInstances"`
	TargetBackends  []string `json:"targetBackends"`
	IdleUnloadAfter string   `json:"idleUnloadAfter"`
}

// ModelInstanceState - состояние экземпляра модели на бэкенде
type ModelInstanceState struct {
	BackendID  string     `json:"backendId"`
	Status     ModelState `json:"status"`
	LoadedAt   time.Time  `json:"loadedAt"`
	LastUsedAt time.Time  `json:"lastUsedAt"`
	UseCount   int64      `json:"useCount"`
}

// ============================================================
// Вариант B: External RPC Coordinator
// ============================================================

// RpcCoordinatorConfig - конфигурация внешнего RPC координатора.
//
// Phase 8 (2026-07-10): P.1 — rpc_coordinator production mode. Расширено полями:
//   - Embedded       — true = поднять ModelCoordinator в balancer (in-process)
//   - Workers        — initial worker URLs для Embedded mode (если coordinator
//                      не получает их динамически через heartbeat)
//   - FailoverPolicy — "circuit_breaker" | "retry" | "fail_fast"
//   - RequestTimeout — non-streaming inference timeout (default 30s)
//   - StreamTimeout  — streaming inference timeout (default 5min)
//
// Legacy поля (WorkerPort, Protocol, MaxRetries, Timeout) сохранены для
// backward-compatible JSON config.
type RpcCoordinatorConfig struct {
	// Enabled — true если rpc_coordinator mode активен. Phase 8: также
	// требует cfg.Balancing.OperatingMode == "rpc_coordinator".
	Enabled bool `json:"enabled"`

	// CoordinatorURL — URL внешнего coordinator (для external mode).
	// Игнорируется если Embedded=true. Default: "" (только Embedded mode).
	CoordinatorURL string `json:"coordinatorURL"`

	// Embedded — true = поднять ModelCoordinator в balancer (in-process).
	// Phase 8.5+ используется в cmd/balancer/main.go для activation logic.
	// Default: false (external mode).
	Embedded bool `json:"embedded"`

	// Workers — initial worker URLs для Embedded mode. Workers обычно
	// регистрируются динамически через heartbeat, но можно pre-register.
	// Default: nil (dynamic registration).
	Workers []string `json:"workers"`

	// FailoverPolicy — как обрабатывать worker failures.
	// "circuit_breaker" (default) — открыть breaker на N failures, half-open
	//                              после resetTimeout.
	// "retry" — retry до MaxRetries на каждого worker.
	// "fail_fast" — сразу 503 без retry.
	FailoverPolicy string `json:"failoverPolicy"`

	// RequestTimeout — non-streaming inference timeout. Default 30s.
	RequestTimeout time.Duration `json:"requestTimeout"`

	// StreamTimeout — streaming inference timeout. Default 5min.
	StreamTimeout time.Duration `json:"streamTimeout"`

	// === Legacy fields (Phase 8: keep для backward compat) ===
	WorkerPort int    `json:"workerPort"`  // (legacy) port for rpcworker discovery
	Timeout    string `json:"timeout"`     // (legacy) "30s" — superseded by RequestTimeout
	Protocol   string `json:"protocol"`    // (legacy) "http" | "grpc"
	MaxRetries int    `json:"maxRetries"`  // (legacy) для FailoverPolicy="retry"

	// CircuitBreaker — настройки per-worker circuit breaker (Phase 8 Session 3.3).
	// Применяется в dispatcher'е: накапливает failure/success по каждому worker'у
	// и skip'ает workers с Open breaker (fail-fast вместо cascade failures).
	CircuitBreaker CircuitBreakerConfig `json:"circuitBreaker"`
}

// CircuitBreakerConfig — параметры circuit breaker (per-worker).
// Phase 8 Session 3.3: defaults применяются если значения = 0.
type CircuitBreakerConfig struct {
	// FailureThreshold — число failures перед Open. Default 5.
	FailureThreshold int `json:"failureThreshold"`
	// SuccessThreshold — successes в HalfOpen для перехода в Closed. Default 1.
	SuccessThreshold int `json:"successThreshold"`
	// ResetTimeoutMs — время в Open перед HalfOpen probe (ms). Default 30000.
	ResetTimeoutMs int `json:"resetTimeoutMs"`
}

// RpcWorkerConfig - конфигурация RPC worker'а
type RpcWorkerConfig struct {
	WorkerID    string `json:"workerId"`
	BackendID   string `json:"backendId"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	SliceLayers string `json:"sliceLayers"` // "1-40"
}

// ============================================================
// Вариант C: Virtual Model Router
// ============================================================

// VirtualModelsConfig - конфигурация виртуальных моделей
type VirtualModelsConfig struct {
	Enabled bool                 `json:"enabled"`
	Models  []VirtualModelConfig `json:"models"`
}

// VirtualModelConfig - конфигурация виртуальной модели.
//
// Phase 8 P.2 (2026-07-11): добавлены 3 поля для alias-on-pool mode (NEW):
//   - Selection   — стратегия выбора backend'а из BackendPool.
//   - BackendPool — список real backends (host:port strings).
//   - ModelName   — physical model name (например, "llama-70b"), общий для всех backends.
//
// Alias-on-pool mode (новый, простой): virtual model = алиас на пул
// backends с одной и той же физической моделью. balancer выбирает
// backend через Selection, делает обычный proxy request к выбранному.
//
// Pipeline mode (legacy): Slices + Coordination = multi-step pipeline
// через несколько бэкендов с разными срезами. Использует ExecutePipeline
// (см. internal/virtualmodel/virtual_model.go).
//
// Какой режим активен:
//   - Если BackendPool != nil && ModelName != "" → alias-on-pool mode.
//   - Иначе → pipeline mode (legacy, Slices + Coordination).
type VirtualModelConfig struct {
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	Slices       []ModelSliceConfig  `json:"slices"`
	Coordination CoordinationConfig  `json:"coordination"`

	// === Alias-on-pool mode (Phase 8 P.2) ===
	Selection   SelectionStrategy `json:"selection"`   // "round_robin" | "least_loaded" | "random"
	BackendPool []string          `json:"backendPool"` // pool of "host:port" strings
	ModelName   string            `json:"modelName"`   // physical model on those backends
}

// SelectionStrategy — алгоритм выбора backend'а из пула.
// Phase 8 P.2.
type SelectionStrategy string

const (
	// SelectionRoundRobin — atomic counter, инкремент на каждый Select.
	SelectionRoundRobin SelectionStrategy = "round_robin"
	// SelectionLeastLoaded — выбирает backend с max FreeSlots (least busy).
	SelectionLeastLoaded SelectionStrategy = "least_loaded"
	// SelectionRandom — math/rand (для тестов и stress testing).
	SelectionRandom SelectionStrategy = "random"
)

// IsAliasOnPoolMode — true если config в alias-on-pool mode (Phase 8 P.2).
// В pipeline mode (legacy) Selection/BackendPool/ModelName игнорируются.
func (c *VirtualModelConfig) IsAliasOnPoolMode() bool {
	return c != nil && len(c.BackendPool) > 0 && c.ModelName != ""
}

// ModelSliceConfig - конфигурация среза модели
type ModelSliceConfig struct {
	ID             string   `json:"id"`
	ModelName      string   `json:"modelName"`
	Ordinal        int      `json:"ordinal"`
	TargetBackends []string `json:"targetBackends"`
	FallbackMode   string   `json:"fallbackMode"` // "retry" | "skip" | "abort"
}

// CoordinationConfig - конфигурация координации pipeline
type CoordinationConfig struct {
	Mode         string `json:"mode"`         // "sequential" | "parallel" | "tree"
	TimeoutMs    int    `json:"timeoutMs"`
	SyncStrategy string `json:"syncStrategy"` // "http-callback" | "direct-response"
}

// ============================================================
// Вариант D: Distributed Inference (Custom Backend)
// ============================================================

// DistInferenceConfig - конфигурация распределённого inference
type DistInferenceConfig struct {
	Enabled  bool               `json:"enabled"`
	GrpcPort int                `json:"grpcPort"`
	Workers  []DistWorkerConfig `json:"workers"`
}

// DistWorkerConfig - конфигурация worker'а распределённого inference
type DistWorkerConfig struct {
	WorkerID      string `json:"workerId"`
	Host          string `json:"host"`
	GrpcPort      int    `json:"grpcPort"`
	LayerRange    string `json:"layerRange"`    // "1-40"
	GPUMode       string `json:"gpuMode"`       // "auto" | "gpu" | "cpu"
	MaxBatchSize  int    `json:"maxBatchSize"`
	KVCacheSizeMB int    `json:"kvCacheSizeMB"`
}