// renderers-cluster.js — R651 (2026-11-20): дашборд, GPU-кластер, ёмкость,
// «доступные модели» и таблица бэкендов вынесены из `renderers.js` (цель §5 — < 1500).
//
// ЗАЧЕМ ВЫНЕСЕНО. Это обзор кластера: карточки дашборда, GPU-карточки, полосы
// ёмкости и таблица бэкендов. Они читают ОДНИ И ТЕ ЖЕ поля метрик и меняются
// вместе (R346/R348/R389: загрузка и разметка панелей), а не когда меняется,
// например, форма параметров загрузки модели.
//
// ПРОГНОЗ НАГРУЗКИ ЖИВЁТ ЗДЕСЬ. `_predCache` + PRED_THRESHOLD/PRED_HYSTERESIS
// (гистерезис 45 с, порог 300 с) были в renderers.js, но использовались только
// этой группой — переехали вместе с ней, чтобы состояние прогноза не осталось в
// файле, который больше ничего о прогнозе не знает.
//
// ПОЧЕМУ ФАБРИКА. Слой берёт из `renderers.js` 20 имён (var ctx.* ниже); список
// посчитан `scripts/webui_extract_deps.js`, а не «на глаз» (R643/R644).
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: тела перенесены как есть, отступ сохранён.
(function () {
    'use strict';

    window.RenderersCluster = function (ctx) {
        var Utils = ctx.Utils;
        // R671b: без этой строки рендер дашборда падал (см. renderers.js).
        var badge = ctx.badge;
        var _t = ctx._t;
        var computeMemorySplit = ctx.computeMemorySplit;
        var donorBadgeHtml = ctx.donorBadgeHtml;
        var emptyRow = ctx.emptyRow;
        var escapeHtml = ctx.escapeHtml;
        var formatMB = ctx.formatMB;
        var formatNumber = ctx.formatNumber;
        var getBackendMode = ctx.getBackendMode;
        var getBackendModeBadge = ctx.getBackendModeBadge;
        var getBackendTypeBadge = ctx.getBackendTypeBadge;
        var getGPUStatus = ctx.getGPUStatus;
        var getProgressClass = ctx.getProgressClass;
        var isZeroTimestamp = ctx.isZeroTimestamp;
        var loading = ctx.loading;
        var parseQuantizationFromName = ctx.parseQuantizationFromName;
        var percent = ctx.percent;
        var queueView = ctx.queueView;
        var stableBackendOrder = ctx.stableBackendOrder;
        var statusBadge = ctx.statusBadge;

        // Состояние прогноза нагрузки (R389): перенесено из renderers.js как есть.
        var _predCache = {};
        var PRED_HYSTERESIS = 45; // seconds — must deviate this much from threshold to switch
        var PRED_THRESHOLD = 300; // seconds

    // ---- Dashboard ----

    function dashboard(backends, sessions, queue, loadedModels) {
        const healthy = backends.filter(b => b.status === 'healthy');
        // Если извне передан cluster-loaded count (рекомендуемый путь) — используем его.
        // Иначе считаем сами по бэкендам.
        //
        // R83 (2026-09-25): раньше fallback учитывал ТОЛЬКО ollama.runningModels,
        // поэтому на чисто llama.cpp-стенде дашборд показывал «ЗАГРУЖЕНО МОДЕЛЕЙ 0»,
        // хотя карточка Ollama Runtime на той же странице рисовала модель из
        // backend.llamaCpp.loadedModels (см. runtimeCluster ниже). Живой скрин с A10:
        // счётчик 0 при загруженной Qwen3.8-27B-UD-Q4_K_M (C-65.5K).
        //
        // R91 (2026-10-09): для llama.cpp берём ТОЛЬКО loadedModels — раньше сюда
        // складывались оба списка (`runningModels.length + loadedModels.length`), и
        // на одной загруженной модели счётчик показывал 2: ту же модель балансер
        // отдавал ещё и в Ollama-совместимой проекции (см. cluster_state.go).
        const loadedCount = (b) => {
            const isLlama = Utils.getBackendType(b) === 'llama_cpp';
            if (isLlama) {
                return (b.llamaCpp?.loadedModels?.length || 0);
            }
            return (b.ollama?.runningModels?.length || 0) + (b.llamaCpp?.loadedModels?.length || 0);
        };
        const totalModels = (typeof loadedModels === 'number')
            ? loadedModels
            : backends.reduce((sum, b) => sum + loadedCount(b), 0);
        const activeSessions = (sessions || []).filter(s => s.active).length;
        const totalRequests = (sessions || []).reduce((sum, s) => sum + (s.requestCount || 0), 0);
        const queueSize = queueView(queue).waiting;
        const queueProcessed = queueView(queue).processed;
        const queueMax = queueView(queue).mode === 'admission' ? 100 : (queue?.max_size || 100);
        const queuePct = percent(queueSize, queueMax);

        Utils.setText('totalBackends', backends.length);
        Utils.setText('healthyBackends', _t('renderers.healthy_count', { count: healthy.length }));
        Utils.setText('totalModels', totalModels);
        Utils.setText('loadedModels', _t('renderers.models_count_loaded', { count: totalModels }));
        Utils.setText('totalSessions', activeSessions);
        Utils.setText('sessionRate', _t('renderers.requests_count', { count: totalRequests }));
        Utils.setText('queueSize', queueSize);
        // R91: подпись под числом очереди. В admission-режиме легаси-счётчик
        // «обработано» всегда ноль, поэтому показываем РЕАЛЬНО обслуженные
        // запросы (served_total) — иначе карточка врёт «0 processed» при
        // работающей очереди. Дубликат id queueProcessed (дашборд и страница
        // «Очередь») устранён: у дашборда он теперь dashboardQueueProcessed.
        Utils.setText('dashboardQueueProcessed', _t('renderers.processed_count', { count: queueProcessed }));
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
            // Round 11 (2026-07-10): render flags for both Ollama AND llama.cpp backends.
            // Before: cppworker cards only showed "default" — operators couldn't see
            // gpu_layers/n_ctx/parallel/kv_cache_type for the most important backend type.
            const backendType = Utils.getBackendType(backend);
            const isOllama = backendType === 'ollama';
            const isLlamaCpp = backendType === 'llama_cpp';
            const flags = isOllama ? (backend.ollama?.runtimeFlags || {}) : {};
            const contexts = isOllama ? (backend.ollama?.modelContexts || []) : [];
            // For llama.cpp we synthesize flag badges from the FIRST loaded model —
            // cppworker reports per-model runtime state, not process-level flags.
            const cppLoaded = isLlamaCpp && Array.isArray(backend.llamaCpp?.loadedModels)
                ? backend.llamaCpp.loadedModels
                : [];

            const flagBadges = [];

            // === Ollama process-level flags ===
            if (isOllama) {
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
            }

            // === llama.cpp per-loaded-model summary ===
            // Show the first loaded model's stats as the cluster summary; if multiple
            // are loaded, append a "+N more" indicator so the operator knows to open
            // the Models tab for full detail.
            if (isLlamaCpp && cppLoaded.length) {
                const m = cppLoaded[0];
                if (m.numGpuLayers !== undefined && m.numGpuLayers !== 0) {
                    const cls = m.numGpuLayers === -1 ? '' : 'warning';
                    flagBadges.push(`<span class="flag-badge ${cls}" title="${escapeHtml(m.name)}">GPU:${m.numGpuLayers === -1 ? 'auto' : m.numGpuLayers}</span>`);
                }
                if (m.contextLength) {
                    flagBadges.push(`<span class="flag-badge" title="${escapeHtml(m.name)}">C:${formatNumber(m.contextLength)}</span>`);
                }
                if (m.batchSize && m.batchSize !== 512) {
                    flagBadges.push(`<span class="flag-badge">B:${m.batchSize}</span>`);
                }
                if (m.quantization) {
                    flagBadges.push(`<span class="flag-badge">Q:${escapeHtml(m.quantization)}</span>`);
                }
                if (cppLoaded.length > 1) {
                    flagBadges.push(`<span class="flag-badge" title="${escapeHtml(cppLoaded.slice(1).map(x => x.name).join(', '))}">+${cppLoaded.length - 1}</span>`);
                }
            }

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

            // For llama.cpp, render per-loaded-model context info (since we don't have
            // modelContexts from the agent — cppworker reports it per-model in loadedModels).
            const cppContextBadges = isLlamaCpp && cppLoaded.length ? cppLoaded.map(m => `
                <span class="context-info">
                    ${escapeHtml(m.name)}: Ctx ${formatNumber(m.contextLength || 0)}
                    <div class="context-tooltip">
                        ${tooltipRow('Quantization', m.quantization || '-')}
                        ${tooltipRow('GPU Layers', m.numGpuLayers === -1 ? 'auto' : (m.numGpuLayers || 0))}
                        ${tooltipRow('Batch Size', m.batchSize || '-')}
                        ${tooltipRow('VRAM', formatMB(m.vramUsage))}
                        ${tooltipRow('RAM', formatMB(m.ramUsage))}
                        ${tooltipRow('State', m.state || 'unknown')}
                    </div>
                </span>
            `).join('') : '';

            return `
                <div class="runtime-card">
                    <div class="runtime-header">
                        <strong>${escapeHtml(backend.id)}</strong>
                        ${statusBadge(backend.status)}
                    </div>
                    <div class="runtime-host">${escapeHtml(backend.host || '-')}</div>
                    <div class="runtime-flags">
                        ${flagBadges.length ? flagBadges.join('') : '<span class="flag-badge">default</span>'}
                    </div>
                    ${contextBadges ? `<div style="margin-top:0.5rem;">${contextBadges}</div>` : ''}
                    ${cppContextBadges ? `<div style="margin-top:0.5rem;">${cppContextBadges}</div>` : ''}
                    ${(window.AutoTuneUI && (backend.llamaCpp?.autoTune || backend.autoTune))
                        ? `<div style="margin-top:0.5rem;">${window.AutoTuneUI.renderBackendAutoTune(backend)}</div>`
                        : ''}
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
        // R88: 0 = «датчик не отдал значение», а не «0 ватт». До этой правки панель
        // показывала «POWER 0W» на всех карточках, хотя nvidia-smi отдаёт 22–47 W
        // (в агенте исправлен парсинг дробного значения). Если значение всё же
        // неизвестно (карта без датчика мощности) — честный прочерк, а не ноль.
        const power = (gpu.powerUsage !== undefined && gpu.powerUsage > 0) ? gpu.powerUsage : '-';
        const powerLimit = gpu.powerLimit !== undefined && gpu.powerLimit > 0 ? gpu.powerLimit : '-';
        const gpuClock = gpu.gpuClock !== undefined && gpu.gpuClock > 0 ? gpu.gpuClock : '-';
        const memClock = gpu.memClock !== undefined && gpu.memClock > 0 ? gpu.memClock : '-';
        const status = getGPUStatus(usage, vramPct, temp);
        const reqCap = pred.requestCapacity !== undefined ? pred.requestCapacity.toFixed(0) + '%' : '-';
        // R90: ОПЕРАТИВНАЯ ПАМЯТЬ ХОСТА. Агенты её и раньше отдавали
        // (system.memoryUsed/memoryTotal), но в GPU-карточке её не было — сводка по
        // бэкенду выглядела неполной: видно VRAM (память карты), но не видно,
        // сколько RAM занято на самой машине (а для image-моделей с offload в RAM
        // это и есть главный ресурс).
        const sys = backend.system || {};
        const ramTotal = sys.memoryTotal || 0;
        const ramUsed = sys.memoryUsed || 0;
        const ramPct = ramTotal > 0 ? percent(ramUsed, ramTotal) : null;
        const ramHint = ramPct !== null
            ? _t('renderers.ram_hint') + ' ' + formatMB(ramUsed) + ' / ' + formatMB(ramTotal)
            : _t('renderers.ram_unknown');
        // Capacity — это ПРОГНОЗ ЗАГРУЗКИ конкретного бэкенда (по его истории
        // запросов), а НЕ метрика видеокарты. Два бэкенда на одной карте дают
        // разные значения по определению; без подписи это выглядело как «агенты
        // показывают разное про одну GPU».
        const capHint = _t('renderers.load_forecast_hint');
        const cardHint = _t('renderers.gpu_card_sample_hint');
        return `
            <div class="gpu-card" title="${escapeHtml(cardHint)}">
                <div class="gpu-card-header">
                    <span class="gpu-card-title">${escapeHtml(backend.id)}</span>
                    <span class="gpu-status ${status}"></span>
                </div>
                <div class="gpu-metrics">
                    ${metric('Host', escapeHtml(backend.host || '-'))}
                    ${metric('GPU', `${usage.toFixed(1)}%`)}
                    ${metric('VRAM', `${vramPct.toFixed(1)}%`)}
                    ${metric('RAM', ramPct !== null ? `${ramPct.toFixed(1)}%` : '-', ramHint)}
                    ${metric('Temp', `${temp}°C`)}
                    ${metric('Power', power !== '-' ? `${power}W${powerLimit !== '-' ? ' / ' + powerLimit + 'W' : ''}` : '-')}
                    ${metric('Clock', `${gpuClock !== '-' ? gpuClock + ' MHz' : '-'}`)}
                    ${metric('MemClk', `${memClock !== '-' ? memClock + ' MHz' : '-'}`)}
                    ${metric(_t('renderers.load_forecast'), reqCap, capHint)}
                </div>
                <div class="progress-bar"><div class="progress-fill ${getProgressClass(usage)}" style="width: ${usage}%"></div></div>
            </div>
        `;
    }

    function metric(label, value, title) {
        return `
            <div class="gpu-metric"${title ? ` title="${escapeHtml(title)}"` : ''}>
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
                        <div class="capacity-cloud-row"><span>${_t('metrics.status')}</span><span>${statusBadge(backend.status)}</span></div>
                        <div class="capacity-cloud-row"><span>${_t('renderers.active_req')}</span><span>${backend.activeRequests || 0}</span></div>
                        <div class="capacity-cloud-row"><span>${_t('renderers.models_loaded')}</span><span>${(Utils.getBackendType(backend) === 'llama_cpp'
                            ? (backend.llamaCpp?.loadedModels || [])
                            : (backend.ollama?.runningModels || [])).length}</span></div>
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
            const isLlamaCpp = Utils.getBackendType(b) === 'llama_cpp';
            if (isLlamaCpp) {
                // Round 18d (2026-07-10): для llama.cpp бэкендов показываем
                // загруженные модели (loadedModels) — это «доступные сейчас» модели.
                // Каждая карточка показывает «Loaded @ backend», оператор видит что
                // уже в памяти cppworker. Раньше для llama.cpp карточка была пустой
                // (использовался только b.ollama.backendCapacity.availableModels).
                const cppLoaded = (b.llamaCpp && Array.isArray(b.llamaCpp.loadedModels))
                    ? b.llamaCpp.loadedModels : [];
                cppLoaded.forEach(function (lm) {
                    // Парсим quantization из имени (cppworker не отдаёт).
                    var quant = lm.quantization || parseQuantizationFromName(lm.name) || '';
                    // Считаем estimated VRAM если cppworker не сообщил.
                    var split = computeMemorySplit(lm);
                    var vramMB = split.vramBytes > 0 ? Math.round(split.vramBytes / 1024 / 1024) : 0;
                    allModels.push({
                        name: lm.name,
                        backendId: b.id,
                        backendStatus: b.status,
                        // Для llama.cpp «canLoad» = уже загружена.
                        canLoad: true,
                        // estimatedVram в MB (для совместимости с formatMB).
                        estimatedVram: vramMB * 1024 * 1024,
                        // Доп. поля для отображения.
                        backendType: 'llama_cpp',
                        loaded: lm.state === 'loaded' || !lm.state,
                        contextLength: lm.contextLength || 0,
                        numGpuLayers: lm.numGpuLayers || 0,
                        quantization: quant,
                        // R83 (2026-09-25): пробрасываем границы контекста.
                        // Раньше здесь собирался НОВЫЙ объект только из перечисленных
                        // полей, поэтому feasibleMaxContext/ggufMaxContext (которые
                        // балансер уже отдаёт в cluster state) до карточки не
                        // доходили — и оператор не видел, влезает ли n_ctx в VRAM.
                        feasibleMaxContext: lm.feasibleMaxContext || 0,
                        ggufMaxContext: lm.ggufMaxContext || lm.maxContext || 0
                    });
                    totalLoadable++;
                });
            } else {
                // Ollama: используем backendCapacity.availableModels.
                const models = b.ollama?.backendCapacity?.availableModels || [];
                models.forEach(m => {
                    allModels.push({ ...m, backendId: b.id, backendStatus: b.status, backendType: 'ollama' });
                    if (m.canLoad) totalLoadable++;
                });
            }
        });

        if (badge) badge.textContent = totalLoadable;

        if (!allModels.length) return loading(_t('renderers.no_available_models'));

        // Сортировка: loadable (true) выше, потом по VRAM asc.
        allModels.sort((a, b) => {
            if (a.canLoad !== b.canLoad) return b.canLoad - a.canLoad;
            return (a.estimatedVram || 0) - (b.estimatedVram || 0);
        });

        // Round 11 (2026-07-10): wrap in a scrollable container instead of hard-capping
        // at 50 models. Operators with 100+ models on a backend (typical for shared
        // llama.cpp fileservers) couldn't load anything past position 50.
        // CSS class .available-models-scroll caps height and enables vertical scroll.
        const items = allModels.map(m => {
            const cls = m.canLoad ? 'model-loadable' : 'model-unloadable';
            const vram = m.estimatedVram || 0;
            // Round 18d: для llama.cpp показываем quant + gpu layers в subtitle.
            var subtitle = '';
            if (m.backendType === 'llama_cpp') {
                var parts = [];
                if (m.contextLength) parts.push('C:' + formatNumber(m.contextLength));
                if (m.numGpuLayers && m.numGpuLayers !== -1) parts.push('GPU:' + m.numGpuLayers);
                else if (m.numGpuLayers === -1) parts.push('GPU:all');
                if (m.quantization) parts.push(m.quantization);
                subtitle = parts.join(' · ');
            }
            var rightText = subtitle
                ? '<span class="model-vram">' + formatMB(vram) + ' · ' + escapeHtml(subtitle) + '</span>' +
                  '<span class="model-backend">@ ' + escapeHtml(m.backendId) + '</span>'
                : '<span class="model-vram">' + formatMB(vram) + ' @ ' + escapeHtml(m.backendId) + '</span>';
            return `
                <div class="available-model-item ${cls}">
                    <div class="model-indicator"></div>
                    <span class="model-name">${escapeHtml(m.name)}</span>
                    ${rightText}
                </div>
            `;
        }).join('');

        return `<div class="available-models-scroll">${items}</div>`;
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
            // Round 18d (2026-07-10): для llama.cpp бэкендов показываем loadedModels
            // вместо runningModels (которые только для Ollama).
            const isLlamaCpp = Utils.getBackendType(b) === 'llama_cpp';
            // R-Image (2026-10-02): у image-бэкенда ни runningModels, ни
            // loadedModels - модели лежат в backend.image.models. Без этой ветки
            // колонка «Модели» показывала 0 у бэкенда с загруженной моделью.
            const isImage = Utils.getBackendType(b) === 'image_cpp';
            const imageModels = isImage ? Utils.getBackendImageModels(b) : [];
            const models = isImage
                ? imageModels.length
                : (isLlamaCpp
                    ? ((b.llamaCpp && Array.isArray(b.llamaCpp.loadedModels)) ? b.llamaCpp.loadedModels.length : 0)
                    : (oll.runningModels?.length || 0));
            // Model details tooltip for the Models column
            // Format expiresAt for display
            function fmtExpires(exp) {
                if (isZeroTimestamp(exp)) return '';
                var d = new Date(exp);
                var now = Date.now();
                var diff = d.getTime() - now;
                if (diff < 0) return ' ⌛Expired';
                if (diff < 60000) return ' ⌛' + Math.round(diff/1000) + 's';
                if (diff < 3600000) return ' ⌛' + Math.round(diff/60000) + 'm';
                return ' ⌛' + Math.round(diff/3600000) + 'h';
            }
            // Round 18d (2026-07-10): для llama.cpp бэкендов берём loadedModels иначе tooltip пустой.
            const modelsList = isImage
                ? imageModels
                : (isLlamaCpp
                    ? ((b.llamaCpp && Array.isArray(b.llamaCpp.loadedModels)) ? b.llamaCpp.loadedModels : [])
                    : (oll.runningModels || []));
            const modelDetailsHtml = modelsList.map(function(m) {
                if (isImage) {
                    // Tooltip: имя - состояние | размер | оценка VRAM | активные запросы.
                    var imgParts = [Utils.imageStateLabel(m.state)];
                    var imgSize = Utils.imageSizeBytes(m);
                    var imgVram = Utils.imageVramEstimateMb(m);
                    if (imgSize > 0) imgParts.push(Utils.formatMB(imgSize / 1024 / 1024));
                    if (imgVram > 0) imgParts.push('VRAM ' + Utils.formatMB(imgVram));
                    if (m.activeQueries) imgParts.push('q:' + m.activeQueries);
                    return '<div class="model-tooltip-row">' + Utils.escapeHtml(m.name) + ' — ' +
                        Utils.escapeHtml(m.family ? (m.family + ' | ' + imgParts.join(' | ')) : imgParts.join(' | ')) +
                        '</div>';
                }
                if (isLlamaCpp) {
                    var ctx = m.contextLength ? 'C:' + formatNumber(m.contextLength) : '';
                    var gpuL = m.numGpuLayers === -1 ? 'GPU:all' : (m.numGpuLayers ? 'GPU:' + m.numGpuLayers : '');
                    var q = m.quantization || parseQuantizationFromName(m.name) || '';
                    var runtime = [ctx, gpuL, q].filter(Boolean).join(' | ');
                    return '<div class="model-tooltip-row">' + Utils.escapeHtml(m.name) + ' — ' +
                        (runtime ? Utils.escapeHtml(runtime) : 'loaded') +
                        '</div>';
                }
                var expStr = fmtExpires(m.expiresAt || m.ExpiresAt);
                return '<div class="model-tooltip-row">' + Utils.escapeHtml(m.name) + ' — ' +
                    Utils.escapeHtml(m.family || '-') + ' | ' +
                    Utils.escapeHtml(m.parameterSize || '-') + ' | ' +
                    Utils.escapeHtml(m.quantization || '-') +
                    (expStr ? ' ' + expStr : '') +
                    '</div>';
            }).join('');
            const modelDetailsTitle = modelsList.map(function(m) {
                if (isImage) {
                    var imgTitleParts = [Utils.imageStateLabel(m.state)];
                    var imgTitleSize = Utils.imageSizeBytes(m);
                    var imgTitleVram = Utils.imageVramEstimateMb(m);
                    if (imgTitleSize > 0) imgTitleParts.push(Utils.formatMB(imgTitleSize / 1024 / 1024));
                    if (imgTitleVram > 0) imgTitleParts.push('VRAM ' + Utils.formatMB(imgTitleVram));
                    return (m.name || '-') + ' — ' + imgTitleParts.join(' | ');
                }
                if (isLlamaCpp) {
                    var ctx = m.contextLength ? 'C:' + formatNumber(m.contextLength) : '';
                    var gpuL = m.numGpuLayers === -1 ? 'GPU:all' : (m.numGpuLayers ? 'GPU:' + m.numGpuLayers : '');
                    var q = m.quantization || parseQuantizationFromName(m.name) || '';
                    var runtime = [ctx, gpuL, q].filter(Boolean).join(' | ');
                    return (m.name || '-') + ' — ' + (runtime || 'loaded');
                }
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
                    <td><div class="backend-id-cell"><span class="backend-id-name">${escapeHtml(b.id)}</span><div class="backend-id-badges">${getBackendTypeBadge(b)} ${getBackendModeBadge(b)}</div></div></td>
                    <td>${statusBadge(b.status)}${donorBadgeHtml(b, backends)}</td>
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
                        <button class="action-btn edit" data-admin-only onclick="ui.editBackend('${escapeHtml(b.id)}')">${_t('common.edit')}</button>
                        <button class="action-btn delete" data-admin-only onclick="ui.confirmDeleteBackend('${escapeHtml(b.id)}')">${_t('common.delete')}</button>
                    </td>
                </tr>
            `;
        }).join('');
    }

        return {
            dashboard: dashboard,
            runtimeCluster: runtimeCluster,
            gpuCluster: gpuCluster
        };
    };
})();
