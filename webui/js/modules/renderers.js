/**
 * UI Renderers — all HTML generation in one place
 */
const Renderers = (function () {
    const { formatNumber, formatMB, getBackendMode, getBackendModeBadge, getGPUStatus, getProgressClass, percent, escapeHtml } = Utils;

    // Cache for prediction hysteresis to prevent flickering between warning/success
    const _predCache = {};
    const PRED_HYSTERESIS = 45; // seconds — must deviate this much from threshold to switch
    const PRED_THRESHOLD = 300; // seconds

    // ---- Generic helpers ----

    function badge(status, type) {
        return `<span class=\"badge badge-${type}\">${escapeHtml(status)}</span>`;
    }

    function loading(text = 'Ожидание данных...') {
        return `<div class=\"loading\">${escapeHtml(text)}</div>`;
    }

    function emptyRow(cols, text) {
        return `<tr><td colspan=\"${cols}\" class=\"loading-cell\">${escapeHtml(text)}</td></tr>`;
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
        Utils.setText('healthyBackends', `${healthy.length} здоровых`);
        Utils.setText('totalModels', totalModels);
        Utils.setText('loadedModels', `${totalModels} загружено`);
        Utils.setText('totalSessions', activeSessions);
        Utils.setText('sessionRate', `${totalRequests} запросов`);
        Utils.setText('queueSize', queueSize);
        Utils.setText('queueProcessed', `${queueProcessed} обработано`);
        Utils.setStyle('queueDashboardFill', 'width', `${queuePct}%`);

        Utils.setHTML('runtimeCluster', runtimeCluster(backends));
        Utils.setHTML('gpuCluster', gpuCluster(backends));
        Utils.setHTML('capacitySection', capacitySection(backends));
        Utils.setHTML('availableModelsList', availableModels(backends));
        Utils.setHTML('backendsTableBody', backendsTable(backends));
    }

    // ---- Runtime Cluster ----

    function runtimeCluster(backends) {
        if (!backends.length) return loading('Нет данных о бэкендах');
        return backends.map(backend => {
            const flags = backend.ollama?.runtimeFlags || {};
            const contexts = backend.ollama?.modelContexts || [];

            const flagBadges = [];
            if (flags.numGpuLayers !== undefined && flags.numGpuLayers !== 0) {
                const cls = flags.numGpuLayers === -1 ? '' : 'warning';
                flagBadges.push(`<span class=\"flag-badge ${cls}\">GPU:${flags.numGpuLayers === -1 ? 'auto' : flags.numGpuLayers}</span>`);
            }
            if (flags.contextLength) flagBadges.push(`<span class=\"flag-badge\">C:${formatNumber(flags.contextLength)}</span>`);
            if (flags.numParallel && flags.numParallel > 1) flagBadges.push(`<span class=\"flag-badge warning\">NP:${flags.numParallel}</span>`);
            if (flags.numThreads) flagBadges.push(`<span class=\"flag-badge\">T:${flags.numThreads}</span>`);
            if (flags.batchSize && flags.batchSize !== 512) flagBadges.push(`<span class=\"flag-badge\">B:${flags.batchSize}</span>`);
            if (flags.lowVram) flagBadges.push(`<span class=\"flag-badge danger\">LOW_VRAM</span>`);
            if (flags.flashAttention) flagBadges.push(`<span class=\"flag-badge\">FA</span>`);
            if (flags.kvCacheQuant && flags.kvCacheQuant !== 'f16') flagBadges.push(`<span class=\"flag-badge warning\">KV:${flags.kvCacheQuant}</span>`);

            const contextBadges = contexts.map(ctx => `
                <span class=\"context-info\">
                    ${escapeHtml(ctx.name)}: Ctx ${formatNumber(ctx.effectiveContext)} (${ctx.contextSource})
                    <div class=\"context-tooltip\">
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
                <div class=\"runtime-card\">
                    <div class=\"runtime-header\">
                        <strong>${escapeHtml(backend.id)}</strong>
                        ${badge(backend.status, backend.status === 'healthy' ? 'success' : 'danger')}
                    </div>
                    <div class=\"runtime-flags\">
                        ${flagBadges.length ? flagBadges.join('') : '<span class=\"flag-badge\">default</span>'}
                    </div>
                    ${contextBadges ? `<div style=\"margin-top:0.5rem;\">${contextBadges}</div>` : ''}
                </div>
            `;
        }).join('');
    }

    function tooltipRow(label, value) {
        return `<div class=\"context-tooltip-row\"><span class=\"context-tooltip-label\">${escapeHtml(label)}</span><span class=\"context-tooltip-value\">${escapeHtml(String(value))}</span></div>`;
    }

    // ---- GPU Cluster ----

    function gpuCluster(backends) {
        if (!backends.length) return loading('Нет данных о бэкендах');

        const gpuBackends = backends.filter(b => getBackendMode(b) === 'gpu');
        const cpuBackends = backends.filter(b => getBackendMode(b) === 'cpu');
        const cloudBackends = backends.filter(b => getBackendMode(b) === 'cloud');

        Utils.setHTML('gpuClusterBadge', `${gpuBackends.length} GPU · ${cpuBackends.length} CPU · ${cloudBackends.length} <svg class=\"badge-icon-svg\" viewBox=\"0 0 24 24\"><circle cx=\"12\" cy=\"12\" r=\"10\" fill=\"currentColor\"/></svg>`);

        return backends.map(b => gpuCard(b)).join('');
    }

    function gpuCard(backend) {
        const mode = getBackendMode(backend);

        if (mode === 'cloud') {
            const activeReq = backend.activeRequests || 0;
            const maxReq = backend.maxConcurrentRequests || 10;
            const models = (backend.ollama?.runningModels || []).length;
            const rps = backend.ollama?.requestsPerSecond || 0;
            return `
                <div class=\"gpu-card gpu-card-cloud\">
                    <div class=\"gpu-card-header\">
                        <span class=\"gpu-card-title\">${escapeHtml(backend.id)}</span>
                        <svg class=\"badge-icon-svg\" viewBox=\"0 0 24 24\"><circle cx=\"12\" cy=\"12\" r=\"10\" fill=\"currentColor\"/></svg>
                    </div>
                    <div class=\"gpu-metrics\">
                        ${metric('Req', `${activeReq}/${maxReq}`)}
                        ${metric('Models', models)}
                        ${metric('RPS', rps.toFixed(1))}
                        ${metric('Status', backend.status)}
                    </div>
                    <div class=\"progress-bar\"><div class=\"progress-fill low\" style=\"width: 0%\"></div></div>
                </div>
            `;
        }

        if (mode === 'cpu') {
            const sys = backend.system || {};
            const cpuUsage = sys.cpuUsagePercent || 0;
            const ramTotal = sys.memoryTotal || 1;
            const ramUsed = sys.memoryUsed || 0;
            const ramPct = percent(ramUsed, ramTotal);
            const temp = sys.cpuTemperature !== undefined ? sys.cpuTemperature : '-';
            const loadAvg = sys.loadAverage1 !== undefined ? sys.loadAverage1.toFixed(2) : '-';
            return `
                <div class=\"gpu-card gpu-card-cpu\">
                    <div class=\"gpu-card-header\">
                        <span class=\"gpu-card-title\">${escapeHtml(backend.id)}</span>
                        ${badge('CPU', 'warning')}
                    </div>
                    <div class=\"gpu-metrics\">
                        ${metric('CPU', `${cpuUsage.toFixed(1)}%`)}
                        ${metric('RAM', `${ramPct.toFixed(1)}%`)}
                        ${metric('Load', loadAvg)}
                        ${metric('Temp', `${temp}°C`)}
                    </div>
                    <div class=\"progress-bar\"><div class=\"progress-fill ${getProgressClass(cpuUsage)}\" style=\"width: ${cpuUsage}%\"></div></div>
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
        const status = getGPUStatus(usage, vramPct, temp);
        return `
            <div class=\"gpu-card\">
                <div class=\"gpu-card-header\">
                    <span class=\"gpu-card-title\">${escapeHtml(backend.id)}</span>
                    <span class=\"gpu-status ${status}\"></span>
                </div>
                <div class=\"gpu-metrics\">
                    ${metric('GPU', `${usage.toFixed(1)}%`)}
                    ${metric('VRAM', `${vramPct.toFixed(1)}%`)}
                    ${metric('Temp', `${temp}°C`)}
                    ${metric('Power', `${power}W`)}
                </div>
                <div class=\"progress-bar\"><div class=\"progress-fill ${getProgressClass(usage)}\" style=\"width: ${usage}%\"></div></div>
            </div>
        `;
    }

    function metric(label, value) {
        return `
            <div class=\"gpu-metric\">
                <div class=\"gpu-metric-label\">${escapeHtml(label)}</div>
                <div class=\"gpu-metric-value\">${escapeHtml(String(value))}</div>
            </div>
        `;
    }

    // ---- Capacity Section ----

    function capacitySection(backends) {
        if (!backends.length) return loading('Нет данных о бэкендах');
        return backends.map(b => capacityCard(b)).join('');
    }

    function capacityCard(backend) {
        const mode = getBackendMode(backend);
        const cap = backend.ollama?.backendCapacity || {};

        if (mode === 'cloud') {
            return `
                <div class=\"capacity-card capacity-cloud\">
                    <div class=\"runtime-header\">
                        <strong>${escapeHtml(backend.id)}</strong>
                        ${getBackendModeBadge(backend)}
                    </div>
                    <div class=\"capacity-cloud-info\">
                        <div class=\"capacity-cloud-row\"><span>Хост</span><span>${escapeHtml(backend.host || '-')}</span></div>
                        <div class=\"capacity-cloud-row\"><span>Статус</span><span>${badge(backend.status, backend.status === 'healthy' ? 'success' : 'danger')}</span></div>
                        <div class=\"capacity-cloud-row\"><span>Active Req</span><span>${backend.activeRequests || 0}</span></div>
                        <div class=\"capacity-cloud-row\"><span>Загружено моделей</span><span>${(backend.ollama?.runningModels || []).length}</span></div>
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
            <div class=\"capacity-card\">
                <div class=\"runtime-header\">
                    <strong>${escapeHtml(backend.id)}</strong>
                    ${getBackendModeBadge(backend)}
                </div>
                <div class=\"capacity-bar-container\">
                    <div class=\"capacity-bar-labels\">
                        <span>${memLabel}: ${formatMB(memUsed)} / ${formatMB(memTotal)}</span>
                        <span>${usedPct.toFixed(1)}%</span>
                    </div>
                    <div class=\"capacity-bar\">
                        <div class=\"capacity-bar-filled\" style=\"width: ${loadedPct}%\"></div>
                        <div class=\"capacity-bar-context\" style=\"width: ${ctxPct}%\"></div>
                        <div class=\"capacity-bar-guaranteed\" style=\"width: ${guarPct}%\"></div>
                    </div>
                </div>
                <div class=\"capacity-stats\">
                    <div class=\"capacity-stat\"><span>Loaded models</span><span>${formatMB(loadedMem)}</span></div>
                    <div class=\"capacity-stat\"><span>Context overhead</span><span>${formatMB(ctxOverhead)}</span></div>
                    <div class=\"capacity-stat\"><span>Free ${memLabel}</span><span>${formatMB(memFree)}</span></div>
                    <div class=\"capacity-stat\"><span>Guaranteed (90%)</span><span>${formatMB(guaranteed)}</span></div>
                </div>
            </div>
        `;
    }

    // ---- Available Models ----

    function availableModels(backends) {
        const badge = document.getElementById('loadableModelCount');
        if (!backends.length) {
            if (badge) badge.textContent = '-';
            return loading('Нет данных');
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

        if (!allModels.length) return loading('Нет доступных моделей');

        allModels.sort((a, b) => {
            if (a.canLoad !== b.canLoad) return b.canLoad - a.canLoad;
            return (a.estimatedVram || 0) - (b.estimatedVram || 0);
        });

        const items = allModels.slice(0, 50).map(m => {
            const cls = m.canLoad ? 'model-loadable' : 'model-unloadable';
            const vram = m.estimatedVram || 0;
            return `
                <div class=\"available-model-item ${cls}\">
                    <div class=\"model-indicator\"></div>
                    <span class=\"model-name\">${escapeHtml(m.name)}</span>
                    <span class=\"model-vram\">${formatMB(vram)} @ ${escapeHtml(m.backendId)}</span>
                </div>
            `;
        }).join('');

        const extra = allModels.length > 50
            ? `<div style=\"text-align:center;color:var(--text-muted);padding:0.5rem;font-size:0.8rem;\">+${allModels.length - 50} моделей</div>`
            : '';

        return items + extra;
    }

    // ---- Backends Table (Dashboard) ----

    function backendsTable(backends) {
        if (!backends.length) return emptyRow(11, 'Нет данных');

        return backends.map(b => {
            const mode = getBackendMode(b);
            const gpu = b.gpu || {};
            const sys = b.system || {};
            const pred = b.prediction || {};
            const oll = b.ollama || {};
            const isCloud = mode === 'cloud';

            const gpuUsage = isCloud ? '-' : (gpu.usagePercent !== undefined ? gpu.usagePercent.toFixed(1) + '%' : '-');
            const vramPercent = isCloud ? '-' : (gpu.memoryTotal > 0 ? percent(gpu.memoryUsed, gpu.memoryTotal).toFixed(1) + '%' : '-');
            const cpuUsage = isCloud ? '-' : (sys.cpuUsagePercent !== undefined ? sys.cpuUsagePercent.toFixed(1) + '%' : '-');
            const ramPercent = isCloud ? '-' : (sys.memoryTotal > 0 ? percent(sys.memoryUsed, sys.memoryTotal).toFixed(1) + '%' : '-');

            const activeReq = b.activeRequests || 0;
            const maxReq = b.maxConcurrentRequests || 10;
            const models = oll.runningModels?.length || 0;
            const rps = oll.requestsPerSecond || 0;
            const secondsToCrit = pred.secondsToCritical || -1;
            const prev = _predCache[b.id];
            let predClass, predText;

            if (secondsToCrit > 0) {
                predText = `${Math.round(secondsToCrit)}с`;
                const isWarning = secondsToCrit < PRED_THRESHOLD;
                if (prev) {
                    // Apply hysteresis: once in warning, stay there until above threshold + margin
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
                    <td><strong>${escapeHtml(b.id)}</strong> ${getBackendModeBadge(b)}</td>
                    <td>${badge(b.status, b.status === 'healthy' ? 'success' : 'danger')}</td>
                    <td>${escapeHtml(gpuUsage)}</td>
                    <td>${escapeHtml(vramPercent)}</td>
                    <td>${escapeHtml(cpuUsage)}</td>
                    <td>${escapeHtml(ramPercent)}</td>
                    <td>${activeReq}/${maxReq}</td>
                    <td>${models}</td>
                    <td>${rps.toFixed(1)}</td>
                    <td>${badge(predText, predClass)}</td>
                    <td>
                        <button class=\"action-btn edit\" onclick=\"ui.editBackend('${escapeHtml(b.id)}')\">Edit</button>
                        <button class=\"action-btn delete\" onclick=\"ui.confirmDeleteBackend('${escapeHtml(b.id)}')\">Del</button>
                    </td>
                </tr>
            `;
        }).join('');
    }

    // ---- Backends Management Page ----

    function backendsPage(backends) {
        const tbody = document.getElementById('backendsManageBody');
        if (!tbody) return;
        if (!backends.length) {
            tbody.innerHTML = emptyRow(13, 'Нет данных');
            return;
        }

        tbody.innerHTML = backends.map(b => {
            const labels = (b.labels || []).join(', ') || '-';
            const lastContact = b.lastAgentContact ? new Date(b.lastAgentContact).toLocaleString('ru') : '-';
            const maxModels = b.maxModels || b.ollama?.maxModels || b.runtimeMaxModels || '-';
            return `
                <tr>
                    <td><strong>${escapeHtml(b.id)}</strong></td>
                    <td>${escapeHtml(b.name || b.id)}</td>
                    <td>${escapeHtml(b.host)}</td>
                    <td>${b.ollamaPort || 11434}</td>
                    <td>${b.agentPort || 18032}</td>
                    <td>${b.weight || 1}</td>
                    <td>${b.maxConcurrentRequests || 10}</td>
                    <td>${maxModels}</td>
                    <td>${badge(b.hasAgent ? 'Да' : 'Нет', b.hasAgent ? 'success' : 'warning')}</td>
                    <td>${escapeHtml(labels)}</td>
                    <td>${escapeHtml(lastContact)}</td>
                    <td>${badge(b.status, b.status === 'healthy' ? 'success' : 'danger')}</td>
                    <td>
                        <button class=\"action-btn edit\" onclick=\"ui.editBackend('${escapeHtml(b.id)}')\">Edit</button>
                        <button class=\"action-btn delete\" onclick=\"ui.confirmDeleteBackend('${escapeHtml(b.id)}')\">Del</button>
                    </td>
                </tr>
            `;
        }).join('');
    }

    // ---- Models Page ----

    function modelsPage(backends) {
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
        if (!backends.length) return loading('Нет данных');

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
                <div class=\"backend-load-bar-row\">
                    <span class=\"backend-load-bar-label\">VRAM ${formatMB(usedVRAM)} / ${formatMB(totalVRAM)}</span>
                    <span class=\"backend-load-bar-value\">${vramPercent.toFixed(1)}%</span>
                </div>
                <div class=\"backend-load-bar\"><div class=\"backend-load-fill vram\" style=\"width: ${vramPercent}%\"></div></div>
            ` : '';

            return `
                <div class=\"backend-load-item\">
                    <div class=\"backend-load-header\">
                        <span class=\"backend-load-name\"><strong>${escapeHtml(b.id)}</strong></span>
                        ${badge(b.status, b.status === 'healthy' ? 'success' : 'danger')}
                    </div>
                    <div class=\"backend-load-stats\">
                        <span class=\"backend-load-stat\">Моделей: <strong>${models.length}</strong></span>
                        <span class=\"backend-load-stat\">Active: <strong>${activeReq}/${maxReq}</strong></span>
                        <span class=\"backend-load-stat\">Свободно: <strong>${freeSlots}</strong></span>
                    </div>
                    ${vramBar}
                    <div class=\"backend-load-bar-row\">
                        <span class=\"backend-load-bar-label\">RAM ${formatMB(usedRAM)} / ${formatMB(totalRAM)}</span>
                        <span class=\"backend-load-bar-value\">${ramPercent.toFixed(1)}%</span>
                    </div>
                    <div class=\"backend-load-bar\"><div class=\"backend-load-fill ram\" style=\"width: ${ramPercent}%\"></div></div>
                </div>
            `;
        }).join('');
    }

    function modelsGrid(allModels, backendMap) {
        if (!allModels.length) return loading('Нет загруженных моделей');

        return allModels.map(m => {
            const vramMB = (m.vramUsage || 0) / 1024 / 1024;
            const ramMB = (m.ramUsage || 0) / 1024 / 1024;
            const sizeGB = (m.size || 0) / 1024 / 1024 / 1024;

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

            return `
                <div class=\"model-card\">
                    <div class=\"model-card-header\">
                        <span class=\"model-name\">${escapeHtml(m.name)}</span>
                        ${badge(m.backend, m.backendStatus === 'healthy' ? 'success' : 'danger')}
                    </div>
                    <div class=\"model-size\">${sizeGB.toFixed(1)} GB</div>
                    <div class=\"model-details\">
                        ${modelDetail('VRAM', `${vramMB.toFixed(0)} MB`)}
                        ${modelDetail('RAM', `${ramMB.toFixed(0)} MB`)}
                        ${modelDetail('Family', m.family || '-')}
                    </div>
                    <div class=\"model-memory-section\">
                        <div class=\"model-memory-title\">Использование памяти</div>
                        ${showVRAM ? memoryBar('VRAM', vramMB, totalVRAM, vramPercent, 'vram') : ''}
                        ${memoryBar('RAM', ramMB, totalRAM, ramPercent, 'ram')}
                    </div>
                </div>
            `;
        }).join('');
    }

    function modelDetail(label, value) {
        return `
            <div class=\"model-detail\">
                <div class=\"model-detail-label\">${escapeHtml(label)}</div>
                <div class=\"model-detail-value\">${escapeHtml(String(value))}</div>
            </div>
        `;
    }

    function memoryBar(label, used, total, pct, type) {
        return `
            <div class=\"model-memory-bar-container\">
                <div class=\"model-memory-bar-labels\">
                    <span>${escapeHtml(label)}</span>
                    <span>${used.toFixed(0)} / ${total.toFixed(0)} MB (${pct.toFixed(1)}%)</span>
                </div>
                <div class=\"model-memory-bar\">
                    <div class=\"model-memory-bar-fill ${type}\" style=\"width: ${pct}%\"></div>
                </div>
            </div>
        `;
    }

    // ---- Sessions Page ----

    function sessionsPage(sessions) {
        const tbody = document.getElementById('sessionsTableBody');
        if (!tbody) return;
        if (!sessions.length) {
            tbody.innerHTML = emptyRow(6, 'Нет активных сессий');
            return;
        }
        tbody.innerHTML = sessions.map(s => `
            <tr>
                <td><code>${escapeHtml(s.id?.substring(0, 16) || 'N/A')}...</code></td>
                <td>${escapeHtml(s.backendId || '-')}</td>
                <td>${escapeHtml(s.model || '-')}</td>
                <td>${s.requestCount || 0}</td>
                <td>${s.lastActivity ? new Date(s.lastActivity).toLocaleString('ru') : '-'}</td>
                <td>${badge(s.active ? 'Активна' : 'Неактивна', s.active ? 'success' : 'warning')}</td>
            </tr>
        `).join('');
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
        Utils.setText('queueAvgWait', avgWait > 0 ? (avgWait / 1000).toFixed(1) + ' с' : '-');

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
        if (!tasks.length) return emptyRow(5, 'Нет задач в очереди');
        return tasks.map((task, index) => {
            const status = task.status || 'pending';
            const statusClass = status === 'processing' ? 'processing' : (status === 'completed' ? 'completed' : 'pending');
            const statusText = status === 'processing' ? 'Обработка' : (status === 'completed' ? 'Завершено' : 'Ожидание');
            const waitTime = task.waitTimeMs ? (task.waitTimeMs / 1000).toFixed(1) + 'с' : '-';
            return `
                <tr>
                    <td>${index + 1}</td>
                    <td><code>${escapeHtml(task.model || '-')}</code></td>
                    <td>${escapeHtml(task.backend || 'Auto')}</td>
                    <td>${waitTime}</td>
                    <td><span class=\"queue-task-status ${statusClass}\">${statusText}</span></td>
                </tr>
            `;
        }).join('');
    }

    function queueHistoryBody(history) {
        const badge = document.getElementById('queueHistoryCount');
        if (badge) badge.textContent = history.length;
        if (!history.length) return emptyRow(6, 'Нет выполненных задач');

        return history.map((task, index) => {
            const enqueued = task.enqueued ? new Date(task.enqueued).toLocaleString('ru', { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '-';
            const completed = task.completed_at ? new Date(task.completed_at).toLocaleString('ru', { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '-';
            const waitMs = task.wait_time_ms || 0;
            const waitStr = waitMs > 0 ? (waitMs / 1000).toFixed(1) + ' с' : '-';
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
            container.innerHTML = loading('Нет логов');
            return;
        }
        container.innerHTML = logEntries.map(log => `
            <div class=\"log-entry\">
                <span class=\"log-time\">${escapeHtml(log.time)}</span>
                <span class=\"log-level ${log.level.toLowerCase()}\">${escapeHtml(log.level)}</span>
                <span class=\"log-message\">${escapeHtml(log.message)}</span>
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
            container.innerHTML = '<div class=\"alert alert-info\">Нет активных предупреждений</div>';
            return;
        }
        container.innerHTML = alerts.map(a => `
            <div class=\"alert alert-${a.level}\">
                <svg class=\"alert-icon-svg\" viewBox=\"0 0 24 24\"><circle cx=\"12\" cy=\"12\" r=\"10\" fill=\"currentColor\"/></svg>
                <span><strong>${escapeHtml(a.backend)}</strong>: ${escapeHtml(a.reason)} через ${a.seconds}с</span>
            </div>
        `).join('');
    }

    // ---- Public API ----
    return {
        dashboard,
        backendsPage,
        modelsPage,
        sessionsPage,
        queuePage,
        logs,
        predictionAlerts
    };
})();