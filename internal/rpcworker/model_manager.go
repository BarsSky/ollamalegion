package rpcworker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// LoadedSlice — информация о загруженном срезе модели.
type LoadedSlice struct {
	ModelName  string    `json:"modelName"`
	ModelPath  string    `json:"modelPath"`
	Layers     string    `json:"layers"`     // "1-40"
	StartLayer int       `json:"startLayer"`
	EndLayer   int       `json:"endLayer"`
	LoadedAt   time.Time `json:"loadedAt"`
	LoadMs     int64     `json:"loadMs"`
	Handle     *bridge.ModelHandle `json:"-"`
}

// ModelManager — управление загруженными срезами моделей на worker'е.
//
// B1: in-memory storage с реальной llama.cpp интеграцией через c/bridge.
// На stub build tag — `bridge.LoadModel` всегда возвращает заглушку,
// так что manager работает без реальной модели (полезно для e2e-тестов).
type ModelManager struct {
	mu       sync.RWMutex
	slices   map[string]*LoadedSlice // modelName → slice
	cfg      WorkerConfig
}

// NewModelManager создаёт новый менеджер срезов.
func NewModelManager(cfg WorkerConfig) *ModelManager {
	return &ModelManager{
		slices: make(map[string]*LoadedSlice),
		cfg:    cfg,
	}
}

// LoadSlice загружает срез модели с диска.
//
// Параметры:
//   - modelName: имя модели (например, "llama-2-7b.Q4_K_M.gguf" или "llama-2-7b")
//   - layers: диапазон слоёв, которые worker обслуживает (например, "1-40")
//
// Алгоритм:
//  1. Резолвим имя в путь через modelsDir.
//  2. Парсим layers в start/end (если задано в запросе — переопределяем cfg.SliceLayers).
//  3. Вызываем bridge.LoadModel.
//  4. Сохраняем в map. Если модель уже загружена — UnloadSlice + LoadSlice (reload).
func (m *ModelManager) LoadSlice(modelName, layers string) (*LoadedSlice, error) {
	if modelName == "" {
		return nil, errors.New("model_name is required")
	}

	start := time.Now()

	modelPath, err := m.resolveModelPath(modelName)
	if err != nil {
		return nil, err
	}

	startLayer, endLayer, slice, err := parseLayersOverride(layers, m.cfg.StartLayer, m.cfg.EndLayer)
	if err != nil {
		return nil, fmt.Errorf("invalid layers: %w", err)
	}

	// Если уже загружена — выгружаем (reload semantics)
	m.mu.Lock()
	if existing, ok := m.slices[modelName]; ok {
		if existing.Handle != nil {
			existing.Handle.FreeModel()
		}
		delete(m.slices, modelName)
	}
	m.mu.Unlock()

	// bridge.LoadModel (stub в llama_stub режиме возвращает handle без загрузки).
	bridgeCfg := bridge.DefaultModelConfig(modelPath)
	bridgeCfg.NContext = 4096
	handle, err := bridge.LoadModel(bridgeCfg)
	if err != nil {
		return nil, fmt.Errorf("bridge.LoadModel failed: %w", err)
	}

	loaded := &LoadedSlice{
		ModelName:  modelName,
		ModelPath:  modelPath,
		Layers:     slice,
		StartLayer: startLayer,
		EndLayer:   endLayer,
		LoadedAt:   time.Now(),
		LoadMs:     time.Since(start).Milliseconds(),
		Handle:     handle,
	}

	m.mu.Lock()
	m.slices[modelName] = loaded
	m.mu.Unlock()

	return loaded, nil
}

// UnloadSlice выгружает срез. Идемпотентно: если модель не загружена — OK.
func (m *ModelManager) UnloadSlice(modelName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.slices[modelName]
	if !ok {
		return nil // идемпотентный OK
	}
	if existing.Handle != nil {
		existing.Handle.FreeModel()
	}
	delete(m.slices, modelName)
	return nil
}

// GetSlice возвращает загруженный срез или nil.
func (m *ModelManager) GetSlice(modelName string) *LoadedSlice {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.slices[modelName]
}

// ListSlices возвращает список загруженных срезов (без internal handle).
func (m *ModelManager) ListSlices() []*LoadedSlice {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*LoadedSlice, 0, len(m.slices))
	for _, s := range m.slices {
		copy := *s
		copy.Handle = nil
		out = append(out, &copy)
	}
	return out
}

// Count возвращает количество загруженных срезов.
func (m *ModelManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.slices)
}

// ModelsDir возвращает путь к директории моделей (для тестов и хелперов).
func (m *ModelManager) ModelsDir() string {
	return m.cfg.ModelsDir
}

// resolveModelPath находит путь к .gguf-файлу по имени.
//
// Поддерживает:
//   - точное имя файла "model.gguf"
//   - имя без расширения "model" → ищем "model.gguf"
//   - абсолютный путь
func (m *ModelManager) resolveModelPath(modelName string) (string, error) {
	if modelName == "" {
		return "", errors.New("model_name is required")
	}

	// Абсолютный путь или содержит разделитель каталогов.
	if filepath.IsAbs(modelName) || strings.ContainsAny(modelName, "/\\") {
		if _, err := os.Stat(modelName); err != nil {
			return "", fmt.Errorf("model file not found: %w", err)
		}
		return modelName, nil
	}

	// Поиск в modelsDir.
	candidates := []string{modelName}
	if !strings.HasSuffix(strings.ToLower(modelName), ".gguf") {
		candidates = append(candidates, modelName+".gguf")
	}

	for _, c := range candidates {
		full := filepath.Join(m.cfg.ModelsDir, c)
		if _, err := os.Stat(full); err == nil {
			return full, nil
		}
	}
	return "", fmt.Errorf("model %q not found in %s (tried: %v)",
		modelName, m.cfg.ModelsDir, candidates)
}

// parseLayersOverride — парсит строку "start-end" с учётом fallback на defaults.
func parseLayersOverride(layers string, defaultStart, defaultEnd int) (int, int, string, error) {
	layers = strings.TrimSpace(layers)
	if layers == "" {
		return defaultStart, defaultEnd, fmt.Sprintf("%d-%d", defaultStart, defaultEnd), nil
	}
	parts := strings.Split(layers, "-")
	if len(parts) != 2 {
		return 0, 0, "", fmt.Errorf("expected format \"start-end\", got %q", layers)
	}
	start, err := parsePositiveInt(parts[0])
	if err != nil {
		return 0, 0, "", fmt.Errorf("invalid start layer: %w", err)
	}
	end, err := parsePositiveInt(parts[1])
	if err != nil {
		return 0, 0, "", fmt.Errorf("invalid end layer: %w", err)
	}
	if end <= start {
		return 0, 0, "", fmt.Errorf("end layer (%d) must be > start (%d)", end, start)
	}
	return start, end, layers, nil
}

func parsePositiveInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a positive integer: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return n, nil
}