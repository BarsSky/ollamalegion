package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// Config - основной struct для работы с конфигурацией
type Config struct {
	path string
	data *types.LoadBalancerConfig
}

// Load - загрузка конфигурации из файла
func Load(path string) (*Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve config path: %w", err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config types.LoadBalancerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Установка значений по умолчанию
	setDefaults(&config)

	return &Config{
		path: absPath,
		data: &config,
	}, nil
}

// LoadFromEnv - загрузка конфигурации из переменных окружения
func LoadFromEnv() (*Config, error) {
	config := &types.LoadBalancerConfig{}
	
	// LoadBalancer settings
	config.LoadBalancer.Host = getEnv("LB_HOST", "0.0.0.0")
	config.LoadBalancer.Port = getEnvInt("LB_PORT", 18080)
	config.LoadBalancer.APIPort = getEnvInt("LB_API_PORT", 18081)
	config.LoadBalancer.TLSHost = getEnv("LB_TLS_HOST", "")
	config.LoadBalancer.TLSPort = getEnvInt("LB_TLS_PORT", 8443)
	
	// TLS settings
	config.TLS.Enabled = getEnvBool("TLS_ENABLED", false)
	config.TLS.CertFile = getEnv("TLS_CERT_FILE", "certs/server.crt")
	config.TLS.KeyFile = getEnv("TLS_KEY_FILE", "certs/server.key")
	config.TLS.MinVersion = getEnv("TLS_MIN_VERSION", "TLS12")
	config.TLS.AutoCert = getEnvBool("TLS_AUTO_CERT", false)
	
	// Auth settings
	config.Auth.Enabled = getEnvBool("AUTH_ENABLED", false)
	config.Auth.Tokens = parseAuthTokensFromEnv()
	config.Auth.HeaderName = getEnv("AUTH_HEADER_NAME", "X-API-Token")
	
	// API Rate Limiting settings
	config.API.RateLimit = getEnvFloat("API_RATE_LIMIT", 100)
	config.API.RateBurst = getEnvFloat("API_RATE_BURST", 200)
	
	// Balancing settings
	config.Balancing.Algorithm = types.BalancingAlgorithm(getEnv("LB_ALGORITHM", "resource-aware"))
	config.Balancing.ModelAffinity = getEnvBool("LB_MODEL_AFFINITY", true)
	config.Balancing.SessionStickiness = getEnvBool("LB_SESSION_STICKINESS", true)
	config.Balancing.HealthCheckInterval = getEnvInt("LB_HEALTH_CHECK_INTERVAL", 10)
	config.Balancing.MetricsInterval = getEnvInt("LB_METRICS_INTERVAL", 5)
	config.Balancing.RequestTimeout = getEnvInt("LB_REQUEST_TIMEOUT", 120)
	config.Balancing.QueueTimeout = getEnvInt("LB_QUEUE_TIMEOUT", 300)
	config.Balancing.QueueMaxSize = getEnvInt("LB_QUEUE_MAX_SIZE", 100)
	
	// Resource limits
	config.Resources.GPU.MaxUsagePercent = getEnvFloat("LB_GPU_MAX_USAGE", 90.0)
	config.Resources.GPU.MaxVRAMUsagePercent = getEnvFloat("LB_GPU_MAX_VRAM", 85.0)
	config.Resources.GPU.MaxTemperature = getEnvInt("LB_GPU_MAX_TEMP", 85)
	
	config.Resources.CPU.MaxUsagePercent = getEnvFloat("LB_CPU_MAX_USAGE", 80.0)
	
	config.Resources.Memory.MaxUsagePercent = getEnvFloat("LB_MEMORY_MAX_USAGE", 85.0)
	
	config.Resources.Disk.MinFreeMB = uint64(getEnvInt("LB_DISK_MIN_FREE_MB", 10240))
	
	// Logging
	config.Logging.Level = getEnv("LB_LOG_LEVEL", "info")
	config.Logging.Format = getEnv("LB_LOG_FORMAT", "json")
	
	// Backends из переменных окружения
	backends := parseBackendsFromEnv()
	if len(backends) > 0 {
		config.Backends = backends
	}
	
	setDefaults(config)
	
	return &Config{
		path: "env",
		data: config,
	}, nil
}

// Get - получение конфигурации
func (c *Config) Get() *types.LoadBalancerConfig {
	return c.data
}

// GetBackend - получение бэкенда по ID
func (c *Config) GetBackend(id string) *types.Backend {
	for i := range c.data.Backends {
		if c.data.Backends[i].ID == id {
			return &c.data.Backends[i]
		}
	}
	return nil
}

// AddBackend - добавление бэкенда
func (c *Config) AddBackend(backend types.Backend) {
	c.data.Backends = append(c.data.Backends, backend)
}

// RemoveBackend - удаление бэкенда по ID
func (c *Config) RemoveBackend(id string) bool {
	for i, b := range c.data.Backends {
		if b.ID == id {
			c.data.Backends = append(c.data.Backends[:i], c.data.Backends[i+1:]...)
			return true
		}
	}
	return false
}

// Save - сохранение конфигурации в файл
func (c *Config) Save() error {
	if c.path == "env" {
		return fmt.Errorf("cannot save config loaded from environment")
	}
	
	data, err := json.MarshalIndent(c.data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	
	if err := os.WriteFile(c.path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	
	return nil
}

// setDefaults - установка значений по умолчанию
func setDefaults(config *types.LoadBalancerConfig) {
	// LoadBalancer defaults
	if config.LoadBalancer.Host == "" {
		config.LoadBalancer.Host = "0.0.0.0"
	}
	if config.LoadBalancer.Port == 0 {
		config.LoadBalancer.Port = 18080
	}
	if config.LoadBalancer.APIPort == 0 {
		config.LoadBalancer.APIPort = 18081
	}
	if config.LoadBalancer.TLSPort == 0 {
		config.LoadBalancer.TLSPort = 8443
	}
	
	// TLS defaults
	if config.TLS.MinVersion == "" {
		config.TLS.MinVersion = "TLS12"
	}
	if config.TLS.CertFile == "" {
		config.TLS.CertFile = "certs/server.crt"
	}
	if config.TLS.KeyFile == "" {
		config.TLS.KeyFile = "certs/server.key"
	}
	
	// Auth defaults
	if config.Auth.HeaderName == "" {
		config.Auth.HeaderName = "X-API-Token"
	}
	
	// API Rate Limiting defaults
	if config.API.RateLimit == 0 {
		config.API.RateLimit = 100 // 100 запросов в секунду
	}
	if config.API.RateBurst == 0 {
		config.API.RateBurst = 200 // burst capacity
	}
	
	// Balancing defaults
	if config.Balancing.Algorithm == "" {
		config.Balancing.Algorithm = types.AlgorithmResourceAware
	}
	if config.Balancing.HealthCheckInterval == 0 {
		config.Balancing.HealthCheckInterval = 10
	}
	if config.Balancing.MetricsInterval == 0 {
		config.Balancing.MetricsInterval = 5
	}
	if config.Balancing.RequestTimeout == 0 {
		config.Balancing.RequestTimeout = 120
	}
	if config.Balancing.QueueTimeout == 0 {
		config.Balancing.QueueTimeout = 300
	}
	if config.Balancing.QueueMaxSize == 0 {
		config.Balancing.QueueMaxSize = 100
	}
	
	// Resource limits defaults
	if config.Resources.GPU.MaxUsagePercent == 0 {
		config.Resources.GPU.MaxUsagePercent = 90.0
	}
	if config.Resources.GPU.MaxVRAMUsagePercent == 0 {
		config.Resources.GPU.MaxVRAMUsagePercent = 85.0
	}
	if config.Resources.GPU.MaxTemperature == 0 {
		config.Resources.GPU.MaxTemperature = 85
	}
	if config.Resources.CPU.MaxUsagePercent == 0 {
		config.Resources.CPU.MaxUsagePercent = 80.0
	}
	if config.Resources.Memory.MaxUsagePercent == 0 {
		config.Resources.Memory.MaxUsagePercent = 85.0
	}
	if config.Resources.Disk.MinFreeMB == 0 {
		config.Resources.Disk.MinFreeMB = 10240 // 10GB
	}
	
	// Logging defaults
	if config.Logging.Level == "" {
		config.Logging.Level = "info"
	}
	if config.Logging.Format == "" {
		config.Logging.Format = "json"
	}
	
	// Backend defaults
	for i := range config.Backends {
		if config.Backends[i].Weight == 0 {
			config.Backends[i].Weight = 1
		}
		if config.Backends[i].MaxConcurrentReqs == 0 {
			config.Backends[i].MaxConcurrentReqs = 10
		}
		if config.Backends[i].OllamaPort == 0 {
			config.Backends[i].OllamaPort = 11434
		}
		if config.Backends[i].AgentPort == 0 {
				config.Backends[i].AgentPort = 18032
			}
		config.Backends[i].Status = types.StatusStarting
	}
}

// parseBackendsFromEnv - парсинг бэкендов из переменных окружения
func parseBackendsFromEnv() []types.Backend {
	var backends []types.Backend
	
	// Ожидаем формат: BACKEND_0_ID, BACKEND_0_HOST, BACKEND_0_PORT, etc.
	for i := 0; i < 100; i++ {
		id := os.Getenv(fmt.Sprintf("BACKEND_%d_ID", i))
		if id == "" {
			continue
		}
		
		host := os.Getenv(fmt.Sprintf("BACKEND_%d_HOST", i))
		if host == "" {
			host = "localhost"
		}
		
		port := getEnvInt(fmt.Sprintf("BACKEND_%d_PORT", i), 11434)
		agentPort := getEnvInt(fmt.Sprintf("BACKEND_%d_AGENT_PORT", i), 9090)
		weight := getEnvInt(fmt.Sprintf("BACKEND_%d_WEIGHT", i), 1)
		maxReqs := getEnvInt(fmt.Sprintf("BACKEND_%d_MAX_REQS", i), 10)
		
		name := os.Getenv(fmt.Sprintf("BACKEND_%d_NAME", i))
		if name == "" {
			name = fmt.Sprintf("Backend %d", i)
		}
		
		backends = append(backends, types.Backend{
			ID:                id,
			Name:              name,
			Host:              host,
			OllamaPort:        port,
			AgentPort:         agentPort,
			Weight:            weight,
			MaxConcurrentReqs: maxReqs,
			Status:            types.StatusStarting,
		})
	}
	
	return backends
}

// parseAuthTokensFromEnv - парсинг API токенов из переменных окружения
func parseAuthTokensFromEnv() []string {
	tokensStr := os.Getenv("AUTH_TOKENS")
	if tokensStr == "" {
		return []string{}
	}
	
	// Разделение по запятой
	parts := strings.Split(tokensStr, ",")
	tokens := make([]string, 0, len(parts))
	for _, token := range parts {
		token = strings.TrimSpace(token)
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

// Вспомогательные функции для переменных окружения
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var v int
		if _, err := fmt.Sscanf(value, "%d", &v); err == nil {
			return v
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		var v float64
		if _, err := fmt.Sscanf(value, "%f", &v); err == nil {
			return v
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return value == "true" || value == "1" || value == "yes"
	}
	return defaultValue
}
