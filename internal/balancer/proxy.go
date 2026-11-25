package balancer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/internal/backendgroup"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/internal/modelreplication"
	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Proxy - HTTP прокси для балансировки запросов
type Proxy struct {
	// R487 (2026-11-11): индекс «request_id → где заявка» (очередь или бэкенд).
	// Без него POST /api/cancel выбирал адресатов по МОДЕЛИ и рассылал приказ во
	// все бэкенды с загруженной моделью — убивая чужую работающую генерацию и не
	// снимая запись из очереди (живой архив второго стенда, 2026-11-11).
	reqIdxR487   *requestIndexR487
	reqIdxMuR487 sync.Mutex

	// R495 S2 (2026-11-18): классификатор «во что уперлась карта» (shadow).
	bottleneckMuR495  sync.Mutex
	bottleneckTracker *bottleneckTrackerR495

	// R598 (R495 S2b, 2026-11-19): EMA скорости префилла/декода — второй источник
	// для классификатора. Только данные: решения по нему не принимаются.
	throughputMuR598 sync.Mutex
	throughputEMA    *throughputEMAR598

	config   *types.LoadBalancerConfig
	backends map[string]*BackendState

	// profilesMu — R423 (2026-11-06): замок карты per-model профилей
	// (config.LlamaCppModelProfiles).
	//
	// ЗАЧЕМ. Профили пишет админка (PUT/DELETE профиля, импорт config.json,
	// загрузка profiles.json с диска), а читают горячие пути запроса: резолв
	// окна, preflight n_ctx, выбор модели для загрузки, AutoTune, таймауты.
	// Это разные горутины, а обычная map в Go не переживает одновременное
	// чтение и запись: получается либо DATA RACE (его и нашёл CI), либо
	// фатальный "concurrent map read and map write" — падение балансера.
	//
	// ПОРЯДОК ЗАМКОВ: profilesMu — ЛИСТОВОЙ, после него нельзя брать p.mu или
	// любой другой замок. Все читатели берут СНИМОК карты (profilesSnapshotR423)
	// и работают с ним уже без замка — см. model_profiles_guard.go.
	profilesMu sync.RWMutex

	// mu — КОРНЕВОЙ лок реестра бэкендов.
	//
	// LOCK ORDER (R284, 2026-10-29). Порядок захвата СТРОГО:
	//
	//	p.mu  ->  state.mu  ->  (чужие локи: metricsMgr.mu, sessionMgr, ...)
	//
	// Обратный порядок запрещён: если кто-то держит state.mu и берёт p.mu,
	// получается инверсия и вечный дедлок (R91 описал это для metricsMgr.mu).
	//
	// ПОД p.mu НЕЛЬЗЯ: сеть, диск, HTTP-вызовы, ожидание каналов, time.Sleep и
	// вызовы методов, которые сами берут p.mu. sync.RWMutex НЕ рекурсивен: если
	// горутина, держащая RLock, возьмёт RLock ещё раз, а между ними встанет
	// писатель (Lock), второй RLock не выдадут НИКОГДА — писатель ждёт первого
	// читателя, читатель ждёт писателя. Ровно это положило балансер под
	// нагрузкой агентских метрик (R284): selectByResourcesExcluding держал RLock
	// и вызывал checkResourceLimits, который брал RLock повторно, а писатель
	// (UpdateBackendAgentStatus на каждый POST метрик) уже ждал.
	//
	// Правила для новых мест:
	//  1. под p.mu — только работа с памятью;
	//  2. методы с суффиксом ...Locked НЕ берут p.mu и требуют, чтобы вызывающий
	//     уже держал его (checkResourceLimitsLocked, findBackendWithModelExcludingLocked);
	//  3. публичный метод, который может быть вызван из-под лока, обязан иметь
	//     ...Locked-вариант, а не «предполагать, что лок свободен»;
	//  4. сторож — lockorder_r284_test.go: он статически ищет в пакете пары
	//     «держит p.mu -> вызывает метод, берущий p.mu» и падает на них.
	mu         sync.RWMutex
	roundRobin int
	sessionMgr *SessionManager
	metricsMgr *MetricsManager
	queueMgr   *QueueManager
	predictor  *Predictor
	// healthChecker — опциональная зависимость. Выставляется через SetHealthChecker
	// после конструктора (избегаем circular dep). Используется для MarkUnhealthy
	// при persistent connection failures (Round 8 fix).
	healthChecker          *HealthChecker
	client                 *http.Client    // Клиент для обычных запросов
	streamingClient        *http.Client    // Клиент для streaming/SSE запросов (без таймаута)
	streamingTransportBase *http.Transport // Базовый Transport для per-request клонов с ResponseHeaderTimeout
	statePath              string          // Путь к state файлу
	saveTimer              *time.Timer     // Таймер для debounced autosave
	saveMu                 sync.Mutex      // Мьютекс для защиты saveTimer
	totalRequests          int64           // Atomic: всего запросов через прокси
	// D4.2 (2026-11-25): попытки инференс-запросов, включая отказы. Растут в
	// ServeHTTP до диспетчеризации; totalRequests (успехи) остаётся как было.
	totalAttempts  int64           // Atomic: попытки запросов (успех + отказ)
	ollamaRouter   *OllamaRouter   // Маршрутизатор Ollama API endpoint'ов
	llamaCppRouter *LlamaCppRouter // Маршрутизатор llama.cpp API endpoint'ов
	// R-Image (2026-09-27): маршрутизатор image-бэкендов (тип image_cpp):
	// /v1/images/*, /sdapi/v1/*, /api/image/*.
	imageRouter *ImageRouter

	// R-Image Phase 6 (2026-10-02): VRAM-гейт и ресурсный лок GPU для
	// сосуществования image-генерации с текстовым инференсом. Вся логика —
	// image_resources.go; инициализируется сразу после imageRouter.
	imageRes *imageResources

	// Round 31 #7 (2026-08-09): token usage tracking.
	// Atomic counters для агрегации prompt/completion tokens по моделям.
	// Используется для мониторинга через /api/v1/stats/tokens endpoint.
	// Ключ = model name, value = ModelTokenUsage.
	tokensByModelMu sync.RWMutex
	tokensByModel   map[string]*ModelTokenUsage

	// R302 (2026-10-29): калибровка оценки промпта по фактическим токенам
	// воркера (см. prompt_calibration.go). nil — калибровка выключена
	// (Proxy собран литералом в тесте); все потребители обязаны это переносить
	// и работать по сырому heuristic.
	promptCalib *promptCalibration

	// llamaCppMetricsPoller синхронизирует metricsMgr.llamaMetrics[].LoadedModels
	// с фактическим состоянием cppworker через периодический poll /api/models.
	// Нужен для отображения загруженных моделей в WebUI (страница GGUF) и
	// API /api/v1/gguf/backends, когда модели загружены в обход балансера.
	llamaCppMetricsPoller *llamaCppMetricsPoller

	// R118.15: канал закрытия петли замеров пропускной способности до узлов
	// раскладки (rpc_link_measure.go). nil — замеры не запускались.
	linkMeasureStop chan struct{}

	// R72 (P0): placement policy — решение о размещении модели.
	// Состояния в Proxy нет: политика читается из конфига
	// (p.config.Balancing.Placement), см. placement.go.

	// nctxReload — координатор adaptive n_ctx auto-reload (Stage 3, 5).
	// Решает, можно ли перезагрузить модель с большим n_ctx (если VRAM
	// позволяет), или вернуть клиенту 413. Вызывается из llamacpp_transport.go
	// когда upstream (cppworker) возвращает ErrNCtxNeedsReload.
	nctxReload *NCtxReloadCoordinator

	// R118.2 (2026-10-14): состояние раскладки слоёв (rpc_layout_guard.go).
	// rpcLayoutApplied — последний применённый состав узлов на бэкенд (чтобы не
	// переписывать конфиг cppworker'а на каждом тике), rpcLayoutReason — причина
	// деградации для API/WebUI, rpcProbe — проверка узла (подменяется в тестах).
	// Все три читаются и пишутся под p.mu; нулевое значение пригодно к работе.
	rpcLayoutApplied map[string]string
	rpcLayoutReason  map[string]string
	rpcProbe         func(string, time.Duration) bool

	// R349 (2026-10-31): короткий кэш проб узлов раскладки. Один проход guard'а
	// дёргал узел несколько раз (список для мастера + решение о приказе +
	// разрешение адреса R350), и донор видел 10-11 TCP-подключений в минуту при
	// простаивающем стенде. Под своим мьютексом, а не под p.mu: проба включает
	// сетевой dial, держать p.mu на это время нельзя.
	rpcProbeMu       sync.Mutex
	rpcProbeCachedBy map[string]rpcProbeCacheEntry

	// R133 (2026-10-16): управление СОБСТВЕННЫМ узлом раскладки бэкенда-донора
	// (rpc_layout_node.go). rpcNodeServed — помним, что узел подняли МЫ
	// (роль donor), чтобы при смене роли погасить именно своё решение, а не
	// ручной тумблер оператора; rpcNodeReason — почему узел не в том состоянии,
	// которого требует роль (для API/WebUI); rpcNodeReachable — последняя
	// проверка доступности узла с балансера. Под p.mu.
	rpcNodeServed    map[string]bool
	rpcNodeReason    map[string]string
	rpcNodeReachable map[string]bool
	// rpcNodeOrder/rpcNodeOrderAt — последний отправленный приказ узлу и его
	// время: повторяем не каждый тик, а только когда он снова нужен.
	// rpcNodeOrderState (R159) — грубое состояние воркера на момент приказа
	// (unknown|up|starting|down): пока состояние не изменилось, повтор глушим,
	// а как только изменилось (узел упал/начал слушать) — действуем сразу.
	rpcNodeOrder      map[string]string
	rpcNodeOrderAt    map[string]time.Time
	rpcNodeOrderState map[string]string

	// R118.15 (2026-10-14): замеры канала до узлов раскладки, сделанные САМИМ
	// балансером (rpc_link_measure.go): backendID → последний замер.
	// Раньше значение приходило только из .env оператора (LB_RPC_LINK_MBPS) и
	// молча устаревало. Под p.mu; нулевое значение пригодно к работе.
	rpcLinkMeasured map[string]rpcLinkMeasurement

	// R118.7 (2026-10-14): запомненные отказы «окно физически недостижимо»
	// (nctx_infeasible.go): cppworker ответил 422 на перезагрузку окна.
	// Пока запись жива, preflight не планирует перезагрузку выше потолка, а
	// отвечает клиенту 413 с причиной. Ключ — backendID|model. Под p.mu.
	nctxInfeasible map[string]nctxInfeasible

	// R119 (2026-10-15): запомненные отказы «такой модели на бэкенде НЕТ»
	// (model_missing.go): cppworker ответил 404 на попытку загрузки, то есть
	// модель отсутствует на диске. Пока запись жива (TTL 60 c), preflight НЕ
	// планирует перезагрузку, а клиент сразу получает 404 с причиной и списком
	// доступных моделей — иначе клиент уходил в retry-цикл 503 «is being
	// reloaded … retry in 30s» и падал по своему таймауту («Internal Server
	// Error»). Ключ — backendID|model. Под p.mu.
	modelMissing map[string]modelMissing
	// R185 (2026-10-20): запомненные ТЕРМИНАЛЬНЫЕ отказы «эта карта не
	// поддерживается образом» (cuda_arch_refusal.go). Живой разбор: образ
	// собран под CUDA_ARCH=86;120, карта sm_75 — первое GPU-ядро убивало воркер
	// SIGABRT, а клиент получал 503 «повтори» и крутил перезагрузки вечно.
	// Теперь отказ запоминается и клиент получает 400 с причиной и советом.
	// Ключ — backendID|model. Под p.mu.
	modelArchUnsupported map[string]modelArchUnsupported
	// R119 (2026-10-15, пункт 4): происхождение параметров загрузки модели —
	// какой слой дал каждое значение (request / profile / default_profile /
	// backend). Считается при загрузке (load_provenance.go) и отдаётся API,
	// чтобы оператор видел источник рядом со значением. Ключ — backendID|model.
	loadProvenance map[string]loadProvenanceRecord

	// lbAutoLoadAsync — R60.33 (2026-09-10): если true, auto-load
	// запускается в goroutine и balancer сразу возвращает 503+Retry-After
	// (вместо sync wait 3 мин). default=true. env LB_AUTO_LOAD_ASYNC=0
	// → legacy sync.
	lbAutoLoadAsync bool

	// lbAutoLoadWait — R67a (2026-09-23): сколько ждать завершения авто-загрузки
	// модели в async-режиме, прежде чем вернуть клиенту 503+Retry-After.
	// env LB_AUTO_LOAD_WAIT_SEC (0 = прежнее поведение «сразу 503»).
	// default 180s. См. autoload_wait.go.
	lbAutoLoadWait time.Duration

	// admission — R67b (2026-09-23): очередь ожидания свободного слота для
	// inference-запросов llama.cpp (см. admission_queue.go). Устраняет «резкий
	// 503» при занятых слотах и даёт per-session справедливость. Предел ожидания
	// (LB_ADMISSION_WAIT_SEC) хранится внутри очереди — так Proxy не растёт и
	// остаётся в своей allocator size class (govet fieldalignment).
	admission *admissionQueue

	// R229 (2026-10-22): протокол ключа admission-очереди (openai/ollama) — бейджи
	// в waiting_detail. Указатель: структура Proxy не растёт (fieldalignment).
	admissionProto *admissionProtocols

	// lbNCtxReloadAsync — R60.47 (2026-09-11): если true, n_ctx auto-reload
	// (triggered by cppworker 400 prompt_too_long) запускается в goroutine и
	// balancer СРАЗУ возвращает 503+Retry-After. default=true. env
	// LB_NCTX_RELOAD_ASYNC=0 → legacy sync reload+retry (single round-trip).
	//
	// Pre-R60.47: handleNCtxReloadActual блокировал HTTP handler до 5 мин
	// (cppworker waitTimeoutSec=300), клиенты (OpenWebUI 30-60s timeout)
	// cancel-или → "empty response". Retry приводил к cascade.
	lbNCtxReloadAsync bool

	// R54.4 (2026-08-24): AutoTune tracker — circuit breakers per (backend, model).
	// Защищает от reload storm когда AutoTune fix не удаётся.
	// Инициализируется lazily в triggerAutoTuneReload (нулевый указатель = default cfg).
	autoTuneTracker *AutoTuneTracker

	// R55.2 (2026-08-24): AutoTune history log - ring buffer последних N=500 events.
	// Используется для operator visibility: "что AutoTune делал за последние 24h?"
	// Инициализируется lazily в AutoTuneHistory() getter.
	autoTuneHistory *AutoTuneHistory

	// R54.9 (2026-08-24): Workload tracker — per-(backend, model) sliding window
	// num_ctx samples. Используется AutoTune для workload-aware KV cache
	// selection (light workload → f16, heavy → q4_0).
	// Инициализируется lazily в WorkloadTracker() getter.
	workloadTracker *WorkloadTracker

	// R245 (2026-08-27): защита ленивой инициализации workloadTracker/autoTuneHistory.
	//
	// WorkloadTracker() вызывается из proxyRequestLlamaCpp на каждый запрос, то есть
	// конкурентно из нескольких goroutine. Без мьютекса два запроса одновременно
	// видели nil, оба писали указатель (data race, ловится -race в CI), а второй
	// экземпляр терял накопленные samples. Отдельный мьютекс (не p.mu) — чтобы не
	// втягивать реестр бэкендов в этот путь и не ломать порядок блокировок.
	autoTuneLazyMu sync.Mutex

	// R54.6 (2026-08-24): ModelManager reference для AutoTune apply (load+unload).
	// Использует existing p.modelManager (set в NewProxy). SetModelManager() — alias
	// для совместимости с R54.6 handler'ами.

	// EventBus (вынесен в eventbus.go)
	eventBus *EventBus

	// R83 (2026-09-25): дедупликация уведомлений о провале загрузки модели.
	//
	// Agent присылает метрики каждые ~10 с, и причина провала живёт в cppworker
	// 10 минут — без дедупликации это ~60 одинаковых уведомлений на один провал,
	// а SSE ring buffer вмещает всего 100 событий, то есть полезное было бы
	// вытеснено (см. internal/api/handlers_events.go). Публикуем только СМЕНУ
	// причины (backendID → key) и одно info-событие при восстановлении.
	// Ключ — LoadFailureInfo.Key() (модель + код причины).
	loadFailureMu   sync.Mutex
	loadFailureSeen map[string]string

	// R700 (2026-11-24): дедупликация уведомлений о СРАБОТАВШИХ ГЕЙТАХ (диск,
	// RAM, VRAM, окно, слоты) — см. gate_notices.go.
	//
	// Ключ — «гейт|узел». Без дедупа один заблокированный по диску узел давал
	// бы событие на каждый заход в очередь (опрос ~6 раз в секунду), а SSE ring
	// хранит 100 событий: полезная история вытеснялась бы потоком одинаковых
	// строк. Публикуется одно уведомление на эпизод + повтор при смене чисел.
	gatesMu   sync.Mutex
	gatesSeen map[string]*gateNoticeStateR700

	// Round 40 #3 (2026-08-18): apiReverseProxy пересылает /api/v1/*
	// запросы с proxy-порта (18080) на локальный API-сервер (18081).
	// Без этого balancer не знает про /api/v1/* пути и они падают
	// в основной flow proxyRequest → queueRequest → зависают на 30s
	// timeout (Round 22 BUG #7). Теперь balancer сразу forward'ит
	// на API сервер, который знает про эти endpoint'ы.
	apiReverseProxy *httputil.ReverseProxy

	// Background controllers
	AutoPull        *AutoPullManager     // Менеджер автоматической загрузки моделей (Pull-on-Demand)
	unloadScheduler *UnloadScheduler     // Планировщик выгрузки моделей
	agentChecker    *agentTimeoutChecker // Проверка таймаута агентов

	// R91 (2026-10-09): каталог моделей НА ДИСКЕ по бэкендам
	// (см. model_catalog.go). Нужен, чтобы не отправлять запрос на узел, где
	// файла модели нет вовсе, и чтобы можно было безопасно поднять модель на
	// узле, где она лежит.
	modelCatalog *modelCatalog

	// R269: отслеживание актуальной стабильной версии llama.cpp (кэш на диске
	// + фоновая петля по TTL). nil = не запущено (тесты/урезанный прокси) —
	// все геттеры обязаны это переживать.
	llamaUpstream *llamaUpstreamTracker

	// RPC model distribution modules
	modelReplication    *modelreplication.ModelGroupManager
	replicationSelector *modelreplication.GroupAwareSelector
	replicationCtrl     *modelreplication.GroupController
	virtualModels       *virtualmodel.Registry
	virtualModelRouter  *virtualmodel.Router
	rpcCoordinator      *rpccoordinator.ModelCoordinator

	// R92 (S2, план 2026-10-07): логические бэкенды — группы узлов, между
	// которыми разрезана ОДНА модель (мастер + воркеры RPC). Реестр неизменяем
	// после построения; состояние узлов подтягивается замыканием
	// groupMemberInfoR92. См. backend_group.go.
	//
	// R420 (2026-11-03): atomic.Pointer, а НЕ голый *Registry.
	//
	// ЖИВОЙ ДЕФЕКТ (CI, -race, tests). NewProxy запускает GroupController
	// (initRpcModules → setupReplicationManager) ДО initBackendGroupsR92, и его
	// горутина читает реестр из freeBackendsForReplicationR379. Без
	// синхронизации запись указателя и чтение в другой горутине не имеют
	// happens-before: race detector показывал гонку на самой map внутри Registry
	// (IsWorker против NewWithModelInfo) — читатель мог увидеть недостроенный
	// объект. atomic.Pointer даёт и happens-before, и безопасную подмену при
	// reload конфига.
	backendGroups atomic.Pointer[backendgroup.Registry]

	// R304 (2026-10-30): снимок ролей раскладки для горячего пути — кто донор и
	// чей он. Неизменяемый объект под atomic.Pointer: читается без p.mu, поэтому
	// предикат «донор не держит своих копий» безопасен и там, где блокировка уже
	// взята (вложенный RLock при ожидающем писателе — дедлок R284).
	// Пересобирается при смене роли, на старте и на каждом тике гарда раскладки.
	// См. donor_exclusive.go.
	donorIdxR304 atomic.Pointer[donorIndexR304]

	// Phase 8 (2026-07-10): P.1 — rpc_coordinator production mode.
	// RpcCoordinatorDispatcher routes inference через coordinator при
	// OperatingMode=rpc_coordinator. Phase 8.5: добавлено поле + scaffold в
	// ServeHTTP. Phase 9: полная интеграция (streaming + circuit breakers).
	rpcDispatcher *RpcCoordinatorDispatcher

	// VirtualRouter routes virtual models (alias-on-pool) при
	// OperatingMode=virtual_router. Phase 8 P.2: перехватывает requests
	// с model=virtual:xxx и выбирает backend через Selector.
	virtualRouter *VirtualRouter

	// Прокси-логгер для отслеживания запросов
	proxyLogger *ProxyLogger

	// Model management
	modelManager *ModelManager

	// Recent clients tracking (for monitor discovery particles)
	recentClients   map[string]*types.RecentClient
	recentClientsMu sync.RWMutex

	// Кеширование доверенных прокси-сетей (парсятся один раз при инициализации)
	trustedNets []*net.IPNet

	// Graceful shutdown
	shuttingDown  atomic.Bool
	activeStreams sync.WaitGroup // счётчик активных SSE-сессий

	// R271 (2026-10-29): учёт активных стримов ПО БЭКЕНДУ и ПО МОДЕЛИ.
	//
	// activeStreams выше — только для graceful shutdown (он глобальный), и по
	// нему нельзя понять, идёт ли стрим на конкретном бэкенде. Из-за этого
	// перезагрузка модели по запросу ВТОРОГО клиента выгружала модель под
	// активным стримом первого, и тот обрывался с «stream ended by upstream
	// before completion». См. active_streams.go.
	streamsR271 activeStreamsR271

	// R664 (шаг A, исправление живого прогона r261120-r679): очередь отложенного
	// роста окна и учёт идущих ходов клиента. Рост по заявке клиента исполняется
	// между ходами, а не в preflight: unload+load под обслуживаемым запросом —
	// это 46.9 с до первого байта (живой факт стенда). Поле-значение (не
	// указатель): нулевое состояние рабочее, поэтому тестовые литералы
	// &Proxy{...} не ломаются. См. growth_deferral.go.
	growthDeferralR664 growthDeferralR664

	// Семафор для ограничения одновременных warmup (загрузка моделей в VRAM).
	// Каждая загрузка делает POST /api/generate, который блокирует слот Ollama.
	// Ограничение предотвращает исчерпание всех свободных слотов загрузками.
	warmupSem chan struct{}

	// coldLoadStateR659 — память о запущенных «загрузках вместо ожидания»
	// (cold_load_placement_r659.go). Нужна, чтобы заявки, одновременно ждущие в
	// очереди, не отправляли по POST /load каждая: загрузка одна, ожидать её
	// можно всем. Поле-значение (не указатель): нулевое состояние рабочее,
	// поэтому тестовые литералы &Proxy{...} и старые инициализаторы не ломаются.
	coldLoadStateR659 coldLoadStateR659

	// modelLatencyTracker собирает per-model метрики генерации (tokens/sec,
	// inter-token gap, first-byte latency) для адаптивного расчёта таймаутов.
	// Используется в proxyRequestLlamaCpp, handleStreamingResponse и proxyRequest
	// для замены глобальных таймаутов на per-model.
	modelLatencyTracker *ModelLatencyTracker

	// metricsHTTPDoer — HTTP-клиент для heartbeat /api/info polling
	// в preflightNCtxReloadIfNeededSync (см. queryBackendReloadPending).
	// По умолчанию nil → используется реальный *http.Client{Timeout: 2s}.
	// Тесты могут подменить на in-memory stub, чтобы не зависеть от сети
	// (см. internal/balancer/nctx_reload_sync_test.go).
	metricsHTTPDoer interface {
		Do(*http.Request) (*http.Response, error)
	}
}

// ServeHTTP - обработка HTTP запросов
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// R253.1: служебный маркер preflight-диалога принимается ТОЛЬКО от нашего
	// self-proxy (loopback + пер-процессный токен). Внешний клиент, приславший
	// такой заголовок, не должен ни отключать рост окна, ни притворяться
	// внутренним запросом — вырезаем до любой обработки.
	stripExternalDialogMarker(r)
	// R479 (2026-11-09): алиасы поверхностей (/ollama/api/*, /openai/v1/*)
	// снимаем ДО определения типа и диспетчеризации. Раньше префикс /ollama
	// понимал только LlamaCppRouter, и на кластере без cppworker эти пути
	// отдавали пустоту/503, хотя /api/* работал.
	if normalized, aliased := normalizeSurfaceAliasPathR479(r.URL.Path); aliased {
		logger.Get().Debugw("surface alias normalized",
			"from", r.URL.Path, "to", normalized, "method", r.Method)
		r.URL.Path = normalized
		r.URL.RawPath = ""
		w.Header().Set("X-LB-Surface-Alias", "1")
	}
	// R479: «последние клиенты» для монитора. recordRecentClient существовал, но
	// не вызывался ниоткуда: cluster_state читал пустую карту, панель показывала
	// пустоту. Возвращаем вызов (и он же использует peekBodyR479 — чтение тела
	// без второй копии).
	p.recordRecentClient(r)
	startTime := time.Now()
	// R493-12a: момент приёма запроса уезжает в контекст — от него считается
	// «сколько клиент ждал» (очередь, авто-загрузка, preflight). Внутри
	// proxyRequestLlamaCpp своё время старта ставится ПОЗЖЕ всех ожиданий, и
	// построенные от него окна статистики показывали 0 там, где клиент ждал
	// десятки секунд.
	r = r.WithContext(withRequestStartR493(r.Context(), startTime))

	// ==== Correlation ID ====
	// Генерируем (или берём из заголовка X-Request-ID) уникальный id для этого
	// запроса и кладём его в контекст. Все downstream-логи (handleOpenAIChat
	// Completions, ensureModelLoadedOnBackend, queryCppWorkerModels, ModelManager
	// .ExecuteOperation, etc.) подхватывают его через RequestIDFromContext()
	// и автоматически добавляют в log-запись как поле request_id. Это позволяет
	// проследить всю цепочку Cline → balancer → cppworker по одному grep'у.
	rid, ctxWithRID := getOrGenerateRequestID(r)
	r = r.WithContext(ctxWithRID)
	w.Header().Set(requestIDHeader, rid) // echo в response для удобства curl-проверки
	// R477 (2026-11-09): стадия хода. Без неё по логу нельзя было сказать, ГДЕ
	// запрос умер (выбор бэкенда, очередь, preflight, апстрим, стрим).
	tracker := newStageTrackerR477()
	r = withStageTrackerR477(r, tracker)
	// R484: трекер ФАКТИЧЕСКОГО транспорта (stream/json) — для request trace и
	// прокси-логов. Флаг stream в запросе и транспорт не одно и то же.
	r = withTransportTrackerR484(r)
	SetStageR477(r.Context(), stageSelectR477)
	// R477: паника в обработчике не должна убивать запрос молча — фиксируем
	// стадию и отвечаем 500, если ещё ничего не отправлено.
	defer func() {
		if rec := recover(); rec != nil {
			stage, _ := CurrentStageR477(r.Context())
			recordFailureR477(diagFailureR477{
				RequestID: rid, Method: r.Method, Path: r.URL.Path,
				Stage: stage, Status: http.StatusInternalServerError,
				Reason: "panic: " + panicTextR477(rec),
			})
			logger.Get().Errorw("ServeHTTP: panic recovered",
				"request_id", rid, "stage", stage, "path", r.URL.Path, "panic", rec)
			func() {
				defer func() { _ = recover() }() // заголовки могли быть отправлены
				http.Error(w, "internal error (request_id: "+rid+")", http.StatusInternalServerError)
			}()
		}
	}()
	// R476 (2026-11-07): одна строка INFO на запрос с длительностью, кодом и
	// бэкендом — по ней ход находится в логе обеих сторон (id тот же у воркера).
	// LB_REQUEST_TRACE=off отключает строку, заголовок остаётся.

	w, finishTrace := startRequestTraceR476(w, r, rid)
	defer finishTrace()
	logger.Get().Debugw("ServeHTTP: request received",
		"request_id", rid,
		"method", r.Method,
		"path", r.URL.Path,
		"remote", r.RemoteAddr,
	)

	// CORS headers для всех ответов (нужно для OpenWebUI и других браузерных клиентов)
	origin := r.Header.Get("Origin")
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, Accept, Origin")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
	}

	// Обработка CORS preflight (OPTIONS) запросов
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// R229 (2026-10-22): явная ОТМЕНА генерации клиентом (Ollama POST /api/cancel).
	// Живой дефект: балансер маршрутизировал отмену как обычную генерацию, не
	// отвечал (таймаут 15 с), а стрим доходил до конца (380 чанков, 62 с).
	// Отмена — служебный приказ воркерам, а не генерация: обрабатываем сразу.
	if isClientCancelPath(r.URL.Path) {
		p.handleClientCancel(w, r)
		return
	}

	// D4.2 (2026-11-25): СЧИТАЕМ ПОПЫТКИ, А НЕ ТОЛЬКО УСПЕХИ.
	//
	// Живой замер: `cluster.totalRequests=0` при `recentClients.requestCount=6`
	// — запросы были (и получали 503), но totalRequests инкрементится только
	// после успешного ответа апстрима. Панель читала 0 как «запросов не было».
	// Теперь попытка считается ДО диспетчеризации, для инференс-путей (чтение и
	// админ-ручки не считаем — иначе число перестанет означать нагрузку).
	if !isReadOnlyOrMgmtEndpoint(r.URL.Path) {
		atomic.AddInt64(&p.totalAttempts, 1)
	}

	// Phase 8 (2026-07-10): P.1 — rpc_coordinator production mode scaffold.
	// Если balancer в OperatingMode=rpc_coordinator + dispatcher инициализирован
	// + path is intercepted inference endpoint → маршрутизируем через
	// ModelCoordinator. Phase 8.5: добавлен scaffold (полная версия в Phase 9).
	//
	// ВАЖНО: scaffold безопасен — если rpcDispatcher == nil (default bundled
	// config) или mode != rpc_coordinator, выполнение идёт дальше как обычно
	// (existing behavior preserved). Phase 9 заменит ServeHTTP stub на полную
	// реализацию (parse body, call ShouldRoute, dispatch Infer/Stream).
	if IsRpcCoordinatorMode(p.config.Balancing.OperatingMode) &&
		p.rpcDispatcher != nil &&
		p.rpcDispatcher.IsRpcPath(r.URL.Path) {
		logger.Get().Debugw("rpc_coordinator: intercepting request",
			"path", r.URL.Path, "method", r.Method)
		p.rpcDispatcher.ServeHTTP(w, r)
		return
	}

	// Phase 8 (2026-07-11): P.2 — virtual_router production mode interceptor.
	// Если balancer в OperatingMode=virtual_router + VirtualRouter инициализирован
	// + path is intercepted inference endpoint → проверяем body на virtual model.
	//
	// ВАЖНО: VirtualRouter парсит body сам (нужно для extract model name).
	// Если model не в registry — falls through к стандартному flow без изменений.
	// Default bundled config (OperatingMode="standard" или "") → не срабатывает.
	//
	// R74 (P1, placement policy): pool для КОНКРЕТНОЙ модели решает политика —
	// `balancing.placement.models[].strategy=pool` (или `auto` для алиаса)
	// включает тот же путь БЕЗ глобального operatingMode=virtual_router.
	if p.virtualRouter != nil &&
		p.virtualRouter.IsActive() &&
		p.virtualRouter.IsVirtualPathRequest(r) {
		if virtualModel, matched := p.virtualRouter.MatchVirtualRequest(r); matched {
			poolEnabled := IsVirtualRouterMode(p.config.Balancing.OperatingMode)
			poolSource := "operatingMode"
			if !poolEnabled {
				sizeGB := float64(p.getModelSizeBytes(virtualModel)) / (1024 * 1024 * 1024)
				decision := p.ResolvePlacement(virtualModel, sizeGB, r.Header.Get("X-LB-Placement"))
				p.recordPlacementDecision(decision)
				if decision.Strategy == types.PlacementPool {
					poolEnabled = true
					poolSource = string(decision.Source)
				}
			}
			if poolEnabled {
				w.Header().Set("X-LB-Placement", string(types.PlacementPool))
				w.Header().Set("X-LB-Placement-Source", poolSource)
				logger.Get().Infow("virtual_router: intercepting request (pool)",
					"path", r.URL.Path, "method", r.Method,
					"model", virtualModel, "pool_source", poolSource)
				p.virtualRouter.ServeHTTP(w, r)
				return
			}
			logger.Get().Warnw(i18n.T("balancer.proxy.virtual_alias_pool_not_allowed_warn"),
				"path", r.URL.Path, "model", virtualModel,
				"operating_mode", p.config.Balancing.OperatingMode)
		}
	}

	// Единый разбор тела запроса (избегаем повторного чтения в recordRecentClient + proxyRequest)
	parsed := p.parseRequestBody(r)
	if parsed.TooLarge {
		sendBodyTooLarge(w, r)
		return
	}
	model := parsed.Model
	isStream := parsed.Stream
	// Кладём разобранное тело в контекст: determineRequestBackendType и
	// slot-handler'ы переиспользуют его без повторного парсинга body.
	r = withParsedRequest(r, parsed)
	// R493 (этап 1): единое решение о запросе — и в контекст, явным ключом.
	r = withRequestPlanR493(r, parsed.Plan)

	// Определяем требуемый тип бэкенда из URL запроса
	bt := p.determineRequestBackendType(r)
	allowedTypes := p.getAllowedTypesList(bt)

	// Трекинг всех клиентов для монитора (использует уже распарсенные данные)
	p.recordRecentClientParsed(r, parsed)

	// R72 (P0) + R76 (P3): placement policy — решение о размещении модели.
	//
	// Решение видно в логах, в счётчиках GET /api/v1/placement и в заголовках
	// ответа. R76 добавил §6 плана: если политика не может обслужить
	// заявленную стратегию, а `placement.fallback=error` (default) — отвечаем
	// явной 503 с причиной, а не подменяем стратегию молча. При `fallback=single`
	// запрос обслуживается обычным путём, но деградация видна в заголовке
	// X-LB-Placement-Fallback.
	if model != "" {
		sizeGB := float64(p.getModelSizeBytes(model)) / (1024 * 1024 * 1024)
		decision := p.ResolvePlacement(model, sizeGB, r.Header.Get("X-LB-Placement"))
		p.recordPlacementDecision(decision)

		if reason, reject := p.placementRejection(decision); reject {
			writePlacementUnavailable(w, decision, reason)
			return
		}

		declaredStrategy := decision.Strategy
		// R79: деградировавшая стратегия (не хватает бэкендов для копий)
		// исполняется как single — клиент не должен видеть X-LB-Placement:
		// replicated, если фактически отвечает одна копия. При fallback=single
		// (или auto.allowDegraded=true) запрос обслуживается, но подмена видна
		// в заголовках: X-LB-Placement = фактически исполненная стратегия,
		// X-LB-Placement-Fallback = заявленная политикой.
		if !decision.Executable || (decision.Degraded && decision.Strategy != types.PlacementSingle) {
			// fallback=single (иначе мы бы уже отказали): исполняем обычный путь.
			w.Header().Set("X-LB-Placement-Fallback", string(declaredStrategy))
			decision.Strategy = types.PlacementSingle
		}
		w.Header().Set("X-LB-Placement", string(decision.Strategy))
		w.Header().Set("X-LB-Placement-Source", string(decision.Source))
		if decision.Source != PlacementSourceDisabled {
			ridLog(ctxWithRID).Debugw(i18n.T("balancer.proxy.placement_decision_debug"),
				"model", model, "strategy", decision.Strategy,
				"declared_strategy", declaredStrategy,
				"source", decision.Source, "reason", decision.Reason,
				"rule", decision.MatchedRule, "model_size_gb", sizeGB,
				"executable", decision.Executable, "degraded", decision.Degraded)
		}
	}

	// Round 27 follow-up (v0.5.15): disabled-profile check на самом раннем
	// этапе ServeHTTP — до routeRequest, до selectBackend, до любого proxy.
	//
	// Почему здесь, а не в ensureModelLoadedOnBackend (как было в v0.5.15.1):
	//   - В mixed mode (bt == "") /api/chat и /api/generate идут через main
	//     proxy flow, который НЕ вызывает ensureModelLoadedOnBackend для
	//     уже-загруженных моделей (они идут сразу в proxyRequest).
	//   - Только llamacpp_router.Route → handleChat вызывает ensureModelLoadedOnBackend,
	//     но это срабатывает только при bt == BackendTypeLlamaCpp.
	//   - В default bundled-full config (single llama.cpp backend) bt == "" → fall
	//     through → proxyRequest → cppworker → SIGABRT.
	//
	// Поэтому ранняя проверка здесь — единственное место, которое ВСЕГДА
	// срабатывает для disabled-модели, независимо от bt и routing path.
	if model != "" {
		if prof, ok := p.GetModelProfile(model); ok && prof.Disabled {
			msg := fmt.Sprintf("model %q is marked as disabled in profile (broken: see profile.notes). "+
				"Request refused. Use a different model or remove the disabled flag from the profile.",
				model)
			ridLog(ctxWithRID).Warnw("ServeHTTP: model disabled in profile, refusing request",
				"path", r.URL.Path, "model", model, "profileNotes", prof.Notes)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
			return
		}
	}

	// Очистка зомби-сессий перед обработкой запроса.
	// Предотвращает ситуацию "все бэкенды заняты" из-за старых оборванных сессий.
	p.cleanupZombieSessions()

	path := r.URL.Path
	clientName := p.getClientName(r)

	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{\"status\":\"healthy\"}"))
		return
	}

	if r.URL.Path == "/metrics" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "# OllamaLegion balancer metrics\n")
		fmt.Fprintf(w, "healthy 1\n")
		return
	}

	// Оборачиваем ResponseWriter для захвата статус-кода
	sr := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
	w = sr

	isChatOrGenerate := (path == "/api/generate" || path == "/api/chat")
	_ = isChatOrGenerate // используется для логирования/defer ниже

	// Объявляем переменные до defer, чтобы замыкание захватило их финальные значения
	var targetBackend string
	var sessionID string

	// Defer-логирование результата запроса
	defer func() {
		if p.proxyLogger == nil {
			return
		}
		durationMs := time.Since(startTime).Milliseconds()
		// R481: request_id и стадия — чтобы запись в «Прокси-логах» можно было
		// связать с логами балансера/воркера и понять, какой компонент дал сбой.
		stageNow, _ := CurrentStageR477(r.Context())
		entry := ProxyLogEntry{
			Timestamp:  startTime,
			Method:     r.Method,
			Path:       path,
			Model:      model,
			ClientIP:   p.getClientRealIP(r),
			UserAgent:  r.UserAgent(),
			ClientName: clientName,
			BackendID:  targetBackend,
			SessionID:  sessionID,
			StatusCode: sr.statusCode,
			DurationMs: durationMs,
			Stream:     isStream,
			RequestID:  rid,
			Stage:      stageNow,
			Transport:  TransportR484(r.Context()),
		}
		// Round 16 (2026-07-10): для 4xx/5xx — заполняем Error field чтобы в WebUI
		// логах (Logs page) видно что произошло. Используем sr.errBody
		// (накоплен в statusRecorder через Write/WriteHeader).
		if sr.statusCode >= 400 && sr.errBody != "" {
			entry.Error = sr.errBody
		}
		p.proxyLogger.Append(entry)
	}()

	ctx := context.WithValue(r.Context(), modelContextKey, model)
	ctx = context.WithValue(ctx, streamContextKey, isStream)
	r = r.WithContext(ctx)

	sessionID = p.getSessionIDWithModel(r, clientName, model)
	if p.routeRequest(w, r) {
		return
	}

	if sessionID != "" && p.config.Balancing.SessionStickiness && !isEmbeddingsRequest(path) {
		// Для всех клиентских запросов (chat, generate) используем session stickiness
		// Embeddings (/api/embed, /api/embeddings) исключаются — они не требуют привязки к сессии
		if session := p.sessionMgr.Get(sessionID); session != nil {
			// R66c (2026-09-22): BackendID читаем через снимок под мьютексом.
			// Раньше здесь было `targetBackend = session.BackendID` по общему
			// указателю, который параллельно переписывает SessionManager.Set
			// (строка ниже) — гонка на каждом кластере с двумя одновременными
			// запросами от одного клиента (session id общий).
			targetBackend, _ = p.sessionMgr.GetBackendID(sessionID)

			p.mu.RLock()
			backendState, exists := p.backends[targetBackend]
			p.mu.RUnlock()

			// R66c: статус бэкенда — под state.mu (его пишут
			// UpdateBackendStatus/EvacuateBackend/agent_manager).
			backendHealthy := false
			if exists && backendState != nil {
				backendState.mu.Lock()
				backendHealthy = backendState.Backend.Status == types.StatusHealthy
				backendState.mu.Unlock()
			}

			if !exists || !backendHealthy {
				logger.Get().Warnw("session backend unavailable, selecting new backend",
					"session_backend", targetBackend,
					"exists", exists,
				)
				targetBackend = ""
			} else {
				// R118.5 (2026-10-14): ПРИВЯЗКА К КОПИИ, КОТОРАЯ НЕ ПОКРЫВАЕТ ОКНО.
				//
				// Живой дефект: сессия клиента была привязана к копии модели с окном
				// 4096 на слот, клиент просил 8192 — при этом на мастере лежала
				// подготовленная копия с 16384 на слот. Запрос уходил в привязанную
				// копию, preflight планировал reload, cppworker отвечал 422, клиент
				// ждал ~33 с и получал окно меньше заготовленного. Теперь, если
				// привязанная копия окно не покрывает, а другая ГОТОВАЯ копия
				// покрывает — перепривязываемся (это и есть «prepared copy wins»).
				if parsed.NumCtx > 0 && !p.modelCopyCoversWindow(targetBackend, model, parsed.NumCtx) {
					if better, why := p.selectModelCopyForWindow(model, parsed.NumCtx, nil, allowedTypes); better != "" && better != targetBackend {
						logger.Get().Infow(i18n.T("balancer.proxy.session_restick_copy_not_covering_window_info"),
							"session", sessionID, "model", model,
							"from", targetBackend, "to", better,
							"requested_n_ctx", parsed.NumCtx, "reason", why)
						targetBackend = better
					}
				}

				forceRebalance := false
				backendState.mu.Lock()
				active := backendState.ActiveReqs
				maxReqs := backendState.Backend.EffectiveMaxConcurrentRequests()
				backendState.mu.Unlock()

				// R139 (2026-10-16): нагрузку ЭТОЙ ЖЕ сессии не считаем поводом
				// уводить её на другой backend.
				//
				// Живой дефект (deepseek harness через балансер, длинная
				// генерация): сессия привязана к прогретой копии, её
				// собственный стрим в полёте (ActiveReqs=1 при
				// MaxConcurrentReqs=1) — и следующий запрос той же сессии
				// «ребалансировался» на менее загруженный backend, где модели
				// нет и её надо грузить заново. Клиент получает десятки секунд
				// тишины (pi-ai stream idle timeout 300000ms), а если тот
				// воркер затем падает — EOF/502. Сессия должна дождаться
				// СВОЕГО слота на прогретой копии.
				//
				// Чужую нагрузку (другие сессии) учитываем как раньше:
				// ребалансировка ради справедливости сохраняется.
				own := p.sessionInflight(sessionID)
				foreign := active - own
				if foreign < 0 {
					foreign = 0
				}

				if maxReqs > 0 && foreign > 0 {
					loadRatio := float64(foreign) / float64(maxReqs)
					if loadRatio >= 1.0 {
						forceRebalance = true
					}
					if loadRatio > 0.50 || forceRebalance {
						// R459 (2026-11-06): «менее загруженный» не значит
						// «лучше». Живой дефект: живую сессию увели на копию с
						// МЕНЬШИМ окном (32768 против 65536 на слот), и
						// агентский ход сломался — контекст перестал
						// помещаться посреди работы. Переезд на холодный
						// бэкенд (модели нет) — тот же класс: unload+load
						// посреди сессии. Отсекаем таких кандидатов в том же
						// проходе отбора, а не после.
						// R480 (P1-4): «нужное окно» считается как
						// max(потребность запроса + резерв, окно АКТИВНОЙ
						// сессии) — вариант C из
						// docs/architecture/r459-window-policy-options.ru.md.
						// Раньше need всегда равнялся окну сессии, и короткий
						// запрос не мог уехать с занятой широкой копии.
						moveGate := p.sessionMoveGateR480(targetBackend, model, sessionID, parsed.NumCtx,
							p.promptTokensForMoveR480(parsed.RawBody, path))
						altBackend := p.findLessLoadedBackendWithModelWhere(model, targetBackend, allowedTypes, moveGate)
						if altBackend != "" {
							logger.Get().Infow("rebalancing session to less loaded backend (same model)",
								"session", sessionID, "from", targetBackend, "to", altBackend,
								"load_ratio", loadRatio, "force", forceRebalance,
								"window_gate", "R459")
							targetBackend = altBackend
						} else {
							altBackend = p.findLessLoadedBackendAnyWhere(model, targetBackend, allowedTypes, moveGate)
							if altBackend != "" {
								logger.Get().Infow("rebalancing session to less loaded backend (any)",
									"session", sessionID, "from", targetBackend, "to", altBackend,
									"load_ratio", loadRatio, "force", forceRebalance,
									"window_gate", "R459")
								targetBackend = altBackend
							} else if forceRebalance {
								// R493 (долг 12m): уровень зависит от того, примет ли
								// запрос единая очередь — см. logForceRebalanceNoAltR493.
								logForceRebalanceNoAltR493(sessionID, targetBackend, loadRatio)
								targetBackend = ""
							}
						}
					}
				}

				// Always update session binding — new backend or re-sticky to current
				if targetBackend != "" && p.config.Balancing.SessionStickiness {
					p.sessionMgr.SetWithProtocol(sessionID, targetBackend, model, clientName, p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
				}
			}
		}
	}

	if targetBackend == "" {
		// Round 22 (2026-08-03): read/mgmt endpoints (show, pull, copy, create,
		// delete, push, blobs, models, models/files) НЕ требуют загруженной
		// модели. Пропускаем SyncModelLoad (P3 warmup), иначе balancer
		// зависает на 10-30s ожидая load модели, которая endpoint'у не нужна.
		// ТАКЖЕ пропускаем VRAM headroom check в selectBackend (P1), потому что
		// read operations не нагружают GPU — они могут работать при 95% VRAM.
		skipWarmup := isReadOnlyOrMgmtEndpoint(path)
		if skipWarmup {
			round22SkipWarmupTotal.Add(1)
		}
		if skipWarmup {
			// Для read/mgmt — выбираем backend минуя VRAM check.
			// Сначала ищем backend где модель загружена, потом любой healthy.
			targetBackend = p.findModelOnAnyBackendNoVRAMCheck(model, bt)
			if targetBackend == "" {
				targetBackend = p.selectAnyHealthy(bt)
			}
		} else {
			// R118.5: при нескольких загруженных копиях предпочитаем ту, что
			// покрывает окно клиента (см. model_copy_window.go).
			targetBackend = p.selectBackendPreferWindow(model, bt, parsed.NumCtx)
		}
		if targetBackend != "" && p.config.Balancing.SessionStickiness && !isEmbeddingsRequest(path) {
			sessionID = p.getSessionIDWithModel(r, clientName, model)
			// R696: привязка создаётся ТОЛЬКО на узел, где модель есть.
			//
			// ЖИВОЙ ДЕФЕКТ (архив второго стенда, r681): selectBackendPreferWindow
			// отдавал простаивающий ASTRA-I9, где нужной модели НЕТ, и привязка
			// сессии создавалась ДО того, как это выяснялось. Дальше за тот же ход
			// срабатывал аварийный обход R303 и перевешивал привязку (3 раза за
			// 13 минут), а если живой копии не находилось — следующий запрос той же
			// сессии возвращался на узел без модели и ждал загрузку. Проверка
			// «модель на узле есть» обязана стоять ДО записи в sessionMgr: это
			// условие корректности привязки, а не её оптимизация.
			//
			// Стоимость: modelAbsentOnBackendR303 отвечает из кэша метрик, а живой
			// опрос делает только когда кэш молчит (и кэширует результат).
			if ok, why := p.sessionBindAllowedR696(targetBackend, model, skipWarmup); ok {
				p.sessionMgr.SetWithProtocol(sessionID, targetBackend, model, clientName, p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
			} else if logger.Get() != nil {
				logger.Get().Infow("R696: привязка сессии не создана — на выбранном узле модели нет",
					"session", sessionID, "backend", targetBackend, "model", model,
					"reason", why, "next_step", "следующий выбор/обход R303 решит заново")
			}
		}
	}

	// R303 (D4): залипшая сессия НЕ должна держать запрос на узле, где модели нет,
	// пока она жива на другом узле.
	//
	// ЖИВОЙ ДЕФЕКТ (двухузловой отчёт, D4): донор держал gemma, кэш метрик этого
	// ещё не видел (поллер раз в ~30 c), запрос попадал на пустой мастер, preflight
	// планировал загрузку с нуля и держал клиента 300 c, после чего отдавал 503
	// «being reloaded to n_ctx=32768 (loaded=0)» — при живой модели рядом.
	//
	// Здесь проверка стоит ДО слотов и до выбора транспорта, поэтому она работает
	// и для залипшей сессии (ветка выше выставила targetBackend из sessionMgr и
	// selectBackendPreferWindow уже не вызывался). Цена: живой опрос только когда
	// кэш о модели на этом узле молчит И модель там действительно отсутствует;
	// результат опроса кэшируется на несколько секунд (live_copy_probe.go).
	if targetBackend != "" && model != "" && !isReadOnlyOrMgmtEndpoint(path) &&
		p.modelAbsentOnBackendR303(targetBackend, model) {
		if alt, altPerSeq, ok := p.findLiveLoadedCopyR303(model, parsed.NumCtx,
			map[string]bool{targetBackend: true}); ok {
			logger.Get().Warnw("R303: сессия залипла на узле без модели — переезжаем туда, где она загружена",
				"session", sessionID, "model", model, "from", targetBackend, "to", alt,
				"reason", "sticky_backend_model_absent",
				"alt_loaded_per_seq", altPerSeq, "requested_n_ctx", parsed.NumCtx,
				"covers_request", parsed.NumCtx <= 0 || altPerSeq >= parsed.NumCtx,
				// D5 (2026-11-25): ЯВНЫЙ ПРИЗНАК ОБХОДА РЕСУРСНОГО ГЕЙТА.
				//
				// R303 перевозит заявку туда, где модель уже загружена, НЕ
				// спрашивая жёсткие лимиты (диск/RAM) — это сознательное
				// спасение живой сессии: иначе запрос ждал бы ресурсов, хотя
				// готовая копия уже работает. Живой разбор второго стенда
				// показал обратную сторону: снаружи это выглядит как «с чужого
				// IP обслуживается, а с моего — нет», и по логу нельзя было
				// понять, что ресурсный гейт был обойдён, а не пройден.
				// Теперь обход назван в самом событии, чтобы разбор не гадал.
				"resource_gate_bypassed", true,
				"bypass_policy", "r303_rescue_loaded_copy")
			targetBackend = alt
			// Привязку сессии перевешиваем на новый узел: иначе следующий запрос
			// той же сессии снова вернётся на узел без модели (пинг-понг 503).
			if sessionID != "" && p.config.Balancing.SessionStickiness && !isEmbeddingsRequest(path) {
				p.sessionMgr.SetWithProtocol(sessionID, targetBackend, model, clientName,
					p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
			}
		}
	}

	// R757 (2026-11-25): ФАЙЛА МОДЕЛИ НЕТ НА ВЫБРАННОМ УЗЛЕ — ПЕРЕЕЗЖАЕМ ТУДА,
	// ГДЕ ОН ЕСТЬ.
	//
	// ЖИВАЯ ЖАЛОБА (архив второго стенда, 06:10): «она не стартанула с запроса от
	// пользователя, а поднятая вручную работала». Разбор лога: выбор отдал
	// A10-126, где файла модели НЕТ (каталог это знал — `R696: привязка сессии не
	// создана ... reason=model_absent_on_backend`), а сам файл лежал на
	// cppworker-gpu-bundled-agent. Гейт автозагрузки честно попытался поднять
	// модель именно на A10-126, воркер ответил `model ... not found in ./models`,
	// и клиент получил терминальный 404 (`model_not_found`, R325.2). Ручная
	// загрузка на узле, где файл есть, подняла модель за 8.2 с.
	//
	// ПОЧЕМУ ЭТОГО НЕ ЛОВИЛИ РАНЬШЕ. R303 спасает только когда модель УЖЕ
	// загружена на другом узле (живая копия). Здесь живых копий не было: модель
	// только что выгрузили/пересобрали, и «правильный» узел определялся именно
	// наличием ФАЙЛА на диске, а не содержимым VRAM. R552 учит выбор под загрузку
	// смотреть в каталог, но применяется он на этапах очереди, а не к уже
	// выбранному узлу.
	//
	// ПОЧЕМУ НЕЛЬЗЯ ПРОСТО ОТДАТЬ 404. Ожидание не поможет только тогда, когда
	// файла нет НИГДЕ в кластере. Если файл есть на другом узле, заявка обязана
	// поехать туда: это ровно тот случай, где пользователь видел «не стартанула»,
	// хотя кластер был готов работать.
	//
	// Условия намеренно узкие: каталог выбранного узла ПРОЧИТАН (known, без
	// dirErr) и файла там нет; модель на этом узле НЕ загружена (иначе каталог
	// устарел, а копия в памяти рабочая); и файл есть хотя бы на одном другом
	// узле. Пока каталог не опрошен — поведение прежнее (никаких догадок).
	if targetBackend != "" && model != "" && !isReadOnlyOrMgmtEndpoint(path) {
		if alt, reason := p.relocateToFileNodeR757(targetBackend, model, allowedTypes); alt != "" && alt != targetBackend {
			logger.Get().Warnw("R757: на выбранном узле НЕТ файла модели — переезжаем туда, где он есть",
				"session", sessionID, "model", model, "from", targetBackend, "to", alt,
				"reason", reason,
				"consequence_if_not", "воркер ответил бы 404 model_not_found без ожидания загрузки (R325.2)")
			targetBackend = alt
			// Привязку сессии перевешиваем: иначе следующий запрос той же сессии
			// снова вернётся на узел без файла (тот же пинг-понг, что в R303/R696).
			if sessionID != "" && p.config.Balancing.SessionStickiness && !isEmbeddingsRequest(path) {
				p.sessionMgr.SetWithProtocol(sessionID, targetBackend, model, clientName,
					p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
			}
		} else if reason == reasonFileAbsentEverywhereR757 {
			logger.Get().Warnw("R757: файла модели нет на выбранном узле и не найдено узла, где он есть",
				"session", sessionID, "model", model, "backend", targetBackend,
				"reason", reason,
				"hint", "проверьте, на какой узел скопирован GGUF, и обновите каталог моделей")
		}
	}

	// Переменная для отслеживания захваченного бэкенда.
	// Используем указатель на строку чтобы корректно обрабатывать
	// освобождение слота в defer без двойного освобождения.
	var acquiredBackend string
	// R73: release-функция lease единой очереди (admission). Если она задана —
	// освобождение слота и учёт per-session inflight делает именно она, чтобы
	// не было двойного releaseSlot.
	var releaseLease func()
	releaseAcquired := func() {
		if releaseLease != nil {
			releaseLease()
			releaseLease = nil
			acquiredBackend = ""
			return
		}
		if acquiredBackend != "" {
			p.releaseSlot(acquiredBackend)
			acquiredBackend = ""
		}
	}
	// Гарантированное освобождение слота при любом выходе из ServeHTTP
	defer releaseAcquired()

	// Round 22 (2026-08-03): read/mgmt endpoints НЕ берут slot. Это короткие
	// операции (HTTP GET, сканирование файлов, ответ из in-memory кэша).
	// Брать slot для них — тратить concurrency budget бэкенда. Если бы
	// взяли slot для /api/show, balancer завис бы на slot-wait когда
	// все слоты заняты inference-запросами (Round 22 BUG #13).
	readOnlyOrMgmt := isReadOnlyOrMgmtEndpoint(path)

	// Атомарный захват слота с retry при неудаче
	if targetBackend != "" && !readOnlyOrMgmt {
		if p.tryAcquireSlot(targetBackend) {
			acquiredBackend = targetBackend
		}
	}

	// Round 22 (2026-08-03): для read/mgmt endpoints — slot не берём,
	// но acquiredBackend всё равно ставим = targetBackend чтобы дальнейший
	// flow (proxyRequest) работал без slot-wait.
	if readOnlyOrMgmt && targetBackend != "" {
		acquiredBackend = targetBackend
	}

	// Если не удалось захватить слот на выбранном бэкенде — пробуем другие
	if targetBackend != "" && acquiredBackend == "" && !readOnlyOrMgmt {
		attemptedBackends := map[string]bool{targetBackend: true}
		const maxRetries = 10
		// R480 (P1-6): уход с занятого слота — тоже переезд живой сессии, и он
		// обязан проходить тот же гейт «нужного окна», что и ребалансировка.
		// Раньше этот путь выбирал копию только по загрузке.
		altGate := p.sessionMoveGateR480(targetBackend, model, sessionID, parsed.NumCtx,
			p.promptTokensForMoveR480(parsed.RawBody, path))
		for retry := 0; retry < maxRetries; retry++ {
			altBackend := p.selectBackendExcludingGatedR480(model, attemptedBackends, bt, altGate)
			if altBackend == "" {
				break
			}
			if p.tryAcquireSlot(altBackend) {
				acquiredBackend = altBackend
				targetBackend = altBackend
				if sessionID != "" && p.config.Balancing.SessionStickiness && !isEmbeddingsRequest(path) {
					p.sessionMgr.SetWithProtocol(sessionID, targetBackend, model, clientName, p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
				}
				logger.Get().Infow("acquired slot on alternate backend",
					"backend", targetBackend, "original", attemptedBackends)
				break
			}
			attemptedBackends[altBackend] = true
		}
	}

	// R301 (2026-10-29): «окно не вырастет» решается ДО admission-очереди.
	//
	// ЖИВОЙ ДЕФЕКТ (стенд 573a03a3, нативная поверхность Ollama :18080).
	// Второй запрос с options.num_ctx=16384 при загруженных 8192 встал в
	// unified queue (wait_max_sec=900) рядом с активным стримом и был отменён
	// клиентом через 120 с: «клиент отменил запрос, пока ждал слот». Ответ
	// «окно сейчас не вырастет» был известен в ту же миллисекунду.
	//
	// Почему guard'ы не спасали: R288/R299 стоят на пути LlamaCppRouter
	// (handleChat/handleGenerate) и на входе preflight'а в транспорте — оба
	// ПОСЛЕ этой очереди. Это ровно тот класс дефекта, который в серии уже
	// повторялся: новый путь не проходит через общий механизм.
	//
	// Проверка ничего не занимает и ничего не перезагружает: только кэш метрик
	// (growthVerdictR301). Тело отказа — стандартное «отложено» (R291/R299), без
	// нового формата: клиент обрабатывает его тем же правилом, что и остальные.
	if !readOnlyOrMgmt && acquiredBackend == "" && model != "" && parsed != nil && len(parsed.RawBody) > 0 {
		// Бэкенд для чисел — тот, где у модели САМОЕ ШИРОКОЕ загруженное окно.
		//
		// Живой факт (стенд 146307dd): при двух копиях (одна из них — мёртвый
		// донор со старыми метриками) отказ показывал loaded=32768, тогда как
		// реально загруженное окно живой копии было 8192/4096. Отказ,
		// посчитанный по меньшей копии, запретил бы запрос, который другая
		// копия обслуживает; поэтому берём максимум из наблюдений, а числа в
		// теле называют этот максимум (в логе — какой именно бэкенд).
		guardBackend := p.backendWithLoadedModelR301(model)
		if guardBackend == "" {
			guardBackend = targetBackend
		}
		if guardBackend != "" {
			v := p.growthVerdictR301(RequestIDFromContext(r.Context()), guardBackend, model, parsed.RawBody, path, parsed.Stream)
			switch {
			case v.Grow:
				// R385: окно можно поднять — запускаем перезагрузку и просим
				// повторить. Клиент получает те же числа, что и при отказе, но
				// с причиной «рост начат», а не «нельзя».
				logGrowthVerdictR301("servehttp-before-admission", guardBackend, model, path, v)
				p.triggerStreamGrowthR385(guardBackend, model, v.Needed, v.Target)
				w.Header().Set("X-NCtx-Reload-Decision", "context-growth-starting")
				// R447: тело пишет ЧИСЛА ВЕРДИКТА (перезагрузка выше уже могла
				// изменить метрики — иначе тело и текст описывали бы разные
				// состояния мира).
				writePreflightDeferredR299Ctx(w, p, withGrowthNumbersR447(r.Context(), v),
					guardBackend, model, parsed.RawBody, path,
					growthStartsMessageR385(model, v), streamGrowthRetryAfterSecR385)
				return
			case v.Defer:
				logGrowthVerdictR301("servehttp-before-admission", guardBackend, model, path, v)
				w.Header().Set("X-NCtx-Reload-Decision", "context-growth")
				writePreflightDeferredR299Ctx(w, p, withGrowthNumbersR447(r.Context(), v),
					guardBackend, model, parsed.RawBody, path,
					growthDeferMessageR301(model, v), growthDeferRetryAfterSecR447)
				return
			}
		}
	}

	// R659 (2026-11-20): заявка не получила слот ни на одной прогретой копии —
	// здесь (после решения R301 о росте окна, до очереди) создаём ёмкость:
	// свободный узел, где модель поднимается с нужным окном, начинает загрузку.
	// Подробности и границы правила — cold_load_placement.go.
	if !readOnlyOrMgmt && acquiredBackend == "" && parsed != nil {
		p.startColdLoadBeforeQueueR659(model, bt, parsed, targetBackend, sessionID, path)
	}

	if acquiredBackend == "" {
		// Round 22 (2026-08-03): read/mgmt endpoints не идут в очередь —
		// они короткие операции, queueing тратит время впустую.
		if readOnlyOrMgmt {
			// Fallback: если не выбрали backend (нет healthy) — return 503
			http.Error(w, "Service unavailable - no healthy backends for read endpoint", http.StatusServiceUnavailable)
			return
		}

		// R73 (2026-09-24): ЕДИНАЯ ОЧЕРЕДЬ. До R73 здесь был legacy QueueManager
		// (пул worker'ов + канал), то есть у Ollama-пути и llama.cpp-пути были
		// РАЗНЫЕ очереди: разные лимиты (queueTimeout против
		// LB_ADMISSION_WAIT_SEC), разная справедливость (у legacy её не было) и
		// разная наблюдаемость. Теперь Ollama-путь ждёт слот в той же
		// admission-очереди и с теми же заголовками X-Queue-*.
		sessionKey := sessionID
		if sessionKey == "" {
			sessionKey = RequestSessionKey(r, nil)
		}
		waited, position := time.Duration(0), 0
		// R329 (2026-10-30): ждущий запрос виден в мониторе и на ЭТОМ пути.
		//
		// Строка «ждёт слот» сначала появилась только в per-backend admission
		// (acquireInferenceAdmission), но Ollama-поверхность ждёт в ЕДИНОЙ очереди
		// (waitForInferenceBackend) — то есть живой запрос Cline/OpenWebUI снова
		// был невидим на мониторе (current_size рос, pending пуст). Регистрируем
		// строку здесь, до ожидания, и снимаем при выходе.
		pendingRow := p.noteAdmissionPendingR329(model, "unified")
		defer p.clearAdmissionPendingR329(pendingRow)
		// R480 (P1-7, P1-8): если сессия привязана к прогретой копии, очередь
		// не выдаёт слот на «холодной»/узкой копии мимо гейта R459 — запрос
		// ждёт слот своей копии (правило R139). Новая сессия (нет привязки)
		// идёт по прежнему пути: ей нужен любой свободный слот. Причины отказа
		// попадают в why_queued вместе с гейтом окна (P1-8).
		sessionFromBackend := ""
		if sessionKey != "" && p.config.Balancing.SessionStickiness && p.sessionMgr != nil {
			// R480: снимок под блокировкой (R66c) — Get() отдаёт общий
			// указатель, и -race ловил гонку с параллельным Set.
			//
			// R493 (2026-11-11): привязка — ПРЕДПОЧТЕНИЕ, а не замок.
			//
			// R493-уточнение (2026-11-12, по архиву стенда): «предпочтение»
			// отдаёт бэкенд только при свободном слоте, но ИСТОЧНИКОМ ОКНА для
			// гейта R459 он остаётся всегда. Иначе при занятой копии гейт не
			// строится вовсе, и очередь берёт свободную копию с МЕНЬШИМ окном:
			// живой итог — уход на A10-126 (131072 < 198656), загрузки на
			// 8-гигабайтные узлы и 503 на стадии select_backend.
			//
			// Решение оператора: занята копия, а бэкенд с нужным окном есть —
			// ставим заявку в очередь К НЕМУ; свободная копия используется, если
			// её окно не меньше нужного (это и проверяет гейт).
			sessionFromBackend = p.stickyBackendForGateR493(sessionKey)
			if preferred := p.stickyBackendIfAvailableR493(sessionKey); preferred == "" &&
				sessionFromBackend != "" && logger.Get() != nil {
				logger.Get().Infow("R493: у копии сессии нет свободного слота — ждём её слот или копию с нужным окном",
					"session", sessionKey, "backend", sessionFromBackend, "model", model)
			}
		}
		unifiedBackend, unifiedRelease, unifiedWaited, unifiedPosition, waitErr :=
			p.waitForInferenceBackendWithMoveR480(ctxWithRID, model, bt, sessionKey, sessionFromBackend,
				parsed.NumCtx, p.promptTokensForMoveR480(parsed.RawBody, path))
		if waitErr == nil {
			acquiredBackend = unifiedBackend
			targetBackend = unifiedBackend
			releaseLease = unifiedRelease
			waited, position = unifiedWaited, unifiedPosition
			// Те же заголовки, что на handler-пути — через общий сеттер
			// (queue_headers.go): клиент видит ожидание одинаково на обеих
			// поверхностях.
			setQueueWaitHeadersR493(w, waited, position)
			if sessionID != "" && p.config.Balancing.SessionStickiness && !isEmbeddingsRequest(path) {
				p.sessionMgr.SetWithProtocol(sessionID, targetBackend, model, clientName, p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
			}
			logger.Get().Infow(i18n.T("balancer.proxy.unified_slot_acquired_info"),
				"backend", acquiredBackend, "model", model,
				"waited_ms", waited.Milliseconds(), "position", position)
		} else {
			switch {
			case errors.Is(waitErr, errAdmissionOverloaded):
				// Backpressure: очередь переполнена — быстрый 503 (прежнее
				// поведение queueRequest при заполнении > highWatermark).
				w.Header().Set("Retry-After", "5")
				http.Error(w, "Service overloaded", http.StatusServiceUnavailable)
			case errors.Is(waitErr, errAdmissionTimeout):
				// R117: отчитываемся РЕАЛЬНЫМ бюджетом ожидания (max(admission,
				// auto-load)), иначе клиент видит 300 c там, где ждал 900.
				//
				// R696-fix: в тело 503 передаём «нужное окно» (R459/R480) — иначе
				// свободная копия с меньшим окном не попадала в backend_rejections
				// и выглядела способной взять заявку; оператор не видел причины
				// ожидания именно в гейте окна.
				gateOpts := admissionUnavailableOpts{}
				if sessionFromBackend != "" {
					gateOpts.Need, gateOpts.NeedSource = p.sessionMoveNeedR480(
						sessionFromBackend, model, sessionKey, parsed.NumCtx,
						p.promptTokensForMoveR480(parsed.RawBody, path))
				}
				writeAdmissionUnavailable(w, p, model, sessionKey, position, waited,
					p.admissionWaitBudget(), gateOpts)
			case errors.Is(waitErr, errAdmissionDisabled):
				http.Error(w, "Service unavailable - all backends busy", http.StatusServiceUnavailable)
			default:
				// Клиент отменил запрос, пока ждал слот: отвечать уже некому.
				//
				// R696: но запись в «Прокси-логах» обязана быть ПРАВДИВОЙ. Раньше
				// statusCode оставался дефолтным 200, и живой архив второго стенда
				// показывал `status=200 bytes=0 first_byte_ms=-1`: оборванное
				// клиентом ожидание в очереди выглядело как пустой УСПЕШНЫЙ ответ
				// (при этом клиент действительно видел пустоту и уходил).
				// 499 «клиент закрыл запрос» ничего не отправляет — писателя уже
				// нет, — но делает отмену видимой в логе и в статистике.
				if !sr.wroteHeader {
					sr.statusCode = statusClientClosedR696
					logger.Get().Infow("R696: заявка отменена клиентом до ответа — статус записан как 499",
						"model", model, "session", sessionKey, "request_id", rid)
				}
				logger.Get().Infow(i18n.T("balancer.proxy.unified_request_canceled_info"),
					"model", model, "session", sessionKey, "error", waitErr)
			}
			// Слот не получен — выполняем запрос нечем: выходим (как и раньше
			// при неудачной постановке в legacy-очередь).
			return
		}
	}

	// Выполняем запрос
	err := p.proxyRequest(w, r, acquiredBackend)
	if err == nil {
		return
	}

	// Проверяем, не является ли ошибка "backend busy" (503) — пробуем другой бэкенд
	var backendBusy *BackendBusyError
	if errors.As(err, &backendBusy) {
		logger.Get().Warnw("backend busy (503), retrying on alternate backend",
			"failed_backend", acquiredBackend, "model", model)
		// Освобождаем слот на текущем бэкенде и пробуем другие
		releaseAcquired()
		// R83: если клиенту уже отдана часть ответа, повтор дописал бы ВТОРОЙ
		// ответ в тот же стрим — именно так выглядит жалоба «OpenWebUI показывает
		// ответ дважды». Повторяем только когда ответ ещё не начат.
		if clientAlreadyCommitted(w) {
			logger.Get().Warnw("alternate-backend retry skipped: response already partially sent",
				"failed_backend", acquiredBackend, "model", model, "error", err)
			return
		}
		attemptedBackends := map[string]bool{targetBackend: true}
		// R480 (P1-6): failover после 503 — тоже переезд; гейт «нужного окна»
		// применяется, чтобы живая сессия не уехала на узкую копию.
		altGate := p.sessionMoveGateR480(acquiredBackend, model, sessionID, parsed.NumCtx,
			p.promptTokensForMoveR480(parsed.RawBody, path))
		for attempt := 1; attempt < 3; attempt++ {
			altBackend := p.selectBackendExcludingGatedR480(model, attemptedBackends, bt, altGate)
			if altBackend == "" {
				break
			}
			if !p.tryAcquireSlot(altBackend) {
				attemptedBackends[altBackend] = true
				continue
			}
			acquiredBackend = altBackend
			if sessionID != "" && p.config.Balancing.SessionStickiness {
				p.sessionMgr.SetWithProtocol(sessionID, altBackend, model, clientName, p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
			}
			attemptedBackends[altBackend] = true

			err := p.proxyRequest(w, r, altBackend)
			if err == nil {
				return
			}
			logger.Get().Errorw("alternate backend also failed",
				"backend", altBackend, "attempt", attempt, "error", err, "model", model)
			releaseAcquired()
		}
		http.Error(w, "Service unavailable - all backends busy", http.StatusServiceUnavailable)
		return
	}

	// Проверяем, не является ли ошибка "model not found" — запускаем auto-pull
	var modelNotFound *ModelNotFoundError
	if errors.As(err, &modelNotFound) && p.AutoPull != nil && model != "" {
		logger.Get().Warnw("model not found on backend, triggering auto-pull",
			"backend", acquiredBackend, "model", model,
			"ollama_error", string(modelNotFound.Body))

		// AutoPull.EnsureModel уже проверяет: есть ли модель на каком-то бэкенде,
		// не загружается ли она где-то ещё (dedup), свободны ли слоты для pull'а
		newBackendID, pullErr := p.AutoPull.EnsureModel(model)
		if pullErr == nil {
			logger.Get().Infow("auto-pull successful, retrying request on new backend",
				"model", model, "new_backend", newBackendID)

			// Обновляем привязку сессии к новому бэкенду
			if sessionID != "" && p.config.Balancing.SessionStickiness {
				p.sessionMgr.SetWithProtocol(sessionID, newBackendID, model, clientName, p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
			}

			// Освобождаем старый слот и захватываем новый
			releaseAcquired()
			// R83: тот же гард, что и в alternate-backend циклах ниже —
			// auto-pull не должен дописывать второй ответ поверх начатого.
			if clientAlreadyCommitted(w) {
				logger.Get().Warnw("auto-pull retry skipped: response already partially sent",
					"backend", acquiredBackend, "model", model, "error", err)
				return
			}
			if p.tryAcquireSlot(newBackendID) {
				acquiredBackend = newBackendID
				err = p.proxyRequest(w, r, newBackendID)
				if err == nil {
					return
				}
				logger.Get().Errorw("auto-pull: request retry failed",
					"model", model, "backend", newBackendID, "error", err)
				// R340: JSON-форма вместо plain-text http.Error (см. autopull_errors.go).
				writeAutoPullErrorR340(w, autoPullErrorFormR340(r.URL.Path), model,
					http.StatusBadGateway, "model pulled but the retried request failed", err)
			} else {
				logger.Get().Warnw("auto-pull: no free slot on new backend",
					"model", model, "backend", newBackendID)
				writeAutoPullErrorR340(w, autoPullErrorFormR340(r.URL.Path), model,
					http.StatusServiceUnavailable,
					fmt.Sprintf("model pulled but no free slot on backend %q", newBackendID), nil)
			}
		} else {
			// Auto-pull не удался — сообщаем клиенту понятную ошибку
			logger.Get().Errorw("auto-pull failed for model",
				"model", model, "error", pullErr)
			writeAutoPullErrorR340(w, autoPullErrorFormR340(r.URL.Path), model,
				http.StatusNotFound, "model not found on any backend and auto-pull failed", pullErr)
		}
		return
	}

	// Обычная обработка ошибки (не model-not-found или auto-pull выключен/недоступен)
	logger.Get().Errorw("backend request failed",
		"backend", acquiredBackend, "error", err, "model", model)
	// R194 (2026-10-20): ОТМЕНА КЛИЕНТА — НЕ ОТКАЗ БЭКЕНДА.
	// Раньше здесь безусловно выполнялось UpdateBackendStatus(Unhealthy), поэтому
	// после обрыва клиента следующий запрос получал 503 (живой замер: 740 мс).
	if isClientDisconnectErr(err) {
		logger.Get().Infow(i18n.T("balancer.proxy.client_canceled_not_unhealthy_info"),
			"backend", acquiredBackend, "model", model, "error", err)
		releaseAcquired()
		return
	}

	// R341 (2026-10-30, найдено аудитом авто-pull F1): «МОДЕЛИ НЕТ» — НЕ «УЗЕЛ
	// СЛОМАН».
	//
	// Живой факт: при выключенном auto-pull (это дефолт, config.bundled.json
	// "enabled": false) ответ воркера «model not found» попадал в общую ветку
	// ошибки — бэкенд помечался unhealthy, recovery-check для него пустой, и
	// клиент получал 503 «all backends failed» вместо честного 404 «модели нет».
	// Один запрос к несуществующей модели выключал здоровый узел из балансировки.
	var missingModel *ModelNotFoundError
	if errors.As(err, &missingModel) {
		logger.Get().Infow("backend is healthy, but the model is missing — not marking unhealthy (R341)",
			"backend", acquiredBackend, "model", model)
		releaseAcquired()
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": fmt.Sprintf("model %q not found on backend %q", model, acquiredBackend),
			"model": model,
			"code":  "model_not_found",
		})
		return
	}
	p.UpdateBackendStatus(acquiredBackend, types.StatusUnhealthy)
	p.scheduleRecoveryCheck(acquiredBackend)

	// Освобождаем проблемный бэкенд и пробуем другие
	releaseAcquired()
	// R83: см. комментарий в ветке backend-busy — повтор после частичной отдачи
	// клиенту даёт дубль ответа, поэтому не повторяем.
	if clientAlreadyCommitted(w) {
		logger.Get().Warnw("backend-failure retry skipped: response already partially sent",
			"failed_backend", acquiredBackend, "model", model, "error", err)
		return
	}
	attemptedBackends := map[string]bool{targetBackend: true}
	// R480 (P1-6): failover после сбоя бэкенда — тоже переезд; узкую копию не
	// выбираем (та же проверка, что у ребалансировки).
	altGate := p.sessionMoveGateR480(acquiredBackend, model, sessionID, parsed.NumCtx,
		p.promptTokensForMoveR480(parsed.RawBody, path))
	for attempt := 1; attempt < 3; attempt++ {
		altBackend := p.selectBackendExcludingGatedR480(model, attemptedBackends, bt, altGate)
		if altBackend == "" {
			break
		}
		if !p.tryAcquireSlot(altBackend) {
			attemptedBackends[altBackend] = true
			continue
		}
		acquiredBackend = altBackend
		if sessionID != "" && p.config.Balancing.SessionStickiness {
			p.sessionMgr.SetWithProtocol(sessionID, altBackend, model, clientName, p.getClientRealIP(r), r.UserAgent(), RequestProtocol(r.URL.Path))
		}
		attemptedBackends[altBackend] = true

		err := p.proxyRequest(w, r, altBackend)
		if err == nil {
			return
		}
		logger.Get().Errorw("backend request failed",
			"backend", altBackend, "attempt", attempt, "error", err, "model", model)
		releaseAcquired()
	}

	http.Error(w, "Service unavailable - all backends failed", http.StatusServiceUnavailable)
}
