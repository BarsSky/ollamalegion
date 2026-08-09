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
	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/internal/runtimeoverrides"
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

	// F.2 (Session F) — инициализация log-broker для live tail через WebSocket /ws/logs.
	// Брокер хранит ring buffer из 100 последних записей; Subscribe() из
	// internal/api/handlers_logs_ws.go получает копию буфера + live channel.
	// logger.Publish(...) в любом месте кода теперь рассылает запись всем WS-клиентам.
	logBroker := logger.NewLogBroker(100)
	logger.SetBroker(logBroker)
	defer logBroker.Stop()

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
	// Round 8 (2026-07-10): wire HealthChecker в Proxy чтобы markBackendConnectionFailed
	// мог помечать backend unhealthy при persistent connection failures.
	proxy.SetHealthChecker(healthChecker)
	
	// Создание API сервера
	apiServer := api.NewServer(proxy, conf, healthChecker)

	// Подключаем сохранение конфига на диск для авто-загрузки моделей (AutoPull)
	apiServer.SetConfigSaver(cfg.Save)

	// Round 31 (2026-08-09): load model profiles из /app/data/profiles.json (writable).
	// config.json может быть read-only (bundled compose), поэтому профили persist'ятся
	// отдельно. На старте мержим с config.json (config wins при коллизиях).
	if loaded, err := proxy.LoadProfilesFromFile(); err != nil {
		log.Printf("Warning: failed to load profiles from disk: %v", err)
	} else if loaded > 0 {
		log.Printf("Loaded %d model profile(s) from /app/data/profiles.json", loaded)
	}

	// Session 17 (2026-07-27): persistent runtime overrides для llamaCpp.
	// В bundled compose config.json монтируется :ro, поэтому WebUI-изменения
	// llamaCpp пишутся в sidecar-файл /app/data/runtime-overrides/llama-cpp.json
	// (writable named volume `balancer_data`). При старте Load+Apply восстанавливает
	// состояние; DELETE endpoint стирает override и возвращает in-memory к base.
	//
	// baseSnapshot — копия LlamaCpp ДО применения override, чтобы можно было
	// корректно сделать "Reset to bundled defaults" без перезапуска.
	//
	// dataDir берётся из /app/data (volume mount). На dev-окружениях без
	// volume (LB_DATA_DIR env override) store.IsEnabled() == false → оверрайды
	// отключены, но in-memory PUT продолжает работать (как до этой сессии).
	dataDir := os.Getenv("LB_DATA_DIR")
	if dataDir == "" {
		dataDir = "/app/data"
	}
	overridesStore := runtimeoverrides.New(dataDir)
	baseSnapshot := conf.LlamaCpp // копия значения (struct value, не pointer)
	if err := overridesStore.ApplyLlamaCppToConfig(conf); err != nil {
		log.Printf("Warning: failed to apply runtime overrides on startup: %v", err)
	}
	if overridesStore.IsEnabled() && overridesStore.HasLlamaCppOverride() {
		fmt.Println("[Runtime] llamaCpp override active (see /api/v1/cluster/llama-cpp/overrides)")
	}
	apiServer.SetOverridesStore(overridesStore, &baseSnapshot)

	// F.α (2026-06-28): session F — подключаем EventBus балансировщика к API
	// для SSE notifications endpoint /api/v1/events (F.α).
	if proxy.EventBus() != nil {
		apiServer.SetEventBus(proxy.EventBus())
	}

	// Phase 8 (2026-07-10): P.1 — rpc_coordinator production mode.
	// Если balancer в OperatingMode=rpc_coordinator И RPC coordinator
	// инициализирован (cfg.RpcCoordinator.Enabled=true) — создаём
	// RpcCoordinatorDispatcher и подключаем к proxy. Dispatcher'у нужно
	// знать coordinator + proxy reference (для fallback в ShouldRoute).
	//
	// Phase 8 Session 2: только non-streaming + 6 unit tests.
	// Phase 8 Session 3: streaming + circuit breaker integration + auth.
	//
	// Phase 8.5 scaffold в Proxy.ServeHTTP уже проверяет IsRpcCoordinatorMode
	// + rpcDispatcher != nil + IsRpcPath — при выполнении всех 3 условий
	// request маршрутизируется через dispatcher (вместо обычного прокси).
	if balancer.IsRpcCoordinatorMode(conf.Balancing.OperatingMode) {
		if coord := proxy.GetRpcCoordinator(); coord != nil {
			dispatcher := balancer.NewRpcCoordinatorDispatcher(coord, proxy)
			// Session 3.3: применяем circuit breaker config из RpcCoordinatorConfig
			// (defaults: failure_threshold=5, success_threshold=1, reset_timeout=30s).
			dispatcher.SetCircuitBreakerConfig(rpccoordinator.CircuitBreakerConfig{
				FailureThreshold: defaultIfZero(conf.Balancing.RpcCoordinator.CircuitBreaker.FailureThreshold, 5),
				SuccessThreshold: defaultIfZero(conf.Balancing.RpcCoordinator.CircuitBreaker.SuccessThreshold, 1),
				ResetTimeout:     time.Duration(defaultIfZero(conf.Balancing.RpcCoordinator.CircuitBreaker.ResetTimeoutMs, 30000)) * time.Millisecond,
			})
			// Session 3.4: wire auth если auth.Enabled.
			// AuthChecker interface в balancer реализуется *api.TokenAuthenticator
			// (см. internal/balancer/rpc_coordinator_dispatcher.go).
			if conf.Auth.Enabled {
				authChecker := api.NewTokenAuthenticator(
					conf.Auth.Tokens,
					conf.Auth.HeaderName,
					conf.Auth.Enabled,
				)
				dispatcher.SetAuthenticator(authChecker)
				logger.Get().Infow("rpc_coordinator_dispatcher: auth wired",
					"token_count", len(conf.Auth.Tokens),
					"header", conf.Auth.HeaderName)
			}
			proxy.SetRpcCoordinatorDispatcher(dispatcher)
			logger.Get().Infow("rpc_coordinator_dispatcher wired",
				"mode", conf.Balancing.OperatingMode,
				"coordinator_enabled", conf.Balancing.RpcCoordinator.Enabled,
				"embedded", conf.Balancing.RpcCoordinator.Embedded,
				"cb_failure_threshold", conf.Balancing.RpcCoordinator.CircuitBreaker.FailureThreshold,
				"cb_reset_timeout_ms", conf.Balancing.RpcCoordinator.CircuitBreaker.ResetTimeoutMs)
		} else {
			logger.Get().Warnw("rpc_coordinator mode is active but coordinator is nil — " +
				"check balancing.rpcCoordinator.enabled in config")
		}
	}

	// Phase 8 (2026-07-11): P.2 — virtual_router production mode.
	// Если balancer в OperatingMode=virtual_router → создаём VirtualRouter
	// поверх существующего VirtualModel Registry (создан в initRpcModules).
	// VirtualRouter перехватывает requests с model=virtual:xxx (на самом деле
	// любое имя, зарегистрированное в VirtualModels config) и выбирает
	// backend через Selector (round_robin / least_loaded / random).
	//
	// Phase 8 P.2 Step 4: wiring в Proxy.ServeHTTP через virtualRouter
	// interceptor (mode=virtual_router + IsVirtualPathRequest + MatchesVirtualRequest).
	//
	// Активация:
	//   1. balancing.operatingMode = "virtual_router"
	//   2. balancing.virtualModels.enabled = true
	//   3. В конфиге есть хотя бы одна VirtualModel в alias-on-pool mode
	//      (BackendPool + ModelName заданы).
	if balancer.IsVirtualRouterMode(conf.Balancing.OperatingMode) {
		vmRegistry := proxy.GetVirtualModelRegistry()
		if vmRegistry != nil {
			vmRegistry.SetEnabled(true)
			router := balancer.NewVirtualRouter(vmRegistry, proxy)
			// Phase 8 P.2 backlog (2026-07-11): wire auth (same pattern as P.1).
			if conf.Auth.Enabled {
				router.SetAuthenticator(api.NewTokenAuthenticator(
					conf.Auth.Tokens,
					conf.Auth.HeaderName,
					conf.Auth.Enabled,
				))
				logger.Get().Infow("virtual_router: auth wired",
					"token_count", len(conf.Auth.Tokens),
					"header", conf.Auth.HeaderName)
			}
			// Phase 8 Item 2 (2026-07-11): wire LoadProvider для least_loaded selector.
			// Без этого SetLoadProvider() selector с least_loaded использует
			// fallback=1 (эквивалент round-robin). Теперь: FreeSlots из
			// proxy.GetBackendFreeSlots() = MaxConcurrentReqs - ActiveReqs.
			router.SetLoadProvider(func(backendID string) (int, bool) {
				return proxy.GetBackendFreeSlots(backendID)
			})
			proxy.SetVirtualRouter(router)
			logger.Get().Infow("virtual_router wired",
				"mode", conf.Balancing.OperatingMode,
				"virtual_models", len(vmRegistry.List()),
				"load_provider", "MaxConcurrentReqs-ActiveReqs")
		} else {
			logger.Get().Warnw("virtual_router mode is active but VirtualModelRegistry is nil — " +
				"check balancing.virtualModels.enabled in config")
		}
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

// defaultIfZero — возвращает def если v == 0, иначе v. Helper для
// optional config значений с defaults.
func defaultIfZero(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
