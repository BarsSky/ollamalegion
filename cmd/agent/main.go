package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"ollama-loadbalancer/internal/agent"
	"ollama-loadbalancer/pkg/env"
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
		healthcheckFlag = flag.Bool("healthcheck", false, "Run healthcheck and exit (for Docker HEALTHCHECK)")
	)

func main() {
	flag.Parse()

	// Режим healthcheck — проверка /health endpoint и выход
	if *healthcheckFlag {
		runHealthcheck(*metricsPort)
		return
	}

	// Загрузка конфигурации из переменных окружения и флагов
	cfg := &types.AgentConfig{
		AgentID:           env.Get("AGENT_ID", *agentID),
		BalancerURL:       env.Get("BALANCER_URL", *balancerURL),
		OllamaURL:         env.Get("OLLAMA_URL", "http://localhost:11434"),
		MetricsPort:       env.GetInt("AGENT_PORT", *metricsPort),
		CollectInterval:   env.GetInt("COLLECT_INTERVAL", *collectInterval),
		HeartbeatInterval: env.GetInt("HEARTBEAT_INTERVAL", *heartbeatInterval),
		GPUMode:               types.PlatformMode(env.Get("GPU_MODE", *gpuMode)),
		NVMLEnabled:           env.GetBool("NVML_ENABLED", *nvmlEnabled),
		PublicHost:            env.Get("AGENT_PUBLIC_HOST", *publicHost),
		MaxModels:             env.GetInt("AGENT_MAX_MODELS", *maxModels),
		MaxConcurrentRequests: env.GetInt("AGENT_MAX_CONCURRENT_REQUESTS", *maxConcurrentRequests),
		Weight:                env.GetInt("AGENT_WEIGHT", *weight),
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

// runHealthcheck — проверяет /health endpoint агента и завершает процесс с кодом 0 (успех) или 1 (ошибка).
// Используется для Docker HEALTHCHECK без внешних зависимостей (curl/wget).
func runHealthcheck(port int) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", port))
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

