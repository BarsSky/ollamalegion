package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ollama-loadbalancer/internal/api"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

var (
	configPath = flag.String("config", "config/config.json", "Path to configuration file")
	port       = flag.Int("port", 0, "Override config port")
	apiPort    = flag.Int("api-port", 0, "Override config API port")
	logLevel   = flag.String("log-level", "", "Override config log level")
)

func main() {
	flag.Parse()
	
	// Загрузка конфигурации
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Printf("Warning: Failed to load config file: %v", err)
		log.Println("Trying to load from environment...")
		
		cfg, err = config.LoadFromEnv()
		if err != nil {
			log.Fatalf("Failed to load configuration: %v", err)
		}
	}
	
	conf := cfg.Get()
	
	// Применение переопределений из командной строки
	if *port > 0 {
		conf.LoadBalancer.Port = *port
	}
	if *apiPort > 0 {
		conf.LoadBalancer.APIPort = *apiPort
	}
	if *logLevel != "" {
		conf.Logging.Level = *logLevel
	}
	
	// ==== Environment переменные (приоритет: flag > env > config) ====
	// LOG_LEVEL — переопределение уровня логгирования.
	// Позволяет включить debug через docker-compose environment без правки config.json.
	if envLevel := os.Getenv("LOG_LEVEL"); envLevel != "" && *logLevel == "" {
		conf.Logging.Level = envLevel
		log.Printf("[ENV]  LOG_LEVEL=%s overrides config level", envLevel)
	}
	
	// Инициализация structured logger (после всех переопределений)
	logger.Init(conf.Logging.Level)
	defer logger.Sync()

	// Применение переопределений из environment (приоритет выше config.json)
	if algo := os.Getenv("LB_ALGORITHM"); algo != "" {
		validAlgorithms := map[string]bool{
			"roundrobin":     true,
			"leastconn":      true,
			"resource-aware": true,
			"model-affinity": true,
		}
		if validAlgorithms[algo] {
			conf.Balancing.Algorithm = types.BalancingAlgorithm(algo)
			logger.Get().Infow("algorithm overridden from environment", "algorithm", algo)
		} else {
			logger.Get().Warnw("invalid LB_ALGORITHM environment value, ignoring", "value", algo)
		}
	}
	if os.Getenv("LB_MODEL_AFFINITY") != "" {
		conf.Balancing.ModelAffinity = os.Getenv("LB_MODEL_AFFINITY") == "true"
	}
	if os.Getenv("LB_SESSION_STICKINESS") != "" {
		conf.Balancing.SessionStickiness = os.Getenv("LB_SESSION_STICKINESS") == "true"
	}
	
	// Проверка и настройка TLS
	if conf.TLS.Enabled {
		fmt.Printf("[TLS]    TLS is enabled (AutoCert: %v)\n", conf.TLS.AutoCert)
		
		// Проверка/генерация сертификатов
		if err := api.EnsureTLSCertificates(&conf.TLS); err != nil {
			log.Fatalf("Failed to setup TLS certificates: %v", err)
		}
	}
	
	fmt.Printf("╔═══════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║         Ollama Load Balancer - Starting                   ║\n")
	fmt.Printf("╠═══════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║ Proxy Port:  %-46d║\n", conf.LoadBalancer.Port)
	fmt.Printf("║ API Port:    %-46d║\n", conf.LoadBalancer.APIPort)
	if conf.TLS.Enabled {
		fmt.Printf("║ TLS Port:    %-46d║\n", conf.LoadBalancer.TLSPort)
	}
	fmt.Printf("║ Algorithm:   %-46s║\n", conf.Balancing.Algorithm)
	fmt.Printf("║ Backends:    %-46d║\n", len(conf.Backends))
	fmt.Printf("╚═══════════════════════════════════════════════════════════╝\n")
	
	// Создание прокси
	proxy := balancer.NewProxy(conf)

	// Создание контроллеров оптимизации балансировки
	prewarmCtrl := balancer.NewPrewarmController(proxy, conf.Balancing.Prewarm)
	modelInstanceCtrl := balancer.NewModelInstanceController(proxy, conf.Balancing.ModelInstances)

	// Создание планировщика выгрузки и тюнера весов
	unloadScheduler := balancer.NewUnloadScheduler(proxy)
	weightTuner := balancer.NewAdaptiveWeightTuner(proxy)

	// Подключение к proxy
	proxy.SetUnloadScheduler(unloadScheduler)
	proxy.SetWeightTuner(weightTuner)

	// Создание health checker
	healthChecker := balancer.NewHealthChecker(
		proxy,
		time.Duration(conf.Balancing.HealthCheckInterval)*time.Second,
		3,
	)
	
	// Создание API сервера
	apiServer := api.NewServer(proxy, conf, healthChecker)

	// Подключаем сохранение конфига на диск для авто-загрузки моделей (AutoPull)
	apiServer.SetConfigSaver(cfg.Save)

	// F.α (2026-06-28): session F — подключаем EventBus балансировщика к API
	// для SSE notifications endpoint /api/v1/events (F.α).
	if proxy.EventBus() != nil {
		apiServer.SetEventBus(proxy.EventBus())
	}
	
	// Запуск health checker
	healthChecker.Start()

	// Запуск контроллеров оптимизации
	prewarmCtrl.Start()
	modelInstanceCtrl.Start()

	// Запуск планировщика выгрузки и тюнера весов
	fmt.Println("[Unload] Starting unload scheduler...")
	unloadScheduler.Start()
	fmt.Println("[Weight] Starting adaptive weight tuner...")
	weightTuner.Start()

	// Создание HTTP сервера для прокси
	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	
	proxyServer := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", conf.LoadBalancer.Host, conf.LoadBalancer.Port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: time.Duration(conf.Balancing.RequestTimeout+30) * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	
	// Создание API сервера
	apiHTTPServer := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", conf.LoadBalancer.Host, conf.LoadBalancer.APIPort),
		Handler:      apiServer,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	
	// HTTPS сервера (если TLS включен)
	var proxyTLSServer *http.Server
	var apiTLSServer *http.Server
	
	if conf.TLS.Enabled {
		// Загрузка TLS конфигурации
		tlsConfig, err := api.LoadTLSConfig(&conf.TLS)
		if err != nil {
			log.Fatalf("Failed to load TLS configuration: %v", err)
		}
		
		// HTTPS прокси сервер
		proxyTLSServer = &http.Server{
			Addr:         fmt.Sprintf("%s:%d", conf.LoadBalancer.Host, conf.LoadBalancer.TLSPort),
			Handler:      mux,
			TLSConfig:    tlsConfig,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: time.Duration(conf.Balancing.RequestTimeout+30) * time.Second,
			IdleTimeout:  120 * time.Second,
		}
		
		// HTTPS API сервер
		apiTLSServer = &http.Server{
			Addr:         fmt.Sprintf("%s:%d", conf.LoadBalancer.Host, conf.LoadBalancer.TLSPort+1),
			Handler:      apiServer,
			TLSConfig:    tlsConfig,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 60 * time.Second,
			IdleTimeout:  120 * time.Second,
		}
		
		// Добавление middleware для редиректа HTTP -> HTTPS (опционально)
		// mux = api.HTTPSRedirectMiddleware(mux)
	}
	
	// Каналы для graceful shutdown
	proxyErr := make(chan error, 1)
	apiErr := make(chan error, 1)
	proxyTLSErr := make(chan error, 1)
	apiTLSErr := make(chan error, 1)
	
	// Запуск серверов
	go func() {
		fmt.Printf("\n[Proxy]  Listening on %s:%d\n", conf.LoadBalancer.Host, conf.LoadBalancer.Port)
		proxyErr <- proxyServer.ListenAndServe()
	}()
	
	go func() {
		fmt.Printf("[API]    Listening on %s:%d\n", conf.LoadBalancer.Host, conf.LoadBalancer.APIPort)
		apiErr <- apiHTTPServer.ListenAndServe()
	}()
	
	// Запуск HTTPS серверов если TLS включен
	if conf.TLS.Enabled {
		go func() {
			certFile := conf.TLS.CertFile
			keyFile := conf.TLS.KeyFile
			fmt.Printf("[Proxy]  HTTPS Listening on %s:%d (cert: %s)\n", conf.LoadBalancer.Host, conf.LoadBalancer.TLSPort, certFile)
			proxyTLSErr <- proxyTLSServer.ListenAndServeTLS(certFile, keyFile)
		}()
		
		go func() {
			certFile := conf.TLS.CertFile
			keyFile := conf.TLS.KeyFile
			fmt.Printf("[API]    HTTPS Listening on %s:%d\n", conf.LoadBalancer.Host, conf.LoadBalancer.TLSPort+1)
			apiTLSErr <- apiTLSServer.ListenAndServeTLS(certFile, keyFile)
		}()
	}
	
	// Ожидание сигнала завершения
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	
	// Ожидание ошибки от любого сервера или сигнала завершения
	select {
	case err := <-proxyErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Proxy server failed: %v", err)
		}
	case err := <-apiErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("API server failed: %v", err)
		}
	case err := <-proxyTLSErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Proxy TLS server failed: %v", err)
		}
	case err := <-apiTLSErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("API TLS server failed: %v", err)
		}
	case sig := <-quit:
		fmt.Printf("\nReceived signal %v, shutting down gracefully...\n", sig)
	}

	// Graceful shutdown sequence:
	// 1. Proxy.Shutdown (запрет новых запросов, завершение активных SSE, сохранение state)
	// 2. HTTP-серверы (Shutdown с таймаутом для активных keep-alive соединений)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	fmt.Println("[Shutdown] Stopping proxy (waiting for active streams)...")
	if err := proxy.Shutdown(shutdownCtx); err != nil {
		log.Printf("[Shutdown] Proxy shutdown error: %v", err)
	}

	fmt.Println("[Shutdown] Stopping HTTP servers...")
	if err := proxyServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[Shutdown] Proxy server shutdown error: %v", err)
	}
	if err := apiHTTPServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[Shutdown] API server shutdown error: %v", err)
	}
	if proxyTLSServer != nil {
		if err := proxyTLSServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("[Shutdown] Proxy TLS server shutdown error: %v", err)
		}
	}
	if apiTLSServer != nil {
		if err := apiTLSServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("[Shutdown] API TLS server shutdown error: %v", err)
		}
	}
	fmt.Println("[Shutdown] Completed. Goodbye!")
}
