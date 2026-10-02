// image_request_metrics.go — R-Image Phase 8 (2026-10-03): счётчики и лента
// запросов к image-бэкендам (тип image_cpp).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Текстовый путь считает запросы в Proxy.recordRequest
// (proxy_request.go) и отдаёт RPS/Avg RT в Ollama-метриках бэкенда. ImageRouter
// идёт МИМО этого кода (у него нет ни admission-очереди, ни Ollama-поверхности),
// поэтому до этого файла про image-трафик не знал никто: в Monitor у
// image-бэкенда было RPS=0.0, Avg RT=«-», Active=«0/10» при живых генерациях.
//
// ГРАНИЦЫ РЕШЕНИЙ (чтобы это не переоткрывали):
//
//   - Счётчики per-backend + ОБЩАЯ лента по пулу: оператору нужны и «сколько на
//     конкретном воркере», и «что вообще происходило с картинками».
//   - Исходов пять, а не два: ok / failed / rejected / accepted / finished
//     (см. types.ImageRequestMetrics — почему async не выдаётся за ok).
//   - Память ограничена ЖЁСТКО: лента 20 записей на бэкенд и 50 общих, выборка
//     длительностей 512 значений, окно RPS 60 с. Image-запросы дорогие
//     (секунды-минуты), но ленту нельзя делать растущей — балансер живёт недели.
//   - Метрика НЕ ходит в воркер и ничего не опрашивает: только факты, которые
//     балансер и так видит (запрос пришёл, гейт отказал, ответ получен).
package balancer

import (
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const (
	// imageRecentLimitPerBackend — сколько записей ленты храним на бэкенд.
	imageRecentLimitPerBackend = 20
	// imageRecentLimitGlobal — сколько записей в общей ленте пула.
	imageRecentLimitGlobal = 50
	// imageDurationSamples — сколько длительностей держим для p50/p95.
	imageDurationSamples = 512
	// imageRPSWindow — окно расчёта RPS (как у текстового recordRequest).
	imageRPSWindow = 60 * time.Second
	// imagePromptLimit — обрезка промпта в ленте (UI показывает подсказку, а не
	// полотно; полный промпт в метриках раздувал бы /api/v1/metrics).
	imagePromptLimit = 120
	// imageRequestStoreMaxBackends — предохранитель от роста карты, если в
	// кластер приходили и уходили сотни бэкендов (метрики мёртвых бэкендов
	// никому не нужны).
	imageRequestStoreMaxBackends = 128
)

// imageBackendRequests — счётчики одного image-бэкенда (или агрегат пула).
type imageBackendRequests struct {
	inFlight int
	total    int64
	ok       int64
	failed   int64
	rejected int64
	accepted int64
	finished int64

	// history — таймстемпы стартов за окно RPS (обрезается на каждом старте).
	history []time.Time
	// durations — длительности ЗАВЕРШЁННЫХ запросов (ok/failed/finished), мс.
	durations    []int64
	durationSum  int64
	lastDuration int64
	lastAt       time.Time

	failures   map[string]int64
	gateDenied map[string]int64
	recent     []types.ImageRequestBrief
}

func newImageBackendRequests() *imageBackendRequests {
	return &imageBackendRequests{
		failures:   make(map[string]int64),
		gateDenied: make(map[string]int64),
	}
}

// imageRequestStore — все счётчики image-запросов балансера.
type imageRequestStore struct {
	mu         sync.Mutex
	perBackend map[string]*imageBackendRequests
	global     *imageBackendRequests
	recent     []types.ImageRequestBrief
	seq        atomic.Int64
}

func newImageRequestStore() *imageRequestStore {
	return &imageRequestStore{
		perBackend: make(map[string]*imageBackendRequests),
		global:     newImageBackendRequests(),
	}
}

// imageRequestHandle — один запрос «в полёте».
//
// finish идемпотентен (sync.Once): путь генерации завершает запрос либо в
// defer роутера, либо сторожем асинхронной генерации — двойной учёт был бы
// прямым враньём в метриках.
type imageRequestHandle struct {
	store     *imageRequestStore
	backendID string
	brief     types.ImageRequestBrief
	started   time.Time
	once      sync.Once
}

// begin — регистрирует старт запроса: inFlight++, total++, окно RPS, лента «в работе».
//
// Лента получает запись СРАЗУ (status=accepted), а не только на финише: иначе
// долгая генерация (десятки секунд) в UI выглядела бы как «ничего не пришло».
func (s *imageRequestStore) begin(backendID string, brief types.ImageRequestBrief) *imageRequestHandle {
	if s == nil {
		return nil
	}
	now := time.Now()
	if brief.At.IsZero() {
		brief.At = now
	}
	if brief.Status == "" {
		brief.Status = types.ImageRequestStatusAccepted
	}
	if brief.ID == "" {
		brief.ID = "img-" + strconv.FormatInt(s.seq.Add(1), 10)
	}
	brief.BackendID = backendID

	h := &imageRequestHandle{store: s, backendID: backendID, brief: brief, started: now}

	s.mu.Lock()
	b := s.perBackend[backendID]
	if b == nil {
		b = newImageBackendRequests()
		s.perBackend[backendID] = b
	}
	s.startLocked(b, now)
	s.startLocked(s.global, now)
	s.pushLocked(&b.recent, brief, imageRecentLimitPerBackend)
	s.pushLocked(&s.recent, brief, imageRecentLimitGlobal)
	s.mu.Unlock()
	return h
}

// startLocked — общая часть старта для per-backend и агрегата.
func (s *imageRequestStore) startLocked(b *imageBackendRequests, now time.Time) {
	b.inFlight++
	b.total++
	b.lastAt = now
	b.history = append(b.history, now)
	cutoff := now.Add(-imageRPSWindow)
	keep := 0
	for keep < len(b.history) && b.history[keep].Before(cutoff) {
		keep++
	}
	if keep > 0 {
		b.history = append(b.history[:0], b.history[keep:]...)
	}
}

// finish — завершает запрос: counts + длительность + обновление ленты.
//
// status: ok | failed | rejected | finished (см. types.ImageRequestMetrics).
func (h *imageRequestHandle) finish(status string, httpStatus int, code, errMsg string, images int) {
	if h == nil || h.store == nil {
		return
	}
	h.once.Do(func() {
		now := time.Now()
		durMS := now.Sub(h.started).Milliseconds()
		if durMS < 0 {
			durMS = 0
		}
		h.store.mu.Lock()
		for _, b := range []*imageBackendRequests{h.store.perBackend[h.backendID], h.store.global} {
			if b == nil {
				continue
			}
			if b.inFlight > 0 {
				b.inFlight--
			}
			switch status {
			case types.ImageRequestStatusOK:
				b.ok++
			case types.ImageRequestStatusFailed:
				b.failed++
			case types.ImageRequestStatusRejected:
				b.rejected++
			case types.ImageRequestStatusFinished:
				b.finished++
			}
			b.lastDuration = durMS
			b.durations = append(b.durations, durMS)
			b.durationSum += durMS
			if len(b.durations) > imageDurationSamples {
				drop := len(b.durations) - imageDurationSamples
				for _, d := range b.durations[:drop] {
					b.durationSum -= d
				}
				b.durations = append(b.durations[:0], b.durations[drop:]...)
			}
			if code != "" {
				switch status {
				case types.ImageRequestStatusRejected:
					b.gateDenied[code]++
				default:
					b.failures[code]++
				}
			}
			entry := h.brief
			entry.At = h.started
			entry.Status = status
			entry.HTTPStatus = httpStatus
			entry.Code = code
			entry.Error = errMsg
			entry.DurationMs = durMS
			entry.Images = images
			h.store.replaceRecentLocked(&b.recent, entry, imageRecentLimitPerBackend)
		}
		h.store.replaceRecentLocked(&h.store.recent, types.ImageRequestBrief{
			ID: h.brief.ID, At: h.started, BackendID: h.backendID, Surface: h.brief.Surface,
			Path: h.brief.Path, Model: h.brief.Model, Prompt: h.brief.Prompt,
			Width: h.brief.Width, Height: h.brief.Height, Steps: h.brief.Steps, Batch: h.brief.Batch,
			DurationMs: durMS, Status: status, HTTPStatus: httpStatus, Code: code,
			Error: errMsg, Images: images,
		}, imageRecentLimitGlobal)
		h.store.mu.Unlock()
	})
}

// markAccepted — запрос принят асинхронно (202): генерация продолжается ПОСЛЕ
// ответа, поэтому запись остаётся «в полёте», а исход посчитает сторож
// асинхронной генерации (status=finished). Счётчик accepted нужен, чтобы
// отличить «в работе у воркера» от «в работе у движка».
func (h *imageRequestHandle) markAccepted() {
	if h == nil || h.store == nil {
		return
	}
	h.store.mu.Lock()
	for _, b := range []*imageBackendRequests{h.store.perBackend[h.backendID], h.store.global} {
		if b != nil {
			b.accepted++
		}
	}
	h.store.mu.Unlock()
}

// gateDenied — отказ гейта: до воркера запрос не дошёл, отдельный исход.
func (s *imageRequestStore) gateDenied(backendID, code, msg string) {
	if s == nil {
		return
	}
	brief := types.ImageRequestBrief{
		ID:        "img-" + strconv.FormatInt(s.seq.Add(1), 10),
		At:        time.Now(),
		BackendID: backendID,
		Status:    types.ImageRequestStatusRejected,
		Code:      code,
		Error:     msg,
	}
	s.mu.Lock()
	// Пустой backendID = «image-бэкенда нет вовсе»: записываем только в агрегат
	// пула, чтобы в per-backend карте не жила запись с пустым ключом.
	targets := []*imageBackendRequests{s.global}
	if backendID != "" {
		b := s.perBackend[backendID]
		if b == nil {
			b = newImageBackendRequests()
			s.perBackend[backendID] = b
		}
		targets = append(targets, b)
		s.pushLocked(&b.recent, brief, imageRecentLimitPerBackend)
	}
	for _, t := range targets {
		t.rejected++
		t.total++
		t.gateDenied[code]++
		t.lastAt = brief.At
	}
	s.pushLocked(&s.recent, brief, imageRecentLimitGlobal)
	s.mu.Unlock()
}

// pushLocked — добавляет запись в ленту (свежие в НАЧАЛЕ) с обрезкой.
func (s *imageRequestStore) pushLocked(dst *[]types.ImageRequestBrief, e types.ImageRequestBrief, limit int) {
	list := append([]types.ImageRequestBrief{e}, *dst...)
	if len(list) > limit {
		list = list[:limit]
	}
	*dst = list
}

// replaceRecentLocked — заменяет «в работе»-запись ленты её финальным исходом.
//
// Ищем по ID: запись уже лежит в ленте с момента старта (status=accepted), и
// добавлять вторую означало бы показывать один запрос дважды.
func (s *imageRequestStore) replaceRecentLocked(dst *[]types.ImageRequestBrief, e types.ImageRequestBrief, limit int) {
	for i := range *dst {
		if (*dst)[i].ID == e.ID {
			(*dst)[i] = e
			return
		}
	}
	s.pushLocked(dst, e, limit)
}

// snapshot — агрегаты бэкенда (nil-safe: нет записей → нули, а не nil).
func (s *imageRequestStore) snapshot(backendID string) *types.ImageRequestMetrics {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.perBackend[backendID]
	if b == nil {
		return &types.ImageRequestMetrics{}
	}
	return b.metricsLocked()
}

// aggregate — агрегаты по всему image-пулу.
func (s *imageRequestStore) aggregate() *types.ImageRequestMetrics {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.global.metricsLocked()
}

func (b *imageBackendRequests) metricsLocked() *types.ImageRequestMetrics {
	out := &types.ImageRequestMetrics{
		InFlight:       b.inFlight,
		Total:          b.total,
		OK:             b.ok,
		Failed:         b.failed,
		Rejected:       b.rejected,
		Accepted:       b.accepted,
		Finished:       b.finished,
		LastDurationMs: b.lastDuration,
		LastRequestAt:  b.lastAt,
	}
	if len(b.history) > 0 {
		out.RPS = float64(len(b.history)) / imageRPSWindow.Seconds()
	}
	if len(b.durations) > 0 {
		out.AvgDurationMs = b.durationSum / int64(len(b.durations))
		sorted := append([]int64(nil), b.durations...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		out.P50DurationMs = percentile(sorted, 0.50)
		out.P95DurationMs = percentile(sorted, 0.95)
	}
	if len(b.failures) > 0 {
		out.FailuresByCode = make(map[string]int64, len(b.failures))
		for k, v := range b.failures {
			out.FailuresByCode[k] = v
		}
	}
	if len(b.gateDenied) > 0 {
		out.GateDeniedByCode = make(map[string]int64, len(b.gateDenied))
		for k, v := range b.gateDenied {
			out.GateDeniedByCode[k] = v
		}
	}
	return out
}

// percentile — перцентиль по методу ближайшего ранга (nearest-rank):
// индекс = ceil(p × N), 1-based. Так p95 для пяти значений — это ПЯТОЕ значение
// (максимум), а не четвёртое: округление вниз по int(p×(N-1)) занижало бы
// «хвост» ровно там, где оператор ищет медленные генерации.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// recentFor — лента бэкенда (свежие в начале, максимум limit).
func (s *imageRequestStore) recentFor(backendID string, limit int) []types.ImageRequestBrief {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.perBackend[backendID]
	if b == nil || len(b.recent) == 0 {
		return nil
	}
	return cloneBriefs(b.recent, limit)
}

// recentGlobal — общая лента пула.
func (s *imageRequestStore) recentGlobal(limit int) []types.ImageRequestBrief {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneBriefs(s.recent, limit)
}

func cloneBriefs(src []types.ImageRequestBrief, limit int) []types.ImageRequestBrief {
	if limit <= 0 || limit > len(src) {
		limit = len(src)
	}
	out := make([]types.ImageRequestBrief, limit)
	copy(out, src[:limit])
	return out
}

// snapshotMap — агрегаты бэкенда в виде map (для /api/v1/metrics).
func (s *imageRequestStore) snapshotMap(backendID string) map[string]interface{} {
	m := s.snapshot(backendID)
	if m == nil {
		return nil
	}
	return imageRequestMetricsMap(m)
}

// aggregateMap — агрегаты пула + общая лента (для /api/v1/metrics).
func (s *imageRequestStore) aggregateMap(recentLimit int) map[string]interface{} {
	if s == nil {
		return nil
	}
	out := imageRequestMetricsMap(s.aggregate())
	if out == nil {
		return nil
	}
	if list := s.recentGlobal(recentLimit); len(list) > 0 {
		out["recent"] = list
	}
	return out
}

func imageRequestMetricsMap(m *types.ImageRequestMetrics) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := map[string]interface{}{
		"in_flight":           m.InFlight,
		"total":               m.Total,
		"ok":                  m.OK,
		"failed":              m.Failed,
		"rejected":            m.Rejected,
		"accepted":            m.Accepted,
		"finished":            m.Finished,
		"rps":                 m.RPS,
		"avg_duration_ms":     m.AvgDurationMs,
		"p50_duration_ms":     m.P50DurationMs,
		"p95_duration_ms":     m.P95DurationMs,
		"last_duration_ms":    m.LastDurationMs,
		"last_request_at":     m.LastRequestAt,
		"failures_by_code":    m.FailuresByCode,
		"gate_denied_by_code": m.GateDeniedByCode,
	}
	return out
}

// imagePoolMetricsFrom — агрегат image-запросов для /api/v1/cluster.
//
// Принимает УЖЕ СОБРАННЫЙ список бэкендов: GetClusterState держит p.mu.RLock, и
// повторный filterBackendsByType внутри него рискует самоблокировкой RWMutex
// (рекурсивный RLock при ожидающем писателе — документированный deadlock).
// Если image-бэкендов нет, блок не отдаётся вовсе: UI скрывает панель, а
// «нулевые счётчики» без единого воркера только путают оператора.
func (p *Proxy) imagePoolMetricsFrom(backends []types.BackendMetrics) *types.ImagePoolMetrics {
	if p == nil {
		return nil
	}
	res := p.imageResources()
	if res == nil {
		return nil
	}
	count := 0
	for i := range backends {
		if backends[i].BackendType == types.BackendTypeImage {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	reqs := res.imageRequests()
	if reqs == nil {
		return &types.ImagePoolMetrics{Backends: count}
	}
	return &types.ImagePoolMetrics{
		Backends: count,
		Requests: reqs.aggregate(),
		Recent:   reqs.recentGlobal(imageRecentLimitGlobal),
	}
}

// prune — предохранитель от роста карты счётчиков.
//
// Срабатывает ТОЛЬКО когда карта переросла imageRequestStoreMaxBackends, и
// удаляет лишь те бэкенды, которых нет в known, по которым нет запросов в
// полёте и не было активности дольше часа. Так «только что зарегистрированный»
// бэкенд (поллер ещё не успел снять снимок) не теряет свои счётчики, а метрики
// давно удалённых воркеров не живут до перезапуска балансера.
func (s *imageRequestStore) prune(known map[string]bool, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.perBackend) <= imageRequestStoreMaxBackends {
		return
	}
	for id, b := range s.perBackend {
		if known[id] || b.inFlight > 0 {
			continue
		}
		if now.Sub(b.lastAt) < time.Hour {
			continue
		}
		delete(s.perBackend, id)
	}
}
