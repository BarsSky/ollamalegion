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
    // Сколько точек РИСУЕМ. Буфер хранит 200 (≈16 мин), но на 60 пикселях ширины
    // больше 60 точек — уже шум; 60 точек = 5 мин при 5-секундном опросе.
    var WINDOW_POINTS = 60;

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

    // ─── Шкала и единицы (R91, 2026-10-08) ───────────────────────────────
    //
    // ПОЧЕМУ ЭТО ГЛАВНАЯ ПРАВКА. Раньше шкала была min..max ПО ОКНУ: любое
    // изменение рисовалось во всю высоту. Живой замер (дамп DOM Monitor):
    // «RAM занято: min 13.3% / max 13.3%» и «GPU: min 40.0% / max 40.0%» —
    // плоские серии, а стоило значению чуть измениться, как на 60×24 появлялась
    // «гора»: по картинке нельзя было отличить 2 % от 90 % и понять, где предел.
    // Теперь у процентов ФИКСИРОВАННАЯ шкала 0..100 %, поэтому высота графика —
    // это доля от максимума, и плитки сравнимы между собой.
    var PCT_KEYS = { gpu: 1, vram: 1, cpu: 1, ram: 1 };

    // unitFor — единица метрики. Раньше подпись всегда добавляла '%', поэтому
    // RPS показывался как «12.0%», а среднее время ответа — как «3450.0%».
    function unitFor(key) {
        if (PCT_KEYS[key]) return '%';
        if (key === 'avgRt') return 'ms';
        return '';
    }

    function fmtVal(v, key) {
        var n = num(v);
        if (key === 'avgRt') return Math.round(n) + unitFor(key);
        return n.toFixed(1) + unitFor(key);
    }

    // fmtShort — то же число без десятых: строка сводки живёт в узкой ячейке
    // таблицы (60 px графика + подпись) и «13.0%» там не помещается.
    function fmtShort(v, key) {
        var n = num(v);
        // Мельче 1 % показываем как «<1%», а не как «0%»: ноль и «почти ноль» —
        // разные вещи, если смотреть на загрузку.
        if (PCT_KEYS[key] && n > 0 && n < 1) return '<1' + unitFor(key);
        return Math.round(n) + unitFor(key);
    }

    // domain — верхняя граница шкалы. Проценты — всегда 100 (см. выше);
    // RPS и время ответа не ограничены сверху, поэтому 0..max с запасом.
    function domain(key, max) {
        if (PCT_KEYS[key]) return 100;
        return Math.max(num(max) * 1.15, 1);
    }

    // windowLabel — сколько времени покрывают нарисованные точки.
    //
    // Считаем по РЕАЛЬНЫМ отметкам времени, а не по числу точек: буфер наполняют
    // ДВА источника — отдельный поллер (5 с) и рендер таблицы (2 с), поэтому
    // «точек × 5 с» давало бы неправду в разы. points нужен только как запасной
    // вариант, когда отметок времени нет вовсе (тесты, старые записи буфера).
    function windowLabel(firstTs, lastTs, points) {
        var first = num(firstTs), last = num(lastTs);
        var sec;
        if (first > 0 && last > 0) {
            // Отметки есть. last === first (все точки в одну миллисекунду) —
            // это «меньше секунды», а НЕ повод считать метку времени за количество.
            sec = Math.max(0, Math.round((last - first) / 1000));
        } else {
            sec = Math.round(Math.max(0, num(points) - 1) * POLL_INTERVAL_MS / 1000);
        }
        if (sec < 1) return '<1 s';
        if (sec < 90) return sec + ' s';
        return Math.round(sec / 60) + ' min';
    }

    // ─── Renderer ────────────────────────────────────────────────────────
    // Возвращает HTML-строку (60×24 SVG + подпись + строка сводки).
    // При < 2 точек — empty-state «Метрики ещё собираются…» (Q5=C).
    // metricKey ∈ { 'gpu', 'vram', 'cpu', 'ram', 'rps', 'avgRt' }.
    // color — CSS-цвет (HEX, var(...), rgba(...)).
    function render(backendId, metricKey, color) {
        var history = (window.metricsHistory || {})[backendId] || [];
        if (history.length < 2) {
            return renderEmpty();
        }
        var values = history.slice(-WINDOW_POINTS).map(function(p) { return p[metricKey] || 0; });
        var n = values.length;
        var firstTs = history[Math.max(0, history.length - WINDOW_POINTS)].t;
        var lastTs = history[history.length - 1].t;
        var min = Math.min.apply(null, values);
        var max = Math.max.apply(null, values);
        var sum = values.reduce(function(a, b) { return a + b; }, 0);
        var avg = sum / n;
        var cur = values[n - 1];
        var top = domain(metricKey, max);

        var W = 60, H = 24, padY = 3;
        function yOf(v) {
            var clamped = Math.max(0, Math.min(top, num(v)));
            return H - padY - (clamped / top) * (H - 2 * padY);
        }
        var pts = values.map(function(v, i) {
            return (i / (n - 1) * W).toFixed(1) + ',' + yOf(v).toFixed(1);
        });
        var linePath = 'M' + pts.join('L');
        // Плоский ноль (простой) — БЕЗ заливки: раньше под нулевой линией рисовался
        // тёмный «горб» на всю ширину, и простаивающий бэкенд выглядел нагруженным.
        var flatZero = max <= 0;
        var areaPath = flatZero ? '' :
            'M0,' + H + ' L' + pts.join(' L') + ' L' + W + ',' + H + ' Z';

        // Сетка 50 % — единственная опорная линия: даёт глазу «половину шкалы»,
        // не превращая 60×24 в клетчатую бумагу (сетка 25/50/75 на такой площади
        // читалась как шум, проверено скриншотом).
        var grid = '';
        if (PCT_KEYS[metricKey]) {
            grid += '<line class="sparkline-grid" x1="0" y1="' + yOf(50).toFixed(1) +
                '" x2="' + W + '" y2="' + yOf(50).toFixed(1) + '"></line>';
        }
        // Риска максимума за окно: видно, где был пик, без чтения цифр.
        var maxTick = '';
        if (max > 0 && max < top * 0.98) {
            maxTick = '<line class="sparkline-max" x1="0" y1="' + yOf(max).toFixed(1) +
                '" x2="' + W + '" y2="' + yOf(max).toFixed(1) + '"></line>';
        }

        var metricLabel = metricLabelFor(metricKey);
        var win = windowLabel(firstTs, lastTs, n);
        var tooltip = metricLabel + ': min ' + fmtVal(min, metricKey) +
            ' / avg ' + fmtVal(avg, metricKey) +
            ' / max ' + fmtVal(max, metricKey) +
            ' / current ' + fmtVal(cur, metricKey) + ' (' + n + ' точек, ' + win + ')';
        var caption = sparklineCaption(metricKey, max);

        return '' +
            '<div class="sparkline-container" title="' + esc(tooltip) + '">' +
                '<svg class="sparkline-svg" viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none">' +
                    grid +
                    (areaPath ? '<path class="sparkline-area" d="' + areaPath + '" fill="' + esc(color) + '"></path>' : '') +
                    maxTick +
                    '<path class="sparkline-path" d="' + linePath + '" stroke="' + esc(color) + '"></path>' +
                '</svg>' +
                '<span class="sparkline-label">' + fmtVal(cur, metricKey) + '</span>' +
                (caption ? '<span class="sparkline-caption">' + esc(caption) + '</span>' : '') +
            '</div>';
    }

    // sparklineCaption — «макс 13%».
    //
    // Берём МАКСИМУМ (а не среднее): по загрузке важно, до чего доходило; полные
    // min/avg/max и окно наблюдения остаются в title. Строка короткая намеренно —
    // ячейка таблицы узкая (60 px графика + 36 px подпись), и вариант
    // «ср 20.0% · макс 30.0% · 5 min» вылезал в соседнюю колонку (проверено
    // скриншотом). При нулевом максимуме строки нет вовсе: сообщать «макс 0%»
    // про простаивающий бэкенд — шум.
    function sparklineCaption(metricKey, max) {
        if (num(max) <= 0) return '';
        var t = function(key, fallback, vars) {
            if (window.I18N && window.I18N.t) {
                var s = window.I18N.t(key, vars);
                if (s && s !== key) return s;
            }
            return fallback;
        };
        return t('monitor.sparkline.caption', 'max {max}', {
            max: fmtShort(max, metricKey)
        });
    }


    // ─── Cluster render (для renderClusterResources) ─────────────────────
    // Cluster sparkline не имеет собственной истории — он показывает
    // моментальный снимок avg/max по кластеру. Используется в renderClusterResources
    // для каждой из 4 метрик (gpu/vram/cpu/ram).
    //
    // R91 (2026-10-08): рисуем не только «сколько сейчас» (одна заливка на всю
    // ширину читалась как тёмное пятно), но и МАКСИМУМ по кластеру за окно —
    // светлой риской. Иначе карточка «avg 0% / max 0%» и «avg 40% / max 67%»
    // выглядели одинаково: однотонная плашка.
    function renderCluster(metricKey, avgVal, maxVal, color) {
        var a = Math.max(0, Math.min(100, num(avgVal)));
        var x = Math.max(0, Math.min(100, num(maxVal)));
        var label = metricLabelFor(metricKey);
        var tip = label + ': avg ' + a.toFixed(0) + '% / max ' + x.toFixed(0) + '%';
        var yTop = (24 - (24 * a / 100)).toFixed(2);
        var hBar = (24 * a / 100).toFixed(2);
        var yMax = (24 - (24 * x / 100)).toFixed(2);
        return '' +
            '<div class="sparkline-container sparkline-cluster" title="' + esc(tip) + '" style="width:60px;height:24px;display:inline-block;position:relative">' +
                '<svg class="sparkline-svg" viewBox="0 0 60 24" preserveAspectRatio="none" width="60" height="24" style="display:block">' +
                    '<rect class="sparkline-area" x="0" y="' + yTop + '" width="60" height="' + hBar + '" fill="' + esc(color) + '" opacity="0.25"/>' +
                    (x > a ? '<line class="sparkline-max" x1="0" y1="' + yMax + '" x2="60" y2="' + yMax + '" stroke="' + esc(color) + '"></line>' : '') +
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
        // Чистые помощники — для юнит-тестов (webui/js/monitor/sparkline.test.js).
        pure: {
            unitFor: unitFor,
            fmtVal: fmtVal,
            fmtShort: fmtShort,
            domain: domain,
            windowLabel: windowLabel,
            metricLabelFor: metricLabelFor
        },
        // Константы для тестов.
        _HISTORY_CAP: HISTORY_CAP,
        _POLL_INTERVAL_MS: POLL_INTERVAL_MS,
        _WINDOW_POINTS: WINDOW_POINTS
    };
})();