package types

// LlamaCppMetrics - метрики llama.cpp бэкенда
type LlamaCppMetrics struct {
	// MaxVRAMNCtx — макс. n_ctx, помещающийся в VRAM (из /api/models cppworker).
	// Используется preflight_nctx для оценки возможности reload без round-trip.
	// 0 = неизвестно (poller ещё не опросил бэкенд).
	MaxVRAMNCtx     int    `json:"maxVramNCtx"`
	MaxRAMNCtx      int    `json:"maxRamNCtx"`
	AvailableVRAMMB uint64 `json:"availableVramMb"`
	TotalVRAMMB     uint64 `json:"totalVramMb"`
	ModelMaxContext int    `json:"modelMaxContext"`
	LoadedModels []LlamaCppModel `json:"loadedModels"`
	// LoadingModels — модели, которые сейчас в процессе загрузки (State="loading").
	// Заполняется из cppworker /api/models/load/progress (либо из notifyModelLoaded callback).
	// Используется UI (монитор, вкладка бэкендов, GGUF-таб) для отображения
	// спиннера и elapsed-time «Загружается model-name 25s».
	// При переходе State=loaded элемент перемещается в LoadedModels, при
	// State=error — остаётся здесь до следующего poll с error-полем.
	LoadingModels     []LlamaCppModel  `json:"loadingModels,omitempty"`
	AvailableModels   []LlamaCppModel  `json:"availableModels,omitempty"`
	ActiveRequests    int              `json:"activeRequests"`
	TotalRequests     int64            `json:"totalRequests"`
	AvgResponseTime   float64          `json:"avgResponseTime"` // ms
	RequestsPerSecond float64          `json:"requestsPerSecond"`
	FreeSlots         int              `json:"freeSlots"`
	AvailableSlots    int              `json:"availableSlots"`
	MaxModels         int              `json:"maxModels"`
	MaxConcurrentReqs int              `json:"maxConcurrentReqs"`
	GPUInfo           *LlamaCppGPUInfo `json:"gpuInfo,omitempty"`
}

// LlamaCppModel - информация о модели llama.cpp
type LlamaCppModel struct {
	Name          string `json:"name"`
	Path          string `json:"path,omitempty"`
	Size          uint64 `json:"size"`      // bytes
	VRAMUsage     uint64 `json:"vramUsage"` // MB
	RAMUsage      uint64 `json:"ramUsage"`  // MB
	ContextLength int    `json:"contextLength"`
	BatchSize     int    `json:"batchSize"`
	NumGPULayers  int    `json:"numGpuLayers"`
	Quantization  string `json:"quantization"`
	State         string `json:"state"` // "loaded", "loading", "error"
	// === Architecture metadata (Round 18+ — для оценки VRAM/RAM split по слоям) ===
	// cppworker reports per-model architecture details, используем для расчёта
	// estimatedVram/estimatedRam (cppworker не сообщает actual usage per-model).
	Architecture string `json:"architecture,omitempty"` // "gemma4", "qwen35moe", ...
	NLayers      int    `json:"nLayers,omitempty"`      // total layers in model
	NKvHeads     int    `json:"nKvHeads,omitempty"`     // n_kv_heads (для KV cache расчёта)
	NEmbd        int    `json:"nEmbd,omitempty"`        // n_embd (для KV cache)
	HeadDimK     int    `json:"headDimK,omitempty"`     // head_dim_k
	HeadDimV     int    `json:"headDimV,omitempty"`     // head_dim_v
	MaxContext   int    `json:"maxContext,omitempty"`  // ggufContextLength (макс n_ctx для этой модели)
	LoadedAt     string `json:"loadedAt,omitempty"`     // RFC3339Nano
	// === Loading state (Шаг «отображение загрузки в мониторе и вкладке бэкендов») ===
	// Заполняются только пока State == "loading" / "error". После успешной
	// загрузки поля обнуляются (omitempty).
	LoadingStartedAt *string `json:"loadingStartedAt,omitempty"` // RFC3339Nano
	LoadingSizeBytes int64   `json:"loadingSizeBytes,omitempty"` // ожидаемый размер модели
	LoadingError     string  `json:"loadingError,omitempty"`     // причина сбоя (если State="error")
}

// LlamaCppGPUInfo - информация о GPU для llama.cpp
type LlamaCppGPUInfo struct {
	Count     int                 `json:"count"`
	Devices   []LlamaCppGPUDevice `json:"devices"`
	TotalVRAM uint64              `json:"totalVram"` // MB
	FreeVRAM  uint64              `json:"freeVram"`  // MB
}

// LlamaCppGPUDevice - одно GPU-устройство
type LlamaCppGPUDevice struct {
	Name        string  `json:"name"`
	MemoryTotal uint64  `json:"memoryTotal"` // MB
	MemoryFree  uint64  `json:"memoryFree"`  // MB
	Utilization float64 `json:"utilization"` // %
}
