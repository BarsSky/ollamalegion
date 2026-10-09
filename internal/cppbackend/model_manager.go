// Package cppbackend — управление GGUF моделями
//
// ModelManager отвечает за:
// - Сканирование директории моделей и обнаружение .gguf файлов
// - Кэширование метаданных моделей (из GGUF header)
// - Автоматическую выгрузку неактивных моделей (idle unload)
// - Загрузку моделей по требованию (lazy load)
package cppbackend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// GGUFAlias — алиас модели, созданный Ollama-совместимым POST /api/create.
//
// R66d (2026-09-23): лежит рядом с моделями как <name>.gguf.json и указывает на
// реальный .gguf через поле source. До этой правки алиасы не попадали ни в
// ListModels/ListAliases, ни в /api/tags, ни в resolveModelPath — созданная
// «модель» была невидимой и незагружаемой.
type GGUFAlias struct {
	// Порядок полей — по требованию govet fieldalignment (govet.enable-all):
	// сначала поля с указателями (time.Time хранит *Location), затем строки,
	// затем числовые скаляры.
	CreatedAt       time.Time `json:"createdAt,omitempty"`
	Name            string    `json:"name"`       // имя модели для клиента
	Source          string    `json:"source"`     // как записано в modelfile (обычно <file>.gguf)
	SourcePath      string    `json:"sourcePath"` // абсолютный путь к .gguf
	Modelfile       string    `json:"modelfile,omitempty"`
	AliasPath       string    `json:"aliasPath"`       // путь к самому .gguf.json
	SourceSizeBytes int64     `json:"sourceSizeBytes"` // размер источника (для /api/tags)
	SourceExists    bool      `json:"sourceExists"`    // файл-источник на месте?
}

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

	// R83 (2026-09-25): параметры реального KV-кэша. Без них KV считался как
	// «2 × block_count × kv_heads × (n_embd/n_heads)», что для гибридных моделей
	// завышает его в разы (у Qwen3.8-27B KV хранят 17 слоёв из 65, head_dim 256,
	// а не 213). Правило — kv_layers.go, методы KVLayers()/KVHeadDim().
	KeyLength             int  `json:"keyLength,omitempty"`
	ValueLength           int  `json:"valueLength,omitempty"`
	NextNPredictLayers    int  `json:"nextnPredictLayers,omitempty"`
	FullAttentionInterval int  `json:"fullAttentionInterval,omitempty"`
	HasRecurrentLayersKey bool `json:"hasRecurrentLayersKey,omitempty"`

	// R83 (2026-10-01): KV-sharing и скользящее окно (gemma4/gemma3n).
	// Без них KV считался как «все слои × n_ctx»: у gemma-4-E4B это 3.17 GB
	// против реальных ~0.3 GB, из-за чего memfit отказывал в полном оффлоаде.
	// Раскладка — kv_layers.go (KVPlan).
	SharedKVLayers int    `json:"sharedKvLayers,omitempty"`
	SlidingWindow  int    `json:"slidingWindow,omitempty"`
	SWAKeyLength   int    `json:"swaKeyLength,omitempty"`
	SWAPattern     string `json:"swaPattern,omitempty"`
}

// ModelManager — управляет модельками
type ModelManager struct {
	modelsDir string
	config    Config

	mu        sync.RWMutex
	ggufFiles map[string]*GGUFModelMeta // filename → meta

	// R66d (2026-09-23): алиасы моделей (<name>.gguf.json), заполняются в
	// ScanModels вместе с ggufFiles. Ключ — имя алиаса (то, что видит клиент).
	aliases map[string]GGUFAlias

	// Round 22 (2026-08-03): history успешных load-операций (name → path).
	// Нужно для resolveModelPath после idle-unload: имя, под которым модель
	// загружалась (например "qwen3-4b"), больше не в ggufFiles (т.к. это alias,
	// не filename), но мы ЗНАЕМ что она соответствует файлу
	// Qwen3-Instruct-2507-q4km.gguf — потому что загружали её с этим path.
	// Без этой истории: auto-pick single .gguf требует len==1, не работает
	// когда в директории 2+ файла (Round 22 BUG #2).
	nameHistory map[string]string // name → resolved file path

	// R65d (2026-09-20): сериализация персистенции nameHistory.
	// См. RecordModelLoad — без этого параллельные записи затирали друг друга,
	// а незавершённая запись ломала удаление каталога модели в тестах.
	persistMu      sync.Mutex
	persistDirty   bool
	persistRunning bool

	// Статистика
	totalScans   int64
	lastScanTime time.Time
	scanDuration time.Duration

	// R93 (2026-10-09): живой инвентарь моделей.
	//
	// ЖАЛОБА: «надо почистить WebUI от моделей, что не лежат на диске, и отработать
	// механизм, который проверяет наличие и фиксирует изменения без перезапуска
	// контейнера». Живой случай: каталог моделей был bind-mount'ом
	// (D:\ollama-legion-models → /app/models), каталог удалили, а /api/models/files
	// продолжал отдавать 3 файла — потому что список лежал в кэше, а ScanModels
	// вызывался только при старте и после import/share. Ошибка чтения каталога
	// вообще оставляла старый кэш нетронутым (см. recordDirFailure).
	rescanTTL    time.Duration // 0 = фоновой проверки нет (только явный ScanModels)
	lastChangeAt time.Time
	lastAdded    []string
	lastRemoved  []string
	lastResized  []string
	dirError     string
	dirErrorAt   time.Time
	changeCount  int64
}

// DefaultModelRescanSec — период фоновой проверки каталога моделей.
//
// 30 с: достаточно, чтобы удаление/добавление файла (в т.ч. через WebUI, scp,
// docker cp) появилось без перезапуска контейнера, и при этом скан каталога с
// десятками GGUF не создаёт заметной нагрузки (os.ReadDir + Stat).
const DefaultModelRescanSec = 30

// EnvModelRescanSec — переменная окружения с периодом фоновой проверки (0 = выкл).
const EnvModelRescanSec = "CPPWORKER_MODELS_RESCAN_SEC"

// modelRescanTTLFromEnv разбирает CPPWORKER_MODELS_RESCAN_SEC.
// Пусто/мусор → DefaultModelRescanSec; 0 → фоновая проверка выключена;
// отрицательное → тоже выключена (явный request-time скан остаётся).
func modelRescanTTLFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv(EnvModelRescanSec))
	if v == "" {
		return DefaultModelRescanSec * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		logger.Get().Warnw("ModelManager: invalid "+EnvModelRescanSec+", using default",
			"value", v, "default_sec", DefaultModelRescanSec)
		return DefaultModelRescanSec * time.Second
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// ModelInventoryStatus — снимок состояния каталога моделей для API/WebUI.
//
// Нужен, чтобы оператор видел не только список, но и КОГДА его проверяли и что
// изменилось (иначе «модель исчезла из WebUI» невозможно отличить от «скан не
// проходил»).
type ModelInventoryStatus struct {
	ScannedAt    time.Time `json:"scannedAt"`
	ChangedAt    time.Time `json:"changedAt,omitempty"`
	DirErrorAt   time.Time `json:"dirErrorAt,omitempty"`
	Dir          string    `json:"dir"`
	DirError     string    `json:"dirError,omitempty"`
	Added        []string  `json:"added,omitempty"`
	Removed      []string  `json:"removed,omitempty"`
	Resized      []string  `json:"resized,omitempty"`
	Count        int       `json:"count"`
	Scans        int64     `json:"scans"`
	Changes      int64     `json:"changes"`
	RescanTTLSec int       `json:"rescanTtlSec"`
}

// NewModelManager создаёт новый ModelManager
func NewModelManager(modelsDir string, cfg Config) *ModelManager {
	m := &ModelManager{
		modelsDir:   modelsDir,
		config:      cfg,
		ggufFiles:   make(map[string]*GGUFModelMeta),
		aliases:     make(map[string]GGUFAlias),
		nameHistory: make(map[string]string),
		rescanTTL:   modelRescanTTLFromEnv(),
	}
	// R60.61 (2026-09-14): restore persisted nameHistory (alias → path map).
	// До этого фикса history терялся при рестарте → OpenWebUI с закешированным
	// алиасом (например "qwen3-instruct" при файле "Qwen3-Instruct-2507-q4km.gguf")
	// получал 503 до первой успешной загрузки с тем же алиасом ("курица и яйцо").
	if err := m.loadNameHistory(); err != nil && !os.IsNotExist(err) {
		logger.Get().Warnw("ModelManager: failed to load persisted nameHistory",
			"path", m.nameHistoryPath(), "error", err)
	}
	return m
}

// nameHistoryPath возвращает путь к persisted файлу nameHistory.
// По умолчанию — рядом с gguf файлами в modelsDir (упрощает cleanup
// при тестах с TempDir и удалении всей директории). В проде —
// /app/models/.name_history.json.
func (m *ModelManager) nameHistoryPath() string {
	return filepath.Join(m.modelsDir, ".name_history.json")
}

// loadNameHistory восстанавливает nameHistory из persisted файла.
// Вызывается из NewModelManager; os.IsNotExist — норма (первый запуск).
func (m *ModelManager) loadNameHistory() error {
	f := m.nameHistoryPath()
	data, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := json.Unmarshal(data, &m.nameHistory); err != nil {
		return fmt.Errorf("nameHistory: invalid JSON: %w", err)
	}
	logger.Get().Infow("ModelManager: restored persisted nameHistory",
		"path", f, "entries", len(m.nameHistory))
	return nil
}

// saveNameHistory персистит nameHistory в файл. Вызывается из RecordModelLoad.
// Best-effort: ошибка записи → warn-лог, но load не блокируется.
func (m *ModelManager) saveNameHistory() {
	f := m.nameHistoryPath()
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		logger.Get().Warnw("ModelManager: failed to mkdir for nameHistory",
			"path", filepath.Dir(f), "error", err)
		return
	}
	m.mu.RLock()
	data, err := json.MarshalIndent(m.nameHistory, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		logger.Get().Warnw("ModelManager: failed to marshal nameHistory",
			"error", err)
		return
	}
	if err := os.WriteFile(f, data, 0o644); err != nil {
		logger.Get().Warnw("ModelManager: failed to write nameHistory",
			"path", f, "error", err)
	}
}

// RecordModelLoad — записывает успешный load (name → path) в nameHistory.
// Вызывается из Backend.LoadModel после успешной загрузки модели в VRAM,
// чтобы resolveModelPath мог найти alias после idle-unload.
//
// Round 22 (2026-08-03).
//
// R60.61 (2026-09-14): persist в /app/data/name_history.json чтобы history
// переживал рестарт.
//
// R65d (2026-09-20) — ИСПРАВЛЕНИЕ ГОНКИ.
//
// Было: `go m.saveNameHistory()` на каждый вызов. Два последствия:
//
//  1. ПОТЕРЯ ДАННЫХ. Две параллельные записи читают nameHistory и пишут файл
//     независимо; побеждает последняя завершившаяся. Загрузка двух моделей
//     подряд (обычный warmup) могла сохранить только один alias. После
//     рестарта второй alias терялся, и OpenWebUI/Cline, присылающие имя без
//     расширения (R60.61 кейс), получали "модель не найдена".
//
//  2. ФЛАКИ ТЕСТОВ. Незавершённая запись держала файл в каталоге модели, а
//     t.TempDir() cleanup падал с "directory is not empty" (Windows). Именно
//     так проявлялся баг в TestR60_57_LoadModelWithOpts_*.
//
// Теперь записи сериализованы: dirty-флаг + один writer-goroutine, который
// перечитывает актуальное состояние под блокировкой. Это coalescing —
// N вызовов RecordModelLoad подряд дают минимум записей без потери данных.
func (m *ModelManager) RecordModelLoad(name, path string) {
	if name == "" || path == "" {
		return
	}
	m.mu.Lock()
	m.nameHistory[name] = path
	m.mu.Unlock()

	m.persistMu.Lock()
	m.persistDirty = true
	if !m.persistRunning {
		m.persistRunning = true
		go m.persistLoop()
	}
	m.persistMu.Unlock()

	logger.Get().Infow("ModelManager.RecordModelLoad: recorded",
		"name", name, "path", path)
}

// persistLoop — единственный writer nameHistory. Работает пока есть dirty-флаг,
// затем завершается (не держит горутину постоянно).
func (m *ModelManager) persistLoop() {
	for {
		m.persistMu.Lock()
		if !m.persistDirty {
			m.persistRunning = false
			m.persistMu.Unlock()
			return
		}
		m.persistDirty = false
		m.persistMu.Unlock()

		// saveNameHistory берёт m.mu.RLock на время маршалинга, поэтому
		// конкурентный RecordModelLoad безопасен и не теряется: он поднимет
		// dirty снова, и цикл выполнит ещё одну итерацию.
		m.saveNameHistory()
	}
}

// WaitForPendingWrites блокирует до завершения всех незавершённых записей
// nameHistory. Нужен для детерминированного teardown: без ожидания фоновый
// writer может дописать файл ПОСЛЕ удаления каталога модели.
//
// Возвращает, когда персистенция завершена или истёк timeout.
func (m *ModelManager) WaitForPendingWrites(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		m.persistMu.Lock()
		running := m.persistRunning
		m.persistMu.Unlock()
		if !running {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
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

// SingleGGUFPath возвращает путь к единственному .gguf файлу в modelsDir,
// если он один, и ""+false в противном случае.
//
// R60.61 (2026-09-14): defensive fallback для случая когда OpenWebUI
// (или другой клиент) присылает имя, отличное от filename на диске.
// Пример: на диске только Qwen3-Instruct-2507-q4km.gguf, а OpenWebUI
// шлёт "qwen3-instruct" (закэшированный алиас). FindModelByPath не находит,
// но мы знаем что других вариантов нет → используем единственный файл.
//
// Используется в lazyload.go как fallback после FindModelByPath failure.
func (m *ModelManager) SingleGGUFPath() (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var only string
	for _, meta := range m.ggufFiles {
		if only != "" {
			return "", false // 2+ файлов — неоднозначно
		}
		only = meta.Path
	}
	if only == "" {
		return "", false
	}
	if _, err := os.Stat(only); err != nil {
		return "", false
	}
	return only, true
}

// ScanModels сканирует директорию на предмет .gguf файлов
func (m *ModelManager) ScanModels() ([]GGUFModelMeta, error) {
	start := time.Now()
	log := logger.Get()

	m.mu.Lock()
	m.totalScans++
	m.mu.Unlock()

	// Проверяем существование директории.
	//
	// R93: недоступный каталог (удалённый bind-mount, отвалившийся сетевой том)
	// больше НЕ оставляет старый кэш — инвентарь обнуляется, причина пишется в
	// dirError и уезжает в API. Иначе WebUI показывал модели, которых нет.
	info, err := os.Stat(m.modelsDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Infow("models directory does not exist, creating",
				"dir", m.modelsDir)
			if mkErr := os.MkdirAll(m.modelsDir, 0755); mkErr != nil {
				// Живой случай: каталог был bind-mount'ом, том удалили —
				// mkdir отвечает «File exists», а ls/ReadDir дают ENOENT.
				return m.recordDirFailure(fmt.Errorf("create models dir: %w", mkErr))
			}
		} else {
			return m.recordDirFailure(fmt.Errorf("stat models dir: %w", err))
		}
	} else if !info.IsDir() {
		return m.recordDirFailure(fmt.Errorf("models path is not a directory: %s", m.modelsDir))
	}

	// Сканируем .gguf файлы
	entries, err := os.ReadDir(m.modelsDir)
	if err != nil {
		return m.recordDirFailure(fmt.Errorf("read models dir: %w", err))
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

	// R66d (2026-09-23): сканируем алиасы моделей (<name>.gguf.json, создаются
	// через Ollama-совместимый POST /api/create). Раньше они не попадали ни в
	// ListModels, ни в /api/tags, ни в resolveModelPath — то есть «создали
	// модель, а её нигде нет и загрузить нельзя».
	newAliases := make(map[string]GGUFAlias)
	aliasFound := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf.json") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(m.modelsDir, name))
		if readErr != nil {
			continue
		}
		var rec struct {
			Name      string `json:"name"`
			Source    string `json:"source"`
			Modelfile string `json:"modelfile"`
			CreatedAt string `json:"created_at"`
		}
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		aliasName := rec.Name
		if aliasName == "" {
			aliasName = strings.TrimSuffix(name, ".gguf.json")
		}
		src := rec.Source
		if src == "" {
			continue
		}
		srcPath := src
		if !filepath.IsAbs(srcPath) {
			if !strings.HasSuffix(strings.ToLower(srcPath), ".gguf") {
				srcPath += ".gguf"
			}
			srcPath = filepath.Join(m.modelsDir, filepath.Base(srcPath))
		}
		a := GGUFAlias{
			Name:       aliasName,
			Source:     src,
			SourcePath: srcPath,
			Modelfile:  rec.Modelfile,
		}
		if fi, statErr := os.Stat(srcPath); statErr == nil {
			a.SourceSizeBytes = fi.Size()
			a.SourceExists = true
		}
		if ts, tsErr := time.Parse(time.RFC3339, rec.CreatedAt); tsErr == nil {
			a.CreatedAt = ts
		}
		a.AliasPath = filepath.Join(m.modelsDir, name)
		newAliases[aliasName] = a
		aliasFound++
	}

	// R66d: после сбора всех алиасов считаем эффективный источник с учётом
	// цепочек (alias → alias → .gguf) — SourceExists/SourceSizeBytes нужны
	// /api/tags, чтобы не показывать клиенту незагружаемые модели.
	for name, a := range newAliases {
		resolved, exists := resolveAliasChain(newAliases, newFiles, name)
		if resolved != "" {
			a.SourcePath = resolved
		}
		a.SourceExists = exists && a.SourcePath != ""
		if a.SourceExists {
			if fi, statErr := os.Stat(a.SourcePath); statErr == nil {
				a.SourceSizeBytes = fi.Size()
			}
		}
		newAliases[name] = a
	}

	// Обновляем кэш + фиксируем ИЗМЕНЕНИЯ инвентаря (R93).
	//
	// Диф нужен, чтобы «модель исчезла/появилась» было видно в логе и в API, а не
	// только по факту «список стал другим»: оператор должен понимать, что именно
	// изменилось, без перезапуска контейнера.
	m.mu.Lock()
	added, removed, resized := diffInventory(m.ggufFiles, newFiles)
	m.ggufFiles = newFiles
	m.aliases = newAliases
	m.lastScanTime = time.Now()
	m.scanDuration = time.Since(start)
	m.dirError = ""
	m.dirErrorAt = time.Time{}
	if len(added) > 0 || len(removed) > 0 || len(resized) > 0 {
		m.lastChangeAt = time.Now()
		m.lastAdded, m.lastRemoved, m.lastResized = added, removed, resized
		m.changeCount++
	}
	changes := len(added) + len(removed) + len(resized)
	m.mu.Unlock()

	if changes > 0 {
		log.Infow("model inventory changed",
			"added", added, "removed", removed, "resized", resized,
			"dir", m.modelsDir, "found", found)
	}
	log.Infow("model scan complete",
		"found", found,
		"aliases", aliasFound,
		"changes", changes,
		"dir", m.modelsDir,
		"duration", m.scanDuration)

	return m.ListModels(), nil
}

// diffInventory — что изменилось между двумя снимками каталога: добавленные,
// удалённые и изменившие размер файлы (отсортированы, чтобы лог был стабильным).
func diffInventory(oldFiles, newFiles map[string]*GGUFModelMeta) (added, removed, resized []string) {
	for name, meta := range newFiles {
		prev, ok := oldFiles[name]
		if !ok {
			added = append(added, name)
			continue
		}
		if prev != nil && meta != nil && prev.SizeBytes != meta.SizeBytes {
			resized = append(resized, name)
		}
	}
	for name := range oldFiles {
		if _, ok := newFiles[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(resized)
	return added, removed, resized
}

// recordDirFailure — каталог моделей недоступен: инвентарь ОБНУЛЯЕТСЯ.
//
// R93 (2026-10-09): раньше ошибка чтения каталога возвращалась наружу, а кэш
// оставался прежним — WebUI продолжал показывать модели, которых на диске уже нет
// (живой случай: bind-mount D:\ollama-legion-models удалили, /app/models даёт
// «File exists» на mkdir и «No such file or directory» на ls). Теперь список
// пуст, а причина лежит в dirError и уезжает в /api/models/files.
func (m *ModelManager) recordDirFailure(cause error) ([]GGUFModelMeta, error) {
	m.mu.Lock()
	prev := make([]string, 0, len(m.ggufFiles))
	for name := range m.ggufFiles {
		prev = append(prev, name)
	}
	sort.Strings(prev)
	m.ggufFiles = make(map[string]*GGUFModelMeta)
	m.aliases = make(map[string]GGUFAlias)
	m.dirError = cause.Error()
	m.dirErrorAt = time.Now()
	m.lastScanTime = time.Now()
	if len(prev) > 0 {
		m.lastChangeAt = time.Now()
		m.lastAdded, m.lastRemoved, m.lastResized = nil, prev, nil
		m.changeCount++
	}
	m.mu.Unlock()

	logger.Get().Warnw("model inventory: models dir unavailable, inventory cleared",
		"dir", m.modelsDir, "error", cause, "removed", prev)
	// Возвращаем пустой список БЕЗ ошибки: вызывающий код (HTTP-хендлеры) должен
	// отдать «моделей нет + причина», а не упасть. Операции импорта/удаления
	// сообщают о своих ошибках сами.
	return []GGUFModelMeta{}, nil
}

// ScanModelsIfStale — пересканировать каталог, если с прошлого скана прошло больше
// rescanTTL (R93). Возвращает true, если скан выполнялся.
//
// Вызывается из /api/models/files и из фонового цикла воркера: так удаление или
// добавление файла на диске становится видно без перезапуска контейнера.
func (m *ModelManager) ScanModelsIfStale() bool {
	m.mu.RLock()
	ttl := m.rescanTTL
	last := m.lastScanTime
	m.mu.RUnlock()
	if ttl <= 0 {
		return false
	}
	if !last.IsZero() && time.Since(last) < ttl {
		return false
	}
	_, _ = m.ScanModels()
	return true
}

// SetRescanTTL — период фоновой проверки каталога (0 = выключить). Для тестов и
// для оператора через env CPPWORKER_MODELS_RESCAN_SEC.
func (m *ModelManager) SetRescanTTL(d time.Duration) {
	m.mu.Lock()
	m.rescanTTL = d
	m.mu.Unlock()
}

// RescanTTL — текущий период фоновой проверки (0 = выключена).
func (m *ModelManager) RescanTTL() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rescanTTL
}

// InventoryStatus — снимок состояния инвентаря для API/WebUI.
func (m *ModelManager) InventoryStatus() ModelInventoryStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := ModelInventoryStatus{
		Dir:          m.modelsDir,
		Count:        len(m.ggufFiles),
		ScannedAt:    m.lastScanTime,
		ChangedAt:    m.lastChangeAt,
		Added:        append([]string(nil), m.lastAdded...),
		Removed:      append([]string(nil), m.lastRemoved...),
		Resized:      append([]string(nil), m.lastResized...),
		DirError:     m.dirError,
		DirErrorAt:   m.dirErrorAt,
		Scans:        m.totalScans,
		Changes:      m.changeCount,
		RescanTTLSec: int(m.rescanTTL / time.Second),
	}
	return st
}

// ListAliases возвращает все алиасы моделей (отсортированы по имени).
//
// R66d (2026-09-23): алиасы создаются Ollama-совместимым POST /api/create и
// лежат как <name>.gguf.json. Используется /api/tags, /api/models/files и
// resolveModelPath, чтобы созданная модель была видна и загружаема.
func (m *ModelManager) ListAliases() []GGUFAlias {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]GGUFAlias, 0, len(m.aliases))
	for _, a := range m.aliases {
		result = append(result, a)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// AliasSourcePath резолвит имя модели как алиас и возвращает путь к .gguf.
//
// Поддерживает цепочки алиас → алиас (например "short" → "gemma-alias" →
// "gemma-4-E4B-it-Q4_K_M.gguf") с ограничением глубины, чтобы цикл не повесил
// resolveModelPath. Возвращает false, если это не алиас или файл-источник
// отсутствует (тогда клиент получит честную ошибку, а не «модель не найдена»).
func (m *ModelManager) AliasSourcePath(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	srcPath, ok := resolveAliasChain(m.aliases, m.ggufFiles, name)
	if ok {
		return srcPath, true
	}
	// R83: если имя НЕ является алиасом, пробуем его варианты (Ollama-тег):
	// клиент просит "qwen3.8:latest", алиас создан как "qwen3.8".
	//
	// ВАЖНО: когда алиас ЕСТЬ, но файл-источник пропал, путь сохраняем как раньше
	// (R66d): вызывающий покажет понятную ошибку загрузки, а не «модель не
	// найдена». Регресс-тест: TestScanModels_Aliases_R66d («битый алиас»).
	if _, isAlias := aliasKey(m.aliases, name); !isAlias {
		for _, v := range ModelNameVariants(name) {
			if p, ok := resolveAliasChain(m.aliases, m.ggufFiles, v); ok {
				return p, true
			}
		}
	}
	return srcPath, false
}

// aliasKey — ключ алиаса для имени (та же нормализация, что в resolveAliasChain:
// trim + снятие .gguf).
func aliasKey(aliases map[string]GGUFAlias, name string) (string, bool) {
	current := strings.TrimSpace(name)
	if strings.HasSuffix(strings.ToLower(current), ".gguf") {
		current = current[:len(current)-5]
	}
	_, ok := aliases[current]
	return current, ok
}

// resolveAliasChain — общая логика резолва алиасов (используется и в ScanModels,
// чтобы заранее посчитать SourcePath/SourceExists для цепочек).
//
// Возвращает (путь-источника, существует-ли-файл). Путь возвращается даже если
// файла нет — вызывающий покажет его в логах/ошибке.
func resolveAliasChain(
	aliases map[string]GGUFAlias,
	files map[string]*GGUFModelMeta,
	name string,
) (string, bool) {
	const maxDepth = 4
	seen := make(map[string]bool, maxDepth)

	current := strings.TrimSpace(name)
	if strings.HasSuffix(strings.ToLower(current), ".gguf") {
		current = current[:len(current)-5]
	}

	var lastPath string
	for depth := 0; depth < maxDepth; depth++ {
		if current == "" || seen[current] {
			return lastPath, false
		}
		seen[current] = true

		a, ok := aliases[current]
		if !ok {
			return lastPath, false
		}
		srcBase := filepath.Base(a.Source)
		lastPath = a.SourcePath

		// 1. Источник — обычный .gguf, который видит скан.
		if meta, has := files[srcBase]; has {
			return meta.Path, true
		}
		// 2. Файл есть на диске (скан мог его ещё не увидеть).
		if _, err := os.Stat(a.SourcePath); err == nil {
			return a.SourcePath, true
		}
		// 3. Источник сам является алиасом — идём по цепочке.
		next := strings.TrimSuffix(srcBase, ".gguf")
		if _, isAlias := aliases[next]; isAlias {
			current = next
			continue
		}
		// 4. Ни файла, ни алиаса — битый алиас.
		return a.SourcePath, false
	}
	return lastPath, false
}

// AliasByName возвращает запись алиаса по имени модели (без .gguf).
func (m *ModelManager) AliasByName(name string) (GGUFAlias, bool) {
	current := strings.TrimSpace(name)
	if strings.HasSuffix(strings.ToLower(current), ".gguf") {
		current = current[:len(current)-5]
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.aliases[current]
	return a, ok
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
	// R83: условие включает KeyLength — параметры реального KV-кэша
	// (key_length/nextn/recurrent) обязаны добираться и для моделей, у которых
	// уже заполнены NLayers/NEmbd/NHeads/ContextLength.
	if meta.NLayers == 0 || meta.NEmbd == 0 || meta.NHeads == 0 || meta.ContextLength == 0 || meta.KeyLength == 0 {
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
			// R83 (2026-09-25): параметры реального KV-кэша — иначе он считался
			// по block_count и n_embd/n_heads (завышение в разы для гибридных
			// моделей). См. kv_layers.go.
			if meta.KeyLength == 0 && hdr.KeyLength > 0 {
				meta.KeyLength = hdr.KeyLength
			}
			if meta.ValueLength == 0 && hdr.ValueLength > 0 {
				meta.ValueLength = hdr.ValueLength
			}
			if meta.NextNPredictLayers == 0 && hdr.NextNPredictLayers > 0 {
				meta.NextNPredictLayers = hdr.NextNPredictLayers
			}
			if meta.FullAttentionInterval == 0 && hdr.FullAttentionInterval > 0 {
				meta.FullAttentionInterval = hdr.FullAttentionInterval
			}
			if hdr.HasRecurrentLayersKey {
				meta.HasRecurrentLayersKey = true
			}
			// R83 (2026-10-01): KV-sharing + скользящее окно (gemma4/gemma3n).
			if meta.SharedKVLayers == 0 && hdr.SharedKVLayers > 0 {
				meta.SharedKVLayers = hdr.SharedKVLayers
			}
			if meta.SlidingWindow == 0 && hdr.SlidingWindow > 0 {
				meta.SlidingWindow = hdr.SlidingWindow
			}
			if meta.SWAKeyLength == 0 && hdr.SWAKeyLength > 0 {
				meta.SWAKeyLength = hdr.SWAKeyLength
			}
			if meta.SWAPattern == "" && hdr.SWAPattern != "" {
				meta.SWAPattern = hdr.SWAPattern
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
