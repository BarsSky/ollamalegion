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

    // ---- Initialization ----

    function init() {
        setupNavigation();
        setupEventListeners();
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
        Utils.setText('pageTitle', getPageTitle(page));
        refreshPage(page);

        if (page === 'backends' || page === 'models') {
            fetchClusterState();
        }
    }

    function getPageTitle(page) {
        const titles = {
            dashboard: 'Dashboard',
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

    function refreshCurrentPage() {
        refreshPage(currentPage);
    }

    // ---- Event Listeners ----

    function setupEventListeners() {
        document.getElementById('refreshBtn').addEventListener('click', () => {
            refreshCurrentPage();
            showToast('Данные обновлены', 'success');
        });

        document.getElementById('addBackendBtn').addEventListener('click', () => openBackendModal());
        document.getElementById('addBackendBtn2').addEventListener('click', () => openBackendModal());

        document.getElementById('modalClose').addEventListener('click', closeModal);
        document.getElementById('modalCancel').addEventListener('click', closeModal);
        document.getElementById('modalSave').addEventListener('click', saveBackend);
        document.getElementById('modalDelete').addEventListener('click', deleteBackend);

        document.getElementById('backendSearch').addEventListener('input', Utils.debounce((e) => filterBackends(e.target.value), 150));
        document.getElementById('sessionSearch').addEventListener('input', Utils.debounce((e) => filterSessions(e.target.value), 150));

        document.getElementById('clearLogs').addEventListener('click', () => {
            data.logs = [];
            renderLogs(data.logs);
        });
        document.getElementById('exportLogs').addEventListener('click', exportLogs);
        document.getElementById('exportBackends').addEventListener('click', exportBackends);

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
                data.backends = payload.data?.backends || [];
                if (currentPage === 'dashboard') {
                    refreshPage('dashboard');
                }
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
                fetchClusterState();
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
                    data.backends = payload.backends || [];
                    if (currentPage === 'dashboard') {
                        refreshPage('dashboard');
                    }
                }
                break;
        }
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
            data.backends = state.backends || [];
            if (currentPage === 'dashboard') {
                refreshPage('dashboard');
            }
        } catch (e) {
            Api.handleError(e, 'Ошибка загрузки состояния кластера');
        }
    }

    async function fetchQueue() {
        try {
            data.queue = await Api.queueStats();
            Utils.setText('queueSize', data.queue.current_size || 0);
            Utils.setText('queueProcessed', `${data.queue.processed_total || 0} обработано`);
            if (currentPage === 'queue') refreshPage('queue');
        } catch (e) {
            Api.handleError(e, 'Ошибка загрузки статистики очереди');
        }
    }

    async function fetchQueueDetails() {
        try {
            const res = await Api.queueDetails();
            const pending = res.pending || [];
            data.queueTasks = pending.map((item, idx) => ({
                id: idx + 1,
                model: item.model || '-',
                backend: item.target || 'Auto',
                status: item.target ? 'processing' : 'pending',
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
            // Update dashboard sessions widget
            const activeCount = data.sessions.filter(s => s.active).length;
            const totalRequests = data.sessions.reduce((sum, s) => sum + (s.requestCount || 0), 0);
            Utils.setText('totalSessions', activeCount);
            Utils.setText('sessionRate', `${totalRequests} запросов`);
            if (currentPage === 'sessions') refreshPage('sessions');
        } catch (e) {
            Api.handleError(e, 'Ошибка загрузки сессий');
        }
    }

    async function loadSettings() {
        try {
            const config = await Api.config();
            const el = document.getElementById('balancingAlgorithm');
            if (el) el.value = config.algorithm || 'resource-aware';
            document.getElementById('modelAffinity').checked = config.modelAffinity !== false;
            document.getElementById('sessionStickiness').checked = config.sessionStickiness !== false;
        } catch (e) {
            Api.handleError(e, 'Ошибка загрузки настроек');
        }
    }

    async function saveSettings() {
        const settings = {
            algorithm: document.getElementById('balancingAlgorithm').value,
            modelAffinity: document.getElementById('modelAffinity').checked,
            sessionStickiness: document.getElementById('sessionStickiness').checked,
            predictionFiltering: document.getElementById('predictionFiltering').checked,
            resources: {
                gpu: { maxUsagePercent: parseInt(document.getElementById('gpuMaxUsage').value) },
                vram: { maxUsagePercent: parseInt(document.getElementById('vramMaxUsage').value) },
                cpu: { maxUsagePercent: parseInt(document.getElementById('cpuMaxUsage').value) },
                memory: { maxUsagePercent: parseInt(document.getElementById('ramMaxUsage').value) },
                disk: { minFreeMB: parseInt(document.getElementById('minFreeDisk').value) }
            }
        };
        showToast('Настройки сохранены (требуется рестарт)', 'success');
        addLog('Настройки обновлены', 'info');
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
            title.textContent = 'Редактировать бэкенд';
            deleteBtn.style.display = 'inline-block';
            fillForm(backend, true);
        } else {
            title.textContent = 'Добавить бэкенд';
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
            showToast('ID и хост обязательны', 'error');
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
            showToast(`Ошибка: ${e.message}`, 'error');
        }
    }

    async function deleteBackend() {
        const id = document.getElementById('formBackendId').value;
        if (!confirm(`Удалить бэкенд ${id}?`)) return;

        try {
            await Api.deleteBackend(id);
            closeModal();
            showToast('Бэкенд удален', 'success');
            addLog(`Бэкенд ${id} удален`, 'info');
            refreshCurrentPage();
        } catch (e) {
            showToast(`Ошибка: ${e.message}`, 'error');
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
            text.textContent = 'Подключено';
        } else {
            dot.classList.remove('connected');
            dot.classList.add('disconnected');
            text.textContent = 'Отключено';
        }
    }

    function showToast(message, type = 'info') {
        const container = document.getElementById('toastContainer');
        const toast = document.createElement('div');
        toast.className = `toast ${type}`;
        const iconSvg = '<svg class=\"toast-icon-svg\" viewBox=\"0 0 24 24\" width=\"16\" height=\"16\"><circle cx=\"12\" cy=\"12\" r=\"10\" fill=\"currentColor\"/></svg>';
        toast.innerHTML = `<span>${iconSvg}</span><span>${Utils.escapeHtml(message)}</span>`;
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