package types

import "strings"

// Состояния модели в том виде, в каком они уходят КЛИЕНТУ (cppworker `state`
// в /api/models, `status` модели в /api/v1/gguf/backends, state в /api/ps).
//
// Не путать с типом ModelState в model_state.go: там внутренние состояния
// балансера (LOADED/LOADING/…, верхний регистр) для warmup-логики. Здесь —
// строчные значения, которые исторически отдаёт llama.cpp-стек, и менять их
// нельзя: их читают WebUI и внешние клиенты.
const (
	// WireStateLoaded — модель в памяти и обслуживает запросы.
	WireStateLoaded = "loaded"
	// WireStateLoading — загрузка в полёте: модель ещё НЕ обслуживает запросы.
	WireStateLoading = "loading"
	// WireStateError — загрузка провалилась.
	WireStateError = "error"
)

// NormalizeModelState — R119: свести `state` модели cppworker к трём понятным
// значениям (WireState*).
//
// ЗАЧЕМ общий helper. Строку состояния отдаёт cppworker (`state` в /api/models),
// и до R119 каждый потребитель толковал её по-своему:
//   - поллер балансера считал пустое значение «loaded» (llamacpp_metrics_poller.go);
//   - хендлер /api/v1/gguf/backends вообще не смотрел на state и жёстко рисовал
//     «loaded» (gguf_backends_handler.go);
//   - /api/ps показывал loading-модель как running (llamacpp_handlers_readonly.go).
//
// Живой дефект (2026-10-15, стенд): cppworker сообщал state="loading",
// context_size=0, а WebUI писал «загружено» — оператор не видел ни загрузки,
// ни провала и слал запросы в пустоту.
//
// Пустое значение трактуется как loaded намеренно: старые сборки cppworker
// state не отдавали, и «потерять» реально загруженную модель хуже, чем показать
// её загруженной.
func NormalizeModelState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "":
		return WireStateLoaded
	case "ready", "loaded":
		return WireStateLoaded
	case "loading":
		return WireStateLoading
	case "error", "failed":
		return WireStateError
	default:
		return strings.ToLower(strings.TrimSpace(state))
	}
}

// ModelStateUsable — R119: обслуживает ли модель запросы ПРЯМО СЕЙЧАС.
//
// Единственный критерий: state нормализуется в loaded. Модель, которая ещё
// грузится (или уже падает), не «загружена» ни для клиента, ни для выбора
// бэкенда, ни для счётчика загруженных моделей.
func ModelStateUsable(state string) bool {
	return NormalizeModelState(state) == WireStateLoaded
}

// RPCNodeStatus — состояние узла раскладки (rpc-server) на самом бэкенде.
//
// R133 (2026-10-16): приходит из /api/models cppworker (поле rpc_server, тот же
// снимок, что отдаёт супервизор дочернего процесса). Балансер назначает роль
// donor — и обязан видеть, что узел действительно поднялся: «enabled» — это
// желание, «running» — процесс, «listening» — порт. Три разных утверждения,
// которые раньше были неотличимы снаружи.
type RPCNodeStatus struct {
	Enabled   bool   `json:"enabled"`
	Running   bool   `json:"running"`
	Listening bool   `json:"listening"`
	Port      int    `json:"port,omitempty"`
	Addr      string `json:"addr,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Restarts  int    `json:"restarts,omitempty"`
	LastError string `json:"lastError,omitempty"`
}

// LlamaCppMetrics - метрики llama.cpp бэкенда
type LlamaCppMetrics struct {
	// MaxVRAMNCtx — макс. n_ctx, помещающийся в VRAM (из /api/models cppworker).
	// Используется preflight_nctx для оценки возможности reload без round-trip.
	// 0 = неизвестно (poller ещё не опросил бэкенд) ИЛИ веса не влезают —
	// различает VramKnown (R83 §9.2).
	MaxVRAMNCtx int `json:"maxVramNCtx"`
	// VramKnown — cppworker сообщил, что VRAM ему известна (top-level
	// `vram_known` в /api/models, R83). Вместе с MaxVRAMNCtx==0 означает «веса
	// модели не помещаются в VRAM ни при каком gpu_layers»: reload бессмыслен, и
	// preflight обязан отказать с причиной, а не гнать клиента в reload.
	VramKnown       bool   `json:"vramKnown"`
	MaxRAMNCtx      int    `json:"maxRamNCtx"`
	AvailableVRAMMB uint64 `json:"availableVramMb"`
	TotalVRAMMB     uint64 `json:"totalVramMb"`
	// R115: состав ёмкости. AvailableVRAMMB — ИТОГ (локальная карта + узлы
	// раскладки, так его считает memfit в cppworker), и по одному числу балансер
	// не может отличить «8 ГБ на карте» от «8 ГБ карты + 7 ГБ удалённого узла».
	// Разница существенна: во втором случае половина слоёв считается по сети, и
	// узел может отвалиться. Поля приходят из /api/models cppworker (R115).
	RPCEnabled       bool     `json:"rpcEnabled"`
	RPCServersActive []string `json:"rpcServersActive"`
	VramLocalFreeMB  uint64   `json:"vramLocalFreeMb"`
	VramRemoteFreeMB uint64   `json:"vramRemoteFreeMb"`
	VramRemoteNodes  int      `json:"vramRemoteNodes"`
	// R132 (2026-10-16): откуда вклад узлов в бюджет у СПРАВОЧНЫХ путей:
	// none (узлы на этом пути не опрашивались) | cache (последний собранный
	// снимок) | probed (узлы спрошены). Без этого «0 МБ узлов» и «узлы не
	// измерялись» в панели выглядят одинаково.
	VramRemoteSource string `json:"vramRemoteSource,omitempty"`
	// RPCNode — состояние СОБСТВЕННОГО узла раскладки ЭТОГО бэкенда (R133).
	// Донор поднимает rpc-server по приказу балансера (роль donor) и обязан
	// отчитываться: поднят ли процесс, слушается ли порт, какая ошибка. Иначе
	// роль в панели — это команда в пустоту, а «поднят» путается с «доступен».
	RPCNode *RPCNodeStatus `json:"rpcNode,omitempty"`
	ModelMaxContext  int      `json:"modelMaxContext"`
	// === Round 37 (2026-08-18): auto-adapt n_ctx 3-tier fields ===
	// Populated from /api/models → feasible_max_context / gguf_max_context.
	// 0 = unknown (cppworker didn't report — pre-Round 37 build).
	// Used by preflight resolveModelMaxContext v2 for auto-relax when
	// profile is conservative.
	MaxFeasibleContext int             `json:"maxFeasibleContext"`
	GGUFMaxContext     int             `json:"ggufMaxContext"`
	LoadedModels       []LlamaCppModel `json:"loadedModels"`
	// LoadingModels — модели, которые сейчас в процессе загрузки (State="loading").
	// Заполняется из cppworker /api/models/load/progress (либо из notifyModelLoaded callback).
	// Используется UI (монитор, вкладка бэкендов, GGUF-таб) для отображения
	// спиннера и elapsed-time «Загружается model-name 25s».
	// При переходе State=loaded элемент перемещается в LoadedModels, при
	// State=error — остаётся здесь до следующего poll с error-полем.
	LoadingModels     []LlamaCppModel  `json:"loadingModels,omitempty"`
	AvailableModels   []LlamaCppModel  `json:"availableModels,omitempty"`
	// R342 (2026-10-30): omitempty у ПОКАЗЫВАЕМЫХ полей.
	//
	// Живой дефект (аудит docs/plans/2026-10-30-abort-and-metrics-audit.md):
	// блок llamaCpp отдаётся в /api/v1/cluster для каждого бэкенда, и нули этих
	// полей в JSON всегда присутствовали. WebUI проверяет `!== undefined` — то есть
	// ноль считался НАСТОЯЩИМ значением и перекрывал правильные данные из ollama.*:
	// Monitor во время генерации показывал «0 активных запросов» и RPS=0.
	// С omitempty нулевое значение не сериализуется, и фолбэк WebUI работает.
	ActiveRequests    int              `json:"activeRequests,omitempty"`
	TotalRequests     int64            `json:"totalRequests"`
	AvgResponseTime   float64          `json:"avgResponseTime,omitempty"` // ms
	RequestsPerSecond float64          `json:"requestsPerSecond,omitempty"`
	FreeSlots         int              `json:"freeSlots,omitempty"`
	AvailableSlots    int              `json:"availableSlots,omitempty"`
	MaxModels         int              `json:"maxModels"`
	MaxConcurrentReqs int              `json:"maxConcurrentReqs"`
	GPUInfo           *LlamaCppGPUInfo `json:"gpuInfo,omitempty"`
}

// LlamaCppModel - информация о модели llama.cpp
type LlamaCppModel struct {
	Name          string `json:"name"`
	Path          string `json:"path,omitempty"`
	Size          uint64 `json:"size"`      // bytes
	VRAMUsage     uint64 `json:"vramUsage"` // MB
	RAMUsage      uint64 `json:"ramUsage"`  // MB
	ContextLength int    `json:"contextLength"`
	BatchSize     int    `json:"batchSize"`
	NumGPULayers  int    `json:"numGpuLayers"`
	Quantization  string `json:"quantization"`
	State         string `json:"state"` // "loaded", "loading", "error"
	// Parallel — R83 (2026-09-29): число параллельных слотов модели (n_parallel).
	//
	// Зачем. Балансер не знал реальную вместимость модели и держал
	// MaxConcurrentReqs=1, то есть СЕРИАЛИЗОВАЛ запросы, даже когда модель
	// загружена с parallel=2/3 и готова обслуживать их одновременно (проверено
	// напрямую: два одновременных запроса при parallel=2 — оба 200). Теперь
	// значение приходит из /api/models и синхронизирует вместимость бэкенда.
	Parallel int `json:"parallel,omitempty"`
	// MaxSlots — сколько слотов cppworker РЕАЛЬНО завёл (`max_slots` в /api/models).
	// Для однослотовой модели это 1 (а не 0), поэтому именно это поле годится как
	// источник вместимости бэкенда.
	MaxSlots int `json:"maxSlots,omitempty"`
	// ContextPerSeq — R83-fix (2026-09-30): окно ОДНОГО слота
	// (`context_per_seq` в /api/models = n_ctx_seq у llama.cpp).
	//
	// ЗАЧЕМ. ContextLength — СУММАРНОЕ окно, которое слоты делят между собой
	// (llama.cpp при kv_unified=false: n_ctx_seq = PAD(n_ctx / n_seq_max, 256)).
	// При parallel=2 модель с суммарным окном 32768 обслуживает каждый запрос
	// только 16384 токенами. Балансер, ограничивая запрос суммарным окном,
	// пропускал num_ctx=32768 — cppworker принимал (32768 ≤ 32768), а фактически
	// слот столько не вмещает.
	ContextPerSeq int `json:"context_per_seq,omitempty"`
	// DeviceLayersExact — R162 (2026-10-17): ТОЧНАЯ раскладка слоёв по устройствам
	// (cppworker перехватывает `load_tensors: layer N assigned to device X`).
	//
	// ЗАЧЕМ ЭТО ЗДЕСЬ. cppworker НЕ отдаёт `gpu_layers` в /api/models (поле пустое),
	// а отдаёт именно раскладку. Балансер раскладку декодировал, но в модель не
	// переносил — и WebUI, читая `numGpuLayers=0`, показывал «gpu_layers=0/42 —
	// модель целиком на CPU» при фактических 42/42 на GPU. Ложное предупреждение
	// отправляло оператора искать несуществующую проблему.
	DeviceLayersExact []DeviceLayerRange `json:"device_layers_exact,omitempty"`
	// === Architecture metadata (Round 18+ — для оценки VRAM/RAM split по слоям) ===
	// cppworker reports per-model architecture details, используем для расчёта
	// estimatedVram/estimatedRam (cppworker не сообщает actual usage per-model).
	Architecture string `json:"architecture,omitempty"` // "gemma4", "qwen35moe", ...
	NLayers      int    `json:"nLayers,omitempty"`      // total layers in model
	NKvHeads     int    `json:"nKvHeads,omitempty"`     // n_kv_heads (для KV cache расчёта)
	NEmbd        int    `json:"nEmbd,omitempty"`        // n_embd (для KV cache)
	HeadDimK     int    `json:"headDimK,omitempty"`     // head_dim_k
	HeadDimV     int    `json:"headDimV,omitempty"`     // head_dim_v
	MaxContext   int    `json:"maxContext,omitempty"`   // ggufContextLength (макс n_ctx для этой модели)
	LoadedAt     string `json:"loadedAt,omitempty"`     // RFC3339Nano
	// === Capabilities (Round 18 P0.1, 2026-08-03) ===
	// Single source of truth для auto-detect (vision, tools, reasoning).
	// Прокидывается клиенту через X-Model-Capabilities, X-Model-Max-Context,
	// X-Model-Architecture headers.
	Capabilities *ModelCapabilities `json:"capabilities,omitempty"`
	// === Runtime params (Round 34, 2026-08-12) — для profile mismatch detection ===
	// cppworker сообщает в /api/models актуальные параметры загруженной модели
	// (Round 26 profile sync). Balancer использует для сравнения с client request
	// в preflight: если client просит kvCacheType=q8_0, а модель загружена с
	// f16, → reload на нужный params (как в n_ctx случае).
	// Пустая строка / 0 = неизвестно.
	KvCacheType   string `json:"kvCacheType,omitempty"`
	FlashAttnType int    `json:"flashAttnType,omitempty"` // -1=auto, 0=off, 1=on
	UseMmap       bool   `json:"useMmap,omitempty"`
	// === Round 37 (2026-08-18): per-model feasible/GGUF max context ===
	// Заполняется из enriched /api/models per-model `feasible_max_context` /
	// `gguf_max_context` (см. cmd/cppworker/handlers_model.go Round 37 changes).
	// Используется balancer'ом для 3-tier resolution: profile vs feasible vs GGUF.
	FeasibleMaxContext int `json:"feasibleMaxContext,omitempty"`
	GGUFMaxContext     int `json:"ggufMaxContext,omitempty"`
	// R83 §9.2 (2026-09-26): per-model `vram_known` из /api/models cppworker.
	// Если модель не загружена, cppworker отдаёт vram_known=false — тогда preflight
	// не делает выводов о «веса не влезают» (см. NCtxBackendState.VRAMKnown).
	VramKnown bool `json:"vramKnown,omitempty"`
	// ReasoningEnabled — R83-политика (2026-10-01): режим reasoning ЗАГРУЖЕННОЙ
	// модели (`reasoning_enabled` в /api/models cppworker).
	//
	// nil = cppworker не сообщил (модель не загружена или старая версия) —
	// сравнивать требование клиента (think=true/false) не с чем, и балансер
	// молчит. false — это ЗНАЧЕНИЕ: модель загружена с выключенным reasoning,
	// и запрос с think=true обязан получить внятную ошибку, а не тихую подмену
	// поведения.
	ReasoningEnabled *bool `json:"reasoningEnabled,omitempty"`
	// DeviceLayers — R118.16 (2026-10-14): ТОЧНАЯ раскладка слоёв по устройствам
	// (`device_layers` в /api/models cppworker) — строки `load_tensors: layer N
	// assigned to device X` из лога llama.cpp, перехваченные мостом.
	//
	// Пусто = неизвестно (старая сборка cppworker или stub) — и это НЕ то же
	// самое, что «все слои на CPU»: пустой список нельзя показывать как раскладку.
	DeviceLayers []DeviceLayerRange `json:"deviceLayers,omitempty"`
	// Parameters — R119 (2026-10-15, пункт 4): происхождение КАЖДОГО параметра
	// загрузки — {"contextLength":{"value":8192,"source":"profile"},…}, где source
	// это request | profile | default_profile | backend.
	//
	// ЗАЧЕМ на этой структуре. WebUI рисует карточку загруженной модели из
	// /api/v1/backends (loadedModels), и до R119 оператор видел только значения:
	// какой слой их поставил — клиентский запрос, per-model профиль, «настройки по
	// умолчанию» или env cppworker — не было видно нигде, и вопрос «настройки
	// применяются?» оставался догадкой. Заполняется балансером из своего реестра
	// provenance при сборке cluster state (cluster_state.go).
	Parameters map[string]interface{} `json:"parameters,omitempty"`
	// === Loading state (Шаг «отображение загрузки в мониторе и вкладке бэкендов») ===
	// Заполняются только пока State == "loading" / "error". После успешной
	// загрузки поля обнуляются (omitempty).
	LoadingStartedAt *string `json:"loadingStartedAt,omitempty"` // RFC3339Nano
	LoadingSizeBytes int64   `json:"loadingSizeBytes,omitempty"` // ожидаемый размер модели
	LoadingError     string  `json:"loadingError,omitempty"`     // причина сбоя (если State="error")
}

// DeviceLayerRange — R118.16 (2026-10-14): непрерывный диапазон слоёв на одном
// устройстве. Это ФАКТ из лога llama.cpp (`load_tensors: layer N assigned to
// device X`), который перехватывает мост cppworker'а (c/bridge/layers_plan.go).
//
// Нумерация слоёв — как в логе: 0…n_layer, где последний номер это выходной
// слой. Node заполняется для устройств RPC0/RPC1 (адрес узла раскладки).
type DeviceLayerRange struct {
	Device string `json:"device"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	Count  int    `json:"count"`
	Node   string `json:"node,omitempty"`
}

// LlamaCppGPUInfo - информация о GPU для llama.cpp
type LlamaCppGPUInfo struct {
	Count     int                 `json:"count"`
	Devices   []LlamaCppGPUDevice `json:"devices"`
	TotalVRAM uint64              `json:"totalVram"` // MB
	FreeVRAM  uint64              `json:"freeVram"`  // MB
}

// LlamaCppGPUDevice - одно GPU-устройство
type LlamaCppGPUDevice struct {
	Name        string  `json:"name"`
	MemoryTotal uint64  `json:"memoryTotal"` // MB
	MemoryFree  uint64  `json:"memoryFree"`  // MB
	Utilization float64 `json:"utilization"` // %
}
