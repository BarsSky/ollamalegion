package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"ollama-loadbalancer/internal/agent"
	"ollama-loadbalancer/pkg/types"
)

var (
	agentID         = flag.String("id", "", "Agent identifier")
	balancerURL     = flag.String("balancer", "http://localhost:8081", "Balancer URL")
	metricsPort     = flag.Int("metrics-port", 9090, "Local metrics port")
	collectInterval = flag.Int("collect-interval", 5, "Metrics collection interval (seconds)")
	heartbeatInterval = flag.Int("heartbeat-interval", 3, "Heartbeat interval (seconds)")
	configPath      = flag.String("config", "", "Path to configuration file")
)

func main() {
	flag.Parse()
	
	// Загрузка конфигурации
	var cfg *types.AgentConfig
	
	if *configPath != "" {
		// Загрузка из файла
		_, err := os.ReadFile(*configPath)
		if err != nil {
			log.Fatalf("Failed to read config file: %v", err)
		}
		
		// Парсинг JSON (упрощенно)
		cfg = &types.AgentConfig{
			AgentID:           *agentID,
			BalancerURL:       *balancerURL,
			MetricsPort:       *metricsPort,
			CollectInterval:   *collectInterval,
			HeartbeatInterval: *heartbeatInterval,
		}
	} else {
		// Использование параметров командной строки
		cfg = &types.AgentConfig{
			AgentID:           getEnv("AGENT_ID", *agentID),
			BalancerURL:       getEnv("BALANCER_URL", *balancerURL),
			MetricsPort:       *metricsPort,
			CollectInterval:   *collectInterval,
			HeartbeatInterval: *heartbeatInterval,
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
