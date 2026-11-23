// app-settings.js — R652 (2026-11-20): применение и сохранение настроек и
// кэш конфигурации бэкендов вынесены из `app.js` (оркестратор, цель §5 — < 1500).
//
// ЗАЧЕМ ВЫНЕСЕНО. В `app.js` жил слой «настройки»: прочитать форму, применить
// серверный/локальный конфиг, сохранить (PUT с проверкой отката — verifySync),
// авто-сохранение по change/input (55 полей) и кэш конфигурации бэкендов (R88).
// Это связная тема со своей причиной меняться; оркестратор (init, роутинг,
// слушатели, обновление страниц) — другое.
//
// ПОЧЕМУ ФАБРИКА. Слой берёт из `app.js` общее состояние (`data`) и четыре
// функции + ДОСТУП К ТЕКУЩЕЙ СТРАНИЦЕ. Страница передаётся геттером, а не
// значением: `currentPage` — это `let`, который меняет switchPage; передача по
// значению заморозила бы его на момент загрузки.
//
// ПОЧЕМУ `autoSaveTimer`/`backendConfigCache` ЗДЕСЬ. Они принадлежали только
// этому слою (проверено: вне группы нет ни одного обращения) и переехали вместе
// с ним — иначе таймер авто-сохранения и кэш жили бы в файле, который о них
// ничего не знает.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: тела перенесены как есть, отступ сохранён.
(function () {
    'use strict';

    window.AppSettings = function (ctx) {
        var data = ctx.data;
        var getCurrentPage = ctx.getCurrentPage;
        var refreshPage = ctx.refreshPage;
        var showToast = ctx.showToast;
        var updateConnectionStatus = ctx.updateConnectionStatus;
        var updateBackendEngineBadge = ctx.updateBackendEngineBadge;

        let autoSaveTimer = null;

        // ---- ЗАПИСЬ В ПОЛЯ (R676) ----
        //
        // ЖИВАЯ ЖАЛОБА. «Постоянное обновление стирает то, что я ввёл в формы».
        // Здесь были десятки прямых `el.value = …` / `el.checked = …`, которые
        // заливали серверные значения независимо от защит R512/R662: защиты
        // глушили ПЕРЕРИСОВКУ страницы, а запись шла мимо них. Замер на стенде:
        // набрал 9999 → клик мимо → один проход applyServerConfig → 2048 при
        // FormDirty.isDirtyPage('settings') === true.
        //
        // Теперь единственная точка записи — SafeForm (js/modules/safe-form.js):
        // он пропускает запись только если поле не в фокусе, его значение не
        // менял оператор и на странице нет свежей несохранённой правки. Поле с
        // введённым значением не трогается, но рендер продолжается — остальные
        // поля, бейджи и таблицы обновляются как обычно.
        //
        // Fallback на прямую запись оставлен осознанно и только на случай, когда
        // модуль вообще не подключён (тогда поведение прежнее, а не «ничего не
        // применяется»); за подключение отвечает webui/tests/page-contract-r676.test.js.
        function writeValue(el, value) {
            if (!el) { return false; }
            var SF = (typeof window !== 'undefined') ? window.SafeForm : null;
            if (SF && typeof SF.setValue === 'function') { return SF.setValue(el, value); }
            el.value = value;
            return true;
        }

        function writeChecked(el, value) {
            if (!el) { return false; }
            var SF = (typeof window !== 'undefined') ? window.SafeForm : null;
            if (SF && typeof SF.setChecked === 'function') { return SF.setChecked(el, value); }
            el.checked = !!value;
            return true;
        }

    // ---- R700: порог диска (Min Free Disk) ----
    //
    // ЖИВАЯ ЖАЛОБА (архив второго стенда). «Снизил порог до 6000, а узел всё
    // равно исключён» / «освободил 40 ГБ в томе моделей, а метрика диска не
    // изменилась». Причина — в том, ЧТО измеряется: агент выполняет `df -m /`
    // в СВОЁМ контейнере, то есть меряет корневую ФС контейнера, а модели лежат
    // в bind-томе /app/models, который в эту метрику не входит. Освобождение
    // тома не меняет число, по которому балансер отбрасывает узел.
    //
    // Поэтому рядом с полем обязано быть два факта: (1) что именно измеряется;
    // (2) сколько там СЕЙЧАС на каждом узле (system.diskFree из /api/v1/cluster).
    // Без них оператор регулирует порог вслепую.
    var MIN_FREE_DISK_DEFAULT = 10240;
    var MIN_FREE_DISK_MAX = 1048576;
    var diskGuardCache = { at: 0, backends: null };

    function i18nT(key, vars, fallback) {
        var out = fallback || key;
        try {
            if (window.I18N && typeof window.I18N.t === 'function') {
                out = window.I18N.t(key, vars);
            }
        } catch (e) { /* i18n не загружен — показываем fallback */ }
        return out;
    }

    // updateDiskGuardBounds — границы и умолчание приходят с сервера
    // (minFreeDiskDefault/minFreeDiskMax в GET /api/v1/cluster/config).
    // Вторая копия чисел в JS разошлась бы с валидацией на сервере.
    function updateDiskGuardBounds(serverConfig) {
        if (!serverConfig) return;
        var d = parseInt(serverConfig.minFreeDiskDefault, 10);
        if (Number.isFinite(d) && d >= 0) MIN_FREE_DISK_DEFAULT = d;
        var m = parseInt(serverConfig.minFreeDiskMax, 10);
        if (Number.isFinite(m) && m > 0) MIN_FREE_DISK_MAX = m;
    }

    // loadDiskGuardBackends — метрики диска по узлам. Кэш 15 с: страница
    // «Настройки» перерисовывается каждые ~5 с, и запрос /api/v1/cluster на
    // каждый тик ради одной подсказки — лишний трафик.
    function loadDiskGuardBackends(force) {
        if (!force && diskGuardCache.backends && (Date.now() - diskGuardCache.at) < 15000) {
            return Promise.resolve(diskGuardCache.backends);
        }
        return Api.cluster().then(function (state) {
            diskGuardCache.backends = (state && state.backends) || [];
            diskGuardCache.at = Date.now();
            return diskGuardCache.backends;
        }).catch(function () {
            // Ошибку здесь не показываем: подсказка — не критичный путь, а
            // «нет данных» честно отрисуется ниже.
            return diskGuardCache.backends;
        });
    }

    // diskFreeMBOf — свободное место узла в МБ.
    //
    // ЕДИНИЦА. `types.SystemMetrics.DiskFree` — уже МЕГАБАЙТЫ: агент выполняет
    // `df -m /` и берёт третье поле (`internal/agent/system.go`, getDiskInfo), а
    // порог `resources.disk.minFreeMB` сравнивается с ним напрямую
    // (slot_manager.go: diskLowR696). Пересчитывать «из байт» здесь нельзя —
    // подсказка показывала бы 9480/1024 = 9 МБ вместо 9480 МБ.
    function diskFreeMBOf(b) {
        if (!b || !b.system) return null;
        var mb = Number(b.system.diskFree);
        if (!Number.isFinite(mb) || mb <= 0) return null;
        return Math.round(mb);
    }

    function clearDiskGuardError() {
        var err = document.getElementById('minFreeDiskError');
        if (err) { err.hidden = true; err.textContent = ''; }
        var el = document.getElementById('minFreeDisk');
        if (el && el.classList && typeof el.classList.remove === 'function') el.classList.remove('invalid');
    }

    function showDiskGuardError(text) {
        var err = document.getElementById('minFreeDiskError');
        if (err) { err.hidden = false; err.textContent = text; }
        var el = document.getElementById('minFreeDisk');
        if (el && el.classList && typeof el.classList.add === 'function') el.classList.add('invalid');
    }

    // refreshDiskGuardHint — «что измеряется + сколько сейчас» рядом с порогом.
    function refreshDiskGuardHint() {
        var hint = document.getElementById('minFreeDiskHint');
        if (!hint) return;
        var el = document.getElementById('minFreeDisk');
        var threshold = el ? parseInt(el.value, 10) : NaN;
        if (!Number.isFinite(threshold) || threshold < 0) threshold = MIN_FREE_DISK_DEFAULT;
        loadDiskGuardBackends(false).then(function (backends) {
            var parts = [];
            parts.push(i18nT('settings.min_free_disk.measured', {},
                'Измеряется `df -m /` — корневая ФС контейнера агента. Модели лежат в bind-томе (/app/models), в эту метрику он не входит: освобождение тома число не изменит.'));
            var rows = [];
            (backends || []).forEach(function (b) {
                var free = diskFreeMBOf(b);
                if (free === null) return;
                rows.push({ id: (b && b.id) || '?', free: free });
            });
            if (!rows.length) {
                parts.push(i18nT('settings.min_free_disk.no_data', {},
                    'Узлы ещё не прислали метрику диска — сравнивать порог не с чем.'));
            } else {
                rows.sort(function (a, b) { return a.free - b.free; });
                var list = rows.map(function (r) { return r.id + ': ' + r.free + ' МБ'; }).join(', ');
                parts.push(i18nT('settings.min_free_disk.current', { backends: list, threshold: threshold },
                    'Сейчас на узлах — ' + list + ' (порог ' + threshold + ' МБ).'));
                if (rows[0].free < threshold) {
                    parts.push(i18nT('settings.min_free_disk.blocked',
                        { backend: rows[0].id, free: rows[0].free, threshold: threshold },
                        'Узел ' + rows[0].id + ' (' + rows[0].free + ' МБ) исключён из выбора: свободно меньше порога ' + threshold + ' МБ.'));
                }
            }
            hint.textContent = parts.join(' ');
        });
    }

    // bindDiskGuardUI — кнопка «по умолчанию» и живое обновление подсказки.
    // Навешивается один раз: loadSettings вызывается на каждый вход на страницу
    // и на каждый тик, повторные слушатели задвоили бы сохранение.
    var diskGuardBound = false;
    function bindDiskGuardUI() {
        if (diskGuardBound) return;
        var input = document.getElementById('minFreeDisk');
        var btn = document.getElementById('minFreeDiskDefault');
        if (!input && !btn) return;
        diskGuardBound = true;
        if (input && input.addEventListener) {
            input.addEventListener('input', function () {
                clearDiskGuardError();
                refreshDiskGuardHint();
            });
        }
        if (btn && btn.addEventListener) {
            btn.addEventListener('click', function (e) {
                if (e && e.preventDefault) e.preventDefault();
                if (input) input.value = String(MIN_FREE_DISK_DEFAULT);
                clearDiskGuardError();
                refreshDiskGuardHint();
                // Сохраняем сразу: иначе «сбросил» осталось бы только в поле,
                // а гейт продолжал бы работать по старому порогу.
                saveSettings(false);
            });
        }
    }

    // ---- Data Fetching ----

    /**
     * backendConfigCache — последняя УСПЕШНО полученная конфигурация бэкендов
     * (id → объект из GET /api/v1/backends).
     *
     * R88 (2026-10-08). ЗАЧЕМ КЭШ. Конфигурационные поля (autoTune, weight,
     * labels, maxModels, apiStyle, engine) есть ТОЛЬКО в /api/v1/backends, а
     * метрики приходят и из других источников — в том числе из WebSocket
     * (`clusterState`/`legacy`), где этих полей нет вовсе. Раньше список
     * бэкендов из WS подменял обогащённый (app-listeners.js → updateBackends),
     * и карточка AutoTune мигала: REST-тик её рисовал, следующий WS-тик стирал.
     * Оператор видел «в какой-то тик пометка есть, в какой-то нет».
     *
     * Теперь конфигурация живёт в кэше и применяется к ЛЮБОМУ источнику, а
     * недоступность /api/v1/backends больше не обнуляет поля — берём последнее
     * известное значение (и только если его никогда не было, полей нет).
     */
    var backendConfigCache = {};

    /**
     * mergeBackendConfig — конфиг + метрики одного бэкенда.
     *
     * База — конфиг (там поля, которых в метриках НЕТ), метрики перекрывают
     * значения, которые есть в обоих источниках (например host/status).
     */
    function mergeBackendConfig(cfg, b) {
        if (!cfg) return b;
        // Копия, чтобы не мутировать состояние, пришедшее из API.
        var merged = Object.assign({}, cfg, b);
        // Явно переносим поля, которых в metrics-типе нет вообще
        // (Object.assign их бы взял из cfg, но список фиксируем, чтобы
        // поведение не зависело от порядка аргументов).
        merged.weight = cfg.weight;
        merged.labels = cfg.labels || [];
        merged.maxModels = cfg.maxModels;
        merged.runtimeMaxModels = cfg.runtimeMaxModels;
        merged.maxConcurrentRequests = (b.maxConcurrentRequests !== undefined && b.maxConcurrentRequests !== null)
            ? b.maxConcurrentRequests
            : cfg.maxConcurrentRequests;
        merged.autoTune = cfg.autoTune;
        merged.apiStyle = cfg.apiStyle;
        merged.effectiveApiStyle = cfg.effectiveApiStyle;
        merged.engine = cfg.engine;
        merged.cppWorkerPort = cfg.cppWorkerPort;
        merged.ollamaPort = cfg.ollamaPort;
        merged.agentPort = cfg.agentPort;
        merged.host = cfg.host || b.host;
        merged.name = cfg.name || b.name;
        merged.type = cfg.type || b.type;
        merged.hasAgent = (cfg.hasAgent !== undefined) ? cfg.hasAgent : b.hasAgent;
        merged.agentId = cfg.agentId || b.agentId;
        merged.lastAgentContact = cfg.lastAgentContact || b.lastAgentContact;
        return merged;
    }

    /**
     * applyBackendConfig — применить КЭШ конфигурации к любому списку бэкендов.
     *
     * Вызывается и из REST-пути (fetchClusterState), и из app-listeners.js
     * (WebSocket-тики), чтобы карточки не мигали между источниками.
     */
    function applyBackendConfig(backends) {
        if (!Array.isArray(backends) || backends.length === 0) return backends;
        return backends.map(function (b) {
            if (!b || !b.id) return b;
            return mergeBackendConfig(backendConfigCache[b.id], b);
        });
    }

    /**
     * enrichBackendsWithConfig — R65d (2026-09-20).
     *
     * Мержит конфигурационные поля бэкенда (weight, labels, maxModels,
     * runtimeMaxModels, autoTune, apiStyle, engine, gpuMode, lastAgentContact)
     * из GET /api/v1/backends в массив, полученный из GET /api/v1/cluster.
     *
     * Почему так, а не «просто взять /api/v1/backends»: страница использует
     * МЕТРИКИ из cluster state (GPU/System/Prediction), а /api/v1/backends их
     * отдаёт не всегда (зависит от наличия agent metrics для бэкенда).
     * Поэтому берём объединение: база — cluster state, конфиг — добавляем.
     *
     * Fail-safe (R88): если /api/v1/backends недоступен (401/500/сеть), берём
     * ПОСЛЕДНЮЮ удачную конфигурацию из кэша — иначе поля AutoTune/Weight/Labels
     * исчезали на один тик и возвращались на следующем (мигание в панели).
     */
    async function enrichBackendsWithConfig(clusterBackends) {
        if (!Array.isArray(clusterBackends) || clusterBackends.length === 0) {
            return clusterBackends;
        }
        if (Api.backends) {
            try {
                const resp = await Api.backends();
                var list = (resp && resp.backends) || (Array.isArray(resp) ? resp : []);
                for (var i = 0; i < list.length; i++) {
                    if (list[i] && list[i].id) backendConfigCache[list[i].id] = list[i];
                }
            } catch (e) {
                // Конфиг недоступен — не ломаем страницу: применяем кэш ниже.
                console.warn('[fetchClusterState] /api/v1/backends unavailable, ' +
                    'использую последнюю известную конфигурацию из кэша:', e && e.message);
            }
        }
        return applyBackendConfig(clusterBackends);
    }

    // Экспорт для app-listeners.js (WebSocket-тики приносят бэкенды БЕЗ конфига:
    // autoTune/weight/labels/maxModels — их надо добрать из кэша, иначе панель
    // теряет карточку AutoTune на каждом WS-тике).
    window.App = window.App || {};
    window.App.applyBackendConfig = applyBackendConfig;

    async function fetchClusterState() {
        try {
            const state = await Api.cluster();
            // R65d (2026-09-20): обогащаем бэкенды их КОНФИГУРАЦИЕЙ.
            //
            // GET /api/v1/cluster отдаёт []types.BackendMetrics — там нет
            // weight, labels, maxModels, autoTune, apiStyle, gpuMode.
            // renderers.js:203,839,841,899 и utils.js:37-40 эти поля читают,
            // поэтому колонки Weight/Labels/MaxModels всегда показывали
            // 1/-/-, карточка AutoTune не рисовалась, а детект cloud-режима по
            // labels не работал.
            //
            // GET /api/v1/backends отдаёт тип types.Backend + метрики
            // (handlers_backends.go:189-214), то есть НАДМНОЖЕСТВО нужных полей.
            // Мержим их в state.backends, метрики из /cluster имеют приоритет.
            state.backends = await enrichBackendsWithConfig(state.backends || []);

            // R-Image: пункт навигации и страница «Image-бэкенды» существуют
            // только пока в кластере есть image_cpp-бэкенд. Синхронизируем
            // видимость на КАЖДОМ опросе /api/v1/cluster (а не только при входе
            // на страницу «Бэкенды»), иначе вкладка не появлялась бы после
            // регистрации image-воркера и не исчезала бы после удаления.
            if (window.ImageBackendsPage) {
                window.ImageBackendsPage.syncVisibility(state.backends);
                // Пока страница открыта, обновляем и таблицу: periodic refresh
                // перерисовывает только settings, поэтому счётчики запросов и RPS
                // воркера замирали бы до ручного «Обновить».
                if (getCurrentPage() === 'image') {
                    window.ImageBackendsPage.render(state.backends);
                    if (window.ImageModelsPage) window.ImageModelsPage.render(state.backends);
                }
            }

            // R59.4 (2026-09-03): updateBackends moved to app-listeners.js in R57.3;
            // call via App.* instead of local ref (which was left behind by the
            // extract and silently produced ReferenceError on every refresh).
            if (App.updateBackends) App.updateBackends(state.backends);
            // Successful REST request — balancer is reachable
            updateConnectionStatus(true);
            // Синхронизация типа бэкенда (Ollama vs llama.cpp) — скрывает/показывает вкладку GGUF и режимы
            if (window.BackendTypeFilter) {
                BackendTypeFilter.syncFromClusterState(state);
            }
            // Бейдж уже обновлён внутри BackendTypeFilter.syncFromClusterState → updateUI → updateEngineBadge
            //
            // R84 (2026-10-03): свежесть данных отмечаем ЗДЕСЬ, а не в каждом
            // вызывающем: fetchClusterState дёргают и опрос, и кнопка, и переход
            // по страницам. Иначе индикатор в шапке показывал «нет данных» до
            // первого тика таймера, хотя данные уже пришли.
            if (window.DataRefresh) window.DataRefresh.markFresh();
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_cluster') : 'Error loading cluster state');
            if (window.DataRefresh) window.DataRefresh.markError(e);
        }
    }

    async function fetchQueue() {
        try {
            data.queue = await Api.queueStats();
            // R91 (2026-10-08): реальная очередь — admission (ожидание слота), а
            // не легаси-счётчики, которые в этом режиме всегда нули. Иначе на
            // дашборде «В очереди 0» при пяти ожидающих запросах.
            const qv = (window.Renderers && typeof Renderers.queueView === 'function')
                ? Renderers.queueView(data.queue)
                : { waiting: data.queue.current_size || 0, processed: data.queue.processed_total || 0 };
            Utils.setText('queueSize', qv.waiting);
            Utils.setText('dashboardQueueProcessed', qv.processed + ' ' + (window.I18N ? I18N.t('app.processed') : 'processed'));
            if (getCurrentPage() === 'queue') refreshPage('queue');
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_queue') : 'Error loading queue statistics');
        }
    }

    async function fetchQueueDetails() {
        try {
            const res = await Api.queueDetails();
            const all = res.all || [];
            data.queueTasks = all.map((item, idx) => ({
                id: idx + 1,
                model: item.model || '-',
                backend: item.target || 'Auto',
                status: item.status || (item.target ? 'processing' : 'pending'),
                waitTimeMs: item.enqueued ? (Date.now() - new Date(item.enqueued).getTime()) : 0,
                enqueued: item.enqueued
            }));
            if (getCurrentPage() === 'queue') refreshPage('queue');
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_queue_details') : 'Error loading queue details');
            data.queueTasks = [];
            if (getCurrentPage() === 'queue') refreshPage('queue');
        }
    }

    async function fetchQueueHistory() {
        try {
            const res = await Api.queueHistory();
            data.queueHistory = res.history || [];
            if (getCurrentPage() === 'queue') refreshPage('queue');
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_queue_history') : 'Error loading queue history');
            data.queueHistory = [];
            if (getCurrentPage() === 'queue') refreshPage('queue');
        }
    }

    async function fetchSessions() {
        try {
            const res = await Api.sessions();
            data.sessions = res.sessions || [];
            const activeCount = data.sessions.length;
            const totalRequests = data.sessions.reduce((sum, s) => sum + (s.requestCount || 0), 0);
            Utils.setText('totalSessions', activeCount);
            Utils.setText('sessionRate', totalRequests + ' ' + (window.I18N ? I18N.t('app.requests') : 'requests'));
            if (getCurrentPage() === 'sessions') refreshPage('sessions');
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_sessions') : 'Error loading sessions');
        }
    }

    /**
     * Сброс настроек к заводским значениям с сохранением типа бэкенда (Ollama / llama.cpp).
     * Показывает модальное окно подтверждения, при успехе перезагружает страницу.
     */
    function resetSettings() {
        // Показываем модальное окно подтверждения
        var confirmed = confirm(
            (window.I18N ? I18N.t('settings.reset_confirm') : 'Сбросить все настройки к заводским значениям? Это действие нельзя отменить.')
        );
        if (!confirmed) return;

        Api.resetConfig().then(function (result) {
            showToast(
                (window.I18N ? I18N.t('settings.reset_success') : 'Настройки сброшены. Страница будет перезагружена.'),
                'success'
            );
            // Сбрасываем локальные ключи, связанные с типом бэкенда и wizard
            localStorage.removeItem('ollamalegion_backend_type');
            localStorage.removeItem('ollamalegion_wizard_done');
            // Перезагрузка через небольшую задержку, чтобы пользователь увидел toast
            setTimeout(function () {
                location.reload();
            }, 1500);
        }).catch(function (err) {
            showToast(
                (window.I18N ? I18N.t('settings.reset_error') : 'Ошибка сброса: ') + (err.message || err),
                'error'
            );
        });
    }

    function loadSettings() {
        // R700: порог диска — кнопка «по умолчанию» и живая подсказка.
        // Навешиваем до запроса: если сервер недоступен, поле всё равно должно
        // объяснять, что измеряется.
        bindDiskGuardUI();
        refreshDiskGuardHint();
        // --- SERVER-FIRST RULE: сервер = источник истины ---
        // Сначала пытаемся загрузить с сервера. Если не получилось — fallback в localStorage.
        Api.config().then(function (serverConfig) {
            applyServerConfig(serverConfig);
            // Запоминаем время последней синхронизации
            localStorage.setItem('ollamalegion_last_sync', Date.now().toString());
        }).catch(function (err) {
            console.warn('Server config unavailable, falling back to localStorage', err);
            // Fallback: загружаем из localStorage
            var saved = localStorage.getItem('ollamalegion_config');
            if (saved) {
                try {
                    var config = JSON.parse(saved);
                    applyLocalConfig(config);
                } catch (e) {
                    console.error('Failed to parse localStorage config', e);
                }
            }
        });
    }

    function applyServerConfig(serverConfig) {
        if (!serverConfig) return;

        // ЗАЩИТА: не перезаписываем UI если wizard активен
        if (document.getElementById('firstRunCard')) return;

        // R513 (2026-11-19): НЕ ПЕРЕТИРАТЬ ФОРМУ, КОГДА В НЕЙ РАБОТАЮТ.
        //
        // Живая жалоба оператора: «обновление держит новую информацию, но не даёт
        // нормально взаимодействовать с формами — постоянно их скрывая».
        // R512 закрыл только таймер и только фокус (тик не запускался, пока
        // курсор в поле), но оставались три обходных пути: кнопка «Обновить» в
        // шапке, клик по индикатору свежести (DataRefresh → App.refreshCurrentPage)
        // и сам refreshPage('settings'), который на каждом тике заново заливал
        // ~40 полей из ответа сервера. Здесь — последний рубеж: пока оператор
        // вводит хоть что-то в формы страницы «Настройки», серверные значения в
        // НИХ не заливаются вовсе. Через один тик после ухода из поля (5 с) форма
        // догоняет сервер; ручной «Обновить» не блокируется — он просто не
        // трогает то, что оператор набирает.
        //
        // Почему страница целиком, а не «поле в поле»: заливка идёт десятками
        // пар «getElementById → value=» по всей функции, и пропуск только
        // активного элемента не спасает от соседнего эффекта — `syncModeFromServer`
        // ниже прячет целые группы полей варианта (settings-ui.js:186-221),
        // из-за чего форма и выглядит «исчезнувшей».
        if (window.RefreshGuard && typeof window.RefreshGuard.editingInId === 'function' &&
            window.RefreshGuard.editingInId('settings-page')) {
            return;
        }
        // R676 (2026-11-21): ЗАСЛОН ВЫШЕ ОСТАЛСЯ, НО ОН БОЛЬШЕ НЕ ЕДИНСТВЕННЫЙ.
        //
        // Он нужен ровно для того, о чём написано в его комментарии: остановить
        // побочные эффекты заливки ЦЕЛИКОМ, пока оператор печатает (скрытие групп
        // полей режима, пересчёт аккордеонов). Значения полей теперь защищает
        // SafeForm ПО КАЖДОМУ ПОЛЮ (writeValue/writeChecked ниже): если поле
        // менял оператор или на странице есть свежая несохранённая правка —
        // значение не заливается, а остальное обновляется. Именно этого не
        // хватало: после ухода из поля (фокус ушёл) тик перезаливал форму.

        writeChecked(document.getElementById('modelAffinity'), serverConfig.modelAffinity !== false);
        writeChecked(document.getElementById('sessionStickiness'), serverConfig.sessionStickiness !== false);
        writeChecked(document.getElementById('useEnhancedScoring'), serverConfig.useEnhancedScoring !== false);
        writeChecked(document.getElementById('predictionFiltering'), serverConfig.predictionFiltering !== false);

        if (serverConfig.gpuMaxUsage) writeValue(document.getElementById('gpuMaxUsage'), serverConfig.gpuMaxUsage);
        if (serverConfig.vramMaxUsage) writeValue(document.getElementById('vramMaxUsage'), serverConfig.vramMaxUsage);
        if (serverConfig.cpuMaxUsage) writeValue(document.getElementById('cpuMaxUsage'), serverConfig.cpuMaxUsage);
        if (serverConfig.ramMaxUsage) writeValue(document.getElementById('ramMaxUsage'), serverConfig.ramMaxUsage);
        if (serverConfig.minFreeDisk) writeValue(document.getElementById('minFreeDisk'), serverConfig.minFreeDisk);
        // R700: границы порога — с сервера, подсказка («что измеряется + сколько
        // сейчас») — сразу после заливки значения.
        updateDiskGuardBounds(serverConfig);
        refreshDiskGuardHint();
        // R544 (F10/W6): поля #apiToken в разметке больше нет — оно было инертным
        // (сервер не отдаёт apiToken в обычном GET и не принимает в PUT). Токен
        // подставляется сервером в WEBUI_CONFIG.API_TOKEN, см. index.html
        // «section-security».

        // Backend Engine — серверный конфиг как fallback.
        // Приоритет: localStorage (выбор пользователя) > серверный config.
        if (serverConfig.backendEngine) {
            // R112 (2026-10-11): единое преобразование «серверный движок → UI-тип»
            // живёт в BackendTypeFilter._engineToType; 'auto' — это UI-тип «Все»
            // («тип не навязан, движок по составу кластера»). Раньше здесь
            // считалось, что «всё, что не llama_cpp, — это Ollama», и смешанный
            // стенд показывал «🦙 Ollama API».
            var engineType = window.BackendTypeFilter
                ? BackendTypeFilter._engineToType(serverConfig.backendEngine)
                : ((serverConfig.backendEngine === 'llama_cpp') ? 'llama_cpp' : 'ollama');
            var currentType = window.BackendTypeFilter ? BackendTypeFilter.getCurrentType() : null;
            // Если пользователь уже выбрал тип — не перезаписываем UI из сервера
            if (currentType !== 'llama_cpp' && currentType !== 'ollama' && currentType !== 'image_cpp') {
                if (window.BackendTypeFilter) {
                    BackendTypeFilter.updateUI(engineType);
                }
            }
            var formBackendTypeEl = document.getElementById('formBackendType');
            if (formBackendTypeEl && engineType !== 'all') {
                writeValue(formBackendTypeEl, engineType);
            }
            // Маркер типа движка в сайдбаре рисует ТОЛЬКО BackendTypeFilter:
            // он знает фактический состав кластера (clusterComposition) и
            // показывает «llama.cpp + Ollama». Вторая копия этой логики здесь
            // уже расходилась с модулем фильтра (см. updateBackendEngineBadge).
            if (window.BackendTypeFilter && BackendTypeFilter.updateEngineBadge) {
                BackendTypeFilter.updateEngineBadge(engineType, false);
            }
        }

        // Operating Mode — критически важно для корректного отображения UI.
        // Сервер = единственный источник истины. Перезаписываем UI жёстко.
        if (serverConfig.operatingMode) {
            if (window.SettingsUI && SettingsUI.syncModeFromServer) {
            } else {
                // Fallback (устаревший путь)
                var radio = document.querySelector('input[name="operatingMode"][value="' + serverConfig.operatingMode + '"]');
                if (radio) writeChecked(radio, true);
            }
        }

        // RPC Settings — Model Replication (Вариант A)
        if (serverConfig.modelReplication) {
            writeChecked(document.getElementById('modelReplicationEnabled'), serverConfig.modelReplication.enabled === true);
            if (serverConfig.modelReplication.defaultMinInstances) writeValue(document.getElementById('modelReplicationMinInstances'), serverConfig.modelReplication.defaultMinInstances);
            if (serverConfig.modelReplication.defaultMaxInstances) writeValue(document.getElementById('modelReplicationMaxInstances'), serverConfig.modelReplication.defaultMaxInstances);
            if (serverConfig.modelReplication.idleUnloadAfter) writeValue(document.getElementById('modelReplicationIdleUnload'), serverConfig.modelReplication.idleUnloadAfter);
        }

        // RPC Settings — RPC Coordinator (Вариант B)
        if (serverConfig.rpcCoordinator) {
            writeChecked(document.getElementById('rpcCoordinatorEnabled'), serverConfig.rpcCoordinator.enabled === true);
            if (serverConfig.rpcCoordinator.coordinatorURL) writeValue(document.getElementById('rpcCoordinatorURL'), serverConfig.rpcCoordinator.coordinatorURL);
            if (serverConfig.rpcCoordinator.workerPort) writeValue(document.getElementById('rpcCoordinatorWorkerPort'), serverConfig.rpcCoordinator.workerPort);
            if (serverConfig.rpcCoordinator.protocol) writeValue(document.getElementById('rpcCoordinatorProtocol'), serverConfig.rpcCoordinator.protocol);
        }
        // R106c (2026-10-12): поле rpcCoordinatorTimeout убрано из формы.

        // RPC Settings — Virtual Models (Вариант C)
        if (serverConfig.virtualModels) {
            writeChecked(document.getElementById('virtualModelsEnabled'), serverConfig.virtualModels.enabled === true);
            if (serverConfig.virtualModels.coordMode) writeValue(document.getElementById('virtualModelsCoordMode'), serverConfig.virtualModels.coordMode);
        }
        // R106c (2026-10-12): поле virtualModelsTimeout убрано из формы.

        // RPC Settings — Distributed Inference (Вариант D)
        if (serverConfig.distInference) {
            writeChecked(document.getElementById('distInferenceEnabled'), serverConfig.distInference.enabled === true);
            if (serverConfig.distInference.grpcPort) writeValue(document.getElementById('distInferenceGrpcPort'), serverConfig.distInference.grpcPort);
        }

        // llama.cpp / GGUF Settings
        var llamaCpp = serverConfig.llamaCpp || {};
        // Хелпер для безопасной установки value (поддерживает "0" как валидное значение).
        // R676: обе записи идут через writeValue/writeChecked (SafeForm) — см. шапку.
        var setVal = function (id, val, fallback) {
            var el = document.getElementById(id);
            if (!el) return;
            if (val === undefined || val === null) {
                if (fallback !== undefined) writeValue(el, fallback);
                return;
            }
            writeValue(el, val);
        };
        var setCheck = function (id, val, fallback) {
            var el = document.getElementById(id);
            if (!el) return;
            if (val === undefined || val === null) {
                if (fallback !== undefined) writeChecked(el, !!fallback);
                return;
            }
            writeChecked(el, !!val);
        };
        // Базовые параметры
        setVal('gpuLayers', llamaCpp.numGpuLayers, -1);
        setVal('ctxSize', llamaCpp.contextLength, 2048);
        setVal('batchSize', llamaCpp.batchSize, 512);
        setVal('flashAttnType', llamaCpp.flashAttnType, -1);
        setVal('nThreads', llamaCpp.nThreads, 0);
        setCheck('flashAttn', llamaCpp.flashAttention, false);
        setCheck('numa', llamaCpp.numa, false);
        setCheck('useMmap', llamaCpp.useMmap, true);
        setCheck('useMlock', llamaCpp.useMlock, false);
        // Multi-GPU
        setVal('gpuStrategy', llamaCpp.strategy, 'vram-ratio');
        if (llamaCpp.tensorSplitStr) {
            setVal('tensorSplit', llamaCpp.tensorSplitStr, '');
        } else if (llamaCpp.tensorSplit && Array.isArray(llamaCpp.tensorSplit)) {
            setVal('tensorSplit', llamaCpp.tensorSplit.join(','), '');
        } else {
            setVal('tensorSplit', '', '');
        }
        setVal('splitMode', llamaCpp.splitMode, -1);
        setVal('mainGpu', llamaCpp.mainGpu, 0);
        setVal('rpcBackend', llamaCpp.rpcBackend, '');
        setCheck('autoGpuDistribution', llamaCpp.autoGpuDistribution, true);
        setCheck('noMemoryMap', llamaCpp.noMemoryMap, false);
        // KV cache
        setVal('kvCacheType', llamaCpp.kvCacheType, '');
        setCheck('noKvOffload', llamaCpp.noKvOffload, false);
        // RoPE/YaRN
        setVal('ropeFreqBase', llamaCpp.ropeFreqBase, 10000);
        setVal('ropeFreqScale', llamaCpp.ropeFreqScale, 1.0);
        setVal('ropeScalingType', llamaCpp.ropeScalingType, 'none');
        setVal('ropeScalingFactor', llamaCpp.ropeScalingFactor, 1.0);
        setVal('yarnExtFactor', llamaCpp.yarnExtFactor, 1.0);
        setVal('yarnAttnFactor', llamaCpp.yarnAttnFactor, 1.0);
        setVal('yarnBetaFast', llamaCpp.yarnBetaFast, 32.0);
        setVal('yarnBetaSlow', llamaCpp.yarnBetaSlow, 1.0);
        // Performance
        setVal('rmsNormEps', llamaCpp.rmsNormEps, 0.00001);
        setVal('idleUnloadMinutes', llamaCpp.idleUnloadMinutes, 0);
        setCheck('enableMetrics', llamaCpp.enableMetrics, true);
        setVal('metricsRetention', llamaCpp.metricsRetentionSeconds, 3600);
        // Session 18: Reasoning/Thinking
        setCheck('enableReasoning', llamaCpp.enableReasoning, false);
        setVal('reasoningBudget', llamaCpp.reasoningBudget, 0);
        // Session 17: показать/скрыть panel "Runtime overrides active" в зависимости
        // от наличия sidecar-файла. Делаем ПОСЛЕ setVal чтобы DOM был готов.
        updateLlamaCppOverridesPanel();
        // Round 35c+ (2026-08-13): обновляем gpu_layers live badge после setVal.
        // Без этого badge показывает "AUTO" (HTML default) даже если value=-1
        // или custom число, что вводит пользователя в заблуждение.
        if (Utils && Utils.updateGpuLayersBadge) {
            Utils.updateGpuLayersBadge('gpuLayers', 'gpuLayersBadge');
        }
    }

    /**
     * Session 17 (2026-07-27): проверяет, есть ли активный sidecar override
     * для llamaCpp-секции, и показывает/скрывает panel с кнопкой "Reset".
     *
     * Вызывается:
     *   - После applyServerConfig (когда UI подгружен значениями)
     *   - После saveSettings (после PUT, чтобы UI обновился)
     *   - После resetLlamaCppOverride (чтобы скрыть panel)
     */
    function updateLlamaCppOverridesPanel() {
        if (!window.Api || !window.Api.getLlamaCppOverride) return;
        const panel = document.getElementById('llamaCppRuntimeOverridesPanel');
        if (!panel) return;
        window.Api.getLlamaCppOverride()
            .then(function (resp) {
                if (resp && resp.exists) {
                    panel.style.display = '';
                } else {
                    panel.style.display = 'none';
                }
            })
            .catch(function () {
                // Store отключён (например, dev-режим) или ошибка сети — скрываем.
                panel.style.display = 'none';
            });
    }

    /**
     * Session 17: handler кнопки "Reset to bundled defaults".
     * Стирает sidecar override и перезагружает UI с base значениями из config.json.
     */
    function resetLlamaCppOverride() {
        if (!window.Api || !window.Api.deleteLlamaCppOverride) {
            showToast('Reset API not available', 'error');
            return;
        }
        const confirmMsg = window.I18N
            ? I18N.t('gguf.overrides_confirm_reset', 'Reset runtime overrides? llamaCpp will revert to bundled defaults from config.json.')
            : 'Reset runtime overrides? llamaCpp will revert to bundled defaults from config.json.';
        if (!confirm(confirmMsg)) return;
        const btn = document.getElementById('llamaCppResetBtn');
        if (btn) btn.disabled = true;
        window.Api.deleteLlamaCppOverride()
            .then(function (resp) {
                const successMsg = window.I18N
                    ? I18N.t('gguf.overrides_reset_done', 'Runtime overrides cleared. llamaCpp reverted to bundled defaults.')
                    : 'Runtime overrides cleared. llamaCpp reverted to bundled defaults.';
                showToast(successMsg, 'success');
                // Перезагружаем UI с сервера (in-memory уже восстановлен).
                return Api.config().then(applyServerConfig);
            })
            .catch(function (err) {
                const errMsg = (window.I18N ? I18N.t('common.error') : 'Error') + ': ' +
                    (err.message || err);
                showToast(errMsg, 'error');
            })
            .then(function () {
                if (btn) btn.disabled = false;
            });
    }

    function applyLocalConfig(config) {
        if (!config) return;
        // R676: заливка локального конфига — та же опасная запись, что и
        // серверная (это fallback, когда сервер недоступен). Идёт через SafeForm:
        // набранное оператором не затирается, остальное применяется.
        writeChecked(document.getElementById('modelAffinity'), config.modelAffinity !== false);
        writeChecked(document.getElementById('sessionStickiness'), config.sessionStickiness !== false);
        writeChecked(document.getElementById('predictionFiltering'), config.predictionFiltering !== false);
        if (config.gpuMaxUsage) writeValue(document.getElementById('gpuMaxUsage'), config.gpuMaxUsage);
        if (config.vramMaxUsage) writeValue(document.getElementById('vramMaxUsage'), config.vramMaxUsage);
        if (config.cpuMaxUsage) writeValue(document.getElementById('cpuMaxUsage'), config.cpuMaxUsage);
        if (config.ramMaxUsage) writeValue(document.getElementById('ramMaxUsage'), config.ramMaxUsage);
        if (config.minFreeDisk) writeValue(document.getElementById('minFreeDisk'), config.minFreeDisk);
        // R700: локальный fallback тоже обновляет подсказку — иначе при
        // недоступном сервере рядом с порогом остался бы пустой блок.
        refreshDiskGuardHint();
        // R544 (F10/W6): #apiToken и четырёх полей агента в разметке больше нет
        // (см. index.html section-security / section-agent). Заполнять их было
        // нечем и незачем: saveSettings эти значения не читал, а сервер их не
        // отдавал — поля выглядели настройкой, которой не существует.

        // Operating Mode
        // R563 (W1): блок выбора режима удалён. Секция #section-mode снесена (R562),
        // радио name="operatingMode" в разметке больше нет — обновлять было нечего,
        // а SettingsUI.updateModeCards/showModeFields остались без потребителя.

        // R544 (F10/W6): блок `if (config.agent)` с заполнением четырёх полей
        // агента удалён вместе с самими полями (§4.8 аудита §S-2: значения не
        // сохранялись, сервер их не отдавал — поля были декорацией). R106c ранее
        // так же сняло «Agent Timeout».
    }

    function saveSettings(silent) {
        var useEnhancedScoring = (document.getElementById('useEnhancedScoring') && document.getElementById('useEnhancedScoring').checked) !== false;
        var modelAffinity = (document.getElementById('modelAffinity') && document.getElementById('modelAffinity').checked) !== false;
        var sessionStickiness = (document.getElementById('sessionStickiness') && document.getElementById('sessionStickiness').checked) !== false;
        var predictionFiltering = (document.getElementById('predictionFiltering') && document.getElementById('predictionFiltering').checked) !== false;
        var gpuMax = parseInt((document.getElementById('gpuMaxUsage') && document.getElementById('gpuMaxUsage').value)) || 90;
        var vramMax = parseInt((document.getElementById('vramMaxUsage') && document.getElementById('vramMaxUsage').value)) || 85;
        var cpuMax = parseInt((document.getElementById('cpuMaxUsage') && document.getElementById('cpuMaxUsage').value)) || 80;
        var ramMax = parseInt((document.getElementById('ramMaxUsage') && document.getElementById('ramMaxUsage').value)) || 85;
        var minDisk = MIN_FREE_DISK_DEFAULT;
        // R700: порог диска читается с ЯВНОЙ валидацией.
        //
        // БЫЛО: `parseInt(el.value) || 10240`. Пустое поле, «12.5» и «-1»
        // превращались в дефолт или (для отрицательного) в мусор на сервере, а
        // оператор видел «Settings saved» — то есть правка «не применилась»
        // без единого сообщения. Теперь недопустимое значение не уходит на
        // сервер: строка ошибки рядом с полем и никакого PUT.
        var minDiskEl = document.getElementById('minFreeDisk');
        if (minDiskEl) {
            var minDiskRaw = String(minDiskEl.value == null ? '' : minDiskEl.value).trim();
            var minDiskNum = Number(minDiskRaw);
            var minDiskBad = minDiskRaw === '' || !Number.isFinite(minDiskNum) ||
                Math.floor(minDiskNum) !== minDiskNum ||
                minDiskNum < 0 || minDiskNum > MIN_FREE_DISK_MAX;
            if (minDiskBad) {
                showDiskGuardError(i18nT('settings.min_free_disk.invalid', { min: 0, max: MIN_FREE_DISK_MAX },
                    'Min Free Disk: нужно целое число МБ от 0 до ' + MIN_FREE_DISK_MAX + '.'));
                if (!silent) showToast(i18nT('settings.min_free_disk.invalid_short', {},
                    'Порог диска не сохранён: недопустимое значение.'), 'error');
                return;
            }
            clearDiskGuardError();
            minDisk = minDiskNum;
        }
        // R65d: apiToken больше не читается и не отправляется — сервер его не
        // принимает (нет поля в PUT-структуре). Смена API-токена на лету
        // заблокировала бы текущего оператора, поэтому токены задаются в
        // конфиге/окружении балансера.

        // RPC settings
        //
        // R66b (2026-09-22): чекбоксов #modelReplicationEnabled /
        // #rpcCoordinatorEnabled / #virtualModelsEnabled / #distInferenceEnabled
        // в разметке НЕТ (проверено по всем webui/*.html). Режим работы в UI
        // выбирается radio-кнопками input[name="operatingMode"], а секции
        // вариантов показываются/скрываются через .variant-section.
        // До фикса здесь читались несуществующие чекбоксы → в PUT всегда уходило
        // enabled:false для всех четырёх режимов, т.е. нажатие «Сохранить»
        // выключало активный режим работы (а после включения обработчиков это
        // стало бы реальным регрессом). Теперь enabled выводится из текущего
        // operatingMode — единственного источника истины в UI; если чекбокс
        // когда-нибудь вернут в разметку, приоритет отдаётся ему.
        var currentOperatingMode = (function () {
            var radio = document.querySelector('input[name="operatingMode"]:checked');
            return radio ? radio.value : 'standard';
        })();
        var modeEnabled = function (id, modeName) {
            var el = document.getElementById(id);
            if (el) return el.checked === true;
            return currentOperatingMode === modeName;
        };
        var modelReplicationEnabled = modeEnabled('modelReplicationEnabled', 'replication');
        var modelReplicationMinInstances = parseInt((document.getElementById('modelReplicationMinInstances') && document.getElementById('modelReplicationMinInstances').value)) || 1;
        var modelReplicationMaxInstances = parseInt((document.getElementById('modelReplicationMaxInstances') && document.getElementById('modelReplicationMaxInstances').value)) || 3;
        var modelReplicationIdleUnload = (document.getElementById('modelReplicationIdleUnload') && document.getElementById('modelReplicationIdleUnload').value) || '10m';

        var rpcCoordinatorEnabled = modeEnabled('rpcCoordinatorEnabled', 'rpc_coordinator');
        var rpcCoordinatorURL = (document.getElementById('rpcCoordinatorURL') && document.getElementById('rpcCoordinatorURL').value) || '';
        var rpcCoordinatorWorkerPort = parseInt((document.getElementById('rpcCoordinatorWorkerPort') && document.getElementById('rpcCoordinatorWorkerPort').value)) || 18050;
        var rpcCoordinatorProtocol = (document.getElementById('rpcCoordinatorProtocol') && document.getElementById('rpcCoordinatorProtocol').value) || 'http';
        // R106c: rpcCoordinatorTimeout из формы убран — серверное значение сохраняется.

        var virtualModelsEnabled = modeEnabled('virtualModelsEnabled', 'virtual_router');
        var virtualModelsCoordMode = (document.getElementById('virtualModelsCoordMode') && document.getElementById('virtualModelsCoordMode').value) || 'sequential';
        // R106c: virtualModelsTimeout из формы убран — серверное значение сохраняется.

        var distInferenceEnabled = modeEnabled('distInferenceEnabled', 'distributed_inference');
        var distInferenceGrpcPort = parseInt((document.getElementById('distInferenceGrpcPort') && document.getElementById('distInferenceGrpcPort').value)) || 19000;

        // R65d (2026-09-20): здесь раньше читались agentCollectInterval /
        // agentHeartbeatInterval / agentMaxConcurrent / agentMaxModels /
        // agentTimeout и отправлялись в секции `agent`. Секция убрана из
        // payload: эти значения описывают конфигурацию ОТДЕЛЬНОГО процесса
        // cmd/agent, и применить их через PUT /api/v1/cluster/config
        // невозможно (у types.LoadBalancerConfig нет поля Agent). Задаются в
        // окружении agent-контейнера.

        // llama.cpp / GGUF settings (имена полей соответствуют Go-структуре LlamaCppConfig)
        // Хелпер: безопасный int (0 = валидное значение, NaN → fallback).
        var intVal = function (id, fallback) {
            var el = document.getElementById(id);
            if (!el) return fallback;
            var n = parseInt(el.value, 10);
            return Number.isFinite(n) ? n : fallback;
        };
        var floatVal = function (id, fallback) {
            var el = document.getElementById(id);
            if (!el) return fallback;
            var n = parseFloat(el.value);
            return Number.isFinite(n) ? n : fallback;
        };
        var checkVal = function (id, fallback) {
            var el = document.getElementById(id);
            if (!el) return fallback;
            return el.checked;
        };
        var strVal = function (id, fallback) {
            var el = document.getElementById(id);
            if (!el) return fallback;
            return el.value;
        };
        // Базовые
        var llamaCppGpuLayers = intVal('gpuLayers', -1);
        var llamaCppCtxSize = intVal('ctxSize', 2048);
        var llamaCppBatchSize = intVal('batchSize', 512);
        var llamaCppFlashAttnType = intVal('flashAttnType', -1);
        var llamaCppNThreads = intVal('nThreads', 0);
        var llamaCppFlashAttn = checkVal('flashAttn', false);
        var llamaCppNuma = checkVal('numa', false);
        var llamaCppUseMmap = checkVal('useMmap', true);
        var llamaCppUseMlock = checkVal('useMlock', false);
        // Multi-GPU
        var llamaCppStrategy = strVal('gpuStrategy', 'vram-ratio');
        var llamaCppTensorSplitStr = strVal('tensorSplit', '');
        var llamaCppSplitMode = intVal('splitMode', -1);
        var llamaCppMainGpu = intVal('mainGpu', 0);
        var llamaCppRpcBackend = strVal('rpcBackend', '');
        var llamaCppAutoGpu = checkVal('autoGpuDistribution', true);
        var llamaCppNoMemoryMap = checkVal('noMemoryMap', false);
        // KV cache
        var llamaCppKvCacheType = strVal('kvCacheType', '');
        var llamaCppNoKvOffload = checkVal('noKvOffload', false);
        // RoPE/YaRN
        var llamaCppRopeFreqBase = floatVal('ropeFreqBase', 10000.0);
        var llamaCppRopeFreqScale = floatVal('ropeFreqScale', 1.0);
        var llamaCppRopeScalingType = strVal('ropeScalingType', 'none');
        var llamaCppRopeScalingFactor = floatVal('ropeScalingFactor', 1.0);
        var llamaCppYarnExtFactor = floatVal('yarnExtFactor', 1.0);
        var llamaCppYarnAttnFactor = floatVal('yarnAttnFactor', 1.0);
        var llamaCppYarnBetaFast = floatVal('yarnBetaFast', 32.0);
        var llamaCppYarnBetaSlow = floatVal('yarnBetaSlow', 1.0);
        // Performance
        var llamaCppRmsNormEps = floatVal('rmsNormEps', 0.00001);
        var llamaCppIdleUnloadMinutes = intVal('idleUnloadMinutes', 0);
        var llamaCppEnableMetrics = checkVal('enableMetrics', true);
        var llamaCppMetricsRetention = intVal('metricsRetention', 3600);
        // Session 18 (2026-07-28): Reasoning/Thinking (gemma-4, deepseek-r1, qwen3-thinking)
        var llamaCppEnableReasoning = checkVal('enableReasoning', false);
        var llamaCppReasoningBudget = intVal('reasoningBudget', 0);

        // Определяем текущий operatingMode из radio-кнопок на странице
        // (R66b: значение уже посчитано выше как currentOperatingMode и
        // используется для вывода enabled-флагов RPC-режимов).

        // Определяем текущий backendEngine из BackendTypeFilter (источник истины после мастера)
        var backendEngine = null;
        if (window.BackendTypeFilter) {
            var currentType = BackendTypeFilter.getCurrentType();
            if (currentType === 'llama_cpp') {
                backendEngine = 'llama_cpp';
            } else if (currentType === 'ollama') {
                backendEngine = 'ollama_api';
            } else if (currentType === 'all') {
                // R112 (2026-10-11): «Все» = тип не навязан. Раньше в payload
                // уходило старое значение (или ничего), и сохранение настроек
                // могло вернуть стенд к одному движку. Сервер принимает auto
                // (см. clusterConfigHandler).
                backendEngine = 'auto';
            }
            // если тип не определён (null/undefined) — не включаем backendEngine в payload,
            // чтобы не перезаписать существующее значение на сервере
        }

        var config = {
            // R563 (W1): operatingMode в payload БОЛЬШЕ НЕ УХОДИТ: радио режима удалены (R562),
            // и значение стало константой 'standard' — сохранение настроек молча сбрасывало бы
            // серверный режим. Сервер применяет поле только если оно непустое, поэтому без него
            // текущее значение сохраняется (тот же приём, что в R543 для мастера).
            // R65d (2026-09-20): секция `agent` УБРАНА из payload.
            //
            // Она отправлялась (collectInterval/heartbeatInterval/
            // maxConcurrentRequests/maxModels/timeout), но применить её сервер
            // не может: types.AgentConfig описывает конфигурацию отдельного
            // процесса cmd/agent, а не балансера (у LoadBalancerConfig нет поля
            // Agent). Сервер молча игнорировал блок, verifySync откатывал форму —
            // оператор видел «Settings saved» при неизменившихся значениях.
            // Тайминги агента задаются в окружении agent-контейнера
            // (AGENT_COLLECT_INTERVAL и т.п.).
            useEnhancedScoring: useEnhancedScoring,
            modelAffinity: modelAffinity,
            sessionStickiness: sessionStickiness,
            predictionFiltering: predictionFiltering,
            gpuMaxUsage: gpuMax,
            vramMaxUsage: vramMax,
            cpuMaxUsage: cpuMax,
            ramMaxUsage: ramMax,
            minFreeDisk: minDisk,
            modelReplication: {
                enabled: modelReplicationEnabled,
                defaultMinInstances: modelReplicationMinInstances,
                defaultMaxInstances: modelReplicationMaxInstances,
                idleUnloadAfter: modelReplicationIdleUnload
            },
            rpcCoordinator: {
                enabled: rpcCoordinatorEnabled,
                coordinatorURL: rpcCoordinatorURL,
                workerPort: rpcCoordinatorWorkerPort,
                protocol: rpcCoordinatorProtocol
            },
            virtualModels: {
                enabled: virtualModelsEnabled,
                coordMode: virtualModelsCoordMode
            },
            distInference: {
                enabled: distInferenceEnabled,
                grpcPort: distInferenceGrpcPort
            },
            llamaCpp: {
                // Базовые
                numGpuLayers: llamaCppGpuLayers,
                contextLength: llamaCppCtxSize,
                batchSize: llamaCppBatchSize,
                flashAttnType: llamaCppFlashAttnType,
                nThreads: llamaCppNThreads,
                flashAttention: llamaCppFlashAttn,
                numa: llamaCppNuma,
                useMmap: llamaCppUseMmap,
                useMlock: llamaCppUseMlock,
                // Multi-GPU
                strategy: llamaCppStrategy,
                tensorSplitStr: llamaCppTensorSplitStr,
                splitMode: llamaCppSplitMode,
                mainGpu: llamaCppMainGpu,
                rpcBackend: llamaCppRpcBackend,
                autoGpuDistribution: llamaCppAutoGpu,
                noMemoryMap: llamaCppNoMemoryMap,
                // KV cache
                kvCacheType: llamaCppKvCacheType,
                noKvOffload: llamaCppNoKvOffload,
                // RoPE/YaRN
                ropeFreqBase: llamaCppRopeFreqBase,
                ropeFreqScale: llamaCppRopeFreqScale,
                ropeScalingType: llamaCppRopeScalingType,
                ropeScalingFactor: llamaCppRopeScalingFactor,
                yarnExtFactor: llamaCppYarnExtFactor,
                yarnAttnFactor: llamaCppYarnAttnFactor,
                yarnBetaFast: llamaCppYarnBetaFast,
                yarnBetaSlow: llamaCppYarnBetaSlow,
                // Performance / lifecycle
                rmsNormEps: llamaCppRmsNormEps,
                idleUnloadMinutes: llamaCppIdleUnloadMinutes,
                enableMetrics: llamaCppEnableMetrics,
                metricsRetentionSeconds: llamaCppMetricsRetention,
                // Session 18: Reasoning/Thinking
                enableReasoning: llamaCppEnableReasoning,
                reasoningBudget: llamaCppReasoningBudget
            }
        };

        // Добавляем backendEngine только если он определён
        if (backendEngine) {
            config.backendEngine = backendEngine;
        }

        // Конфигурация хранится на сервере, localStorage больше не используется для настроек
        // (только theme и language остаются в localStorage)
        Api.updateConfig(config).then(function () {
            return verifySync(config);
        }).then(function () {
            // R700: порог применился на сервере — подсказка должна сразу
            // показать новое сравнение (иначе рядом с полем висит старая
            // строка «узел исключён», хотя он уже проходит).
            refreshDiskGuardHint();
            if (!silent) showToast(window.I18N ? I18N.t('settings.saved') : 'Settings saved', 'success');
        }).catch(function (err) {
            if (!silent) showToast((window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (err.message || err), 'error');
        });
    }

    /**
     * SERVER-FIRST verify: после PUT делаем GET и сравниваем критические поля.
     * При расхождении перезагружаем UI из сервера (источник истины).
     *
     * R65d (2026-09-20): добавлена проверка СЕКЦИЙ (modelReplication,
     * rpcCoordinator, virtualModels, distInference) и полей llamaCpp, которые
     * раньше молча терялись. До этого verifySync сравнивал только algorithm /
     * operatingMode / useEnhancedScoring / backendEngine, поэтому когда PUT
     * игнорировал, например, modelReplication (поля не было в серверной
     * структуре), проверка проходила успешно, показывался тост «Settings saved»,
     * а applyServerConfig откатывал форму. Оператор видел успех при
     * неизменившейся настройке.
     *
     * Теперь такие расхождения возвращают ошибку → пользователь видит, что
     * настройка не применилась, а не «тихий» откат.
     */
    function verifySync(expectedConfig) {
        return Api.config().then(function (serverConfig) {
            // Всегда перезагружаем UI из сервера для гарантии синхронности
            applyServerConfig(serverConfig);

            var mismatches = [];
            if (expectedConfig.operatingMode && serverConfig.operatingMode !== expectedConfig.operatingMode) {
                mismatches.push('operatingMode: expected ' + expectedConfig.operatingMode + ', got ' + serverConfig.operatingMode);
            }
            if (expectedConfig.useEnhancedScoring !== undefined && serverConfig.useEnhancedScoring !== expectedConfig.useEnhancedScoring) {
                mismatches.push('useEnhancedScoring: expected ' + expectedConfig.useEnhancedScoring + ', got ' + serverConfig.useEnhancedScoring);
            }
            if (expectedConfig.backendEngine && serverConfig.backendEngine !== expectedConfig.backendEngine) {
                mismatches.push('backendEngine: expected ' + expectedConfig.backendEngine + ', got ' + serverConfig.backendEngine);
            }

            // Секции: сравниваем только те поля, которые форма реально
            // редактирует. groups/workers/models задаются отдельными
            // endpoint'ами и намеренно не участвуют.
            var sectionFields = {
                modelReplication: ['enabled', 'defaultMinInstances', 'defaultMaxInstances', 'idleUnloadAfter'],
                rpcCoordinator: ['enabled', 'coordinatorURL', 'workerPort', 'protocol', 'timeout'],
                virtualModels: ['enabled', 'coordMode', 'timeout'],
                distInference: ['enabled', 'grpcPort']
            };
            Object.keys(sectionFields).forEach(function (section) {
                var expected = expectedConfig[section];
                var actual = serverConfig[section];
                if (!expected) return;
                if (!actual) {
                    mismatches.push(section + ': сервер не вернул секцию');
                    return;
                }
                sectionFields[section].forEach(function (field) {
                    if (expected[field] === undefined) return;
                    if (actual[field] !== expected[field]) {
                        mismatches.push(section + '.' + field + ': expected ' + expected[field] + ', got ' + actual[field]);
                    }
                });
            });

            // llamaCpp: поля, которые WebUI редактирует и которые до R65d
            // отсутствовали в Go-структуре (молча игнорировались). Теперь они
            // есть в types.LlamaCppConfig, поэтому расхождение — реальная ошибка.
            if (expectedConfig.llamaCpp && serverConfig.llamaCpp) {
                ['numGpuLayers', 'contextLength', 'batchSize', 'kvCacheType',
                 'flashAttnType', 'splitMode', 'idleUnloadMinutes',
                 'enableMetrics', 'metricsRetentionSeconds',
                 'enableReasoning', 'reasoningBudget'].forEach(function (field) {
                    if (expectedConfig.llamaCpp[field] === undefined) return;
                    if (serverConfig.llamaCpp[field] !== expectedConfig.llamaCpp[field]) {
                        mismatches.push('llamaCpp.' + field + ': expected ' +
                            expectedConfig.llamaCpp[field] + ', got ' + serverConfig.llamaCpp[field]);
                    }
                });
            }

            if (mismatches.length > 0) {
                console.warn('[verifySync] Config mismatch detected:', mismatches);
                var msg = (window.I18N ? I18N.t('wizard.error') : 'Config sync failed');
                return Promise.reject(new Error(msg + ': ' + mismatches.join('; ')));
            }
        });
    }

    function autoSaveSettings() {
        if (autoSaveTimer) clearTimeout(autoSaveTimer);
        autoSaveTimer = setTimeout(function () {
            saveSettings(true);
        }, 800);
    }

        return {
            loadSettings: loadSettings,
            saveSettings: saveSettings,
            resetSettings: resetSettings,
            resetLlamaCppOverride: resetLlamaCppOverride,
            autoSaveSettings: autoSaveSettings,
            applyServerConfig: applyServerConfig,
            applyLocalConfig: applyLocalConfig,
            applyBackendConfig: applyBackendConfig,
            updateLlamaCppOverridesPanel: updateLlamaCppOverridesPanel,
            verifySync: verifySync,
            mergeBackendConfig: mergeBackendConfig,
            // R700: подсказка у порога диска (что измеряется + текущее значение).
            // Экспортируется для теста: проверяется поведение (текст с числами и
            // упоминанием измеряемой ФС), а не факт наличия функции в файле.
            refreshDiskGuardHint: refreshDiskGuardHint,
            // R671 (2026-11-21): пять сборщиков данных, ПОТЕРЯННЫХ при выносе в
            // фабрику (R652-R655). Они определены выше в этом же файле, но не
            // возвращались, а `app.js` продолжал звать их bare-именами — панель
            // падала на `ReferenceError: fetchClusterState is not defined` в первой
            // строке `ui.init()` (app.js:66) и не запускала НИЧЕГО: ни навигацию,
            // ни WebSocket, ни первый запрос данных. Отсюда «—», «Загрузка…» и
            // «Подключение…» у залогиненного оператора.
            fetchClusterState: fetchClusterState,
            fetchQueue: fetchQueue,
            fetchQueueDetails: fetchQueueDetails,
            fetchQueueHistory: fetchQueueHistory,
            fetchSessions: fetchSessions
        };
    };
})();
