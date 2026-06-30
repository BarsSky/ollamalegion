// OllamaLegion — live sparkline renderer для Monitor.
//
// Ring buffer `window.metricsHistory[backendId]` хранит до 200 точек (≈16 мин
// при 5-секундном интервале). Каждая точка: { t, gpu, vram, cpu, ram, rps, avgRt }.
//
// Поллер — отдельный, раз в 5 сек (`startMetricsHistoryPoller`), независим от
// основного `MA.refreshInterval` (2 сек), чтобы не нагружать UI при длинных
// исторических диапазонах.
//
// При перезагрузке страницы буфер сбрасывается (это by design — тренды
// не переживают reload, в отличие от системных метрик в WebUI).
//
// Sparkline визуализирует мини-тренд (60×24 SVG): area + path, label =
// последнее значение, tooltip = "{metric}: min {min}% / max {max}% /
// current {current}%". При < 2 точек рендерится empty-state.
//
// Использование:
//   window.Sparkline.recordMetricsHistory(backendId, { gpu, vram, cpu, ram, rps, avgRt });
//   const html = window.Sparkline.render(backendId, 'gpu', 'var(--accent)');

(function() {
    'use strict';

    // ─── Storage ─────────────────────────────────────────────────────────
    // Per-backend ring buffer. Окно — 200 точек (≈16 мин @ 5s).
    // Глобально для простоты cross-component доступа; reset при reload страницы.
    if (!window.metricsHistory) {
        window.metricsHistory = {};
    }
    if (!window.sparklinePollerStarted) {
        window.sparklinePollerStarted = false;
    }

    var HISTORY_CAP = 200;        // максимум точек в буфере
    var POLL_INTERVAL_MS = 5000;  // 5 сек — независимо от основного MA.refreshInterval

    // ─── History record ──────────────────────────────────────────────────
    // Принимает плоский объект метрик. Неизвестные поля → 0.
    // Возвращает длину буфера (для тестов).
    function recordMetricsHistory(backendId, metrics) {
        if (!backendId) return 0;
        if (!window.metricsHistory[backendId]) {
            window.metricsHistory[backendId] = [];
        }
        var buf = window.metricsHistory[backendId];
        buf.push({
            t:    Date.now(),
            gpu:  num(metrics && metrics.gpu),
            vram: num(metrics && metrics.vram),
            cpu:  num(metrics && metrics.cpu),
            ram:  num(metrics && metrics.ram),
            rps:  num(metrics && metrics.rps),
            avgRt: num(metrics && metrics.avgRt)
        });
        // Trim до cap (FIFO).
        while (buf.length > HISTORY_CAP) {
            buf.shift();
        }
        return buf.length;
    }

    function num(v) {
        if (v === null || v === undefined || isNaN(v)) return 0;
        return Number(v) || 0;
    }

    // ─── Renderer ────────────────────────────────────────────────────────
    // Возвращает HTML-строку (60×24 SVG + label).
    // При < 2 точек — empty-state «Метрики ещё собираются…» (Q5=C).
    // metricKey ∈ { 'gpu', 'vram', 'cpu', 'ram', 'rps', 'avgRt' }.
    // color — CSS-цвет (HEX, var(...), rgba(...)).
    function render(backendId, metricKey, color) {
        var history = (window.metricsHistory || {})[backendId] || [];
        if (history.length < 2) {
            return renderEmpty();
        }
        var values = history.slice(-20).map(function(p) { return p[metricKey] || 0; });
        var min = Math.min.apply(null, values);
        var max = Math.max.apply(null, values);
        var cur = values[values.length - 1];
        var range = max - min || 1;
        var width = 60, height = 24;
        var points = values.map(function(v, i) {
            var x = (i / (values.length - 1)) * width;
            var y = height - ((v - min) / range) * (height - 4) - 2;
            return x.toFixed(1) + ',' + y.toFixed(1);
        });
        var linePath = 'M' + points[0] +
            points.slice(1).map(function(p) { return 'L' + p; }).join(' ');
        var areaPath = 'M0,' + height + ' L' + points[0] +
            points.slice(1).map(function(p) { return 'L' + p; }).join(' ') +
            ' L' + width + ',' + height + ' Z';
        // Tooltip: "{metric}: min {min}% / max {max}% / current {current}%" (Q4=C)
        var metricLabel = metricLabelFor(metricKey);
        var tooltip = metricLabel + ': min ' + min.toFixed(1) + '% / max ' +
            max.toFixed(1) + '% / current ' + cur.toFixed(1) + '%';
        return '' +
            '<div class="sparkline-container" title="' + esc(tooltip) + '">' +
                '<svg class="sparkline-svg" viewBox="0 0 ' + width + ' ' + height + '" preserveAspectRatio="none">' +
                    '<path class="sparkline-area" d="' + areaPath + '" fill="' + esc(color) + '"></path>' +
                    '<path class="sparkline-path" d="' + linePath + '" stroke="' + esc(color) + '"></path>' +
                '</svg>' +
                '<span class="sparkline-label">' + cur.toFixed(1) + '%</span>' +
            '</div>';
    }


    // ─── Cluster render (для renderClusterResources) ─────────────────────
    // Cluster sparkline не имеет собственной истории — он показывает
    // моментальный снимок avg/max по кластеру. Используется в renderClusterResources
    // для каждой из 4 метрик (gpu/vram/cpu/ram).
    function renderCluster(metricKey, avgVal, maxVal, color) {
        var a = num(avgVal);
        var x = num(maxVal);
        var label = metricLabelFor(metricKey);
        var ratio = Math.max(0, Math.min(100, a));
        var tip = label + ': avg ' + a.toFixed(0) + '% / max ' + x.toFixed(0) + '%';
        var yTop = (24 - (24 * ratio / 100)).toFixed(2);
        var hBar = (24 * ratio / 100).toFixed(2);
        return '' +
            '<div class="sparkline-container sparkline-cluster" title="' + esc(tip) + '" style="width:60px;height:24px;display:inline-block;position:relative">' +
                '<svg class="sparkline-svg" viewBox="0 0 60 24" preserveAspectRatio="none" width="60" height="24" style="display:block">' +
                    '<rect class="sparkline-area" x="0" y="' + yTop + '" width="60" height="' + hBar + '" fill="' + esc(color) + '" opacity="0.25"/>' +
                '</svg>' +
                '<span class="sparkline-label" style="position:absolute;top:0;left:0;width:100%;height:100%;display:flex;align-items:center;justify-content:center;font-size:9px;font-weight:600;color:' + esc(color) + '">' + a.toFixed(0) + '%</span>' +
            '</div>';
    }

    function renderEmpty() {
        // Q5=C: empty-state «Метрики ещё собираются…» (i18n через window.I18N если есть)
        var empty = (window.I18N && window.I18N.t)
            ? window.I18N.t('monitor.sparkline.empty')
            : 'Метрики ещё собираются…';
        return '<div class="sparkline-container sparkline-empty" title="' + esc(empty) + '">' +
            '<span class="sparkline-label sparkline-label-empty">—</span>' +
            '</div>';
    }

    function metricLabelFor(key) {
        if (window.I18N && window.I18N.t) {
            // Reuse existing metric labels (если есть) — fallback на статику.
            var map = {
                'gpu': 'metrics.gpuUtil',
                'vram': 'metrics.vramUsed',
                'cpu': 'metrics.cpuUsed',
                'ram': 'metrics.ramUsed',
                'rps': 'metrics.rps',
                'avgRt': 'metrics.avgRt'
            };
            if (map[key]) return window.I18N.t(map[key]);
        }
        var fallback = {
            'gpu': 'GPU', 'vram': 'VRAM', 'cpu': 'CPU', 'ram': 'RAM',
            'rps': 'RPS', 'avgRt': 'Avg RT'
        };
        return fallback[key] || key;
    }

    function esc(s) {
        // HTML-entity encoding: строим карту из кода → литерал (избегаем
        // проблем с авто-substitution в replace_in_file, который съедает
        // "&"-подобные последовательности при записи).
        var AMP = String.fromCharCode(38) + 'amp;';
        var LT  = String.fromCharCode(38) + 'lt;';
        var GT  = String.fromCharCode(38) + 'gt;';
        var QUOT = String.fromCharCode(38) + 'quot;';
        return String(s)
            .replace(/&/g, AMP)
            .replace(/</g, LT)
            .replace(/>/g, GT)
            .replace(/"/g, QUOT);
    }

    // ─── Poller ──────────────────────────────────────────────────────────
    // Q6=C: отдельный poller, раз в 5 сек, независим от основного цикла.
    // Подписывается на window.fetchAllSafe через polling data: каждый вызов
    // берёт текущее состояние бэкендов и пушит в буфер.
    // NB: для простоты используем публичный window.MA (Monitor App) если есть.
    function startPoller() {
        if (window.sparklinePollerStarted) return;
        window.sparklinePollerStarted = true;
        setInterval(function() {
            // Берём актуальные бэкенды из Monitor state.
            var backends = (window.MA && window.MA.lastBackends) || [];
            if (!Array.isArray(backends) || backends.length === 0) return;
            for (var i = 0; i < backends.length; i++) {
                var b = backends[i];
                if (!b || !b.id) continue;
                // Извлекаем метрики из текущей структуры (см. ui-renderer.js:142-161).
                var gpu = 0, vram = 0, cpu = 0, ram = 0, rps = 0, avgRt = 0;
                if (b.gpu && typeof b.gpu.usagePercent === 'number') gpu = b.gpu.usagePercent;
                if (b.vram && typeof b.vram.usagePercent === 'number') vram = b.vram.usagePercent;
                if (b.system) {
                    if (typeof b.system.cpuUsagePercent === 'number') cpu = b.system.cpuUsagePercent;
                    if (typeof b.system.memoryUsagePercent === 'number') ram = b.system.memoryUsagePercent;
                }
                if (typeof b.memoryUsagePercent === 'number') ram = b.memoryUsagePercent;
                if (typeof b.rps === 'number') rps = b.rps;
                if (typeof b.avgResponseTimeMs === 'number') avgRt = b.avgResponseTimeMs;
                recordMetricsHistory(b.id, {
                    gpu: gpu, vram: vram, cpu: cpu, ram: ram, rps: rps, avgRt: avgRt
                });
            }
        }, POLL_INTERVAL_MS);
    }

    // ─── Reset (для тестов и для reset-кнопки в UI, если добавим) ────────
    function reset(backendId) {
        if (backendId) {
            delete window.metricsHistory[backendId];
        } else {
            window.metricsHistory = {};
        }
    }

    // ─── Export ──────────────────────────────────────────────────────────
    window.Sparkline = {
        recordMetricsHistory: recordMetricsHistory,
        render: render,
        renderCluster: renderCluster,
        startPoller: startPoller,
        reset: reset,
        // Константы для тестов.
        _HISTORY_CAP: HISTORY_CAP,
        _POLL_INTERVAL_MS: POLL_INTERVAL_MS
    };
})();