package types

import "time"

// BackendMetrics - метрики бэкенда в реальном времени
type BackendMetrics struct {
	ID        string        `json:"id"`
	Timestamp time.Time     `json:"timestamp"`
	Status    BackendStatus `json:"status"`
	HasAgent  bool          `json:"hasAgent"` // Флаг наличия активного агента

	// Идентификатор текущего запроса, для которого собраны эти метрики
	// (correlation id, прокидывается из middleware и логов).
	RequestID string `json:"request_id,omitempty"`

	// Тип бэкенда и движок
	BackendType BackendType   `json:"backendType"`
	Engine      BackendEngine `json:"engine"`

	// Конфигурация бэкенда (атомарно копируется из Backend)
	Host          string `json:"host"`
	OllamaPort    int    `json:"ollamaPort"`
	CppWorkerPort int    `json:"cppWorkerPort,omitempty"` // Порт cppworker для llama.cpp-бэкендов
	// ImagePort — порт image-воркера (тип image_cpp). Заполняется из
	// Backend.EffectiveImagePort(); без него Monitor/WebUI не могли показать
	// отдельный порт image-бэкенда (жалоба оператора, 2026-10-02).
	ImagePort int `json:"imagePort,omitempty"`

	// Image — данные image-бэкенда (модели, состояние, VRAM) из поллера
	// image_resources. nil для текстовых бэкендов.
	// Без этого поля WebUI/Monitor видели image-бэкенд «пустым»: ни моделей,
	// ни состояния (у текстовых они лежат в Ollama/LlamaCpp).
	Image *ImageBackendMetrics `json:"image,omitempty"`

	// GPU метрики
	GPU GPUMetrics `json:"gpu"`

	// Системные метрики
	System SystemMetrics `json:"system"`

	// Ollama метрики (только для ollama-бэкендов)
	Ollama OllamaMetrics `json:"ollama"`

	// llama.cpp метрики (только для llama_cpp-бэкендов)
	LlamaCpp LlamaCppMetrics `json:"llamaCpp,omitempty"`

	// LoadFailure — причина последнего провала загрузки модели (R83).
	//
	// Заполняет agent из cppworker /api/models (поле load_failure) и пересылает
	// в этом же push'е. Балансер публикует уведомление только на СМЕНУ причины
	// (см. LoadFailureInfo.Key), поэтому поток событий не зависит от частоты
	// опроса и не забивает SSE-буфер (в нём всего 100 событий).
	LoadFailure *LoadFailureInfo `json:"loadFailure,omitempty"`

	// LoadDegraded — модель загружена, но работает деградированно (R83 §9.4
	// шаг 1б, 2026-09-26): сегодня это cpu_only, когда веса не влезли в VRAM.
	//
	// Почему отдельное поле, а не loadFailure: загрузка УДАЛАСЬ. Балансер
	// публикует уведомление с severity=warning и отдельной дедупликацией
	// (DegradedLoadInfo.Key) — «не хватило памяти» о работающей модели было бы
	// ложью, а последующее «загрузка восстановлена» — второй ложью.
	LoadDegraded *DegradedLoadInfo `json:"loadDegraded,omitempty"`

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
	RequestTimeout        int      `json:"requestTimeout"`        // Per-backend статический таймаут
	RuntimeRequestTimeout int      `json:"runtimeRequestTimeout"` // Runtime-значение (адаптивное)
	EffectiveTimeout      int      `json:"effectiveTimeout"`      // Эффективный таймаут (макс. приоритет)
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

	// UUIDs — ФИЗИЧЕСКИЕ идентификаторы карт этой машины
	// (nvidia-smi --query-gpu=uuid, например "GPU-6f8d5f3b-…").
	//
	// ЗАЧЕМ (R-Image follow-up, 2026-10-07). Один хост может держать НЕСКОЛЬКО
	// бэкендов — типовой случай: на одной рабочей машине подняты и cppworker
	// (текст), и imageworker (картинки). Оба процесса читают nvidia-smi ОДНОЙ
	// карты и присылают балансеру ОДНИ И ТЕ ЖЕ memoryTotal/memoryUsed. Балансер
	// складывал их как разные ресурсы и показывал удвоенную память (16 GB на
	// карте в 8 GB), а в UI «Всего VRAM» выглядело как две карты.
	//
	// UUID — единственный признак, который различает «две машины» и «одна
	// машина, два контейнера»: у контейнеров на одном хосте он совпадает
	// побайтово, у разных физических карт — различается. Имя хоста для этого
	// НЕ подходит: балансер видит имена контейнеров (cppworker-gpu, imageworker),
	// то есть разные «хосты» у одной машины.
	//
	// Пустой срез = «неизвестно» (не-nvidia платформа или nvidia-smi без прав):
	// такие бэкенды считаются разными машинами, как раньше.
	UUIDs []string `json:"uuids,omitempty"`
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
