/**
 * Monitor Charts Module
 * Canvas 2D real-time charts for RPS, latency, GPU/VRAM usage
 * Used by: monitor.html
 * Dependencies: MonitorApp (MA), monitor-metrics.js
 */

const MonitorCharts = (() => {
    'use strict';

    // ===== Chart Data Buffers =====
    const MAX_POINTS = 120; // 2 minutes at 1s interval
    const chartData = {
        rps: { labels: [], values: [], color: '#3b82f6', label: 'RPS' },
        latency: { labels: [], values: [], color: '#a855f7', label: 'Latency (ms)' },
        gpu: { labels: [], values: [], color: '#22c55e', label: 'GPU %' },
        vram: { labels: [], values: [], color: '#f59e0b', label: 'VRAM %' }
    };

    // ===== Canvas Setup =====
    let canvas = null;
    let ctx = null;
    let animationId = null;

    function init(containerId) {
        const container = document.getElementById(containerId);
        if (!container) {
            console.warn('[MonitorCharts] Container not found:', containerId);
            return false;
        }

        // Create canvas if not exists
        canvas = container.querySelector('canvas');
        if (!canvas) {
            canvas = document.createElement('canvas');
            canvas.style.width = '100%';
            canvas.style.height = '100%';
            canvas.style.display = 'block';
            container.appendChild(canvas);
        }

        ctx = canvas.getContext('2d');
        resize();
        window.addEventListener('resize', resize);
        return true;
    }

    function resize() {
        if (!canvas || !canvas.parentElement) return;
        const rect = canvas.parentElement.getBoundingClientRect();
        const dpr = window.devicePixelRatio || 1;
        canvas.width = rect.width * dpr;
        canvas.height = rect.height * dpr;
        canvas.style.width = rect.width + 'px';
        canvas.style.height = rect.height + 'px';
        if (ctx) ctx.scale(dpr, dpr);
    }

    // ===== Data Management =====
    function pushData(series, value) {
        const now = new Date();
        const timeLabel = now.getHours().toString().padStart(2, '0') + ':' +
                         now.getMinutes().toString().padStart(2, '0') + ':' +
                         now.getSeconds().toString().padStart(2, '0');

        chartData[series].values.push(value);
        chartData[series].labels.push(timeLabel);

        if (chartData[series].values.length > MAX_POINTS) {
            chartData[series].values.shift();
            chartData[series].labels.shift();
        }
    }

    function updateFromMetrics(metrics) {
        if (!metrics) return;

        const rps = metrics.rps || 0;
        // Latency: среднее время ответа по всем бэкендам (avgResponseTime в ms).
        // Раньше был несуществующий metrics.avg_wait_time_ms → график был 0.
        const backends = metrics.backends || [];
        let latency = 0, latCount = 0;
        backends.forEach(b => {
            const ollama = b.ollama || {};
            if (ollama.avgResponseTime != null && ollama.avgResponseTime > 0) {
                latency += ollama.avgResponseTime;
                latCount++;
            }
        });
        if (latCount > 0) latency /= latCount;

        pushData('rps', rps);
        pushData('latency', latency);

        // GPU/VRAM averages across backends.
        // Поля: b.gpu.usagePercent, b.vramUsagePercent (вычислено в GetClusterState).
        // Раньше читал несуществующее b.vram.usagePercent → график был 0.
        let gpuAvg = 0, vramAvg = 0, count = 0;
        backends.forEach(b => {
            if (b.gpu && b.gpu.usagePercent != null) {
                gpuAvg += b.gpu.usagePercent;
                count++;
            }
            if (b.vramUsagePercent != null) {
                vramAvg += b.vramUsagePercent;
            }
        });
        if (count > 0) {
            gpuAvg /= count;
            vramAvg /= count;
        }
        pushData('gpu', gpuAvg);
        pushData('vram', vramAvg);
    }

    // ===== Drawing =====
    function draw() {
        if (!ctx || !canvas) return;

        const width = canvas.width / (window.devicePixelRatio || 1);
        const height = canvas.height / (window.devicePixelRatio || 1);
        const dpr = window.devicePixelRatio || 1;

        ctx.clearRect(0, 0, width, height);

        // Background grid
        drawGrid(width, height);

        // Draw each active chart series
        const activeSeries = ['rps', 'latency', 'gpu', 'vram'];
        const layout = calculateLayout(width, height, activeSeries.length);

        activeSeries.forEach((key, index) => {
            const rect = layout[index];
            drawSeries(key, rect.x, rect.y, rect.w, rect.h);
        });
    }

    function calculateLayout(width, height, count) {
        // 2x2 grid
        const cols = 2;
        const rows = 2;
        const gap = 8;
        const cellW = (width - gap * (cols + 1)) / cols;
        const cellH = (height - gap * (rows + 1)) / rows;

        return [
            { x: gap, y: gap, w: cellW, h: cellH },
            { x: gap * 2 + cellW, y: gap, w: cellW, h: cellH },
            { x: gap, y: gap * 2 + cellH, w: cellW, h: cellH },
            { x: gap * 2 + cellW, y: gap * 2 + cellH, w: cellW, h: cellH }
        ];
    }

    function drawGrid(width, height) {
        ctx.strokeStyle = getComputedStyle(document.documentElement).getPropertyValue('--border-color') || 'rgba(255,255,255,0.1)';
        ctx.lineWidth = 0.5;
        ctx.setLineDash([2, 4]);

        // Horizontal lines
        for (let i = 1; i < 5; i++) {
            const y = height * i / 5;
            ctx.beginPath();
            ctx.moveTo(0, y);
            ctx.lineTo(width, y);
            ctx.stroke();
        }

        ctx.setLineDash([]);
    }

    function drawSeries(key, x, y, w, h) {
        const data = chartData[key];
        if (!data.values.length) return;

        const padding = { top: 20, right: 10, bottom: 20, left: 40 };
        const chartW = w - padding.left - padding.right;
        const chartH = h - padding.top - padding.bottom;

        // Background
        ctx.fillStyle = getComputedStyle(document.documentElement).getPropertyValue('--bg-secondary') || 'rgba(255,255,255,0.03)';
        ctx.fillRect(x, y, w, h);

        // Title
        const isLight = document.documentElement.getAttribute('data-theme') === 'light';
        ctx.fillStyle = isLight ? 'rgba(0,0,0,0.6)' : 'rgba(255,255,255,0.6)';
        ctx.font = 'bold 11px system-ui, sans-serif';
        ctx.textAlign = 'left';
        ctx.fillText(data.label, x + padding.left, y + 14);

        // Current value
        const current = data.values[data.values.length - 1] || 0;
        ctx.fillStyle = data.color;
        ctx.font = 'bold 13px system-ui, sans-serif';
        ctx.textAlign = 'right';
        ctx.fillText(current.toFixed(1), x + w - padding.right, y + 14);

        // Find min/max for scaling
        let minVal = Math.min(...data.values);
        let maxVal = Math.max(...data.values);
        if (maxVal === minVal) { maxVal += 1; minVal -= 1; }
        const range = maxVal - minVal || 1;

        // Draw line
        ctx.strokeStyle = data.color;
        ctx.lineWidth = 2;
        ctx.lineJoin = 'round';
        ctx.beginPath();

        data.values.forEach((val, i) => {
            const px = x + padding.left + (i / (MAX_POINTS - 1)) * chartW;
            const py = y + padding.top + chartH - ((val - minVal) / range) * chartH;
            if (i === 0) ctx.moveTo(px, py);
            else ctx.lineTo(px, py);
        });
        ctx.stroke();

        // Fill area under line
        ctx.fillStyle = data.color + '20'; // 12% opacity
        ctx.lineTo(x + padding.left + chartW, y + padding.top + chartH);
        ctx.lineTo(x + padding.left, y + padding.top + chartH);
        ctx.closePath();
        ctx.fill();

        // Y-axis labels
        ctx.fillStyle = isLight ? 'rgba(0,0,0,0.4)' : 'rgba(255,255,255,0.3)';
        ctx.font = '9px monospace';
        ctx.textAlign = 'right';
        ctx.fillText(maxVal.toFixed(0), x + padding.left - 4, y + padding.top + 8);
        ctx.fillText(minVal.toFixed(0), x + padding.left - 4, y + padding.top + chartH);

        // X-axis labels (first and last)
        if (data.labels.length >= 2) {
            ctx.textAlign = 'left';
            ctx.fillText(data.labels[0], x + padding.left, y + h - 4);
            ctx.textAlign = 'right';
            ctx.fillText(data.labels[data.labels.length - 1], x + w - padding.right, y + h - 4);
        }
    }

    // ===== Animation Loop =====
    function startAnimation() {
        if (animationId) return;
        (function loop() {
            draw();
            animationId = requestAnimationFrame(loop);
        })();
    }

    function stopAnimation() {
        if (animationId) {
            cancelAnimationFrame(animationId);
            animationId = null;
        }
    }

    // ===== Public API =====
    return {
        init,
        resize,
        pushData,
        updateFromMetrics,
        draw,
        startAnimation,
        stopAnimation
    };
})();