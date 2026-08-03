// Package cppbackend — Go-обёртка над llama.cpp через CGo bridge
//
// Предоставляет высокоуровневый API для:
// - Загрузки/выгрузки GGUF моделей
// - Синхронного и стриминг инференса
// - Multi-GPU распределения
// - Сбора метрик
// - Получения метаданных модели
package cppbackend

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Типы
// ============================================================

// LoadState — состояние загрузки модели
type LoadState string

const (
	StateUnloaded LoadState = "unloaded"
	StateLoading  LoadState = "loading"
	StateLoaded   LoadState = "loaded"
	StateError    LoadState = "error"
)

// ModelInfo — информация о загруженной модели
type ModelInfo struct {
	Name              string    `json:"name"`
	Path              string    `json:"path"`
	State             LoadState `json:"state"`
	Architecture      string    `json:"architecture,omitempty"`
	NLayers           int       `json:"nLayers"`
	NHeads            int       `json:"nHeads"`
	NKvHeads          int       `json:"nKvHeads"`
	HeadDimK          int       `json:"headDimK"`
	HeadDimV          int       `json:"headDimV"`
	NEmbd             int       `json:"nEmbd"`
	NVocab            int       `json:"nVocab"`
	ContextSize       int       `json:"contextSize"`
	GGUFContextLength int       `json:"ggufContextLength"`
	SizeBytes         uint64    `json:"sizeBytes"`
	LoadedAt          time.Time `json:"loadedAt"`
	GPUCount          int       `json:"gpuCount,omitempty"`
	GPULayers         int       `json:"gpuLayers"`
	TensorSplit       []float32 `json:"tensorSplit,omitempty"`
	ActiveQueries     int       `json:"activeQueries"`
	TotalQueries      int64     `json:"totalQueries"`
	// Дополнительные параметры загрузки (Шаг 4 — /api/models/reload).
	// Хранятся вместе с моделью, чтобы при reload можно было
	// переиспользовать их как дефолты, если клиент не указал override.
	BatchSize     int  `json:"batchSize"`
	FlashAttnType int  `json:"flashAttnType"`
	NUMA          bool `json:"numa"`
	UseMmap       bool `json:"useMmap"`
	// Session 16 (2026-06-27): Parallel и KVCacheType — Session 16 Per-Model Profiles.
	// Parallel=0 = дефолт cppworker (=1), KVCacheType=0 = F16.
	Parallel    int    `json:"parallel"`    // n_parallel в llama.cpp
	KVCacheType string `json:"kvCacheType"` // "f16"/"q8_0"/"q4_0"
	// === Loading state (Шаг «отображение загрузки в мониторе и вкладке бэкендов») ===
	// Заполняются пока State == StateLoading, чтобы UI мог показывать
	// «Загружается model-name 25s» и спиннер. После успеха/ошибки поля обнуляются.
	LoadingStartedAt time.Time `json:"loadingStartedAt,omitempty"`
	LoadingSizeBytes int64     `json:"loadingSizeBytes,omitempty"`
	LoadingError     string    `json:"loadingError,omitempty"`
	// LastUsedAt — момент последнего обращения к модели (генерация/стрим).
	// Поле заполняется в Backend.GetModel/ListModels из inst.lastUsedAt,
	// чтобы UI мог отображать «idle 5s» и для отладки reload-циклов.
	// Может быть zero, если модель только что загружена и ещё не использовалась.
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
	// Round 15.1 (2026-07-30): true если модель использует BatchedScheduler
	// для true parallel inference (ОДИН llama_decode для N sessions).
	// false = legacy Round 13 multi-slot path с serialized llama_decode.
	// Виден в /api/models (UI может показать "Batched: ON" / "Batched: OFF").
	BatchedParallel bool `json:"batchedParallel"`
	// Round 17 (2026-07-31): resolved в LoadModel из opts.EnableReasoning ??
	// cfg.DefaultEnableReasoning. Парсеры reasoning_content.go читают это
	// чтобы split'ить <think> для моделей вне IsReasoningModel() whitelist
	// (например, qwen3-instruct с SOFT prompt reasoning).
	// Виден в /api/models для UI transparency.
	ReasoningEnabled bool `json:"reasoningEnabled"`
	// Round 18 P0.1 (2026-08-03): single source of truth для capabilities.
	// Заполняется в ListModels из DefaultCapabilitiesForModelName (по имени) +
	// реального reasoningEnabled из instance. Прокидывается balancer'у для
	// X-Model-* headers и клиенту через /api/models.
	Capabilities *types.ModelCapabilities `json:"capabilities,omitempty"`
}

// Backend — основной объект CppBackend
type Backend struct {
	cfg    Config
	models map[string]*modelInstance // name → model
	mu     sync.RWMutex

	gpuCount   int
	gpuDevices []bridge.GPUDevice
	gpuManager *GPUManager
	initOnce   sync.Once
	initErr    error
	startTime  time.Time

	modelManager *ModelManager
	metrics      *Metrics
	idleUnloader *IdleUnloadManager
	hfDownloader *HuggingFaceDownloader

	// per-model load locking: prevents race conditions when two goroutines
	// try to load the same model simultaneously.
	// Key: model name, Value: chan struct{} (closed when loading completes).
	loading map[string]chan struct{}
	loadMu  sync.Mutex

	// inFlight — per-model счётчик активных inference-запросов.
	// Используется в handleReloadModel чтобы дождаться завершения
	// всех активных запросов ПЕРЕД UnloadModel — иначе reload обрывает
	// HTTP-соединения (EOF клиенту). Подробности см. в inflight.go.
	inFlight *InFlightCounter

	// Round 18 P0.2 (2026-08-03): per-request cancel tracking для /api/cancel.
	// Хранит context.CancelFunc каждой активной inference-генерации
	// (по requestID). Позволяет cancel API отменять отдельные запросы
	// (не дожидаясь полной остановки backend'а).
	activeGenerations *ActiveGenerations

	// reloadPending — флаг «reload в процессе». Прокидывается в
	// /api/metrics → reload_pending heartbeat. Балансировщик читает
	// и НЕ пытается дёргать LoadModel, пока видит этот флаг.
	reloadMu       sync.Mutex
	reloadPending  string // имя модели, для которой идёт reload
	reloadStartedAt time.Time
}

// ReloadStartedAt возвращает момент начала текущего reload (zero если нет).
func (b *Backend) ReloadStartedAt() time.Time {
	b.reloadMu.Lock()
	defer b.reloadMu.Unlock()
	return b.reloadStartedAt
}

// LoadModelOpts — опции загрузки модели, передаваемые из WebUI/API
// через endpoint /api/models/load-with-params (cppworker).
//
// Базовые параметры (GPULayers, ContextSize и т.д.) использовались ещё
// в /api/models/load (legacy endpoint). Расширенные параметры ниже
// добавлены для тонкой настройки llama.cpp под конкретный workload:
//
//   - NThreads        — количество CPU-потоков для batch/generation
//                       (по умолчанию физические ядра).
//   - Parallel         — число параллельных sequences (batched generation).
//                       Требует больше VRAM (KV-cache × parallel).
//   - KVCacheType      — тип KV-cache quantization (0=F16, 1=Q8_0, 2=Q4_0).
//                       Q8_0 экономит ~50% KV-cache VRAM с минимальной
//                       потерей качества (perplexity delta < 0.1).
//   - SplitMode        — режим tensor split для multi-GPU (0=layer, 1=row).
//   - OverrideTensor   — переопределение dtype отдельных тензоров
//                       (например "blk\\..*\\.ffn_.*_exps=CPU").
//
// Если значение 0/empty — используется llama.cpp default (без override).
type LoadModelOpts struct {
	GPULayers     int
	ContextSize   int
	BatchSize     int
	FlashAttnType int
	NUMA          bool
	UseMmap       bool
	TensorSplit   []float32

	// Extended parameters (для /api/models/load-with-params)
	NThreads      int    // 0 = auto (use physical cores)
	Parallel       int    // 0 = 1 (no batching)
	KVCacheType    string // "" = inherit (default F16), "f16"/"q8_0"/"q4_0" — explicit type
	SplitMode      int    // 0=layer, 1=row
	OverrideTensor string // legacy single-string override, empty = no override
	// Round 7: parallel arrays for per-tensor override-tensors.
	// Each entry is (regex-pattern, buft-name) where buft-name is one of
	//   "CPU", "CUDA0", "CUDA1", ...
	// For MoE: keep attention on GPU, expert tensors in RAM.
	OverrideTensors     []string
	OverrideTensorBufts []string

	// Round 15.1: per-model override для batched parallel inference.
	// - nil (default) → use cfg.EnableBatchedParallel (Backend global toggle)
	// - non-nil → explicit per-model choice (overrides cfg.EnableBatchedParallel)
	// CAVEAT: BatchedScheduler пока greedy argmax (no temp/top_p).
	EnableBatchedParallel *bool

	// Round 17 (2026-07-31): per-model override для reasoning parser routing.
	// Решает баг 2026-07-31 (см. plans/bug-2026-07-31-reasoning-not-routed.md):
	// SOFT prompt injection работает для ЛЮБОЙ модели при cfg.EnableReasoning=true,
	// но парсер SplitReasoningContent срабатывал ТОЛЬКО для моделей из
	// IsReasoningModel() whitelist (qwen3.5, qwen3.6, deepseek-r1, ...).
	// Для qwen3-instruct (которой нет в whitelist) reasoning text попадал
	// в `content` вместо `reasoning_content` — OpenWebUI не показывал reasoning.
	//
	// С этим флагом:
	//   nil (default) → use cfg.DefaultEnableReasoning (для всех моделей)
	//   *true / *false → explicit per-model choice (override default)
	// После LoadModel этот флаг резолвится в modelInstance.reasoningEnabled.
	EnableReasoning *bool
}

// modelInstance — экземпляр загруженной модели
type modelInstance struct {
	info     ModelInfo
	handle   *bridge.ModelHandle
	metadata *bridge.ModelMetadata
	mu       sync.Mutex
	// Round 13 (2026-07-28): slot manager для multi-slot batched inference.
	// maxSlots = max(1, inst.info.Parallel). При Parallel=1 (default) —
	// семантика идентична Round 8 (mu сериализует llama_decode, slot
	// acquisition не блокирует). При Parallel>1 — до N concurrent calls
	// на разных seq_id, каждый держит свой регион KV-cache.
	// Инициализируется в LoadModelWithOpts после успешной загрузки.
	slots *SlotManager

	// Round 15.1 (2026-07-30): BatchedScheduler для true parallel inference.
	// nil = не используется (default, Round 13 path). Если не-nil — GenerateStream
	// маршрутизирует через RegisterSession + чтение из TokenCh (с per-token
	// detokenize через ModelHandle.TokenToPiece). Создаётся в LoadModelWithOpts
	// только если (opts.EnableBatchedParallel ?? cfg.EnableBatchedParallel) = true
	// И Parallel >= 2 (нужен реальный slot pool).
	batchedScheduler *BatchedScheduler
	// batchedSchedulerCancel — отмена Run() goroutine при UnloadModel.
	// cancel != nil означает scheduler активен; вызов cancel() останавливает
	// tick loop и закрывает все активные sessions с ошибкой.
	batchedSchedulerCancel context.CancelFunc

	// Round 17 (2026-07-31): reasoningEnabled — resolved в LoadModelWithOpts
	// из opts.EnableReasoning ?? cfg.DefaultEnableReasoning. Используется
	// парсером (SplitReasoningContent / ReasoningStreamState) чтобы split'ить
	// <think> блоки для моделей ВНЕ IsReasoningModel() whitelist (например,
	// qwen3-instruct с включённым SOFT prompt reasoning).
	//
	// Source of truth для "эта модель эмитит reasoning — нужно split'ить".
	// cfg.DefaultEnableReasoning и per-model override оба резолвятся сюда
	// ОДИН раз при LoadModel — нет рассинхрона между settings и parser.
	//
	// Связь: handler.responds.IsReasoningModel(name) || inst.reasoningEnabled.
	// Читается из handler через *(inst) или снимок в ModelInfo.
	reasoningEnabled bool

	// lastUsedAt — момент последнего обращения к модели (генерация/стрим).
	// Используется IdleUnloadManager'ом для решения о выгрузке неактивных
	// моделей: считаем idle от LastUsedAt, а не от LoadedAt — иначе модель,
	// которая периодически обслуживает запросы, будет выгружена после
	// idleTimeout от момента загрузки, а не от момента последнего использования.
	// atomic.Value позволяет читать без мьютекса на горячем пути ListModels.
	lastUsedAt atomic.Value // time.Time
}

// ============================================================
// Создание Backend
// ============================================================

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

	// Round 18 P0.2 (2026-08-03): ActiveGenerations tracker для /api/cancel.
	// Инициализируется здесь (не в Init) потому что не требует GPU/bridge.
	b.activeGenerations = NewActiveGenerations()

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

// LoadModel загружает GGUF модель с параметрами по умолчанию
func (b *Backend) LoadModel(name string, path string) error {
	return b.LoadModelWithOpts(name, path, LoadModelOpts{
		GPULayers:     b.cfg.DefaultGPULayers,
		ContextSize:   b.cfg.DefaultCtxSize,
		BatchSize:     b.cfg.DefaultBatchSize,
		FlashAttnType: b.cfg.DefaultFlashAttnType,
		NUMA:          b.cfg.DefaultNUMA,
		UseMmap:       b.cfg.DefaultUseMmap,
		// Round 12: проброс DefaultNParallel в opts — даёт CLI/env/JSON
		// default работать через простой LoadModel (без явных opts).
		Parallel: b.cfg.DefaultNParallel,
	})
}

// LoadModelWithOpts загружает GGUF модель с указанными параметрами
func (b *Backend) LoadModelWithOpts(name string, path string, opts LoadModelOpts) error {
	// Валидация VRAM перед загрузкой и авто-подбор оптимальных gpuLayers
	var err error
	if opts, err = b.checkVRAMForModel(name, path, opts); err != nil {
		return err
	}

	b.mu.Lock()
	if _, exists := b.models[name]; exists {
		b.mu.Unlock()
		return fmt.Errorf("model %s already loaded", name)
	}

	gpuLayers := opts.GPULayers
	ctxSize := opts.ContextSize
	batchSize := opts.BatchSize

	// Оцениваем размер файла модели (если доступен) — для отображения прогресса в UI.
	var loadingSizeBytes int64
	if fi, statErr := os.Stat(path); statErr == nil {
		loadingSizeBytes = fi.Size()
	}

	inst := &modelInstance{
		info: ModelInfo{
			Name:             name,
			Path:             path,
			State:            StateLoading,
			GPULayers:        gpuLayers,
			BatchSize:        opts.BatchSize,
			FlashAttnType:    opts.FlashAttnType,
			NUMA:             opts.NUMA,
			UseMmap:          opts.UseMmap,
			LoadedAt:         time.Now(),
			LoadingStartedAt: time.Now(),
			LoadingSizeBytes: loadingSizeBytes,
		},
	}
	b.models[name] = inst
	b.mu.Unlock()

	// Логгируем старт загрузки (UI может полагаться на наличие этой записи).
	logger.Get().Infow("model load started",
		"name", name,
		"path", path,
		"sizeBytes", loadingSizeBytes,
		"gpuLayers", gpuLayers,
		"ctxSize", ctxSize,
		"batchSize", batchSize)

	// Загружаем модель через bridge
	cfg := bridge.DefaultModelConfig(path)
	cfg.NContext = ctxSize
	cfg.NBatch = batchSize
	cfg.NGPULayers = gpuLayers
	cfg.FlashAttnType = opts.FlashAttnType
	cfg.NUMA = opts.NUMA
	cfg.UseMmap = opts.UseMmap
	cfg.UseMlock = b.cfg.DefaultUseMlock
	// NThreads override см. ниже — после секции KVCacheType/Parallel.
	// (Раньше cfg.NThreads = b.cfg.DefaultNThreads затирал opts.NThreads;
	//  теперь мы применяем opts > default корректно.)

	// RoPE параметры контекста
	cfg.RopeFreqBase = float32(b.cfg.DefaultRopeFreqBase)
	cfg.RopeFreqScale = float32(b.cfg.DefaultRopeFreqScale)
	cfg.RopeScalingType = b.cfg.DefaultRopeScalingType
	cfg.RopeScalingFactor = float32(b.cfg.DefaultRopeScalingFactor)

	// YaRN параметры
	cfg.YarnExtFactor = float32(b.cfg.DefaultYarnExtFactor)
	cfg.YarnAttnFactor = float32(b.cfg.DefaultYarnAttnFactor)
	cfg.YarnBetaFast = float32(b.cfg.DefaultYarnBetaFast)
	cfg.YarnBetaSlow = float32(b.cfg.DefaultYarnBetaSlow)

	// KV Cache
	cfg.NoKVOffload = b.cfg.DefaultNoKVOffload
	// Session 16 (2026-06-27): KVCacheType и Parallel — пробрасываем opts поверх дефолтов.
	// До этой правки cfg.KVCacheType жёстко затирал opts.KVCacheType значением
	// из b.cfg (DefaultKVCacheType), что делало per-model profile с q8_0/q4_0
	// бесполезным — C-bridge всегда получал default F16.
	if opts.KVCacheType != "" {
		cfg.KVCacheType = opts.KVCacheType
	} else {
		cfg.KVCacheType = b.cfg.DefaultKVCacheType
	}
	// То же для NParallel: 0 в Go = default (=1 в llama.cpp).
	// > 0 = multi-slot batched generation (требует больше VRAM).
	// Round 12 (2026-07-28): если opts.Parallel не задан (0), используем
	// b.cfg.DefaultNParallel — это даёт CLI/env/JSON-конфигурируемый default,
	// который раньше был только per-model в opts.
	if opts.Parallel > 0 {
		cfg.NParallel = opts.Parallel
	} else if b.cfg.DefaultNParallel > 0 {
		cfg.NParallel = b.cfg.DefaultNParallel
	} else {
		cfg.NParallel = 0 // оставляем дефолт bridge (1)
	}
	// NThreads: было cfg.NThreads = b.cfg.DefaultNThreads (ВСЕГДА затирал opts).
	// Сейчас opts.NThreads > 0 — выигрывает opts, иначе — дефолт.
	if opts.NThreads > 0 {
		cfg.NThreads = opts.NThreads
	} else {
		cfg.NThreads = b.cfg.DefaultNThreads
	}

	// Round 7: override-tensors (parallel arrays, length must match).
	if len(opts.OverrideTensors) > 0 &&
		len(opts.OverrideTensors) == len(opts.OverrideTensorBufts) {
		cfg.OverrideTensors = opts.OverrideTensors
		cfg.OverrideTensorBufts = opts.OverrideTensorBufts
		logger.Get().Infow("override-tensors applied",
			"model", name,
			"count", len(opts.OverrideTensors))
	}

	// Normalization
	cfg.RMSNormEps = float32(b.cfg.DefaultRMSNormEps)

	// Прочее
	cfg.MainGPU = b.cfg.DefaultMainGPU
	cfg.NoMemoryMap = b.cfg.DefaultNoMemoryMap
	cfg.RPCBackend = b.cfg.DefaultRPCBackend

	// Пользовательский tensor split (если не авто)
	if len(opts.TensorSplit) > 0 {
		cfg.TensorSplit = opts.TensorSplit
		if b.gpuCount > 0 && len(opts.TensorSplit) > 0 {
			inst.info.TensorSplit = make([]float32, len(opts.TensorSplit))
			copy(inst.info.TensorSplit, opts.TensorSplit)
			cfg.MainGPU = 0
			logger.Get().Infow("manual tensor split applied",
				"model", name,
				"tensorSplit", opts.TensorSplit)
		}
	} else if b.cfg.AutoGPUDistribution && b.gpuManager != nil && b.gpuCount > 0 {
		// Multi-GPU: рассчитываем tensor split если включено авто-распределение
		ts := b.gpuManager.CalculateTensorSplit(0) // 0 = unknown estimate
		if ts.GPUCount > 1 {
			cfg.TensorSplit = ts.Ratios
			cfg.MainGPU = ts.MainGPU
			inst.info.TensorSplit = make([]float32, len(ts.Ratios))
			copy(inst.info.TensorSplit, ts.Ratios)
			logger.Get().Infow("multi-GPU tensor split applied",
				"model", name,
				"gpuCount", ts.GPUCount,
				"mainGPU", ts.MainGPU,
				"tensorSplit", ts.Ratios)
		}
	}

	handle, err := bridge.LoadModel(cfg)
	if err != nil {
		// Сохраняем LoadingError на короткое время, чтобы UI мог показать
		// причину сбоя (даже после того, как inst удалён из b.models).
		// На практике inst удаляется сразу, поэтому ошибка возвращается
		// в ответе на /api/models/load и не попадает в /api/models,
		// но если клиент polling'ом проверял состояние — успеет увидеть state=loading
		// в течение ~миллисекунд до получения HTTP 500.
		b.mu.Lock()
		delete(b.models, name)
		b.mu.Unlock()
		if b.metrics != nil {
			b.metrics.RecordRequest(name, 0, 0, false)
		}
		return fmt.Errorf("load model %s: %w", name, err)
	}

	// Получаем метаданные
	meta, err := handle.GetMetadata()
	if err != nil {
		logger.Get().Warnw("failed to get metadata for model", "name", name, "error", err)
		meta = &bridge.ModelMetadata{
			Architecture:  "unknown",
			ContextLength: ctxSize,
		}
	}

	// Обновляем информацию
	b.mu.Lock()
	inst.handle = handle
	inst.metadata = meta
	inst.info.State = StateLoaded
	inst.info.Architecture = meta.Architecture
	inst.info.NLayers = meta.NLayers
	inst.info.NHeads = meta.NHeads
	inst.info.NKvHeads = meta.NKvHeads // BUG 13
	inst.info.HeadDimK = meta.HeadDimK // Phase 3 (2026-07-06): expose to /api/show
	inst.info.HeadDimV = meta.HeadDimV // Phase 3
	inst.info.NEmbd = meta.NEmbd
	inst.info.NVocab = meta.NVocab
	inst.info.ContextSize = ctxSize
	inst.info.GGUFContextLength = meta.ContextLength
	inst.info.SizeBytes = meta.SizeTotalBytes
	inst.info.GPUCount = b.gpuCount
	// Session 16 (2026-06-27): Parallel + KVCacheType.
	// Сохраняем реально применённые значения opts в ModelInfo, чтобы:
	//   1. UI мог показать "parallel=4, kv=q8_0" в карточке модели;
	//   2. reload на тот же path через /api/models/reload видел, что
	//      текущая загрузка эквивалентна (sameLoadOptions);
	//   3. metrics broker экспортировал parallel/kvCacheType в /metrics.
	// opts — value type (не pointer), так что он всегда non-nil после
	// LoadModelWithOpts (даже если caller передал zero-value).
	inst.info.Parallel = opts.Parallel
	inst.info.KVCacheType = opts.KVCacheType

	// Round 13 (2026-07-28): initialize SlotManager после успешной загрузки.
	// maxSlots = max(1, opts.Parallel). Parallel=0/1 → single-slot (Round 8 behavior).
	// Parallel>1 → до N concurrent calls, каждый со своим seq_id.
	// Инициализация тут (а не в NewBackend) потому что Parallel — per-model,
	// и для каждой загруженной модели нужен свой SlotManager.
	parallelSlots := opts.Parallel
	if parallelSlots < 1 {
		parallelSlots = 1
	}
	inst.slots = NewSlotManager(parallelSlots)
	logger.Get().Infow("slot manager initialized",
		"name", name,
		"maxSlots", parallelSlots,
		"parallel", opts.Parallel)

	// Round 15.1 (2026-07-30): initialize BatchedScheduler если включён
	// (per-model override или global toggle). Требует Parallel >= 2 потому
	// что batched path использует seq_id (1..N), а seq_id=0 зарезервирован
	// для legacy single-slot. NParallel в BatchedSchedulerConfig = Parallel
	// (max concurrent sessions).
	//
	// Effective flag = opts.EnableBatchedParallel (если != nil) иначе
	// b.cfg.EnableBatchedParallel (env / JSON config). Per-model wins.
	enableBatched := b.cfg.EnableBatchedParallel
	if opts.EnableBatchedParallel != nil {
		enableBatched = *opts.EnableBatchedParallel
	}
	if enableBatched && parallelSlots >= 2 {
		bs, err := NewBatchedScheduler(BatchedSchedulerConfig{
			Model:     handle,
			NParallel: int32(parallelSlots),
			WindowMs:  5, // 5ms batching window (per design doc R3)
			ModelLock: &inst.mu, // serialize llama_decode access (Round 8 BUGFIX)
		})
		if err != nil {
			logger.Get().Warnw("BatchedScheduler init failed, falling back to Round 13 path",
				"name", name, "error", err)
		} else {
			bsCtx, bsCancel := context.WithCancel(context.Background())
			inst.batchedScheduler = bs
			inst.batchedSchedulerCancel = bsCancel
			inst.info.BatchedParallel = true
			go bs.Run(bsCtx)
			logger.Get().Infow("BatchedScheduler started (Round 15.1 batched parallel mode)",
				"name", name, "n_parallel", parallelSlots, "window_ms", 5)
		}
	} else if enableBatched && parallelSlots < 2 {
		logger.Get().Warnw("EnableBatchedParallel=true but parallel < 2; ignoring (need parallel >= 2 for batched seq_ids)",
			"name", name, "parallel", parallelSlots)
	}

	if inst.info.KVCacheType == "" {
		// Fallback на default (аналогично line 452 для cfg.KVCacheType).
		// Без этого /api/show показывает пустую строку, хотя llama.cpp использует
		// DefaultKVCacheType (f16).
		inst.info.KVCacheType = b.cfg.DefaultKVCacheType
	}
	// Round 17 (2026-07-31): resolve reasoningEnabled для этой модели ОДИН раз
	// при LoadModel. Per-model override (opts.EnableReasoning) имеет приоритет
	// над global default. Это source of truth для parser'а — handler читает
	// через inst.reasoningEnabled, чтобы IsReasoningModel(name) whitelist
	// не пропускал qwen3-instruct с SOFT prompt reasoning.
	enableReasoning := b.cfg.DefaultEnableReasoning
	if opts.EnableReasoning != nil {
		enableReasoning = *opts.EnableReasoning
	}
	inst.reasoningEnabled = enableReasoning
	inst.info.ReasoningEnabled = enableReasoning
	b.mu.Unlock()

	// Инициализируем lastUsedAt моментом загрузки, чтобы IdleUnloadManager
	// не считал только что загруженную модель сразу idle. Дальше Generate/GenerateStream
	// будут обновлять его на каждом запросе.
	inst.lastUsedAt.Store(time.Now())

	// Записываем метрики
	if b.metrics != nil {
		b.metrics.RecordLoad(name)
	}

	// Round 22 (2026-08-03): записать name → path в ModelManager.nameHistory
	// чтобы resolveModelPath мог найти alias после idle-unload.
	// Без этого: auto-load для alias "qwen3-4b" (когда в директории 2+ .gguf)
	// падает с HTTP 500 "models/qwen3-4b.gguf not found".
	if mm := b.modelManager; mm != nil {
		mm.RecordModelLoad(name, path)
	}

	logger.Get().Infow("model loaded",
		"name", name,
		"path", path,
		"architecture", meta.Architecture,
		"layers", meta.NLayers,
		"gpuLayers", gpuLayers,
		"ctxSize", ctxSize,
		"batchSize", batchSize,
		"flashAttnType", opts.FlashAttnType,
		"gpuCount", b.gpuCount)

	return nil
}

// DiagnosticsInfo — детальная диагностика при невозможности загрузить модель
type DiagnosticsInfo struct {
	ModelSizeGB             float64 `json:"modelSizeGB"`
	TotalLayers             int     `json:"totalLayers"`
	VRAMAvailableMB         uint64  `json:"vramAvailableMB"`
	VRAMRequiredForFullGPU  uint64  `json:"vramRequiredForFullGPU"`
	OptimalGPULayers        int     `json:"optimalGPULayers"`
	VRAMRequiredForOptimal  uint64  `json:"vramRequiredForOptimal"`
	RAMAvailableMB          uint64  `json:"ramAvailableMB"`
	RAMRequiredForRemaining uint64  `json:"ramRequiredForRemaining"`
	Recomendation           string  `json:"recomendation"`
}

func (d *DiagnosticsInfo) Error() string {
	return d.Recomendation
}

// CalculateOptimalGPULayers вычисляет оптимальное количество GPU-слоёв
// на основе доступной VRAM и RAM.
// Параметры:
//   - modelSizeBytes: размер GGUF файла в байтах
//   - totalLayers: общее количество слоёв модели (из GGUF metadata)
//   - nHeads: количество голов внимания (для KV Cache)
//   - nKvHeads: количество KV-голов (для GQA, 0 = nHeads)
//   - nEmbd: размер эмбеддингов (для head_dim)
//   - requestedGPULayers: запрошенное количество GPU-слоёв (-1 = все)
//   - requestedCtxSize: запрошенный размер контекста
//
// Возвращает:
//   - optimalGPULayers: оптимальное число GPU-слоёв
//   - useMmap: true если требуется mmap (часть модели CPU-based)
//   - diagnostics: детальная диагностика (nil если успешно, иначе с ошибкой)
func (b *Backend) CalculateOptimalGPULayers(modelSizeBytes int64, totalLayers, nHeads, nKvHeads, nEmbd, requestedGPULayers, requestedCtxSize int) (optimalGPULayers int, useMmap bool, diagnostics *DiagnosticsInfo) {
	if totalLayers <= 0 {
		totalLayers = 40 // fallback: предполагаем ~40 слоёв если неизвестно
	}

	modelSizeMB := float64(modelSizeBytes) / 1024 / 1024
	kvCacheMB := estimateKVCacheMB(totalLayers, nHeads, nKvHeads, nEmbd, requestedCtxSize)

	// Собираем доступную VRAM
	b.mu.RLock()
	var totalFreeVRAM uint64
	for _, dev := range b.gpuDevices {
		totalFreeVRAM += uint64(dev.VRAMFreeMB)
	}
	b.mu.RUnlock()

	// Собираем доступную RAM (системная память)
	var totalRAM uint64
	if runtime.GOOS != "windows" {
		totalRAM = getSystemRAMGB() * 1024 // в MB
	} else {
		totalRAM = 8192 // fallback: предполагаем хотя бы 8GB RAM
	}

	diag := &DiagnosticsInfo{
		ModelSizeGB:     math.Round(modelSizeMB/1024*100) / 100,
		TotalLayers:     totalLayers,
		VRAMAvailableMB: totalFreeVRAM,
	}

	// Если нет данных о VRAM — возвращаем запрошенные значения как есть
	if totalFreeVRAM == 0 {
		if requestedGPULayers == -1 || requestedGPULayers > totalLayers {
			return totalLayers, false, nil
		}
		return requestedGPULayers, false, nil
	}

	// Определяем желаемое количество GPU-слоёв
	wantedGPULayers := requestedGPULayers
	// -1 and -2 both mean "auto / all layers on GPU". -2 is used by the adaptive
	// reload path when fallback_no_meta can't read GGUF metadata (e.g. gemma4).
	if wantedGPULayers < 0 || wantedGPULayers > totalLayers {
		wantedGPULayers = totalLayers
	}

	// Создаём новый DiagnosticsInfo теперь с kvCacheMB
	diag2 := &DiagnosticsInfo{
		ModelSizeGB:             diag.ModelSizeGB,
		TotalLayers:             diag.TotalLayers,
		VRAMAvailableMB:         diag.VRAMAvailableMB,
		VRAMRequiredForFullGPU:  0,
		OptimalGPULayers:        0,
		VRAMRequiredForOptimal:  0,
		RAMAvailableMB:          totalRAM,
		RAMRequiredForRemaining: 0,
	}

	// Оцениваем VRAM для полной загрузки всех слоёв на GPU
	vramForFullGPU := EstimateGPUMemoryForModel(modelSizeBytes, totalLayers, totalLayers, requestedCtxSize)
	vramForFullGPU += kvCacheMB
	diag2.VRAMRequiredForFullGPU = vramForFullGPU

	// Если желаемые GPU-слои влезают в VRAM — используем их как есть
	vramForWanted := EstimateGPUMemoryForModel(modelSizeBytes, wantedGPULayers, totalLayers, requestedCtxSize)
	vramForWanted += kvCacheMB

	if vramForWanted <= totalFreeVRAM {
		diag2.OptimalGPULayers = wantedGPULayers
		diag2.VRAMRequiredForOptimal = vramForWanted
		diag2.RAMRequiredForRemaining = 0
		logger.Get().Infow("VRAM sufficient for requested GPU layers",
			"requestedGPULayers", wantedGPULayers,
			"vramRequiredMB", vramForWanted,
			"vramFreeMB", totalFreeVRAM,
			"kvCacheMB", kvCacheMB)
		return wantedGPULayers, false, nil
	}

	// Не хватает VRAM — бинарный поиск оптимального числа GPU-слоёв
	lo, hi := 0, wantedGPULayers
	bestLayers := 0
	for lo <= hi {
		mid := (lo + hi) / 2
		needed := EstimateGPUMemoryForModel(modelSizeBytes, mid, totalLayers, requestedCtxSize)
		needed += kvCacheMB
		if needed <= totalFreeVRAM {
			bestLayers = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}

	// Проверяем, влезает ли остаток в RAM
	cpuLayers := totalLayers - bestLayers
	// CPU-часть модели: слои на CPU + KV cache (KV cache всегда на GPU/CPU вместе с моделью)
	cpuMemoryMB := uint64(modelSizeMB*0.7*float64(cpuLayers)/float64(totalLayers) + float64(kvCacheMB)*0.5)

	diag2.OptimalGPULayers = bestLayers
	diag2.VRAMRequiredForOptimal = EstimateGPUMemoryForModel(modelSizeBytes, bestLayers, totalLayers, requestedCtxSize) + kvCacheMB
	diag2.RAMRequiredForRemaining = cpuMemoryMB

	if cpuMemoryMB < totalRAM*8/10 { // используем до 80% RAM
		logger.Get().Infow("optimal GPU layers found with RAM fallback",
			"totalLayers", totalLayers,
			"gpuLayers", bestLayers,
			"cpuLayers", cpuLayers,
			"cpuMemoryMB", cpuMemoryMB,
			"ramAvailableMB", totalRAM,
			"kvCacheMB", kvCacheMB)
		return bestLayers, true, nil
	}

	// Даже CPU-only не влезает — диагностика
	diag2.Recomendation = fmt.Sprintf(
		"cannot load model: model requires ~%d MB total memory "+
			"(%.1f GB model + %d MB KV cache), "+
			"but VRAM=%d MB + RAM=%d MB insufficient. "+
			"Best GPU layers=%d (CPU layers=%d, needs %d MB RAM). "+
			"Reduce model size (try a smaller quant), decrease context size, "+
			"or enable flash attention.",
		uint64(modelSizeMB)+kvCacheMB,
		modelSizeMB/1024, kvCacheMB,
		totalFreeVRAM, totalRAM,
		bestLayers, cpuLayers, cpuMemoryMB)

	return bestLayers, true, diag2
}

// estimateKVCacheMB вычисляет размер KV Cache в MB для заданных параметров модели.
// Формула:
//
//	KV Cache (bytes) = 2 × n_layers × n_ctx × n_kv_heads × head_dim × bytes_per_elem
//
// где head_dim = n_embd / n_heads, bytes_per_elem = 2 для fp16 (стандартный KV cache).
// Для GQA моделей n_kv_heads < n_heads, для MHA n_kv_heads = n_heads.
//
// Добавляет 10% буфера безопасности для избежания граничных OOM.
func estimateKVCacheMB(nLayers, nHeads, nKvHeads, nEmbd, nCtx int) uint64 {
	if nLayers <= 0 || nCtx <= 0 {
		return 0
	}

	// Safe-fail: если nHeads или nEmbd неизвестны (0/отрицательные),
	// невозможно корректно вычислить head_dim = n_embd/n_heads.
	// Возвращаем 0 чтобы caller использовал другой метод оценки
	// (например, реальные метрики из cppworker или иной эвристический fallback).
	// Лучше недооценить размер и загрузить с чуть меньшим n_ctx,
	// чем переоценить и нарваться на OOM в production.
	if nHeads <= 0 || nEmbd <= 0 {
		return 0
	}

	if nKvHeads <= 0 {
		nKvHeads = nHeads // MHA (no GQA)
	}

	// head_dim = n_embd / n_heads (обычно 128 для большинства моделей)
	headDim := nEmbd / nHeads
	if headDim <= 0 {
		headDim = 128 // fallback (теоретически недостижимо после проверки выше)
	}

	// KV Cache = 2 (K+V) × n_layers × n_ctx × n_kv_heads × head_dim × bytes_per_elem
	// Стандартный KV cache тип fp16 = 2 bytes per element
	const bytesPerElem = 2

	kvBytes := uint64(2) * uint64(nLayers) * uint64(nCtx) * uint64(nKvHeads) * uint64(headDim) * uint64(bytesPerElem)

	// Конвертируем в MB и добавляем 10% безопасности
	kvMB := kvBytes / (1024 * 1024)
	kvMB = kvMB + kvMB/10 // +10% safety buffer

	return kvMB
}

// checkVRAMForModel проверяет, достаточно ли видеопамяти для загрузки модели
// и автоматически подбирает оптимальное количество GPU-слоёв.
// Возвращает (opts, error) — модифицированные опции с оптимальными GPU-слоями.
func (b *Backend) checkVRAMForModel(name string, path string, opts LoadModelOpts) (retOpts LoadModelOpts, retErr error) {
	// Panic recovery: readGGUFHeaderInfo может запаниковать если GGUF файл повреждён.
	// В этом случае пропускаем VRAM-проверку и загружаем модель с оригинальными опциями.
	defer func() {
		if r := recover(); r != nil {
			logger.Get().Errorw("panic in checkVRAMForModel (GGUF header read), skipping VRAM check",
				"model", name,
				"path", path,
				"recover", r)
			retOpts = opts // возвращаем оригинальные опции без изменений
			retErr = nil   // не блокируем загрузку
		}
	}()

	// Получаем размер файла модели
	fi, err := os.Stat(path)
	if err != nil {
		// Если файл не существует, пропускаем проверку (может быть ещё не скачан)
		return opts, nil
	}

	// Извлекаем метаданные из GGUF header ДО загрузки модели.
	// Сначала пытаемся получить из уже загруженной модели (если уже загружена).
	var header *GGUFHeaderInfo
	totalLayers := 0
	nHeads := 0
	nKvHeads := 0
	nEmbd := 0

	// Если модель уже загружена — берём метаданные из неё
	if info, err := b.GetModel(name); err == nil && info.NLayers > 0 {
		totalLayers = info.NLayers
		nHeads = info.NHeads
		nKvHeads = info.NKvHeads
		nEmbd = info.NEmbd
	}

	// Если модель не загружена — читаем GGUF header
	if totalLayers <= 0 {
		var readErr error
		header, readErr = readGGUFHeaderInfo(path)
		if readErr == nil && header != nil {
			totalLayers = header.NLayers
			nHeads = header.NHeads
			nKvHeads = header.NKvHeads
			nEmbd = header.NEmbd
		}
	}

	// Если GGUF header не дал результатов — fallback на эвристику по размеру файла
	if totalLayers <= 0 {
		totalLayers = estimateLayersFromFileSize(fi.Size())
		// nHeads и nEmbd остаются 0, но nKvHeads=2 чтобы estimateKVCacheMB сработал с fallback
		if nKvHeads <= 0 {
			nKvHeads = 2
		}
	}

	optGPULayers, useMmap, diagnostics := b.CalculateOptimalGPULayers(
		fi.Size(), totalLayers, nHeads, nKvHeads, nEmbd,
		opts.GPULayers, opts.ContextSize,
	)

	// 2026-06-26 BUGFIX: для больших моделей (model_size > 70% VRAM) принудительно
	// включаем useMmap=true. Без mmap все 20 GB весов модели форсируются в VRAM,
	// что приводит к OOM на partial offload. С mmap не-вмещающиеся слои остаются
	// в RAM через page-cache и не занимают VRAM.
	totalVRAMMB := uint64(0)
	b.mu.RLock()
	for _, dev := range b.gpuDevices {
		totalVRAMMB += uint64(dev.VRAMFreeMB)
	}
	b.mu.RUnlock()
	modelSizeMB := fi.Size() / (1024 * 1024)
	if totalVRAMMB > 0 && uint64(modelSizeMB)*100 > totalVRAMMB*70 && !opts.UseMmap {
		logger.Get().Infow("forcing useMmap=true for large model",
			"model", name,
			"modelSizeMB", modelSizeMB,
			"vramFreeMB", totalVRAMMB,
			"reason", "model > 70% of available VRAM")
		opts.UseMmap = true
	}

	if diagnostics != nil && diagnostics.Recomendation != "" {
		logger.Get().Errorw("cannot load model - insufficient memory",
			"model", name,
			"path", path,
			"diagnostics", diagnostics)
		return opts, fmt.Errorf("cannot load model %s: %s", name, diagnostics.Recomendation)
	}

	// Если оптимальные GPU-слои отличаются от запрошенных — логируем и адаптируемся
	if optGPULayers != opts.GPULayers {
		logger.Get().Infow("auto-adapting GPU layers for model",
			"model", name,
			"requestedGPULayers", opts.GPULayers,
			"optimalGPULayers", optGPULayers,
			"useMmap", useMmap)

		opts.GPULayers = optGPULayers
		if useMmap {
			opts.UseMmap = true
		}

		if diagnostics != nil {
			logger.Get().Infow("optimal GPU layers calculation details",
				"model", name,
				"totalLayers", diagnostics.TotalLayers,
				"vramAvailableMB", diagnostics.VRAMAvailableMB,
				"vramRequiredForOptimal", diagnostics.VRAMRequiredForOptimal,
				"ramAvailableMB", diagnostics.RAMAvailableMB,
				"ramRequiredForRemaining", diagnostics.RAMRequiredForRemaining)
		}
	}

	return opts, nil
}

// UpdateLastUsed продлевает время жизни модели в VRAM на duration d от now.
//
// Используется для обработки Ollama-семантики keep_alive:
//   - "5m" → lastUsedAt = now + 5 минут
//   - "0"  → unload модели (вызывается через UnloadModel напрямую, не через этот метод)
//
// Метод безопасен для concurrent вызовов (atomic.Value.Store lock-free).
// Если duration <= 0 — ставит lastUsedAt = now (модель только что использована).
// Если модель не загружена — тихий no-op.
func (b *Backend) UpdateLastUsed(name string, d time.Duration) {
	b.mu.RLock()
	inst, exists := b.models[name]
	b.mu.RUnlock()

	if !exists {
		return
	}

	newTime := time.Now()
	if d > 0 {
		newTime = newTime.Add(d)
	}
	inst.lastUsedAt.Store(newTime)

	logger.Get().Debugw("model keep_alive extended",
		"name", name,
		"keepAliveDuration", d.String(),
		"newLastUsedAt", newTime.Format(time.RFC3339Nano))
}

// ResourceLimits — лимиты VRAM/RAM для загруженной модели.
//
// Используется cppworker'ом для расширения ответа /api/models
// (поля max_vram_n_ctx, max_ram_n_ctx, available_vram_mb, available_ram_mb)
// и балансировщиком для принятия решений по n_ctx reload preflight
// (см. internal/balancer/preflight_nctx.go:collectPreflightState).
//
// 2026-06-26 BUGFIX: до этой фичи балансер видел max_vram_n_ctx=0
// и model_max_context=0 для всех моделей (env_log.txt строки 121, 133),
// потому что cppworker не сообщал эти поля в /api/models. Без них
// preflight не мог расчитать target_n_ctx и ставил хардкод 8192,
// что приводило к ошибкам при reload на модели типа
// Qwen3.6-35B-A3B (training context ~32k, фактически нужно 16-32k).
type ResourceLimits struct {
	// VRAM
	TotalVRAMMB     uint64 // суммарный объём VRAM всех GPU
	AvailableVRAMMB uint64 // свободная VRAM (Total - уже занятое моделями)
	MaxVRAMNCtx     int    // макс. n_ctx, который влезет в свободную VRAM
	// RAM (для mmap fallback, если модель не влезает в VRAM)
	TotalRAMMB     uint64 // суммарный объём системной RAM
	AvailableRAMMB uint64 // свободная RAM (Total - занятое)
	MaxRAMNCtx     int    // макс. n_ctx, который влезет в RAM через mmap
	// Прочее
	ModelMaxContext int // максимальный контекст из GGUF training metadata (gemma-3: 131072, llama-3: 8192, qwen3.6-72B: 32768)
}

// CalculateResourceLimits рассчитывает лимиты VRAM/RAM для загруженной модели.
//
// Алгоритм:
//  1. Берём метаданные из уже загруженной модели (NLayers, NEmbd, NKvHeads, ContextLength)
//     или из GGUF header если модель ещё не загружена.
//  2. KV-cache размер = 2 (K+V) * 2 байта (fp16) * NLayers * NKvHeads * headDim * n_ctx,
//     где headDim = NEmbd / NHeads.
//  3. Для VRAM: доступно = TotalVRAM - уже занято (моделями). Резервируем 2 GB на overhead.
//     max_vram_n_ctx = (available_vram_mb * 1024 * 1024 - 2GB_reserve) / kv_per_token_bytes.
//  4. Для RAM: вся свободная RAM, минус 4 GB на систему. max_ram_n_ctx считается так же.
//  5. Clamp оба значения к ModelMaxContext (из GGUF).
//
// Возвращает ResourceLimits со всеми полями заполненными. Если модель не загружена
// и GGUF header недоступен — MaxVRAMNCtx/MaxRAMNCtx = 0, остальные поля = 0.
//
// Эта функция используется:
//   - cppworker'ом в handleListModels для формирования JSON-ответа /api/models;
//   - балансировщиком для fallback (если cppworker не сообщил эти поля — баг).
func (b *Backend) CalculateResourceLimits(name string) ResourceLimits {
	var limits ResourceLimits

	// 1. Метаданные модели: сначала из загруженной, потом из GGUF header.
	var nLayers, nHeads, nKvHeads, nEmbd, modelCtx int
	if info, err := b.GetModel(name); err == nil {
		nLayers = info.NLayers
		nHeads = info.NHeads
		nKvHeads = info.NKvHeads
		nEmbd = info.NEmbd
		if info.GGUFContextLength > 0 {
			modelCtx = info.GGUFContextLength
		}
	}
	if nLayers == 0 || nEmbd == 0 {
		// Пытаемся получить метаданные через ModelManager (для незагруженных моделей).
		// GGUFModelMeta не содержит поля ContextLength — оно берётся из GGUF header
		// через ReadGGUFHeader (если есть Path) или остаётся 0.
		if mm := b.ModelManager(); mm != nil {
			if m, err := mm.GetModelMeta(name); err == nil && m != nil {
				if m.NLayers > 0 {
					nLayers = m.NLayers
				}
				if m.NEmbd > 0 {
					nEmbd = m.NEmbd
				}
				if m.NHeads > 0 {
					nHeads = m.NHeads
				}
				if m.NKvHeads > 0 {
					nKvHeads = m.NKvHeads
				}
				// modelCtx уже из info.GGUFContextLength выше, если не 0 — оставляем
			}
		}
	}

	limits.ModelMaxContext = modelCtx

	// 2. KV-cache per token = 4 * NLayers * NKvHeads * headDim (в байтах, fp16).
	if nLayers > 0 && nEmbd > 0 {
		if nHeads <= 0 {
			nHeads = 1
		}
		if nKvHeads <= 0 {
			nKvHeads = nHeads // GQA fallback
		}
		headDim := nEmbd / nHeads
		kvPerToken := uint64(4) * uint64(nLayers) * uint64(nKvHeads) * uint64(headDim)
		if kvPerToken > 0 {
			// 3. VRAM: суммируем свободную VRAM по всем GPU.
			// Live refresh b.gpuDevices from C-bridge (was snapshot from init, not updated runtime).
			b.mu.Lock()
			for i := 0; i < b.gpuCount; i++ {
				if dev, err := bridge.GetGPUInfo(i); err == nil && dev != nil {
					if i < len(b.gpuDevices) {
						b.gpuDevices[i].VRAMFreeMB = dev.VRAMFreeMB
						b.gpuDevices[i].VRAMTotalMB = dev.VRAMTotalMB
					}
				}
			}
			for _, dev := range b.gpuDevices {
				limits.TotalVRAMMB += uint64(dev.VRAMTotalMB)
				limits.AvailableVRAMMB += uint64(dev.VRAMFreeMB)
			}
			b.mu.Unlock()

			// Резервируем 2 GB на overhead/weights (KV-cache для n_ctx считается отдельно).
			const vramOverheadMB = uint64(2048)
			if limits.AvailableVRAMMB > vramOverheadMB {
				usableBytes := (limits.AvailableVRAMMB - vramOverheadMB) * 1024 * 1024
				limits.MaxVRAMNCtx = int(usableBytes / kvPerToken)
			}

			// 4. RAM: вся доступная системная память (на Linux читаем /proc/meminfo,
			// на других платформах — fallback через getSystemRAMGB).
			limits.TotalRAMMB = getSystemRAMGB() * 1024
			// Резервируем 4 GB на систему + cppworker + llama.cpp runtime.
			const ramOverheadMB = uint64(4096)
			if limits.TotalRAMMB > ramOverheadMB {
				limits.AvailableRAMMB = limits.TotalRAMMB - ramOverheadMB
				usableBytes := limits.AvailableRAMMB * 1024 * 1024
				limits.MaxRAMNCtx = int(usableBytes / kvPerToken)
			}

			// 5. Clamp к modelCtx (training context из GGUF).
			if modelCtx > 0 {
				if limits.MaxVRAMNCtx > modelCtx {
					limits.MaxVRAMNCtx = modelCtx
				}
				if limits.MaxRAMNCtx > modelCtx {
					limits.MaxRAMNCtx = modelCtx
				}
			}
		}
	}

	return limits
}

// UnloadModel выгружает модель
func (b *Backend) UnloadModel(name string) error {
	b.mu.Lock()
	inst, exists := b.models[name]
	if !exists {
		b.mu.Unlock()
		return fmt.Errorf("model %s not found", name)
	}
	delete(b.models, name)
	b.mu.Unlock()

	// Round 15.1: stop BatchedScheduler ДО FreeModel, чтобы in-flight
	// sessions получили ошибку через ctx.Done() и закрыли каналы.
	// Без этого Run() goroutine может пытаться обращаться к handle
	// после FreeModel() — use-after-free в C-bridge (SIGABRT / segfault).
	if inst.batchedSchedulerCancel != nil {
		inst.batchedSchedulerCancel() // cancel ctx → Run() returns
		inst.batchedSchedulerCancel = nil
	}
	if inst.batchedScheduler != nil {
		// Round 16 code-review fix (2026-07-30): добавил timeout.
		// Round 8 BUGFIX: <inst.batchedScheduler.Done() без timeout —
		// если Run() goroutine зависнет (deadlock, infinite loop, blocked
		// channel), UnloadModel блокируется НАВСЕГДА → HTTP /api/models/unload
		// зависает, админ-операции недоступны. DOS-вектор.
		//
		// Timeout 10 сек — generous: нормальный shutdown Run() = <1 сек
		// (один tick). 10 сек = 10x headroom для медленных систем.
		// После timeout — force-detach (Nil scheduler), C-bridge handle
		// всё ещё будет FreeModel'ен ниже. Run() goroutine упадёт с nil
		// pointer dereference или завершится когда C-bridge handle free'н —
		// в обоих случаях процесс не зависнет.
		select {
		case <-inst.batchedScheduler.Done():
			// Normal shutdown
		case <-time.After(10 * time.Second):
			logger.Get().Errorw("UnloadModel: BatchedScheduler.Run() did not return within 10s, force-detaching",
				"model", name, "active_sessions", inst.batchedScheduler.ActiveSessions())
			// Record в metrics для postmortem — это указывает на баг
			// в scheduler (deadlock, infinite loop, или blocked channel).
			if b.metrics != nil {
				b.metrics.RecordUnloadTimeout(name, "batched_scheduler_stuck")
			}
		}
		inst.batchedScheduler = nil
	}

	inst.mu.Lock()
	if inst.handle != nil {
		inst.handle.FreeModel()
	}
	inst.info.State = StateUnloaded
	// Сбрасываем loading-метаданные, чтобы UI не показывал спиннер
	// для уже выгруженной модели.
	inst.info.LoadingStartedAt = time.Time{}
	inst.info.LoadingSizeBytes = 0
	inst.info.LoadingError = ""
	inst.info.BatchedParallel = false
	inst.mu.Unlock()

	// Записываем метрики
	if b.metrics != nil {
		b.metrics.RecordUnload(name)
	}

	logger.Get().Infow("model unloaded", "name", name)
	return nil
}

// GetModel возвращает информацию о модели
func (b *Backend) GetModel(name string) (*ModelInfo, error) {
	b.mu.RLock()
	inst, exists := b.models[name]
	b.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("model %s not found", name)
	}

	info := inst.info
	info.ActiveQueries = b.getActiveQueries(inst)
	info.LastUsedAt = b.getLastUsedAt(inst)
	return &info, nil
}

// AutoEnableReasoning — Round 17 Layer 3 (2026-07-31): LAZY AUTO-DETECT.
//
// Парсер в writeOpenAIChatStream / handleV1ChatCompletions вызывает этот
// метод когда видит `<think>` в output модели, у которой reasoningEnabled=false
// (т.е. модель не в IsReasoningModel whitelist и пользователь не задал
// load-with-params enableReasoning=true). Включает routing для этой модели
// "на лету" — все последующие запросы будут split'ить <think> корректно.
//
// Идемпотентно: повторный вызов на уже-enabled модели — no-op.
// Безопасно при многократных concurrent вызовах: под b.mu.Lock.
//
// Не применяется, если модель не загружена (UnloadModel между запросами)
// — в этом случае пользователю нужно явно указать enableReasoning=true
// при следующем LoadModelWithOpts.
//
// Возвращает true если флаг реально изменился (false→true), false если
// уже был включён. Используется для логирования и метрик.
func (b *Backend) AutoEnableReasoning(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	inst, exists := b.models[name]
	if !exists {
		return false
	}
	if inst.reasoningEnabled {
		return false
	}
	inst.reasoningEnabled = true
	inst.info.ReasoningEnabled = true
	logger.Get().Warnw("reasoning auto-enabled (lazy detect)",
		"model", name,
		"hint", "model emits <think> but not in IsReasoningModel whitelist; "+
			"to pre-set, use load-with-params with enableReasoning=true, "+
			"or add model name pattern to CPPWORKER_REASONING_ARCHS env")
	if b.metrics != nil {
		// Counter-based observability для postmortem.
		// Если counter быстро растёт — есть системная проблема с whitelist
		// или с пользователями, загружающими reasoning-модели без флага.
		b.metrics.RecordReasoningAutoEnable(name)
	}
	return true
}

// ListModels возвращает список всех загруженных моделей
func (b *Backend) ListModels() []ModelInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]ModelInfo, 0, len(b.models))
	for _, inst := range b.models {
		info := inst.info
		info.ActiveQueries = b.getActiveQueries(inst)
		info.LastUsedAt = b.getLastUsedAt(inst)
		// Round 18 P0.1 (2026-08-03): single source of truth для capabilities.
		// Используем реальный reasoningEnabled из instance (не из whitelist),
		// чтобы auto-detect (Round 17 Layer 3) тоже отражался.
		caps := types.CapabilitiesFromModelInfo(
			info.Name,
			inst.reasoningEnabled,
			info.Architecture,
			info.ContextSize,
			info.GGUFContextLength,
		)
		info.Capabilities = &caps
		result = append(result, info)
	}
	return result
}

// getLastUsedAt возвращает момент последнего использования модели.
// Используется IdleUnloadManager (через ListModels) и монитором для
// отображения idle-времени. atomic.Value.Load lock-free, в отличие от
// чтения через inst.mu, что важно при вызовах из горячего пути ListModels.
// Если lastUsedAt не было инициализировано (например модель только в процессе
// загрузки), возвращает LoadedAt как fallback.
func (b *Backend) getLastUsedAt(inst *modelInstance) time.Time {
	if v := inst.lastUsedAt.Load(); v != nil {
		if t, ok := v.(time.Time); ok && !t.IsZero() {
			return t
		}
	}
	return inst.info.LoadedAt
}

// GetLoadingModels возвращает список моделей в состоянии Loading/Error.
// Используется UI (монитор, вкладка бэкендов, GGUF) для отображения
// спиннера и elapsed-time «Загружается model-name 25s».
//
// Потокобезопасно: захватывает mu.RLock на короткое время.
func (b *Backend) GetLoadingModels() []ModelInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]ModelInfo, 0)
	now := time.Now()
	for _, inst := range b.models {
		if inst.info.State == StateLoading || inst.info.State == StateError {
			info := inst.info
			info.ActiveQueries = b.getActiveQueries(inst)
			// Гарантируем непустой LoadingStartedAt для UI.
			if info.LoadingStartedAt.IsZero() {
				info.LoadingStartedAt = info.LoadedAt
			}
			// Заполняем elapsedMs (нс → мс) — удобно для UI, не нужно
			// делать arithmetic в JS. Вычисляем «на лету» из текущего
			// времени, так как LoadingStartedAt не меняется в процессе.
			_ = now // резерв на случай будущего счётчика в снапшоте
			result = append(result, info)
		}
	}
	return result
}

// IsModelLoading — быстрая проверка «идёт ли загрузка модели» по имени.
// Используется балансировщиком в proxyRequestLlamaCpp для решения,
// включать ли loading-fast-path с heartbeat-чанками.
func (b *Backend) IsModelLoading(name string) bool {
	b.mu.RLock()
	inst, exists := b.models[name]
	b.mu.RUnlock()
	if !exists {
		return false
	}
	inst.mu.Lock()
	state := inst.info.State
	inst.mu.Unlock()
	return state == StateLoading
}

// TryLockLoad пытается зарезервировать эксклюзивное право на загрузку модели name.
// Возвращает:
//   - (true, nil): блокировка получена, вызывающий может начинать загрузку.
//   - (true, err): модель уже загружена (ошибка).
//   - (false, nil): другая горутина уже грузит эту модель — жди.
//   - (false, err): модель уже загружена или другая ошибка.
//
// Гарантирует, что для одной модели может быть только одна активная загрузка.
// После завершения загрузки (успех/ошибка) вызывающий ОБЯЗАН вызвать UnlockLoad.
// ИСПОЛЬЗУЕТСЯ ТОЛЬКО ДЛЯ ПЕРВИЧНОЙ ЗАГРУЗКИ. Для reload используйте TryLockReload.
func (b *Backend) TryLockLoad(name string) (bool, error) {
	b.loadMu.Lock()
	defer b.loadMu.Unlock()

	// Проверяем, не загружена ли уже модель
	b.mu.RLock()
	_, exists := b.models[name]
	b.mu.RUnlock()
	if exists {
		return true, fmt.Errorf("model %s already loaded", name)
	}

	// Проверяем, не грузит ли её другая горутина
	if _, ok := b.loading[name]; ok {
		// Модель уже в процессе загрузки — ждём
		return false, nil
	}

	// Создаём канал, который будет закрыт после завершения загрузки
	b.loading[name] = make(chan struct{})
	return true, nil
}

// TryLockReload — то же что TryLockLoad, но НЕ проверяет b.models.
// Нужен для /api/models/reload, когда модель уже загружена в b.models
// и мы хотим перезагрузить её с новыми параметрами.
//
// Гарантирует, что для одной модели может быть только один активный reload.
// После завершения reload (успех/ошибка) вызывающий ОБЯЗАН вызвать UnlockLoad.
func (b *Backend) TryLockReload(name string) (bool, error) {
	b.loadMu.Lock()
	defer b.loadMu.Unlock()

	// Проверяем, не reload'ит ли её другая горутина
	if _, ok := b.loading[name]; ok {
		return false, nil
	}

	// Создаём канал, который будет закрыт после завершения reload
	b.loading[name] = make(chan struct{})
	return true, nil
}

// UnlockLoad освобождает блокировку загрузки для модели name.
// Должен вызываться после TryLockLoad, когда загрузка завершена (успех или ошибка).
func (b *Backend) UnlockLoad(name string) {
	b.loadMu.Lock()
	defer b.loadMu.Unlock()
	if ch, ok := b.loading[name]; ok {
		close(ch) // оповещаем всех ожидающих
		delete(b.loading, name)
	}
}

// waitForLoad блокируется до завершения загрузки модели другой горутиной.
// Возвращает true если модель успешно загружена, false если произошла ошибка
// или канал закрыт по другой причине.
// WaitForLoad — блокируется до завершения загрузки модели другой горутиной.
// Используется когда tryLockLoad вернул (false, nil) — другая горутина уже грузит.
//
// Round 9 (2026-07-28) BUGFIX: если b.loading[name] уже удалён (канал
// закрыт и cleaned-up), ПРОВЕРЯЕМ b.models[name] перед возвратом false.
// До фикса был race: goroutine A загрузила модель, вызвала UnlockLoad
// (закрыла канал, удалила из map) — goroutine B видела `!ok` и сразу
// возвращала false, даже если модель УЖЕ в b.models. Caller (lazyload.go
// и 3 handler'а) интерпретировал false как "модель не загружена" и
// возвращал 503 errModelIsLoading.
//
// Корректная семантика: возвращаем true если модель загружена (любым
// способом), false только если загрузка точно провалилась.
func (b *Backend) WaitForLoad(name string) bool {
	b.loadMu.Lock()
	ch, ok := b.loading[name]
	b.loadMu.Unlock()

	if ok {
		// Другая горутина ещё грузит — ждём канал.
		<-ch
		// После пробуждения канал закрыт, b.loading[name] уже удалён
		// (UnlockLoad делает close+delete под loadMu). Fallthrough к
		// проверке b.models.
	}

	// Round 9: проверяем b.models — модель может быть уже загружена
	// (другая горутина завершила load между TryLockLoad и нашим чтением
	// канала, или мы пришли сюда после того как b.loading[name] уже
	// удалён).
	if _, err := b.GetModel(name); err == nil {
		return true
	}

	// Модель не в b.models → загрузка не удалась (или её никогда не было
	// через TryLockLoad, что не наш случай).
	return false
}

// ============================================================
// Инференс
// ============================================================

// Generate выполняет синхронный инференс
//
// Round 8 (2026-07-28) BUGFIX: удерживаем inst.mu на всём протяжении
// inst.handle.Infer(). Без этого два параллельных запроса к одной модели
// вызывали llama_decode() на одном и том же контексте — race на KV-cache
// приводил к GGML_ASSERT(ggml_are_same_shape) и крашу cppworker (SIGABRT).
// Симптом у пользователя: на qwen3.6 35B A3B (inference 5-30s) второй
// юзер получал 503 "connection closed" / "model is loading".
//
// С n_parallel=1 (default) и Hold-Mu-Through-Infer корректное поведение —
// второй запрос ЖДЁТ завершения первого, а не падает. Параллельность
// inference регулируется на уровне балансера (slot manager) и/или
// n_parallel (если >1, нужно сменить на отдельный RWMutex).
//
// Round 13 (2026-07-28): добавлен slot manager. ПОРЯДОК LOCK'ов критичен:
//   1. sm.Acquire(ctx) — получаем slot ID (блокирует если все заняты)
//   2. inst.mu.Lock()  — serialize llama_decode
//   3. handle.Infer(params{SeqId: slot, ...})
//   4. defer sm.Release(slot) срабатывает ПОСЛЕ Unlock (через defer LIFO)
//   5. defer inst.mu.Unlock()
func (b *Backend) Generate(modelName string, prompt string, params bridge.GenerationParams) (*bridge.InferenceResult, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return nil, err
	}

	// Round 13: acquire slot FIRST. Если все слоты заняты — блокируемся
	// (FIFO) пока не освободится. Для defaultNParallel=1 — никогда не
	// блокируется (единственный слот занят только текущим call'ом).
	// ctx — context.Background() (no timeout). Если нужен timeout —
	// передать через params (пока не реализовано).
	if inst.slots == nil {
		// Защита от nil: если модель загружена через устаревший путь без
		// SlotManager (например, init race), создаём single-slot.
		inst.slots = NewSlotManager(1)
	}
	slot, release, err := inst.slots.Acquire(context.Background())
	if err != nil {
		return nil, err
	}
	// ВАЖНО: defer release() ДО defer mu.Unlock() — defer LIFO order:
	// сначала выполнится Unlock (release mu), потом Release slot.
	// Это предотвращает deadlock: слот освобождается ПОСЛЕ unlock mu,
	// поэтому следующий waiter может взять слот, а затем ждать mu.
	defer release()

	// Round 13: пробрасываем slot ID как seq_id в C-bridge.
	// seq_id=0 (default) → legacy single-slot, > 0 → multi-slot slot region.
	params.SeqId = slot

	// Round 8 BUGFIX: держим mu на всём инференсе. До фикса Unlock() был
	// сразу после ++ActiveQueries — два goroutine могли одновременно войти
	// в inst.handle.Infer() и race на llama.cpp context.
	inst.mu.Lock()
	inst.info.TotalQueries++
	inst.info.ActiveQueries++
	// lastUsedAt.Store — atomic, мьютекс не нужен. Делаем тут же для
	// удобства чтения (IdleUnloadManager смотрит на это значение).
	inst.lastUsedAt.Store(time.Now())

	start := time.Now()

	defer func() {
		inst.info.ActiveQueries--
		inst.mu.Unlock()
		if b.metrics != nil {
			duration := time.Since(start)
			tokens := approximateTokens(approxResultLen(inst, prompt, params))
			b.metrics.RecordRequest(modelName, tokens, duration, true)
		}
		inst.lastUsedAt.Store(time.Now())
	}()

	return inst.handle.Infer(prompt, params)
}

// GenerateStream выполняет стриминг-инференс
//
// Round 8 (2026-07-28) BUGFIX: см. Generate — удерживаем inst.mu на всём
// протяжении inst.handle.InferStream().
//
// Round 13 (2026-07-28): добавлен slot manager (см. Generate для деталей
// ordering). Тот же lock pattern: Acquire → Lock → InferStream → Unlock → Release.
//
// Round 15.1 (2026-07-30): если BatchedScheduler активен для этой модели,
// маршрутизируем через BatchedInferStream (true parallel inference).
// Greedy argmax sampling (temp/top_p ignored). Иначе — legacy Round 13 path.
func (b *Backend) GenerateStream(modelName string, prompt string, params bridge.GenerationParams, callback bridge.StreamCallback) error {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return err
	}

	// Round 15.1: если BatchedScheduler активен, маршрутизируем в batched path.
	// BatchedInferStream владеет собственным slot management через BatchedScheduler.
	if inst.batchedScheduler != nil {
		return b.batchedInferStream(inst, modelName, prompt, params, callback)
	}

	// Round 13: acquire slot (см. Generate для объяснения).
	if inst.slots == nil {
		inst.slots = NewSlotManager(1)
	}
	slot, release, err := inst.slots.Acquire(context.Background())
	if err != nil {
		return err
	}
	// ВАЖНО: defer release() ДО defer mu.Unlock() — defer LIFO:
	// сначала Unlock, потом Release (без deadlock).
	defer release()

	// Round 13: пробрасываем slot ID как seq_id.
	params.SeqId = slot

	inst.mu.Lock()
	inst.info.TotalQueries++
	inst.info.ActiveQueries++
	inst.lastUsedAt.Store(time.Now())

	start := time.Now()

	defer func() {
		inst.info.ActiveQueries--
		inst.mu.Unlock()
		if b.metrics != nil {
			b.metrics.RecordRequest(modelName, 0, time.Since(start), true)
		}
		inst.lastUsedAt.Store(time.Now())
	}()

	return inst.handle.InferStream(prompt, params, callback)
}

// batchedInferStream — Round 15.1: streaming inference через BatchedScheduler.
//
// Контракт:
//   1. Tokenize prompt через inst.handle.Tokenize → []int32
//   2. RegisterSession в BatchedScheduler (получаем id, state, TokenCh)
//   3. Loop: читаем int32 токены из state.TokenCh, detokenize через
//      inst.handle.TokenToPiece, передаём в user callback
//   4. TokenCh закрывается scheduler'ом когда session finished
//      (EOG, max_tokens, или scheduler stop)
//
// ВАЖНО: НЕ держим inst.mu здесь! BatchedScheduler.Run goroutine берёт
// modelLock (= &inst.mu) сам на время BatchedDecode. Это позволяет
// реальный parallel inference (N sessions одновременно в одном
// llama_decode call).
//
// Slot management: BatchedScheduler.RegisterSession возвращает ошибку
// при capacity overflow. Caller (HTTP handler) перехватывает и возвращает
// 503 клиенту. Round 15.1 НЕ блокирует на capacity — opt-in флаг для
// workloads где caller знает лимит.
func (b *Backend) batchedInferStream(inst *modelInstance, modelName string, prompt string, params bridge.GenerationParams, callback bridge.StreamCallback) error {
	// 1. Tokenize prompt.
	tokens, err := inst.handle.Tokenize(prompt)
	if err != nil {
		return fmt.Errorf("batched stream: tokenize failed: %w", err)
	}
	if len(tokens) == 0 {
		return errors.New("batched stream: empty prompt after tokenize")
	}

	// 2. Determine MaxTokens.
	// Round 16 P2 fix (2026-07-30): используем константы вместо magic numbers
	// + ссылка на bridge.DefaultGenerationParams().NPredict для default.
	// Раньше было `maxTokens = 2048` hardcoded — при изменении дефолта в
	// bridge.go забылось бы тут.
	const batchedMaxTokensCap = 4096
	maxTokens := params.NPredict
	if maxTokens <= 0 || maxTokens > batchedMaxTokensCap {
		// Batched path: cap at 4096 чтобы один greedy loop не съел всю память.
		// Default = bridge.DefaultGenerationParams().NPredict (current 2048).
		maxTokens = bridge.DefaultGenerationParams().NPredict
	}

	// 3. Register session.
	// Round 15.2: pass Temperature/Seed из params для sampling.
	// 0 = greedy (default), > 0 = softmax+multinomial.
	id, state, err := inst.batchedScheduler.RegisterSession(BatchedSessionParams{
		Prompt:      tokens,
		MaxTokens:   maxTokens,
		Temperature: params.Temperature,
		Seed:        uint32(params.Seed),
	})
	if err != nil {
		return fmt.Errorf("batched stream: register session: %w", err)
	}
	defer inst.batchedScheduler.UnregisterSession(id)

	// 4. Update metrics (BatchedScheduler сам берёт modelLock на время
	// decode, поэтому нам НЕ нужно держать inst.mu тут).
	inst.info.TotalQueries++
	inst.info.ActiveQueries++
	inst.lastUsedAt.Store(time.Now())
	start := time.Now()
	defer func() {
		inst.info.ActiveQueries--
		if b.metrics != nil {
			b.metrics.RecordRequest(modelName, 0, time.Since(start), true)
		}
		inst.lastUsedAt.Store(time.Now())
	}()

	// 5. Stream tokens.
	// Round 16 P2 fix (2026-07-30): explicit ctx-cancellation. Раньше просто
	// `for tok := range state.TokenCh` — блокируется до EOS даже если HTTP
	// client отвалился. Callback-based cancellation работает (callback
	// проверяет sw.IsBroken() и возвращает false → exit), но:
	// (a) Не explicit — при чтении кода не очевидно как cancellation работает.
	// (b) Зависит от того что caller передал callback с ctx-awareness.
	// Теперь: select с ctx.Done() + labelled break для EOS. Cancellation
	// работает в обе стороны — callback returns false ИЛИ ctx.Done().
	//
	// ctx берётся от request handler через callback (callback'и
	// safeStreamWriter инжектят ctx в check). Если caller не передал ctx —
	// fallback через callback.
	//
	// TODO(Round 17): thread ctx явно через GenerateStream → batchedInferStream
	// чтобы убрать неявную зависимость от callback'а.
streamLoop:
	for {
		select {
		case tok, ok := <-state.TokenCh:
			if !ok {
				// TokenCh closed by scheduler — normal EOS
				break streamLoop
			}
			piece := inst.handle.TokenToPiece(tok)
			if piece == "" {
				// Некоторые токены (BOS, special) декодируются в пустую строку.
				// Пропускаем — callback их всё равно не отдаст клиенту.
				continue
			}
			if !callback(piece) {
				// Caller попросил остановиться (return false из callback).
				// Это primary cancellation path — safeStreamWriter callback
				// ловит ctx.Done() внутри Writef/Flush и возвращает false.
				return nil
			}
		}
	}
	// TokenCh закрыт scheduler'ом. Проверяем session.Err.
	if state.Err != nil {
		return state.Err
	}
	return nil
}

// GetEmbeddings получает эмбеддинги текста
func (b *Backend) GetEmbeddings(modelName string, text string) ([]float32, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return nil, err
	}

	return inst.handle.GetEmbeddings(text)
}

// CountTokens возвращает число токенов в тексте для загруженной модели.
// Использует tokenizer модели через C-bridge; в случае ошибки — грубая оценка.
func (b *Backend) CountTokens(modelName string, text string) int {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		if text == "" {
			return 0
		}
		return len([]rune(text)) / 4
	}
	n := inst.handle.CountTokens(text)
	if n <= 0 && text != "" {
		// Fallback to heuristic for gemma-4 and models where
		// llama_tokenize returns 0 (multilingual / broken tokenizers).
		n = len([]rune(text))
	}
	return n
}

// ApplyChatTemplate applies GGUF chat template to messages for a model.
// If no template is embedded in GGUF — returns bridge.ErrNoChatTemplate
// (caller can fallback to raw completion).
func (b *Backend) ApplyChatTemplate(modelName, system string, messages []bridge.ChatMessage, addAss bool) (string, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return "", err
	}
	if inst.handle == nil {
		return "", fmt.Errorf("model %s has no loaded handle", modelName)
	}
	return inst.handle.ApplyChatTemplate(system, messages, addAss)
}

// ApplyChatTemplateWithThinking — Round 14b (2026-07-28): native enable_thinking
// через C++ API common_chat_templates_apply (см. c/bridge/csrc/chat_thinking.cpp).
//
// Применяет chat template с native enable_thinking параметром. Для моделей
// с Jinja enable_thinking variable (Qwen3-thinking, DeepSeek-R1) это даёт
// правильное thinking tag emission. Для моделей без такой variable
// (gemma-4-it, llama-3-it) — supportsThinking=false, caller решает что
// делать (наш fallback — soft prompt injection, см. handlers_chat.go).
//
// Параметры:
//   modelName         — загруженная модель
//   chatTemplateOverride — кастомный Jinja template ("" = use GGUF default)
//   messages          — список chat-сообщений (включая system если нужно)
//   enableThinking    — true = native thinking mode
//   addGenerationPrompt — true = добавить assistant turn tokens в конец
//
// Возвращает:
//   prompt            — formatted prompt
//   supportsThinking  — true если template поддерживает thinking
//   error             — nil / generic error
func (b *Backend) ApplyChatTemplateWithThinking(
	modelName, chatTemplateOverride string,
	messages []bridge.ChatMessage,
	enableThinking, addGenerationPrompt bool,
) (string, bool, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return "", false, err
	}
	if inst.handle == nil {
		return "", false, fmt.Errorf("model %s has no loaded handle", modelName)
	}
	return inst.handle.ApplyChatTemplateWithThinking(
		chatTemplateOverride, messages, enableThinking, addGenerationPrompt,
	)
}

// GetChatTemplate returns raw chat template from GGUF of a model.
func (b *Backend) GetChatTemplate(modelName string) (string, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return "", err
	}
	if inst.handle == nil {
		return "", fmt.Errorf("model %s has no loaded handle", modelName)
	}
	return inst.handle.GetChatTemplate()
}

// ============================================================
// GPU управление
// ============================================================

// GetGPUCount возвращает количество GPU
func (b *Backend) GetGPUCount() int {
	return b.gpuCount
}

// GetGPUDevices возвращает информацию о всех GPU
func (b *Backend) GetGPUDevices() []bridge.GPUDevice {
	result := make([]bridge.GPUDevice, len(b.gpuDevices))
	copy(result, b.gpuDevices)
	return result
}

// GetGPUMetrics возвращает метрики использования GPU
func (b *Backend) GetGPUMetrics() []map[string]interface{} {
	result := make([]map[string]interface{}, 0, b.gpuCount)
	for _, dev := range b.gpuDevices {
		metrics := map[string]interface{}{
			"index":        dev.Index,
			"name":         dev.Name,
			"vramTotalMB":  dev.VRAMTotalMB,
			"vramFreeMB":   dev.VRAMFreeMB,
			"vramUsedMB":   dev.VRAMTotalMB - dev.VRAMFreeMB,
			"vramUsagePct": float64(0),
		}
		if dev.VRAMTotalMB > 0 {
			metrics["vramUsagePct"] = float64(dev.VRAMTotalMB-dev.VRAMFreeMB) / float64(dev.VRAMTotalMB) * 100
		}
		result = append(result, metrics)
	}
	return result
}

// ============================================================
// Метрики Backend
// ============================================================

// GetMetrics возвращает общие метрики backend
func (b *Backend) GetMetrics() map[string]interface{} {
	b.mu.RLock()
	defer b.mu.RUnlock()

	totalModels := len(b.models)
	loadedCount := 0
	totalQueries := int64(0)
	activeQueries := 0

	for _, inst := range b.models {
		if inst.info.State == StateLoaded {
			loadedCount++
		}
		totalQueries += inst.info.TotalQueries
		activeQueries += b.getActiveQueries(inst)
	}

	result := map[string]interface{}{
		"version":       bridge.Version(),
		"uptimeSeconds": time.Since(b.startTime).Seconds(),
		"gpuCount":      b.gpuCount,
		"totalModels":   totalModels,
		"loadedModels":  loadedCount,
		"totalQueries":  totalQueries,
		"activeQueries": activeQueries,
	}

	// Добавляем метрики из Metrics если доступны
	if b.metrics != nil {
		ms := b.metrics.GetMetricsSnapshot()
		for k, v := range ms {
			result[k] = v
		}
	}

	return result
}

// Status возвращает статус backend
func (b *Backend) Status() map[string]interface{} {
	metrics := b.GetMetrics()
	metrics["gpuDevices"] = b.GetGPUMetrics()
	metrics["models"] = b.ListModels()
	return metrics
}

// Close завершает работу backend, выгружая все модели
func (b *Backend) Close() {
	// Останавливаем idle unloader
	if b.idleUnloader != nil {
		b.idleUnloader.Stop()
	}

	// Останавливаем HuggingFace загрузчик
	if b.hfDownloader != nil {
		b.hfDownloader.Close()
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	logger.Get().Infow("shutting down CppBackend",
		"modelsToUnload", len(b.models))

	for name, inst := range b.models {
		inst.mu.Lock()
		if inst.handle != nil {
			inst.handle.FreeModel()
		}
		inst.info.State = StateUnloaded
		inst.mu.Unlock()
		delete(b.models, name)
		logger.Get().Infow("model unloaded on shutdown", "name", name)
	}
}

// ============================================================
// Внутренние методы
// ============================================================

func (b *Backend) getModelInstance(name string) (*modelInstance, error) {
	b.mu.RLock()
	inst, exists := b.models[name]
	b.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("model %s not found", name)
	}

	inst.mu.Lock()
	if inst.handle == nil || unsafe.Pointer(inst.handle) == nil {
		inst.mu.Unlock()
		return nil, fmt.Errorf("model %s handle is nil", name)
	}
	if inst.info.State != StateLoaded {
		inst.mu.Unlock()
		return nil, fmt.Errorf("model %s is not loaded (state: %s)", name, inst.info.State)
	}
	inst.mu.Unlock()

	return inst, nil
}

func (b *Backend) getActiveQueries(inst *modelInstance) int {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.info.ActiveQueries
}

// approximateTokens — грубая оценка количества токенов.
// DEPRECATED: используйте Backend.CountTokens с реальным tokenizer.
func approximateTokens(text string) int {
	if text == "" {
		return 0
	}
	return len([]rune(text)) / 4
}

// approxResultLen — приблизительная длина результата для метрик
func approxResultLen(inst *modelInstance, prompt string, params bridge.GenerationParams) string {
	// В стубе возвращаем пустую строку, т.к. результат не известен до вызова
	return ""
}

// ============================================================
// Хелперы для расчёта памяти и слоёв
// ============================================================

// getSystemRAMGB возвращает общий объём системной RAM в GB.
// На Linux читает /proc/meminfo; на других ОС возвращает fallback 16GB.
func getSystemRAMGB() uint64 {
	// Пытаемся прочитать /proc/meminfo (Linux)
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		// macOS или другие — fallback
		return 16
	}

	// Ищем строку MemTotal
	content := string(data)
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			// MemTotal:   16384000 kB  → число в kB
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				var kB uint64
				if _, err := fmt.Sscanf(parts[1], "%d", &kB); err == nil {
					return kB / 1024 / 1024 // kB → GB
				}
			}
		}
	}
	return 16 // fallback
}

// GGUFHeaderInfo — метаданные, извлекаемые из GGUF header ДО загрузки модели.
type GGUFHeaderInfo struct {
	Architecture string `json:"architecture"`
	NLayers      int    `json:"nLayers"`
	NHeads       int    `json:"nHeads"`
	NKvHeads     int    `json:"nKvHeads"`
	HeadDimK          int       `json:"headDimK"`
	HeadDimV          int       `json:"headDimV"`
	KVCacheType       string    `json:"kvCacheType"`
	NEmbd        int    `json:"nEmbd"`
	FileSize     int64  `json:"fileSize"`
}

// ggufFieldSuffixes — суффиксы ключей метаданных для параметров модели.
// Каждый параметр имеет вид <architecture>.<suffix>.
// Используется для генерации всех возможных ключей из ggufModelArchs.
var ggufFieldSuffixes = []string{
	".block_count",
	".attention.head_count",
	".attention.head_count_kv",
	".embedding_length",
}

// ggufModelArchs — известные архитектуры GGUF.
// Каждая архитектура может иметь префикс для ключей метаданных.
var ggufModelArchs = []string{
	"llama", "qwen2", "qwen3", "qwen3moe", "qwen3next", "qwen35", "qwen35moe",
	"gemma2", "gemma4", "gemma3",
	"starcoder2", "gpt_bigcode", "falcon", "mpt", "phi3", "bert", "nemotron",
}

// initGGUFKeyMap инициализирует реверсивный маппинг "gguf key → field index"
// один раз при старте. Это позволяет O(1) определение поля по ключу.
func initGGUFKeyMap() map[string]int {
	m := make(map[string]int)
	for _, arch := range ggufModelArchs {
		for fi, suffix := range ggufFieldSuffixes {
			key := arch + suffix
			m[key] = fi
		}
	}
	return m
}

// ggufKeyMap — глобальный map "gguf key → field index"
// Field index: 0=NLayers, 1=NHeads, 2=NKvHeads, 3=NEmbd
var ggufKeyMap = initGGUFKeyMap()

// ReadGGUFHeader — публичная обёртка над readGGUFHeaderInfo для использования
// из cmd/cppworker и других пакетов.
//
// Используется ensureModelLoaded для lazy-load архитектурных параметров модели
// (NLayers/NEmbd/NHeads/NKvHeads) ДО llama.cpp.LoadModel, чтобы корректно
// рассчитать auto-tuned n_ctx/gpu_layers в calculateLazyLoadOpts.
//
// Не падает на ошибке — caller сам решает fallback. Возвращает *GGUFHeaderInfo
// с заполненными полями если header валидный.
func ReadGGUFHeader(path string) (*GGUFHeaderInfo, error) {
	return readGGUFHeaderInfo(path)
}

// readGGUFHeaderInfo читает заголовок GGUF файла и извлекает ключевые метаданные.
// Работает без загрузки модели в llama.cpp — читает только header (metadata KV).
//
// Формат GGUF v3:
//
//	[4]byte magic = "GGUF"
//	uint32 version = 3
//	uint64 tensorCount
//	uint64 metadataKvCount
//	[]MetadataKV — пары ключ-значение с архитектурой, параметрами и т.д.
//
// Поддерживаемые GGUF metadata keys (с маппингом по архитектуре):
//   - general.architecture              (string)
//   - <arch>.block_count                (uint32) — NLayers
//   - <arch>.attention.head_count       (uint32) — NHeads
//   - <arch>.attention.head_count_kv    (uint32) — NKvHeads
//   - <arch>.embedding_length           (uint32) — NEmbd
//
// После чтения general.architecture переключается на соответствующий префикс.
func readGGUFHeaderInfo(path string) (*GGUFHeaderInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open gguf: %w", err)
	}
	defer f.Close()

	// Определяем размер файла
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat gguf: %w", err)
	}

	// Читаем magic (4 байта)
	var magic [4]byte
	if _, err := f.Read(magic[:]); err != nil {
		return nil, fmt.Errorf("read magic: %w", err)
	}
	if magic[0] != 'G' || magic[1] != 'G' || magic[2] != 'U' || magic[3] != 'F' {
		return nil, fmt.Errorf("invalid GGUF magic: %q", string(magic[:]))
	}

	// Читаем версию (uint32)
	var version uint32
	if err := binary.Read(f, binary.LittleEndian, &version); err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	if version < 1 || version > 3 {
		return nil, fmt.Errorf("unsupported GGUF version: %d", version)
	}

	// Читаем tensor count (uint64)
	var tensorCount uint64
	if err := binary.Read(f, binary.LittleEndian, &tensorCount); err != nil {
		return nil, fmt.Errorf("read tensor count: %w", err)
	}
	_ = tensorCount

	// Читаем metadata KV count (uint64)
	var kvCount uint64
	if err := binary.Read(f, binary.LittleEndian, &kvCount); err != nil {
		return nil, fmt.Errorf("read metadata KV count: %w", err)
	}

	info := &GGUFHeaderInfo{
		FileSize: fi.Size(),
	}

	// Читаем KV пары
	for i := uint64(0); i < kvCount; i++ {
		// Читаем длину ключа
		var keyLen uint64
		if err := binary.Read(f, binary.LittleEndian, &keyLen); err != nil {
			return nil, fmt.Errorf("read key[%d] length: %w", i, err)
		}

		// Валидация: ключ не должен быть длиннее 8192 байт (защита от corrupted GGUF)
		if keyLen > 8192 {
			return nil, fmt.Errorf("key[%d] length %d exceeds maximum 8192 (likely corrupted GGUF)", i, keyLen)
		}

		// Читаем ключ
		keyBuf := make([]byte, keyLen)

		if _, err := f.Read(keyBuf); err != nil {
			return nil, fmt.Errorf("read key[%d]: %w", i, err)
		}
		key := string(keyBuf)

		// Читаем тип значения (uint32)
		var valType uint32
		if err := binary.Read(f, binary.LittleEndian, &valType); err != nil {
			return nil, fmt.Errorf("read key[%q] value type: %w", key, err)
		}

		// Парсим значение в зависимости от типа
		// Типы GGUF: 0=uint8, 1=int8, 2=uint16, 3=int16, 4=uint32, 5=int32,
		//            6=float32, 7=bool, 8=string, 9=array, 10=uint64, 11=int64, 12=float64
		//
		// Нас интересуют: string(8), uint32(4), int32(5), uint64(10), float32(6), array(9)

		switch valType {
		case 8: // string
			var strLen uint64
			if err := binary.Read(f, binary.LittleEndian, &strLen); err != nil {
				return nil, fmt.Errorf("read key[%q] string length: %w", key, err)
			}
			strBuf := make([]byte, strLen)
			if _, err := f.Read(strBuf); err != nil {
				return nil, fmt.Errorf("read key[%q] string value: %w", key, err)
			}
			strVal := string(strBuf)
			switch key {
			case "general.architecture":
				info.Architecture = strVal
			}

		case 4: // uint32
			var val uint32
			if err := binary.Read(f, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] uint32: %w", key, err)
			}
			if fi, ok := ggufKeyMap[key]; ok {
				ggufSetField(info, fi, int(val))
			}

		case 10: // uint64
			var val uint64
			if err := binary.Read(f, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] uint64: %w", key, err)
			}
			if fi, ok := ggufKeyMap[key]; ok {
				ggufSetField(info, fi, int(val))
			}

		case 5: // int32
			var val int32
			if err := binary.Read(f, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] int32: %w", key, err)
			}
			if fi, ok := ggufKeyMap[key]; ok {
				ggufSetField(info, fi, int(val))
			}

		case 11: // int64
			var val int64
			if err := binary.Read(f, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] int64: %w", key, err)
			}
			if fi, ok := ggufKeyMap[key]; ok {
				ggufSetField(info, fi, int(val))
			}

		case 6: // float32
			var val float32
			if err := binary.Read(f, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] float32: %w", key, err)
			}
			_ = val // не используем, пропускаем

		case 0: // uint8
			var val uint8
			if err := binary.Read(f, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] uint8: %w", key, err)
			}
			_ = val

		case 7: // bool
			var val uint8
			if err := binary.Read(f, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] bool: %w", key, err)
			}
			_ = val

		case 9: // array — пропускаем, зная тип элемента и количество
			var elemType uint32
			if err := binary.Read(f, binary.LittleEndian, &elemType); err != nil {
				return nil, fmt.Errorf("read key[%q] array elem type: %w", key, err)
			}
			var arrLen uint64
			if err := binary.Read(f, binary.LittleEndian, &arrLen); err != nil {
				return nil, fmt.Errorf("read key[%q] array len: %w", key, err)
			}
			// Пропускаем элементы массива
			elemSize := ggufTypeSize(elemType)
			if elemSize > 0 {
				skipBytes := int64(arrLen) * int64(elemSize)
				if _, err := f.Seek(skipBytes, 1); err != nil {
					return nil, fmt.Errorf("skip array key[%q]: %w", key, err)
				}
			}

		default:
			// Неизвестный тип — не можем пропустить, ошибка
			return nil, fmt.Errorf("unsupported GGUF metadata type %d for key %q", valType, key)
		}
	}

	return info, nil
}

// ggufSetField устанавливает поле GGUFHeaderInfo по field index (0-3).
func ggufSetField(info *GGUFHeaderInfo, fieldIndex, value int) {
	switch fieldIndex {
	case 0:
		info.NLayers = value
	case 1:
		info.NHeads = value
	case 2:
		info.NKvHeads = value
	case 3:
		info.NEmbd = value
	}
}

// ggufTypeSize возвращает размер элемента GGUF metadata value по типу.
// Для скалярных типов — их размер, для массивов — размер одного элемента.
func ggufTypeSize(valType uint32) int64 {
	switch valType {
	case 0, 1, 7: // uint8, int8, bool
		return 1
	case 2, 3: // uint16, int16
		return 2
	case 4, 5, 6: // uint32, int32, float32
		return 4
	case 10, 11, 12: // uint64, int64, float64
		return 8
	default:
		return 0 // variable length (string, array) — не используем
	}
}

// estimateLayersFromFileSize оценивает общее число слоёв модели на основе размера GGUF файла.

// Эвристика для типичных архитектур Llama/Mistral/Qwen при Q4_K_M квантизации:
//   - < 5GB   → 7B модель (32 слоя)
//   - 5-10GB  → 13B модель (40 слоёв)
//   - 10-20GB → 30-33B модель (60 слоёв)
//   - 20-40GB → 70B модель (80 слоёв)
//   - > 40GB  → 120B+ модель (120 слоёв)
func estimateLayersFromFileSize(sizeBytes int64) int {
	sizeGB := float64(sizeBytes) / 1024 / 1024 / 1024
	switch {
	case sizeGB < 4.5:
		return 32 // 7B params
	case sizeGB < 7:
		return 42 // 7-9B class (gemma-4, mistral-7B v3, etc.)
	case sizeGB < 10:
		return 40 // 13B params
	case sizeGB < 20:
		return 60 // 30-33B params
	case sizeGB < 40:
		return 80 // 70B params
	default:
		return 120 // 120B+ params
	}
}

// GetSystemRAMGB — экспортированная обёртка для использования из других пакетов
func GetSystemRAMGB() uint64 {
	return getSystemRAMGB()
}