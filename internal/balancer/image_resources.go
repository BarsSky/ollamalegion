// image_resources.go — R-Image Phase 6 (2026-10-02): VRAM-гейт и политика
// сосуществования image-генерации с текстовым инференсом на одной GPU.
//
// ПРОБЛЕМА. Диффузионная модель (FLUX Q4 — пики 3.7–6.4 GB, Q8 — до 12 GB) и
// текстовая LLM на одной карте в VRAM обычно не сосуществуют: если балансер
// выдаёт текстовый слот во время генерации (или наоборот), одна из сторон
// падает OOM уже внутри движка — то есть ошибкой, которую клиент видит как
// «сломался бэкенд», а не как нехватку памяти.
//
// ЧТО ЗДЕСЬ:
//  1. Сбор метрик image-воркеров: периодический poll GET /api/image/models
//     (по образцу llamaCppMetricsPoller: 30 с базово, 2 с пока модель
//     загружается) + опциональный снимок VRAM воркера из
//     GET /api/image/capabilities (sdworker отдаёт там nvidia-smi-снимок).
//  2. Оценка VRAM image-модели по приоритету источников: профиль балансера →
//     число от воркера → размер файлов bundle'а × коэффициент → «неизвестно».
//  3. Гейт types.EvaluateImageVRAM (замороженный контракт
//     pkg/types/image_policy.go) плюс ЧЕСТНЫЕ отказы, когда модель на бэкенде
//     не готова: image_model_not_loaded / image_model_loading /
//     image_model_error. Ленивой загрузки у sdworker НЕТ (он отвечает
//     409 model_not_loaded — см. internal/sdbackend/jobs.go:654), поэтому
//     «пропустить наугад» означало бы гарантированную ошибку глубже.
//  4. Ресурсный лок GPU для политики exclusive: генерация владеет картой,
//     второй запрос ждёт QueueWaitTimeoutSec и получает 429, а предохранитель
//     ExclusiveLockTimeoutSec принудительно снимает лок с зависшей генерации
//     (иначе карта была бы занята навсегда). Ключ лока — host + индекс GPU,
//     когда индекс задан у ОБЕИХ сторон (см. locksConflict); иначе — host.
//
// ГРАНИЦЫ РЕШЕНИЙ (почему именно так — чтобы это не переоткрывали заново):
//
//   - Свободную VRAM НЕ выдумываем. Источники по приоритету: (1) число от
//     воркера в /api/image/models, (2) nvidia-smi-снимок воркера из
//     /api/image/capabilities, (3) available_vram_mb ТЕКСТОВОГО бэкенда на ТОМ ЖЕ
//     хосте (его уже собирает llamaCppMetricsPoller). Если ни одного — 0 =
//     «неизвестно», и EvaluateImageVRAM в этом случае НЕ блокирует.
//   - Гейт судит только при достоверных данных: воркер ответил контрактом
//     (/api/image/models с ключом models) и известно состояние модели. Иначе —
//     fail-open: «голый» sd-server (без /api/image/*), 401/403, таймаут или
//     битый JSON не должны превращаться в 503 на рабочей конфигурации.
//   - ПОЛИТИКУ решает замороженный контракт types.EvaluateImageVRAM, а не этот
//     файл: по умолчанию неизвестная оценка VRAM генерацию ПРОПУСКАЕТ (с WARN в
//     логе), строгость включается opt-in настройкой
//     balancing.image.blockOnUnknownVramEstimate (FIX-2). Отказ
//     insufficient_vram — чистая арифметика: применяется, когда известны и
//     оценка, и свободная VRAM.
//   - Текстовую сторону НЕ блокируем в глубине: минимальный хук в
//     backend_selector.go просто не выбирает текстового кандидата на хосте с
//     активным image-локом, а ждёт освобождения существующая admission-очередь
//     (unified_queue_r73.go) со своим лимитом LB_ADMISSION_WAIT_SEC и 503 +
//     Retry-After по его истечении.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Коды отказов и константы
// ============================================================

// Коды отказов, которых нет в замороженном контракте pkg/types/image_policy.go
// (там только VRAM-вердикты): состояние модели и занятость GPU.
const (
	// ImageGateModelNotLoaded — на бэкенде нет загруженной модели. Ленивой
	// загрузки у воркера нет: без этого отказа клиент получил бы 409
	// model_not_loaded из воркера, то есть менее понятную ошибку.
	ImageGateModelNotLoaded = "image_model_not_loaded"
	// ImageGateModelLoading — модель ещё грузится (спавн процесса + 2–12 GB).
	ImageGateModelLoading = "image_model_loading"
	// ImageGateModelError — модель не поднялась; message несёт причину воркера.
	ImageGateModelError = "image_model_error"
	// ImageGateGPUBusy — лок GPU занят другой генерацией (exclusive-политика).
	ImageGateGPUBusy = "image_gpu_busy"
)

const (
	// imageModelsPath — контракт image-воркера (Phase 3): список моделей,
	// состояний, оценок VRAM и active_queries.
	imageModelsPath = "/api/image/models"

	// imageWorkerVRAMPath — ЕДИНСТВЕННОЕ, что мы берём из capabilities-эндпоинта
	// воркера: снимок VRAM (nvidia-smi). Основной poll — /api/image/models;
	// агрегатор capabilities живёт отдельно (image_capabilities.go), чтобы не
	// дублировать чужой код.
	//
	// Имя константы отличается от imageWorkerCapabilitiesPath намеренно: та
	// принадлежит агрегатору Phase 4.
	imageWorkerVRAMPath = "/api/image/capabilities"

	// imageMetricsInterval — базовый интервал поллера (как у
	// llamaCppMetricsPoller: 30 с, чтобы не грузить воркер).
	imageMetricsInterval = 30 * time.Second
	// imageMetricsFastInterval — интервал, пока модель грузится/выгружается:
	// оператор и WebUI должны видеть смену состояния почти сразу.
	imageMetricsFastInterval = 2 * time.Second
	// imageMetricsStaleAfter — возраст, после которого снимок считается
	// протухшим и перед решением гейта обновляется СИНХРОННО. Маленькое
	// значение (секунды) — потому что цена ошибки высока в обе стороны:
	// протухший «loaded» пропустит запрос к выгруженной модели, а протухший
	// «not_loaded» откажет в рабочей генерации.
	imageMetricsStaleAfter = 5 * time.Second
	// imageMetricsProbeTimeout — таймаут одного HTTP-опроса воркера.
	imageMetricsProbeTimeout = 3 * time.Second
	// imageMetricsPollSlice — «кусок» сна поллера: Stop()/shutdown замечаются
	// в пределах этого интервала, а не через 30 с (Proxy.Shutdown не трогаем —
	// Phase 6 меняет proxy.go минимально, см. отчёт).
	imageMetricsPollSlice = 2 * time.Second
	// imageProfilesRefreshInterval — как часто перечитывать файл профилей
	// image-моделей. Профили правятся оператором/API без рестарта балансера,
	// а файл маленький.
	imageProfilesRefreshInterval = 30 * time.Second
	// maxTextVramSnapshotAge — насколько свежим должен быть снимок VRAM
	// текстового бэкенда, чтобы ему можно было верить (поллер ходит раз в 30 с;
	// 2 минуты — запас на один пропущенный цикл).
	maxTextVramSnapshotAge = 2 * time.Minute
	// maxImageModelsBody — предохранитель на размер тела ответа воркера.
	maxImageModelsBody = 4 << 20
	// imageVramFileCoefficient — коэффициент «размер файлов весов → пик VRAM».
	//
	// Почему 1.15: по официальным замерам sd.cpp (plans/2026-09-27-…-plan.md §6)
	// пик практически равен размеру файла — FLUX.1-dev q8_0: файл 12.71 GiB →
	// пик 12068 MB (≈0.93), FLUX q4_0: 6.77 GiB → 6395 MB (≈0.92), SD1.5 q8_0:
	// 1.76 GiB → 2.1 GB (≈1.19). Верх берём с небольшим запасом на VAE/latent и
	// фрагментацию: это ГРУБАЯ оценка последнего уровня, и она идёт в дело
	// только когда нет ни профиля, ни числа от воркера.
	imageVramFileCoefficient = 1.15
	// imageLockWaitPoll — период перепроверки лока внутри ожидания: ожидающие
	// просыпаются и по закрытию канала (release), и по таймеру (страховка от
	// потерянного пробуждения).
	imageLockWaitPoll = 100 * time.Millisecond
	// imageAsyncWatchInterval — период опроса воркера сторожем АСИНХРОННОЙ
	// генерации (нативный /api/image/generate отвечает 202 и генерирует фоном).
	imageAsyncWatchInterval = 3 * time.Second
	// imageAsyncIdlePolls — сколько подряд «active_queries == 0» после того, как
	// активность уже наблюдалась, означают конец генерации.
	imageAsyncIdlePolls = 2
	// imageAsyncStartupGracePolls — сколько опросов ждать старта генерации,
	// прежде чем решить, что джоба отработала мгновенно (быстрый sd-turbo 4 шага
	// может закончиться между 202 и первым опросом).
	imageAsyncStartupGracePolls = 5
	// imageAsyncMaxPollErrors — сколько подряд неудачных опросов воркера терпим,
	// прежде чем снять лок (мёртвый воркер не должен держать GPU до предохранителя).
	imageAsyncMaxPollErrors = 3
)

// ============================================================
// Wire-типы воркера
// ============================================================

// imageModelEntry — одна модель в ответе GET /api/image/models.
//
// Контракт воркера (заморожен): {name,state,size_bytes,vram_estimate_mb,
// active_queries,family,...}. Незнакомые поля игнорируются сознательно:
// воркер расширяется, а падать на новом ключе нельзя.
type imageModelEntry struct {
	Name           string                 `json:"name"`
	State          string                 `json:"state"`
	Family         string                 `json:"family"`
	SizeBytes      int64                  `json:"size_bytes"`
	VramEstimateMB int                    `json:"vram_estimate_mb"`
	ActiveQueries  int64                  `json:"active_queries"`
	Error          string                 `json:"error"`
	Files          []types.ImageModelFile `json:"files"`
}

// imageModelsResponse — ответ GET /api/image/models.
//
// Models — УКАЗАТЕЛЬ на срез: nil означает «ключа models в ответе нет», то есть
// воркер контракта не знает (например, на порту «голый» sd-server). Это
// принципиально отличается от «моделей нет» (пустой срез) — см. contractOK.
//
// VramFreeMB/VramTotalMB — опциональные поля: сегодняшний воркер их в
// /api/image/models НЕ отдаёт (VRAM берётся из capabilities), но контракт
// расширяемый, и тесты/будущий воркер могут их прислать.
type imageModelsResponse struct {
	Models       *[]imageModelEntry `json:"models"`
	State        string             `json:"state"`
	CurrentModel string             `json:"current_model"`
	VramFreeMB   int                `json:"vram_free_mb"`
	VramTotalMB  int                `json:"vram_total_mb"`
}

// imageVramWire — блок vram в ответе GET /api/image/capabilities воркера.
// Заполняется воркером из nvidia-smi (internal/sdbackend/vram.go).
type imageVramWire struct {
	Available bool   `json:"available"`
	UsedMB    int64  `json:"used_mb"`
	TotalMB   int64  `json:"total_mb"`
	Source    string `json:"source"`
}

// imageCapabilitiesVRAMResponse — подмножество ответа capabilities (только VRAM).
type imageCapabilitiesVRAMResponse struct {
	VRAM *imageVramWire `json:"vram"`
}

// ============================================================
// Снимок метрик одного бэкенда
// ============================================================

// imageBackendMetrics — ИММУТАБЕЛЬНЫЙ снимок состояния одного image-бэкенда.
//
// Иммутабельность — не стилистика: гейт читает снимок без блокировки на время
// расчёта, а поллер публикует НОВЫЙ указатель под r.mu. Мутация на месте дала бы
// гонку, которую -race поймал бы на реальной нагрузке.
type imageBackendMetrics struct {
	// at — когда снимок получен (для решения «протух ли»).
	at time.Time
	// backendID — чей это снимок.
	backendID string
	// contractOK — воркер ответил контрактом (/api/image/models с ключом models).
	// false = данных нет (другой сервис на порту, 404/405, битый JSON, 5xx):
	// гейт по такому снимку НЕ судит (fail-open).
	contractOK bool
	// models — список моделей воркера как есть (копия ответа).
	models []imageModelEntry
	// state — top-level state воркера (loaded/loading/not_loaded/error).
	state string
	// currentModel — имя текущей модели воркера (может быть пустым).
	currentModel string
	// vramFreeMB / vramTotalMB — свободная/полная VRAM хоста, если воркер её
	// сообщил (0 = неизвестно; НЕ «ноль свободно»).
	vramFreeMB  int
	vramTotalMB int
	// vramSource — откуда взято число (для логов и метрик).
	vramSource string
	// lastErr — ошибка последнего опроса. Непустая означает «снимок
	// недостоверен»: гейт его не использует (fail-open), но продолжает
	// показывать оператору последнюю известную модель.
	lastErr string
}

// loaded — загруженная модель (state == "loaded"), nil если такой нет.
func (s *imageBackendMetrics) loaded() *imageModelEntry {
	return s.firstWithState(imageStateLoaded)
}

// loading — модель в процессе загрузки.
func (s *imageBackendMetrics) loading() *imageModelEntry {
	return s.firstWithState(imageStateLoading)
}

// failed — модель, которая не поднялась.
func (s *imageBackendMetrics) failed() *imageModelEntry {
	return s.firstWithState(imageStateError)
}

// activeQueries — суммарная активность по моделям воркера (для сторожа
// асинхронной генерации). Сумма, а не первая модель: активность может быть
// приписана только текущей модели, но суммирование не зависит от того, какую
// именно запись воркер считает текущей.
func (s *imageBackendMetrics) activeQueries() int64 {
	if s == nil {
		return 0
	}
	var total int64
	for i := range s.models {
		total += s.models[i].ActiveQueries
	}
	return total
}

// modelNames — известные имена моделей (для человекочитаемой ошибки).
func (s *imageBackendMetrics) modelNames() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.models))
	for i := range s.models {
		if n := strings.TrimSpace(s.models[i].Name); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// firstWithState — первая модель с указанным состоянием (детерминированно:
// порядок ответа воркера стабилен, он отдаёт модели по алфавиту).
func (s *imageBackendMetrics) firstWithState(state string) *imageModelEntry {
	if s == nil {
		return nil
	}
	for i := range s.models {
		if s.models[i].State == state {
			return &s.models[i]
		}
	}
	return nil
}

// Состояния воркера (snake_case, как в контракте /api/image/models).
// Дублируют internal/sdbackend (тот пакет балансеру недоступен по слоям) —
// значения заморожены контрактом, расхождение поймал бы контракт-тест.
const (
	imageStateLoaded    = "loaded"
	imageStateLoading   = "loading"
	imageStateError     = "error"
	imageStateNotLoaded = "not_loaded"
)

// ============================================================
// Ресурсный лок GPU
// ============================================================

// imageHostLock — состояние лока одной GPU (или всего хоста). Все поля защищены
// imageResources.mu.
type imageHostLock struct {
	// holder — владелец лока (nil при свободном локе).
	holder *imageLockHolder
	// releaseCh — канал, закрываемый при освобождении: будит ожидающих.
	releaseCh chan struct{}
	// host — хост лока (нужен для проверки конфликтов между ключами хоста).
	host string
	// gpuIndex — индекс GPU лока; 0 = «неизвестно» = лок на весь хост.
	gpuIndex int
	// held — занят ли лок.
	held bool
}

// imageLockKey — ключ лока в карте r.locks.
//
// ЗАЧЕМ СОСТАВНОЙ КЛЮЧ (R-Image follow-up, 2026-10-02): раньше лок брался по
// host, и на multi-GPU хосте генерация на одной карте блокировала текстовый
// бэкенд на другой. Теперь ключ — host + индекс GPU, КОГДА индекс задан; при
// gpuIndex == 0 ключ равен хосту (прежнее поведение) — индекс «неизвестно»
// не даёт права сузить лок.
func imageLockKey(host string, gpuIndex int) string {
	if gpuIndex > 0 {
		return host + "#gpu" + strconv.Itoa(gpuIndex)
	}
	return host
}

// locksConflict — пересекаются ли два лока по GPU.
//
// КОНСЕРВАТИЗМ (почему именно так): если индекс не задан ХОТЯ БЫ У ОДНОЙ
// стороны, считаем, что она занимает весь хост — какой GPU она использует, нам
// неизвестно, а «пропустить» текстовый запрос на карту генерации означает OOM
// внутри движка (ровно то, от чего защищает политика exclusive). Сужаем лок
// только когда ОБЕ стороны назвали свою карту.
func locksConflict(a, b *imageHostLock) bool {
	if a == nil || b == nil || a.host != b.host {
		return false
	}
	if a.gpuIndex == 0 || b.gpuIndex == 0 {
		return true
	}
	return a.gpuIndex == b.gpuIndex
}

// lockOrderLess — детерминированный порядок конфликтующих локов: сначала
// хостовый (он блокирует всё), затем по возрастанию индекса GPU, затем по
// ключу. Нужен, чтобы выбор «на чьём releaseCh спать» не зависел от порядка
// обхода map (иначе при нескольких локах хоста ожидание становилось бы
// случайным — а timer-страховка всего лишь 100 мс).
func lockOrderLess(a, b *imageHostLock) bool {
	if a == nil || b == nil {
		return a == nil && b != nil
	}
	if (a.gpuIndex == 0) != (b.gpuIndex == 0) {
		return a.gpuIndex == 0
	}
	if a.gpuIndex != b.gpuIndex {
		return a.gpuIndex < b.gpuIndex
	}
	return imageLockKey(a.host, a.gpuIndex) < imageLockKey(b.host, b.gpuIndex)
}

// imageLockHolder — право владения локом GPU на время генерации.
//
// Инвариант: Release() идемпотентен (sync.Once) и безопасен при гонке со
// сторожевым таймером-предохранителем: кто первый — тот и освобождает, второй
// вызов становится no-op.
type imageLockHolder struct {
	res        *imageResources
	fuse       *time.Timer
	done       chan struct{}
	key        string
	host       string
	backendID  string
	model      string
	acquiredAt time.Time
	gpuIndex   int
	once       sync.Once
	// handedOff — лок передан сторожу асинхронной генерации: отложенный
	// Release() из HTTP-обработчика обязан стать no-op.
	handedOff atomic.Bool
}

// Release — освободить лок. Безопасен при повторном вызове и при гонке с
// предохранителем.
func (h *imageLockHolder) Release(reason string) {
	if h == nil {
		return
	}
	h.once.Do(func() {
		if h.fuse != nil {
			h.fuse.Stop()
		}
		if h.res != nil {
			h.res.unlock(h)
		}
		close(h.done)
		held := time.Since(h.acquiredAt)
		logger.Get().Infow("image GPU lock released",
			"host", h.host,
			"gpu_index", h.gpuIndex,
			"backend", h.backendID,
			"model", h.model,
			"reason", reason,
			"held_ms", held.Milliseconds())
	})
}

// HandOff — передать лок сторожу асинхронной генерации. После вызова
// Release() из обработчика запроса ничего не делает: лок снимет сторож.
func (h *imageLockHolder) HandOff() {
	if h != nil {
		h.handedOff.Store(true)
	}
}

// ============================================================
// imageResources
// ============================================================

// imageResources — nil-safe доступ к состоянию Phase 6 (тип imageResources)
// у Proxy.
//
// nil-safe: тесты и внутренние вызовы иногда работают с Proxy, собранным
// вручную (&Proxy{}), где NewProxy не выполнялся. Тогда гейт просто не
// применяется (нет состояния — нет и решения), а не паникует.
func (p *Proxy) imageResources() *imageResources {
	if p == nil {
		return nil
	}
	return p.imageRes
}

// imageResources — состояние Phase 6 у Proxy: кэш метрик image-бэкендов,
// ресурсные локи GPU и счётчики наблюдаемости.
//
// Один экземпляр на Proxy (создаётся в NewProxy). Отдельная структура, а не
// поля Proxy, — чтобы не раздувать Proxy и держать всю логику Phase 6 в одном
// файле (см. договорённость о зонах правок).
type imageResources struct {
	proxy        *Proxy
	profiles     *config.ImageModelProfileStore
	lockWaitHist *histogramCollector
	stopCh       chan struct{}

	snapshots  map[string]*imageBackendMetrics
	locks      map[string]*imageHostLock
	gateDenied map[string]int64

	mu        sync.Mutex
	profileMu sync.Mutex
	stopOnce  sync.Once

	gateAllowed       atomic.Int64
	gateDeniedTotal   atomic.Int64
	lockAcquired      atomic.Int64
	lockRejected      atomic.Int64
	lockForced        atomic.Int64
	lockWaits         atomic.Int64
	lockWaitSumMS     atomic.Int64
	textSlotsWithheld atomic.Int64
	// textWaitCapped — сколько раз ожидание ТЕКСТОВОГО запроса было урезано до
	// balancing.image.queueWaitTimeoutSec, потому что единственная причина
	// отсутствия слота — активный image-лок (Phase 6 follow-up, п.2).
	textWaitCapped    atomic.Int64
	gateSkippedNoData atomic.Int64
	started           atomic.Bool

	profilesAt           time.Time
	queueWaitOverride    time.Duration
	lockFuseOverride     time.Duration
	probeTimeoutOverride time.Duration
	staleAfterOverride   time.Duration
	pollIntervalOverride time.Duration
}

// newImageResources — конструктор. Поллер НЕ стартует здесь: Start() вызывает
// NewProxy (как для llamaCppMetricsPoller).
func newImageResources(proxy *Proxy) *imageResources {
	return &imageResources{
		proxy:        proxy,
		snapshots:    make(map[string]*imageBackendMetrics),
		locks:        make(map[string]*imageHostLock),
		gateDenied:   make(map[string]int64),
		lockWaitHist: newHistogramCollector(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30),
		stopCh:       make(chan struct{}),
	}
}

// settings — секция balancing.image из конфига (nil-safe).
func (r *imageResources) settings() types.ImageResourceSettings {
	if r == nil || r.proxy == nil || r.proxy.config == nil {
		return types.ImageResourceSettings{}
	}
	return r.proxy.config.Balancing.Image
}

// queueWait — сколько exclusive-режим ждёт освобождения GPU.
func (r *imageResources) queueWait() time.Duration {
	if r != nil && r.queueWaitOverride > 0 {
		return r.queueWaitOverride
	}
	return time.Duration(r.settings().EffectiveQueueWaitTimeout()) * time.Second
}

// lockFuse — предохранитель «лок нельзя держать вечно».
func (r *imageResources) lockFuse() time.Duration {
	if r != nil && r.lockFuseOverride > 0 {
		return r.lockFuseOverride
	}
	return time.Duration(r.settings().EffectiveExclusiveLockTimeout()) * time.Second
}

// probeTimeout — таймаут одного HTTP-опроса воркера.
func (r *imageResources) probeTimeout() time.Duration {
	if r != nil && r.probeTimeoutOverride > 0 {
		return r.probeTimeoutOverride
	}
	return imageMetricsProbeTimeout
}

// staleAfter — возраст снимка, после которого гейт обновляет его синхронно.
func (r *imageResources) staleAfter() time.Duration {
	if r != nil && r.staleAfterOverride > 0 {
		return r.staleAfterOverride
	}
	return imageMetricsStaleAfter
}

// interval — текущий интервал поллера (быстрый, пока модель грузится).
// Настраивается тестами через pollIntervalOverride.
func (r *imageResources) interval() time.Duration {
	if r == nil {
		return imageMetricsInterval
	}
	if r.pollIntervalOverride > 0 {
		return r.pollIntervalOverride
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.snapshots {
		if s != nil && (s.loading() != nil || s.state == imageStateLoading) {
			return imageMetricsFastInterval
		}
	}
	return imageMetricsInterval
}

// ============================================================
// Поллер метрик
// ============================================================

// Start — запуск фонового поллера (идемпотентно).
func (r *imageResources) Start() {
	if r == nil || !r.started.CompareAndSwap(false, true) {
		return
	}
	go r.loop()
}

// Stop — остановка поллера (идемпотентно). NewProxy Shutdown не меняет —
// поллер сам завершается, увидев p.shuttingDown (см. loop).
func (r *imageResources) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stopCh) })
}

// stopped — запрошена ли остановка.
func (r *imageResources) stopped() bool {
	if r == nil {
		return true
	}
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

// loop — цикл поллера.
//
// ПЕРВЫЙ ОПРОС — НЕ НЕМЕДЛЕННЫЙ, а через интервал (FIX-1 интеграции Phase 6).
// Почему: немедленный pollAll в Start() добавлял HTTP-запрос в image-воркер в
// момент старта балансера — это ломало тесты/скрипты, считающие обращения к
// /api/image/models (например internal/api TestImageBackendProxy_ModelsAlias
// ожидает ровно одно обращение), и дёргало сеть до того, как она реально нужна.
// Актуальность метрик К МОМЕНТУ ЗАПРОСА обеспечивает не поллер, а ленивая
// синхронная догрузка ensureFreshFor() перед гейтом — поэтому задержка первого
// тика на корректность решений не влияет.
//
// Бэкендов без типа image_cpp цикл не трогает вообще: pollAll выходит до любого
// сетевого вызова (см. проверку filterBackendsByType).
func (r *imageResources) loop() {
	for {
		if r.pollingDone() {
			return
		}
		// Спим «кусками»: Stop/shutdown замечаются в пределах imageMetricsPollSlice.
		deadline := time.Now().Add(r.interval())
		for time.Now().Before(deadline) {
			if r.pollingDone() {
				return
			}
			slice := time.Until(deadline)
			if slice > imageMetricsPollSlice {
				slice = imageMetricsPollSlice
			}
			timer := time.NewTimer(slice)
			select {
			case <-r.stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if r.pollingDone() {
			return
		}
		r.pollAll(context.Background())
	}
}

// pollingDone — пора ли завершать цикл (Stop или Shutdown прокси).
func (r *imageResources) pollingDone() bool {
	if r.stopped() {
		return true
	}
	return r.proxy != nil && r.proxy.shuttingDown.Load()
}

// pollAll — параллельный опрос всех ЗДОРОВЫХ image-бэкендов.
func (r *imageResources) pollAll(ctx context.Context) {
	if r == nil || r.proxy == nil {
		return
	}
	backends := r.proxy.filterBackendsByType(types.BackendTypeImage)
	if len(backends) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, st := range backends {
		if st == nil || st.Backend == nil {
			continue
		}
		id := st.Backend.ID
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			r.refresh(ctx, id)
		}(id)
	}
	wg.Wait()
}

// refresh — опросить бэкенд и опубликовать новый снимок.
//
// Ошибка опроса НЕ стирает последний снимок (иначе UI/логи потеряли бы модель),
// но помечает его lastErr: гейт по недостоверному снимку не судит.
func (r *imageResources) refresh(ctx context.Context, backendID string) *imageBackendMetrics {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	snap, err := r.fetchModels(ctx, backendID)
	if err != nil {
		failed := r.metricsSnapshot(backendID)
		cp := imageBackendMetrics{backendID: backendID}
		if failed != nil {
			cp = *failed
		}
		cp.at = time.Now()
		cp.lastErr = err.Error()
		r.store(&cp)
		logger.Get().Debugw("image metrics poll failed",
			"backend", backendID, "error", err)
		return &cp
	}
	if snap != nil {
		r.store(snap)
	}
	return snap
}

// store — публикация снимка (под mu; снимок иммутабелен после этого).
func (r *imageResources) store(s *imageBackendMetrics) {
	if r == nil || s == nil {
		return
	}
	r.mu.Lock()
	r.snapshots[s.backendID] = s
	r.mu.Unlock()
}

// metricsSnapshot — текущий снимок бэкенда (nil, если опроса ещё не было).
func (r *imageResources) metricsSnapshot(backendID string) *imageBackendMetrics {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshots[backendID]
}

// ensureFreshFor — снимок для решения гейта: свежий из кэша либо ЛЕНИВАЯ
// СИНХРОННАЯ догрузка с коротким таймаутом (imageMetricsProbeTimeout).
//
// ЗАЧЕМ ЛЕНИВАЯ ДОГРУЗКА, ЕСЛИ ЕСТЬ ПОЛЛЕР: поллер экономит сеть (первый тик —
// через интервал, дальше 30 с), но решение принимается per-request, и протухший
// (или отсутствующий) снимок стоит дорого в обе стороны — пропуск запроса к
// выгруженной модели или отказ рабочей генерации. Поэтому в момент запроса
// метрики при необходимости обновляются здесь; порог свежести — staleAfter
// (5 с), то есть подряд идущие генерации переиспользуют один опрос.
func (r *imageResources) ensureFreshFor(ctx context.Context, backendID string) *imageBackendMetrics {
	prev := r.metricsSnapshot(backendID)
	if prev != nil && prev.lastErr == "" && time.Since(prev.at) < r.staleAfter() {
		return prev
	}
	return r.refresh(ctx, backendID)
}

// fetchModels — GET /api/image/models (+ опционально VRAM из capabilities).
//
// proxyRequestToBackend переиспользуется сознательно: он уже знает, что порт
// image-бэкенда берётся из EffectiveImagePort(), и не даёт дублировать
// построение URL/таймаута.
func (r *imageResources) fetchModels(ctx context.Context, backendID string) (*imageBackendMetrics, error) {
	if r.proxy == nil {
		return nil, fmt.Errorf("image resources: proxy is nil")
	}
	// ВАЖНО: URL здесь — ОТНОСИТЕЛЬНЫЙ путь. proxyRequestToBackend сам
	// подставляет host:port бэкенда (EffectiveImagePort) и склеивает
	// `http://host:port` + r.URL.String(); абсолютный URL дал бы мусор вида
	// `http://127.0.0.1:18093http://image-backend/api/image/models`.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageModelsPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.proxy.proxyRequestToBackend(req, backendID, r.probeTimeout())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 404/405 = воркер контракта не знает («голый» sd-server на этом порту).
	// Это НЕ ошибка гейта: данных нет — значит, гейт молчит (fail-open).
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return &imageBackendMetrics{
			at:         time.Now(),
			backendID:  backendID,
			contractOK: false,
			lastErr:    fmt.Sprintf("%s returned HTTP %d (worker contract unsupported)", imageModelsPath, resp.StatusCode),
		}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned HTTP %d", imageModelsPath, resp.StatusCode)
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxImageModelsBody))
	if readErr != nil {
		return nil, fmt.Errorf("read %s body: %w", imageModelsPath, readErr)
	}
	var wire imageModelsResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", imageModelsPath, err)
	}

	snap := &imageBackendMetrics{
		at:           time.Now(),
		backendID:    backendID,
		state:        wire.State,
		currentModel: wire.CurrentModel,
		vramFreeMB:   wire.VramFreeMB,
		vramTotalMB:  wire.VramTotalMB,
	}
	if wire.VramFreeMB > 0 {
		snap.vramSource = "worker:/api/image/models"
	}
	if wire.Models == nil {
		// Ключа models нет — контракт не подтверждён: данными не пользуемся.
		snap.lastErr = fmt.Sprintf("%s has no \"models\" key (worker contract unsupported)", imageModelsPath)
		return snap, nil
	}
	snap.contractOK = true
	snap.models = *wire.Models

	// VRAM воркера: /api/image/models её не несёт, поэтому один дополнительный
	// (best-effort) запрос к capabilities, где sdworker отдаёт nvidia-smi-снимок.
	if snap.vramFreeMB <= 0 {
		r.enrichVRAMFromCapabilities(ctx, backendID, snap)
	}
	return snap, nil
}

// enrichVRAMFromCapabilities — best-effort снимок свободной VRAM воркера.
// Любая ошибка здесь не влияет на остальные метрики (VRAM остаётся неизвестной,
// и гейт по ней не судит).
func (r *imageResources) enrichVRAMFromCapabilities(ctx context.Context, backendID string, snap *imageBackendMetrics) {
	if r == nil || r.proxy == nil || snap == nil {
		return
	}
	// Относительный путь — см. комментарий в fetchModels.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageWorkerVRAMPath, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.proxy.proxyRequestToBackend(req, backendID, r.probeTimeout())
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageModelsBody))
	if err != nil {
		return
	}
	var wire imageCapabilitiesVRAMResponse
	if err := json.Unmarshal(body, &wire); err != nil || wire.VRAM == nil || !wire.VRAM.Available {
		return
	}
	if wire.VRAM.TotalMB <= 0 {
		return
	}
	free := wire.VRAM.TotalMB - wire.VRAM.UsedMB
	if free <= 0 {
		return
	}
	snap.vramFreeMB = int(free)
	snap.vramTotalMB = int(wire.VRAM.TotalMB)
	snap.vramSource = "worker:" + imageWorkerVRAMPath
}

// ============================================================
// Гейт: решение на запрос генерации
// ============================================================

// imageGateDecision — результат «VRAM-гейт + лок GPU» для одного запроса.
type imageGateDecision struct {
	// Allowed — можно ли проксировать запрос.
	Allowed bool
	// Status/Code/Message/Hint/RetryAfterSec — готовый HTTP-отказ.
	Status        int
	Code          string
	Message       string
	Hint          string
	RetryAfterSec int
	// Verdict — решение EvaluateImageVRAM (для логов/метрик).
	Verdict types.ImageVRAMVerdict

	res    *imageResources
	holder *imageLockHolder
}

// Release — освободить лок GPU. Обязателен в defer у обработчика запроса;
// при передаче лока сторожу асинхронной генерации ничего не делает.
func (d imageGateDecision) Release() {
	if d.res == nil || d.holder == nil {
		return
	}
	d.res.releaseFromRequest(d.holder)
}

// beforeGeneration — гейт + захват лока перед проксированием генерации.
//
// Порядок шагов важен: сначала состояние модели (иначе гейт считал бы VRAM
// модели, которую воркер не держит), затем VRAM, затем лок.
func (r *imageResources) beforeGeneration(ctx context.Context, backendID string) imageGateDecision {
	allow := imageGateDecision{Allowed: true, Code: types.ImageGateOK}
	if r == nil || r.proxy == nil {
		return allow
	}
	settings := r.settings()
	backend := r.proxy.GetBackend(backendID)
	host := ""
	gpuIndex := 0
	if backend != nil {
		host = backend.Host
		// Индекс GPU: явный gpuIndex → CppWorkerConfig.MainGPU → 0 («неизвестно»).
		// При 0 лок остаётся хостовым — см. locksConflict.
		gpuIndex = backend.EffectiveGPUIndex()
	}

	if settings.GateDisabled {
		logger.Get().Debugw("image gate: disabled by config (balancing.image.gateDisabled)",
			"backend", backendID, "host", host)
		return imageGateDecision{Allowed: true, Code: types.ImageGateDisabled}
	}

	snap := r.ensureFreshFor(ctx, backendID)

	if d, denied := r.checkImageModelReady(snap); denied {
		d.res = r
		return d
	}

	if d, denied := r.checkVRAM(snap, host, settings); denied {
		d.res = r
		return d
	}

	// Лок GPU — только в exclusive: offload/dedicated означают, что совместная
	// работа с текстом разрешена (медленнее, но LLM не блокируется).
	if settings.EffectiveCoexistencePolicy() != types.ImageCoexistenceExclusive {
		return imageGateDecision{Allowed: true, Code: types.ImageGateOK}
	}

	holder, ok, waited := r.acquireLock(ctx, backendID, host, gpuIndex, r.loadedModelName(snap))
	if !ok {
		retryAfter := int(r.queueWait().Seconds())
		if retryAfter < 1 {
			retryAfter = 1
		}
		r.gateDeniedTotal.Add(1)
		r.mu.Lock()
		r.gateDenied[ImageGateGPUBusy]++
		r.mu.Unlock()
		logger.Get().Warnw("image GPU lock busy: generation rejected",
			"host", host, "gpu_index", gpuIndex, "backend", backendID,
			"waited_ms", waited.Milliseconds(),
			"queue_wait_sec", int(r.queueWait().Seconds()))
		return imageGateDecision{
			Allowed:       false,
			Status:        http.StatusTooManyRequests,
			Code:          ImageGateGPUBusy,
			Message:       fmt.Sprintf("another image generation is using the GPU on host %s", host),
			Hint:          "retry after the current generation finishes, or set balancing.image.coexistence to offload/dedicated if the image worker does not need the whole GPU",
			RetryAfterSec: retryAfter,
			res:           r,
		}
	}
	if waited > 0 {
		r.lockWaitHist.Observe(waited.Seconds())
	}
	return imageGateDecision{Allowed: true, Code: types.ImageGateOK, res: r, holder: holder}
}

// checkImageModelReady — отказ, если модель на бэкенде не готова.
//
// Почему это НЕ дублирование поведения воркера: sdworker на запрос генерации
// без загруженной модели отвечает 409 model_not_loaded (ленивой загрузки нет).
// Наш 503 с подсказкой «что сделать» полезнее клиенту, а поведение воркера при
// этом не меняется.
func (r *imageResources) checkImageModelReady(s *imageBackendMetrics) (imageGateDecision, bool) {
	allow := imageGateDecision{Allowed: true, Code: types.ImageGateOK}
	if s == nil || !s.contractOK || s.lastErr != "" {
		// Данных нет → не судим (fail-open). Сюда попадают: «голый» sd-server,
		// 401/403 (токен воркера балансер не знает), таймаут, битый JSON.
		return allow, false
	}
	if s.loaded() != nil {
		return allow, false
	}

	names := strings.Join(s.modelNames(), ", ")
	known := names
	if known == "" {
		known = "none"
	}

	if loading := s.loading(); loading != nil || s.state == imageStateLoading {
		name := s.currentModel
		if loading != nil {
			name = loading.Name
		}
		return imageGateDecision{
			Allowed:       false,
			Status:        http.StatusServiceUnavailable,
			Code:          ImageGateModelLoading,
			Message:       fmt.Sprintf("image model %q is still loading on the backend", name),
			Hint:          "wait for the load to finish: GET /api/image/models/load/progress",
			RetryAfterSec: 5,
		}, true
	}

	if failed := s.failed(); failed != nil || s.state == imageStateError {
		name := s.currentModel
		detail := ""
		if failed != nil {
			name = failed.Name
			if failed.Error != "" {
				detail = ": " + failed.Error
			}
		}
		return imageGateDecision{
			Allowed:       false,
			Status:        http.StatusServiceUnavailable,
			Code:          ImageGateModelError,
			Message:       fmt.Sprintf("image model %q failed to load%s", name, detail),
			Hint:          "check the bundle and profile (files, quantization, runtime placement), then reload: POST /api/image/models/load",
			RetryAfterSec: 10,
		}, true
	}

	return imageGateDecision{
		Allowed: false,
		Status:  http.StatusServiceUnavailable,
		Code:    ImageGateModelNotLoaded,
		Message: fmt.Sprintf("no image model is loaded on the image backend (known models: %s)", known),
		Hint:    `load a model first: POST /api/image/models/load {"name":"<model>"}`,
	}, true
}

// checkVRAM — VRAM-гейт. Возвращает (решение, отказ).
//
// ГРАНИЦА ОТВЕТСТВЕННОСТИ (после FIX-2): политику «блокировать ли неизвестную
// оценку» задаёт ЗАМОРОЖЕННЫЙ КОНТРАКТ types.EvaluateImageVRAM (по умолчанию
// неизвестная оценка пропускается, строгость — opt-in
// balancing.image.blockOnUnknownVramEstimate). Балансер не добавляет сюда своей
// политики: он лишь решает, ДОСТАТОЧНО ЛИ ДАННЫХ, чтобы вообще судить о модели.
// Если данных нет (воркер без контракта / ошибка опроса / неизвестная модель) —
// гейт молчит (fail-open): это не «неизвестная оценка модели», а отсутствие
// сведений о ней, и превращать это в 503 значило бы ломать рабочие конфигурации
// (в том числе «голый» sd-server на порту image-бэкенда).
func (r *imageResources) checkVRAM(s *imageBackendMetrics, host string, settings types.ImageResourceSettings) (imageGateDecision, bool) {
	allow := imageGateDecision{Allowed: true, Code: types.ImageGateOK}
	est := r.resolveVRAMEstimate(s)
	// R-Image (2026-10-02, найдено ЖИВЫМ прогоном): сюда мы попадаем только когда
	// модель УЖЕ загружена (dataKnown требует loaded() != nil), то есть веса
	// резидентны. Сравнивать с текущим free полный вес модели нельзя: на
	// 8-гиговой карте после загрузки SD1.5 Q4 (пик ~2.6 GB) свободно ~0.9 GB,
	// и КАЖДЫЙ запрос получал 503 insufficient_vram при полностью готовом
	// движке (в логе: required_mb=2600, free_mb=895).
	// Генерации нужны рабочие буферы (латенты, VAE-декод, внимание) — их и
	// проверяем; вопрос «влезут ли веса» решается на этапе загрузки модели
	// (там оценка сравнивается со свободной VRAM до загрузки).
	gateEst := est
	if est.IsKnown() {
		gateEst.RequiredMB = imageWorkingSetMB(est.RequiredMB)
		gateEst.Detail = est.Detail + "; weights already resident -> gate checks the working set only"
	}
	free, freeSource := r.freeVRAMFor(host, s)
	shared := r.hostHasTextNeighbour(host)
	dataKnown := s != nil && s.contractOK && s.lastErr == "" && s.loaded() != nil

	if !dataKnown {
		r.gateSkippedNoData.Add(1)
		logger.Get().Debugw("image VRAM gate skipped: no trustworthy model data",
			"backend", s.backendIDOrEmpty(),
			"host", host,
			"reason", "worker did not report the /api/image/models contract (or the poll failed)",
			"estimate_source", est.Source,
			"text_neighbour", shared)
		return allow, false
	}

	verdict := types.EvaluateImageVRAM(gateEst, free, settings)
	logger.Get().Infow("image VRAM gate decision",
		"backend", s.backendIDOrEmpty(),
		"host", host,
		"model", r.loadedModelName(s),
		"allowed", verdict.Allowed,
		"reason_code", verdict.ReasonCode,
		"required_mb", gateEst.RequiredMB,
		"weights_mb", est.RequiredMB,
		"estimate_source", est.Source,
		"estimate_detail", gateEst.Detail,
		"free_mb", free,
		"free_source", freeSource,
		"headroom_mb", settings.VramHeadroomMB,
		"text_neighbour", shared,
		"policy", string(settings.EffectiveCoexistencePolicy()))

	if !verdict.Allowed {
		r.gateDeniedTotal.Add(1)
		r.mu.Lock()
		r.gateDenied[verdict.ReasonCode]++
		r.mu.Unlock()
		return imageGateDecision{
			Allowed: false,
			Status:  http.StatusServiceUnavailable,
			Code:    verdict.ReasonCode,
			Message: verdict.Message,
			Hint:    verdict.Hint,
			Verdict: verdict,
		}, true
	}
	r.gateAllowed.Add(1)
	return imageGateDecision{Allowed: true, Code: verdict.ReasonCode, Verdict: verdict}, false
}

// imageWorkingSetMB — оценка РАБОЧЕГО набора генерации, когда веса уже в VRAM.
//
// Heuristic и почему именно такая: после загрузки модели нужно место под латенты
// (масштабируются от разрешения, не от веса), VAE-декод и буферы внимания.
// 20% веса — практичный прокси для типовых 512–1024 px; нижняя граница 256 MB
// защищает мелкие модели от заведомо недостаточной проверки, верхняя 1 GB —
// от того, чтобы «рабочий набор» крупной модели превратился в тот же полный вес.
//
// Это осознанно НЕ оценка «влезут ли веса» — она делается на этапе загрузки
// модели (свободная VRAM до загрузки против оценки из профиля/воркера).
func imageWorkingSetMB(weightsMB int) int {
	ws := weightsMB / 5
	if ws < 256 {
		ws = 256
	}
	if ws > 1024 {
		ws = 1024
	}
	return ws
}

// backendIDOrEmpty — nil-safe ID снимка (для логов).
func (s *imageBackendMetrics) backendIDOrEmpty() string {
	if s == nil {
		return ""
	}
	return s.backendID
}

// loadedModelName — имя загруженной модели (пустая строка, если неизвестно).
func (r *imageResources) loadedModelName(s *imageBackendMetrics) string {
	if s == nil {
		return ""
	}
	if m := s.loaded(); m != nil {
		return m.Name
	}
	return s.currentModel
}

// ============================================================
// Оценка VRAM
// ============================================================

// resolveVRAMEstimate — оценка потребности image-модели в VRAM.
//
// Приоритет источников (заморожен в описании types.ImageVramEstimate):
// профиль балансера → число от воркера → размеры файлов bundle'а × коэффициент
// → «unknown» (RequiredMB=0).
func (r *imageResources) resolveVRAMEstimate(s *imageBackendMetrics) types.ImageVramEstimate {
	loaded := s.loaded()
	if loaded == nil {
		// Модель неизвестна — оценивать нечего. (Гейт в этом случае либо не
		// применяется, либо отказывает кодом unknown_vram_estimate.)
		return types.ImageVramEstimate{Source: "unknown", Detail: "no loaded image model reported by the worker"}
	}

	if p, ok := r.profileFor(loaded.Name); ok && p.VramEstimateMB > 0 {
		return types.ImageVramEstimate{
			RequiredMB: p.VramEstimateMB,
			Source:     "profile",
			Detail:     fmt.Sprintf("balancer profile %q: vramEstimateMb=%d", p.Name, p.VramEstimateMB),
		}
	}

	if loaded.VramEstimateMB > 0 {
		return types.ImageVramEstimate{
			RequiredMB: loaded.VramEstimateMB,
			Source:     "worker",
			Detail:     fmt.Sprintf("worker /api/image/models reported vram_estimate_mb=%d for %q", loaded.VramEstimateMB, loaded.Name),
		}
	}

	bytes := loaded.SizeBytes
	detail := fmt.Sprintf("bundle size_bytes=%d from worker", loaded.SizeBytes)
	if bytes <= 0 {
		if p, ok := r.profileFor(loaded.Name); ok {
			var sum int64
			for _, f := range p.Files {
				sum += f.SizeBytes
			}
			bytes, detail = sum, fmt.Sprintf("sum of profile file sizes=%d", sum)
		}
	}
	if bytes > 0 {
		mb := int(float64(bytes) / (1024 * 1024) * imageVramFileCoefficient)
		if mb > 0 {
			return types.ImageVramEstimate{
				RequiredMB: mb,
				Source:     "files",
				Detail:     fmt.Sprintf("%s × %.2f (weights + VAE/latent overhead)", detail, imageVramFileCoefficient),
			}
		}
	}

	return types.ImageVramEstimate{
		Source: "unknown",
		Detail: fmt.Sprintf("model %q has no vramEstimateMb and no file-size data (profile/worker)", loaded.Name),
	}
}

// freeVRAMFor — свободная VRAM хоста, если её вообще можно узнать.
//
// Порядок: воркер (число в /api/image/models или nvidia-smi-снимок в
// capabilities) → available_vram_mb текстового бэкенда того же хоста → 0
// («неизвестно»). Ноль здесь — НЕ «память кончилась»: EvaluateImageVRAM при
// freeMB <= 0 гейт пропускает.
func (r *imageResources) freeVRAMFor(host string, s *imageBackendMetrics) (int, string) {
	if s != nil && s.vramFreeMB > 0 {
		src := s.vramSource
		if src == "" {
			src = "worker"
		}
		return s.vramFreeMB, src
	}
	if free, src := r.textNeighbourFreeVRAM(host); free > 0 {
		return free, src
	}
	return 0, "unknown"
}

// textNeighbourFreeVRAM — available_vram_mb текстовых бэкендов того же хоста.
//
// ПОЧЕМУ ЭТО НЕ «ВЫДУМАННОЕ ЧИСЛО»: cppworker отдаёт в /api/models живой
// снимок VRAM (bridge.GetGPUInfo → cudaMemGetInfo, см.
// cmd/cppworker/handlers_model.go), поллер балансера кладёт его в
// llamaMetrics[].AvailableVRAMMB. На общем хосте это и есть бюджет карты.
//
// ОГРАНИЧЕНИЯ (сознательные): берётся МАКСИМУМ по бэкендам хоста (на машине с
// несколькими GPU available_vram_mb — сумма по устройствам, и минимум давал бы
// ложные отказы), плюс требование свежести снимка (maxTextVramSnapshotAge).
// Fallback на минимум/конкретное устройство — отдельная задача (нужен GPU-индекс
// в конфиге бэкенда, которого в контракте Phase 1 нет).
func (r *imageResources) textNeighbourFreeVRAM(host string) (int, string) {
	if r == nil || r.proxy == nil || host == "" {
		return 0, ""
	}
	p := r.proxy
	ids := make([]string, 0, 2)
	p.mu.RLock()
	for id, st := range p.backends {
		if st == nil || st.Backend == nil {
			continue
		}
		if st.Backend.Host != host {
			continue
		}
		if normalizeBackendType(st.Backend.Type) == types.BackendTypeImage {
			continue
		}
		if st.Backend.Status == types.StatusOffline {
			continue
		}
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	if len(ids) == 0 {
		return 0, ""
	}

	now := time.Now()
	best, src := 0, ""
	p.metricsMgr.mu.RLock()
	for _, id := range ids {
		lm := p.metricsMgr.llamaMetrics[id]
		if lm == nil || lm.AvailableVRAMMB == 0 {
			continue
		}
		if bm := p.metricsMgr.metrics[id]; bm != nil && !bm.Timestamp.IsZero() &&
			now.Sub(bm.Timestamp) > maxTextVramSnapshotAge {
			continue
		}
		if int(lm.AvailableVRAMMB) > best {
			best = int(lm.AvailableVRAMMB)
			src = "text_neighbour:" + id
		}
	}
	p.metricsMgr.mu.RUnlock()
	return best, src
}

// hostHasTextNeighbour — есть ли на хосте текстовый (не image) бэкенд.
// Именно это делает сценарий «сосуществование» реальным.
func (r *imageResources) hostHasTextNeighbour(host string) bool {
	if r == nil || r.proxy == nil || host == "" {
		return false
	}
	p := r.proxy
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, st := range p.backends {
		if st == nil || st.Backend == nil {
			continue
		}
		if st.Backend.Host != host {
			continue
		}
		if normalizeBackendType(st.Backend.Type) == types.BackendTypeImage {
			continue
		}
		if st.Backend.Status == types.StatusOffline {
			continue
		}
		return true
	}
	return false
}

// profileFor — профиль image-модели на балансере (только чтение).
//
// Профили лежат в отдельном файле (internal/config.ImageModelProfileStore),
// потому что pkg/types/config.go — замороженный контракт Phase 1. Хранилище
// перечитывается не чаще imageProfilesRefreshInterval: оператор правит
// vramEstimateMb без рестарта балансера.
func (r *imageResources) profileFor(name string) (types.ImageModelProfile, bool) {
	if r == nil || strings.TrimSpace(name) == "" {
		return types.ImageModelProfile{}, false
	}
	store := r.profileStore()
	if store == nil {
		return types.ImageModelProfile{}, false
	}
	// Ошибку загрузки профилей НЕ глотаем: битый/нечитаемый файл — это причина,
	// по которой гейт скажет «неизвестно», и оператор обязан её увидеть
	// (иначе выглядит как «профиль есть, а гейт его не видит»).
	if err := store.EnsureLoaded(); err != nil {
		logger.Get().Warnw("image model profiles load failed: VRAM estimate falls back to worker/files",
			"path", store.Path(), "error", err)
		return types.ImageModelProfile{}, false
	}
	if p, ok := store.Get(name); ok {
		return p, true
	}
	// Регистр: воркер отдаёт имя каталога (bundle), профиль мог быть сохранён с
	// другим регистром. Это не «угадывание» — сравнение точное, но без регистра.
	lower := strings.ToLower(name)
	for n, p := range store.List() {
		if strings.ToLower(n) == lower {
			return p, true
		}
	}
	return types.ImageModelProfile{}, false
}

// profileStore — ленивый read-only доступ к хранилищу профилей image-моделей.
func (r *imageResources) profileStore() *config.ImageModelProfileStore {
	if r == nil {
		return nil
	}
	r.profileMu.Lock()
	defer r.profileMu.Unlock()
	now := time.Now()
	if r.profiles == nil {
		r.profiles = config.NewImageModelProfileStore("")
		r.profilesAt = now
		logger.Get().Infow("image model profiles store initialized (read-only)",
			"path", r.profiles.Path())
		return r.profiles
	}
	if now.Sub(r.profilesAt) > imageProfilesRefreshInterval {
		if err := r.profiles.Load(); err != nil {
			logger.Get().Warnw("image model profiles reload failed",
				"path", r.profiles.Path(), "error", err)
		}
		r.profilesAt = now
	}
	return r.profiles
}

// ============================================================
// Лок GPU: захват/освобождение/предохранитель
// ============================================================

// acquireLock — взять лок GPU (host + индекс, когда индекс задан), ожидая
// освобождения не дольше queueWait. Возвращает (holder, ok, waited).
func (r *imageResources) acquireLock(ctx context.Context, backendID, host string, gpuIndex int, model string) (*imageLockHolder, bool, time.Duration) {
	if r == nil {
		return nil, true, 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := imageLockKey(host, gpuIndex)
	wait := r.queueWait()
	start := time.Now()
	deadline := start.Add(wait)

	for {
		r.mu.Lock()
		lk := r.locks[key]
		if lk == nil {
			lk = &imageHostLock{host: host, gpuIndex: gpuIndex}
			r.locks[key] = lk
		}
		// Конфликт ищем по ВСЕМ локам хоста, а не только по своему ключу:
		// хостовый лок (индекс неизвестен) и лок другой карты — разные ключи,
		// но первый обязан блокировать всех (см. locksConflict).
		conflict := r.conflictLocked(lk)
		if conflict == nil {
			h := &imageLockHolder{
				res:        r,
				done:       make(chan struct{}),
				key:        key,
				host:       host,
				gpuIndex:   gpuIndex,
				backendID:  backendID,
				model:      model,
				acquiredAt: time.Now(),
			}
			lk.held = true
			lk.holder = h
			lk.releaseCh = make(chan struct{})
			r.mu.Unlock()

			fuse := r.lockFuse()
			if fuse > 0 {
				h.fuse = time.AfterFunc(fuse, func() { r.forceRelease(h) })
			}
			r.lockAcquired.Add(1)
			waited := time.Since(start)
			if waited > 0 {
				r.lockWaits.Add(1)
				r.lockWaitSumMS.Add(waited.Milliseconds())
			}
			logger.Get().Infow("image GPU lock acquired",
				"host", host,
				"gpu_index", gpuIndex,
				"lock_key", key,
				"backend", backendID,
				"model", model,
				"waited_ms", waited.Milliseconds(),
				"fuse_sec", int(fuse.Seconds()))
			return h, true, waited
		}
		holder := conflict.holder
		releaseCh := conflict.releaseCh
		r.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			r.lockRejected.Add(1)
			return nil, false, time.Since(start)
		}
		if holder != nil {
			logger.Get().Debugw("image GPU lock busy, waiting",
				"host", host,
				"gpu_index", gpuIndex,
				"held_by_backend", holder.backendID,
				"held_gpu_index", holder.gpuIndex,
				"held_ms", time.Since(holder.acquiredAt).Milliseconds(),
				"remaining_ms", remaining.Milliseconds())
		}

		nap := remaining
		if nap > imageLockWaitPoll {
			nap = imageLockWaitPoll
		}
		timer := time.NewTimer(nap)
		select {
		case <-releaseCh:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			r.lockRejected.Add(1)
			return nil, false, time.Since(start)
		}
		timer.Stop()
	}
}

// conflictLocked — удерживаемый лок, конфликтующий с lk (mu удерживается).
// Выбор детерминирован (см. lockOrderLess).
func (r *imageResources) conflictLocked(lk *imageHostLock) *imageHostLock {
	if lk == nil {
		return nil
	}
	// СВОЙ ключ занят — это и есть конфликт (другой запрос держит ту же карту).
	// Отдельная ветка обязательна: в общем цикле свой лок пропускается как
	// «сам себя», и без неё повторный захват того же (host, gpu) проходил бы
	// молча — воспроизведено тестом TestImageLock_ExclusiveSecondRequestRejected.
	if lk.held {
		return lk
	}
	var best *imageHostLock
	for _, other := range r.locks {
		if other == lk || !other.held {
			continue
		}
		if !locksConflict(lk, other) {
			continue
		}
		if best == nil || lockOrderLess(other, best) {
			best = other
		}
	}
	return best
}

// unlock — снять лок (вызывается из imageLockHolder.Release под sync.Once).
func (r *imageResources) unlock(h *imageLockHolder) {
	if r == nil || h == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lk := r.locks[h.key]
	if lk == nil || !lk.held || lk.holder != h {
		return
	}
	lk.held = false
	lk.holder = nil
	if lk.releaseCh != nil {
		close(lk.releaseCh)
		lk.releaseCh = nil
	}
}

// forceRelease — предохранитель: лок держат дольше ExclusiveLockTimeoutSec.
//
// Зачем: зависшая генерация (или потерянный обработчик) иначе заблокировала бы
// текстовый трафик на хосте навсегда. Освобождаем принудительно и пишем WARN —
// это операторское событие, а не рутина.
func (r *imageResources) forceRelease(h *imageLockHolder) {
	if r == nil || h == nil {
		return
	}
	r.mu.Lock()
	lk := r.locks[h.key]
	stillHeld := lk != nil && lk.held && lk.holder == h
	r.mu.Unlock()
	if !stillHeld {
		return // уже освобождён штатно — предохранитель не нужен
	}
	r.lockForced.Add(1)
	logger.Get().Warnw("image GPU lock held too long — forced release (предохранитель exclusiveLockTimeoutSec)",
		"host", h.host,
		"gpu_index", h.gpuIndex,
		"backend", h.backendID,
		"model", h.model,
		"held_sec", int(time.Since(h.acquiredAt).Seconds()),
		"limit_sec", int(r.lockFuse().Seconds()))
	h.Release("fuse_timeout")
}

// releaseFromRequest — освобождение из HTTP-обработчика: no-op, если лок уже
// передан сторожу асинхронной генерации.
func (r *imageResources) releaseFromRequest(h *imageLockHolder) {
	if h == nil {
		return
	}
	if h.handedOff.Load() {
		logger.Get().Debugw("image GPU lock release skipped: ownership handed to async watcher",
			"host", h.host, "gpu_index", h.gpuIndex, "backend", h.backendID, "model", h.model)
		return
	}
	h.Release("request_done")
}

// gpuLockHeld — занят ли хост ЛЮБЫМ локом (без учёта индекса GPU): «есть ли на
// хосте активная генерация вообще». Используется диагностикой/тестами; боевой
// путь текстовой стороны — gpuLockHeldFor с индексом.
func (r *imageResources) gpuLockHeld(host string) bool {
	if r == nil || host == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, lk := range r.locks {
		if lk != nil && lk.held && lk.host == host {
			return true
		}
	}
	return false
}

// gpuLockHeldFor — блокирует ли удерживаемый лок эту (host, gpuIndex) пару.
//
// Правило то же, что и у взятия лока (locksConflict): лок другой карты того же
// хоста не блокирует, а лок с неизвестным индексом (или проверка без индекса)
// блокирует весь хост — консервативно.
func (r *imageResources) gpuLockHeldFor(host string, gpuIndex int) bool {
	if r == nil || host == "" {
		return false
	}
	probe := &imageHostLock{host: host, gpuIndex: gpuIndex}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, lk := range r.locks {
		if lk != nil && lk.held && locksConflict(probe, lk) {
			return true
		}
	}
	return false
}

// noteTextSlotWithheld — счётчик «текстовый кандидат пропущен из-за image-лока».
func (r *imageResources) noteTextSlotWithheld() {
	if r != nil {
		r.textSlotsWithheld.Add(1)
	}
}

// noteTextWaitCapped — счётчик «ожидание текстового запроса урезано до
// balancing.image.queueWaitTimeoutSec, потому что единственная причина
// отсутствия кандидата — активный image-лок» (см. imageLockWaitCap).
func (r *imageResources) noteTextWaitCapped() {
	if r != nil {
		r.textWaitCapped.Add(1)
	}
}

// watchAsyncGeneration — сторож асинхронной генерации.
//
// Зачем: нативный POST /api/image/generate отвечает 202 сразу, а генерация
// продолжается в воркере. Освободить лок в момент ответа (как для
// синхронных /v1/images/* и /sdapi/v1/*) означало бы пустить текст на GPU
// ВНУТРЬ генерации. Сторож опрашивает active_queries воркера и снимает лок,
// когда генерация закончилась (или по предохранителю/остановке).
func (r *imageResources) watchAsyncGeneration(h *imageLockHolder) {
	if r == nil || h == nil {
		return
	}
	h.HandOff()
	go func() {
		defer h.Release("async_generation_finished")
		activeSeen := false
		idle := 0
		pending := 0
		failures := 0
		ticker := time.NewTicker(imageAsyncWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-h.done:
				return
			case <-r.stopCh:
				return
			case <-ticker.C:
			}

			ctx, cancel := context.WithTimeout(context.Background(), r.probeTimeout())
			snap := r.refresh(ctx, h.backendID)
			cancel()

			if snap == nil || !snap.contractOK || snap.lastErr != "" {
				failures++
				if failures >= imageAsyncMaxPollErrors {
					logger.Get().Warnw("async image generation watch: worker unreachable, releasing GPU lock",
						"backend", h.backendID, "host", h.host, "failures", failures)
					return
				}
				continue
			}
			failures = 0

			if snap.activeQueries() > 0 {
				activeSeen = true
				idle = 0
				continue
			}
			if activeSeen {
				idle++
				if idle >= imageAsyncIdlePolls {
					logger.Get().Infow("async image generation finished (active_queries=0)",
						"backend", h.backendID, "host", h.host)
					return
				}
				continue
			}
			// Генерация ещё не началась (или уже закончилась мгновенно) —
			// ждём старта ограниченное время, чтобы не держать лок вечно.
			pending++
			if pending >= imageAsyncStartupGracePolls {
				logger.Get().Infow("async image generation watch: no activity observed, releasing GPU lock",
					"backend", h.backendID, "host", h.host, "polls", pending)
				return
			}
		}
	}()
}

// ============================================================
// Наблюдаемость
// ============================================================

// snapshotMetrics — срез состояния Phase 6 для /api/metrics и диагностики.
// clusterSnapshot — снимок состояния image-бэкенда для WebUI/Monitor
// (types.BackendMetrics.Image).
//
// R-Image (2026-10-02): данные уже собираются для гейта VRAM, поэтому UI
// получает их без дополнительных запросов. nil — если по бэкенду нет снимка
// (например, он только что зарегистрирован и поллер ещё не успел).
func (r *imageResources) clusterSnapshot(backendID string) *types.ImageBackendMetrics {
	if r == nil || backendID == "" {
		return nil
	}
	r.mu.Lock()
	s := r.snapshots[backendID]
	r.mu.Unlock()
	if s == nil {
		return nil
	}

	out := &types.ImageBackendMetrics{
		State:        s.state,
		CurrentModel: s.currentModel,
		VramFreeMB:   s.vramFreeMB,
		VramTotalMB:  s.vramTotalMB,
		UpdatedAt:    s.at,
		LastError:    s.lastErr,
	}
	// Модели отдаём ВСЕ (включая незагруженные): оператору нужно видеть, что
	// вообще установлено и что можно загрузить, а не только текущую модель.
	for _, m := range s.models {
		out.Models = append(out.Models, types.ImageModelBrief{
			Name:           m.Name,
			State:          m.State,
			Family:         m.Family,
			SizeBytes:      m.SizeBytes,
			VramEstimateMB: m.VramEstimateMB,
			ActiveQueries:  m.ActiveQueries,
			Error:          m.Error,
		})
	}
	return out
}

func (r *imageResources) snapshotMetrics() map[string]interface{} {
	if r == nil {
		return map[string]interface{}{}
	}
	settings := r.settings()

	r.mu.Lock()
	held := 0
	for _, lk := range r.locks {
		if lk != nil && lk.held {
			held++
		}
	}
	byReason := make(map[string]int64, len(r.gateDenied))
	for k, v := range r.gateDenied {
		byReason[k] = v
	}
	perBackend := make(map[string]interface{}, len(r.snapshots))
	for id, s := range r.snapshots {
		if s == nil {
			continue
		}
		entry := map[string]interface{}{
			"state":            s.state,
			"contract_ok":      s.contractOK,
			"current_model":    s.currentModel,
			"active_queries":   s.activeQueries(),
			"vram_free_mb":     s.vramFreeMB,
			"vram_total_mb":    s.vramTotalMB,
			"vram_source":      s.vramSource,
			"snapshot_age_sec": int(time.Since(s.at).Seconds()),
		}
		if m := s.loaded(); m != nil {
			entry["loaded_model"] = m.Name
			entry["loaded_vram_estimate_mb"] = m.VramEstimateMB
			entry["loaded_bundle_bytes"] = m.SizeBytes
		}
		if s.lastErr != "" {
			entry["error"] = s.lastErr
		}
		perBackend[id] = entry
	}
	r.mu.Unlock()

	waits := r.lockWaits.Load()
	avgWaitMS := int64(0)
	if waits > 0 {
		avgWaitMS = r.lockWaitSumMS.Load() / waits
	}

	// Строгость к неизвестной оценке НЕ читаем из поля напрямую: спросим
	// замороженный контракт. Так отчёт метрик не зависит от имени настройки
	// (FIX-2 переводит её в opt-in BlockOnUnknownVRAMEstimate) и всегда
	// показывает ФАКТИЧЕСКОЕ поведение гейта.
	unknownBlocked := !types.EvaluateImageVRAM(types.ImageVramEstimate{}, 0, settings).Allowed

	return map[string]interface{}{
		"coexistence_policy":         string(settings.EffectiveCoexistencePolicy()),
		"gate_disabled":              settings.GateDisabled,
		"vram_headroom_mb":           settings.VramHeadroomMB,
		"unknown_estimate_blocked":   unknownBlocked,
		"queue_wait_timeout_sec":     int(r.queueWait().Seconds()),
		"exclusive_lock_timeout_sec": int(r.lockFuse().Seconds()),
		"gate_allowed_total":         r.gateAllowed.Load(),
		"gate_denied_total":          r.gateDeniedTotal.Load(),
		"gate_denied_by_reason":      byReason,
		"gate_skipped_no_data_total": r.gateSkippedNoData.Load(),
		"lock_acquired_total":        r.lockAcquired.Load(),
		"lock_held":                  held,
		"lock_wait_total":            waits,
		"lock_wait_ms_avg":           avgWaitMS,
		"lock_wait_time_histogram":   r.lockWaitHist.Snapshot(),
		"lock_rejected_total":        r.lockRejected.Load(),
		"lock_force_released_total":  r.lockForced.Load(),
		"text_slots_withheld_total":  r.textSlotsWithheld.Load(),
		// Phase 6 follow-up (п.2): ожидание текстового запроса урезано до
		// balancing.image.queueWaitTimeoutSec (единственная причина — image-лок).
		"text_wait_capped_total": r.textWaitCapped.Load(),
		"backends":               perBackend,
	}
}
