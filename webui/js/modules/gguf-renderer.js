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
    const _ = function (key, vars) {
        return window.I18N ? I18N.t(key, vars) : key;
    };

    // ---- State ----
    let state = {
        registeredBackends: [],
        backendDataLoaded: false,
        selectedBackendId: null,
        // Показывать нерабочие (unhealthy/offline/draining) бэкенды в GGUF-списке.
        // По умолчанию скрываем, чтобы не отображались заглушки/недоступные ноды.
        showUnhealthy: false,
        // Detail data for selected backend
        detailPane: 'about', // 'about' | 'models' | 'downloads' | 'hf' | 'settings'
        detailLoading: false,
        detailError: null,
        workerInfo: null,
        gpuInfo: null,
        localModels: [],
        loadedModels: [],
        // Runtime-параметры (n_ctx, gpu_layers, batch_size, flash_attn, n_layers, n_embd,
        // gguf_context_length) реально загруженных моделей. Загружаются параллельно
        // с loadedModels из /api/v1/cppworker/config/runtime. Используются в
        // renderLoadedPane() чтобы показать «default: 8192, runtime: 32768».
        runtimeModels: {},
        // === Round 26 v0.5.13: Active queries per model ===
        // Polling /api/models/active-queries каждые 3s для отображения
        // busy badge "🔴 Generating (N active)" на loaded model card.
        // Помогает UX: пользователь видит, почему apply ждёт, и не
        // путает это с "настройки заблокированы".
        activeQueries: {}, // key: model name, value: number
        _activeQueriesTimer: null,
        activeDownloads: [],
        downloadProgress: {},
        // HF search within the detail view
        hfSearchQuery: '',
        hfSearchResults: [],
        hfSearchSelected: null,
        hfModelFiles: [],
        hfSearching: false,
        // Settings (load options) — per-backend but stored globally
        loadOptions: {
            gpuLayers: -1,
            ctxSize: 2048,
            batchSize: 512,
            flashAttn: false,
            numa: false,
            useMmap: true,
            tensorSplit: null,
            autoGpuDistribution: true,
            strategy: 'vram-ratio'
        },
        // Reference на текущую DOM-панель для делегированного обработчика кликов.
        // Нужно, чтобы onDetailPanelClick() работал даже после refreshDetailPanel(),
        // который пересоздаёт содержимое панели (но не сам узел #ggufDetailPanel —
        // в данный момент узел не пересоздаётся, но на будущее держим ссылку).
        _detailPanel: null
    };

    // Статусы, которые считаем «нерабочими» и по умолчанию скрываем в GGUF.
    var UNHEALTHY_STATUSES = { unhealthy: true, offline: true, draining: true, ollama_unavailable: true };

    function isHealthyBackend(b) {
        return !UNHEALTHY_STATUSES[b.status];
    }

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

    function renderBackendsPanel() {
        return '' +
            '<aside class="gguf-backends-panel">' +
                '<div class="gguf-backends-panel-header">' +
                    '<h3><i class="fas fa-network-wired"></i> ' + _('gguf.registered_backends') + '</h3>' +
                    '<button class="gguf-link-button" id="ggufRefreshBackends" title="' + _('gguf.refresh_backend') + '"><i class="fas fa-sync"></i></button>' +
                '</div>' +
                '<div class="gguf-backend-list" id="ggufBackendList">' +
                    renderBackendsList() +
                '</div>' +
                '<div style="margin-top:auto;padding-top:8px;border-top:1px solid var(--border);">' +
                    '<label style="display:flex;align-items:center;gap:6px;padding:6px 4px;font-size:12px;cursor:pointer;margin-bottom:8px;">' +
                        '<input type="checkbox" id="ggufShowUnhealthy" ' + (state.showUnhealthy ? 'checked' : '') + '>' +
                        '<span>' + (_('gguf.show_unhealthy') || 'Показать недоступные') + '</span>' +
                    '</label>' +
                    '<button class="btn btn-sm btn-secondary" id="ggufOpenConnectModal" style="width:100%;justify-content:center;">' +
                        '<i class="fas fa-plug"></i> ' + _('gguf.alternate_url_btn') +
                    '</button>' +
                '</div>' +
            '</aside>';
    }

    function renderBackendsList() {
        if (!state.backendDataLoaded) {
            return '<div class="gguf-empty-state">' + _('gguf.loading_backends') + '</div>';
        }
        var visible = visibleBackends();
        if (!visible || visible.length === 0) {
            return '<div class="gguf-empty-state">' + _('gguf.no_registered_backends') + '</div>';
        }
        return visible.map(function (b) {
            const isActive = state.selectedBackendId === b.id;
            const statusClass = b.status === 'healthy' ? 'connected' :
                (b.status === 'offline' ? 'disconnected' : 'warn');
            const statusLabel = b.status === 'healthy' ? _('gguf.backend_healthy') :
                (b.status === 'offline' ? _('gguf.backend_unreachable') : (b.status || '-'));
            const modelCount = (b.models && b.models.length) || 0;
            // ==== Sidebar loading indicator (Issue: «отображение состояния загрузки») ====
            // Если у бэкенда есть модели в state.loadingModels, показываем их под именем
            // со спиннером и elapsed-таймером: «⟳ Загружается model-name 12s».
            let loadingHtml = '';
            const loadingArr = (state.loadingModels && state.loadingModels[b.id]) || [];
            if (loadingArr.length > 0) {
                loadingHtml = loadingArr.map(function (lm) {
                    const startedAt = lm.loadingStartedAt ? new Date(lm.loadingStartedAt).getTime() : Date.now();
                    const elapsedMs = (lm.elapsedMs && lm.elapsedMs > 0)
                        ? lm.elapsedMs
                        : (Date.now() - startedAt);
                    const elapsedSec = Math.max(0, Math.floor(elapsedMs / 1000));
                    let elapsedLabel;
                    if (elapsedSec < 60) elapsedLabel = elapsedSec + 's';
                    else elapsedLabel = Math.floor(elapsedSec / 60) + 'm ' + (elapsedSec % 60) + 's';
                    let stateIcon, stateColor, stateLabel;
                    if (lm.state === 'error') {
                        stateIcon = 'fa-times-circle';
                        stateColor = 'var(--danger, #d9534f)';
                        stateLabel = _('gguf.load_error_short') || 'Load error';
                    } else {
                        stateIcon = 'fa-spinner fa-spin';
                        stateColor = 'var(--accent, #4a9eff)';
                        stateLabel = _('gguf.loading_indicator') || 'Loading';
                    }
                    return '<div class="gguf-backend-loading-row" style="display:flex;align-items:center;gap:6px;margin-top:4px;padding:3px 6px;background:rgba(74,158,255,0.08);border-radius:4px;font-size:11px;color:' + stateColor + ';" title="' + Utils.escapeHtml(lm.name) + '">' +
                        '<i class="fas ' + stateIcon + '" style="font-size:10px;flex-shrink:0;"></i>' +
                        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">' +
                            stateLabel + ': ' + Utils.escapeHtml(lm.name) +
                        '</span>' +
                        '<span style="font-family:monospace;font-size:10px;opacity:0.85;">' + elapsedLabel + '</span>' +
                    '</div>';
                }).join('');
            }
            return '' +
                '<div class="gguf-backend-item ' + (isActive ? 'active' : '') + '" data-backend-id="' + Utils.escapeHtml(b.id) + '">' +
                    '<div class="gguf-backend-item-header">' +
                        '<span class="gguf-status-dot ' + statusClass + '"></span>' +
                        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">' + Utils.escapeHtml(b.id) + '</span>' +
                        '<span class="badge" style="background:var(--llamacpp-badge);font-size:10px;padding:2px 6px;">cpp</span>' +
                    '</div>' +
                    '<div class="gguf-backend-item-meta">' +
                        '<span class="gguf-backend-item-status">' + statusLabel + '</span>' +
                        '<span><i class="fas fa-cube"></i> ' + modelCount + '</span>' +
                    '</div>' +
                    (loadingHtml ? '<div class="gguf-backend-loading-list">' + loadingHtml + '</div>' : '') +
                '</div>';
        }).join('');
    }

    // ---- Right panel: detail ----

    function renderDetailPanel() {
        if (!state.selectedBackendId) {
            return renderEmptyDetail();
        }
        const backend = currentBackend();
        if (!backend) {
            return renderEmptyDetail();
        }
        return '' +
            renderDetailHeader(backend) +
            renderDetailTabs() +
            '<div class="gguf-detail-content">' +
                renderDetailPane() +
            '</div>';
    }

    function renderEmptyDetail() {
        return '' +
            '<div class="gguf-detail-header">' +
                '<h2 class="gguf-detail-title"><i class="fas fa-cube"></i> ' + _('gguf.master_title') + '</h2>' +
            '</div>' +
            '<div class="gguf-detail-content">' +
                '<div class="gguf-empty-detail">' +
                    '<div class="empty-icon"><i class="fas fa-arrow-left"></i></div>' +
                    '<h3>' + _('gguf.no_backend_selected') + '</h3>' +
                    '<p>' + _('gguf.select_backend_prompt') + '</p>' +
                '</div>' +
            '</div>';
    }

    function renderDetailHeader(backend) {
        const statusClass = backend.status === 'healthy' ? 'connected' :
            (backend.status === 'offline' ? 'disconnected' : 'warn');
        const statusLabel = backend.status === 'healthy' ? _('gguf.backend_healthy') :
            (backend.status === 'offline' ? _('gguf.backend_unreachable') : (backend.status || '-'));
        const url = GgufApi.buildBackendWorkerUrl(backend);
        return '' +
            '<div class="gguf-detail-header">' +
                '<div class="gguf-detail-header-info">' +
                    '<h2 class="gguf-detail-title">' +
                        '<span class="gguf-status-dot ' + statusClass + '"></span>' +
                        Utils.escapeHtml(backend.id) +
                    '</h2>' +
                    '<div class="gguf-url-block" title="' + Utils.escapeHtml(url) + '">' +
                        '<i class="fas fa-link"></i>' +
                        '<span class="url-text">' + Utils.escapeHtml(url) + '</span>' +
                        '<button class="gguf-link-button" id="ggufCopyUrl" title="' + _('gguf.copy_url') + '"><i class="fas fa-copy"></i></button>' +
                    '</div>' +
                '</div>' +
                '<div class="gguf-detail-actions">' +
                    '<button class="btn btn-sm btn-secondary" id="ggufRefreshDetail"><i class="fas fa-sync"></i> ' + _('gguf.refresh_backend') + '</button>' +
                '</div>' +
            '</div>';
    }

    function renderDetailTabs() {
        const tabs = [
            { id: 'about',     icon: 'fas fa-info-circle',    label: _('gguf.tab_about') },
            { id: 'models',    icon: 'fas fa-cube',           label: _('gguf.tab_models') },
            { id: 'loaded',    icon: 'fas fa-brain',          label: _('gguf.tab_loaded') },
            { id: 'downloads', icon: 'fas fa-download',       label: _('gguf.tab_downloads') },
            { id: 'hf',        icon: 'fab fa-huggingface',    label: _('gguf.tab_hf') },
            { id: 'settings',  icon: 'fas fa-cog',            label: _('gguf.tab_settings') }
        ];
        return '<div class="gguf-detail-tabs">' +
            tabs.map(function (t) {
                return '<button class="gguf-detail-tab ' + (state.detailPane === t.id ? 'active' : '') + '" data-detail-tab="' + t.id + '">' +
                    '<i class="' + t.icon + '"></i> ' + t.label +
                '</button>';
            }).join('') +
        '</div>';
    }

    function renderDetailPane() {
        if (state.detailLoading) {
            return '<div class="gguf-empty-state"><i class="fas fa-spinner fa-spin"></i> ' + _('common.loading') + '</div>';
        }
        if (state.detailError) {
            return '<div class="gguf-empty-state">' +
                '<i class="fas fa-exclamation-triangle"></i> ' + state.detailError +
            '</div>';
        }
        switch (state.detailPane) {
            case 'about':     return renderAboutPane();
            case 'models':    return renderModelsPane();
            case 'loaded':    return renderLoadedPane();
            case 'downloads': return renderDownloadsPane();
            case 'hf':        return renderHuggingFacePane();
            case 'settings':  return renderSettingsPane();
            default:          return renderAboutPane();
        }
    }

    function renderAboutPane() {
        const backend = currentBackend();
        if (!backend) return '';
        const gpus = renderGpuInfoCards();
        const worker = renderWorkerInfoCard();
        return '<div class="gguf-about-grid">' +
            '<div>' +
                '<div class="gguf-about-section">' +
                    '<h4><i class="fas fa-microchip"></i> ' + _('gguf.gpu_info') + '</h4>' +
                    gpus +
                '</div>' +
            '</div>' +
            '<div>' +
                '<div class="gguf-about-section">' +
                    '<h4><i class="fas fa-server"></i> ' + _('gguf.worker_info') + '</h4>' +
                    worker +
                '</div>' +
                '<div class="gguf-about-section" style="margin-top:12px;">' +
                    '<h4><i class="fas fa-cube"></i> ' + _('gguf.models_on_disk') + '</h4>' +
                    '<div class="gguf-stat-value">' +
                        (state.localModels ? state.localModels.length : 0) +
                        ' <span class="gguf-stat-value muted">' + _('gguf.about_models_total') + '</span>' +
                    '</div>' +
                '</div>' +
            '</div>' +
        '</div>';
    }

    function renderGpuInfoCards() {
        if (state.gpuInfo) {
            const devices = state.gpuInfo.devices || (Array.isArray(state.gpuInfo) ? state.gpuInfo : [state.gpuInfo]);
            if (devices.length > 0) {
                return '<div class="gguf-stats-grid">' +
                    devices.map(function (gpu) {
                        const name = gpu.name || gpu.brand || gpu.model || '-';
                        const memTotal = gpu.memoryTotal || gpu.totalMemory || 0;
                        const memFree = gpu.memoryFree || gpu.freeMemory || 0;
                        const memUsed = (memTotal && memFree) ? (memTotal - memFree) : (gpu.memoryUsed || 0);
                        const util = gpu.utilization || gpu.usagePercent || 0;
                        return '<div class="gguf-stat-card">' +
                            '<div class="gguf-stat-label">' + Utils.escapeHtml(name) + '</div>' +
                            '<div class="gguf-stat-value">' + formatFileSize(memTotal) + '</div>' +
                            '<div class="gguf-stat-label" style="margin-top:6px;">' + _('common.free') + '</div>' +
                            '<div class="gguf-stat-value muted">' + formatFileSize(memFree) + '</div>' +
                            '<div class="gguf-stat-label" style="margin-top:6px;">' + _('metrics.gpu_util') + '</div>' +
                            '<div class="gguf-stat-value muted">' + Math.round(util) + '%</div>' +
                        '</div>';
                    }).join('') +
                '</div>';
            }
        }
        return '<div class="gguf-stat-value muted">' + _('gguf.no_gpu') + '</div>';
    }

    function renderWorkerInfoCard() {
        if (state.workerInfo) {
            const version = state.workerInfo.version || state.workerInfo.llamaVersion || '-';
            const gpuCount = state.gpuInfo ? (state.gpuInfo.count || (Array.isArray(state.gpuInfo) ? state.gpuInfo.length : 1)) : 0;
            const loadedCount = state.loadedModels ? state.loadedModels.length : 0;
            return '<div class="gguf-stats-grid">' +
                '<div class="gguf-stat-card">' +
                    '<div class="gguf-stat-label">' + _('gguf.version') + '</div>' +
                    '<div class="gguf-stat-value">' + Utils.escapeHtml(version) + '</div>' +
                '</div>' +
                '<div class="gguf-stat-card">' +
                    '<div class="gguf-stat-label">' + _('gguf.gpu_count') + '</div>' +
                    '<div class="gguf-stat-value">' + gpuCount + '</div>' +
                '</div>' +
                '<div class="gguf-stat-card">' +
                    '<div class="gguf-stat-label">' + _('gguf.loaded_models_header') + '</div>' +
                    '<div class="gguf-stat-value">' + loadedCount + '</div>' +
                '</div>' +
            '</div>';
        }
        return '<div class="gguf-stat-value muted">' + _('common.disconnected') + '</div>';
    }

    function renderModelsPane() {
        const models = state.localModels || [];
        if (models.length === 0) {
            return '<div class="gguf-empty-state">' +
                '<div class="empty-icon"><i class="fas fa-folder-open"></i></div>' +
                '<div>' + _('gguf.no_local_models') + '</div>' +
                '<p style="font-size:12px;margin-top:8px;">' + _('gguf.no_models_on_disk_hint') + '</p>' +
            '</div>';
        }
        return models.map(function (m, idx) {
            const name = m.name || m.filename || m.path || '-';
            const size = m.size ? formatFileSize(m.size) : '-';
            const quant = m.quantization || m.quant || '-';
            // Round 24 (2026-08-04) Bug #2 fix: isLoaded check was unreliable.
            // Old check `(lm.model || lm.name) === name || lm.path === m.path` failed
            // when user loaded via API (e.g., balancer auto-warmup) with name without
            // .gguf extension: lm.name="Qwen3-Instruct-2507-q4km" vs
            // m.name="Qwen3-Instruct-2507-q4km.gguf" → no match → state shows
            // "Load" button for already-loaded model. Or reverse: stale loadedModels
            // after switch backends → "Unload" button for unloaded model.
            //
            // Robust check: compare normalized names (strip .gguf) AND check if
            // lm.path basename matches m.name (when lm.path is available).
            const nameNoExt = stripGGUF(name);
            const isLoaded = state.loadedModels.some(function (lm) {
                const lmName = lm.model || lm.name || '';
                // 1) Exact match (handles case where both have .gguf or both don't).
                if (lmName && lmName === name) return true;
                // 2) Normalized match (strip .gguf from both sides).
                if (lmName && stripGGUF(lmName) === nameNoExt) return true;
                // 3) Path basename match (m.name is a basename from /api/models/files;
                //    lm.path is full path from /api/models — compare basenames).
                if (lm.path) {
                    const lmPathBase = lm.path.split(/[\\/]/).pop();
                    if (lmPathBase === name || stripGGUF(lmPathBase) === nameNoExt) {
                        return true;
                    }
                }
                return false;
            });
            return '<div class="gguf-local-card">' +
                '<div class="gguf-local-icon"><i class="fas fa-cube"></i></div>' +
                '<div class="gguf-local-info">' +
                    '<div class="gguf-local-name">' + Utils.escapeHtml(name) + '</div>' +
                    '<div class="gguf-local-meta">' +
                        '<span>' + _('gguf.model_size') + ': ' + size + '</span>' +
                        '<span>' + _('gguf.quantization') + ': ' + quant + '</span>' +
                        (isLoaded ? '<span class="badge badge-success">' + _('gguf.tab_loaded') + '</span>' : '') +
                    '</div>' +
                '</div>' +
                '<div class="gguf-local-actions" style="display:flex;gap:6px;flex-wrap:wrap;">' +
                    (isLoaded
                        ? '<button class="btn btn-sm btn-danger gguf-unload-btn" data-handle="' + Utils.escapeHtml(name) + '">' +
                            '<i class="fas fa-stop"></i> ' + _('gguf.unload_model') +
                          '</button>'
                        : '<button class="btn btn-sm btn-primary gguf-load-btn" data-idx="' + idx + '">' +
                            '<i class="fas fa-play"></i> ' + _('gguf.load_model') +
                          '</button>') +
                    '<button class="btn btn-sm btn-secondary gguf-delete-btn" data-idx="' + idx + '" title="' + _('gguf.delete_model') + '">' +
                        '<i class="fas fa-trash"></i>' +
                    '</button>' +
                '</div>' +
            '</div>';
        }).join('');
    }

    function renderLoadedPane() {
        const models = state.loadedModels || [];
        const loadingArr = (state.selectedBackendId && state.loadingModels && state.loadingModels[state.selectedBackendId]) || [];
        if (models.length === 0 && loadingArr.length === 0) {
            return '<div class="gguf-empty-state">' +
                '<div class="empty-icon"><i class="fas fa-brain"></i></div>' +
                '<div>' + _('gguf.no_loaded_models') + '</div>' +
            '</div>';
        }
        // ==== Loading models block (Issue: «отображение состояния загрузки») ====
        // Рисуется ПЕРЕД списком загруженных моделей, чтобы пользователь видел,
        // что идёт загрузка. Каждая модель — карточка с индикатором прогресса и elapsed.
        const loadingHtml = loadingArr.map(function (lm) {
            const startedAt = lm.loadingStartedAt ? new Date(lm.loadingStartedAt).getTime() : Date.now();
            const elapsedMs = (lm.elapsedMs && lm.elapsedMs > 0) ? lm.elapsedMs : (Date.now() - startedAt);
            const elapsedSec = Math.max(0, Math.floor(elapsedMs / 1000));
            const elapsedLabel = elapsedSec < 60
                ? elapsedSec + 's'
                : Math.floor(elapsedSec / 60) + 'm ' + (elapsedSec % 60) + 's';
            let stateIcon, stateColor, stateText;
            if (lm.state === 'error') {
                stateIcon = 'fa-times-circle';
                stateColor = 'var(--danger, #d9534f)';
                stateText = (_('gguf.load_error_short') || 'Load error');
            } else {
                stateIcon = 'fa-spinner fa-spin';
                stateColor = 'var(--accent, #4a9eff)';
                stateText = (_('gguf.loading_indicator') || 'Loading');
            }
            return '<div class="gguf-loaded-model-item gguf-loaded-model-loading" style="border-left:3px solid ' + stateColor + ';">' +
                '<div class="gguf-loaded-model-name">' +
                    '<i class="fas ' + stateIcon + '" style="color:' + stateColor + ';margin-right:6px;"></i>' +
                    Utils.escapeHtml(lm.name) +
                '</div>' +
                '<div class="gguf-loaded-model-info">' +
                    '<span style="color:' + stateColor + ';">' + stateText + '</span>' +
                    '<span style="margin-left:12px;">' + (_('gguf.elapsed') || 'Elapsed') + ': ' + elapsedLabel + '</span>' +
                '</div>' +
                (lm.error ? '<div class="gguf-loaded-model-info" style="color:var(--danger,#d9534f);font-size:11px;">' +
                    '<i class="fas fa-exclamation-triangle"></i> ' + Utils.escapeHtml(lm.error) +
                '</div>' : '') +
            '</div>';
        }).join('');
        const loadedHtml = models.map(function (m) {
            const name = m.model || m.name || m.path || '-';
            // Ищем runtime-параметры (n_ctx, gpu_layers) этой модели.
            // Ключ ищем по имени или по path, потому что cppworker может вернуть
            // «gemma-4.gguf» а loadedModels — «gemma-4» (без расширения).
            const rt = (state.runtimeModels && (state.runtimeModels[name] || state.runtimeModels[(m.path || '').split(/[\\/]/).pop()] || state.runtimeModels[m.path])) || null;
            const ctx = m.ctxSize || m.contextSize || '-';
            const vram = m.vramBytes ? formatFileSize(m.vramBytes) : (m.vramUsage ? m.vramUsage + ' MB' : '-');
            // Runtime-блок (показываем рядом с default если rt есть).
            // Поля из cppworker: context_size, gpu_layers, batch_size, flash_attn_type,
            // gguf_context_length, n_layers, n_embd, state.
            let rtHtml = '';
            if (rt) {
                const rtCtx = rt.context_size || 0;
                const rtGpu = (rt.gpu_layers !== undefined) ? rt.gpu_layers : null;
                const rtBatch = rt.batch_size || 0;
                const rtFa = rt.flash_attn_type;
                const rtLayers = rt.n_layers || 0;
                const rtState = rt.state || 'loaded';
                const rtCtxBadge = (rtCtx && rtCtx !== ctx) ? '<span class="badge" style="background:var(--accent);margin-left:6px;">runtime: ' + Utils.escapeHtml(String(rtCtx)) + '</span>' : '';
                const rtGpuBadge = (rtGpu !== null && rtGpu !== undefined && rtGpu !== m.gpuLayers) ?
                    '<span class="badge" style="background:var(--bg-tertiary);margin-left:6px;padding:2px 6px;font-size:10px;">gpu: ' + Utils.escapeHtml(String(rtGpu)) + '</span>' : '';
                const meta = [];
                if (rtCtx) meta.push('<span title="Runtime n_ctx">ctx=' + Utils.escapeHtml(String(rtCtx)) + '</span>');
                if (rtGpu !== null && rtGpu !== undefined) meta.push('<span title="Runtime GPU layers">gpu_layers=' + Utils.escapeHtml(String(rtGpu)) + '</span>');
                if (rtBatch) meta.push('<span title="Runtime batch size">batch=' + Utils.escapeHtml(String(rtBatch)) + '</span>');
                if (rtFa !== undefined && rtFa !== null) meta.push('<span title="Flash attention type">fa=' + Utils.escapeHtml(String(rtFa)) + '</span>');
                if (rtLayers) meta.push('<span title="Model layers">layers=' + Utils.escapeHtml(String(rtLayers)) + '</span>');
                if (rt.gguf_context_length && rt.gguf_context_length !== rtCtx) {
                    meta.push('<span title="Max n_ctx per GGUF metadata">gguf_max=' + Utils.escapeHtml(String(rt.gguf_context_length)) + '</span>');
                }
                if (meta.length > 0) {
                    rtHtml = '<div class="gguf-loaded-model-info" style="margin-top:4px;display:flex;gap:12px;flex-wrap:wrap;font-family:monospace;font-size:11px;">' +
                        meta.join('') +
                        '</div>';
                }
            }
            // Round 26 v0.5.13: busy badge. Если activeQueries > 0, показываем
            // "🔴 Generating (N active)" inline рядом с именем модели. Это
            // разъясняет пользователю, почему apply может ждать.
            //
            // Round 32 #2 (2026-08-10): busy badge теперь имеет встроенную кнопку
            // "Cancel" — клик вызывает cppworkerCancelGeneration.post(backend.id, name),
            // что отправляет POST /api/cancel в cppworker → AbortWatcher →
            // bridge.RequestAbort → C-bridge прерывает текущий llama_decode при
            // следующей проверке abort флага (каждые ~100ms после Round 32 n_batch=64).
            // Без этой кнопки пользователь не мог отменить генерацию из WebUI —
            // приходилось закрывать чат-клиент (Cline/OpenWebUI) чтобы balancer
            // увидел TCP close.
            const activeCount = (state.activeQueries && state.activeQueries[name]) || 0;
            const busyBadge = activeCount > 0
                ? '<span class="gguf-loaded-busy-badge" style="display:inline-flex;align-items:center;gap:6px;margin-left:8px;padding:2px 8px;background:rgba(217,83,79,0.15);color:#ff7b76;border-radius:10px;font-size:11px;font-weight:600;" title="' + (_('gguf.busy_badge_title') || 'Active generations') + '">' +
                    '<span style="display:inline-block;width:6px;height:6px;background:#ff7b76;border-radius:50%;animation:gguf-pulse 1.2s infinite;"></span>' +
                    (_('gguf.busy_badge') || 'Generating') + ' (' + activeCount + ' ' + (_('gguf.active_short') || 'active') + ')' +
                    // Round 32 #2: cancel button внутри busy badge.
                    // data-model-name используется в event handler для определения
                    // какую модель отменять.
                    '<button class="btn btn-xs gguf-cancel-gen-btn" data-model-name="' + Utils.escapeHtml(name) + '" ' +
                            'style="margin-left:4px;padding:1px 8px;font-size:10px;line-height:1.4;background:#d9534f;color:#fff;border:none;border-radius:8px;cursor:pointer;" ' +
                            'title="' + (_('gguf.cancel_generation') || 'Cancel active generation') + '">' +
                        '<i class="fas fa-times"></i> ' + (_('common.cancel') || 'Cancel') +
                    '</button>' +
                  '</span>'
                : '';

            return '<div class="gguf-loaded-model-item">' +
                '<div class="gguf-loaded-model-name">' + Utils.escapeHtml(name) + busyBadge + '</div>' +
                '<div class="gguf-loaded-model-info">' +
                    '<span>' + _('gguf.ctx_size') + ': ' + ctx + '</span>' +
                    '<span style="margin-left:12px;">VRAM: ' + vram + '</span>' +
                '</div>' +
                rtHtml +
                '<button class="btn btn-sm btn-danger gguf-unload-btn" data-handle="' + Utils.escapeHtml(m.handle || name) + '">' +
                    '<i class="fas fa-stop"></i> ' + _('gguf.unload_model') +
                '</button>' +
            '</div>';
        }).join('');
        return (loadingHtml ? '<div class="gguf-loaded-loading-block">' + loadingHtml + '</div>' : '') + loadedHtml;
    }

    function renderDownloadsPane() {
        const dls = state.activeDownloads || [];
        const history = state.downloadHistory || [];
        const activeHtml = dls.length === 0
            ? ('<div class="gguf-empty-state" style="margin-bottom:16px;">' +
                '<div class="empty-icon"><i class="fas fa-download"></i></div>' +
                '<div>' + _('gguf.no_active_downloads') + '</div>' +
               '</div>')
            : ('<div class="gguf-downloads-section">' +
                '<h5 style="margin:0 0 8px 0;font-size:13px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px;">' +
                    _('gguf.active_downloads_for_backend') + '</h5>' +
                dls.map(renderDownloadItem).join('') +
               '</div>');

        const historyItems = history.filter(function (h) {
            // Не дублируем активные в истории
            return !dls.some(function (dl) { return dl.modelId === h.modelId && dl.filename === h.filename; });
        });
        const historyHtml = historyItems.length === 0
            ? ''
            : ('<div class="gguf-downloads-section" style="margin-top:16px;padding-top:16px;border-top:1px solid var(--border-color);">' +
                '<h5 style="margin:0 0 8px 0;font-size:13px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.5px;">' +
                    (_('gguf.download_history') || 'Recent downloads') + '</h5>' +
                historyItems.map(renderDownloadItem).join('') +
               '</div>');

        return activeHtml + historyHtml;
    }

    function renderDownloadItem(dl) {
        const progress = state.downloadProgress[dl.modelId + '/' + dl.filename];
        const pct = progress ? (progress.progressPct || progress.percent || progress.completed || 0) : (dl.progressPct || dl.ProgressPct || 0);
        const speed = progress ? (progress.speedBps || progress.speed || '') : (dl.speedBps || dl.speed || '');
        const status = progress ? (progress.status || '') : (dl.status || dl.Status || '');
        const isCompleted = status === 'completed';
        const isFailed = status === 'failed' || status === 'error';
        const isCancelled = status === 'cancelled';
        const isInterrupted = status === 'interrupted'; // Round 17.3
        const fillColor = isCompleted ? 'var(--success)' : isFailed ? 'var(--danger)' : isCancelled ? 'var(--text-muted)' : (isInterrupted ? 'var(--warning)' : 'var(--accent)');
        const totalBytes = dl.totalBytes || dl.TotalBytes || 0;
        const downloaded = dl.downloaded || dl.Downloaded || 0;
        const sizeLabel = totalBytes > 0
            ? (formatFileSize(downloaded) + ' / ' + formatFileSize(totalBytes))
            : (downloaded > 0 ? formatFileSize(downloaded) : '');
        const speedLabel = speed ? formatFileSize(speed) + '/s' : '';
        // Round 17.3 (2026-08-03): пути + resume state.
        const tempPath = progress ? (progress.tempPath || '') : (dl.tempPath || dl.TempPath || '');
        const finalPath = progress ? (progress.finalPath || '') : (dl.finalPath || dl.FinalPath || '');
        const resumable = progress ? !!progress.resumable : !!dl.resumable;
        const resumedFrom = progress ? (progress.resumedFrom || 0) : (dl.resumedFrom || 0);
        const isActive = (status === 'downloading' || status === 'queued' || status === 'starting' || status === '');

        return '<div class="gguf-download-item">' +
            '<div class="gguf-download-info">' +
                '<div class="gguf-download-model">' + Utils.escapeHtml(dl.modelId) + '</div>' +
                '<div class="gguf-download-file">' + Utils.escapeHtml(dl.filename) + '</div>' +
                // Round 17.3: показываем пути маленьким серым текстом.
                (finalPath ? '<div class="gguf-download-paths" style="font-size:10px;color:var(--text-muted);margin-top:2px;line-height:1.4;">' +
                    '<i class="fas fa-folder-open" style="margin-right:3px;"></i>' + Utils.escapeHtml(finalPath) +
                    (tempPath ? '<br><i class="fas fa-file-download" style="margin-right:3px;"></i><span style="opacity:0.8;">temp: </span>' + Utils.escapeHtml(tempPath) : '') +
                  '</div>' : '') +
                (resumable ? '<div style="font-size:10px;color:var(--warning);margin-top:2px;"><i class="fas fa-pause-circle"></i> ' +
                    (_('gguf.partial_download_resumable') || 'Partial download — can be resumed') +
                    (resumedFrom > 0 ? ' (' + formatFileSize(resumedFrom) + ' ' + (_('gguf.already_downloaded') || 'already downloaded') + ')' : '') +
                  '</div>' : '') +
            '</div>' +
            '<div class="gguf-download-progress-bar">' +
                '<div class="gguf-progress-fill" style="width:' + pct + '%;background:' + fillColor + '"></div>' +
            '</div>' +
            '<div class="gguf-download-stats">' +
                '<span>' + Math.round(pct) + '%</span>' +
                (sizeLabel ? '<span>' + sizeLabel + '</span>' : '') +
                (speedLabel ? '<span>' + speedLabel + '</span>' : '') +
                (status ? '<span style="margin-left:8px;text-transform:capitalize;">' + status + '</span>' : '') +
            '</div>' +
            '<div class="gguf-download-actions">' +
                // Cancel для активных
                (isActive
                    ? '<button class="btn btn-sm btn-danger gguf-cancel-dl-btn" data-model-id="' + Utils.escapeHtml(dl.modelId) + '" data-filename="' + Utils.escapeHtml(dl.filename) + '">' +
                        '<i class="fas fa-ban"></i> ' + _('gguf.cancel_download') +
                      '</button>'
                    : '') +
                // Round 17.3: Resume для прерванных
                (isInterrupted && resumable
                    ? '<button class="btn btn-sm btn-warning gguf-resume-dl-btn" data-model-id="' + Utils.escapeHtml(dl.modelId) + '" data-filename="' + Utils.escapeHtml(dl.filename) + '" data-quantization="' + Utils.escapeHtml(dl.quantization || '') + '">' +
                        '<i class="fas fa-play"></i> ' + (_('gguf.resume_download') || 'Resume') +
                      '</button>'
                    : '') +
                // Round 17.3: Delete для completed/failed/interrupted
                ((isCompleted || isFailed || isCancelled || isInterrupted)
                    ? '<button class="btn btn-sm btn-secondary gguf-delete-dl-btn" data-model-id="' + Utils.escapeHtml(dl.modelId) + '" data-filename="' + Utils.escapeHtml(dl.filename) + '" title="' + (_('gguf.delete_from_disk') || 'Delete downloaded file from disk') + '">' +
                        '<i class="fas fa-trash"></i> ' + (_('gguf.delete') || 'Delete') +
                      '</button>'
                    : '') +
            '</div>' +
        '</div>';
    }

    function renderHuggingFacePane() {
        return '' +
            '<div class="gguf-search-form">' +
                '<div class="gguf-search-row">' +
                    '<input type="text" id="ggufHfDetailSearch" class="form-control gguf-search-input" value="' + Utils.escapeHtml(state.hfSearchQuery) + '" placeholder="' + _('gguf.search_placeholder') + '">' +
                    '<button class="btn btn-primary" id="ggufHfDetailSearchBtn">' + _('gguf.search_btn') + '</button>' +
                '</div>' +
            '</div>' +
            '<div id="ggufHfSearchResults">' +
                renderHfSearchResults() +
            '</div>' +
            '<div id="ggufHfModelFiles" style="' + (state.hfSearchSelected ? '' : 'display:none;') + ';margin-top:16px;">' +
                renderHfModelFiles() +
            '</div>';
    }

    /**
     * Format bytes as human-readable size (KB, MB, GB, TB).
     */
    function formatBytesShort(bytes) {
        if (!bytes || bytes === 0) return '-';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = Math.floor(Math.log(bytes) / Math.log(1024));
        return (bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
    }

    /**
     * Universal field extractors for HF file records.
     * CppWorker API returns {path, sizeBytes, isGGUF, quantization};
     * raw HF API returns {rfilename, size, ...}.
     */
    function hfFileName(f) { return f.path || f.rfilename || f.filename || f.name || ''; }
    function hfFileSize(f) { return f.sizeBytes || f.size || 0; }
    function hfFileQuant(f) {
        if (f.quantization && f.quantization !== 'unknown') return f.quantization;
        var m = hfFileName(f).toUpperCase().match(/[._-](Q\d[_A-Z]*|F16|F32|FP16|FP32|BF16)[._-]/);
        return m ? m[1] : '';
    }
    function hfIsGgufFile(f) {
        if (f.isGGUF === true) return true;
        if (f.isGGUF === false) return false;
        return /\.gguf$/i.test(hfFileName(f));
    }
    /**
     * Filter out non-model auxiliary files (mmproj, etc.) and limit to top-N
     * most popular quants. Возвращает top файлов по приоритету Q4_K_M > Q5_K_M > ...
     */
    function pickTopGgufFiles(files, maxCount) {
        if (!Array.isArray(files) || files.length === 0) return [];
        var popQ = ['Q4_K_M','Q5_K_M','Q6_K','Q8_0','Q4_0','Q4_K_S','Q3_K_M','Q2_K'];
        var gguf = files.filter(function (f) {
            return hfIsGgufFile(f) && !/mmproj/i.test(hfFileName(f));
        });
        gguf = gguf.map(function (f) { f.__quant = hfFileQuant(f); return f; });
        gguf.sort(function (a, b) {
            var qa = (a.__quant || '').toUpperCase();
            var qb = (b.__quant || '').toUpperCase();
            var pa = popQ.indexOf(qa);
            var pb = popQ.indexOf(qb);
            if (pa === -1) pa = 999;
            if (pb === -1) pb = 999;
            if (pa !== pb) return pa - pb;
            return (hfFileSize(a) || 0) - (hfFileSize(b) || 0);
        });
        return gguf.slice(0, maxCount || 6);
    }

    function renderHfSearchResults() {
        if (state.hfSearching) {
            return '<div class="gguf-empty-state"><i class="fas fa-spinner fa-spin"></i> ' + _('common.loading') + '</div>';
        }
        if (!state.hfSearchResults || state.hfSearchResults.length === 0) {
            return '<div class="gguf-empty-state">' + (state.hfSearchQuery ? _('gguf.no_results') : '') + '</div>';
        }
        var filtered = state.hfSearchResults.filter(function (m) {
            return m && m.files && Array.isArray(m.files) && m.files.length > 0;
        });
        if (filtered.length === 0) {
            return '<div class="gguf-empty-state">' + (state.hfSearchQuery ? _('gguf.no_gguf_files_found') : _('gguf.no_results')) + '</div>';
        }
        var toolbar = filtered.length > 1
            ? '<div class="gguf-results-toolbar" style="display:flex;justify-content:flex-end;margin-bottom:8px;">' +
                '<button class="btn btn-sm btn-secondary gguf-collapse-all-btn" title="' + (_('gguf.collapse_all') || 'Collapse all') + '">' +
                    '<i class="fas fa-compress-arrows-alt"></i> ' + (_('gguf.collapse_all') || 'Collapse all') +
                '</button>' +
              '</div>'
            : '';
        return toolbar + '<div class="gguf-results-list">' +
            filtered.map(function (model, idx) {
                const downloads = model.downloads || 0;
                const likes = model.likes || 0;
                const updated = model.lastModified ? new Date(model.lastModified).toLocaleDateString() : '-';
                const modelId = model.id || model.modelId || '';
                const isLikelyGGUF = /gguf/i.test(modelId) || (model.hasGguf === true);
                const totalSize = model.totalSize || 0;
                const recommended = model.recommended || '';
                const topFiles = pickTopGgufFiles(model.files, 5);
                const remaining = (model.files || []).length - topFiles.length;

                const filesHtml = (function () {
                    if (topFiles.length === 0) return '';
                    var html = '<div class="gguf-files-list" style="margin-top:10px;border-top:1px solid var(--border);padding-top:8px;">';
                    topFiles.forEach(function (f) {
                        var fname = hfFileName(f);
                        var fsize = formatBytesShort(hfFileSize(f));
                        var fquant = hfFileQuant(f) || '-';
                        var isRecommended = recommended && fname === recommended;
                        html += '<div class="gguf-file-row" data-model-idx="' + idx + '" data-filename="' + Utils.escapeHtml(fname) + '" style="display:flex;align-items:center;gap:8px;padding:6px 4px;border-bottom:1px solid var(--border-subtle,#eee);">' +
                            '<i class="fas fa-cube" style="color:var(--text-muted);"></i>' +
                            '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-family:monospace;font-size:12px;" title="' + Utils.escapeHtml(fname) + '">' + Utils.escapeHtml(fname) + '</span>' +
                            '<span class="badge" style="background:var(--accent);font-size:10px;">' + Utils.escapeHtml(fquant) + '</span>' +
                            '<span style="font-size:11px;color:var(--text-muted);min-width:60px;text-align:right;">' + fsize + '</span>' +
                            (isRecommended ? '<span class="badge" style="background:var(--success);font-size:9px;" title="' + (_('gguf.recommended') || 'Recommended') + '">' + (_('gguf.recommended') || '★') + '</span>' : '') +
                            '<button class="btn btn-sm btn-primary gguf-download-file-btn" data-model-idx="' + idx + '" data-filename="' + Utils.escapeHtml(fname) + '" title="' + (_('gguf.download') || 'Download') + ': ' + Utils.escapeHtml(fname) + '" style="padding:2px 8px;font-size:12px;">' +
                                '<i class="fas fa-download"></i>' +
                            '</button>' +
                            '<div class="gguf-file-progress" data-model-idx="' + idx + '" data-filename="' + Utils.escapeHtml(fname) + '" style="display:none;flex-basis:100%;margin-top:4px;">' +
                                '<div class="gguf-progress-bar" style="height:4px;background:var(--bg-tertiary,#333);border-radius:2px;overflow:hidden;">' +
                                    '<div class="gguf-progress-fill" style="height:100%;width:0%;background:var(--success);transition:width 0.2s;"></div>' +
                                '</div>' +
                                '<div class="gguf-progress-text" style="font-size:10px;color:var(--text-muted);margin-top:2px;">0%</div>' +
                            '</div>' +
                        '</div>';
                    });
                    if (remaining > 0) {
                        html += '<div class="gguf-files-more" style="padding:4px;text-align:center;font-size:11px;color:var(--text-muted);">' +
                            '+' + remaining + ' ' + (_('gguf.more_files') || 'more files') +
                        '</div>';
                    }
                    html += '</div>';
                    return html;
                })();

                return '<div class="gguf-result-card" data-model-idx="' + idx + '">' +
                    '<div class="gguf-result-header" style="cursor:pointer;">' +
                        '<div class="gguf-result-title">' +
                            '<i class="fas fa-cube"></i> ' + Utils.escapeHtml(modelId) +
                        '</div>' +
                        '<div class="gguf-result-meta">' +
                            '<span title="' + _('gguf.downloads') + '"><i class="fas fa-download"></i> ' + downloads.toLocaleString() + '</span>' +
                            '<span title="' + _('gguf.likes') + '"><i class="fas fa-heart"></i> ' + likes.toLocaleString() + '</span>' +
                            '<span title="' + _('gguf.last_updated') + '"><i class="fas fa-calendar"></i> ' + updated + '</span>' +
                            (isLikelyGGUF ? '<span class="badge badge-info" title="GGUF"><i class="fas fa-cube"></i> GGUF</span>' : '') +
                            (totalSize > 0 ? '<span title="' + (_('gguf.total_size') || 'Total size') + '"><i class="fas fa-hdd"></i> ' + formatBytesShort(totalSize) + '</span>' : '') +
                        '</div>' +
                    '</div>' +
                    filesHtml +
                    '<div class="gguf-result-actions" style="display:flex;gap:6px;flex-wrap:wrap;margin-top:8px;">' +
                        '<button class="btn btn-sm btn-success gguf-quick-download-btn" data-model-idx="' + idx + '" title="' + (_('gguf.quick_download_tooltip') || 'Auto-pick best .gguf and start download') + '">' +
                            '<i class="fas fa-bolt"></i> ' + (_('gguf.download') || 'Download') + ' (' + (_('gguf.recommended') || 'Recommended') + ')' +
                        '</button>' +
                    '</div>' +
                '</div>';
            }).join('') +
        '</div>';
    }

    function renderHfModelFiles() {
        if (!state.hfSearchSelected) return '';
        const modelId = state.hfSearchSelected.id || state.hfSearchSelected.modelId || '';
        const files = state.hfModelFiles || [];
        function fileName(f) { return f.path || f.rfilename || f.filename || f.name || ''; }
        function fileSize(f) { return f.sizeBytes || f.size || 0; }
        function isGgufFile(f) {
            if (f.isGGUF === true) return true;
            if (f.isGGUF === false) return false;
            return /\.gguf$/i.test(fileName(f));
        }
        return '' +
            '<div class="gguf-files-header" style="display:flex;justify-content:space-between;align-items:center;">' +
                '<h4 style="margin:0;font-size:14px;"><i class="fas fa-file"></i> ' + Utils.escapeHtml(modelId) + '</h4>' +
                '<button class="btn btn-sm btn-secondary" id="ggufCloseHfFiles"><i class="fas fa-times"></i> ' + _('common.close') + '</button>' +
            '</div>' +
            '<div class="gguf-files-grid">' +
                (files.length === 0
                    ? '<div class="gguf-empty-state">' + _('gguf.no_results') + '</div>'
                    : files.map(function (file) {
                        const fname = fileName(file);
                        const size = fileSize(file) ? formatFileSize(fileSize(file)) : '-';
                        const gguf = isGgufFile(file);
                        return '<div class="gguf-file-card">' +
                            '<div class="gguf-file-icon"><i class="fas fa-' + (gguf ? 'file' : 'file-lines') + '"></i></div>' +
                            '<div class="gguf-file-info">' +
                                '<div class="gguf-file-name">' + Utils.escapeHtml(fname) + '</div>' +
                                '<div class="gguf-file-size">' + size + '</div>' +
                            '</div>' +
                            '<div class="gguf-file-actions">' +
                                (gguf
                                    ? '<button class="btn btn-sm btn-primary gguf-hf-download-btn" data-filename="' + Utils.escapeHtml(fname) + '" data-model-id="' + Utils.escapeHtml(modelId) + '">' +
                                        '<i class="fas fa-download"></i> ' + _('gguf.download') +
                                      '</button>'
                                    : '') +
                            '</div>' +
                        '</div>';
                    }).join('')) +
            '</div>';
    }

    /**
     * Settings tab в gguf-рендерере.
     *
     * Раньше здесь была «мёртвая» форма gguf-settings-form, которая меняла только
     * локальный state.loadOptions и не отправляла ничего на бэкенд. Теперь таб
     * показывает два осмысленных блока, привязанных к выбранному бэкенду:
     *
     *   1) Backend load options — реальные llama.cpp дефолты cppworker'а.
     *      Загружаются и сохраняются через /api/v1/cppworker/config[+/update].
     *
     *   2) Per-Model Profiles — список профилей n_ctx, общий для всех
     *      зарегистрированных cppworker'ов (эндпоинт /api/v1/cppworker/model-profiles
     *      на балансировщике). Использует уже существующий модуль window.CppWorkerParams.
     */
    function renderSettingsPane() {
        const backend = currentBackend();
        const backendId = backend ? Utils.escapeHtml(backend.id) : '';
        const isRegistered = !!(backend && backend.url && backend.url.indexOf('http') === 0 && backend.id && /^(?:[a-z0-9_-]+)$/i.test(backend.id));
        return '' +
            '<div class="gguf-settings-form">' +

            // ---------- (1) Backend load options ----------
            '<div class="settings-subsection" id="gguf-backend-options-section">' +
                '<h4 style="margin:0 0 6px 0;font-size:14px;display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-sliders-h"></i> ' +
                    '<span>' + _('gguf.backend_options_title') + '</span>' +
                '</h4>' +
                '<p style="color:var(--text-muted);margin:0 0 12px 0;font-size:12px;">' +
                    _('gguf.backend_options_desc') +
                '</p>' +
                '<div id="ggufBackendOptionsContainer" data-backend-id="' + backendId + '">' +
                    '<div class="loading"><i class="fas fa-spinner fa-spin"></i> ' + (window.I18N ? I18N.t('common.loading', 'Loading…') : 'Loading…') + '</div>' +
                '</div>' +
            '</div>' +

            // ---------- (2) Per-Model Profiles ----------
            '<div class="settings-subsection" id="gguf-profiles-section" style="margin-top:24px;padding-top:16px;border-top:1px solid var(--border-color, #2a3441);">' +
                '<h4 style="margin:0 0 6px 0;font-size:14px;display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-layer-group"></i> ' +
                    '<span>' + _('gguf.profiles_section_title') + '</span>' +
                '</h4>' +
                '<p style="color:var(--text-muted);margin:0 0 12px 0;font-size:12px;">' +
                    _('gguf.profiles_section_desc') +
                '</p>' +
                (isRegistered
                    ? ('<div id="cppProfilesList">' +
                        '<div class="cpp-profiles-empty">' + (window.I18N ? I18N.t('settings.profiles.empty', '') : '') + '</div>' +
                       '</div>' +
                       '<button class="btn btn-primary" id="cppProfileAddBtn" style="margin-top:8px;">' +
                           '<i class="fas fa-plus"></i> ' +
                           '<span>' + (window.I18N ? I18N.t('settings.profiles.add', 'Add profile…') : 'Add profile…') + '</span>' +
                       '</button>')
                    : ('<div class="gguf-empty-detail" style="padding:12px;text-align:left;">' +
                        '<i class="fas fa-info-circle"></i> ' + _('gguf.profiles_only_registered') +
                       '</div>')) +
            '</div>' +

            '</div>';
    }

    // ---- Backend options load/save ----

    /**
     * Загрузить текущие llama.cpp-дефолты выбранного cppworker'а и отрендерить
     * редактируемую форму внутри #ggufBackendOptionsContainer.
     */
    async function loadAndRenderBackendOptions() {
        const container = document.getElementById('ggufBackendOptionsContainer');
        if (!container) return;
        const backend = currentBackend();
        if (!backend) {
            container.innerHTML = '<div class="gguf-empty-state">' + (window.I18N ? I18N.t('gguf.no_backend_selected') : '') + '</div>';
            return;
        }
        container.innerHTML = '<div class="loading"><i class="fas fa-spinner fa-spin"></i> ' + (window.I18N ? I18N.t('common.loading', 'Loading…') : 'Loading…') + '</div>';
        try {
            const data = await GgufApi.requestViaBackend(backend.id, '/api/v1/cppworker/config');
            const cfg = (data && data.config) || {};
            container.innerHTML = renderBackendOptionsForm(cfg, backend);
            // Привязываем handlers
            const reloadBtn = container.querySelector('#ggufBackendOptionsReload');
            if (reloadBtn) reloadBtn.addEventListener('click', loadAndRenderBackendOptions);
            const saveBtn = container.querySelector('#ggufBackendOptionsSave');
            if (saveBtn) saveBtn.addEventListener('click', saveBackendOptions);
            showToast(_('gguf.backend_options_loaded'), 'success');
        } catch (e) {
            container.innerHTML =
                '<div class="gguf-empty-state" style="color:var(--danger);">' +
                    '<i class="fas fa-exclamation-triangle"></i> ' +
                    Utils.escapeHtml(e.message || String(e)) +
                    '<br><small>' + Utils.escapeHtml(_('gguf.config_unavailable')) + '</small>' +
                '</div>' +
                '<div style="margin-top:8px;">' +
                    '<button class="btn btn-secondary btn-sm" id="ggufBackendOptionsReload">' +
                        '<i class="fas fa-sync"></i> ' + Utils.escapeHtml(_('gguf.reload_backend_options')) +
                    '</button>' +
                '</div>';
            const retry = document.getElementById('ggufBackendOptionsReload');
            if (retry) retry.addEventListener('click', loadAndRenderBackendOptions);
        }
    }

    function renderBackendOptionsForm(cfg, backend) {
        // Маппинг полей cppbackend.Config ↔ UI. Полное покрытие (~25 полей),
        // сгруппировано по секциям: General / Multi-GPU / KV-cache / RoPE-YaRN / Perf.
        //
        // Все inputs имеют префикс "ggufOpt" для удобства findElementById.
        const v = function (key, fallback) {
            return (cfg[key] !== undefined && cfg[key] !== null) ? cfg[key] : fallback;
        };
        const chk = function (key, fallback) {
            if (cfg[key] === undefined || cfg[key] === null) return !!fallback;
            return !!cfg[key];
        };
        const ctx = v('defaultCtxSize', 4096);
        const batch = v('defaultBatchSize', 512);
        const gpu = v('defaultGpuLayers', -1);
        const fa = v('defaultFlashAttnType', -1);
        const numa = chk('defaultNuma', false);
        const mmap = chk('defaultUseMmap', true);
        const mlock = chk('defaultUseMlock', false);
        const threads = v('defaultNThreads', 0);
        const rmsNormEps = v('defaultRmsNormEps', 0.00001);
        // Multi-GPU
        const autoGpu = chk('autoGpuDistribution', true);
        const splitMode = v('defaultSplitMode', -1);
        const mainGpu = v('defaultMainGpu', 0);
        const rpcBackend = v('defaultRpcBackend', '') || '';
        const noMemoryMap = chk('defaultNoMemoryMap', false);
        // Tensor split: из бэка приходит []float32.
        let tensorSplitVal = '';
        if (cfg.defaultTensorSplit && Array.isArray(cfg.defaultTensorSplit)) {
            tensorSplitVal = cfg.defaultTensorSplit.join(',');
        }
        // KV cache
        const kvCacheType = v('defaultKvCacheType', '') || '';
        const noKvOffload = chk('defaultNoKvOffload', false);
        // RoPE/YaRN
        const ropeFreqBase = v('defaultRopeFreqBase', 10000.0);
        const ropeFreqScale = v('defaultRopeFreqScale', 1.0);
        const ropeScalingType = v('defaultRopeScalingType', 'none') || 'none';
        const ropeScalingFactor = v('defaultRopeScalingFactor', 1.0);
        const yarnExtFactor = v('defaultYarnExtFactor', 1.0);
        const yarnAttnFactor = v('defaultYarnAttnFactor', 1.0);
        const yarnBetaFast = v('defaultYarnBetaFast', 32.0);
        const yarnBetaSlow = v('defaultYarnBetaSlow', 1.0);
        // Performance / lifecycle
        const idleUnload = v('idleUnloadMinutes', 0);
        const enableMetrics = chk('enableMetrics', true);
        const metricsRetention = v('metricsRetentionSeconds', 3600);
        // Session 18 Round 15 (2026-07-29): Inference defaults — параллелизм и reasoning.
        // defaultNParallel: 0 = inherit cppworker default (1), 1..8 = parallel slots per model.
        // enableReasoning: включает soft+native thinking path (Round 11/14).
        //   cppworker handler принимает ключ "enableReasoning" (без "default" префикса),
        //   см. handlers_config.go:387 applyBool("enableReasoning", ...).
        // reasoningBudget: max thinking tokens before answer (0 = unlimited).
        const nParallel = v('defaultNParallel', 0);
        const enableReasoning = chk('enableReasoning', false);
        const reasoningBudget = v('reasoningBudget', 0);
        // Diagnostics
        const nodeName = (cfg.nodeName != null) ? cfg.nodeName : (backend ? backend.id : '');
        const balancerUrl = (cfg.balancerUrl != null) ? cfg.balancerUrl : '';
        const uptime = (cfg.uptime != null) ? cfg.uptime : '';
        return '' +
            // ====== General ======
            '<h6 class="gguf-section-header"><i class="fas fa-sliders-h"></i> ' + Utils.escapeHtml(_('gguf.tab_general') || 'General') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.ctx_size')) + '</label>' +
                    '<input type="number" id="ggufOptCtxSize" class="form-control" value="' + ctx + '" min="256" max="262144" step="64">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.ctx_size_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.batch_size')) + '</label>' +
                    '<input type="number" id="ggufOptBatchSize" class="form-control" value="' + batch + '" min="1" max="4096">' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.gpu_layers')) + '</label>' +
                    '<input type="number" id="ggufOptGpuLayers" class="form-control" value="' + gpu + '" min="-1" max="200">' +
                    '<small style="color:var(--text-muted);">-1 = all layers</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.flash_attn_type')) + '</label>' +
                    '<input type="number" id="ggufOptFlashAttn" class="form-control" value="' + fa + '" min="-1" max="2">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.flash_attn_type_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.n_threads')) + '</label>' +
                    '<input type="number" id="ggufOptNThreads" class="form-control" value="' + threads + '" min="0" max="256">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.n_threads_desc')) + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:repeat(4,1fr);gap:12px;">' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptNuma" ' + (numa ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.numa')) + '</label></div>' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptUseMmap" ' + (mmap ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.use_mmap')) + '</label></div>' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptUseMlock" ' + (mlock ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.use_mlock')) + '</label></div>' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptAutoGpu" ' + (autoGpu ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.auto_gpu_distribution')) + '</label></div>' +
            '</div>' +
            // ====== Multi-GPU ======
            '<h6 class="gguf-section-header"><i class="fas fa-layer-group"></i> ' + Utils.escapeHtml(_('gguf.tab_multi_gpu') || 'Multi-GPU') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.tensor_split')) + '</label>' +
                    '<input type="text" id="ggufOptTensorSplit" class="form-control" value="' + Utils.escapeHtml(tensorSplitVal) + '" placeholder="0.5,0.5">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.tensor_split_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.split_mode')) + '</label>' +
                    '<input type="number" id="ggufOptSplitMode" class="form-control" value="' + splitMode + '" min="-1" max="3">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.split_mode_desc') || '-1=default, 0=NONE, 1=LAYER, 2=ROW, 3=TENSOR') + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.main_gpu')) + '</label>' +
                    '<input type="number" id="ggufOptMainGpu" class="form-control" value="' + mainGpu + '" min="0" max="16">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rpc_backend')) + '</label>' +
                    '<select id="ggufOptRpcBackend" class="form-control">' +
                        '<option value=""' + (rpcBackend === '' ? ' selected' : '') + '>— default —</option>' +
                        '<option value="cuda"' + (rpcBackend === 'cuda' ? ' selected' : '') + '>CUDA</option>' +
                        '<option value="vulkan"' + (rpcBackend === 'vulkan' ? ' selected' : '') + '>Vulkan</option>' +
                        '<option value="kompute"' + (rpcBackend === 'kompute' ? ' selected' : '') + '>Kompute</option>' +
                    '</select>' +
                '</div>' +
            '</div>' +
            '<div class="form-row">' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptNoMemoryMap" ' + (noMemoryMap ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.no_memory_map')) + '</label></div>' +
            '</div>' +
            // ====== KV Cache ======
            '<h6 class="gguf-section-header"><i class="fas fa-memory"></i> ' + Utils.escapeHtml(_('gguf.tab_kv_cache') || 'KV-cache') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.kv_cache_type')) + '</label>' +
                    '<select id="ggufOptKvCacheType" class="form-control">' +
                        '<option value=""' + (kvCacheType === '' ? ' selected' : '') + '>— inherit (F16) —</option>' +
                        '<option value="f16"' + (kvCacheType === 'f16' ? ' selected' : '') + '>f16 (default)</option>' +
                        '<option value="f32"' + (kvCacheType === 'f32' ? ' selected' : '') + '>f32 (full precision)</option>' +
                        '<option value="q8_0"' + (kvCacheType === 'q8_0' ? ' selected' : '') + '>q8_0 (-50% VRAM)</option>' +
                        '<option value="q4_0"' + (kvCacheType === 'q4_0' ? ' selected' : '') + '>q4_0 (-75% VRAM)</option>' +
                    '</select>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.kv_cache_type_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label><input type="checkbox" id="ggufOptNoKvOffload" ' + (noKvOffload ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.no_kv_offload')) + '</label>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.no_kv_offload_desc') || 'Держать KV-cache в RAM (не выгружать на GPU)') + '</small>' +
                '</div>' +
            '</div>' +
            // ====== RoPE & YaRN ======
            '<h6 class="gguf-section-header"><i class="fas fa-wave-square"></i> ' + Utils.escapeHtml(_('gguf.tab_rope_yarn') || 'RoPE / YaRN') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_freq_base')) + '</label>' +
                    '<input type="number" id="ggufOptRopeFreqBase" class="form-control" value="' + ropeFreqBase + '" min="1" step="0.0001">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_freq_scale')) + '</label>' +
                    '<input type="number" id="ggufOptRopeFreqScale" class="form-control" value="' + ropeFreqScale + '" min="0" step="0.0001">' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_scaling_type')) + '</label>' +
                    '<select id="ggufOptRopeScalingType" class="form-control">' +
                        '<option value="none"' + (ropeScalingType === 'none' ? ' selected' : '') + '>none</option>' +
                        '<option value="linear"' + (ropeScalingType === 'linear' ? ' selected' : '') + '>linear</option>' +
                        '<option value="yarn"' + (ropeScalingType === 'yarn' ? ' selected' : '') + '>yarn</option>' +
                    '</select>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_scaling_factor')) + '</label>' +
                    '<input type="number" id="ggufOptRopeScalingFactor" class="form-control" value="' + ropeScalingFactor + '" min="0" step="0.01">' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:repeat(4,1fr);gap:12px;">' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_ext_factor')) + '<input type="number" id="ggufOptYarnExtFactor" class="form-control" value="' + yarnExtFactor + '" min="0" step="0.01"></label></div>' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_attn_factor')) + '<input type="number" id="ggufOptYarnAttnFactor" class="form-control" value="' + yarnAttnFactor + '" min="0" step="0.01"></label></div>' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_beta_fast')) + '<input type="number" id="ggufOptYarnBetaFast" class="form-control" value="' + yarnBetaFast + '" min="0" step="0.01"></label></div>' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_beta_slow')) + '<input type="number" id="ggufOptYarnBetaSlow" class="form-control" value="' + yarnBetaSlow + '" min="0" step="0.01"></label></div>' +
            '</div>' +
            // ====== Performance / Metrics / Lifecycle ======
            '<h6 class="gguf-section-header"><i class="fas fa-tachometer-alt"></i> ' + Utils.escapeHtml(_('gguf.tab_perf') || 'Performance') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rms_norm_eps')) + '</label>' +
                    '<input type="number" id="ggufOptRmsNormEps" class="form-control" value="' + rmsNormEps + '" min="0" step="0.000001">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.idle_unload_minutes')) + '</label>' +
                    '<input type="number" id="ggufOptIdleUnloadMinutes" class="form-control" value="' + idleUnload + '" min="0" max="10080">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.idle_unload_desc') || '0 = off, >0 = unload after N minutes idle') + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptEnableMetrics" ' + (enableMetrics ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.enable_metrics')) + '</label></div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.metrics_retention')) + '</label>' +
                    '<input type="number" id="ggufOptMetricsRetention" class="form-control" value="' + metricsRetention + '" min="0">' +
                '</div>' +
            '</div>' +
            // ====== Inference (Round 15, 2026-07-29) ======
            // n_parallel + enableReasoning + reasoningBudget — default значения
            // для ВСЕХ загружаемых моделей на этом backend. Per-Model Profile
            // (cppworker-params.js) может override'нуть для конкретной модели.
            '<h6 class="gguf-section-header"><i class="fas fa-brain"></i> ' + Utils.escapeHtml(_('gguf.tab_inference') || 'Inference') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.n_parallel') || 'Parallel sequences (n_parallel)') + '</label>' +
                    '<input type="number" id="ggufOptNParallel" class="form-control" value="' + nParallel + '" min="0" max="8">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.n_parallel_desc') || 'Default parallel slots per model. 0 = 1 (sequential), 1..8 = multi-slot KV-cache isolation. > 1 = more VRAM.') + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label><input type="checkbox" id="ggufOptEnableReasoning" ' + (enableReasoning ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.enable_reasoning') || 'Enable reasoning/thinking') + '</label>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.enable_reasoning_desc') || 'Soft+native thinking path for gemma-4, deepseek-r1, qwen3-thinking.') + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.reasoning_budget') || 'Reasoning budget (tokens)') + '</label>' +
                    '<input type="number" id="ggufOptReasoningBudget" class="form-control" value="' + reasoningBudget + '" min="0" max="100000" step="256">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.reasoning_budget_desc') || 'Max thinking tokens before answer. 0 = unlimited.') + '</small>' +
                '</div>' +
            '</div>' +
            // ====== Diagnostics ======
            '<div class="form-group" style="color:var(--text-muted);font-size:12px;margin-top:8px;">' +
                (nodeName ? ('<div><b>node:</b> ' + Utils.escapeHtml(nodeName) + '</div>') : '') +
                (balancerUrl ? ('<div><b>balancer:</b> ' + Utils.escapeHtml(balancerUrl) + '</div>') : '') +
                (uptime ? ('<div><b>uptime:</b> ' + Utils.escapeHtml(uptime) + '</div>') : '') +
            '</div>' +
            '<div class="form-actions" style="margin-top:12px;display:flex;gap:8px;">' +
                '<button class="btn btn-primary" id="ggufBackendOptionsSave">' +
                    '<i class="fas fa-save"></i> ' + Utils.escapeHtml(_('gguf.save_backend_options')) +
                '</button>' +
                '<button class="btn btn-secondary" id="ggufBackendOptionsReload">' +
                    '<i class="fas fa-sync"></i> ' + Utils.escapeHtml(_('gguf.reload_backend_options')) +
                '</button>' +
            '</div>';
    }

    async function saveBackendOptions() {
        const backend = currentBackend();
        if (!backend) return;
        // Парсим значения из формы. Хелперы parseIntOr/parseFloatOr/strVal
        // (определены ниже) устойчивы к NaN/пустым полям.
        const splitStr = parseStr('ggufOptTensorSplit', '');
        let tensorSplit = null;
        if (splitStr.trim() !== '') {
            // Парсим "0.5,0.5" → [0.5, 0.5]
            const parts = splitStr.split(',').map(s => parseFloat(s.trim()));
            if (parts.every(p => Number.isFinite(p))) {
                tensorSplit = parts;
            }
        }
        const payload = {
            // Базовые
            defaultCtxSize: parseIntOr(document.getElementById('ggufOptCtxSize').value, 4096),
            defaultBatchSize: parseIntOr(document.getElementById('ggufOptBatchSize').value, 512),
            defaultGpuLayers: parseIntOr(document.getElementById('ggufOptGpuLayers').value, -1),
            defaultFlashAttnType: parseIntOr(document.getElementById('ggufOptFlashAttn').value, -1),
            defaultNuma: !!document.getElementById('ggufOptNuma').checked,
            defaultUseMmap: !!document.getElementById('ggufOptUseMmap').checked,
            defaultUseMlock: !!document.getElementById('ggufOptUseMlock').checked,
            defaultNThreads: parseIntOr(document.getElementById('ggufOptNThreads').value, 0),
            defaultRmsNormEps: parseFloatOr(document.getElementById('ggufOptRmsNormEps').value, 0.00001),
            // Multi-GPU
            autoGpuDistribution: !!document.getElementById('ggufOptAutoGpu').checked,
            defaultSplitMode: parseIntOr(document.getElementById('ggufOptSplitMode').value, -1),
            defaultMainGpu: parseIntOr(document.getElementById('ggufOptMainGpu').value, 0),
            defaultRpcBackend: parseStr('ggufOptRpcBackend', ''),
            defaultNoMemoryMap: !!document.getElementById('ggufOptNoMemoryMap').checked,
            defaultTensorSplit: tensorSplit,
            // KV cache
            defaultKvCacheType: parseStr('ggufOptKvCacheType', ''),
            defaultNoKvOffload: !!document.getElementById('ggufOptNoKvOffload').checked,
            // RoPE/YaRN
            defaultRopeFreqBase: parseFloatOr(document.getElementById('ggufOptRopeFreqBase').value, 10000.0),
            defaultRopeFreqScale: parseFloatOr(document.getElementById('ggufOptRopeFreqScale').value, 1.0),
            defaultRopeScalingType: parseStr('ggufOptRopeScalingType', 'none'),
            defaultRopeScalingFactor: parseFloatOr(document.getElementById('ggufOptRopeScalingFactor').value, 1.0),
            defaultYarnExtFactor: parseFloatOr(document.getElementById('ggufOptYarnExtFactor').value, 1.0),
            defaultYarnAttnFactor: parseFloatOr(document.getElementById('ggufOptYarnAttnFactor').value, 1.0),
            defaultYarnBetaFast: parseFloatOr(document.getElementById('ggufOptYarnBetaFast').value, 32.0),
            defaultYarnBetaSlow: parseFloatOr(document.getElementById('ggufOptYarnBetaSlow').value, 1.0),
            // Performance
            idleUnloadMinutes: parseIntOr(document.getElementById('ggufOptIdleUnloadMinutes').value, 0),
            enableMetrics: !!document.getElementById('ggufOptEnableMetrics').checked,
            metricsRetentionSeconds: parseIntOr(document.getElementById('ggufOptMetricsRetention').value, 3600),
            // Inference (Round 15, 2026-07-29): n_parallel + reasoning.
            // enableReasoning / reasoningBudget — без "default" префикса, как в app.js
            // (handler applyBool("enableReasoning") в handlers_config.go:387).
            defaultNParallel: parseIntOr(document.getElementById('ggufOptNParallel').value, 0),
            enableReasoning: !!document.getElementById('ggufOptEnableReasoning').checked,
            reasoningBudget: parseIntOr(document.getElementById('ggufOptReasoningBudget').value, 0)
        };
        // state.loadOptions для совместимости с локальным кэшем (loadModelAt)
        state.loadOptions.autoGpuDistribution = !!document.getElementById('ggufOptAutoGpu').checked;
        const saveBtn = document.getElementById('ggufBackendOptionsSave');
        if (saveBtn) saveBtn.disabled = true;
        try {
            const resp = await GgufApi.requestViaBackend(backend.id, '/api/v1/cppworker/config/update', {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            });
            // Показываем пользователю, что применилось и сколько ошибок валидации.
            let msg = _('gguf.backend_options_saved');
            if (resp && resp.applied) {
                msg += ' (' + resp.applied.length + ' полей)';
            }
            if (resp && resp.validation_errors && resp.validation_errors.length > 0) {
                showToast('⚠️ ' + resp.validation_errors.length + ' validation errors: ' +
                    resp.validation_errors.slice(0, 3).join('; '), 'error');
            }
            showToast(msg, 'success');
            // Re-fetch config чтобы пользователь сразу видел, что изменения применились.
            // cppworker возвращает applied[] и reload_started[] — показываем их в toast.
            if (resp && resp.reload_started && resp.reload_started.length > 0) {
                showToast('Reloading ' + resp.reload_started.length + ' model(s) with new defaults...', 'info');
            }
            // Перезагружаем форму с актуальными значениями из cppworker.
            await loadAndRenderBackendOptions();
            // Также обновляем runtime-параметры загруженных моделей (context_size и т.д.).
            try {
                const rtData = await GgufApi.getRuntimeConfigViaBackend(backend.id);
                if (rtData && rtData.loaded_models) {
                    state.runtimeModels = {};
                    (rtData.loaded_models || []).forEach(function (m) {
                        state.runtimeModels[m.name] = m;
                    });
                }
            } catch (rtErr) {
                // runtime config может быть недоступен на старых cppworker — игнорируем.
            }
        } catch (e) {
            showToast(_('gguf.backend_options_save_error') + ': ' + (e.message || e), 'error');
        } finally {
            if (saveBtn) saveBtn.disabled = false;
        }
    }

    // Хелперы для парсинга значений из формы (parseIntOr объявлена ниже,
    // parseFloatOr/parseStr определяем тут же — близко к месту использования).
    function parseFloatOr(v, def) {
        const n = parseFloat(v);
        return Number.isFinite(n) ? n : def;
    }
    function parseStr(id, def) {
        const el = document.getElementById(id);
        if (!el) return def;
        return el.value;
    }

    function parseIntOr(v, def) {
        const n = parseInt(v, 10);
        return Number.isFinite(n) ? n : def;
    }

    /**
     * Инициализировать Per-Model Profiles внутри gguf-настроек.
     * Использует window.CppWorkerParams (cppworker-params.js).
     */
    function mountProfilesInSettings() {
        const btn = document.getElementById('cppProfileAddBtn');
        if (btn && !btn._ggufBound) {
            btn._ggufBound = true;
            btn.addEventListener('click', function () {
                if (window.CppWorkerParams && typeof window.CppWorkerParams.openWizard === 'function') {
                    window.CppWorkerParams.openWizard('', null);
                }
            });
        }
        if (window.CppWorkerParams && typeof window.CppWorkerParams.loadAndRender === 'function') {
            window.CppWorkerParams.loadAndRender();
        }
    }

    function renderConnectModal() {
        return '' +
            '<div class="gguf-modal-backdrop" id="ggufConnectModal">' +
                '<div class="gguf-modal">' +
                    '<h3><i class="fas fa-plug"></i> ' + _('gguf.modal_title') + '</h3>' +
                    '<p>' + _('gguf.modal_desc') + '</p>' +
                    '<div class="form-group">' +
                        '<label>' + (_('gguf.cppworker_url_placeholder') || 'cppworker URL') + '</label>' +
                        '<input type="text" id="ggufConnectUrl" class="form-control" placeholder="http://host:18091" value="' + Utils.escapeHtml(GgufApi.getUrl()) + '">' +
                    '</div>' +
                    '<div class="gguf-modal-actions">' +
                        '<button class="btn btn-secondary" id="ggufConnectCancel"><i class="fas fa-times"></i> ' + (_('common.cancel') || 'Cancel') + '</button>' +
                        '<button class="btn btn-primary" id="ggufConnectSubmit"><i class="fas fa-plug"></i> ' + (_('gguf.connect') || 'Connect') + '</button>' +
                    '</div>' +
                '</div>' +
            '</div>';
    }

    // ---- Delegated detail panel click handler ----

    /**
     * Единый делегированный обработчик кликов на #ggufDetailPanel.
     * ВАЖНО: привязывается ОДИН раз (см. флаг detailPanel._ggufDetailClickBound в bindEvents()).
     * Раньше этот обработчик добавлялся при каждом вызове bindEvents() (а bindEvents
     * вызывается из render / selectBackend / refreshDetailPanel / refreshDetailPane),
     * что приводило к N-кратному дублированию кликов — один клик по кнопке Download
     * запускал N параллельных POST /api/hf/download, первый выигрывал, остальные
     * получали 500 "download already in progress".
     */
    function onDetailPanelClick(e) {
        // Detail tabs
        var tab = e.target.closest('.gguf-detail-tab');
        if (tab) {
            var id = tab.getAttribute('data-detail-tab');
            if (id) {
                state.detailPane = id;
                refreshDetailPane();
                if (id === 'downloads') {
                    refreshActiveDownloads();
                    // Запускаем polling активных загрузок (если ещё не запущен),
                    // чтобы пользователь видел прогресс уже идущих скачиваний.
                    if (state.activeDownloads && state.activeDownloads.length > 0) {
                        startDownloadPolling();
                    }
                } else if (id === 'settings') {
                    // Settings tab: подгружаем реальные llama.cpp-дефолты выбранного
                    // cppworker'а и монтируем Per-Model Profiles (cppworker-params.js).
                    loadAndRenderBackendOptions();
                    mountProfilesInSettings();
                }
            }
            return;
        }
        var loadBtn = e.target.closest('.gguf-load-btn');
        if (loadBtn) {
            var idx = parseInt(loadBtn.getAttribute('data-idx'));
            if (!isNaN(idx) && state.localModels[idx]) {
                loadOnSelectedBackend(state.localModels[idx]);
            }
            return;
        }
        var unloadBtn = e.target.closest('.gguf-unload-btn');
        if (unloadBtn) {
            var handle = unloadBtn.getAttribute('data-handle');
            if (handle) unloadOnSelectedBackend(handle);
            return;
        }
        var delBtn = e.target.closest('.gguf-delete-btn');
        if (delBtn) {
            var di = parseInt(delBtn.getAttribute('data-idx'));
            if (!isNaN(di) && state.localModels[di]) {
                deleteOnSelectedBackend(state.localModels[di]);
            }
            return;
        }
        var cancelDl = e.target.closest('.gguf-cancel-dl-btn');
        if (cancelDl) {
            cancelDownload(cancelDl.getAttribute('data-model-id'), cancelDl.getAttribute('data-filename'));
            return;
        }
        // Round 32 #2 (2026-08-10): cancel generation button в busy badge.
        // Клик по кнопке отменяет активную inference-генерацию через cppworker
        // /api/cancel endpoint (см. cppworkerCancelGeneration в api.js).
        // AbortWatcher → bridge.RequestAbort → C-bridge прерывает при следующей
        // проверке abort флага (каждые ~100ms после Round 32 n_batch=64).
        var cancelGen = e.target.closest('.gguf-cancel-gen-btn');
        if (cancelGen) {
            cancelActiveGeneration(cancelGen.getAttribute('data-model-name'));
            e.preventDefault();
            e.stopPropagation();
            return;
        }
        // Round 17.3 (2026-08-03): Resume кнопка для прерванных загрузок.
        // Re-trigger download — backend обнаружит .download файл и пошлёт Range request.
        var resumeDl = e.target.closest('.gguf-resume-dl-btn');
        if (resumeDl) {
            var rmi = resumeDl.getAttribute('data-model-id');
            var rfn = resumeDl.getAttribute('data-filename');
            startDownload(rmi, rfn);
            return;
        }
        // Round 17.3: Delete кнопка — удаляет скачанный/частичный файл из контейнера.
        var deleteDl = e.target.closest('.gguf-delete-dl-btn');
        if (deleteDl) {
            var dmi = deleteDl.getAttribute('data-model-id');
            var dfn = deleteDl.getAttribute('data-filename');
            deleteDownloadedFile(dmi, dfn);
            return;
        }
        var viewHf = e.target.closest('.gguf-view-hf-files-btn');
        if (viewHf) {
            var vi = parseInt(viewHf.getAttribute('data-model-idx'));
            if (!isNaN(vi) && state.hfSearchResults[vi]) {
                viewHfFiles(state.hfSearchResults[vi]);
            }
            return;
        }
        var quickDl = e.target.closest('.gguf-quick-download-btn');
        if (quickDl) {
            var qi = parseInt(quickDl.getAttribute('data-model-idx'));
            if (!isNaN(qi) && state.hfSearchResults[qi]) {
                quickDownloadHf(state.hfSearchResults[qi]);
            }
            return;
        }
        var closeHf = e.target.closest('#ggufCloseHfFiles');
        if (closeHf) {
            state.hfSearchSelected = null;
            state.hfModelFiles = [];
            refreshDetailPane();
            return;
        }
        var dlHf = e.target.closest('.gguf-hf-download-btn');
        if (dlHf) {
            startDownload(dlHf.getAttribute('data-model-id'), dlHf.getAttribute('data-filename'));
            return;
        }
        // Per-file download (from the inline file list inside HF search result cards)
        var dlFile = e.target.closest('.gguf-download-file-btn');
        if (dlFile) {
            var fi = parseInt(dlFile.getAttribute('data-model-idx'));
            var fname = dlFile.getAttribute('data-filename');
            if (!isNaN(fi) && fname && state.hfSearchResults[fi]) {
                var modelId = state.hfSearchResults[fi].id || state.hfSearchResults[fi].modelId;
                showInlineHfFileProgress(fi, fname, 0);
                startDownload(modelId, fname);
            }
            return;
        }
        // Toggle file list in a result card (clicking the header)
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
            showToast(_('gguf.collapsed') || 'Collapsed', 'info');
            return;
        }
        var copyBtn = e.target.closest('#ggufCopyUrl');
        if (copyBtn) {
            GgufApi.buildBackendWorkerUrlAsync(currentBackend()).then(function (url) {
                if (navigator.clipboard && url) {
                    navigator.clipboard.writeText(url).then(function () {
                        showToast(_('gguf.url_copied'), 'success');
                    }).catch(function () {
                        showToast(url, 'info');
                    });
                } else if (url) {
                    showToast(url, 'info');
                }
            }).catch(function (err) {
                showToast('Copy URL error: ' + err.message, 'error');
            });
            return;
        }
    }

    // ---- Event binding ----

    function bindEvents(container) {
        // Backend list click — ОДИН раз через флаг, чтобы не было дублей при refreshBackends().
        var list = container.querySelector('#ggufBackendList');
        if (list && !list._ggufListClickBound) {
            list._ggufListClickBound = true;
            list.addEventListener('click', function (e) {
                var item = e.target.closest('.gguf-backend-item');
                if (item) {
                    var id = item.getAttribute('data-backend-id');
                    if (id) selectBackend(id);
                }
            });
        }

        // Toggle "Show unhealthy" checkbox
        var showUnhealthyCb = container.querySelector('#ggufShowUnhealthy');
        if (showUnhealthyCb) {
            showUnhealthyCb.addEventListener('change', function () {
                state.showUnhealthy = this.checked;
                updateBackendsList();
            });
        }

        // Refresh backends (эти кнопки могут пересоздаваться, поэтому перепривязка безопасна)
        var refreshBackendsBtn = container.querySelector('#ggufRefreshBackends');
        if (refreshBackendsBtn) {
            refreshBackendsBtn.addEventListener('click', function () { refreshBackends(); });
        }

        // Refresh detail
        var refreshDetailBtn = container.querySelector('#ggufRefreshDetail');
        if (refreshDetailBtn) {
            refreshDetailBtn.addEventListener('click', function () { refreshDetail(); });
        }

        // Detail panel: делегированный обработчик кликов — ОДИН раз за время жизни панели.
        // Без флага _ggufDetailClickBound каждый refreshDetailPanel() добавлял бы ещё одну
        // копию onDetailPanelClick → 1 клик по «Download» запускал бы N параллельных
        // startDownload() → 500 «download already in progress».
        var detailPanel = container.querySelector('#ggufDetailPanel');
        if (detailPanel && !detailPanel._ggufDetailClickBound) {
            detailPanel._ggufDetailClickBound = true;
            state._detailPanel = detailPanel;
            detailPanel.addEventListener('click', onDetailPanelClick);
        } else if (detailPanel) {
            // Если DOM-узел заменили (после selectBackend), обновим ссылку.
            // Сам обработчик уже на старом узле — он продолжает работать, т.к.
            // refreshDetailPanel() не заменяет #ggufDetailPanel, а только innerHTML.
            state._detailPanel = detailPanel;
        }

        // Settings change handlers (привязываются к новым элементам после refresh).
        if (detailPanel) {
            bindSettingsChange(detailPanel, 'ggufDetailAutoGpu', 'autoGpuDistribution', 'checked');
            bindSettingsChange(detailPanel, 'ggufDetailStrategy', 'strategy', 'value');
            bindSettingsChange(detailPanel, 'ggufDetailGpuLayers', 'gpuLayers', 'valueInt');
            bindSettingsChange(detailPanel, 'ggufDetailCtxSize', 'ctxSize', 'valueInt');
            bindSettingsChange(detailPanel, 'ggufDetailBatchSize', 'batchSize', 'valueInt');
            bindSettingsChange(detailPanel, 'ggufDetailFlashAttn', 'flashAttn', 'checked');
            bindSettingsChange(detailPanel, 'ggufDetailNuma', 'numa', 'checked');
            bindSettingsChange(detailPanel, 'ggufDetailUseMmap', 'useMmap', 'checked');
            bindSettingsChange(detailPanel, 'ggufDetailTensorSplit', 'tensorSplit', 'valueOrNull');

            // HF search
            var hfSearchBtn = detailPanel.querySelector('#ggufHfDetailSearchBtn');
            if (hfSearchBtn) {
                hfSearchBtn.addEventListener('click', function () { doHfSearch(); });
            }
            var hfInput = detailPanel.querySelector('#ggufHfDetailSearch');
            if (hfInput) {
                hfInput.addEventListener('keydown', function (e) {
                    if (e.key === 'Enter') doHfSearch();
                });
            }
        }

        // HF token
        var saveHfBtn = container.querySelector('#ggufSaveHfToken');
        if (saveHfBtn) {
            saveHfBtn.addEventListener('click', function () {
                var t = container.querySelector('#ggufHfToken');
                if (t) {
                    GgufApi.setHFToken(t.value.trim());
                    showToast('HF Token saved', 'success');
                }
            });
        }
        var clearHfBtn = container.querySelector('#ggufClearHfToken');
        if (clearHfBtn) {
            clearHfBtn.addEventListener('click', function () {
                var t = container.querySelector('#ggufHfToken');
                if (t) t.value = '';
                GgufApi.setHFToken('');
                showToast('HF Token cleared', 'info');
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
                        GgufApi.setUrl(url);
                        showToast('URL set: ' + url, 'success');
                    }
                }
                var modal = document.getElementById('ggufConnectModal');
                if (modal) modal.classList.remove('active');
            });
        }
    }

    function bindSettingsChange(scope, id, stateKey, kind) {
        var el = scope.querySelector('#' + id);
        if (!el) return;
        el.addEventListener('change', function () {
            if (kind === 'checked') state.loadOptions[stateKey] = this.checked;
            else if (kind === 'value') state.loadOptions[stateKey] = this.value;
            else if (kind === 'valueInt') state.loadOptions[stateKey] = parseInt(this.value) || 0;
            else if (kind === 'valueOrNull') state.loadOptions[stateKey] = this.value || null;
        });
    }

    // ---- Actions ----

    function selectBackend(backendId) {
        if (state.selectedBackendId === backendId) {
            return;
        }
        // Round 26 v0.5.13: cleanup old active-queries polling
        stopActiveQueriesPolling();
        state.activeQueries = {};
        state.selectedBackendId = backendId;
        state.detailPane = 'about';
        state.workerInfo = null;
        state.gpuInfo = null;
        state.localModels = [];
        state.loadedModels = [];
        state.runtimeModels = {};
        state.activeDownloads = [];
        state.downloadProgress = {};
        state.hfSearchQuery = '';
        state.hfSearchResults = [];
        state.hfSearchSelected = null;
        state.hfModelFiles = [];
        state.detailError = null;
        var list = document.getElementById('ggufBackendList');
        if (list) list.innerHTML = renderBackendsList();
        var panel = document.getElementById('ggufDetailPanel');
        if (panel) panel.innerHTML = renderDetailPanel();
        bindEvents(document.getElementById('gguf-page'));
        refreshDetail();
    }

    function currentBackend() {
        if (!state.selectedBackendId) return null;
        return state.registeredBackends.find(function (b) { return b.id === state.selectedBackendId; });
    }

    function loadOnSelectedBackend(model) {
        var backend = currentBackend();
        if (!backend) return;
        var name = model.name || model.filename || '';
        var path = model.path || '';
        if (!name) return;
        showToast(_('gguf.loading_model'), 'info');
        var opts = {
            path: path,
            gpuLayers: state.loadOptions.gpuLayers,
            ctxSize: state.loadOptions.ctxSize,
            batchSize: state.loadOptions.batchSize,
            flashAttn: state.loadOptions.flashAttn,
            numa: state.loadOptions.numa,
            useMmap: state.loadOptions.useMmap
        };
        if (!state.loadOptions.autoGpuDistribution && state.loadOptions.tensorSplit) {
            opts.tensorSplit = state.loadOptions.tensorSplit.split(',').map(parseFloat).filter(function (n) { return !isNaN(n); });
        }

        // === Шаг «отображение загрузки в мониторе и вкладке бэкендов» ===
        // 1) Оптимистично помечаем модель как loading, чтобы UI сразу
        //    показал спиннер (sidebar: «Загружается model-name 0s»).
        // 2) Запускаем polling /api/models/load/progress каждые 1.5 сек.
        // 3) На успехе/ошибке — polling остановится автоматически.
        markLoadingModel(backend.id, name, path);
        if (window.GgufLoadProgress) {
            GgufLoadProgress.startPolling(backend, function () { return currentBackend() === backend; });
        }

        GgufApi.manageModel(backend.id, 'load', name, opts).then(function (data) {
            if (data && data.success === false) {
                showToast(_('gguf.load_error') + ': ' + (data.error || 'unknown'), 'error');
                markLoadFailed(backend.id, name, (data && data.error) || 'load failed');
                return;
            }
            showToast(_('gguf.load_success'), 'success');
            // Polling сам остановится, когда увидит state=loaded в progress.
            refreshDetail();
        }).catch(function (err) {
            showToast(_('gguf.load_error') + ': ' + err.message, 'error');
            markLoadFailed(backend.id, name, err && err.message);
        });
    }

    /**
     * Помечает модель как loading в state.loadingModels[backendID] (оптимистично).
     * Используется sidebar-индикатором «Загружается model-name 0s» в renderBackendsList().
     */
    function markLoadingModel(backendId, name, path) {
        if (!state.loadingModels) state.loadingModels = {};
        if (!state.loadingModels[backendId]) state.loadingModels[backendId] = [];
        var arr = state.loadingModels[backendId];
        var existing = arr.find(function (m) { return m && m.name === name; });
        if (existing) return;
        arr.push({
            name: name,
            path: path || '',
            state: 'loading',
            loadingStartedAt: new Date().toISOString(),
            loadingSizeBytes: 0,
            elapsedMs: 0,
            error: ''
        });
        // Триггерим ререндер sidebar'а, чтобы спиннер появился сразу.
        if (typeof updateBackendsList === 'function') {
            try { updateBackendsList(); } catch (e) { /* noop */ }
        }
    }

    /**
     * Помечает модель как загруженную (state=loaded) или с ошибкой (state=error).
     * Используется polling'ом и при ошибке loadOnSelectedBackend.
     */
    function markLoadFailed(backendId, name, errMsg) {
        if (!state.loadingModels || !state.loadingModels[backendId]) return;
        var arr = state.loadingModels[backendId];
        var item = arr.find(function (m) { return m && m.name === name; });
        if (item) {
            item.state = 'error';
            item.error = errMsg || 'unknown';
        }
        if (typeof updateBackendsList === 'function') {
            try { updateBackendsList(); } catch (e) { /* noop */ }
        }
    }

    function unloadOnSelectedBackend(handle) {
        var backend = currentBackend();
        if (!backend) return;
        if (!confirm(_('gguf.confirm_unload') || 'Unload this model from memory?')) return;
        GgufApi.manageModel(backend.id, 'unload', handle).then(function (data) {
            if (data && data.success === false) {
                showToast(_('gguf.unload_error') + ': ' + (data.error || 'unknown'), 'error');
                return;
            }
            showToast(_('gguf.unload_success'), 'success');
            refreshDetail();
        }).catch(function (err) {
            showToast(_('gguf.unload_error') + ': ' + err.message, 'error');
        });
    }

    function deleteOnSelectedBackend(model) {
        var backend = currentBackend();
        if (!backend) return;
        var name = model.name || model.filename || model.path || '';
        if (!name) return;
        if (!confirm(_('gguf.confirm_delete'))) return;
        GgufApi.deleteModelViaBackend(backend.id, name).then(function () {
            showToast(_('gguf.delete_success'), 'success');
            refreshDetail();
        }).catch(function (err) {
            showToast(_('gguf.delete_error') + ': ' + (err.message || 'unknown error'), 'error');
        });
    }

    function doHfSearch() {
        var input = document.getElementById('ggufHfDetailSearch');
        if (!input) return;
        var query = input.value.trim();
        if (!query) return;
        state.hfSearchQuery = query;
        state.hfSearching = true;
        state.hfSearchResults = [];
        state.hfSearchSelected = null;
        state.hfModelFiles = [];
        refreshDetailPane();
        var backend = currentBackend();
        if (!backend) {
            state.hfSearching = false;
            showToast('Backend not selected', 'error');
            refreshDetailPane();
            return;
        }
        GgufApi.searchModelsViaBackend(backend.id, query, 10).then(function (data) {
            var raw = (data && data.results) || data || [];
            state.hfSearchResults = Array.isArray(raw) ? raw : [];
            state.hfSearching = false;
            refreshDetailPane();
        }).catch(function (err) {
            state.hfSearching = false;
            showToast(_('gguf.search_error') + ': ' + (err.message || 'unknown error'), 'error');
            refreshDetailPane();
        });
    }

    /**
     * Quick download: для выбранной модели автоматически
     * 1) получить список файлов через /api/hf/files;
     * 2) выбрать рекомендованный .gguf (приоритет: Q4_K_M > Q5_K_M > Q6_K > Q8_0 > Q4_0 > любой .gguf);
     * 3) запустить скачивание через startDownload();
     * 4) переключиться на tab "Downloads" для отслеживания прогресса.
     */
    function quickDownloadHf(model) {
        var modelId = model.id || model.modelId || '';
        if (!modelId) {
            showToast('Model ID missing', 'error');
            return;
        }
        showToast(_('gguf.search_btn') + ' ' + modelId + '…', 'info');
        var backend = currentBackend();
        if (!backend) {
            showToast('Backend not selected', 'error');
            return;
        }
        GgufApi.listModelFilesViaBackend(backend.id, modelId, 'main').then(function (data) {
            var files = (data && data.files) || data || [];
            if (!Array.isArray(files) || files.length === 0) {
                showToast(_('gguf.no_gguf_files_found') || 'No .gguf files found in this model', 'error');
                return;
            }
            function fileName(f) {
                return f.path || f.rfilename || f.filename || f.name || '';
            }
            function fileSize(f) {
                return f.sizeBytes || f.size || 0;
            }
            function isGgufFile(f) {
                if (f.isGGUF === true) return true;
                if (f.isGGUF === false) return false;
                return /\.gguf$/i.test(fileName(f));
            }
            function fileQuant(f) {
                if (f.quantization && f.quantization !== 'unknown') return f.quantization;
                var name = fileName(f).toUpperCase();
                var m = name.match(/[._-](Q\d[_A-Z]*|F16|F32|FP16|FP32|BF16)[._-]/);
                if (m) return m[1].replace(/_/g, '_');
                return '';
            }
            var quantPriority = ['Q4_K_M', 'Q5_K_M', 'Q6_K', 'Q8_0', 'Q4_0', 'Q4_K_S', 'Q3_K_M', 'Q2_K', 'Q5_0', 'Q5_1', 'Q4_1', 'Q3_K_S', 'Q3_K_L', 'Q2_K_S', 'F16', 'F32'];
            function pickQuant(quantOrName) {
                var upper = String(quantOrName || '').toUpperCase();
                for (var i = 0; i < quantPriority.length; i++) {
                    if (upper.indexOf(quantPriority[i]) !== -1) return i;
                }
                return 999;
            }
            var ggufFiles = files.filter(isGgufFile);
            if (ggufFiles.length === 0) {
                showToast(_('gguf.no_gguf_files_found') || 'No .gguf files found in this model', 'error');
                return;
            }
            ggufFiles.sort(function (a, b) {
                var qa = fileQuant(a) || fileName(a);
                var qb = fileQuant(b) || fileName(b);
                var pa = pickQuant(qa);
                var pb = pickQuant(qb);
                if (pa !== pb) return pa - pb;
                var sa = fileSize(a);
                var sb = fileSize(b);
                return sb - sa;
            });
            var best = ggufFiles[0];
            var bestName = fileName(best);
            showToast(_('gguf.downloading') + ': ' + bestName, 'info');
            startDownload(modelId, bestName);
        }).catch(function (err) {
            showToast(_('gguf.search_error') + ': ' + err.message, 'error');
        });
    }

    function viewHfFiles(model) {
        state.hfSearchSelected = model;
        state.hfModelFiles = [];
        refreshDetailPane();
        var modelId = model.id || model.modelId || '';
        var backend = currentBackend();
        if (!backend) {
            showToast('Backend not selected', 'error');
            return;
        }
        GgufApi.listModelFilesViaBackend(backend.id, modelId, 'main').then(function (data) {
            var rawFiles = (data && data.files) || data || [];
            state.hfModelFiles = Array.isArray(rawFiles) ? rawFiles : [];
            refreshDetailPane();
        }).catch(function (err) {
            showToast('Error loading files: ' + (err.message || 'unknown error'), 'error');
        });
    }

    function startDownload(modelId, filename) {
        var backend = currentBackend();
        if (!backend) {
            showToast('Backend not selected', 'error');
            return;
        }
        // ==== ЗАЩИТА ОТ ДУБЛЕЙ (уровень 2) ====
        // На уровне event-binder'ов уже есть флаги _ggufDetailClickBound /
        // _ggufListClickBound, но они не помогают, если render()/selectBackend()
        // пересоздали #ggufDetailPanel (старый флаг остаётся на отсоединённом узле,
        // а новый узел получает чистый обработчик). Дополнительно используем
        // in-flight map, чтобы второй одновременный клик на Download не уходил
        // на сервер вообще, а сразу показывал текущий прогресс.
        var inflightKey = backend.id + '::' + modelId + '/' + filename;
        if (_downloadInFlight[inflightKey]) {
            if (!_inflightToastShown[inflightKey]) {
                _inflightToastShown[inflightKey] = true;
                showToast('Download already in progress — showing progress', 'info');
            }
            state.detailPane = 'downloads';
            refreshDetailPane();
            refreshActiveDownloads();
            startDownloadPolling();
            return;
        }
        _downloadInFlight[inflightKey] = true;

        // ==== Немедленная обратная связь (уровень 3) ====
        // До фактического HTTP POST показываем toast и переключаемся на
        // вкладку Downloads — пользователь сразу видит, что клик обработан
        // и загрузка стартовала. Без этого при сетевых задержках 2-5с (типично
        // для HF) кнопка «висит», а пользователь кликает повторно.
        showToast(_('gguf.downloading') + ': ' + filename, 'info');
        state.detailPane = 'downloads';
        // OPTIMISTIC PLACEHOLDER: добавляем запись в state.activeDownloads ДО HTTP-ответа,
        // чтобы пользователь сразу увидел прогресс-бар (а не пустую вкладку).
        if (!state.activeDownloads) state.activeDownloads = [];
        var _oaAlready = state.activeDownloads.some(function (dl) {
            return dl && dl.modelId === modelId && dl.filename === filename;
        });
        if (!_oaAlready) {
            state.activeDownloads.push({
                modelId: modelId,
                filename: filename,
                optimistic: true,
                progressPct: 0,
                status: 'queued'
            });
        }
        if (!state.downloadProgress[modelId + '/' + filename]) {
            state.downloadProgress[modelId + '/' + filename] = { percent: 0, status: 'starting', optimistic: true };
        }
        refreshDetailPane();
        refreshActiveDownloads();
        startDownloadPolling();

        GgufApi.startDownloadViaBackend(backend.id, modelId, filename, 'main').then(function () {
            refreshActiveDownloads();
        }).catch(function (err) {
            var msg = (err && err.message) || '';
            if (/already in progress/i.test(msg) || (err && err.status === 409)) {
                showToast('Download already in progress — showing progress', 'info');
                state.detailPane = 'downloads';
                if (!state.activeDownloads) state.activeDownloads = [];
                var _oaAlready2 = state.activeDownloads.some(function (dl) {
                    return dl && dl.modelId === modelId && dl.filename === filename;
                });
                if (!_oaAlready2) {
                    state.activeDownloads.push({
                        modelId: modelId,
                        filename: filename,
                        optimistic: true,
                        progressPct: 0,
                        status: 'resumed'
                    });
                }
                refreshDetailPane();
                refreshActiveDownloads();
                startDownloadPolling();
            } else if (err && (err.status === 401 || err.status === 403)) {
                showToast(_('gguf.download_failed') + ': HF token required. Set HF Token above and try again.', 'error');
            } else if (err && err.status === 404) {
                showToast(_('gguf.download_failed') + ': Model not found on HuggingFace. Check the repo name.', 'error');
            } else {
                showToast(_('gguf.download_failed') + ': ' + msg, 'error');
            }
        }).then(function () {
            // Сбрасываем in-flight сразу после завершения promise (успех или ошибка).
            // Раньше использовался setTimeout(500), что при быстрых ответах могло
            // пропустить второй клик и привести к дублю на сервере.
            delete _downloadInFlight[inflightKey];
            delete _inflightToastShown[inflightKey];
        });
    }

    function cancelDownload(modelId, filename) {
        var backend = currentBackend();
        if (!backend) return;
        GgufApi.cancelDownloadViaBackend(backend.id, modelId, filename).then(function () {
            showToast(_('gguf.download_cancelled'), 'info');
            delete state.downloadProgress[modelId + '/' + filename];
            refreshActiveDownloads();
        }).catch(function (err) {
            showToast('Cancel error: ' + (err.message || 'unknown error'), 'error');
        });
    }

    /**
     * Round 32 #2 (2026-08-10): cancelActiveGeneration — отменяет активную
     * inference-генерацию для указанной модели. Вызывается кнопкой "Cancel"
     * внутри busy badge на loaded model card.
     *
     * Flow:
     *   1. POST /api/cancel в cppworker (через balancer proxy)
     *   2. cppworker AbortWatcher → bridge.RequestAbort → C-bridge atomic flag
     *   3. C-bridge проверяет flag между llama_decode батчами (каждые ~100ms
     *      после Round 32 n_batch=64) → прерывает генерацию
     *   4. busy badge автоматически обновится при следующем poll (через 3s)
     *
     * Race condition: если generation завершится до того, как cancel дойдёт
     * (например, модель выдала final token ровно перед cancel), cancelled=0
     * — это нормально, не error.
     */
    function cancelActiveGeneration(modelName) {
        var backend = currentBackend();
        if (!backend) return;
        if (!window.Api || !window.Api.cppworkerCancelGeneration) {
            showToast('Cancel API not available', 'error');
            return;
        }
        // Visual feedback: disable button пока запрос в полёте
        var btns = document.querySelectorAll('.gguf-cancel-gen-btn[data-model-name="' + modelName + '"]');
        for (var i = 0; i < btns.length; i++) {
            btns[i].disabled = true;
            btns[i].textContent = '⏳';
        }
        window.Api.cppworkerCancelGeneration.post(backend.id, modelName).then(function (result) {
            showToast(
                (_('gguf.generation_cancelled') || 'Generation cancelled') +
                    ' (' + (result.cancelled || 0) + ' ' + (_('gguf.cancelled_short') || 'cancelled') + ')',
                'info'
            );
            // Принудительно обновляем busy badge — следующий poll через 3s
            // сам подхватит новое состояние, но мы форсируем сейчас для UX.
            // (опционально — можно trigger refreshActiveQueriesPolling)
            // Round 32 #2 (2026-08-10): broadcast cancellation к другим вкладкам.
            // Если у пользователя открыто несколько вкладок webui (например
            // dashboard + GGUF), cancel в одной должен обновить busy badge
            // во всех остальных немедленно, а не через 3s polling.
            if (typeof broadcastCrossTab === 'function') {
                broadcastCrossTab('generationCancelled', { model: modelName });
            }
        }).catch(function (err) {
            showToast('Cancel error: ' + (err.message || 'unknown error'), 'error');
        }).finally(function () {
            // Re-enable button через 1s — polling обновит activeQueries
            setTimeout(function () {
                for (var i = 0; i < btns.length; i++) {
                    btns[i].disabled = false;
                    btns[i].innerHTML = '<i class="fas fa-times"></i> ' + (_('common.cancel') || 'Cancel');
                }
            }, 1000);
        });
    }

    /**
     * Round 17.3 (2026-08-03): удаляет скачанный/частичный файл из контейнера
     * через новый endpoint DELETE /api/hf/cleanup. UI: с подтверждением,
     * потому что освобождает диск и необратимо.
     */
    function deleteDownloadedFile(modelId, filename) {
        var backend = currentBackend();
        if (!backend) return;
        var confirmed = window.confirm(
            (_('gguf.delete_confirm') || 'Delete downloaded file from disk? This frees up disk space and cannot be undone.') +
            '\n\n' + modelId + '\n' + filename
        );
        if (!confirmed) return;
        GgufApi.deleteDownloadedFileViaBackend(backend.id, modelId, filename).then(function (resp) {
            var freedMB = resp && resp.result && resp.result.bytesFreed
                ? (resp.result.bytesFreed / (1024 * 1024)).toFixed(1) + ' MB'
                : '';
            showToast(
                (_('gguf.file_deleted') || 'File deleted from disk') +
                (freedMB ? ' (' + _('gguf.disk_freed') + ': ' + freedMB + ')' : ''),
                'success'
            );
            // Очищаем локальный progress и обновляем список
            delete state.downloadProgress[modelId + '/' + filename];
            // Помечаем запись в downloadHistory как очищенную (UI не показывает Delete повторно)
            refreshActiveDownloads();
        }).catch(function (err) {
            showToast('Delete error: ' + (err.message || 'unknown error'), 'error');
        });
    }

    /**
     * Show or update the inline progress bar for a specific file in the HF search results.
     */
    function showInlineHfFileProgress(modelIdx, filename, percent, status) {
        if (state.detailPane !== 'hf') return;
        var nodes = document.querySelectorAll('.gguf-file-progress');
        for (var i = 0; i < nodes.length; i++) {
            var n = nodes[i];
            if (n.getAttribute('data-model-idx') == modelIdx &&
                n.getAttribute('data-filename') === filename) {
                n.style.display = 'flex';
                var fill = n.querySelector('.gguf-progress-fill');
                var text = n.querySelector('.gguf-progress-text');
                if (fill) fill.style.width = Math.min(100, Math.max(0, percent)) + '%';
                if (text) {
                    var label = Math.round(percent) + '%';
                    if (status) label += ' — ' + status;
                    text.textContent = label;
                }
                if (percent >= 100) {
                    if (fill) fill.style.background = 'var(--success, #28a745)';
                }
                break;
            }
        }
    }

    function startDownloadPolling() {
        if (_pollTimer) {
            console.log('[gguf-renderer] startDownloadPolling: timer already running, skip');
            return;
        }
        console.log('[gguf-renderer] startDownloadPolling: start', { activeCount: (state.activeDownloads || []).length });
        _pollTimer = setInterval(function () {
            if (!state.activeDownloads || state.activeDownloads.length === 0) {
                console.log('[gguf-renderer] startDownloadPolling: no active downloads, stopping');
                clearInterval(_pollTimer);
                _pollTimer = null;
                return;
            }
            var backend = currentBackend();
            if (!backend) {
                console.log('[gguf-renderer] startDownloadPolling: no backend selected, stopping');
                clearInterval(_pollTimer);
                _pollTimer = null;
                return;
            }
            state.activeDownloads.forEach(function (dl) {
                GgufApi.getDownloadProgressViaBackend(backend.id, dl.modelId, dl.filename)
                    .then(function (data) {
                        if (data) {
                            state.downloadProgress[dl.modelId + '/' + dl.filename] = data;
                            if (state.detailPane === 'downloads') refreshDetailPane();
                            if (state.detailPane === 'hf' && data.progressPct !== undefined) {
                                var modelIdx = -1;
                                for (var i = 0; i < state.hfSearchResults.length; i++) {
                                    if (state.hfSearchResults[i].id === dl.modelId) { modelIdx = i; break; }
                                }
                                if (modelIdx >= 0) {
                                    showInlineHfFileProgress(modelIdx, dl.filename, data.progressPct, data.status);
                                }
                            }
                        }
                    })
                    .catch(function (err) { console.log('[gguf-renderer] startDownloadPolling: progress error', { error: err && err.message }); });
            });
        }, 1500);
    }

    // ---- Data refresh ----

    function refreshBackends() {
        GgufApi.getLlamaCppBackends().then(function (data) {
            state.registeredBackends = (data && data.backends) || [];
            state.backendDataLoaded = true;
            updateBackendsList();
            if (state.selectedBackendId) {
                var exists = state.registeredBackends.some(function (b) { return b.id === state.selectedBackendId; });
                if (!exists) {
                    state.selectedBackendId = null;
                    var panel = document.getElementById('ggufDetailPanel');
                    if (panel) panel.innerHTML = renderDetailPanel();
                    bindEvents(document.getElementById('gguf-page'));
                }
            }
        }).catch(function (err) {
            console.error('[GGUF] Backends fetch failed:', err && err.message || err);
            state.registeredBackends = [];
            state.backendDataLoaded = true;
            updateBackendsList();
        });
    }

    function updateBackendsList() {
        var list = document.getElementById('ggufBackendList');
        if (list) list.innerHTML = renderBackendsList();
    }

    function refreshDetail() {
        var backend = currentBackend();
        if (!backend) return;
        if (_detailRefreshInProgress) return;
        _detailRefreshInProgress = true;
        state.detailLoading = true;
        state.detailError = null;
        updateDetailLoading();
        Promise.all([
            GgufApi.getInfoViaBackend(backend.id).then(function (info) { state.workerInfo = info; }).catch(function () { state.workerInfo = null; }),
            GgufApi.getGpuInfoViaBackend(backend.id).then(function (info) { state.gpuInfo = info; }).catch(function () { state.gpuInfo = null; }),
            GgufApi.listLocalModelsViaBackend(backend.id).then(function (data) {
                var arr = (data && data.files) || data;
                state.localModels = Array.isArray(arr) ? arr : [];
            }).catch(function () { state.localModels = []; }),
            GgufApi.listLoadedModelsViaBackend(backend.id).then(function (data) {
                var arr = (data && data.models) || data;
                state.loadedModels = Array.isArray(arr) ? arr : [];
            }).catch(function () {
                if (backend && Array.isArray(backend.models)) {
                    state.loadedModels = backend.models.map(function (m) {
                        return {
                            model: m.name,
                            name: m.name,
                            ctxSize: m.contextSize,
                            status: m.status
                        };
                    });
                } else {
                    state.loadedModels = [];
                }
            }),
            GgufApi.listActiveDownloadsViaBackend(backend.id).then(function (data) {
                var active = (data && data.active) || [];
                var history = (data && data.history) || [];
                state.activeDownloads = Array.isArray(active) ? active : [];
                state.downloadHistory = Array.isArray(history) ? history : [];
            }).catch(function () { state.activeDownloads = []; state.downloadHistory = []; }),
            // === Runtime-параметры загруженных моделей (n_ctx, gpu_layers, ...) ===
            // Параллельный запрос к /api/v1/cppworker/config/runtime. Используется
            // в renderLoadedPane() чтобы показать «default: 8192, runtime: 32768 (gemma-4)».
            // Если endpoint недоступен (старая версия cppworker'а) — игнорируем.
            GgufApi.getRuntimeConfigViaBackend(backend.id).then(function (data) {
                var arr = (data && data.loaded_models) || data || [];
                // Ключ — model.name (fallback path). Значение — весь объект runtime-параметров.
                var runtimeMap = {};
                if (Array.isArray(arr)) {
                    for (var i = 0; i < arr.length; i++) {
                        var rm = arr[i];
                        if (!rm) continue;
                        var key = rm.name || rm.path || ('unknown-' + i);
                        runtimeMap[key] = rm;
                    }
                }
                state.runtimeModels = runtimeMap;
            }).catch(function () {
                // Не очищаем runtimeModels — пусть остаётся последний успешный snapshot
                // (если backend временно недоступен, мы не хотим мигать «0 загружено»).
            })
        ]).then(function () {
            _detailRefreshInProgress = false;
            state.detailLoading = false;
            if (backend.status === 'healthy' && !state.workerInfo && !state.gpuInfo && state.localModels.length === 0 && state.loadedModels.length === 0) {
                state.detailError = _('gguf.backend_unreachable') + ' — ' + backend.id;
            }
            refreshDetailPanel();
            // Если есть активные загрузки — сразу запускаем polling, чтобы
            // пользователь видел прогресс после загрузки страницы / перехода
            // на другой бэкенд.
            if (state.activeDownloads && state.activeDownloads.length > 0) {
                startDownloadPolling();
            }
        }).catch(function (err) {
            _detailRefreshInProgress = false;
            state.detailLoading = false;
            state.detailError = err.message;
            refreshDetailPanel();
        });

        // === Round 26 v0.5.13: запустить polling active-queries ===
        // Параллельно с загрузкой loaded list. Polling даёт busy badge в UI.
        startActiveQueriesPolling();
    }

    // ===== Active queries polling (Round 26 v0.5.13) =====

    /**
     * Запустить polling /api/models/active-queries для текущего выбранного бэкенда.
     * Polling каждые 3 секунды. Авто-cleanup при смене бэкенда.
     */
    function startActiveQueriesPolling() {
        // Очищаем предыдущий таймер, если был
        stopActiveQueriesPolling();
        const backend = currentBackend();
        if (!backend) return;
        if (!window.Api || !window.Api.cppworkerActiveQueries) return;

        const tick = function () {
            // Polling всех загруженных моделей параллельно
            const models = (state.loadedModels || []).map(function (m) {
                return (m.model || m.name || m.path || '').replace(/\.gguf$/, '');
            }).filter(Boolean);
            if (models.length === 0) {
                state.activeQueries = {};
                refreshDetailPane();
                return;
            }
            Promise.all(models.map(function (name) {
                return window.Api.cppworkerActiveQueries.get(backend.id, name)
                    .then(function (data) { return [name, data.activeQueries || 0]; })
                    .catch(function () { return [name, 0]; });
            })).then(function (results) {
                const newMap = {};
                let changed = false;
                results.forEach(function (pair) {
                    newMap[pair[0]] = pair[1];
                    if ((state.activeQueries[pair[0]] || 0) !== pair[1]) changed = true;
                });
                if (changed) {
                    state.activeQueries = newMap;
                    refreshDetailPane();
                }
            });
        };

        // Первый tick сразу, потом каждые 3s
        tick();
        state._activeQueriesTimer = setInterval(tick, 3000);

        // Round 32 #2 (2026-08-10): expose tick через state для cross-tab sync.
        // refreshActiveQueriesPolling (в public API ниже) вызывает tick()
        // немедленно, чтобы busy badge обновился сразу после broadcast.
        state._activeQueriesTickFn = tick;
    }

    /**
     * Остановить polling active-queries.
     */
    function stopActiveQueriesPolling() {
        if (state._activeQueriesTimer) {
            clearInterval(state._activeQueriesTimer);
            state._activeQueriesTimer = null;
        }
    }

    function refreshActiveDownloads() {
        var backend = currentBackend();
        if (!backend) {
            console.log('[gguf-renderer] refreshActiveDownloads: no backend selected');
            return;
        }
        console.log('[gguf-renderer] refreshActiveDownloads: enter', { backendId: backend.id });
        GgufApi.listActiveDownloadsViaBackend(backend.id).then(function (data) {
            var active = (data && data.active) || [];
            var history = (data && data.history) || [];
            state.activeDownloads = Array.isArray(active) ? active : [];
            state.downloadHistory = Array.isArray(history) ? history : [];
            console.log('[gguf-renderer] refreshActiveDownloads: ok', { backendId: backend.id, activeCount: state.activeDownloads.length, historyCount: state.downloadHistory.length, data });
            refreshDetailPane();
        }).catch(function (err) {
            console.log('[gguf-renderer] refreshActiveDownloads: listActiveDownloadsViaBackend error', { backendId: backend.id, error: err && err.message });
            state.activeDownloads = [];
            state.downloadHistory = [];
            refreshDetailPane();
        });
    }

    function updateDetailLoading() {
        var content = document.querySelector('#ggufDetailPanel .gguf-detail-content');
        if (content) {
            content.innerHTML = '<div class="gguf-empty-state"><i class="fas fa-spinner fa-spin"></i> ' + _('common.loading') + '</div>';
        }
    }

    function refreshDetailPanel() {
        var panel = document.getElementById('ggufDetailPanel');
        if (!panel) return;
        panel.innerHTML = renderDetailPanel();
        bindEvents(document.getElementById('gguf-page'));
    }

    function refreshDetailPane() {
        var content = document.querySelector('#ggufDetailPanel .gguf-detail-content');
        if (!content) return;
        content.innerHTML = renderDetailPane();
        // Round 15 (2026-07-29): обновляем класс .active на кнопках вкладок —
        // без этого после клика на таб контент переключался, но визуальный
        // highlight оставался на ПРЕДЫДУЩЕЙ вкладке (пользователь видел
        // "выделение на Инфо, хотя контент Настройки"). Меняем класс на
        // существующих кнопках чтобы не ломать делегированный click handler.
        var tabsContainer = document.querySelector('#ggufDetailPanel .gguf-detail-tabs');
        if (tabsContainer) {
            var tabBtns = tabsContainer.querySelectorAll('.gguf-detail-tab');
            for (var i = 0; i < tabBtns.length; i++) {
                var btn = tabBtns[i];
                if (btn.getAttribute('data-detail-tab') === state.detailPane) {
                    btn.classList.add('active');
                } else {
                    btn.classList.remove('active');
                }
            }
        }
        var panel = document.getElementById('ggufDetailPanel');
        if (panel) {
            bindSettingsChange(panel, 'ggufDetailAutoGpu', 'autoGpuDistribution', 'checked');
            bindSettingsChange(panel, 'ggufDetailStrategy', 'strategy', 'value');
            bindSettingsChange(panel, 'ggufDetailGpuLayers', 'gpuLayers', 'valueInt');
            bindSettingsChange(panel, 'ggufDetailCtxSize', 'ctxSize', 'valueInt');
            bindSettingsChange(panel, 'ggufDetailBatchSize', 'batchSize', 'valueInt');
            bindSettingsChange(panel, 'ggufDetailFlashAttn', 'flashAttn', 'checked');
            bindSettingsChange(panel, 'ggufDetailNuma', 'numa', 'checked');
            bindSettingsChange(panel, 'ggufDetailUseMmap', 'useMmap', 'checked');
            bindSettingsChange(panel, 'ggufDetailTensorSplit', 'tensorSplit', 'valueOrNull');
            var hfSearchBtn = panel.querySelector('#ggufHfDetailSearchBtn');
            if (hfSearchBtn) {
                hfSearchBtn.addEventListener('click', function () { doHfSearch(); });
            }
            var hfInput = panel.querySelector('#ggufHfDetailSearch');
            if (hfInput) {
                hfInput.addEventListener('keydown', function (e) {
                    if (e.key === 'Enter') doHfSearch();
                });
            }
        }
    }

    // ---- Helpers ----

    // stripGGUF — убирает суффикс ".gguf" (case-insensitive) из имени файла.
    // Используется в Bug #2 fix (Round 24) для нормализации сравнения
    // локального имени файла и имени загруженной модели.
    //   "Qwen3-Instruct-2507-q4km.gguf" → "Qwen3-Instruct-2507-q4km"
    //   "Qwen3-Instruct-2507-q4km.GGUF" → "Qwen3-Instruct-2507-q4km"
    //   "Qwen3-Instruct-2507-q4km"      → "Qwen3-Instruct-2507-q4km" (no change)
    function stripGGUF(name) {
        if (!name) return '';
        if (name.length > 5 && name.slice(-5).toLowerCase() === '.gguf') {
            return name.slice(0, -5);
        }
        return name;
    }

    function formatFileSize(bytes) {
        if (!bytes || bytes === 0) return '0 B';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = Math.floor(Math.log(bytes) / Math.log(1024));
        return (bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
    }

    function showToast(message, type) {
        if (window.showToast) {
            window.showToast(message, type);
            return;
        }
        // Fallback: гарантированно показать пользователю, даже если глобальный
        // window.showToast не зарегистрирован (например, страница открыта напрямую
        // или скрипт ещё не успел загрузиться). Используем простой DOM-fallback:
        // создаём или переиспользуем #ggufDebugToastHost и показываем плашку внизу.
        try {
            var hostId = 'ggufDebugToastHost';
            var host = document.getElementById(hostId);
            if (!host) {
                host = document.createElement('div');
                host.id = hostId;
                host.style.cssText = 'position:fixed;left:16px;bottom:16px;z-index:99999;display:flex;flex-direction:column;gap:6px;pointer-events:none;';
                document.body.appendChild(host);
            }
            var el = document.createElement('div');
            el.style.cssText = 'pointer-events:auto;padding:8px 12px;border-radius:6px;color:#fff;font-size:13px;box-shadow:0 2px 8px rgba(0,0,0,.2);max-width:480px;word-break:break-word;';
            var bg = type === 'error' ? '#d9534f' : type === 'success' ? '#5cb85c' : type === 'info' ? '#5bc0de' : '#777';
            el.style.background = bg;
            el.textContent = '[' + (type || 'log') + '] ' + message;
            host.appendChild(el);
            setTimeout(function () { if (el.parentNode) el.parentNode.removeChild(el); }, 6000);
        } catch (e) {
            // Если DOM недоступен — остаётся console.log
        }
        console.log('[' + (type || 'log') + '] ' + message);
    }

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
