// CppBackend Worker — HTTP сервер для llama.cpp GGUF моделей
//
// Предоставляет REST API для загрузки/выгрузки GGUF моделей,
// инференса (синхронного и стриминг), управления multi-GPU,
// совместимый с API форматом Ollama.
//
// Usage:
//
//	cppworker --port 18091 --models-dir ./models
//	cppworker --port 18091 --gpu-layers -1 --flash-attn
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"

	"go.uber.org/zap"
)

// ============================================================
// Флаги командной строки
// ============================================================

var (
	startupTime          = time.Now() // Используется для /api/diagnostics uptime.
	port                 = flag.Int("port", 18092, "HTTP server port (default 18092; 18091 is legacy)")
	modelsDir            = flag.String("models-dir", "./models", "Directory with GGUF model files")
	// DefaultCtxSize = 8192 (а не 4096): при 4096 у OpenWebUI с tools
	// (system + tool definitions ~3000-5000 токенов + user message)
	// prompt не влезает → reload на каждой tool-итерации.
	// 8192 — минимум для стабильной работы OpenWebUI с tools.
	ctxSize              = flag.Int("ctx-size", 8192, "Default context size")
	batchSize            = flag.Int("batch-size", 512, "Default batch size")
	gpuLayers            = flag.Int("gpu-layers", -1, "GPU layers (-1=all, 0=CPU)")
	flashAttn            = flag.Int("flash-attn", -1, "Flash Attention type: -1=auto, 0=disabled, 1=enabled")
	numa                 = flag.Bool("numa", false, "Enable NUMA optimization")
	noMmap               = flag.Bool("no-mmap", false, "Disable mmap")
	ramFallbackNCtx      = flag.Bool("ram-fallback-n-ctx", false, "Auto-reload model with requested n_ctx using RAM when VRAM is insufficient")
	ramFallbackGpuLayers = flag.Int("ram-fallback-gpu-layers", -1, "GPU layers to use during RAM fallback (-1=keep current, 0=CPU-only)")
	ramFallbackMaxNCtx   = flag.Int("ram-fallback-max-n-ctx", 32768, "Max n_ctx allowed for RAM fallback")
	verbose              = flag.Bool("verbose", false, "Verbose logging")
	allowedOrigin        = flag.String("cors-origin", "*", "CORS allowed origin")
	envFile              = flag.String("env", "", "Path to .env configuration file (optional)")
	preloadModels        = flag.Bool("preload-models", false, "Preload all .gguf models at startup (disabled by default — use with care, may exhaust VRAM)")
	writeTimeout         = flag.Duration("write-timeout", 30*time.Minute, "HTTP WriteTimeout for streaming inference (use 0 for no timeout)")
	healthCheck          = flag.Bool("healthcheck", false, "Run a one-shot health probe against /health and exit")
)

// ============================================================
// Sentinels
// ============================================================

// errModelIsLoading возвращается ensureModelLoaded, когда модель ещё
// загружается (другая горутина вызвала LoadModelWithOpts). Хендлеры
// перехватывают её и отвечают 503 Service Unavailable с JSON
// {"error":"model is loading", "loading":true, "elapsedMs": N, "retryAfterMs": 3000}.
// Клиент (Ollama/OpenWebUI/наш WebUI) интерпретирует это как «подожди и повтори»
// и не разрывает соединение.
var errModelIsLoading = fmt.Errorf("model is loading")

// ============================================================
// Global state
// ============================================================

// balancerReg — глобальная ссылка на auto-registration, чтобы хендлеры
// (handleLoadModel, ensureModelLoaded) могли уведомлять балансировщик
// о событиях загрузки/выгрузки моделей. nil, если auto-registration
// отключён (CPPWORKER_BALANCER_URL не задан).
var balancerReg *balancerRegistration

var backend *cppbackend.Backend
var uptimeStart = time.Now()

// packageLogger — единая точка доступа к zap-логгеру для goroutine'ов,
// которые не получают *zap.SugaredLogger параметром. Используется в
// balancerRegistration.notifyModelLoaded (fire-and-forget callback).
// Возвращает SugaredLogger (sugar), API совместим с logger.Get().
func packageLogger() *zap.SugaredLogger {
	return logger.Get()
}

// ============================================================
// Main
// ============================================================

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

	dotEnvPath := *envFile
	if dotEnvPath == "" {
		candidates := []string{".env", "config/cppworker.env", "/app/.env"}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				dotEnvPath = p
				break
			}
		}
	}
	if dotEnvPath != "" {
		if err := cppbackend.LoadDotEnvFile(dotEnvPath); err != nil {
			log.Warnw("failed to load .env file, continuing with defaults", "path", dotEnvPath, "error", err)
		} else {
			log.Infow("loaded configuration from .env", "path", dotEnvPath)
		}
	}

	cfg := cppbackend.LoadConfigFromEnv()
	// Если --port не передан явно (равен default 18092), а CPPWORKER_PORT
	// задан в env с отличным значением — применяем env.
	if !isFlagSet("port") {
		if envPort := os.Getenv("CPPWORKER_PORT"); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil && p > 0 && p <= 65535 {
				*port = p
				log.Infow("applied CPPWORKER_PORT from env (flag default not overridden)", "port", p)
			}
		}
	}
	// RAM fallback feature flags из env (если флаги не переданы явно).
	if !isFlagSet("ram-fallback-n-ctx") {
		if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_N_CTX"); envVal != "" {
			*ramFallbackNCtx = parseBoolEnv(envVal)
		}
	}
	if !isFlagSet("ram-fallback-gpu-layers") {
		if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_GPU_LAYERS"); envVal != "" {
			if v, err := strconv.Atoi(envVal); err == nil && v >= -1 {
				*ramFallbackGpuLayers = v
			}
		}
	}
	if !isFlagSet("ram-fallback-max-n-ctx") {
		if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_MAX_N_CTX"); envVal != "" {
			if v, err := strconv.Atoi(envVal); err == nil && v >= 512 {
				*ramFallbackMaxNCtx = v
			}
		}
	}
	// WriteTimeout из env (если флаг не передан явно).
	if !isFlagSet("write-timeout") {
		if envVal := os.Getenv("CPPWORKER_WRITE_TIMEOUT"); envVal != "" {
			if d, err := time.ParseDuration(envVal); err == nil && d >= 0 {
				*writeTimeout = d
				log.Infow("applied CPPWORKER_WRITE_TIMEOUT from env", "write_timeout", d.String())
			}
		}
	}

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			cfg.Port = *port
		case "models-dir":
			cfg.ModelsDir = *modelsDir
		case "ctx-size":
			cfg.DefaultCtxSize = *ctxSize
		case "batch-size":
			cfg.DefaultBatchSize = *batchSize
		case "gpu-layers":
			cfg.DefaultGPULayers = *gpuLayers
		case "flash-attn":
			cfg.DefaultFlashAttnType = *flashAttn
		case "numa":
			cfg.DefaultNUMA = *numa
		case "no-mmap":
			cfg.DefaultUseMmap = !*noMmap
		}
	})

	if err := cfg.Validate(); err != nil {
		log.Fatalw("invalid configuration", "error", err)
	}

	log.Infow("CppBackend Worker starting",
		"port", cfg.Port, "host", cfg.Host, "modelsDir", cfg.ModelsDir,
		"gpuLayers", cfg.DefaultGPULayers, "flashAttn", cfg.DefaultFlashAttnType,
		"numa", cfg.DefaultNUMA,
		"ramFallbackNCtx", *ramFallbackNCtx,
		"ramFallbackGpuLayers", *ramFallbackGpuLayers,
		"ramFallbackMaxNCtx", *ramFallbackMaxNCtx,
		"writeTimeout", writeTimeout.String())

	if err := os.MkdirAll(cfg.ModelsDir, 0755); err != nil {
		log.Fatalw("failed to create models directory", "dir", cfg.ModelsDir, "error", err)
	}

	cc := cfg
	currentConfig = &cc

	backend = cppbackend.NewBackend(cfg)
	if err := backend.Init(); err != nil {
		log.Fatalw("failed to initialize backend", "error", err)
	}
	log.Infow("Backend initialized", "version", backend.Version(), "gpuCount", backend.GetGPUCount())

	router := setupRouter()
	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: *writeTimeout,
		IdleTimeout:  120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Infow("HTTP server listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalw("HTTP server error", "error", err)
		}
	}()

	// Авто-загрузка моделей при старте (только если --preload-models)
	if *preloadModels {
		go autoLoadModels(cfg)
	} else {
		log.Infow("preload-models disabled (models will be loaded lazily by the balancer on first request)")
	}

	// Auto-registration в балансировщике (если CPPWORKER_BALANCER_URL задан).
	balancerRegCtx, balancerRegCancel := context.WithCancel(context.Background())
	defer balancerRegCancel()
	balancerReg = newBalancerRegistration(&cfg)
	if balancerReg != nil {
		balancerReg.start(balancerRegCtx, log)
	} else {
		log.Infow("balancer auto-registration disabled (CPPWORKER_BALANCER_URL not set); cppworker will work in standalone mode")
	}

	<-quit
	log.Infow("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	balancerRegCancel()
	if balancerReg != nil {
		balancerReg.stop(ctx, log)
	}
	backend.Close()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalw("Server forced to shutdown", "error", err)
	}
	log.Infow("CppBackend Worker stopped")
}
