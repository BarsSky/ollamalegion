package cppbackend

import (
	"fmt"
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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

	// R83 (2026-09-25): последний шаг — варианты внешнего имени (Ollama-тег и
	// регистр). Живой кейс: клиент просит "qwen3.8:latest", файл на диске —
	// "Qwen3.8-27B-UD-Q4_K_M.gguf". Contains-поиск выше нашёл бы "qwen3.8", но
	// тег ":latest" его ломал, и cppworker уходил открывать несуществующий
	// "models/qwen3.8:latest.gguf" → 500 от llama.cpp вместо внятного 404.
	if foundPath, _, ok := m.findModelByVariantsLocked(path); ok {
		return foundPath, nil
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
	backend idleUnloadBackend

	// R119 (2026-10-15): таймаут и интервал проверки меняются НА ХОДУ — оператор
	// задаёт idleUnloadMinutes из WebUI (PUT /api/v1/cppworker/config/update), и
	// значение обязано дойти до живого менеджера. Раньше оба поля записывались
	// один раз в конструкторе, поэтому правка в UI меняла только JSON-конфиг:
	// UI показывал 3 минуты, а выгрузка по-прежнему происходила через 10.
	mu            sync.RWMutex
	idleTimeout   time.Duration
	checkInterval time.Duration

	stopCh   chan struct{}
	stopOnce sync.Once
	running  bool
}

// idleUnloadBackend — минимум, нужный менеджеру автовыгрузки: список моделей с их
// состоянием/активностью и выгрузка. Интерфейс (а не конкретный *Backend) —
// чтобы инвариант «не выгружать модель во время генерации» проверялся
// детерминированным юнит-тестом: живьём генерацию дольше таймаута простоя
// воспроизвести быстро нельзя (стенд отдаёт 300 токенов за 10 c), а цена ошибки —
// оборванный ответ клиенту.
type idleUnloadBackend interface {
	ListModels() []ModelInfo
	UnloadModel(name string) error
}

// NewIdleUnloadManager создаёт менеджер выгрузки неактивных моделей
func NewIdleUnloadManager(backend *Backend, idleTimeout time.Duration) *IdleUnloadManager {
	return &IdleUnloadManager{
		backend:       backend,
		idleTimeout:   idleTimeout,
		checkInterval: idleCheckIntervalFor(idleTimeout),
		stopCh:        make(chan struct{}),
	}
}

// newIdleUnloadManagerFor — конструктор для тестов: принимает любой источник
// моделей (см. idleUnloadBackend).
func newIdleUnloadManagerFor(backend idleUnloadBackend, idleTimeout time.Duration) *IdleUnloadManager {
	return &IdleUnloadManager{
		backend:       backend,
		idleTimeout:   idleTimeout,
		checkInterval: idleCheckIntervalFor(idleTimeout),
		stopCh:        make(chan struct{}),
	}
}

// idleCheckIntervalFor — как часто проверять простой: половина таймаута, но не
// чаще 5 минут (для больших таймаутов) и не реже 1 секунды (для тестов и
// маленьких значений).
func idleCheckIntervalFor(idleTimeout time.Duration) time.Duration {
	interval := idleTimeout / 2
	if interval > 5*time.Minute {
		interval = 5 * time.Minute
	}
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

// SetIdleTimeout — R119: применить новый таймаут к ЖИВОМУ менеджеру.
//
// Тикер пересоздаётся с новым интервалом: старый тикер продолжает тикать по
// прежнему расписанию, и без перезапуска изменение вступало бы в силу только
// после следующего (старого) срабатывания — то есть «через 5 минут» вместо
// «сразу». d <= 0 игнорируется: выключение делает Backend.SetIdleUnloadTimeout.
func (m *IdleUnloadManager) SetIdleTimeout(d time.Duration) {
	if m == nil || d <= 0 {
		return
	}
	m.mu.Lock()
	m.idleTimeout = d
	m.checkInterval = idleCheckIntervalFor(d)
	m.mu.Unlock()
}

// IdleTimeout — действующий таймаут (для диагностики и тестов).
func (m *IdleUnloadManager) IdleTimeout() time.Duration {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.idleTimeout
}

// CheckInterval — действующий интервал проверки (для диагностики и тестов).
func (m *IdleUnloadManager) CheckInterval() time.Duration {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.checkInterval
}

// Running — запущен ли фоновый цикл.
func (m *IdleUnloadManager) Running() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}

// Start запускает фоновый мониторинг. Идемпотентен: повторный вызов не создаёт
// второй цикл (иначе две горутины выгружали бы модели параллельно).
func (m *IdleUnloadManager) Start() {
	m.mu.Lock()
	if m.running || m.idleTimeout <= 0 {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()

	go func() {
		for {
			m.mu.RLock()
			interval := m.checkInterval
			m.mu.RUnlock()

			timer := time.NewTimer(interval)
			select {
			case <-timer.C:
				m.checkAndUnload()
			case <-m.stopCh:
				timer.Stop()
				m.mu.Lock()
				m.running = false
				m.mu.Unlock()
				return
			}
		}
	}()
}

// Stop останавливает мониторинг. Идемпотентен (stopOnce): повторный вызов не
// паникует на закрытии уже закрытого канала.
func (m *IdleUnloadManager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
}

// idleUnloadModelRef — минимальный интерфейс модели, нужный для проверки idle.
// Используется вместо полного Backend, чтобы избежать циклической зависимости
// при тестировании и упростить unit-тесты.
func (m *IdleUnloadManager) checkAndUnload() {
	models := m.backend.ListModels()
	now := time.Now()

	m.mu.RLock()
	idleTimeout := m.idleTimeout
	m.mu.RUnlock()
	if idleTimeout <= 0 {
		return
	}

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
		if idleTime > idleTimeout {
			logger.Get().Infow("idle unload",
				"model", model.Name,
				"idleTime", idleTime.String(),
				"referenceTime", referenceTime.Format(time.RFC3339Nano),
				"lastUsedAtSet", !model.LastUsedAt.IsZero(),
				"timeout", idleTimeout.String(),
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
