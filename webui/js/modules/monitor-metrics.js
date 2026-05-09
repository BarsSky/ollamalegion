/**
 * Monitor Metrics Module
 * WebSocket subscription, metric updates, DOM sync for monitor
 * Used by: monitor.html
 * Dependencies: MonitorApp (MA), MonitorCharts, MonitorBackends
 */

const MonitorMetrics = (() => {
    'use strict';

    const MA = window.MonitorApp || {};
    let ws = null;
    let reconnectTimer = null;
    let metricCallbacks = [];

    // ===== WebSocket Connection =====
    function connect() {
        if (ws) {
            ws.close();
            ws = null;
        }

        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const wsUrl = `${protocol}//${window.location.host}/ws/metrics`;

        try {
            ws = new WebSocket(wsUrl);

            ws.onopen = () => {
                console.log('[MonitorMetrics] WebSocket connected');
                if (reconnectTimer) {
                    clearTimeout(reconnectTimer);
                    reconnectTimer = null;
                }
                if (typeof MA.onWSConnect === 'function') MA.onWSConnect();
            };

            ws.onmessage = (event) => {
                try {
                    const data = JSON.parse(event.data);
                    notifyUpdate(data);
                } catch (e) {
                    console.error('[MonitorMetrics] Parse error:', e);
                }
            };

            ws.onclose = () => {
                ws = null;
                reconnectTimer = setTimeout(connect, 5000);
            };

            ws.onerror = (err) => {
                console.error('[MonitorMetrics] WebSocket error:', err);
            };
        } catch (e) {
            console.error('[MonitorMetrics] Failed to create WS:', e);
        }
    }

    function disconnect() {
        if (ws) {
            ws.close();
            ws = null;
        }
        if (reconnectTimer) {
            clearTimeout(reconnectTimer);
            reconnectTimer = null;
        }
    }

    // ===== Metric Updates =====
    function onUpdate(callback) {
        metricCallbacks.push(callback);
    }

    function notifyUpdate(data) {
        metricCallbacks.forEach(cb => {
            try { cb(data); } catch (e) { console.error('[MonitorMetrics] Callback error:', e); }
        });
    }

    // ===== DOM Sync Helpers =====
    function updateText(id, text) {
        const el = document.getElementById(id);
        if (el) el.textContent = text;
    }

    function updateHTML(id, html) {
        const el = document.getElementById(id);
        if (el) el.innerHTML = html;
    }

    function setClass(id, className) {
        const el = document.getElementById(id);
        if (el) el.className = className;
    }

    // ===== Batch Update from Cluster Data =====
    function updateClusterMetrics(cluster) {
        if (!cluster) return;

        const backends = cluster.backends || [];
        const act = backends.reduce((s, b) => s + (b.activeRequests || 0), 0);
        const totV = backends.reduce((s, b) => s + ((b.vram && b.vram.totalGB) || 0), 0);
        const usdV = backends.reduce((s, b) => s + ((b.vram && b.vram.usedGB) || 0), 0);
        const tml = backends.reduce((s, b) => s + ((b.models || []).length), 0);

        updateText('statBackends', backends.length);
        updateText('statActive', act);
        updateText('statVRAM', totV > 0 ? usdV.toFixed(1) + '/' + totV.toFixed(0) + 'G' : '—');
        updateText('statModelsLoaded', tml);

        // Update charts if available
        if (typeof MonitorCharts !== 'undefined' && MonitorCharts.updateFromMetrics) {
            MonitorCharts.updateFromMetrics({
                rps: cluster.rps || 0,
                backends: backends
            });
        }
    }

    function updateQueueMetrics(queueStats) {
        if (!queueStats) return;
        updateText('statPending', queueStats.current_size || 0);
        updateText('statProcessing', queueStats.processing_count || 0);
    }

    // ===== Public API =====
    return {
        connect,
        disconnect,
        onUpdate,
        updateText,
        updateHTML,
        setClass,
        updateClusterMetrics,
        updateQueueMetrics
    };
})();