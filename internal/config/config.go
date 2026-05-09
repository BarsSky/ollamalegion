package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ollama-loadbalancer/pkg/env"
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
	config.LoadBalancer.Host = env.Get("LB_HOST", "0.0.0.0")
	config.LoadBalancer.Port = env.GetInt("LB_PORT", 18080)
	config.LoadBalancer.APIPort = env.GetInt("LB_API_PORT", 18081)
	config.LoadBalancer.TLSHost = env.Get("LB_TLS_HOST", "")
	config.LoadBalancer.TLSPort = env.GetInt("LB_TLS_PORT", 8443)
	config.LoadBalancer.StatePath = env.Get("LB_STATE_PATH", "data/state.json")
	
	// TLS settings
	config.TLS.Enabled = env.GetBool("TLS_ENABLED", false)
	config.TLS.CertFile = env.Get("TLS_CERT_FILE", "certs/server.crt")
	config.TLS.KeyFile = env.Get("TLS_KEY_FILE", "certs/server.key")
	config.TLS.MinVersion = env.Get("TLS_MIN_VERSION", "TLS12")
	config.TLS.AutoCert = env.GetBool("TLS_AUTO_CERT", false)
	
	// Auth settings
	config.Auth.Enabled = env.GetBool("AUTH_ENABLED", false)
	config.Auth.Tokens = parseAuthTokensFromEnv()
	config.Auth.HeaderName = env.Get("AUTH_HEADER_NAME", "X-API-Token")
	
	// API Rate Limiting settings
	config.API.RateLimit = env.GetFloat("API_RATE_LIMIT", 100)
	config.API.RateBurst = env.GetFloat("API_RATE_BURST", 200)
	
	// Balancing settings
	config.Balancing.Algorithm = types.BalancingAlgorithm(env.Get("LB_ALGORITHM", "resource-aware"))
	config.Balancing.ModelAffinity = env.GetBool("LB_MODEL_AFFINITY", true)
	config.Balancing.SessionStickiness = env.GetBool("LB_SESSION_STICKINESS", true)
	config.Balancing.HealthCheckInterval = env.GetInt("LB_HEALTH_CHECK_INTERVAL", 10)
	config.Balancing.MetricsInterval = env.GetInt("LB_METRICS_INTERVAL", 5)
	config.Balancing.RequestTimeout = env.GetInt("LB_REQUEST_TIMEOUT", 120)
	config.Balancing.QueueTimeout = env.GetInt("LB_QUEUE_TIMEOUT", 300)
	config.Balancing.QueueMaxSize = env.GetInt("LB_QUEUE_MAX_SIZE", 100)
	config.Balancing.QueueWorkers = env.GetInt("LB_QUEUE_WORKERS", 4)
	
	// Resource limits
	config.Resources.GPU.MaxUsagePercent = env.GetFloat("LB_GPU_MAX_USAGE", 90.0)
	config.Resources.GPU.MaxVRAMUsagePercent = env.GetFloat("LB_GPU_MAX_VRAM", 85.0)
	config.Resources.GPU.MaxTemperature = env.GetInt("LB_GPU_MAX_TEMP", 85)
	
	config.Resources.CPU.MaxUsagePercent = env.GetFloat("LB_CPU_MAX_USAGE", 80.0)
	
	config.Resources.Memory.MaxUsagePercent = env.GetFloat("LB_MEMORY_MAX_USAGE", 85.0)
	
	config.Resources.Disk.MinFreeMB = uint64(env.GetInt("LB_DISK_MIN_FREE_MB", 10240))
	
	// Logging
	config.Logging.Level = env.Get("LB_LOG_LEVEL", "info")
	config.Logging.Format = env.Get("LB_LOG_FORMAT", "json")
	
	// --- RPC Model Distribution Module settings (from env) ---
	// Вариант A: Model Replication
	config.Balancing.ModelReplication.Enabled = env.GetBool("LB_MODEL_REPLICATION_ENABLED", false)
	config.Balancing.ModelReplication.DefaultMinInstances = env.GetInt("LB_MODEL_REPLICATION_MIN_INSTANCES", 1)
	config.Balancing.ModelReplication.DefaultMaxInstances = env.GetInt("LB_MODEL_REPLICATION_MAX_INSTANCES", 3)
	config.Balancing.ModelReplication.IdleUnloadAfter = env.Get("LB_MODEL_REPLICATION_IDLE_UNLOAD", "10m")

	// Вариант B: RPC Coordinator
	config.Balancing.RpcCoordinator.Enabled = env.GetBool("LB_RPC_COORDINATOR_ENABLED", false)
	config.Balancing.RpcCoordinator.CoordinatorURL = env.Get("LB_RPC_COORDINATOR_URL", "")
	config.Balancing.RpcCoordinator.WorkerPort = env.GetInt("LB_RPC_COORDINATOR_PORT", 18050)
	config.Balancing.RpcCoordinator.Protocol = env.Get("LB_RPC_COORDINATOR_PROTOCOL", "http")
	config.Balancing.RpcCoordinator.Timeout = env.Get("LB_RPC_COORDINATOR_TIMEOUT", "30s")

	// Вариант C: Virtual Models
	config.Balancing.VirtualModels.Enabled = env.GetBool("LB_VIRTUAL_MODELS_ENABLED", false)
	vmTimeout := env.GetInt("LB_VIRTUAL_MODELS_TIMEOUT", 30000)
	if len(config.Balancing.VirtualModels.Models) > 0 {
		for i := range config.Balancing.VirtualModels.Models {
			m := &config.Balancing.VirtualModels.Models[i]
			if m.Coordination.TimeoutMs == 0 {
				m.Coordination.TimeoutMs = vmTimeout
			}
			if m.Coordination.Mode == "" {
				m.Coordination.Mode = env.Get("LB_VIRTUAL_MODELS_COORD_MODE", "sequential")
			}
		}
	}

	// Вариант D: Distributed Inference
	config.Balancing.DistInference.Enabled = env.GetBool("LB_DIST_INFERENCE_ENABLED", false)
	config.Balancing.DistInference.GrpcPort = env.GetInt("LB_DIST_INFERENCE_GRPC_PORT", 19000)

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
	if config.LoadBalancer.StatePath == "" {
		config.LoadBalancer.StatePath = "data/state.json"
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
	if config.Balancing.QueueWorkers == 0 {
		config.Balancing.QueueWorkers = 4
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
	
	// Model Replication defaults (Вариант A)
	mr := &config.Balancing.ModelReplication
	if mr.DefaultMinInstances == 0 {
		mr.DefaultMinInstances = 1
	}
	if mr.DefaultMaxInstances == 0 {
		mr.DefaultMaxInstances = 3
	}
	if mr.IdleUnloadAfter == "" {
		mr.IdleUnloadAfter = "10m"
	}
	for gi := range mr.Groups {
		g := &mr.Groups[gi]
		if g.MinInstances == 0 {
			g.MinInstances = 1
		}
		if g.MaxInstances == 0 {
			g.MaxInstances = 3
		}
	}

	// RPC Coordinator defaults (Вариант B)
	rc := &config.Balancing.RpcCoordinator
	if rc.WorkerPort == 0 {
		rc.WorkerPort = 18050
	}
	if rc.Timeout == "" {
		rc.Timeout = "30s"
	}
	if rc.Protocol == "" {
		rc.Protocol = "http"
	}
	if rc.MaxRetries == 0 {
		rc.MaxRetries = 3
	}

	// Virtual Models defaults (Вариант C)
	vm := &config.Balancing.VirtualModels
	for vi := range vm.Models {
		m := &vm.Models[vi]
		if m.Coordination.TimeoutMs == 0 {
			m.Coordination.TimeoutMs = 30000
		}
		if m.Coordination.Mode == "" {
			m.Coordination.Mode = "sequential"
		}
		if m.Coordination.SyncStrategy == "" {
			m.Coordination.SyncStrategy = "direct-response"
		}
		for si := range m.Slices {
			s := &m.Slices[si]
			if s.FallbackMode == "" {
				s.FallbackMode = "retry"
			}
		}
	}

	// Distributed Inference defaults (Вариант D)
	di := &config.Balancing.DistInference
	if di.GrpcPort == 0 {
		di.GrpcPort = 19000
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
		
		port := env.GetInt(fmt.Sprintf("BACKEND_%d_PORT", i), 11434)
		agentPort := env.GetInt(fmt.Sprintf("BACKEND_%d_AGENT_PORT", i), 18032)
		weight := env.GetInt(fmt.Sprintf("BACKEND_%d_WEIGHT", i), 1)
		maxReqs := env.GetInt(fmt.Sprintf("BACKEND_%d_MAX_REQS", i), 10)
		
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

