/**
 * app-listeners.js — Event listener setup + WebSocket event handling.
 *
 * R57.3 (2026-09-03): extracted from webui/js/app.js.
 *
 * Этот файл содержит "registration" код:
 *   - setupNavigation — навигация между страницами
 *   - setupRestartHandler — кнопка restart balancer
 *   - setupWebSocketEvents + helpers — WS events + cross-tab sync
 *   - setupApiEvents — глобальный обработчик api-error
 *   - setupLogsTabNavigation — переключение System/Proxy табов в Logs
 *
 * НЕ содержит (намеренно оставлено в app.js):
 *   - setupEventListeners — слишком много cross-cuts с modal/state/data функциями
 *     в app.js. Будет разделено в R57.4+ когда будет спроектирован общий
 *     context object.
 *
 * Загружается ПОСЛЕ app-core.js и ДО app.js (см. webui/index.html).
 *
 * Контракт:
 *   - window.App = { ... } — shared namespace (из app-core.js)
 *   - window.App.context = { ... } — populated by app.js ПЕРЕД вызовом setup*()
 *   - window.App.setupNavigation / setupRestartHandler / setupWebSocketEvents /
 *     setupApiEvents / setupLogsTabNavigation
 *   - window.broadcastCrossTab — exposed globally (используется gguf-renderer.js)
 *   - window.refreshActiveQueriesCrossTab — exposed globally (используется gguf-renderer.js)
 *
 * Зависимости (предполагаются уже загруженными):
 *   - window.I18N.t (i18n/index.js)
 *   - window.Utils (utils.js)
 *   - window.Api (api.js)
 *   - window.WebSocketManager (modules/websocket.js)
 *   - window.Renderers.* (modules/renderers.js) — proxyLogs, copyProxyLogs
 *   - window.AutoTuneHistory (modules/autotune_history.js)
 *   - window.GgufRenderer (modules/gguf-renderer.js) — refreshActiveQueriesPolling
 */
(function() {
    'use strict';

    const App = (window.App = window.App || {});

    // ---- Restart Handler ----

    App.setupRestartHandler = function() {
        const ctx = App.context;
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
                window.Api.post('/api/v1/admin/restart').then(function () {
                    if (indicator) indicator.innerHTML = '<div style="color: var(--success); padding: 8px;">' + (window.I18N ? I18N.t('settings.restarted') : 'Balancer restarted successfully') + '</div>';
                    App.showToast(window.I18N ? I18N.t('settings.restarted') : 'Balancer restarted successfully', 'success');
                }).catch(function (err) {
                    if (indicator) indicator.innerHTML = '<div style="color: var(--danger); padding: 8px;">' + (window.I18N ? I18N.t('settings.restart_error') : 'Balancer restart failed') + '</div>';
                    App.showToast((window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (err.message || err), 'error');
                });
            });
        }
    };

    // ---- Navigation ----

    App.setupNavigation = function() {
        const ctx = App.context;
        document.querySelectorAll('.nav-item').forEach(function(item) {
            item.addEventListener('click', function (e) {
                e.preventDefault();
                if (ctx && ctx.switchPage) ctx.switchPage(item.dataset.page);
            });
        });
    };

    // ---- WebSocket Events + Cross-tab Sync ----
    // Round 32 #2 (2026-08-10): Cross-tab sync через BroadcastChannel API.
    // R54.8: initAutoTuneEventHandlers moved to app-core.js (вызывается отдельно).

    let _crossTabChannel = null;
    function getCrossTabChannel() {
        if (_crossTabChannel !== null) return _crossTabChannel;
        if (typeof BroadcastChannel === 'undefined') {
            _crossTabChannel = false;
            return _crossTabChannel;
        }
        try {
            _crossTabChannel = new BroadcastChannel('ollama-legion-sync');
            _crossTabChannel.onmessage = handleCrossTabMessage;
            return _crossTabChannel;
        } catch (e) {
            _crossTabChannel = false;
            return _crossTabChannel;
        }
    }

    function handleCrossTabMessage(event) {
        if (!event || !event.data) return;
        if (event.data.source && event.data.source !== 'ollama-legion-webui') return;
        const data = event.data;
        if (data.type === 'clusterStateChanged') {
            const ctx = App.context;
            if (ctx && ctx.fetchClusterState) ctx.fetchClusterState();
            window.refreshActiveQueriesCrossTab();
        } else if (data.type === 'generationCancelled') {
            window.refreshActiveQueriesCrossTab();
        }
    }

    function broadcastCrossTab(type, extra) {
        const ch = getCrossTabChannel();
        if (!ch || ch === false) return;
        try {
            ch.postMessage(Object.assign({ source: 'ollama-legion-webui', type: type }, extra || {}));
        } catch (e) {
            console.debug('broadcastCrossTab failed:', e);
        }
    }

    // Expose cross-tab helpers в window для доступа из gguf-renderer.js и других модулей.
    window.broadcastCrossTab = broadcastCrossTab;
    window.refreshActiveQueriesCrossTab = function () {
        if (window.GgufRenderer && typeof window.GgufRenderer.refreshActiveQueriesPolling === 'function') {
            window.GgufRenderer.refreshActiveQueriesPolling();
        }
    };

    function backendsEqual(a, b) {
        if (a.length !== b.length) return false;
        const normalize = arr => JSON.stringify(arr.map(x => ({
            id: x.id,
            status: x.status,
            activeRequests: x.activeRequests,
            'gpu.usagePercent': x.gpu && x.gpu.usagePercent,
            'gpu.memoryUsed': x.gpu && x.gpu.memoryUsed,
            'system.cpuUsagePercent': x.system && x.system.cpuUsagePercent,
            'system.memoryUsed': x.system && x.system.memoryUsed,
            'prediction.secondsToCritical': x.prediction && x.prediction.secondsToCritical
        })).sort(function(m, n) { return m.id.localeCompare(n.id); }));
        return normalize(a) === normalize(b);
    }

    function updateBackends(newBackends) {
        const ctx = App.context;
        if (!ctx || !ctx.data) return;
        newBackends = [...newBackends].sort(function(a, b) { return (a.id || '').localeCompare(b.id || ''); });
        if (backendsEqual(ctx.data.backends, newBackends)) return;
        ctx.data.backends = newBackends;
        if (ctx.currentPage === 'dashboard') scheduleDashboardRender();
        broadcastCrossTab('clusterStateChanged');
    }

    function applyStatusChange(backendId, newStatus) {
        const ctx = App.context;
        if (!ctx || !ctx.data) return;
        const b = ctx.data.backends.find(x => x.id === backendId);
        if (b && b.status !== newStatus) {
            b.status = newStatus;
            if (ctx.currentPage === 'dashboard') scheduleDashboardRender();
        } else if (!b) {
            if (ctx.fetchClusterState) ctx.fetchClusterState();
        }
    }

    function scheduleDashboardRender() {
        const ctx = App.context;
        if (ctx.dashboardRenderTimer) clearTimeout(ctx.dashboardRenderTimer);
        ctx.dashboardRenderTimer = setTimeout(function() {
            ctx.dashboardRenderTimer = null;
            if (ctx.refreshPage) ctx.refreshPage('dashboard');
        }, 250);
    }

    function handleWebSocketData(payload) {
        const ctx = App.context;
        const eventType = payload.eventType || 'legacy';

        switch (eventType) {
            case 'clusterState':
                updateBackends((payload.data && payload.data.backends) || []);
                break;
            case 'backendAdd':
                if (ctx && ctx.addLog) {
                    ctx.addLog(window.I18N ? I18N.t('app.backend_added', { name: (payload.data && payload.data.name) || payload.backendId }) : 'Backend added: ' + ((payload.data && payload.data.name) || payload.backendId), 'info');
                }
                if (ctx && ctx.fetchClusterState) ctx.fetchClusterState();
                break;
            case 'backendRemove':
                if (ctx && ctx.addLog) {
                    ctx.addLog(window.I18N ? I18N.t('app.backend_removed', { id: payload.backendId }) : 'Backend removed: ' + payload.backendId, 'info');
                }
                if (ctx && ctx.fetchClusterState) ctx.fetchClusterState();
                break;
            case 'statusChange':
                if (ctx && ctx.addLog) {
                    ctx.addLog(window.I18N ? I18N.t('app.status_changed', { id: payload.backendId, old: payload.data && payload.data.oldStatus, new: payload.data && payload.data.newStatus }) : 'Status ' + payload.backendId + ': ' + (payload.data && payload.data.oldStatus) + ' -> ' + (payload.data && payload.data.newStatus), 'warning');
                }
                applyStatusChange(payload.backendId, payload.data && payload.data.newStatus);
                break;
            case 'limitsChange':
                if (ctx && ctx.addLog) {
                    ctx.addLog(window.I18N ? I18N.t('app.limits_changed', { id: payload.backendId }) : 'Limits ' + payload.backendId + ' updated', 'info');
                }
                if (ctx && ctx.fetchClusterState) ctx.fetchClusterState();
                break;
            case 'proxy_log':
                if (ctx && ctx.addProxyLog && payload.data && payload.data.entry) {
                    ctx.addProxyLog(payload.data.entry);
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

    App.setupWebSocketEvents = function() {
        const ctx = App.context;
        window.addEventListener('ws-open', function() {
            if (ctx && ctx.updateConnectionStatus) ctx.updateConnectionStatus(true);
            if (ctx && ctx.addLog) {
                ctx.addLog(window.I18N ? I18N.t('app.ws_connected') : 'WebSocket connected', 'info');
            }
        });

        window.addEventListener('ws-status', function(e) {
            if (ctx && ctx.updateConnectionStatus) ctx.updateConnectionStatus(e.detail.connected);
        });

        window.addEventListener('ws-error', function() {
            if (ctx && ctx.updateConnectionStatus) ctx.updateConnectionStatus(false);
            if (ctx && ctx.addLog) {
                ctx.addLog(window.I18N ? I18N.t('app.ws_error') : 'WebSocket error', 'error');
            }
        });

        window.addEventListener('ws-reconnecting', function(e) {
            const detail = e.detail || {};
            if (ctx && ctx.addLog) {
                ctx.addLog(window.I18N ? I18N.t('app.ws_reconnect', { attempt: detail.attempt, max: detail.max, delay: Math.round((detail.delay || 0) / 1000) }) : 'Reconnecting... (' + detail.attempt + '/' + detail.max + ') in ' + Math.round((detail.delay || 0) / 1000) + 's', 'warn');
            }
        });

        window.addEventListener('ws-max-reconnect', function() {
            if (ctx && ctx.addLog) {
                ctx.addLog(window.I18N ? I18N.t('app.ws_max_reconnect') : 'Max reconnection attempts reached', 'error');
            }
        });

        window.addEventListener('ws-message', function(e) {
            handleWebSocketData(e.detail);
        });

        window.WebSocketManager.connect();
        // R54.8: init AutoTune event handlers (toast notifications)
        if (App.initAutoTuneEventHandlers) {
            App.initAutoTuneEventHandlers();
        }
        // R55.2: init AutoTune history buttons (modal timeline)
        if (window.AutoTuneHistory && typeof window.AutoTuneHistory.initHistoryButtons === 'function') {
            window.AutoTuneHistory.initHistoryButtons();
        }
    };

    // ---- API Events ----

    App.setupApiEvents = function() {
        const ctx = App.context;
        window.addEventListener('api-error', function(e) {
            const detail = e.detail || {};
            const message = detail.message;
            App.showToast(message, 'error');
            if (ctx && ctx.addLog) ctx.addLog(message, 'error');
            if (message && (message.includes('Network error') || message.includes('Failed to fetch') || message.includes('NetworkError'))) {
                if (ctx && ctx.updateConnectionStatus) ctx.updateConnectionStatus(false);
            }
        });
    };

    // ---- Logs Tab Navigation ----

    App.setupLogsTabNavigation = function() {
        const ctx = App.context;
        const Renderers = window.Renderers;
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
                    if (Renderers && typeof Renderers.proxyLogs === 'function' && ctx && ctx.data) {
                        Renderers.proxyLogs(ctx.data.proxyLogs);
                    }
                }
            });
        });
    };
})();
