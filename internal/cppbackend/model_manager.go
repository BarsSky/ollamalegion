package cppbackend

import (
	"encoding/json"
	"fmt"
	"ollama-loadbalancer/pkg/logger"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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

	// R707 (2026-11-25): послойная раскладка KV. Прежние поля описывали КЛАСС
	// глобальных слоёв; здесь — вторая ширина (V) оконного класса, число KV-голов
	// по слоям и визиты loop-схемы. Без них gemma-4 нельзя посчитать точнее, чем
	// «20 оконных слоёв шириной 512», а Inkling-2-Mini — вообще по слоям.
	// Раскладка и формула — kv_layers.go (KVPlan), internal/memfit/kv.go.
	SWAValueLength  int   `json:"swaValueLength,omitempty"`  // <arch>.attention.value_length_swa
	KVHeadsPerLayer []int `json:"kvHeadsPerLayer,omitempty"` // массив <arch>.attention.head_count_kv
	LoopCount       int   `json:"loopCount,omitempty"`       // <arch>.num_loops (loop-схема)
	LoopCoreStart   int   `json:"loopCoreStart,omitempty"`   // <arch>.loop.core_start / .loop.start
	LoopCoreEnd     int   `json:"loopCoreEnd,omitempty"`     // <arch>.loop.core_end / .loop.end
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
