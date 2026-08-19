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
		collectInterval = flag.Int("collect-interval", -1, "Metrics collection interval in seconds (-1 = use METRICS_INTERVAL env or default 5)")
		metricsInterval = flag.Int("metrics-interval", -1, "Metrics collection interval in seconds (alias for collect-interval, -1 = use COLLECT_INTERVAL env or default 5)")
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
		CppWorkerURL:      env.Get("CPPWORKER_URL", "http://localhost:18091"),
		MetricsPort:       env.GetInt("AGENT_PORT", *metricsPort),
		CollectInterval:   resolveInterval(env.GetInt("METRICS_INTERVAL", -1), env.GetInt("COLLECT_INTERVAL", -1), *collectInterval, *metricsInterval, 5),
		HeartbeatInterval: env.GetInt("HEARTBEAT_INTERVAL", *heartbeatInterval),
		GPUMode:               types.PlatformMode(env.Get("GPU_MODE", *gpuMode)),
		NVMLEnabled:           env.GetBool("NVML_ENABLED", *nvmlEnabled),
		PublicHost:            env.Get("AGENT_PUBLIC_HOST", *publicHost),
		MaxModels:             env.GetInt("AGENT_MAX_MODELS", *maxModels),
		MaxConcurrentRequests: env.GetInt("AGENT_MAX_CONCURRENT_REQUESTS", *maxConcurrentRequests),
		Weight:                env.GetInt("AGENT_WEIGHT", *weight),
		BackendType:           types.BackendType(env.Get("BACKEND_TYPE", "ollama")),
		NodeLabels:            env.Get("NODE_LABELS", ""),
		// Round 41 (2026-08-19): X-API-Token для auth на балансере. Передаётся
		// в каждом защищённом запросе (register/metrics/heartbeat). Если пусто —
		// agent не шлёт заголовок (dev-режим с отключённым auth на балансере).
		BalancerToken:         env.Get("BALANCER_TOKEN", ""),
		// 2026-06-30: CppWorkerHost/Port используются в register() как host/cppWorkerPort,
		// чтобы de-dup по (host, port) в /api/v1/gguf/backends корректно склеивал
		// cppworker-gpu и cppworker-gpu-bundled-agent (агент — observer физического cppworker).
		// Fallback на CppWorkerURL (если задан) иначе localhost:18092.
		CppWorkerHost:         env.Get("AGENT_CPPWORKER_HOST", ""),
		CppWorkerPort:         env.GetInt("AGENT_CPPWORKER_PORT", 0),
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
	
	// 2026-06-30: баннер зависит от backend type — раньше был всегда
	// "Ollama Load Balancer - Agent", что вводило в заблуждение при запуске
	// на llama.cpp бэкенде (выглядело как баг конфигурации). Теперь имя и
	// engine-line определяются backend type, а default-конфигурация agent'а
	// для llama.cpp бэкендов включает NVML по умолчанию (бессмысленно
	// собирать GPU-метрики без NVML, если у нас GPU-платформа).
	engineLabel := "Ollama"
	defaultNVML := false
	switch cfg.BackendType {
	case types.BackendTypeLlamaCpp:
		engineLabel = "llama.cpp (CppWorker)"
		// llama.cpp-бэкенды собираются на GPU-хостах с CppWorker, NVML —
		// основной источник GPU/VRAM метрик, default ON.
		if !env.HasExplicit("NVML_ENABLED") && !*nvmlEnabled {
			cfg.NVMLEnabled = true
		}
		defaultNVML = cfg.NVMLEnabled
	case types.BackendTypeOllama:
		engineLabel = "Ollama"
		defaultNVML = cfg.NVMLEnabled
	default:
		engineLabel = cfg.BackendType.Label()
		defaultNVML = cfg.NVMLEnabled
	}

	fmt.Printf("╔═══════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║         Ollama Load Balancer - Agent                      ║\n")
	fmt.Printf("║         Engine: %-41s║\n", engineLabel)
	fmt.Printf("╠═══════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║ Agent ID:    %-46s║\n", cfg.AgentID)
	fmt.Printf("║ Backend:     %-46s║\n", cfg.BackendType.Label())
	fmt.Printf("║ Balancer:    %-46s║\n", cfg.BalancerURL)
	fmt.Printf("║ Public Host:  %-46s║\n", cfg.PublicHost)
	fmt.Printf("║ Mode:        %-46s║\n", string(cfg.GPUMode))
	fmt.Printf("║ NVML:        %-46v║\n", defaultNVML)
	fmt.Printf("║ Collect:     %-46ds║\n", cfg.CollectInterval)
	fmt.Printf("║ Heartbeat:   %-46ds║\n", cfg.HeartbeatInterval)
	fmt.Printf("╚═══════════════════════════════════════════════════════════╝\n")
	
	// Создание и запуск агента
	// Round 41 (2026-08-19): предупреждаем, если токен не задан — все защищённые
	// запросы к балансеру получат 401 в продакшен-конфиге (auth: enabled).
	if cfg.BalancerToken == "" {
		fmt.Printf("[Agent]  WARNING: BALANCER_TOKEN is empty — agent will NOT send X-API-Token header.\n")
		fmt.Printf("[Agent]  This is fine for dev (auth disabled on balancer), but will fail with 401 in production.\n")
	}

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

// resolveInterval — выбирает первый неотрицательный приоритет:
// env METRICS_INTERVAL > env COLLECT_INTERVAL > flag metrics-interval > flag collect-interval > default
func resolveInterval(metricsEnv, collectEnv, collectFlag, metricsFlag, defaultVal int) int {
	if metricsEnv > 0 {
		return metricsEnv
	}
	if collectEnv > 0 {
		return collectEnv
	}
	if metricsFlag > 0 {
		return metricsFlag
	}
	if collectFlag > 0 {
		return collectFlag
	}
	return defaultVal
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

