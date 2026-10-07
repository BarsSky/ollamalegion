package types

import (
	"os"
	"strings"
	"time"
)

// BackendStatus - статус бэкенда
type BackendStatus string

const (
	StatusHealthy           BackendStatus = "healthy"
	StatusUnhealthy         BackendStatus = "unhealthy"
	StatusOffline           BackendStatus = "offline"
	StatusStarting          BackendStatus = "starting"
	StatusDraining          BackendStatus = "draining"
	StatusOllamaUnavailable BackendStatus = "ollama_unavailable" // Агент жив, но Ollama недоступна
)

// PlatformMode - режим работы платформы
type PlatformMode string

const (
	ModeAuto PlatformMode = "auto"
	ModeGPU  PlatformMode = "gpu"
	ModeCPU  PlatformMode = "cpu"
)

// Backend - конфигурация бэкенда
type Backend struct {
	ID                  string        `json:"id"`
	Name                string        `json:"name"`
	Host                string        `json:"host"`
	OllamaPort          int           `json:"ollamaPort"`
	AgentPort           int           `json:"agentPort"`
	Weight              int           `json:"weight"`
	MaxConcurrentReqs   int           `json:"maxConcurrentRequests"`
	MaxModels           int           `json:"maxModels"`
	Labels              []string      `json:"labels"`
	Status              BackendStatus `json:"status"`
	LastHealthCheck     time.Time     `json:"lastHealthCheck"`
	ConsecutiveFailures int           `json:"consecutiveFailures"`
	ActiveRequests      int           `json:"activeRequests"`
	HasAgent            bool          `json:"hasAgent"`
	AgentID             string        `json:"agentId,omitempty"` // ID прикреплённого агента v2
	LastAgentContact    time.Time     `json:"lastAgentContact"`

	// NodeAddr — R-MultiHost (2026-10-07): сетевой адрес УЗЛА, с которого
	// пришла регистрация (IP-источник запроса, см. api/registration_guard.go).
	//
	// ЗАЧЕМ. В multi-host развёртывании compose-файл на каждой машине
	// одинаков, поэтому все «идентифицирующие» строки совпадают буквально:
	// host = cppworker-gpu (имя контейнера), backendID = cppworker-gpu-bundled-agent,
	// cppWorkerPort = 18092. Протокол регистрации до этого поля не нёс НИ ОДНОГО
	// признака, отличающего машину A от машины B, поэтому вторая машина
	// «прилипала» к записи первой: FindBackendByHostPort находил её по
	// (host, port), BackendExists — по ID, и агент второй машины молча
	// переписывал чужие AgentID/AgentPort/метрики. Наблюдалось на живой стойке:
	// единственная llama_cpp-запись с URL первой машины показывала CPU/GPU
	// второй (i5-13420H, чужой GPU UUID) — «странное отображение» страницы модели.
	//
	// NodeAddr — единственный признак, который различает узлы, и он же —
	// основание для отказа (409) в registration_guard.go. Пустая строка =
	// «узел неизвестен» (запись, созданная до этой ревизии, или ручное создание
	// из WebUI): такие записи НЕ защищаются, поведение остаётся прежним.
	NodeAddr string `json:"nodeAddr,omitempty"`

	// Тип бэкенда (ollama / llama_cpp)
	Type BackendType `json:"type"`

	// Engine — движок инференса (ollama_api / llama_cpp / auto).
	// Если auto — определяется по Type бэкенда.
	Engine BackendEngine `json:"engine"`

	// ApiStyle — явный API-стиль, который бэкенд говорит с балансером
	// (ollama-native / openai-compatible). Round 51.2 (2026-08-20):
	// если пусто — EffectiveAPIStyle() выводит из Type. R51.3+ routing
	// будет использовать EffectiveAPIStyle() вместо isLlamaCppBackend.
	// См. docs/superpowers/specs/2026-08-19-balancer-api-routing-design.md §2.
	ApiStyle APIStyle `json:"apiStyle,omitempty"`

	// GPU Mode (auto / gpu / cpu)
	GPUMode PlatformMode `json:"gpuMode"`

	// Runtime-лимиты (меняются через API без перезапуска)
	RuntimeMaxModels             int `json:"runtimeMaxModels"`
	RuntimeMaxConcurrentRequests int `json:"runtimeMaxConcurrentRequests"`

	// RuntimeCapacityFromNode — R70 (2026-09-24): вместимость пришла от самой
	// ноды (саморегистрация cppworker'а: реальный n_parallel). Пока true,
	// вместимостью считается MaxConcurrentReqs ноды
	// (см. EffectiveMaxConcurrentRequests), а не runtime-значение.
	// R71 (2026-09-24): сериализуется — иначе после перезапуска балансера
	// признак терялся и «эхо» агента снова перекрывало реальный n_parallel.
	// Сбрасывается оператором: PUT /limits или правка бэкенда из WebUI.
	RuntimeCapacityFromNode bool `json:"runtimeCapacityFromNode,omitempty"`

	// RuntimeModelSlots — R83 (2026-09-29): сколько параллельных сессий РЕАЛЬНО
	// заведено у загруженной модели (cppworker отдаёт `max_slots` в /api/models).
	//
	// Зачем отдельное поле, а не MaxConcurrentReqs. MaxConcurrentReqs — статическая
	// вместимость бэкенда, и её каждые 30 секунд переписывает перерегистрация агента
	// (значение из AGENT_MAX_CONCURRENT_REQUESTS). Поэтому авто-значение из слотов
	// модели там не выживало: poller ставил 2, ближайший heartbeat возвращал 1 —
	// воспроизведено на живом стенде. Здесь живёт ФАКТ (слоты), агент его не трогает,
	// а итоговую вместимость считает EffectiveMaxConcurrentRequests().
	//
	// 0 = неизвестно (старая сборка cppworker) → прежнее поведение.
	RuntimeModelSlots int `json:"runtimeModelSlots,omitempty"`

	// OllamaConfig — желаемые runtime-флаги Ollama, передаваемые агенту (только для ollama-типа)
	OllamaConfig *OllamaDesiredConfig `json:"ollamaConfig,omitempty"`

	// CppWorkerPort — порт cppworker для llama.cpp-бэкендов (по умолчанию 18091)
	CppWorkerPort int `json:"cppWorkerPort,omitempty"`

	// ImagePort — порт image-воркера (diffusion / stable-diffusion.cpp) для
	// бэкендов типа image_cpp. R-Image (2026-09-27): image-бэкенд полностью
	// отдельный (свой порт, свой каталог моделей, свой API-стиль).
	// Если 0 — используется CppWorkerPort (обратная совместимость и ручная
	// настройка «одним портом»), иначе DefaultImageWorkerPort (18093).
	ImagePort int `json:"imagePort,omitempty"`

	// GPUIndex — индекс GPU, на которой реально работает этот бэкенд.
	//
	// УКАЗАТЕЛЬ, а не int: 0 — валидный индекс ПЕРВОЙ карты, и в одном int
	// «явно GPU 0» неотличимо от «не задано». Семантика (R-Image follow-up,
	// 2026-10-02):
	//
	//	nil      — индекс НЕИЗВЕСТЕН: какой картой пользуется бэкенд, мы не
	//	           знаем → лок сосуществования берётся на весь хост
	//	           (консервативно: пустить текст на карту генерации = OOM
	//	           внутри движка);
	//	&0       — ЯВНО GPU 0: индекс известен, лок = host#gpu0, и занятая
	//	           генерацией первая карта больше НЕ блокирует бэкенд на второй
	//	           карте того же хоста;
	//	&N (N>0) — явно GPU N.
	//
	// Отличие nil от &0 — ровно то, из-за чего поле стало указателем: без него
	// объявить первую карту было нельзя (лок оставался хостовым), как и
	// СБРОСИТЬ индекс через PUT (0 трактовался как «не менять»).
	//
	// Зачем поле: лок сосуществования image/text брался по host целиком, и на
	// multi-GPU хосте генерация на одной карте блокировала текстовый бэкенд на
	// другой. Ключ лока = host + индекс, когда индекс известен у ОБЕИХ сторон;
	// иначе — по хосту (см. internal/balancer/image_resources.go: locksConflict).
	GPUIndex *int `json:"gpuIndex,omitempty"`

	// CppWorkerConfig — настройки llama.cpp (только для llama_cpp-типа)
	CppWorkerConfig *LlamaCppConfig `json:"cppWorkerConfig,omitempty"`

	// CppWorkerApiToken — Bearer token для аутентификации на cppworker endpoints
	// (например /api/models/reload требует authMiddleware).
	// Если пусто, balancer НЕ отправляет Authorization header
	// (подходит для bundled-режима без auth или если cppworker тоже без auth).
	// Round 7 (2026-07-09): используется в reloadModelOnCppWorker для apply профилей.
	CppWorkerApiToken string `json:"cppWorkerApiToken,omitempty"`

	// RequestTimeout — пер-бэкенд таймаут запроса (сек), 0 = использовать глобальный LB_REQUEST_TIMEOUT
	RequestTimeout int `json:"requestTimeout"`
	// RuntimeRequestTimeout — runtime-значение таймаута от балансера (меняется адаптивно, сохраняется в state.json)
	RuntimeRequestTimeout int `json:"runtimeRequestTimeout"`
}

// EffectiveImagePort — R-Image (2026-09-27): порт image-воркера для бэкенда
// типа image_cpp. Приоритет: явный ImagePort → CppWorkerPort (ручная настройка
// «одним портом») → DefaultImageWorkerPort (18093).
func (b *Backend) EffectiveImagePort() int {
	if b == nil {
		return DefaultImageWorkerPort
	}
	if b.ImagePort > 0 {
		return b.ImagePort
	}
	if b.CppWorkerPort > 0 {
		return b.CppWorkerPort
	}
	return DefaultImageWorkerPort
}

// GPUIndexPtr — указатель на индекс GPU для Backend.GPUIndex.
//
// Единственная причина существования: не размножать по API/тестам взятие
// адреса локальной переменной и не терять разницу между &0 («явно первая
// карта») и nil («индекс неизвестен»).
func GPUIndexPtr(i int) *int { return &i }

// EffectiveGPUIndex — индекс GPU бэкенда: (индекс, известен ли он).
//
// Приоритет: явный GPUIndex → CppWorkerConfig.MainGPU (только > 0) → «неизвестно».
//
// Семантика возврата (от неё напрямую зависит ключ лока сосуществования):
//
//	(i, true)  — индекс ИЗВЕСТЕН; i == 0 означает ЯВНО первую карту, а не
//	             «не задано»;
//	(0, false) — индекс НЕИЗВЕСТЕН: лок обязан остаться хостовым, сузить его
//	             «на глазок» нельзя (текст на карту генерации = OOM).
//
// Фолбэк на MainGPU осознанный: у llama.cpp-бэкенда MainGPU — это ровно тот
// device, на который cppworker кладёт веса (internal/cppbackend:
// cfg.MainGPU = ts.MainGPU из авто-распределения), то есть конфигурация уже
// называет карту, и игнорировать её значило бы оставлять хостовый лок там, где
// сторона сама сказала, какая у неё GPU. MainGPU == 0 индексом НЕ считается:
// ноль там — «авто/не задано», и трактовать его как «явно карта 0» значило бы
// сузить лок по догадке.
//
// Отрицательный явный индекс смысла не имеет (валидация — на HTTP-границе,
// см. internal/api/handlers_backends.go) и здесь трактуется как «неизвестно»:
// так состояние, попавшее в state.json правкой руками, не создаёт лок с ключом
// вида host#gpu-1.
func (b *Backend) EffectiveGPUIndex() (int, bool) {
	if b == nil {
		return 0, false
	}
	if b.GPUIndex != nil {
		if *b.GPUIndex >= 0 {
			return *b.GPUIndex, true
		}
		return 0, false
	}
	if b.CppWorkerConfig != nil && b.CppWorkerConfig.MainGPU > 0 {
		return b.CppWorkerConfig.MainGPU, true
	}
	return 0, false
}

// EffectiveMaxConcurrentRequests — R71 (2026-09-24): единое правило вместимости
// бэкенда для admission-очереди, слотов и отчётов.
//
// Приоритет:
//  1. RuntimeCapacityFromNode && MaxConcurrentReqs > 0 — вместимость сообщила
//     сама нода (cppworker: реальный n_parallel). Runtime-значение в этом случае
//     историческое: агент получал его в ответе на heartbeat и возвращал обратно
//     («эхо»), из-за чего узел с n_parallel=1 жил с порогом очереди 4.
//  2. RuntimeMaxConcurrentRequests > 0 — операторский лимит
//     (PUT /api/v1/backends/{id}/limits или правка из WebUI; эти пути снимают
//     RuntimeCapacityFromNode).
//  3. MaxConcurrentReqs — статический лимит из конфига.
//
// 0 или -1 означают «лимит не задан» — как и в прежней логике
// «runtime > 0 ? runtime : max».
//
// R83 (2026-09-29): перед всеми этими источниками стоит RuntimeModelSlots —
// фактическое число слотов, с которым модель загружена (cppworker отдаёт
// max_slots в /api/models). Именно оно означает реальную способность бэкенда
// держать параллельные сессии; MaxConcurrentReqs — лишь статический дефолт,
// который вдобавок переписывается перерегистрацией агента каждые 30 секунд
// (поэтому авто-значение там не выживало).
func (b *Backend) EffectiveMaxConcurrentRequests() int {
	if b == nil {
		return 0
	}
	// R83 (2026-09-29): учёт ФАКТИЧЕСКИХ слотов модели — opt-in.
	//
	// Авто-привязка вместимости к слотам меняет поведение уже проверенных путей
	// (admission-очередь, слот-менеджер, выбор бэкенда), поэтому по умолчанию она
	// ВЫКЛЮЧЕНА: стенд работает ровно как раньше. Оператор включает её осознанно,
	// когда модель загружена с parallel > 1 и он хочет, чтобы балансер пропускал
	// столько запросов параллельно: LB_CAPACITY_FROM_MODEL_SLOTS=true.
	slots := 0
	if CapacityFromModelSlotsEnabled() {
		slots = b.RuntimeModelSlots
	}
	return ResolveEffectiveCapacity(slots, b.RuntimeMaxConcurrentRequests,
		b.RuntimeCapacityFromNode, b.MaxConcurrentReqs)
}

// CapacityFromModelSlotsEnabled — включён ли учёт фактических слотов модели.
//
// Читается из окружения на каждом вызове: резолвер горячий, но чтение одной
// переменной дешевле, чем риск рассинхрона кэша (в тестах окружение меняется
// через t.Setenv).
func CapacityFromModelSlotsEnabled() bool {
	for _, name := range []string{"LB_CAPACITY_FROM_MODEL_SLOTS", "CPPWORKER_CAPACITY_FROM_SLOTS"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

// ResolveEffectiveCapacity — чистая функция резолвера вместимости: вынесена
// отдельно, чтобы её можно было проверить тестом без конструирования Backend и
// чтобы правило было в одном месте.
//
// R83 (2026-09-29): первым идёт RuntimeModelSlots — ФАКТИЧЕСКОЕ число слотов
// загруженной модели (cppworker отдаёт max_slots в /api/models). Это реальная
// способность бэкенда держать параллельные сессии; MaxConcurrentReqs — лишь
// статический дефолт, который вдобавок переписывается перерегистрацией агента
// каждые 30 секунд (поэтому авто-значение там не выживало: poller ставил 2,
// ближайший heartbeat возвращал 1 — воспроизведено на живом стенде).
//
// Операторский лимит (runtimeMax) уважается, но НЕ может превысить фактические
// слоты: иначе балансер открыл бы ложные свободные слоты и слал больше запросов,
// чем cppworker обслуживает.
func ResolveEffectiveCapacity(modelSlots, runtimeMax int, capacityFromNode bool, staticMax int) int {
	if modelSlots > 0 {
		if runtimeMax > 0 && runtimeMax <= modelSlots {
			return runtimeMax
		}
		return modelSlots
	}
	if capacityFromNode && staticMax > 0 {
		return staticMax
	}
	if runtimeMax > 0 {
		return runtimeMax
	}
	return staticMax
}

// OllamaDesiredConfig — желаемая конфигурация Ollama, передаваемая агенту через heartbeat
type OllamaDesiredConfig struct {
	NumGPULayers    int    `json:"numGpuLayers"`    // Количество слоёв на GPU (-1 = не менять)
	ContextLength   int    `json:"contextLength"`   // Размер контекста (-1 = не менять)
	NumParallel     int    `json:"numParallel"`     // Параллельных запросов (-1 = не менять)
	NumThreads      int    `json:"numThreads"`      // Потоков CPU (-1 = не менять)
	BatchSize       int    `json:"batchSize"`       // Размер батча (-1 = не менять)
	MaxLoadedModels int    `json:"maxLoadedModels"` // Макс. загруженных моделей (-1 = не менять)
	FlashAttention  *bool  `json:"flashAttention"`  // Flash Attention (nil = не менять)
	KVCacheQuant    string `json:"kvCacheQuant"`    // Квантование KV cache ("" = не менять)
	Source          string `json:"source"`          // Источник: balancer
}

// HealthCheckResult - результат проверки здоровья
type HealthCheckResult struct {
	BackendID string        `json:"backendId"`
	Healthy   bool          `json:"healthy"`
	Latency   time.Duration `json:"latency"`
	Error     string        `json:"error,omitempty"`
	Timestamp time.Time     `json:"timestamp"`
}
