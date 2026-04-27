/**
 * OllamaLegion WebUI
 * Real-time dashboard для мониторинга и управления Ollama кластером
 */

const API_BASE = window.location.hostname === 'localhost' ? 'http://localhost:18081' : '';
const WS_URL = API_BASE.replace('http', 'ws') + '/ws/metrics';

class OllamaLegionUI {
    constructor() {
        this.ws = null;
        this.reconnectInterval = 3000;
        this.reconnectAttempts = 0;
        this.maxReconnectAttempts = 10;
        this.data = {
            backends: [],
            sessions: [],
            queue: {},
            metrics: [],
            models: [],
            logs: []
        };
        this.currentPage = 'dashboard';
        this.eventSource = null;
        
        this.init();
    }

    init() {
        this.setupNavigation();
        this.fetchClusterState();     // начальная загрузка данных до WebSocket
        this.setupWebSocket();
        this.setupEventListeners();
        this.startPeriodicRefresh();  // запуск periodic fetch (sessions, queue)
        this.addLog('WebUI инициализирован', 'info');
    }

    // Navigation
    setupNavigation() {
        document.querySelectorAll('.nav-item').forEach(item => {
            item.addEventListener('click', (e) => {
                e.preventDefault();
                const page = item.dataset.page;
                this.switchPage(page);
            });
        });
    }

    switchPage(page) {
        document.querySelectorAll('.page').forEach(p => p.classList.remove('active'));
        document.querySelectorAll('.nav-item').forEach(n => n.classList.remove('active'));
        
        const targetPage = document.getElementById(page + '-page');
        const targetNav = document.querySelector(`[data-page="${page}"]`);
        
        if (targetPage) targetPage.classList.add('active');
        if (targetNav) targetNav.classList.add('active');
        
        this.currentPage = page;
        document.getElementById('pageTitle').textContent = this.getPageTitle(page);
        
        // Обновить данные страницы
        this.refreshPage(page);
        
        // Fallback: при переключении на backends/models/sessions подгрузить данные через REST
        if (page === 'backends' || page === 'models') {
            this.fetchClusterState();
        }
    }

    getPageTitle(page) {
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

    refreshPage(page) {
        switch(page) {
            case 'dashboard':
                this.renderDashboard();
                break;
            case 'backends':
                this.renderBackendsPage();
                break;
            case 'models':
                this.renderModelsPage();
                break;
            case 'sessions':
                this.renderSessionsPage();
                break;
            case 'queue':
                this.renderQueuePage();
                break;
            case 'logs':
                this.renderLogs();
                break;
            case 'settings':
                this.loadSettings();
                break;
        }
    }

    // WebSocket
    setupWebSocket() {
        this.connectWebSocket();
    }

    connectWebSocket() {
        try {
            this.ws = new WebSocket(WS_URL);
            
            this.ws.onopen = () => {
                this.reconnectAttempts = 0;
                this.updateConnectionStatus(true);
                this.addLog('WebSocket подключен', 'info');
            };
            
            this.ws.onmessage = (event) => {
                try {
                    const data = JSON.parse(event.data);
                    this.handleWebSocketData(data);
                } catch (e) {
                    console.error('WS parse error:', e);
                }
            };
            
            this.ws.onclose = () => {
                this.updateConnectionStatus(false);
                this.attemptReconnect();
            };
            
            this.ws.onerror = (error) => {
                this.updateConnectionStatus(false);
                this.addLog('Ошибка WebSocket', 'error');
                console.error('WebSocket error:', error);
            };
        } catch (e) {
            this.updateConnectionStatus(false);
            this.attemptReconnect();
        }
    }

    attemptReconnect() {
        if (this.reconnectAttempts >= this.maxReconnectAttempts) {
            this.addLog('Максимальное количество попыток переподключения', 'error');
            return;
        }
        
        this.reconnectAttempts++;
        this.addLog(`Переподключение... (${this.reconnectAttempts}/${this.maxReconnectAttempts})`, 'warn');
        
        setTimeout(() => {
            this.connectWebSocket();
        }, this.reconnectInterval);
    }

    handleWebSocketData(data) {
        // Новый event-driven формат WebSocket
        const eventType = data.eventType || 'legacy';

        switch (eventType) {
            case 'clusterState':
                // Periodic snapshot с полным состоянием кластера
                this.data.backends = (data.data && data.data.backends) || [];
                this.renderDashboard();
                this.renderPredictionAlerts();
                break;

            case 'backendAdd':
                this.addLog(`Бэкенд добавлен: ${data.data && data.data.name || data.backendId}`, 'info');
                // Запрашиваем полное состояние для обновления UI
                this.fetchClusterState();
                break;

            case 'backendRemove':
                this.addLog(`Бэкенд удалён: ${data.backendId}`, 'info');
                this.fetchClusterState();
                break;

            case 'statusChange':
                this.addLog(
                    `Статус ${data.backendId}: ${data.data && data.data.oldStatus} → ${data.data && data.data.newStatus}`,
                    'warning'
                );
                this.fetchClusterState();
                break;

            case 'limitsChange':
                this.addLog(`Лимиты ${data.backendId} обновлены`, 'info');
                this.fetchClusterState();
                break;

            case 'ping':
                // heartbeat ping — игнорируем
                break;

            case 'legacy':
            default:
                // Fallback для старых сообщений без eventType
                if (data.backends) {
                    this.data.backends = data.backends || [];
                    this.renderDashboard();
                    this.renderPredictionAlerts();
                }
                break;
        }
    }

    updateConnectionStatus(connected) {
        const status = document.getElementById('connectionStatus');
        const dot = status.querySelector('.status-dot');
        const text = status.querySelector('.status-text');
        
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

    // Event Listeners
    setupEventListeners() {
        // Refresh button
        document.getElementById('refreshBtn').addEventListener('click', () => {
            this.refreshCurrentPage();
            this.showToast('Данные обновлены', 'success');
        });

        // Add backend buttons
        document.getElementById('addBackendBtn').addEventListener('click', () => {
            this.openBackendModal();
        });
        document.getElementById('addBackendBtn2').addEventListener('click', () => {
            this.openBackendModal();
        });

        // Modal
        document.getElementById('modalClose').addEventListener('click', () => this.closeModal());
        document.getElementById('modalCancel').addEventListener('click', () => this.closeModal());
        document.getElementById('modalSave').addEventListener('click', () => this.saveBackend());
        document.getElementById('modalDelete').addEventListener('click', () => this.deleteBackend());

        // Backend search
        document.getElementById('backendSearch').addEventListener('input', (e) => {
            this.filterBackends(e.target.value);
        });

        // Session search
        document.getElementById('sessionSearch').addEventListener('input', (e) => {
            this.filterSessions(e.target.value);
        });

        // Logs
        document.getElementById('clearLogs').addEventListener('click', () => {
            this.data.logs = [];
            this.renderLogs();
        });
        document.getElementById('exportLogs').addEventListener('click', () => {
            this.exportLogs();
        });

        // Export backends
        document.getElementById('exportBackends').addEventListener('click', () => {
            this.exportBackends();
        });

        // Settings
        document.getElementById('saveSettings').addEventListener('click', () => {
            this.saveSettings();
        });
        document.getElementById('resetSettings').addEventListener('click', () => {
            this.loadSettings();
        });

        // Close modal on outside click
        document.getElementById('backendModal').addEventListener('click', (e) => {
            if (e.target.id === 'backendModal') this.closeModal();
        });
    }

    // Dashboard
    renderDashboard() {
        const backends = [...this.data.backends].sort((a, b) => (a.id || '').localeCompare(b.id || ''));
        const healthy = backends.filter(b => b.status === 'healthy');
        
        // Summary metrics
        document.getElementById('totalBackends').textContent = backends.length;
        document.getElementById('healthyBackends').textContent = `${healthy.length} здоровых`;
        
        const totalModels = backends.reduce((sum, b) => sum + (b.ollama?.runningModels?.length || 0), 0);
        document.getElementById('totalModels').textContent = totalModels;
        document.getElementById('loadedModels').textContent = `${totalModels} загружено`;
        
        const totalSessions = backends.reduce((sum, b) => sum + (b.activeRequests || 0), 0);
        const totalRPS = backends.reduce((sum, b) => sum + (b.ollama?.requestsPerSecond || 0), 0);
        document.getElementById('totalSessions').textContent = totalSessions;
        document.getElementById('sessionRate').textContent = `${totalRPS.toFixed(1)} req/s`;
        
        const queueSize = this.data.queue?.current_size || 0;
        const queueProcessed = this.data.queue?.processed_total || 0;
        document.getElementById('queueSize').textContent = queueSize;
        document.getElementById('queueProcessed').textContent = `${queueProcessed} обработано`;
        
        // Ollama Runtime
        this.renderRuntimeCluster(backends);
        
        // GPU Cluster
        this.renderGPUCluster(backends);
        
        // Backend Capacity
        this.renderCapacitySection(backends);
        
        // Available to Load
        this.renderAvailableModels(backends);
        
        // Backends table
        this.renderBackendsTable(backends);
    }

    renderRuntimeCluster(backends) {
        const container = document.getElementById('runtimeCluster');
        if (!backends.length) {
            container.innerHTML = '<div class="loading">Нет данных о бэкендах</div>';
            return;
        }
        
        container.innerHTML = backends.map(backend => {
            const flags = backend.ollama?.runtimeFlags || {};
            const contexts = backend.ollama?.modelContexts || [];
            
            const flagBadges = [];
            if (flags.numGpuLayers !== undefined && flags.numGpuLayers !== 0) {
                const cls = flags.numGpuLayers === -1 ? '' : 'warning';
                flagBadges.push(`<span class="flag-badge ${cls}">GPU:${flags.numGpuLayers === -1 ? 'auto' : flags.numGpuLayers}</span>`);
            }
            if (flags.contextLength) flagBadges.push(`<span class="flag-badge">C:${this.formatNumber(flags.contextLength)}</span>`);
            if (flags.numParallel && flags.numParallel > 1) flagBadges.push(`<span class="flag-badge warning">NP:${flags.numParallel}</span>`);
            if (flags.numThreads) flagBadges.push(`<span class="flag-badge">T:${flags.numThreads}</span>`);
            if (flags.batchSize && flags.batchSize !== 512) flagBadges.push(`<span class="flag-badge">B:${flags.batchSize}</span>`);
            if (flags.lowVram) flagBadges.push(`<span class="flag-badge danger">LOW_VRAM</span>`);
            if (flags.flashAttention) flagBadges.push(`<span class="flag-badge">FA</span>`);
            if (flags.kvCacheQuant && flags.kvCacheQuant !== 'f16') flagBadges.push(`<span class="flag-badge warning">KV:${flags.kvCacheQuant}</span>`);
            
            const contextBadges = contexts.map(ctx => `
                <span class="context-info">
                    ${ctx.name}: Ctx ${this.formatNumber(ctx.effectiveContext)} (${ctx.contextSource})
                    <div class="context-tooltip">
                        <div class="context-tooltip-row"><span class="context-tooltip-label">Context</span><span class="context-tooltip-value">${this.formatNumber(ctx.contextLength)}</span></div>
                        <div class="context-tooltip-row"><span class="context-tooltip-label">Effective</span><span class="context-tooltip-value">${this.formatNumber(ctx.effectiveContext)}</span></div>
                        <div class="context-tooltip-row"><span class="context-tooltip-label">Model Memory</span><span class="context-tooltip-value">${this.formatMB(ctx.modelMemoryMB)}</span></div>
                        <div class="context-tooltip-row"><span class="context-tooltip-label">Context Memory</span><span class="context-tooltip-value">${this.formatMB(ctx.contextMemoryMB)}</span></div>
                        <div class="context-tooltip-row"><span class="context-tooltip-label">KV Cache</span><span class="context-tooltip-value">${this.formatMB(ctx.kvCacheMemoryMB)}</span></div>
                        <div class="context-tooltip-row"><span class="context-tooltip-label">Total</span><span class="context-tooltip-value">${this.formatMB(ctx.totalMemoryMB)}</span></div>
                        <div class="context-tooltip-row"><span class="context-tooltip-label">Layers</span><span class="context-tooltip-value">${ctx.numLayers || '-'}</span></div>
                        <div class="context-tooltip-row"><span class="context-tooltip-label">Precision</span><span class="context-tooltip-value">${ctx.precisionBits || 16}-bit</span></div>
                    </div>
                </span>
            `).join('');
            
            return `
                <div class="runtime-card">
                    <div class="runtime-header">
                        <strong>${backend.id}</strong>
                        <span class="badge ${backend.status === 'healthy' ? 'badge-success' : 'badge-danger'}">${backend.status}</span>
                    </div>
                    <div class="runtime-flags">
                        ${flagBadges.length ? flagBadges.join('') : '<span class="flag-badge">default</span>'}
                    </div>
                    ${contextBadges ? `<div style="margin-top:0.5rem;">${contextBadges}</div>` : ''}
                </div>
            `;
        }).join('');
    }

    renderCapacitySection(backends) {
        const container = document.getElementById('capacitySection');
        if (!backends.length) {
            container.innerHTML = '<div class="loading">Нет данных о бэкендах</div>';
            return;
        }
        
        container.innerHTML = backends.map(backend => {
            const cap = backend.ollama?.backendCapacity || {};
            const gpu = backend.gpu || {};
            const vramTotal = gpu.memoryTotal || 1;
            const vramUsed = gpu.memoryUsed || 0;
            const vramFree = gpu.memoryFree || 0;
            
            const loadedVram = cap.loadedModelVram || 0;
            const ctxOverhead = cap.contextOverheadMB || 0;
            const guaranteed = cap.guaranteedVram || 0;
            
            const usedPct = vramTotal > 0 ? (vramUsed / vramTotal * 100) : 0;
            const loadedPct = vramTotal > 0 ? (loadedVram / vramTotal * 100) : 0;
            const ctxPct = vramTotal > 0 ? (ctxOverhead / vramTotal * 100) : 0;
            const guarPct = vramTotal > 0 ? (guaranteed / vramTotal * 100) : 0;
            
            return `
                <div class="capacity-card">
                    <div class="runtime-header">
                        <strong>${backend.id}</strong>
                        <span class="badge badge-info">${cap.mode || 'gpu'}</span>
                    </div>
                    <div class="capacity-bar-container">
                        <div class="capacity-bar-labels">
                            <span>VRAM: ${this.formatMB(vramUsed)} / ${this.formatMB(vramTotal)}</span>
                            <span>${usedPct.toFixed(1)}%</span>
                        </div>
                        <div class="capacity-bar">
                            <div class="capacity-bar-filled" style="width: ${loadedPct}%"></div>
                            <div class="capacity-bar-context" style="width: ${ctxPct}%"></div>
                            <div class="capacity-bar-guaranteed" style="width: ${guarPct}%"></div>
                        </div>
                    </div>
                    <div class="capacity-stats">
                        <div class="capacity-stat"><span>Loaded models</span><span>${this.formatMB(loadedVram)}</span></div>
                        <div class="capacity-stat"><span>Context overhead</span><span>${this.formatMB(ctxOverhead)}</span></div>
                        <div class="capacity-stat"><span>Free VRAM</span><span>${this.formatMB(vramFree)}</span></div>
                        <div class="capacity-stat"><span>Guaranteed (90%)</span><span>${this.formatMB(guaranteed)}</span></div>
                    </div>
                </div>
            `;
        }).join('');
    }

    renderAvailableModels(backends) {
        const container = document.getElementById('availableModelsList');
        const badge = document.getElementById('loadableModelCount');
        
        if (!backends.length) {
            container.innerHTML = '<div class="loading">Нет данных</div>';
            badge.textContent = '-';
            return;
        }
        
        let totalLoadable = 0;
        const allModels = [];
        
        backends.forEach(backend => {
            const cap = backend.ollama?.backendCapacity || {};
            const models = cap.availableModels || [];
            models.forEach(m => {
                allModels.push({
                    ...m,
                    backendId: backend.id,
                    backendStatus: backend.status
                });
                if (m.canLoad) totalLoadable++;
            });
        });
        
        badge.textContent = totalLoadable;
        
        if (!allModels.length) {
            container.innerHTML = '<div class="loading">Нет доступных моделей</div>';
            return;
        }
        
        // Sort: loadable first, then by estimated VRAM
        allModels.sort((a, b) => {
            if (a.canLoad !== b.canLoad) return b.canLoad - a.canLoad;
            return (a.estimatedVram || 0) - (b.estimatedVram || 0);
        });
        
        container.innerHTML = allModels.slice(0, 50).map(m => {
            const loadable = m.canLoad;
            const cls = loadable ? 'model-loadable' : 'model-unloadable';
            const vram = m.estimatedVram || 0;
            return `
                <div class="available-model-item ${cls}">
                    <div class="model-indicator"></div>
                    <span class="model-name">${m.name}</span>
                    <span class="model-vram">${this.formatMB(vram)} @ ${m.backendId}</span>
                </div>
            `;
        }).join('');
        
        if (allModels.length > 50) {
            container.innerHTML += `<div style="text-align:center;color:var(--text-muted);padding:0.5rem;font-size:0.8rem;">+${allModels.length - 50} моделей</div>`;
        }
    }

    formatNumber(n) {
        if (n === undefined || n === null) return '-';
        if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
        if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
        return n.toString();
    }

    formatMB(mb) {
        if (mb === undefined || mb === null) return '-';
        if (mb >= 1024) return (mb / 1024).toFixed(1) + ' GB';
        return Math.round(mb) + ' MB';
    }

    renderGPUCluster(backends) {
        const container = document.getElementById('gpuCluster');
        const badge = document.getElementById('gpuClusterBadge');
        
        if (!backends.length) {
            container.innerHTML = '<div class="loading">Нет данных о бэкендах</div>';
            badge.textContent = '-';
            return;
        }
        
        badge.textContent = `${backends.length} GPU`;
        
        container.innerHTML = backends.map(backend => {
            const gpu = backend.gpu || {};
            const usage = gpu.usagePercent || 0;
            const vramUsed = gpu.memoryUsed || 0;
            const vramTotal = gpu.memoryTotal || 1;
            const vramPercent = (vramUsed / vramTotal * 100).toFixed(1);
            const temp = gpu.temperature || 0;
            const power = gpu.powerUsage || 0;
            const status = this.getGPUStatus(usage, vramPercent, temp);
            
            return `
                <div class="gpu-card">
                    <div class="gpu-card-header">
                        <span class="gpu-card-title">${backend.id}</span>
                        <span class="gpu-status ${status}"></span>
                    </div>
                    <div class="gpu-metrics">
                        <div class="gpu-metric">
                            <div class="gpu-metric-label">GPU</div>
                            <div class="gpu-metric-value">${usage.toFixed(1)}%</div>
                        </div>
                        <div class="gpu-metric">
                            <div class="gpu-metric-label">VRAM</div>
                            <div class="gpu-metric-value">${vramPercent}%</div>
                        </div>
                        <div class="gpu-metric">
                            <div class="gpu-metric-label">Temp</div>
                            <div class="gpu-metric-value">${temp}°C</div>
                        </div>
                        <div class="gpu-metric">
                            <div class="gpu-metric-label">Power</div>
                            <div class="gpu-metric-value">${power}W</div>
                        </div>
                    </div>
                    <div class="progress-bar">
                        <div class="progress-fill ${this.getProgressClass(usage)}" style="width: ${usage}%"></div>
                    </div>
                </div>
            `;
        }).join('');
    }

    getGPUStatus(gpu, vram, temp) {
        if (gpu > 90 || vram > 90 || temp > 85) return 'critical';
        if (gpu > 70 || vram > 70 || temp > 75) return 'warning';
        return 'healthy';
    }

    getProgressClass(value) {
        if (value > 80) return 'high';
        if (value > 50) return 'medium';
        return 'low';
    }

    // Backends Table
    renderBackendsTable(backends) {
        const tbody = document.getElementById('backendsTableBody');
        
        if (!backends.length) {
            tbody.innerHTML = '<tr><td colspan="11" class="loading-cell">Нет данных</td></tr>';
            return;
        }
        
        tbody.innerHTML = backends.map(b => {
            const gpu = b.gpu || {};
            const sys = b.system || {};
            const pred = b.prediction || {};
            const oll = b.ollama || {};
            
            const gpuUsage = gpu.usagePercent || 0;
            const vramTotal = gpu.memoryTotal || 1;
            const vramUsed = gpu.memoryUsed || 0;
            const vramPercent = (vramUsed / vramTotal * 100).toFixed(1);
            const cpuUsage = sys.cpuUsagePercent || 0;
            const ramTotal = sys.memoryTotal || 1;
            const ramUsed = sys.memoryUsed || 0;
            const ramPercent = (ramUsed / ramTotal * 100).toFixed(1);
            const activeReq = b.activeRequests || 0;
            const maxReq = b.maxConcurrentRequests || 10;
            const models = oll.runningModels?.length || 0;
            const rps = oll.requestsPerSecond || 0;
            const secondsToCrit = pred.secondsToCritical || -1;
            const predClass = secondsToCrit > 0 && secondsToCrit < 300 ? 'warning' : 'success';
            const predText = secondsToCrit > 0 ? `${Math.round(secondsToCrit)}с` : 'OK';
            
            return `
                <tr>
                    <td><strong>${b.id}</strong></td>
                    <td><span class="badge badge-${b.status === 'healthy' ? 'success' : 'danger'}">${b.status}</span></td>
                    <td>${gpuUsage.toFixed(1)}%</td>
                    <td>${vramPercent}%</td>
                    <td>${cpuUsage.toFixed(1)}%</td>
                    <td>${ramPercent}%</td>
                    <td>${activeReq}/${maxReq}</td>
                    <td>${models}</td>
                    <td>${rps.toFixed(1)}</td>
                    <td><span class="badge badge-${predClass}">${predText}</span></td>
                    <td>
                        <button class="action-btn edit" onclick="ui.editBackend('${b.id}')">✎</button>
                        <button class="action-btn delete" onclick="ui.confirmDeleteBackend('${b.id}')">🗑</button>
                    </td>
                </tr>
            `;
        }).join('');
    }

    filterBackends(query) {
        const rows = document.querySelectorAll('#backendsTableBody tr');
        const lowerQuery = query.toLowerCase();
        
        rows.forEach(row => {
            const text = row.textContent.toLowerCase();
            row.style.display = text.includes(lowerQuery) ? '' : 'none';
        });
    }

    // Backends Page
    renderBackendsPage() {
        const tbody = document.getElementById('backendsManageBody');
        const backends = [...this.data.backends].sort((a, b) => (a.id || '').localeCompare(b.id || ''));
        
        if (!backends.length) {
            tbody.innerHTML = '<tr><td colspan="13" class="loading-cell">Нет данных</td></tr>';
            return;
        }
        
        tbody.innerHTML = backends.map(b => {
            const labels = (b.labels || []).join(', ') || '-';
            const lastContact = b.lastAgentContact ? new Date(b.lastAgentContact).toLocaleString('ru') : '-';
            const maxModels = b.maxModels || b.ollama?.maxModels || b.runtimeMaxModels || '-';
            return `
            <tr>
                <td><strong>${b.id}</strong></td>
                <td>${b.name || b.id}</td>
                <td>${b.host}</td>
                <td>${b.ollamaPort || 11434}</td>
                <td>${b.agentPort || 18032}</td>
                <td>${b.weight || 1}</td>
                <td>${b.maxConcurrentRequests || 10}</td>
                <td>${maxModels}</td>
                <td><span class="badge badge-${b.hasAgent ? 'success' : 'warning'}">${b.hasAgent ? 'Да' : 'Нет'}</span></td>
                <td>${labels}</td>
                <td>${lastContact}</td>
                <td><span class="badge badge-${b.status === 'healthy' ? 'success' : 'danger'}">${b.status}</span></td>
                <td>
                    <button class="action-btn edit" onclick="ui.editBackend('${b.id}')">✎</button>
                    <button class="action-btn delete" onclick="ui.confirmDeleteBackend('${b.id}')">🗑</button>
                </td>
            </tr>
        `}).join('');
    }

    // Models Page
    renderModelsPage() {
        const backends = this.data.backends;
        const allModels = [];
        
        backends.forEach(b => {
            (b.ollama?.runningModels || []).forEach(m => {
                allModels.push({
                    ...m,
                    backend: b.id,
                    backendStatus: b.status
                });
            });
        });
        
        document.getElementById('modelsTotal').textContent = allModels.length;
        document.getElementById('modelsLoaded').textContent = allModels.filter(m => m.backendStatus === 'healthy').length;
        
        const container = document.getElementById('modelsGrid');
        
        if (!allModels.length) {
            container.innerHTML = '<div class="loading">Нет загруженных моделей</div>';
            return;
        }
        
        container.innerHTML = allModels.map(m => {
            const vramMB = (m.vramUsage || 0) / 1024 / 1024;
            const ramMB = (m.ramUsage || 0) / 1024 / 1024;
            const sizeGB = (m.size || 0) / 1024 / 1024 / 1024;
            
            return `
                <div class="model-card">
                    <div class="model-card-header">
                        <span class="model-name">${m.name}</span>
                        <span class="badge badge-${m.backendStatus === 'healthy' ? 'success' : 'danger'}">${m.backend}</span>
                    </div>
                    <div class="model-size">${sizeGB.toFixed(1)} GB</div>
                    <div class="model-details">
                        <div class="model-detail">
                            <div class="model-detail-label">VRAM</div>
                            <div class="model-detail-value">${vramMB.toFixed(0)} MB</div>
                        </div>
                        <div class="model-detail">
                            <div class="model-detail-label">RAM</div>
                            <div class="model-detail-value">${ramMB.toFixed(0)} MB</div>
                        </div>
                        <div class="model-detail">
                            <div class="model-detail-label">Family</div>
                            <div class="model-detail-value">${m.family || '-'}</div>
                        </div>
                        <div class="model-detail">
                            <div class="model-detail-label">Format</div>
                            <div class="model-detail-value">${m.format || '-'}</div>
                        </div>
                        <div class="model-detail">
                            <div class="model-detail-label">Params</div>
                            <div class="model-detail-value">${m.parameterSize || '-'}</div>
                        </div>
                        <div class="model-detail">
                            <div class="model-detail-label">Quant</div>
                            <div class="model-detail-value">${m.quantization || '-'}</div>
                        </div>
                    </div>
                </div>
            `;
        }).join('');
    }

    // Sessions Page
    renderSessionsPage() {
        // Fetch sessions from API
        this.fetchSessions();
    }

    async fetchSessions() {
        try {
            const response = await fetch(`${API_BASE}/api/v1/sessions`);
            if (!response.ok) throw new Error('Failed to fetch sessions');
            
            const data = await response.json();
            this.data.sessions = data.sessions || [];
            // Обновляем sessions-виджет на dashboard
            this.updateSessionsDashboard();
            this.renderSessionsTable();
        } catch (e) {
            console.error('Failed to fetch sessions:', e);
            this.addLog('Ошибка загрузки сессий', 'error');
        }
    }

    // Обновление sessions-виджета на dashboard
    updateSessionsDashboard() {
        const sessions = this.data.sessions || [];
        const activeCount = sessions.filter(s => s.active).length;
        const totalRequests = sessions.reduce((sum, s) => sum + (s.requestCount || 0), 0);
        const elTotal = document.getElementById('totalSessions');
        const elRate = document.getElementById('sessionRate');
        if (elTotal) elTotal.textContent = activeCount;
        if (elRate) elRate.textContent = `${totalRequests} запросов`;
    }

    renderSessionsTable() {
        const tbody = document.getElementById('sessionsTableBody');
        const sessions = this.data.sessions;
        
        if (!sessions.length) {
            tbody.innerHTML = '<tr><td colspan="6" class="loading-cell">Нет активных сессий</td></tr>';
            return;
        }
        
        tbody.innerHTML = sessions.map(s => `
            <tr>
                <td><code>${s.id?.substring(0, 16) || 'N/A'}...</code></td>
                <td>${s.backendId || '-'}</td>
                <td>${s.model || '-'}</td>
                <td>${s.requestCount || 0}</td>
                <td>${s.lastActivity ? new Date(s.lastActivity).toLocaleString('ru') : '-'}</td>
                <td><span class="badge badge-${s.active ? 'success' : 'warning'}">${s.active ? 'Активна' : 'Неактивна'}</span></td>
            </tr>
        `).join('');
    }

    filterSessions(query) {
        const rows = document.querySelectorAll('#sessionsTableBody tr');
        const lowerQuery = query.toLowerCase();
        
        rows.forEach(row => {
            const text = row.textContent.toLowerCase();
            row.style.display = text.includes(lowerQuery) ? '' : 'none';
        });
    }

    // Queue Page
    renderQueuePage() {
        const queue = this.data.queue || {};
        const current = queue.current_size || 0;
        const max = queue.max_size || 100;
        const processed = queue.processed_total || 0;
        const workers = queue.workers || 4;
        
        document.getElementById('queueCurrentSize').textContent = current;
        document.getElementById('queueMaxSize').textContent = max;
        document.getElementById('queueProcessed').textContent = processed;
        document.getElementById('queueWorkers').textContent = workers;
        
        const percent = max > 0 ? (current / max * 100) : 0;
        document.getElementById('queueFill').style.width = percent + '%';
        document.getElementById('queueMidLabel').textContent = Math.round(max / 2);
        document.getElementById('queueMaxLabel').textContent = max;
    }

    // Logs
    addLog(message, level = 'info') {
        const entry = {
            time: new Date().toLocaleTimeString('ru'),
            level: level.toUpperCase(),
            message
        };
        
        this.data.logs.unshift(entry);
        if (this.data.logs.length > 500) {
            this.data.logs = this.data.logs.slice(0, 500);
        }
        
        if (this.currentPage === 'logs') {
            this.renderLogs();
        }
    }

    renderLogs() {
        const container = document.getElementById('logsContainer');
        
        if (!this.data.logs.length) {
            container.innerHTML = '<div class="loading">Нет логов</div>';
            return;
        }
        
        container.innerHTML = this.data.logs.map(log => `
            <div class="log-entry">
                <span class="log-time">${log.time}</span>
                <span class="log-level ${log.level.toLowerCase()}">${log.level}</span>
                <span class="log-message">${this.escapeHtml(log.message)}</span>
            </div>
        `).join('');
        
        container.scrollTop = 0;
    }

    exportLogs() {
        const content = this.data.logs.map(l => `[${l.time}] ${l.level}: ${l.message}`).join('\n');
        this.downloadFile(content, 'ollamalegion-logs.txt', 'text/plain');
    }

    exportBackends() {
        const content = JSON.stringify(this.data.backends, null, 2);
        this.downloadFile(content, 'ollamalegion-backends.json', 'application/json');
    }

    downloadFile(content, filename, type) {
        const blob = new Blob([content], { type });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = filename;
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        URL.revokeObjectURL(url);
    }

    // Prediction Alerts
    renderPredictionAlerts() {
        const alerts = [];
        const backends = this.data.backends;
        
        backends.forEach(b => {
            const pred = b.prediction || {};
            const seconds = pred.secondsToCritical;
            
            if (seconds > 0 && seconds < 300) {
                alerts.push({
                    backend: b.id,
                    reason: pred.criticalReason || 'unknown',
                    seconds: Math.round(seconds),
                    level: seconds < 120 ? 'danger' : 'warning'
                });
            }
        });
        
        const container = document.getElementById('alertsList');
        
        if (!alerts.length) {
            container.innerHTML = '<div class="alert alert-info">Нет активных предупреждений</div>';
            return;
        }
        
        container.innerHTML = alerts.map(a => `
            <div class="alert alert-${a.level}">
                <span>⚠️</span>
                <span><strong>${a.backend}</strong>: ${a.reason} через ${a.seconds}с</span>
            </div>
        `).join('');
    }

    // Backend CRUD
    openBackendModal(backendId = null) {
        const modal = document.getElementById('backendModal');
        const title = document.getElementById('modalTitle');
        const deleteBtn = document.getElementById('modalDelete');
        
        if (backendId) {
            const backend = this.data.backends.find(b => b.id === backendId);
            if (!backend) return;
            
            title.textContent = 'Редактировать бэкенд';
            deleteBtn.style.display = 'inline-block';
            
            document.getElementById('formBackendId').value = backend.id;
            document.getElementById('formBackendId').disabled = true;
            document.getElementById('formBackendName').value = backend.name || '';
            document.getElementById('formBackendHost').value = backend.host || '';
            document.getElementById('formBackendOllamaPort').value = backend.ollamaPort || 11434;
            document.getElementById('formBackendAgentPort').value = backend.agentPort || 18032;
            document.getElementById('formBackendWeight').value = backend.weight || 1;
            document.getElementById('formBackendMaxConcurrent').value = backend.maxConcurrentRequests || 10;
            document.getElementById('formBackendLabels').value = (backend.labels || []).join(',');
        } else {
            title.textContent = 'Добавить бэкенд';
            deleteBtn.style.display = 'none';
            
            document.getElementById('formBackendId').value = '';
            document.getElementById('formBackendId').disabled = false;
            document.getElementById('formBackendName').value = '';
            document.getElementById('formBackendHost').value = '';
            document.getElementById('formBackendOllamaPort').value = 11434;
            document.getElementById('formBackendAgentPort').value = 18032;
            document.getElementById('formBackendWeight').value = 1;
            document.getElementById('formBackendMaxConcurrent').value = 10;
            document.getElementById('formBackendLabels').value = '';
        }
        
        modal.classList.add('active');
    }

    closeModal() {
        document.getElementById('backendModal').classList.remove('active');
    }

    async saveBackend() {
        const id = document.getElementById('formBackendId').value.trim();
        const name = document.getElementById('formBackendName').value.trim();
        const host = document.getElementById('formBackendHost').value.trim();
        const ollamaPort = parseInt(document.getElementById('formBackendOllamaPort').value) || 11434;
        const agentPort = parseInt(document.getElementById('formBackendAgentPort').value) || 18032;
        const weight = parseFloat(document.getElementById('formBackendWeight').value) || 1;
        const maxConcurrent = parseInt(document.getElementById('formBackendMaxConcurrent').value) || 10;
        const labels = document.getElementById('formBackendLabels').value.split(',').map(l => l.trim()).filter(Boolean);
        
        if (!id || !host) {
            this.showToast('ID и хост обязательны', 'error');
            return;
        }
        
        const backend = {
            id,
            name: name || id,
            host,
            ollamaPort,
            agentPort,
            weight,
            maxConcurrentRequests: maxConcurrent,
            labels
        };
        
        const isEdit = document.getElementById('formBackendId').disabled;
        const method = isEdit ? 'PUT' : 'POST';
        const url = isEdit ? `${API_BASE}/api/v1/backends/${id}` : `${API_BASE}/api/v1/backends`;
        
        try {
            const response = await fetch(url, {
                method,
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(backend)
            });
            
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            
            this.closeModal();
            this.showToast(isEdit ? 'Бэкенд обновлен' : 'Бэкенд добавлен', 'success');
            this.addLog(`Бэкенд ${id} ${isEdit ? 'обновлен' : 'добавлен'}`, 'info');
            this.refreshCurrentPage();
        } catch (e) {
            this.showToast(`Ошибка: ${e.message}`, 'error');
            this.addLog(`Ошибка сохранения бэкенда: ${e.message}`, 'error');
        }
    }

    editBackend(id) {
        this.openBackendModal(id);
    }

    confirmDeleteBackend(id) {
        this.openBackendModal(id);
        // The delete button is shown in edit mode
    }

    async deleteBackend() {
        const id = document.getElementById('formBackendId').value;
        
        if (!confirm(`Удалить бэкенд ${id}?`)) return;
        
        try {
            const response = await fetch(`${API_BASE}/api/v1/backends/${id}`, {
                method: 'DELETE'
            });
            
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            
            this.closeModal();
            this.showToast('Бэкенд удален', 'success');
            this.addLog(`Бэкенд ${id} удален`, 'info');
            this.refreshCurrentPage();
        } catch (e) {
            this.showToast(`Ошибка: ${e.message}`, 'error');
        }
    }

    // Settings
    async loadSettings() {
        try {
            const response = await fetch(`${API_BASE}/api/v1/health`);
            if (!response.ok) return;
            
            const config = await response.json();
            // Note: The health endpoint may not return full config
            // This is a placeholder - actual implementation would fetch config
        } catch (e) {
            console.error('Failed to load settings:', e);
        }
    }

    async saveSettings() {
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
        
        // Note: Currently there's no API endpoint to save settings dynamically
        // This would need to be implemented in the backend
        this.showToast('Настройки сохранены (требуется рестарт)', 'success');
        this.addLog('Настройки обновлены', 'info');
    }

    // Utility
    refreshCurrentPage() {
        this.refreshPage(this.currentPage);
    }

    showToast(message, type = 'info') {
        const container = document.getElementById('toastContainer');
        const toast = document.createElement('div');
        toast.className = `toast ${type}`;
        
        const icons = { success: '✅', error: '❌', warning: '⚠️', info: 'ℹ️' };
        toast.innerHTML = `<span>${icons[type] || 'ℹ️'}</span><span>${message}</span>`;
        
        container.appendChild(toast);
        
        setTimeout(() => {
            toast.style.opacity = '0';
            toast.style.transform = 'translateX(100%)';
            setTimeout(() => toast.remove(), 300);
        }, 4000);
    }

    escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text;
        return div.innerHTML;
    }

    // Periodic refresh for non-WS data
    startPeriodicRefresh() {
        // Начальная загрузка queue и sessions
        this.fetchQueue();
        this.fetchSessions();
        
        setInterval(() => {
            // Всегда обновляем queue (нужно для dashboard-виджета)
            this.fetchQueue();
            // Всегда обновляем sessions (нужно для dashboard-виджета)
            this.fetchSessions();
        }, 5000);
    }

    async fetchClusterState() {
        try {
            const response = await fetch(`${API_BASE}/api/v1/cluster`);
            if (!response.ok) throw new Error('Failed to fetch cluster state');
            
            const state = await response.json();
            this.data.backends = state.backends || [];
            this.renderDashboard();
            this.renderPredictionAlerts();
        } catch (e) {
            console.error('Failed to fetch cluster state:', e);
        }
    }

    async fetchQueue() {
        try {
            const response = await fetch(`${API_BASE}/api/v1/queue/stats`);
            if (response.ok) {
                this.data.queue = await response.json();
                // Обновляем queue-виджеты на dashboard всегда
                this.updateQueueDashboard();
                if (this.currentPage === 'queue') {
                    this.renderQueuePage();
                }
            }
        } catch (e) {
            console.error('Failed to fetch queue:', e);
        }
    }

    // Обновление queue-виджетов на dashboard
    updateQueueDashboard() {
        const queue = this.data.queue || {};
        const queueSize = queue.current_size || 0;
        const queueProcessed = queue.processed_total || 0;
        const elSize = document.getElementById('queueSize');
        const elProcessed = document.getElementById('queueProcessed');
        if (elSize) elSize.textContent = queueSize;
        if (elProcessed) elProcessed.textContent = `${queueProcessed} обработано`;
    }
}

// Initialize
const ui = new OllamaLegionUI();