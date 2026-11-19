// proxy_new.go — R629 (2026-11-19): сборка `Proxy` (конструктор и всё, что он
// подключает: транспорты, менеджеры, очереди, вотчдоги).
//
// ЗАЧЕМ ВЫНЕСЕНО. `NewProxy` — 340 строк связывания, и это единственная часть
// `proxy.go`, которая ничего не решает про запросы: она только собирает объект.
// Пока конструктор лежал рядом с `ServeHTTP`, «что создаётся при старте» и «что
// происходит на запросе» читались как один текст.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть, порядок инициализации тот же
// (он важен: часть полей читается горутинами сразу после запуска).
package balancer

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"time"
)

func NewProxy(config *types.LoadBalancerConfig) *Proxy {
	// Транспорт для обычных запросов
	regularTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// Транспорт для streaming запросов - без сжатия и с увеличенными буферами.
	// ResponseHeaderTimeout=0 (disabled) — таймаут первого байта теперь управляется
	// per-request контекстом (context.WithTimeout) на основе 3-tier адаптивных
	// таймаутов (Profile > ModelLatencyTracker > config). Это критично для
	// тяжёлых моделей (CPU/partial offload), где первый токен может задерживаться
	// на 60+ секунд (загрузка в VRAM + prompt processing).
	// Транспортный ResponseHeaderTimeout не может быть per-model динамическим,
	// поэтому выносим FirstByteTimeout на уровень контекста в proxyRequest /
	// proxyRequestLlamaCpp. Старый ResponseHeaderTimeout обрывал соединение ДО
	// того, как адаптивный контекст вступал в силу, вызывая
	// TransferEncodingError в OpenWebUI.
	//
	// Контекстный таймаут (FirstByteTimeout + StreamTimeout) покрывает весь
	// lifecycle запроса: ожидание заголовков + стриминг + idle между чанками.
	streamingTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // Важно для SSE
		ResponseHeaderTimeout: 0,    // Отключено — управляется per-request контекстом
	}

	// Определяем TTL сессий из конфигурации
	sessionTTL := time.Duration(config.Balancing.SessionTTL) * time.Second
	if sessionTTL <= 0 {
		sessionTTL = 15 * time.Minute
	}
	sessionIdleTTL := time.Duration(config.Balancing.SessionIdleTTL) * time.Second
	if sessionIdleTTL <= 0 {
		sessionIdleTTL = 5 * time.Minute
	}
	// R251 (2026-10-28): порог зомби-сессий из AdvancedTiming (default 120 с).
	// До этого SessionManager держал ХАРДКОД 120 с и не знал о конфиге, а
	// slot_manager.go вообще считал зомби по 60 с — пауза префилла длиннее минуты
	// на живом запросе освобождала слот дважды.
	zombieThreshold := time.Duration(config.Balancing.AdvancedTiming.ZombieSessionThresholdSec) * time.Second
	if zombieThreshold <= 0 {
		zombieThreshold = defaultZombieSessionThreshold
	}

	// R546 (T-7 аудита согласованности): heartbeat живого стрима обязан быть
	// ЧАЩЕ половины порога «зомби».
	//
	// Что уже было в порядке: R252 оживляет сессию во время стрима —
	// startStreamSession() зовёт HeartbeatStream() каждые getHeartbeatInterval()
	// (дефолт 15 с) и снимает флаг на Stop(). Поэтому утверждение аудита
	// «LastRequestAt не обновляется в стриме» на сегодня неверно.
	//
	// Что оставалось ДЕФЕКТОМ: обе величины задаются оператором независимо
	// (AdvancedTiming.HeartbeatIntervalSec и ZombieSessionThresholdSec), и при
	// heartbeat ≥ порога живой стрим снова попадает под cleanup() — ровно та
	// жалоба, с которой начали («сессия исчезает посреди генерации»). Проверить
	// это на стенде дорого (нужен стрим длиннее порога), поэтому правило
	// применяется здесь: heartbeat = min(заданный, порог/2), а расхождение
	// печатается Warn-ом с обоими числами — молча менять политику нельзя.
	if hbSec := config.Balancing.AdvancedTiming.HeartbeatIntervalSec; hbSec > 0 {
		hb := time.Duration(hbSec) * time.Second
		if hb*2 > zombieThreshold {
			applied := zombieThreshold / 2
			if applied < time.Second {
				applied = time.Second
			}
			logger.Get().Warnw("R546: heartbeat стрима реже половины порога зомби — живой стрим считался бы мёртвым",
				"heartbeat_sec", hb.Seconds(),
				"zombie_threshold_sec", zombieThreshold.Seconds(),
				"applied_heartbeat_sec", applied.Seconds(),
				"config", "balancing.advancedTiming (HeartbeatIntervalSec / ZombieSessionThresholdSec)")
			config.Balancing.AdvancedTiming.HeartbeatIntervalSec = int(applied.Seconds())
		}
	}

	p := &Proxy{
		config:        config,
		backends:      make(map[string]*BackendState),
		tokensByModel: make(map[string]*ModelTokenUsage),
		sessionMgr:    NewSessionManagerWithTTL(sessionTTL, sessionIdleTTL),
		metricsMgr:    NewMetricsManager(),
		predictor:     NewPredictor(),
		statePath:     config.LoadBalancer.StatePath,
		// R302 (2026-10-29): калибровка оценки промпта живёт рядом со state.json
		// и переживает перезапуск. Пустой StatePath = только в памяти.
		promptCalib:   newPromptCalibration(promptCalibrationPath(config.LoadBalancer.StatePath)),
		eventBus:      NewEventBus(),
		modelCatalog:  newModelCatalog(),
		recentClients: make(map[string]*types.RecentClient),
		trustedNets:   parseTrustedProxyCIDRs(config.LoadBalancer.TrustedProxies),
		warmupSem:     make(chan struct{}, maxConcurrentWarmups(config)),
		// R60.33 (2026-09-10): default async auto-load. Sync mode (legacy)
		// доступен через env LB_AUTO_LOAD_ASYNC=0. Async mode решает
		// проблему OpenWebUI timeout 60-120s на cold start (sync load = 3 мин
		// → connection aborted → JSON parse error).
		//
		// R66b (2026-09-22): читаем env через IsAutoLoadAsyncEnabled().
		// Раньше здесь стояло ЖЁСТКОЕ true, а функция-парсер env вызывалась
		// только из собственного теста — то есть задокументированный escape
		// hatch LB_AUTO_LOAD_ASYNC=0 не работал вообще: оператор выставлял
		// переменную, а балансер всё равно отвечал 503+Retry-After.
		lbAutoLoadAsync: IsAutoLoadAsyncEnabled(),

		// R67a (2026-09-23): сколько ждать завершения авто-загрузки перед 503.
		// В async-режиме раньше клиент получал ошибку сразу («загрузится через
		// 30-180с»), и проходил только повторный запрос. Теперь ждём
		// LB_AUTO_LOAD_WAIT_SEC (default 180s) — первый же запрос обслуживается.
		lbAutoLoadWait: AutoLoadWaitTimeout(),

		// R67b (2026-09-23): admission-очередь вместо «резкого 503» на занятых
		// слотах. LB_ADMISSION_WAIT_SEC=0 возвращает прежнее поведение.
		admission: newAdmissionQueue(AdmissionWaitTimeout()),

		// R60.47 (2026-09-11): default async n_ctx reload. Sync mode (legacy)
		// доступен через env LB_NCTX_RELOAD_ASYNC=0. Async mode решает
		// проблему "empty response" в OpenWebUI когда cppworker возвращает
		// 400 prompt_too_long → balancer sync reload (waitTimeoutSec=300) →
		// HTTP handler blocked → client 30-60s timeout → empty body.
		//
		// R66b: та же ошибка, что и выше — env не читался (жёсткое true).
		lbNCtxReloadAsync: IsNCtxReloadAsyncEnabled(),

		// Клиент для обычных запросов: Timeout=0 (без глобального), т.к. таймаут
		// задаётся per-request через context.WithTimeout в proxyRequest
		// на основе getEffectiveTimeout().
		client: &http.Client{
			Timeout:   0, // Таймаут управляется через контекст запроса
			Transport: regularTransport,
		},
		// Streaming клиент без общего таймаута для long-running запросов
		streamingClient: &http.Client{
			Timeout:   0, // Нет таймаута для streaming
			Transport: streamingTransport,
		},
		// Базовый Transport для per-request клонов с ResponseHeaderTimeout
		// (используется в newStreamingClientWithResponseHeaderTimeout).
		// Применяется для streaming + явный FirstByteTimeout>0 (PF-5 fix).
		streamingTransportBase: streamingTransport,
	}

	// QueueManager создаём после инициализации p, чтобы передать корректный proxy
	p.queueMgr = NewQueueManager(p, config.Balancing.QueueMaxSize)

	// R251 (2026-10-28): порог зомби-сессий — из конфига и в ОДНОМ месте (раньше
	// SessionManager держал хардкод 120 с, а preemption слота — 60 с, и оба
	// расходились с AdvancedTiming).
	if p.sessionMgr != nil {
		p.sessionMgr.SetZombieThreshold(zombieThreshold)
	}

	// Прокси-логгер использует тот же EventBus для WebSocket-трансляции
	p.proxyLogger = NewProxyLogger(1000, p.eventBus)

	// R269: слежение за стабильной версией llama.cpp (GitHub releases по TTL).
	// Стартуем здесь, а не в main: тогда и тесты NewProxy получают кэш, а сам
	// опрос идёт фоновой петлёй и не участвует в hot path запросов.
	p.startLlamaUpstreamTracking()

	// R229 (2026-10-22): очередь знает протокол сессии (openai/ollama) — бейджи
	// в waiting_detail. Резолвер — менеджер сессий этого же прокси.
	p.admission.protocolOf = p.protocolOfSession
	p.admissionProto = newAdmissionProtocols()

	// Инициализация бэкендов из config
	for i := range config.Backends {
		backend := config.Backends[i]
		p.backends[backend.ID] = &BackendState{
			Backend:         &backend,
			ActiveReqs:      0,
			LastUsed:        time.Time{},
			WarmingUpModels: make(map[string]*types.WarmupState),
		}
	}

	// Загрузка сохранённого state (поверх config — runtime-данные приоритетнее)
	if err := p.LoadState(); err != nil {
		logger.Get().Infow("state file not loaded, using config only", "error", err)
	}

	// Запуск фоновой проверки таймаута агентов (60 секунд — увеличено для стабильности)
	//
	// R66c (2026-09-22): вызываем СИНХРОННО, без `go`. StartAgentTimeoutChecker
	// сам поднимает тикер-горутину и сразу возвращает управление, так что
	// обёртка `go` ничего не давала — зато создавала две проблемы:
	//   1) `p.agentChecker = checker` писалось из горутины без синхронизации,
	//      и Shutdown/StopAgentTimeoutChecker читали это поле параллельно
	//      (DATA RACE, найден детектором гонок в TestNewAutoPullManager:
	//      Write at StartAgentTimeoutChecker vs Read at Shutdown);
	//   2) при быстром NewProxy → Shutdown горутина ещё не успевала записать
	//      указатель, Shutdown его не видел и НЕ закрывал stop-канал —
	//      тикер-горутина утекала и продолжала править state.Backend.HasAgent
	//      после остановки прокси.
	p.StartAgentTimeoutChecker(60 * time.Second)

	// Инициализация OllamaRouter для агрегации и целевой маршрутизации API
	p.ollamaRouter = NewOllamaRouter(p)

	// Инициализация LlamaCppRouter для llama.cpp бэкендов
	p.llamaCppRouter = NewLlamaCppRouter(p)

	// R-Image (2026-09-27): маршрутизатор image-бэкендов (sd.cpp / sd-server).
	p.imageRouter = NewImageRouter(p)

	// R-Image Phase 6 (2026-10-02): VRAM-гейт + лок GPU. Поллер метрик
	// image-воркеров (GET /api/image/models) стартует сразу: без него гейт
	// принимал бы решения по пустому кэшу. Останавливается сам при Shutdown
	// прокси (видит p.shuttingDown), поэтому Shutdown не меняется.
	p.imageRes = newImageResources(p)
	p.imageRes.Start()

	// Round 40 #3 (2026-08-18): инициализация reverse proxy для /api/v1/*.
	// Endpoint'ы под /api/v1/ обслуживаются API-сервером (cmd/balancer/api,
	// порт 18081), а не proxy flow. Сюда входят: /api/v1/cluster/*,
	// /api/v1/cppworker/*, /api/v1/events (SSE), /api/v1/rpc/tp/* и др.
	// Без этого клиент, отправивший POST /api/v1/cppworker/config на proxy
	// 18080, получает 30s timeout (Round 22 BUG #7) вместо ответа.
	apiHost := config.LoadBalancer.Host
	if apiHost == "" || apiHost == "0.0.0.0" {
		apiHost = "127.0.0.1"
	}
	apiPort := config.LoadBalancer.APIPort
	if apiPort == 0 {
		apiPort = 18081
	}
	apiURL := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s:%d", apiHost, apiPort),
	}
	p.apiReverseProxy = httputil.NewSingleHostReverseProxy(apiURL)
	// Director (default): keep original Host header (don't override to 127.0.0.1).
	// API server uses Host header for SNI/reverse-proxy detection in WebUI.
	// Round 40 #3 (2026-08-18): flush interval 100ms — same as default,
	// but explicit для forward-compat с streaming endpoints.
	p.apiReverseProxy.FlushInterval = 100 * time.Millisecond

	// Запуск фонового poll llama.cpp бэкендов для синхронизации LoadedModels
	// с фактическим состоянием cppworker. Нужен для корректного отображения
	// загруженных моделей в WebUI (страница GGUF) и API /api/v1/gguf/backends.
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)
	p.llamaCppMetricsPoller.Start()

	// R118.15 (2026-10-14): фоновые замеры канала до узлов раскладки (можно
	// выключить LB_RPC_LINK_MEASURE_SEC=0).
	p.linkMeasureStop = make(chan struct{})
	p.StartRPCLinkMeasurer(p.linkMeasureStop)

	// Инициализация AutoPullManager (Pull-on-Demand)
	if config.Balancing.AutoPull.Enabled {
		p.AutoPull = NewAutoPullManager(p, config.Balancing.AutoPull)
		logger.Get().Infow("auto-pull manager initialized",
			"enabled", config.Balancing.AutoPull.Enabled,
			"max_concurrent", config.Balancing.AutoPull.MaxConcurrent,
			"timeout", config.Balancing.AutoPull.PullTimeout)
	}

	// Инициализация background controllers.
	//
	// R502 / аудит X-1: здесь только СОЗДАНИЕ планировщика выгрузки; запуск —
	// фаза запуска (`main` зовёт GetUnloadScheduler().Start()). Раньше объект
	// стартовал прямо в конструкторе, а `main` создавал второй и подменял
	// указатель: первый цикл продолжал жить со своим, никогда не обновляемым
	// `lastUsed` (RecordModelUse идёт через указатель прокси).
	//
	// R499 / аудит S-1: тюнер весов удалён (его веса не читались скорингом,
	// а RecordOutcome недостижим на llama.cpp-пути).
	p.unloadScheduler = NewUnloadScheduler(p)

	// Инициализация nctxReload координатора (Stage 3, 5).
	// Загружает конфиг из BalancingSettings.NCtxReload (см. internal/config).
	// Если конфиг не задан — использует безопасные defaults (AutoReloadNCtx=false).
	p.nctxReload = NewNCtxReloadCoordinator(loadNCtxReloadConfig(p.config))
	// R70 (2026-09-24): координатор получает от Proxy хинты для reload'а —
	// рабочий kvCacheType (профиль/загруженная модель), чтобы адаптивная
	// стратегия cppworker считала по реальному типу, а не по f16.
	p.nctxReload.SetReloadHintsProvider(p.reloadHintsFor)
	// R493 (2026-11-17): перезагрузка не начинается, пока на бэкенде идёт стрим.
	// Решатели проверяют занятость ДО решения, но между решением и unload+load
	// клиент успевает начать новый ход (живой факт второго стенда: reload в
	// 06:24:53 и 06:25:43 при идущих ходах Cline).
	p.nctxReload.SetBeforeReloadGuard(func(backendID, model string) (bool, string) {
		if active := p.ActiveStreamsOnBackend(backendID); active > 0 {
			return false, fmt.Sprintf("на бэкенде идёт %d стрим(ов)", active)
		}
		return true, ""
	})
	logger.Get().Infow("nctx reload coordinator initialized",
		"auto_reload", p.nctxReload.Config().AutoReloadNCtx)

	// Инициализация RPC Model Distribution модулей
	p.initRpcModules()

	// R92 (S2 плана 2026-10-07): логические бэкенды из balancing.backendGroups[].
	// Пустой список (default) → реестр пуст, поведение полностью прежнее.
	p.initBackendGroupsR92(config.Balancing)

	// Инициализация ModelManager для управления моделями на бэкендах
	p.modelManager = NewModelManager(p)
	logger.Get().Infow("model manager initialized")

	// Инициализация ModelLatencyTracker для per-model адаптивных таймаутов.
	// Собирает метрики генерации (tokens/sec, inter-token gap, first-byte latency)
	// и автоматически вычисляет рекомендуемые таймауты для каждой модели.
	p.modelLatencyTracker = NewModelLatencyTracker()
	logger.Get().Infow("model latency tracker initialized")

	// R72 (P0): placement policy — предупреждения валидации в лог при старте.
	// Ошибки не блокируют запуск: политика выключена по умолчанию, а решения
	// видны в GET /api/v1/placement. План:
	// docs/plans/2026-09-23-multi-backend-placement-policy.md
	if cfg := config.Balancing.Placement; cfg.Enabled || len(cfg.Models) > 0 || len(cfg.Classes) > 0 {
		warnings := cfg.Validate()
		logger.Get().Infow("placement policy initialized",
			"enabled", cfg.Enabled, "models", len(cfg.Models), "classes", len(cfg.Classes),
			"allow_request_override", cfg.AllowRequestOverride, "warnings", len(warnings))
		for _, w := range warnings {
			logger.Get().Warnw("placement policy config needs attention", "warning", w)
		}
		// R74 (P1): strategy=replicated исполняется группами репликации —
		// создаём их по политике (идемпотентно) сразу при старте.
		for _, action := range p.syncPlacementReplicationGroups() {
			logger.Get().Infow("placement: replication sync", "action", action)
		}
	}

	return p

}
