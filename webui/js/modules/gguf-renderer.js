/**
 * GGUF Renderer — master-detail layout
 *
 * Left panel:  list of registered llama.cpp backends
 * Right panel: detail view of the selected backend
 *              with tabs: About / Models / Downloads / HuggingFace / Settings
 *
 * Backends come from the balancer API (/api/v1/gguf/backends).
 * Once a backend is selected, its detail view calls the cppworker
 * API directly on the backend's URL.
 */
const GgufRenderer = (window.GgufRenderer = (function () {
    // R57.5a: namespace for split modules (helpers, state, list, detail, actions, refresh).
    // Each new file (gguf-renderer-helpers.js etc.) adds its functions to this namespace.
    // Current keys at R57.5a: stripGGUF, formatFileSize, showToast (set by helpers.js).
    var M = window.GgufModule = window.GgufModule || {};

    // Local aliases to namespace functions. Existing call sites (61 of them)
    // keep using bare `stripGGUF(...)`, `showToast(...)` etc. — they resolve
    // to M.* which is set by gguf-renderer-helpers.js loaded before this file.
    var stripGGUF = M.stripGGUF;
    // State and helpers (from gguf-renderer-state.js loaded BEFORE this file).
    // 80+ call sites in this file use bare `state` and `isHealthyBackend(b)` — these
    // aliases preserve backward compat without renaming.
    var state = M.state;
    var isHealthyBackend = M.isHealthyBackend;
    var visibleBackends = M.visibleBackends;
    // Left panel renderers (from gguf-renderer-list.js loaded BEFORE this file).
    var renderBackendsPanel = M.renderBackendsPanel;
    var renderBackendsList = M.renderBackendsList;

    // Action handlers (from gguf-renderer-actions.js loaded BEFORE this file).
    // Critical: onDetailPanelClick handler (in gguf-renderer.js) uses these
    // via onclick="ui.X(...)" — must remain available as bare identifiers.
    var selectBackend = M.selectBackend;
    var currentBackend = M.currentBackend;
    var loadOnSelectedBackend = M.loadOnSelectedBackend;
    var markLoadingModel = M.markLoadingModel;
    var markLoadFailed = M.markLoadFailed;
    var unloadOnSelectedBackend = M.unloadOnSelectedBackend;
    var deleteOnSelectedBackend = M.deleteOnSelectedBackend;
    var cancelActiveGeneration = M.cancelActiveGeneration;
    var doHfSearch = M.doHfSearch;
    var viewHfFiles = M.viewHfFiles;
    var startDownload = M.startDownload;
    var cancelDownload = M.cancelDownload;
    var deleteDownloadedFile = M.deleteDownloadedFile;

    // Data refresh (from gguf-renderer-refresh.js loaded BEFORE this file).
    var refreshBackends = M.refreshBackends;
    var updateBackendsList = M.updateBackendsList;
    var refreshDetail = M.refreshDetail;
    var startActiveQueriesPolling = M.startActiveQueriesPolling;
    var stopActiveQueriesPolling = M.stopActiveQueriesPolling;
    var refreshActiveQueriesPolling = M.refreshActiveQueriesPolling;
    var startDownloadPolling = M.startDownloadPolling;
    var refreshActiveDownloads = M.refreshActiveDownloads;
    var updateDetailLoading = M.updateDetailLoading;
    var refreshDetailPanel = M.refreshDetailPanel;
    var refreshDetailPane = M.refreshDetailPane;

    // Event handlers (from gguf-renderer-detail.js loaded BEFORE this file).
    var onDetailPanelClick = M.onDetailPanelClick;
    var bindEvents = M.bindEvents;
    var bindSettingsChange = M.bindSettingsChange;

    // Right panel render functions (from gguf-renderer-detail-render.js).
    var renderDetailPanel = M.renderDetailPanel;
    var renderEmptyDetail = M.renderEmptyDetail;
    var renderDetailHeader = M.renderDetailHeader;
    var renderDetailTabs = M.renderDetailTabs;
    var renderDetailPane = M.renderDetailPane;
    var renderAboutPane = M.renderAboutPane;
    var renderModelsPane = M.renderModelsPane;
    var renderLoadedPane = M.renderLoadedPane;
    var renderDownloadsPane = M.renderDownloadsPane;
    var renderHuggingFacePane = M.renderHuggingFacePane;
    var renderSettingsPane = M.renderSettingsPane;
    var loadAndRenderBackendOptions = M.loadAndRenderBackendOptions;
    var mountProfilesInSettings = M.mountProfilesInSettings;
    var renderConnectModal = M.renderConnectModal;






    var formatFileSize = M.formatFileSize;
    var showToast = M.showToast;

    const _ = function (key, vars) {
        return window.I18N ? I18N.t(key, vars) : key;
    };


    // Статусы, которые считаем «нерабочими» и по умолчанию скрываем в GGUF.

    function visibleBackends() {
        if (state.showUnhealthy) return state.registeredBackends;
        return state.registeredBackends.filter(isHealthyBackend);
    }

    let _refreshInProgress = false;
    let _detailRefreshInProgress = false;
    let _pollTimer = null;

    // Защита от двойных POST /api/hf/download: если запрос уже в полёте для конкретной
    // (modelId, filename) пары, второй вызов startDownload() сразу показывает прогресс,
    // не дожидаясь ответа сервера. Это страховка от утечки обработчиков
    // (bindEvents / delegated click), которая проявляется на стороне сервера как
    // 500 "download already in progress".
    const _downloadInFlight = Object.create(null); // key: "modelId/filename" -> true
    const _inflightToastShown = Object.create(null); // key -> true (чтобы не спамить тост)

    // ---- Main render ----

    function render(container) {
        if (!container) return;
        container.innerHTML = buildPageHtml();
        bindEvents(container);
        // === Регистрируем callback, чтобы GgufLoadProgress мог дёргать
        // renderBackendsList() при обновлении loadingModels. ===
        if (window.GgufLoadProgress && typeof window.GgufLoadProgress.registerBackendsRefreshFn === 'function') {
            window.GgufLoadProgress.registerBackendsRefreshFn(updateBackendsList);
        }
        // Initial backend list
        refreshBackends();
        // If a backend is already selected, fetch its data
        if (state.selectedBackendId) {
            refreshDetail();
        }
    }

    function buildPageHtml() {
        return '' +
            // Page-level header with HF token (small bar, applies to all backends)
            '<div class="gguf-hf-token-bar" id="ggufHfTokenBar" style="display:flex;align-items:center;gap:8px;padding:8px 12px;background:var(--bg-secondary);border-radius:8px;margin-bottom:8px;border:1px solid var(--border);">' +
                '<span style="font-size:13px;white-space:nowrap;"><i class="fas fa-key"></i> HF Token:</span>' +
                '<input type="password" id="ggufHfToken" class="form-control" style="flex:1;max-width:360px;" placeholder="hf_..." value="' + Utils.escapeHtml(GgufApi.getHFToken() || '') + '">' +
                '<button class="btn btn-sm btn-secondary" id="ggufSaveHfToken" title="' + _('gguf.copy_url') + '">' + (window.I18N ? I18N.t('common.save') : 'Save') + '</button>' +
                '<button class="btn btn-sm btn-secondary" id="ggufClearHfToken" title="' + (window.I18N ? I18N.t('models.close') : 'Clear') + '"><i class="fas fa-times"></i></button>' +
            '</div>' +

            // Master-detail grid
            '<div class="gguf-master-layout">' +
                // Left: backend list
                renderBackendsPanel() +
                // Right: detail panel
                '<div class="gguf-detail-panel" id="ggufDetailPanel">' +
                    renderDetailPanel() +
                '</div>' +
            '</div>' +

            // Connect-to-cppworker modal (bypass balancer)
            renderConnectModal();
    }

    // ---- Left panel: backends ----

    // ---- Public API ----
    return {
        render: render,
        getState: function () { return state; },
        selectBackend: selectBackend,
        refreshBackends: refreshBackends,
        refreshDetail: refreshDetail,
        // Экспортируем для GgufLoadProgress и тестов:
        markLoadingModel: markLoadingModel,
        markLoadFailed: markLoadFailed,
        updateBackendsList: updateBackendsList,
        // Round 32 #2 (2026-08-10): force refresh active-queries polling.
        // Используется из cross-tab sync (app.js BroadcastChannel handler)
        // когда другая вкладка отменила generation — мы форсируем refresh
        // busy badge чтобы увидеть изменения немедленно (вместо 3s polling).
        refreshActiveQueriesPolling: function () {
            // Просто вызываем сохранённый tick — он дёргает cppworkerActiveQueries
            // для всех loaded models и обновляет state.activeQueries. Если
            // состояние изменилось, refreshDetailPane перерисует busy badge.
            if (typeof state._activeQueriesTickFn === 'function') {
                state._activeQueriesTickFn();
            } else if (typeof startActiveQueriesPolling === 'function') {
                // Timer не запущен (no backend selected) — start it
                startActiveQueriesPolling();
            }
        }
    };
})());
