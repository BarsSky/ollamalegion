package types

// BackendType — тип бэкенда (ollama или llama.cpp)
type BackendType string

const (
	BackendTypeOllama   BackendType = "ollama"
	BackendTypeLlamaCpp BackendType = "llama_cpp"
)

// BackendEngine — движок инференса
type BackendEngine string

const (
	EngineOllamaAPI BackendEngine = "ollama_api" // HTTP REST API Ollama
	EngineLlamaCPP  BackendEngine = "llama_cpp"  // llama.cpp через gRPC/HTTP
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
	RopeScalingType   string  `json:"ropeScalingType"`   // none / linear / yarn
	RopeScalingFactor float64 `json:"ropeScalingFactor"`
	YarnExtFactor     float64 `json:"yarnExtFactor"`
	YarnAttnFactor    float64 `json:"yarnAttnFactor"`
	YarnBetaFast      float64 `json:"yarnBetaFast"`
	YarnBetaSlow      float64 `json:"yarnBetaSlow"`

	// Дополнительные
	NoMemoryMap    bool   `json:"noMemoryMap"`    // отключить mmap
	RPCBackend     string `json:"rpcBackend"`     // backend для RPC (cuda/vulkan/kompute)
	ModelURL       string `json:"modelUrl"`       // URL для скачивания модели
	ChatTemplate   string `json:"chatTemplate"`   // шаблон чата
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
	default:
		return ""
	}
}

// DefaultLlamaCppConfig возвращает конфигурацию llama.cpp по умолчанию
func DefaultLlamaCppConfig() *LlamaCppConfig {
	return &LlamaCppConfig{
		GrpcPort:         19000,
		NumGPULayers:     -1, // все слои на GPU
		ContextLength:    2048,
		BatchSize:        512,
		FlashAttention:   false,
		NUMA:             false,
		UseMMap:          true,
		UseMLock:         false,
		MainGPU:          0,
		MaxConcurrentReqs: 10,
		Strategy:         "vram-ratio",

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

// ModeBackendTypes — маппинг OperatingMode → допустимые типы бэкендов
var ModeBackendTypes = map[string][]BackendType{
	"standard":              {BackendTypeOllama, BackendTypeLlamaCpp},
	"replication":           {BackendTypeOllama, BackendTypeLlamaCpp},
	"rpc_coordinator":       {BackendTypeOllama, BackendTypeLlamaCpp},
	"virtual_router":        {BackendTypeLlamaCpp},
	"distributed_inference": {BackendTypeLlamaCpp},
}

// ModeEngines — маппинг OperatingMode → движок инференса
var ModeEngines = map[string]BackendEngine{
	"standard":              EngineAuto,
	"replication":           EngineAuto,
	"rpc_coordinator":       EngineAuto,
	"virtual_router":        EngineLlamaCPP,
	"distributed_inference": EngineLlamaCPP,
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
	default:
		return EngineOllamaAPI
	}
}

// IsModeOllama возвращает true, если режим использует Ollama
func IsModeOllama(mode string) bool {
	engine, ok := ModeEngines[mode]
	if !ok {
		return true // по умолчанию считаем Ollama
	}
	return engine == EngineOllamaAPI
}

// IsModeLlamaCpp возвращает true, если режим использует llama.cpp
func IsModeLlamaCpp(mode string) bool {
	engine, ok := ModeEngines[mode]
	if !ok {
		return false
	}
	return engine == EngineLlamaCPP
}

// AllBackendTypes возвращает список всех типов бэкендов
func AllBackendTypes() []BackendType {
	return []BackendType{BackendTypeOllama, BackendTypeLlamaCpp}
}

// BackendTypeLabel возвращает человекочитаемое название типа бэкенда
func (bt BackendType) Label() string {
	switch bt {
	case BackendTypeOllama:
		return "Ollama"
	case BackendTypeLlamaCpp:
		return "llama.cpp"
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
	default:
		return "❓"
	}
}