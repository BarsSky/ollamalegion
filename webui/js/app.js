/**
 * OllamaLegion WebUI — Orchestrator
 * Imports: Utils, Api, WebSocketManager, Renderers (loaded before this file)
 */
const ui = (function () {
    const { dashboard, backendsPage, modelsPage, sessionsPage, queuePage, logs: renderLogs, predictionAlerts, proxyLogs: renderProxyLogs, copyProxyLogs: renderCopyProxyLogs, agentsPage: renderAgentsPage, renderAgentDetails } = Renderers;

    // State
    const data = {
        backends: [],
        sessions: [],
        queue: {},
        queueTasks: [],
        queueHistory: [],
        logs: [],
        models: [],
        proxyLogs: [],
        agents: null
    };
    let currentPage = 'dashboard';
    let refreshTimer = null;
    let dashboardRenderTimer = null;
    let autoSaveTimer = null;

    // ---- Initialization ----

    function init() {
        initTheme();
        setupI18n();
        setupNavigation();
        setupEventListeners();
        setupRestartHandler();
        setupApiEvents();
        setupWebSocketEvents();

        // Initial data load — cluster state first, drives connection status
        fetchClusterState().then(function () {
            fetchQueue();
            fetchQueueDetails();
            fetchQueueHistory();
            fetchSessions();
        });

        // Load agents on startup
        fetchAgents();

        // Periodic refresh
        startPeriodicRefresh();

        // Initialize Settings UI modules
        if (window.SettingsUI) {
            SettingsUI.setupAccordion();
            SettingsUI.restoreAccordionState();
            SettingsUI.setupModeSelector();
            SettingsUI.setupBackendEngineSwitch();
            SettingsUI.initBackendEngineCards();
        }

        // Check if setup wizard should be shown — проверяем сервер, а не localStorage
        if (window.SetupWizard && window.SetupWizard.isInitialized) {
            window.SetupWizard.isInitialized().then(function (initialized) {
                if (!initialized) {
                    window.SetupWizard.start();
                }
            }).catch(function () {
                // При ошибке подключения не показываем wizard — сервер может быть недоступен
                console.warn('Failed to check initialization status from server');
            });
        }

        // Setup logs tab navigation (System / Proxy sub-tabs)
        setupLogsTabNavigation();
        // Load proxy logs from REST API on startup
        fetchProxyLogs();

        addLog(window.I18N ? I18N.t('app.webui_initialized') : 'WebUI initialized', 'info');
    }

    // ---- Theme ----

    function initTheme() {
        var saved = localStorage.getItem('ollamalegion_theme') || 'dark';
        document.documentElement.setAttribute('data-theme', saved);
        updateThemeToggleIcon(saved);
        var toggleBtn = document.getElementById('themeToggle');
        if (toggleBtn) {
            toggleBtn.addEventListener('click', function () {
                var current = document.documentElement.getAttribute('data-theme') || 'dark';
                var next = current === 'dark' ? 'light' : 'dark';
                document.documentElement.setAttribute('data-theme', next);
                localStorage.setItem('ollamalegion_theme', next);
                updateThemeToggleIcon(next);
            });
        }
    }

    function updateThemeToggleIcon(theme) {
        var btn = document.getElementById('themeToggle');
        if (btn) {
            btn.innerHTML = theme === 'dark' ? '☀️' : '🌙';
            btn.title = theme === 'dark'
                ? (window.I18N ? I18N.t('settings.theme_light') : 'Light theme')
                : (window.I18N ? I18N.t('settings.theme_dark') : 'Dark theme');
        }
    }

    // ---- i18n ----

    function updateUITranslations() {
        document.querySelectorAll('[data-i18n]').forEach(function (el) {
            var key = el.getAttribute('data-i18n');
            if (key && window.I18N) {
                if (el.tagName === 'INPUT' && el.hasAttribute('data-i18n-placeholder')) {
                    el.placeholder = I18N.t(el.getAttribute('data-i18n-placeholder'));
                } else {
                    el.textContent = I18N.t(key);
                }
            }
        });
        // Process data-i18n-title
        document.querySelectorAll('[data-i18n-title]').forEach(function (el) {
            var key = el.getAttribute('data-i18n-title');
            if (key && window.I18N) {
                el.title = I18N.t(key);
            }
        });
        // Update page title
        if (currentPage && window.I18N) {
            var titleKey = 'header.' + currentPage;
            var h1 = document.getElementById('pageTitle');
            if (h1) h1.textContent = I18N.t(titleKey);
        }
    }

    function setupI18n() {
        var langSelect = document.getElementById('langSelect');
        if (langSelect && window.I18N) {
            var currentLang = I18N.getLang();
            langSelect.value = currentLang;
            langSelect.addEventListener('change', function () {
                I18N.setLang(this.value);
                updateUITranslations();
                updateThemeToggleIcon(document.documentElement.getAttribute('data-theme') || 'dark');
                showToast(window.I18N ? I18N.t('app.lang_changed') : 'Language changed', 'success');
            });
        }
        window.addEventListener('i18n:changed', function () {
            updateUITranslations();
            updateThemeToggleIcon(document.documentElement.getAttribute('data-theme') || 'dark');
        });
        // Process data-i18n-placeholder on initial load
        document.querySelectorAll('[data-i18n-placeholder]').forEach(function (el) {
            var key = el.getAttribute('data-i18n-placeholder');
            if (key && window.I18N) {
                el.placeholder = I18N.t(key);
            }
        });
        updateUITranslations();
    }

    // ---- Restart Handler ----

    function setupRestartHandler() {
        var restartBtn = document.getElementById('restartBalancerBtn');
        var restartModal = document.getElementById('restartConfirmModal');
        var modalConfirm = document.getElementById('restartModalConfirm');
        var modalCancel = document.getElementById('restartModalCancel');
        var modalClose = document.getElementById('restartModalClose');
        var indicator = document.getElementById('restartIndicator');

        function showRestartModal() { if (restartModal) restartModal.classList.add('active'); }
        function hideRestartModal() { if (restartModal) restartModal.classList.remove('active'); }

        if (restartBtn) restartBtn.addEventListener('click', showRestartModal);
        if (modalClose) modalClose.addEventListener('click', hideRestartModal);
        if (modalCancel) modalCancel.addEventListener('click', hideRestartModal);
        if (restartModal) restartModal.addEventListener('click', function (e) { if (e.target.id === 'restartConfirmModal') hideRestartModal(); });

        if (modalConfirm) {
            modalConfirm.addEventListener('click', function () {
                hideRestartModal();
                if (indicator) {
                    indicator.style.display = 'block';
                    indicator.innerHTML = '<div class="restart-indicator"><div class="restart-spinner"></div><span>' + (window.I18N ? I18N.t('settings.restarting') : 'Restarting balancer...') + '</span></div>';
                }
                Api.post('/api/v1/admin/restart').then(function () {
                    if (indicator) indicator.innerHTML = '<div style="color: var(--success); padding: 8px;">' + (window.I18N ? I18N.t('settings.restarted') : 'Balancer restarted successfully') + '</div>';
                    showToast(window.I18N ? I18N.t('settings.restarted') : 'Balancer restarted successfully', 'success');
                }).catch(function (err) {
                    if (indicator) indicator.innerHTML = '<div style="color: var(--danger); padding: 8px;">' + (window.I18N ? I18N.t('settings.restart_error') : 'Balancer restart failed') + '</div>';
                    showToast((window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (err.message || err), 'error');
                });
            });
        }
    }

    // ---- Navigation ----

    function setupNavigation() {
        document.querySelectorAll('.nav-item').forEach(item => {
            item.addEventListener('click', (e) => {
                e.preventDefault();
                switchPage(item.dataset.page);
            });
        });
    }

    function switchPage(page) {
        document.querySelectorAll('.page').forEach(p => p.classList.remove('active'));
        document.querySelectorAll('.nav-item').forEach(n => n.classList.remove('active'));

        const targetPage = document.getElementById(page + '-page');
        const targetNav = document.querySelector(`[data-page="${page}"]`);

        if (targetPage) targetPage.classList.add('active');
        if (targetNav) targetNav.classList.add('active');

        currentPage = page;
        var titleKey = 'header.' + page;
        var h1 = document.getElementById('pageTitle');
        if (h1 && window.I18N) h1.textContent = I18N.t(titleKey);
        else Utils.setText('pageTitle', getPageTitle(page));

        refreshPage(page);

        if (page === 'backends' || page === 'models') {
            fetchClusterState();
        }
    }

    function getPageTitle(page) {
        // Fallback only — i18n handles actual titles via header.* keys
        const titles = {
            dashboard: 'Dashboard',
            monitor: 'Cluster Monitor',
            backends: 'Backend Management',
            models: 'Models',
            sessions: 'Sessions',
            queue: 'Queue',
            gguf: 'GGUF Models',
            logs: 'System Logs',
            settings: 'Settings',
            agents: 'Agents'
        };
        return titles[page] || 'Dashboard';
    }

    // Статусы, которые считаем нерабочими и скрываем в UI по умолчанию.
    var UNHEALTHY_STATUSES_UI = { unhealthy: true, offline: true, draining: true, ollama_unavailable: true };

    /**
     * Фильтрует бэкенды по текущему типу (ollama / llama_cpp) и по статусу.
     * Нерабочие бэкенды (unhealthy/offline/draining/ollama_unavailable) скрываются,
     * чтобы в WebUI не отображались заглушки/недоступные ноды.
     * Использует BackendTypeFilter как клиентский fallback.
     * Если сервер уже отфильтровал — фильтр пройдёт без изменений.
     */
    function filterBackendsForUI(backends) {
        if (!Array.isArray(backends)) return backends;
        var filtered = backends.filter(function (b) {
            return !UNHEALTHY_STATUSES_UI[b.status];
        });
        if (!window.BackendTypeFilter) return filtered;
        var type = BackendTypeFilter.getCurrentType();
        return BackendTypeFilter.filterBackends(filtered, type);
    }

    function refreshPage(page) {
        switch (page) {
            case 'dashboard':
                var filteredBackends = filterBackendsForUI(data.backends);
                dashboard(filteredBackends, data.sessions, data.queue);
                predictionAlerts(filteredBackends);
                break;
            case 'monitor':
                sendMonitorConfig();
                break;
            case 'backends':
                var filteredBackendsB = filterBackendsForUI(data.backends);
                backendsPage([...filteredBackendsB].sort(function(a, b) { return (a.id || '').localeCompare(b.id || ''); }));
                break;
            case 'models':
                modelsPage(filterBackendsForUI(data.backends));
                break;
            case 'sessions':
                sessionsPage(data.sessions);
                break;
            case 'queue':
                queuePage(data.queue, data.queueTasks, data.queueHistory);
                break;
            case 'logs':
                renderLogs(data.logs);
                // Also render proxy logs when switching to logs page (only if proxy tab is active)
                var proxyTab = document.getElementById('logsTabProxy');
                if (proxyTab && proxyTab.classList.contains('active')) {
                    renderProxyLogs(data.proxyLogs);
                }
                break;
            case 'agents':
                if (data.agents) renderAgentsPage(data.agents);
                break;
            case 'gguf':
                if (window.GgufRenderer) {
                    var container = document.getElementById('ggufContainer');
                    if (container) GgufRenderer.render(container);
                }
                break;
            case 'settings':
                loadSettings();
                setTimeout(function() { loadBackendLimits(); }, 100);
                break;
        }
    }

    function sendMonitorConfig() {
        const frame = document.getElementById('monitorFrame');
        if (!frame || !frame.contentWindow) return;
        const CFG = window.WEBUI_CONFIG || {};
        frame.contentWindow.postMessage({
            type: 'ollamalegion-config',
            apiBase: CFG.API_BASE || '',
            apiToken: CFG.API_TOKEN || '',
            refreshInterval: 2000,
            lang: localStorage.getItem('ollamalegion_lang') || 'ru'
        }, '*');
    }

    function refreshCurrentPage() {
        refreshPage(currentPage);
    }

    // ---- Event Listeners ----

    function setupEventListeners() {
        document.getElementById('refreshBtn').addEventListener('click', () => {
            refreshCurrentPage();
            showToast(window.I18N ? I18N.t('common.success') : 'Data updated', 'success');
        });

        document.getElementById('addBackendBtn').addEventListener('click', () => openBackendModal());
        if (document.getElementById('addBackendBtn2')) {
            document.getElementById('addBackendBtn2').addEventListener('click', () => openBackendModal());
        }

        document.getElementById('modalClose').addEventListener('click', closeModal);
        document.getElementById('modalCancel').addEventListener('click', closeModal);
        document.getElementById('modalSave').addEventListener('click', saveBackend);
        document.getElementById('modalDelete').addEventListener('click', deleteBackend);

        var backendSearch = document.getElementById('backendSearch');
        if (backendSearch) backendSearch.addEventListener('input', Utils.debounce((e) => filterBackends(e.target.value), 150));
        var sessionSearch = document.getElementById('sessionSearch');
        if (sessionSearch) sessionSearch.addEventListener('input', Utils.debounce((e) => filterSessions(e.target.value), 150));

        document.getElementById('clearLogs').addEventListener('click', () => {
            data.logs = [];
            renderLogs(data.logs);
        });
        var exportLogsBtn = document.getElementById('exportLogs');
        if (exportLogsBtn) exportLogsBtn.addEventListener('click', exportLogs);
        var exportBackendsBtn = document.getElementById('exportBackends');
        if (exportBackendsBtn) exportBackendsBtn.addEventListener('click', exportBackends);

        document.getElementById('saveSettings').addEventListener('click', function () { saveSettings(false); });
        document.getElementById('resetSettings').addEventListener('click', resetSettings);

        // Export / Import Config buttons
        var exportBtn = document.getElementById('exportSettingsBtn');
        if (exportBtn) exportBtn.addEventListener('click', function () {
            if (window.ConfigIO) ConfigIO.exportConfig();
        });
        var exportBtn2 = document.getElementById('exportSettingsBtn2');
        if (exportBtn2) exportBtn2.addEventListener('click', function () {
            if (window.ConfigIO) ConfigIO.exportConfig();
        });

        var importBtn = document.getElementById('importSettingsBtn');
        if (importBtn) importBtn.addEventListener('click', function () {
            if (window.ConfigIO) {
                ConfigIO.importConfigFromFile().then(function (data) {
                    return ConfigIO.showImportPreview(data);
                }).then(function (confirmed) {
                    if (confirmed) ConfigIO.applyConfig(data);
                }).catch(function (err) {
                    showToast(err.message || 'Import failed', 'error');
                });
            }
        });
        var importBtn2 = document.getElementById('importSettingsBtn2');
        if (importBtn2) importBtn2.addEventListener('click', function () {
            if (window.ConfigIO) {
                ConfigIO.importConfigFromFile().then(function (data) {
                    return ConfigIO.showImportPreview(data);
                }).then(function (confirmed) {
                    if (confirmed) ConfigIO.applyConfig(data);
                }).catch(function (err) {
                    showToast(err.message || 'Import failed', 'error');
                });
            }
        });

        // Auto-save on settings form changes
        var settingsFields = ['balancingAlgorithm', 'useEnhancedScoring', 'modelAffinity', 'sessionStickiness', 'predictionFiltering', 'gpuMaxUsage', 'vramMaxUsage', 'cpuMaxUsage', 'ramMaxUsage', 'minFreeDisk', 'modelReplicationMinInstances', 'modelReplicationMaxInstances', 'modelReplicationIdleUnload', 'rpcCoordinatorURL', 'rpcCoordinatorWorkerPort', 'rpcCoordinatorProtocol', 'rpcCoordinatorTimeout', 'virtualModelsCoordMode', 'virtualModelsTimeout', 'distInferenceGrpcPort', 'agentCollectInterval', 'agentHeartbeatInterval', 'agentMaxConcurrent', 'agentMaxModels', 'agentTimeout', 'gpuLayers', 'ctxSize', 'batchSize', 'gpuStrategy', 'tensorSplit', 'autoGpuDistribution', 'flashAttn', 'numa', 'useMmap'];
        settingsFields.forEach(function (id) {
            var el = document.getElementById(id);
            if (el) {
                el.addEventListener('change', autoSaveSettings);
                if (el.tagName === 'INPUT' && el.type === 'number') {
                    el.addEventListener('input', autoSaveSettings);
                }
            }
        });

        document.getElementById('backendModal').addEventListener('click', (e) => {
            if (e.target.id === 'backendModal') closeModal();
        });

        // Backend type selector in add-backend form — sync with BackendTypeFilter
        var formBackendType = document.getElementById('formBackendType');
        if (formBackendType) {
            formBackendType.addEventListener('change', function () {
                var newType = this.value;
                if (window.BackendTypeFilter) {
                    BackendTypeFilter.setCurrentType(newType);
                }
            });
        }

        // Type filter buttons (Все / Ollama / llama.cpp) на Dashboard и Backends page
        document.querySelectorAll('.type-filter-group').forEach(function(group) {
            group.querySelectorAll('.type-filter-btn').forEach(function(btn) {
                btn.addEventListener('click', function() {
                    // Снять active со всех кнопок в этой группе
                    group.querySelectorAll('.type-filter-btn').forEach(function(b) { b.classList.remove('active'); });
                    this.classList.add('active');
                    // Применить фильтр через BackendTypeFilter
                    var filterType = this.dataset.type;
                    if (window.BackendTypeFilter) {
                        BackendTypeFilter.setCurrentType(filterType);
                    }
                    // Обновить текущую страницу
                    refreshCurrentPage();
                });
            });
        });

        // Agents page buttons
        var refreshAgentsBtn = document.getElementById('refreshAgentsBtn');
        if (refreshAgentsBtn) {
            refreshAgentsBtn.addEventListener('click', function() {
                fetchAgents();
            });
        }
        var closeAgentDetailsBtn = document.getElementById('closeAgentDetails');
        if (closeAgentDetailsBtn) {
            closeAgentDetailsBtn.addEventListener('click', function() {
                var card = document.getElementById('agentDetailsCard');
                if (card) card.style.display = 'none';
            });
        }

        // Models Manage button on Models page
        var modelsManageBtn = document.getElementById('modelsManageBtn');
        if (modelsManageBtn) {
            modelsManageBtn.addEventListener('click', function() {
                if (data.backends && data.backends.length > 0) {
                    openModelManageModal(data.backends[0].id);
                } else {
                    showToast(window.I18N ? I18N.t('models.no_backends') : 'No backends available', 'error');
                }
            });
        }

        // Models search/filter
        var modelsSearch = document.getElementById('modelsSearch');
        if (modelsSearch) {
            modelsSearch.addEventListener('input', Utils.debounce(function(e) {
                filterModels(e.target.value);
            }, 150));
        }

        // Models refresh button
        var refreshModelsBtn = document.getElementById('refreshModelsBtn');
        if (refreshModelsBtn) {
            refreshModelsBtn.addEventListener('click', function() {
                fetchClusterState().then(function() {
                    modelsPage(data.backends);
                    showToast(window.I18N ? I18N.t('common.success') : 'Models refreshed', 'success');
                });
            });
        }

        // Model Management Modal buttons
        var modelManageCloseBtn = document.getElementById('modelManageClose');
        if (modelManageCloseBtn) {
            modelManageCloseBtn.addEventListener('click', closeModelManageModal);
        }
        var modelManageCancelBtn = document.getElementById('modelManageCancel');
        if (modelManageCancelBtn) {
            modelManageCancelBtn.addEventListener('click', closeModelManageModal);
        }
        var modelManageModal = document.getElementById('modelManageModal');
        if (modelManageModal) {
            modelManageModal.addEventListener('click', function(e) {
                if (e.target.id === 'modelManageModal') closeModelManageModal();
            });
        }
        var modelPullBtn = document.getElementById('modelPullBtn');
        if (modelPullBtn) {
            modelPullBtn.addEventListener('click', function() {
                var backendId = modelManageModal ? modelManageModal.dataset.backendId : null;
                if (!backendId) {
                    showToast('Backend ID not found', 'error');
                    return;
                }
                var modelName = document.getElementById('modelPullName') ? document.getElementById('modelPullName').value.trim() : '';
                if (!modelName) {
                    showToast(window.I18N ? I18N.t('models.model_name_placeholder') : 'Enter model name', 'error');
                    return;
                }
                var insecure = document.getElementById('modelPullInsecure') ? document.getElementById('modelPullInsecure').checked : false;
                var options = {};
                if (insecure) options.insecure = true;
                executeModelOperation(backendId, 'pull', modelName, options);
            });
        }

        // Proxy logs buttons
        var clearProxyLogsBtn = document.getElementById('clearProxyLogs');
        if (clearProxyLogsBtn) {
            clearProxyLogsBtn.addEventListener('click', function() {
                data.proxyLogs = [];
                renderProxyLogs(data.proxyLogs);
            });
        }
        var refreshProxyLogsBtn = document.getElementById('refreshProxyLogs');
        if (refreshProxyLogsBtn) {
            refreshProxyLogsBtn.addEventListener('click', function() {
                fetchProxyLogs();
            });
        }
        var copyProxyLogsBtn = document.getElementById('copyProxyLogs');
        if (copyProxyLogsBtn) {
            copyProxyLogsBtn.addEventListener('click', function() {
                renderCopyProxyLogs();
            });
        }

        // Backend Limits button
        var saveBackendLimitsBtn = document.getElementById('saveBackendLimitsBtn');
        if (saveBackendLimitsBtn) {
            saveBackendLimitsBtn.addEventListener('click', function() {
                saveBackendLimits();
            });
        }

        // CppWorker port Auto-detect button (форма регистрации бэкенда)
        var cppPortDetectBtn = document.getElementById('formBackendCppWorkerPortDetect');
        if (cppPortDetectBtn) {
            cppPortDetectBtn.addEventListener('click', function () {
                var hostEl = document.getElementById('formBackendHost');
                var portEl = document.getElementById('formBackendCppWorkerPort');
                var host = hostEl ? hostEl.value.trim() : '';
                if (!host) {
                    showToast('Сначала укажите host', 'error');
                    return;
                }
                cppPortDetectBtn.disabled = true;
                var originalText = cppPortDetectBtn.innerHTML;
                cppPortDetectBtn.innerHTML = '⏳ Detecting…';
                autoDetectCppWorkerPort(host).then(function (detected) {
                    cppPortDetectBtn.disabled = false;
                    cppPortDetectBtn.innerHTML = originalText;
                    if (detected) {
                        if (portEl) portEl.value = detected;
                        showToast('Обнаружен порт CppWorker: ' + detected, 'success');
                    } else {
                        showToast('CppWorker не найден на портах 18092/18091/18093/18090. Укажите вручную.', 'error');
                    }
                }).catch(function () {
                    cppPortDetectBtn.disabled = false;
                    cppPortDetectBtn.innerHTML = originalText;
                    showToast('Ошибка auto-detect', 'error');
                });
            });
        }

        // Retake Setup Wizard button (Danger Zone)
        var retakeBtn = document.getElementById('retakeSetupWizardBtn');
        if (retakeBtn) {
            retakeBtn.addEventListener('click', function() {
                var confirmed = confirm(
                    (window.I18N ? I18N.t('wizard.retake_confirm') : 'Все текущие настройки будут сброшены. Продолжить?')
                );
                if (!confirmed) return;

                // Сбрасываем initialized на сервере
                if (window.Api && window.Api.updateConfig) {
                    window.Api.updateConfig({ initialized: false }).then(function () {
                        localStorage.removeItem('ollamalegion_wizard_done');
                        localStorage.removeItem('ollamalegion_backend_type');
                        if (window.SetupWizard) {
                            window.SetupWizard.start();
                        }
                    }).catch(function (err) {
                        showToast(
                            (window.I18N ? I18N.t('common.error') : 'Ошибка') + ': ' + (err.message || err),
                            'error'
                        );
                    });
                } else {
                    localStorage.removeItem('ollamalegion_wizard_done');
                    if (window.SetupWizard) {
                        window.SetupWizard.start();
                    }
                }
            });
        }
    }

    // ---- WebSocket Events ----

    function setupWebSocketEvents() {
        window.addEventListener('ws-open', () => {
            updateConnectionStatus(true);
            addLog(window.I18N ? I18N.t('app.ws_connected') : 'WebSocket connected', 'info');
        });

        window.addEventListener('ws-status', (e) => {
            updateConnectionStatus(e.detail.connected);
        });

        window.addEventListener('ws-error', () => {
            updateConnectionStatus(false);
            addLog(window.I18N ? I18N.t('app.ws_error') : 'WebSocket error', 'error');
        });

        window.addEventListener('ws-reconnecting', (e) => {
            const { attempt, max, delay } = e.detail;
            addLog(window.I18N ? I18N.t('app.ws_reconnect', { attempt: attempt, max: max, delay: Math.round(delay / 1000) }) : `Reconnecting... (${attempt}/${max}) in ${Math.round(delay / 1000)}s`, 'warn');
        });

        window.addEventListener('ws-max-reconnect', () => {
            addLog(window.I18N ? I18N.t('app.ws_max_reconnect') : 'Max reconnection attempts reached', 'error');
        });

        window.addEventListener('ws-message', (e) => {
            handleWebSocketData(e.detail);
        });

        WebSocketManager.connect();
    }

    function handleWebSocketData(payload) {
        const eventType = payload.eventType || 'legacy';

        switch (eventType) {
            case 'clusterState':
                updateBackends(payload.data?.backends || []);
                break;
            case 'backendAdd':
                addLog(window.I18N ? I18N.t('app.backend_added', { name: payload.data?.name || payload.backendId }) : `Backend added: ${payload.data?.name || payload.backendId}`, 'info');
                fetchClusterState();
                break;
            case 'backendRemove':
                addLog(window.I18N ? I18N.t('app.backend_removed', { id: payload.backendId }) : `Backend removed: ${payload.backendId}`, 'info');
                fetchClusterState();
                break;
            case 'statusChange':
                addLog(window.I18N ? I18N.t('app.status_changed', { id: payload.backendId, old: payload.data?.oldStatus, new: payload.data?.newStatus }) : `Status ${payload.backendId}: ${payload.data?.oldStatus} \u2192 ${payload.data?.newStatus}`, 'warning');
                applyStatusChange(payload.backendId, payload.data?.newStatus);
                break;
            case 'limitsChange':
                addLog(window.I18N ? I18N.t('app.limits_changed', { id: payload.backendId }) : `Limits ${payload.backendId} updated`, 'info');
                fetchClusterState();
                break;
            case 'proxy_log':
                if (payload.data?.entry) {
                    addProxyLog(payload.data.entry);
                }
                break;
            case 'ping':
                break;
            case 'legacy':
            default:
                if (payload.backends) {
                    updateBackends(payload.backends || []);
                }
                break;
        }
    }

    function backendsEqual(a, b) {
        if (a.length !== b.length) return false;
        const normalize = arr => JSON.stringify(arr.map(x => ({
            id: x.id,
            status: x.status,
            activeRequests: x.activeRequests,
            'gpu.usagePercent': x.gpu?.usagePercent,
            'gpu.memoryUsed': x.gpu?.memoryUsed,
            'system.cpuUsagePercent': x.system?.cpuUsagePercent,
            'system.memoryUsed': x.system?.memoryUsed,
            'prediction.secondsToCritical': x.prediction?.secondsToCritical
        })).sort((m, n) => m.id.localeCompare(n.id)));
        return normalize(a) === normalize(b);
    }

    function updateBackends(newBackends) {
        newBackends = [...newBackends].sort((a, b) => (a.id || '').localeCompare(b.id || ''));
        if (backendsEqual(data.backends, newBackends)) return;
        data.backends = newBackends;
        if (currentPage === 'dashboard') scheduleDashboardRender();
    }

    function applyStatusChange(backendId, newStatus) {
        const b = data.backends.find(x => x.id === backendId);
        if (b && b.status !== newStatus) {
            b.status = newStatus;
            if (currentPage === 'dashboard') scheduleDashboardRender();
        } else if (!b) {
            fetchClusterState();
        }
    }

    function scheduleDashboardRender() {
        if (dashboardRenderTimer) clearTimeout(dashboardRenderTimer);
        dashboardRenderTimer = setTimeout(() => {
            dashboardRenderTimer = null;
            refreshPage('dashboard');
        }, 250);
    }

    // ---- API Events ----

    function setupApiEvents() {
        window.addEventListener('api-error', (e) => {
            const { message, error } = e.detail;
            showToast(message, 'error');
            addLog(message, 'error');
            // If it's a network error or the balancer is unreachable, mark as disconnected
            if (message && (message.includes('Network error') || message.includes('Failed to fetch') || message.includes('NetworkError'))) {
                updateConnectionStatus(false);
            }
        });
    }

    // ---- Data Fetching ----

    async function fetchClusterState() {
        try {
            const state = await Api.cluster();
            updateBackends(state.backends || []);
            // Successful REST request — balancer is reachable
            updateConnectionStatus(true);
            // Синхронизация типа бэкенда (Ollama vs llama.cpp) — скрывает/показывает вкладку GGUF и режимы
            if (window.BackendTypeFilter) {
                BackendTypeFilter.syncFromClusterState(state);
            }
            // Бейдж уже обновлён внутри BackendTypeFilter.syncFromClusterState → updateUI → updateEngineBadge
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_cluster') : 'Error loading cluster state');
        }
    }

    async function fetchQueue() {
        try {
            data.queue = await Api.queueStats();
            Utils.setText('queueSize', data.queue.current_size || 0);
            Utils.setText('queueProcessed', (data.queue.processed_total || 0) + ' ' + (window.I18N ? I18N.t('app.processed') : 'processed'));
            if (currentPage === 'queue') refreshPage('queue');
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
            if (currentPage === 'queue') refreshPage('queue');
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_queue_details') : 'Error loading queue details');
            data.queueTasks = [];
            if (currentPage === 'queue') refreshPage('queue');
        }
    }

    async function fetchQueueHistory() {
        try {
            const res = await Api.queueHistory();
            data.queueHistory = res.history || [];
            if (currentPage === 'queue') refreshPage('queue');
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_queue_history') : 'Error loading queue history');
            data.queueHistory = [];
            if (currentPage === 'queue') refreshPage('queue');
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
            if (currentPage === 'sessions') refreshPage('sessions');
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
        if (document.getElementById('setupWizardModal')) return;

        var algEl = document.getElementById('balancingAlgorithm');
        if (algEl && serverConfig.algorithm) algEl.value = serverConfig.algorithm;
        var maEl = document.getElementById('modelAffinity');
        if (maEl) maEl.checked = serverConfig.modelAffinity !== false;
        var ssEl = document.getElementById('sessionStickiness');
        if (ssEl) ssEl.checked = serverConfig.sessionStickiness !== false;

        var useESEl = document.getElementById('useEnhancedScoring');
        if (useESEl) useESEl.checked = serverConfig.useEnhancedScoring !== false;

        var pfEl = document.getElementById('predictionFiltering');
        if (pfEl) pfEl.checked = serverConfig.predictionFiltering !== false;

        var gpuEl = document.getElementById('gpuMaxUsage');
        if (gpuEl && serverConfig.gpuMaxUsage) gpuEl.value = serverConfig.gpuMaxUsage;
        var vramEl = document.getElementById('vramMaxUsage');
        if (vramEl && serverConfig.vramMaxUsage) vramEl.value = serverConfig.vramMaxUsage;
        var cpuEl = document.getElementById('cpuMaxUsage');
        if (cpuEl && serverConfig.cpuMaxUsage) cpuEl.value = serverConfig.cpuMaxUsage;
        var ramEl = document.getElementById('ramMaxUsage');
        if (ramEl && serverConfig.ramMaxUsage) ramEl.value = serverConfig.ramMaxUsage;
        var diskEl = document.getElementById('minFreeDisk');
        if (diskEl && serverConfig.minFreeDisk) diskEl.value = serverConfig.minFreeDisk;
        var tokenEl = document.getElementById('apiToken');
        if (tokenEl && serverConfig.apiToken) tokenEl.value = serverConfig.apiToken;

        // Backend Engine — серверный конфиг как fallback.
        // Приоритет: localStorage (выбор пользователя) > серверный config.
        if (serverConfig.backendEngine) {
            var engineType = (serverConfig.backendEngine === 'llama_cpp') ? 'llama_cpp' : 'ollama';
            var currentType = window.BackendTypeFilter ? BackendTypeFilter.getCurrentType() : null;
            // Если пользователь уже выбрал тип — не перезаписываем UI из сервера
            if (currentType !== 'llama_cpp' && currentType !== 'ollama') {
                if (window.BackendTypeFilter) {
                    BackendTypeFilter.updateUI(engineType);
                }
            }
            var formBackendTypeEl = document.getElementById('formBackendType');
            if (formBackendTypeEl) {
                formBackendTypeEl.value = engineType;
            }
            // Обновляем маркер типа движка в сайдбаре (текст + иконка)
            var badge = document.getElementById('backendEngineBadge');
            var label = document.getElementById('backendEngineLabel');
            var iconEl = badge ? badge.querySelector('.engine-icon') : null;
            if (badge) {
                badge.classList.remove('engine-ollama', 'engine-llama_cpp', 'engine-auto');
                badge.classList.add('engine-' + engineType);
            }
            if (label) {
                label.textContent = engineType === 'llama_cpp' ? '🦒 llama.cpp' : '🦙 Ollama API';
            }
            if (iconEl) {
                iconEl.textContent = engineType === 'llama_cpp' ? '🦒' : '🦙';
            }
        }

        // Operating Mode — критически важно для корректного отображения UI.
        // Сервер = единственный источник истины. Перезаписываем UI жёстко.
        if (serverConfig.operatingMode) {
            if (window.SettingsUI && SettingsUI.syncModeFromServer) {
                SettingsUI.syncModeFromServer(serverConfig.operatingMode);
            } else {
                // Fallback (устаревший путь)
                var radio = document.querySelector('input[name="operatingMode"][value="' + serverConfig.operatingMode + '"]');
                if (radio) radio.checked = true;
            }
        }

        // RPC Settings — Model Replication (Вариант A)
        var mrEnabledEl = document.getElementById('modelReplicationEnabled');
        if (mrEnabledEl && serverConfig.modelReplication) mrEnabledEl.checked = serverConfig.modelReplication.enabled === true;
        var mrMinEl = document.getElementById('modelReplicationMinInstances');
        if (mrMinEl && serverConfig.modelReplication && serverConfig.modelReplication.defaultMinInstances) mrMinEl.value = serverConfig.modelReplication.defaultMinInstances;
        var mrMaxEl = document.getElementById('modelReplicationMaxInstances');
        if (mrMaxEl && serverConfig.modelReplication && serverConfig.modelReplication.defaultMaxInstances) mrMaxEl.value = serverConfig.modelReplication.defaultMaxInstances;
        var mrIdleEl = document.getElementById('modelReplicationIdleUnload');
        if (mrIdleEl && serverConfig.modelReplication && serverConfig.modelReplication.idleUnloadAfter) mrIdleEl.value = serverConfig.modelReplication.idleUnloadAfter;

        // RPC Settings — RPC Coordinator (Вариант B)
        var rcEnabledEl = document.getElementById('rpcCoordinatorEnabled');
        if (rcEnabledEl && serverConfig.rpcCoordinator) rcEnabledEl.checked = serverConfig.rpcCoordinator.enabled === true;
        var rcUrlEl = document.getElementById('rpcCoordinatorURL');
        if (rcUrlEl && serverConfig.rpcCoordinator && serverConfig.rpcCoordinator.coordinatorURL) rcUrlEl.value = serverConfig.rpcCoordinator.coordinatorURL;
        var rcPortEl = document.getElementById('rpcCoordinatorWorkerPort');
        if (rcPortEl && serverConfig.rpcCoordinator && serverConfig.rpcCoordinator.workerPort) rcPortEl.value = serverConfig.rpcCoordinator.workerPort;
        var rcProtoEl = document.getElementById('rpcCoordinatorProtocol');
        if (rcProtoEl && serverConfig.rpcCoordinator && serverConfig.rpcCoordinator.protocol) rcProtoEl.value = serverConfig.rpcCoordinator.protocol;
        var rcTimeoutEl = document.getElementById('rpcCoordinatorTimeout');
        if (rcTimeoutEl && serverConfig.rpcCoordinator && serverConfig.rpcCoordinator.timeout) rcTimeoutEl.value = serverConfig.rpcCoordinator.timeout;

        // RPC Settings — Virtual Models (Вариант C)
        var vmEnabledEl = document.getElementById('virtualModelsEnabled');
        if (vmEnabledEl && serverConfig.virtualModels) vmEnabledEl.checked = serverConfig.virtualModels.enabled === true;
        var vmModeEl = document.getElementById('virtualModelsCoordMode');
        if (vmModeEl && serverConfig.virtualModels && serverConfig.virtualModels.coordMode) vmModeEl.value = serverConfig.virtualModels.coordMode;
        var vmTimeoutEl = document.getElementById('virtualModelsTimeout');
        if (vmTimeoutEl && serverConfig.virtualModels && serverConfig.virtualModels.timeout) vmTimeoutEl.value = serverConfig.virtualModels.timeout;

        // RPC Settings — Distributed Inference (Вариант D)
        var diEnabledEl = document.getElementById('distInferenceEnabled');
        if (diEnabledEl && serverConfig.distInference) diEnabledEl.checked = serverConfig.distInference.enabled === true;
        var diPortEl = document.getElementById('distInferenceGrpcPort');
        if (diPortEl && serverConfig.distInference && serverConfig.distInference.grpcPort) diPortEl.value = serverConfig.distInference.grpcPort;

        // llama.cpp / GGUF Settings
        var llamaCpp = serverConfig.llamaCpp || {};
        var gpuLayersEl = document.getElementById('gpuLayers');
        if (gpuLayersEl && llamaCpp.numGpuLayers !== undefined) gpuLayersEl.value = llamaCpp.numGpuLayers;
        var ctxSizeEl = document.getElementById('ctxSize');
        if (ctxSizeEl && llamaCpp.contextLength) ctxSizeEl.value = llamaCpp.contextLength;
        var batchSizeEl = document.getElementById('batchSize');
        if (batchSizeEl && llamaCpp.batchSize) batchSizeEl.value = llamaCpp.batchSize;
        var gpuStrategyEl = document.getElementById('gpuStrategy');
        if (gpuStrategyEl && llamaCpp.strategy) gpuStrategyEl.value = llamaCpp.strategy;
        var tensorSplitEl = document.getElementById('tensorSplit');
        if (tensorSplitEl && llamaCpp.tensorSplitStr) tensorSplitEl.value = llamaCpp.tensorSplitStr;
        else if (tensorSplitEl && llamaCpp.tensorSplit && Array.isArray(llamaCpp.tensorSplit)) tensorSplitEl.value = llamaCpp.tensorSplit.join(',');
        var autoGpuEl = document.getElementById('autoGpuDistribution');
        if (autoGpuEl) autoGpuEl.checked = llamaCpp.autoGpuDistribution !== false;
        var flashAttnEl = document.getElementById('flashAttn');
        if (flashAttnEl && llamaCpp.flashAttention !== undefined) flashAttnEl.checked = !!llamaCpp.flashAttention;
        var numaEl = document.getElementById('numa');
        if (numaEl && llamaCpp.numa !== undefined) numaEl.checked = !!llamaCpp.numa;
        var useMmapEl = document.getElementById('useMmap');
        if (useMmapEl) useMmapEl.checked = llamaCpp.useMmap !== false;
    }

    function applyLocalConfig(config) {
        if (!config) return;
        var algEl = document.getElementById('balancingAlgorithm');
        if (algEl && config.algorithm) algEl.value = config.algorithm;
        var maEl = document.getElementById('modelAffinity');
        if (maEl) maEl.checked = config.modelAffinity !== false;
        var ssEl = document.getElementById('sessionStickiness');
        if (ssEl) ssEl.checked = config.sessionStickiness !== false;
        var pfEl = document.getElementById('predictionFiltering');
        if (pfEl) pfEl.checked = config.predictionFiltering !== false;
        var gpuEl = document.getElementById('gpuMaxUsage');
        if (gpuEl && config.gpuMaxUsage) gpuEl.value = config.gpuMaxUsage;
        var vramEl = document.getElementById('vramMaxUsage');
        if (vramEl && config.vramMaxUsage) vramEl.value = config.vramMaxUsage;
        var cpuEl = document.getElementById('cpuMaxUsage');
        if (cpuEl && config.cpuMaxUsage) cpuEl.value = config.cpuMaxUsage;
        var ramEl = document.getElementById('ramMaxUsage');
        if (ramEl && config.ramMaxUsage) ramEl.value = config.ramMaxUsage;
        var diskEl = document.getElementById('minFreeDisk');
        if (diskEl && config.minFreeDisk) diskEl.value = config.minFreeDisk;
        var tokenEl = document.getElementById('apiToken');
        if (tokenEl && config.apiToken) tokenEl.value = config.apiToken;

        // Operating Mode
        if (config.operatingMode) {
            var radio = document.querySelector('input[name="operatingMode"][value="' + config.operatingMode + '"]');
            if (radio) {
                radio.checked = true;
                if (window.SettingsUI) {
                    SettingsUI.updateModeCards(config.operatingMode);
                    SettingsUI.showModeFields(config.operatingMode);
                }
            }
        }

        if (config.agent) {
            var ag = config.agent;
            var agentCollEl = document.getElementById('agentCollectInterval');
            if (agentCollEl && ag.collectInterval) agentCollEl.value = ag.collectInterval;
            var agentHbEl = document.getElementById('agentHeartbeatInterval');
            if (agentHbEl && ag.heartbeatInterval) agentHbEl.value = ag.heartbeatInterval;
            var agentMcEl = document.getElementById('agentMaxConcurrent');
            if (agentMcEl && ag.maxConcurrentRequests) agentMcEl.value = ag.maxConcurrentRequests;
            var agentMmEl = document.getElementById('agentMaxModels');
            if (agentMmEl && ag.maxModels) agentMmEl.value = ag.maxModels;
            var agentToEl = document.getElementById('agentTimeout');
            if (agentToEl && ag.timeout) agentToEl.value = ag.timeout;
        }
    }

    function saveSettings(silent) {
        var algorithm = (document.getElementById('balancingAlgorithm') && document.getElementById('balancingAlgorithm').value) || 'resource-aware';
        var useEnhancedScoring = (document.getElementById('useEnhancedScoring') && document.getElementById('useEnhancedScoring').checked) !== false;
        var modelAffinity = (document.getElementById('modelAffinity') && document.getElementById('modelAffinity').checked) !== false;
        var sessionStickiness = (document.getElementById('sessionStickiness') && document.getElementById('sessionStickiness').checked) !== false;
        var predictionFiltering = (document.getElementById('predictionFiltering') && document.getElementById('predictionFiltering').checked) !== false;
        var gpuMax = parseInt((document.getElementById('gpuMaxUsage') && document.getElementById('gpuMaxUsage').value)) || 90;
        var vramMax = parseInt((document.getElementById('vramMaxUsage') && document.getElementById('vramMaxUsage').value)) || 85;
        var cpuMax = parseInt((document.getElementById('cpuMaxUsage') && document.getElementById('cpuMaxUsage').value)) || 80;
        var ramMax = parseInt((document.getElementById('ramMaxUsage') && document.getElementById('ramMaxUsage').value)) || 85;
        var minDisk = parseInt((document.getElementById('minFreeDisk') && document.getElementById('minFreeDisk').value)) || 10240;
        var apiToken = (document.getElementById('apiToken') && document.getElementById('apiToken').value) || '';

        // RPC settings
        var modelReplicationEnabled = document.getElementById('modelReplicationEnabled') ? document.getElementById('modelReplicationEnabled').checked : false;
        var modelReplicationMinInstances = parseInt((document.getElementById('modelReplicationMinInstances') && document.getElementById('modelReplicationMinInstances').value)) || 1;
        var modelReplicationMaxInstances = parseInt((document.getElementById('modelReplicationMaxInstances') && document.getElementById('modelReplicationMaxInstances').value)) || 3;
        var modelReplicationIdleUnload = (document.getElementById('modelReplicationIdleUnload') && document.getElementById('modelReplicationIdleUnload').value) || '10m';

        var rpcCoordinatorEnabled = document.getElementById('rpcCoordinatorEnabled') ? document.getElementById('rpcCoordinatorEnabled').checked : false;
        var rpcCoordinatorURL = (document.getElementById('rpcCoordinatorURL') && document.getElementById('rpcCoordinatorURL').value) || '';
        var rpcCoordinatorWorkerPort = parseInt((document.getElementById('rpcCoordinatorWorkerPort') && document.getElementById('rpcCoordinatorWorkerPort').value)) || 18050;
        var rpcCoordinatorProtocol = (document.getElementById('rpcCoordinatorProtocol') && document.getElementById('rpcCoordinatorProtocol').value) || 'http';
        var rpcCoordinatorTimeout = (document.getElementById('rpcCoordinatorTimeout') && document.getElementById('rpcCoordinatorTimeout').value) || '30s';

        var virtualModelsEnabled = document.getElementById('virtualModelsEnabled') ? document.getElementById('virtualModelsEnabled').checked : false;
        var virtualModelsCoordMode = (document.getElementById('virtualModelsCoordMode') && document.getElementById('virtualModelsCoordMode').value) || 'sequential';
        var virtualModelsTimeout = parseInt((document.getElementById('virtualModelsTimeout') && document.getElementById('virtualModelsTimeout').value)) || 30000;

        var distInferenceEnabled = document.getElementById('distInferenceEnabled') ? document.getElementById('distInferenceEnabled').checked : false;
        var distInferenceGrpcPort = parseInt((document.getElementById('distInferenceGrpcPort') && document.getElementById('distInferenceGrpcPort').value)) || 19000;

        // Agent settings
        var agentCollectInterval = parseInt((document.getElementById('agentCollectInterval') && document.getElementById('agentCollectInterval').value)) || 15;
        var agentHeartbeatInterval = parseInt((document.getElementById('agentHeartbeatInterval') && document.getElementById('agentHeartbeatInterval').value)) || 30;
        var agentMaxConcurrent = parseInt((document.getElementById('agentMaxConcurrent') && document.getElementById('agentMaxConcurrent').value)) || 10;
        var agentMaxModels = parseInt((document.getElementById('agentMaxModels') && document.getElementById('agentMaxModels').value)) || 5;
        var agentTimeout = parseInt((document.getElementById('agentTimeout') && document.getElementById('agentTimeout').value)) || 10;

        // llama.cpp / GGUF settings (имена полей соответствуют Go-структуре LlamaCppConfig)
        var llamaCppGpuLayers = parseInt((document.getElementById('gpuLayers') && document.getElementById('gpuLayers').value)) || -1;
        var llamaCppCtxSize = parseInt((document.getElementById('ctxSize') && document.getElementById('ctxSize').value)) || 2048;
        var llamaCppBatchSize = parseInt((document.getElementById('batchSize') && document.getElementById('batchSize').value)) || 512;
        var llamaCppStrategy = (document.getElementById('gpuStrategy') && document.getElementById('gpuStrategy').value) || 'vram-ratio';
        var llamaCppTensorSplitStr = (document.getElementById('tensorSplit') && document.getElementById('tensorSplit').value) || '';
        var llamaCppAutoGpu = (document.getElementById('autoGpuDistribution') && document.getElementById('autoGpuDistribution').checked) !== false;
        var llamaCppFlashAttn = (document.getElementById('flashAttn') && document.getElementById('flashAttn').checked) === true;
        var llamaCppNuma = (document.getElementById('numa') && document.getElementById('numa').checked) === true;
        var llamaCppUseMmap = (document.getElementById('useMmap') && document.getElementById('useMmap').checked) !== false;

        // Определяем текущий operatingMode из radio-кнопок на странице
        var operatingModeRadio = document.querySelector('input[name="operatingMode"]:checked');
        var operatingMode = operatingModeRadio ? operatingModeRadio.value : 'standard';

        // Определяем текущий backendEngine из BackendTypeFilter (источник истины после мастера)
        var backendEngine = null;
        if (window.BackendTypeFilter) {
            var currentType = BackendTypeFilter.getCurrentType();
            if (currentType === 'llama_cpp') {
                backendEngine = 'llama_cpp';
            } else if (currentType === 'ollama') {
                backendEngine = 'ollama_api';
            }
            // если тип не определён (null/undefined) — не включаем backendEngine в payload,
            // чтобы не перезаписать существующее значение на сервере
        }

        var config = {
            operatingMode: operatingMode,
            agent: {
                collectInterval: agentCollectInterval,
                heartbeatInterval: agentHeartbeatInterval,
                maxConcurrentRequests: agentMaxConcurrent,
                maxModels: agentMaxModels,
                timeout: agentTimeout
            },
            algorithm: algorithm,
            useEnhancedScoring: useEnhancedScoring,
            modelAffinity: modelAffinity,
            sessionStickiness: sessionStickiness,
            predictionFiltering: predictionFiltering,
            gpuMaxUsage: gpuMax,
            vramMaxUsage: vramMax,
            cpuMaxUsage: cpuMax,
            ramMaxUsage: ramMax,
            minFreeDisk: minDisk,
            apiToken: apiToken,
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
                protocol: rpcCoordinatorProtocol,
                timeout: rpcCoordinatorTimeout
            },
            virtualModels: {
                enabled: virtualModelsEnabled,
                coordMode: virtualModelsCoordMode,
                timeout: virtualModelsTimeout
            },
            distInference: {
                enabled: distInferenceEnabled,
                grpcPort: distInferenceGrpcPort
            },
            llamaCpp: {
                numGpuLayers: llamaCppGpuLayers,
                contextLength: llamaCppCtxSize,
                batchSize: llamaCppBatchSize,
                strategy: llamaCppStrategy,
                tensorSplitStr: llamaCppTensorSplitStr,
                autoGpuDistribution: llamaCppAutoGpu,
                flashAttention: llamaCppFlashAttn,
                numa: llamaCppNuma,
                useMmap: llamaCppUseMmap
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
            if (!silent) showToast(window.I18N ? I18N.t('settings.saved') : 'Settings saved', 'success');
        }).catch(function (err) {
            if (!silent) showToast((window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (err.message || err), 'error');
        });
    }

    /**
     * SERVER-FIRST verify: после PUT делаем GET и сравниваем критические поля.
     * При расхождении перезагружаем UI из сервера (источник истины).
     */
    function verifySync(expectedConfig) {
        return Api.config().then(function (serverConfig) {
            // Всегда перезагружаем UI из сервера для гарантии синхронности
            applyServerConfig(serverConfig);

            var mismatches = [];
            if (expectedConfig.algorithm && serverConfig.algorithm !== expectedConfig.algorithm) {
                mismatches.push('algorithm: expected ' + expectedConfig.algorithm + ', got ' + serverConfig.algorithm);
            }
            if (expectedConfig.operatingMode && serverConfig.operatingMode !== expectedConfig.operatingMode) {
                mismatches.push('operatingMode: expected ' + expectedConfig.operatingMode + ', got ' + serverConfig.operatingMode);
            }
            if (expectedConfig.useEnhancedScoring !== undefined && serverConfig.useEnhancedScoring !== expectedConfig.useEnhancedScoring) {
                mismatches.push('useEnhancedScoring: expected ' + expectedConfig.useEnhancedScoring + ', got ' + serverConfig.useEnhancedScoring);
            }
            if (expectedConfig.backendEngine && serverConfig.backendEngine !== expectedConfig.backendEngine) {
                mismatches.push('backendEngine: expected ' + expectedConfig.backendEngine + ', got ' + serverConfig.backendEngine);
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

    // ---- Periodic Refresh ----

    function startPeriodicRefresh() {
        const interval = (window.WEBUI_CONFIG?.REFRESH_INTERVAL || 5000);
        refreshTimer = setInterval(() => {
            fetchQueue();
            fetchQueueDetails();
            fetchQueueHistory();
            fetchSessions();
        }, interval);
    }

    // ---- Backend CRUD ----

    function openBackendModal(backendId = null) {
        const modal = document.getElementById('backendModal');
        const title = document.getElementById('modalTitle');
        const deleteBtn = document.getElementById('modalDelete');

        if (backendId) {
            // Берём метрики для отображения (есть gpu, prediction)
            const backend = data.backends.find(b => b.id === backendId);
            if (!backend) return;
            title.textContent = window.I18N ? I18N.t('backends.edit') : 'Edit Backend';
            deleteBtn.style.display = 'inline-block';
            // Сначала показываем форму с данными из кластера
            fillForm(backend, true);
            // Асинхронно подгружаем конфиг бэкенда с weight, maxModels и т.д.
            Api.getBackend(backendId).then(function (config) {
                // config — это BackendMetrics с полями Config внутри (weight, maxConcurrentReqs...)
                // или сам Backend-объект с weight
                if (config && config.weight !== undefined && config.weight !== null) {
                    document.getElementById('formBackendWeight').value = config.weight;
                }
                if (config && config.maxConcurrentRequests !== undefined && config.maxConcurrentRequests !== null) {
                    document.getElementById('formBackendMaxConcurrent').value = config.maxConcurrentRequests;
                }
                if (config && config.maxConcurrentReqs !== undefined && config.maxConcurrentReqs !== null) {
                    document.getElementById('formBackendMaxConcurrent').value = config.maxConcurrentReqs;
                }
                if (config && (config.maxModels !== undefined && config.maxModels !== null && config.maxModels !== 0)) {
                    document.getElementById('formBackendMaxModels').value = config.maxModels;
                }
                if (config && config.runtimeMaxModels !== undefined && config.runtimeMaxModels !== null && config.runtimeMaxModels !== 0) {
                    document.getElementById('formBackendMaxModels').value = config.runtimeMaxModels;
                }
                if (config && (config.runtimeMaxConcurrentRequests !== undefined && config.runtimeMaxConcurrentRequests !== null && config.runtimeMaxConcurrentRequests !== 0)) {
                    document.getElementById('formBackendMaxConcurrent').value = config.runtimeMaxConcurrentRequests;
                }
                if (config && config.gpuMode) {
                    var modeEl = document.getElementById('formBackendGpuMode');
                    if (modeEl) modeEl.value = config.gpuMode;
                }
            }).catch(function () {
                // Не фатально — данные уже загружены из кластера
            });
        } else {
            title.textContent = window.I18N ? I18N.t('backends.add') : 'Add Backend';
            deleteBtn.style.display = 'none';
            fillForm(null, false);
        }

        modal.classList.add('active');
    }

    function fillForm(backend, isEdit) {
        document.getElementById('formBackendId').value = backend?.id || '';
        document.getElementById('formBackendId').disabled = isEdit;
        document.getElementById('formBackendName').value = backend?.name || '';
        document.getElementById('formBackendHost').value = backend?.host || '';
        document.getElementById('formBackendOllamaPort').value = backend?.ollamaPort || 11434;
        document.getElementById('formBackendAgentPort').value = backend?.agentPort || 18032;
        document.getElementById('formBackendWeight').value = backend?.weight || 1;
        document.getElementById('formBackendMaxConcurrent').value = backend?.maxConcurrentRequests || backend?.maxConcurrentReqs || 10;
        document.getElementById('formBackendMaxModels').value = backend?.maxModels || backend?.runtimeMaxModels || 0;
        document.getElementById('formBackendLabels').value = (backend?.labels || []).join(',');

        // Синхронизируем селектор типа бэкенда с BackendTypeFilter
        var formBackendType = document.getElementById('formBackendType');
        if (formBackendType) {
            var currentType = 'ollama';
            if (window.BackendTypeFilter) {
                currentType = BackendTypeFilter.getCurrentType();
            }
            if (backend && backend.type) {
                currentType = backend.type === 'llama_cpp' ? 'llama_cpp' : 'ollama';
            }
            formBackendType.value = currentType;
            // Применяем фильтрацию полей
            if (window.BackendTypeFilter) {
                BackendTypeFilter.toggleBackendFormFields(currentType);
            }
        }

        // GPU Mode — из capacity.mode или platformMode
        var gpuMode = backend?.gpuMode || backend?.platformMode || 'auto';
        if (backend?.ollama?.backendCapacity?.mode) {
            gpuMode = backend.ollama.backendCapacity.mode;
        }
        var modeEl = document.getElementById('formBackendGpuMode');
        if (modeEl) {
            modeEl.value = gpuMode;
        }
    }

    function closeModal() {
        document.getElementById('backendModal').classList.remove('active');
    }

    async function saveBackend() {
        const id = document.getElementById('formBackendId').value.trim();
        const name = document.getElementById('formBackendName').value.trim();
        const host = document.getElementById('formBackendHost').value.trim();
        const ollamaPort = parseInt(document.getElementById('formBackendOllamaPort').value) || 11434;
        const agentPort = parseInt(document.getElementById('formBackendAgentPort').value) || 18032;
        const weight = parseFloat(document.getElementById('formBackendWeight').value) || 1;
        const maxConcurrent = parseInt(document.getElementById('formBackendMaxConcurrent').value) || 10;
        const maxModels = parseInt(document.getElementById('formBackendMaxModels').value) || 0;
        const gpuMode = (document.getElementById('formBackendGpuMode') && document.getElementById('formBackendGpuMode').value) || 'auto';
        const labels = document.getElementById('formBackendLabels').value.split(',').map(l => l.trim()).filter(Boolean);

        if (!id || !host) {
            showToast(window.I18N ? I18N.t('common.error') : 'ID and host are required', 'error');
            return;
        }

        var backendType = (document.getElementById('formBackendType') && document.getElementById('formBackendType').value) || 'ollama';

        // cppWorker port:
        //   1. Берём значение из формы, если оно заполнено и > 0
        //   2. Иначе пробуем auto-detect (probe 18092/18091/18093/18090 на /info)
        //   3. Fallback: 18092 (актуальный default для современных llama.cpp-инсталляций).
        // Старое значение 18090 было НЕВЕРНЫМ — это agentPort, а не cppworker port.
        var cppWorkerPortInput = parseInt((document.getElementById('formBackendCppWorkerPort') && document.getElementById('formBackendCppWorkerPort').value)) || 0;
        var cppGrpcPort = parseInt((document.getElementById('formBackendCppGrpcPort') && document.getElementById('formBackendCppGrpcPort').value)) || 19000;

        var payload = { id, name: name || id, host, ollamaPort, agentPort, weight, maxConcurrentRequests: maxConcurrent, maxModels, gpuMode, labels, backendType: backendType };

        // Для llama.cpp-бэкенда гарантируем корректный cppWorkerPort.
        // Если поле пустое — пробуем auto-detect на лету (synchronously-parallel HTTP probes).
        if (backendType === 'llama_cpp') {
            payload.cppWorkerPort = cppWorkerPortInput > 0 ? cppWorkerPortInput : 18092;
            payload.grpcPort = cppGrpcPort;

            // Асинхронно проверяем реальный доступный порт и обновляем бэкенд,
            // если поле было пустым и auto-detect нашёл что-то отличное от 18092.
            if (cppWorkerPortInput <= 0) {
                autoDetectCppWorkerPort(host).then(function (detected) {
                    if (detected && detected !== payload.cppWorkerPort) {
                        payload.cppWorkerPort = detected;
                        // Обновляем бэкенд (PATCH) — чтобы в config зафиксировался правильный порт
                        Api.updateBackend(id, { cppWorkerPort: detected }).then(function () {
                            if (window.console && console.log) {
                                console.log('[app.js] auto-detected cppworker port:', detected, 'for backend', id);
                            }
                        }).catch(function (err) {
                            if (window.console && console.warn) {
                                console.warn('[app.js] failed to update cppworker port:', err);
                            }
                        });
                    }
                }).catch(function () { /* ignore */ });
            }
        }
        const isEdit = document.getElementById('formBackendId').disabled;

        try {
            if (isEdit) {
                await Api.updateBackend(id, payload);
                // Дополнительно обновляем runtime-лимиты через PUT /api/v1/backends/{id}/limits
                try {
                    await Api.updateBackendLimitsFull(id, maxConcurrent, maxModels);
                } catch (limitsErr) {
                    // Не фатально — лимиты может установить позже через агента
                    addLog(window.I18N ? I18N.t('app.backend_limits_warn', { id: id }) : 'Warning: runtime limits for ' + id + ' not updated', 'warn');
                }
                showToast(window.I18N ? I18N.t('app.backend_saved') : 'Backend updated', 'success');
                addLog(window.I18N ? I18N.t('app.backend_updated', { id: id }) : 'Backend ' + id + ' updated', 'info');
            } else {
                await Api.createBackend(payload);
                showToast(window.I18N ? I18N.t('app.backend_created') : 'Backend added', 'success');
                addLog(window.I18N ? I18N.t('app.backend_created') : 'Backend ' + id + ' added', 'info');
            }
            closeModal();
            refreshCurrentPage();
        } catch (e) {
            showToast((window.I18N ? I18N.t('common.error') : 'Ошибка') + ': ' + (e.message || e), 'error');
        }
    }

    async function deleteBackend() {
        const id = document.getElementById('formBackendId').value;
        if (!confirm(window.I18N ? I18N.t('backends.confirm_delete', { name: id }) : `Delete backend ${id}?`)) return;

        try {
            await Api.deleteBackend(id);
            closeModal();
            showToast(window.I18N ? I18N.t('app.backend_deleted') : 'Backend deleted', 'success');
            addLog(window.I18N ? I18N.t('app.backend_deleted', { id: id }) : `Backend ${id} deleted`, 'info');
            refreshCurrentPage();
        } catch (e) {
            showToast((window.I18N ? I18N.t('common.error') : 'Ошибка') + ': ' + (e.message || e), 'error');
        }
    }

    function editBackend(id) {
        openBackendModal(id);
    }

    function confirmDeleteBackend(id) {
        openBackendModal(id);
    }

    // ---- Filtering ----

    function filterBackends(query) {
        const rows = document.querySelectorAll('#backendsTableBody tr');
        const lower = query.toLowerCase();
        rows.forEach(row => {
            row.style.display = row.textContent.toLowerCase().includes(lower) ? '' : 'none';
        });
    }

    function filterSessions(query) {
        const rows = document.querySelectorAll('#sessionsTableBody tr');
        const lower = query.toLowerCase();
        rows.forEach(row => {
            row.style.display = row.textContent.toLowerCase().includes(lower) ? '' : 'none';
        });
    }

    function filterModels(query) {
        const cards = document.querySelectorAll('#modelsGrid .model-card');
        const lower = query.toLowerCase();
        cards.forEach(card => {
            card.style.display = card.textContent.toLowerCase().includes(lower) ? '' : 'none';
        });
    }

    // ---- UI Utilities ----

    /**
     * Обновление маркера типа движка инференса в сайдбаре.
     * Принимает clusterState (или любой объект с полями backendEngine, backendTypeCounts, operatingMode).
     * Отображает: 🦙 Ollama API, 🦒 llama.cpp, или 🔌 Автоопределение...
     */
    function updateBackendEngineBadge(state) {
        var badge = document.getElementById('backendEngineBadge');
        var label = document.getElementById('backendEngineLabel');
        if (!badge || !label) return;

        // Приоритет: выбор пользователя (localStorage через BackendTypeFilter) > cluster state
        var userType = window.BackendTypeFilter ? BackendTypeFilter.getCurrentType() : null;
        var effectiveType = (state && state.effectiveBackendType) || '';
        var engine = (state && state.backendEngine) || '';
        var counts = (state && state.backendTypeCounts) || {};

        // Remove old colour classes
        badge.classList.remove('engine-ollama', 'engine-llama_cpp', 'engine-auto');

        var icon = '🔌';
        var text = (window.I18N ? I18N.t('dashboard.engine_auto') : 'Автоопределение...');
        var cssClass = 'engine-auto';
        var title = (window.I18N ? I18N.t('dashboard.engine_hint_auto') : 'Тип движка: автоопределение');

        // Приоритет 1: выбор пользователя (localStorage)
        if (userType === 'llama_cpp') {
            icon = '🦒';
            text = 'llama.cpp';
            cssClass = 'engine-llama_cpp';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_llama_cpp') : 'Движок: llama.cpp');
        } else if (userType === 'ollama') {
            icon = '🦙';
            text = 'Ollama API';
            cssClass = 'engine-ollama';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_ollama') : 'Движок: Ollama API');
        } else if (effectiveType === 'llama_cpp') {
            icon = '🦒';
            text = 'llama.cpp';
            cssClass = 'engine-llama_cpp';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_llama_cpp') : 'Движок: llama.cpp');
        } else if (effectiveType === 'ollama') {
            icon = '🦙';
            text = 'Ollama API';
            cssClass = 'engine-ollama';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_ollama') : 'Движок: Ollama API');
        } else if (engine === 'llama_cpp') {
            icon = '🦒';
            text = 'llama.cpp';
            cssClass = 'engine-llama_cpp';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_llama_cpp') : 'Движок: llama.cpp');
        } else if (engine === 'ollama_api') {
            icon = '🦙';
            text = 'Ollama API';
            cssClass = 'engine-ollama';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_ollama') : 'Движок: Ollama API');
        }

        // Append per-type counts to title if available
        if (counts && Object.keys(counts).length > 0) {
            var parts = [];
            if (counts.ollama) parts.push('🦙 Ollama: ' + counts.ollama);
            if (counts.llama_cpp) parts.push('🦒 llama.cpp: ' + counts.llama_cpp);
            if (parts.length > 0) {
                title += ' | ' + parts.join(', ');
            }
        }

        var iconEl = badge.querySelector('.engine-icon');
        if (iconEl) iconEl.textContent = icon;
        label.textContent = text;
        badge.classList.add(cssClass);
        badge.title = title;
    }

    function updateConnectionStatus(connected) {
        const status = document.getElementById('connectionStatus');
        if (!status) return;
        const dot = status.querySelector('.status-dot');
        const text = status.querySelector('.status-text');
        if (!dot || !text) return;

        if (connected) {
            dot.classList.remove('disconnected');
            dot.classList.add('connected');
            text.textContent = window.I18N ? I18N.t('common.connected') : 'Connected';
        } else {
            dot.classList.remove('connected');
            dot.classList.add('disconnected');
            text.textContent = window.I18N ? I18N.t('common.disconnected') : 'Disconnected';
        }
    }

    function showToast(message, type = 'info') {
        const container = document.getElementById('toastContainer');
        if (!container) return;
        const toast = document.createElement('div');
        toast.className = `toast ${type}`;
        toast.innerHTML = `<span>${Utils.escapeHtml(message)}</span>`;
        container.appendChild(toast);

        setTimeout(() => {
            toast.style.opacity = '0';
            toast.style.transform = 'translateX(100%)';
            setTimeout(() => toast.remove(), 300);
        }, 4000);
    }

    function addLog(message, level = 'info') {
        var locale = (window.I18N && I18N.getLang() === 'ru') ? 'ru' : 'en';
        const entry = {
            time: new Date().toLocaleTimeString(locale),
            level: level.toUpperCase(),
            message
        };
        data.logs.unshift(entry);
        if (data.logs.length > 500) data.logs = data.logs.slice(0, 500);
        if (currentPage === 'logs') renderLogs(data.logs);
    }

    // ---- Export ----

    function exportLogs() {
        const content = data.logs.map(l => `[${l.time}] ${l.level}: ${l.message}`).join('\n');
        Utils.downloadFile(content, 'ollamalegion-logs.txt', 'text/plain');
    }

    function exportBackends() {
        const content = JSON.stringify(data.backends, null, 2);
        Utils.downloadFile(content, 'ollamalegion-backends.json', 'application/json');
    }

    // ---- Proxy Logs ----

    function addProxyLog(entry) {
        if (!entry) return;
        var locale = (window.I18N && I18N.getLang() === 'ru') ? 'ru' : 'en';
        if (entry.timestamp) {
            entry._time = new Date(entry.timestamp).toLocaleTimeString(locale);
        } else {
            entry._time = new Date().toLocaleTimeString(locale);
        }
        data.proxyLogs.unshift(entry);
        if (data.proxyLogs.length > 500) data.proxyLogs = data.proxyLogs.slice(0, 500);
        if (currentPage === 'logs') {
            var proxyTab = document.getElementById('logsTabProxy');
            if (proxyTab && proxyTab.classList.contains('active')) {
                renderProxyLogs(data.proxyLogs);
            }
        }
    }

    // ---- Agents ----

    async function fetchAgents() {
        try {
            data.agents = await Api.agentsStats();
            if (currentPage === 'agents') renderAgentsPage(data.agents);
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_agents') : 'Error loading agents');
        }
    }

    async function showAgentDetails(agentId) {
        var card = document.getElementById('agentDetailsCard');
        var content = document.getElementById('agentDetailsContent');
        if (!card || !content) return;
        content.innerHTML = '<div class="loading">' + (window.I18N ? I18N.t('common.loading') : 'Loading...') + '</div>';
        card.style.display = 'block';
        try {
            var info = await Api.agentInfo(agentId);
            renderAgentDetails(info);
        } catch (e) {
            content.innerHTML = '<div class="error">' + (window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (e.message || e) + '</div>';
        }
    }

    function restartAgent(backendId) {
        if (!confirm((window.I18N ? I18N.t('agents.confirm_restart') : 'Restart agent on backend') + ' ' + backendId + '?')) return;
        showToast((window.I18N ? I18N.t('agents.restarting') : 'Restarting agent...'), 'info');
        Api.restartAgent(backendId).then(function(result) {
            showToast((window.I18N ? I18N.t('agents.restarted') : 'Agent restarted'), 'success');
            addLog('Agent restarted on ' + backendId, 'info');
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('agents.restart_error') : 'Restart error') + ': ' + (err.message || err), 'error');
        });
    }

    function viewAgentLogs(backendId) {
        showToast((window.I18N ? I18N.t('agents.loading_logs') : 'Loading logs...'), 'info');
        Api.agentLogs(backendId, 200).then(function(data) {
            var logs = data.logs || data.entries || [];
            var content = logs.length ? logs.map(function(l) {
                var time = l.time || l.timestamp || '';
                var level = l.level || 'INFO';
                var msg = l.message || l.msg || '';
                var levelClass = level.toLowerCase();
                return '<div class="log-entry ' + levelClass + '"><span class="log-time">' + Utils.escapeHtml(time) + '</span> <span class="log-level log-level-' + levelClass + '">' + Utils.escapeHtml(level) + '</span> ' + Utils.escapeHtml(msg) + '</div>';
            }).join('') : '<div style="text-align:center;color:var(--text-muted);padding:20px;">' + (window.I18N ? I18N.t('agents.no_logs') : 'No logs available') + '</div>';

            var modalHtml = '<div id="agentLogsModal" class="modal active" style="z-index:2000;"><div class="modal-content" style="max-width:800px;">' +
                '<div class="modal-header"><h3>' + (window.I18N ? I18N.t('agents.logs_title') : 'Agent Logs') + ' — ' + Utils.escapeHtml(backendId) + '</h3><button class="modal-close" onclick="document.getElementById(\'agentLogsModal\').remove()">×</button></div>' +
                '<div class="modal-body"><div class="agent-logs-content">' + content + '</div></div>' +
                '<div class="modal-footer"><button class="btn btn-secondary" onclick="document.getElementById(\'agentLogsModal\').remove()">' + (window.I18N ? I18N.t('common.close') : 'Закрыть') + '</button></div>' +
                '</div></div>';

            var existing = document.getElementById('agentLogsModal');
            if (existing) existing.remove();
            document.body.insertAdjacentHTML('beforeend', modalHtml);
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('agents.logs_error') : 'Failed to load logs') + ': ' + (err.message || err), 'error');
        });
    }

    function fetchProxyLogs() {
        Api.proxyLogs(100).then(function(res) {
            data.proxyLogs = res.entries || [];
            if (currentPage === 'logs') {
                renderLogs(data.logs);
                var proxyTab = document.getElementById('logsTabProxy');
                if (proxyTab && proxyTab.classList.contains('active')) {
                    renderProxyLogs(data.proxyLogs);
                }
            }
        }).catch(function(err) {
            console.error('Failed to fetch proxy logs:', err);
        });
    }

    function loadBackendLimits() {
        var tbody = document.getElementById('backendLimitsBody');
        if (!tbody) return;
        var backends = data.backends || [];
        if (backends.length === 0) {
            tbody.innerHTML = '<tr><td colspan="6" class="loading-cell">' + (window.I18N ? I18N.t('common.loading') : 'Loading...') + '</td></tr>';
            return;
        }
        tbody.innerHTML = backends.map(function(b) {
            var maxConcurrent = b.maxConcurrentRequests || b.activeRequests || 10;
            var maxModels = b.maxModels || 0;
            var statusClass = b.status === 'healthy' ? 'badge-success' : (b.status === 'warning' ? 'badge-warning' : 'badge-error');
            var statusText = b.status || (window.I18N ? I18N.t('common.unknown') : 'Unknown');
            return '<tr>' +
                '<td><strong>' + Utils.escapeHtml(b.id || '-') + '</strong></td>' +
                '<td>' + Utils.escapeHtml(b.host || '-') + '</td>' +
                '<td>' + (b.agentPort || '-') + '</td>' +
                '<td><input type="number" class="form-control backend-limit-input" data-backend-id="' + Utils.escapeHtml(b.id) + '" data-limit-type="maxConcurrent" value="' + maxConcurrent + '" min="1" style="width:100px;"></td>' +
                '<td><input type="number" class="form-control backend-limit-input" data-backend-id="' + Utils.escapeHtml(b.id) + '" data-limit-type="maxModels" value="' + maxModels + '" min="0" style="width:100px;"></td>' +
                '<td><span class="badge ' + statusClass + '">' + statusText + '</span></td>' +
            '</tr>';
        }).join('');
    }

    function saveBackendLimits() {
        var inputs = document.querySelectorAll('.backend-limit-input');
        var limits = {};
        inputs.forEach(function(input) {
            var backendId = input.dataset.backendId;
            var limitType = input.dataset.limitType;
            if (!limits[backendId]) limits[backendId] = {};
            limits[backendId][limitType] = parseInt(input.value) || 0;
        });
        var promises = Object.keys(limits).map(function(backendId) {
            var l = limits[backendId];
            return Api.updateBackendLimitsFull(backendId, l.maxConcurrent || 10, l.maxModels || 0);
        });
        Promise.all(promises).then(function() {
            showToast(window.I18N ? I18N.t('settings.saved') : 'Backend limits saved', 'success');
            fetchClusterState();
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (err.message || err), 'error');
        });
    }

    function setupLogsTabNavigation() {
        document.querySelectorAll('.logs-tab').forEach(function(tab) {
            tab.addEventListener('click', function() {
                document.querySelectorAll('.logs-tab').forEach(function(t) { t.classList.remove('active'); });
                document.querySelectorAll('.logs-panel').forEach(function(p) { p.classList.remove('active'); });
                this.classList.add('active');
                var tabName = this.dataset.logsTab;
                var panelName = 'logsPanel' + tabName.charAt(0).toUpperCase() + tabName.slice(1);
                var panel = document.getElementById(panelName);
                if (panel) panel.classList.add('active');
                if (tabName === 'proxy') {
                    renderProxyLogs(data.proxyLogs);
                }
            });
        });
    }

    // ---- Model Management ----

    function openModelManageModal(backendId) {
        const backend = data.backends.find(b => b.id === backendId);
        if (!backend) {
            showToast('Backend not found', 'error');
            return;
        }

        const modal = document.getElementById('modelManageModal');
        if (!modal) return;

        // Store backendId in dataset for pull button
        modal.dataset.backendId = backendId;

        // Set title
        const title = modal.querySelector('.modal-header h3');
        if (title) title.textContent = (window.I18N ? I18N.t('models.manage_title') : 'Model Management') + ' — ' + backendId;

        // B-08: Скрываем Pull Model секцию для llama_cpp бэкендов
        var bt = backend.backend_type || backend.BackendType || 'ollama';
        var pullSection = modal.querySelector('.model-manage-section');
        if (pullSection) {
            pullSection.style.display = (bt === 'llama_cpp') ? 'none' : '';
        }

        // Show operations list with loading
        const modelsBody = document.getElementById('modelManageModelsBody');
        if (modelsBody) {
            modelsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' + (window.I18N ? I18N.t('common.loading') : 'Loading...') + '</td></tr>';
        }

        // Show active operations
        refreshModelOpsStatus();

        modal.classList.add('active');

        // Load models from backend
        loadBackendModels(backendId);
    }

    function loadBackendModels(backendId) {
        const modelsBody = document.getElementById('modelManageModelsBody');
        if (!modelsBody) return;

        Api.backendModels(backendId).then(function(data) {
            const models = data.models || [];
            if (models.length === 0) {
                modelsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' + (window.I18N ? I18N.t('models.no_models_backend') : 'No models') + '</td></tr>';
                return;
            }
            modelsBody.innerHTML = models.map(function(m) {
                const sizeStr = m.size ? (m.size / 1024 / 1024 / 1024).toFixed(2) + ' GB' : '-';
                const modified = m.modifiedAt ? new Date(m.modifiedAt).toLocaleString(Utils._locale()) : '-';
                const loadedBadge = m.loaded
                    ? '<span class="badge badge-success">' + (window.I18N ? I18N.t('models.loaded_status') : 'Loaded') + '</span>'
                    : '<span class="badge badge-secondary">' + (window.I18N ? I18N.t('models.unloaded_status') : 'Not loaded') + '</span>';
                return '<tr>' +
                    '<td><strong>' + Utils.escapeHtml(m.name) + '</strong></td>' +
                    '<td>' + (m.digest ? m.digest.substring(0, 16) + '...' : '-') + '</td>' +
                    '<td>' + sizeStr + '</td>' +
                    '<td>' + modified + '</td>' +
                    '<td>' + loadedBadge + '</td>' +
                    '<td>' +
                        '<button class="action-btn edit" onclick="ui.executeModelOperation(\'' + Utils.escapeHtml(backendId) + '\', \'load\', \'' + Utils.escapeHtml(m.name) + '\')" title="' + (window.I18N ? I18N.t('models.load_hint') : 'Load') + '">' + (window.I18N ? I18N.t('models.load') : 'Load') + '</button>' +
                        '<button class="action-btn delete" onclick="ui.executeModelOperation(\'' + Utils.escapeHtml(backendId) + '\', \'unload\', \'' + Utils.escapeHtml(m.name) + '\')" title="' + (window.I18N ? I18N.t('models.unload_hint') : 'Unload') + '">' + (window.I18N ? I18N.t('models.unload') : 'Unload') + '</button>' +
                    '</td>' +
                '</tr>';
            }).join('');
        }).catch(function(err) {
            modelsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' + (window.I18N ? I18N.t('models.operation_error') : 'Error') + ': ' + (err.message || err) + '</td></tr>';
        });
    }

    function executeModelOperation(backendId, operation, modelName, options = {}) {
        showToast((window.I18N ? I18N.t('models.operation_running') : 'Operation in progress...') + ' ' + operation + ' ' + modelName, 'info');

        Api.backendModelOperation(backendId, operation, modelName, options).then(function(result) {
            if (result && result.success) {
                showToast((window.I18N ? I18N.t('models.operation_success') : 'Success') + ': ' + operation + ' ' + modelName, 'success');
                addLog('Model op success: ' + operation + ' ' + modelName + ' on ' + backendId, 'info');
                // Reload models after a short delay
                setTimeout(function() {
                    loadBackendModels(backendId);
                    refreshModelOpsStatus();
                }, 1500);
            } else {
                var errMsg = (result && result.error) || (window.I18N ? I18N.t('models.operation_error') : 'Operation failed');
                showToast(errMsg, 'error');
                addLog('Model op error: ' + operation + ' ' + modelName + ' on ' + backendId + ': ' + errMsg, 'error');
            }
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('models.operation_error') : 'Error') + ': ' + (err.message || err), 'error');
            addLog('Model op error: ' + operation + ' ' + modelName + ' on ' + backendId + ': ' + (err.message || err), 'error');
        });
    }

    function refreshModelOpsStatus() {
        Api.modelOperationsStatus().then(function(data) {
            const ops = data.operations || [];
            const opsBody = document.getElementById('modelOpsBody');
            if (!opsBody) return;

            if (ops.length === 0) {
                opsBody.innerHTML = '<tr><td colspan="5" class="loading-cell">' + (window.I18N ? I18N.t('models.no_active_ops') : 'No active operations') + '</td></tr>';
                return;
            }

            opsBody.innerHTML = ops.map(function(op) {
                return '<tr>' +
                    '<td>' + Utils.escapeHtml(op.operation || '-') + '</td>' +
                    '<td>' + Utils.escapeHtml(op.modelName || '-') + '</td>' +
                    '<td>' + Utils.escapeHtml(op.backendId || '-') + '</td>' +
                    '<td>' + (op.startedAt ? new Date(op.startedAt).toLocaleString(Utils._locale()) : '-') + '</td>' +
                    '<td><span class="badge badge-info">' + Utils.escapeHtml(op.status || 'running') + '</span></td>' +
                '</tr>';
            }).join('');
        }).catch(function(err) {
            // Silently ignore — this is a background refresh
            console.error('Failed to refresh model ops status:', err);
        });
    }

    function closeModelManageModal() {
        const modal = document.getElementById('modelManageModal');
        if (modal) modal.classList.remove('active');
    }

    // ---- CppWorker port auto-detection ----
    // Probes common cppworker ports on the given host and returns the first
    // port that responds OK to /info (or /health). 18092 first (актуальный
    // default для современных llama.cpp-инсталляций), затем 18091 (stub),
    // 18093 (доп. профиль) и 18090 (legacy/agent).
    async function autoDetectCppWorkerPort(host) {
        if (!host) return null;
        const candidates = [18092, 18091, 18093, 18090];
        // Параллельные probe-запросы (AbortController + 1.5s timeout на каждый)
        const probes = candidates.map(function (port) {
            return new Promise(function (resolve) {
                try {
                    const ctrl = new AbortController();
                    const timeoutId = setTimeout(function () { ctrl.abort(); }, 1500);
                    fetch('http://' + host + ':' + port + '/info', {
                        signal: ctrl.signal,
                        mode: 'cors',
                        cache: 'no-store',
                        headers: { 'Accept': 'application/json' }
                    }).then(function (resp) {
                        clearTimeout(timeoutId);
                        if (resp && resp.ok) resolve(port);
                        else resolve(null);
                    }).catch(function () {
                        clearTimeout(timeoutId);
                        resolve(null);
                    });
                } catch (e) {
                    resolve(null);
                }
            });
        });
        const results = await Promise.all(probes);
        // Возвращаем первый непустой результат
        for (var i = 0; i < results.length; i++) {
            if (results[i]) return results[i];
        }
        return null;
    }

    // ---- Public API ----
    return {
        init,
        editBackend,
        confirmDeleteBackend,
        openModelManageModal,
        executeModelOperation,
        loadBackendModels,
        refreshModelOpsStatus,
        closeModelManageModal,
        showAgentDetails,
        fetchAgents,
        restartAgent,
        viewAgentLogs
    };

})();


// Auto-init when DOM ready
document.addEventListener('DOMContentLoaded', () => ui.init());

// ---- Global helpers for model card actions (used by renderers.js modelsGrid) ----
window.modelCardAction = function(operation, backendId, modelName) {
    if (!backendId || !modelName) return;
    if (operation === 'delete') {
        if (!confirm((window.I18N ? I18N.t('models.confirm_delete') : 'Delete model') + ' ' + modelName + '?')) return;
    }
    ui.executeModelOperation(backendId, operation, modelName);
};
