/**
 * Monitor Metrics Module
 * WebSocket subscription, metric updates, DOM sync for monitor
 * Used by: monitor.html
 * Dependencies: MonitorApp (MA), MonitorCharts, MonitorBackends
 */

const MonitorMetrics = (() => {
    'use strict';

    const MA = window.MonitorApp || {};

    /**
     * aggregateVRAM — суммарная память кластера БЕЗ двойного счёта.
     *
     * ЗАЧЕМ (R-Image follow-up, 2026-10-07). Одна физическая машина может
     * держать несколько бэкендов: типовой случай — на одной рабочей станции
     * подняты и cppworker (текст), и imageworker (картинки). Каждый агент читает
     * nvidia-smi ОДНОЙ видеокарты и присылает ОДНИ И ТЕ ЖЕ memoryTotal/Used.
     * Наивная сумма по бэкендам показывала 16 GB на карте в 8 GB: в шапке
     * Monitor «VRAM 14.5/16G» вместо реальных «7.1/8G».
     *
     * КАК ОТЛИЧАЕМ «две машины» от «одна машина, два бэкенда»: по UUID карт
     * (gpu.uuids, приходит от агента). У контейнеров одного хоста UUID совпадает
     * побайтово, у разных физических карт — различается. Имя хоста для этого НЕ
     * годится: балансер видит имена контейнеров (cppworker-gpu, imageworker) и
     * считал бы их разными машинами.
     *
     * ФОЛБЭК, когда UUID нет (не-nvidia платформа, старый агент): группируем по
     * имени хоста и берём максимум в группе. Это ровно то, что просил оператор:
     * один хост = одна физическая карта. Обоснование: узлы с несколькими
     * картами в кластере объявляют их отдельными бэкендами, то есть у двух
     * бэкендов одного хоста карта общая по определению.
     *
     * @param {Array} backends — cluster.backends
     * @returns {{totalGB: number, usedGB: number, groups: number, deduped: boolean}}
     */
    function aggregateVRAM(backends) {
        const list = Array.isArray(backends) ? backends : [];
        const byGPU = new Map();
        let deduped = false;

        list.forEach((b) => {
            const v = (b && b.vram) || null;
            if (!v || !v.totalGB) return;

            const g = (b && b.gpu) || {};
            const rawUUIDs = g.uuids || g.UUIDs;
            const uuids = Array.isArray(rawUUIDs) ? rawUUIDs.filter(Boolean).sort() : [];
            let key;
            if (uuids.length) {
                key = 'gpu:' + uuids.join(',');
            } else {
                key = 'host:' + (b.host || b.Host || b.id || b.ID || 'unknown');
            }

            const total = v.totalGB || 0;
            const used = v.usedGB || 0;
            const prev = byGPU.get(key);
            if (prev) {
                // Две записи об одной карте: берём БОЛЬШЕЕ значение, а не сумму —
                // агенты читают один счётчик и могут разойтись на несколько МБ
                // (разное время опроса). Максимум не занижает и не удваивает.
                deduped = true;
                prev.totalGB = Math.max(prev.totalGB, total);
                prev.usedGB = Math.max(prev.usedGB, used);
            } else {
                byGPU.set(key, { totalGB: total, usedGB: used });
            }
        });

        let totalGB = 0;
        let usedGB = 0;
        byGPU.forEach((v) => {
            totalGB += v.totalGB;
            usedGB += v.usedGB;
        });

        return { totalGB: totalGB, usedGB: usedGB, groups: byGPU.size, deduped: deduped };
    }

    // Экспорт на window: этим же хелпером пользуются ui-renderer.js и любые
    // будущие модули Monitor. Без единой точки правки дедуп разъехался бы по
    // трём местам (в проекте уже были три раздельные суммы по бэкендам).
    if (typeof window !== 'undefined') {
        window.MonitorAggregates = window.MonitorAggregates || {};
        window.MonitorAggregates.aggregateVRAM = aggregateVRAM;
    }

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
        // Дедуп по физической GPU: два бэкенда одной машины видят одну карту
        // (см. aggregateVRAM). Без этого «VRAM» в шапке был удвоенным.
        const vram = aggregateVRAM(backends);
        const totV = vram.totalGB;
        const usdV = vram.usedGB;
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