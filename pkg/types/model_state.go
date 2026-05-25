package types

import "time"

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

// ModelDetails - детали модели из /api/tags
type ModelDetails struct {
	Family        string `json:"family"`
	Format        string `json:"format"`
	ParameterSize string `json:"parameterSize"`
	Quantization  string `json:"quantization"`
}