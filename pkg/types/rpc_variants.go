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

// RpcCoordinatorConfig - конфигурация внешнего RPC координатора
type RpcCoordinatorConfig struct {
	Enabled        bool   `json:"enabled"`
	CoordinatorURL string `json:"coordinatorURL"`
	WorkerPort     int    `json:"workerPort"`
	Timeout        string `json:"timeout"`    // "30s"
	Protocol       string `json:"protocol"`   // "http" | "grpc"
	MaxRetries     int    `json:"maxRetries"`
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

// VirtualModelConfig - конфигурация виртуальной модели
type VirtualModelConfig struct {
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	Slices       []ModelSliceConfig  `json:"slices"`
	Coordination CoordinationConfig  `json:"coordination"`
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