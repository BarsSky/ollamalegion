// Package cppbackend — конфигурация для CppBackend
package cppbackend

import (
	"bufio"
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

	// Параметры потоков CPU
	DefaultNThreads int `json:"defaultNThreads"` // 0 = auto

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

	// HuggingFace
	HuggingFaceToken string `json:"huggingFaceToken,omitempty"`
	HFMirror         string `json:"hfMirror,omitempty"` // e.g. https://hf-mirror.com

	// Метрики
	EnableMetrics     bool `json:"enableMetrics"`
	MetricsRetentionS int  `json:"metricsRetentionSeconds"`

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
		// DefaultCtxSize = 8192 (а не 4096): при 4096 у OpenWebUI с tools
		// (system + tool definitions ~3000-5000 токенов + user message)
		// prompt не влезает → reload на каждой tool-итерации.
		// 8192 — минимум для стабильной работы OpenWebUI с tools.
		// Можно override через CPPWORKER_CTX_SIZE или CPPWORKER_DEFAULT_CTX_SIZE.
		DefaultCtxSize:    8192,
		DefaultBatchSize:  512,
		DefaultGPULayers:  -1, // все слои на GPU
		DefaultFlashAttnType:  -1,
		DefaultNUMA:         false,
		DefaultUseMmap:    true,
		DefaultUseMlock:   false,

		DefaultNThreads:         0, // auto
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
		DefaultRPCBackend:   "cuda",
		DefaultNoMemoryMap:  false,

		EnableMetrics:     true,
		MetricsRetentionS: 3600,
		// IdleUnloadMinutes: 0 = автовыгрузка моделей ВЫКЛЮЧЕНА по умолчанию.
		// Если нужна автоматическая выгрузка после простоя, задайте явно:
		//   export CPPWORKER_IDLE_UNLOAD_MINUTES=30
		// или через JSON-конфиг: {"idleUnloadMinutes": 30}.
		IdleUnloadMinutes: 0,
	}
}

// LoadConfigFromEnv загружает конфигурацию из переменных окружения
func LoadConfigFromEnv() Config {
	cfg := DefaultConfig()

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
