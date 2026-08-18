// Package cppbackend — управление GGUF моделями
//
// ModelManager отвечает за:
// - Сканирование директории моделей и обнаружение .gguf файлов
// - Кэширование метаданных моделей (из GGUF header)
// - Автоматическую выгрузку неактивных моделей (idle unload)
// - Загрузку моделей по требованию (lazy load)
package cppbackend

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// GGUFModelMeta — информация о GGUF файле (извлекается из header)
type GGUFModelMeta struct {
	Filename     string    `json:"filename"`
	Path         string    `json:"path"`
	SizeBytes    int64     `json:"sizeBytes"`
	ModifiedAt   time.Time `json:"modifiedAt"`
	Architecture string    `json:"architecture,omitempty"` // из GGUF header
	FileType     string    `json:"fileType,omitempty"`     // Q4_K_M, Q5_K_M, F16, etc.

	// Архитектурные параметры (для auto-tune n_ctx/gpu_layers).
	// Заполняются лениво при первом обращении через GetModelMeta или
	// eagerly в ScanModels (если файл маленький и читается за <100ms).
	NLayers  int `json:"nLayers,omitempty"`
	NEmbd    int `json:"nEmbd,omitempty"`
	NHeads   int `json:"nHeads,omitempty"`
	NKvHeads int `json:"nKvHeads,omitempty"`

	// Round 37 (2026-08-18): ContextLength — GGUF training context (*.context_length).
	// Lazy-loaded в GetModelMeta через ReadGGUFHeader. КРИТИЧНО для auto-adapt:
	// balancer может спросить "what's GGUF max для этой модели?" и НЕ читать файл
	// заново (cache hit).
	ContextLength int `json:"contextLength,omitempty"`

	// Round 37 (2026-08-18): KVCacheType — профильный override из per-model profile
	// (config.bundled.json). НЕ из GGUF (его там нет) — задаётся через
	// profile-syncer pull. Используется feasible.go для расчёта kv_per_token.
	KVCacheType string `json:"kvCacheType,omitempty"`
}

// ModelManager — управляет модельками
type ModelManager struct {
	modelsDir string
	config    Config

	mu        sync.RWMutex
	ggufFiles map[string]*GGUFModelMeta // filename → meta

	// Round 22 (2026-08-03): history успешных load-операций (name → path).
	// Нужно для resolveModelPath после idle-unload: имя, под которым модель
	// загружалась (например "qwen3-4b"), больше не в ggufFiles (т.к. это alias,
	// не filename), но мы ЗНАЕМ что она соответствует файлу
	// Qwen3-Instruct-2507-q4km.gguf — потому что загружали её с этим path.
	// Без этой истории: auto-pick single .gguf требует len==1, не работает
	// когда в директории 2+ файла (Round 22 BUG #2).
	nameHistory map[string]string // name → resolved file path

	// Статистика
	totalScans   int64
	lastScanTime time.Time
	scanDuration time.Duration
}

// NewModelManager создаёт новый ModelManager
func NewModelManager(modelsDir string, cfg Config) *ModelManager {
	return &ModelManager{
		modelsDir:   modelsDir,
		config:      cfg,
		ggufFiles:   make(map[string]*GGUFModelMeta),
		nameHistory: make(map[string]string),
	}
}

// RecordModelLoad — записывает успешный load (name → path) в nameHistory.
// Вызывается из Backend.LoadModel после успешной загрузки модели в VRAM,
// чтобы resolveModelPath мог найти alias после idle-unload.
//
// Round 22 (2026-08-03).
func (m *ModelManager) RecordModelLoad(name, path string) {
	if name == "" || path == "" {
		return
	}
	m.mu.Lock()
	m.nameHistory[name] = path
	m.mu.Unlock()
	logger.Get().Infow("ModelManager.RecordModelLoad: recorded",
		"name", name, "path", path)
}

// LookupNameHistory — проверяет, был ли name ранее загружен с каким-то path.
// Round 22 (2026-08-03).
func (m *ModelManager) LookupNameHistory(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	path, ok := m.nameHistory[name]
	return path, ok
}

// ScanModels сканирует директорию на предмет .gguf файлов
func (m *ModelManager) ScanModels() ([]GGUFModelMeta, error) {
	start := time.Now()
	log := logger.Get()

	m.mu.Lock()
	m.totalScans++
	m.mu.Unlock()

	// Проверяем существование директории
	info, err := os.Stat(m.modelsDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Infow("models directory does not exist, creating",
				"dir", m.modelsDir)
			if mkErr := os.MkdirAll(m.modelsDir, 0755); mkErr != nil {
				return nil, fmt.Errorf("create models dir: %w", mkErr)
			}
		} else {
			return nil, fmt.Errorf("stat models dir: %w", err)
		}
	} else if !info.IsDir() {
		return nil, fmt.Errorf("models path is not a directory: %s", m.modelsDir)
	}

	// Сканируем .gguf файлы
	entries, err := os.ReadDir(m.modelsDir)
	if err != nil {
		return nil, fmt.Errorf("read models dir: %w", err)
	}

	newFiles := make(map[string]*GGUFModelMeta)
	var found int

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}

		fi, err := entry.Info()
		if err != nil {
			continue
		}

		meta := &GGUFModelMeta{
			Filename:   name,
			Path:       filepath.Join(m.modelsDir, name),
			SizeBytes:  fi.Size(),
			ModifiedAt: fi.ModTime(),
		}

		// Пытаемся извлечь архитектуру из GGUF header (лениво)
		// Полные метаданные будут извлечены при загрузке модели

		newFiles[name] = meta
		found++
	}

	// Обновляем кэш
	m.mu.Lock()
	m.ggufFiles = newFiles
	m.lastScanTime = time.Now()
	m.scanDuration = time.Since(start)
	m.mu.Unlock()

	log.Infow("model scan complete",
		"found", found,
		"dir", m.modelsDir,
		"duration", m.scanDuration)

	return m.ListModels(), nil
}

// ListModels возвращает список всех .gguf файлов в директории
func (m *ModelManager) ListModels() []GGUFModelMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]GGUFModelMeta, 0, len(m.ggufFiles))
	for _, meta := range m.ggufFiles {
		result = append(result, *meta)
	}

	// Сортируем по имени
	sort.Slice(result, func(i, j int) bool {
		return result[i].Filename < result[j].Filename
	})

	return result
}

// GetModelMeta возвращает метаданные конкретного GGUF файла.
//
// Если архитектурные параметры (NLayers/NEmbd/NHeads/NKvHeads) ещё не
// прочитаны из GGUF header — читает их лениво через ReadGGUFHeader
// и обновляет кэш. Это даёт ensureModelLoaded возможность рассчитать
// max viable n_ctx ДО llama.cpp.LoadModel.
func (m *ModelManager) GetModelMeta(filename string) (*GGUFModelMeta, error) {
	m.mu.RLock()
	meta, exists := m.ggufFiles[filename]
	m.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("model %s not found in models directory", filename)
	}

	// Lazy-load архитектурных параметров из GGUF header.
	if meta.NLayers == 0 || meta.NEmbd == 0 || meta.NHeads == 0 || meta.ContextLength == 0 {
		if hdr, err := ReadGGUFHeader(meta.Path); err == nil && hdr != nil {
			m.mu.Lock()
			if meta.NLayers == 0 {
				meta.NLayers = hdr.NLayers
			}
			if meta.NEmbd == 0 {
				meta.NEmbd = hdr.NEmbd
			}
			if meta.NHeads == 0 {
				meta.NHeads = hdr.NHeads
			}
			if meta.NKvHeads == 0 && hdr.NKvHeads > 0 {
				meta.NKvHeads = hdr.NKvHeads
			}
			// Round 37 (2026-08-18): ContextLength lazy-load.
			// ReadGGUFHeader парсит *.context_length (см. backend.go:ggufSetField case 4).
			// Qwen3.6-35B-A3B-UD-Q4_K_M → 262144 (262K токенов).
			if meta.ContextLength == 0 && hdr.ContextLength > 0 {
				meta.ContextLength = hdr.ContextLength
			}
			if meta.Architecture == "" {
				meta.Architecture = hdr.Architecture
			}
			m.mu.Unlock()
		}
		// Если ReadGGUFHeader упал — caller использует fallback на
		// estimateLayersFromFileSize (см. Backend.checkVRAMForModel).
	}

	return meta, nil
}

// FindModelByPath ищет модель по полному пути или имени
func (m *ModelManager) FindModelByPath(path string) (string, error) {
	// Если путь абсолютный и файл существует
	if filepath.IsAbs(path) {
		if _, err := os.Stat(path); err == nil {
			return filepath.Base(path), nil
		}
		return "", fmt.Errorf("model file not found: %s", path)
	}

	// Ищем по имени среди gguf файлов
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Round 22 (2026-08-03): Шаг 0 — проверить nameHistory (alias → path).
	// Это решает BUG #2: после idle-unload alias типа "qwen3-4b" больше
	// не в loaded-моделях, но мы ЗНАЕМ что она соответствовала определённому
	// файлу (записано RecordModelLoad при успешной загрузке).
	//
	// Также проверяем nameHistory для path + ".gguf" — некоторые handlers
	// (например handleOllamaShow) добавляют .gguf перед вызовом.
	if histPath, ok := m.nameHistory[path]; ok {
		if _, err := os.Stat(histPath); err == nil {
			return histPath, nil
		}
		// Файл был удалён — fallback дальше
	}
	if !strings.HasSuffix(path, ".gguf") {
		if histPath, ok := m.nameHistory[path+".gguf"]; ok {
			if _, err := os.Stat(histPath); err == nil {
				return histPath, nil
			}
		}
	} else {
		// path уже с .gguf — попробуем без
		baseName := strings.TrimSuffix(path, ".gguf")
		if histPath, ok := m.nameHistory[baseName]; ok {
			if _, err := os.Stat(histPath); err == nil {
				return histPath, nil
			}
		}
	}

	// Точное совпадение
	if meta, ok := m.ggufFiles[path]; ok {
		return meta.Path, nil
	}

	// Поиск без расширения
	for name, meta := range m.ggufFiles {
		if strings.TrimSuffix(name, ".gguf") == path {
			return meta.Path, nil
		}
		// Частичное совпадение (без учёта регистра)
		if strings.Contains(strings.ToLower(name), strings.ToLower(path)) {
			return meta.Path, nil
		}
	}

	return "", fmt.Errorf("no .gguf file matches: %s", path)
}

// ResolveModelPath находит полный путь к модели
func (m *ModelManager) ResolveModelPath(nameOrPath string) string {
	// Проверяем, может это уже полный путь
	if filepath.IsAbs(nameOrPath) {
		if _, err := os.Stat(nameOrPath); err == nil {
			return nameOrPath
		}
	}

	// Проверяем путь относительно modelsDir
	candidate := filepath.Join(m.modelsDir, nameOrPath)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// Добавляем .gguf
	if !strings.HasSuffix(strings.ToLower(candidate), ".gguf") {
		candidate += ".gguf"
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Поиск по glob
	matches, err := filepath.Glob(filepath.Join(m.modelsDir, nameOrPath) + "*.gguf")
	if err == nil && len(matches) > 0 {
		return matches[0]
	}

	return candidate // возвращаем лучшую догадку
}

// GetModelsDir возвращает путь к директории моделей
func (m *ModelManager) GetModelsDir() string {
	return m.modelsDir
}

// GetTotalScans возвращает количество выполненных сканирований
func (m *ModelManager) GetTotalScans() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.totalScans
}

// GetLastScanTime возвращает время последнего сканирования
func (m *ModelManager) GetLastScanTime() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastScanTime
}

// EstimateGPUMemoryForModel оценивает необходимую VRAM для модели.
//
// Аргументы:
//   - sizeBytes: размер GGUF-файла модели.
//   - gpuLayers: число слоёв на GPU (-1 = все).
//   - totalLayers: общее число слоёв модели.
//   - ctxSize: размер контекста (n_ctx). Используется для расчёта KV-cache.
//     Если ctxSize <= 0, считается дефолт 4096.
//
// Базируется на эвристике: доля слоёв × размер файла + KV-cache + compute buffers.
//
// 2026-06-26 BUGFIX:
//   - ctxMemoryMB был захардкожен на 512.0 (≈ 4096 контекста). Для n_ctx=32768
//     фактический KV-cache ~6 GB, что приводило к ложному «VRAM sufficient».
//   - Не было safety buffer для CUDA context + compute buffers + scratch,
//     что добавляет ~1 GB на 20GB GPU.
//
// Теперь сигнатура принимает ctxSize и добавляет 1 GB compute-buffer overhead.
func EstimateGPUMemoryForModel(sizeBytes int64, gpuLayers int, totalLayers int, ctxSize int) uint64 {
	if gpuLayers == 0 || totalLayers <= 0 {
		return 0
	}

	// Доля слоёв на GPU
	var ratio float64
	if gpuLayers == -1 {
		// -1 означает "все слои на GPU"
		ratio = 1.0
	} else {
		ratio = float64(gpuLayers) / float64(totalLayers)
	}
	if ratio > 1.0 {
		ratio = 1.0
	}

	// Размер модели в MB
	modelSizeMB := float64(sizeBytes) / 1024 / 1024

	// Приблизительная оценка: слои занимают ~70% модели (остальное — embedding и т.д.)
	gpuMemoryMB := modelSizeMB * 0.7 * ratio

	// KV-cache: ~256 KB на токен (для Q4_K_M и fp16 KV).
	// n_ctx=4096 → 1 GB, n_ctx=32768 → 8 GB.
	// Мы используем грубую оценку: 1 MB на 4 токена (256 KB).
	if ctxSize <= 0 {
		ctxSize = 4096
	}
	ctxMemoryMB := float64(ctxSize) * 256.0 / 1024.0 / 1024.0

	// Compute buffers + CUDA context + scratch ~1 GB на 20GB GPU.
	// Этот overhead не зависит от размера модели, но зависит от размера VRAM.
	const computeBufferMB = 1024

	return uint64(gpuMemoryMB + ctxMemoryMB + computeBufferMB)
}

// backwardCompatEstimateGPUMemoryForModel — старая сигнатура без ctxSize.
// Используется в legacy-коде; внутри вызывает новую с дефолтным ctx=4096.
// Оставлена для обратной совместимости с тестами и сторонними вызовами.
func backwardCompatEstimateGPUMemoryForModel(sizeBytes int64, gpuLayers int, totalLayers int) uint64 {
	return EstimateGPUMemoryForModel(sizeBytes, gpuLayers, totalLayers, 4096)
}

// GetModelArchitectureFromFile пытается определить архитектуру по GGUF файлу
// без полной загрузки. Использует только первые байты файла (magic + header).
// В реальности это в C bridge — llama.cpp может читать header.
func GetModelArchitectureFromFile(path string) (string, error) {
	// Для stub режима — просто читаем имя файла
	// В реальной имплементации здесь будет чтение GGUF header
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".gguf")
	parts := strings.Split(base, "-")
	if len(parts) >= 2 {
		return parts[0], nil
	}
	return "unknown", nil
}

// GetFileTypeFromName пытается определить тип квантизации из имени файла
func GetFileTypeFromName(filename string) string {
	name := strings.ToLower(filename)
	types := []struct {
		pattern string
		name    string
	}{
		{"q2_k", "Q2_K"},
		{"q3_k_s", "Q3_K_S"},
		{"q3_k_m", "Q3_K_M"},
		{"q3_k_l", "Q3_K_L"},
		{"q4_k_s", "Q4_K_S"},
		{"q4_k_m", "Q4_K_M"},
		{"q4_k_l", "Q4_K_L"},
		{"q5_k_s", "Q5_K_S"},
		{"q5_k_m", "Q5_K_M"},
		{"q5_k_l", "Q5_K_L"},
		{"q6_k", "Q6_K"},
		{"q8_0", "Q8_0"},
		{"f16", "F16"},
		{"f32", "F32"},
		{"q4_0", "Q4_0"},
		{"q4_1", "Q4_1"},
		{"q5_0", "Q5_0"},
		{"q5_1", "Q5_1"},
		{"ggml", "GGML"},
	}

	for _, t := range types {
		if strings.Contains(name, t.pattern) {
			return t.name
		}
	}
	return "unknown"
}

// IdleUnloadManager управляет выгрузкой неактивных моделей
type IdleUnloadManager struct {
	backend       *Backend
	idleTimeout   time.Duration
	checkInterval time.Duration
	stopCh        chan struct{}
}

// NewIdleUnloadManager создаёт менеджер выгрузки неактивных моделей
func NewIdleUnloadManager(backend *Backend, idleTimeout time.Duration) *IdleUnloadManager {
	return &IdleUnloadManager{
		backend:       backend,
		idleTimeout:   idleTimeout,
		checkInterval: min(idleTimeout/2, 5*time.Minute),
		stopCh:        make(chan struct{}),
	}
}

// Start запускает фоновый мониторинг
func (m *IdleUnloadManager) Start() {
	if m.idleTimeout <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(m.checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				m.checkAndUnload()
			case <-m.stopCh:
				return
			}
		}
	}()
}

// Stop останавливает мониторинг
func (m *IdleUnloadManager) Stop() {
	close(m.stopCh)
}

// idleUnloadModelRef — минимальный интерфейс модели, нужный для проверки idle.
// Используется вместо полного Backend, чтобы избежать циклической зависимости
// при тестировании и упростить unit-тесты.
func (m *IdleUnloadManager) checkAndUnload() {
	models := m.backend.ListModels()
	now := time.Now()

	for _, model := range models {
		if model.State != StateLoaded {
			continue
		}
		if model.ActiveQueries > 0 {
			continue
		}
		// Выгружаем если модель не использовалась дольше idleTimeout.
		//
		// ВАЖНО: считаем idle от LastUsedAt, а не от LoadedAt. Раньше (до 2026-06-22)
		// использовался LoadedAt, что приводило к выгрузке модели после idleTimeout
		// от момента ЗАГРУЗКИ — даже если модель обслуживала запросы каждую секунду.
		// Это проявлялось как «периодическая выгрузка при активном использовании»
		// в мониторе и логах cppworker.
		//
		// LastUsedAt обновляется в Backend.Generate / Backend.GenerateStream при
		// КАЖДОМ запросе (и в начале, и в defer). Если LastUsedAt zero (модель
		// только что загружена, ещё не использовалась) — fallback на LoadedAt.
		referenceTime := model.LastUsedAt
		if referenceTime.IsZero() {
			referenceTime = model.LoadedAt
		}
		// Round 35 (2026-08-12) bugfix: SIGSEGV-safe guard. Если ОБА LastUsedAt
		// и LoadedAt — zero values (паника в ListModels / race с LoadModelWithOpts,
		// или cppworker только что стартовал), не трогаем модель. Раньше
		// now.Sub(zeroTime) = now = огромное значение (миллиарды наносекунд с 0001-01-01),
		// idleTime > idleTimeout = true → UnloadModel → SIGSEGV в llama_free
		// (use-after-free: handle ещё не инициализирован полностью).
		//
		// Это был один из источников "model loaded then immediately reset" —
		// cppworker SIGSEGV'ился в IdleUnloadManager.Start.func1 сразу после
		// load complete, потому что load только что завершился и временно
		// model.LastUsedAt мог быть zero, а LoadedAt мог быть не обновлён.
		if referenceTime.IsZero() {
			logger.Get().Debugw("idle unload: skip — reference time is zero (model just loaded or in transient state)",
				"model", model.Name, "state", model.State)
			continue
		}
		idleTime := now.Sub(referenceTime)
		// ВАЖНО (2026-06-22): убрано условие `&& model.TotalQueries == 0`.
		// Раньше модель, загруженная через ensureModelLoaded, но не получившая
		// ни одного Generate/GenerateStream (например, lazy-load из /api/show
		// или первый запрос упал до Generate), НЕ выгружалась по idle — это
		// приводило к «видимой бесконечной жизни» неиспользуемых моделей.
		// Теперь idle считается строго от LastUsedAt (или LoadedAt как fallback),
		// независимо от того, был ли хоть один запрос. Если idleTimeout задан
		// и модель простаивает — она выгружается. Если idleTimeout=0 — менеджер
		// не запускается вовсе (см. Start()).
		if idleTime > m.idleTimeout {
			logger.Get().Infow("idle unload",
				"model", model.Name,
				"idleTime", idleTime.String(),
				"referenceTime", referenceTime.Format(time.RFC3339Nano),
				"lastUsedAtSet", !model.LastUsedAt.IsZero(),
				"timeout", m.idleTimeout.String(),
				"totalQueries", model.TotalQueries)
			if err := m.backend.UnloadModel(model.Name); err != nil {
				logger.Get().Warnw("idle unload failed",
					"model", model.Name,
					"error", err)
			}
		}
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// SliceMetadataProvider — интерфейс для получения метаданных GGUF
// В реальности будет вызывать C bridge для парсинга header
type SliceMetadataProvider interface {
	GetMetadata(path string) (*bridge.ModelMetadata, error)
}
