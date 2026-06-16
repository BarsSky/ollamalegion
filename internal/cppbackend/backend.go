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
	"fmt"
	"os"
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
	// Валидация VRAM перед загрузкой
	if vramErr := b.checkVRAMForModel(name, path, opts); vramErr != nil {
		return vramErr
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

// checkVRAMForModel проверяет, достаточно ли видеопамяти для загрузки модели
func (b *Backend) checkVRAMForModel(name string, path string, opts LoadModelOpts) error {
	// Получаем размер файла модели
	fi, err := os.Stat(path)
	if err != nil {
		// Если файл не существует, пропускаем проверку (может быть ещё не скачан)
		return nil
	}
	modelFileSizeMB := float64(fi.Size()) / 1024 / 1024

	// Оцениваем VRAM под модель
	estimatedVRAM := EstimateGPUMemoryForModel(fi.Size(), opts.GPULayers, 40) // предполагаем ~40 слоёв
	// Добавляем контекст: ~1MB на 1K контекста * ctxSize
	ctxOverheadMB := float64(opts.ContextSize) / 1024.0
	estimatedVRAM += uint64(ctxOverheadMB)

	// Проверяем доступную VRAM на всех GPU
	b.mu.RLock()
	defer b.mu.RUnlock()

	var totalFreeVRAM uint64
	for _, dev := range b.gpuDevices {
		totalFreeVRAM += uint64(dev.VRAMFreeMB)
	}

	if totalFreeVRAM == 0 {
		return nil // нет данных о VRAM — пропускаем проверку
	}

	if estimatedVRAM > totalFreeVRAM {
		return fmt.Errorf("insufficient VRAM: model needs ~%d MB (%d MB file + %.0f MB context), but only %d MB free. Reduce gpuLayers or ctxSize",
			estimatedVRAM, int(modelFileSizeMB), ctxOverheadMB, totalFreeVRAM)
	}

	logger.Get().Infow("VRAM check passed",
		"model", name,
		"estimatedVRAM_MB", estimatedVRAM,
		"availableVRAM_MB", totalFreeVRAM,
		"modelFileSize_MB", int(modelFileSizeMB))

	return nil
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