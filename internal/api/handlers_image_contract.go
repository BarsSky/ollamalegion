// handlers_image_contract.go — Phase 4 (discovery): «расшифровка» контракта
// генерации изображений для клиентов и LLM-инструмента.
//
// ЧТО ЗДЕСЬ ЕСТЬ:
//
//	GET /api/v1/image/capabilities — агрегат возможностей всех HEALTHY image-бэкендов
//	                                 (см. internal/balancer/image_capabilities.go)
//	GET /api/v1/image/contract     — самодостаточный документ: порты, endpoints,
//	                                 поля запроса с нормализацией, лимиты, модели,
//	                                 curl-примеры и JSON-Schema инструмента
//	                                 `generate_image` + инструкция для агента.
//
// ЗАЧЕМ: план (plans/2026-09-27-image-generation-backend-plan.md §3.3, Phase 4)
// требует, чтобы клиент, прочитав ОДИН документ, мог собрать валидный запрос БЕЗ
// чтения кода, а LLM-агент — вызвать генерацию по схеме инструмента.
//
// ПРИНЦИП «НЕ ДУБЛИРОВАТЬ ВЫЧИСЛИМОЕ»:
//   - порты берутся из конфига балансера (с задокументированными дефолтами),
//     а не хардкодятся в тексте примеров;
//   - лимиты — из агрегата (A); фиксированными остаются только те значения,
//     которые из агрегата НЕ выводятся (per-backend вместимость очереди и TTL
//     результата: агрегат даёт их СУММУ/минимум по кластеру);
//   - примеры curl собираются строкой с фактическими портами;
//   - enum'ы sampler/scheduler/model — из агрегата, а не из констант.
package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/imagetool"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Константы контракта
// ============================================================

// Дефолты портов. Значения совпадают с internal/config/config.go:136-141
// (LB_PORT/LB_API_PORT/LB_OPENAI_PORT). Дублируются здесь осознанно: пакет
// internal/api не должен тянуть internal/config (main передаёт готовый конфиг),
// а контракт обязан быть валидным даже если конфиг пришёл из тестов/файла без
// этих полей.
const (
	imageContractDefaultProxyPort  = 18080
	imageContractDefaultAPIPort    = 18081
	imageContractDefaultOpenAIPort = 18079
)

// Фиксированные лимиты воркера (не выводятся из агрегата).
//
// ПОЧЕМУ КОНСТАНТЫ, А НЕ ИЗ АГРЕГАТА: агрегат даёт max_queue_size как СУММУ по
// бэкендам, а клиенту нужен ещё и ориентир «сколько влезет в один воркер».
// Источник значений — дефолты воркера: internal/sdbackend/config.go
// (MaxConcurrent: 64 — сознательно равен max_queue_size движка, см. комментарий
// там же) и internal/sdbackend/jobs.go (jobTTL = 600 с).
const (
	imageContractQueuePerBackend = 64
	imageContractResultTTLSecond = 600
)

// Абсолютные границы движка — внутренние константы sdbackend
// (internal/sdbackend/normalize.go: MinImageSide/MaxImageSide/SizeMultiple/
// MaxBatchCount/MinSteps/MaxSteps/maxCFGScale).
//
// ПОЧЕМУ ДУБЛИРУЕМ: контракт обязан оставаться валидным и полезным, даже когда
// НИ ОДИН image-воркер не ответил (агрегат пуст). Это ровно те значения, которые
// применит движок, поэтому «фиксированные» лимиты в ответе — не выдумка.
// Пакет internal/api не может импортировать internal/sdbackend: тот тянет
// супервизор субпроцесса (os/exec, syscall) — для management-API это лишняя
// зависимость; расхождение поймает тест TestImageContract_MandatorySections
// (границы 64…4096 в описании size).
const (
	imageContractMinSide      = 64
	imageContractMaxSide      = 4096
	imageContractSizeMultiple = 64
	imageContractMaxBatch     = 8
	imageContractMinSteps     = 1
	imageContractMaxSteps     = 100
	imageContractMaxCFG       = 30.0
)

// imageContractBuildTimeout — бюджет сбора агрегата для contract-хендлера.
// Пробы идут параллельно (imageCapabilitiesProbeTimeout = 5 с каждая), поэтому
// 10 с — с запасом на медленный воркер.
const imageContractBuildTimeout = 10 * time.Second

// ============================================================
// JSON-модель ответа /api/v1/image/contract
// ============================================================

// imageContractResponse — «расшифровка» контракта целиком.
//
// Именование ключей: camelCase для наших структурных полей (как в остальном
// management-API) и snake_case внутри блоков, которые приходят из агрегата/
// воркера (max_queue_size, all_models) — переименовывать их нельзя, это
// опубликованный контракт воркера.
type imageContractResponse struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// EngineRevision — за пинованную версию stable-diffusion.cpp отвечает движок
	// (types.PinnedSDServerRevision); клиенту полезно для баг-репортов.
	EngineRevision string `json:"engine_revision"`

	Ports  imageContractPorts  `json:"ports"`
	Auth   imageContractAuth   `json:"auth"`
	Limits imageContractLimits `json:"limits"`
	Models imageContractModels `json:"models"`

	Discovery     map[string]string       `json:"discovery"`
	Endpoints     []imageContractEndpoint `json:"endpoints"`
	RequestFields []imageContractField    `json:"requestFields"`
	Examples      []imageContractExample  `json:"examples"`
	Tool          imagetool.Tool          `json:"tool"`
	ToolUsage     imageContractToolUsage  `json:"toolInstructions"`
	Warnings      []string                `json:"warnings,omitempty"`
}

// imageContractPorts — фактические порты балансера/воркера.
type imageContractPorts struct {
	// OpenAI — OpenAI-поверхность (и /sdapi/v1/*) — для клиентов генерации.
	OpenAI int `json:"openai"`
	// Proxy — универсальная поверхность (Ollama + транзит /api/image/*).
	Proxy int `json:"proxy"`
	// API — management-плоскость (/api/v1/*).
	API int `json:"api"`
	// Worker — порт image-воркера по умолчанию (у бэкенда может быть свой
	// imagePort; точный список — GET /api/v1/image/backends).
	Worker int `json:"worker"`
	// OpenAIEnabled=false, если слушатель выключен (LB_OPENAI_PORT < 0) —
	// тогда openai=0 и генерацию надо звать через proxy-порт.
	OpenAIEnabled bool `json:"openaiEnabled"`
}

// imageContractAuth — нужен ли токен на management-плоскости.
type imageContractAuth struct {
	Enabled bool   `json:"enabled"`
	Header  string `json:"header"`
	Note    string `json:"note,omitempty"`
}

// imageContractLimits — лимиты: агрегат + невыводимые из агрегата фиксированные.
//
// balancer.ImageAggregatedLimits встроен (encoding/json «поднимает» его поля
// наверх): клиент читает `limits.min_width`, `limits.max_queue_size` и т.д.
// Поля CancelQueuedOnly/SeedAlwaysPositive внешнего уровня ПЕРЕКРЫВАЮТ
// указатели агрегата — так в JSON попадает одно значение (с фолбэком на
// инвариант движка, если ни один бэкенд не ответил).
type imageContractLimits struct {
	balancer.ImageAggregatedLimits

	QueueSizePerBackend int  `json:"queue_size_per_backend"`
	ResultTTLSeconds    int  `json:"result_ttl_seconds"`
	CancelQueuedOnly    bool `json:"cancel_queued_only"`
	StepProgress        bool `json:"step_progress"`
	SeedAlwaysPositive  bool `json:"seed_always_positive"`

	Notes []string `json:"notes,omitempty"`
}

// imageContractModels — доступные модели с дефолтами (из агрегата).
type imageContractModels struct {
	AllModels []string `json:"all_models"`
	// Available=false — ни один воркер не ответил (discovery деградировал).
	Available bool                            `json:"available"`
	Models    []balancer.ImageAggregatedModel `json:"models"`
}

// imageContractEndpoint — одна ручка контракта.
type imageContractEndpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Port: "openai" | "proxy" | "api" — см. ports выше.
	Port string `json:"port"`
	// Audience: "client" (генерация/список моделей) | "management" (управление).
	Audience string `json:"audience"`
	Purpose  string `json:"purpose"`
	Notes    string `json:"notes,omitempty"`
}

// imageContractField — поле запроса генерации с описанием нормализации.
//
// Normalization — то, что делает НАША сторона (sdbackend.NormalizeGeneration),
// а не движок: движок width/height не валидирует, batch/steps клампятся молча,
// а seed в OpenAI-ветке не читает вовсе (см. docs/image-generation.md §4).
type imageContractField struct {
	Name          string      `json:"name"`
	Type          string      `json:"type"`
	Required      bool        `json:"required"`
	Minimum       interface{} `json:"minimum,omitempty"`
	Maximum       interface{} `json:"maximum,omitempty"`
	MultipleOf    interface{} `json:"multipleOf,omitempty"`
	Default       interface{} `json:"default,omitempty"`
	Enum          []string    `json:"enum,omitempty"`
	Normalization string      `json:"normalization"`
	Ignored       bool        `json:"ignored,omitempty"`
}

// imageContractExample — curl-пример с фактическими портами.
type imageContractExample struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Audience string `json:"audience"`
	Method   string `json:"method"`
	URL      string `json:"url"`
	Curl     string `json:"curl"`
	Expected string `json:"expected,omitempty"`
}

// imageContractToolUsage — краткая инструкция агенту: куда и что слать.
type imageContractToolUsage struct {
	URL             string   `json:"url"`
	Method          string   `json:"method"`
	ContentType     string   `json:"contentType"`
	ArgumentsToBody string   `json:"argumentsToBody"`
	ResponsePath    string   `json:"responsePath"`
	ImageEncoding   string   `json:"imageEncoding"`
	Steps           []string `json:"steps"`
	Limitations     []string `json:"limitations,omitempty"`
}

// ============================================================
// Хендлеры
// ============================================================

// handleImageCapabilities — GET /api/v1/image/capabilities.
//
// Агрегат по всем здоровым image_cpp-бэкендам (READ-ONLY). Всегда 200: даже
// пустой кластер — валидный ответ (backends.total=0). Ошибки опроса отдельных
// воркеров приходят в backends.errors/probes, а не статусом ответа.
func (s *Server) handleImageCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.proxy == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proxy unavailable"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), imageContractBuildTimeout)
	defer cancel()

	agg := s.proxy.AggregateImageCapabilities(ctx, s.imageWorkerAPIToken)
	s.writeJSON(w, http.StatusOK, agg)
}

// handleImageContract — GET /api/v1/image/contract.
//
// Самодостаточная «расшифровка»: порты, ручки, поля с нормализацией, лимиты,
// модели, curl-примеры и JSON-Schema инструмента generate_image.
func (s *Server) handleImageContract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.proxy == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proxy unavailable"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), imageContractBuildTimeout)
	defer cancel()

	agg := s.proxy.AggregateImageCapabilities(ctx, s.imageWorkerAPIToken)
	s.writeJSON(w, http.StatusOK, s.buildImageContract(agg))
}

// ============================================================
// Сборка контракта
// ============================================================

// buildImageContract — собрать документ из агрегата + конфига.
func (s *Server) buildImageContract(agg *balancer.ImageCapabilitiesAggregate) *imageContractResponse {
	if agg == nil {
		agg = &balancer.ImageCapabilitiesAggregate{}
	}
	ports := s.imageContractPorts()
	host := s.imageContractHost()

	return &imageContractResponse{
		GeneratedAt:    time.Now().UTC(),
		EngineRevision: types.PinnedSDServerRevision,
		Ports:          ports,
		Auth:           s.imageContractAuth(),
		Limits:         buildImageContractLimits(agg),
		Models: imageContractModels{
			AllModels: agg.AllModels,
			Available: agg.Backends.Answered > 0,
			Models:    agg.Models,
		},
		Discovery: map[string]string{
			"contract":     fmt.Sprintf("http://%s:%d/api/v1/image/contract", host, ports.API),
			"capabilities": fmt.Sprintf("http://%s:%d/api/v1/image/capabilities", host, ports.API),
			"backends":     fmt.Sprintf("http://%s:%d/api/v1/image/backends", host, ports.API),
			"rawModels":    fmt.Sprintf("http://%s:%d/api/v1/image/models", host, ports.API),
			"openAIModels": fmt.Sprintf("http://%s:%d/v1/models", host, ports.OpenAI),
			"openAIImages": fmt.Sprintf("http://%s:%d/v1/images/generations", host, ports.OpenAI),
			"a1111Txt2Img": fmt.Sprintf("http://%s:%d/sdapi/v1/txt2img", host, ports.OpenAI),
			"nativeJobs":   fmt.Sprintf("http://%s:%d/api/image/jobs/{id}", host, ports.Proxy),
		},
		Endpoints:     imageContractEndpoints(ports),
		RequestFields: buildImageContractFields(agg),
		Examples:      buildImageContractExamples(host, ports, agg),
		Tool:          buildImageContractTool(agg),
		ToolUsage:     buildImageContractToolUsage(host, ports),
		Warnings:      agg.Warnings,
	}
}

// imageContractPorts — фактические порты (конфиг → дефолт).
func (s *Server) imageContractPorts() imageContractPorts {
	out := imageContractPorts{
		OpenAI:        imageContractDefaultOpenAIPort,
		Proxy:         imageContractDefaultProxyPort,
		API:           imageContractDefaultAPIPort,
		Worker:        types.DefaultImageWorkerPort,
		OpenAIEnabled: true,
	}
	if s.config == nil {
		return out
	}
	if p := s.config.LoadBalancer.Port; p != 0 {
		out.Proxy = p
	}
	if p := s.config.LoadBalancer.APIPort; p != 0 {
		out.API = p
	}
	switch p := s.config.LoadBalancer.OpenAIPort; {
	case p < 0:
		// Отрицательное значение = «слушатель не поднимать» (docs §5).
		out.OpenAI = 0
		out.OpenAIEnabled = false
	case p > 0:
		out.OpenAI = p
	}
	return out
}

// imageContractHost — хост для примеров/ссылок.
//
// ПОЧЕМУ localhost ВМЕСТО 0.0.0.0: конфиг часто слушает все интерфейсы, но
// скопированный в терминал curl с 0.0.0.0 не работает на части ОС. Если
// оператор задал конкретный хост — используем его.
func (s *Server) imageContractHost() string {
	if s.config == nil {
		return "localhost"
	}
	h := strings.TrimSpace(s.config.LoadBalancer.Host)
	switch h {
	case "", "0.0.0.0", "::", "[::]":
		return "localhost"
	}
	return h
}

// imageContractAuth — нужен ли токен management-плоскости.
func (s *Server) imageContractAuth() imageContractAuth {
	out := imageContractAuth{
		Header: types.HeaderXAPIToken,
		Note: "management-плоскость (/api/v1/*) требует X-API-Token, если auth включён; " +
			"клиентам генерации (/v1/images/*, /sdapi/v1/*) ключ не проверяется — подойдёт любой",
	}
	if s.config == nil {
		return out
	}
	out.Enabled = s.config.Auth.Enabled
	if strings.TrimSpace(s.config.Auth.HeaderName) != "" {
		out.Header = s.config.Auth.HeaderName
	}
	return out
}

// buildImageContractLimits — агрегат + фиксированные значения и пояснения.
//
// Границы, которые не сообщил НИ ОДИН бэкенд (0 в агрегате), заменяются
// абсолютными границами движка: документ обязан быть исполнимым и в пустом
// кластере (клиент/LLM читает contract до появления воркеров). Сумму очередей
// (max_queue_size) НЕ выдумываем: 0 = «кластер ничего не сообщил».
func buildImageContractLimits(agg *balancer.ImageCapabilitiesAggregate) imageContractLimits {
	merged := agg.Limits
	merged.MinWidth = intOr(merged.MinWidth, imageContractMinSide)
	merged.MaxWidth = intOr(merged.MaxWidth, imageContractMaxSide)
	merged.MinHeight = intOr(merged.MinHeight, imageContractMinSide)
	merged.MaxHeight = intOr(merged.MaxHeight, imageContractMaxSide)
	merged.SizeMultiple = intOr(merged.SizeMultiple, imageContractSizeMultiple)
	merged.MaxBatchCount = intOr(merged.MaxBatchCount, imageContractMaxBatch)
	merged.MinSteps = intOr(merged.MinSteps, imageContractMinSteps)
	merged.MaxSteps = intOr(merged.MaxSteps, imageContractMaxSteps)
	if merged.MaxCFGScale <= 0 {
		merged.MaxCFGScale = imageContractMaxCFG
	}

	return imageContractLimits{
		ImageAggregatedLimits: merged,
		QueueSizePerBackend:   imageContractQueuePerBackend,
		ResultTTLSeconds:      imageContractResultTTLSecond,
		StepProgress:          false,
		// Фолбэк на инвариант движка: если ни один воркер не ответил, поведение
		// всё равно такое (мы ВСЕГДА резолвим seed в положительное число).
		CancelQueuedOnly:   boolOr(agg.Limits.CancelQueuedOnly, true),
		SeedAlwaysPositive: boolOr(agg.Limits.SeedAlwaysPositive, true),
		Notes: []string{
			"max_queue_size — СУММА очередей кластера (0 = ни один воркер не ответил); queue_size_per_backend — вместимость одного воркера",
			"границы, помеченные нулём в /api/v1/image/capabilities, здесь заменены абсолютными границами движка (64…4096, batch 8, steps 1…100, кратность 64)",
			"result_ttl_seconds — сколько живёт результат завершённой джобы (нативный путь)",
			"step_progress=false: движок не отдаёт прогресс по шагам (примитивы есть, в сервер не проброшены)",
			"cancel_queued_only=true: генерацию в полёте движок прервать не может → 409",
		},
	}
}

// boolOr — значение указателя либо фолбэк.
func boolOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

// imageContractEndpoints — таблица ручек. Порядок фиксирован (документ читает
// человек и LLM, «дрожащий» порядок мешает диффам).
func imageContractEndpoints(p imageContractPorts) []imageContractEndpoint {
	return []imageContractEndpoint{
		{
			Method: http.MethodGet, Path: "/api/v1/image/contract", Port: "api", Audience: "management",
			Purpose: "этот документ: порты, ручки, поля, лимиты, примеры, схема инструмента",
		},
		{
			Method: http.MethodGet, Path: "/api/v1/image/capabilities", Port: "api", Audience: "management",
			Purpose: "агрегат возможностей image-бэкендов (samplers/schedulers/loras/upscalers/limits/models)",
			Notes:   "READ-ONLY; при недоступном воркере остальные всё равно отдаются (см. backends.errors)",
		},
		{
			Method: http.MethodGet, Path: "/api/v1/image/backends", Port: "api", Audience: "management",
			Purpose: "список image-бэкендов с их imagePort и статусом",
			Notes:   "публичный (без токена) — нужен странице Image для первичной отрисовки",
		},
		{
			Method: http.MethodGet, Path: "/api/v1/image/models", Port: "api", Audience: "management",
			Purpose: "«сырые» ответы воркеров GET /api/image/models как есть",
		},
		{
			Method: http.MethodGet, Path: "/v1/models", Port: "openai", Audience: "client",
			Purpose: "список моделей для OpenAI-клиентов",
			Notes:   "image-модели приходят алиасами sd-cpp-local, dall-e-2, dall-e-3 (n8n фильтрует id по ^dall-)",
		},
		{
			Method: http.MethodPost, Path: "/v1/images/generations", Port: "openai", Audience: "client",
			Purpose: "генерация изображения (OpenAI Images, синхронно)",
			Notes:   "ответ {created, data:[{b64_json|url, revised_prompt}]}; поле model игнорируется движком",
		},
		{
			Method: http.MethodPost, Path: "/sdapi/v1/txt2img", Port: "openai", Audience: "client",
			Purpose: "генерация изображения (A1111 API: SillyTavern, LibreChat SD tool, Open WebUI)",
			Notes:   "batch_size × n_iter перемножаются и зажимаются в 1…8; неизвестные A1111-поля игнорируются",
		},
		{
			Method: http.MethodPost, Path: "/api/image/generate", Port: "proxy", Audience: "management",
			Purpose: "нативная асинхронная генерация: 202 + {id, poll_url}",
			Notes:   "балансер сам выбирает image-бэкенд; для пиновки на конкретный — путь /api/v1/image/backends/{backendId}/proxy/api/image/generate",
		},
		{
			Method: http.MethodGet, Path: "/api/image/jobs/{id}", Port: "proxy", Audience: "management",
			Purpose: "состояние джобы и результат",
			Notes:   "результат живёт result_ttl_seconds; при нескольких воркерах пиновать бэкенд через /api/v1/image/backends/{backendId}/proxy/api/image/jobs/{id}",
		},
		{
			Method: http.MethodPost, Path: "/api/image/jobs/{id}/cancel", Port: "proxy", Audience: "management",
			Purpose: "отмена джобы",
			Notes:   "только статус queued; для generating — 409 (ограничение движка, не балансера)",
		},
	}
}

// buildImageContractFields — поля запроса генерации с нашей нормализацией.
//
// bounds — из агрегата, с фолбэком на константы движка (64…4096, steps 1…100,
// n 1…8): контракт обязан оставаться валидным и без единого живого воркера.
func buildImageContractFields(agg *balancer.ImageCapabilitiesAggregate) []imageContractField {
	lim := agg.Limits
	minSide := intOr(lim.MinWidth, imageContractMinSide)
	maxSide := intOr(lim.MaxWidth, imageContractMaxSide)
	if lim.MaxHeight > 0 && lim.MaxHeight < maxSide {
		maxSide = lim.MaxHeight
	}
	maxBatch := intOr(lim.MaxBatchCount, imageContractMaxBatch)
	minSteps := intOr(lim.MinSteps, imageContractMinSteps)
	maxSteps := intOr(lim.MaxSteps, imageContractMaxSteps)
	multiple := intOr(lim.SizeMultiple, imageContractSizeMultiple)
	maxCFG := imageContractMaxCFG
	if lim.MaxCFGScale > 0 {
		maxCFG = lim.MaxCFGScale
	}

	samplers := supportNames(agg.Samplers)
	schedulers := supportNames(agg.Schedulers)

	return []imageContractField{
		{
			Name: "prompt", Type: "string", Required: true,
			Normalization: "обязательное поле; пустая строка → 400 prompt_required",
		},
		{
			Name: "model", Type: "string", Default: "sd-cpp-local",
			Normalization: "принимается любой и используется только для выбора профиля дефолтов; движок модель из запроса игнорирует (одна модель на процесс — смена через POST /api/v1/image/backends/{id}/models/load)",
		},
		{
			Name: "size", Type: "string", Default: "auto",
			Normalization: fmt.Sprintf(`"WxH" или "auto"/""; clamp %d…%d (абсолютные границы движка), фактически по живому кластеру %d…%d, округление до кратного %d; "auto"/"" → размер из профиля модели; невалидный формат → 400 invalid_size`, imageContractMinSide, imageContractMaxSide, minSide, maxSide, multiple),
		},
		{
			Name: "width", Type: "integer", Minimum: minSide, Maximum: maxSide, MultipleOf: multiple,
			Normalization: "наше расширение: имеет приоритет над size; clamp + округление до кратности",
		},
		{
			Name: "height", Type: "integer", Minimum: minSide, Maximum: maxSide, MultipleOf: multiple,
			Normalization: "то же, что width",
		},
		{
			Name: "steps", Type: "integer", Minimum: minSteps, Maximum: maxSteps, Default: 20,
			Normalization: fmt.Sprintf("≤0 → дефолт профиля (20; 4 у turbo/distilled); >%d → clamp (движок молча клампит сам, но тогда длина data[] расходится с ожиданием клиента)", maxSteps),
		},
		{
			Name: "cfg_scale", Type: "number", Minimum: 0, Maximum: maxCFG, Default: 7.0,
			Normalization: fmt.Sprintf("≤0 → дефолт профиля (7.0; 1.0 у turbo/distilled); >%.0f → clamp", maxCFG),
		},
		{
			Name: "seed", Type: "integer",
			Normalization: "если не задан (или ≤0, как A1111 seed=-1) — резолвится в СЛУЧАЙНОЕ положительное число 256…2^31-2; это обязательная нормализация: OpenAI-ветка движка поле seed не читает и берёт 42, то есть без неё все картинки одинаковые",
		},
		{
			Name: "negative_prompt", Type: "string",
			Normalization: "не задан → берётся из профиля модели",
		},
		{
			Name: "n", Type: "integer", Minimum: 1, Maximum: maxBatch, Default: 1,
			Normalization: fmt.Sprintf("A1111-эквивалент — batch_size × n_iter (перемножаются, затем clamp 1…%d); в ответе ровно столько элементов data[]", maxBatch),
		},
		{
			Name: "response_format", Type: "string", Enum: []string{"b64_json", "url"}, Default: "b64_json",
			Normalization: `отсутствует/b64_json → base64; "url" — воркер сохраняет PNG и отдаёт ссылку (движок умеет только b64)`,
		},
		{
			Name: "output_format", Type: "string", Enum: []string{"png", "jpeg", "webp"}, Default: "png",
			Normalization: "пробрасывается движку; неизвестное значение → 400",
		},
		{
			Name: "output_compression", Type: "integer", Minimum: 0, Maximum: 100,
			Normalization: "clamp 0…100; для PNG — no-op",
		},
		{
			Name: "sampler", Type: "string", Enum: samplers,
			Normalization: "не задан → из профиля модели; неизвестный движку сэмплер молча заменяется дефолтным",
		},
		{
			Name: "scheduler", Type: "string", Enum: schedulers,
			Normalization: "не задан → из профиля модели",
		},
		{
			Name: "quality", Type: "string", Ignored: true,
			Normalization: "из OpenAI Images: игнорируется (движок не поддерживает)",
		},
		{
			Name: "style", Type: "string", Ignored: true,
			Normalization: "из OpenAI Images: игнорируется",
		},
		{
			Name: "user", Type: "string", Ignored: true,
			Normalization: "из OpenAI Images: игнорируется",
		},
		{
			Name: "background", Type: "string", Ignored: true,
			Normalization: "из OpenAI Images: игнорируется",
		},
	}
}

// buildImageContractExamples — curl-примеры с фактическими портами.
func buildImageContractExamples(host string, p imageContractPorts, agg *balancer.ImageCapabilitiesAggregate) []imageContractExample {
	openAIBase := fmt.Sprintf("http://%s:%d", host, p.OpenAI)
	proxyBase := fmt.Sprintf("http://%s:%d", host, p.Proxy)
	apiBase := fmt.Sprintf("http://%s:%d", host, p.API)
	model := "dall-e-2"
	if len(agg.AllModels) > 0 {
		model = agg.AllModels[0]
	}

	examples := []imageContractExample{
		{
			ID: "openai-images", Title: "OpenAI Images: генерация (Open WebUI, n8n, OpenAI SDK, AnythingLLM/localai)",
			Audience: "client", Method: http.MethodPost, URL: openAIBase + "/v1/images/generations",
			Curl: fmt.Sprintf(
				"curl -sS -X POST %s/v1/images/generations \\\n"+
					"  -H 'Content-Type: application/json' \\\n"+
					"  -d '{\"model\":\"%s\",\"prompt\":\"a cat sitting on a windowsill, soft light\",\"size\":\"512x512\",\"n\":1,\"steps\":8,\"cfg_scale\":1.0}'",
				openAIBase, model),
			Expected: `{"created":...,"data":[{"b64_json":"<base64 png>","revised_prompt":""}]}`,
		},
		{
			ID: "a1111-txt2img", Title: "A1111: генерация (SillyTavern, LibreChat SD tool, Open WebUI automatic1111)",
			Audience: "client", Method: http.MethodPost, URL: openAIBase + "/sdapi/v1/txt2img",
			Curl: "curl -sS -X POST " + openAIBase + "/sdapi/v1/txt2img \\\n" +
				"  -H 'Content-Type: application/json' \\\n" +
				"  -d '{\"prompt\":\"a cat\",\"negative_prompt\":\"blurry\",\"width\":512,\"height\":512,\"steps\":8,\"cfg_scale\":1.0,\"batch_size\":1,\"seed\":-1,\"sampler_name\":\"euler\"}'",
			Expected: `{"images":["<base64 png>"],"parameters":{...},"info":"..."}`,
		},
		{
			ID: "native-async-generate", Title: "Нативный async: поставить задачу (202 + id)",
			Audience: "management", Method: http.MethodPost, URL: proxyBase + "/api/image/generate",
			Curl: "curl -sS -X POST " + proxyBase + "/api/image/generate \\\n" +
				"  -H 'Content-Type: application/json' \\\n" +
				"  -d '{\"prompt\":\"a cat\",\"width\":512,\"height\":512,\"steps\":8,\"cfg\":1.0,\"sync\":false}'",
			Expected: `{"id":"img_ab12...","state":"queued","poll_url":"/api/image/jobs/img_ab12..."}`,
		},
		{
			ID: "native-job-status", Title: "Нативный async: узнать состояние/получить результат",
			Audience: "management", Method: http.MethodGet, URL: proxyBase + "/api/image/jobs/{id}",
			Curl: "curl -sS " + proxyBase + "/api/image/jobs/<id>" +
				"\n# пин на конкретный воркер (нужно при нескольких image-бэкендах):\n" +
				"curl -sS " + apiBase + "/api/v1/image/backends/<backendId>/proxy/api/image/jobs/<id>",
			Expected: `{"job":{"id":"...","state":"completed","result":{"images":[{"b64_json":"..."}]}}}`,
		},
		{
			ID: "native-job-cancel", Title: "Нативный async: отменить задачу (только queued)",
			Audience: "management", Method: http.MethodPost, URL: proxyBase + "/api/image/jobs/{id}/cancel",
			Curl:     "curl -sS -X POST " + proxyBase + "/api/image/jobs/<id>/cancel",
			Expected: `{"status":"cancelled","job":{...}}; для идущей генерации — 409 (движок не умеет mid-flight cancel)`,
		},
		{
			ID: "discovery", Title: "Discovery: контракт и возможности",
			Audience: "management", Method: http.MethodGet, URL: apiBase + "/api/v1/image/contract",
			Curl: "curl -sS " + apiBase + "/api/v1/image/contract" +
				"\ncurl -sS " + apiBase + "/api/v1/image/capabilities",
			Expected: "этот документ и агрегат возможностей (требуют X-API-Token, если auth включён)",
		},
	}

	if !p.OpenAIEnabled {
		// Слушатель OpenAI выключен: подсказываем рабочий транзитный путь, чтобы
		// клиент не бился в закрытый порт.
		examples = append(examples, imageContractExample{
			ID: "proxy-fallback", Title: "OpenAI-слушатель выключен (openai=0): транзит через proxy-порт",
			Audience: "client", Method: http.MethodPost, URL: proxyBase + "/v1/images/generations",
			Curl: "curl -sS -X POST " + proxyBase + "/v1/images/generations \\\n" +
				"  -H 'Content-Type: application/json' \\\n" +
				"  -d '{\"model\":\"" + model + "\",\"prompt\":\"a cat\",\"size\":\"512x512\"}'",
			Expected: `{"created":...,"data":[{"b64_json":"..."}]}`,
		})
	}
	return examples
}

// buildImageContractTool — JSON-Schema инструмента generate_image.
//
// R84 (2026-10-03): сама схема живёт в internal/imagetool — её же использует
// балансер, когда автоматически подмешивает инструмент в /v1/chat/completions и
// исполняет вызов сам (tool-loop). Здесь только маппинг агрегата в нейтральный
// Spec: две копии схемы неизбежно разъехались бы по именам полей и границам.
func buildImageContractTool(agg *balancer.ImageCapabilitiesAggregate) imagetool.Tool {
	lim := agg.Limits
	maxSide := intOr(lim.MaxWidth, imageContractMaxSide)
	if lim.MaxHeight > 0 && lim.MaxHeight < maxSide {
		maxSide = lim.MaxHeight
	}
	return imagetool.Build(imagetool.Spec{
		MinSide:      intOr(lim.MinWidth, imageContractMinSide),
		MaxSide:      maxSide,
		SizeMultiple: intOr(lim.SizeMultiple, imageContractSizeMultiple),
		MinSteps:     intOr(lim.MinSteps, imageContractMinSteps),
		MaxSteps:     intOr(lim.MaxSteps, imageContractMaxSteps),
		Models:       agg.AllModels,
	})
}

// buildImageContractToolUsage — «как вызвать» для агента.
func buildImageContractToolUsage(host string, p imageContractPorts) imageContractToolUsage {
	base := fmt.Sprintf("http://%s:%d", host, p.OpenAI)
	if !p.OpenAIEnabled {
		base = fmt.Sprintf("http://%s:%d", host, p.Proxy)
	}
	return imageContractToolUsage{
		URL:         base + "/v1/images/generations",
		Method:      http.MethodPost,
		ContentType: "application/json",
		ArgumentsToBody: "аргументы инструмента переносятся в тело JSON как есть (snake_case: prompt, negative_prompt, width, height, steps, cfg, seed, model); " +
			"обязательное поле модели добавлять не нужно — 'model' опционален",
		ResponsePath: "data[0].b64_json — base64 изображения (по умолчанию PNG); data[0].url — только если запросили response_format=\"url\"",
		ImageEncoding: "картинка приходит В base64 (не ссылкой): декодируйте data[0].b64_json и вложите как image/png; " +
			"для HTML — src=\"data:image/png;base64,<b64>\"",
		Steps: []string{
			"1. Вызови инструмент generate_image с prompt (остальные поля — по необходимости).",
			fmt.Sprintf("2. Отправь POST %s с заголовком Content-Type: application/json и телом = аргументы инструмента.", base+"/v1/images/generations"),
			"3. Возьми data[0].b64_json, декодируй base64 → это готовый PNG-файл.",
			"4. Покажи/сохрани картинку в диалоге; в тексте ответа кратко опиши, что изображено.",
			"5. Нужен воспроизводимый результат — передай seed из ответа при повторном вызове.",
		},
		Limitations: []string{
			"нет прогресса по шагам — запрос синхронный (ответ придёт по завершении генерации)",
			"одна модель на процесс: смена модели = load/unload на бэкенде (POST /api/v1/image/backends/{id}/models/load)",
			"генерация на воркере сериализована: параллельные запросы встают в очередь (queue_size_per_backend)",
			"отмена доступна только для задач в статусе queued (нативный путь /api/image/jobs/{id}/cancel)",
		},
	}
}

// ============================================================
// Мелкие хелперы
// ============================================================

// intOr — значение либо фолбэк при 0/отрицательном («не сообщено»).
func intOr(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

// supportNames — имена из union-списка агрегата (для enum'ов).
func supportNames(in []balancer.ImageCapabilitySupport) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.Name)
	}
	return out
}
