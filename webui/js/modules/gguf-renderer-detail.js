/**
 * gguf-renderer-detail.js — Delegated event handlers for detail panel + bindEvents.
 *
 * R57.5e (2026-09-03): extracted from webui/js/modules/gguf-renderer.js.
 *
 * Содержит:
 *   - onDetailPanelClick(e) — единый делегированный обработчик кликов на #ggufDetailPanel
 *   - bindEvents(container) — привязка обработчиков кнопок/checkbox'ов
 *   - bindSettingsChange(scope, id, stateKey, kind) — утилита для settings
 *
 * Зависимости (должны быть загружены раньше):
 *   - window.GgufModule.state, M.refreshDetailPane, M.refreshActiveDownloads,
 *     M.startDownloadPolling, M.loadOnSelectedBackend, M.unloadOnSelectedBackend,
 *     M.deleteOnSelectedBackend, M.cancelDownload, M.cancelActiveGeneration,
 *     M.startDownload, M.deleteDownloadedFile, M.viewHfFiles, M.quickDownloadHf,
 *     M.doHfSearch, M.showToast, M._
 *   - window.GgufModule (loadAndRenderBackendOptions, mountProfilesInSettings — set by gguf-renderer-detail-render.js)
 *
 * Загружается ДО gguf-renderer.js (в defer-цепочке).
 */
(function() {
    'use strict';

    const M = (window.GgufModule = window.GgufModule || {});

    // ---- Delegated detail panel click handler ----

    M.onDetailPanelClick = function(e) {
        const state = M.state;
        // Detail tabs
        var tab = e.target.closest('.gguf-detail-tab');
        if (tab) {
            var id = tab.getAttribute('data-detail-tab');
            if (id) {
                state.detailPane = id;
                // R59.9 (2026-09-03): call refreshDetailPanel (not just
                // refreshDetailPane) so the tab row itself re-renders
                // with the new `active` class. Before this, content
                // updated but the active tab highlight stayed on the
                // old tab. refreshDetailPanel re-runs renderDetailPanel
                // which re-emits the tab row + the active-tab content.
                if (typeof M.refreshDetailPanel === 'function') {
                    M.refreshDetailPanel();
                } else if (typeof M.refreshDetailPane === 'function') {
                    M.refreshDetailPane();
                }
                if (id === 'downloads') {
                    // R66.4 (2026-09-16): подтягиваем orphan .download файлы тоже,
                    // чтобы UI мог показать "residual files" с кнопкой Delete.
                    if (typeof M.refreshActiveDownloads === 'function') M.refreshActiveDownloads();
                    if (typeof M.refreshOrphanDownloads === 'function') M.refreshOrphanDownloads();
                    if (state.activeDownloads && state.activeDownloads.length > 0) {
                        if (typeof M.startDownloadPolling === 'function') M.startDownloadPolling();
                    }
                } else if (id === 'settings') {
                    // Settings tab: подгружаем реальные llama.cpp-дефолты выбранного
                    // cppworker'а и монтируем Per-Model Profiles (cppworker-params.js).
                    // R60.46 (2026-09-16): fix JS loading-order bug. Bare references
                    // `loadAndRenderBackendOptions` / `mountProfilesInSettings` были
                    // undefined because gguf-renderer-detail.js загружается ДО
                    // gguf-renderer-detail-render.js (который определяет функции
                    // через M.loadAndRenderBackendOptions = ...). typeof check
                    // проходил (typeof undefined === 'undefined'), но call никогда
                    // не выполнялся → Settings tab показывал "Loading..." вечно.
                    // Fix: используем M.* namespace (работает всегда — функции
                    // появляются в M.* до того как пользователь кликнет на таб).
                    if (typeof M.loadAndRenderBackendOptions === 'function') M.loadAndRenderBackendOptions();
                    if (typeof M.mountProfilesInSettings === 'function') M.mountProfilesInSettings();
                }
            }
            return;
        }
        var loadBtn = e.target.closest('.gguf-load-btn');
        if (loadBtn) {
            var idx = parseInt(loadBtn.getAttribute('data-idx'));
            if (!isNaN(idx) && state.localModels[idx]) {
                if (typeof M.loadOnSelectedBackend === 'function') M.loadOnSelectedBackend(state.localModels[idx]);
            }
            return;
        }
        var unloadBtn = e.target.closest('.gguf-unload-btn');
        if (unloadBtn) {
            var handle = unloadBtn.getAttribute('data-handle');
            if (handle && typeof M.unloadOnSelectedBackend === 'function') M.unloadOnSelectedBackend(handle);
            return;
        }
        var delBtn = e.target.closest('.gguf-delete-btn');
        if (delBtn) {
            var di = parseInt(delBtn.getAttribute('data-idx'));
            if (!isNaN(di) && state.localModels[di]) {
                if (typeof M.deleteOnSelectedBackend === 'function') M.deleteOnSelectedBackend(state.localModels[di]);
            }
            return;
        }
        var cancelDl = e.target.closest('.gguf-cancel-dl-btn');
        if (cancelDl) {
            if (typeof M.cancelDownload === 'function') {
                M.cancelDownload(cancelDl.getAttribute('data-model-id'), cancelDl.getAttribute('data-filename'));
            }
            return;
        }
        // Round 32 #2 (2026-08-10): cancel generation button в busy badge.
        // Клик по кнопке отменяет активную inference-генерацию через cppworker
        // /api/cancel endpoint.
        var cancelGen = e.target.closest('.gguf-cancel-gen-btn');
        if (cancelGen) {
            if (typeof M.cancelActiveGeneration === 'function') {
                M.cancelActiveGeneration(cancelGen.getAttribute('data-model-name'));
            }
            e.preventDefault();
            e.stopPropagation();
            return;
        }
        // Round 17.3 (2026-08-03): Resume кнопка для прерванных загрузок.
        var resumeDl = e.target.closest('.gguf-resume-dl-btn');
        if (resumeDl) {
            var rmi = resumeDl.getAttribute('data-model-id');
            var rfn = resumeDl.getAttribute('data-filename');
            if (typeof M.startDownload === 'function') M.startDownload(rmi, rfn);
            return;
        }
        // Round 17.3: Delete кнопка — удаляет скачанный/частичный файл из контейнера.
        var deleteDl = e.target.closest('.gguf-delete-dl-btn');
        if (deleteDl) {
            var dmi = deleteDl.getAttribute('data-model-id');
            var dfn = deleteDl.getAttribute('data-filename');
            if (typeof M.deleteDownloadedFile === 'function') M.deleteDownloadedFile(dmi, dfn);
            return;
        }
        // R66.4 (2026-09-16): Delete кнопка для orphan файла — modelId неизвестен,
        // cleanup endpoint ищет только по filename.
        var deleteOrphan = e.target.closest('.gguf-delete-orphan-btn');
        if (deleteOrphan) {
            var ofn = deleteOrphan.getAttribute('data-filename');
            if (typeof M.deleteDownloadedFile === 'function') {
                // Пустой modelId — backend игнорирует, ищет только filename.
                M.deleteDownloadedFile('', ofn);
            }
            return;
        }
        // R66.4 (2026-09-16): "Clear all residual files" — батч-удаление всех orphan.
        var cleanupAll = e.target.closest('#ggufCleanupAllOrphans,.gguf-cleanup-all-orphans-btn');
        if (cleanupAll) {
            if (typeof M.cleanupAllOrphans === 'function') M.cleanupAllOrphans();
            return;
        }
        var viewHf = e.target.closest('.gguf-view-hf-files-btn');
        if (viewHf) {
            var vi = parseInt(viewHf.getAttribute('data-model-idx'));
            if (!isNaN(vi) && state.hfSearchResults[vi]) {
                if (typeof M.viewHfFiles === 'function') M.viewHfFiles(state.hfSearchResults[vi]);
            }
            return;
        }
        var quickDl = e.target.closest('.gguf-quick-download-btn');
        if (quickDl) {
            var qi = parseInt(quickDl.getAttribute('data-model-idx'));
            if (!isNaN(qi) && state.hfSearchResults[qi]) {
                if (typeof M.quickDownloadHf === 'function') M.quickDownloadHf(state.hfSearchResults[qi]);
            }
            return;
        }
        var closeHf = e.target.closest('#ggufCloseHfFiles');
        if (closeHf) {
            state.hfSearchSelected = null;
            state.hfModelFiles = [];
            if (typeof M.refreshDetailPane === 'function') M.refreshDetailPane();
            return;
        }
        var dlHf = e.target.closest('.gguf-hf-download-btn');
        if (dlHf) {
            if (typeof M.startDownload === 'function') {
                M.startDownload(dlHf.getAttribute('data-model-id'), dlHf.getAttribute('data-filename'));
            }
            return;
        }
        // Per-file download (from the inline file list inside HF search result cards)
        var dlFile = e.target.closest('.gguf-download-file-btn');
        if (dlFile) {
            var fi = parseInt(dlFile.getAttribute('data-model-idx'));
            var fname = dlFile.getAttribute('data-filename');
            if (!isNaN(fi) && fname && state.hfSearchResults[fi]) {
                var modelId = state.hfSearchResults[fi].id || state.hfSearchResults[fi].modelId;
                // R66b: здесь был вызов showInlineHfFileProgress() под проверкой
                // `typeof ... === 'function'`, но такой функции в проекте нет —
                // ветка мертва. Прогресс per-file показывает startDownload()
                // (gguf-load-progress.js), поэтому мёртвый вызов убран.
                if (typeof M.startDownload === 'function') M.startDownload(modelId, fname);
            }
            return;
        }
        // Toggle file list in a result card
        var cardHeader = e.target.closest('.gguf-result-header');
        if (cardHeader) {
            var card = cardHeader.closest('.gguf-result-card');
            if (card) {
                var filesList = card.querySelector('.gguf-files-list');
                if (filesList) {
                    var isCollapsed = filesList.style.display === 'none';
                    filesList.style.display = isCollapsed ? '' : 'none';
                }
            }
            return;
        }
        // Collapse all results
        var collapseAll = e.target.closest('.gguf-collapse-all-btn');
        if (collapseAll) {
            var lists = document.querySelectorAll('#ggufHfSearchResults .gguf-files-list');
            lists.forEach(function (el) { el.style.display = 'none'; });
            if (typeof M.showToast === 'function') {
                M.showToast(M._('gguf.collapsed') || 'Collapsed', 'info');
            }
            return;
        }
        // R66 (2026-09-16): Suggestion chips / direct-id chips in empty state.
        // Клик по чипу заполняет поле и триггерит search или сразу viewHfFiles.
        var sugg = e.target.closest('.gguf-suggestion-chip');
        if (sugg) {
            var q = sugg.getAttribute('data-suggestion') || '';
            var inputEl = document.getElementById('ggufHfDetailSearch');
            if (inputEl) {
                inputEl.value = q;
                state.hfSearchQuery = q;
                if (typeof M.refreshDetail === 'function') M.refreshDetail();
                if (typeof M.doHfSearch === 'function') M.doHfSearch();
            }
            return;
        }
        var directChip = e.target.closest('.gguf-direct-chip');
        if (directChip) {
            var dq = directChip.getAttribute('data-direct') || '';
            var dInputEl = document.getElementById('ggufHfDetailSearch');
            if (dInputEl) dInputEl.value = dq;
            state.hfSearchQuery = dq;
            if (typeof M.refreshDetail === 'function') M.refreshDetail();
            if (dq.indexOf('/') >= 0 && typeof M.viewHfFiles === 'function') {
                M.viewHfFiles({ id: dq });
            } else if (typeof M.doHfSearch === 'function') {
                M.doHfSearch();
            }
            return;
        }
        var copyBtn = e.target.closest('#ggufCopyUrl');
        if (copyBtn) {
            if (window.GgufApi && typeof M.currentBackend === 'function') {
                window.GgufApi.buildBackendWorkerUrlAsync(M.currentBackend()).then(function (url) {
                    if (navigator.clipboard && url) {
                        navigator.clipboard.writeText(url).then(function () {
                            if (typeof M.showToast === 'function') M.showToast(M._('gguf.url_copied'), 'success');
                        }).catch(function () {
                            if (typeof M.showToast === 'function') M.showToast(url, 'info');
                        });
                    } else if (url) {
                        if (typeof M.showToast === 'function') M.showToast(url, 'info');
                    }
                }).catch(function (err) {
                    if (typeof M.showToast === 'function') M.showToast('Copy URL error: ' + err.message, 'error');
                });
            }
            return;
        }
        // R66-FIX (2026-09-16): кнопка «Найти» (#ggufHfDetailSearchBtn).
        // До этого биндили напрямую — терялся после первого refreshDetail().
        // Через делегацию на #ggufDetailPanel — переживает любые перерисовки.
        var hfBtn = e.target.closest('#ggufHfDetailSearchBtn');
        if (hfBtn) {
            var inp = document.getElementById('ggufHfDetailSearch');
            if (inp && M.state) M.state.hfSearchQuery = inp.value;
            if (typeof M.doHfSearch === 'function') M.doHfSearch();
            return;
        }
    };

    // ---- Delegated input/keydown for HF search ----
    // R66-FIX (2026-09-16): биндим ОДИН раз на #ggufDetailPanel (в bindEvents),
    // а не напрямую на input/button. refreshDetail() пересоздаёт input,
    // и addEventListener на старой ноде теряется.
    //
    // R66.1 (2026-09-16): убрал debounced live-search. Пользователь жаловался,
    // что автопоиск на 300мс срабатывал посреди ввода «qwen3.8» и не давал
    // допечатать запрос. Теперь search запускается ТОЛЬКО:
    //   - кликом по «Найти»
    //   - нажатием Enter в поле
    //   - кликом по suggestion-chip / direct-chip
    //   - вводом «org/model» (с `/`) → сразу открываем файлы без search
    //
    // Input listener теперь только синхронизирует state.hfSearchQuery
    // (нужно для случаев, когда «Найти» кликают ПОСЛЕ набора текста).
    M.onDetailPanelInput = function(e) {
        var input = e.target;
        if (!input || input.id !== 'ggufHfDetailSearch') return;
        var state = M.state;
        if (!state) return;
        // Только синхронизация value → state. Никаких API-вызовов.
        state.hfSearchQuery = input.value;
        // Если пользователь стирает поле до <2 chars — очищаем результаты.
        var q = input.value.trim();
        if (q.length < 2 && state.hfSearchResults && state.hfSearchResults.length) {
            state.hfSearchResults = [];
            state.hfLastError = null;
            if (typeof M.refreshDetail === 'function') M.refreshDetail();
        }
    };

    M.onDetailPanelKeydown = function(e) {
        if (e.key !== 'Enter') return;
        var input = e.target;
        if (!input || input.id !== 'ggufHfDetailSearch') return;
        var state = M.state;
        if (state) state.hfSearchQuery = input.value;
        var q = input.value.trim();
        if (q.length >= 2 && q.indexOf('/') >= 0) {
            if (typeof M.viewHfFiles === 'function') M.viewHfFiles({ id: q });
            return;
        }
        if (typeof M.doHfSearch === 'function') M.doHfSearch();
    };

    // ---- Event binding ----

    M.bindEvents = function(container) {
        const state = M.state;
        // Backend list click — ОДИН раз через флаг, чтобы не было дублей при refreshBackends().
        var list = container.querySelector('#ggufBackendList');
        if (list && !list._ggufListClickBound) {
            list._ggufListClickBound = true;
            list.addEventListener('click', function (e) {
                var item = e.target.closest('.gguf-backend-item');
                if (item) {
                    var id = item.getAttribute('data-backend-id');
                    if (id && typeof M.selectBackend === 'function') M.selectBackend(id);
                }
            });
        }

        // Toggle "Show unhealthy" checkbox
        var showUnhealthyCb = container.querySelector('#ggufShowUnhealthy');
        if (showUnhealthyCb) {
            showUnhealthyCb.addEventListener('change', function () {
                state.showUnhealthy = this.checked;
                if (typeof M.updateBackendsList === 'function') M.updateBackendsList();
            });
        }

        // Refresh backends (эти кнопки могут пересоздаваться, поэтому перепривязка безопасна)
        var refreshBackendsBtn = container.querySelector('#ggufRefreshBackends');
        if (refreshBackendsBtn) {
            refreshBackendsBtn.addEventListener('click', function () {
                if (typeof M.refreshBackends === 'function') M.refreshBackends();
            });
        }

        // Refresh detail
        var refreshDetailBtn = container.querySelector('#ggufRefreshDetail');
        if (refreshDetailBtn) {
            refreshDetailBtn.addEventListener('click', function () {
                if (typeof M.refreshDetail === 'function') M.refreshDetail();
            });
        }

        // Detail panel: делегированный обработчик кликов — ОДИН раз за время жизни панели.
        var detailPanel = container.querySelector('#ggufDetailPanel');
        if (detailPanel && !detailPanel._ggufDetailClickBound) {
            detailPanel._ggufDetailClickBound = true;
            state._detailPanel = detailPanel;
            detailPanel.addEventListener('click', M.onDetailPanelClick);
            // R66-FIX (2026-09-16): input/keydown тоже делегируем на панель.
            // Раньше биндили напрямую на hfInput — но refreshDetail() пересоздаёт
            // #ggufHfDetailSearch, и addEventListener терялся. После первого же
            // клика по «Найти» новый input/button оставались без обработчиков.
            detailPanel.addEventListener('input', M.onDetailPanelInput);
            detailPanel.addEventListener('keydown', M.onDetailPanelKeydown);
        } else if (detailPanel) {
            state._detailPanel = detailPanel;
        }

        // Settings change handlers (привязываются к новым элементам после refresh).
        if (detailPanel) {
            M.bindSettingsChange(detailPanel, 'ggufDetailAutoGpu', 'autoGpuDistribution', 'checked');
            M.bindSettingsChange(detailPanel, 'ggufDetailStrategy', 'strategy', 'value');
            M.bindSettingsChange(detailPanel, 'ggufDetailGpuLayers', 'gpuLayers', 'valueInt');
            M.bindSettingsChange(detailPanel, 'ggufDetailCtxSize', 'ctxSize', 'valueInt');
            M.bindSettingsChange(detailPanel, 'ggufDetailBatchSize', 'batchSize', 'valueInt');
            M.bindSettingsChange(detailPanel, 'ggufDetailFlashAttn', 'flashAttn', 'checked');
            M.bindSettingsChange(detailPanel, 'ggufDetailNuma', 'numa', 'checked');
            M.bindSettingsChange(detailPanel, 'ggufDetailUseMmap', 'useMmap', 'checked');
            M.bindSettingsChange(detailPanel, 'ggufDetailTensorSplit', 'tensorSplit', 'valueOrNull');

            // Кнопка «Найти» обрабатывается в onDetailPanelClick (там же, где
            // suggestion-chip / direct-chip / file-row / и т.д.).
            // Прямой addEventListener НЕ вешаем — refreshDetail() пересоздаёт DOM,
            // и обработчик теряется (R66, 2026-09-16).
        }

        // HF token
        var saveHfBtn = container.querySelector('#ggufSaveHfToken');
        if (saveHfBtn) {
            saveHfBtn.addEventListener('click', function () {
                var t = container.querySelector('#ggufHfToken');
                if (t) {
                    if (window.GgufApi && typeof window.GgufApi.setHFToken === 'function') {
                        window.GgufApi.setHFToken(t.value.trim());
                    }
                    if (typeof M.showToast === 'function') M.showToast('HF Token saved', 'success');
                }
            });
        }
        var clearHfBtn = container.querySelector('#ggufClearHfToken');
        if (clearHfBtn) {
            clearHfBtn.addEventListener('click', function () {
                var t = container.querySelector('#ggufHfToken');
                if (t) t.value = '';
                if (window.GgufApi && typeof window.GgufApi.setHFToken === 'function') {
                    window.GgufApi.setHFToken('');
                }
                if (typeof M.showToast === 'function') M.showToast('HF Token cleared', 'info');
            });
        }

        // Connect modal
        var openConnectBtn = container.querySelector('#ggufOpenConnectModal');
        if (openConnectBtn) {
            openConnectBtn.addEventListener('click', function () {
                var modal = document.getElementById('ggufConnectModal');
                if (modal) modal.classList.add('active');
            });
        }
        var cancelConnectBtn = container.querySelector('#ggufConnectCancel');
        if (cancelConnectBtn) {
            cancelConnectBtn.addEventListener('click', function () {
                var modal = document.getElementById('ggufConnectModal');
                if (modal) modal.classList.remove('active');
            });
        }
        var submitConnectBtn = container.querySelector('#ggufConnectSubmit');
        if (submitConnectBtn) {
            submitConnectBtn.addEventListener('click', function () {
                var input = document.getElementById('ggufConnectUrl');
                if (input) {
                    var url = input.value.trim();
                    if (url) {
                        if (window.GgufApi && typeof window.GgufApi.setUrl === 'function') {
                            window.GgufApi.setUrl(url);
                        }
                        if (typeof M.showToast === 'function') M.showToast('URL set: ' + url, 'success');
                    }
                }
                var modal = document.getElementById('ggufConnectModal');
                if (modal) modal.classList.remove('active');
            });
        }
    };

    M.bindSettingsChange = function(scope, id, stateKey, kind) {
        const state = M.state;
        var el = scope.querySelector('#' + id);
        if (!el) return;
        el.addEventListener('change', function () {
            if (kind === 'checked') state.loadOptions[stateKey] = this.checked;
            else if (kind === 'value') state.loadOptions[stateKey] = this.value;
            else if (kind === 'valueInt') state.loadOptions[stateKey] = parseInt(this.value) || 0;
            else if (kind === 'valueOrNull') state.loadOptions[stateKey] = this.value || null;
        });
    };
})();
