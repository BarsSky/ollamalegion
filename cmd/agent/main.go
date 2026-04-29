package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"ollama-loadbalancer/internal/agent"
	"ollama-loadbalancer/pkg/types"
)

	var (
		agentID         = flag.String("id", "", "Agent identifier")
		balancerURL     = flag.String("balancer", "http://localhost:18081", "Balancer URL")
		metricsPort     = flag.Int("metrics-port", 18032, "Local metrics port")
		collectInterval = flag.Int("collect-interval", 5, "Metrics collection interval (seconds)")
		heartbeatInterval = flag.Int("heartbeat-interval", 3, "Heartbeat interval (seconds)")
		configPath      = flag.String("config", "", "Path to configuration file")
		gpuMode         = flag.String("gpu-mode", "auto", "Platform mode: auto, gpu, cpu")
		nvmlEnabled     = flag.Bool("nvml", false, "Enable NVML")
		publicHost      = flag.String("public-host", "", "Public IP/hostname accessible by balancer (optional, auto-detected if empty)")
		maxModels       = flag.Int("max-models", -1, "Max models limit (-1 = unlimited/not set)")
		maxConcurrentRequests = flag.Int("max-concurrent-requests", -1, "Max concurrent requests limit (-1 = unlimited/not set)")
		weight          = flag.Int("weight", 1, "Backend priority weight (1-100)")
	)

func main() {
	flag.Parse()

	// Загрузка конфигурации из переменных окружения и флагов
	cfg := &types.AgentConfig{
		AgentID:           getEnv("AGENT_ID", *agentID),
		BalancerURL:       getEnv("BALANCER_URL", *balancerURL),
		OllamaURL:         getEnv("OLLAMA_URL", "http://localhost:11434"),
		MetricsPort:       getEnvInt("AGENT_PORT", *metricsPort),
		CollectInterval:   getEnvInt("COLLECT_INTERVAL", *collectInterval),
		HeartbeatInterval: getEnvInt("HEARTBEAT_INTERVAL", *heartbeatInterval),
		GPUMode:               types.PlatformMode(getEnv("GPU_MODE", *gpuMode)),
		NVMLEnabled:           getEnvBool("NVML_ENABLED", *nvmlEnabled),
		PublicHost:            getEnv("AGENT_PUBLIC_HOST", *publicHost),
		MaxModels:             getEnvInt("AGENT_MAX_MODELS", *maxModels),
		MaxConcurrentRequests: getEnvInt("AGENT_MAX_CONCURRENT_REQUESTS", *maxConcurrentRequests),
		Weight:                getEnvInt("AGENT_WEIGHT", *weight),
	}

	// Если передан файл конфигурации — загружаем из него
	if *configPath != "" {
		data, err := os.ReadFile(*configPath)
		if err != nil {
			log.Fatalf("Failed to read config file: %v", err)
		}
		if err := json.Unmarshal(data, cfg); err != nil {
			log.Fatalf("Failed to parse config file: %v", err)
		}
	}
	
	// Проверка обязательных параметров
	if cfg.AgentID == "" {
		// Попытка получить ID из hostname
		hostname, _ := os.Hostname()
		if hostname != "" {
			cfg.AgentID = hostname
		} else {
			log.Fatal("Agent ID is required. Use -id flag or AGENT_ID environment variable")
		}
	}
	
	fmt.Printf("╔═══════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║         Ollama Load Balancer - Agent                      ║\n")
	fmt.Printf("╠═══════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║ Agent ID:    %-46s║\n", cfg.AgentID)
	fmt.Printf("║ Balancer:    %-46s║\n", cfg.BalancerURL)
	fmt.Printf("║ Public Host:  %-46s║\n", cfg.PublicHost)
	fmt.Printf("║ Mode:        %-46s║\n", string(cfg.GPUMode))
	fmt.Printf("║ NVML:        %-46v║\n", cfg.NVMLEnabled)
	fmt.Printf("║ Collect:     %-46ds║\n", cfg.CollectInterval)
	fmt.Printf("║ Heartbeat:   %-46ds║\n", cfg.HeartbeatInterval)
	fmt.Printf("╚═══════════════════════════════════════════════════════════╝\n")
	
	// Создание и запуск агента
	agentInstance := agent.NewAgent(cfg)
	
	if err := agentInstance.Start(); err != nil {
		log.Fatalf("Failed to start agent: %v", err)
	}
	
	fmt.Printf("\n[Agent]  Started successfully\n")
	fmt.Printf("[Agent]  Collecting metrics every %d seconds\n", cfg.CollectInterval)
	fmt.Printf("[Agent]  Sending heartbeat every %d seconds\n", cfg.HeartbeatInterval)
	
	// Ожидание сигнала завершения
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	
	fmt.Println("\n[Agent]  Shutting down...")
	agentInstance.Stop()
	fmt.Println("[Agent]  Stopped.")
}

// getEnv - получение переменной окружения или значения по умолчанию
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvInt - получение int из переменной окружения или значения по умолчанию
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return defaultValue
}

// getEnvBool - получение bool из переменной окружения или значения по умолчанию
func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if b, err := strconv.ParseBool(value); err == nil {
			return b
		}
	}
	return defaultValue
}
