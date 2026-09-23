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
                _refreshInProgress = false;
                if (typeof M.updateBackendsList === 'function') M.updateBackendsList();
            })
            .catch(function (err) {
                _refreshInProgress = false;
                console.warn('refreshBackends failed:', err);
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
    M.refreshDetail = function() {
        const state = M.state;
        if (!state.selectedBackendId) return Promise.resolve();
        if (_detailRefreshInProgress) return Promise.resolve();
        _detailRefreshInProgress = true;
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
                _detailRefreshInProgress = false;
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
                _detailRefreshInProgress = false;
                console.warn('refreshDetail failed:', err);
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
                    scheduleNext();
                })
                .catch(function () {
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
