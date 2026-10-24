package types

// BackendType — тип бэкенда (ollama или llama.cpp)
type BackendType string

const (
	BackendTypeOllama   BackendType = "ollama"
	BackendTypeLlamaCpp BackendType = "llama_cpp"
	// BackendTypeImage — отдельный бэкенд генерации изображений (diffusion, sd.cpp).
	// R-Image (2026-09-27): «полностью отдельный бэкенд» — свой порт (ImagePort,
	// default 18093), свой каталог моделей, свой API-стиль (OpenAI-совместимый).
	// Правила cppworker (n_ctx, chat-шаблоны, reasoning/tools, /api/tags, /api/pull)
	// на него НЕ распространяются. Маршрутизация — только по явному признаку
	// (endpoint /v1/images/*, /sdapi/v1/*, префикс модели sd:/image:).
	BackendTypeImage BackendType = "image_cpp"
)

// DefaultImageWorkerPort — порт image-воркера по умолчанию.
// 18091/18092 заняты cppworker'ами, 18093 свободен.
const DefaultImageWorkerPort = 18093

// DefaultEmbeddedAgentPort — порт /health и метрик ВСТРОЕННОГО агента
// (R-Image, 2026-10-07).
//
// ПОЧЕМУ 18033, А НЕ 18032. 18032 — порт внешнего контейнера `agent`
// (AGENT_PORT по умолчанию). Встроенный агент намеренно слушает другой порт,
// чтобы в переходный период (внешний агент ещё поднят, встроенный уже включён)
// два процесса не подрались за один порт и чтобы в записи бэкенда было видно,
// откуда пришли метрики. Балансер опрашивает метрики по agentPort конкретной
// записи, поэтому значение здесь — не контракт, а дефолт.
const DefaultEmbeddedAgentPort = 18033

// BackendEngine — движок инференса
type BackendEngine string

const (
	EngineOllamaAPI BackendEngine = "ollama_api" // HTTP REST API Ollama
	EngineLlamaCPP  BackendEngine = "llama_cpp"  // llama.cpp через gRPC/HTTP
	EngineImageCPP  BackendEngine = "image_cpp"  // stable-diffusion.cpp (sd-server)
	EngineAuto      BackendEngine = "auto"       // автоопределение
)

// APIStyle — API-стиль, который бэкенд говорит с балансером.
// Round 51.2 (2026-08-20): явное поле в Backend struct (вместо inference по Type).
// Когда явно не задан, EffectiveAPIStyle() выводит из Type:
//   - BackendTypeOllama   → APIStyleOllamaNative
//   - BackendTypeLlamaCpp → APIStyleOpenAICompatible (сохраняем R50 поведение)
type APIStyle string

const (
	// APIStyleOllamaNative — бэкенд говорит нативный Ollama API (/api/*).
	APIStyleOllamaNative APIStyle = "ollama-native"

	// APIStyleOpenAICompatible — бэкенд говорит OpenAI-совместимый API (/v1/*).
	APIStyleOpenAICompatible APIStyle = "openai-compatible"
)

// IsValidAPIStyle — true, если стиль известен балансеру (ollama-native или openai-compatible).
// Прочие значения (включая пустую строку) считаются невалидными — caller должен
// использовать EffectiveAPIStyle() для fallback на вывод по Type.
func (s APIStyle) IsValidAPIStyle() bool {
	switch s {
	case APIStyleOllamaNative, APIStyleOpenAICompatible:
		return true
	default:
		return false
	}
}

// EffectiveAPIStyle — возвращает API-стиль, который бэкенд фактически будет говорить.
//
// Приоритет:
//  1. Если Backend.ApiStyle валиден (ollama-native / openai-compatible) —
//     возвращает его. Это явный выбор оператора при регистрации.
//  2. Иначе выводит из Backend.Type:
//     - BackendTypeLlamaCpp → APIStyleOpenAICompatible (R50 поведение, маршрут через llamacpp_router.go → /v1/*)
//     - BackendTypeOllama (или пусто) → APIStyleOllamaNative (безопасный default: balancer is Ollama-first)
//
// Round 51.2 (2026-08-20): новая логика для R51.2. R50 вывод по Type (Type→Style) жёстко
// зашит в proxy_request.go:isLlamaCppBackend. R51.3+ — миграция роутинга на EffectiveAPIStyle().
//
// Безопасен при nil-получателе: возвращает APIStyleOllamaNative (default).
func (b *Backend) EffectiveAPIStyle() APIStyle {
	if b == nil {
		return APIStyleOllamaNative
	}
	if b.ApiStyle.IsValidAPIStyle() {
		return b.ApiStyle
	}
	// Fallback: inference from Type (сохраняет R50 поведение по умолчанию).
	if b.Type == BackendTypeLlamaCpp {
		return APIStyleOpenAICompatible
	}
	// R-Image: image-бэкенд говорит на OpenAI-совместимом API (/v1/images/*,
	// /sdapi/v1/*) — значит и стиль openai-compatible.
	if b.Type == BackendTypeImage {
		return APIStyleOpenAICompatible
	}
	return APIStyleOllamaNative
}

// LlamaCppConfig — конфигурация llama.cpp бэкенда
type LlamaCppConfig struct {
	GrpcPort            int       `json:"grpcPort"`
	NumGPULayers        int       `json:"numGpuLayers"`
	ContextLength       int       `json:"contextLength"`
	BatchSize           int       `json:"batchSize"`
	FlashAttention      bool      `json:"flashAttention"`
	NUMA                bool      `json:"numa"`
	UseMMap             bool      `json:"useMmap"`
	UseMLock            bool      `json:"useMlock"`
	TensorSplit         []float64 `json:"tensorSplit,omitempty"`
	TensorSplitStr      string    `json:"tensorSplitStr,omitempty"` // строковое представление для API (напр. "0.5,0.5")
	MainGPU             int       `json:"mainGpu"`
	ModelPath           string    `json:"modelPath,omitempty"`
	MaxConcurrentReqs   int       `json:"maxConcurrentReqs"`
	Strategy            string    `json:"strategy"`            // "vram-ratio", "manual", "round-robin"
	AutoGpuDistribution bool      `json:"autoGpuDistribution"` // авто-распределение по GPU

	// Параметры потоков CPU
	NThreads int `json:"nThreads"` // кол-во потоков CPU (0 = auto)

	// RoPE параметры (позиционное кодирование)
	RopeFreqBase  float64 `json:"ropeFreqBase"`
	RopeFreqScale float64 `json:"ropeFreqScale"`

	// KV Cache
	NoKVOffload bool   `json:"noKvOffload"` // не выгружать KV cache на GPU
	KVCacheType string `json:"kvCacheType"` // f16 / f32 / q8_0 / q4_0

	// Параметры нормализации
	RMSNormEps float64 `json:"rmsNormEps"`

	// Параметры контекста
	RopeScalingType   string  `json:"ropeScalingType"` // none / linear / yarn
	RopeScalingFactor float64 `json:"ropeScalingFactor"`
	YarnExtFactor     float64 `json:"yarnExtFactor"`
	YarnAttnFactor    float64 `json:"yarnAttnFactor"`
	YarnBetaFast      float64 `json:"yarnBetaFast"`
	YarnBetaSlow      float64 `json:"yarnBetaSlow"`

	// Дополнительные
	NoMemoryMap  bool   `json:"noMemoryMap"`  // отключить mmap
	RPCBackend   string `json:"rpcBackend"`   // backend для RPC (cuda/vulkan/kompute)
	ModelURL     string `json:"modelUrl"`     // URL для скачивания модели
	ChatTemplate string `json:"chatTemplate"` // шаблон чата

	// ===== R65d (2026-09-20): поля, которые WebUI уже отправлял =====
	//
	// Аудит 2026-09-20: webui/js/app.js:1253-1292 добавляет эти ключи в payload
	// PUT /api/v1/cluster/config, но их НЕ БЫЛО в LlamaCppConfig. json.Decoder
	// молча игнорирует неизвестные ключи, поэтому оператор менял значения в
	// форме, получал тост «Settings saved», а затем verifySync откатывал поля к
	// прежним (читая их из GET). То есть эти настройки выглядели рабочими, но
	// не сохранялись.
	//
	// FlashAttnType — новый способ задания flash-attention: -1 = auto,
	// 0 = off, 1 = on. Legacy bool FlashAttention остаётся для совместимости.
	FlashAttnType int `json:"flashAttnType"`
	// SplitMode — режим multi-GPU: -1 = auto, 0 = none, 1 = layer, 2 = row.
	SplitMode int `json:"splitMode"`
	// IdleUnloadMinutes — выгрузка простаивающей модели, 0 = отключено.
	IdleUnloadMinutes int `json:"idleUnloadMinutes"`
	// EnableMetrics — собирать per-model метрики на cppworker.
	EnableMetrics bool `json:"enableMetrics"`
	// MetricsRetentionSeconds — сколько секунд хранить per-model метрики.
	MetricsRetentionSeconds int `json:"metricsRetentionSeconds"`
	// EnableReasoning — включать reasoning/thinking по умолчанию для моделей.
	EnableReasoning bool `json:"enableReasoning"`
	// ReasoningBudget — лимит токенов на reasoning, 0 = без лимита.
	ReasoningBudget int `json:"reasoningBudget"`
}

// ToBackendType преобразует BackendEngine в BackendType.
// EngineOllamaAPI → BackendTypeOllama, EngineLlamaCPP → BackendTypeLlamaCpp,
// остальные (Auto, пустой) → пустая строка (требует явного fallback).
func (e BackendEngine) ToBackendType() BackendType {
	switch e {
	case EngineOllamaAPI:
		return BackendTypeOllama
	case EngineLlamaCPP:
		return BackendTypeLlamaCpp
	case EngineImageCPP:
		return BackendTypeImage
	default:
		return ""
	}
}

// DefaultLlamaCppConfig возвращает конфигурацию llama.cpp по умолчанию
func DefaultLlamaCppConfig() *LlamaCppConfig {
	return &LlamaCppConfig{
		GrpcPort:          19000,
		NumGPULayers:      -1, // все слои на GPU
		ContextLength:     2048,
		BatchSize:         512,
		FlashAttention:    false,
		NUMA:              false,
		UseMMap:           true,
		UseMLock:          false,
		MainGPU:           0,
		MaxConcurrentReqs: 10,
		Strategy:          "vram-ratio",

		NThreads:        0, // auto
		RopeFreqBase:    10000.0,
		RopeFreqScale:   1.0,
		NoKVOffload:     false,
		KVCacheType:     "f16",
		RMSNormEps:      1e-5,
		RopeScalingType: "none",
		NoMemoryMap:     false,
		RPCBackend:      "cuda",
	}
}

// R112 (2026-10-11): РАЗДЕЛЕНИЕ ПО ТИПУ БЭКЕНДА УБРАНО.
//
// Раньше operatingMode определял, какой ТИП бэкенда допустим: virtual_router и
// distributed_inference разрешали ТОЛЬКО llama_cpp, остальные — оба текстовых
// типа. Из-за этого llama.cpp и Ollama не могли работать в одном кластере:
// второй тип не регистрировался (валидация), отфильтровывался (allowedTypes) или
// деактивировался при смене режима. Живое следствие — cppworker с Ollama-
// поверхностью (/api/*) и Ollama-нода рядом не уживались, хотя это ровно тот
// сценарий, ради которого балансер существует: «Ollama API должно работать
// одинаково для всех клиентов, независимо от того, кто обслуживает запрос».
//
// Новое правило: ТИП БЭКЕНДА — свойство БЭКЕНДА, а не режима. Режим управляет
// только своими функциональными блоками (replication / rpc_coordinator /
// virtual_models / dist_inference). Любой режим совместим с любым типом, а
// фактический состав кластера определяется зарегистрированными бэкендами
// (см. balancer.getBackendEngine / getEffectiveBackendType).
//
// Массив один и тот же для всех режимов намеренно: он read-only (никто его не
// мутирует — только читает и фильтрует), а разные литералы неизбежно разошлись
// бы при следующем добавлении типа.
var allModeBackendTypes = []BackendType{BackendTypeOllama, BackendTypeLlamaCpp, BackendTypeImage}

// ModeBackendTypes — маппинг OperatingMode → допустимые типы бэкендов.
// С R112 у всех режимов один и тот же набор (см. комментарий выше).
var ModeBackendTypes = map[string][]BackendType{
	"standard":              allModeBackendTypes,
	"replication":           allModeBackendTypes,
	"rpc_coordinator":       allModeBackendTypes,
	"virtual_router":        allModeBackendTypes,
	"distributed_inference": allModeBackendTypes,
}

// ModeEngines — маппинг OperatingMode → движок инференса.
//
// R112: у всех режимов EngineAuto. Режим больше НЕ назначает движок — иначе
// переключение режима меняло бы правила инференса (n_ctx, KV-типы, offload) для
// бэкендов, которые к этому режиму отношения не имеют. Движок определяется
// составом кластера: только llama.cpp → EngineLlamaCPP, только Ollama →
// EngineOllamaAPI, оба типа → EngineAuto (правила одного движка не навязываются
// чужому пулу).
var ModeEngines = map[string]BackendEngine{
	"standard":              EngineAuto,
	"replication":           EngineAuto,
	"rpc_coordinator":       EngineAuto,
	"virtual_router":        EngineAuto,
	"distributed_inference": EngineAuto,
}

// IsModeCompatibleWithBackendType проверяет совместимость режима и типа бэкенда
func IsModeCompatibleWithBackendType(mode string, bt BackendType) bool {
	allowed, ok := ModeBackendTypes[mode]
	if !ok {
		// Неизвестный режим — разрешаем оба типа
		return true
	}
	for _, t := range allowed {
		if t == bt {
			return true
		}
	}
	return false
}

// ResolveEngine определяет эффективный движок по типу бэкенда.
// Если engine == EngineAuto или пустой — вычисляет по BackendType.
func ResolveEngine(engine BackendEngine, bt BackendType) BackendEngine {
	if engine != "" && engine != EngineAuto {
		return engine
	}
	switch bt {
	case BackendTypeLlamaCpp:
		return EngineLlamaCPP
	case BackendTypeImage:
		return EngineImageCPP
	default:
		return EngineOllamaAPI
	}
}

// IsModeOllama возвращает true, если режим ЖЁСТКО привязан к Ollama.
//
// R112: с момента унификации такой привязки нет ни у одного режима (ModeEngines
// везде EngineAuto), поэтому функция всегда возвращает false для штатных
// режимов. Оставлена как публичный контракт для внешних вызывающих и для
// будущих режимов с явной привязкой; НЕ используйте её для выбора типа бэкенда —
// фактический состав кластера даёт balancer.getBackendEngine().
func IsModeOllama(mode string) bool {
	engine, ok := ModeEngines[mode]
	if !ok {
		return false
	}
	return engine == EngineOllamaAPI
}

// IsModeLlamaCpp возвращает true, если режим ЖЁСТКО привязан к llama.cpp.
// R112: см. IsModeOllama — штатные режимы больше не привязывают движок.
func IsModeLlamaCpp(mode string) bool {
	engine, ok := ModeEngines[mode]
	if !ok {
		return false
	}
	return engine == EngineLlamaCPP
}

// IsModeImage возвращает true, если режим жёстко привязан к image-движку.
// R-Image (2026-09-27): сейчас ни один OperatingMode не является image-only,
// поэтому функция нужна для симметрии с IsModeOllama/IsModeLlamaCpp и для
// будущих режимов (например image_tool_loop).
func IsModeImage(mode string) bool {
	engine, ok := ModeEngines[mode]
	if !ok {
		return false
	}
	return engine == EngineImageCPP
}

// AllBackendTypes возвращает список всех типов бэкендов
func AllBackendTypes() []BackendType {
	return []BackendType{BackendTypeOllama, BackendTypeLlamaCpp, BackendTypeImage}
}

// BackendTypeLabel возвращает человекочитаемое название типа бэкенда
func (bt BackendType) Label() string {
	switch bt {
	case BackendTypeOllama:
		return "Ollama"
	case BackendTypeLlamaCpp:
		return "llama.cpp"
	case BackendTypeImage:
		return "image.cpp"
	default:
		return string(bt)
	}
}

// BackendTypeEmoji возвращает emoji для типа бэкенда
func (bt BackendType) Emoji() string {
	switch bt {
	case BackendTypeOllama:
		return "🦙"
	case BackendTypeLlamaCpp:
		return "🦒"
	case BackendTypeImage:
		return "🎨"
	default:
		return "❓"
	}
}
