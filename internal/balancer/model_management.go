package balancer

import (
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/types"
)

// ModelManager — менеджер операций с моделями на бэкендах.
// Отправляет HTTP-запросы напрямую к Ollama API или к CppWorker (для llama.cpp),
// в зависимости от типа бэкенда.
type ModelManager struct {
	proxy  *Proxy
	client *http.Client
	mu     sync.RWMutex
	// Активные операции (modelName -> backendID -> startedAt)
	activeOps map[string]map[string]time.Time
}

// modelOpTTL — таймаут жизни «залипших» операций (после которого запись автоматически
// считается устаревшей и может быть перезаписана). Защищает от ситуации, когда
// предыдущая операция упала по сети и оставила запись, блокирующую повторные попытки.
const modelOpTTL = 10 * time.Minute

// NewModelManager — создание менеджера моделей
func NewModelManager(proxy *Proxy) *ModelManager {
	return &ModelManager{
		proxy: proxy,
		client: &http.Client{
			// R83/v67 (2026-10-02): Timeout снят (был 10 минут).
			//
			// 10 минут — это duration-кап на РАБОТУ: скачивание/выгрузка
			// 15-гигабайтного GGUF физически не укладывается в него, а
			// http.Client.Timeout убивает запрос, не отменяя саму операцию у
			// upstream. Оператор видел «Client.Timeout exceeded while awaiting
			// headers» при живом прогрессе и считал, что балансер не умеет
			// работать с большими моделями.
			//
			// Отсутствие работы видно по ошибке соединения/чтения — таймаут для
			// этого не нужен (см. доктрину: капы допустимы только как opt-in).
			// Явный кап возвращается через LB_ALLOW_MODEL_OP_TIMEOUT_SEC.
			Timeout: modelOpHTTPTimeout(),
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		activeOps: make(map[string]map[string]time.Time),
	}
}

// modelOpHTTPTimeout — duration-кап для HTTP-клиента операций с моделью
// (pull/push/load/reload/unload).
//
// R83/v67 (2026-10-02): по умолчанию 0 = БЕЗ капа: операции ждут терминального
// состояния (успех/ошибка/недоступность), а не «10 минут по часам». Кап
// возвращается только явным opt-in оператора, и при взведении печатается WARN,
// чтобы причина обрыва больше не искалась вслепую.
func modelOpHTTPTimeout() time.Duration {
	return optInTimeoutSeconds("LB_ALLOW_MODEL_OP_TIMEOUT_SEC", i18n.T("balancer.model_management.opt_in_what_model_ops"))
}

// loadRequestTimeout — duration-кап на конкретный HTTP-запрос загрузки модели.
// По умолчанию 0 (см. modelOpHTTPTimeout). Отдельное имя — потому что раньше
// здесь подставлялся Balancing.ModelLoadTimeout (120s), который на больших
// моделях обрывал загрузку, продолжавшуюся на бэкенде.
func loadRequestTimeout() time.Duration {
	return optInTimeoutSeconds("LB_ALLOW_MODEL_LOAD_TIMEOUT_SEC", i18n.T("balancer.model_management.opt_in_what_load_request"))
}

// ModelOpRequest — запрос на выполнение операции с моделью
type ModelOpRequest struct {
	Operation   string `json:"operation"` // pull, push, delete, load, unload
	ModelName   string `json:"modelName"`
	ContextSize *int   `json:"contextSize,omitempty"` // optional: override n_ctx для загрузки
	GPULayers   *int   `json:"gpuLayers,omitempty"`   // optional: override gpu_layers для загрузки
	Insecure    bool   `json:"insecure,omitempty"`
	Stream      bool   `json:"stream,omitempty"`

	// Force — R66c (2026-09-22): принудительная выгрузка занятой модели.
	//
	// cppworker по умолчанию отказывает в unload, если по модели есть активные
	// inference-запросы: отвечает 409 с "model is busy with active inference
	// requests" (cmd/cppworker/handlers_model.go:904-929) и принимает
	// ?force=true, чтобы отменить активные генерации и всё-таки выгрузить.
	// Балансер этот флаг не прокидывал, а поле отсутствовало — поэтому из WebUI
	// выгрузить занятую модель было НЕВОЗМОЖНО: пользователь видел только
	// "Cannot unload: model is busy" без выхода из ситуации.
	//
	// nil/false = прежнее поведение (безопасный unload).
	Force *bool `json:"force,omitempty"`
	// Round 7: per-tensor override (parallel arrays) для MoE.
	// Если заданы и согласованы по длине — load через /api/models/load-with-params.
	OverrideTensors     []string `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string `json:"overrideTensorBufts,omitempty"`

	// R54.6 (2026-08-24): дополнительные параметры для AutoTune apply.
	// cppworker /api/models/load принимает эти поля; если nil — cppworker
	// использует default (model profile из cppworker-defaults.json).
	KVCacheType *string `json:"kvCacheType,omitempty"` // f16 | q8_0 | q4_0
	UseMmap     *bool   `json:"useMmap,omitempty"`     // default true
	FlashAttn   *int    `json:"flashAttn,omitempty"`   // -1=auto, 0=off, 1=on
	BatchSize   *int    `json:"batchSize,omitempty"`

	// R83 (2026-09-29): ПАРАЛЛЕЛЬНОСТЬ и РАЗМЫШЛЕНИЯ.
	//
	// ДЕФЕКТ. Cppworker принимает оба поля в /api/models/load-with-params
	// (cmd/cppworker/types.go: loadWithParamsRequest.Parallel,
	// .EnableReasoning), но в структуре запроса БАЛАНСЕРА их не было — и они
	// молча терялись по пути WebUI → балансер → cppworker. Настройки «на
	// размышления» и «параллельность» на странице моделей GGUF сохранялись, а к
	// модели не применялись: оператор видел, что параметры «не применились».
	//
	// Parallel — число одновременных сессий (слотов) модели. Без него cppworker
	// поднимает n_parallel=1 независимо от того, что оператор выставил в UI.
	Parallel *int `json:"parallel,omitempty"`
	// ContextPerSeq — R83 (2026-09-30): окно контекста НА КЛИЕНТА (слот).
	//
	// contextSize — суммарное окно, которое слоты делят (llama.cpp:
	// n_ctx_seq = n_ctx / n_seq_max). Если оператор задаёт окно «на клиента»,
	// именно это поле уходит в cppworker; итоговый суммарный n_ctx считает
	// cppworker (он знает и слоты, и env CPPWORKER_N_PARALLEL), а memfit там же
	// проверяет суммарный объём.
	ContextPerSeq *int `json:"contextPerSeq,omitempty"`
	// EnableReasoning — per-model override reasoning-парсера (thinking).
	EnableReasoning *bool `json:"enableReasoning,omitempty"`
}

// ModelOpResult — результат операции с моделью
type ModelOpResult struct {
	Success   bool   `json:"success"`
	Operation string `json:"operation"`
	ModelName string `json:"modelName"`
	BackendID string `json:"backendId"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`

	// Busy — R66c (2026-09-22): операция отклонена, потому что модель занята
	// активными inference-запросами (cppworker ответил 409 Conflict).
	// RetryWithForce=true означает, что повтор с force=true её выгрузит
	// (ценой обрыва активных генераций) — WebUI показывает по этому полю
	// подтверждение и повторяет запрос.
	Busy           bool `json:"busy,omitempty"`
	RetryWithForce bool `json:"retryWithForce,omitempty"`

	// Deferred — R292 (2026-10-29): операция ОТЛОЖЕНА, потому что на бэкенде идёт
	// живой клиентский стрим, а load с другими параметрами для cppworker означает
	// unload+load — то есть выгрузку модели под чужой генерацией.
	//
	// ПОЧЕМУ ОТДЕЛЬНОЕ ПОЛЕ, А НЕ Busy. Busy ставится по ответу ВОРКЕРА (409 «model
	// is busy»), то есть когда мы уже попросили его выгрузить модель. Здесь мы не
	// просили вовсе: воркер ни при чём, и в circuit breaker это попадать не должно —
	// иначе серия отложений выглядела бы как серия сбоев загрузки и закрывала
	// breaker на здоровом стенде.
	Deferred      bool `json:"deferred,omitempty"`
	RetryAfterSec int  `json:"retryAfterSec,omitempty"`

	// WorkerStatus — R312 (2026-10-30): HTTP-статус, которым ответил ВОРКЕР.
	//
	// ЗАЧЕМ. Раньше он склеивался в текст ошибки («cppworker error (HTTP 404): …»),
	// и API-слой не мог отличить «модели нет — повтор не поможет» от «воркер
	// сломался — повтори». Живой факт: загрузка несуществующей модели отдавала
	// HTTP 500 при 404 от воркера, то есть клиенту (Cline, DSH, hermes) говорили
	// «повтори, станет лучше» про модель, которой нет на диске.
	WorkerStatus int `json:"workerStatus,omitempty"`

	// RoleRefused — R304 (2026-10-30): загрузку запретила РОЛЬ бэкенда (донор
	// раскладки), а не транспорт, память или занятость.
	//
	// ПОЧЕМУ ОТДЕЛЬНОЕ ПОЛЕ. «Это донор, копии ему нельзя» и «что-то сломалось» —
	// разные сообщения оператору: первое лечится сменой роли, второе — нет. Без
	// признака API отдавал 500 «internal», и панель показывала поломку балансера
	// вместо объяснения роли. По этому полю ответ становится 409 с причиной.
	RoleRefused bool `json:"roleRefused,omitempty"`

	// StillLoading — R262 (2026-10-28): загрузка НЕ провалилась, а ещё идёт.
	//
	// Живой факт: оператор получал HTTP 500 «poll timeout» при том, что воркер
	// модель догрузил. «Мы не дождались» и «загрузка провалилась» были одним
	// ответом, и отличить одно от другого было нельзя. При StillLoading Error
	// пуст, а HTTP-слой отвечает 202 Accepted: работа идёт, спроси позже.
	StillLoading bool `json:"stillLoading,omitempty"`
	// PollAfterSec — через сколько секунд имеет смысл опросить статус
	// (при StillLoading). Ноль = опрос не предлагается.
	PollAfterSec int `json:"pollAfterSec,omitempty"`
}

// ModelInfo — информация о модели на бэкенде
type ModelInfo struct {
	Name       string `json:"name"`
	Model      string `json:"model,omitempty"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
	ModifiedAt string `json:"modifiedAt,omitempty"`
	Loaded     bool   `json:"loaded"` // загружена ли в память

	// Round 18 (2026-07-10): llama.cpp runtime fields — populated from cppworker
	// /api/models. cppworker tracks these per-model: context length (n_ctx),
	// GPU layers (n_gpu_layers), quantization, file path, batch size, etc.
	// Используются WebUI Models tab для корректных метрик в карточках
	// (раньше показывались нули/прочерки потому что aggregator endpoint
	// не пробрасывал эти поля из cppworker).
	ContextLength int    `json:"contextLength,omitempty"` // n_ctx
	NumGPULayers  int    `json:"numGpuLayers,omitempty"`  // -1 = all, 0 = cpu only
	BatchSize     int    `json:"batchSize,omitempty"`
	Quantization  string `json:"quantization,omitempty"`
	GGUFPath      string `json:"ggufPath,omitempty"`
	State         string `json:"state,omitempty"` // loaded | loading | unloaded | error
	Architecture  string `json:"architecture,omitempty"`
	VRAMUsage     uint64 `json:"vramUsage,omitempty"` // MB (cppworker returns 0 — not tracked)
	RAMUsage      uint64 `json:"ramUsage,omitempty"`  // MB (cppworker returns 0 — not tracked)
	LoadedAt      string `json:"loadedAt,omitempty"`
	// Architecture metadata (для VRAM/RAM split estimation на frontend).
	// cppworker /api/models reports эти поля — пробрасываем в API.
	NLayers    int `json:"nLayers,omitempty"`
	NKvHeads   int `json:"nKvHeads,omitempty"`
	NEmbd      int `json:"nEmbd,omitempty"`
	HeadDimK   int `json:"headDimK,omitempty"`
	HeadDimV   int `json:"headDimV,omitempty"`
	MaxContext int `json:"maxContext,omitempty"` // ggufContextLength — макс n_ctx для модели
}

// resolveBackendPort — возвращает порт инференса в зависимости от движка бэкенда.
// Для llama.cpp — CppWorkerPort (с fallback 18092), для ollama — OllamaPort.
func (mm *ModelManager) resolveBackendPort(backend *types.Backend) int {
	engine := types.ResolveEngine(backend.Engine, backend.Type)
	if engine == types.EngineLlamaCPP {
		if backend.CppWorkerPort > 0 {
			return backend.CppWorkerPort
		}
		return 18092 // актуальный default для современных cppworker
	}
	return backend.OllamaPort
}

// isLlamaCppBackend — true, если бэкенд работает через cppworker, а не Ollama.
func (mm *ModelManager) isLlamaCppBackend(backend *types.Backend) bool {
	engine := types.ResolveEngine(backend.Engine, backend.Type)
	return engine == types.EngineLlamaCPP
}
