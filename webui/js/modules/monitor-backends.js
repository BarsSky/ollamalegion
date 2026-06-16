/**
 * Monitor Backends Module
 * Backend table rendering with sorting and filtering
 * Used by: monitor.html
 * Dependencies: MonitorApp (MA)
 */

const MonitorBackends = (() => {
    'use strict';

    const MA = window.MonitorApp || {};
    let currentSort = { column: 'id', asc: true };
    let currentFilter = '';

    // ===== Sorting =====
    function sort(backends, column, asc) {
        currentSort = { column, asc };
        return [...backends].sort((a, b) => {
            let va, vb;
            switch (column) {
                case 'id': va = a.id || ''; vb = b.id || ''; break;
                case 'status': va = a.status || ''; vb = b.status || ''; break;
                case 'gpu': va = (a.gpu && a.gpu.usagePercent) || 0; vb = (b.gpu && b.gpu.usagePercent) || 0; break;
                case 'vram': va = (a.vram && a.vram.usagePercent) || 0; vb = (b.vram && b.vram.usagePercent) || 0; break;
                case 'cpu': va = (a.system && a.system.cpuUsagePercent) || 0; vb = (b.system && b.system.cpuUsagePercent) || 0; break;
                case 'ram': va = a.memoryUsagePercent || 0; vb = b.memoryUsagePercent || 0; break;
                case 'active': va = a.activeRequests || 0; vb = b.activeRequests || 0; break;
                case 'rps': va = (a.ollama && a.ollama.requestsPerSecond) || 0; vb = (b.ollama && b.ollama.requestsPerSecond) || 0; break;
                case 'score': va = a.score || 0; vb = b.score || 0; break;
                case 'uptime': va = a.lastSeen || ''; vb = b.lastSeen || ''; break;
                default: va = a.id || ''; vb = b.id || '';
            }

            if (typeof va === 'string') {
                return asc ? va.localeCompare(vb) : vb.localeCompare(va);
            }
            return asc ? va - vb : vb - va;
        });
    }

    // ===== Filtering =====
    function filter(backends, query) {
        currentFilter = query.toLowerCase();
        if (!currentFilter) return backends;
        return backends.filter(b => {
            const id = (b.id || '').toLowerCase();
            const status = (b.status || '').toLowerCase();
            const models = (b.models || []).join(' ').toLowerCase();
            return id.includes(currentFilter) || status.includes(currentFilter) || models.includes(currentFilter);
        });
    }

    // ===== Table Rendering =====
    function renderTable(backends) {
        const filtered = filter(backends, currentFilter);
        const sorted = sort(filtered, currentSort.column, currentSort.asc);
        const tbody = document.querySelector('#backendsTable tbody');
        if (!tbody) return;

        if (!sorted.length) {
            var noDataText = (typeof window !== 'undefined' && window.I18N) ? window.I18N.t('monitor.common.noData') : 'Нет данных';
            // colspan = 13 (12 + новая колонка Loading). Если в таблице другой # столбцов,
            // используем безопасное значение 13.
            tbody.innerHTML = '<tr><td colspan="13" style="color:var(--text-secondary);text-align:center;padding:16px">' + noDataText + '</td></tr>';
            return;
        }

        tbody.innerHTML = sorted.map(b => renderRow(b)).join('');
        bindSortHeaders();
    }

    function renderRow(b) {
        const mr = b.maxConcurrentRequests || 10;
        const a = b.activeRequests || 0;
        const gu = (b.gpu && b.gpu.usagePercent != null) ? b.gpu.usagePercent : 0;
        const vu = (b.vram && b.vram.usagePercent != null) ? b.vram.usagePercent : 0;
        const cu = (b.system && b.system.cpuUsagePercent != null) ? b.system.cpuUsagePercent : 0;
        const ru = (b.memoryUsagePercent != null) ? b.memoryUsagePercent : ((b.system && b.system.memoryUsagePercent != null) ? b.system.memoryUsagePercent : 0);
        const sc = (b.score || b.weight || 0).toFixed(2);
        const scs = b.status === 'healthy' || b.status === 'active' || b.status === 'ready' ? 'badge-green' :
                    (b.status === 'error' || b.status === 'unhealthy' ? 'badge-red' :
                    (b.status === 'ollama_unavailable' ? 'badge-orange' : 'badge-yellow'));
        const up = b.lastSeen ? MA.fmtDur ? MA.fmtDur(Date.now() - new Date(b.lastSeen).getTime()) : '-' : '-';
        const rps = (b.ollama && b.ollama.requestsPerSecond != null) ? b.ollama.requestsPerSecond : 0;

        // ==== Loading column (Issue: «отображение загрузки в мониторе») ====
        // Поддерживается два источника:
        //   1) b.loadingModels: [{name, state, loadingStartedAt, loadingSizeBytes, error}] — массив объектов
        //   2) b.loadingModelCount: число (если API отдаёт только счётчик)
        //   3) b.llamaCppMetrics.loadingModels: альтернативный путь (через metrics-broker)
        const loadingArr = (b.loadingModels && Array.isArray(b.loadingModels) && b.loadingModels.length > 0)
            ? b.loadingModels
            : ((b.llamaCppMetrics && Array.isArray(b.llamaCppMetrics.loadingModels)) ? b.llamaCppMetrics.loadingModels : []);
        const loadingCount = (b.loadingModelCount != null)
            ? b.loadingModelCount
            : loadingArr.length;
        let loadingCell;
        if (loadingCount > 0 && loadingArr.length === 0) {
            // Только счётчик известен
            loadingCell = '<td class="col-right"><span class="badge" title="' +
                (window.I18N ? I18N.t('gguf.col_loading_count', { count: loadingCount }) : ('Loading: ' + loadingCount)) +
                '"><i class="fas fa-spinner fa-spin"></i> ' + loadingCount + '</span></td>';
        } else if (loadingArr.length > 0) {
            // Полный список с elapsed-таймером
            const cells = loadingArr.slice(0, 3).map(function (lm) {
                const startedAt = lm.loadingStartedAt ? new Date(lm.loadingStartedAt).getTime() : Date.now();
                const elapsedMs = (lm.elapsedMs && lm.elapsedMs > 0) ? lm.elapsedMs : (Date.now() - startedAt);
                const elapsedSec = Math.max(0, Math.floor(elapsedMs / 1000));
                const elapsedLabel = elapsedSec < 60
                    ? elapsedSec + 's'
                    : Math.floor(elapsedSec / 60) + 'm ' + (elapsedSec % 60) + 's';
                if (lm.state === 'error') {
                    return '<span class="badge badge-red" title="' + MA.esc(lm.error || 'error') + '">' +
                        '<i class="fas fa-times-circle"></i> ' + MA.esc(lm.name) +
                    '</span>';
                }
                return '<span class="badge" style="background:rgba(74,158,255,0.12);color:#4a9eff;border-color:rgba(74,158,255,0.3)" title="' +
                    (window.I18N ? I18N.t('gguf.loading_indicator') : 'Loading') + ': ' + MA.esc(lm.name) + '">' +
                    '<i class="fas fa-spinner fa-spin"></i> ' + MA.esc(lm.name) + ' ' + elapsedLabel +
                '</span>';
            }).join(' ');
            const more = loadingArr.length > 3 ? (' +' + (loadingArr.length - 3)) : '';
            loadingCell = '<td class="col-right">' + cells + more + '</td>';
        } else {
            loadingCell = '<td class="col-right" style="color:var(--text-secondary)">-</td>';
        }

        // Hidden metrics tooltip data
        const powerLimit = b.gpu && b.gpu.powerLimit ? `Power: ${b.gpu.powerLimit}W` : '';
        const gpuClock = b.gpu && b.gpu.clock ? `Clock: ${b.gpu.gpuClock || b.gpu.clock}MHz` : '';
        const memClock = b.gpu && b.gpu.memClock ? `Mem: ${b.gpu.memClock}MHz` : '';
        const diskInfo = b.system && b.system.diskUsed != null ? `Disk: ${(b.system.diskUsed/1024).toFixed(1)}/${(b.system.diskTotal/1024).toFixed(0)}G` : '';
        const netInfo = b.system && b.system.networkRX != null ? `Net: ↓${formatBytes(b.system.networkRX)}/s ↑${formatBytes(b.system.networkTX)}/s` : '';
        const tooltip = [powerLimit, gpuClock, memClock, diskInfo, netInfo].filter(Boolean).join(' | ');

        return `<tr data-backend="${MA.esc(b.id)}" title="${MA.esc(tooltip)}">
            <td><strong>${MA.esc(b.id)}</strong></td>
            <td><span class="badge ${scs}">${b.status}</span></td>
            <td>${bar(gu)} ${gu.toFixed(0)}%</td>
            <td>${bar(vu)} ${vu.toFixed(0)}%</td>
            <td>${bar(cu)} ${cu.toFixed(0)}%</td>
            <td>${bar(ru)} ${ru.toFixed(0)}%</td>
            <td class="col-right">${a}/${mr}</td>
            <td class="col-right">${rps > 0 ? rps.toFixed(1) : '-'}</td>
            <td class="col-right">${sc}</td>
            <td>${(b.models || []).slice(0, 3).map(m => `<span class="badge" style="background:rgba(168,85,247,0.12);color:var(--purple-accent);border-color:rgba(168,85,247,0.2)">${MA.esc(m)}</span>`).join(' ')}</td>
            <td class="col-right">${up}</td>
            ${loadingCell}
        </tr>`;
    }

    function bar(value) {
        const p = Math.max(0, Math.min(100, value || 0));
        const c = p >= 80 ? 'var(--danger)' : p >= 50 ? 'var(--warning)' : 'var(--success)';
        return `<span class="bar-track"><span class="bar-fill" style="width:${p}%;background:${c}"></span></span>`;
    }

    function formatBytes(bytes) {
        if (!bytes || bytes === 0) return '0 B';
        const k = 1024;
        const sizes = ['B', 'KB', 'MB', 'GB'];
        const i = Math.floor(Math.log(bytes) / Math.log(k));
        return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
    }

    // ===== Sort Header Binding =====
    function bindSortHeaders() {
        document.querySelectorAll('#backendsTable th[data-sort]').forEach(th => {
            th.style.cursor = 'pointer';
            th.onclick = () => {
                const col = th.getAttribute('data-sort');
                const asc = currentSort.column !== col ? true : !currentSort.asc;
                const tbody = document.querySelector('#backendsTable tbody');
                if (!tbody) return;
                const rows = Array.from(tbody.querySelectorAll('tr'));
                const backends = rows.map(tr => {
                    const id = tr.getAttribute('data-backend');
                    return MA.lastData && MA.lastData.cluster && MA.lastData.cluster.backends ?
                        MA.lastData.cluster.backends.find(b => b.id === id) : null;
                }).filter(Boolean);
                renderTable(backends);
            };
        });
    }

    // ===== Filter Input =====
    function setupFilter(inputId) {
        const input = document.getElementById(inputId);
        if (!input) return;
        input.addEventListener('input', (e) => {
            currentFilter = e.target.value;
            if (MA.lastData && MA.lastData.cluster && MA.lastData.cluster.backends) {
                renderTable(MA.lastData.cluster.backends);
            }
        });
    }

    // ===== Public API =====
    return {
        renderTable,
        sort,
        filter,
        setupFilter,
        getCurrentSort: () => currentSort,
        getCurrentFilter: () => currentFilter
    };
})();