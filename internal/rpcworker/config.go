// Package rpcworker — Worker HTTP Server для RPC Coordinator (Вариант B).
//
// Worker обслуживает срез (slice) модели — набор слоёв трансформера.
// Координатор регистрирует worker'ов и шлёт им inference-запросы через
// `internal/rpccoordinator.WorkerClient`.
//
// Реализованные эндпоинты (см. `internal/rpccoordinator/worker_client.go`):
//
//   - GET  /rpc/health   — состояние worker'а и список загруженных срезов.
//   - POST /rpc/load     — загрузить модель с заданными слоями.
//   - POST /rpc/unload   — выгрузить модель.
//   - POST /rpc/infer    — выполнить inference среза.
//   - GET  /rpc/metrics  — метрики worker'а (active_requests, infer_total).
//   - POST /rpc/kv_sync  — синхронизировать KV-cache (stub, B4).
//   - GET  /rpc/kv_fetch — получить KV-cache шард (stub, B4).
//
// B1 — MVP в stub-режиме. Реальная llama.cpp интеграция — B1.real.
package rpcworker

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// WorkerConfig — конфигурация RPC Worker.
type WorkerConfig struct {
	// HTTP сервер
	Host string `json:"host"`
	Port int    `json:"port"`

	// Идентификация
	WorkerID      string `json:"workerId"`
	SliceLayers   string `json:"sliceLayers"` // "1-40"
	StartLayer    int    `json:"startLayer"`  // вычисляется из SliceLayers
	EndLayer      int    `json:"endLayer"`    // вычисляется из SliceLayers
	CoordinatorURL string `json:"coordinatorUrl,omitempty"`

	// Модели
	ModelsDir string `json:"modelsDir"`

	// Auth
	AuthToken string `json:"authToken,omitempty"`

	// Таймауты
	HealthCheckInterval time.Duration `json:"healthCheckInterval"`
	WriteTimeout        time.Duration `json:"writeTimeout"`
	ReadTimeout         time.Duration `json:"readTimeout"`

	// Прочее
	LogLevel string `json:"logLevel"`
	StubMode bool   `json:"stubMode"` // true для build tag llama_stub или принудительно
}

// DefaultWorkerConfig возвращает конфигурацию по умолчанию.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		Host:                "0.0.0.0",
		Port:                18080,
		WorkerID:            "",
		SliceLayers:         "",
		StartLayer:          0,
		EndLayer:            0,
		CoordinatorURL:      "",
		ModelsDir:           "./models",
		AuthToken:           "",
		HealthCheckInterval: 30 * time.Second,
		WriteTimeout:        5 * time.Minute,
		ReadTimeout:         30 * time.Second,
		LogLevel:            "info",
		StubMode:            false,
	}
}

// LoadConfigFromEnv загружает конфигурацию из переменных окружения.
func LoadConfigFromEnv() (WorkerConfig, error) {
	cfg := DefaultWorkerConfig()
	cfg.WorkerID = os.Getenv("RPC_WORKER_ID")
	cfg.SliceLayers = os.Getenv("RPC_WORKER_LAYERS")
	cfg.CoordinatorURL = os.Getenv("RPC_WORKER_COORDINATOR_URL")
	cfg.ModelsDir = getEnvOrDefault("RPC_WORKER_MODELS_DIR", "./models")
	cfg.Host = getEnvOrDefault("RPC_WORKER_HOST", "0.0.0.0")
	cfg.Port = getEnvIntOrDefault("RPC_WORKER_PORT", 18080)
	cfg.AuthToken = os.Getenv("RPC_WORKER_TOKEN")
	cfg.LogLevel = getEnvOrDefault("RPC_WORKER_LOG_LEVEL", "info")
	cfg.StubMode = strings.EqualFold(os.Getenv("RPC_WORKER_STUB_MODE"), "true") ||
		strings.EqualFold(os.Getenv("RPC_WORKER_STUB_MODE"), "1")

	if d := os.Getenv("RPC_WORKER_HEALTHCHECK_INTERVAL"); d != "" {
		if parsed, err := time.ParseDuration(d); err == nil && parsed > 0 {
			cfg.HealthCheckInterval = parsed
		}
	}
	if d := os.Getenv("RPC_WORKER_WRITE_TIMEOUT"); d != "" {
		if parsed, err := time.ParseDuration(d); err == nil && parsed > 0 {
			cfg.WriteTimeout = parsed
		}
	}
	if d := os.Getenv("RPC_WORKER_READ_TIMEOUT"); d != "" {
		if parsed, err := time.ParseDuration(d); err == nil && parsed > 0 {
			cfg.ReadTimeout = parsed
		}
	}

	if err := cfg.parseSliceLayers(); err != nil {
		return cfg, fmt.Errorf("invalid RPC_WORKER_LAYERS=%q: %w", cfg.SliceLayers, err)
	}
	if cfg.WorkerID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "rpcworker-unknown"
		}
		cfg.WorkerID = hostname
	}
	return cfg, cfg.Validate()
}

// parseSliceLayers парсит строку "1-40" в StartLayer=1, EndLayer=40.
func (c *WorkerConfig) parseSliceLayers() error {
	s := strings.TrimSpace(c.SliceLayers)
	if s == "" {
		c.StartLayer = 0
		c.EndLayer = 0
		return nil
	}
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return fmt.Errorf("expected format \"start-end\", got %q", s)
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || start < 0 {
		return fmt.Errorf("invalid start layer %q", parts[0])
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || end <= start {
		return fmt.Errorf("invalid end layer %q (must be > start=%d)", parts[1], start)
	}
	c.StartLayer = start
	c.EndLayer = end
	return nil
}

// Validate проверяет корректность конфигурации.
func (c *WorkerConfig) Validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port: %d", c.Port)
	}
	if c.Host == "" {
		return fmt.Errorf("host is required")
	}
	if c.ModelsDir == "" {
		return fmt.Errorf("modelsDir is required")
	}
	if c.StartLayer > 0 && c.EndLayer <= c.StartLayer {
		return fmt.Errorf("endLayer (%d) must be > startLayer (%d)", c.EndLayer, c.StartLayer)
	}
	return nil
}

// BaseURL возвращает внешний URL worker'а (для регистрации в координаторе).
func (c *WorkerConfig) BaseURL() string {
	return fmt.Sprintf("http://%s:%d", c.Host, c.Port)
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}