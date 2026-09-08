// CppBackend Worker ? HTTP ?????? ??? llama.cpp GGUF ???????
//
// ????????????? REST API ??? ????????/???????? GGUF ???????,
// ????????? (??????????? ? ????????), ?????????? multi-GPU,
// ??????????? ? API ???????? Ollama.
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
	"strings"
	"syscall"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"

	"go.uber.org/zap"
)

// ============================================================
// ????? ????????? ??????
// ============================================================

var (
	startupTime = time.Now() // ???????????? ??? /api/diagnostics uptime.
	port        = flag.Int("port", 18092, "HTTP server port (default 18092; 18091 is legacy)")
	modelsDir   = flag.String("models-dir", "./models", "Directory with GGUF model files")
	// DefaultCtxSize = 32768 (? ?? 8192): 8192 ? ??????? ??? OpenWebUI ? tools,
	// ?? ??? production-????????????? ? ???????? ???????? ????? ????????????.
	// 32768 ????????? system + history ~7000 ??????? + user message + ????? ~25000.
	// ???????? ???????? ???????????? config/cppworker-defaults.json (single source of truth);
	// ???? flag default ???????????? ?????? ??? --ctx-size ??? env ? ??? JSON.
	ctxSize              = flag.Int("ctx-size", 32768, "Default context size")
	batchSize            = flag.Int("batch-size", 512, "Default batch size")
	gpuLayers            = flag.Int("gpu-layers", -1, "GPU layers (-1=all, 0=CPU)")
	flashAttn            = flag.Int("flash-attn", -1, "Flash Attention type: -1=auto, 0=disabled, 1=enabled")
	numa                 = flag.Bool("numa", false, "Enable NUMA optimization")
	noMmap               = flag.Bool("no-mmap", false, "Disable mmap")
	ramFallbackNCtx      = flag.Bool("ram-fallback-n-ctx", false, "Auto-reload model with requested n_ctx using RAM when VRAM is insufficient")
	ramFallbackGpuLayers = flag.Int("ram-fallback-gpu-layers", -1, "GPU layers to use during RAM fallback (-1=keep current, 0=CPU-only, -2=AUTO via auto-offload)")
	ramFallbackMaxNCtx   = flag.Int("ram-fallback-max-n-ctx", 32768, "Max n_ctx allowed for RAM fallback")
	// ramFallbackAllowTools ? ? 2026-06-23 ????????? reload ??? tools,
	// ????? balancer (preflight) ??? ??????????? ??????????? n_ctx ??? ???????
	// prompt ?? Cline/OpenWebUI. ?????? ????????? (reload off ??? tools) ?????
	// ??????? ????? --ram-fallback-allow-tools=false ??? CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS=false.
	ramFallbackAllowTools = flag.Bool("ram-fallback-allow-tools", true, "Allow RAM-fallback reload for tools-requests (works with balancer preflight to dynamically resize n_ctx). Default true.")
	// Round 37 (2026-08-18): -feasible / -autoLoad flags for auto-adapt n_ctx.
	// Production bug: profile 32768 + GGUF 262144 → balancer 413 без объяснения.
	// -feasible: print feasible n_ctx для модели и exit (no load, no HTTP server).
	// -autoLoad: auto-detect best n_ctx, load model, exit (calls /api/models/load).
	feasibleModel = flag.String("feasible", "", "Print feasible n_ctx for model and exit (Round 37: use BEFORE -load to validate profile vs hardware). Implies -no-server.")
	autoLoadModel = flag.String("auto-load", "", "Auto-detect best n_ctx via ComputeFeasible and load model. Implies -no-server.")
	// autoOffload ? ????-?????? ????? GPU-????? ?? ?????? ??????? .gguf ?????
	// ? ????????? VRAM. ???????????? ??? ram-fallback-gpu-layers=-2 ??? ???
	// handleLoadModel/handleCppWorkerUpdateConfig, ???? ?????? ?? ???????.
	autoOffload = flag.Bool("auto-offload", false, "Auto-calculate gpu_layers based on model size and available VRAM (solves OOM for 19GB+ models on 24GB GPU)")
	// autoTuneNCtx ? AutoTuneNCtx: ?????????? n_ctx ? gpu_layers ??? reload.
	// ??? ????????? cppworker ??? RAM-fallback reload ???????? ???????
	// ???????????? n_ctx, ??????? ?????????? ? VRAM (? ?????? partial offload
	// ????? mmap ? RAM). ???? requested_n_ctx ?? ??????? ???? ??? cpu-only
	// offload (gpu_layers=0) ? ????????? n_ctx ?? ??????????? ??????????.
	// ??????: ?Cline ??????? 53K prompt, ????????? n_ctx=53K, VRAM=8GB? ?
	// cppworker ????????????? ?????? ? gpu_layers=0 (????? mmap ? RAM) ?
	// n_ctx=??????????? ????????? (???????? 32K), ????? inference ??????
	// ??? ???? 3 ?prompt too long?.
	autoTuneNCtx  = flag.Bool("auto-tune-nctx", false, "Auto-tune n_ctx + gpu_layers on RAM-fallback reload based on available VRAM/RAM (solves 'prompt too long' when VRAM is insufficient)")
	allowedOrigin = flag.String("cors-origin", "*", "CORS allowed origin")
	envFile       = flag.String("env", "", "Path to .env configuration file (optional)")
	preloadModels = flag.Bool("preload-models", false, "Preload all .gguf models at startup (disabled by default ? use with care, may exhaust VRAM)")
	writeTimeout  = flag.Duration("write-timeout", 30*time.Minute, "HTTP WriteTimeout for streaming inference (use 0 for no timeout)")
	healthCheck   = flag.Bool("healthcheck", false, "Run a one-shot health probe against /health and exit")
	verbose       = flag.Bool("verbose", false, "Enable verbose (debug) logging")
)

// ============================================================
// Sentinels
// ============================================================

// errModelIsLoading ???????????? ensureModelLoaded, ????? ?????? ???
// ??????????? (?????? ???????? ??????? LoadModelWithOpts). ????????
// ????????????? ?? ? ???????? 503 Service Unavailable ? JSON
// {"error":"model is loading", "loading":true, "elapsedMs": N, "retryAfterMs": 3000}.
// ?????? (Ollama/OpenWebUI/??? WebUI) ?????????????? ??? ??? ???????? ? ????????
// ? ?? ????????? ??????????.
var errModelIsLoading = fmt.Errorf("model is loading")

// ============================================================
// Global state
// ============================================================

// balancerReg ? ?????????? ?????? ?? auto-registration, ????? ????????
// (handleLoadModel, ensureModelLoaded) ????? ?????????? ?????????????
// ? ???????? ????????/???????? ???????. nil, ???? auto-registration
// ???????? (CPPWORKER_BALANCER_URL ?? ?????).
var balancerReg *balancerRegistration

// profileSyncer — pull-based per-model profile sync from balancer (Round 26, 2026-08-06).
// Самостоятельный модуль: не зависит от balancerReg (registration может быть отключена
// через CPPWORKER_REGISTER_DISABLE, а sync профилей всё равно нужен).
// nil, если balancer URL не задан или sync отключён.
var profileSyncer *profileSyncerT
var feasibleSync *feasibleSyncT

var backend *cppbackend.Backend
var uptimeStart = time.Now()

// packageLogger ? ?????? ????? ??????? ? zap-??????? ??? goroutine'??,
// ??????? ?? ???????? *zap.SugaredLogger ??????????. ???????????? ?
// balancerRegistration.notifyModelLoaded (fire-and-forget callback).
// ?????????? SugaredLogger (sugar), API ????????? ? logger.Get().
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
	// ???? --port ?? ??????? ???? (????? default 18092), ? CPPWORKER_PORT
	// ????? ? env ? ???????? ????????? ? ????????? env.
	if !isFlagSet("port") {
		if envPort := os.Getenv("CPPWORKER_PORT"); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil && p > 0 && p <= 65535 {
				*port = p
				log.Infow("applied CPPWORKER_PORT from env (flag default not overridden)", "port", p)
			}
		}
	}
	// RAM fallback feature flags ?? env (???? ????? ?? ???????? ????).
	// R60.18 F4: единый helper — вызывается и на startup, и на /config/reload
	// (handlers_config.go) с одинаковым приоритетом CLI flag > env > default.
	applyRAMFallbackFromEnvWithSkip(ramFallbackNCtx, ramFallbackGpuLayers, ramFallbackMaxNCtx,
		isFlagSet("ram-fallback-n-ctx"), isFlagSet("ram-fallback-gpu-layers"), isFlagSet("ram-fallback-max-n-ctx"))
	// ram-fallback-allow-tools ?? env (???? ???? ?? ??????? ????).
	if !isFlagSet("ram-fallback-allow-tools") {
		if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS"); envVal != "" {
			*ramFallbackAllowTools = parseBoolEnv(envVal)
			log.Infow("applied CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS from env",
				"ram_fallback_allow_tools", *ramFallbackAllowTools)
		}
	}
	// ????????? ???? ? ?????????? ?????????? (???????????? ? inference.go).
	tryRamFallbackReloadAllowTools = *ramFallbackAllowTools
	// auto-offload ?? env (???? ???? ?? ??????? ????).
	if !isFlagSet("auto-offload") {
		if envVal := os.Getenv("CPPWORKER_AUTO_OFFLOAD"); envVal != "" {
			*autoOffload = parseBoolEnv(envVal)
			log.Infow("applied CPPWORKER_AUTO_OFFLOAD from env", "auto_offload", *autoOffload)
		}
	}
	// auto-tune-nctx ?? env (???? ???? ?? ??????? ????).
	if !isFlagSet("auto-tune-nctx") {
		if envVal := os.Getenv("CPPWORKER_AUTO_TUNE_NCTX"); envVal != "" {
			*autoTuneNCtx = parseBoolEnv(envVal)
			log.Infow("applied CPPWORKER_AUTO_TUNE_NCTX from env", "auto_tune_nctx", *autoTuneNCtx)
		}
	}
	// CPPWORKER_AUTO_KV_CACHE: auto-select optimal kvCacheType in fallback_no_meta.
	// When true (default), tries q4_0→q8_0→f16 based on available VRAM.
	// Set to false to always use f16. No CLI flag — env-only.
	if envVal := os.Getenv("CPPWORKER_AUTO_KV_CACHE"); envVal != "" {
		autoKVCacheEnabled = parseBoolEnv(envVal)
		log.Infow("applied CPPWORKER_AUTO_KV_CACHE from env", "auto_kv_cache", autoKVCacheEnabled)
	}

	// WriteTimeout ?? env (???? ???? ?? ??????? ????).
	if !isFlagSet("write-timeout") {
		if envVal := os.Getenv("CPPWORKER_WRITE_TIMEOUT"); envVal != "" {
			if d, err := time.ParseDuration(envVal); err == nil && d >= 0 {
				*writeTimeout = d
				log.Infow("applied CPPWORKER_WRITE_TIMEOUT from env", "write_timeout", d.String())
			}
		}
	}

	// Phase 8 P.4 (2026-07-11): multi-GPU tensor_split + split_mode.
	// CPPWORKER_TENSOR_SPLIT — comma-separated proportions, e.g. "0.5,0.5"
	// for 2-GPU box, "0.7,0.3" for uneven split. Empty = auto.
	if envVal := strings.TrimSpace(os.Getenv("CPPWORKER_TENSOR_SPLIT")); envVal != "" {
		parts := strings.Split(envVal, ",")
		ts := make([]float32, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			v, perr := strconv.ParseFloat(p, 32)
			if perr != nil || v < 0 || v > 1 {
				log.Warnw("CPPWORKER_TENSOR_SPLIT: invalid proportion, ignoring",
					"value", p, "error", perr)
				ts = nil
				break
			}
			ts = append(ts, float32(v))
		}
		if len(ts) > 0 {
			// Scale to sum=1.0 (llama.cpp requirement).
			var sum float32
			for _, v := range ts {
				sum += v
			}
			if sum > 0 {
				for i := range ts {
					ts[i] /= sum
				}
			}
			cfg.DefaultTensorSplit = ts
			log.Infow("applied CPPWORKER_TENSOR_SPLIT from env",
				"proportions", ts, "gpus", len(ts))
		}
	}
	// CPPWORKER_SPLIT_MODE — -1=default (layer), 0=none, 1=layer,
	// 2=row (deprecated), 3=tensor (experimental, requires NCCL).
	if envVal := strings.TrimSpace(os.Getenv("CPPWORKER_SPLIT_MODE")); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v >= -1 && v <= 3 {
			cfg.DefaultSplitMode = v
			log.Infow("applied CPPWORKER_SPLIT_MODE from env", "mode", v)
		} else {
			log.Warnw("CPPWORKER_SPLIT_MODE: invalid value, ignoring (use -1..3)", "value", envVal)
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
		"ramFallbackAllowTools", *ramFallbackAllowTools,
		"autoOffload", *autoOffload,
		"autoTuneNCtx", *autoTuneNCtx,
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
	// Round 37 (2026-08-18): register backend for CLI tools (-feasible, -auto-load).
	cppbackend.SetGlobalBackend(backend)
	log.Infow("Backend initialized", "version", backend.Version(), "gpuCount", backend.GetGPUCount())

	// Round 37 (2026-08-18): CLI mode — exit BEFORE starting HTTP server.
	// -feasible <model>: print feasible n_ctx and exit.
	// -auto-load <model>: auto-detect n_ctx, load model, exit.
	// Оба используют GetBackend() → SetGlobalBackend выше обязателен.
	if *feasibleModel != "" {
		os.Exit(runFeasible(*feasibleModel, os.Stdout))
	}
	if *autoLoadModel != "" {
		os.Exit(runAutoLoad(*autoLoadModel, os.Stdout))
	}

	// Round 17 (2026-07-31): startup self-test для reasoning routing.
	// Логируем какие модели БУДУТ иметь reasoning routing при LoadModel —
	// это помогает оператору сразу увидеть конфигурационные баги (типа
	// "gemma-4-it" матчит "gemma-4" prefix и reasoning попадает в IT-model
	// которая его не поддерживает).
	reasoningList := getReasoningArchList()
	log.Infow("reasoning self-test: default reasoning prefix list",
		"prefixes_count", len(reasoningList),
		"prefixes", reasoningList,
		"hint", "set CPPWORKER_REASONING_ARCHS=... to add custom prefixes; per-model override via load-with-params enableReasoning")

	// Initialize adaptive loader (EnvironmentProfile, NaN-healer, strategy selector)
	log.Infow("adaptive: initializing EnvironmentProfile...")
	initAdaptiveLoader(func(modelName string, reductionPct float64) error {
		log.Infow("adaptive NaN-heal: auto-reload triggered", "model", modelName, "reduction", reductionPct)
		if err := backend.UnloadModel(modelName); err != nil {
			return err
		}
		return nil
	})

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

	// ????-???????? ??????? ??? ?????? (?????? ???? --preload-models)
	if *preloadModels {
		go autoLoadModels(cfg)
	} else {
		log.Infow("preload-models disabled (models will be loaded lazily by the balancer on first request)")
	}

	// Auto-registration ? ?????????????? (???? CPPWORKER_BALANCER_URL ?????).
	balancerRegCtx, balancerRegCancel := context.WithCancel(context.Background())
	defer balancerRegCancel()
	balancerReg = newBalancerRegistration(&cfg)
	if balancerReg != nil {
		balancerReg.start(balancerRegCtx, log)
	} else {
		switch {
		case isRegisterDisabled():
			log.Infow("balancer Go-side auto-registration disabled via CPPWORKER_REGISTER_DISABLE; "+
				"registration is expected to be done by external script (e.g. bundled register-with-balancer.sh)",
				"balancerURL", os.Getenv("CPPWORKER_BALANCER_URL"))
		default:
			log.Infow("balancer auto-registration disabled (CPPWORKER_BALANCER_URL not set); cppworker will work in standalone mode")
		}
	}

	// Round 26 (2026-08-06): pull-based per-model profile sync. Self-healing после recreate:
	// cppworker сам подтянет актуальный профиль из балансера (contextLength, numGpuLayers,
	// kvCacheType, ...) ДО того, как начнёт обслуживать первый запрос.
	// Работает независимо от balancerReg (даже если registration отключена).
	balancerURL := resolveBalancerURL()
	balancerToken := resolveBalancerToken()
	if balancerURL != "" {
		// backendID: используем то же значение, что и для register, чтобы логи можно было сопоставить.
		// Если register отключён (CPPWORKER_REGISTER_DISABLE=true), profileSyncer всё равно работает —
		// ему нужен только URL балансера + токен.
		backendID := os.Getenv("CPPWORKER_REGISTER_NAME")
		if backendID == "" {
			backendID = "cppworker-unknown"
		}
		profileSyncer = newProfileSyncer(balancerURL, balancerToken, backendID, log)
		profileSyncer.start(balancerRegCtx)
		log.Infow("profileSyncer started (pull-based profile sync from balancer)",
			"balancerURL", balancerURL, "backendID", backendID)

		// Round 37 (2026-08-18): feasibleSync — background warning when
		// profile.contextLength is conservative vs hardware. Detects the
		// 2026-08-18 production bug class (profile 32768 vs feasible 65536).
		// Works only if profileSyncer is active (need profile.contextLength to compare).
		feasibleSync = newFeasibleSync(5*time.Minute, 1*time.Hour, balancerURL)
		feasibleSync.logger = log
		feasibleSync.start(balancerRegCtx)
		log.Infow("feasibleSync started (background conservative-profile detector)",
			"interval", 5*time.Minute, "warnThrottle", 1*time.Hour)
	} else {
		log.Infow("profileSyncer disabled (no balancer URL env) — feasibleSync also disabled (needs profile to compare)")
	}

	<-quit
	log.Infow("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	balancerRegCancel()
	if balancerReg != nil {
		balancerReg.stop(ctx, log)
	}
	// Round 26 (2026-08-06): graceful stop profile syncer.
	if profileSyncer != nil {
		profileSyncer.stop()
	}
	backend.Close()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalw("Server forced to shutdown", "error", err)
	}
	log.Infow("CppBackend Worker stopped")
}
