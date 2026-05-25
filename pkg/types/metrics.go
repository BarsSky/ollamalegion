package types

import "time"

// BackendMetrics - метрики бэкенда в реальном времени
type BackendMetrics struct {
	ID        string        `json:"id"`
	Timestamp time.Time     `json:"timestamp"`
	Status    BackendStatus `json:"status"`
	HasAgent  bool          `json:"hasAgent"` // Флаг наличия активного агента

	// Тип бэкенда и движок
	BackendType BackendType    `json:"backendType"`
	Engine      BackendEngine  `json:"engine"`

	// Конфигурация бэкенда (атомарно копируется из Backend)
	Host       string `json:"host"`
	OllamaPort int    `json:"ollamaPort"`

	// GPU метрики
	GPU GPUMetrics `json:"gpu"`

	// Системные метрики
	System SystemMetrics `json:"system"`

	// Ollama метрики (только для ollama-бэкендов)
	Ollama OllamaMetrics `json:"ollama"`

	// llama.cpp метрики (только для llama_cpp-бэкендов)
	LlamaCpp LlamaCppMetrics `json:"llamaCpp,omitempty"`

	// Прогноз критического состояния
	Prediction Prediction `json:"prediction"`

	// --- Monitor-friendly computed fields (filled by GetClusterState) ---
	Score                 float64  `json:"score"`                 // Calculated routing score
	MaxConcurrentRequests int      `json:"maxConcurrentRequests"` // From backend config
	Models                []string `json:"models"`                // Names of running models
	VRAMUsagePercent      float64  `json:"vramUsagePercent"`      // GPU memory usage %
	VRAMTotalGB           float64  `json:"vramTotalGB"`           // Total VRAM in GB
	VRAMUsedGB            float64  `json:"vramUsedGB"`            // Used VRAM in GB
	MemoryUsagePercent    float64  `json:"memoryUsagePercent"`    // RAM usage %
	// Таймауты запросов (заполняются в GetClusterState)
	RequestTimeout        int `json:"requestTimeout"`        // Per-backend статический таймаут
	RuntimeRequestTimeout int `json:"runtimeRequestTimeout"` // Runtime-значение (адаптивное)
	EffectiveTimeout      int `json:"effectiveTimeout"`      // Эффективный таймаут (макс. приоритет)
	WarmingUpModels       []string `json:"warmingUpModels"`       // Модели в превентивной загрузке
}

// GPUMetrics - метрики GPU
type GPUMetrics struct {
	UsagePercent float64 `json:"usagePercent"` // Загрузка GPU %
	MemoryTotal  uint64  `json:"memoryTotal"`  // Всего VRAM (MB)
	MemoryUsed   uint64  `json:"memoryUsed"`   // Использовано VRAM (MB)
	MemoryFree   uint64  `json:"memoryFree"`   // Свободно VRAM (MB)
	Temperature  int     `json:"temperature"`  // Температура (°C)
	PowerUsage   int     `json:"powerUsage"`   // Потребление (W)
	PowerLimit   int     `json:"powerLimit"`   // Лимит мощности (W)
	GPUClock     int     `json:"gpuClock"`     // Частота GPU (MHz)
	MemClock     int     `json:"memClock"`     // Частота памяти (MHz)
}

// CPUMetrics - расширенные метрики CPU
type CPUMetrics struct {
	UsagePercent  float64   `json:"usagePercent"`  // Общая загрузка CPU %
	UsagePerCore  []float64 `json:"usagePerCore"`  // Загрузка по ядрам
	CoreCount     int       `json:"coreCount"`     // Количество ядер
	ThreadCount   int       `json:"threadCount"`   // Количество потоков
	Model         string    `json:"model"`         // Модель процессора
	LoadAverage1  float64   `json:"loadAverage1"`  // Load average 1 мин
	LoadAverage5  float64   `json:"loadAverage5"`  // Load average 5 мин
	LoadAverage15 float64   `json:"loadAverage15"` // Load average 15 мин
	Temperature   int       `json:"temperature"`   // Температура CPU (°C)
	Throttled     bool      `json:"throttled"`     // CPU троттлинг
}

// SystemMetrics - системные метрики
type SystemMetrics struct {
	CPUUsagePercent float64 `json:"cpuUsagePercent"` // Загрузка CPU %

	// Расширенные CPU метрики
	CPU CPUMetrics `json:"cpu"`

	MemoryTotal uint64 `json:"memoryTotal"` // Всего RAM (MB)
	MemoryUsed  uint64 `json:"memoryUsed"`  // Использовано RAM (MB)
	MemoryFree  uint64 `json:"memoryFree"`  // Свободно RAM (MB)

	DiskTotal uint64 `json:"diskTotal"` // Всего диска (MB)
	DiskUsed  uint64 `json:"diskUsed"`  // Использовано диска (MB)
	DiskFree  uint64 `json:"diskFree"`  // Свободно диска (MB)

	NetworkRX uint64 `json:"networkRX"` // Получено байт
	NetworkTX uint64 `json:"networkTX"` // Отправлено байт

	NetworkRXRate float64 `json:"networkRXRate"` // Скорость получения (байт/с)
	NetworkTXRate float64 `json:"networkTXRate"` // Скорость отправки (байт/с)
}