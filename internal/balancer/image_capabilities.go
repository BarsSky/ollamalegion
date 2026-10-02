// image_capabilities.go — Phase 4 (discovery): агрегация возможностей
// image-бэкендов (sdworker / sd.cpp) в один ответ для клиентов и LLM-инструмента.
//
// ЗАЧЕМ: клиенту (страница Image, агент с инструментом generate_image, внешний
// SDK) нужен ОДИН вызов, который расскажет, что умеет кластер: какие samplers/
// schedulers/loras/upscalers доступны, какие границы генерации действуют, какие
// модели лежат на воркерах и с какими дефолтами. Без агрегата клиент был бы
// обязан сам обойти все image-бэкенды (а он про них может не знать).
//
// ПРИНЦИПЫ (сознательные решения, не случайность):
//   - READ-ONLY. Здесь нет ни load/unload, ни генерации — только GET.
//     Поэтому агрегатор безопасно вызывать из discovery-хендлеров.
//   - НИКОГДА не возвращает ошибку. Недоступный воркер — это ДАННЫЕ
//     (Backends.Errors + Backends.Probes[i].Error), а не отказ всего ответа:
//     иначе один упавший воркер ломал бы discovery всему кластеру, хотя
//     остальные живы. Пустой кластер (нет image_cpp вовсе) — тоже валидный
//     ответ: backends.total=0 и пустые списки.
//   - Мёрж лимитов КОНСЕРВАТИВНЫЙ: клиент, соблюдающий агрегированные границы,
//     обязан уложиться в границы КАЖДОГО бэкенда (см. mergeLimits).
//   - Короткий TTL-кэш (см. imageCapabilitiesCacheTTL): страница/агент дёргает
//     endpoint часто, а каждый вызов — это N HTTP-запросов в воркеры.
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Константы опроса и кэша
// ============================================================

// imageCapabilitiesCacheTTL — TTL кэша агрегата.
//
// ПОЧЕМУ ИМЕННО 8 СЕКУНД:
//   - discovery дёргают часто (открытие страницы Image, выбор модели в агенте,
//     Connect у клиента), а каждый промах кэша = N HTTP-запросов в воркеры;
//   - сами данные меняются редко: samplers/schedulers/limits фиксированы
//     сборкой движка, список моделей — только при load/unload/скачивании
//     bundle'а, то есть заметно реже 8 с;
//   - 8 с < периода health-check (10 с), поэтому статус бэкенда в агрегате не
//     «стареет» сильнее, чем его знает сам балансер, и переход
//     healthy→unhealthy проявляется в агрегате в пределах одного health-цикла;
//   - верхняя граница из плана (5–10 с): больше — клиент слишком долго не
//     видит свежезагруженную модель и приходит к оператору с «модель не
//     появилась».
const imageCapabilitiesCacheTTL = 8 * time.Second

// imageCapabilitiesProbeTimeout — таймаут ОДНОГО опроса воркера.
//
// Почему 5 с (как у health-probe, internal/balancer/health.go:222): это
// дешёвый GET, но воркер в момент load может отвечать медленно. Пробы идут
// ПАРАЛЛЕЛЬНО, поэтому общий бюджет вызова ≈ этот таймаут, а не N × 5 с.
const imageCapabilitiesProbeTimeout = 5 * time.Second

// maxImageCapabilitiesBody — предохранитель на размер ответа воркера.
// capabilities весит единицы КБ, но содержит списки LoRA/upscaler'ов, которые
// на большой инсталляции могут разрастись; читать бесконечное тело нельзя.
const maxImageCapabilitiesBody = 8 << 20

// Пути опроса. Основной — наш воркер sdworker (Phase 3); фолбэк — «голый»
// sd-server, у которого нет /api/image/* (см. health.go:196-217: на порту
// 18093 может стоять и то, и другое).
const (
	imageWorkerCapabilitiesPath = "/api/image/capabilities"
	imageEngineCapabilitiesPath = "/sdcpp/v1/capabilities"
)

// ============================================================
// Публичные типы агрегата (JSON-контракт /api/v1/image/capabilities)
// ============================================================

// ImageCapabilitiesAggregate — результат агрегации по всем опрошенным
// image-бэкендам.
type ImageCapabilitiesAggregate struct {
	// GeneratedAt — момент СБОРА данных (не момент ответа из кэша).
	GeneratedAt time.Time `json:"generatedAt"`
	// Cached — true, если ответ отдан из TTL-кэша (данные те же, что и при
	// последнем сборе). Клиенту полезно для отладки «почему модель не видна».
	Cached bool `json:"cached"`
	// CacheTTLSeconds — текущий TTL кэша (секунды): клиент понимает, насколько
	// агрегат может отставать от реальности.
	CacheTTLSeconds int `json:"cache_ttl_seconds"`

	Backends ImageCapabilitiesBackends `json:"backends"`

	// Samplers/Schedulers — объединение по бэкендам с указанием источников.
	Samplers   []ImageCapabilitySupport `json:"samplers"`
	Schedulers []ImageCapabilitySupport `json:"schedulers"`
	// Loras/Upscalers — объединение с сохранением доп. полей движка.
	Loras     []ImageLoraSupport     `json:"loras"`
	Upscalers []ImageUpscalerSupport `json:"upscalers"`

	Limits ImageAggregatedLimits `json:"limits"`

	// Models — модели ПО БЭКЕНДАМ (одно имя может лежать на нескольких).
	Models []ImageAggregatedModel `json:"models"`
	// AllModels — уникальные имена (отсортированы) — то, что нужно клиенту для
	// выпадающего списка/JSON-Schema.
	AllModels []string `json:"all_models"`

	// Warnings — предупреждения воркеров + причины пропуска бэкендов.
	Warnings []string `json:"warnings,omitempty"`
}

// ImageCapabilitiesBackends — сводка опроса: сколько бэкендов в кластере,
// сколько опрошено/ответило/упало и почему.
type ImageCapabilitiesBackends struct {
	// Total — ВСЕ image_cpp-бэкенды кластера (включая нездоровые).
	Total int `json:"total"`
	// Probed — сколько реально опрошено (здоровые + transient starting).
	Probed int `json:"probed"`
	// Answered — ответили 2xx и валидным JSON.
	Answered int `json:"answered"`
	// Failed — опрошены, но недоступны/ответили ошибкой/невалидным JSON.
	Failed int `json:"failed"`
	// Skipped — не опрошены по статусу (unhealthy/offline/draining/...).
	Skipped int `json:"skipped"`
	// Errors — только причины ошибок (плоский список для алертов/логов).
	Errors []ImageBackendProbeError `json:"errors,omitempty"`
	// Probes — по одной записи на КАЖДЫЙ image-бэкенд кластера (включая
	// пропущенные): оператор видит, что именно не так с конкретным воркером.
	Probes []ImageBackendProbe `json:"probes"`
}

// ImageBackendProbeError — причина недоступности одного бэкенда.
type ImageBackendProbeError struct {
	BackendID string `json:"backendId"`
	Error     string `json:"error"`
}

// ImageBackendProbe — состояние опроса одного бэкенда.
type ImageBackendProbe struct {
	BackendID string `json:"backendId"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	// Status — статус бэкенда в балансере (healthy/unhealthy/...).
	Status string `json:"status"`
	// Probed=false + Skipped=true + Reason — бэкенд не опрашивался.
	Probed  bool   `json:"probed"`
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// OK=true — ответ 2xx и валидный JSON.
	OK         bool `json:"ok"`
	HTTPStatus int  `json:"httpStatus,omitempty"`
	// Source: "worker" (/api/image/capabilities) | "engine" (/sdcpp/v1/capabilities).
	Source string `json:"source,omitempty"`
	// State/Ready/Model — из ответа воркера (у «голого» движка state пуст).
	State     string `json:"state,omitempty"`
	Ready     bool   `json:"ready,omitempty"`
	Model     string `json:"model,omitempty"`
	LatencyMS int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// ImageCapabilitySupport — одно значение (sampler/scheduler) и кто его умеет.
type ImageCapabilitySupport struct {
	Name string `json:"name"`
	// Backends — ID бэкендов, сообщивших это значение (отсортированы).
	Backends []string `json:"backends"`
}

// ImageLoraSupport — LoRA и её источники.
type ImageLoraSupport struct {
	Name     string   `json:"name"`
	Path     string   `json:"path,omitempty"`
	Backends []string `json:"backends"`
}

// ImageUpscalerSupport — апскейлер и его источники.
type ImageUpscalerSupport struct {
	Name         string   `json:"name"`
	Model        bool     `json:"model,omitempty"`
	ImageUpscale bool     `json:"image_upscale,omitempty"`
	Backends     []string `json:"backends"`
}

// ImageAggregatedModel — модель на конкретном бэкенде.
type ImageAggregatedModel struct {
	BackendID      string                 `json:"backendId"`
	Name           string                 `json:"name"`
	State          string                 `json:"state"`
	Family         string                 `json:"family"`
	SizeBytes      int64                  `json:"size_bytes,omitempty"`
	VramEstimateMB int                    `json:"vram_estimate_mb,omitempty"`
	ActiveQueries  int64                  `json:"active_queries"`
	Disabled       bool                   `json:"disabled,omitempty"`
	Defaults       types.ImageGenDefaults `json:"defaults"`
}

// ImageAggregatedLimits — консервативный мёрж лимитов по бэкендам.
//
// 0/пусто = ни один бэкенд это поле не сообщил (не «ноль»!). Так клиент
// отличает «движок не сказал» от «запрещено».
type ImageAggregatedLimits struct {
	MinWidth  int `json:"min_width"`
	MaxWidth  int `json:"max_width"`
	MinHeight int `json:"min_height"`
	MaxHeight int `json:"max_height"`
	// SizeMultiple — общий шаг сетки: LCM по бэкендам (см. mergeLimits).
	SizeMultiple  int     `json:"size_multiple"`
	MaxBatchCount int     `json:"max_batch_count"`
	MinSteps      int     `json:"min_steps"`
	MaxSteps      int     `json:"max_steps"`
	MaxCFGScale   float64 `json:"max_cfg_scale,omitempty"`
	// MaxQueueSize — СУММА очередей (кластер может принять столько задач).
	MaxQueueSize int `json:"max_queue_size"`
	// QueueInFlight — суммарно генерируется прямо сейчас (живая нагрузка).
	QueueInFlight int `json:"queue_in_flight"`
	// CompletedJobTTLSeconds — минимум по бэкендам (результат живёт не дольше
	// самого «короткого» воркера).
	CompletedJobTTLSeconds int `json:"completed_job_ttl_seconds,omitempty"`
	// GenerationTimeoutSeconds — минимум по бэкендам (гарантия «ответ придёт
	// не позже»: клиент с таймаутом меньше этого значения может не дождаться).
	GenerationTimeoutSeconds int `json:"generation_timeout_seconds,omitempty"`
	// Флаги возможностей: AND по бэкендам, nil/отсутствует = никто не сообщил.
	CancelQueuedOnly          *bool `json:"cancel_queued_only,omitempty"`
	SeedAlwaysPositive        *bool `json:"seed_always_positive,omitempty"`
	SupportsResponseFormatURL *bool `json:"supports_response_format_url,omitempty"`
	// SourceBackends — чьи лимиты учтены (пусто = лимитов не сообщил никто).
	SourceBackends []string `json:"source_backends"`
}

// ============================================================
// Публичный API
// ============================================================

// AggregateImageCapabilities — собрать возможности всех ЗДОРОВЫХ image-бэкендов.
//
// tokenFor — резолвер токена бэкенда (nil = без токена). Параметр, а не env-lookup
// внутри балансера: у API-слоя уже есть imageWorkerAPIToken с правильным
// приоритетом (бэкенд → IMAGEWORKER_API_TOKEN → CPPWORKER_API_TOKEN → LB_API_TOKEN),
// и дублировать эту логику здесь не нужно.
//
// Возвращает ВСЕГДА непустой агрегат (даже если ни одного image-бэкенда нет).
func (p *Proxy) AggregateImageCapabilities(ctx context.Context, tokenFor func(backendID string) string) *ImageCapabilitiesAggregate {
	if p == nil {
		return emptyImageCapabilitiesAggregate()
	}
	if ctx == nil {
		ctx = context.Background()
	}

	backends := make([]types.Backend, 0)
	for _, b := range p.GetAllBackends() {
		if b.Type == types.BackendTypeImage {
			backends = append(backends, b)
		}
	}
	// Детерминированный порядок: ответ API не должен «дрожать» между вызовами,
	// иначе тесты и диффы контракта становятся бесполезными.
	sort.Slice(backends, func(i, j int) bool { return backends[i].ID < backends[j].ID })

	signature := imageCapabilitiesSignature(backends)

	// Кэш держит мьютекс НА ВРЕМЯ ОПРОСА (single-flight): два параллельных
	// клиента не удваивают нагрузку на воркеры. Блокировка короткая — пробы
	// ограничены imageCapabilitiesProbeTimeout и идут параллельно.
	imageCapsCacheMu.Lock()
	defer imageCapsCacheMu.Unlock()

	now := time.Now()
	if imageCapsCache.valid && imageCapsCache.signature == signature &&
		now.Sub(imageCapsCache.at) < imageCapabilitiesCacheTTLNow() {
		// Копия структуры (не указатель): Cached=true не должен «залипать» в
		// кэше. Срезы внутри копии общие с кэшем — потребители (JSON-сериализация
		// в хендлерах) их только читают; правило «кэш иммутабелен» держим здесь.
		hit := *imageCapsCache.value
		hit.Cached = true
		return &hit
	}

	agg := p.probeImageCapabilities(ctx, backends, tokenFor)
	imageCapsCache = imageCapsCacheEntry{
		valid:     true,
		signature: signature,
		at:        now,
		value:     agg,
	}
	return agg
}

// ResetImageCapabilitiesCache — сброс кэша агрегата (тесты и ручная
// диагностика оператором после load/unload модели).
func ResetImageCapabilitiesCache() {
	imageCapsCacheMu.Lock()
	defer imageCapsCacheMu.Unlock()
	imageCapsCache = imageCapsCacheEntry{}
}

// ============================================================
// Кэш
// ============================================================

// imageCapabilitiesCacheTTLOverride — переопределение TTL для тестов
// (0 = использовать imageCapabilitiesCacheTTL). Отдельная переменная вместо
// инъекции часов: TTL-тест «протухает» через реальный sleep, без мока времени
// и без гонок с параллельными тестами пакета.
var imageCapabilitiesCacheTTLOverride time.Duration

type imageCapsCacheEntry struct {
	valid     bool
	signature string
	at        time.Time
	value     *ImageCapabilitiesAggregate
}

var (
	imageCapsCacheMu sync.Mutex
	imageCapsCache   imageCapsCacheEntry
)

// imageCapabilitiesCacheTTLNow — эффективный TTL (с учётом тестового override).
func imageCapabilitiesCacheTTLNow() time.Duration {
	if imageCapabilitiesCacheTTLOverride > 0 {
		return imageCapabilitiesCacheTTLOverride
	}
	return imageCapabilitiesCacheTTL
}

// imageCapabilitiesSignature — отпечаток ТОПОЛОГИИ image-бэкендов.
//
// ПОЧЕМУ ПОДПИСЬ, А НЕ ПРОСТО TTL: кэш пакетный (Proxy не наш тип для правки —
// см. зоны ответственности), и без подписи один кластер/тест получал бы данные
// другого. Подпись включает статус, поэтому переход healthy→unhealthy сразу
// инвалидирует кэш (иначе агрегат 8 с показывал бы модели умершего воркера).
func imageCapabilitiesSignature(backends []types.Backend) string {
	var sb strings.Builder
	for _, b := range backends {
		sb.WriteString(b.ID)
		sb.WriteByte('@')
		sb.WriteString(b.Host)
		sb.WriteByte(':')
		sb.WriteString(fmt.Sprintf("%d", b.EffectiveImagePort()))
		sb.WriteByte('/')
		sb.WriteString(string(b.Status))
		sb.WriteByte(';')
	}
	return sb.String()
}

// ============================================================
// Опрос
// ============================================================

// imageCapProbeableStatuses — статусы, при которых бэкенд опрашивается.
//
// healthy — очевидно. starting добавлен осознанно: это статус «до первого
// health-check» (backend_registry.go:90-91), в котором бэкенд живёт первые
// ~10 с после рестарта балансера или регистрации воркера. Опрос read-only,
// дешёвый и с таймаутом, зато discovery не пуст в это окно. Нездоровые
// (unhealthy/offline/draining/ollama_unavailable) НЕ опрашиваются: их видно в
// Backends.Probes со Skipped=true и причиной.
var imageCapProbeableStatuses = map[types.BackendStatus]bool{
	types.StatusHealthy:  true,
	types.StatusStarting: true,
}

// imageCapProbeOutcome — результат опроса одного бэкенда (для мёржа).
type imageCapProbeOutcome struct {
	probe   ImageBackendProbe
	payload *imageCapabilitiesPayload
}

// probeImageCapabilities — параллельный опрос всех бэкендов + мёрж.
//
// Мёрж выполняется ПОСЛЕ wg.Wait() в порядке отсортированных бэкендов: так
// результат детерминирован (важно для «кто первый сообщил path LoRA» и для
// порядка Backends.Probes), чего не дал бы мёрж из горутин.
func (p *Proxy) probeImageCapabilities(
	ctx context.Context,
	backends []types.Backend,
	tokenFor func(backendID string) string,
) *ImageCapabilitiesAggregate {
	agg := emptyImageCapabilitiesAggregate()
	agg.GeneratedAt = time.Now().UTC()
	agg.CacheTTLSeconds = int(imageCapabilitiesCacheTTLNow().Seconds())
	agg.Backends.Total = len(backends)

	outcomes := make([]*imageCapProbeOutcome, len(backends))
	var wg sync.WaitGroup
	for i := range backends {
		token := ""
		if tokenFor != nil {
			token = tokenFor(backends[i].ID)
		}
		wg.Add(1)
		go func(idx int, b types.Backend, token string) {
			defer wg.Done()
			outcomes[idx] = p.probeImageBackendCapabilities(ctx, b, token)
		}(i, backends[i], token)
	}
	wg.Wait()

	union := newImageCapsUnion()
	var limAcc imageCapsLimitsAccumulator

	for _, oc := range outcomes {
		if oc == nil {
			continue
		}
		agg.Backends.Probes = append(agg.Backends.Probes, oc.probe)
		switch {
		case oc.probe.Skipped:
			agg.Backends.Skipped++
			agg.Warnings = append(agg.Warnings,
				fmt.Sprintf("backend %s skipped: %s", oc.probe.BackendID, oc.probe.Reason))
			continue
		case !oc.probe.Probed:
			continue
		}
		agg.Backends.Probed++
		if !oc.probe.OK || oc.payload == nil {
			agg.Backends.Failed++
			agg.Backends.Errors = append(agg.Backends.Errors, ImageBackendProbeError{
				BackendID: oc.probe.BackendID,
				Error:     oc.probe.Error,
			})
			continue
		}
		agg.Backends.Answered++

		pl := oc.payload
		id := oc.probe.BackendID
		for _, name := range pl.effectiveSamplers() {
			union.addSampler(name, id)
		}
		for _, name := range pl.effectiveSchedulers() {
			union.addScheduler(name, id)
		}
		for _, l := range pl.effectiveLoras() {
			union.addLora(l.Name, l.Path, id)
		}
		for _, u := range pl.effectiveUpscalers() {
			union.addUpscaler(u.Name, u.Model, u.ImageUpscale, id)
		}
		// Лимиты: бэкенд без лимитов в мёрже НЕ участвует (иначе нули
		// «затирали» бы границы живых воркеров).
		if lim, ok := pl.effectiveLimits(); ok {
			limAcc.merge(id, lim)
		}
		for _, m := range pl.Models {
			agg.Models = append(agg.Models, ImageAggregatedModel{
				BackendID:      id,
				Name:           m.Name,
				State:          m.State,
				Family:         m.Family,
				SizeBytes:      m.SizeBytes,
				VramEstimateMB: m.VramEstimateMB,
				ActiveQueries:  m.ActiveQueries,
				Disabled:       m.Disabled,
				Defaults:       m.Defaults,
			})
		}
		for _, w := range pl.Warnings {
			agg.Warnings = append(agg.Warnings, fmt.Sprintf("backend %s: %s", id, w))
		}
	}

	sort.SliceStable(agg.Models, func(i, j int) bool {
		if agg.Models[i].BackendID != agg.Models[j].BackendID {
			return agg.Models[i].BackendID < agg.Models[j].BackendID
		}
		return agg.Models[i].Name < agg.Models[j].Name
	})

	agg.Samplers = union.samplers()
	agg.Schedulers = union.schedulers()
	agg.Loras = union.loras()
	agg.Upscalers = union.upscalers()
	agg.Limits = limAcc.result()
	agg.AllModels = union.modelNames(agg.Models)

	if agg.Backends.Total > 0 && agg.Backends.Answered == 0 {
		agg.Warnings = append(agg.Warnings,
			"no image backend answered: capabilities aggregate is empty")
	}
	return agg
}

// probeImageBackendCapabilities — опрос одного бэкенда: сначала наш воркер,
// при 404/405 — «голый» sd-server (/sdcpp/v1/capabilities).
func (p *Proxy) probeImageBackendCapabilities(
	ctx context.Context,
	b types.Backend,
	token string,
) *imageCapProbeOutcome {
	probe := ImageBackendProbe{
		BackendID: b.ID,
		Host:      b.Host,
		Port:      b.EffectiveImagePort(),
		Status:    string(b.Status),
	}
	if !imageCapProbeableStatuses[b.Status] {
		probe.Skipped = true
		probe.Reason = fmt.Sprintf("backend status is %q (не healthy/starting)", string(b.Status))
		return &imageCapProbeOutcome{probe: probe}
	}
	probe.Probed = true

	// Один бюджет на обе попытки: фолбэк не должен удваивать время ответа.
	probeCtx, cancel := context.WithTimeout(ctx, imageCapabilitiesProbeTimeout)
	defer cancel()

	paths := []struct {
		path   string
		source string
	}{
		{imageWorkerCapabilitiesPath, "worker"},
		{imageEngineCapabilitiesPath, "engine"},
	}

	var lastErr string
	for _, cand := range paths {
		payload, status, err := p.fetchImageCapabilities(probeCtx, b, cand.path, token)
		probe.HTTPStatus = status
		if err != nil {
			lastErr = err.Error()
			// Сетевая ошибка/таймаут — фолбэк на другой путь бессмысленен
			// (тот же хост и порт), поэтому прекращаем.
			break
		}
		if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			lastErr = fmt.Sprintf("%s returned HTTP %d", cand.path, status)
			continue // пробуем «голый» движок
		}
		if status < 200 || status >= 300 {
			lastErr = fmt.Sprintf("%s returned HTTP %d", cand.path, status)
			break
		}
		probe.Source = cand.source
		probe.OK = true
		probe.State = payload.State
		probe.Ready = payload.Ready
		probe.Model = string(payload.Model)
		probe.Error = ""
		return &imageCapProbeOutcome{probe: probe, payload: payload}
	}

	probe.OK = false
	probe.Error = lastErr
	if probe.Error == "" {
		probe.Error = "capabilities probe failed"
	}
	return &imageCapProbeOutcome{probe: probe}
}

// fetchImageCapabilities — GET path у бэкенда через общий helper
// proxyRequestToBackend (не дублируем построение URL/порта/таймаута).
func (p *Proxy) fetchImageCapabilities(
	ctx context.Context,
	b types.Backend,
	path, token string,
) (*imageCapabilitiesPayload, int, error) {
	// URL нужен только как «носитель» пути: proxyRequestToBackend сам подставит
	// host:port бэкенда (EffectiveImagePort) и создаст клиент с таймаутом.
	// ВАЖНО: путь относительный — URL.String() у запроса с абсолютным URL даёт
	// «//host/path», и склейка с базой превращается в мусор.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set(types.HeaderXAPIToken, token)
	}

	resp, err := p.proxyRequestToBackend(req, b.ID, imageCapabilitiesProbeTimeout)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxImageCapabilitiesBody))
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("read capabilities body: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, nil
	}
	var payload imageCapabilitiesPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("capabilities is not valid JSON: %w", err)
	}
	return &payload, resp.StatusCode, nil
}

// ============================================================
// JSON-модель ответа воркера / движка
// ============================================================

// imageCapabilitiesPayload — надмножество ответов sdworker
// (/api/image/capabilities) и «голого» sd-server (/sdcpp/v1/capabilities).
//
// ПОЧЕМУ ОДНА СТРУКТУРА НА ОБА: поля samplers/schedulers/loras/upscalers/limits
// у движка лежат в КОРНЕ ответа, а воркер отдаёт их вложенными в `engine` (сырой
// ответ движка без потерь). Разбираем оба места, приоритет — корень (актуальный
// контракт воркера важнее сырого движка).
type imageCapabilitiesPayload struct {
	Ready      bool               `json:"ready"`
	State      string             `json:"state"`
	Model      imageCapModelName  `json:"model"`
	Samplers   []string           `json:"samplers"`
	Schedulers []string           `json:"schedulers"`
	Loras      []imageCapLora     `json:"loras"`
	Upscalers  []imageCapUpscaler `json:"upscalers"`
	Limits     imageCapLimits     `json:"limits"`
	Models     []imageCapModel    `json:"models"`
	Warnings   []string           `json:"warnings"`
	Engine     *imageCapEngine    `json:"engine"`
}

// imageCapModelName — поле `model`, которое в двух контрактах имеет РАЗНУЮ форму:
//   - воркер (CapabilitiesResponse.Model): строка — имя загруженной модели;
//   - движок (Capabilities.Model): объект {name, stem, path}.
//
// ПОЧЕМУ СВОЙ UnmarshalJSON, А НЕ string/any: json.Unmarshal в один тип падает
// на несовпадении формы («cannot unmarshal object into Go value of type string»),
// а это сломало бы ВСЮ пробу «голого» sd-server на ровном месте. Незнакомая форма
// (не строка и не объект) не считается ошибкой: discovery обязан деградировать
// тихо, а не падать.
type imageCapModelName string

func (m *imageCapModelName) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*m = ""
		return nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*m = imageCapModelName(s)
	case '{':
		var obj struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return err
		}
		*m = imageCapModelName(obj.Name)
	default:
		*m = ""
	}
	return nil
}

// imageCapEngine — подмножество сырого ответа движка (то, что воркер кладёт в
// `engine`). Незнакомые поля игнорируются сознательно: capabilities движка
// расширяются каждую неделю, и падать на новом поле нельзя.
type imageCapEngine struct {
	Samplers   []string           `json:"samplers"`
	Schedulers []string           `json:"schedulers"`
	Loras      []imageCapLora     `json:"loras"`
	Upscalers  []imageCapUpscaler `json:"upscalers"`
	Limits     imageCapLimits     `json:"limits"`
}

// imageCapLora — LoRA в ответе движка.
type imageCapLora struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// imageCapUpscaler — апскейлер в ответе движка.
type imageCapUpscaler struct {
	Name         string `json:"name"`
	Model        bool   `json:"model"`
	ImageUpscale bool   `json:"image_upscale"`
}

// imageCapLimits — надмножество LimitsInfo воркера и CapLimits движка.
// Разница: воркер кладёт вместимость очереди в `queue_size`, движок — в
// `max_queue_size` (см. sdbackend/service.go:183, client.go:98).
type imageCapLimits struct {
	MinWidth             int     `json:"min_width"`
	MaxWidth             int     `json:"max_width"`
	MinHeight            int     `json:"min_height"`
	MaxHeight            int     `json:"max_height"`
	SizeMultiple         int     `json:"size_multiple"`
	MaxBatchCount        int     `json:"max_batch_count"`
	MinSteps             int     `json:"min_steps"`
	MaxSteps             int     `json:"max_steps"`
	MaxCFGScale          float64 `json:"max_cfg_scale"`
	MaxQueueSize         int     `json:"max_queue_size"`
	QueueSize            int     `json:"queue_size"`
	QueueInFlight        int     `json:"queue_in_flight"`
	CompletedJobTTLSec   int     `json:"completed_job_ttl_seconds"`
	GenerationTimeoutSec int     `json:"generation_timeout_seconds"`
	// *bool: отличаем «не сообщил» (nil) от «сообщил false».
	CancelQueuedOnly          *bool `json:"cancel_queued_only"`
	SeedAlwaysPositive        *bool `json:"seed_always_positive"`
	SupportsResponseFormatURL *bool `json:"supports_response_format_url"`
}

// imageCapModel — модель в ответе воркера (совпадает с /api/image/models).
type imageCapModel struct {
	Name           string                 `json:"name"`
	State          string                 `json:"state"`
	Family         string                 `json:"family"`
	SizeBytes      int64                  `json:"size_bytes"`
	VramEstimateMB int                    `json:"vram_estimate_mb"`
	ActiveQueries  int64                  `json:"active_queries"`
	Disabled       bool                   `json:"disabled"`
	Defaults       types.ImageGenDefaults `json:"defaults"`
}

// --- эффективные (корень → engine) аксессоры --------------------------------

func (pl *imageCapabilitiesPayload) effectiveSamplers() []string {
	if len(pl.Samplers) > 0 {
		return pl.Samplers
	}
	if pl.Engine != nil {
		return pl.Engine.Samplers
	}
	return nil
}

func (pl *imageCapabilitiesPayload) effectiveSchedulers() []string {
	if len(pl.Schedulers) > 0 {
		return pl.Schedulers
	}
	if pl.Engine != nil {
		return pl.Engine.Schedulers
	}
	return nil
}

func (pl *imageCapabilitiesPayload) effectiveLoras() []imageCapLora {
	if len(pl.Loras) > 0 {
		return pl.Loras
	}
	if pl.Engine != nil {
		return pl.Engine.Loras
	}
	return nil
}

func (pl *imageCapabilitiesPayload) effectiveUpscalers() []imageCapUpscaler {
	if len(pl.Upscalers) > 0 {
		return pl.Upscalers
	}
	if pl.Engine != nil {
		return pl.Engine.Upscalers
	}
	return nil
}

// effectiveLimits — лимиты бэкенда + признак «лимиты вообще есть».
// Бэкенд без лимитов в мёрже не участвует (требование консервативности:
// отсутствие данных не должно расширять границы).
func (pl *imageCapabilitiesPayload) effectiveLimits() (imageCapLimits, bool) {
	if pl.Limits.hasAny() {
		return pl.Limits, true
	}
	if pl.Engine != nil && pl.Engine.Limits.hasAny() {
		return pl.Engine.Limits, true
	}
	return imageCapLimits{}, false
}

// hasAny — сообщил ли движок/воркер хоть какие-то лимиты.
func (l imageCapLimits) hasAny() bool {
	return l.MinWidth > 0 || l.MaxWidth > 0 || l.MinHeight > 0 || l.MaxHeight > 0 ||
		l.MaxBatchCount > 0 || l.queueCapacity() > 0 || l.MaxSteps > 0
}

// queueCapacity — вместимость очереди: воркер (queue_size) → движок (max_queue_size).
func (l imageCapLimits) queueCapacity() int {
	if l.MaxQueueSize > 0 {
		return l.MaxQueueSize
	}
	return l.QueueSize
}

// ============================================================
// Объединение (union) возможностей
// ============================================================

// imageCapsUnion — union-аккумулятор samplers/schedulers/loras/upscalers.
//
// ПОЧЕМУ SOURCES ХРАНЯТСЯ: клиенту важно не только «есть sampler X», но и
// «на каком бэкенде он есть» — иначе при нескольких воркерах разных сборок
// выбор sampler'а превращается в лотерею (на одном воркере он есть, на другом
// движок молча подменит дефолтным).
type imageCapsUnion struct {
	samplerOrder   []string
	samplerBy      map[string]map[string]bool
	schedulerOrder []string
	schedulerBy    map[string]map[string]bool
	loraOrder      []string
	loraBy         map[string]*ImageLoraSupport
	upscalerOrder  []string
	upscalerBy     map[string]*ImageUpscalerSupport
}

func newImageCapsUnion() *imageCapsUnion {
	return &imageCapsUnion{
		samplerBy:   map[string]map[string]bool{},
		schedulerBy: map[string]map[string]bool{},
		loraBy:      map[string]*ImageLoraSupport{},
		upscalerBy:  map[string]*ImageUpscalerSupport{},
	}
}

func (u *imageCapsUnion) addSampler(name, backendID string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if u.samplerBy[name] == nil {
		u.samplerBy[name] = map[string]bool{}
		u.samplerOrder = append(u.samplerOrder, name)
	}
	u.samplerBy[name][backendID] = true
}

func (u *imageCapsUnion) addScheduler(name, backendID string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if u.schedulerBy[name] == nil {
		u.schedulerBy[name] = map[string]bool{}
		u.schedulerOrder = append(u.schedulerOrder, name)
	}
	u.schedulerBy[name][backendID] = true
}

func (u *imageCapsUnion) addLora(name, path, backendID string) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(path)
	}
	if name == "" {
		return
	}
	item, ok := u.loraBy[name]
	if !ok {
		item = &ImageLoraSupport{Name: name, Path: path}
		u.loraBy[name] = item
		u.loraOrder = append(u.loraOrder, name)
	} else if item.Path == "" {
		item.Path = path
	}
	item.Backends = append(item.Backends, backendID)
}

func (u *imageCapsUnion) addUpscaler(name string, model, imageUpscale bool, backendID string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	item, ok := u.upscalerBy[name]
	if !ok {
		item = &ImageUpscalerSupport{Name: name}
		u.upscalerBy[name] = item
		u.upscalerOrder = append(u.upscalerOrder, name)
	}
	// Флаги объединяем по OR: если хоть одна сборка умеет model-upscale —
	// клиент имеет право попробовать (и получит внятную ошибку на другой).
	item.Model = item.Model || model
	item.ImageUpscale = item.ImageUpscale || imageUpscale
	item.Backends = append(item.Backends, backendID)
}

// samplerList/schedulers/loras/upscalers — детерминированные срезы с
// отсортированными источниками.
func (u *imageCapsUnion) samplers() []ImageCapabilitySupport {
	out := make([]ImageCapabilitySupport, 0, len(u.samplerOrder))
	for _, name := range u.samplerOrder {
		out = append(out, ImageCapabilitySupport{Name: name, Backends: imgCapsSortedKeys(u.samplerBy[name])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (u *imageCapsUnion) schedulers() []ImageCapabilitySupport {
	out := make([]ImageCapabilitySupport, 0, len(u.schedulerOrder))
	for _, name := range u.schedulerOrder {
		out = append(out, ImageCapabilitySupport{Name: name, Backends: imgCapsSortedKeys(u.schedulerBy[name])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (u *imageCapsUnion) loras() []ImageLoraSupport {
	out := make([]ImageLoraSupport, 0, len(u.loraOrder))
	for _, name := range u.loraOrder {
		item := u.loraBy[name]
		item.Backends = dedupSorted(item.Backends)
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (u *imageCapsUnion) upscalers() []ImageUpscalerSupport {
	out := make([]ImageUpscalerSupport, 0, len(u.upscalerOrder))
	for _, name := range u.upscalerOrder {
		item := u.upscalerBy[name]
		item.Backends = dedupSorted(item.Backends)
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// modelNames — уникальные имена моделей из среза (отсортированы).
func (u *imageCapsUnion) modelNames(models []ImageAggregatedModel) []string {
	set := map[string]bool{}
	for _, m := range models {
		if strings.TrimSpace(m.Name) == "" {
			continue
		}
		set[m.Name] = true
	}
	return imgCapsSortedKeys(set)
}

// ============================================================
// Мёрж лимитов
// ============================================================

// imageCapsLimitsAccumulator — накопитель консервативного мёржа лимитов.
//
// Правила (ключевое требование: клиент, соблюдающий агрегат, обязан уложиться
// в границы КАЖДОГО бэкенда):
//   - min_width/min_height = MAX (самый высокий нижний порог);
//   - max_width/max_height/max_batch_count/max_steps/max_cfg_scale = MIN;
//   - max_queue_size/queue_in_flight = СУММА (кластер реально примет столько);
//   - completed_job_ttl_seconds/generation_timeout_seconds = MIN (гарантия
//     «не позже»);
//   - size_multiple = LCM (сетка, кратная всем шагам бэкендов);
//   - флаги возможностей = AND (true только если так у всех);
//   - поле, которое бэкенд не сообщил (0/nil), в мёрже НЕ участвует.
type imageCapsLimitsAccumulator struct {
	seen          bool
	minWidth      int
	maxWidth      int
	minHeight     int
	maxHeight     int
	sizeMultiple  int
	maxBatch      int
	minSteps      int
	maxSteps      int
	maxCFG        float64
	queueSize     int
	queueInFlight int
	ttlSeconds    int
	genTimeout    int
	cancelQueued  *bool
	seedPositive  *bool
	supportsURL   *bool
	sources       []string
}

func (acc *imageCapsLimitsAccumulator) merge(backendID string, l imageCapLimits) {
	acc.seen = true
	acc.sources = append(acc.sources, backendID)

	acc.minWidth = imgCapsMaxInt(acc.minWidth, l.MinWidth)
	acc.minHeight = imgCapsMaxInt(acc.minHeight, l.MinHeight)
	acc.maxWidth = imgCapsMinPositive(acc.maxWidth, l.MaxWidth)
	acc.maxHeight = imgCapsMinPositive(acc.maxHeight, l.MaxHeight)
	acc.maxBatch = imgCapsMinPositive(acc.maxBatch, l.MaxBatchCount)
	acc.minSteps = imgCapsMaxInt(acc.minSteps, l.MinSteps)
	acc.maxSteps = imgCapsMinPositive(acc.maxSteps, l.MaxSteps)
	if l.MaxCFGScale > 0 {
		if acc.maxCFG == 0 || l.MaxCFGScale < acc.maxCFG {
			acc.maxCFG = l.MaxCFGScale
		}
	}
	acc.sizeMultiple = imgCapsLCM(acc.sizeMultiple, l.SizeMultiple)
	acc.queueSize += l.queueCapacity()
	acc.queueInFlight += l.QueueInFlight
	acc.ttlSeconds = imgCapsMinPositive(acc.ttlSeconds, l.CompletedJobTTLSec)
	acc.genTimeout = imgCapsMinPositive(acc.genTimeout, l.GenerationTimeoutSec)

	acc.cancelQueued = imgCapsAndBool(acc.cancelQueued, l.CancelQueuedOnly)
	acc.seedPositive = imgCapsAndBool(acc.seedPositive, l.SeedAlwaysPositive)
	acc.supportsURL = imgCapsAndBool(acc.supportsURL, l.SupportsResponseFormatURL)
}

func (acc *imageCapsLimitsAccumulator) result() ImageAggregatedLimits {
	if !acc.seen {
		// Ни один бэкенд не сообщил лимитов: отдаём нули (не выдумываем
		// границы — клиент возьмёт фиксированный контракт движка).
		return ImageAggregatedLimits{SourceBackends: []string{}}
	}
	return ImageAggregatedLimits{
		MinWidth:                  acc.minWidth,
		MaxWidth:                  acc.maxWidth,
		MinHeight:                 acc.minHeight,
		MaxHeight:                 acc.maxHeight,
		SizeMultiple:              acc.sizeMultiple,
		MaxBatchCount:             acc.maxBatch,
		MinSteps:                  acc.minSteps,
		MaxSteps:                  acc.maxSteps,
		MaxCFGScale:               acc.maxCFG,
		MaxQueueSize:              acc.queueSize,
		QueueInFlight:             acc.queueInFlight,
		CompletedJobTTLSeconds:    acc.ttlSeconds,
		GenerationTimeoutSeconds:  acc.genTimeout,
		CancelQueuedOnly:          acc.cancelQueued,
		SeedAlwaysPositive:        acc.seedPositive,
		SupportsResponseFormatURL: acc.supportsURL,
		SourceBackends:            dedupSorted(acc.sources),
	}
}

// ============================================================
// Мелкие хелперы (имена с префиксом imgCaps: в пакете уже есть min/max)
// ============================================================

func imgCapsMaxInt(a, b int) int {
	if b > a {
		return b
	}
	return a
}

// imgCapsMinPositive — минимум по ненулевым значениям (0 = «не сообщено»).
func imgCapsMinPositive(cur, v int) int {
	if v <= 0 {
		return cur
	}
	if cur == 0 || v < cur {
		return v
	}
	return cur
}

// imgCapsAndBool — AND с учётом «не сообщено» (nil).
func imgCapsAndBool(cur, v *bool) *bool {
	if v == nil {
		return cur
	}
	if cur == nil {
		b := *v
		return &b
	}
	b := *cur && *v
	return &b
}

// imgCapsGCD — НОД (алгоритм Евклида).
func imgCapsGCD(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

// imgCapsLCM — НОК с защитой от переполнения.
//
// Зачем НОК, а не max: клиент должен получить шаг сетки, кратный шагу КАЖДОГО
// бэкенда. max(8,12)=12 — не кратно 8, а такой размер отвергнет один из
// воркеров. При абсурдных значениях (>1e6, переполнение) откатываемся к max —
// это заведомо «грубее», но не даёт мусорного числа.
func imgCapsLCM(a, b int) int {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	l := a / imgCapsGCD(a, b) * b
	if l <= 0 || l > 1<<20 {
		return imgCapsMaxInt(a, b)
	}
	return l
}

// sortedKeys — отсортированные ключи множества (детерминированный JSON).
func imgCapsSortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dedupSorted — уникальные значения в отсортированном виде.
func dedupSorted(in []string) []string {
	set := make(map[string]bool, len(in))
	for _, v := range in {
		if strings.TrimSpace(v) == "" {
			continue
		}
		set[v] = true
	}
	return imgCapsSortedKeys(set)
}

// emptyImageCapabilitiesAggregate — валидный «пустой» агрегат.
//
// ВАЖНО: пустые списки, а не nil — JSON-контракт обещает массивы
// (`samplers: []`), и клиент не должен ловить null.
func emptyImageCapabilitiesAggregate() *ImageCapabilitiesAggregate {
	return &ImageCapabilitiesAggregate{
		GeneratedAt:     time.Now().UTC(),
		CacheTTLSeconds: int(imageCapabilitiesCacheTTLNow().Seconds()),
		Backends: ImageCapabilitiesBackends{
			Errors: []ImageBackendProbeError{},
			Probes: []ImageBackendProbe{},
		},
		Samplers:   []ImageCapabilitySupport{},
		Schedulers: []ImageCapabilitySupport{},
		Loras:      []ImageLoraSupport{},
		Upscalers:  []ImageUpscalerSupport{},
		Limits: ImageAggregatedLimits{
			SourceBackends: []string{},
		},
		Models:    []ImageAggregatedModel{},
		AllModels: []string{},
	}
}
