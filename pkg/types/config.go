package types

// LoadBalancerConfig - конфигурация балансировщика
type LoadBalancerConfig struct {
	LoadBalancer LoadBalancerSettings `json:"loadBalancer"`
	Backends     []Backend            `json:"backends"`
	Balancing    BalancingSettings    `json:"balancing"`
	Resources    ResourceLimits       `json:"resources"`
	Logging      LoggingSettings      `json:"logging"`
	API          APISettings          `json:"api"`
	TLS          TLSConfig            `json:"tls"`
	Auth         AuthConfig           `json:"auth"`
	Initialized  bool                 `json:"initialized"` // true = первичная настройка выполнена
	BackendEngine BackendEngine       `json:"backendEngine,omitempty"` // выбранный движок инференса
	LlamaCpp      LlamaCppConfig      `json:"llamaCpp"`   // глобальные настройки llama.cpp / GGUF

	// LlamaCppModelProfiles — per-model профили параметров загрузки (Шаг 5).
	// Ключ — имя модели (как в запросе: e.g. "gemma-4-E4B-it-Q4_K_M").
	// Применяется через 3-tier resolver в balancer для num_ctx override.
	LlamaCppModelProfiles map[string]LlamaCppModelProfile `json:"llamaCppModelProfiles,omitempty"`

	// DefaultModelProfile — профиль по умолчанию для моделей, не имеющих
	// записи в LlamaCppModelProfiles. Используется как fallback-потолок при
	// clamping per-request num_ctx (Phase D.3-fix).
	DefaultModelProfile *LlamaCppModelProfile `json:"defaultModelProfile,omitempty"`

	// ModeMigrationNotes — R97 (S7 плана 2026-10-07): заметки автоконверта
	// `operatingMode` → `balancing.placement`, снятые при загрузке конфига.
	//
	// json:"-" НАМЕРЕННО: это служебная диагностика, а не часть конфиг-файла.
	// Если бы поле сериализовалось, миграция записывала бы себя в конфиг и
	// «залипала» (а вместе с ней и устаревшие заметки). Нужно, чтобы оператор
	// видел конвертацию не только в логе, но и в GET /api/v1/placement:
	// «режим X переведён в политику Y, fallback=single выставлен автоматически».
	ModeMigrationNotes []string `json:"-"`
}

// LoadBalancerSettings - настройки балансировщика
type LoadBalancerSettings struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	APIPort int    `json:"apiPort"`
	// OpenAIPort — R-Image (2026-09-27): отдельная «OpenAI-поверхность» балансера.
	// Обслуживает только OpenAI-стиль (/v1/chat/completions, /v1/completions,
	// /v1/embeddings, /v1/models, /v1/images/*) и A1111-совместимый прогон
	// (/sdapi/v1/*) для image-бэкендов. Стриминг решается ТОЛЬКО полем stream
	// в теле запроса (без legacy auto-stream). Порт 18080 (Port) при этом
	// сохраняет прежнее поведение без изменений. 0 = порт не поднимать.
	OpenAIPort      int      `json:"openAiPort"`
	TLSHost         string   `json:"tlsHost"`          // хост для HTTPS
	TLSPort         int      `json:"tlsPort"`          // порт для HTTPS
	StatePath       string   `json:"statePath"`        // путь к файлу сохранения состояния
	TrustedProxies  []string `json:"trustedProxies"`   // CIDR или IP доверенных прокси
	ClientIPHeaders []string `json:"clientIPHeaders"`  // Приоритет заголовков для IP клиента
	AlwaysTrustProxyHeaders bool `json:"alwaysTrustProxyHeaders"`
}

// TLSConfig - конфигурация TLS/SSL
type TLSConfig struct {
	Enabled    bool   `json:"enabled"`
	CertFile   string `json:"certFile"`
	KeyFile    string `json:"keyFile"`
	MinVersion string `json:"minVersion"`
	AutoCert   bool   `json:"autoCert"`
}

// AuthConfig - конфигурация аутентификации API
type AuthConfig struct {
	Enabled    bool     `json:"enabled"`
	Tokens     []string `json:"tokens"`
	HeaderName string   `json:"headerName"`
}

// APISettings - настройки API
type APISettings struct {
	RateLimit float64 `json:"rateLimit"`
	RateBurst float64 `json:"rateBurst"`
}

// LoggingSettings - настройки логирования
type LoggingSettings struct {
	Level  string `json:"level"`
	Format string `json:"format"`
	// Language — R234 (2026-10-24): язык логов и сообщений об ошибках.
	//
	// "ru" (по умолчанию) | "en" | "auto" (взять из LANG/LC_ALL).
	// Пустое значение = "ru": до R234 все сообщения были русскими, и менять
	// поведение у существующих конфигов без строки language нельзя.
	//
	// Источник истины — балансер: он отдаёт язык через GET /api/v1/i18n,
	// меняет через PUT (WebUI) и сохраняет в эту секцию; воркеры подтягивают
	// его с балансера, поэтому env воркеров нужен только для автономного старта.
	Language string `json:"language,omitempty"`
}

// ResourceLimits - лимиты ресурсов для принятия решений
type ResourceLimits struct {
	GPU    GPULimits    `json:"gpu"`
	CPU    CPULimits    `json:"cpu"`
	Memory MemoryLimits `json:"memory"`
	Disk   DiskLimits   `json:"disk"`
}

// GPULimits - лимиты GPU
type GPULimits struct {
	MaxUsagePercent     float64 `json:"maxUsagePercent"`
	MaxVRAMUsagePercent float64 `json:"maxVramUsagePercent"`
	MaxTemperature      int     `json:"maxTemperature"`
}

// CPULimits - лимиты CPU
type CPULimits struct {
	MaxUsagePercent float64 `json:"maxUsagePercent"`
}

// MemoryLimits - лимиты памяти
type MemoryLimits struct {
	MaxUsagePercent float64 `json:"maxUsagePercent"`
}

// DiskLimits - лимиты диска
type DiskLimits struct {
	MinFreeMB uint64 `json:"minFreeMB"`
}