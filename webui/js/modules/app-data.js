// app-data.js — R655 (2026-11-20): обновление страниц и данных, периодический
// опрос, фильтры, статус соединения и тосты вынесены из `app.js` (цель < 1500).
//
// ЗАЧЕМ ВЫНЕСЕНО. Одна тема — «как данные попадают на экран»: refreshPage для
// каждой страницы, опрос по таймеру с паузой по видимости (setupVisibilityRefresh),
// провайдеры страниц, фильтры списков (бэкенды/сессии/модели), сортировка моделей,
// бейдж движка, статус соединения и showToast. Оркестратор (init, роутинг,
// слушатели) — другое.
//
// `refreshTimer` ЖИВЁТ ЗДЕСЬ: таймер принадлежал только этому слою (проверено:
// вне его обращений нет) и переехал вместе с логикой опроса.
//
// ПОЧЕМУ ФАБРИКА. Слой берёт из `app.js` состояние (`data`), рендереры, слой
// журналов/настроек и ДОСТУП К ТЕКУЩЕЙ СТРАНИЦЕ геттером (currentPage — это let).
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: тела перенесены как есть, отступ сохранён.
(function () {
    'use strict';

    window.AppData = function (ctx) {
        var Api = ctx.Api;
        var I18N = ctx.I18N;
        var UNHEALTHY_STATUSES_UI = ctx.UNHEALTHY_STATUSES_UI;
        var Utils = ctx.Utils;
        var addLog = ctx.addLog;
        var addLogEntry = ctx.addLogEntry;
        var addProxyLog = ctx.addProxyLog;
        var backendsPage = ctx.backendsPage;
        var currentPage = ctx.currentPage;
        var dashboard = ctx.dashboard;
        var data = ctx.data;
        var exportBackends = ctx.exportBackends;
        var exportLogs = ctx.exportLogs;
        var exportProxyLogs = ctx.exportProxyLogs;
        var init = ctx.init;
        var initLogsStream = ctx.initLogsStream;
        var loadSettings = ctx.loadSettings;
        var modelsPage = ctx.modelsPage;
        var predictionAlerts = ctx.predictionAlerts;
        var queuePage = ctx.queuePage;
        var renderAgentDetails = ctx.renderAgentDetails;
        var renderAgentsPage = ctx.renderAgentsPage;
        var renderLogs = ctx.renderLogs;
        var renderNotification = ctx.renderNotification;
        var renderNotificationsList = ctx.renderNotificationsList;
        var renderProxyLogs = ctx.renderProxyLogs;
        var sessionsPage = ctx.sessionsPage;
        var setupNotificationsUI = ctx.setupNotificationsUI;
        // R671 (2026-11-21): сборщики данных и вспомогательные функции, которые в
        // этом файле вызываются bare-именами. Раньше они не приходили в ctx вовсе —
        // первый же тик/рендер падал на ReferenceError, а init() умирал ещё раньше
        // (см. app.js:66). Отсутствие значения теперь не роняет файл молча.
        var fetchClusterState = ctx.fetchClusterState;
        var fetchQueue = ctx.fetchQueue;
        var fetchQueueDetails = ctx.fetchQueueDetails;
        var fetchQueueHistory = ctx.fetchQueueHistory;
        var fetchSessions = ctx.fetchSessions;
        var fetchAgents = ctx.fetchAgents;
        var fetchProxyLogs = ctx.fetchProxyLogs;
        var loadBackendLimits = ctx.loadBackendLimits;
        var startOpsAutoRefresh = ctx.startOpsAutoRefresh;
        var stopOpsAutoRefresh = ctx.stopOpsAutoRefresh;
        // R671: `_t` в этом файле никогда не был объявлен (он есть только локально в
        // app-modals.js). Заменяем на перевод через I18N с честным fallback.
        var _t = ctx._t || function (key, fallback) {
            try {
                if (I18N && typeof I18N.t === 'function') {
                    var v = I18N.t(key);
                    if (v && v !== key) { return v; }
                }
            } catch (e) { /* нет i18n — fallback */ }
            return fallback;
        };

        let refreshTimer = null;

    function filterBackendsForUI(backends) {
        if (!Array.isArray(backends)) return backends;
        var filtered = backends.filter(function (b) {
            // Skip agent-only registrations: same host + cppWorkerPort as another backend.
            // Agent provides GPU metrics but is not a separate inference backend.
            if (b.type === 'llama_cpp' && b.hasAgent) {
                // Check if there's a non-agent backend with same host + cppWorkerPort
                var hasNonAgent = backends.some(function (bb) {
                    return bb.id !== b.id && bb.type === 'llama_cpp' &&
                           !bb.hasAgent && bb.host === b.host &&
                           bb.cppWorkerPort === b.cppWorkerPort;
                });
                if (hasNonAgent) return false; // hide agent duplicate
            }
            return !UNHEALTHY_STATUSES_UI[b.status];
        });
        if (!window.BackendTypeFilter) return filtered;
        var type = BackendTypeFilter.getCurrentType();
        return BackendTypeFilter.filterBackends(filtered, type);
    }

    // R670: какие страницы уже отрисованы хотя бы раз (ключ — имя страницы).
    var renderedPages = Object.create(null);

    function refreshPage(page, options) {
        // R666 (2026-11-21): ЗАЩИТА ОТ ПЕРЕРИСОВКИ — В ЕДИНСТВЕННОЙ ТОЧКЕ ВХОДА.
        //
        // Живая жалоба (продолжение R512/R662): «постоянный сброс обновлением форм
        // остался». Причина: R662 защитил ТОЛЬКО тик автообновления, а refreshPage
        // зовут ещё и обработчики WebSocket-снапшотов (app-listeners.js:213 и др.) и
        // межвкладочная рассылка. Балансер шлёт кластерное состояние каждые
        // несколько секунд, значит форма перезаполнялась мимо любого тика.
        // Теперь правило одно: пока оператор правит поля страницы (фокус — R512)
        // или у страницы есть свежая несохранённая правка (form-dirty, TTL 2 мин),
        // перерисовка этой страницы не выполняется НИ ОДНИМ вызывающим.
        // Обходной путь для осознанных случаев (первый рендер) — {force:true}.
        // R670 (2026-11-21): ЗАЩИТА НЕ ОТМЕНЯЕТ ПЕРВЫЙ РЕНДЕР.
        //
        // Живая жалоба: «на странице всё висит „Загрузка…“, метрики — прочерки, внизу
        // „Подключение…“» — то есть данные так и не нарисовались ни разу. Причина:
        // R666 поставил защиту в единственную точку входа refreshPage, и если на
        // момент загрузки страницы в документе уже есть редактируемое поле с фокусом
        // (экран входа, поле в шапке) или свежая правка, первый рендер пропускался —
        // а второго без данных не будет. Теперь защита включается только ПОСЛЕ того,
        // как страница хотя бы раз отрисована: первая отрисовка неприкосновенна.
        if (!(options && options.force) && page === currentPage && renderedPages[page] === true) {
            if (window.RefreshGuard && typeof window.RefreshGuard.isEditingIn === 'function' &&
                window.RefreshGuard.isEditingIn(document.getElementById(page + '-page'))) {
                return;
            }
            if (window.FormDirty && typeof window.FormDirty.isDirtyPage === 'function' &&
                window.FormDirty.isDirtyPage(page)) {
                return;
            }
        }
        renderedPages[page] = true;
        switch (page) {
            case 'dashboard':
                var filteredBackends = filterBackendsForUI(data.backends);
                // Cluster-level loaded count учитывает и ollama-, и llama_cpp-бэкенды.
                // Запрашиваем асинхронно, не блокируем рендер dashboard.
                Api.clusterModels.loaded().then(function (loadedResp) {
                    if (loadedResp && typeof loadedResp.count === 'number') {
                        // R232: здесь раньше стоял вызов _t(), а он объявлен ЛОКАЛЬНО
                        // внутри renderModelDetails* — в этой области видимости его нет,
                        // ReferenceError молча съедался .catch(), и кластерный счётчик
                        // «загружено моделей» не обновлялся никогда (в демо это было
                        // видно как «0», хотя модель уже загружена).
                        var loadedLabel = (window.I18N && window.I18N.t)
                            ? window.I18N.t('renderers.models_count_loaded', { count: loadedResp.count })
                            : ('Загружено: ' + loadedResp.count);
                        Utils.setText('loadedModels', loadedLabel);
                        Utils.setText('totalModels', loadedResp.count);
                    }
                }).catch(function () { /* тихо: fallback на ollama.runningModels внутри dashboard() */ });
                dashboard(filteredBackends, data.sessions, data.queue);
                predictionAlerts(filteredBackends);
                break;
            case 'monitor':
                sendMonitorConfig();
                break;
            case 'virtual-models':
                // R595: страница переехала из отдельного virtual-models.html в SPA.
                if (window.VirtualModelsPage) {
                    window.VirtualModelsPage.mount();
                    window.VirtualModelsPage.registerProvider();
                    window.VirtualModelsPage.load();
                }
                break;
            case 'backends':
                var filteredBackendsB = filterBackendsForUI(data.backends);
                // 2026-10-03: делегированный обработчик «Подключение клиентов»
                // (показать/скопировать ключ, копировать curl) — один на документ,
                // поэтому монтируем при первом входе на страницу «Бэкенды».
                if (window.ClientAccess && typeof window.ClientAccess.mount === 'function') {
                    window.ClientAccess.mount();
                }
                backendsPage([...filteredBackendsB].sort(function(a, b) { return (a.id || '').localeCompare(b.id || ''); }));
                // R-Image: видимость пункта «Image-бэкенды» зависит от состава
                // кластера, а не от активной страницы, поэтому синхронизируем её
                // и здесь (в дополнение к fetchClusterState).
                if (window.ImageBackendsPage) {
                    window.ImageBackendsPage.syncVisibility(data.backends);
                }
                break;
            case 'models':
                modelsPage(filterBackendsForUI(data.backends));
                // Session A — Bulk operations: после re-render моделей сбросить
                // stale selections в toolbar (selected Set живёт дольше DOM).
                if (window.bulkModels && typeof window.bulkModels.renderToolbar === 'function') {
                    window.bulkModels.renderToolbar();
                }
                break;
            case 'sessions':
                sessionsPage(data.sessions);
                break;
            case 'queue':
                queuePage(data.queue, data.queueTasks, data.queueHistory);
                break;
            case 'diagnostics':
                if (window.Diagnostics && typeof window.Diagnostics.mount === 'function') {
                    window.Diagnostics.mount();
                }
                break;
            case 'logs':
                renderLogs(data.logs);
                // Also render proxy logs when switching to logs page (only if proxy tab is active)
                var proxyTab = document.getElementById('logsTabProxy');
                if (proxyTab && proxyTab.classList.contains('active')) {
                    renderProxyLogs(data.proxyLogs);
                }
                break;
            case 'agents':
                if (data.agents) renderAgentsPage(data.agents);
                break;
            case 'gguf':
                if (window.GgufRenderer) {
                    var container = document.getElementById('ggufContainer');
                    if (container) {
                        // R493 (2026-11-17): render() — это МОНТИРОВАНИЕ страницы
                        // (container.innerHTML = buildPageHtml() + refreshBackends()),
                        // а не перерисовка из готовых данных. Пока refreshPage
                        // вызывался только при переходе на страницу, это было
                        // безвредно. Теперь его вызывает и тик авто-обновления,
                        // и оказалось, что каждые 5 секунд страница GGUF
                        // пересобиралась целиком: пары запросов
                        // GET /api/v1/gguf/backends за тик (провайдер + render),
                        // сброс введённых значений и потеря позиции.
                        //
                        // Правило: монтируем ОДИН раз (пока разметки страницы нет
                        // в DOM), дальше DOM обновляют провайдеры страницы —
                        // refreshBackends / refreshDetail / refreshActiveDownloads.
                        var mounted = container.querySelector('.gguf-master-layout');
                        if (!mounted) {
                            GgufRenderer.render(container);
                        }
                    }
                }
                break;
            case 'image':
                // R-Image Phase 9: страница «Image-модели» — табы в стиле «GGUF
                // модели». image-page.js наполняет табы «Модели на диске»,
                // «Загруженные» и «Настройки», image-models-hf.js — «HuggingFace»
                // и «Загрузки», image-backends-page.js — таб «Обзор», а шелл
                // (image-models-page.js) переключает табы и делает «Проверку бэкенда».
                if (window.ImagePage) {
                    window.ImagePage.init();
                }
                if (window.ImageModelsHf) {
                    window.ImageModelsHf.mount();
                }
                if (window.ImageModelsPage) {
                    window.ImageModelsPage.mount();
                    window.ImageModelsPage.render(data.backends);
                }
                if (window.ImageBackendsPage) {
                    window.ImageBackendsPage.mount();
                    window.ImageBackendsPage.render(data.backends);
                    window.ImageBackendsPage.syncVisibility(data.backends);
                }
                break;
            case 'reference':
                // R493 (2026-11-17): страница «Справка и шаблоны».
                //
                // Только МОНТИРОВАНИЕ, ровно как у case 'gguf' выше: refreshPage
                // вызывается и при переходе на страницу, и каждым тиком
                // авто-обновления. Если бы здесь стояла полная отрисовка, тик
                // пересобирал бы DOM и стирал введённый фильтр и выбранный
                // бэкенд. Данные обновляет провайдер 'reference' в реестре
                // DataRefresh (js/modules/reference-page.js), а mount()
                // идемпотентен — второй и последующие вызовы ничего не делают.
                if (window.ReferencePage && typeof window.ReferencePage.mount === 'function') {
                    window.ReferencePage.mount();
                }
                break;
            case 'settings':
                loadSettings();
                // R305 (этап 3 рефакторинга настроек): реестр «горячих» настроек
                // балансера — ОТДЕЛЬНЫЙ эндпоинт (GET /api/v1/settings), в
                // /api/v1/cluster/config его нет. Перечитываем при каждом входе на
                // страницу: значение могли поменять из другого браузера или через
                // API, и показывать вчерашнее effective было бы враньём.
                // Рисует и меняет значения только модуль — здесь один вызов.
                if (window.SettingsRegistry && typeof window.SettingsRegistry.load === 'function') {
                    window.SettingsRegistry.load();
                }
                setTimeout(function() { loadBackendLimits(); }, 100);
                // R66b (2026-09-22): пересчитать высоты секций аккордеона ПОСЛЕ
                // того как страница стала видимой. restoreAccordionState()
                // вызывается при загрузке, когда #settings-page ещё display:none,
                // поэтому body.scrollHeight == 0 и в inline max-height попадал 0px
                // (секции выглядели как пустые полоски и требовали двух кликов).
                if (window.SettingsUI && typeof SettingsUI.reapplyAccordionHeights === 'function') {
                    SettingsUI.reapplyAccordionHeights();
                }
                // R54.7 (2026-08-24): init AutoTune Settings
                if (window.AutoTuneSettings) {
                    window.AutoTuneSettings.init();
                }
                break;
        }
    }

    function sendMonitorConfig() {
        const frame = document.getElementById('monitorFrame');
        if (!frame || !frame.contentWindow) return;
        const CFG = window.WEBUI_CONFIG || {};
        // R60.16.1 (2026-09-08): API base fallback chain for the monitor iframe.
        // If WEBUI_CONFIG.API_BASE is empty (entrypoint.sh default), fall back to
        // window.location.origin so the iframe's fetch() calls resolve to the
        // same origin (which nginx proxies to loadbalancer). This makes the
        // monitor work in BOTH docker-network and host-browser contexts.
        const apiBase = CFG.API_BASE || (window.location && window.location.origin) || '';
        frame.contentWindow.postMessage({
            type: 'ollamalegion-config',
            apiBase: apiBase,
            apiToken: CFG.API_TOKEN || '',
            refreshInterval: 2000,
            lang: localStorage.getItem('ollamalegion_lang') || 'ru'
        }, '*');
        // R493 (2026-11-17): вместе с конфигом сообщаем текущее состояние паузы.
        //
        // Живой дефект (замер: при СОХРАНЁННОЙ паузе iframe дал 67 запросов за
        // 12 с): состояние паузы отправлялось только по событию (клик по паузе,
        // скрытие вкладки). Iframe загружается позже и о сохранённой паузе не
        // узнавал никогда — его собственный 2-секундный цикл работал всегда и
        // давал 76 % фоновых запросов WebUI (78 из 103 за 15 с), в том числе на
        // страницах, где монитор не виден.
        syncMonitorIframePause();
    }

    /**
     * monitorIframeMustPause — должен ли монитор в iframe молчать прямо сейчас.
     *
     * Правило одно и то же для трёх поводов (страница, пауза, скрытая вкладка):
     * метрики монитора нужны только когда оператор их видит.
     */
    function monitorIframeMustPause() {
        if (currentPage !== 'monitor') return true;
        if (typeof document !== 'undefined' && document.visibilityState === 'hidden') return true;
        if (window.DataRefresh && typeof window.DataRefresh.isAuto === 'function') {
            return !window.DataRefresh.isAuto();
        }
        return false;
    }

    /** syncMonitorIframePause — отправить текущее состояние паузы в iframe. */
    function syncMonitorIframePause() {
        postMonitorPause(monitorIframeMustPause());
    }

    /**
     * refreshCurrentPage — обновить данные АКТИВНОЙ страницы.
     *
     * R84 (2026-10-03): сначала провайдеры страницы (их регистрирует
     * data-refresh.js: список моделей image-бэкенда, метрики монитора, лог
     * прокси, GGUF-список и т.д.), затем перерисовка из уже обновлённого
     * состояния. Раньше здесь была ТОЛЬКО refreshPage(), то есть отрисовка
     * локальной копии data.* — кнопка «Обновить» визуально ничего не меняла.
     */
    function refreshCurrentPage() {
        if (window.DataRefresh && typeof window.DataRefresh.refresh === 'function') {
            return window.DataRefresh.refresh(currentPage).then(function () {
                // R513: у «Настроек» провайдер уже обошёл все её источники —
                // повторная перерисовка была бы вторым проходом по тем же API
                // (и вторым поводом перетереть форму).
                if (currentPage !== 'settings') refreshPage(currentPage);
                return true;
            });
        }
        refreshPage(currentPage);
        return Promise.resolve(true);
    }

    function startPeriodicRefresh() {
        const interval = (window.WEBUI_CONFIG?.REFRESH_INTERVAL || 5000);
        stopPeriodicRefresh();
        // 2026-06-29: добавляем fetchClusterState() в periodic refresh. Без этого
        // метрики дашборда (totalBackends, healthyBackends и т.д.) обновлялись
        // ТОЛЬКО через WebSocket — если WS не подключён (балансер за прокси,
        // CORS preflight, таймаут рукопожатия), карточки метрик оставались
        // с дефолтом "-" из HTML (webui/index.html:138-139). Periodic REST-poll
        // гарантирует, что метрики обновятся даже при неработающем WS.
        //
        // R84 (2026-10-03):
        //   - fetchAgents() здесь НЕ БЫЛ вызван, хотя комментарий это обещал:
        //     страница «Агенты» обновлялась только вручную (кнопкой, которую мы
        //     удалили) — теперь агенты в общем цикле;
        //   - цикл уважает паузу авто-обновления (иконка в шапке) и скрытую
        //     вкладку: 24/7-дашборд больше не молотит API в фоне;
        //   - по завершении тика обновляется индикатор свежести.
        refreshTimer = setInterval(() => {
            if (periodicRefreshPaused()) return;
            if (window.DataRefresh && !window.DataRefresh.isAuto()) return;
            // R512 (2026-11-18): НЕ перерисовывать страницу, пока оператор вводит
            // текст. Живая жалоба: «обновление держит новую информацию, но не даёт
            // работать с формами — постоянно их скрывая». Причина: refreshPage для
            // настроек заново заполняет поля из ответа сервера каждые 5 секунд —
            // набранный символ затирался, курсор уезжал, секции схлопывались.
            // Фокус в поле ввода = «прямо сейчас идёт набор», тик пропускается
            // целиком (и запросы, и перерисовка): следующий тик после ухода из поля
            // принесёт свежие данные. Логика «что считается вводом» — в
            // js/modules/refresh-guard.js (там же объяснение, почему не «dirty»-флаг).
            if (window.RefreshGuard && typeof window.RefreshGuard.isEditingIn === 'function') {
                var editingPageEl = document.getElementById(currentPage + '-page');
                if (window.RefreshGuard.isEditingIn(editingPageEl)) {
                    return;
                }
            }
            // R662 (2026-11-20): ФОКУС — НЕ ЕДИНСТВЕННОЕ «ИДЁТ ВВОД».
            //
            // Живая жалоба (продолжение R512): набрал значение → кликнул чекбокс,
            // вкладку или пустое место → фокус уже не в текстовом поле → следующий
            // тик перерисовывает страницу и затирает набранное. Модуль form-dirty.js
            // помнит время последней ПРАВКИ на странице и не даёт тику её стирать.
            // Ограничители: TTL 2 минуты (авто-обновление возвращается само) и сброс
            // на submit формы — то есть «заморозки навсегда» из комментария R512 нет.
            if (window.FormDirty && typeof window.FormDirty.isDirtyPage === 'function' &&
                window.FormDirty.isDirtyPage(currentPage)) {
                return;
            }
            fetchClusterState().then(function () {
                if (window.DataRefresh) window.DataRefresh.markFresh();
            }).catch(function (e) {
                if (window.DataRefresh) window.DataRefresh.markError(e);
            });
            // R493 (2026-11-17): ТАЙМЕР ОБЯЗАН ДЕЛАТЬ ТО ЖЕ, ЧТО КНОПКА.
            //
            // Живой дефект (жалоба оператора «авто-обновление не работает, а на
            // странице GGUF загруженные модели видны только после F5»): тик
            // обновлял общие данные (cluster, очередь, сессии, агенты) и НЕ
            // дёргал провайдеров активной страницы — их вызывала только кнопка
            // «Обновить». Собственные источники страниц (GGUF-список и панель
            // деталей, image-модели и загрузки, диагностика) оставались
            // замороженными до ручного обновления, хотя индикатор в шапке
            // показывал «Обновлено только что».
            //
            // Правило теперь одно на оба входа: провайдеры страницы + перерисовка
            // страницы из уже обновлённого состояния.
            var DR = window.DataRefresh;
            var pageHasOwnSources = !!(DR && typeof DR.hasProvider === 'function' && DR.hasProvider(currentPage));
            if (!pageHasOwnSources) {
                // Страницы без собственных провайдеров (dashboard, backends, help)
                // живут на общем наборе запросов — как и раньше.
                fetchAgents();
                fetchQueue();
                fetchQueueDetails();
                fetchQueueHistory();
                fetchSessions();
            }
            if (DR && typeof DR.refresh === 'function') {
                DR.refresh(currentPage).then(function () {
                    // Перерисовка: fetch-функции обновляют `data.*`, но не DOM
                    // страницы. Без этого авто-обновление обновляло данные, а
                    // оператор видел прежние числа (ровно то, на что жаловались).
                    //
                    // R513 (2026-11-19): для «Настроек» перерисовка НЕ нужна —
                    // провайдер уже обошёл все её источники (серверный конфиг,
                    // реестр, лимиты, AutoTune), и повторный refreshPage только
                    // сделал бы второй проход по тем же API. Раньше четыре
                    // перерисовки за тик и были причиной «формы пропадают».
                    if (currentPage === 'settings') { return; }
                    refreshPage(currentPage);
                }).catch(function () { /* ошибку покажет индикатор свежести */ });
                return;
            }
            fetchAgents();
            fetchQueue();
            fetchQueueDetails();
            fetchQueueHistory();
            fetchSessions();
            // R66b (2026-09-22): пока открыта вкладка Settings, обновляем таблицу
            // «Лимиты бэкендов». Раньше единственным триггером был вход на вкладку
            // (refreshPage → loadBackendLimits), и если в тот момент список бэкендов
            // был пуст/недоступен, loadBackendLimits() оставлял строку
            // «Загрузка...» НАВСЕГДА — periodic refresh её не перерисовывал.
            // R493: при живом DataRefresh это делает провайдер 'settings'.
            if (currentPage === 'settings') {
                loadBackendLimits();
            }
        }, interval);
    }

    /**
     * postMonitorPause — сообщить iframe монитора о паузе/возобновлении.
     *
     * Монитор — отдельный документ со своим 2-секундным циклом и своим
     * sparkline-поллером: из родителя его таймеры не остановить, поэтому
     * договариваемся сообщением (тот же канал, что и ollamalegion-config).
     */
    function postMonitorPause(paused) {
        var frame = document.getElementById('monitorFrame');
        if (!frame || !frame.contentWindow) return;
        try {
            frame.contentWindow.postMessage({ type: 'ollamalegion-pause', paused: !!paused }, '*');
        } catch (e) { /* iframe ещё не загрузился — не критично */ }
    }

    /** stopPeriodicRefresh — снять таймер (повторный вызов безопасен). */
    function stopPeriodicRefresh() {
        if (refreshTimer) clearInterval(refreshTimer);
        refreshTimer = null;
    }

    /**
     * periodicRefreshPaused — опрос остановлен из-за скрытой вкладки.
     *
     * ЗАЧЕМ: WebUI часто держат открытым сутками в фоне; раньше цикл раз в 5 с
     * продолжал ходить в API и на скрытой вкладке. При возврате на вкладку
     * данные обновляются сразу (см. setupVisibilityRefresh), поэтому пауза
     * незаметна для оператора.
     */
    function periodicRefreshPaused() {
        return !!(typeof document !== 'undefined' && document.visibilityState === 'hidden');
    }

    /**
     * setupVisibilityRefresh — пауза опроса на скрытой вкладке и мгновенное
     * обновление при возврате.
     */
    function setupVisibilityRefresh() {
        if (typeof document === 'undefined' || !document.addEventListener) return;
        document.addEventListener('visibilitychange', function () {
            if (document.visibilityState === 'hidden') {
                stopPeriodicRefresh();
                syncMonitorIframePause();
                if (window.DataRefresh && window.DataRefresh.isAuto()) stopOpsAutoRefresh();
                return;
            }
            syncMonitorIframePause();
            if (window.DataRefresh && window.DataRefresh.isAuto()) startOpsAutoRefresh();
            startPeriodicRefresh();
            if (window.DataRefresh && !window.DataRefresh.isAuto()) return;
            fetchClusterState().then(function () {
                if (window.DataRefresh) window.DataRefresh.markFresh();
            }).catch(function () { /* индикатор уже покажет состояние */ });
            refreshPage(currentPage);
        });
    }

    /**
     * registerPageProviders — что обновляет страница при нажатии «Обновить».
     *
     * R84 (2026-10-03): до этого у страниц были СВОИ кнопки «Обновить», и часть
     * из них обновляла лишь локальную копию данных. Теперь источники описаны
     * здесь, а кнопка и индикатор в шапке — одни на приложение.
     */
    function registerPageProviders() {
        if (!window.DataRefresh || typeof window.DataRefresh.register !== 'function') return;
        var DR = window.DataRefresh;
        // Модели: перечитать состав кластера и перерисовать карточки/тулбар.
        DR.register('models', function () {
            return Promise.resolve(fetchClusterState()).then(function () {
                modelsPage(data.backends);
                if (window.bulkModels && typeof window.bulkModels.renderToolbar === 'function') {
                    window.bulkModels.renderToolbar();
                }
            });
        });
        DR.register('agents', function () { return fetchAgents(); });
        DR.register('logs', function () { return fetchProxyLogs(); });
        DR.register('sessions', function () { return fetchSessions(); });
        DR.register('queue', function () {
            return Promise.all([fetchQueue(), fetchQueueDetails(), fetchQueueHistory()]);
        });
        DR.register('monitor', function () {
            // Метрики монитора живут в отдельном iframe и своём цикле
            // (fetchAllSafe); кнопка «Обновить» раньше их не касалась вовсе.
            if (typeof window.fetchAllSafe === 'function') return window.fetchAllSafe();
            sendMonitorConfig();
            return true;
        });
        // R513 (2026-11-19): «Настройки» — единственная страница, которую тик
        // раньше перерисовывал ЦЕЛИКОМ: refreshPage('settings') заново тянул
        // серверный конфиг (заливал ~40 полей), реестр горячих настроек
        // (innerHTML панели), лимиты бэкендов (innerHTML таблицы с input'ами) и
        // AutoTune — по одному и тому же таймеру, четырьмя проходами. Теперь все
        // её источники — в одном провайдере, и каждый рисует только СВОЮ панель.
        // Пропуск перерисовки при вводе делает не этот код, а сами модули
        // (js/modules/refresh-guard.js): так правило одно и для тика, и для
        // кнопки «Обновить», и для клика по индикатору свежести.
        DR.register('settings', function () {
            loadSettings();
            if (window.SettingsRegistry && typeof window.SettingsRegistry.load === 'function') {
                window.SettingsRegistry.load();
            }
            return Promise.resolve(loadBackendLimits());
        });
    }


    // ---- Filtering ----

    function filterBackends(query) {
        const rows = document.querySelectorAll('#backendsTableBody tr');
        const lower = query.toLowerCase();
        rows.forEach(row => {
            row.style.display = row.textContent.toLowerCase().includes(lower) ? '' : 'none';
        });
    }

    function filterSessions(query) {
        const rows = document.querySelectorAll('#sessionsTableBody tr');
        const lower = query.toLowerCase();
        rows.forEach(row => {
            row.style.display = row.textContent.toLowerCase().includes(lower) ? '' : 'none';
        });
    }

    /**
     * Filter + search + type-filter для карточек моделей на Models tab.
     *
     * Логика:
     * - `query` (search) — подстрочный поиск по всему тексту карточки.
     * - `activeType` (из #modelsTypeFilter) — фильтр по типу бэкенда (ollama/llama_cpp/all).
     * - Результат: card.style.display = 'none' для не подходящих, '' для подходящих.
     *
     * Session B — Filter/Search + Sort improvements:
     * - Добавлено отображение счётчика видимых карточек в #modelsCount (если есть).
     * - Добавлен empty-state #modelsEmpty.
     * - Сохраняем query в localStorage для восстановления после refresh страницы.
     */
    function filterModels(query) {
        const cards = document.querySelectorAll('#modelsGrid .model-card');
        const lower = (query || '').toLowerCase();

        // Текущий выбранный тип бэкенда
        var activeTypeBtn = document.querySelector('#modelsTypeFilter .filter-btn.active');
        var activeType = activeTypeBtn ? activeTypeBtn.dataset.filterType : 'all';

        var visibleCount = 0;
        cards.forEach(function (card) {
            var text = card.textContent.toLowerCase();
            var matchQuery = !lower || text.indexOf(lower) !== -1;

            // Определяем тип бэкенда по data-атрибуту карточки или по badge
            var cardType = card.dataset.backendType || card.getAttribute('data-backend-type') || '';
            if (!cardType) {
                // Fallback: ищем в badge тексте 🦙 Ollama / 🦒 llama.cpp
                if (text.indexOf('🦙 ollama') !== -1) cardType = 'ollama';
                else if (text.indexOf('🦒 llama.cpp') !== -1) cardType = 'llama_cpp';
            }
            var matchType = (activeType === 'all') || (cardType === activeType);

            var show = matchQuery && matchType;
            card.style.display = show ? '' : 'none';
            if (show) visibleCount++;
        });

        // Session B: показываем счётчик "X из Y" в #modelsCount, если есть.
        var counter = document.getElementById('modelsCount');
        if (counter) {
            if (cards.length > 0) {
                var total = cards.length;
                if (visibleCount === total) {
                    counter.textContent = _t('models.filter_count_all', { count: total });
                } else {
                    counter.textContent = _t('models.filter_count_filtered', { visible: visibleCount, total: total });
                }
                counter.style.display = '';
            } else {
                counter.style.display = 'none';
            }
        }

        // Показываем/скрываем empty-state
        var empty = document.getElementById('modelsEmpty');
        if (empty) {
            if (cards.length > 0 && visibleCount === 0) {
                empty.hidden = false;
            } else {
                empty.hidden = true;
            }
        }

        // Сохраняем query в localStorage для восстановления при refresh.
        try {
            localStorage.setItem('ollamalegion_models_search', lower || '');
        } catch (e) { /* localStorage недоступен — ignore */ }
    }

    /**
     * Применить сортировку карточек моделей в соответствии с #modelsSortBy.
     * Поддерживает: name-asc, name-desc, size-desc, size-asc, vram-desc, vram-asc, backend-asc.
     * Карточки сортируются in-place через appendChild (DOM reordering).
     *
     * Session B — Filter/Search + Sort improvements: добавлены VRAM-режимы.
     * VRAM-режимы полезны для планирования GPU-нагрузки (какие модели занимают
     * больше всего видеопамяти — отображаются первыми при vram-desc).
     */
    function applyModelsSort() {
        var grid = document.getElementById('modelsGrid');
        if (!grid) return;
        var sortSelect = document.getElementById('modelsSortBy');
        if (!sortSelect) return;
        var sortKey = sortSelect.value || 'name-asc';

        var cards = Array.prototype.slice.call(grid.querySelectorAll('.model-card'));
        if (cards.length === 0) return;

        function getName(card) {
            return (card.dataset.modelName || card.querySelector('.model-card-title')?.textContent || '').trim().toLowerCase();
        }
        function getSize(card) {
            // Размер в байтах из data-size-bytes, иначе парсим текст
            var v = card.dataset.sizeBytes;
            if (v) return parseInt(v, 10) || 0;
            var t = card.textContent || '';
            var m = t.match(/([\d.]+)\s*(GB|MB|KB|B)\b/i);
            if (!m) return 0;
            var n = parseFloat(m[1]) || 0;
            var u = m[2].toUpperCase();
            if (u === 'GB') return n * 1024 * 1024 * 1024;
            if (u === 'MB') return n * 1024 * 1024;
            if (u === 'KB') return n * 1024;
            return n;
        }
        function getVram(card) {
            // VRAM в MB из data-vram-mb (Session B).
            var v = card.dataset.vramMb;
            if (v !== undefined && v !== '') return parseInt(v, 10) || 0;
            // Fallback: парсим "VRAM: 1234 MB" из текста карточки.
            var t = card.textContent || '';
            var m = t.match(/VRAM[^\d]*([\d.]+)\s*MB/i);
            if (!m) return 0;
            return Math.round(parseFloat(m[1])) || 0;
        }
        function getBackend(card) {
            return (card.dataset.backendId || card.dataset.backend || '').toLowerCase();
        }

        cards.sort(function (a, b) {
            switch (sortKey) {
                case 'name-desc':
                    return getName(b).localeCompare(getName(a));
                case 'size-desc':
                    return getSize(b) - getSize(a);
                case 'size-asc':
                    return getSize(a) - getSize(b);
                case 'vram-desc':
                    return getVram(b) - getVram(a);
                case 'vram-asc':
                    return getVram(a) - getVram(b);
                case 'backend-asc':
                    var ba = getBackend(a), bb = getBackend(b);
                    if (ba !== bb) return ba.localeCompare(bb);
                    return getName(a).localeCompare(getName(b));
                case 'name-asc':
                default:
                    return getName(a).localeCompare(getName(b));
            }
        });

        // Перемещаем карточки в DOM в новом порядке
        var fragment = document.createDocumentFragment();
        cards.forEach(function (c) { fragment.appendChild(c); });
        grid.appendChild(fragment);
    }

    // ---- UI Utilities ----

    /**
     * Обновление маркера типа движка инференса в сайдбаре.
     * Принимает clusterState (или любой объект с полями backendEngine, backendTypeCounts, operatingMode).
     * Отображает: 🦙 Ollama API, 🦒 llama.cpp, 🦒🦙 llama.cpp + Ollama, или 🔌 Автоопределение...
     *
     * R112 (2026-10-11): ЕДИНАЯ реализация маркера — BackendTypeFilter.updateEngineBadge
     * (он учитывает фактический состав кластера: clusterComposition). Здесь раньше
     * лежала ВТОРАЯ копия той же логики — две копии неизбежно расходились, поэтому
     * функция делегирует, а старая ветка оставлена только как аварийный fallback
     * на случай, если модуль фильтра не загрузился.
     */
    function updateBackendEngineBadge(state) {
        if (window.BackendTypeFilter && window.BackendTypeFilter.updateEngineBadge) {
            BackendTypeFilter.updateEngineBadge((state && state.effectiveBackendType) || '', false);
            return;
        }
        var badge = document.getElementById('backendEngineBadge');
        var label = document.getElementById('backendEngineLabel');
        if (!badge || !label) return;

        // Приоритет: выбор пользователя (localStorage через BackendTypeFilter) > cluster state
        var userType = window.BackendTypeFilter ? BackendTypeFilter.getCurrentType() : null;
        var effectiveType = (state && state.effectiveBackendType) || '';
        var engine = (state && state.backendEngine) || '';
        var counts = (state && state.backendTypeCounts) || {};

        // Remove old colour classes
        badge.classList.remove('engine-ollama', 'engine-llama_cpp', 'engine-image_cpp', 'engine-auto');

        var icon = '🔌';
        var text = (window.I18N ? I18N.t('dashboard.engine_auto') : 'Автоопределение...');
        var cssClass = 'engine-auto';
        var title = (window.I18N ? I18N.t('dashboard.engine_hint_auto') : 'Тип движка: автоопределение');

        // Приоритет 1: выбор пользователя (localStorage)
        if (userType === 'llama_cpp') {
            icon = '🦒';
            text = 'llama.cpp';
            cssClass = 'engine-llama_cpp';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_llama_cpp') : 'Движок: llama.cpp');
        } else if (userType === 'ollama') {
            icon = '🦙';
            text = 'Ollama API';
            cssClass = 'engine-ollama';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_ollama') : 'Движок: Ollama API');
        } else if (userType === 'all') {
            // Round 18g: пользователь выбрал "Все" — показываем нейтральный badge.
            icon = '🔌';
            text = (window.I18N ? I18N.t('dashboard.engine_all') : 'Все движки');
            cssClass = 'engine-auto';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_all') : 'Движок: все');
        } else if (userType === 'image_cpp') {
            // R-Image Phase 5: пользователь выбрал фильтр «image.cpp» в type switcher.
            // Без этой ветки badge откатывался к серверному текстовому движку на
            // каждом cluster update (2 с) — выбор фильтра выглядел «непринятым».
            icon = '🎨';
            text = 'image.cpp';
            cssClass = 'engine-image_cpp';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_image_cpp') : 'Движок: image.cpp (stable-diffusion.cpp)');
        } else if (effectiveType === 'llama_cpp') {
            icon = '🦒';
            text = 'llama.cpp';
            cssClass = 'engine-llama_cpp';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_llama_cpp') : 'Движок: llama.cpp');
        } else if (effectiveType === 'ollama') {
            icon = '🦙';
            text = 'Ollama API';
            cssClass = 'engine-ollama';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_ollama') : 'Движок: Ollama API');
        } else if (engine === 'llama_cpp') {
            icon = '🦒';
            text = 'llama.cpp';
            cssClass = 'engine-llama_cpp';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_llama_cpp') : 'Движок: llama.cpp');
        } else if (engine === 'ollama_api') {
            icon = '🦙';
            text = 'Ollama API';
            cssClass = 'engine-ollama';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_ollama') : 'Движок: Ollama API');
        }

        // Append per-type counts to title if available
        if (counts && Object.keys(counts).length > 0) {
            var parts = [];
            if (counts.ollama) parts.push('🦙 Ollama: ' + counts.ollama);
            if (counts.llama_cpp) parts.push('🦒 llama.cpp: ' + counts.llama_cpp);
            if (parts.length > 0) {
                title += ' | ' + parts.join(', ');
            }
        }

        var iconEl = badge.querySelector('.engine-icon');
        if (iconEl) iconEl.textContent = icon;
        label.textContent = text;
        badge.classList.add(cssClass);
        badge.title = title;
    }

    function updateConnectionStatus(connected) {
        const status = document.getElementById('connectionStatus');
        if (!status) return;
        const dot = status.querySelector('.status-dot');
        const text = status.querySelector('.status-text');
        if (!dot || !text) return;

        if (connected) {
            dot.classList.remove('disconnected');
            dot.classList.add('connected');
            text.textContent = window.I18N ? I18N.t('common.connected') : 'Connected';
        } else {
            dot.classList.remove('connected');
            dot.classList.add('disconnected');
            text.textContent = window.I18N ? I18N.t('common.disconnected') : 'Disconnected';
        }
    }

    function showToast(message, type = 'info') {
        const container = document.getElementById('toastContainer');
        if (!container) return;
        const toast = document.createElement('div');
        toast.className = `toast ${type}`;
        toast.innerHTML = `<span>${Utils.escapeHtml(message)}</span>`;
        container.appendChild(toast);

        setTimeout(() => {
            toast.style.opacity = '0';
            toast.style.transform = 'translateX(100%)';
            setTimeout(() => toast.remove(), 300);
        }, 4000);
    }

    /**
     * Добавить запись лога в data.logs и (если пользователь на Logs tab) перерисовать.
     *
     * F.2 (Session F): поддерживает 2 формата входа:
     *   1. addLog(message, level) — старая сигнатура, используется для локальных событий WebUI.
     *   2. addLogEntry(entry) — новая, для записей из WebSocket /ws/logs. entry = {time, level, message, source}.
     *
     * Backend присылает time как ISO-строку (RFC3339) или Date; здесь мы нормализуем
     * к локализованному HH:MM:SS для совместимости с renderLogs().
     */

        return {
            filterBackendsForUI: filterBackendsForUI,
            refreshPage: refreshPage,
            sendMonitorConfig: sendMonitorConfig,
            monitorIframeMustPause: monitorIframeMustPause,
            syncMonitorIframePause: syncMonitorIframePause,
            refreshCurrentPage: refreshCurrentPage,
            startPeriodicRefresh: startPeriodicRefresh,
            postMonitorPause: postMonitorPause,
            stopPeriodicRefresh: stopPeriodicRefresh,
            periodicRefreshPaused: periodicRefreshPaused,
            setupVisibilityRefresh: setupVisibilityRefresh,
            registerPageProviders: registerPageProviders,
            filterBackends: filterBackends,
            filterSessions: filterSessions,
            filterModels: filterModels,
            applyModelsSort: applyModelsSort,
            updateBackendEngineBadge: updateBackendEngineBadge,
            updateConnectionStatus: updateConnectionStatus,
            showToast: showToast
        };
    };
})();
