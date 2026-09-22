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

    M.unloadOnSelectedBackend = function(handle, force) {
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
        // R66c (2026-09-22): force=true выгружает ЗАНЯТУЮ модель.
        //
        // cppworker отказывает в unload, если по модели есть активные
        // inference-запросы: 409 + "model is busy with active inference
        // requests", и принимает ?force=true (обрывает генерации). Раньше
        // балансер этот флаг не прокидывал, и в UI не было никакого выхода:
        // пользователь видел "Unload failed: ... busy" и модель оставалась
        // в VRAM. Теперь на busy предлагаем подтверждение и повторяем с force.
        api.unloadModel(backend.id, handle, force ? { force: true } : {})
            .then(function (result) {
                if (result && result.success === false) {
                    if (result.retryWithForce || result.busy || result.status === 409) {
                        var msg = M._('gguf.confirm_unload_force') ||
                            'Модель занята активными запросами. Прервать их и выгрузить?';
                        if (typeof confirm !== 'function' || !confirm(msg)) return;
                        return M.unloadOnSelectedBackend(handle, true);
                    }
                    M.showToast('Unload failed: ' + (result.error || 'unknown error'), 'error');
                    return;
                }
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

    /**
     * Translate GgufApi error → user-friendly title + hint.
     * Использует err.code, который ставит и request(), и requestViaBackend().
     * Возвращает {title, hint, toastKind}.
     */
    function formatHfError(err) {
        const code = err && err.code;
        const baseHint = err && err.message ? String(err.message) : '';
        switch (code) {
            case 'auth_required':
                return {
                    title: 'HuggingFace token required',
                    hint: 'Save your HF token in the bar above (gated or private repo).',
                    toastKind: 'warn'
                };
            case 'not_found':
                return {
                    title: 'Model or repo not found',
                    hint: baseHint || 'No such repository on HuggingFace Hub.',
                    toastKind: 'warn'
                };
            case 'rate_limited':
                return {
                    title: 'HuggingFace rate limit',
                    hint: 'Add an HF token (free account lifts limits) and try again in a minute.',
                    toastKind: 'warn'
                };
            case 'timeout':
                return {
                    title: 'cppworker is not responding',
                    hint: baseHint,
                    toastKind: 'error'
                };
            case 'server_error':
                return {
                    title: 'cppworker error',
                    hint: baseHint || 'Backend returned 5xx. Check cppworker logs.',
                    toastKind: 'error'
                };
            case 'network':
                return {
                    title: 'Network error',
                    hint: baseHint || 'Check connection to cppworker.',
                    toastKind: 'error'
                };
            default:
                return {
                    title: 'HF search failed',
                    hint: baseHint,
                    toastKind: 'error'
                };
        }
    }

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
        // R65-FIX (2026-09-16): GgufApi называет метод searchModels, не hfSearch.
        // Старое имя заставляло всегда срабатывать guard "API not available".
        if (typeof api.searchModels !== 'function') {
            state.hfSearching = false;
            M.showToast('GgufApi.searchModels is not available', 'error');
            return;
        }
        api.searchModels(q, 20)
            .then(function (resp) {
                // R66.2 (2026-09-16): cppworker возвращает {count, query, results: [...]}.
                // Если записать весь объект в state.hfSearchResults, .filter() падает
                // с TypeError "filter is not a function" → "infinite loading".
                // Берём только массив results (или пустой массив если формат неожиданный).
                var arr = (resp && Array.isArray(resp.results)) ? resp.results :
                          (Array.isArray(resp) ? resp : []);
                state.hfSearchResults = arr;
                state.hfSearching = false;
                state.hfLastError = null;
                if (typeof M.refreshDetail === 'function') M.refreshDetail();
            })
            .catch(function (err) {
                state.hfSearching = false;
                const formatted = formatHfError(err);
                state.hfLastError = { title: formatted.title, hint: formatted.hint };
                if (typeof M.showToast === 'function') {
                    M.showToast(formatted.title + ': ' + formatted.hint, formatted.toastKind);
                }
                if (typeof M.refreshDetail === 'function') M.refreshDetail();
            });
    };

    M.quickDownloadHf = function(model) {
        // R66.3 (2026-09-16): раньше вызов "Скачать рекомендуемое" просто открывал
        // список файлов (viewHfFiles). Это вводило в заблуждение — кнопка с молнией
        // намекает на немедленное скачивание, а на деле требовала ещё один клик.
        // Теперь: если у модели есть recommended файл (cppworker возвращает поле
        // recommended в результатах search), сразу стартуем скачивание. Иначе —
        // fallback на список файлов (например, для моделей без GGUF-тегов).
        if (model && model.recommended) {
            if (typeof M.startDownload === 'function') {
                M.startDownload(model.id, model.recommended);
                return;
            }
        }
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
        // R65-FIX (2026-09-16): GgufApi.listModelFiles, не hfListFiles.
        if (typeof api.listModelFiles !== 'function') {
            M.showToast('GgufApi.listModelFiles is not available', 'error');
            return;
        }
        api.listModelFiles(model.id)
            .then(function (resp) {
                // R66.2 (2026-09-16): cppworker возвращает {count, modelId, files: [...]}.
                var arr = (resp && Array.isArray(resp.files)) ? resp.files :
                          (Array.isArray(resp) ? resp : []);
                state.hfModelFiles = arr;
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
        // R65-FIX (2026-09-16): GgufApi.startDownload, не hfDownload.
        if (typeof api.startDownload !== 'function') {
            M.showToast('GgufApi.startDownload is not available', 'error');
            return;
        }
        api.startDownload(modelId, filename)
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
        // R66.4 (2026-09-16): правильное имя метода — deleteDownload, не hfDelete.
        if (typeof api.deleteDownload !== 'function') {
            M.showToast('GgufApi.deleteDownload is not available', 'error');
            return;
        }
        api.deleteDownload(modelId, filename)
            .then(function (resp) {
                if (resp && resp.status === 'noop') {
                    M.showToast(M._('gguf.file_not_found') || 'File not found (already deleted?)', 'info');
                } else {
                    var freed = (resp && resp.result && resp.result.bytesFreed) || 0;
                    var msg = M._('gguf.file_deleted') || 'File deleted';
                    if (freed > 0) {
                        msg += '. ' + (M._('gguf.disk_freed', { size: formatBytesShort(freed) }) || ('Disk freed: ' + formatBytesShort(freed)));
                    }
                    M.showToast(msg, 'success');
                }
                // Удаляем из обоих списков — activeDownloads + downloadHistory + orphanDownloads
                state.activeDownloads = (state.activeDownloads || []).filter(function (d) {
                    return !(d.modelId === modelId && d.filename === filename);
                });
                state.downloadHistory = (state.downloadHistory || []).filter(function (d) {
                    return !(d.modelId === modelId && d.filename === filename);
                });
                state.orphanDownloads = (state.orphanDownloads || []).filter(function (d) {
                    return d.filename !== filename;
                });
                if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
            })
            .catch(function (err) {
                M.showToast('Delete failed: ' + ((err && err.message) || err), 'error');
            });
    };

    // R66.4 (2026-09-16): пакетное удаление всех orphan .download файлов.
    // Полезно когда на диске скопилось несколько недокачанных файлов и пользователь
    // хочет разом всё почистить. Последовательно вызывает deleteDownload для каждого.
    M.cleanupAllOrphans = function() {
        const state = M.state;
        var orphans = state.orphanDownloads || [];
        if (orphans.length === 0) {
            M.showToast(M._('gguf.no_orphans_to_cleanup') || 'No residual files', 'info');
            return;
        }
        var totalSize = orphans.reduce(function (s, o) { return s + (o.size || 0); }, 0);
        var msg = (M._('gguf.confirm_cleanup_all_orphans', { count: orphans.length, size: formatBytesShort(totalSize) }) ||
                  ('Delete ' + orphans.length + ' residual files (' + formatBytesShort(totalSize) + ')?'));
        if (!confirm(msg)) return;
        var api = window.GgufApi || window.Api;
        if (typeof api.deleteDownload !== 'function') {
            M.showToast('GgufApi.deleteDownload is not available', 'error');
            return;
        }
        // Последовательно — параллельные DELETE могут нагрузить cppworker.
        // Каждый callback уменьшает state.orphanDownloads и обновляет UI.
        var totalFreed = 0;
        var i = 0;
        function next() {
            if (i >= orphans.length) {
                M.showToast(M._('gguf.disk_freed', { size: formatBytesShort(totalFreed) }) ||
                            ('Freed ' + formatBytesShort(totalFreed)), 'success');
                if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
                return;
            }
            var o = orphans[i++];
            api.deleteDownload('', o.filename)
                .then(function (resp) {
                    if (resp && resp.result && resp.result.bytesFreed) totalFreed += resp.result.bytesFreed;
                    state.orphanDownloads = (state.orphanDownloads || []).filter(function (d) {
                        return d.filename !== o.filename;
                    });
                    if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
                })
                .catch(function () {
                    // Продолжаем даже если один файл не удалился
                })
                .then(next);
        }
        next();
    };

    // Local helper для красивого показа freed size
    function formatBytesShort(b) {
        if (!b || b === 0) return '0 B';
        var u = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = Math.floor(Math.log(b) / Math.log(1024));
        return (b / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0) + ' ' + u[i];
    }
})();
