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
                // localModels: list of model names that exist on the backend
                // (BackendMetrics.models — for llama_cpp this is the same as
                // the loaded model names; for ollama it's the pulled library).
                state.localModels = Array.isArray(data.models) ? data.models : [];
                // loadedModels: detailed objects (BackendMetrics.llamaCpp.loadedModels).
                // For non-llama_cpp backends this is `ollama.runningModels` shape.
                if (data.backendType === 'llama_cpp' && data.llamaCpp && Array.isArray(data.llamaCpp.loadedModels)) {
                    state.loadedModels = data.llamaCpp.loadedModels;
                } else if (data.ollama && Array.isArray(data.ollama.runningModels)) {
                    state.loadedModels = data.ollama.runningModels;
                } else {
                    state.loadedModels = [];
                }
                // runtimeModels: bundle of dynamic runtime signals
                // (gpu usage, system metrics, prediction score).
                state.runtimeModels = {
                    gpu: data.gpu || null,
                    system: data.system || null,
                    prediction: data.prediction || null,
                    score: data.score || 0,
                    llamaCpp: data.llamaCpp || null,
                    ollama: data.ollama || null,
                };
                _detailRefreshInProgress = false;
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
        if (typeof api.hfDownloadProgress !== 'function') {
            return;
        }
        // Poll all active downloads
        var promises = state.activeDownloads.map(function (d) {
            return api.hfDownloadProgress(d.modelId, d.filename)
                .then(function (progress) {
                    state.downloadProgress[d.modelId + '/' + d.filename] = progress;
                })
                .catch(function () {
                    // download probably failed
                    state.activeDownloads = state.activeDownloads.filter(function (x) {
                        return !(x.modelId === d.modelId && x.filename === d.filename);
                    });
                });
        });
        Promise.all(promises).then(function () {
            if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
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
