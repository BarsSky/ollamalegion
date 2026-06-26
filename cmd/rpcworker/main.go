// Command rpcworker — Worker HTTP Server для RPC Coordinator (Вариант B).
//
// Worker обслуживает срез модели (набор слоёв) и отвечает на inference-запросы
// от `internal/rpccoordinator.ModelCoordinator`. Эндпоинты описаны в
// `internal/rpccoordinator/worker_client.go`.
//
// Использование:
//
//	rpcworker --port 18080 --layers "1-40" --worker-id gpu-1 \
//	          --models-dir /models --coordinator-url http://coord:18090
//
// Или через переменные окружения (см. internal/rpcworker/config.go):
//
//	RPC_WORKER_PORT=18080 RPC_WORKER_LAYERS=1-40 ...
//
// В Docker используется встроенная healthcheck-команда:
//
//	rpcworker -healthcheck
//
// Которая сама читает RPC_WORKER_PORT и шлёт GET /rpc/health на localhost.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ollama-loadbalancer/internal/rpcworker"
	"ollama-loadbalancer/pkg/logger"
)

// Version — версия rpcworker (инжектируется через ldflags при сборке).
var Version = "rpcworker-0.1.0-stub"

func main() {
	// Спец-команда: rpcworker -healthcheck.
	if len(os.Args) >= 2 && os.Args[1] == "-healthcheck" {
		os.Exit(runHealthCheck())
	}

	var (
		flagHost        = flag.String("host", "", "HTTP listen host (RPC_WORKER_HOST)")
		flagPort        = flag.Int("port", 0, "HTTP listen port (RPC_WORKER_PORT)")
		flagWorkerID    = flag.String("worker-id", "", "Worker ID (RPC_WORKER_ID)")
		flagLayers      = flag.String("layers", "", "Slice layers, e.g. \"1-40\" (RPC_WORKER_LAYERS)")
		flagModelsDir   = flag.String("models-dir", "", "Directory with .gguf files (RPC_WORKER_MODELS_DIR)")
		flagCoordURL    = flag.String("coordinator-url", "", "Coordinator URL for auto-register (RPC_WORKER_COORDINATOR_URL)")
		flagToken       = flag.String("token", "", "Bearer token for auth (RPC_WORKER_TOKEN)")
		flagLogLevel    = flag.String("log-level", "", "Log level: debug|info|warn|error (RPC_WORKER_LOG_LEVEL)")
		flagStubMode    = flag.Bool("stub", false, "Force stub mode (no real llama.cpp)")
		flagShowVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *flagShowVersion {
		fmt.Printf("rpcworker %s\n", Version)
		os.Exit(0)
	}

	// 1. Конфигурация из env + флаги (флаги в приоритете).
	cfg, err := rpcworker.LoadConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rpcworker: invalid env config: %v\n", err)
		os.Exit(2)
	}
	if *flagHost != "" {
		cfg.Host = *flagHost
	}
	if *flagPort > 0 {
		cfg.Port = *flagPort
	}
	if *flagWorkerID != "" {
		cfg.WorkerID = *flagWorkerID
	}
	if *flagLayers != "" {
		cfg.SliceLayers = *flagLayers
		if err := cfg.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "rpcworker: invalid --layers %q: %v\n", *flagLayers, err)
			os.Exit(2)
		}
	}
	if *flagModelsDir != "" {
		cfg.ModelsDir = *flagModelsDir
	}
	if *flagCoordURL != "" {
		cfg.CoordinatorURL = *flagCoordURL
	}
	if *flagToken != "" {
		cfg.AuthToken = *flagToken
	}
	if *flagLogLevel != "" {
		cfg.LogLevel = *flagLogLevel
	}
	if *flagStubMode {
		cfg.StubMode = true
	}

	logger.Init(cfg.LogLevel)
	logger.Get().Infow("rpcworker starting",
		"version", Version,
		"worker_id", cfg.WorkerID,
		"host", cfg.Host,
		"port", cfg.Port,
		"layers", cfg.SliceLayers,
		"models_dir", cfg.ModelsDir,
		"coordinator_url", cfg.CoordinatorURL,
		"stub_mode", cfg.StubMode,
	)

	if err := cfg.Validate(); err != nil {
		logger.Get().Fatalw("invalid config", "error", err)
	}

	// 2. Сервер.
	manager := rpcworker.NewModelManager(cfg)
	srv := rpcworker.NewWorkerServer(cfg, manager, Version)

	// 3. Опциональная авто-регистрация в координаторе.
	if cfg.CoordinatorURL != "" {
		go runCoordinatorHeartbeat(cfg, manager)
	}

	// 4. Graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case sig := <-sigCh:
		logger.Get().Infow("rpcworker shutdown signal", "signal", sig.String())
	case err := <-errCh:
		if err != nil {
			logger.Get().Errorw("rpcworker serve error", "error", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Get().Errorw("rpcworker shutdown error", "error", err)
		os.Exit(1)
	}
	logger.Get().Infow("rpcworker stopped")
}

// runCoordinatorHeartbeat — фоновый heartbeat на coordinator.
//
// Периодически шлёт POST на `{coordinator}/api/v1/rpc/workers/register`
// (контракт будет уточнён в B2). В B1 — best-effort, ошибки только логируются.
func runCoordinatorHeartbeat(cfg rpcworker.WorkerConfig, manager *rpcworker.ModelManager) {
	url := cfg.CoordinatorURL + "/api/v1/rpc/workers/register"
	ticker := time.NewTicker(cfg.HealthCheckInterval)
	defer ticker.Stop()

	// Первая отправка сразу (не ждём тикер).
	sendRegistration(url, cfg, manager)

	for range ticker.C {
		sendRegistration(url, cfg, manager)
	}
}

func sendRegistration(url string, cfg rpcworker.WorkerConfig, manager *rpcworker.ModelManager) {
	payload := map[string]interface{}{
		"worker_id":     cfg.WorkerID,
		"host":          cfg.Host,
		"port":          cfg.Port,
		"slice_layers":  cfg.SliceLayers,
		"stub_mode":     cfg.StubMode,
		"loaded_slices": manager.ListSlices(),
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := newJSONRequest("POST", url, payload)
	if err != nil {
		logger.Get().Warnw("heartbeat: bad request", "error", err)
		return
	}
	if cfg.AuthToken != "" {
		req.Header.Set("X-API-Token", cfg.AuthToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		logger.Get().Warnw("heartbeat: coordinator unreachable", "url", url, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		logger.Get().Warnw("heartbeat: coordinator returned error", "url", url, "status", resp.StatusCode)
		return
	}
	logger.Get().Debugw("heartbeat: ok", "url", url, "status", resp.StatusCode)
}

// runHealthCheck — реализация `-healthcheck`.
//
// Делает GET /rpc/health на localhost:<port> и выходит с 0/1.
func runHealthCheck() int {
	port := os.Getenv("RPC_WORKER_PORT")
	if port == "" {
		port = "18080"
	}
	timeout := 3 * time.Second
	if t := os.Getenv("RPC_WORKER_HEALTHCHECK_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%s/rpc/health", port)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rpcworker -healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "rpcworker -healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}