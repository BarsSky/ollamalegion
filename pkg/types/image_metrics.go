package types

import "time"

// R-Image (2026-10-02): снимок состояния image-бэкенда (тип image_cpp) для
// WebUI/Monitor и управляющего API.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ БЛОК: у текстовых бэкендов модели и состояние лежат в
// Ollama/LlamaCpp-метриках, а image-воркер отдаёт свой контракт
// (/api/image/models) — без отдельного поля Monitor и страница Models видели
// image-бэкенд «пустым»: ни порта, ни моделей, ни состояния (жалоба оператора).
//
// Данные заполняет поллер image_resources (internal/balancer), который и так
// опрашивает воркер для гейта VRAM, поэтому дополнительной нагрузки нет.

// ImageModelBrief — краткая запись модели image-воркера для UI.
type ImageModelBrief struct {
	Name           string `json:"name"`
	State          string `json:"state"` // not_loaded | loading | loaded | error
	Family         string `json:"family,omitempty"`
	SizeBytes      int64  `json:"sizeBytes,omitempty"`
	VramEstimateMB int    `json:"vramEstimateMb,omitempty"`
	ActiveQueries  int64  `json:"activeQueries,omitempty"`
	Error          string `json:"error,omitempty"`
}

// ImageBackendMetrics — состояние image-бэкенда, как его видит балансер.
type ImageBackendMetrics struct {
	// State — состояние воркера целиком (loaded/loading/not_loaded/error).
	State string `json:"state,omitempty"`
	// CurrentModel — имя текущей модели воркера (может быть пустым).
	CurrentModel string `json:"currentModel,omitempty"`
	// Models — известные воркеру bundle'ы (включая незагруженные).
	Models []ImageModelBrief `json:"models,omitempty"`
	// VramFreeMB/VramTotalMB — VRAM хоста, если воркер её сообщил
	// (0 = неизвестно; это НЕ «ноль свободно»).
	VramFreeMB  int `json:"vramFreeMb,omitempty"`
	VramTotalMB int `json:"vramTotalMb,omitempty"`
	// PinnedRevision — версия движка, под которую написан контракт.
	PinnedRevision string `json:"pinnedRevision,omitempty"`
	// UpdatedAt — когда снимок получен (UI показывает свежесть данных).
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	// LastError — ошибка последнего опроса воркера (снимок недостоверен,
	// но UI должен видеть, что именно не так, а не пустоту).
	LastError string `json:"lastError,omitempty"`

	// ==== R-Image Phase 8 (2026-10-03): поток запросов ====================
	//
	// ЗАЧЕМ. До этого блока балансер НЕ считал image-запросы вообще: счётчики
	// текстового пути (recordRequest) вызывались только в proxy_request.go, а
	// ImageRouter шёл мимо них. Следствие, которое видел оператор: в Monitor у
	// image-бэкенда RPS = 0.0, Avg RT = «-», Active = «0/10» — то есть по
	// таблице нельзя было понять, идут ли на него запросы и с каким исходом.
	//
	// Requests — агрегаты (in-flight/total/ok/failed/rejected/RPS/длительности),
	// Recent — лента последних запросов этого бэкенда (свежие в начале).
	Requests *ImageRequestMetrics `json:"requests,omitempty"`
	Recent   []ImageRequestBrief  `json:"recent,omitempty"`
}

// Исходы одного image-запроса (types.ImageRequestBrief.Status). Вынесены
// константами, потому что по ним фильтрует UI и по ним же пишутся счётчики:
// строковый литерал в двух местах разошёлся бы молча.
const (
	// ImageRequestStatusOK — синхронная генерация отдала картинку.
	ImageRequestStatusOK = "ok"
	// ImageRequestStatusFailed — запрос дошёл до воркера/движка и упал.
	ImageRequestStatusFailed = "failed"
	// ImageRequestStatusRejected — отказал гейт балансера (до воркера не дошёл).
	ImageRequestStatusRejected = "rejected"
	// ImageRequestStatusAccepted — асинхронная постановка (202), генерация идёт.
	ImageRequestStatusAccepted = "accepted"
	// ImageRequestStatusFinished — асинхронная генерация завершилась (per-job
	// исход балансер не отслеживает; см. комментарий к ImageRequestMetrics).
	ImageRequestStatusFinished = "finished"
)

// ImageRequestMetrics — агрегаты запросов к image-бэкенду (или ко всему
// image-пулу, если это агрегат кластера).
//
// Исходы (Status в ленте) различаются осознанно, потому что означают разное:
//
//	ok       — синхронная генерация отдала картинку;
//	failed   — запрос дошёл до воркера/движка и завершился ошибкой (5xx, разрыв);
//	rejected — до воркера НЕ дошёл: отказал гейт (VRAM/лок/очередь/нет модели);
//	accepted — асинхронная постановка (202): генерация ещё идёт;
//	finished — асинхронная генерация завершилась (per-job исход балансер не
//	           отслеживает: воркер отдаёт только active_queries, поэтому
//	           выдавать это за «ok» было бы враньём).
type ImageRequestMetrics struct {
	// InFlight — сколько запросов сейчас внутри (для async — до конца генерации).
	InFlight int `json:"inFlight"`
	// Total — сколько запросов СТАРТОВАЛО (штук begin), а не сумма исходов ниже:
	// Accepted — подмножество ещё НЕ завершённых (async-постановки 202), а
	// OK/Failed/Rejected/Finished — завершённые. Складывать их нельзя.
	Total    int64 `json:"total"`
	OK       int64 `json:"ok"`
	Failed   int64 `json:"failed"`
	Rejected int64 `json:"rejected"`
	Accepted int64 `json:"accepted"`
	Finished int64 `json:"finished"`
	// RPS — запросов в секунду по скользящему окну 60 с (как у текстового пути).
	RPS float64 `json:"rps"`
	// AvgDurationMs/P50/P95 — по завершённым (ok/failed) запросам.
	AvgDurationMs int64 `json:"avgDurationMs,omitempty"`
	P50DurationMs int64 `json:"p50DurationMs,omitempty"`
	P95DurationMs int64 `json:"p95DurationMs,omitempty"`
	// LastDurationMs/LastRequestAt — последний завершённый и время последнего старта.
	LastDurationMs int64     `json:"lastDurationMs,omitempty"`
	LastRequestAt  time.Time `json:"lastRequestAt,omitempty"`
	// FailuresByCode — ошибки воркера/движка по коду (image_backend_error, …).
	FailuresByCode map[string]int64 `json:"failuresByCode,omitempty"`
	// GateDeniedByCode — отказы гейта по reasonCode (insufficient_vram,
	// gpu_busy, queue_timeout, model_not_loaded, …).
	GateDeniedByCode map[string]int64 `json:"gateDeniedByCode,omitempty"`
}

// ImageRequestBrief — одна запись ленты image-запросов для Monitor/WebUI.
//
// Поля «что именно генерировали» (Model/Width/Height/Steps/Batch/Prompt) берутся
// из тела запроса только для небольших JSON-тел генерации: multipart (edits) и
// крупные тела дают минимум (путь + исход), чтобы не буферизовать мегабайты
// картинок в памяти балансера.
type ImageRequestBrief struct {
	ID         string    `json:"id,omitempty"`
	At         time.Time `json:"at"`
	BackendID  string    `json:"backendId,omitempty"`
	Surface    string    `json:"surface,omitempty"` // openai | legacy
	Path       string    `json:"path,omitempty"`    // /v1/images/generations, …
	Model      string    `json:"model,omitempty"`
	Prompt     string    `json:"prompt,omitempty"` // обрезан (см. imagePromptLimit)
	Width      int       `json:"width,omitempty"`
	Height     int       `json:"height,omitempty"`
	Steps      int       `json:"steps,omitempty"`
	Batch      int       `json:"batch,omitempty"`
	DurationMs int64     `json:"durationMs,omitempty"`
	Status     string    `json:"status"` // ok | failed | rejected | accepted | finished
	HTTPStatus int       `json:"httpStatus,omitempty"`
	Code       string    `json:"code,omitempty"`
	Error      string    `json:"error,omitempty"` // короткое сообщение движка/гейта
	Images     int       `json:"images,omitempty"`
}
