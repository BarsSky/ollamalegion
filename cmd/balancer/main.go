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
	
	// Проверка и настройка TLS
	if conf.TLS.Enabled {
		fmt.Printf("[TLS]    TLS is enabled (AutoCert: %v)\n", conf.TLS.AutoCert)
		
		// Проверка/генерация сертификатов
		if err := api.EnsureTLSCertificates(&conf.TLS); err != nil {
			log.Fatalf("Failed to setup TLS certificates: %v", err)
		}
		
		// Загрузка TLS конфигурации
		tlsConfig, err := api.LoadTLSConfig(&conf.TLS)
		if err != nil {
			log.Fatalf("Failed to load TLS configuration: %v", err)
		}
		
		// Сохраняем TLS конфигурацию для использования в серверах
		_ = tlsConfig // будет использовано ниже
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
	
	// Создание health checker
	healthChecker := balancer.NewHealthChecker(
		proxy,
		time.Duration(conf.Balancing.HealthCheckInterval)*time.Second,
		3,
	)
	
	// Создание API сервера
	apiServer := api.NewServer(proxy, conf, healthChecker)
	
	// Запуск health checker
	healthChecker.Start()
	
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
		fmt.Printf("\nReceived signal %v, shutting down...\n", sig)
	}
	
	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	
	fmt.Println("[Proxy]  Shutting down proxy server...")
	if err := proxyServer.Shutdown(ctx); err != nil {
		log.Printf("Proxy server shutdown error: %v", err)
	}
	
	fmt.Println("[API]    Shutting down API server...")
	if err := apiHTTPServer.Shutdown(ctx); err != nil {
		log.Printf("API server shutdown error: %v", err)
	}
	
	// Shutdown HTTPS серверов
	if conf.TLS.Enabled {
		fmt.Println("[Proxy]  Shutting down HTTPS proxy server...")
		if err := proxyTLSServer.Shutdown(ctx); err != nil {
			log.Printf("HTTPS proxy server shutdown error: %v", err)
		}
		
		fmt.Println("[API]    Shutting down HTTPS API server...")
		if err := apiTLSServer.Shutdown(ctx); err != nil {
			log.Printf("HTTPS API server shutdown error: %v", err)
		}
	}
	
	// Сохраняем состояние перед завершением
	fmt.Println("[State]  Saving state...")
	if err := proxy.FlushState(); err != nil {
		log.Printf("State flush error: %v", err)
	}

	healthChecker.Stop()
	
	fmt.Println("Ollama Load Balancer stopped.")
}
