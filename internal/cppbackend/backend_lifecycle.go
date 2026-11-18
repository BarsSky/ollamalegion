package cppbackend

import (
	"fmt"
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
	"os"
	"strings"
	"time"
)

// NewBackend создаёт новый CppBackend
func NewBackend(cfg Config) *Backend {
	if cfg.DefaultCtxSize <= 0 {
		cfg.DefaultCtxSize = 4096
	}
	if cfg.DefaultBatchSize <= 0 {
		cfg.DefaultBatchSize = 512
	}

	metrics := NewMetrics()
	modelManager := NewModelManager(cfg.ModelsDir, cfg)

	b := &Backend{
		cfg:          cfg,
		models:       make(map[string]*modelInstance),
		startTime:    time.Now(),
		modelManager: modelManager,
		metrics:      metrics,
		gpuManager:   NewGPUManager(cfg.TensorSplitStrategy),
		loading:      make(map[string]chan struct{}),
		loadClaims:   make(map[string]*loadClaimMetaR303),
		inFlight:     NewInFlightCounter(),
	}

	// Idle unload manager.
	// Источник idleTimeout — отдельное поле IdleUnloadMinutes (по дефолту 0 = ВЫКЛЮЧЕНО).
	// Раньше здесь использовался MetricsRetentionS (по дефолту 3600 секунд = 60 минут),
	// что путало метрики с автовыгрузкой и приводило к выгрузке модели через час простоя
	// даже если пользователь этого не хотел. Теперь для автовыгрузки нужно явно задать
	// CPPWORKER_IDLE_UNLOAD_MINUTES (или idleUnloadMinutes в JSON-конфиге).
	if cfg.IdleUnloadMinutes > 0 {
		b.idleUnloader = NewIdleUnloadManager(b, time.Duration(cfg.IdleUnloadMinutes)*time.Minute)
	}
	// HuggingFace downloader
	b.hfDownloader = NewHuggingFaceDownloader(
		cfg.HuggingFaceToken,
		cfg.HFMirror,
		cfg.DownloadsDir,
		cfg.ModelsDir,
	)
	// R66d (2026-09-23): после успешной загрузки пересканируем каталог моделей.
	// ListModels()/GetModelMeta()/FindModelByPath() работают по кэшу ggufFiles,
	// который заполняется только в ScanModels(), поэтому без этого скачанная
	// модель не появлялась ни в /api/models/files (вкладка GGUF в WebUI), ни в
	// списке доступных для загрузки — до рестарта cppworker.
	b.hfDownloader.SetOnDownloadComplete(func(filename string) {
		if b.modelManager == nil {
			return
		}
		if _, err := b.modelManager.ScanModels(); err != nil {
			logger.Get().Warnw("rescan after HF download failed",
				"filename", filename, "error", err)
			return
		}
		logger.Get().Infow("rescan after HF download complete", "filename", filename)
	})

	// Round 18 P0.2 (2026-08-03): ActiveGenerations tracker для /api/cancel.
	// Инициализируется здесь (не в Init) потому что не требует GPU/bridge.
	b.activeGenerations = NewActiveGenerations()

	// Round 18 P0.3 (2026-08-04): UserTracker для per-user admission.
	b.userTracker = NewUserTracker()

	return b
}

// ActiveGenerations возвращает tracker активных генераций для /api/cancel.
// Round 18 P0.2 (2026-08-03). nil-safe.
func (b *Backend) ActiveGenerations() *ActiveGenerations {
	if b == nil {
		return nil
	}
	return b.activeGenerations
}

// UserTracker возвращает per-user parallel counter для admission control.
// Round 18 P0.3 (2026-08-04). nil-safe.
func (b *Backend) UserTracker() *UserTracker {
	if b == nil {
		return nil
	}
	return b.userTracker
}

// Init инициализирует backend (инициализация bridge + GPU discovery)
func (b *Backend) Init() error {
	var initErr error
	b.initOnce.Do(func() {
		log := logger.Get()
		log.Infow("initializing CppBackend")

		// Инициализация C bridge
		if err := bridge.Init(); err != nil {
			initErr = fmt.Errorf("bridge init failed: %w", err)
			b.initErr = initErr
			log.Errorw("bridge init failed", "error", initErr)
			return
		}

		// GPU Discovery
		b.gpuCount = bridge.GetGPUCount()
		log.Infow("GPU discovery", "count", b.gpuCount)

		b.gpuDevices = make([]bridge.GPUDevice, 0, b.gpuCount)
		for i := 0; i < b.gpuCount; i++ {
			dev, err := bridge.GetGPUInfo(i)
			if err != nil {
				log.Warnw("failed to get GPU info", "index", i, "error", err)
				continue
			}
			b.gpuDevices = append(b.gpuDevices, *dev)
			log.Infow("GPU detected",
				"index", dev.Index,
				"name", dev.Name,
				"vramTotalMB", dev.VRAMTotalMB,
				"vramFreeMB", dev.VRAMFreeMB,
				"computeCap", fmt.Sprintf("%d.%d", dev.ComputeCapMajor, dev.ComputeCapMinor))
		}

		// Инициализируем GPUManager
		if b.gpuManager != nil && b.gpuCount > 0 {
			b.gpuManager.InitGPU(b.gpuDevices)
		}

		// R135 (2026-10-16): совместимы ли CUDA-ядра ОБРАЗА с этими картами.
		//
		// ЖИВОЙ ДЕФЕКТ: образ, собранный под sm_86/sm_120, на Quadro RTX 4000
		// (sm_75) загружал модель успешно и падал с SIGABRT на первом ядре
		// («CUDA error: no kernel image is available for execution on the
		// device»). Снаружи это «backend недоступен» и следующая загрузка с
		// другими параметрами. Проверка дешёвая (строки + capability из NVML) и
		// выполняется ДО первой загрузки модели, чтобы причина была в логе и в
		// API, а не выяснялась по стеку из C-моста.
		if warn := b.LogCUDAArchCompat(); warn != "" {
			b.cudaArchWarning = warn
		}

		// Сканируем директорию моделей
		if b.modelManager != nil {
			if _, err := b.modelManager.ScanModels(); err != nil {
				log.Warnw("model scan failed", "error", err)
			}
		}

		// Запускаем idle unloader
		if b.idleUnloader != nil {
			b.idleUnloader.Start()
		}

		log.Infow("CppBackend initialized",
			"bridgeVersion", bridge.Version(),
			"gpuCount", b.gpuCount,
			"port", b.cfg.Port)
	})

	return initErr
}

// Version возвращает версию bridge
func (b *Backend) Version() string {
	return bridge.Version()
}

// Config возвращает конфигурацию backend
func (b *Backend) Config() Config {
	return b.cfg
}

// ModelManager возвращает менеджер моделей
func (b *Backend) ModelManager() *ModelManager {
	return b.modelManager
}

// InFlight возвращает per-model счётчик активных inference-запросов.
// Используется в handleReloadModel чтобы дождаться завершения
// всех активных запросов перед UnloadModel — иначе reload обрывает
// HTTP-соединения (EOF клиенту). Подробности см. в inflight.go.
//
// nil-safe: если NewBackend не был вызван, возвращает nil — Inc/Dec
// на nil-получателе являются no-op.
func (b *Backend) InFlight() *InFlightCounter {
	return b.inFlight
}

// SetReloadPending устанавливает имя модели, для которой сейчас идёт reload.
// Используется в handleReloadModel и читается через GetReloadPending
// для прокидывания в /api/metrics heartbeat (reload_pending).
// Пустая строка = нет активного reload.
func (b *Backend) SetReloadPending(modelName string) {
	b.reloadMu.Lock()
	defer b.reloadMu.Unlock()
	b.reloadPending = modelName
	if modelName != "" {
		b.reloadStartedAt = time.Now()
	} else {
		b.reloadStartedAt = time.Time{}
	}
}

// GetReloadPending возвращает имя модели, для которой идёт reload
// (пустая строка = нет активного reload).
func (b *Backend) GetReloadPending() string {
	b.reloadMu.Lock()
	defer b.reloadMu.Unlock()
	return b.reloadPending
}

// Metrics возвращает метрики backend
func (b *Backend) Metrics() *Metrics {
	return b.metrics
}

// HFDownloader возвращает загрузчик HuggingFace
func (b *Backend) HFDownloader() *HuggingFaceDownloader {
	return b.hfDownloader
}

// ============================================================
// Управление моделями
// ============================================================

// resolveNParallel — R83 (2026-09-29): сколько параллельных слотов запрошено
// для одной загружаемой модели.
//
//   - perModel > 0  — per-model запрос (WebUI/профиль/поле `parallel` в
//     /api/models/load-with-params) всегда побеждает;
//   - иначе cfgDefault > 0 (env CPPWORKER_N_PARALLEL) — инфраструктурный
//     дефолт контейнера;
//   - иначе 0 — «не задано», C-bridge оставляет свой внутренний дефолт (1).
//
// Одно правило на C-bridge (cfg.NParallel) и на Go SlotManager: иначе
// llama.cpp выделяет KV-cache под N слотов, а cppworker обслуживает 1 —
// ровно тот рассинхрон, из-за которого второй параллельный клиент молча
// висел в SlotManager.Acquire до таймаута (503).
func resolveNParallel(perModel int, cfgDefault int) int {
	if perModel > 0 {
		return perModel
	}
	if cfgDefault > 0 {
		return cfgDefault
	}
	return 0
}

// visionInferenceEnabled — R248/B3: публиковать ли capabilities.vision.
//
// Проектор (mmproj) уже загружен и mtmd подтвердил vision; флаг отвечает на
// другой вопрос — подключён ли приём картинок в запросах. С R248 он подключён
// на обеих поверхностях (/api/chat и /v1/chat/completions), включая
// буферизованный streaming, поэтому по умолчанию ВКЛЮЧЕН.
//
// CPPWORKER_VISION_MMPROJ=0/false/off — операторский «стоп»: модель остаётся
// текстовой снаружи (картинки → честный 415), даже если проектор загружен.
func visionInferenceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_VISION_MMPROJ"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// parallelSlotsFor — минимальное число слотов SlotManager (никогда < 1):
// 0/отрицательное «запрошено» означает одиночный слот.
func parallelSlotsFor(requested int) int {
	if requested > 0 {
		return requested
	}
	return 1
}
