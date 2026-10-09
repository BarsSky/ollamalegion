package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
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
	//
	// R93 (2026-10-09): 300 → 60. Воркер теперь и сам пересканирует каталог по TTL
	// (CPPWORKER_MODELS_RESCAN_SEC, default 30 с), а балансеру нужно это заметить:
	// при 300 с удалённая модель жила в WebUI до 5 минут, что и выглядело как
	// «призраки». 60 с — компромисс: файловый листинг дешёвый, а задержка
	// появления/исчезновения модели в WebUI ограничена ~1.5 минутами.
	DefaultModelCatalogRefreshSec = 60

	// EnvModelCatalogRefreshSec — переопределение периода, секунды (0 = выключить).
	EnvModelCatalogRefreshSec = "LB_MODEL_CATALOG_REFRESH_SEC"

	// modelCatalogFetchTimeout — таймаут опроса воркера (файловый листинг быстрый:
	// fs.ReadDir без чтения метаданных модели).
	modelCatalogFetchTimeout = 10 * time.Second
)

// ModelCatalogRefreshInterval — период фонового обновления каталога (экспорт для
// админ-API: оператор должен видеть, с какой периодичностью балансер вообще
// проверяет наличие моделей на диске).
func ModelCatalogRefreshInterval() time.Duration { return modelCatalogRefreshInterval() }

// modelCatalogRefreshInterval — период обновления каталога из окружения.
//
// LB_MODEL_CATALOG_REFRESH_SEC:
//   - пусто        → DefaultModelCatalogRefreshSec (60 с);
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
//
// R93 (2026-10-09): кроме списка храним ИЗМЕНЕНИЯ (added/removed/changedAt) и
// причину недоступности каталога у воркера (dirError). Без этого оператор видел
// только «список стал другим» и не мог отличить «файлы удалили» от «каталог
// отвалился» — ровно та жалоба, из-за которой это и делалось.
type backendCatalogEntry struct {
	fetchedAt time.Time
	changedAt time.Time
	byName    map[string]string // нормализованное имя → имя у воркера
	dirError  string
	names     []string // как у воркера (для API и логов)
	added     []string
	removed   []string
}

// catalogChange — результат обновления снимка: что изменилось и было ли изменение.
type catalogChange struct {
	DirError string
	Added    []string
	Removed  []string
	Changed  bool
}

// modelCatalog — потокобезопасный кэш «какие модели лежат на диске у бэкенда».
//
// Порядок полей — по требованию govet fieldalignment (govet enable-all в
// .golangci.yml): указателесодержащие поля первыми. Начни с sync.RWMutex, и префикс
// указателей вырастет с 8 до 32 байт — CI-линт это ловит (only-new-issues).
type modelCatalog struct {
	byID map[string]*backendCatalogEntry
	mu   sync.RWMutex
}

func newModelCatalog() *modelCatalog {
	return &modelCatalog{byID: make(map[string]*backendCatalogEntry)}
}

// store — записать список файлов бэкенда (ключи нормализуются).
//
// R93: возвращает, ЧТО изменилось по сравнению с предыдущим снимком: добавленные
// и удалённые модели (нормализованные ключи, отсортированы) + признак изменения.
// Первый снимок изменением не считается (было «не знаем» → стало «знаем»).
func (mc *modelCatalog) store(backendID string, names []string, dirError string, now time.Time) catalogChange {
	if mc == nil || backendID == "" {
		return catalogChange{}
	}
	entry := &backendCatalogEntry{
		fetchedAt: now,
		byName:    make(map[string]string, len(names)),
		names:     make([]string, 0, len(names)),
		dirError:  strings.TrimSpace(dirError),
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
	prev := mc.byID[backendID]
	var change catalogChange
	if prev != nil {
		for key, name := range entry.byName {
			if _, ok := prev.byName[key]; !ok {
				change.Added = append(change.Added, name)
			}
		}
		for key, name := range prev.byName {
			if _, ok := entry.byName[key]; !ok {
				change.Removed = append(change.Removed, name)
			}
		}
		sort.Strings(change.Added)
		sort.Strings(change.Removed)
		change.Changed = len(change.Added) > 0 || len(change.Removed) > 0
		if change.Changed {
			entry.changedAt = now
			entry.added = change.Added
			entry.removed = change.Removed
		} else {
			// Изменений нет — сохраняем последнюю ИСТОРИЮ изменения, чтобы WebUI
			// мог показать «последнее изменение: N минут назад».
			entry.changedAt = prev.changedAt
			entry.added = prev.added
			entry.removed = prev.removed
		}
	}
	entry.dirError = strings.TrimSpace(dirError)
	change.DirError = entry.dirError
	mc.byID[backendID] = entry
	mc.mu.Unlock()
	return change
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
			ChangedAt: entry.changedAt,
			Added:     append([]string{}, entry.added...),
			Removed:   append([]string{}, entry.removed...),
			DirError:  entry.dirError,
			AgeSec:    int(now.Sub(entry.fetchedAt).Seconds()),
		}
	}
	return out
}

// ModelCatalogSnapshot — снимок каталога одного бэкенда (для JSON-ответа).
//
// Порядок полей — time.Time, затем срезы, затем скаляры (govet fieldalignment).
type ModelCatalogSnapshot struct {
	FetchedAt time.Time `json:"fetchedAt"`
	ChangedAt time.Time `json:"changedAt,omitempty"`
	DirError  string    `json:"dirError,omitempty"`
	Files     []string  `json:"files"`
	Added     []string  `json:"added,omitempty"`
	Removed   []string  `json:"removed,omitempty"`
	AgeSec    int       `json:"ageSec"`
}

// catalogHTTPClient — клиент опроса листинга файлов (короткий таймаут: это
// fs.ReadDir, а не генерация; висящий воркер не должен задерживать обновление).
var catalogHTTPClient = &http.Client{Timeout: modelCatalogFetchTimeout}

// backendEndpointSnapshot — базовый URL воркера и токен, снятые ПОД p.mu+state.mu.
//
// ПОЧЕМУ НЕ p.GetBackend + p.getBackendBaseURL. p.mu защищает только КАРТУ
// бэкендов, а поля самой структуры пишутся под p.mu/state.mu (см. GetAllBackends и
// UpdateBackend): прямое разыменование указателя из GetBackend вне блокировки —
// гонка. Живой случай (ловится `go test -race ./internal/api`): фоновое обновление
// каталога читало Host/CppWorkerPort/CppWorkerApiToken, пока heartbeat вызывал
// UpdateBackend и перезаписывал структуру — DATA RACE в
// fetchBackendModelCatalog → getBackendBaseURL.
func (p *Proxy) backendEndpointSnapshot(backendID string) (base, token string, ok bool) {
	p.mu.RLock()
	state, exists := p.backends[backendID]
	if !exists || state == nil || state.Backend == nil {
		p.mu.RUnlock()
		return "", "", false
	}
	state.mu.Lock()
	host := state.Backend.Host
	token = state.Backend.CppWorkerApiToken
	port := p.getBackendPort(state.Backend)
	state.mu.Unlock()
	p.mu.RUnlock()

	if host == "" || port <= 0 {
		return "", "", false
	}
	return fmt.Sprintf("http://%s:%d", host, port), token, true
}

// fetchBackendModelCatalog — прочитать список файлов (и алиасов) у воркера.
//
// R93: вместе со списком возвращаем dirError воркера — «каталог моделей
// недоступен» (удалённый bind-mount, отвалившийся том). Это НЕ то же самое, что
// «моделей нет»: список в обоих случаях пуст, а причина видна только здесь.
func (p *Proxy) fetchBackendModelCatalog(ctx context.Context, backendID string) ([]string, string, error) {
	base, token, ok := p.backendEndpointSnapshot(backendID)
	if !ok {
		return nil, "", fmt.Errorf("backend %q not found or has no reachable endpoint", backendID)
	}
	url := base + "/api/models/files"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	// Токен воркера — если он у записи есть (cppworker с включенной auth).
	if tok := strings.TrimSpace(token); tok != "" {
		req.Header.Set("X-API-Token", tok)
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := catalogHTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var doc struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
		Aliases []struct {
			Name string `json:"name"`
		} `json:"aliases"`
		DirError string `json:"dirError"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, "", fmt.Errorf("decode /api/models/files: %w", err)
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
	return names, strings.TrimSpace(doc.DirError), nil
}

// refreshBackendModelCatalog — обновить каталог одного бэкенда.
func (p *Proxy) refreshBackendModelCatalog(backendID string) {
	if p == nil || p.modelCatalog == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelCatalogFetchTimeout)
	defer cancel()

	names, dirErr, err := p.fetchBackendModelCatalog(ctx, backendID)
	if err != nil {
		// Не ошибка уровня стенда: воркер мог моргнуть на reload. Каталог
		// остаётся прежним, следующий цикл попробует снова.
		logger.Get().Debugw("model catalog: fetch failed", "backend", backendID, "error", err)
		return
	}
	change := p.modelCatalog.store(backendID, names, dirErr, time.Now())
	if change.Changed {
		// R93: изменение инвентаря фиксируем явно — в лог и в EventBus (колокольчик
		// и recent-errors в WebUI). Оператор узнаёт, что модели появились/исчезли,
		// без похода по воркерам и без перезапуска контейнеров.
		logger.Get().Infow("model catalog changed",
			"backend", backendID, "added", change.Added, "removed", change.Removed,
			"files", len(names))
		p.publishModelInventoryChanged(backendID, change, len(names))
	} else {
		logger.Get().Debugw("model catalog refreshed",
			"backend", backendID, "files", len(names), "dir_error", dirErr)
	}
	if dirErr != "" {
		logger.Get().Warnw("model catalog: worker reports models dir unavailable",
			"backend", backendID, "dir_error", dirErr)
	}
}

// publishModelInventoryChanged — уведомление об изменении инвентаря моделей.
//
// Дедупликация по составу изменения: повторные снимки с тем же дифом не спамят
// (каталог обновляется каждые LB_MODEL_CATALOG_REFRESH_SEC).
func (p *Proxy) publishModelInventoryChanged(backendID string, change catalogChange, total int) {
	if p == nil {
		return
	}
	key := "model_inventory_changed|" + strings.Join(change.Added, ",") + "|" + strings.Join(change.Removed, ",")
	msg := "Изменён состав моделей на диске: " + backendID
	if len(change.Added) > 0 {
		msg += ", добавлено: " + strings.Join(change.Added, ", ")
	}
	if len(change.Removed) > 0 {
		msg += ", удалено: " + strings.Join(change.Removed, ", ")
	}
	severity := types.SeverityInfo
	if len(change.Removed) > 0 && len(change.Added) == 0 {
		severity = types.SeverityWarning
	}
	p.publishLoadFailureEventDeduped(backendID, key, severity, msg, map[string]interface{}{
		"event_kind":      "model_inventory_changed",
		"backend":         backendID,
		"added":           change.Added,
		"removed":         change.Removed,
		"files_total":     total,
		"catalog_refresh": EnvModelCatalogRefreshSec,
	})
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
