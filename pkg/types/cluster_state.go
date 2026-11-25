package types

import "time"

// ClusterState - состояние кластера
type ClusterState struct {
	Timestamp       time.Time `json:"timestamp"`
	TotalBackends   int       `json:"totalBackends"`
	HealthyBackends int       `json:"healthyBackends"`
	TotalRequests   int64     `json:"totalRequests"`
	// D4.2 (2026-11-25): totalRequests считает ТОЛЬКО успешно проксированные
	// запросы (инкремент стоит после ответа апстрима). При отказах панель
	// показывала 0 там, где клиенты уже получали 503: живой замер на стенде —
	// `totalRequests=0` при `recentClients.requestCount=6`. TotalAttempts —
	// все попытки инференс-запросов, включая отказы, чтобы число сходилось с
	// recentClients и «активных запросов» в панели.
	TotalAttempts        int64               `json:"totalAttempts"`
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

	// GPUMaxTemp — порог температуры GPU (в °C) из конфигурации ресурсов
	// (Resources.GPU.MaxTemperature, env LB_GPU_MAX_TEMP).
	//
	// R757 (2026-11-25): зачем в состоянии кластера. Балансер давно оценивает
	// перегрев как soft-условие и пишет в лог строки вида
	// `backend GPU горячее заданного предела (soft, не блокирует) temperature=88
	// threshold=85`, но WebUI этот порог не получал вообще: в Monitor видно
	// только GPU%, а сам порог — лишь в логе контейнера. Оператор видел жалобы
	// на троттлинг и не мог понять, какая температура считается критичной.
	// Теперь панель рисует температуру с подсветкой по этому порогу и явным
	// текстом, так что значение конфигурации становится видимым.
	//
	// 0 = порог не задан (панель показывает температуру без оценки).
	GPUMaxTemp int `json:"gpuMaxTemp,omitempty"`
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
