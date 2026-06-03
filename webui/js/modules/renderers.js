/**
 * UI Renderers — all HTML generation in one place
 */
const Renderers = (function () {
    const { formatNumber, formatMB, getBackendMode, getBackendModeBadge, getBackendTypeBadge, getGPUStatus, getProgressClass, percent, escapeHtml } = Utils;

    // Cache for prediction hysteresis to prevent flickering between warning/success
    const _predCache = {};
    const PRED_HYSTERESIS = 45; // seconds — must deviate this much from threshold to switch
    const PRED_THRESHOLD = 300; // seconds

    // Stable backend order: new backends appended at the end, removed ones leave
    // gaps that are filled by shifting remaining backends. Ensures visual stability.
    const _backendOrder = new Map(); // id -> { index, addedAt }

    const _t = (k, p) => window.I18N ? window.I18N.t(k, p) : k;

    function stableBackendOrder(backends) {
      const now = Date.now();
      const seen = new Set();
      // Mark existing and assign insertion indices for new backends
      backends.forEach(b => {
        const id = b.id || b.ID;
        if (!id) return;
        seen.add(id);
        if (!_backendOrder.has(id)) {
          _backendOrder.set(id, { index: _backendOrder.size, addedAt: now });
        }
      });
      // Remove backends that no longer exist and recompact indices
      const entries = [];
      for (const [id, meta] of _backendOrder) {
        if (seen.has(id)) {
          entries.push({ id, index: meta.index, addedAt: meta.addedAt });
        }
      }
      entries.sort((a, b) => a.index - b.index);
      _backendOrder.clear();
      entries.forEach((e, i) => {
        _backendOrder.set(e.id, { index: i, addedAt: e.addedAt });
      });
      // Sort backends by their stable index
      return [...backends].sort((a, b) => {
        const ia = _backendOrder.get(a.id || a.ID)?.index ?? Infinity;
        const ib = _backendOrder.get(b.id || b.ID)?.index ?? Infinity;
        return ia - ib;
      });
    }

    // ---- Generic helpers ----

    function badge(status, type) {
        return `<span class="badge badge-${type}">${escapeHtml(status)}</span>`;
    }

    function loading(text) {
        if (!text) text = _t('renderers.loading_data');
        return `<div class="loading">${escapeHtml(text)}</div>`;
    }

    function emptyRow(cols, text) {
        return `<tr><td colspan="${cols}" class="loading-cell">${escapeHtml(text)}</td></tr>`;
    }

    // ---- Dashboard ----

    function dashboard(backends, sessions, queue) {
        const healthy = backends.filter(b => b.status === 'healthy');
        const totalModels = backends.reduce((sum, b) => sum + (b.ollama?.runningModels?.length || 0), 0);
        const activeSessions = (sessions || []).filter(s => s.active).length;
        const totalRequests = (sessions || []).reduce((sum, s) => sum + (s.requestCount || 0), 0);
        const queueSize = queue?.current_size || 0;
        const queueProcessed = queue?.processed_total || 0;
        const queueMax = queue?.max_size || 100;
        const queuePct = percent(queueSize, queueMax);

        Utils.setText('totalBackends', backends.length);
        Utils.setText('healthyBackends', _t('renderers.healthy_count', { count: healthy.length }));
        Utils.setText('totalModels', totalModels);
        Utils.setText('loadedModels', _t('renderers.models_count_loaded', { count: totalModels }));
        Utils.setText('totalSessions', activeSessions);
        Utils.setText('sessionRate', _t('renderers.requests_count', { count: totalRequests }));
        Utils.setText('queueSize', queueSize);
        Utils.setText('queueProcessed', _t('renderers.processed_count', { count: queueProcessed }));
        Utils.setStyle('queueDashboardFill', 'width', `${queuePct}%`);

        Utils.setHTML('runtimeCluster', runtimeCluster(backends));
        Utils.setHTML('gpuCluster', gpuCluster(backends));
        Utils.setHTML('capacitySection', capacitySection(backends));
        Utils.setHTML('availableModelsList', availableModels(backends));
        Utils.setHTML('backendsTableBody', backendsTable(backends));
    }

    // ---- Runtime Cluster ----

    function runtimeCluster(backends) {
        backends = stableBackendOrder(backends);
        if (!backends.length) return loading(_t('renderers.no_backend_data'));
        return backends.map(backend => {
            const flags = backend.ollama?.runtimeFlags || {};
            const contexts = backend.ollama?.modelContexts || [];

            const flagBadges = [];
            if (flags.numGpuLayers !== undefined && flags.numGpuLayers !== 0) {
                const cls = flags.numGpuLayers === -1 ? '' : 'warning';
                flagBadges.push(`<span class="flag-badge ${cls}">GPU:${flags.numGpuLayers === -1 ? 'auto' : flags.numGpuLayers}</span>`);
            }
            if (flags.contextLength) flagBadges.push(`<span class="flag-badge">C:${formatNumber(flags.contextLength)}</span>`);
            if (flags.numParallel && flags.numParallel > 1) flagBadges.push(`<span class="flag-badge warning">NP:${flags.numParallel}</span>`);
            if (flags.numThreads) flagBadges.push(`<span class="flag-badge">T:${flags.numThreads}</span>`);
            if (flags.batchSize && flags.batchSize !== 512) flagBadges.push(`<span class="flag-badge">B:${flags.batchSize}</span>`);
            if (flags.lowVram) flagBadges.push(`<span class="flag-badge danger">LOW_VRAM</span>`);
            if (flags.flashAttention) flagBadges.push(`<span class="flag-badge">FA</span>`);
            if (flags.kvCacheQuant && flags.kvCacheQuant !== 'f16') flagBadges.push(`<span class="flag-badge warning">KV:${flags.kvCacheQuant}</span>`);

            const contextBadges = contexts.map(ctx => `
                <span class="context-info">
                    ${escapeHtml(ctx.name)}: Ctx ${formatNumber(ctx.effectiveContext)} (${ctx.contextSource})
                    <div class="context-tooltip">
                        ${tooltipRow('Context', formatNumber(ctx.contextLength))}
                        ${tooltipRow('Effective', formatNumber(ctx.effectiveContext))}
                        ${tooltipRow('Model Memory', formatMB(ctx.modelMemoryMB))}
                        ${tooltipRow('Context Memory', formatMB(ctx.contextMemoryMB))}
                        ${tooltipRow('KV Cache', formatMB(ctx.kvCacheMemoryMB))}
                        ${tooltipRow('Total', formatMB(ctx.totalMemoryMB))}
                        ${tooltipRow('Layers', ctx.numLayers || '-')}
                        ${tooltipRow('Precision', `${ctx.precisionBits || 16}-bit`)}
                    </div>
                </span>
            `).join('');

            return `
                <div class="runtime-card">
                    <div class="runtime-header">
                        <strong>${escapeHtml(backend.id)}</strong>
                        ${badge(backend.status, backend.status === 'healthy' ? 'success' : 'danger')}
                    </div>
                    <div class="runtime-host">${escapeHtml(backend.host || '-')}</div>
                    <div class="runtime-flags">
                        ${flagBadges.length ? flagBadges.join('') : '<span class="flag-badge">default</span>'}
                    </div>
                    ${contextBadges ? `<div style="margin-top:0.5rem;">${contextBadges}</div>` : ''}
                </div>
            `;
        }).join('');
    }

    function tooltipRow(label, value) {
        return `<div class="context-tooltip-row"><span class="context-tooltip-label">${escapeHtml(label)}</span><span class="context-tooltip-value">${escapeHtml(String(value))}</span></div>`;
    }

    // ---- GPU Cluster ----

    function gpuCluster(backends) {
        backends = stableBackendOrder(backends);
        if (!backends.length) return loading(_t('renderers.no_backend_data'));

        const gpuBackends = backends.filter(b => getBackendMode(b) === 'gpu');
        const cpuBackends = backends.filter(b => getBackendMode(b) === 'cpu');
        const cloudBackends = backends.filter(b => getBackendMode(b) === 'cloud');

        Utils.setHTML('gpuClusterBadge', `${gpuBackends.length} GPU · ${cpuBackends.length} CPU · ${cloudBackends.length} <svg class="badge-icon-svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="currentColor"/></svg>`);

        return backends.map(b => gpuCard(b)).join('');
    }

    function gpuCard(backend) {
        const mode = getBackendMode(backend);
        const pred = backend.prediction || {};

        if (mode === 'cloud') {
            const activeReq = backend.activeRequests || 0;
            const maxReq = backend.maxConcurrentRequests || 10;
            const models = (backend.ollama?.runningModels || []).length;
            const rps = backend.ollama?.requestsPerSecond || 0;
            const reqCap = pred.requestCapacity !== undefined ? pred.requestCapacity.toFixed(0) + '%' : '-';
            return `
                <div class="gpu-card gpu-card-cloud">
                    <div class="gpu-card-header">
                        <span class="gpu-card-title">${escapeHtml(backend.id)}</span>
                        <svg class="badge-icon-svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="currentColor"/></svg>
                    </div>
                    <div class="gpu-metrics">
                        ${metric('Host', escapeHtml(backend.host || '-'))}
                        ${metric('Req', `${activeReq}/${maxReq}`)}
                        ${metric('Models', models)}
                        ${metric('RPS', rps.toFixed(1))}
                        ${metric('Capacity', reqCap)}
                        ${metric('Status', backend.status)}
                    </div>
                    <div class="progress-bar"><div class="progress-fill low" style="width: 0%"></div></div>
                </div>
            `;
        }

        if (mode === 'cpu') {
            const sys = backend.system || {};
            const cpu = sys.cpu || {};
            const cpuUsage = sys.cpuUsagePercent || 0;
            const ramTotal = sys.memoryTotal || 1;
            const ramUsed = sys.memoryUsed || 0;
            const ramPct = percent(ramUsed, ramTotal);
            const temp = cpu.temperature !== undefined ? cpu.temperature : (sys.cpuTemperature !== undefined ? sys.cpuTemperature : '-');
            const loadAvg1 = cpu.loadAverage1 !== undefined ? cpu.loadAverage1.toFixed(2) : (sys.loadAverage1 !== undefined ? sys.loadAverage1.toFixed(2) : '-');
            const loadAvg5 = cpu.loadAverage5 !== undefined ? cpu.loadAverage5.toFixed(2) : '-';
            const loadAvg15 = cpu.loadAverage15 !== undefined ? cpu.loadAverage15.toFixed(2) : '-';
            const cores = cpu.coreCount || '-';
            const model = cpu.model || '';
            const throttled = cpu.throttled !== undefined ? (cpu.throttled ? '⚠️ YES' : 'OK') : '-';
            const reqCap = pred.requestCapacity !== undefined ? pred.requestCapacity.toFixed(0) + '%' : '-';
            return `
                <div class="gpu-card gpu-card-cpu">
                    <div class="gpu-card-header">
                        <span class="gpu-card-title">${escapeHtml(backend.id)}</span>
                        ${badge('CPU', 'warning')}
                    </div>
                    <div class="gpu-metrics">
                        ${metric('Host', escapeHtml(backend.host || '-'))}
                        ${metric('CPU', `${cpuUsage.toFixed(1)}%`)}
                        ${metric('RAM', `${ramPct.toFixed(1)}%`)}
                        ${metric('Load', `${loadAvg1} / ${loadAvg5} / ${loadAvg15}`)}
                        ${metric('Cores', `${cores} ${model ? '(' + model + ')' : ''}`)}
                        ${metric('Temp', `${temp}°C`)}
                        ${metric('Throttle', throttled)}
                        ${metric('Capacity', reqCap)}
                    </div>
                    <div class="progress-bar"><div class="progress-fill ${getProgressClass(cpuUsage)}" style="width: ${cpuUsage}%"></div></div>
                </div>
            `;
        }

        // GPU mode
        const gpu = backend.gpu || {};
        const usage = gpu.usagePercent || 0;
        const vramUsed = gpu.memoryUsed || 0;
        const vramTotal = gpu.memoryTotal || 1;
        const vramPct = percent(vramUsed, vramTotal);
        const temp = gpu.temperature !== undefined ? gpu.temperature : '-';
        const power = gpu.powerUsage !== undefined ? gpu.powerUsage : '-';
        const powerLimit = gpu.powerLimit !== undefined && gpu.powerLimit > 0 ? gpu.powerLimit : '-';
        const gpuClock = gpu.gpuClock !== undefined && gpu.gpuClock > 0 ? gpu.gpuClock : '-';
        const memClock = gpu.memClock !== undefined && gpu.memClock > 0 ? gpu.memClock : '-';
        const status = getGPUStatus(usage, vramPct, temp);
        const reqCap = pred.requestCapacity !== undefined ? pred.requestCapacity.toFixed(0) + '%' : '-';
        return `
            <div class="gpu-card">
                <div class="gpu-card-header">
                    <span class="gpu-card-title">${escapeHtml(backend.id)}</span>
                    <span class="gpu-status ${status}"></span>
                </div>
                <div class="gpu-metrics">
                    ${metric('Host', escapeHtml(backend.host || '-'))}
                    ${metric('GPU', `${usage.toFixed(1)}%`)}
                    ${metric('VRAM', `${vramPct.toFixed(1)}%`)}
                    ${metric('Temp', `${temp}°C`)}
                    ${metric('Power', `${power}W ${powerLimit !== '-' ? '/ ' + powerLimit + 'W' : ''}`)}
                    ${metric('Clock', `${gpuClock !== '-' ? gpuClock + ' MHz' : '-'}`)}
                    ${metric('MemClk', `${memClock !== '-' ? memClock + ' MHz' : '-'}`)}
                    ${metric('Capacity', reqCap)}
                </div>
                <div class="progress-bar"><div class="progress-fill ${getProgressClass(usage)}" style="width: ${usage}%"></div></div>
            </div>
        `;
    }

    function metric(label, value) {
        return `
            <div class="gpu-metric">
                <div class="gpu-metric-label">${escapeHtml(label)}</div>
                <div class="gpu-metric-value">${escapeHtml(String(value))}</div>
            </div>
        `;
    }

    // ---- Capacity Section ----

    function capacitySection(backends) {
        backends = stableBackendOrder(backends);
        if (!backends.length) return loading(_t('renderers.no_backend_data'));
        return backends.map(b => capacityCard(b)).join('');
    }

    function capacityCard(backend) {
        const mode = getBackendMode(backend);
        const cap = backend.ollama?.backendCapacity || {};

        if (mode === 'cloud') {
            return `
                <div class="capacity-card capacity-cloud">
                    <div class="runtime-header">
                        <strong>${escapeHtml(backend.id)}</strong>
                        ${getBackendModeBadge(backend)}
                    </div>
                    <div class="capacity-cloud-info">
                        <div class="capacity-cloud-row"><span>${_t('renderers.host')}</span><span>${escapeHtml(backend.host || '-')}</span></div>
                        <div class="capacity-cloud-row"><span>${_t('metrics.status')}</span><span>${badge(backend.status, backend.status === 'healthy' ? 'success' : 'danger')}</span></div>
                        <div class="capacity-cloud-row"><span>${_t('renderers.active_req')}</span><span>${backend.activeRequests || 0}</span></div>
                        <div class="capacity-cloud-row"><span>${_t('renderers.models_loaded')}</span><span>${(backend.ollama?.runningModels || []).length}</span></div>
                    </div>
                </div>
            `;
        }

        const isCPU = mode === 'cpu';
        const sys = backend.system || {};
        const memTotal = isCPU ? (sys.memoryTotal || 1) : (backend.gpu?.memoryTotal || 1);
        const memUsed = isCPU ? (sys.memoryUsed || 0) : (backend.gpu?.memoryUsed || 0);
        const memFree = isCPU ? (sys.memoryFree || 0) : (backend.gpu?.memoryFree || 0);
        const loadedMem = cap.loadedModelVram || 0;
        const ctxOverhead = cap.contextOverheadMB || 0;
        const guaranteed = cap.guaranteedVram || 0;

        const usedPct = percent(memUsed, memTotal);
        const loadedPct = percent(loadedMem, memTotal);
        const ctxPct = percent(ctxOverhead, memTotal);
        const guarPct = percent(guaranteed, memTotal);
        const memLabel = isCPU ? 'RAM' : 'VRAM';

        return `
            <div class="capacity-card">
                <div class="runtime-header">
                    <strong>${escapeHtml(backend.id)}</strong>
                    ${getBackendModeBadge(backend)}
                </div>
                <div class="capacity-host"><span>${_t('renderers.host')}</span><span>${escapeHtml(backend.host || '-')}</span></div>
                <div class="capacity-bar-container">
                    <div class="capacity-bar-labels">
                        <span>${memLabel}: ${formatMB(memUsed)} / ${formatMB(memTotal)}</span>
                        <span>${usedPct.toFixed(1)}%</span>
                    </div>
                    <div class="capacity-bar">
                        <div class="capacity-bar-filled" style="width: ${loadedPct}%"></div>
                        <div class="capacity-bar-context" style="width: ${ctxPct}%"></div>
                        <div class="capacity-bar-guaranteed" style="width: ${guarPct}%"></div>
                    </div>
                </div>
                <div class="capacity-stats">
                    <div class="capacity-stat"><span>${_t('renderers.loaded_models')}</span><span>${formatMB(loadedMem)}</span></div>
                    <div class="capacity-stat"><span>${_t('renderers.context_overhead')}</span><span>${formatMB(ctxOverhead)}</span></div>
                    <div class="capacity-stat"><span>${_t('renderers.free_memory', { memLabel })}</span><span>${formatMB(memFree)}</span></div>
                    <div class="capacity-stat"><span>${_t('renderers.guaranteed_90')}</span><span>${formatMB(guaranteed)}</span></div>
                </div>
            </div>
        `;
    }

    // ---- Available Models ----

    function availableModels(backends) {
        const badge = document.getElementById('loadableModelCount');
        if (!backends.length) {
            if (badge) badge.textContent = '-';
            return loading(_t('renderers.no_data'));
        }

        let totalLoadable = 0;
        const allModels = [];
        backends.forEach(b => {
            const models = b.ollama?.backendCapacity?.availableModels || [];
            models.forEach(m => {
                allModels.push({ ...m, backendId: b.id, backendStatus: b.status });
                if (m.canLoad) totalLoadable++;
            });
        });

        if (badge) badge.textContent = totalLoadable;

        if (!allModels.length) return loading(_t('renderers.no_available_models'));

        allModels.sort((a, b) => {
            if (a.canLoad !== b.canLoad) return b.canLoad - a.canLoad;
            return (a.estimatedVram || 0) - (b.estimatedVram || 0);
        });

        const items = allModels.slice(0, 50).map(m => {
            const cls = m.canLoad ? 'model-loadable' : 'model-unloadable';
            const vram = m.estimatedVram || 0;
            return `
                <div class="available-model-item ${cls}">
                    <div class="model-indicator"></div>
                    <span class="model-name">${escapeHtml(m.name)}</span>
                    <span class="model-vram">${formatMB(vram)} @ ${escapeHtml(m.backendId)}</span>
                </div>
            `;
        }).join('');

        const extra = allModels.length > 50
            ? `<div style="text-align:center;color:var(--text-muted);padding:0.5rem;font-size:0.8rem;">${_t('renderers.extra_models', { count: allModels.length - 50 })}</div>`
            : '';

        return items + extra;
    }

    // ---- Backends Table (Dashboard) ----

    function backendsTable(backends) {
        backends = stableBackendOrder(backends);
        if (!backends.length) return emptyRow(13, _t('renderers.no_data'));

        return backends.map(b => {
            const mode = getBackendMode(b);
            const gpu = b.gpu || {};
            const sys = b.system || {};
            const cpu = sys.cpu || {};
            const pred = b.prediction || {};
            const oll = b.ollama || {};
            const isCloud = mode === 'cloud';

            const gpuUsage = isCloud ? '-' : (gpu.usagePercent !== undefined ? gpu.usagePercent.toFixed(1) + '%' : '-');
            const vramPercent = isCloud ? '-' : (gpu.memoryTotal > 0 ? percent(gpu.memoryUsed, gpu.memoryTotal).toFixed(1) + '%' : '-');
            const cpuUsage = isCloud ? '-' : (sys.cpuUsagePercent !== undefined ? sys.cpuUsagePercent.toFixed(1) + '%' : '-');
            const ramPercent = isCloud ? '-' : (sys.memoryTotal > 0 ? percent(sys.memoryUsed, sys.memoryTotal).toFixed(1) + '%' : '-');

            // CPU Details
            const coreCount = cpu.coreCount || '-';
            const threadCount = cpu.threadCount || '-';
            const cpuModel = cpu.model ? cpu.model.split(' ').slice(0, 2).join(' ') : '';
            const loadAvg = cpu.loadAverage1 !== undefined ? cpu.loadAverage1.toFixed(2) : (sys.loadAverage1 !== undefined ? sys.loadAverage1.toFixed(2) : '-');
            const cpuTemp = cpu.temperature !== undefined ? cpu.temperature + '°C' : (sys.cpuTemperature !== undefined ? sys.cpuTemperature + '°C' : '-');
            const throttled = cpu.throttled !== undefined ? (cpu.throttled ? '⚠️' : 'OK') : '-';

            // GPU hidden metrics tooltip
            const gpuHidden = !isCloud && (gpu.powerLimit > 0 || gpu.gpuClock > 0 || gpu.memClock > 0)
                ? `<span class="gpu-hidden-hint" title="Limit: ${gpu.powerLimit || '-'}W | GPU: ${gpu.gpuClock || '-'} MHz | Mem: ${gpu.memClock || '-'} MHz">⚡</span>` : '';

            const activeReq = b.activeRequests || 0;
            const maxReq = b.maxConcurrentRequests || 10;
            const models = oll.runningModels?.length || 0;
            // Model details tooltip for the Models column
            // Format expiresAt for display
            function fmtExpires(exp) {
                if (!exp) return '';
                var d = new Date(exp);
                if (isNaN(d.getTime())) return '';
                var now = Date.now();
                var diff = d.getTime() - now;
                if (diff < 0) return ' ⌛Expired';
                if (diff < 60000) return ' ⌛' + Math.round(diff/1000) + 's';
                if (diff < 3600000) return ' ⌛' + Math.round(diff/60000) + 'm';
                return ' ⌛' + Math.round(diff/3600000) + 'h';
            }
            const modelDetailsHtml = (oll.runningModels || []).map(function(m) {
                var expStr = fmtExpires(m.expiresAt || m.ExpiresAt);
                return '<div class="model-tooltip-row">' + Utils.escapeHtml(m.name) + ' — ' +
                    Utils.escapeHtml(m.family || '-') + ' | ' +
                    Utils.escapeHtml(m.parameterSize || '-') + ' | ' +
                    Utils.escapeHtml(m.quantization || '-') +
                    (expStr ? ' ' + expStr : '') +
                    '</div>';
            }).join('');
            const modelDetailsTitle = (oll.runningModels || []).map(function(m) {
                var expStr = fmtExpires(m.expiresAt || m.ExpiresAt);
                return (m.name || '-') + ' — ' + (m.family || '-') + ' | ' + (m.parameterSize || '-') + ' | ' + (m.quantization || '-') + (expStr ? ' ' + expStr : '');
            }).join('\n');

            const rps = oll.requestsPerSecond || 0;
            const avgRT = oll.avgResponseTime !== undefined && oll.avgResponseTime > 0 ? oll.avgResponseTime.toFixed(0) + 'ms' : '-';
            const reqCap = pred.requestCapacity !== undefined ? pred.requestCapacity.toFixed(0) + '%' : '-';
            const secondsToCrit = pred.secondsToCritical || -1;
            const prev = _predCache[b.id];
            let predClass, predText;

            if (secondsToCrit > 0) {
                predText = `${Math.round(secondsToCrit)}с`;
                const isWarning = secondsToCrit < PRED_THRESHOLD;
                if (prev) {
                    if (prev.class === 'warning' && secondsToCrit < PRED_THRESHOLD + PRED_HYSTERESIS) {
                        predClass = 'warning';
                    } else if (prev.class === 'success' && secondsToCrit > PRED_THRESHOLD - PRED_HYSTERESIS) {
                        predClass = 'success';
                    } else {
                        predClass = isWarning ? 'warning' : 'success';
                    }
                } else {
                    predClass = isWarning ? 'warning' : 'success';
                }
            } else {
                predText = 'OK';
                predClass = 'success';
            }
            _predCache[b.id] = { class: predClass, text: predText };

            return `
                <tr>
                    <td><strong>${escapeHtml(b.id)}</strong> ${getBackendTypeBadge(b)} ${getBackendModeBadge(b)}</td>
                    <td>${badge(b.status, b.status === 'healthy' ? 'success' : 'danger')}</td>
                    <td>${escapeHtml(gpuUsage)}${gpuHidden}</td>
                    <td>${escapeHtml(vramPercent)}</td>
                    <td>${escapeHtml(cpuUsage)}</td>
                    <td>${escapeHtml(ramPercent)}</td>
                    <td class="col-right">${activeReq}/${maxReq}</td>
                    <td class="col-right" title="${modelDetailsTitle ? Utils.escapeHtml(modelDetailsTitle) : ''}">${models}</td>
                    <td class="col-right">${rps.toFixed(1)}</td>
                    <td class="col-right">${avgRT}</td>
                    <td class="col-right">${reqCap}</td>
                    <td>${badge(predText, predClass)}</td>
                    <td>
                        <button class="action-btn edit" onclick="ui.editBackend('${escapeHtml(b.id)}')">${_t('common.edit')}</button>
                        <button class="action-btn delete" onclick="ui.confirmDeleteBackend('${escapeHtml(b.id)}')">${_t('common.delete')}</button>
                    </td>
                </tr>
            `;
        }).join('');
    }

    // ---- Backends Management Page ----

    // Ollama parameter reference with descriptions (technical - kept as-is)
    const OLLAMA_PARAMS = {
        numGpuLayers:     { label: 'GPU Layers (-ngl)', desc: 'Кол-во слоёв модели на GPU. -1 = все слои, 0 = только CPU. Определяет, какая часть модели загружается в VRAM.' },
        contextLength:    { label: 'Context Length (-c)', desc: 'Размер контекстного окна в токенах. По умолчанию 2048. Больше = длиннее история диалога, но больше VRAM/RAM.' },
        numParallel:      { label: 'Parallel Reqs (-np)', desc: 'Максимум одновременных запросов к одной модели. По умолчанию 1. Увеличивает throughput, но требует больше VRAM.' },
        numThreads:       { label: 'Threads (-t)', desc: 'Количество потоков CPU для вычислений. По умолчанию = кол-во ядер. Влияет на скорость инференса на CPU.' },
        batchSize:        { label: 'Batch Size (-b)', desc: 'Размер батча токенов для обработки. По умолчанию 512. Больше = быстрее, но больше потребление памяти.' },
        gpuSplitMode:     { label: 'GPU Split Mode', desc: 'Режим разделения модели по GPU: none (без разделения), layer (по слоям), row (по строкам тензоров).' },
        mainGpu:          { label: 'Main GPU', desc: 'Индекс основного GPU (0-based) для multi-GPU конфигураций. На него загружается большая часть модели.' },
        lowVram:          { label: 'Low VRAM Mode', desc: 'Режим экономии VRAM. Не загружает все веса модели в GPU-память сразу, подгружает по мере необходимости. Медленнее, но меньше VRAM.' },
        f16kv:            { label: 'FP16 KV Cache', desc: 'Использование 16-битной точности для KV-кэша. Экономит ~50% памяти кэша. True = FP16 включён (по умолчанию).' },
        kvCacheQuant:     { label: 'KV Cache Quant', desc: 'Тип квантования KV-кэша: f16 (без квантования), q8_0 (8-бит), q4_0 (4-бит). Сильнее квантование = меньше памяти, но возможна потеря качества.' },
        flashAttention:   { label: 'Flash Attention', desc: 'Алгоритм эффективного внимания. Снижает использование VRAM и ускоряет инференс. Требует поддержки GPU (Ampere+).' },
        source:           { label: 'Источник флагов', desc: 'Откуда взяты настройки: process-args (аргументы командной строки), env (переменные окружения), default (встроенные значения).' }
    };

    function renderOllamaParams(backend) {
        // Определяем тип бэкенда (используем унифицированную утилиту)
        var bt = Utils.getBackendType(backend);
        var isLlamaCpp = (bt === 'llama_cpp');

        const flags = backend.ollama?.runtimeFlags || {};
        const cap = backend.ollama?.backendCapacity || {};
        const contexts = backend.ollama?.modelContexts || [];
        const runningModels = backend.ollama?.runningModels || [];

        // Для llama_cpp показываем упрощённые параметры вместо Ollama-специфичных флагов
        if (isLlamaCpp) {
            var cppParams = backend.llama_cpp || {};
            var cppHtml = '<div class="be-detail-section"><div class="be-detail-title">🦒 ' + escapeHtml(_t('renderers.section_llama_cpp_params') || 'llama.cpp Parameters') + '</div><div class="be-params-grid">';
            if (cppParams.modelPath) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.model_path') || 'Model Path') + '</span><span class="be-param-value">' + escapeHtml(cppParams.modelPath) + '</span></div>';
            if (cppParams.contextLength !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.context')) + '</span><span class="be-param-value">' + escapeHtml(String(cppParams.contextLength)) + '</span></div>';
            if (cppParams.nGpuLayers !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.gpu_layers') || 'GPU Layers') + '</span><span class="be-param-value">' + escapeHtml(String(cppParams.nGpuLayers)) + '</span></div>';
            if (cppParams.nThreads !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.threads') || 'Threads') + '</span><span class="be-param-value">' + escapeHtml(String(cppParams.nThreads)) + '</span></div>';
            if (cppParams.batchSize !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.batch_size') || 'Batch Size') + '</span><span class="be-param-value">' + escapeHtml(String(cppParams.batchSize)) + '</span></div>';
            if (cppParams.quantization) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.quantization') || 'Quantization') + '</span><span class="be-param-value">' + escapeHtml(cppParams.quantization) + '</span></div>';
            cppHtml += '</div></div>';

            // Добавляем Disk/Network как обычно
            const sys = backend.system || {};
            if (sys.diskTotal !== undefined || sys.diskUsed !== undefined || sys.networkRX !== undefined) {
                cppHtml += '<div class="be-detail-section"><div class="be-detail-title">' + escapeHtml(_t('renderers.section_disk_network')) + '</div><div class="be-params-grid">';
                if (sys.diskTotal !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.disk_total')) + '</span><span class="be-param-value">' + formatMB(sys.diskTotal) + '</span></div>';
                if (sys.diskUsed !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.disk_used')) + '</span><span class="be-param-value">' + formatMB(sys.diskUsed) + '</span></div>';
                if (sys.diskFree !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.disk_free')) + '</span><span class="be-param-value">' + formatMB(sys.diskFree) + '</span></div>';
                if (sys.networkRX !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.network_rx')) + '</span><span class="be-param-value">' + formatMB(sys.networkRX) + '</span></div>';
                if (sys.networkTX !== undefined) cppHtml += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(_t('renderers.network_tx')) + '</span><span class="be-param-value">' + formatMB(sys.networkTX) + '</span></div>';
                cppHtml += '</div></div>';
            }

            // Показываем loaded models если есть
            if (runningModels.length > 0) {
                cppHtml += '<div class="be-detail-section"><div class="be-detail-title">' + escapeHtml(_t('renderers.section_loaded_models')) + '</div><div class="be-params-grid">';
                runningModels.forEach(function(m) {
                    var sizeGB = (m.size || 0) / 1024 / 1024 / 1024;
                    cppHtml += '<div class="be-param-item" style="grid-column: 1/-1;">' +
                        '<span class="be-param-label">' + escapeHtml(m.name) + '</span>' +
                        '<span class="be-param-value">' + sizeGB.toFixed(1) + ' GB</span>' +
                    '</div>';
                });
                cppHtml += '</div></div>';
            }
            return cppHtml;
        }

        // Section: Runtime Flags (Ollama only)
        let flagsHtml = `<div class="be-detail-section"><div class="be-detail-title">${_t('renderers.section_runtime_flags')}</div><div class="be-params-grid">`;

        const mainFlags = ['numGpuLayers', 'contextLength', 'numParallel', 'numThreads', 'batchSize'];
        mainFlags.forEach(key => {
            if (flags[key] !== undefined && flags[key] !== 0 && flags[key] !== '') {
                const param = OLLAMA_PARAMS[key];
                const value = key === 'numGpuLayers' && flags[key] === -1 ? 'auto' : String(flags[key]);
                flagsHtml += `<div class="be-param-item" title="${escapeHtml(param.desc)}">
                    <span class="be-param-label">${escapeHtml(param.label)}</span>
                    <span class="be-param-value">${escapeHtml(value)}</span>
                </div>`;
            }
        });

        const extraFlags = ['gpuSplitMode', 'mainGpu', 'kvCacheQuant', 'source'];
        extraFlags.forEach(key => {
            if (flags[key] !== undefined && flags[key] !== '' && String(flags[key]) !== '0') {
                const param = OLLAMA_PARAMS[key];
                flagsHtml += `<div class="be-param-item" title="${escapeHtml(param.desc)}">
                    <span class="be-param-label">${escapeHtml(param.label)}</span>
                    <span class="be-param-value">${escapeHtml(String(flags[key]))}</span>
                </div>`;
            }
        });

        const boolFlags = [
            { key: 'lowVram', label: 'Low VRAM', desc: OLLAMA_PARAMS.lowVram.desc },
            { key: 'f16kv', label: 'FP16 KV Cache', desc: OLLAMA_PARAMS.f16kv.desc },
            { key: 'flashAttention', label: 'Flash Attention', desc: OLLAMA_PARAMS.flashAttention.desc }
        ];
        boolFlags.forEach(({ key, label, desc }) => {
            if (flags[key] !== undefined) {
                const val = flags[key] ? '✅ ' + _t('common.yes') : '❌ ' + _t('common.no');
                const cls = flags[key] ? 'be-param-on' : 'be-param-off';
                flagsHtml += `<div class="be-param-item ${cls}" title="${escapeHtml(desc)}">
                    <span class="be-param-label">${escapeHtml(label)}</span>
                    <span class="be-param-value">${val}</span>
                </div>`;
            }
        });

        flagsHtml += '</div></div>';

        // Section: Backend Capacity
        let capHtml = '';
        if (cap.freeVram !== undefined || cap.loadedModelVram !== undefined) {
            capHtml = `<div class="be-detail-section"><div class="be-detail-title">${_t('renderers.section_backend_capacity')}</div><div class="be-params-grid">`;
            if (cap.freeVram !== undefined) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.free_vram')}</span><span class="be-param-value">${formatMB(cap.freeVram)}</span></div>`;
            if (cap.guaranteedVram !== undefined) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.guaranteed_90')}</span><span class="be-param-value">${formatMB(cap.guaranteedVram)}</span></div>`;
            if (cap.loadedModelVram !== undefined) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.models_vram')}</span><span class="be-param-value">${formatMB(cap.loadedModelVram)}</span></div>`;
            if (cap.contextOverheadMB !== undefined) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.context_overhead')}</span><span class="be-param-value">${formatMB(cap.contextOverheadMB)}</span></div>`;
            if (cap.loadableModelCount !== undefined) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.loadable_models_count')}</span><span class="be-param-value">${cap.loadableModelCount}</span></div>`;
            if (cap.mode) capHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.platform_mode')}</span><span class="be-param-value">${escapeHtml(String(cap.mode))}</span></div>`;
            capHtml += '</div></div>';
        }

        // Section: Model Contexts
        let ctxHtml = '';
        if (contexts.length > 0) {
            ctxHtml = `<div class="be-detail-section"><div class="be-detail-title">${_t('renderers.section_model_contexts')}</div>`;
            contexts.forEach(ctx => {
                ctxHtml += `<div class="be-model-ctx">
                    <div class="be-model-ctx-name">${escapeHtml(ctx.name)}</div>
                    <div class="be-params-grid" style="margin-bottom:0;">
                        <div class="be-param-item"><span class="be-param-label">${_t('renderers.context')}</span><span class="be-param-value">${formatNumber(ctx.contextLength)}</span></div>
                        <div class="be-param-item"><span class="be-param-label">${_t('renderers.effective')}</span><span class="be-param-value">${formatNumber(ctx.effectiveContext)}</span></div>
                        <div class="be-param-item"><span class="be-param-label">${_t('common.total')} (${_t('renderers.model_memory')})</span><span class="be-param-value">${formatMB(ctx.totalMemoryMB)}</span></div>
                        <div class="be-param-item"><span class="be-param-label">${_t('renderers.kv_cache')}</span><span class="be-param-value">${formatMB(ctx.kvCacheMemoryMB)}</span></div>
                        <div class="be-param-item"><span class="be-param-label">${_t('renderers.layers')}</span><span class="be-param-value">${ctx.numLayers || '-'}</span></div>
                        <div class="be-param-item"><span class="be-param-label">${_t('renderers.kv_precision')}</span><span class="be-param-value">${ctx.precisionBits || 16}-bit</span></div>
                    </div>
                </div>`;
            });
            ctxHtml += '</div>';
        }

        // Section: Loaded Models
        let modelsHtml = '';
        if (runningModels.length > 0) {
            modelsHtml = `<div class="be-detail-section"><div class="be-detail-title">${_t('renderers.section_loaded_models')}</div><div class="be-params-grid">`;
            runningModels.forEach(m => {
                const sizeGB = (m.size || 0) / 1024 / 1024 / 1024;
                const ramGB = (m.ramUsage || 0) / 1024 / 1024 / 1024;
                var digestShort = (m.digest || m.Digest || '').substring(0, 12);
                var expDate = m.expiresAt || m.ExpiresAt || '';
                modelsHtml += `<div class="be-param-item" style="grid-column: 1/-1;">
                    <span class="be-param-label">${escapeHtml(m.name)}</span>
                    <span class="be-param-value">${sizeGB.toFixed(1)} GB | ${escapeHtml(m.family || '-')} | ${escapeHtml(m.parameterSize || '-')} | ${escapeHtml(m.quantization || '-')}` +
                    (digestShort ? ` <code title="${_t('renderers.digest')}: ${escapeHtml(digestShort)}">${escapeHtml(digestShort)}</code>` : '') +
                    (expDate ? ` <span title="${_t('renderers.expires')}: ${escapeHtml(expDate)}" style="font-size:10px;color:var(--warning)">⌛${escapeHtml(expDate.substring(0, 10))}</span>` : '') +
                    (ramGB > 0.5 ? ` <span style="font-size:10px;color:var(--text-secondary)">RAM: ${ramGB.toFixed(2)} GB</span>` : '') +
                `</div>`;
            });
            modelsHtml += '</div></div>';
        }


        // Section: Disk / Network
        const sys = backend.system || {};
        let diskNetHtml = '';
        if (sys.diskTotal !== undefined || sys.diskUsed !== undefined || sys.diskFree !== undefined || sys.networkRX !== undefined || sys.networkTX !== undefined) {
            diskNetHtml = `<div class="be-detail-section"><div class="be-detail-title">${_t('renderers.section_disk_network')}</div><div class="be-params-grid">`;
            if (sys.diskTotal !== undefined || sys.diskUsed !== undefined || sys.diskFree !== undefined) {
                if (sys.diskTotal !== undefined) diskNetHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.disk_total')}</span><span class="be-param-value">${formatMB(sys.diskTotal)}</span></div>`;
                if (sys.diskUsed !== undefined) diskNetHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.disk_used')}</span><span class="be-param-value">${formatMB(sys.diskUsed)}</span></div>`;
                if (sys.diskFree !== undefined) diskNetHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.disk_free')}</span><span class="be-param-value">${formatMB(sys.diskFree)}</span></div>`;
            }
            if (sys.networkRX !== undefined) diskNetHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.network_rx')}</span><span class="be-param-value">${formatMB(sys.networkRX)}</span></div>`;
            if (sys.networkTX !== undefined) diskNetHtml += `<div class="be-param-item"><span class="be-param-label">${_t('renderers.network_tx')}</span><span class="be-param-value">${formatMB(sys.networkTX)}</span></div>`;
            diskNetHtml += '</div></div>';
        }

        return flagsHtml + capHtml + ctxHtml + modelsHtml + diskNetHtml;
    }

    function backendsPage(backends) {
        const tbody = document.getElementById('backendsManageBody');
        if (!tbody) return;
        if (!backends.length) {
            tbody.innerHTML = emptyRow(13, _t('renderers.no_data'));
            return;
        }

        tbody.innerHTML = backends.map((b, idx) => {
            const labels = (b.labels || []).join(', ') || '-';
            const lastContact = b.lastAgentContact ? new Date(b.lastAgentContact).toLocaleString(Utils._locale()) : '-';
            const maxModels = b.runtimeMaxModels || b.maxModels || b.ollama?.maxModels || '-';
            const detailsHtml = renderOllamaParams(b);
            const rowId = 'be-row-' + idx;
            const safeId = escapeHtml(b.id);

            const agentActionsHtml = b.hasAgent ? `
                <div class="agent-actions">
                    <button class="btn btn-restart" onclick="ui.restartAgent('${safeId}')" title="${_t('agents.restart_hint')}">${_t('agents.restart')}</button>
                    <button class="btn btn-logs" onclick="ui.viewAgentLogs('${safeId}')" title="${_t('agents.view_logs_hint')}">${_t('agents.view_logs')}</button>
                    <button class="btn btn-config" onclick="ui.showAgentDetails('${safeId}')" title="${_t('agents.config_hint')}">${_t('agents.config')}</button>
                </div>
            ` : '';

            return `
                <tr class="be-main-row" data-expand="${rowId}" style="cursor:pointer;">
                    <td><strong>${escapeHtml(b.id)}</strong> ${getBackendTypeBadge(b)} <span class="be-expand-icon">▶</span></td>
                    <td>${escapeHtml(b.name || b.id)}</td>
                    <td>${escapeHtml(b.host)}</td>
                    <td>${(Utils.getBackendType(b) === 'llama_cpp' ? (b.cppWorkerPort || b.port || 8080) : (b.ollamaPort || 11434))}</td>
                    <td>${b.agentPort || 18032}</td>
                    <td>${b.weight || 1}</td>
                    <td>${b.maxConcurrentRequests || 10}</td>
                    <td>${maxModels}</td>
                    <td>${badge(b.hasAgent ? _t('common.yes') : _t('common.no'), b.hasAgent ? 'success' : 'warning')}</td>
                    <td>${escapeHtml(labels)}</td>
                    <td>${escapeHtml(lastContact)}</td>
                    <td>${badge(b.status, b.status === 'healthy' ? 'success' : 'danger')}</td>
                    <td>
                        <button class="action-btn edit" onclick="event.stopPropagation(); ui.editBackend('${safeId}')">${_t('common.edit')}</button>
                        <button class="action-btn delete" onclick="event.stopPropagation(); ui.confirmDeleteBackend('${safeId}')">${_t('common.delete')}</button>
                        <button class="action-btn model" onclick="event.stopPropagation(); ui.openModelManageModal('${safeId}')" title="${_t('models.manage_title')}">${_t('models.manage_action')}</button>
                    </td>
                </tr>
                <tr class="be-detail-row" id="${rowId}" style="display:none;">
                    <td colspan="13">
                        <div class="be-detail-content">
                            ${detailsHtml}
                            ${agentActionsHtml}
                        </div>
                    </td>
                </tr>
            `;
        }).join('');

        // Attach row expand handlers
        tbody.querySelectorAll('.be-main-row').forEach(row => {
            row.addEventListener('click', function () {
                const targetId = this.getAttribute('data-expand');
                const detailRow = document.getElementById(targetId);
                const icon = this.querySelector('.be-expand-icon');
                if (detailRow) {
                    if (detailRow.style.display === 'none' || detailRow.style.display === '') {
                        detailRow.style.display = 'table-row';
                        if (icon) icon.textContent = '▼';
                    } else {
                        detailRow.style.display = 'none';
                        if (icon) icon.textContent = '▶';
                    }
                }
            });
        });
    }

    // ---- Models Page ----

    function modelsPage(backends) {
        backends = stableBackendOrder(backends);
        const allModels = [];
        const backendMap = {};
        backends.forEach(b => {
            backendMap[b.id] = b;
            (b.ollama?.runningModels || []).forEach(m => {
                allModels.push({ ...m, backend: b.id, backendStatus: b.status });
            });
        });

        Utils.setText('modelsTotal', allModels.length);
        Utils.setText('modelsLoaded', allModels.filter(m => m.backendStatus === 'healthy').length);

        Utils.setHTML('backendLoadList', backendLoad(backends));
        Utils.setHTML('modelsGrid', modelsGrid(allModels, backendMap));
    }

    function backendLoad(backends) {
        backends = stableBackendOrder(backends);
        if (!backends.length) return loading(_t('renderers.no_data'));

        return backends.map(b => {
            const mode = getBackendMode(b);
            const isGPU = mode === 'gpu';
            const gpu = b.gpu || {};
            const sys = b.system || {};
            const oll = b.ollama || {};

            const totalVRAM = gpu.memoryTotal || 0;
            const usedVRAM = gpu.memoryUsed || 0;
            const totalRAM = sys.memoryTotal || 0;
            const usedRAM = sys.memoryUsed || 0;
            const vramPercent = percent(usedVRAM, totalVRAM);
            const ramPercent = percent(usedRAM, totalRAM);

            const models = oll.runningModels || [];
            const activeReq = oll.activeRequests || 0;
            const maxReq = oll.maxConcurrentRequests || 10;
            const freeSlots = oll.freeSlots || 0;

            const vramBar = isGPU ? `
                <div class="backend-load-bar-row">
                    <span class="backend-load-bar-label">VRAM ${formatMB(usedVRAM)} / ${formatMB(totalVRAM)}</span>
                    <span class="backend-load-bar-value">${vramPercent.toFixed(1)}%</span>
                </div>
                <div class="backend-load-bar"><div class="backend-load-fill vram" style="width: ${vramPercent}%"></div></div>
            ` : '';

            return `
                <div class="backend-load-item">
                    <div class="backend-load-header">
                        <span class="backend-load-name"><strong>${escapeHtml(b.id)}</strong></span>
                        ${badge(b.status, b.status === 'healthy' ? 'success' : 'danger')}
                    </div>
                    <div class="backend-load-stats">
                        <span class="backend-load-stat">${_t('renderers.models_label')} <strong>${models.length}</strong></span>
                        <span class="backend-load-stat">${_t('renderers.active')}: <strong>${activeReq}/${maxReq}</strong></span>
                        <span class="backend-load-stat">${_t('renderers.free_label')} <strong>${freeSlots}</strong></span>
                    </div>
                    ${vramBar}
                    <div class="backend-load-bar-row">
                        <span class="backend-load-bar-label">RAM ${formatMB(usedRAM)} / ${formatMB(totalRAM)}</span>
                        <span class="backend-load-bar-value">${ramPercent.toFixed(1)}%</span>
                    </div>
                    <div class="backend-load-bar"><div class="backend-load-fill ram" style="width: ${ramPercent}%"></div></div>
                </div>
            `;
        }).join('');
    }

    function fmtExpiresShort(exp) {
        if (!exp) return '';
        var expDate = new Date(exp);
        if (isNaN(expDate.getTime())) return '';
        var now = Date.now();
        var diff = expDate.getTime() - now;
        if (diff < 0) return '⌛' + _t('renderers.expired');
        var sec = Math.floor(diff / 1000);
        if (sec < 60) return '⌛' + sec + 's';
        if (sec < 3600) return '⌛' + Math.floor(sec / 60) + 'm';
        if (sec < 86400) return '⌛' + Math.floor(sec / 3600) + 'h';
        return '⌛' + exp.substring(0, 10);
    }

    function modelsGrid(allModels, backendMap) {
        if (!allModels.length) return loading(_t('models.no_models'));

        return allModels.map(m => {
            const vramMB = (m.vramUsage || 0) / 1024 / 1024;
            const ramMB = (m.ramUsage || 0) / 1024 / 1024;
            const sizeGB = (m.size || 0) / 1024 / 1024 / 1024;
            const ramGB = (m.ramUsage || 0) / 1024 / 1024 / 1024;
            var digestShort = (m.digest || m.Digest || '').substring(0, 12);
            var expDate = m.expiresAt || m.ExpiresAt || '';
            var expShort = fmtExpiresShort(expDate);

            const backend = backendMap[m.backend] || {};
            const mode = getBackendMode(backend);
            const isGPU = mode === 'gpu';
            const gpu = backend.gpu || {};
            const sys = backend.system || {};

            const totalVRAM = isGPU ? (gpu.memoryTotal || 1) : 0;
            const totalRAM = sys.memoryTotal || 1;
            const vramPercent = totalVRAM > 0 ? Math.min(percent(vramMB, totalVRAM), 100) : 0;
            const ramPercent = totalRAM > 0 ? Math.min(percent(ramMB, totalRAM), 100) : 0;
            const showVRAM = isGPU && totalVRAM > 0;

            const safeBackend = escapeHtml(m.backend);
            const safeName = escapeHtml(m.name).replace(/'/g, "\\'");
            
            return `
                <div class="model-card" data-backend="${safeBackend}" data-model="${safeName}">
                    <div class="model-card-header">
                        <span class="model-name">${escapeHtml(m.name)}</span>
                        ${getBackendTypeBadge(backend)} ${badge(m.backend, m.backendStatus === 'healthy' ? 'success' : 'danger')}
                    </div>
                    <div class="model-size">${sizeGB.toFixed(1)} GB</div>
                    <div class="model-details">
                        ${modelDetail('VRAM', `${vramMB.toFixed(0)} MB`)}
                        ${modelDetail('RAM', `${ramMB.toFixed(0)} MB`)}
                        ${backendTypeDetails(m, backend)}
                        ${digestShort ? '<div class="model-detail"><div class="model-detail-label">' + _t('renderers.digest') + '</div><div class="model-detail-value"><code style="font-size:10px;background:var(--bg-secondary);padding:1px 4px;border-radius:3px">' + escapeHtml(digestShort) + '</code></div></div>' : ''}
                        ${expShort ? '<div class="model-detail"><div class="model-detail-label">' + _t('renderers.expires') + '</div><div class="model-detail-value" title="' + escapeHtml(expDate) + '" style="color:var(--warning);font-size:11px">' + expShort + '</div></div>' : ''}
                        ${ramGB > 0.5 ? '<div class="model-detail"><div class="model-detail-label">RAM</div><div class="model-detail-value" style="font-size:11px;color:var(--text-secondary)">' + ramGB.toFixed(2) + ' GB</div></div>' : ''}
                    </div>
                    <div class="model-memory-section">
                        <div class="model-memory-title">${_t('models.memory_title')}</div>
                        ${showVRAM ? memoryBar('VRAM', vramMB, totalVRAM, vramPercent, 'vram') : ''}
                        ${memoryBar('RAM', ramMB, totalRAM, ramPercent, 'ram')}
                    </div>
                    <div class="model-card-actions">
                        <button class="btn btn-load" onclick="window.modelCardAction('load', '${safeBackend}', '${safeName}')" ${m.backendStatus !== 'healthy' ? 'disabled' : ''}>${_t('models.load')}</button>
                        <button class="btn btn-unload" onclick="window.modelCardAction('unload', '${safeBackend}', '${safeName}')" ${m.backendStatus !== 'healthy' ? 'disabled' : ''}>${_t('models.unload')}</button>
                        <button class="btn btn-delete" onclick="window.modelCardAction('delete', '${safeBackend}', '${safeName}')" ${m.backendStatus !== 'healthy' ? 'disabled' : ''}>${_t('models.delete')}</button>
                    </div>
                </div>
            `;
        }).join('');
    }

    /**
     * Возвращает детали модели в зависимости от типа бэкенда (B-09, B-10).
     * Ollama: Family/Format/Params/Quant
     * llama_cpp: GGUF Path/Quantization
     */
    function backendTypeDetails(model, backend) {
        var bt = Utils.getBackendType(backend);
        if (bt === 'llama_cpp') {
            // GGUF-специфичные поля
            var ggufPath = model.ggufPath || model.path || '-';
            var ggufQuant = model.quantization || model.ggufQuant || '-';
            return modelDetail('GGUF Path', ggufPath) +
                modelDetail('Quantization', ggufQuant);
        }
        // Ollama-специфичные поля
        return modelDetail('Family', model.family || '-') +
            modelDetail('Format', model.format || '-') +
            modelDetail('Params', model.parameterSize || '-') +
            modelDetail('Quant', model.quantization || '-');
    }

    function modelDetail(label, value) {
        return `
            <div class="model-detail">
                <div class="model-detail-label">${escapeHtml(label)}</div>
                <div class="model-detail-value">${escapeHtml(String(value))}</div>
            </div>
        `;
    }

    function memoryBar(label, used, total, pct, type) {
        return `
            <div class="model-memory-bar-container">
                <div class="model-memory-bar-labels">
                    <span>${escapeHtml(label)}</span>
                    <span>${used.toFixed(0)} / ${total.toFixed(0)} MB (${pct.toFixed(1)}%)</span>
                </div>
                <div class="model-memory-bar">
                    <div class="model-memory-bar-fill ${type}" style="width: ${pct}%"></div>
                </div>
            </div>
        `;
    }

    // ---- Sessions Page ----

    function getClientIcon(clientName) {
        if (!clientName) return '👤';
        const name = clientName.toLowerCase();
        if (name.includes('cline')) return '🦾';
        if (name.includes('openwebui') || name.includes('open-webui')) return '🌐';
        if (name.includes('curl')) return '📡';
        if (name.includes('python') || name.includes('requests')) return '🐍';
        if (name.includes('postman')) return '📮';
        if (name.includes('insomnia')) return '💤';
        return '👤';
    }

    function sessionsPage(sessions) {
        const tbody = document.getElementById('sessionsTableBody');
        if (!tbody) return;
        if (!sessions.length) {
            tbody.innerHTML = emptyRow(7, _t('sessions.no_sessions'));
            return;
        }
        tbody.innerHTML = sessions.map(s => {
            const clientIcon = getClientIcon(s.clientName);
            return `
            <tr>
                <td><code>${escapeHtml(s.id?.substring(0, 16) || 'N/A')}...</code></td>
                <td>${escapeHtml(s.backendId || '-')}</td>
                <td>${escapeHtml(s.model || '-')}</td>
                <td>${s.requestCount || 0}</td>
                <td>${s.lastRequestAt ? new Date(s.lastRequestAt).toLocaleString(Utils._locale()) : '-'}</td>
                <td>${escapeHtml(s.clientIP || '-')}</td>
                <td>${clientIcon} ${escapeHtml(s.clientName || '-')}</td>
            </tr>
            `;
        }).join('');
    }

    // ---- Queue Page ----

    function queuePage(queue, queueTasks, queueHistory) {
        const current = queue?.current_size || 0;
        const max = queue?.max_size || 100;
        const processed = queue?.processed_total || 0;
        const workers = queue?.workers || 0;
        const avgWait = queue?.avg_wait_time_ms || 0;

        Utils.setText('queueCurrentSize', current);
        Utils.setText('queueMaxSize', max);
        Utils.setText('queueProcessed', processed);
        Utils.setText('queueWorkers', workers);
        Utils.setText('queueAvgWait', avgWait > 0 ? _t('renderers.queue_avg_wait', { seconds: (avgWait / 1000).toFixed(1) }) : '-');

        const pct = percent(current, max);
        Utils.setStyle('queueFill', 'width', `${pct}%`);
        Utils.setText('queueMidLabel', Math.round(max / 2));
        Utils.setText('queueMaxLabel', max);

        const badgeEl = document.getElementById('queueTasksCount');
        if (badgeEl) badgeEl.textContent = current;

        Utils.setHTML('queueTasksBody', queueTasksBody(queueTasks));
        Utils.setHTML('queueHistoryBody', queueHistoryBody(queueHistory));
    }

    function queueTasksBody(tasks) {
        if (!tasks.length) return emptyRow(5, _t('queue.no_tasks'));
        return tasks.map((task, index) => {
            const status = task.status || 'pending';
            const statusClass = status === 'processing' ? 'processing' : (status === 'completed' ? 'completed' : 'pending');
            const statusText = status === 'processing' ? _t('queue.processing') : (status === 'completed' ? _t('queue.completed') : _t('queue.pending'));
            const waitTime = task.waitTimeMs ? (task.waitTimeMs / 1000).toFixed(1) + _t('renderers.seconds') : '-';
            return `
                <tr>
                    <td>${index + 1}</td>
                    <td><code>${escapeHtml(task.model || '-')}</code></td>
                    <td>${escapeHtml(task.backend || 'Auto')}</td>
                    <td>${waitTime}</td>
                    <td><span class="queue-task-status ${statusClass}">${statusText}</span></td>
                </tr>
            `;
        }).join('');
    }

    function queueHistoryBody(history) {
        const badge = document.getElementById('queueHistoryCount');
        if (badge) badge.textContent = history.length;
        if (!history.length) return emptyRow(6, _t('renderers.no_completed_tasks'));

        return history.map((task, index) => {
            const enqueued = task.enqueued ? new Date(task.enqueued).toLocaleString(Utils._locale(), { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '-';
            const completed = task.completed_at ? new Date(task.completed_at).toLocaleString(Utils._locale(), { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '-';
            const waitMs = task.wait_time_ms || 0;
            const waitStr = waitMs > 0 ? (waitMs / 1000).toFixed(1) + _t('renderers.seconds') : '-';
            return `
                <tr>
                    <td>${index + 1}</td>
                    <td><code>${escapeHtml(task.model || '-')}</code></td>
                    <td>${escapeHtml(task.target || 'Auto')}</td>
                    <td>${enqueued}</td>
                    <td>${completed}</td>
                    <td>${waitStr}</td>
                </tr>
            `;
        }).join('');
    }

    // ---- Logs ----

    function logs(logEntries) {
        const container = document.getElementById('logsContainer');
        if (!container) return;
        if (!logEntries.length) {
            container.innerHTML = loading(_t('logs.no_logs'));
            return;
        }
        container.innerHTML = logEntries.map(log => `
            <div class="log-entry">
                <span class="log-time">${escapeHtml(log.time)}</span>
                <span class="log-level ${log.level.toLowerCase()}">${escapeHtml(log.level)}</span>
                <span class="log-message">${escapeHtml(log.message)}</span>
            </div>
        `).join('');
        container.scrollTop = 0;
    }

    // ---- Prediction Alerts ----

    function predictionAlerts(backends) {
        const alerts = [];
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
        if (!container) return;
        if (!alerts.length) {
            container.innerHTML = `<div class="alert alert-info">${_t('renderers.no_active_alerts')}</div>`;
            return;
        }
        container.innerHTML = alerts.map(a => `
            <div class="alert alert-${a.level}">
                <svg class="alert-icon-svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="currentColor"/></svg>
                <span><strong>${escapeHtml(a.backend)}</strong>: ${escapeHtml(a.reason)} ${_t('common.in')} ${a.seconds}${_t('renderers.seconds')}</span>
            </div>
        `).join('');
    }

    // ---- Proxy Logs ----

    function copyProxyLogs() {
        const container = document.getElementById('proxyLogsContainer');
        const table = container && container.querySelector('table.data-table');
        if (!table) return;
        const rows = table.querySelectorAll('tr');
        let csv = '';
        rows.forEach(function(row) {
            const cells = row.querySelectorAll('th, td');
            const rowData = Array.from(cells).map(function(cell) {
                return '"' + cell.textContent.trim().replace(/"/g, '""') + '"';
            }).join('\t');
            csv += rowData + '\n';
        });
        navigator.clipboard.writeText(csv).then(function() {
            const btn = document.getElementById('copyProxyLogs');
            if (btn) {
                const orig = btn.textContent;
                btn.textContent = '✓ ' + (window.I18N ? I18N.t('common.success') : 'Copied');
                setTimeout(function() { btn.textContent = orig; }, 2000);
            }
        }).catch(function() {});
    }

    function proxyLogs(entries) {
        const container = document.getElementById('proxyLogsContainer');
        if (!container) return;
        if (!entries.length) {
            container.innerHTML = `<div class="proxy-log-placeholder">${_t('logs.proxy_waiting')}</div>`;
            return;
        }
        const locale = (window.I18N && I18N.getLang() === 'ru') ? 'ru' : 'en';
        var html = '<table class="data-table proxy-logs-table"><thead><tr>' +
            '<th>' + _t('logs.time') + '</th>' +
            '<th>' + _t('logs.method') + '</th>' +
            '<th>' + _t('logs.path') + '</th>' +
            '<th>' + _t('logs.model') + '</th>' +
            '<th>' + _t('logs.client') + '</th>' +
            '<th>' + _t('logs.status') + '</th>' +
            '<th>' + _t('logs.duration') + '</th>' +
            '<th>' + _t('logs.backend') + '</th>' +
            '</tr></thead><tbody>';
        entries.slice(0, 500).forEach(function(e) {
            var time = e._time || (e.timestamp ? new Date(e.timestamp).toLocaleTimeString(locale) : '-');
            var method = e.method || 'GET';
            var path = e.path || '-';
            var model = e.model || '-';
            var client = '';
            if (e.clientName) {
                client = escapeHtml(e.clientName);
            } else if (e.clientIP) {
                client = escapeHtml(e.clientIP);
            } else {
                client = '-';
            }
            var status = e.statusCode ? String(e.statusCode) : '-';
            var duration = e.durationMs ? e.durationMs + 'ms' : '-';
            var backend = e.backendID || '-';
            var statusClass = 'log-status-ok';
            if (status !== '-' && parseInt(status) >= 400) statusClass = 'log-status-err';
            html += '<tr>' +
                '<td class="log-time-cell">' + escapeHtml(time) + '</td>' +
                '<td><span class="log-method-badge log-method-' + method.toLowerCase() + '">' + escapeHtml(method) + '</span></td>' +
                '<td class="log-path-cell" title="' + escapeHtml(path) + '">' + escapeHtml(path.length > 60 ? path.substring(0, 60) + '...' : path) + '</td>' +
                '<td>' + escapeHtml(model) + '</td>' +
                '<td>' + client + '</td>' +
                '<td><span class="' + statusClass + '">' + escapeHtml(status) + '</span></td>' +
                '<td>' + escapeHtml(duration) + '</td>' +
                '<td>' + escapeHtml(backend) + '</td>' +
                '</tr>';
        });
        html += '</tbody></table>';
        container.innerHTML = html;
    }

    // ---- Agents Page ----

    function agentsPage(agentsData) {
        const tbody = document.getElementById('agentsTableBody');
        if (!tbody) return;
        if (!agentsData || !agentsData.agents || !agentsData.agents.length) {
            tbody.innerHTML = emptyRow(9, _t('agents.no_agents'));
            return;
        }

        Utils.setText('agentsTotal', agentsData.totalAgents || 0);
        Utils.setText('agentsHealthy', agentsData.healthyAgents || 0);
        Utils.setText('agentsOffline', (agentsData.totalAgents || 0) - (agentsData.healthyAgents || 0));

        tbody.innerHTML = agentsData.agents.map(function(a) {
            const status = a.status || 'unknown';
            const statusClass = status === 'healthy' ? 'success' : (status === 'unhealthy' ? 'danger' : 'warning');
            const uptime = a.uptime ? formatUptime(a.uptime) : '-';
            const lastHb = a.lastHeartbeat ? new Date(a.lastHeartbeat).toLocaleString(Utils._locale()) : (a.lastAgentContact ? new Date(a.lastAgentContact).toLocaleString(Utils._locale()) : '-');
            return '<tr>' +
                '<td><strong>' + escapeHtml(a.id || '-') + '</strong></td>' +
                '<td>' + escapeHtml(a.host || '-') + '</td>' +
                '<td>' + escapeHtml(String(a.agentPort || a.port || '-')) + '</td>' +
                '<td>' + badge(status, statusClass) + '</td>' +
                '<td>' + escapeHtml(a.platform || '-') + '</td>' +
                '<td>' + escapeHtml(uptime) + '</td>' +
                '<td>' + escapeHtml(lastHb) + '</td>' +
                '<td>' + escapeHtml(a.ollamaVersion || a.ollama_version || '-') + '</td>' +
                '<td>' +
                    '<button class="action-btn view" onclick="ui.showAgentDetails(\'' + escapeHtml(a.id) + '\')" title="' + _t('agents.details') + '">' + _t('agents.details') + '</button>' +
                '</td>' +
            '</tr>';
        }).join('');
    }

    function formatUptime(seconds) {
        if (!seconds || seconds <= 0) return '-';
        const d = Math.floor(seconds / 86400);
        const h = Math.floor((seconds % 86400) / 3600);
        const m = Math.floor((seconds % 3600) / 60);
        const s = seconds % 60;
        let parts = [];
        if (d > 0) parts.push(d + 'd');
        if (h > 0) parts.push(h + 'h');
        if (m > 0) parts.push(m + 'm');
        if (s > 0 || parts.length === 0) parts.push(s + 's');
        return parts.join(' ');
    }

    function renderAgentDetails(agentInfo) {
        const container = document.getElementById('agentDetailsContent');
        if (!container) return;
        if (!agentInfo) {
            container.innerHTML = '<div class="loading">' + _t('renderers.no_data') + '</div>';
            return;
        }

        const gpu = agentInfo.gpu || agentInfo.metrics?.gpu || {};
        const sys = agentInfo.system || agentInfo.metrics?.system || {};
        const oll = agentInfo.ollama || agentInfo.metrics?.ollama || {};
        const cap = oll.backendCapacity || {};
        const runningModels = oll.runningModels || [];
        const flags = oll.runtimeFlags || {};

        let html = '<div class="be-detail-content">';

        // Section: Basic Info
        html += '<div class="be-detail-section"><div class="be-detail-title">' + _t('agents.settings_title') + '</div><div class="be-params-grid">';
        html += '<div class="be-param-item"><span class="be-param-label">' + _t('agents.id') + '</span><span class="be-param-value">' + escapeHtml(agentInfo.id || '-') + '</span></div>';
        html += '<div class="be-param-item"><span class="be-param-label">' + _t('backends.host') + '</span><span class="be-param-value">' + escapeHtml(agentInfo.host || '-') + '</span></div>';
        html += '<div class="be-param-item"><span class="be-param-label">' + _t('backends.agent_port') + '</span><span class="be-param-value">' + escapeHtml(String(agentInfo.agentPort || agentInfo.port || '-')) + '</span></div>';
        html += '<div class="be-param-item"><span class="be-param-label">' + _t('agents.platform') + '</span><span class="be-param-value">' + escapeHtml(agentInfo.platform || '-') + '</span></div>';
        html += '<div class="be-param-item"><span class="be-param-label">' + _t('agents.uptime') + '</span><span class="be-param-value">' + escapeHtml(formatUptime(agentInfo.uptime)) + '</span></div>';
        html += '<div class="be-param-item"><span class="be-param-label">' + _t('agents.last_heartbeat') + '</span><span class="be-param-value">' + (agentInfo.lastHeartbeat ? escapeHtml(new Date(agentInfo.lastHeartbeat).toLocaleString(Utils._locale())) : (agentInfo.lastAgentContact ? escapeHtml(new Date(agentInfo.lastAgentContact).toLocaleString(Utils._locale())) : '-')) + '</span></div>';
        html += '<div class="be-param-item"><span class="be-param-label">' + _t('agents.ollama_version') + '</span><span class="be-param-value">' + escapeHtml(agentInfo.ollamaVersion || agentInfo.ollama_version || '-') + '</span></div>';

        // Runtime flags
        if (agentInfo.runtimeFlags || Object.keys(flags).length > 0) {
            const rf = agentInfo.runtimeFlags || flags;
            const flagKeys = Object.keys(rf).filter(k => rf[k] !== undefined && rf[k] !== null && rf[k] !== '');
            flagKeys.forEach(function(k) {
                html += '<div class="be-param-item"><span class="be-param-label">' + escapeHtml(k) + '</span><span class="be-param-value">' + escapeHtml(String(rf[k])) + '</span></div>';
            });
        }

        html += '</div></div>';

        // Section: GPU Metrics
        if (gpu.usagePercent !== undefined || gpu.memoryUsed !== undefined) {
            html += '<div class="be-detail-section"><div class="be-detail-title">' + _t('renderers.section_gpu') + '</div><div class="be-params-grid">';
            if (gpu.usagePercent !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('metrics.gpu_usage') + '</span><span class="be-param-value">' + gpu.usagePercent.toFixed(1) + '%</span></div>';
            if (gpu.memoryUsed !== undefined && gpu.memoryTotal !== undefined) {
                html += '<div class="be-param-item"><span class="be-param-label">' + _t('metrics.vram_usage') + '</span><span class="be-param-value">' + formatMB(gpu.memoryUsed) + ' / ' + formatMB(gpu.memoryTotal) + ' (' + percent(gpu.memoryUsed, gpu.memoryTotal).toFixed(1) + '%)</span></div>';
            }
            if (gpu.temperature !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('metrics.gpu_temp') + '</span><span class="be-param-value">' + gpu.temperature + '°C</span></div>';
            html += '</div></div>';
        }

        // Section: System Metrics
        if (sys.cpuUsagePercent !== undefined || sys.memoryUsed !== undefined) {
            html += '<div class="be-detail-section"><div class="be-detail-title">' + _t('renderers.section_system') + '</div><div class="be-params-grid">';
            if (sys.cpuUsagePercent !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('metrics.cpu_usage') + '</span><span class="be-param-value">' + sys.cpuUsagePercent.toFixed(1) + '%</span></div>';
            if (sys.memoryUsed !== undefined && sys.memoryTotal !== undefined) {
                html += '<div class="be-param-item"><span class="be-param-label">' + _t('metrics.ram_usage') + '</span><span class="be-param-value">' + formatMB(sys.memoryUsed) + ' / ' + formatMB(sys.memoryTotal) + ' (' + percent(sys.memoryUsed, sys.memoryTotal).toFixed(1) + '%)</span></div>';
            }
            if (sys.cpuTemperature !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('metrics.cpu_temp') + '</span><span class="be-param-value">' + sys.cpuTemperature + '°C</span></div>';
            html += '</div></div>';
        }

        // Section: Capacity
        if (cap.freeVram !== undefined) {
            html += '<div class="be-detail-section"><div class="be-detail-title">' + _t('renderers.section_backend_capacity') + '</div><div class="be-params-grid">';
            if (cap.freeVram !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('renderers.free_vram') + '</span><span class="be-param-value">' + formatMB(cap.freeVram) + '</span></div>';
            if (cap.loadedModelVram !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('renderers.models_vram') + '</span><span class="be-param-value">' + formatMB(cap.loadedModelVram) + '</span></div>';
            html += '</div></div>';
        }

        // Section: Disk / Network
        if (sys.diskTotal !== undefined || sys.diskUsed !== undefined || sys.diskFree !== undefined || sys.networkRX !== undefined || sys.networkTX !== undefined) {
            html += '<div class="be-detail-section"><div class="be-detail-title">' + _t('renderers.section_disk_network') + '</div><div class="be-params-grid">';
            if (sys.diskTotal !== undefined || sys.diskUsed !== undefined || sys.diskFree !== undefined) {
                if (sys.diskTotal !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('renderers.disk_total') + '</span><span class="be-param-value">' + formatMB(sys.diskTotal) + '</span></div>';
                if (sys.diskUsed !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('renderers.disk_used') + '</span><span class="be-param-value">' + formatMB(sys.diskUsed) + '</span></div>';
                if (sys.diskFree !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('renderers.disk_free') + '</span><span class="be-param-value">' + formatMB(sys.diskFree) + '</span></div>';
            }
            if (sys.networkRX !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('renderers.network_rx') + '</span><span class="be-param-value">' + formatMB(sys.networkRX) + '</span></div>';
            if (sys.networkTX !== undefined) html += '<div class="be-param-item"><span class="be-param-label">' + _t('renderers.network_tx') + '</span><span class="be-param-value">' + formatMB(sys.networkTX) + '</span></div>';
            html += '</div></div>';
        }

        // Section: Running Models
        if (runningModels.length > 0) {
            html += '<div class="be-detail-section"><div class="be-detail-title">' + _t('renderers.section_loaded_models') + '</div><div class="be-params-grid">';
            runningModels.forEach(function(m) {
                const sizeGB = (m.size || 0) / 1024 / 1024 / 1024;
                const ramGB = (m.ramUsage || 0) / 1024 / 1024 / 1024;
                var digestShort = (m.digest || m.Digest || '').substring(0, 12);
                var expDate = m.expiresAt || m.ExpiresAt || '';
                html += '<div class="be-param-item" style="grid-column: 1/-1;">' +
                    '<span class="be-param-label">' + escapeHtml(m.name) + '</span>' +
                    '<span class="be-param-value">' + sizeGB.toFixed(1) + ' GB | ' + escapeHtml(m.family || '-') + ' | ' + escapeHtml(m.parameterSize || '-') + ' | ' + escapeHtml(m.quantization || '-') +
                    (digestShort ? ' <code title="' + _t('renderers.digest') + ': ' + escapeHtml(digestShort) + '">' + escapeHtml(digestShort) + '</code>' : '') +
                    (expDate ? ' <span title="' + _t('renderers.expires') + ': ' + escapeHtml(expDate) + '" style="font-size:10px;color:var(--warning)">⌛' + escapeHtml(expDate.substring(0, 10)) + '</span>' : '') +
                    (ramGB > 0.5 ? ' <span style="font-size:10px;color:var(--text-secondary)">RAM: ' + ramGB.toFixed(2) + ' GB</span>' : '') +
                    '</span>' +
                '</div>';
            });
            html += '</div></div>';
        }


        html += '</div>';
        container.innerHTML = html;
    }

    // ---- GGUF Models Page ----

    function ggufPage(data) {
        var container = document.getElementById('ggufContainer');
        if (!container) return;
        if (!data || !data.backends || !data.backends.length) {
            container.innerHTML = loading(_t('gguf.no_backends'));
            return;
        }

        Utils.setText('ggufTotalBackends', data.backends.length);

        // Build backend selector
        var selectorHtml = '<div class="gguf-backend-selector"><label>' + _t('gguf.select_backend') + ': </label><select id="ggufBackendSelect" onchange="Renderers.ggufFilterBackend()" style="margin-left:0.5rem;padding:0.3rem 0.5rem;border-radius:4px;border:1px solid var(--border);background:var(--bg);color:var(--text);">';
        selectorHtml += '<option value="">' + _t('gguf.all_backends') + '</option>';
        data.backends.forEach(function(b) {
            var selected = (window._ggufSelectedBackend === b.id) ? ' selected' : '';
            selectorHtml += '<option value="' + escapeHtml(b.id) + '"' + selected + '>' + escapeHtml(b.id) + ' (' + escapeHtml(b.status) + ')</option>';
        });
        selectorHtml += '</select></div>';

        container.innerHTML = selectorHtml + '<div id="ggufBackendCards" style="display:grid;gap:1rem;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));"></div>';
        renderGgufCardsInto(data.backends);
    }

    function ggufFilterBackend() {
        var sel = document.getElementById('ggufBackendSelect');
        var selected = sel ? sel.value : '';
        window._ggufSelectedBackend = selected;
        // Re-fetch data and re-render
        if (window.API && window.API.fetchGgufBackends) {
            window.API.fetchGgufBackends().then(function(data) {
                if (data && data.backends) {
                    var filtered = selected
                        ? data.backends.filter(function(b) { return b.id === selected; })
                        : data.backends;
                    renderGgufCardsInto(filtered);
                }
            }).catch(function() {});
        }
    }

    function renderGgufCardsInto(backends) {
        var container = document.getElementById('ggufBackendCards');
        if (!container) return;
        if (!backends || !backends.length) {
            container.innerHTML = '<div class="gguf-no-backends">' + _t('gguf.no_backends') + '</div>';
            return;
        }
        container.innerHTML = backends.map(function(b) {
            var modelsHtml = (b.models || []).map(function(m) {
                return '<div class="gguf-model-item" style="display:flex;justify-content:space-between;padding:0.3rem 0;border-bottom:1px solid var(--border-light);"><span class="gguf-model-name" style="font-weight:500;">' + escapeHtml(m.name) + '</span><span class="gguf-model-status ' + m.status + '">' + escapeHtml(m.status) + '</span>' + (m.contextSize ? '<span class="gguf-model-ctx" style="font-size:0.8rem;color:var(--text-muted);">ctx: ' + m.contextSize + '</span>' : '') + '</div>';
            }).join('') || '<div class="gguf-no-models" style="padding:0.5rem;color:var(--text-muted);">' + _t('gguf.no_models') + '</div>';

            var vramPct = b.vramUsagePercent || 0;
            var gpuMem = b.gpuMemory || {};
            var backendType = b.type || 'llama_cpp';
            var typeBadge = (backendType === 'llama_cpp') ? badge('llama.cpp', 'info') : badge(backendType, 'info');

            return '<div class="gguf-backend-card" style="background:var(--bg-card);border:1px solid var(--border);border-radius:8px;padding:1rem;">' +
                '<div class="gguf-backend-header" style="display:flex;align-items:center;gap:0.5rem;margin-bottom:0.5rem;">' +
                    '<span class="gguf-backend-name" style="font-weight:600;font-size:1.1rem;">' + escapeHtml(b.id) + '</span>' +
                    badge(b.status, b.status === 'healthy' ? 'success' : 'danger') +
                    typeBadge +
                '</div>' +
                '<div class="gguf-backend-url" style="font-size:0.85rem;color:var(--text-muted);margin-bottom:0.5rem;">' + escapeHtml(b.url || (b.host + ':' + (b.cppWorkerPort || b.ollamaPort))) + '</div>' +
                '<div class="gguf-backend-metrics" style="display:flex;justify-content:space-between;font-size:0.85rem;margin-bottom:0.3rem;">' +
                    '<span>' + _t('gguf.vram') + ': ' + formatMB(gpuMem.usedMB || 0) + ' / ' + formatMB(gpuMem.totalMB || 0) + '</span>' +
                    '<span style="font-weight:600;">' + vramPct.toFixed(1) + '%</span>' +
                '</div>' +
                '<div class="gguf-backend-bar" style="height:6px;background:var(--bg-secondary);border-radius:3px;margin-bottom:0.5rem;"><div class="gguf-backend-fill" style="height:100%;width:' + vramPct + '%;background:var(--accent);border-radius:3px;"></div></div>' +
                '<div class="gguf-backend-models" style="margin-top:0.5rem;">' +
                    '<div class="gguf-backend-section-title" style="font-weight:600;margin-bottom:0.3rem;">' + _t('gguf.models') + ' (' + (b.models ? b.models.length : 0) + ')</div>' +
                    modelsHtml +
                '</div>' +
            '</div>';
        }).join('');
    }

    // ---- Public API ----
    return {
        dashboard,
        backendsPage,
        modelsPage,
        sessionsPage,
        queuePage,
        logs,
        predictionAlerts,
        proxyLogs,
        copyProxyLogs,
        agentsPage,
        renderAgentDetails,
        ggufPage,
        ggufFilterBackend,
        renderGgufCardsInto
    };
})();
