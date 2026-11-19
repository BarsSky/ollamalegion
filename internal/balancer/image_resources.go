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
//     когда индекс ИЗВЕСТЕН у ОБЕИХ сторон (см. locksConflict); иначе — host.
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
//     (unified_queue.go) со своим лимитом LB_ADMISSION_WAIT_SEC и 503 +
//     Retry-After по его истечении.
package balancer

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// gpuRef — индекс GPU для ключа лока: сам индекс + признак «ИЗВЕСТЕН».
//
// Двух полей вместо «int, где 0 = неизвестно» требует сама модель предметной
// области: 0 — валидный индекс первой карты, и без флага объявить её нельзя
// (см. types.Backend.GPUIndex, types.Backend.EffectiveGPUIndex). known == false
// означает «какой картой пользуется сторона — неизвестно», и тогда лок
// остаётся хостовым: сузить защиту на догадке нельзя (текст на карту генерации
// = OOM внутри движка).
type gpuRef struct {
	index int
	known bool
}

// knownGPU — явно названная карта (в том числе 0).
func knownGPU(index int) gpuRef { return gpuRef{index: index, known: true} }

// unknownGPU — индекс неизвестен: лок на весь хост.
func unknownGPU() gpuRef { return gpuRef{} }

// imageHostLock — состояние лока одной GPU (или всего хоста). Все поля защищены
// imageResources.mu.
type imageHostLock struct {
	// holder — владелец лока (nil при свободном локе).
	holder *imageLockHolder
	// releaseCh — канал, закрываемый при освобождении: будит ожидающих.
	releaseCh chan struct{}
	// host — хост лока (нужен для проверки конфликтов между ключами хоста).
	host string
	// gpu — карта лока; known == false = «неизвестно» = лок на весь хост.
	gpu gpuRef
	// held — занят ли лок.
	held bool
}

// imageLockKey — ключ лока в карте r.locks.
//
// ЗАЧЕМ СОСТАВНОЙ КЛЮЧ (R-Image follow-up, 2026-10-02): раньше лок брался по
// host, и на multi-GPU хосте генерация на одной карте блокировала текстовый
// бэкенд на другой. Теперь ключ — host + индекс GPU, когда индекс ИЗВЕСТЕН; при
// неизвестном индексе ключ равен хосту (прежнее поведение) — «неизвестно» не
// даёт права сузить лок.
//
// Явный GPU 0 даёт ключ host#gpu0 (а не host): иначе первая карта снова
// оказалась бы «хостовым» локом и блокировала бы индексированных соседей по
// одному лишь совпадению ключа.
func imageLockKey(host string, gpu gpuRef) string {
	if !gpu.known {
		return host
	}
	return host + "#gpu" + strconv.Itoa(gpu.index)
}

// locksConflict — пересекаются ли два лока по GPU.
//
// КОНСЕРВАТИЗМ (почему именно так): если индекс НЕИЗВЕСТЕН ХОТЯ БЫ У ОДНОЙ
// стороны, считаем, что она занимает весь хост — какой GPU она использует, нам
// неизвестно, а «пропустить» текстовый запрос на карту генерации означает OOM
// внутри движка (ровно то, от чего защищает политика exclusive). Сужаем лок
// только когда ОБЕ стороны назвали свою карту (и тогда 0 — такая же карта, как
// любая другая).
func locksConflict(a, b *imageHostLock) bool {
	if a == nil || b == nil || a.host != b.host {
		return false
	}
	if !a.gpu.known || !b.gpu.known {
		return true
	}
	return a.gpu.index == b.gpu.index
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
	if a.gpu.known != b.gpu.known {
		return !a.gpu.known
	}
	if a.gpu.index != b.gpu.index {
		return a.gpu.index < b.gpu.index
	}
	return imageLockKey(a.host, a.gpu) < imageLockKey(b.host, b.gpu)
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
	gpu        gpuRef
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
			"gpu_index", h.gpu.index,
			"gpu_known", h.gpu.known,
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
