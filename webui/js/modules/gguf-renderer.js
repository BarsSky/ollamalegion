/**
 * GGUF Renderer — renders the GGUF Models page UI
 * Relies on GgufApi for data, and I18N for translations
 */
const GgufRenderer = (window.GgufRenderer = (function () {
    const _ = function (key, vars) {
        return window.I18N ? I18N.t(key, vars) : key;
    };

    // State
    let state = {
        connected: false,
        connectionAttempted: false,
        workerInfo: null,
        gpuInfo: null,
        searchResults: [],
        localModels: [],
        loadedModels: [],
        activeDownloads: [],
        downloadProgress: {},
        currentPage: 'search', // 'search' | 'local' | 'settings'
        searchQuery: '',
        searchLimit: 10,
        selectedModel: null,
        modelFiles: [],
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
        }
    };
    // Protection against recursive render cycles
    let _refreshInProgress = false;
    let _renderLock = false;

    // ---- Main render function ----

    function render(container) {
        if (!container) return;
        container.innerHTML = buildPageHtml();
        bindEvents(container);
        // Restore saved HF token to input field
        var hfTokenInput = document.getElementById('ggufHfToken');
        if (hfTokenInput) {
            hfTokenInput.value = GgufApi.getHFToken() || '';
        }
        // Автоматически пробуем подключиться к cppworker при первой загрузке страницы
        if (!state.connected) {
            autoConnect();
        } else {
            refreshData();
        }
    }

    /**
     * Автоматическая попытка подключения к cppworker по умолчанию.
     * Если не удаётся — показываем информативное сообщение.
     */
    function autoConnect() {
        state.connectionAttempted = true;
        updateConnectionStatus(_('gguf.connecting'), 'connecting');
        GgufApi.testConnection().then(function (result) {
            if (result.ok) {
                state.connected = true;
                state.connectionAttempted = false;
                state.workerInfo = result.info;
                updateConnectionStatus(_('gguf.connected'), 'connected');
                hideConnectionHelp();
                refreshAllData();
            } else {
                state.connected = false;
                updateConnectionStatus(_('gguf.disconnected'), 'disconnected');
                updateGpuInfoSection();
            }
        }).catch(function (err) {
            state.connected = false;
            updateConnectionStatus(_('gguf.disconnected'), 'disconnected');
        });
    }

    /**
     * Показать справочное сообщение, если cppworker недоступен.
     */
    function showConnectionHelp() {
        var container = document.getElementById('ggufContainer');
        if (!container) return;
        // Добавляем help-блок после connection bar, если его ещё нет
        var existing = document.getElementById('ggufConnectionHelp');
        if (existing) return;
        var helpHtml = '<div id="ggufConnectionHelp" class="gguf-connection-help" style="padding:16px;margin:8px 0;background:var(--bg-secondary);border-radius:8px;border:1px solid var(--border);">' +
            '<div style="font-size:14px;margin-bottom:8px;">⚠️ ' + _('gguf.cppworker_unavailable') + '</div>' +
            '<div style="font-size:12px;color:var(--text-secondary);">' +
                _('gguf.cppworker_unavailable_desc') +
            '</div>' +
            '<div style="margin-top:12px;font-size:12px;color:var(--text-secondary);">' +
                '📌 ' + _('gguf.check_worker_url') + ': <code style="background:var(--bg-primary);padding:2px 6px;border-radius:3px;">' + GgufApi.getUrl() + '</code>' +
            '</div>' +
        '</div>';
        var connectionBar = document.getElementById('ggufConnectionBar');
        if (connectionBar && connectionBar.parentNode) {
            connectionBar.insertAdjacentHTML('afterend', helpHtml);
        }
    }

    /**
     * Убрать справочное сообщение при успешном подключении.
     */
    function hideConnectionHelp() {
        var help = document.getElementById('ggufConnectionHelp');
        if (help) help.remove();
    }

    // ---- Page HTML ----

    function buildPageHtml() {
        // Connection help block — rendered as part of the page, not via DOM insertion
        var connectionHelpHtml = '';
        if (!state.connected && state.connectionAttempted) {
            connectionHelpHtml = '<div id="ggufConnectionHelp" class="gguf-connection-help" style="padding:16px;margin:8px 0;background:var(--bg-secondary);border-radius:8px;border:1px solid var(--border);">' +
                '<div style="font-size:14px;margin-bottom:8px;">⚠️ ' + _('gguf.cppworker_unavailable') + '</div>' +
                '<div style="font-size:12px;color:var(--text-secondary);">' +
                    _('gguf.cppworker_unavailable_desc') +
                '</div>' +
                '<div style="margin-top:12px;font-size:12px;color:var(--text-secondary);">' +
                    '📌 ' + _('gguf.check_worker_url') + ': <code style="background:var(--bg-primary);padding:2px 6px;border-radius:3px;">' + GgufApi.getUrl() + '</code>' +
                '</div>' +
            '</div>';
        }
        return '' +
            // Connection Status Bar
            '<div class="gguf-connection-bar" id="ggufConnectionBar">' +
                '<div class="gguf-connection-indicator">' +
                    '<span class="gguf-status-dot ' + (state.connected ? 'connected' : 'disconnected') + '" id="ggufStatusDot"></span>' +
                    '<span id="ggufConnectionText">' + (state.connected ? _('gguf.connected') : _('gguf.disconnected')) + '</span>' +
                '</div>' +
                '<div class="gguf-connection-form">' +
                    '<input type="text" id="ggufWorkerUrl" class="form-control gguf-url-input" value="' + GgufApi.getUrl() + '" placeholder="' + _('gguf.cppworker_url_placeholder') + '">' +
                    '<button class="btn btn-primary" id="ggufConnectBtn">' + (state.connected ? _('gguf.disconnect') : _('gguf.connect')) + '</button>' +
                '</div>' +
            '</div>' +
            connectionHelpHtml +
            // HuggingFace Token Bar (для gated моделей)
            '<div class="gguf-hf-token-bar" id="ggufHfTokenBar" style="display:flex;align-items:center;gap:8px;padding:8px 12px;background:var(--bg-secondary);border-radius:8px;margin-bottom:8px;border:1px solid var(--border);">' +
                '<span style="font-size:13px;white-space:nowrap;"><i class="fas fa-key"></i> HF Token:</span>' +
                '<input type="password" id="ggufHfToken" class="form-control" style="flex:1;max-width:360px;" placeholder="hf_..." value="' + Utils.escapeHtml(GgufApi.getHFToken() || '') + '">' +
                '<button class="btn btn-sm btn-secondary" id="ggufSaveHfToken" title="' + _('gguf.download') + '">' + (window.I18N ? I18N.t('common.save') : 'Save') + '</button>' +
                '<button class="btn btn-sm btn-secondary" id="ggufClearHfToken" title="' + (window.I18N ? I18N.t('models.close') : 'Clear') + '"><i class="fas fa-times"></i></button>' +
            '</div>' +

            // Tab Bar
            '<div class="gguf-tabs">' +
                '<button class="gguf-tab ' + (state.currentPage === 'search' ? 'active' : '') + '" data-gguf-tab="search">' +
                    '<i class="fas fa-search"></i> ' + _('gguf.search') +
                '</button>' +
                '<button class="gguf-tab ' + (state.currentPage === 'local' ? 'active' : '') + '" data-gguf-tab="local">' +
                    '<i class="fas fa-folder-open"></i> ' + _('gguf.local_models') +
                '</button>' +
                '<button class="gguf-tab ' + (state.currentPage === 'settings' ? 'active' : '') + '" data-gguf-tab="settings">' +
                    '<i class="fas fa-cog"></i> ' + _('gguf.strategy') +
                '</button>' +
            '</div>' +

            // Tab Content
            '<div class="gguf-content">' +
                renderSearchTab() +
                renderLocalTab() +
                renderSettingsTab() +
            '</div>' +

            // Loaded Models Section (always visible at bottom)
            '<div class="gguf-section gguf-loaded-section" id="ggufLoadedSection">' +
                '<div class="gguf-section-header">' +
                    '<h3><i class="fas fa-brain"></i> ' + _('gguf.loaded_models_header') + '</h3>' +
                    '<span class="badge badge-info" id="ggufLoadedCount">0</span>' +
                '</div>' +
                '<div class="gguf-loaded-models" id="ggufLoadedModels">' +
                    '<div class="gguf-empty-state">' + _('gguf.no_loaded_models') + '</div>' +
                '</div>' +
            '</div>' +

            // GPU + Worker Info section
            '<div class="gguf-section gguf-info-section">' +
                renderInfoCards() +
            '</div>';
    }

    // ---- Search Tab ----

    function renderSearchTab() {
        const active = state.currentPage === 'search';
        return '' +
            '<div class="gguf-tab-content' + (active ? ' active' : '') + '" id="ggufSearchTab" style="' + (active ? '' : 'display:none;') + '">' +
                // Search Form
                '<div class="gguf-search-form">' +
                    '<div class="gguf-search-row">' +
                        '<input type="text" id="ggufSearchInput" class="form-control gguf-search-input" value="' + Utils.escapeHtml(state.searchQuery) + '" placeholder="' + _('gguf.search_placeholder') + '">' +
                        '<select id="ggufSearchLimit" class="form-control gguf-limit-select">' +
                            '<option value="5" ' + (state.searchLimit === 5 ? 'selected' : '') + '>5</option>' +
                            '<option value="10" ' + (state.searchLimit === 10 ? 'selected' : '') + '>10</option>' +
                            '<option value="20" ' + (state.searchLimit === 20 ? 'selected' : '') + '>20</option>' +
                            '<option value="50" ' + (state.searchLimit === 50 ? 'selected' : '') + '>50</option>' +
                        '</select>' +
                        '<button class="btn btn-primary" id="ggufSearchBtn">' + _('gguf.search_btn') + '</button>' +
                    '</div>' +
                '</div>' +

                // Search Results
                '<div id="ggufSearchResults" class="gguf-search-results">' +
                    renderSearchResults() +
                '</div>' +

                // Model Files (shown when a model is selected)
                '<div id="ggufModelFiles" class="gguf-model-files" style="' + (state.selectedModel ? '' : 'display:none;') + '">' +
                    renderModelFiles() +
                '</div>' +
            '</div>';
    }

    function renderSearchResults() {
        if (!state.searchResults || state.searchResults.length === 0) {
            return '<div class="gguf-empty-state">' + _('gguf.no_results') + '</div>';
        }
        return '<div class="gguf-results-list">' +
            state.searchResults.map(function (model, idx) {
                const downloads = model.downloads || 0;
                const likes = model.likes || 0;
                const updated = model.lastModified ? new Date(model.lastModified).toLocaleDateString() : '-';
                const isSelected = state.selectedModel && state.selectedModel.id === model.id;
                return '' +
                    '<div class="gguf-result-card ' + (isSelected ? 'selected' : '') + '" data-model-idx="' + idx + '">' +
                        '<div class="gguf-result-header">' +
                            '<div class="gguf-result-title">' + Utils.escapeHtml(model.id || model.modelId || '') + '</div>' +
                            '<div class="gguf-result-author">' + _('gguf.author') + ': ' + Utils.escapeHtml(model.author || '-') + '</div>' +
                        '</div>' +
                        '<div class="gguf-result-meta">' +
                            '<span title="' + _('gguf.downloads') + '"><i class="fas fa-download"></i> ' + downloads.toLocaleString() + '</span>' +
                            '<span title="' + _('gguf.likes') + '"><i class="fas fa-heart"></i> ' + likes.toLocaleString() + '</span>' +
                            '<span title="' + _('gguf.last_updated') + '"><i class="fas fa-calendar"></i> ' + updated + '</span>' +
                        '</div>' +
                        '<div class="gguf-result-actions">' +
                            '<button class="btn btn-sm btn-secondary gguf-view-files-btn" data-model-idx="' + idx + '">' +
                                '<i class="fas fa-list"></i> ' + _('gguf.view_files') +
                            '</button>' +
                        '</div>' +
                    '</div>';
            }).join('') +
            '</div>';
    }

    function renderModelFiles() {
        if (!state.selectedModel) return '';
        const modelId = state.selectedModel.id || state.selectedModel.modelId || '';
        const files = state.modelFiles || [];
        return '' +
            '<div class="gguf-files-header">' +
                '<h4><i class="fas fa-file"></i> ' + Utils.escapeHtml(modelId) + '</h4>' +
                '<button class="btn btn-sm btn-secondary" id="ggufCloseFiles"><i class="fas fa-times"></i> ' + _('common.close') + '</button>' +
            '</div>' +
            '<div class="gguf-files-grid">' +
                (files.length === 0
                    ? '<div class="gguf-empty-state">' + _('gguf.no_results') + '</div>'
                    : files.map(function (file, idx) {
                        const size = file.size ? formatFileSize(file.size) : '-';
                        const isGGUF = file.rfilename && file.rfilename.endsWith('.gguf');
                        const isDownloading = state.activeDownloads && state.activeDownloads.some(function (d) {
                            return d.modelId === modelId && d.filename === file.rfilename;
                        });
                        return '' +
                            '<div class="gguf-file-card">' +
                                '<div class="gguf-file-icon"><i class="fas fa-' + (isGGUF ? 'file' : 'file-lines') + '"></i></div>' +
                                '<div class="gguf-file-info">' +
                                    '<div class="gguf-file-name">' + Utils.escapeHtml(file.rfilename || file.name || '') + '</div>' +
                                    '<div class="gguf-file-size">' + size + '</div>' +
                                '</div>' +
                                '<div class="gguf-file-actions">' +
                                    (isGGUF
                                        ? '<button class="btn btn-sm btn-primary gguf-download-btn" data-filename="' + Utils.escapeHtml(file.rfilename || '') + '" data-model-id="' + Utils.escapeHtml(modelId) + '">' +
                                            (isDownloading ? _('gguf.downloading') : _('gguf.download')) +
                                          '</button>'
                                        : '') +
                                '</div>' +
                            '</div>';
                    }).join(''))
            '</div>';
    }

    // ---- Local Tab ----

    function renderLocalTab() {
        const active = state.currentPage === 'local';
        return '' +
            '<div class="gguf-tab-content' + (active ? ' active' : '') + '" id="ggufLocalTab" style="' + (active ? '' : 'display:none;') + '">' +
                '<div class="gguf-local-header">' +
                    '<h4><i class="fas fa-folder-open"></i> ' + _('gguf.local_models') + '</h4>' +
                    '<button class="btn btn-sm btn-secondary" id="ggufRefreshLocal"><i class="fas fa-sync"></i> ' + _('gguf.refresh') + '</button>' +
                '</div>' +
                '<div class="gguf-local-grid" id="ggufLocalGrid">' +
                    renderLocalModels() +
                '</div>' +
                // Active Downloads
                '<div class="gguf-section" id="ggufActiveDownloads">' +
                    '<div class="gguf-section-header">' +
                        '<h4><i class="fas fa-download"></i> ' + _('gguf.active_downloads') + '</h4>' +
                    '</div>' +
                    '<div id="ggufDownloadsList">' +
                        renderActiveDownloads() +
                    '</div>' +
                '</div>' +
            '</div>';
    }

    function renderLocalModels() {
        if (!state.localModels || state.localModels.length === 0) {
            return '<div class="gguf-empty-state">' + _('gguf.no_local_models') + '</div>';
        }
        return state.localModels.map(function (model, idx) {
            const size = model.size ? formatFileSize(model.size) : '-';
            const quant = model.quantization || model.quant || '-';
            return '' +
                '<div class="gguf-local-card">' +
                    '<div class="gguf-local-icon"><i class="fas fa-cube"></i></div>' +
                    '<div class="gguf-local-info">' +
                        '<div class="gguf-local-name">' + Utils.escapeHtml(model.name || model.filename || model.path || '') + '</div>' +
                        '<div class="gguf-local-meta">' +
                            '<span>' + _('gguf.model_size') + ': ' + size + '</span>' +
                            '<span>' + _('gguf.quantization') + ': ' + quant + '</span>' +
                        '</div>' +
                    '</div>' +
                    '<div class="gguf-local-actions">' +
                        '<button class="btn btn-sm btn-primary gguf-load-btn" data-model-idx="' + idx + '">' +
                            '<i class="fas fa-play"></i> ' + _('gguf.load_model') +
                        '</button>' +
                    '</div>' +
                '</div>';
        }).join('');
    }

    function renderActiveDownloads() {
        if (!state.activeDownloads || state.activeDownloads.length === 0) {
            return '<div class="gguf-empty-state">' + _('gguf.no_active_downloads') + '</div>';
        }
        return state.activeDownloads.map(function (dl) {
            const progress = state.downloadProgress[dl.modelId + '/' + dl.filename];
            const pct = progress ? progress.percent || progress.completed || 0 : 0;
            const speed = progress ? progress.speed || '' : '';
            return '' +
                '<div class="gguf-download-item">' +
                    '<div class="gguf-download-info">' +
                        '<div class="gguf-download-model">' + Utils.escapeHtml(dl.modelId) + '</div>' +
                        '<div class="gguf-download-file">' + Utils.escapeHtml(dl.filename) + '</div>' +
                    '</div>' +
                    '<div class="gguf-download-progress-bar">' +
                        '<div class="gguf-progress-fill" style="width:' + pct + '%"></div>' +
                    '</div>' +
                    '<div class="gguf-download-stats">' +
                        '<span>' + Math.round(pct) + '%</span>' +
                        (speed ? '<span>' + speed + '</span>' : '') +
                    '</div>' +
                    '<div class="gguf-download-actions">' +
                        '<button class="btn btn-sm btn-danger gguf-cancel-dl-btn" data-model-id="' + Utils.escapeHtml(dl.modelId) + '" data-filename="' + Utils.escapeHtml(dl.filename) + '">' +
                            '<i class="fas fa-ban"></i> ' + _('gguf.cancel_download') +
                        '</button>' +
                    '</div>' +
                '</div>';
        }).join('');
    }

    // ---- Settings Tab ----

    function renderSettingsTab() {
        const active = state.currentPage === 'settings';
        return '' +
            '<div class="gguf-tab-content' + (active ? ' active' : '') + '" id="ggufSettingsTab" style="' + (active ? '' : 'display:none;') + '">' +
                '<div class="gguf-settings-form">' +
                    '<div class="form-group">' +
                        '<label>' +
                            '<input type="checkbox" id="ggufAutoGpu" ' + (state.loadOptions.autoGpuDistribution ? 'checked' : '') + '>' +
                            ' ' + _('gguf.auto_gpu_distribution') +
                        '</label>' +
                    '</div>' +
                    '<div class="form-group">' +
                        '<label>' + _('gguf.strategy') + '</label>' +
                        '<select id="ggufStrategy" class="form-control">' +
                            '<option value="vram-ratio" ' + (state.loadOptions.strategy === 'vram-ratio' ? 'selected' : '') + '>' + _('gguf.strategy_vram_ratio') + '</option>' +
                            '<option value="manual" ' + (state.loadOptions.strategy === 'manual' ? 'selected' : '') + '>' + _('gguf.strategy_manual') + '</option>' +
                            '<option value="round-robin" ' + (state.loadOptions.strategy === 'round-robin' ? 'selected' : '') + '>' + _('gguf.strategy_round_robin') + '</option>' +
                        '</select>' +
                    '</div>' +
                    '<div class="form-row">' +
                        '<div class="form-group">' +
                            '<label>' + _('gguf.gpu_layers') + '</label>' +
                            '<input type="number" id="ggufGpuLayers" class="form-control" value="' + state.loadOptions.gpuLayers + '" min="-1" max="200">' +
                            '<small style="color:var(--text-muted);">-1 = all layers</small>' +
                        '</div>' +
                        '<div class="form-group">' +
                            '<label>' + _('gguf.ctx_size') + '</label>' +
                            '<input type="number" id="ggufCtxSize" class="form-control" value="' + state.loadOptions.ctxSize + '" min="512" max="32768" step="512">' +
                        '</div>' +
                    '</div>' +
                    '<div class="form-row">' +
                        '<div class="form-group">' +
                            '<label>' + _('gguf.batch_size') + '</label>' +
                            '<input type="number" id="ggufBatchSize" class="form-control" value="' + state.loadOptions.batchSize + '" min="1" max="4096">' +
                        '</div>' +
                        '<div class="form-group">' +
                            '<label>' + _('gguf.tensor_split') + '</label>' +
                            '<input type="text" id="ggufTensorSplit" class="form-control" value="' + (state.loadOptions.tensorSplit || '') + '" placeholder="e.g. 0.5,0.5">' +
                            '<small style="color:var(--text-muted);">Comma-separated ratios summing to 1.0</small>' +
                        '</div>' +
                    '</div>' +
                    '<div class="form-row">' +
                        '<div class="form-group">' +
                            '<label>' +
                                '<input type="checkbox" id="ggufFlashAttn" ' + (state.loadOptions.flashAttn ? 'checked' : '') + '>' +
                                ' ' + _('gguf.flash_attn') +
                            '</label>' +
                        '</div>' +
                        '<div class="form-group">' +
                            '<label>' +
                                '<input type="checkbox" id="ggufNuma" ' + (state.loadOptions.numa ? 'checked' : '') + '>' +
                                ' ' + _('gguf.numa') +
                            '</label>' +
                        '</div>' +
                        '<div class="form-group">' +
                            '<label>' +
                                '<input type="checkbox" id="ggufUseMmap" ' + (state.loadOptions.useMmap ? 'checked' : '') + '>' +
                                ' ' + _('gguf.use_mmap') +
                            '</label>' +
                        '</div>' +
                    '</div>' +
                '</div>' +
            '</div>';
    }

    // ---- Info Cards ----

    function renderInfoCards() {
        let gpuHtml = '';
        if (state.gpuInfo) {
            const devices = state.gpuInfo.devices || (Array.isArray(state.gpuInfo) ? state.gpuInfo : [state.gpuInfo]);
            if (devices.length > 0) {
                gpuHtml = devices.map(function (gpu) {
                    const name = gpu.name || gpu.brand || gpu.model || '-';
                    const memTotal = gpu.memoryTotal || gpu.totalMemory || 0;
                    const memFree = gpu.memoryFree || gpu.freeMemory || 0;
                    const util = gpu.utilization || gpu.usagePercent || 0;
                    return '' +
                        '<div class="gguf-info-card">' +
                            '<div class="gguf-info-label">' + Utils.escapeHtml(name) + '</div>' +
                            '<div class="gguf-info-value">VRAM: ' + formatFileSize(memTotal) + '</div>' +
                            '<div class="gguf-info-value">' + _('common.free') + ': ' + formatFileSize(memFree) + '</div>' +
                            '<div class="gguf-info-value">' + _('metrics.gpu_util') + ': ' + Math.round(util) + '%</div>' +
                        '</div>';
                }).join('');
            }
        }
        if (!gpuHtml) {
            gpuHtml = '<div class="gguf-info-card">' +
                '<div class="gguf-info-label">' + _('gguf.no_gpu') + '</div>' +
            '</div>';
        }

        let workerHtml = '';
        if (state.workerInfo) {
            const version = state.workerInfo.version || state.workerInfo.llamaVersion || '-';
            const gpuCount = state.gpuInfo ? (state.gpuInfo.count || (Array.isArray(state.gpuInfo) ? state.gpuInfo.length : 1)) : 0;
            workerHtml = '' +
                '<div class="gguf-info-card">' +
                    '<div class="gguf-info-label">' + _('gguf.worker_info') + '</div>' +
                    '<div class="gguf-info-value">' + _('gguf.version') + ': ' + Utils.escapeHtml(version) + '</div>' +
                    '<div class="gguf-info-value">' + _('gguf.gpu_count') + ': ' + gpuCount + '</div>' +
                '</div>';
        } else {
            workerHtml = '<div class="gguf-info-card">' +
                '<div class="gguf-info-label">' + _('gguf.worker_info') + '</div>' +
                '<div class="gguf-info-value">' + _('common.disconnected') + '</div>' +
            '</div>';
        }

        return '' +
            '<div class="gguf-info-grid">' +
                '<div class="gguf-info-column">' +
                    '<h4><i class="fas fa-microchip"></i> ' + _('gguf.gpu_info') + '</h4>' +
                    '<div class="gguf-info-cards">' + gpuHtml + '</div>' +
                '</div>' +
                '<div class="gguf-info-column">' +
                    '<h4><i class="fas fa-server"></i> ' + _('gguf.worker_info') + '</h4>' +
                    '<div class="gguf-info-cards">' + workerHtml + '</div>' +
                '</div>' +
            '</div>';
    }

    // ---- Event Binding ----

    function bindEvents(container) {
        // Connection
        const connectBtn = container.querySelector('#ggufConnectBtn');
        if (connectBtn) {
            connectBtn.addEventListener('click', function () {
                if (state.connected) {
                    disconnectWorker();
                } else {
                    connectToWorker();
                }
            });
        }
        const urlInput = container.querySelector('#ggufWorkerUrl');
        if (urlInput) {
            urlInput.addEventListener('keydown', function (e) {
                if (e.key === 'Enter') connectToWorker();
            });
        }

        // Tab switching
        container.querySelectorAll('.gguf-tab').forEach(function (tab) {
            tab.addEventListener('click', function () {
                switchTab(this.dataset.ggufTab);
            });
        });

        // Search
        const searchBtn = container.querySelector('#ggufSearchBtn');
        if (searchBtn) {
            searchBtn.addEventListener('click', function () { doSearch(); });
        }
        const searchInput = container.querySelector('#ggufSearchInput');
        if (searchInput) {
            searchInput.addEventListener('keydown', function (e) {
                if (e.key === 'Enter') doSearch();
            });
        }
        const searchLimit = container.querySelector('#ggufSearchLimit');
        if (searchLimit) {
            searchLimit.addEventListener('change', function () {
                state.searchLimit = parseInt(this.value) || 10;
            });
        }

        // View files buttons (delegated)
        container.addEventListener('click', function (e) {
            const viewBtn = e.target.closest('.gguf-view-files-btn');
            if (viewBtn) {
                const idx = parseInt(viewBtn.dataset.modelIdx);
                if (!isNaN(idx) && state.searchResults[idx]) {
                    viewModelFiles(state.searchResults[idx]);
                }
                return;
            }

            const closeFilesBtn = e.target.closest('#ggufCloseFiles');
            if (closeFilesBtn) {
                state.selectedModel = null;
                state.modelFiles = [];
                refreshUi();
                return;
            }

            const downloadBtn = e.target.closest('.gguf-download-btn');
            if (downloadBtn) {
                const modelId = downloadBtn.dataset.modelId;
                const filename = downloadBtn.dataset.filename;
                if (modelId && filename) {
                    startDownloadModel(modelId, filename);
                }
                return;
            }

            const cancelBtn = e.target.closest('.gguf-cancel-dl-btn');
            if (cancelBtn) {
                const modelId = cancelBtn.dataset.modelId;
                const filename = cancelBtn.dataset.filename;
                if (modelId && filename) {
                    cancelDownload(modelId, filename);
                }
                return;
            }

            const loadBtn = e.target.closest('.gguf-load-btn');
            if (loadBtn) {
                const idx = parseInt(loadBtn.dataset.modelIdx);
                if (!isNaN(idx) && state.localModels[idx]) {
                    loadLocalModel(state.localModels[idx]);
                }
                return;
            }
        });

        // Settings changes
        const autoGpu = container.querySelector('#ggufAutoGpu');
        if (autoGpu) {
            autoGpu.addEventListener('change', function () {
                state.loadOptions.autoGpuDistribution = this.checked;
            });
        }
        const strategy = container.querySelector('#ggufStrategy');
        if (strategy) {
            strategy.addEventListener('change', function () {
                state.loadOptions.strategy = this.value;
            });
        }
        const gpuLayers = container.querySelector('#ggufGpuLayers');
        if (gpuLayers) {
            gpuLayers.addEventListener('change', function () {
                state.loadOptions.gpuLayers = parseInt(this.value) || -1;
            });
        }
        const ctxSize = container.querySelector('#ggufCtxSize');
        if (ctxSize) {
            ctxSize.addEventListener('change', function () {
                state.loadOptions.ctxSize = parseInt(this.value) || 2048;
            });
        }
        const batchSize = container.querySelector('#ggufBatchSize');
        if (batchSize) {
            batchSize.addEventListener('change', function () {
                state.loadOptions.batchSize = parseInt(this.value) || 512;
            });
        }
        const flashAttn = container.querySelector('#ggufFlashAttn');
        if (flashAttn) {
            flashAttn.addEventListener('change', function () {
                state.loadOptions.flashAttn = this.checked;
            });
        }
        const numa = container.querySelector('#ggufNuma');
        if (numa) {
            numa.addEventListener('change', function () {
                state.loadOptions.numa = this.checked;
            });
        }
        const useMmap = container.querySelector('#ggufUseMmap');
        if (useMmap) {
            useMmap.addEventListener('change', function () {
                state.loadOptions.useMmap = this.checked;
            });
        }
        const tensorSplit = container.querySelector('#ggufTensorSplit');
        if (tensorSplit) {
            tensorSplit.addEventListener('input', function () {
                state.loadOptions.tensorSplit = this.value || null;
            });
        }

        // Refresh local
        const refreshLocal = container.querySelector('#ggufRefreshLocal');
        if (refreshLocal) {
            refreshLocal.addEventListener('click', function () {
                refreshLocalModels();
                refreshActiveDownloads();
            });
        }

        // HuggingFace Token buttons
        var saveHfTokenBtn = container.querySelector('#ggufSaveHfToken');
        if (saveHfTokenBtn) {
            saveHfTokenBtn.addEventListener('click', function () {
                var tokenInput = document.getElementById('ggufHfToken');
                if (tokenInput) {
                    var token = tokenInput.value.trim();
                    GgufApi.setHFToken(token);
                    showToast(token ? 'HF Token saved' : 'HF Token cleared', 'success');
                }
            });
        }
        var clearHfTokenBtn = container.querySelector('#ggufClearHfToken');
        if (clearHfTokenBtn) {
            clearHfTokenBtn.addEventListener('click', function () {
                var tokenInput = document.getElementById('ggufHfToken');
                if (tokenInput) {
                    tokenInput.value = '';
                    GgufApi.setHFToken('');
                    showToast('HF Token cleared', 'info');
                }
            });
        }
    }

    // ---- Actions ----

    function connectToWorker() {
        const urlInput = document.getElementById('ggufWorkerUrl');
        if (!urlInput) return;
        const url = urlInput.value.trim() || 'http://localhost:18091';
        GgufApi.setUrl(url);
        state.connectionAttempted = true;
        updateConnectionStatus(_('gguf.connecting'), 'connecting');
        GgufApi.testConnection().then(function (result) {
            if (result.ok) {
                state.connected = true;
                state.connectionAttempted = false;
                state.workerInfo = result.info;
                updateConnectionStatus(_('gguf.connected'), 'connected');
                refreshAllData();
            } else {
                state.connected = false;
                updateConnectionStatus(_('gguf.connection_error') + ': ' + result.error, 'disconnected');
                updateGpuInfoSection();
            }
        }).catch(function (err) {
            state.connected = false;
            updateConnectionStatus(_('gguf.connection_error') + ': ' + err.message, 'disconnected');
        });
    }

    function disconnectWorker() {
        state.connected = false;
        state.workerInfo = null;
        state.gpuInfo = null;
        updateConnectionStatus(_('gguf.disconnected'), 'disconnected');
        refreshUi();
    }

    function switchTab(tab) {
        state.currentPage = tab;
        refreshUi();
    }

    function doSearch() {
        const input = document.getElementById('ggufSearchInput');
        if (!input) return;
        const query = input.value.trim();
        if (!query) return;
        state.searchQuery = query;
        state.searchResults = [];
        state.selectedModel = null;
        state.modelFiles = [];
        refreshUi();
        GgufApi.searchModels(query, state.searchLimit).then(function (data) {
            state.searchResults = (data && data.models) || data || [];
            refreshUi();
        }).catch(function (err) {
            showToast(_('gguf.search_error') + ': ' + err.message, 'error');
        });
    }

    function viewModelFiles(model) {
        state.selectedModel = model;
        state.modelFiles = [];
        refreshUi();
        const modelId = model.id || model.modelId || '';
        GgufApi.listModelFiles(modelId).then(function (data) {
            state.modelFiles = (data && data.files) || data || [];
            refreshUi();
        }).catch(function (err) {
            showToast('Error loading files: ' + err.message, 'error');
        });
    }

    function startDownloadModel(modelId, filename) {
        GgufApi.startDownload(modelId, filename).then(function () {
            showToast(_('gguf.downloading'), 'info');
            refreshActiveDownloads();
            // Start polling progress
            pollDownloadProgress(modelId, filename);
        }).catch(function (err) {
            showToast(_('gguf.download_failed') + ': ' + err.message, 'error');
        });
    }

    function pollDownloadProgress(modelId, filename) {
        const interval = setInterval(function () {
            GgufApi.getDownloadProgress(modelId, filename).then(function (data) {
                if (data) {
                    const key = modelId + '/' + filename;
                    state.downloadProgress[key] = data;
                    refreshUi();
                    if (data.completed >= 100 || data.status === 'completed' || data.done) {
                        clearInterval(interval);
                        showToast(_('gguf.download_completed'), 'success');
                        refreshLocalModels();
                        refreshActiveDownloads();
                    }
                    if (data.status === 'failed' || data.error) {
                        clearInterval(interval);
                        showToast(_('gguf.download_failed'), 'error');
                        refreshActiveDownloads();
                    }
                    if (data.status === 'cancelled') {
                        clearInterval(interval);
                        refreshActiveDownloads();
                    }
                }
            }).catch(function () {
                // Silently ignore polling errors
            });
        }, 1000);
    }

    function cancelDownload(modelId, filename) {
        GgufApi.cancelDownload(modelId, filename).then(function () {
            showToast(_('gguf.download_cancelled'), 'info');
            const key = modelId + '/' + filename;
            delete state.downloadProgress[key];
            refreshActiveDownloads();
            refreshUi();
        }).catch(function (err) {
            showToast('Cancel error: ' + err.message, 'error');
        });
    }

    function loadLocalModel(model) {
        const modelName = model.name || model.filename || '';
        const modelPath = model.path || '';
        if (!modelName) return;
        showToast(_('gguf.loading_model'), 'info');
        const options = {
            path: modelPath, // explicit path for HF-downloaded models
            gpuLayers: state.loadOptions.gpuLayers,
            ctxSize: state.loadOptions.ctxSize,
            batchSize: state.loadOptions.batchSize,
            flashAttn: state.loadOptions.flashAttn,
            numa: state.loadOptions.numa,
            useMmap: state.loadOptions.useMmap
        };
        if (!state.loadOptions.autoGpuDistribution && state.loadOptions.tensorSplit) {
            options.tensorSplit = state.loadOptions.tensorSplit.split(',').map(parseFloat).filter(function (n) { return !isNaN(n); });
        }
        GgufApi.loadModel(modelName, options).then(function () {
            showToast(_('gguf.model_loaded'), 'success');
            refreshLoadedModels();
        }).catch(function (err) {
            showToast(_('gguf.model_load_error') + ': ' + err.message, 'error');
        });
    }

    // ---- Data Refresh ----

    function refreshData() {
        if (!state.connected) return;
        refreshAllData();
    }

    /**
     * Fetch all data in parallel and do ONE incremental UI update.
     * This replaces the old pattern where each individual refresh*() called refreshUi()
     * which called render() which called refreshAllData() again — causing exponential fetches.
     */
    function refreshAllData() {
        if (_refreshInProgress || !state.connected) return;
        _refreshInProgress = true;

        Promise.all([
            GgufApi.getGpuInfo().then(function (data) { state.gpuInfo = data; }).catch(function () {}),
            GgufApi.listLocalModels().then(function (data) {
                var arr = (data && data.files) || data;
                state.localModels = Array.isArray(arr) ? arr : [];
            }).catch(function () { state.localModels = []; }),
            GgufApi.listLoadedModels().then(function (data) {
                var arr = (data && data.models) || data;
                state.loadedModels = Array.isArray(arr) ? arr : [];
            }).catch(function () { state.loadedModels = []; }),
            GgufApi.listActiveDownloads().then(function (data) {
                var arr = (data && data.downloads) || data;
                state.activeDownloads = Array.isArray(arr) ? arr : [];
            }).catch(function () { state.activeDownloads = []; })
        ]).then(function () {
            _refreshInProgress = false;
            // Единый инкрементальный update UI после получения всех данных
            updateLoadedModelsSection();
            updateLocalModelsSection();
            updateActiveDownloadsSection();
            updateGpuInfoSection();
        });
    }

    /**
     * Incremental update: only rewrites loaded models DOM, no full render().
     */
    function refreshGpuInfo() {
        if (!state.connected) return;
        GgufApi.getGpuInfo().then(function (data) {
            state.gpuInfo = data;
            updateGpuInfoSection();
        }).catch(function () {});
    }

    /**
     * Incremental update: only rewrites local models DOM, no full render().
     */
    function refreshLocalModels() {
        if (!state.connected) return;
        GgufApi.listLocalModels().then(function (data) {
            var arr = (data && data.files) || data;
            state.localModels = Array.isArray(arr) ? arr : [];
            updateLocalModelsSection();
        }).catch(function () {
            state.localModels = [];
            updateLocalModelsSection();
        });
    }

    /**
     * Incremental update: only rewrites loaded models DOM, no full render().
     */
    function refreshLoadedModels() {
        if (!state.connected) return;
        GgufApi.listLoadedModels().then(function (data) {
            var arr = (data && data.models) || data;
            state.loadedModels = Array.isArray(arr) ? arr : [];
            updateLoadedModelsSection();
        }).catch(function () {
            state.loadedModels = [];
            updateLoadedModelsSection();
        });
    }

    /**
     * Incremental update: only rewrites active downloads DOM, no full render().
     */
    function refreshActiveDownloads() {
        if (!state.connected) return;
        GgufApi.listActiveDownloads().then(function (data) {
            var arr = (data && data.downloads) || data;
            state.activeDownloads = Array.isArray(arr) ? arr : [];
            updateActiveDownloadsSection();
        }).catch(function () {
            state.activeDownloads = [];
            updateActiveDownloadsSection();
        });
    }

    // ---- Incremental DOM Update Helpers ----

    function updateLoadedModelsSection() {
        const countEl = document.getElementById('ggufLoadedCount');
        if (countEl) countEl.textContent = state.loadedModels.length;
        const container = document.getElementById('ggufLoadedModels');
        if (!container) return;
        if (!state.loadedModels || state.loadedModels.length === 0) {
            container.innerHTML = '<div class="gguf-empty-state">' + _('gguf.no_loaded_models') + '</div>';
            return;
        }
        container.innerHTML = state.loadedModels.map(function (m) {
            const name = m.model || m.name || m.path || '-';
            const ctx = m.ctxSize || m.contextSize || '-';
            return '' +
                '<div class="gguf-loaded-model-item">' +
                    '<div class="gguf-loaded-model-name">' + Utils.escapeHtml(name) + '</div>' +
                    '<div class="gguf-loaded-model-info">' +
                        '<span>' + _('gguf.ctx_size') + ': ' + ctx + '</span>' +
                    '</div>' +
                    '<button class="btn btn-sm btn-danger gguf-unload-btn" data-handle="' + Utils.escapeHtml(m.handle || m.model || '') + '">' +
                        '<i class="fas fa-stop"></i> ' + _('gguf.unload_model') +
                    '</button>' +
                '</div>';
        }).join('');
        // Bind unload buttons
        container.querySelectorAll('.gguf-unload-btn').forEach(function (btn) {
            btn.addEventListener('click', function () {
                const handle = this.dataset.handle;
                if (handle) {
                    GgufApi.unloadModel(handle).then(function () {
                        showToast(_('gguf.model_unloaded'), 'success');
                        refreshLoadedModels();
                    }).catch(function (err) {
                        showToast(_('gguf.model_unload_error') + ': ' + err.message, 'error');
                    });
                }
            });
        });
    }

    function updateLocalModelsSection() {
        var grid = document.getElementById('ggufLocalGrid');
        if (!grid) return;
        if (!state.localModels || state.localModels.length === 0) {
            grid.innerHTML = '<div class="gguf-empty-state">' + _('gguf.no_local_models') + '</div>';
            return;
        }
        grid.innerHTML = state.localModels.map(function (model, idx) {
            const size = model.size ? formatFileSize(model.size) : '-';
            const quant = model.quantization || model.quant || '-';
            return '' +
                '<div class="gguf-local-card">' +
                    '<div class="gguf-local-icon"><i class="fas fa-cube"></i></div>' +
                    '<div class="gguf-local-info">' +
                        '<div class="gguf-local-name">' + Utils.escapeHtml(model.name || model.filename || model.path || '') + '</div>' +
                        '<div class="gguf-local-meta">' +
                            '<span>' + _('gguf.model_size') + ': ' + size + '</span>' +
                            '<span>' + _('gguf.quantization') + ': ' + quant + '</span>' +
                        '</div>' +
                    '</div>' +
                    '<div class="gguf-local-actions">' +
                        '<button class="btn btn-sm btn-primary gguf-load-btn" data-model-idx="' + idx + '">' +
                            '<i class="fas fa-play"></i> ' + _('gguf.load_model') +
                        '</button>' +
                    '</div>' +
                '</div>';
        }).join('');
    }

    function updateActiveDownloadsSection() {
        var list = document.getElementById('ggufDownloadsList');
        if (!list) return;
        if (!state.activeDownloads || state.activeDownloads.length === 0) {
            list.innerHTML = '<div class="gguf-empty-state">' + _('gguf.no_active_downloads') + '</div>';
            return;
        }
        list.innerHTML = state.activeDownloads.map(function (dl) {
            const progress = state.downloadProgress[dl.modelId + '/' + dl.filename];
            const pct = progress ? progress.percent || progress.completed || 0 : 0;
            const speed = progress ? progress.speed || '' : '';
            return '' +
                '<div class="gguf-download-item">' +
                    '<div class="gguf-download-info">' +
                        '<div class="gguf-download-model">' + Utils.escapeHtml(dl.modelId) + '</div>' +
                        '<div class="gguf-download-file">' + Utils.escapeHtml(dl.filename) + '</div>' +
                    '</div>' +
                    '<div class="gguf-download-progress-bar">' +
                        '<div class="gguf-progress-fill" style="width:' + pct + '%"></div>' +
                    '</div>' +
                    '<div class="gguf-download-stats">' +
                        '<span>' + Math.round(pct) + '%</span>' +
                        (speed ? '<span>' + speed + '</span>' : '') +
                    '</div>' +
                    '<div class="gguf-download-actions">' +
                        '<button class="btn btn-sm btn-danger gguf-cancel-dl-btn" data-model-id="' + Utils.escapeHtml(dl.modelId) + '" data-filename="' + Utils.escapeHtml(dl.filename) + '">' +
                            '<i class="fas fa-ban"></i> ' + _('gguf.cancel_download') +
                        '</button>' +
                    '</div>' +
                '</div>';
        }).join('');
    }

    function updateGpuInfoSection() {
        var infoSection = document.querySelector('#gguf-page .gguf-info-section');
        if (!infoSection) return;
        infoSection.innerHTML = renderInfoCards();
    }

    function updateSearchResultsSection() {
        var results = document.getElementById('ggufSearchResults');
        if (!results) return;
        results.innerHTML = renderSearchResults();
    }

    function updateModelFilesSection() {
        var files = document.getElementById('ggufModelFiles');
        if (!files) return;
        files.innerHTML = renderModelFiles();
        files.style.display = state.selectedModel ? '' : 'none';
    }

    // ---- UI Updates ----

    function updateConnectionStatus(text, className) {
        const dot = document.getElementById('ggufStatusDot');
        const textEl = document.getElementById('ggufConnectionText');
        const connectBtn = document.getElementById('ggufConnectBtn');
        const urlInput = document.getElementById('ggufWorkerUrl');
        if (dot) {
            dot.className = 'gguf-status-dot ' + className;
        }
        if (textEl) textEl.textContent = text;
        if (connectBtn) {
            connectBtn.textContent = state.connected ? _('gguf.disconnect') : _('gguf.connect');
        }
        if (urlInput) {
            urlInput.disabled = state.connected;
        }
    }

    /**
     * Full page re-render. Use ONLY for initial page load and tab switches.
     * Avoid calling from individual data fetch methods — use incremental updates instead.
     */
    function refreshUi() {
        if (_renderLock) return;
        _renderLock = true;
        try {
            const container = document.getElementById('gguf-page');
            if (!container) { _renderLock = false; return; }
            render(container);
        } finally {
            _renderLock = false;
        }
    }

    // ---- Helpers ----

    function formatFileSize(bytes) {
        if (!bytes || bytes === 0) return '0 B';
        const units = ['B', 'KB', 'MB', 'GB', 'TB'];
        const i = Math.floor(Math.log(bytes) / Math.log(1024));
        return (bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
    }

    function showToast(message, type) {
        if (window.showToast) {
            window.showToast(message, type);
        } else {
            console.log('[' + type + '] ' + message);
        }
    }

    // ---- Public API ----

    return {
        render: render,
        getState: function () { return state; },
        refreshData: refreshData,
        refreshLoadedModels: refreshLoadedModels,
        refreshLocalModels: refreshLocalModels,
        refreshActiveDownloads: refreshActiveDownloads,
        connectToWorker: connectToWorker
    };
})());
