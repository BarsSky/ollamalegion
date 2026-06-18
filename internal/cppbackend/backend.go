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
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
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
	Name          string    `json:"name"`
	Path          string    `json:"path"`
	State         LoadState `json:"state"`
	Architecture  string    `json:"architecture,omitempty"`
	NLayers       int       `json:"nLayers"`
	NHeads        int       `json:"nHeads"`
	NKvHeads      int       `json:"nKvHeads"`
	NEmbd         int       `json:"nEmbd"`
	NVocab        int       `json:"nVocab"`
	ContextSize   int       `json:"contextSize"`
	SizeBytes     uint64    `json:"sizeBytes"`
	LoadedAt      time.Time `json:"loadedAt"`
	GPUCount      int       `json:"gpuCount,omitempty"`
	GPULayers     int       `json:"gpuLayers"`
	TensorSplit   []float32 `json:"tensorSplit,omitempty"`
	ActiveQueries int       `json:"activeQueries"`
	TotalQueries  int64     `json:"totalQueries"`
	// Дополнительные параметры загрузки (Шаг 4 — /api/models/reload).
	// Хранятся вместе с моделью, чтобы при reload можно было
	// переиспользовать их как дефолты, если клиент не указал override.
	BatchSize     int       `json:"batchSize"`
	FlashAttnType int       `json:"flashAttnType"`
	NUMA          bool      `json:"numa"`
	UseMmap       bool      `json:"useMmap"`
	// === Loading state (Шаг «отображение загрузки в мониторе и вкладке бэкендов») ===
	// Заполняются пока State == StateLoading, чтобы UI мог показывать
	// «Загружается model-name 25s» и спиннер. После успеха/ошибки поля обнуляются.
	LoadingStartedAt time.Time `json:"loadingStartedAt,omitempty"`
	LoadingSizeBytes int64     `json:"loadingSizeBytes,omitempty"`
	LoadingError     string    `json:"loadingError,omitempty"`
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
}

// LoadModelOpts — опции загрузки модели, передаваемые из WebUI/API
type LoadModelOpts struct {
	GPULayers     int
	ContextSize   int
	BatchSize     int
	FlashAttnType int
	NUMA          bool
	UseMmap       bool
	TensorSplit   []float32
}

// modelInstance — экземпляр загруженной модели
type modelInstance struct {
	info     ModelInfo
	handle   *bridge.ModelHandle
	metadata *bridge.ModelMetadata
	mu       sync.Mutex
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
	}

	// Idle unload manager
	if cfg.MetricsRetentionS > 0 {
		b.idleUnloader = NewIdleUnloadManager(b, time.Duration(cfg.MetricsRetentionS)*time.Second)
	}

	// HuggingFace downloader
	b.hfDownloader = NewHuggingFaceDownloader(
		cfg.HuggingFaceToken,
		cfg.HFMirror,
		cfg.DownloadsDir,
		cfg.ModelsDir,
	)

	return b
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
			Name:              name,
			Path:              path,
			State:             StateLoading,
			GPULayers:         gpuLayers,
			BatchSize:         opts.BatchSize,
			FlashAttnType:     opts.FlashAttnType,
			NUMA:              opts.NUMA,
			UseMmap:           opts.UseMmap,
			LoadedAt:          time.Now(),
			LoadingStartedAt:  time.Now(),
			LoadingSizeBytes:  loadingSizeBytes,
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
	cfg.NThreads = b.cfg.DefaultNThreads

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
	cfg.KVCacheType = b.cfg.DefaultKVCacheType

	// Нормализация
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
	inst.info.NEmbd = meta.NEmbd
	inst.info.NVocab = meta.NVocab
	inst.info.ContextSize = meta.ContextLength
	inst.info.SizeBytes = meta.SizeTotalBytes
	inst.info.GPUCount = b.gpuCount
	b.mu.Unlock()

	// Записываем метрики
	if b.metrics != nil {
		b.metrics.RecordLoad(name)
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
	if wantedGPULayers == -1 || wantedGPULayers > totalLayers {
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
	vramForFullGPU := EstimateGPUMemoryForModel(modelSizeBytes, totalLayers, totalLayers)
	vramForFullGPU += kvCacheMB
	diag2.VRAMRequiredForFullGPU = vramForFullGPU

	// Если желаемые GPU-слои влезают в VRAM — используем их как есть
	vramForWanted := EstimateGPUMemoryForModel(modelSizeBytes, wantedGPULayers, totalLayers)
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
		needed := EstimateGPUMemoryForModel(modelSizeBytes, mid, totalLayers)
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
	diag2.VRAMRequiredForOptimal = EstimateGPUMemoryForModel(modelSizeBytes, bestLayers, totalLayers) + kvCacheMB
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
	if nLayers <= 0 || nHeads <= 0 || nEmbd <= 0 || nCtx <= 0 {
		return 0
	}

	if nKvHeads <= 0 {
		nKvHeads = nHeads // MHA (no GQA)
	}

	// head_dim = n_embd / n_heads (обычно 128 для большинства моделей)
	headDim := nEmbd / nHeads
	if headDim <= 0 {
		headDim = 128 // fallback
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
func (b *Backend) checkVRAMForModel(name string, path string, opts LoadModelOpts) (LoadModelOpts, error) {
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
		// nHeads и nEmbd остаются 0 — estimateKVCacheMB вернёт 0
	}

	optGPULayers, useMmap, diagnostics := b.CalculateOptimalGPULayers(
		fi.Size(), totalLayers, nHeads, nKvHeads, nEmbd,
		opts.GPULayers, opts.ContextSize,
	)

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
	return &info, nil
}

// ListModels возвращает список всех загруженных моделей
func (b *Backend) ListModels() []ModelInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]ModelInfo, 0, len(b.models))
	for _, inst := range b.models {
		info := inst.info
		info.ActiveQueries = b.getActiveQueries(inst)
		result = append(result, info)
	}
	return result
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
// Используется когда tryLockLoad вернул (false, nil) — другая горутина уже грузит.
func (b *Backend) WaitForLoad(name string) bool {
	b.loadMu.Lock()
	ch, ok := b.loading[name]
	b.loadMu.Unlock()
	if !ok {
		return false // загрузка уже завершена
	}
	<-ch // ждём завершения загрузки

	// Проверяем результат
	_, err := b.GetModel(name)
	return err == nil
}

// ============================================================
// Инференс
// ============================================================

// Generate выполняет синхронный инференс
func (b *Backend) Generate(modelName string, prompt string, params bridge.GenerationParams) (*bridge.InferenceResult, error) {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return nil, err
	}

	inst.mu.Lock()
	inst.info.TotalQueries++
	inst.info.ActiveQueries++
	inst.mu.Unlock()

	start := time.Now()

	defer func() {
		inst.mu.Lock()
		inst.info.ActiveQueries--
		inst.mu.Unlock()
		if b.metrics != nil {
			duration := time.Since(start)
			tokens := approximateTokens(approxResultLen(inst, prompt, params))
			b.metrics.RecordRequest(modelName, tokens, duration, true)
		}
	}()

	return inst.handle.Infer(prompt, params)
}

// GenerateStream выполняет стриминг-инференс
func (b *Backend) GenerateStream(modelName string, prompt string, params bridge.GenerationParams, callback bridge.StreamCallback) error {
	inst, err := b.getModelInstance(modelName)
	if err != nil {
		return err
	}

	inst.mu.Lock()
	inst.info.TotalQueries++
	inst.info.ActiveQueries++
	inst.mu.Unlock()

	start := time.Now()

	defer func() {
		inst.mu.Lock()
		inst.info.ActiveQueries--
		inst.mu.Unlock()
		if b.metrics != nil {
			duration := time.Since(start)
			b.metrics.RecordRequest(modelName, 0, duration, true)
		}
	}()

	return inst.handle.InferStream(prompt, params, callback)
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
	return inst.handle.CountTokens(text)
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
			"index":       dev.Index,
			"name":        dev.Name,
			"vramTotalMB": dev.VRAMTotalMB,
			"vramFreeMB":  dev.VRAMFreeMB,
			"vramUsedMB":  dev.VRAMTotalMB - dev.VRAMFreeMB,
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
	"llama", "qwen2", "gemma2", "starcoder2", "gpt_bigcode",
	"falcon", "mpt", "phi3", "bert", "nemotron",
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

// readGGUFHeaderInfo читает заголовок GGUF файла и извлекает ключевые метаданные.
// Работает без загрузки модели в llama.cpp — читает только header (metadata KV).
//
// Формат GGUF v3:
//   [4]byte magic = "GGUF"
//   uint32 version = 3
//   uint64 tensorCount
//   uint64 metadataKvCount
//   []MetadataKV — пары ключ-значение с архитектурой, параметрами и т.д.
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
	case sizeGB < 5:
		return 32 // 7B params
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
