// sdworker — Go-воркер генерации изображений: управляет субпроцессом sd-server
// (stable-diffusion.cpp) и отдаёт HTTP API для балансера и клиентов.
//
// R-Image / Phase 3 (2026-09-28). См. plans/2026-09-27-image-generation-backend-plan.md §5.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ПРОЦЕСС С СУБПРОЦЕССОМ, А НЕ cgo-ВСТРАИВАНИЕ sd.cpp:
// у sd-server нет hot-swap модели (sd_ctx создаётся один раз до listen()),
// он использует ПАТЧЕННЫЙ форк ggml — линковка в один бинарь с llama.cpp даёт
// конфликт символов/ABI. Внешний процесс даёт изоляцию падений (OOM VAE на
// Vulkan роняет движок, а не воркер), независимый апгрейд и простой kill.
// См. docs/research-sdcpp-lowvram-integration.md §5.3.
//
// Usage:
//
//	sdworker --port 18093 --models-dir ./models/image
//	sdworker -healthcheck           # one-shot probe для HEALTHCHECK в Dockerfile
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/version"

	"go.uber.org/zap"
)

var (
	port      = flag.Int("port", sdbackend.DefaultPort, "HTTP API port (default 18093)")
	serverPort = flag.Int("sd-server-port", 18094, "sd-server listen port (loopback)")
	modelsDir = flag.String("models-dir", "./models/image", "Directory with image model bundles (<dir>/<model>/...)")
	sdBin     = flag.String("sd-server-bin", "sd-server", "Path to the sd-server binary (or its name in PATH)")
	configPath = flag.String("config", "", "Path to JSON config file (SDWORKER_CONFIG)")
	preload   = flag.String("preload", "", "Load this image model at startup (empty = load on first request)")
	idleMin   = flag.Int("idle-unload-minutes", -1, "Idle unload timeout in minutes (0 = never unload; -1 = keep config value)")
	loraDir   = flag.String("lora-dir", "", "Directory with LoRA files (--lora-model-dir of sd-server)")
	upscalersDir = flag.String("hires-upscalers-dir", "", "Directory with ESRGAN upscalers (--hires-upscalers-dir)")
	baseURL   = flag.String("base-url", "", "Public base URL of this worker (for response_format:\"url\")")
	healthCheck = flag.Bool("healthcheck", false, "Run a one-shot health probe against /health and exit")
	verbose   = flag.Bool("verbose", false, "Enable debug logging")
)

func main() {
	flag.Parse()

	if *healthCheck {
		runHealthCheck()
		return
	}

	logLevel := "info"
	if *verbose {
		logLevel = "debug"
	}
	logger.Init(logLevel)
	log := logger.Get()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalw("invalid configuration", "error", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalw("invalid configuration", "error", err)
	}

	log.Infow("sdworker starting",
		"version", version.Get().String(),
		"port", cfg.Port,
		"sd_server_bin", cfg.SDServerBin,
		"sd_server_port", cfg.ServerPort,
		"models_dir", cfg.ModelsDir,
		"idle_unload_minutes", cfg.IdleUnloadMinutes,
		"max_concurrent", cfg.MaxConcurrent,
		"pinned_sd_server_revision", sdbackend.PinnedRevision())

	svc, err := sdbackend.NewService(&cfg)
	if err != nil {
		log.Fatalw("failed to initialize image service", "error", err)
	}

	app := newApp(svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	// AUTO-REGISTRATION в балансере (SDWORKER_BALANCER_URL). Отключается
	// SDWORKER_REGISTER_DISABLE — bundled-стек регистрирует воркера скриптом.
	regCtx, regCancel := context.WithCancel(context.Background())
	defer regCancel()
	var reg *balancerRegistration
	if cfgReg := newBalancerRegistration(&cfg); cfgReg != nil {
		reg = cfgReg
		reg.start(regCtx, log)
	} else if isRegisterDisabled() {
		log.Infow("balancer auto-registration disabled via SDWORKER_REGISTER_DISABLE")
	} else {
		log.Infow("balancer auto-registration disabled (SDWORKER_BALANCER_URL not set); standalone mode")
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler: app.setupRouter(),
		// ReadTimeout не ставим: A1111-клиенты шлют base64-картинки в img2img
		// (медленный приём тела). WriteTimeout НЕ ставим намеренно: генерация
		// идёт минутами, а duration-кап обрывал бы её ровно посередине
		// (тот же вывод, что в cppworker R83/v67).
		IdleTimeout: 120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Infow("sdworker HTTP server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalw("HTTP server error", "error", err)
		}
	}()

	<-quit
	log.Infow("shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	regCancel()
	if reg != nil {
		reg.stop(shutdownCtx, log)
	}
	// Сначала гасим субпроцесс sd-server: иначе он останется сиротой с занятой
	// VRAM (в Docker PID 1 уходит, дочерние процессы на Windows/Linux живут).
	svc.Shutdown(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warnw("HTTP server shutdown error", "error", err)
	}
	log.Infow("sdworker stopped")
}

// loadConfig — конфиг из файла/env + CLI-флаги (флаги имеют приоритет).
func loadConfig() (sdbackend.Config, error) {
	path := *configPath
	if path == "" {
		path = os.Getenv("SDWORKER_CONFIG")
	}
	cfg, err := sdbackend.LoadConfigFromEnv()
	if err != nil {
		return cfg, err
	}
	if path != "" {
		fileCfg, err := sdbackend.LoadConfigFromFile(path)
		if err != nil {
			return cfg, err
		}
		// Файл — база, env (уже применённый в LoadConfigFromEnv) имеет приоритет
		// для тех полей, которые env реально задал. Практический компромисс:
		// перечитываем файл как основу и повторно применяем env.
		cfg = fileCfg
		if err := cfg.ApplyEnv(); err != nil {
			return cfg, err
		}
	}
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			cfg.Port = *port
		case "sd-server-port":
			cfg.ServerPort = *serverPort
		case "models-dir":
			cfg.ModelsDir = *modelsDir
		case "sd-server-bin":
			cfg.SDServerBin = *sdBin
		case "preload":
			cfg.PreloadModel = *preload
		case "lora-dir":
			cfg.LoraModelDir = *loraDir
		case "hires-upscalers-dir":
			cfg.HiresUpscalersDir = *upscalersDir
		case "base-url":
			cfg.BaseURL = *baseURL
		}
	})
	if *idleMin >= 0 {
		cfg.IdleUnloadMinutes = *idleMin
	}
	// SDWORKER_PORT из env — если флаг не задан явно (как в cppworker).
	if !isFlagSet("port") {
		if envPort := strings.TrimSpace(os.Getenv("SDWORKER_PORT")); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil && p > 0 && p <= 65535 {
				cfg.Port = p
			}
		}
	}
	return cfg, nil
}

// isFlagSet — задан ли флаг в командной строке явно.
func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// runHealthCheck — one-shot probe (для HEALTHCHECK в Dockerfile, без curl).
//
// Дёргаем СВОЙ /health, а не /sdcpp/v1/capabilities движка: воркер обязан
// отвечать даже когда модель не загружена (иначе контейнер уходил бы в
// unhealthy между генерациями, потому что idle-unload убил субпроцесс).
func runHealthCheck() {
	probePort := *port
	if !isFlagSet("port") {
		if envPort := strings.TrimSpace(os.Getenv("SDWORKER_PORT")); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil && p > 0 && p <= 65535 {
				probePort = p
			}
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", probePort)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck failed: HTTP %d from %s\n", resp.StatusCode, url)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "healthcheck passed")
	os.Exit(0)
}

// App — HTTP-слой воркера.
type App struct {
	svc       *sdbackend.Service
	startedAt time.Time
	// progress — трекер загрузки модели (для /api/image/models/load/progress).
	progress *progressTracker
	// loadCtx — контекст фоновых загрузок. Живёт ДОЛЬШЕ HTTP-запроса: клиент
	// может закрыть вкладку, а загрузка модели обязана доиграть до конца,
	// иначе останется «наполовину поднятый» sd-server.
	loadCtx context.Context
}

// newApp — конструктор с инициализированными зависимостями.
func newApp(svc *sdbackend.Service) *App {
	return &App{
		svc:       svc,
		startedAt: time.Now(),
		progress:  newProgressTracker(),
		loadCtx:   context.Background(),
	}
}

// appLog — логгер для хендлеров.
func appLog() *zap.SugaredLogger { return logger.Get() }
