package types

// LlamaCppMetrics - метрики llama.cpp бэкенда
type LlamaCppMetrics struct {
	LoadedModels      []LlamaCppModel `json:"loadedModels"`
	AvailableModels   []LlamaCppModel `json:"availableModels,omitempty"`
	ActiveRequests    int             `json:"activeRequests"`
	TotalRequests     int64           `json:"totalRequests"`
	AvgResponseTime   float64         `json:"avgResponseTime"`   // ms
	RequestsPerSecond float64         `json:"requestsPerSecond"`
	FreeSlots         int             `json:"freeSlots"`
	AvailableSlots    int             `json:"availableSlots"`
	MaxModels         int             `json:"maxModels"`
	MaxConcurrentReqs int             `json:"maxConcurrentReqs"`
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
}

// LlamaCppGPUInfo - информация о GPU для llama.cpp
type LlamaCppGPUInfo struct {
	Count       int               `json:"count"`
	Devices     []LlamaCppGPUDevice `json:"devices"`
	TotalVRAM   uint64            `json:"totalVram"`   // MB
	FreeVRAM    uint64            `json:"freeVram"`    // MB
}

// LlamaCppGPUDevice - одно GPU-устройство
type LlamaCppGPUDevice struct {
	Name        string `json:"name"`
	MemoryTotal uint64 `json:"memoryTotal"` // MB
	MemoryFree  uint64 `json:"memoryFree"`  // MB
	Utilization float64 `json:"utilization"` // %
}