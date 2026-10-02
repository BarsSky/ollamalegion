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
