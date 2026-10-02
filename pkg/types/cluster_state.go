package types

import "time"

// ClusterState - состояние кластера
type ClusterState struct {
	Timestamp            time.Time           `json:"timestamp"`
	TotalBackends        int                 `json:"totalBackends"`
	HealthyBackends      int                 `json:"healthyBackends"`
	TotalRequests        int64               `json:"totalRequests"`
	ActiveRequests       int                 `json:"activeRequests"`
	QueuedRequests       int                 `json:"queuedRequests"`
	RPS                  float64             `json:"rps"`
	TotalGPUUsage        float64             `json:"totalGpuUsage"`
	OperatingMode        string              `json:"operatingMode"`           // Текущий вариант работы балансера
	BackendEngine        BackendEngine       `json:"backendEngine"`           // Текущий движок инференса
	EffectiveBackendType BackendType         `json:"effectiveBackendType"`    // Доминирующий тип бэкенда (пусто = смешанный)
	RecentClients        []RecentClient      `json:"recentClients,omitempty"` // Последние HTTP-клиенты (включая /api/tags)
	BackendTypeCounts    map[BackendType]int `json:"backendTypeCounts"`       // Количество бэкендов по типам
	Backends             []BackendMetrics    `json:"backends"`

	// Image — агрегат по image-пулу (R-Image Phase 8, 2026-10-03): счётчики
	// запросов генерации и общая лента последних запросов.
	//
	// ЗАЧЕМ ЗДЕСЬ, А НЕ ТОЛЬКО В /api/v1/metrics: Monitor опрашивает именно
	// /api/v1/cluster и рисует панели из этого ответа. Без агрегата в кластере
	// панель «запросы к image-бэкендам» требовала бы второго запроса на каждом
	// тике (5 с), то есть удвоения трафика ради одной панели.
	//
	// Per-backend счётчики лежат в Backends[i].Image.Requests — их достаточно
	// для колонок таблицы; здесь только то, что относится ко всему пулу.
	Image *ImagePoolMetrics `json:"image,omitempty"`
}

// ImagePoolMetrics — агрегат image-запросов по всему пулу (все типы image_cpp).
type ImagePoolMetrics struct {
	// Backends — сколько image-бэкендов в кластере (включая нездоровые).
	Backends int `json:"backends"`
	// Requests — агрегаты запросов по пулу; Recent — общая лента (свежие в начале).
	Requests *ImageRequestMetrics `json:"requests,omitempty"`
	Recent   []ImageRequestBrief  `json:"recent,omitempty"`
}

// RecentClient — клиент, обратившийся к балансеру за последние N секунд
type RecentClient struct {
	ClientName     string    `json:"clientName"`
	ClientIP       string    `json:"clientIP"`
	UserAgent      string    `json:"userAgent"`
	LastRequestAt  time.Time `json:"lastRequestAt"`
	RequestCount   int       `json:"requestCount"`
	IsStreamActive bool      `json:"isStreamActive"`  // Активен ли streaming-запрос сейчас
	Model          string    `json:"model,omitempty"` // Текущая модель (если известна)
}
