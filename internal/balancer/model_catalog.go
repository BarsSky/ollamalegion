package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// R91 (2026-10-09): каталог моделей НА ДИСКЕ по бэкендам.
//
// ЗАЧЕМ. Балансер знал только «что ЗАГРУЖЕНО» (llamaMetrics[].LoadedModels и
// metricsMgr.BackendMetrics), но не «что ЛЕЖИТ НА ДИСКЕ». Из этого следовали две
// проблемы, обе найдены нагрузочным прогоном на двух машинах:
//
//  1. Текстовый запрос мог уйти на узел с пустым каталогом моделей и получить
//     503 model_not_found, хотя на другом узле файл лежит (у CPPWORKER-34 каталог
//     пуст, у cppworker-gpu-bundled-agent — gemma-4). Выбор был «первый healthy из
//     обхода карты»: наличие файла не проверялось вообще.
//  2. Поднять модель на втором узле (когда на первом она не загружена) было
//     нельзя: загрузка на узел без файла — та же 503, а какой узел имеет файл,
//     балансер не знал. Это блокировало безопасный cross-node warmup.
//
// ИСТОЧНИК ДАННЫХ: GET <worker>/api/models/files — тот же ответ, что рисует
// страница GGUF (files[].name + aliases[].name); токен воркера добавляем, если он
// есть у записи бэкенда. Файлы — основа; алиасы тоже кладём в набор, потому что
// клиент может попросить модель по имени алиаса (POST /api/create).
//
// СВЕЖЕСТЬ. Каталог — кэш, а не истина в последней инстанции:
//   - обновляется фоновым циклом (в poller'е метрик llama.cpp) с интервалом
//     LB_MODEL_CATALOG_REFRESH_SEC (default 300, 0 = выключить);
//   - обновляется по требованию, когда выбор бэкенда увидел протухшую запись
//     (асинхронно: запрос клиента НЕ ждёт сети до воркера);
//   - «записи нет» (known=false) — это НЕ «файла нет». Если каталога нет вовсе,
//     выбор работает как раньше (любой healthy узел): отсутствие телеметрии не
//     должно ломать рабочий стенд.
const (
	// DefaultModelCatalogRefreshSec — период перечитывания списка файлов на диске.
	DefaultModelCatalogRefreshSec = 300

	// EnvModelCatalogRefreshSec — переопределение периода, секунды (0 = выключить).
	EnvModelCatalogRefreshSec = "LB_MODEL_CATALOG_REFRESH_SEC"

	// modelCatalogFetchTimeout — таймаут опроса воркера (файловый листинг быстрый:
	// fs.ReadDir без чтения метаданных модели).
	modelCatalogFetchTimeout = 10 * time.Second
)

// modelCatalogRefreshInterval — период обновления каталога из окружения.
//
// LB_MODEL_CATALOG_REFRESH_SEC:
//   - пусто        → DefaultModelCatalogRefreshSec (300 с);
//   - 0 или меньше → 0 = каталог не обновляется автоматически (выбор бэкенда
//     работает как раньше);
//   - не число     → default + WARN (опечатка в .env не ломает стенд и не прячется).
func modelCatalogRefreshInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv(EnvModelCatalogRefreshSec))
	if v == "" {
		return DefaultModelCatalogRefreshSec * time.Second
	}
	sec, err := strconv.Atoi(v)
	if err != nil {
		logger.Get().Warnw("LB_MODEL_CATALOG_REFRESH_SEC is not a number, using default",
			"value", v, "default_sec", DefaultModelCatalogRefreshSec)
		return DefaultModelCatalogRefreshSec * time.Second
	}
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

// normalizeCatalogModelName — ключ сопоставления имени модели.
//
// Приводим к нижнему регистру, отбрасываем каталог, расширение .gguf и
// пробелы: клиент просит `gemma-4-E4B-it-Q4_K_M`, воркер отдаёт
// `gemma-4-E4B-it-Q4_K_M.gguf` (а в логах встречается и полный путь
// `/app/models/…gguf`) — это одна и та же модель.
func normalizeCatalogModelName(name string) string {
	s := strings.TrimSpace(name)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\\", "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, ".gguf")
	return strings.TrimSpace(s)
}

// backendCatalogEntry — что известно о файлах одного бэкенда.
type backendCatalogEntry struct {
	fetchedAt time.Time
	byName    map[string]string // нормализованное имя → имя у воркера
	names     []string          // как у воркера (для API и логов)
}

// modelCatalog — потокобезопасный кэш «какие модели лежат на диске у бэкенда».
type modelCatalog struct {
	mu   sync.RWMutex
	byID map[string]*backendCatalogEntry
}

func newModelCatalog() *modelCatalog {
	return &modelCatalog{byID: make(map[string]*backendCatalogEntry)}
}

// store — записать список файлов бэкенда (ключи нормализуются).
func (mc *modelCatalog) store(backendID string, names []string, now time.Time) {
	if mc == nil || backendID == "" {
		return
	}
	entry := &backendCatalogEntry{
		fetchedAt: now,
		byName:    make(map[string]string, len(names)),
		names:     make([]string, 0, len(names)),
	}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		entry.names = append(entry.names, n)
		if key := normalizeCatalogModelName(n); key != "" {
			entry.byName[key] = n
		}
	}
	mc.mu.Lock()
	mc.byID[backendID] = entry
	mc.mu.Unlock()
}

// lookup — есть ли модель на диске бэкенда.
//
// Возвращает (есть, известно, когда снят снимок). Известно=false означает «каталог
// этого бэкенда не опрашивался» — это НЕ «файла нет».
func (mc *modelCatalog) lookup(backendID, model string) (bool, bool, time.Time) {
	if mc == nil || backendID == "" {
		return false, false, time.Time{}
	}
	mc.mu.RLock()
	entry := mc.byID[backendID]
	mc.mu.RUnlock()
	if entry == nil {
		return false, false, time.Time{}
	}
	if model == "" {
		return false, true, entry.fetchedAt
	}
	key := normalizeCatalogModelName(model)
	if key == "" {
		return false, true, entry.fetchedAt
	}
	_, ok := entry.byName[key]
	return ok, true, entry.fetchedAt
}

// stale — нужен ли бэкенду новый снимок (нет записи или она старше interval).
func (mc *modelCatalog) stale(backendID string, interval time.Duration, now time.Time) bool {
	if interval <= 0 {
		return false
	}
	if mc == nil {
		return false
	}
	mc.mu.RLock()
	entry := mc.byID[backendID]
	mc.mu.RUnlock()
	if entry == nil {
		return true
	}
	return now.Sub(entry.fetchedAt) > interval
}

// drop — забыть бэкенд (запись удалена или перестала быть llama.cpp).
func (mc *modelCatalog) drop(backendID string) {
	if mc == nil || backendID == "" {
		return
	}
	mc.mu.Lock()
	delete(mc.byID, backendID)
	mc.mu.Unlock()
}

// snapshot — копия каталога для админ-API (backendID → файлы + возраст снимка).
func (mc *modelCatalog) snapshot() map[string]ModelCatalogSnapshot {
	out := make(map[string]ModelCatalogSnapshot)
	if mc == nil {
		return out
	}
	now := time.Now()
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	for id, entry := range mc.byID {
		if entry == nil {
			continue
		}
		out[id] = ModelCatalogSnapshot{
			Files:     append([]string{}, entry.names...),
			FetchedAt: entry.fetchedAt,
			AgeSec:    int(now.Sub(entry.fetchedAt).Seconds()),
		}
	}
	return out
}

// ModelCatalogSnapshot — снимок каталога одного бэкенда (для JSON-ответа).
type ModelCatalogSnapshot struct {
	Files     []string  `json:"files"`
	FetchedAt time.Time `json:"fetchedAt"`
	AgeSec    int       `json:"ageSec"`
}

// catalogHTTPClient — клиент опроса листинга файлов (короткий таймаут: это
// fs.ReadDir, а не генерация; висящий воркер не должен задерживать обновление).
var catalogHTTPClient = &http.Client{Timeout: modelCatalogFetchTimeout}

// fetchBackendModelCatalog — прочитать список файлов (и алиасов) у воркера.
func (p *Proxy) fetchBackendModelCatalog(ctx context.Context, backendID string) ([]string, error) {
	backend := p.GetBackend(backendID)
	if backend == nil {
		return nil, fmt.Errorf("backend %q not found", backendID)
	}
	base := p.getBackendBaseURL(backend)
	url := base + "/api/models/files"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Токен воркера — если он у записи есть (cppworker с включённой auth).
	if token := strings.TrimSpace(backend.CppWorkerApiToken); token != "" {
		req.Header.Set("X-API-Token", token)
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := catalogHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var doc struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
		Aliases []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode /api/models/files: %w", err)
	}

	names := make([]string, 0, len(doc.Files)+len(doc.Aliases))
	for _, f := range doc.Files {
		if n := strings.TrimSpace(f.Name); n != "" {
			names = append(names, n)
		}
	}
	for _, a := range doc.Aliases {
		if n := strings.TrimSpace(a.Name); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// refreshBackendModelCatalog — обновить каталог одного бэкенда.
func (p *Proxy) refreshBackendModelCatalog(backendID string) {
	if p == nil || p.modelCatalog == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelCatalogFetchTimeout)
	defer cancel()

	names, err := p.fetchBackendModelCatalog(ctx, backendID)
	if err != nil {
		// Не ошибка уровня стенда: воркер мог моргнуть на reload. Каталог
		// остаётся прежним, следующий цикл попробует снова.
		logger.Get().Debugw("model catalog: fetch failed", "backend", backendID, "error", err)
		return
	}
	p.modelCatalog.store(backendID, names, time.Now())
	logger.Get().Debugw("model catalog refreshed", "backend", backendID, "files", len(names))
}

// refreshStaleModelCatalogs — обновить каталоги бэкендов, у которых снимок протух.
//
// Вызывается из poller'а метрик (свой интервал poll'а — 30 с) и после регистрации
// бэкенда. Тяжёлая часть (сеть) — по одному запросу на бэкенд; при выключенном
// LB_MODEL_CATALOG_REFRESH_SEC (0) не делает ничего.
func (p *Proxy) refreshStaleModelCatalogs() {
	if p == nil || p.modelCatalog == nil {
		return
	}
	interval := modelCatalogRefreshInterval()
	if interval <= 0 {
		return
	}
	now := time.Now()
	for _, b := range p.GetAllBackends() {
		if normalizeBackendType(b.Type) != types.BackendTypeLlamaCpp {
			continue
		}
		if b.Status != types.StatusHealthy {
			continue
		}
		if !p.modelCatalog.stale(b.ID, interval, now) {
			continue
		}
		p.refreshBackendModelCatalog(b.ID)
	}
}

// BackendModelOnDisk — есть ли модель на диске бэкенда (known=false = «не знаем»).
//
// Экспортировано для админ-API и тестов: диагностика «почему запрос ушёл сюда».
func (p *Proxy) BackendModelOnDisk(backendID, model string) (has bool, known bool) {
	if p == nil || p.modelCatalog == nil {
		return false, false
	}
	has, known, _ = p.modelCatalog.lookup(backendID, model)
	return has, known
}

// ModelCatalogSnapshot — снимок каталога по всем бэкендам (админ-API).
func (p *Proxy) ModelCatalogSnapshot() map[string]ModelCatalogSnapshot {
	if p == nil || p.modelCatalog == nil {
		return map[string]ModelCatalogSnapshot{}
	}
	return p.modelCatalog.snapshot()
}

// RefreshModelCatalogNow — принудительное обновление каталога (админ-API, тесты).
//
// Работает и при LB_MODEL_CATALOG_REFRESH_SEC=0: это явный запрос оператора, а не
// фоновая активность.
func (p *Proxy) RefreshModelCatalogNow() map[string]ModelCatalogSnapshot {
	if p == nil || p.modelCatalog == nil {
		return map[string]ModelCatalogSnapshot{}
	}
	for _, b := range p.GetAllBackends() {
		if normalizeBackendType(b.Type) != types.BackendTypeLlamaCpp {
			continue
		}
		if b.Status == types.StatusOffline {
			continue
		}
		p.refreshBackendModelCatalog(b.ID)
	}
	return p.modelCatalog.snapshot()
}

// findLeastLoadedBackendWithModelOnDisk — наименее загруженный healthy узел,
// у которого ФАЙЛ модели есть на диске (по каталогу).
//
// Нужен там, где модель не загружена ни на одном узле: без этой проверки выбор
// падал на «первый healthy» и запрос уходил на узел с пустым каталогом (503
// model_not_found). Узлы, каталог которых не опрошен, кандидатами не становятся —
// за них отвечает прежний fallback, поэтому отсутствие каталога поведение не
// меняет.
func (p *Proxy) findLeastLoadedBackendWithModelOnDisk(modelName string, allowedTypes []types.BackendType) string {
	return p.findLeastLoadedOnDisk(modelName, allowedTypes, nil)
}

// findLeastLoadedBackendWithModelOnDiskExcluding — то же, но с исключением узлов
// (переезд с упавшего бэкенда: свою же копию повторно не выбираем).
func (p *Proxy) findLeastLoadedBackendWithModelOnDiskExcluding(modelName string, exclude map[string]bool) string {
	return p.findLeastLoadedOnDisk(modelName, []types.BackendType{types.BackendTypeLlamaCpp}, exclude)
}

func (p *Proxy) findLeastLoadedOnDisk(modelName string, allowedTypes []types.BackendType, exclude map[string]bool) string {
	if p == nil || modelName == "" || p.modelCatalog == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	interval := modelCatalogRefreshInterval()
	var bestBackendID string
	var bestLoadRatio float64 = 2.0
	var bestScore float64
	stale := false

	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if exclude[id] {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		has, known, fetchedAt := p.modelCatalog.lookup(id, modelName)
		if !known || !has {
			continue
		}
		if interval > 0 && time.Since(fetchedAt) > interval {
			stale = true
		}
		if !p.checkResourceLimits(id) {
			continue
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if maxReqs <= 0 {
			continue
		}
		loadRatio := float64(active) / float64(maxReqs)
		if bestBackendID == "" || loadRatio < bestLoadRatio {
			bestBackendID, bestLoadRatio, bestScore = id, loadRatio, p.calculateScore(id)
			continue
		}
		if loadRatio == bestLoadRatio {
			if score := p.calculateScore(id); score > bestScore || (score == bestScore && id < bestBackendID) {
				bestBackendID, bestScore = id, score
			}
		}
	}

	if stale && bestBackendID != "" {
		// Снимок протух — обновляем в фоне, но решение принимаем по тому, что
		// есть: ждать сеть в горячем пути выбора нельзя.
		go p.refreshStaleModelCatalogs()
	}
	return bestBackendID
}
