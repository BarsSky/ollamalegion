/**
 * OllamaLegion WebUI — Orchestrator
 * Imports: Utils, Api, WebSocketManager, Renderers (loaded before this file)
 */

// R57.2 (2026-09-03): theme/density/i18n/AutoTune/showToast перенесены
// в webui/js/app-core.js (window.App namespace). В этом файле остались:
// state, init() (вызывает App.*), data fetching, navigation, event listeners,
// settings apply, page render dispatch, backend CRUD, model management,
// agents, logs, export — всё что внутри IIFE.

const ui = (function () {
    const { dashboard, backendsPage, modelsPage, sessionsPage, queuePage, logs: renderLogs, predictionAlerts, proxyLogs: renderProxyLogs, copyProxyLogs: renderCopyProxyLogs, agentsPage: renderAgentsPage, renderAgentDetails } = Renderers;

    // R54.8 (2026-08-24): AutoTune WebSocket handler — toast notifications
    // when autonomous reload triggers/succeeds/fails. Слушает ws-message события
    // (WebSocketManager в modules/websocket.js диспатчит их после JSON.parse).
    // Wire format: payload.eventType (string), payload.backendId, payload.model,
    // payload.severity, payload.message, payload.data (object).
    // State
    const data = {
        backends: [],
        sessions: [],
        queue: {},
        queueTasks: [],
        queueHistory: [],
        logs: [],
        models: [],
        proxyLogs: [],
        agents: null
    };
    let currentPage = 'dashboard';
    let dashboardRenderTimer = null;

    // ---- Initialization ----

    function init() {
        // R57.2 (2026-09-03): theme/density/i18n/AutoTune moved to webui/js/app-core.js.
        // app.js must be loaded AFTER app-core.js (see webui/index.html).
        // R57.3: populate App.context for app-listeners.js setup functions.
        // Each function reads from App.context to access app.js state and helpers.
        //
        // R679: флаг «приложение инициализировано». Нужен auth.js → restoreSession,
        // чтобы отличить два случая явного входа:
        //   * сессия истекла в ЖИВОЙ панели — init() уже был, каналы и страницы
        //     подняты, данные можно перечитать на месте (без F5);
        //   * страница ОТКРЫТА ГОСТЕМ — boot-гейт не звал init(), и восстанавливать
        //     нечего: перезагрузка здесь единственный уместный путь (она и есть
        //     штатная загрузка приложения).
        // Флаг ставится ПЕРВОЙ строкой: любая поздняя ошибка не должна оставлять
        // панель в состоянии «инициализирована наполовину, но считается гостем».
        window.__olAppInitialized = true;
        if (!window.App) {
            console.error('app.js: window.App not defined — app-core.js must be loaded BEFORE app.js');
        } else {
            App.context = {
                data: data,
                // R493 (2026-11-17): currentPage — ЖИВОЕ значение, а не снимок.
                //
                // Живой дефект (диагностика WebUI, замер на стенде): здесь лежала
                // копия `currentPage` на момент инициализации, то есть навсегда
                // 'dashboard'. app-listeners.js:171,186 проверяет
                // `ctx.currentPage === 'dashboard'` перед перерисовкой — условие
                // было истинно на КАЖДОЙ странице, и скрытый дашборд
                // перерисовывался каждым тиком (88 мутаций DOM за 20 с при
                // активной странице «Бэкенды»), а активная страница не
                // перерисовывалась вовсе (0 мутаций за 30 с).
                //
                // Геттер сохраняет все существующие чтения и убирает класс
                // «снимок вместо значения»: подставить сюда вторую копию снова
                // нельзя — свойство без сеттера.
                get currentPage() { return currentPage; },
                dashboardRenderTimer: dashboardRenderTimer,
                addLog: addLog,
                addProxyLog: addProxyLog,
                updateConnectionStatus: updateConnectionStatus,
                fetchClusterState: fetchClusterState,
                fetchProxyLogs: fetchProxyLogs,
                refreshPage: refreshPage,
                switchPage: switchPage,
                autoDetectCppWorkerPort: autoDetectCppWorkerPort
            };
            App.initTheme();
            App.initDensity();
            App.setupI18n();
            App.initAutoTuneEventHandlers();
            App.setupNavigation();
            App.setupRestartHandler();
            App.setupWebSocketEvents();
            App.setupApiEvents();
            App.setupLogsTabNavigation();

            // R66b (2026-09-22): setupEventListeners() была определена ниже в этом
            // файле, но НИКОГДА не вызывалась (единственное упоминание — её
            // объявление). Из-за этого были мертвы ВСЕ обработчики, которые она
            // навешивает: #saveSettings, #resetSettings, export/import конфига,
            // GGUF-пресеты, авто-сохранение 55 полей настроек (autoSaveSettings),
            // #saveBackendLimitsBtn, #retakeSetupWizardBtn, detect-port бэкенда,
            // #refreshBtn и кнопки модалок. Внешне это выглядело как «элементы
            // настроек не отрабатывают» — клик ничего не делал и никакой ошибки
            // в консоли не было.
            setupEventListeners();
        }

        // R91: прямая ссылка (?page=queue / #queue) применяется ПОСЛЕ навигации,
        // иначе setupNavigation() вернёт дашборд и перетрёт выбор.
        var deepLink = pageFromURL();
        if (deepLink && deepLink !== 'dashboard') switchPage(deepLink);

        // R513 (2026-11-19): навигация обязана переживать F5 и кнопки «назад/вперёд».
        //
        // ЖИВАЯ ЖАЛОБА. «F5 не возвращает на ту же страницу». Причина: URL не
        // менялся НИКОГДА — в webui/js не было ни одного pushState/replaceState
        // (проверено grep: 0 совпадений), pageFromURL читал ?page=/hash только
        // один раз при загрузке. То есть адрес был замороженной ссылкой: если в
        // нём однажды оказался ?page=settings, F5 открывал «Настройки» с любой
        // страницы. Здесь — вторая половина механизма: слушаем историю и хэш,
        // а switchPage пишет выбранную страницу в адрес (см. syncPageURL).
        //
        // pageFromURL() в обработчике, а не запомненная страница: браузер сам
        // восстанавливает адрес при «назад», и источником истины остаётся URL.
        if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
            window.addEventListener('popstate', function () {
                var p = pageFromURL();
                if (p) switchPage(p);
            });
            window.addEventListener('hashchange', function () {
                var p = pageFromURL();
                if (p) switchPage(p);
            });
        }

        // R84 (2026-10-03): индикатор свежести данных в шапке (#dataFreshness)
        // и переключатель авто-обновления. Провайдеры страниц регистрируем ДО
        // первого рендера, иначе первое нажатие «Обновить» не нашло бы их.
        if (window.DataRefresh) {
            window.DataRefresh.mount();
            registerPageProviders();
            // Пауза авто-обновления останавливает общий цикл, возобновление —
            // запускает его заново и сразу подтягивает данные.
            window.DataRefresh.onAutoChange(function (on) {
                if (on) {
                    startPeriodicRefresh();
                    fetchClusterState();
                    startOpsAutoRefresh();
                } else {
                    stopPeriodicRefresh();
                    stopOpsAutoRefresh();
                }
                // Метрики монитора живут в iframe со своими таймерами: состояние
                // паузы считается по тому же правилу, что и для страницы
                // (R493: пауза, скрытая вкладка и «монитор не открыт»).
                syncMonitorIframePause();
            });
        }

        // R679 (2026-11-21): ПАУЗА МОНИТОРА ПОВТОРЯЕТСЯ ПОСЛЕ ЗАГРУЗКИ IFRAME.
        //
        // ЖИВОЙ ФАКТ (замер на стенде): страница «Бэкенды», авто-обновление
        // включено, а монитор в iframe опрашивает API каждые 2 с — и после
        // истечения сессии продолжает получать 401 (11 запросов за цикл).
        // Причина не в паузе, а в ГОНКЕ: родитель отправляет
        // {type:'ollamalegion-pause', paused:true} один раз при инициализации,
        // когда обработчик postMessage в iframe ещё не зарегистрирован (его
        // регистрирует js/monitor/state.js, загружаемый позже). Сообщение
        // уходит в пустоту, и iframe спокойно живёт своим циклом.
        // Здесь досылаем состояние паузы на load — тогда «монитор не открыт ⇒
        // он молчит» (правило R493) выполняется и на практике.
        var monitorFrameEl = document.getElementById('monitorFrame');
        if (monitorFrameEl && monitorFrameEl.addEventListener) {
            monitorFrameEl.addEventListener('load', function () {
                try { syncMonitorIframePause(); } catch (e) { /* iframe не готов */ }
            });
        }
        setupVisibilityRefresh();

        // Initial data load — cluster state first, drives connection status
        fetchClusterState().then(function () {
            fetchQueue();
            fetchQueueDetails();
            fetchQueueHistory();
            fetchSessions();
        });

        // Load agents on startup
        fetchAgents();

        // Periodic refresh
        startPeriodicRefresh();

        // Initialize Settings UI modules
        if (window.SettingsUI) {
            SettingsUI.setupAccordion();
            SettingsUI.restoreAccordionState();
            SettingsUI.setupModeSelector();
            SettingsUI.setupBackendEngineSwitch();
            SettingsUI.initBackendEngineCards();
            // R55.10 (2026-08-25): Theme picker — 7 swatch buttons в Settings
            // → General. Использует window.ollamalegion_switchTheme
            // (выставлен выше в initTheme).
            if (SettingsUI.setupThemePicker) {
                SettingsUI.setupThemePicker();
            }
        }

        // ---- Notifications (F.α — SSE EventBus) ----
        // EventSource подключается к /api/v1/events (ring buffer + heartbeat + live stream).
        // Токен передаётся в query (?token=...) потому что EventSource API не поддерживает custom headers.
        if (window.notifications && window.notifications.init) {
            window.notifications.init({
                token: (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '',
                onNew: function (ev) { renderNotification(ev); },
                onClear: function () { renderNotificationsList(); }
            });
            setupNotificationsUI();
        }

        // R596 (2026-11-19): мастер первого запуска заменён карточкой.
        // Карточка спрашивает ТРИ порога, которые реально читает slot_manager.go
        // (gpu/cpu/ram), и пишет их вместе с `initialized: true` одним PUT.
        // Источник истины — сервер (config.json), localStorage — запасной путь.
        if (window.FirstRunCard && typeof window.FirstRunCard.check === 'function') {
            window.FirstRunCard.check().catch(function () {
                console.warn('Failed to check initialization status from server');
            });
        }

        // R59.4 (2026-09-03): removed leftover call `setupLogsTabNavigation()` —
        // function moved to app-listeners.js in R57.3 (exposed as
        // App.setupLogsTabNavigation, called at line 68 above). The bare call
        // here crashed init() with `ReferenceError: setupLogsTabNavigation is not
        // defined`, killing the whole init() before fetchClusterState ran.

        // Load proxy logs from REST API on startup
        fetchProxyLogs();

        // F.2 (Session F): live tail системных логов через WebSocket /ws/logs.
        // Заменяет polling на Logs tab: при connect получаем snapshot, далее live stream.
        initLogsStream();

        addLog(window.I18N ? I18N.t('app.webui_initialized') : 'WebUI initialized', 'info');
    }


    function switchPage(page) {
        var prevPage = currentPage;
        document.querySelectorAll('.page').forEach(p => p.classList.remove('active'));
        document.querySelectorAll('.nav-item').forEach(n => n.classList.remove('active'));

        const targetPage = document.getElementById(page + '-page');
        const targetNav = document.querySelector(`[data-page="${page}"]`);

        if (targetPage) targetPage.classList.add('active');
        if (targetNav) targetNav.classList.add('active');

        currentPage = page;
        // R666 (2026-11-21): переход по разделам — это осознанный уход со старой
        // страницы, поэтому защита от перерисовки снимается. Без сброса флаг
        // «несохранённая правка» пережил бы переход, и первый же рендер страницы
        // был бы пропущен (пустой раздел на 2 минуты TTL).
        //
        // R676 (2026-11-21): снимается защита ТОЛЬКО с покидаемой страницы.
        // Здесь стоял `clearAll()`, то есть переход на «Бэкенды» стирал свежую
        // правку на «Настройках» (и на любой другой странице тоже) — защита
        // снималась с ЧУЖИХ страниц, и вернувшись оператор видел серверные
        // значения вместо набранных. Возврат на ту же страницу (deep-link,
        // повторный клик) защиту не снимает вовсе.
        if (window.FormDirty && typeof window.FormDirty.clear === 'function' && prevPage !== page) {
            window.FormDirty.clear(prevPage);
        }
        // R513 (2026-11-19): страница попадает в адрес, иначе F5 возвращает не
        // туда (см. syncPageURL и обработчики popstate/hashchange в init).
        syncPageURL(page);
        // R493 (2026-11-17): App.state.currentPage — ВТОРАЯ копия страницы (её
        // читает app-core.js:165 при смене языка для заголовка). Она никогда не
        // обновлялась: заголовок при смене языка брался от 'dashboard'. Держим
        // обе копии в одном месте, чтобы они не разъезжались.
        if (window.App && window.App.state) window.App.state.currentPage = page;
        var titleKey = 'header.' + page;
        var h1 = document.getElementById('pageTitle');
        if (h1 && window.I18N) h1.textContent = I18N.t(titleKey);
        else Utils.setText('pageTitle', getPageTitle(page));

        refreshPage(page);
        // R493 (2026-11-17): монитор метрик живёт в iframe со своим 2-секундным
        // циклом. Он нужен только когда страница «Монитор» открыта: замер на
        // стенде — 76 % фоновых запросов WebUI (78 из 103 за 15 с) шло из
        // невидимого iframe. Уходя со страницы, останавливаем его цикл; возвращаясь
        // — возобновляем (если авто-обновление не на паузе и вкладка видима).
        syncMonitorIframePause();

        if (page === 'backends' || page === 'models' || page === 'image') {
            fetchClusterState();
        }
    }

    // R91 (2026-10-08): прямая ссылка на страницу — ?page=queue или #queue.
    //
    // Зачем: у WebUI не было никакого способа открыть страницу, кроме клика по
    // меню, поэтому оператор не мог дать коллеге ссылку «вот очередь», а
    // headless-проверка страниц (скриншоты) не могла выбрать нужную вкладку.
    // Значение проверяем по списку известных страниц, чтобы мусор в URL не
    // приводил к пустому экрану.
    //
    // R513 (2026-11-19): список известных страниц исправлен по факту разметки.
    // Было: `'help'` — страницы `#help-page` в index.html НЕТ (help — это раздел
    // внутри «Справки и шаблонов»), а `'agents'` — есть `#agents-page`, но
    // страницы не было в списке, поэтому ссылка ?page=agents молча открывала
    // дашборд. `virtual-models`, `health`, `monitor` — отдельные html/iframe, их
    // SPA-роутер не открывает; первые две в списке отсутствуют осознанно (клик
    // по ним отдаётся браузеру, см. app-listeners.js → setupNavigation).
    function pageFromURL() {
        var known = ['dashboard', 'monitor', 'backends', 'models', 'sessions', 'queue', 'gguf', 'image', 'virtual-models', 'agents', 'logs', 'diagnostics', 'reference', 'settings'];
        var candidate = '';
        try {
            // ?page= приоритетнее хэша: так адрес, записанный switchPage
            // (syncPageURL), не перебивается старым #hash, оставшимся в ссылке.
            var qs = new URLSearchParams(window.location.search || '');
            candidate = qs.get('page') || '';
            if (!candidate && window.location.hash) candidate = window.location.hash.replace(/^#\/?/, '');
        } catch (e) { /* старый браузер — просто останемся на дашборде */ }
        candidate = String(candidate || '').toLowerCase();
        return known.indexOf(candidate) >= 0 ? candidate : '';
    }

    /**
     * syncPageURL — записать открытую страницу в адрес (R513).
     *
     * replaceState, а не pushState: лишние записи истории сделали бы «назад»
     * бесполезным (каждый клик по меню — шаг назад). При этом адрес всё равно
     * обновляется, поэтому F5 открывает ту же страницу.
     *
     * Ошибки глушим осознанно: на `file://` и в старых браузерах History API
     * может быть недоступен, а навигация по SPA обязана остаться рабочей.
     */
    function syncPageURL(page) {
        if (!page) return;
        try {
            if (!window.history || typeof window.history.replaceState !== 'function') return;
            var href = window.location.href;
            var url = new URL(href);
            if (url.searchParams.get('page') === page) return; // адрес уже правильный
            url.searchParams.set('page', page);
            url.hash = ''; // #hash — устаревший способ, ?page= теперь источник истины
            window.history.replaceState(null, '', url.pathname + url.search);
        } catch (e) { /* навигация важнее адреса */ }
    }

    function getPageTitle(page) {
        // Fallback only — i18n handles actual titles via header.* keys
        const titles = {
            dashboard: 'Dashboard',
            monitor: 'Cluster Monitor',
            backends: 'Backend Management',
            models: 'Models',
            sessions: 'Sessions',
            queue: 'Queue',
            gguf: 'GGUF Models',
            image: 'Image models',
            logs: 'System Logs',
            diagnostics: 'Diagnostics',
            // R493 (2026-11-17): страница «Справка и шаблоны». Заголовок в шапке
            // берётся из i18n по ключу header.reference; это только fallback,
            // если пакет переводов не поднялся.
            reference: 'Reference & Templates',
            settings: 'Settings',
            agents: 'Agents',
            // R675 (2026-11-21): страница «Виртуальные модели» (data-page=
            // "virtual-models"). В этой карте её не было, поэтому при
            // неработающем пакете переводов заголовок молча схлопывался в
            // 'Dashboard' — то есть подменял раздел чужим именем. Ключ
            // header.virtual-models для i18n добавлен в ru.js/en.js.
            'virtual-models': 'Virtual models'
        };
        return titles[page] || 'Dashboard';
    }

    // Статусы, которые считаем нерабочими и скрываем в UI по умолчанию.
    var UNHEALTHY_STATUSES_UI = { unhealthy: true, offline: true, draining: true, ollama_unavailable: true };

    /**
     * Фильтрует бэкенды по текущему типу (ollama / llama_cpp) и по статусу.
     * Нерабочие бэкенды (unhealthy/offline/draining/ollama_unavailable) скрываются,
     * чтобы в WebUI не отображались заглушки/недоступные ноды.
     * Использует BackendTypeFilter как клиентский fallback.
     * Если сервер уже отфильтровал — фильтр пройдёт без изменений.
     */

    // ---- Event Listeners ----

    function setupEventListeners() {
        // R66b (2026-09-22): хелперы модалки Add/Edit Backend объявлены НЕ здесь,
        // а в app-modals.js и опубликованы как window.BackendCRUD.*. В этом файле
        // bare-имён openBackendModal/closeModal/saveBackend/deleteBackend нет
        // (проверено grep: 0 объявлений), поэтому первый же вызов давал
        // ReferenceError: closeModal is not defined — а поскольку
        // setupEventListeners() вызывается из init(), падала вся инициализация
        // (обработчики ниже 331 не навешивались: saveSettings, reset, export/
        // import, GGUF-пресеты, автосейв, лимиты бэкендов, retake-визард).
        // Локальные обёртки с проверкой на наличие — чтобы отсутствие
        // app-modals.js не ломало остальную инициализацию.
        var CRUD = window.BackendCRUD || {};
        var openBackendModal = function () { if (CRUD.openBackendModal) CRUD.openBackendModal(); };
        var closeModal = function () { if (CRUD.closeModal) CRUD.closeModal(); };
        var saveBackend = function () { if (CRUD.saveBackend) CRUD.saveBackend(); };
        var deleteBackend = function () { if (CRUD.deleteBackend) CRUD.deleteBackend(); };

        document.getElementById('refreshBtn').addEventListener('click', () => {
            // R84 (2026-10-03): кнопка в шапке — ЕДИНСТВЕННАЯ кнопка обновления
            // приложения. Она обязана дёргать и общие данные (cluster/очередь/
            // сессии), и провайдеров активной страницы (см. refreshCurrentPage),
            // а результат показывать индикатором свежести, а не тостом «успешно».
            var markFresh = function () {
                if (window.DataRefresh && window.DataRefresh.markFresh) window.DataRefresh.markFresh();
            };
            if (window.DataRefresh && typeof window.DataRefresh.refresh === 'function') {
                // Общие данные (cluster → бэкенды/узлы/ресурсы) + провайдеры
                // активной страницы; ошибку покажет индикатор свежести.
                fetchClusterState().catch(function () { return null; })
                    .then(function () { return window.DataRefresh.refresh(currentPage); })
                    .then(function (ok) {
                        // R513: «Настройки» перерисовывает свой провайдер,
                        // повторный refreshPage не нужен (см. refreshCurrentPage).
                        if (currentPage !== 'settings') refreshPage(currentPage);
                        if (!ok) {
                            showToast(window.I18N ? I18N.t('refresh.failed', 'Не удалось обновить данные') : 'Refresh failed', 'error');
                        }
                    });
                return;
            }
            // R83 (2026-09-29): сначала ЗАПРОС к балансеру, потом рендер.
            //
            // Было: только refreshCurrentPage(), а эта функция рисует страницу из
            // ЛОКАЛЬНОЙ копии data.* — то есть кнопка «Обновить» перерисовывала то,
            // что уже было в памяти, и по свежим данным ничего не запрашивала.
            // Пока живы периодический опрос (REFRESH_INTERVAL) и WebSocket, разница
            // незаметна, но при обрыве WS кнопка выглядела неработающей: тост
            // «успешно» появляется, а цифры не меняются.
            //
            // Ровно так же работает соседняя кнопка refreshModelsBtn ниже
            // (fetchClusterState().then(modelsPage)).
            var done = function () {
                refreshCurrentPage();
                markFresh();
                showToast(window.I18N ? I18N.t('common.success') : 'Data updated', 'success');
            };
            if (typeof fetchClusterState === 'function') {
                fetchClusterState().then(done).catch(done);
            } else {
                done();
            }
        });

        document.getElementById('addBackendBtn').addEventListener('click', () => openBackendModal());
        if (document.getElementById('addBackendBtn2')) {
            document.getElementById('addBackendBtn2').addEventListener('click', () => openBackendModal());
        }

        document.getElementById('modalClose').addEventListener('click', closeModal);
        document.getElementById('modalCancel').addEventListener('click', closeModal);
        document.getElementById('modalSave').addEventListener('click', saveBackend);
        document.getElementById('modalDelete').addEventListener('click', deleteBackend);

        var backendSearch = document.getElementById('backendSearch');
        if (backendSearch) backendSearch.addEventListener('input', Utils.debounce((e) => filterBackends(e.target.value), 150));
        var sessionSearch = document.getElementById('sessionSearch');
        if (sessionSearch) sessionSearch.addEventListener('input', Utils.debounce((e) => filterSessions(e.target.value), 150));

        document.getElementById('clearLogs').addEventListener('click', () => {
            data.logs = [];
            renderLogs(data.logs);
        });
        // R481: поиск и уровень для «Системных логов» реально фильтруют список.
        // Раньше селект уровня применялся только при экспорте, а поиска не было
        // вовсе — «поля не корректно заполняются, не информативно».
        var logsSearchEl = document.getElementById('logsSearch');
        if (logsSearchEl) {
            logsSearchEl.addEventListener('input', Utils.debounce(function (e) {
                window.logFilters = window.logFilters || {};
                window.logFilters.search = e.target.value;
                renderLogs(data.logs);
            }, 150));
        }
        var logsLevelEl = document.getElementById('logsLevelFilter');
        if (logsLevelEl) {
            logsLevelEl.addEventListener('change', function () {
                window.logFilters = window.logFilters || {};
                window.logFilters.level = logsLevelEl.value;
                renderLogs(data.logs);
            });
        }
        // R481: из «Логов» видно, как открыть «Диагностику» и скачать архив.
        var logsOpenDiagBtn = document.getElementById('logsOpenDiagnostics');
        if (logsOpenDiagBtn) {
            logsOpenDiagBtn.addEventListener('click', function () {
                var nav = document.querySelector('.nav-item[data-page="diagnostics"]');
                if (nav) nav.click();
            });
        }
        var logsDownloadBundleBtn = document.getElementById('logsDownloadBundle');
        if (logsDownloadBundleBtn) {
            logsDownloadBundleBtn.addEventListener('click', function () {
                if (window.Diagnostics && typeof window.Diagnostics.downloadBundle === 'function') {
                    window.Diagnostics.downloadBundle(logsDownloadBundleBtn);
                }
            });
        }
        var exportProxyLogsBtn = document.getElementById('exportProxyLogs');
        if (exportProxyLogsBtn) {
            exportProxyLogsBtn.addEventListener('click', function () { exportProxyLogs(); });
        }
        var exportLogsBtn = document.getElementById('exportLogs');
        if (exportLogsBtn) exportLogsBtn.addEventListener('click', exportLogs);        // R389 (2026-11-02): выгрузка логов БАЛАНСЕРА (серверный буфер), а не
        // только того, что браузер успел получить по WebSocket. Фильтры берём
        // из панели: уровень и формат — те же селекты, что у кнопки «Экспорт».
        var exportServerLogsBtn = document.getElementById('exportServerLogs');
        if (exportServerLogsBtn) exportServerLogsBtn.addEventListener('click', async function () {
            var levelEl = document.getElementById('logsLevelFilter');
            var formatEl = document.getElementById('logsExportFormat');
            try {
                var res = await window.Api.exportServerLogs({
                    level: levelEl ? levelEl.value : 'all',
                    format: formatEl ? formatEl.value : 'txt'
                });
                if (typeof addLogEntry === 'function') {
                    addLogEntry({
                        time: new Date(),
                        level: 'info',
                        message: 'Логи балансера выгружены: ' + res.filename + ' (записей: ' + res.entries + ')',
                        source: 'webui'
                    });
                }
            } catch (err) {
                if (typeof addLogEntry === 'function') {
                    addLogEntry({
                        time: new Date(),
                        level: 'error',
                        message: 'Экспорт логов балансера не удался: ' + (err && err.message ? err.message : err),
                        source: 'webui'
                    });
                }
            }
        });
        var exportBackendsBtn = document.getElementById('exportBackends');
        if (exportBackendsBtn) exportBackendsBtn.addEventListener('click', exportBackends);

        document.getElementById('saveSettings').addEventListener('click', function () { saveSettings(false); });
        document.getElementById('resetSettings').addEventListener('click', resetSettings);

        // Session 17 (2026-07-27): runtime overrides "Reset to bundled defaults" кнопка.
        var llamaCppResetBtn = document.getElementById('llamaCppResetBtn');
        if (llamaCppResetBtn) {
            llamaCppResetBtn.addEventListener('click', function () { resetLlamaCppOverride(); });
        }

        // Session 18 (2026-07-28): Quick Presets bar (Speed / Memory / Context / CPU-only)
        // Клик по пресету заполняет форму рекомендованными значениями и подсвечивает активную кнопку.
        var PRESETS = {
            speed:   { ctxSize: 4096,  gpuLayers: -2, kvCacheType: 'f16',  flashAttn: true,  nThreads: 0,  useMmap: true,  batchSize: 512, idleUnloadMinutes: 0 },
            memory:  { ctxSize: 2048,  gpuLayers: -2, kvCacheType: 'q4_0', flashAttn: true,  nThreads: 0,  useMmap: true,  batchSize: 256, idleUnloadMinutes: 5 },
            context: { ctxSize: 32768, gpuLayers: -2, kvCacheType: 'q8_0', flashAttn: true,  nThreads: 0,  useMmap: true,  batchSize: 512, idleUnloadMinutes: 0, ropeScalingType: 'yarn', ropeScalingFactor: 4 },
            cpu:     { ctxSize: 2048,  gpuLayers: 0,  kvCacheType: 'q8_0', flashAttn: true,  nThreads: 8,  useMmap: true,  batchSize: 256, idleUnloadMinutes: 0 }
        };
        function applyGgufPreset(name) {
            var preset = PRESETS[name];
            if (!preset) return;
            // Заполняем поля формы (id-ы совпадают с index.html)
            var map = {
                ctxSize: 'ctxSize', gpuLayers: 'gpuLayers', kvCacheType: 'kvCacheType',
                flashAttn: 'flashAttn', nThreads: 'nThreads', useMmap: 'useMmap',
                batchSize: 'batchSize', idleUnloadMinutes: 'idleUnloadMinutes',
                ropeScalingType: 'ropeScalingType', ropeScalingFactor: 'ropeScalingFactor'
            };
            Object.keys(map).forEach(function (k) {
                if (preset[k] === undefined) return;
                var el = document.getElementById(map[k]);
                if (!el) return;
                // R676 (2026-11-21): запись в поле — через SafeForm.
                //
                // Пресет — ЯВНОЕ действие оператора (он нажал кнопку), поэтому
                // запись принудительная: иначе защита отклоняла бы её, и кнопка
                // молча «не срабатывала бы». После записи страница помечается
                // «есть несохранённая правка», иначе тик авто-обновления вернул бы
                // серверные значения раньше, чем оператор нажмёт «Сохранить», —
                // пресет выглядел бы применённым и тут же пропадал.
                if (el.type === 'checkbox') {
                    SafeForm.setChecked(el, !!preset[k], { force: true });
                } else {
                    SafeForm.setValue(el, preset[k], { force: true });
                }
                if (window.FormDirty && typeof window.FormDirty.noteEdit === 'function') {
                    window.FormDirty.noteEdit(el);
                }
            });
            // Visual feedback: подсветить активную кнопку
            document.querySelectorAll('#ggufPresetSpeed, #ggufPresetMemory, #ggufPresetContext, #ggufPresetCpu')
                .forEach(function (btn) { btn.classList.toggle('active', btn.dataset.preset === name); });
            // Тост/уведомление (используем глобальный showToast если есть)
            var label = (window.I18N && I18N.t('gguf.preset_applied', 'Preset applied:') + ' ' + name) || ('Preset: ' + name);
            if (typeof window.showToast === 'function') window.showToast(label, 'success');
        }
        ['ggufPresetSpeed', 'ggufPresetMemory', 'ggufPresetContext', 'ggufPresetCpu'].forEach(function (id) {
            var btn = document.getElementById(id);
            if (btn) btn.addEventListener('click', function () { applyGgufPreset(btn.dataset.preset); });
        });

        // Export / Import Config buttons
        var exportBtn = document.getElementById('exportSettingsBtn');
        if (exportBtn) exportBtn.addEventListener('click', function () {
            if (window.ConfigIO) ConfigIO.exportConfig();
        });

        var importBtn = document.getElementById('importSettingsBtn');
        if (importBtn) importBtn.addEventListener('click', function () {
            if (window.ConfigIO) {
                ConfigIO.importConfigFromFile().then(function (data) {
                    return ConfigIO.showImportPreview(data);
                }).then(function (confirmed) {
                    if (confirmed) ConfigIO.applyConfig(data);
                }).catch(function (err) {
                    showToast(err.message || 'Import failed', 'error');
                });
            }
        });

        // Auto-save on settings form changes
        var settingsFields = ['useEnhancedScoring', 'modelAffinity', 'sessionStickiness', 'predictionFiltering', 'gpuMaxUsage', 'vramMaxUsage', 'cpuMaxUsage', 'ramMaxUsage', 'minFreeDisk', 'modelReplicationMinInstances', 'modelReplicationMaxInstances', 'modelReplicationIdleUnload', 'rpcCoordinatorURL', 'rpcCoordinatorWorkerPort', 'rpcCoordinatorProtocol', 'virtualModelsCoordMode', 'distInferenceGrpcPort', 'gpuLayers', 'ctxSize', 'batchSize', 'flashAttnType', 'nThreads', 'gpuStrategy', 'tensorSplit', 'splitMode', 'mainGpu', 'rpcBackend', 'noMemoryMap', 'kvCacheType', 'noKvOffload', 'ropeFreqBase', 'ropeFreqScale', 'ropeScalingType', 'ropeScalingFactor', 'yarnExtFactor', 'yarnAttnFactor', 'yarnBetaFast', 'yarnBetaSlow', 'rmsNormEps', 'idleUnloadMinutes', 'enableMetrics', 'metricsRetention', 'autoGpuDistribution', 'flashAttn', 'numa', 'useMmap', 'useMlock', 'enableReasoning', 'reasoningBudget'];
        settingsFields.forEach(function (id) {
            var el = document.getElementById(id);
            if (el) {
                el.addEventListener('change', autoSaveSettings);
                if (el.tagName === 'INPUT' && el.type === 'number') {
                    el.addEventListener('input', autoSaveSettings);
                }
            }
        });

        document.getElementById('backendModal').addEventListener('click', (e) => {
            if (e.target.id === 'backendModal') closeModal();
        });

        // Backend type selector in add-backend form — sync with BackendTypeFilter
        var formBackendType = document.getElementById('formBackendType');
        if (formBackendType) {
            formBackendType.addEventListener('change', function () {
                var newType = this.value;
                if (window.BackendTypeFilter) {
                    // R-Image Phase 5: image_cpp — тип КОНКРЕТНОГО бэкенда, а не
                    // «движок кластера». setCurrentType('image_cpp') не сохраняется
                    // на сервер (guard в _saveToServer), но менял бы глобальный
                    // фильтр списка бэкендов; при выборе image_cpp нужно лишь
                    // показать соответствующий блок полей формы.
                    if (newType === 'image_cpp') {
                        BackendTypeFilter.toggleBackendFormFields(newType);
                    } else {
                        BackendTypeFilter.setCurrentType(newType);
                    }
                }
            });
        }

        // Type filter buttons (Все / Ollama / llama.cpp) на Dashboard и Backends page
        document.querySelectorAll('.type-filter-group').forEach(function(group) {
            group.querySelectorAll('.type-filter-btn').forEach(function(btn) {
                btn.addEventListener('click', function() {
                    // Снять active со всех кнопок в этой группе
                    group.querySelectorAll('.type-filter-btn').forEach(function(b) { b.classList.remove('active'); });
                    this.classList.add('active');
                    // Применить фильтр через BackendTypeFilter
                    var filterType = this.dataset.type;
                    if (window.BackendTypeFilter) {
                        BackendTypeFilter.setCurrentType(filterType);
                    }
                    // Обновить текущую страницу
                    refreshCurrentPage();
                });
            });
        });

        // Agents page buttons
        // R84: кнопка «Обновить» удалена — агенты теперь и в периодическом
        // опросе (startPeriodicRefresh), и в провайдерах страницы.
        var closeAgentDetailsBtn = document.getElementById('closeAgentDetails');
        if (closeAgentDetailsBtn) {
            closeAgentDetailsBtn.addEventListener('click', function() {
                var card = document.getElementById('agentDetailsCard');
                if (card) card.style.display = 'none';
            });
        }

        // Models Manage button on Models page
        var modelsManageBtn = document.getElementById('modelsManageBtn');
        if (modelsManageBtn) {
            modelsManageBtn.addEventListener('click', function() {
                if (data.backends && data.backends.length > 0) {
                    openModelManageModal(data.backends[0].id);
                } else {
                    showToast(window.I18N ? I18N.t('models.no_backends') : 'No backends available', 'error');
                }
            });
        }

        // Models search/filter
        var modelsSearch = document.getElementById('modelsSearch');
        if (modelsSearch) {
            // Session B: восстанавливаем последний query из localStorage.
            try {
                var saved = localStorage.getItem('ollamalegion_models_search');
                // R676: запись в поле — через SafeForm (force: это инициализация
                // страницы, а не обновление; база запоминается, чтобы следующая
                // программная запись не считала строку «правкой оператора»).
                if (saved) SafeForm.setValue(modelsSearch, saved, { force: true });
            } catch (e) { /* ignore */ }
            modelsSearch.addEventListener('input', Utils.debounce(function(e) {
                filterModels(e.target.value);
            }, 150));
        }

        // Models backend-type filter (🦙/🦒/All)
        var modelsTypeFilter = document.getElementById('modelsTypeFilter');
        if (modelsTypeFilter) {
            var filterButtons = modelsTypeFilter.querySelectorAll('.filter-btn');
            filterButtons.forEach(function(btn) {
                btn.addEventListener('click', function() {
                    filterButtons.forEach(function(b) { b.classList.remove('active'); });
                    btn.classList.add('active');
                    var currentSearch = modelsSearch ? modelsSearch.value : '';
                    filterModels(currentSearch);
                });
            });
        }

        // Models sort dropdown
        var modelsSortBy = document.getElementById('modelsSortBy');
        if (modelsSortBy) {
            modelsSortBy.addEventListener('change', function() {
                applyModelsSort();
            });
        }

        // R84 (2026-10-03): кнопки «Обновить» на страницах «Модели» и «Агенты»
        // удалены — их источники теперь провайдеры (см. registerPageProviders),
        // которые дёргает общая кнопка/индикатор в шапке.

        // Model Management Modal buttons
        var modelManageCloseBtn = document.getElementById('modelManageClose');
        if (modelManageCloseBtn) {
            modelManageCloseBtn.addEventListener('click', closeModelManageModal);
        }
        var modelManageCancelBtn = document.getElementById('modelManageCancel');
        if (modelManageCancelBtn) {
            modelManageCancelBtn.addEventListener('click', closeModelManageModal);
        }
        var modelManageModal = document.getElementById('modelManageModal');
        if (modelManageModal) {
            modelManageModal.addEventListener('click', function(e) {
                if (e.target.id === 'modelManageModal') closeModelManageModal();
            });
        }
        var modelPullBtn = document.getElementById('modelPullBtn');
        if (modelPullBtn) {
            modelPullBtn.addEventListener('click', function() {
                var backendId = modelManageModal ? modelManageModal.dataset.backendId : null;
                if (!backendId) {
                    showToast('Backend ID not found', 'error');
                    return;
                }
                var modelName = document.getElementById('modelPullName') ? document.getElementById('modelPullName').value.trim() : '';
                if (!modelName) {
                    showToast(window.I18N ? I18N.t('models.model_name_placeholder') : 'Enter model name', 'error');
                    return;
                }
                var insecure = document.getElementById('modelPullInsecure') ? document.getElementById('modelPullInsecure').checked : false;
                var options = {};
                if (insecure) options.insecure = true;
                executeModelOperation(backendId, 'pull', modelName, options);
            });
        }

        // Proxy logs buttons
        var clearProxyLogsBtn = document.getElementById('clearProxyLogs');
        if (clearProxyLogsBtn) {
            clearProxyLogsBtn.addEventListener('click', function() {
                data.proxyLogs = [];
                renderProxyLogs(data.proxyLogs);
            });
        }
        // R84: кнопка «Обновить» на странице логов удалена — fetchProxyLogs
        // зарегистрирован провайдером страницы 'logs'.
        var copyProxyLogsBtn = document.getElementById('copyProxyLogs');
        if (copyProxyLogsBtn) {
            copyProxyLogsBtn.addEventListener('click', function() {
                renderCopyProxyLogs();
            });
        }

        // Backend Limits button
        var saveBackendLimitsBtn = document.getElementById('saveBackendLimitsBtn');
        if (saveBackendLimitsBtn) {
            saveBackendLimitsBtn.addEventListener('click', function() {
                saveBackendLimits();
            });
        }

        // CppWorker port Auto-detect button (форма регистрации бэкенда)
        var cppPortDetectBtn = document.getElementById('formBackendCppWorkerPortDetect');
        if (cppPortDetectBtn) {
            cppPortDetectBtn.addEventListener('click', function () {
                var hostEl = document.getElementById('formBackendHost');
                var portEl = document.getElementById('formBackendCppWorkerPort');
                var host = hostEl ? hostEl.value.trim() : '';
                if (!host) {
                    showToast('Сначала укажите host', 'error');
                    return;
                }
                cppPortDetectBtn.disabled = true;
                var originalText = cppPortDetectBtn.innerHTML;
                cppPortDetectBtn.innerHTML = '⏳ Detecting…';
                autoDetectCppWorkerPort(host).then(function (detected) {
                    cppPortDetectBtn.disabled = false;
                    cppPortDetectBtn.innerHTML = originalText;
                    if (detected) {
                        // R676: ответ на явный клик по «Определить» — запись
                        // принудительная (иначе защита поля отклонила бы её и
                        // кнопка выглядела бы сломанной), база запоминается.
                        if (portEl) SafeForm.setValue(portEl, detected, { force: true });
                        showToast('Обнаружен порт CppWorker: ' + detected, 'success');
                    } else {
                        showToast('CppWorker не найден на портах 18092/18091/18093/18090. Укажите вручную.', 'error');
                    }
                }).catch(function () {
                    cppPortDetectBtn.disabled = false;
                    cppPortDetectBtn.innerHTML = originalText;
                    showToast('Ошибка auto-detect', 'error');
                });
            });
        }

        // Retake Setup Wizard button (Danger Zone)
        var retakeBtn = document.getElementById('retakeSetupWizardBtn');
        if (retakeBtn) {
            retakeBtn.addEventListener('click', function() {
                var confirmed = confirm(
                    (window.I18N ? I18N.t('wizard.retake_confirm') : 'Все текущие настройки будут сброшены. Продолжить?')
                );
                if (!confirmed) return;

                // Сбрасываем initialized на сервере
                if (window.Api && window.Api.updateConfig) {
                    window.Api.updateConfig({ initialized: false }).then(function () {
                        localStorage.removeItem('ollamalegion_wizard_done');
                        localStorage.removeItem('ollamalegion_backend_type');
                        if (window.FirstRunCard) {
                            window.FirstRunCard.check();
                        }
                    }).catch(function (err) {
                        showToast(
                            (window.I18N ? I18N.t('common.error') : 'Ошибка') + ': ' + (err.message || err),
                            'error'
                        );
                    });
                } else {
                    localStorage.removeItem('ollamalegion_wizard_done');
                    if (window.FirstRunCard) {
                        window.FirstRunCard.check();
                    }
                }
            });
        }
    }




    // R652 (2026-11-20): настройки и кэш конфигурации бэкендов вынесены в
    // app-settings.js (фабрика). currentPage передаётся геттером — это let.
    // R655: настройки и данные зависят друг от друга (refreshPage/showToast ↔ loadSettings),
    // поэтому четыре зависимости, объявленные НИЖЕ, передаются ленивыми обёртками.
    var settingsUi = window.AppSettings({
        data: data,
        getCurrentPage: function () { return currentPage; },
        refreshPage: function () { return refreshPage.apply(null, arguments); },
        showToast: function () { return showToast.apply(null, arguments); },
        updateConnectionStatus: function () { return updateConnectionStatus.apply(null, arguments); },
        updateBackendEngineBadge: function () { return updateBackendEngineBadge.apply(null, arguments); }
    });
    var loadSettings = settingsUi.loadSettings;
    var saveSettings = settingsUi.saveSettings;
    var resetSettings = settingsUi.resetSettings;
    var resetLlamaCppOverride = settingsUi.resetLlamaCppOverride;
    var autoSaveSettings = settingsUi.autoSaveSettings;
    // R671 (2026-11-21): те же пять имён, что фабрика теперь возвращает. Без этих
    // алиасов `App.context` (строка 66) падал на `ReferenceError`, потому что
    // bare-имена fetchClusterState/fetchQueue/... в этом файле нигде не объявлены.
    var fetchClusterState = settingsUi.fetchClusterState;
    var fetchQueue = settingsUi.fetchQueue;
    var fetchQueueDetails = settingsUi.fetchQueueDetails;
    var fetchQueueHistory = settingsUi.fetchQueueHistory;
    var fetchSessions = settingsUi.fetchSessions;

    // ---- Periodic Refresh ----

    // R653 (2026-11-20): журналы, прокси-логи, выгрузки и уведомления вынесены
    // в app-logs.js (фабрика). currentPage передаётся геттером — это let.
    var logUi = window.AppLogs({
        data: data,
        getCurrentPage: function () { return currentPage; },
        init: init,
        renderAgentDetails: renderAgentDetails,
        renderAgentsPage: renderAgentsPage,
        renderLogs: renderLogs,
        renderProxyLogs: renderProxyLogs,
                showToast: function () { return showToast.apply(null, arguments); }
    });
    var addLog = logUi.addLog;
    var addLogEntry = logUi.addLogEntry;
    var addProxyLog = logUi.addProxyLog;
    var initLogsStream = logUi.initLogsStream;
    var exportLogs = logUi.exportLogs;
    var exportProxyLogs = logUi.exportProxyLogs;
    var exportBackends = logUi.exportBackends;
    var renderNotification = logUi.renderNotification;
    var renderNotificationsList = logUi.renderNotificationsList;
    var setupNotificationsUI = logUi.setupNotificationsUI;
    var fetchAgents = logUi.fetchAgents;
    var showAgentDetails = logUi.showAgentDetails;

    // R655 (2026-11-20): обновление страниц/данных, опрос, фильтры и тосты
    // вынесены в app-data.js (фабрика). currentPage передаётся геттером — это let.
    var dataUi = window.AppData({
        Api: Api,
        I18N: I18N,
        UNHEALTHY_STATUSES_UI: UNHEALTHY_STATUSES_UI,
        Utils: Utils,
        addLog: addLog,
        addLogEntry: addLogEntry,
        addProxyLog: addProxyLog,
        backendsPage: backendsPage,
        // R676 (2026-11-21): КОНТРАКТ СТРАНИЦЫ. Здесь стоял ключ `currentPage` со
        // значением-функцией, а app-data.js привязывал его к локальной
        // `var currentPage` — и внутри фабрики это была ФУНКЦИЯ. Сравнение
        // `page === currentPage` было ложным всегда, `document.getElementById(
        // currentPage + '-page')` возвращал null, тик не перерисовывал ни одной
        // страницы, а защиты R662/R666/R670 были мёртвым кодом. Имя ключа теперь
        // совпадает с тем, как его читают (вызовом) — как в app-logs.js/app-settings.js.
        getCurrentPage: function () { return currentPage; },
        dashboard: dashboard,
        data: data,
        exportBackends: exportBackends,
        exportLogs: exportLogs,
        exportProxyLogs: exportProxyLogs,
        init: init,
        initLogsStream: initLogsStream,
        loadSettings: loadSettings,
        // R671 (2026-11-21): зависимости, которые app-data.js зовёт bare-именами и
        // которые НЕ передавались в фабрику (часть потеряна при выносе R652-R655,
        // часть объявлена ниже — поэтому ленивые обёртки, как и у остальных
        // циклических зависимостей этого литерала).
        fetchClusterState: fetchClusterState,
        fetchQueue: fetchQueue,
        fetchQueueDetails: fetchQueueDetails,
        fetchQueueHistory: fetchQueueHistory,
        fetchSessions: fetchSessions,
        fetchAgents: fetchAgents,
        fetchProxyLogs: function () { return fetchProxyLogs.apply(null, arguments); },
        loadBackendLimits: function () { return loadBackendLimits.apply(null, arguments); },
        startOpsAutoRefresh: function () { return startOpsAutoRefresh.apply(null, arguments); },
        stopOpsAutoRefresh: function () { return stopOpsAutoRefresh.apply(null, arguments); },
        modelsPage: modelsPage,
        predictionAlerts: predictionAlerts,
        queuePage: queuePage,
        renderAgentDetails: renderAgentDetails,
        renderAgentsPage: renderAgentsPage,
        renderLogs: renderLogs,
        renderNotification: renderNotification,
        renderNotificationsList: renderNotificationsList,
        renderProxyLogs: renderProxyLogs,
        sessionsPage: sessionsPage,
        setupNotificationsUI: setupNotificationsUI
    });
    var filterBackendsForUI = dataUi.filterBackendsForUI;
    var refreshPage = dataUi.refreshPage;
    var sendMonitorConfig = dataUi.sendMonitorConfig;
    var monitorIframeMustPause = dataUi.monitorIframeMustPause;
    var syncMonitorIframePause = dataUi.syncMonitorIframePause;
    var refreshCurrentPage = dataUi.refreshCurrentPage;
    var startPeriodicRefresh = dataUi.startPeriodicRefresh;
    var postMonitorPause = dataUi.postMonitorPause;
    var stopPeriodicRefresh = dataUi.stopPeriodicRefresh;
    var periodicRefreshPaused = dataUi.periodicRefreshPaused;
    var setupVisibilityRefresh = dataUi.setupVisibilityRefresh;
    var registerPageProviders = dataUi.registerPageProviders;
    var filterBackends = dataUi.filterBackends;
    var filterSessions = dataUi.filterSessions;
    var filterModels = dataUi.filterModels;
    var applyModelsSort = dataUi.applyModelsSort;
    var updateBackendEngineBadge = dataUi.updateBackendEngineBadge;
    var updateConnectionStatus = dataUi.updateConnectionStatus;
    var showToast = dataUi.showToast;


    // R654 (2026-11-20): модели/агенты/лимиты и модалка деталей модели вынесены
    // в app-models.js (фабрика). currentPage передаётся геттером — это let.
    var modelUi = window.AppModels({
        Api: Api,
        I18N: I18N,
        Utils: Utils,
        addLog: addLog,
        backendsPage: backendsPage,
        // R676 (2026-11-21): тот же починенный контракт, что у AppData выше —
        // геттер обязан называться `getCurrentPage` и вызываться. Иначе
        // `currentPage === 'backends'` / `=== 'logs'` в app-models.js ложны всегда.
        getCurrentPage: function () { return currentPage; },
        data: data,
        filterBackendsForUI: filterBackendsForUI,
        renderLogs: renderLogs,
        renderProxyLogs: renderProxyLogs,
        showToast: showToast,
        // R673 (2026-11-21): saveBackendLimits (app-models.js:160) после сохранения
        // зовёт fetchClusterState() — в фабрике этого имени нет, падение после
        // тоста «saved»: оператор видит «сохранено», данные не обновляются.
        // Ленивая обёртка, как у соседних циклических зависимостей.
        fetchClusterState: function () { return fetchClusterState.apply(null, arguments); }
    });
    var restartAgent = modelUi.restartAgent;
    var viewAgentLogs = modelUi.viewAgentLogs;
    var fetchProxyLogs = modelUi.fetchProxyLogs;
    var toggleBackendEnabled = modelUi.toggleBackendEnabled;
    var loadBackendLimits = modelUi.loadBackendLimits;
    var saveBackendLimits = modelUi.saveBackendLimits;
    var openModelManageModal = modelUi.openModelManageModal;
    var loadBackendModels = modelUi.loadBackendModels;
    var executeModelOperation = modelUi.executeModelOperation;
    var refreshModelOpsStatus = modelUi.refreshModelOpsStatus;
    var closeModelManageModal = modelUi.closeModelManageModal;
    var startOpsAutoRefresh = modelUi.startOpsAutoRefresh;
    var stopOpsAutoRefresh = modelUi.stopOpsAutoRefresh;
    var cancelOperationByKey = modelUi.cancelOperationByKey;
    var donorMarkForBackend = modelUi.donorMarkForBackend;
    var donorLoadBlocked = modelUi.donorLoadBlocked;
    var donorBlockedText = modelUi.donorBlockedText;


    // ---- CppWorker port auto-detection ----
    // Probes common cppworker ports on the given host and returns the first
    // port that responds OK to /info (or /health). 18092 first (актуальный
    // default для современных llama.cpp-инсталляций), затем 18091 (stub),
    // 18093 (доп. профиль) и 18090 (legacy/agent).
    async function autoDetectCppWorkerPort(host) {
        if (!host) return null;
        const candidates = [18092, 18091, 18093, 18090];
        // Параллельные probe-запросы (AbortController + 1.5s timeout на каждый)
        const probes = candidates.map(function (port) {
            return new Promise(function (resolve) {
                try {
                    const ctrl = new AbortController();
                    const timeoutId = setTimeout(function () { ctrl.abort(); }, 1500);
                    fetch('http://' + host + ':' + port + '/info', {
                        signal: ctrl.signal,
                        mode: 'cors',
                        cache: 'no-store',
                        headers: { 'Accept': 'application/json' }
                    }).then(function (resp) {
                        clearTimeout(timeoutId);
                        if (resp && resp.ok) resolve(port);
                        else resolve(null);
                    }).catch(function () {
                        clearTimeout(timeoutId);
                        resolve(null);
                    });
                } catch (e) {
                    resolve(null);
                }
            });
        });
        const results = await Promise.all(probes);
        // Возвращаем первый непустой результат
        for (var i = 0; i < results.length; i++) {
            if (results[i]) return results[i];
        }
        return null;
    }


    // ---- R679: возврат в админский режим после явного входа, БЕЗ F5 ----
    //
    // ЗАЧЕМ. При истечении сессии открытая панель переводится в гостя на месте
    // (auth.js → expireSession) и глушит всё, что шумит: авто-обновление,
    // WebSocket, SSE-уведомления, live-tail логов. После входа эти каналы нужно
    // поднять и ПЕРЕЧИТАТЬ данные — иначе оператор получает работающий, но
    // пустой интерфейс и всё равно жмёт F5. Эта функция и есть альтернатива
    // перезагрузке: состояние страницы, выбранная вкладка и всё, что оператор
    // успел открыть, остаётся на месте.
    //
    // ГДЕ ПЕРЕЗАГРУЗКА ВСЁ-ТАКИ УМЕСТНА. Если страница была ОТКРЫТА ГОСТЕМ,
    // boot-гейт ниже не вызывал ui.init(), и поднимать нечего — там auth.js
    // делает location.reload(), то есть перезагрузка остаётся штатным путём
    // загрузки приложения (и это единственное место, где она уместна).
    // ---- R679: остановка всего, что само стучится в API ----------------
    //
    // ЗАЧЕМ. При истечении сессии каждый самостоятельный цикл (авто-обновление,
    // статус операций, монитор в iframe со своими 2 с) продолжает ходить в API и
    // получать 401. Именно это видел оператор как «поток ошибок». Останавливаем
    // их ОДНИМ вызовом из session-expiry.js; поднимает их resumeAfterAuth.
    function pauseForSessionExpiry() {
        try { stopPeriodicRefresh(); } catch (e) { /* таймера ещё нет */ }
        try { stopOpsAutoRefresh(); } catch (e) { /* модалка закрыта */ }
        // Монитор — ОТДЕЛЬНЫЙ документ со своими 2-секундными таймерами: из
        // родителя их не остановить, поэтому договариваемся сообщением.
        // Сообщение может потеряться: обработчик postMessage в iframe
        // регистрируется его собственными <script>, то есть ПОЗЖЕ, чем родитель
        // может отправить первый pause. Повторяем отправку и ловим load — иначе
        // «остановили» остаётся словами, а монитор продолжает получать 401.
        var sendPause = function () {
            try { postMonitorPause(true); } catch (e) { /* iframe ещё не загружен */ }
        };
        sendPause();
        setTimeout(sendPause, 500);
        setTimeout(sendPause, 1500);
        try {
            var frame = document.getElementById('monitorFrame');
            if (frame && frame.addEventListener) { frame.addEventListener('load', sendPause, { once: true }); }
        } catch (e) { /* нет iframe — не критично */ }
        return true;
    }
    window.App = window.App || {};

    function resumeAfterAuth() {
        try {            if (window.WebSocketManager) {
                if (typeof window.WebSocketManager.resetAttempts === 'function') {
                    window.WebSocketManager.resetAttempts();
                }
                window.WebSocketManager.connect();
            }
        } catch (e) { console.warn('[app] WS после входа не поднялся:', e && e.message); }

        try {
            if (window.notifications && typeof window.notifications.resume === 'function') {
                window.notifications.resume();
            }
        } catch (e) { console.warn('[app] уведомления после входа не поднялись:', e && e.message); }

        try {
            if (window.logsStream && typeof window.logsStream.resume === 'function') {
                window.logsStream.resume();
            }
        } catch (e) { console.warn('[app] live-tail логов после входа не поднялся:', e && e.message); }

        try {
            if (window.DataRefresh && typeof window.DataRefresh.setAuthExpired === 'function') {
                window.DataRefresh.setAuthExpired(false);
            }
            startPeriodicRefresh();
            if (!window.DataRefresh || window.DataRefresh.isAuto()) {
                startOpsAutoRefresh();
            }
            // R679: монитор в iframe тоже надо разбудить (на странице «Монитор»
            // он был остановлен вместе с остальными каналами).
            syncMonitorIframePause();
            // Перечитываем данные активной страницы: панель не должна остаться
            // пустой до следующего тика авто-обновления.
            fetchClusterState().then(function () {
                refreshPage(currentPage);
            }).catch(function () { /* индикатор свежести покажет состояние */ });
        } catch (e) {
            console.warn('[app] данные после входа не перечитаны:', e && e.message);
        }
        return true;
    }

    // ---- Public API ----

    // R66b (2026-09-22): публикуем в window функции, которые другие модули
    // ищут через `typeof X === 'function'`. Проверки резолвятся в ГЛОБАЛЬНУЮ
    // область, а функции объявлены внутри этого IIFE, поэтому условия были вечно
    // ложными и действия молча не выполнялись:
    //   * config-io.js:400 — после импорта конфига НИКОГДА не вызывался
    //     autoSaveSettings, т.е. импортированные настройки не сохранялись на
    //     сервер (пользователь видел «импортировано», а PUT не уходил);
    //   * mode-wizard.js:373 — после применения режима не перечитывались настройки.
    //
    // ВАЖНО: присваивание обязано быть ВНУТРИ IIFE. Снаружи этих идентификаторов
    // не существует (они объявлены здесь), и попытка обратиться к ним на верхнем
    // уровне скрипта даёт ReferenceError, который обрывает весь файл — включая
    // регистрацию DOMContentLoaded → ui.init() (проверено на стенде: приложение
    // не инициализировалось вообще).
    window.autoSaveSettings = autoSaveSettings;
    window.loadSettings = loadSettings;
    // R679: возврат в админский режим после явного входа без перезагрузки.
    // Присваивание ВНУТРИ IIFE — иначе ReferenceError (см. комментарий выше).
    window.App = window.App || {};
    window.App.resumeAfterAuth = resumeAfterAuth;
    window.App.pauseForSessionExpiry = pauseForSessionExpiry;

    return {
        init,
        resumeAfterAuth,
        pauseForSessionExpiry,
        editBackend: window.BackendCRUD.editBackend,

        confirmDeleteBackend: window.BackendCRUD.confirmDeleteBackend,

        openModelManageModal,
        executeModelOperation,
        loadBackendModels,
        refreshModelOpsStatus,
        closeModelManageModal,
        showAgentDetails,
        fetchAgents,
        restartAgent,
        // R250: «не использовать бэкенд» из списка бэкендов (кнопка в строке)
        toggleBackendEnabled,
        viewAgentLogs,
        // Models tab UI helpers (Roadmap Q3 W3-4: Filter/Search)
        filterModels,
        applyModelsSort,
        // Models tab Pull progress (Roadmap Q3 W3-4 sub-task)
        startOpsAutoRefresh,
        stopOpsAutoRefresh,
        cancelOperationByKey
    };

})();


// R66b (2026-09-22): публикуем ui в window.
//
// `ui` — это const внутри classic-script'а, поэтому в window он НЕ попадал.
// Между тем на него ссылаются:
//   * inline-обработчики в генерируемой разметке — onclick="ui.executeModelOperation(...)"
//     (страница Models, кнопки Load/Unload/Delete, app.js:2225-2226) и
//     onclick="ui.cancelOperationByKey(...)" (app.js:2428). Без window.ui клик
//     по этим кнопкам падал с ReferenceError — «выгрузка модели не работает»;
//   * modules/backend-type-filter.js:344,369 — if (window.ui && window.ui.currentPage…)
//     ветка никогда не срабатывала, т.е. авто-переход на dashboard при скрытии
//     текущей страницы не работал;
//   * modules/config-io.js:32 — if (window.ui && window.ui.data…) не срабатывал,
//     из-за чего в экспортируемый конфиг не попадал список бэкендов.
window.ui = ui;
window.App = window.App || {};
window.App.ui = ui;

// Auto-init when DOM ready
// R206 (2026-10-21): СНАЧАЛА ВХОД, ПОТОМ ДАННЫЕ.
//
// Раньше ui.init() стартовал сразу и дёргал /api/v1/* до аутентификации: на экране
// входа это давало 401 в консоли и «пустые» панели. Теперь ждём /auth/me; если вход
// не настроен на сервере — стартуем сразу (режим break-glass токена).
document.addEventListener('DOMContentLoaded', () => {
    const ready = (typeof window.__authReady === 'function') ? window.__authReady() : Promise.resolve();
    Promise.resolve(ready).then(() => {
        // R222: пользователи есть, входа нет — панель за экраном входа. Данные не
        // грузим: иначе десятки запросов получают 401 и в консоли висит «HTTP 401:
        // Unauthorized» (живая проверка R222 это ловила). После входа страница
        // перезагружается и init проходит уже с сессией.
        const st = (window.Auth && typeof window.Auth.state === 'function') ? window.Auth.state() : null;
        if (st && st.usersConfigured && !st.authenticated && !st.tokenAuth) { return; }
        ui.init();
    });
});
