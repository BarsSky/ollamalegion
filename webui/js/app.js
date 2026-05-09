/**
 * OllamaLegion WebUI — Orchestrator
 * Imports: Utils, Api, WebSocketManager, Renderers (loaded before this file)
 */
const ui = (function () {
    const { dashboard, backendsPage, modelsPage, sessionsPage, queuePage, logs: renderLogs, predictionAlerts } = Renderers;

    // State
    const data = {
        backends: [],
        sessions: [],
        queue: {},
        queueTasks: [],
        queueHistory: [],
        logs: [],
        models: []
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

        // Periodic refresh
        startPeriodicRefresh();

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
                ? (window.I18N ? I18N.t('settings.theme_light') : 'Светлая тема')
                : (window.I18N ? I18N.t('settings.theme_dark') : 'Темная тема');
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
                    indicator.innerHTML = '<div class="restart-indicator"><div class="restart-spinner"></div><span>' + (window.I18N ? I18N.t('settings.restarting') : 'Перезапуск балансера...') + '</span></div>';
                }
                Api.post('/api/v1/admin/restart').then(function () {
                    if (indicator) indicator.innerHTML = '<div style="color: var(--success); padding: 8px;">' + (window.I18N ? I18N.t('settings.restarted') : 'Балансер успешно перезапущен') + '</div>';
                    showToast(window.I18N ? I18N.t('settings.restarted') : 'Балансер успешно перезапущен', 'success');
                }).catch(function (err) {
                    if (indicator) indicator.innerHTML = '<div style="color: var(--danger); padding: 8px;">' + (window.I18N ? I18N.t('settings.restart_error') : 'Ошибка перезапуска') + '</div>';
                    showToast((window.I18N ? I18N.t('settings.restart_error') : 'Ошибка') + ': ' + (err.message || err), 'error');
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
            logs: 'System Logs',
            settings: 'Settings'
        };
        return titles[page] || 'Dashboard';
    }

    function refreshPage(page) {
        switch (page) {
            case 'dashboard':
                dashboard(data.backends, data.sessions, data.queue);
                predictionAlerts(data.backends);
                break;
            case 'monitor':
                sendMonitorConfig();
                break;
            case 'backends':
                backendsPage([...data.backends].sort((a, b) => (a.id || '').localeCompare(b.id || '')));
                break;
            case 'models':
                modelsPage(data.backends);
                break;
            case 'sessions':
                sessionsPage(data.sessions);
                break;
            case 'queue':
                queuePage(data.queue, data.queueTasks, data.queueHistory);
                break;
            case 'logs':
                renderLogs(data.logs);
                break;
            case 'settings':
                loadSettings();
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
            showToast(window.I18N ? I18N.t('common.success') : 'Данные обновлены', 'success');
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
        document.getElementById('resetSettings').addEventListener('click', loadSettings);

        // Auto-save on settings form changes
        var settingsFields = ['balancingAlgorithm', 'useEnhancedScoring', 'modelAffinity', 'sessionStickiness', 'predictionFiltering', 'gpuMaxUsage', 'vramMaxUsage', 'cpuMaxUsage', 'ramMaxUsage', 'minFreeDisk'];
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
                addLog(window.I18N ? I18N.t('app.status_changed', { id: payload.backendId, old: payload.data?.oldStatus, new: payload.data?.newStatus }) : `Status ${payload.backendId}: ${payload.data?.oldStatus} → ${payload.data?.newStatus}`, 'warning');
                applyStatusChange(payload.backendId, payload.data?.newStatus);
                break;
            case 'limitsChange':
                addLog(window.I18N ? I18N.t('app.limits_changed', { id: payload.backendId }) : `Limits ${payload.backendId} updated`, 'info');
                fetchClusterState();
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

    function loadSettings() {
        var saved = localStorage.getItem('ollamalegion_config');
        var config = saved ? JSON.parse(saved) : null;

        if (config) {
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
        }

        // Также загружаем с сервера — серверные значения имеют приоритет
        Api.config().then(function (serverConfig) {
            var algEl = document.getElementById('balancingAlgorithm');
            if (algEl && serverConfig.algorithm) algEl.value = serverConfig.algorithm;
            var maEl = document.getElementById('modelAffinity');
            if (maEl) maEl.checked = serverConfig.modelAffinity !== false;
            var ssEl = document.getElementById('sessionStickiness');
            if (ssEl) ssEl.checked = serverConfig.sessionStickiness !== false;

            // Новые поля из расширенного конфига
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
        }).catch(function () {});
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

        var config = {
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
            apiToken: apiToken
        };

        localStorage.setItem('ollamalegion_config', JSON.stringify(config));

        Api.updateConfig(config).then(function () {
            if (!silent) showToast(window.I18N ? I18N.t('settings.saved') : 'Настройки сохранены', 'success');
        }).catch(function (err) {
            if (!silent) showToast((window.I18N ? I18N.t('common.error') : 'Ошибка') + ': ' + (err.message || err), 'error');
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
            title.textContent = window.I18N ? I18N.t('backends.edit') : 'Редактировать бэкенд';
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
            title.textContent = window.I18N ? I18N.t('backends.add') : 'Добавить бэкенд';
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
            showToast(window.I18N ? I18N.t('common.error') : 'ID и хост обязательны', 'error');
            return;
        }

        const payload = { id, name: name || id, host, ollamaPort, agentPort, weight, maxConcurrentRequests: maxConcurrent, maxModels, gpuMode, labels };
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
        if (!confirm(window.I18N ? I18N.t('backends.confirm_delete', { name: id }) : `Удалить бэкенд ${id}?`)) return;

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

    // ---- UI Utilities ----

    function updateConnectionStatus(connected) {
        const status = document.getElementById('connectionStatus');
        if (!status) return;
        const dot = status.querySelector('.status-dot');
        const text = status.querySelector('.status-text');
        if (!dot || !text) return;

        if (connected) {
            dot.classList.remove('disconnected');
            dot.classList.add('connected');
            text.textContent = window.I18N ? I18N.t('common.connected') : 'Подключено';
        } else {
            dot.classList.remove('connected');
            dot.classList.add('disconnected');
            text.textContent = window.I18N ? I18N.t('common.disconnected') : 'Отключено';
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

    // ---- Public API ----
    return {
        init,
        editBackend,
        confirmDeleteBackend
    };
})();

// Auto-init when DOM ready
document.addEventListener('DOMContentLoaded', () => ui.init());