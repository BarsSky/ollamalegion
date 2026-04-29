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

    // ---- Initialization ----

    function init() {
        initTheme();
        setupI18n();
        setupNavigation();
        setupEventListeners();
        setupRestartHandler();
        setupWebSocketEvents();
        setupApiEvents();

        // Initial data load
        fetchClusterState();
        fetchQueue();
        fetchQueueDetails();
        fetchQueueHistory();
        fetchSessions();

        // Periodic refresh
        startPeriodicRefresh();

        addLog('WebUI инициализирован', 'info');
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
                showToast(currentLang === 'ru' ? 'Язык изменён' : 'Language changed', 'success');
            });
        }
        window.addEventListener('i18n:changed', function () {
            updateUITranslations();
            updateThemeToggleIcon(document.documentElement.getAttribute('data-theme') || 'dark');
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
                Api.post('/restart').then(function () {
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
        const titles = {
            dashboard: 'Dashboard',
            monitor: 'Монитор кластера',
            backends: 'Управление бэкендами',
            models: 'Модели',
            sessions: 'Сессии',
            queue: 'Очередь',
            logs: 'Логи и события',
            settings: 'Настройки'
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
            refreshInterval: 2000
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

        document.getElementById('saveSettings').addEventListener('click', saveSettings);
        document.getElementById('resetSettings').addEventListener('click', loadSettings);

        document.getElementById('backendModal').addEventListener('click', (e) => {
            if (e.target.id === 'backendModal') closeModal();
        });
    }

    // ---- WebSocket Events ----

    function setupWebSocketEvents() {
        window.addEventListener('ws-open', () => {
            updateConnectionStatus(true);
            addLog('WebSocket подключен', 'info');
        });

        window.addEventListener('ws-status', (e) => {
            updateConnectionStatus(e.detail.connected);
        });

        window.addEventListener('ws-error', () => {
            updateConnectionStatus(false);
            addLog('Ошибка WebSocket', 'error');
        });

        window.addEventListener('ws-reconnecting', (e) => {
            const { attempt, max, delay } = e.detail;
            addLog(`Переподключение... (${attempt}/${max}) через ${Math.round(delay / 1000)}с`, 'warn');
        });

        window.addEventListener('ws-max-reconnect', () => {
            addLog('Максимальное количество попыток переподключения', 'error');
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
                addLog(`Бэкенд добавлен: ${payload.data?.name || payload.backendId}`, 'info');
                fetchClusterState();
                break;
            case 'backendRemove':
                addLog(`Бэкенд удалён: ${payload.backendId}`, 'info');
                fetchClusterState();
                break;
            case 'statusChange':
                addLog(`Статус ${payload.backendId}: ${payload.data?.oldStatus} → ${payload.data?.newStatus}`, 'warning');
                applyStatusChange(payload.backendId, payload.data?.newStatus);
                break;
            case 'limitsChange':
                addLog(`Лимиты ${payload.backendId} обновлены`, 'info');
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
            const { message } = e.detail;
            showToast(message, 'error');
            addLog(message, 'error');
        });
    }

    // ---- Data Fetching ----

    async function fetchClusterState() {
        try {
            const state = await Api.cluster();
            updateBackends(state.backends || []);
        } catch (e) {
            Api.handleError(e, 'Ошибка загрузки состояния кластера');
        }
    }

    async function fetchQueue() {
        try {
            data.queue = await Api.queueStats();
            Utils.setText('queueSize', data.queue.current_size || 0);
            Utils.setText('queueProcessed', (data.queue.processed_total || 0) + ' обработано');
            if (currentPage === 'queue') refreshPage('queue');
        } catch (e) {
            Api.handleError(e, 'Ошибка загрузки статистики очереди');
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
            Api.handleError(e, 'Ошибка загрузки деталей очереди');
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
            Api.handleError(e, 'Ошибка загрузки истории очереди');
            data.queueHistory = [];
            if (currentPage === 'queue') refreshPage('queue');
        }
    }

    async function fetchSessions() {
        try {
            const res = await Api.sessions();
            data.sessions = res.sessions || [];
            const activeCount = data.sessions.filter(s => s.active).length;
            const totalRequests = data.sessions.reduce((sum, s) => sum + (s.requestCount || 0), 0);
            Utils.setText('totalSessions', activeCount);
            Utils.setText('sessionRate', totalRequests + ' запросов');
            if (currentPage === 'sessions') refreshPage('sessions');
        } catch (e) {
            Api.handleError(e, 'Ошибка загрузки сессий');
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

        // Also fetch from server if available
        Api.config().then(function (serverConfig) {
            var algEl = document.getElementById('balancingAlgorithm');
            if (algEl && serverConfig.algorithm) algEl.value = serverConfig.algorithm;
            var maEl = document.getElementById('modelAffinity');
            if (maEl) maEl.checked = serverConfig.modelAffinity !== false;
            var ssEl = document.getElementById('sessionStickiness');
            if (ssEl) ssEl.checked = serverConfig.sessionStickiness !== false;
        }).catch(function () {});
    }

    function saveSettings() {
        var algorithm = (document.getElementById('balancingAlgorithm') && document.getElementById('balancingAlgorithm').value) || 'resource-aware';
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

        Api.post('/config', config).then(function () {
            showToast(window.I18N ? I18N.t('settings.saved') : 'Настройки сохранены', 'success');
        }).catch(function (err) {
            showToast((window.I18N ? I18N.t('common.error') : 'Ошибка') + ': ' + (err.message || err), 'error');
        });
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
            const backend = data.backends.find(b => b.id === backendId);
            if (!backend) return;
            title.textContent = window.I18N ? I18N.t('backends.edit') : 'Редактировать бэкенд';
            deleteBtn.style.display = 'inline-block';
            fillForm(backend, true);
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
        document.getElementById('formBackendMaxConcurrent').value = backend?.maxConcurrentRequests || 10;
        document.getElementById('formBackendLabels').value = (backend?.labels || []).join(',');
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
        const labels = document.getElementById('formBackendLabels').value.split(',').map(l => l.trim()).filter(Boolean);

        if (!id || !host) {
            showToast(window.I18N ? I18N.t('common.error') : 'ID и хост обязательны', 'error');
            return;
        }

        const payload = { id, name: name || id, host, ollamaPort, agentPort, weight, maxConcurrentRequests: maxConcurrent, labels };
        const isEdit = document.getElementById('formBackendId').disabled;

        try {
            if (isEdit) {
                await Api.updateBackend(id, payload);
                showToast('Бэкенд обновлен', 'success');
                addLog(`Бэкенд ${id} обновлен`, 'info');
            } else {
                await Api.createBackend(payload);
                showToast('Бэкенд добавлен', 'success');
                addLog(`Бэкенд ${id} добавлен`, 'info');
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
            showToast('Бэкенд удален', 'success');
            addLog(`Бэкенд ${id} удален`, 'info');
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
        const entry = {
            time: new Date().toLocaleTimeString('ru'),
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