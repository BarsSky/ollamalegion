/**
 * gguf-renderer-refresh.js — Data refresh + active queries polling.
 *
 * R57.5d-2 (2026-09-03): extracted from webui/js/modules/gguf-renderer.js.
 *
 * Содержит функции periodic refresh:
 *   - refreshBackends() — обновить список бэкендов
 *   - updateBackendsList() — DOM-only update левой панели
 *   - refreshDetail() — обновить выбранный бэкенд
 *   - startActiveQueriesPolling() / stopActiveQueriesPolling()
 *   - refreshActiveQueriesPolling() — forced refresh (cross-tab sync)
 *   - startDownloadPolling() / stopDownloadPolling()
 *   - refreshActiveDownloads() — обновить активные загрузки
 *   - updateDetailLoading() / refreshDetailPanel() / refreshDetailPane()
 *
 * Зависимости:
 *   - window.GgufModule.state, M.renderBackendsList, M.renderDetailPanel
 *   - window.GgufApi, window.Api, window.Utils
 *
 * Загружается ДО gguf-renderer.js (в defer-цепочке).
 */
(function() {
    'use strict';

    const M = (window.GgufModule = window.GgufModule || {});

    // ---- Disk-models fallback (R60.5) ----
    // Асинхронная догрузка списка .gguf файлов на диске через /api/models/files
    // (R60.3 endpoint). Используется когда state.localModels пуст (нет загруженных
    // моделей), чтобы вкладка «Модели» не выглядела пустой.
    let _diskFetchInFlight = new Set(); // debounce: backendId, чтобы не спамить
    function fetchDiskModelsAsync(backendId, state) {
        if (_diskFetchInFlight.has(backendId)) return;
        _diskFetchInFlight.add(backendId);
        const tick = (state && state._diskFetchTick) || 0;
        if (state) state._diskFetchTick = tick + 1;
        if (typeof window.GgufApi === 'undefined' || typeof window.GgufApi.listLocalModelsViaBackend !== 'function') {
            _diskFetchInFlight.delete(backendId);
            return;
        }
        window.GgufApi.listLocalModelsViaBackend(backendId)
            .then(function (filesBody) {
                // R60.3 /api/models/files: {count, dir, files: [{name, size, quantization, ...}]}
                const files = (filesBody && Array.isArray(filesBody.files)) ? filesBody.files : [];
                if (files.length === 0) {
                    // No disk files either — leave localModels as []
                    return;
                }
                // Normalize: webui renderModelsPane ожидает объекты с полями
                // {name, size, quantization, state}. У файлов state всегда
                // "available" (не загружен).
                const normalized = files.map(function (f) {
                    return {
                        name: f.name || '-',
                        size: typeof f.size === 'number' ? f.size : (f.sizeBytes || 0),
                        quantization: f.quantization || '',
                        state: 'available', // не загружена
                        path: f.path || '',
                    };
                });
                // Replace localModels на нормализованный список файлов.
                // Не мерджим с loadedModels — рендерер сам матчит isLoaded.
                if (state.localModels.length === 0 || state._diskFetchTick > tick) {
                    state.localModels = normalized;
                    // Force re-render detail pane.
                    if (typeof M.refreshDetailPanel === 'function') {
                        M.refreshDetailPanel(backendId);
                    }
                }
            })
            .catch(function (err) {
                console.warn('[gguf-renderer] fetchDiskModelsAsync failed for', backendId, err);
            })
            .finally(function () {
                _diskFetchInFlight.delete(backendId);
            });
    }

    // ---- Версия воркера -------------------------------------------------------
    //
    // В панели «Инфо» поле «Версия» показывало прочерк: state.workerInfo
    // синтезируется из /api/v1/backends/{id} (BackendMetrics), а там версии нет —
    // ни у cppworker, ни у ollama-бэкенда. Версию знает сам воркер: GET /health
    // отдаёт {"status":"ok","version":"0.2.0 (real llama.cpp linked)"}, и этот путь
    // доступен через прокси балансера для ЛЮБОГО зарегистрированного бэкенда,
    // включая удалённый.
    //
    // Результат кэшируется на 10 минут: версия меняется только при обновлении
    // образа, а refreshDetail вызывается часто (выбор бэкенда, поллинг загрузок).
    const WORKER_VERSION_TTL_MS = 10 * 60 * 1000;
    const _workerVersionInFlight = new Set();
    function fetchWorkerVersionAsync(backendId) {
        // R91 (2026-10-08): КРИТИЧНО. Здесь была ссылка на `state` без объявления.
        //
        // История: изначально в этом файле был module-scope `const state =
        // M.state;`, но в R65d (коммит 15ea588) его убрали, а функции перевели на
        // параметр `state`. `fetchWorkerVersionAsync` добавили ПОЗЖЕ (коммит
        // 196d625) в старом стиле — с голым `state` — и она падала с
        // `ReferenceError: state is not defined` при КАЖДОМ обновлении деталей:
        // вызов идёт из refreshDetail ПОСЛЕ заполнения workerInfo/gpuInfo, поэтому
        // исключение обрывало остаток .then — state.localModels, state.loadedModels
        // и state.backendRuntime не заполнялись, refreshDetailPanel не вызывался.
        //
        // Живая жалоба: «пропала вообще вся информация кроме инфо — какие модели,
        // какие настройки»: «Инфо» рисуется из workerInfo/gpuInfo (они успевали
        // заполниться), а вкладки «Модели»/«Загруженные»/«Настройки» — пустые,
        // потому что их данные так и не записывались.
        const state = M.state;
        if (!backendId) return;
        const cached = state.workerVersions && state.workerVersions[backendId];
        if (cached && (Date.now() - cached.at) < WORKER_VERSION_TTL_MS) {
            if (state.workerInfo) state.workerInfo.version = cached.version;
            return;
        }
        if (_workerVersionInFlight.has(backendId)) return;
        if (typeof window.GgufApi === 'undefined' || typeof window.GgufApi.requestViaBackend !== 'function') return;
        _workerVersionInFlight.add(backendId);
        window.GgufApi.requestViaBackend(backendId, '/health')
            .then(function (body) {
                // requestViaBackend возвращает объект для application/json и строку
                // иначе — разбираем обе формы, /health у воркеров отдаёт JSON.
                let parsed = body;
                if (typeof body === 'string') {
                    try { parsed = JSON.parse(body); } catch (e) { parsed = null; }
                }
                const version = parsed && (parsed.version || parsed.build || parsed.llamaVersion);
                if (!version) return;
                if (!state.workerVersions) state.workerVersions = {};
                state.workerVersions[backendId] = { version: String(version), at: Date.now() };
                // Показываем только если этот бэкенд всё ещё выбран.
                if (state.selectedBackendId === backendId && state.workerInfo) {
                    state.workerInfo.version = String(version);
                    if (typeof M.refreshDetailPanel === 'function') M.refreshDetailPanel(backendId);
                }
            })
            .catch(function () {
                // best-effort: /health может быть недоступен (старый воркер,
                // чужой бэкенд) — панель просто останется с прочерком.
            })
            .finally(function () {
                _workerVersionInFlight.delete(backendId);
            });
    }

    // ---- Data refresh ----

    let _refreshInProgress = false;
    M.refreshBackends = function() {
        const state = M.state;
        if (_refreshInProgress) return Promise.resolve();
        _refreshInProgress = true;
        // R59.7 (2026-09-03): gguf-renderer.js used to fall back to `window.GgufApi`
        // here, but `GgufApi` (gguf-api.js) only exposes a per-cppworker surface
        // (setUrl / loadModel / etc.) — it has no `listBackends()` method, and
        // the IIFE never assigned itself to `window.GgufApi` (same R59.3 / R59.6
        // pattern: `const X = (function(){...})()` без trailing `window.X = X`).
        // Result: `api.listBackends` was always undefined → `refreshBackends`
        // returned `Promise.resolve()` silently → `state.registeredBackends`
        // stayed empty → GGUF page rendered "no registered backends" even
        // though dashboard/monitor showed the same backend fine. The dashboard
        // works because it uses `Api.cluster()` which has the `window.Api =
        // Api` export (api.js:544).
        //
        // Fix: call `Api.fetchGgufBackends()` directly. The `/api/v1/gguf/backends`
        // endpoint is a public admin endpoint (proxied by nginx → balancer
        // admin) and already does the per-backend dedup that the GGUF page
        // wants to display.
        if (typeof window.Api === 'undefined' || typeof window.Api.fetchGgufBackends !== 'function') {
            _refreshInProgress = false;
            return Promise.resolve();
        }
        return window.Api.fetchGgufBackends()
            .then(function (resp) {
                // Endpoint returns { backends: [...] } per api.md §GGUF
                var backends = (resp && resp.backends) || resp || [];
                state.registeredBackends = Array.isArray(backends) ? backends : [];
                state.backendDataLoaded = true;
                if (typeof M.updateBackendsList === 'function') M.updateBackendsList();
            })
            .catch(function (err) {
                console.warn('refreshBackends failed:', err);
            })
            .finally(function () {
                // R91 (2026-10-08): снятие флага — в finally, а не в then/catch.
                // Иначе единственный незавершившийся промис (зависший fetch без
                // таймаута) навсегда блокирует обновление списка бэкендов.
                _refreshInProgress = false;
            });
    };

    M.updateBackendsList = function() {
        const list = document.getElementById('ggufBackendList');
        if (!list) return;
        if (typeof M.renderBackendsList === 'function') {
            list.innerHTML = M.renderBackendsList();
        }
    };

    let _detailRefreshInProgress = false;
    // R91 (2026-10-08): момент старта текущего обновления — для сторожевого
    // предела ниже.
    let _detailRefreshStartedAt = 0;
    // DETAIL_REFRESH_STALE_MS — через сколько миллисекунд незавершённое обновление
    // считается зависшим и НЕ блокирует новое.
    //
    // ЗАЧЕМ. Флаг _detailRefreshInProgress защищает от параллельных запросов, но
    // если промис не завершается (fetch без таймаута до неотвечающего балансера),
    // флаг остаётся взведённым НАВСЕГДА, и панель деталей перестаёт обновляться:
    // оператор выбирает другой бэкенд, вкладки «Модели»/«Настройки» пустые, а
    // «Инфо» показывает последнее отрисованное. Живая жалоба 2026-10-08:
    // «с большой задержкой открываются настройки бэкенда, пропала вся информация
    // кроме инфо». Теперь у чтения есть таймаут (api.js: API_READ_TIMEOUT_MS), а
    // этот предел разблокирует панель даже если промис подвесил не fetch.
    const DETAIL_REFRESH_STALE_MS = 30000;
    M.refreshDetail = function() {
        const state = M.state;
        // R-MultiHost (2026-10-07): синхронизируем выбранный бэкенд с API-клиентом
        // ПЕРЕД любым действием с диском воркера. Выбор мог быть восстановлен не
        // через M.selectBackend (например, общим renderers.js), поэтому одной
        // синхронизации в обработчике клика недостаточно.
        if (typeof M._syncBackendToApi === 'function') M._syncBackendToApi(state.selectedBackendId);
        if (!state.selectedBackendId) return Promise.resolve();
        if (_detailRefreshInProgress) {
            const age = Date.now() - _detailRefreshStartedAt;
            if (age < DETAIL_REFRESH_STALE_MS) return Promise.resolve();
            console.warn('[gguf-renderer] предыдущее обновление деталей висит ' +
                Math.round(age / 1000) + ' с — считаю зависшим и запускаю новое');
        }
        _detailRefreshInProgress = true;
        _detailRefreshStartedAt = Date.now();
        var backend = (state.registeredBackends || []).find(function (b) { return b.id === state.selectedBackendId; });
        if (!backend) {
            _detailRefreshInProgress = false;
            return Promise.resolve();
        }
        // R59.8 (2026-09-03): call Api.getBackend directly. R57.5d-2 used
        // `window.GgufApi || window.Api` but (a) GgufApi has no getBackend
        // method (its surface is per-cppworker loadModel/unloadModel/etc.),
        // (b) the response shape it expected — {worker, gpu, localModels,
        // loadedModels, runtimeModels} — doesn't match what
        // /api/v1/backends/{id} actually returns. The endpoint
        // returns a full `BackendMetrics` with `gpu`, `system`,
        // `llamaCpp.loadedModels`, `models` (and a list of other fields);
        // we map those into the state shape the renderers expect.
        if (typeof window.Api === 'undefined' || typeof window.Api.getBackend !== 'function') {
            _detailRefreshInProgress = false;
            return Promise.resolve();
        }
        return window.Api.getBackend(backend.id)
            .then(function (data) {
                data = data || {};
                // R91 (2026-10-08): успешный ответ снимает ошибку предыдущей
                // попытки — иначе панель осталась бы с плашкой ошибки навсегда.
                state.detailError = null;
                // workerInfo: synthesised from the metric's host/port/type/status
                // — there is no separate "worker" field on the backend metrics.
                state.workerInfo = {
                    id: data.id,
                    name: data.name || data.id,
                    host: data.host,
                    cppWorkerPort: data.cppWorkerPort,
                    ollamaPort: data.ollamaPort,
                    type: data.backendType || data.type,
                    engine: data.engine,
                    status: data.status,
                    hasAgent: data.hasAgent,
                    labels: data.labels,
                    maxConcurrentRequests: data.maxConcurrentRequests,
                    vramUsagePercent: data.vramUsagePercent,
                    vramTotalGB: data.vramTotalGB,
                    vramUsedGB: data.vramUsedGB,
                    memoryUsagePercent: data.memoryUsagePercent,
                };
                // gpuInfo is at top level (BackendMetrics.GPU).
                state.gpuInfo = data.gpu || null;
                // Версия воркера: в BackendMetrics её нет, спрашиваем /health через
                // прокси балансера (best-effort, с кэшем).
                fetchWorkerVersionAsync(backend.id);
                // localModels: list of model objects that exist on the backend.
                // R60.4 (2026-09-04): switch source from `data.models` (array of strings,
                // just names) to `data.llamaCpp.loadedModels` (array of objects with
                // full meta: name, path, size, quantization, state, contextLength, etc.).
                // Before R60.4, webui gguf-renderer-detail.js:232-234 read m.size and
                // m.quantization on strings, which always returned undefined — UI showed
                // empty "Размер: -" and "Квантизация: -" even though the model was loaded.
                // For ollama backends, fall back to `data.ollama.runningModels` shape.
                if (data.backendType === 'llama_cpp' && data.llamaCpp && Array.isArray(data.llamaCpp.loadedModels)) {
                    state.localModels = data.llamaCpp.loadedModels;
                } else if (data.ollama && Array.isArray(data.ollama.runningModels)) {
                    state.localModels = data.ollama.runningModels;
                } else {
                    state.localModels = [];
                }
                // R60.5 (2026-09-07): fallback на /api/models/files (R60.3 endpoint) когда
                // нет загруженных моделей. Иначе вкладка "Модели" пустая если ни одна
                // модель не загружена, хотя файлы .gguf есть на диске. Догружаем
                // асинхронно, не блокируем основной refresh.
                if (state.localModels.length === 0 && data.backendType === 'llama_cpp' && state.selectedBackendId) {
                    fetchDiskModelsAsync(backend.id, state);
                }
                // loadedModels: detailed objects (BackendMetrics.llamaCpp.loadedModels).
                // For non-llama_cpp backends this is `ollama.runningModels` shape.
                if (data.backendType === 'llama_cpp' && data.llamaCpp && Array.isArray(data.llamaCpp.loadedModels)) {
                    state.loadedModels = data.llamaCpp.loadedModels;
                } else if (data.ollama && Array.isArray(data.ollama.runningModels)) {
                    state.loadedModels = data.ollama.runningModels;
                } else {
                    state.loadedModels = [];
                }
                // runtimeModels: R66d (2026-09-23) — ИСПРАВЛЕНО.
                //
                // БАГ: сюда писался «бандл» {gpu, system, prediction, score,
                // llamaCpp, ollama}, а renderLoadedPane() читает это поле как
                // КАРТУ ПО ИМЕНИ МОДЕЛИ:
                //   state.runtimeModels[name] || ...basename(m.path)...
                // Из-за несовпадения форм rt всегда был null, и блок runtime-
                // параметров (ctx=, gpu_layers=, batch=, fa=, layers=,
                // gguf_max=) НИКОГДА не отрисовывался на вкладке загруженных
                // моделей. Пользователь видел только default-значения и не мог
                // понять, с какой конфигурацией модель реально загружена
                // (= «не видно информации о моделях / трудно понять
                // конфигурацию, поэтому работа медленная»).
                //
                // Теперь: бандл кладём в state.backendRuntime (для метрик
                // бэкенда), а state.runtimeModels остаётся картой имя→runtime,
                // которую асинхронно наполняем из
                // GET /api/v1/cppworker/config/runtime.
                state.backendRuntime = {
                    gpu: data.gpu || null,
                    system: data.system || null,
                    prediction: data.prediction || null,
                    score: data.score || 0,
                    llamaCpp: data.llamaCpp || null,
                    ollama: data.ollama || null,
                };
                if (!state.runtimeModels || typeof state.runtimeModels !== 'object') {
                    state.runtimeModels = {};
                }
                // Подтягиваем реальные runtime-параметры загруженных моделей
                // (n_ctx/gpu_layers/batch/flash_attn/n_layers) — без этого
                // вкладка «Загруженные» показывает только defaults из метрик.
                fetchRuntimeModelsAsync(backend.id, state);
                // R59.8: refresh BOTH the panel (header + tabs + content)
                // and the pane (just the active tab's content). The
                // header shows status pills that depend on workerInfo,
                // and tabs may need to recompute their counts from
                // loadedModels/localModels.
                if (typeof M.refreshDetailPanel === 'function') M.refreshDetailPanel();
                else if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
            })
            .catch(function (err) {
                // R91 (2026-10-08): НЕ молчим. Раньше здесь был только
                // console.warn, поэтому при неудаче/таймауте оператор видел
                // пустые вкладки без объяснения («пропала вся информация кроме
                // инфо»). Теперь причина видна в самой панели вместе с кнопкой
                // «Повторить».
                console.warn('refreshDetail failed:', err);
                const isTimeout = !!(err && err.code === 'timeout');
                const msg = (err && err.message) ? String(err.message) : String(err);
                state.detailError = (window.I18N
                    ? I18N.t(isTimeout ? 'gguf.detail_load_timeout' : 'gguf.detail_load_failed')
                    : (isTimeout ? 'Backend data timed out' : 'Failed to load backend data')) +
                    ' — ' + msg;
                if (typeof M.refreshDetailPanel === 'function') M.refreshDetailPanel();
                else if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
            })
            .finally(function () {
                // R91 (2026-10-08): флаг снимается в finally. Это главная защита
                // от «зависшей панели»: раньше снятие жило в then/catch, и любой
                // незавершившийся промис отключал обновление деталей до
                // перезагрузки страницы.
                _detailRefreshInProgress = false;
            });
    };

    /**
     * R66d: асинхронно загрузить runtime-конфигурацию загруженных моделей
     * (реальные n_ctx / gpu_layers / batch / flash_attn / n_layers) и разложить
     * её по именам в state.runtimeModels.
     *
     * Источник — GgufApi.getRuntimeConfigViaBackend(backendId) →
     * GET /api/v1/cppworker/runtime-config через прокси балансера. Ошибки
     * игнорируем: без runtime-данных вкладка просто покажет defaults (как
     * раньше), а не сломается.
     */
    function fetchRuntimeModelsAsync(backendId, state) {
        if (!backendId) return;
        if (!window.GgufApi || typeof window.GgufApi.getRuntimeConfigViaBackend !== 'function') return;
        // Защита от параллельных запросов на один и тот же бэкенд.
        state._runtimeFetchFor = state._runtimeFetchFor || {};
        if (state._runtimeFetchFor[backendId]) return;
        state._runtimeFetchFor[backendId] = true;
        window.GgufApi.getRuntimeConfigViaBackend(backendId)
            .then(function (rtData) {
                state._runtimeFetchFor[backendId] = false;
                var list = (rtData && rtData.loaded_models) || [];
                var map = {};
                list.forEach(function (m) {
                    if (m && m.name) map[m.name] = m;
                });
                state.runtimeModels = map;
                // Перерисовываем только активную вкладку — не запускаем
                // refreshDetail (иначе получим цикл запросов).
                if (state.detailPane === 'models' && typeof M.refreshDetailPane === 'function') {
                    M.refreshDetailPane();
                }
            })
            .catch(function (err) {
                state._runtimeFetchFor[backendId] = false;
                if (window.console && console.debug) {
                    console.debug('[gguf] runtime config fetch failed:', err && err.message);
                }
            });
    }

    M.startActiveQueriesPolling = function() {
        const state = M.state;
        if (state._activeQueriesTimer) return;
        // 3s baseline, 1s when active inference detected (adaptive)
        var SLOW_INTERVAL_MS = 3000;
        var FAST_INTERVAL_MS = 1000;
        var fast = false;
        var api = window.GgufApi || window.Api;
        function tick() {
            if (!shouldPoll()) { scheduleNext(); return; }
            if (!state.selectedBackendId) {
                state.activeQueries = {};
                scheduleNext();
                return;
            }
            if (typeof api.activeQueries !== 'function') {
                scheduleNext();
                return;
            }
            api.activeQueries(state.selectedBackendId)
                .then(function (data) {
                    var next = (data && data.queries) || {};
                    var prev = state.activeQueries || {};
                    // Update if changed
                    var changed = JSON.stringify(next) !== JSON.stringify(prev);
                    state.activeQueries = next;
                    if (changed && state.detailPane === 'models') {
                        // busy badge changed — refresh detail pane
                        if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
                    }
                    // Switch to fast mode if any model has active queries
                    var hasActive = Object.values(next).some(function (n) { return n && n > 0; });
                    fast = hasActive;
                })
                .catch(function () {
                    // best-effort: опрос активных запросов не критичен
                })
                .finally(function () {
                    // R91 (2026-10-08): следующий тик планируется в finally —
                    // иначе незавершившийся (зависший) запрос останавливал опрос
                    // навсегда, и индикаторы «занято» на вкладке «Модели»
                    // замирали.
                    scheduleNext();
                });
        }
        function scheduleNext() {
            if (state._activeQueriesTimer) clearTimeout(state._activeQueriesTimer);
            state._activeQueriesTimer = setTimeout(function() {
                state._activeQueriesTimer = null;
                tick();
            }, fast ? FAST_INTERVAL_MS : SLOW_INTERVAL_MS);
        }
        state._activeQueriesTickFn = tick;
        scheduleNext();
    };

    M.stopActiveQueriesPolling = function() {
        const state = M.state;
        if (state._activeQueriesTimer) {
            clearTimeout(state._activeQueriesTimer);
            state._activeQueriesTimer = null;
        }
    };

    /**
 * shouldPoll — общее правило опроса (data-refresh.js): пауза авто-обновления и
 * скрытая вкладка останавливают и поллинги GGUF-страницы. Без этого «пауза» в
 * шапке не влияла на прогресс загрузок (проверено живьём).
 */
function shouldPoll() {
    if (window.DataRefresh && typeof window.DataRefresh.shouldPoll === 'function') {
        return window.DataRefresh.shouldPoll();
    }
    return true;
}

M.refreshActiveQueriesPolling = function() {
        // Force immediate refresh (cross-tab sync).
        const state = M.state;
        if (typeof state._activeQueriesTickFn === 'function') {
            state._activeQueriesTickFn();
        } else if (typeof M.startActiveQueriesPolling === 'function') {
            M.startActiveQueriesPolling();
        }
    };

    let _downloadsPollTimer = null;
    M.startDownloadPolling = function() {
        const state = M.state;
        if (_downloadsPollTimer) return;
        function tick() {
            if (!shouldPoll()) return;
            if (!state.activeDownloads || state.activeDownloads.length === 0) {
                if (_downloadsPollTimer) { clearInterval(_downloadsPollTimer); _downloadsPollTimer = null; }
                return;
            }
            if (typeof M.refreshActiveDownloads === 'function') M.refreshActiveDownloads();
        }
        _downloadsPollTimer = setInterval(tick, 2000);
        tick();
    };

    M.refreshActiveDownloads = function() {
        const state = M.state;
        if (!state.activeDownloads || state.activeDownloads.length === 0) {
            if (_downloadsPollTimer) { clearInterval(_downloadsPollTimer); _downloadsPollTimer = null; }
            if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
            return;
        }
        var api = window.GgufApi || window.Api;
        // R65-FIX (2026-09-16): GgufApi.getDownloadProgress, не hfDownloadProgress.
        if (typeof api.getDownloadProgress !== 'function') {
            return;
        }
        // Poll all active downloads
        var promises = state.activeDownloads.map(function (d) {
            return api.getDownloadProgress(d.modelId, d.filename)
                .then(function (progress) {
                    state.downloadProgress[d.modelId + '/' + d.filename] = progress;
                })
                .catch(function (err) {
                    // R66.3 (2026-09-16): 404 = race condition (POST ещё не дошёл),
                    // не удалять из state. Только 3+ подряд network/5xx ошибок
                    // → запись в history как "failed" + удаление.
                    if (err && err.code === 'not_found') return;
                    d._consecutiveErrors = (d._consecutiveErrors || 0) + 1;
                    if (d._consecutiveErrors >= 3) {
                        if (!state.downloadHistory) state.downloadHistory = [];
                        state.downloadHistory.push(Object.assign({}, d, {
                            status: 'failed',
                            endedAt: Date.now()
                        }));
                        state.activeDownloads = state.activeDownloads.filter(function (x) {
                            return !(x.modelId === d.modelId && x.filename === d.filename);
                        });
                    }
                });
        });
        Promise.all(promises).then(function () {
            if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
        });
    };

    /**
     * R66.4 (2026-09-16): подтянуть orphan .download файлы с cppworker.
     * Это файлы, которые остались на диске после прерванных загрузок
     * (network timeout, crash, container restart) — занимают место, но
     * не привязаны ни к одной активной загрузке.
     *
     * GET /api/hf/downloads теперь возвращает { active, history, orphans[] }.
     * Кладём orphans в state, чтобы renderDownloadsPane показал их
     * отдельным блоком "Residual files" с кнопкой 🗑 Delete.
     */
    M.refreshOrphanDownloads = function() {
        const state = M.state;
        var api = window.GgufApi || window.Api;
        if (typeof api.listActiveDownloads !== 'function') return;
        api.listActiveDownloads()
            .then(function (resp) {
                // resp = { active: [], history: [], orphans: [] }
                var arr = (resp && Array.isArray(resp.orphans)) ? resp.orphans : [];
                state.orphanDownloads = arr;
                if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
            })
            .catch(function () {
                // Тихо игнорируем — это cosmetic, не критично
                if (state.orphanDownloads && state.orphanDownloads.length) {
                    state.orphanDownloads = [];
                    if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
                }
            });
    };

    M.updateDetailLoading = function(loading) {
        const state = M.state;
        state.detailLoading = !!loading;
        if (typeof M.refreshDetailPanel === 'function') M.refreshDetailPanel();
    };

    M.refreshDetailPanel = function() {
        const panel = document.getElementById('ggufDetailPanel');
        if (!panel) return;
        if (typeof M.renderDetailPanel === 'function') {
            panel.innerHTML = M.renderDetailPanel();
        }
    };

    M.refreshDetailPane = function() {
        const pane = document.getElementById('ggufDetailContent');
        if (!pane) return;
        if (typeof M.renderDetailPane === 'function') {
            pane.innerHTML = M.renderDetailPane();
        }
    };
})();
