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
	// === Round 37 (2026-08-18): auto-adapt n_ctx 3-tier fields ===
	// Populated from /api/models → feasible_max_context / gguf_max_context.
	// 0 = unknown (cppworker didn't report — pre-Round 37 build).
	// Used by preflight resolveModelMaxContext v2 for auto-relax when
	// profile is conservative.
	MaxFeasibleContext int `json:"maxFeasibleContext"`
	GGUFMaxContext     int `json:"ggufMaxContext"`
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
	// === Capabilities (Round 18 P0.1, 2026-08-03) ===
	// Single source of truth для auto-detect (vision, tools, reasoning).
	// Прокидывается клиенту через X-Model-Capabilities, X-Model-Max-Context,
	// X-Model-Architecture headers.
	Capabilities *ModelCapabilities `json:"capabilities,omitempty"`
	// === Runtime params (Round 34, 2026-08-12) — для profile mismatch detection ===
	// cppworker сообщает в /api/models актуальные параметры загруженной модели
	// (Round 26 profile sync). Balancer использует для сравнения с client request
	// в preflight: если client просит kvCacheType=q8_0, а модель загружена с
	// f16, → reload на нужный params (как в n_ctx случае).
	// Пустая строка / 0 = неизвестно.
	KvCacheType     string `json:"kvCacheType,omitempty"`
	FlashAttnType   int    `json:"flashAttnType,omitempty"` // -1=auto, 0=off, 1=on
	UseMmap         bool   `json:"useMmap,omitempty"`
	// === Round 37 (2026-08-18): per-model feasible/GGUF max context ===
	// Заполняется из enriched /api/models per-model `feasible_max_context` /
	// `gguf_max_context` (см. cmd/cppworker/handlers_model.go Round 37 changes).
	// Используется balancer'ом для 3-tier resolution: profile vs feasible vs GGUF.
	FeasibleMaxContext int `json:"feasibleMaxContext,omitempty"`
	GGUFMaxContext     int `json:"ggufMaxContext,omitempty"`
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
