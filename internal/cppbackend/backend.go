package cppbackend

import (
	"fmt"
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/types"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================
// Типы
// ============================================================

// LoadState — состояние загрузки модели
type LoadState string

const (
	StateUnloaded LoadState = "unloaded"
	StateLoading  LoadState = "loading"
	StateLoaded   LoadState = "loaded"
	StateError    LoadState = "error"
)

// ErrModelAlreadyLoadedAs — sentinel для dedup загрузки по file path.
//
// Round 32 #8 (2026-08-10): когда клиент запрашивает загрузку с другим name,
// но тот же path уже загружен, LoadModelWithOpts возвращает wrapped
// ErrModelAlreadyLoadedAs{ExistingName: ...} — handler может через
// errors.As извлечь existing name и вернуть клиенту status=loaded.
//
// До этого фикса: возвращался plain fmt.Errorf("model X already loaded") —
// handler не различал "уже загружено под другим именем" vs "реальный конфликт
// имён" → 2x VRAM было занято (gemma-4 vs gemma-4.gguf грузились как 2 разные
// модели), и балансер начинал блокировать inference по VRAM headroom.
type AlreadyLoadedAsError struct {
	ExistingName string
	Path         string
}

func (e *AlreadyLoadedAsError) Error() string {
	return fmt.Sprintf("model already loaded as %q (same file path %q)", e.ExistingName, e.Path)
}

// ModelInfo — информация о загруженной модели
type ModelInfo struct {
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	State        LoadState `json:"state"`
	Architecture string    `json:"architecture,omitempty"`
	NLayers      int       `json:"nLayers"`
	NHeads       int       `json:"nHeads"`
	NKvHeads     int       `json:"nKvHeads"`
	HeadDimK     int       `json:"headDimK"`
	HeadDimV     int       `json:"headDimV"`
	// HeadDimSource — R705 (2026-11-24): откуда взята ширина головы K/V.
	//
	//   bridge.HeadDimSourceGGUF     — <arch>.attention.key_length из модели (факт);
	//   bridge.HeadDimSourceComputed — прежняя формула n_embd/n_heads (оценка);
	//   ""                           — чисел нет вовсе (неизвестно).
	//
	// Поле нужно, чтобы «занижённая ширина» больше не выглядела измеренной: по
	// нему /api/models отдаёт head_dim_source, а балансер (R659/AutoTune) видит,
	// можно ли доверять геометрии.
	HeadDimSource     string    `json:"headDimSource,omitempty"`
	NEmbd             int       `json:"nEmbd"`
	NVocab            int       `json:"nVocab"`
	ContextSize       int       `json:"contextSize"`
	GGUFContextLength int       `json:"ggufContextLength"`
	SizeBytes         uint64    `json:"sizeBytes"`
	LoadedAt          time.Time `json:"loadedAt"`
	GPUCount          int       `json:"gpuCount,omitempty"`
	GPULayers         int       `json:"gpuLayers"`
	TensorSplit       []float32 `json:"tensorSplit,omitempty"`
	// R248/B3: vision-возможность — ЯВНЫЙ факт, а не догадка по имени.
	// Vision=true только если после загрузки модели удалось загрузить mmproj
	// и mtmd подтвердил vision (см. Bridge LoadMMProj). По этому полю
	// cppworker отдаёт capabilities.vision, а балансер решает: passthrough
	// картинок или честный 415.
	Vision      bool   `json:"vision"`
	MmprojPath  string `json:"mmprojPath,omitempty"`
	VisionError string `json:"visionError,omitempty"`
	// R248 (2026-10-27): честная деградация offload.
	//
	// Живой случай: Qwen3.8-27B (15,3 ГБ) на узле с 8 ГБ VRAM загрузилась как
	// «offloaded 20/66 layers to GPU» + 10184 MiB CPU_Mapped, но /api/models
	// показывал только запрошенное gpuLayers (-1) и НИЧЕГО о том, что большая
	// часть модели осталась в RAM. Оператор видел «загружено» и не понимал,
	// почему модель медленная. Теперь наружу уходит фактический offload и
	// признак деградации.
	OffloadDegraded bool   `json:"offloadDegraded,omitempty"`
	OffloadNote     string `json:"offloadNote,omitempty"`
	ActiveQueries   int    `json:"activeQueries"`
	TotalQueries    int64  `json:"totalQueries"`
	// Дополнительные параметры загрузки (Шаг 4 — /api/models/reload).
	// Хранятся вместе с моделью, чтобы при reload можно было
	// переиспользовать их как дефолты, если клиент не указал override.
	BatchSize     int  `json:"batchSize"`
	FlashAttnType int  `json:"flashAttnType"`
	NUMA          bool `json:"numa"`
	UseMmap       bool `json:"useMmap"`
	// Session 16 (2026-06-27): Parallel и KVCacheType — Session 16 Per-Model Profiles.
	// Parallel=0 = дефолт cppworker (=1), KVCacheType=0 = F16.
	Parallel    int    `json:"parallel"`    // n_parallel в llama.cpp
	KVCacheType string `json:"kvCacheType"` // "f16"/"q8_0"/"q4_0"
	// R118.16 (2026-10-14): ТОЧНАЯ раскладка слоёв по устройствам, как её
	// напечатал llama.cpp при загрузке (`load_tensors: layer N assigned to
	// device X`). Пусто = не удалось перехватить (stub-сборка или старая
	// llama.cpp) — потребитель обязан отличить это от «все слои на CPU».
	DeviceLayers []bridge.DeviceLayerRange `json:"deviceLayers,omitempty"`
	// R176 (2026-10-19): РАСКЛАДКА НЕ ПРИМЕНИЛАСЬ, ХОТЯ УЗЛЫ ПЕРЕДАЛИ.
	// Заполняется по ФАКТУ после загрузки (сверка переданных серверов с
	// фактической раскладкой слоёв): иначе тихое выпадение узла выглядело как
	// успешная загрузка, и оператор узнавал о деградации по скорости.
	LayoutDegraded       bool   `json:"layout_degraded,omitempty"`
	LayoutDegradedReason string `json:"layout_degraded_reason,omitempty"`
	// ContextPerSeq — R83 (2026-09-30): окно, которое фактически получил ОДИН
	// слот (llama.cpp: n_ctx_seq = n_ctx / n_seq_max). Показывается оператору,
	// потому что ContextSize в API — суммарное окно, и без этого поля «16384 +
	// parallel=2» выглядело как 16384 на клиента, хотя реально это 8192.
	ContextPerSeq int `json:"contextPerSeq,omitempty"`
	// === Loading state (Шаг «отображение загрузки в мониторе и вкладке бэкендов») ===
	// Заполняются пока State == StateLoading, чтобы UI мог показывать
	// «Загружается model-name 25s» и спиннер. После успеха/ошибки поля обнуляются.
	LoadingStartedAt time.Time `json:"loadingStartedAt,omitempty"`
	LoadingSizeBytes int64     `json:"loadingSizeBytes,omitempty"`
	LoadingError     string    `json:"loadingError,omitempty"`
	// LoadingProgressPct — R215 (2026-10-21): реальный процент прочитанных
	// весов (0..100) из progress-callback llama.cpp. -1 = неизвестно (stub-сборка
	// или загрузка ещё не отдала первый callback). Заполняется на ходу, поэтому
	// оператор видит, сколько осталось, и отмена перестаёт быть «наугад».
	LoadingProgressPct int `json:"loadingProgressPct,omitempty"`
	// LastUsedAt — момент последнего обращения к модели (генерация/стрим).
	// Поле заполняется в Backend.GetModel/ListModels из inst.lastUsedAt,
	// чтобы UI мог отображать «idle 5s» и для отладки reload-циклов.
	// Может быть zero, если модель только что загружена и ещё не использовалась.
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
	// Round 15.1 (2026-07-30): true если модель использует BatchedScheduler
	// для true parallel inference (ОДИН llama_decode для N sessions).
	// false = legacy Round 13 multi-slot path с serialized llama_decode.
	// Виден в /api/models (UI может показать "Batched: ON" / "Batched: OFF").
	BatchedParallel bool `json:"batchedParallel"`
	// Round 17 (2026-07-31): resolved в LoadModel из opts.EnableReasoning ??
	// cfg.DefaultEnableReasoning. Парсеры reasoning_content.go читают это
	// чтобы split'ить <think> для моделей вне IsReasoningModel() whitelist
	// (например, qwen3-instruct с SOFT prompt reasoning).
	// Виден в /api/models для UI transparency.
	ReasoningEnabled bool `json:"reasoningEnabled"`
	// Round 18 P0.1 (2026-08-03): single source of truth для capabilities.
	// Заполняется в ListModels из DefaultCapabilitiesForModelName (по имени) +
	// реального reasoningEnabled из instance. Прокидывается balancer'у для
	// X-Model-* headers и клиенту через /api/models.
	Capabilities *types.ModelCapabilities `json:"capabilities,omitempty"`
}

// Backend — основной объект CppBackend
type Backend struct {
	cfg    Config
	models map[string]*modelInstance // name → model
	mu     sync.RWMutex

	gpuCount   int
	gpuDevices []bridge.GPUDevice
	gpuManager *GPUManager
	initOnce   sync.Once
	initErr    error
	startTime  time.Time

	// R135 (2026-10-16): предупреждение о несовпадении CUDA-арок образа и карт
	// (см. cuda_arch_guard.go). Пусто = расхождений нет или образ не сообщил
	// свои архитектуры. Отдаётся в /api/info как cuda_arch_warning, чтобы
	// оператор увидел причину до первой загрузки модели.
	cudaArchWarning string

	// S1 (2026-10-13): снимок устройств раскладки (локальные + RPC-узлы) для
	// планировщика. Заполняется через rpcDeviceSnapshot с TTL rpcDeviceTTL:
	// список нужен ДО загрузки (memfit/tensor_split), а его получение — сетевой
	// запрос к узлам.
	rpcDevMu   sync.Mutex
	rpcDevices []bridge.DeviceInfo
	rpcDevAt   time.Time
	// R229: состав узлов, для которого собран rpcDevices. Изменился (донор исключён)
	// → снимок недействителен, иначе в нём остаются устройства мёртвого узла.
	rpcDevKey string

	// R229 (2026-10-22): узлы, исключённые как НЕДОСТУПНЫЕ на время работы с
	// моделью (TCP-проба, см. rpc_reach.go). Отдаётся в /api/models как
	// rpc_excluded: оператор видит, почему модель работает без донора.
	// Отдельный мьютекс: b.mu/rpcDevMu не должны пересекаться с этим полем.
	excludedRPCMu sync.Mutex
	excludedRPC   []string
	excludedRPCAt time.Time
	// R215 (2026-10-21): сколько гейтов готовности узла подряд не смогли получить
	// устройства. Два подряд при доступном по TCP узле = он завис и держит VRAM
	// прошлой сессии (см. rpc_ready_budget.go); счётчик отдаётся в
	// /api/models как rpc_layout.stuck_suspected.
	rpcReadyTimeoutStreak atomic.Int32
	// R224 (2026-10-22): восстановление после потери RPC-сессии ВО ВРЕМЯ
	// генерации. Сессия с узлом может умереть после загрузки (узел пересоздан,
	// сеть порвана): декодирование падает, а модель остаётся загруженной с мёртвым
	// устройством — каждая генерация падает, пока её не выгрузят вручную.
	// Выгружаем сами по факту сбоя (см. rpc_session_lost.go), не чаще
	// rpcSessionUnloadCooldown на модель.
	sessionLostMu sync.Mutex
	sessionLostAt map[string]time.Time
	// unloadForRecovery — точка подмены для тестов (nil → b.UnloadModel).
	unloadForRecovery func(name string) error
	// R174 (2026-10-18): ПОСЛЕДНИЙ УСПЕШНЫЙ снимок С УЗЛАМИ и список узлов, для
	// которых он снят.
	//
	// ЗАЧЕМ. Живой дефект на стенде: применить профиль из WebUI → модель
	// перезагружается → раскладка ИСЧЕЗАЕТ (было CPU:1 CUDA0:23 RPC0:19, стало
	// CPU:23 CUDA0:20). Причина не в приказе, а в защите R115: во время работы
	// (reload держит loadMu) rpcDeviceSnapshot НЕ обращается к узлу — иначе
	// запрос памяти устройства влезает в единственное соединение узла и клиент
	// падает по GGML_ABORT. Если кэш (TTL 30 с) к этому моменту истёк, снимок
	// собирался БЕЗ узлов → планировщик видел только локальную карту, клал 23 слоя
	// в RAM и молчал. Теперь при занятости берём последний успешный снимок, если
	// список узлов не менялся: планирование перестаёт зависеть от того, успел ли
	// истечь кэш, а к занятому узлу мы по-прежнему не лезем.
	rpcDevGood    []bridge.DeviceInfo
	rpcDevGoodAt  time.Time
	rpcDevGoodKey string
	// rpcWarnAt — когда последний раз писали в лог «узел раскладки недоступен»
	// (R114): без ограничения GET /api/models, который зовёт MemfitBudget, сыпал бы
	// одной и той же ошибкой на каждый запрос.
	rpcWarnAt time.Time

	// R114 (2026-10-13): наблюдение за узлами раскладки у ЗАГРУЖЕННЫХ моделей.
	// Пропал узел → модель выгружается в простое, иначе первый же запрос к ней
	// уронит мастер (abort внутри C-моста). См. rpc_health.go.
	rpcWatchMu   sync.Mutex
	rpcWatchdogs map[string]*rpcWatchdog

	modelManager *ModelManager
	metrics      *Metrics
	idleUnloader *IdleUnloadManager
	hfDownloader *HuggingFaceDownloader

	// per-model load locking: prevents race conditions when two goroutines
	// try to load the same model simultaneously.
	// Key: model name, Value: chan struct{} (closed when loading completes).
	loading map[string]chan struct{}
	// R303: метаданные заявок на загрузку (кто взял, когда). Позволяют отличить
	// «грузится» от «заявка осталась от умершей горутины» и дать оператору
	// возраст заявки вместо бесконечного ожидания. См. load_claims.go.
	loadClaims map[string]*loadClaimMetaR303
	loadMu     sync.Mutex

	// loadSingleFlight — R83 (шаг 3, инцидент 2026-09-25): сериализация загрузок.
	//
	// checkVRAMForModel считает бюджет VRAM ДО распределения, поэтому вторая
	// параллельная загрузка видит свободную память, которой уже нет. Живой лог:
	// два плана (gpu_layers=29 и 18) ушли в llama.cpp одновременно, свободной VRAM
	// стало 0 МБ, и процесс умер по SIGABRT с повреждением кучи glibc
	// («malloc(): unaligned tcache chunk detected»), после чего контейнер
	// перезапустился. Пока идёт загрузка, второй запрос обязан её ДОЖДАТЬСЯ, а
	// затем увидеть модель в b.models и получить «already loaded» — вместо второй
	// загрузки того же файла. Загрузки редки и длинны, поэтому мьютекс общий,
	// а не per-model: на один процесс приходится один GPU-бэкенд.
	loadSingleFlight sync.Mutex

	// inFlight — per-model счётчик активных inference-запросов.
	// Используется в handleReloadModel чтобы дождаться завершения
	// всех активных запросов ПЕРЕД UnloadModel — иначе reload обрывает
	// HTTP-соединения (EOF клиенту). Подробности см. в inflight.go.
	inFlight *InFlightCounter

	// Round 18 P0.2 (2026-08-03): per-request cancel tracking для /api/cancel.
	// Хранит context.CancelFunc каждой активной inference-генерации
	// (по requestID). Позволяет cancel API отменять отдельные запросы
	// (не дожидаясь полной остановки backend'а).
	activeGenerations *ActiveGenerations

	// Round 18 P0.3 (2026-08-04): per-user parallel counter.
	// Admission control: каждый X-User-Id имеет свой bucket, max = MaxParallelPerUser
	// из Config. Защищает от сценария "один клиент занял все слоты".
	userTracker *UserTracker

	// reloadPending — флаг «reload в процессе». Прокидывается в
	// /api/metrics → reload_pending heartbeat. Балансировщик читает
	// и НЕ пытается дёргать LoadModel, пока видит этот флаг.
	reloadMu        sync.Mutex
	reloadPending   string // имя модели, для которой идёт reload
	reloadStartedAt time.Time

	// Round 25 (2026-08-04): load time history для динамической оценки
	// estimateLoadTimeMs. Хранит последние N=20 успешных load'ов с
	// (size, ctxSize, durationMs, timestamp). Используется в cppworker'е
	// (handlers_model_async.go) для compute avgSpeedBytesPerSec на
	// основе реальных измерений вместо хардкода 100 MB/s.
	//
	// Thread-safe: ring buffer с защитой mu.RLock для чтения,
	// mu.Lock для записи (record add при завершении load).
	loadHistoryMu sync.RWMutex
	loadHistory   []LoadTimeRecord // FIFO, max 20 entries
}

// ReloadStartedAt возвращает момент начала текущего reload (zero если нет).
func (b *Backend) ReloadStartedAt() time.Time {
	b.reloadMu.Lock()
	defer b.reloadMu.Unlock()
	return b.reloadStartedAt
}
