/**
 * gguf-renderer-actions.js — Backend action handlers (load, unload, cancel, download, HF search).
 *
 * R57.5d (2026-09-03): extracted from webui/js/modules/gguf-renderer.js.
 *
 * Содержит action handlers которые вызываются из delegated click handler
 * (onDetailPanelClick) или из bindings:
 *   - selectBackend(id) — выбор бэкенда
 *   - loadOnSelectedBackend(model) — загрузить модель
 *   - unloadOnSelectedBackend(handle) — выгрузить модель
 *   - deleteOnSelectedBackend(model) — удалить модель
 *   - markLoadingModel / markLoadFailed — busy state tracking
 *   - cancelActiveGeneration(modelName) — отмена генерации
 *   - doHfSearch() — HF search
 *   - quickDownloadHf(model) / viewHfFiles / startDownload / cancelDownload
 *   - deleteDownloadedFile(modelId, filename)
 *   - showInlineHfFileProgress / startDownloadPolling
 *
 * Зависимости:
 *   - window.GgufModule.state, M.refreshDetail, M.refreshBackends (cross-module calls)
 *   - window.GgufModule._ (I18N), M.showToast, M.stripGGUF (from helpers)
 *   - window.GgufApi, window.Api, window.Utils
 *
 * ВАЖНО: т.к. R57.5d — это ВТОРАЯ часть (R57.5d-1 = actions, R57.5d-2 = refresh
 * в следующем коммите), все references на not-yet-extracted функции
 * (refreshDetail, refreshBackends) вызываются через window.GgufModule
 * (после их extract'а в R57.5d-2) или через прямую ссылку.
 *
 * Загружается ДО gguf-renderer.js (в defer-цепочке).
 */
(function() {
    'use strict';

    const M = (window.GgufModule = window.GgufModule || {});

    // ---- Action handlers ----

    M.selectBackend = function(backendId) {
        const state = M.state;
        if (state.selectedBackendId === backendId) return;
        state.selectedBackendId = backendId;
        // Invalidate cached Settings form (R32 #6) для нового бэкенда
        // (cache живёт до явной очистки, но renderSettingsPane() перерисует
        // когда панель будет открыта).
        // R59.8 (2026-09-03): trigger refreshDetail so the right panel actually
        // loads workerInfo / gpuInfo / loadedModels for the new backend.
        // Before this, the detail panel was stuck on the empty state because
        // refreshBackends only redraws the LEFT (backends list), and no one
        // called refreshDetail. Now: redraw list (so the `.active` class
        // moves to the new item) + load backend metrics.
        if (typeof M.updateBackendsList === 'function') M.updateBackendsList();
        if (typeof M.refreshDetail === 'function') M.refreshDetail();
    };

    M.currentBackend = function() {
        const state = M.state;
        if (!state.selectedBackendId) return null;
        return state.registeredBackends.find(function (b) { return b.id === state.selectedBackendId; }) || null;
    };

    M.loadOnSelectedBackend = function(model) {
        const state = M.state;
        const backend = M.currentBackend();
        if (!backend) {
            M.showToast(M._('gguf.no_backend_selected') || 'No backend selected', 'error');
            return;
        }
        var handle = backend.id;
        // Mark as loading immediately
        M.markLoadingModel(handle, model.name || model, model.path);
        // Используем GgufApi (loader endpoint) или Api (fallback)
        var api = window.GgufApi || window.Api;
        if (typeof api.loadModel !== 'function') {
            M.showToast('loadModel API not available', 'error');
            M.markLoadFailed(handle, model.name || model, 'API not available');
            return;
        }
        api.loadModel(handle, model.name, { path: model.path || null })
            .then(function () {
                M.showToast(M._('gguf.model_loaded') || 'Model loaded: ' + model.name, 'success');
                if (typeof M.refreshDetail === 'function') M.refreshDetail();
            })
            .catch(function (err) {
                M.markLoadFailed(handle, model.name || model, (err && err.message) || String(err));
                M.showToast('Load failed: ' + ((err && err.message) || err), 'error');
            });
    };

    M.markLoadingModel = function(backendId, name, path) {
        const state = M.state;
        if (!state.loadingModels) state.loadingModels = {};
        if (!state.loadingModels[backendId]) state.loadingModels[backendId] = [];
        state.loadingModels[backendId].push({
            name: name,
            path: path,
            state: 'loading',
            loadingStartedAt: new Date().toISOString(),
            elapsedMs: 0
        });
        if (typeof M.refreshBackends === 'function') M.refreshBackends();
        if (typeof M.refreshDetail === 'function') M.refreshDetail();
    };

    M.markLoadFailed = function(backendId, name, errMsg) {
        const state = M.state;
        if (state.loadingModels && state.loadingModels[backendId]) {
            const item = state.loadingModels[backendId].find(function (lm) { return lm.name === name; });
            if (item) item.state = 'error';
        }
        if (errMsg && typeof M.addLog === 'function') M.addLog('Load failed: ' + name + ' — ' + errMsg, 'error');
        if (typeof M.refreshBackends === 'function') M.refreshBackends();
        if (typeof M.refreshDetail === 'function') M.refreshDetail();
    };

    M.unloadOnSelectedBackend = function(handle) {
        const backend = M.currentBackend();
        if (!backend) {
            M.showToast(M._('gguf.no_backend_selected') || 'No backend selected', 'error');
            return;
        }
        var api = window.GgufApi || window.Api;
        if (typeof api.unloadModel !== 'function') {
            M.showToast('unloadModel API not available', 'error');
            return;
        }
        api.unloadModel(backend.id, handle)
            .then(function () {
                M.showToast(M._('gguf.model_unloaded') || 'Model unloaded', 'success');
                if (typeof M.refreshDetail === 'function') M.refreshDetail();
            })
            .catch(function (err) {
                M.showToast('Unload failed: ' + ((err && err.message) || err), 'error');
            });
    };

    M.deleteOnSelectedBackend = function(model) {
        const backend = M.currentBackend();
        if (!backend) {
            M.showToast(M._('gguf.no_backend_selected') || 'No backend selected', 'error');
            return;
        }
        if (!confirm(M._('gguf.confirm_delete_model', { name: model.name || model }) ||
                      'Delete model ' + (model.name || model) + '?')) return;
        var api = window.GgufApi || window.Api;
        if (typeof api.deleteModel !== 'function') {
            M.showToast('deleteModel API not available', 'error');
            return;
        }
        api.deleteModel(backend.id, model.name || model)
            .then(function () {
                M.showToast(M._('gguf.model_deleted') || 'Model deleted', 'success');
                if (typeof M.refreshDetail === 'function') M.refreshDetail();
            })
            .catch(function (err) {
                M.showToast('Delete failed: ' + ((err && err.message) || err), 'error');
            });
    };

    M.cancelActiveGeneration = function(modelName) {
        const state = M.state;
        const backend = M.currentBackend();
        if (!backend) return;
        var api = window.GgufApi || window.Api;
        if (typeof api.cancelGeneration !== 'function') {
            M.showToast('cancelGeneration API not available', 'error');
            return;
        }
        api.cancelGeneration(backend.id, modelName)
            .then(function () {
                M.showToast(M._('gguf.generation_cancelled') || 'Generation cancelled', 'info');
                if (typeof M.refreshActiveQueriesPolling === 'function') M.refreshActiveQueriesPolling();
            })
            .catch(function (err) {
                M.showToast('Cancel failed: ' + ((err && err.message) || err), 'error');
            });
    };

    // ---- HF search actions ----

    M.doHfSearch = function() {
        const state = M.state;
        if (state.hfSearching) return;
        if (!state.hfSearchQuery || state.hfSearchQuery.trim().length < 2) {
            M.showToast(M._('gguf.hf_query_too_short') || 'Query too short (min 2 chars)', 'warn');
            return;
        }
        state.hfSearching = true;
        if (typeof M.refreshDetail === 'function') M.refreshDetail();
        var api = window.GgufApi || window.Api;
        var q = state.hfSearchQuery.trim();
        if (typeof api.hfSearch !== 'function') {
            state.hfSearching = false;
            M.showToast('hfSearch API not available', 'error');
            return;
        }
        api.hfSearch(q, 10)
            .then(function (results) {
                state.hfSearchResults = results || [];
                state.hfSearching = false;
                if (typeof M.refreshDetail === 'function') M.refreshDetail();
            })
            .catch(function (err) {
                state.hfSearching = false;
                M.showToast('HF search failed: ' + ((err && err.message) || err), 'error');
                if (typeof M.refreshDetail === 'function') M.refreshDetail();
            });
    };

    M.quickDownloadHf = function(model) {
        if (typeof M.viewHfFiles === 'function') M.viewHfFiles(model);
    };

    M.viewHfFiles = function(model) {
        const state = M.state;
        if (!model || !model.id) {
            M.showToast('No model selected', 'error');
            return;
        }
        state.hfSearchSelected = model;
        state.hfModelFiles = [];
        if (typeof M.refreshDetail === 'function') M.refreshDetail();
        var api = window.GgufApi || window.Api;
        if (typeof api.hfListFiles !== 'function') {
            M.showToast('hfListFiles API not available', 'error');
            return;
        }
        api.hfListFiles(model.id)
            .then(function (files) {
                state.hfModelFiles = files || [];
                if (typeof M.refreshDetail === 'function') M.refreshDetail();
            })
            .catch(function (err) {
                M.showToast('HF list files failed: ' + ((err && err.message) || err), 'error');
            });
    };

    M.startDownload = function(modelId, filename) {
        const state = M.state;
        if (!state.activeDownloads) state.activeDownloads = [];
        state.activeDownloads.push({ modelId: modelId, filename: filename, startedAt: Date.now() });
        if (typeof M.startDownloadPolling === 'function') M.startDownloadPolling();
        var api = window.GgufApi || window.Api;
        if (typeof api.hfDownload !== 'function') {
            M.showToast('hfDownload API not available', 'error');
            return;
        }
        api.hfDownload(modelId, filename)
            .then(function () {
                M.showToast(M._('gguf.download_started') || 'Download started: ' + filename, 'success');
                if (typeof M.refreshActiveDownloads === 'function') M.refreshActiveDownloads();
            })
            .catch(function (err) {
                M.showToast('Download failed: ' + ((err && err.message) || err), 'error');
                state.activeDownloads = state.activeDownloads.filter(function (d) {
                    return !(d.modelId === modelId && d.filename === filename);
                });
                if (typeof M.refreshActiveDownloads === 'function') M.refreshActiveDownloads();
            });
    };

    M.cancelDownload = function(modelId, filename) {
        var api = window.GgufApi || window.Api;
        if (typeof api.hfCancel !== 'function') {
            M.showToast('hfCancel API not available', 'error');
            return;
        }
        api.hfCancel(modelId, filename)
            .then(function () {
                M.showToast(M._('gguf.download_cancelled') || 'Download cancelled', 'info');
                if (typeof M.refreshActiveDownloads === 'function') M.refreshActiveDownloads();
            })
            .catch(function (err) {
                M.showToast('Cancel failed: ' + ((err && err.message) || err), 'error');
            });
    };

    M.deleteDownloadedFile = function(modelId, filename) {
        const state = M.state;
        if (!confirm(M._('gguf.confirm_delete_file', { name: filename }) ||
                      'Delete ' + filename + '?')) return;
        var api = window.GgufApi || window.Api;
        if (typeof api.hfDelete !== 'function') {
            M.showToast('hfDelete API not available', 'error');
            return;
        }
        api.hfDelete(modelId, filename)
            .then(function () {
                M.showToast(M._('gguf.file_deleted') || 'File deleted', 'success');
                if (typeof M.refreshActiveDownloads === 'function') M.refreshActiveDownloads();
            })
            .catch(function (err) {
                M.showToast('Delete failed: ' + ((err && err.message) || err), 'error');
            });
    };
})();
