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
	Filename     string `json:"filename"`
	Path         string `json:"path"`
	SizeBytes    int64  `json:"sizeBytes"`
	ModifiedAt   time.Time `json:"modifiedAt"`
	Architecture string `json:"architecture,omitempty"` // из GGUF header
	FileType     string `json:"fileType,omitempty"`     // Q4_K_M, Q5_K_M, F16, etc.
}

// ModelManager — управляет модельками
type ModelManager struct {
	modelsDir string
	config    Config

	mu        sync.RWMutex
	ggufFiles map[string]*GGUFModelMeta // filename → meta

	// Статистика
	totalScans      int64
	lastScanTime    time.Time
	scanDuration    time.Duration
}

// NewModelManager создаёт новый ModelManager
func NewModelManager(modelsDir string, cfg Config) *ModelManager {
	return &ModelManager{
		modelsDir: modelsDir,
		config:    cfg,
		ggufFiles: make(map[string]*GGUFModelMeta),
	}
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

// GetModelMeta возвращает метаданные конкретного GGUF файла
func (m *ModelManager) GetModelMeta(filename string) (*GGUFModelMeta, error) {
	m.mu.RLock()
	meta, exists := m.ggufFiles[filename]
	m.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("model %s not found in models directory", filename)
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

// EstimateGPUMemoryForModel оценивает необходимую VRAM для модели
// Основано на эвристике: параметры модели × тип квантизации
func EstimateGPUMemoryForModel(sizeBytes int64, gpuLayers int, totalLayers int) uint64 {
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

	// Добавляем контекст: ~1MB на 1K контекста
	ctxMemoryMB := 512.0 // пример для 4096 контекста

	return uint64(gpuMemoryMB + ctxMemoryMB)
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
	backend      *Backend
	idleTimeout  time.Duration
	checkInterval time.Duration
	stopCh       chan struct{}
}

// NewIdleUnloadManager создаёт менеджер выгрузки неактивных моделей
func NewIdleUnloadManager(backend *Backend, idleTimeout time.Duration) *IdleUnloadManager {
	return &IdleUnloadManager{
		backend:      backend,
		idleTimeout:  idleTimeout,
		checkInterval: min(idleTimeout/2, 5*time.Minute),
		stopCh:       make(chan struct{}),
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
		// Выгружаем если модель не использовалась дольше idleTimeout
		idleTime := now.Sub(model.LoadedAt)
		if idleTime > m.idleTimeout && model.TotalQueries == 0 {
			logger.Get().Infow("idle unload",
				"model", model.Name,
				"idleTime", idleTime.String(),
				"timeout", m.idleTimeout.String())
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
