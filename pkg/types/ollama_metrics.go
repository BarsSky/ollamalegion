package types

import "time"

// OllamaMetrics - метрики Ollama
type OllamaMetrics struct {
	RunningModels         []RunningModel     `json:"runningModels"`         // Запущенные (загруженные в память) модели
	AvailableModels       []RunningModel     `json:"availableModels"`       // Доступные модели (все, что можно загрузить)
	ActiveRequests        int                `json:"activeRequests"`        // Активные запросы (от балансировщика — точные)
	TotalRequests         int64              `json:"totalRequests"`         // Всего запросов (от балансировщика)
	AvgResponseTime       float64            `json:"avgResponseTime"`       // Среднее время ответа (ms)
	RequestsPerSecond     float64            `json:"requestsPerSecond"`     // RPS (от балансировщика)
	MaxModels             int                `json:"maxModels"`             // Максимум доступных для загрузки моделей (-1 = авто)
	MaxConcurrentRequests int                `json:"maxConcurrentRequests"` // Максимум одновременных запросов (-1 = авто)
	FreeSlots             int                `json:"freeSlots"`             // Свободные слоты для запросов (от балансера)
	AvailableSlots        int                `json:"availableSlots"`        // Реальные доступные слоты (от агента, с учётом VRAM)
	RuntimeFlags          OllamaRuntimeFlags `json:"runtimeFlags"`          // Флаги запуска Ollama
	ModelContexts         []ModelContextInfo `json:"modelContexts"`         // Информация о контексте по моделям
	BackendCapacity       BackendCapacity    `json:"backendCapacity"`       // Оценка ёмкости бэкенда
	ModelSizes            map[string]int64   `json:"modelSizes"`            // Размеры всех доступных моделей (modelName → bytes)
	OllamaAvailable       bool               `json:"ollamaAvailable"`       // Доступность Ollama API на бэкенде (от агента)
}

// OllamaRuntimeFlags - флаги запуска процесса Ollama
type OllamaRuntimeFlags struct {
	NumGPULayers    int    `json:"numGpuLayers"`    // Количество слоёв на GPU (-ngl, --num-gpu-layers)
	ContextLength   int    `json:"contextLength"`   // Размер контекста (-c, --ctx-size)
	NumParallel     int    `json:"numParallel"`     // Параллельных запросов (-np, --parallel)
	NumThreads      int    `json:"numThreads"`      // Потоков CPU (-t, --threads)
	BatchSize       int    `json:"batchSize"`       // Размер батча (-b, --batch-size)
	GPUSplitMode    string `json:"gpuSplitMode"`    // Режим разделения GPU (--split-mode)
	MainGPU         int    `json:"mainGpu"`         // Основной GPU (--main-gpu)
	LowVRAM         bool   `json:"lowVram"`         // Режим low VRAM (--low-vram)
	F16KV           bool   `json:"f16kv"`           // FP16 для KV cache (--no-kv-offload отключает)
	KVCacheQuant    string `json:"kvCacheQuant"`    // Квантование KV cache (--cache-type-k)
	FlashAttention  bool   `json:"flashAttention"`  // Flash Attention (--flash-attn)
	MaxLoadedModels int    `json:"maxLoadedModels"` // Максимум загруженных моделей
	Source          string `json:"source"`          // Источник: process-args / env / default
}

// ModelContextInfo - информация о контексте модели
type ModelContextInfo struct {
	Name             string `json:"name"`             // Название модели
	ContextLength    int    `json:"contextLength"`    // Размер контекста (токенов)
	ContextSource    string `json:"contextSource"`    // Источник: modelfile / env / runtime / default
	EffectiveContext int    `json:"effectiveContext"` // Фактический контекст с учётом флагов
	ContextMemoryMB  uint64 `json:"contextMemoryMB"`  // Память контекста (MB)
	KVCacheMemoryMB  uint64 `json:"kvCacheMemoryMB"`  // Память KV cache (MB)
	ModelMemoryMB    uint64 `json:"modelMemoryMB"`    // Память самой модели (MB)
	TotalMemoryMB    uint64 `json:"totalMemoryMB"`    // Общая память модели + контекст (MB)
	NumLayers        int    `json:"numLayers"`        // Количество слоёв (для расчёта)
	HiddenSize       int    `json:"hiddenSize"`       // Размер скрытого слоя (для расчёта)
	PrecisionBits    int    `json:"precisionBits"`    // Точность KV cache (16 или 32)
}

// AvailableModel - доступная модель с оценкой загружаемости
type AvailableModel struct {
	Name          string `json:"name"`          // Название модели
	Size          uint64 `json:"size"`          // Размер модели (bytes)
	VRAMUsage     uint64 `json:"vramUsage"`     // Оценка VRAM (MB)
	CanLoad       bool   `json:"canLoad"`       // Может ли быть загружена
	LoadReason    string `json:"loadReason"`    // Причина (если не может)
	ContextLength int    `json:"contextLength"` // Контекст модели
	EstimatedVRAM uint64 `json:"estimatedVram"` // Оценка total VRAM с контекстом (MB)
	Family        string `json:"family"`        // Семейство
	ParameterSize string `json:"parameterSize"` // Размер параметров
	Quantization  string `json:"quantization"`  // Квантование
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