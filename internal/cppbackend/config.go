// Package cppbackend — конфигурация для CppBackend
package cppbackend

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config — расширенная конфигурация CppBackend
type Config struct {
	// HTTP сервер
	Port        int    `json:"port"`
	Host        string `json:"host"`
	MetricsPort int    `json:"metricsPort,omitempty"`

	// Директории
	ModelsDir    string `json:"modelsDir"`
	DownloadsDir string `json:"downloadsDir,omitempty"`
	CacheDir     string `json:"cacheDir,omitempty"`

	// Параметры по умолчанию
	DefaultCtxSize    int  `json:"defaultCtxSize"`
	DefaultBatchSize  int  `json:"defaultBatchSize"`
	DefaultGPULayers  int  `json:"defaultGpuLayers"`
	DefaultFlashAttnType int  `json:"defaultFlashAttnType"`
	DefaultNUMA          bool `json:"defaultNuma"`
	DefaultUseMmap    bool `json:"defaultUseMmap"`
	DefaultUseMlock   bool `json:"defaultUseMlock"`

	// Phase 8 P.4 (2026-07-11): multi-GPU tensor_split + split_mode defaults.
	// DefaultTensorSplit — массив пропорций (e.g. [0.5, 0.5] для 2 GPU).
	// nil/empty = auto (llama.cpp решает по VRAM). Применяется при n_gpu_layers > 0.
	// DefaultSplitMode — -1=default (LAYER), 0=NONE, 1=LAYER, 2=ROW, 3=TENSOR.
	// См. также docs/phase-8-p3-research.md.
	DefaultTensorSplit []float32 `json:"defaultTensorSplit,omitempty"`
	DefaultSplitMode   int       `json:"defaultSplitMode"`

	// Параметры потоков CPU
	DefaultNThreads int `json:"defaultNThreads"` // 0 = auto

	// Round 12 (2026-07-28): n_parallel — число параллельных sequences
	// для batched generation. 0 = дефолт llama.cpp (=1), 1 = явное n_parallel=1,
	// 2..8 = multi-slot batched generation (требует больше VRAM для KV-cache).
	// ВАЖНО: даже при NParallel>1 инференс пока СЕРИАЛИЗУЕТСЯ через Round 8
	// inst.mu lock (backend.go:1319-1379) — для настоящего параллельного
	// инференса требуется ещё C-bridge slot pool (Round 13+, future work).
	DefaultNParallel int `json:"defaultNParallel"` // 0 = bridge default (1)

	// RoPE параметры контекста
	DefaultRopeFreqBase     float64 `json:"defaultRopeFreqBase"`
	DefaultRopeFreqScale    float64 `json:"defaultRopeFreqScale"`
	DefaultRopeScalingType  string  `json:"defaultRopeScalingType"`  // none / linear / yarn
	DefaultRopeScalingFactor float64 `json:"defaultRopeScalingFactor"`

	// YaRN параметры
	DefaultYarnExtFactor  float64 `json:"defaultYarnExtFactor"`
	DefaultYarnAttnFactor float64 `json:"defaultYarnAttnFactor"`
	DefaultYarnBetaFast   float64 `json:"defaultYarnBetaFast"`
	DefaultYarnBetaSlow   float64 `json:"defaultYarnBetaSlow"`

	// KV Cache
	DefaultNoKVOffload bool   `json:"defaultNoKvOffload"`
	DefaultKVCacheType string `json:"defaultKvCacheType"` // f16 / f32 / q8_0 / q4_0

	// Параметры нормализации
	DefaultRMSNormEps float64 `json:"defaultRmsNormEps"`

	// Мulti-GPU
	AutoGPUDistribution bool   `json:"autoGpuDistribution"`
	TensorSplitStrategy  string `json:"tensorSplitStrategy"`  // "auto", "vram-ratio", "manual"
	DefaultMainGPU       int    `json:"defaultMainGpu"`
	DefaultRPCBackend    string `json:"defaultRpcBackend"`    // cuda / vulkan / kompute
	DefaultNoMemoryMap   bool   `json:"defaultNoMemoryMap"`   // отключить mmap для конкретных моделей

	// Session 18 (2026-07-28): Включает reasoning-режим для моделей которые
	// поддерживают thinking (gemma-4, deepseek-r1, qwen3-thinking, и т.п.).
	// При включении — к chat template добавляется thinking_mode и парсер
	// извлекает think-блоки в отдельное поле `reasoning_content`.
	DefaultEnableReasoning bool  `json:"defaultEnableReasoning"`
	DefaultReasoningBudget  int   `json:"defaultReasoningBudget"` // max tokens for thinking (0 = no limit)

	// HuggingFace
	HuggingFaceToken string `json:"huggingFaceToken,omitempty"`
	HFMirror         string `json:"hfMirror,omitempty"` // e.g. https://hf-mirror.com

	// Метрики
	EnableMetrics     bool `json:"enableMetrics"`
	MetricsRetentionS int  `json:"metricsRetentionSeconds"`

	// Session 18 (2026-07-28): Reasoning/Thinking для моделей gemma-4,
	// deepseek-r1, qwen3-thinking и др. При включении — chat template
	// получает thinking_mode и парсер reasoning_content.go извлекает
	// think-блоки в отдельное поле. ReasoningBudget — max токенов на
	// размышления (0 = без лимита).
	EnableReasoning bool `json:"enableReasoning"`
	ReasoningBudget int  `json:"reasoningBudget"`

	// Round 15.1 (2026-07-30): opt-in флаг для batched parallel inference.
	// При false (default) используется Round 13 multi-slot path с serialized
	// llama_decode через inst.mu — backward compat. При true Backend создаёт
	// BatchedScheduler для каждой загруженной модели и маршрутизирует
	// streaming inference через него (ОДИН llama_decode call для N concurrent
	// requests → real GPU matmul sharing).
	//
	// CAVEAT: BatchedScheduler (Round 15.1) использует GREEDY argmax sampling
	// — temperature/top_p/top_k НЕ применяются. Это regression vs Round 13.
	// Round 15.2 добавит полный Sampler chain. Use case: best for short
	// classification/extraction workloads, не для creative generation.
	EnableBatchedParallel bool `json:"enableBatchedParallel"`

	// IdleUnloadMinutes — автоматическая выгрузка моделей из VRAM после N минут простоя.
	// 0 (по умолчанию) = автовыгрузка ВЫКЛЮЧЕНА. Модель держится в VRAM, пока:
	//   - пользователь явно не вызовет /api/models/unload или /api/profiles/* unload,
	//   - или не выключит контейнер,
	//   - или не запросит другую модель (LRU-eviction).
	// Раньше IdleUnloadManager использовал MetricsRetentionS (по дефолту 3600 секунд =
	// 60 минут) как idleTimeout, что путало метрики с автовыгрузкой и иногда вызывало
	// выгрузку через час, даже если пользователь этого не хотел.
	IdleUnloadMinutes int `json:"idleUnloadMinutes"`

	// HuggingFace CLI
	HuggingFaceCLIPath string `json:"huggingFaceCliPath,omitempty"` // путь к huggingface-cli
}

// DefaultConfig возвращает конфигурацию по умолчанию
func DefaultConfig() Config {
	return Config{
		Port:              18091,
		Host:              "0.0.0.0",
		MetricsPort:       18092,
		ModelsDir:         "./models",
		DownloadsDir:      "./downloads",
		CacheDir:          "./cache",
		// DefaultCtxSize = 32768: достаточно для длинных диалогов
		// (system + history ~7000 токенов + user message) с запасом
		// ~25000 токенов на ответ. Предыдущее значение 8192 было
		// минимумом для OpenWebUI с tools, но для production-использования
		// с длинными сессиями этого недостаточно.
		// Можно override через CPPWORKER_CTX_SIZE, CPPWORKER_DEFAULT_CTX_SIZE
		// или config/cppworker-defaults.json.
		DefaultCtxSize:    32768,
		DefaultBatchSize:  512,
		DefaultGPULayers:  -1, // все слои на GPU
		DefaultFlashAttnType:  -1,
		DefaultNUMA:         false,
		DefaultUseMmap:    true,
		DefaultUseMlock:   false,

		DefaultNThreads:         0, // auto
		DefaultNParallel:        0, // 0 = bridge default (1); >0 = multi-slot batched
		DefaultRopeFreqBase:     10000.0,
		DefaultRopeFreqScale:    1.0,
		DefaultRopeScalingType:  "none",
		DefaultRopeScalingFactor: 1.0,

		DefaultYarnExtFactor:  1.0,
		DefaultYarnAttnFactor: 1.0,
		DefaultYarnBetaFast:   32.0,
		DefaultYarnBetaSlow:   1.0,

		DefaultNoKVOffload: false,
		DefaultKVCacheType: "f16",
		DefaultRMSNormEps:  1e-5,

		AutoGPUDistribution: true,
		TensorSplitStrategy: "vram-ratio",
		DefaultMainGPU:      0,
		DefaultEnableReasoning: false, // по умолчанию выключено; пользователь включает per-model
		DefaultReasoningBudget: 0,     // 0 = без лимита; иначе макс. токенов на thinking
		DefaultRPCBackend:   "cuda",
		DefaultNoMemoryMap:  false,

		EnableMetrics:     true,
		MetricsRetentionS: 3600,
		EnableReasoning:    false, // Session 18: по умолчанию выключено
		ReasoningBudget:    0,     // 0 = без лимита на thinking-токены

		// Round 15.1: opt-in. Greedy argmax пока — НЕ для production
		// creative generation, только для тестов / classification workloads.
		EnableBatchedParallel: false,
		// IdleUnloadMinutes: 0 = автовыгрузка моделей ВЫКЛЮЧЕНА по умолчанию.
		// Если нужна автоматическая выгрузка после простоя, задайте явно:
		//   export CPPWORKER_IDLE_UNLOAD_MINUTES=30
		// или через JSON-конфиг: {"idleUnloadMinutes": 30}.
		IdleUnloadMinutes: 0,
	}
}

// LoadConfigFromFile загружает конфигурацию из JSON-файла.
// Файл config/cppworker-defaults.json — единый источник истины
// для дефолтных параметров. Если файл не найден — возвращаем DefaultConfig().
func LoadConfigFromFile(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // файла нет — используем хардкод-дефолты
		}
		return cfg, fmt.Errorf("read config file %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config file %s: %w", path, err)
	}
	return cfg, nil
}

// LoadConfigFromEnv загружает конфигурацию из переменных окружения.
// Приоритет: env-переменные > config/cppworker-defaults.json > DefaultConfig().
func LoadConfigFromEnv() Config {
	// 1. Загружаем defaults из JSON-файла (единый источник истины)
	cfg, err := LoadConfigFromFile("config/cppworker-defaults.json")
	if err != nil {
		// Логгируем предупреждение, но продолжаем с DefaultConfig
		fmt.Fprintf(os.Stderr, "[cppbackend] WARNING: failed to load config/cppworker-defaults.json: %v — using hardcoded defaults\n", err)
		cfg = DefaultConfig()
	}

	if v := os.Getenv("CPPWORKER_PORT"); v != "" {
		cfg.Port = parseInt(v, cfg.Port)
	}
	if v := os.Getenv("CPPWORKER_HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("CPPWORKER_METRICS_PORT"); v != "" {
		cfg.MetricsPort = parseInt(v, cfg.MetricsPort)
	}
	if v := os.Getenv("CPPWORKER_MODELS_DIR"); v != "" {
		cfg.ModelsDir = v
	}
	if v := os.Getenv("CPPWORKER_DOWNLOADS_DIR"); v != "" {
		cfg.DownloadsDir = v
	}
	if v := os.Getenv("CPPWORKER_CACHE_DIR"); v != "" {
		cfg.CacheDir = v
	}
	if v := os.Getenv("CPPWORKER_CTX_SIZE"); v != "" {
		cfg.DefaultCtxSize = parseInt(v, cfg.DefaultCtxSize)
	}
	// Alias: CPPWORKER_DEFAULT_CTX_SIZE — для совместимости с
	// .env-файлами, где пользователь ожидает именно "default" ctx size.
	if v := os.Getenv("CPPWORKER_DEFAULT_CTX_SIZE"); v != "" {
		cfg.DefaultCtxSize = parseInt(v, cfg.DefaultCtxSize)
	}
	if v := os.Getenv("CPPWORKER_BATCH_SIZE"); v != "" {
		cfg.DefaultBatchSize = parseInt(v, cfg.DefaultBatchSize)
	}
	if v := os.Getenv("CPPWORKER_GPU_LAYERS"); v != "" {
		cfg.DefaultGPULayers = parseInt(v, cfg.DefaultGPULayers)
	}
	if v := os.Getenv("CPPWORKER_FLASH_ATTN_TYPE"); v != "" {
		cfg.DefaultFlashAttnType = parseInt(v, cfg.DefaultFlashAttnType)
	} else if v := os.Getenv("CPPWORKER_FLASH_ATTN"); v != "" {
		// Legacy alias: CPPWORKER_FLASH_ATTN=0 → disabled, 1 → enabled (type 1)
		switch strings.ToLower(v) {
		case "0", "false", "no", "off":
			cfg.DefaultFlashAttnType = 0
		case "1", "true", "yes", "on":
			cfg.DefaultFlashAttnType = 1
		default:
			cfg.DefaultFlashAttnType = parseInt(v, cfg.DefaultFlashAttnType)
		}
	}
	if v := os.Getenv("CPPWORKER_NUMA"); v != "" {
		cfg.DefaultNUMA = v == "1" || strings.ToLower(v) == "true"
	}
	if v := os.Getenv("CPPWORKER_USE_MMAP"); v != "" {
		cfg.DefaultUseMmap = v == "1" || strings.ToLower(v) == "true"
	}
	if v := os.Getenv("CPPWORKER_USE_MLOCK"); v != "" {
		cfg.DefaultUseMlock = v == "1" || strings.ToLower(v) == "true"
	}

	// Потоки CPU
	if v := os.Getenv("CPPWORKER_N_THREADS"); v != "" {
		cfg.DefaultNThreads = parseInt(v, cfg.DefaultNThreads)
	}

	// Round 12: n_parallel для batched generation.
	// 0 = bridge default (=1), 1..8 = multi-slot batched. -1 в env = 0 (legacy).
	if v := os.Getenv("CPPWORKER_N_PARALLEL"); v != "" {
		if n := parseInt(v, cfg.DefaultNParallel); n >= 0 {
			cfg.DefaultNParallel = n
		}
	}

	// RoPE параметры
	if v := os.Getenv("CPPWORKER_ROPE_FREQ_BASE"); v != "" {
		cfg.DefaultRopeFreqBase = parseFloat(v, cfg.DefaultRopeFreqBase)
	}
	if v := os.Getenv("CPPWORKER_ROPE_FREQ_SCALE"); v != "" {
		cfg.DefaultRopeFreqScale = parseFloat(v, cfg.DefaultRopeFreqScale)
	}
	if v := os.Getenv("CPPWORKER_ROPE_SCALING_TYPE"); v != "" {
		cfg.DefaultRopeScalingType = v
	}
	if v := os.Getenv("CPPWORKER_ROPE_SCALING_FACTOR"); v != "" {
		cfg.DefaultRopeScalingFactor = parseFloat(v, cfg.DefaultRopeScalingFactor)
	}

	// YaRN
	if v := os.Getenv("CPPWORKER_YARN_EXT_FACTOR"); v != "" {
		cfg.DefaultYarnExtFactor = parseFloat(v, cfg.DefaultYarnExtFactor)
	}
	if v := os.Getenv("CPPWORKER_YARN_ATTN_FACTOR"); v != "" {
		cfg.DefaultYarnAttnFactor = parseFloat(v, cfg.DefaultYarnAttnFactor)
	}
	if v := os.Getenv("CPPWORKER_YARN_BETA_FAST"); v != "" {
		cfg.DefaultYarnBetaFast = parseFloat(v, cfg.DefaultYarnBetaFast)
	}
	if v := os.Getenv("CPPWORKER_YARN_BETA_SLOW"); v != "" {
		cfg.DefaultYarnBetaSlow = parseFloat(v, cfg.DefaultYarnBetaSlow)
	}

	// KV Cache
	if v := os.Getenv("CPPWORKER_NO_KV_OFFLOAD"); v != "" {
		cfg.DefaultNoKVOffload = v == "1" || strings.ToLower(v) == "true"
	}
	if v := os.Getenv("CPPWORKER_KV_CACHE_TYPE"); v != "" {
		cfg.DefaultKVCacheType = v
	}

	// Нормализация
	if v := os.Getenv("CPPWORKER_RMS_NORM_EPS"); v != "" {
		cfg.DefaultRMSNormEps = parseFloat(v, cfg.DefaultRMSNormEps)
	}

	// Multi-GPU
	if v := os.Getenv("CPPWORKER_MAIN_GPU"); v != "" {
		cfg.DefaultMainGPU = parseInt(v, cfg.DefaultMainGPU)
	}
	if v := os.Getenv("CPPWORKER_RPC_BACKEND"); v != "" {
		cfg.DefaultRPCBackend = v
	}
	if v := os.Getenv("CPPWORKER_NO_MEMORY_MAP"); v != "" {
		cfg.DefaultNoMemoryMap = v == "1" || strings.ToLower(v) == "true"
	}

	// HuggingFace
	if v := os.Getenv("HF_TOKEN"); v != "" {
		cfg.HuggingFaceToken = v
	}
	if v := os.Getenv("HF_MIRROR"); v != "" {
		cfg.HFMirror = v
	}

	// GPU Distribution
	if v := os.Getenv("CPPWORKER_AUTO_GPU_DIST"); v != "" {
		cfg.AutoGPUDistribution = v == "1" || strings.ToLower(v) == "true"
	}
	if v := os.Getenv("CPPWORKER_TENSOR_SPLIT_STRATEGY"); v != "" {
		cfg.TensorSplitStrategy = v
	}

	// Метрики
	if v := os.Getenv("CPPWORKER_ENABLE_METRICS"); v != "" {
		cfg.EnableMetrics = v == "1" || strings.ToLower(v) == "true"
	}

	// Idle unload (автовыгрузка моделей).
	// Значение по умолчанию 0 = ВЫКЛЮЧЕНО. Если задано N — модели выгружаются
	// после N минут простоя (отсчёт от последнего использования).
	if v := os.Getenv("CPPWORKER_IDLE_UNLOAD_MINUTES"); v != "" {
		cfg.IdleUnloadMinutes = parseInt(v, cfg.IdleUnloadMinutes)
	}

	// Round 15.1: opt-in флаг для batched parallel inference. Default = false
	// (Round 13 multi-slot path — backward compat). Включение — только для
	// workloads где greedy argmax acceptable.
	if v := os.Getenv("CPPWORKER_ENABLE_BATCHED_PARALLEL"); v != "" {
		cfg.EnableBatchedParallel = v == "1" || strings.ToLower(v) == "true"
	}

	return cfg
}

// Validate проверяет конфигурацию на корректность
func (c Config) Validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port: %d", c.Port)
	}
	if c.ModelsDir == "" {
		return fmt.Errorf("modelsDir is required")
	}
	if c.DefaultCtxSize < 256 {
		return fmt.Errorf("defaultCtxSize must be >= 256, got %d", c.DefaultCtxSize)
	}
	if c.DefaultBatchSize < 1 {
		return fmt.Errorf("defaultBatchSize must be >= 1, got %d", c.DefaultBatchSize)
	}
	return nil
}

// LoadDotEnvFile загружает переменные из .env файла в окружение.
// Поддерживает:
//   - Комментарии (строки, начинающиеся с #)
//   - Пустые строки
//   - Значения с = и без кавычек
//   - Маппинг LLAMA_* → CPPWORKER_* для совместимости с LoadConfigFromEnv
func LoadDotEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Удаляем инлайн-комментарии (если # после пробела)
		if idx := strings.Index(line, " #"); idx > 0 {
			line = line[:idx]
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Уже установленные переменные не перезаписываем
		if os.Getenv(key) != "" {
			continue
		}
		os.Setenv(key, value)

		// Маппинг LLAMA_* → CPPWORKER_* для LoadConfigFromEnv
		mappedKey := mapLlamaToCppWorkerEnv(key)
		if mappedKey != "" && os.Getenv(mappedKey) == "" {
			os.Setenv(mappedKey, value)
		}
	}
	return scanner.Err()
}

// mapLlamaToCppWorkerEnv — маппинг переменных LLAMA_* → CPPWORKER_*
func mapLlamaToCppWorkerEnv(key string) string {
	mapping := map[string]string{
		"LLAMA_HTTP_PORT":        "CPPWORKER_PORT",
		"LLAMA_WRITE_TIMEOUT":    "CPPWORKER_WRITE_TIMEOUT",
		"LLAMA_GRPC_PORT":        "", // grpc порт отдельно
		"LLAMA_MODELS_DIR":       "CPPWORKER_MODELS_DIR",
		"LLAMA_CTX_SIZE":         "CPPWORKER_CTX_SIZE",
		"LLAMA_BATCH_SIZE":       "CPPWORKER_BATCH_SIZE",
		"LLAMA_N_GPU_LAYERS":     "CPPWORKER_GPU_LAYERS",
		"LLAMA_FLASH_ATTN":       "CPPWORKER_FLASH_ATTN",
		"LLAMA_NUMA":             "CPPWORKER_NUMA",
		"LLAMA_MMAP":             "CPPWORKER_USE_MMAP",
		"LLAMA_MLOCK":            "CPPWORKER_USE_MLOCK",
		"LLAMA_MAIN_GPU":         "CPPWORKER_MAIN_GPU",
		"LLAMA_THREADS":          "CPPWORKER_N_THREADS",
		"LLAMA_ROPE_FREQ_BASE":   "CPPWORKER_ROPE_FREQ_BASE",
		"LLAMA_ROPE_FREQ_SCALE":  "CPPWORKER_ROPE_FREQ_SCALE",
		"LLAMA_CACHE_TYPE_K":     "CPPWORKER_KV_CACHE_TYPE",
		"LLAMA_CACHE_TYPE_V":     "",
		"LLAMA_RMS_NORM_EPS":     "CPPWORKER_RMS_NORM_EPS",
		"LLAMA_TENSOR_SPLIT":     "",
		"LLAMA_IDLE_UNLOAD":      "",
		"LLAMA_ENFORCE_REPLICATION": "",
		"NODE_NAME":              "",
		"BALANCER_URL":           "",
		"AGENT_ENABLED":          "",
		"AGENT_PORT":             "",
		"AGENT_COLLECT_INTERVAL": "",
		"AGENT_HEARTBEAT_INTERVAL": "",
		"AGENT_TIMEOUT":          "",
		"API_TOKEN":              "",
		"RPC_COORDINATOR_URL":    "",
		"RPC_WORKER_PORT":        "",
		"NODE_LABELS":            "",
	}
	if mapped, ok := mapping[key]; ok {
		return mapped
	}
	return ""
}

// SaveConfigToDotEnv сохраняет конфигурацию в .env файл
func (c Config) SaveConfigToDotEnv(path string) error {
	var sb strings.Builder
	sb.WriteString("# CppWorker (llama.cpp) configuration\n")
	sb.WriteString(fmt.Sprintf("LLAMA_HTTP_PORT=%d\n", c.Port))
	sb.WriteString(fmt.Sprintf("LLAMA_MODELS_DIR=%s\n", c.ModelsDir))
	sb.WriteString(fmt.Sprintf("LLAMA_CTX_SIZE=%d\n", c.DefaultCtxSize))
	sb.WriteString(fmt.Sprintf("LLAMA_BATCH_SIZE=%d\n", c.DefaultBatchSize))
	sb.WriteString(fmt.Sprintf("LLAMA_N_GPU_LAYERS=%d\n", c.DefaultGPULayers))
	sb.WriteString(fmt.Sprintf("LLAMA_FLASH_ATTN_TYPE=%d\n", c.DefaultFlashAttnType))
	sb.WriteString(fmt.Sprintf("LLAMA_NUMA=%v\n", c.DefaultNUMA))
	sb.WriteString(fmt.Sprintf("LLAMA_MMAP=%v\n", c.DefaultUseMmap))
	sb.WriteString(fmt.Sprintf("LLAMA_MLOCK=%v\n", c.DefaultUseMlock))
	sb.WriteString(fmt.Sprintf("LLAMA_MAIN_GPU=%d\n", c.DefaultMainGPU))
	sb.WriteString(fmt.Sprintf("LLAMA_THREADS=%d\n", c.DefaultNThreads))
	sb.WriteString(fmt.Sprintf("LLAMA_RMS_NORM_EPS=%f\n", c.DefaultRMSNormEps))
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

func parseInt(s string, defaultVal int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return v
}

func parseFloat(s string, defaultVal float64) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return defaultVal
	}
	return v
}
