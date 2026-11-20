// renderers-pages.js — R649 (2026-11-20): страницы «Бэкенды», «Модели», «Сессии»,
// «Агенты» и «GGUF» вынесены из `renderers.js` (цель §5 — < 1500 строк).
//
// ЗАЧЕМ ВЫНЕСЕНО. Это самый крупный связный слой `renderers.js`: пять страниц рисуют
// таблицы и карточки по данным поллера и меняются вместе (R-Image, R346/R348, R211,
// R229). Остальной файл — карточки дашборда, GPU-кластер, ёмкость, логи и очередь,
// у него другая причина меняться.
//
// ПОЧЕМУ ФАБРИКА, А НЕ ГЛОБАЛЬНЫЙ ОБЪЕКТ. Слой берёт из `renderers.js` 23 имени (var
// ctx.* ниже). Список получен `scripts/webui_extract_deps.js`, а не «на глаз»: три
// прошлых выноса падали ReferenceError-ом ровно потому, что зависимости собирали по
// `function`-объявлениям (R640 backendChoices, R643 escapeHtml — деструктуризация
// Utils, R643 OLLAMA_PARAMS — const-таблица).
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: тела перенесены как есть, отступ сохранён.
(function () {
    'use strict';

    window.RenderersPages = function (ctx) {
        var Utils = ctx.Utils;
        var _t = ctx._t;
        var backendLoad = ctx.backendLoad;
        var badge = ctx.badge;
        var computeMemorySplit = ctx.computeMemorySplit;
        var dedupModelCards = ctx.dedupModelCards;
        var donorBadgeHtml = ctx.donorBadgeHtml;
        var donorDetailHtml = ctx.donorDetailHtml;
        var emptyRow = ctx.emptyRow;
        var escapeHtml = ctx.escapeHtml;
        var formatUptime = ctx.formatUptime;
        var getBackendTypeBadge = ctx.getBackendTypeBadge;
        var getClientIcon = ctx.getClientIcon;
        var loading = ctx.loading;
        var logs = ctx.logs;
        var modelsGrid = ctx.modelsGrid;
        var parseQuantizationFromName = ctx.parseQuantizationFromName;
        var protocolBadge = ctx.protocolBadge;
        var renderGgufCardsInto = ctx.renderGgufCardsInto;
        var renderOllamaParams = ctx.renderOllamaParams;
        var stableBackendOrder = ctx.stableBackendOrder;
        var statusBadge = ctx.statusBadge;
        var statusLabel = ctx.statusLabel;

    /**
     * R-Image (2026-10-02): детали image-бэкенда для раскрытой строки страницы
     * «Бэкенды». Ollama- и llama.cpp-секции для него неприменимы (нет
     * runtimeFlags, backendCapacity, loadedModels), поэтому оператор видел
     * почти пустую строку, хотя /api/v1/metrics уже отдаёт:
     *   imagePort, image.state, image.currentModel, image.vramFreeMb/vramTotalMb,
     *   image.updatedAt, image.lastError, image.models[{name,state,family,
     *   sizeBytes,vramEstimateMb,activeQueries}].
     * Показываем ровно это. Значения экранируем: строки приходят от воркера.
     */
    function backendsPage(backends) {
        const tbody = document.getElementById('backendsManageBody');
        if (!tbody) return;
        if (!backends.length) {
            tbody.innerHTML = emptyRow(14, _t('renderers.no_data'));
            return;
        }

        tbody.innerHTML = backends.map((b, idx) => {
            const labels = (b.labels || []).join(', ') || '-';
            // Utils.formatDate — не только «пусто ли поле»: Go отдаёт нулевое
            // время строкой "0001-01-01T00:00:00Z", и раньше у image-бэкенда
            // (без агента) в этой колонке стояло «01.01.1, 02:30:17».
            const lastContact = Utils.formatDate(b.lastAgentContact);
            const maxModels = b.runtimeMaxModels || b.maxModels || b.ollama?.maxModels || '-';
            const detailsHtml = renderOllamaParams(b) + donorDetailHtml(b, backends);
            const rowId = 'be-row-' + idx;
            const safeId = escapeHtml(b.id);
            // R-Image (2026-10-02): у image-бэкенда порт воркера - imagePort.
            // cppWorkerPort у него 0, а ollamaPort приходит из конфига (11434) и
            // к sd-server отношения не имеет: оператор шёл по нему и не находил
            // воркер. Поэтому для image-строки показываем imagePort.
            const isImageRow = Utils.getBackendType(b) === 'image_cpp';
            const imagePort = isImageRow ? Utils.getBackendImagePort(b) : 0;
            const portHtml = isImageRow
                ? (imagePort > 0
                    ? '<span title="' + escapeHtml(_t('image.worker_port')) + '">' + imagePort + ' 🎨</span>'
                    : '<span title="' + escapeHtml(_t('image.worker_port')) + '">' + escapeHtml(_t('image.port_unset')) + '</span>')
                : String(Utils.getBackendType(b) === 'llama_cpp' ? (b.cppWorkerPort || b.port || 8080) : (b.ollamaPort || 11434));

            // ==== Loading column («вкладка бэкендов» — Issue: «отображение загрузки») ====
            // R346 (2026-10-30): удалён третий путь `b.llamaCppMetrics.loadingModels`.
            // Такого ключа не отдаёт НИ ОДИН эндпоинт (ни /api/v1/cluster, ни
            // /api/v1/backends: метрики llama.cpp приходят как `llamaCpp`), поэтому
            // ветка была мёртвой и лишь маскировала отсутствие данных: пустой массив
            // выглядел как «источник есть, но моделей нет».
            // Источники: 1) b.loadingModels (из /api/v1/backends),
            //            2) b.loadingModelCount (только счётчик).
            const loadingArr = (b.loadingModels && Array.isArray(b.loadingModels) && b.loadingModels.length > 0)
                ? b.loadingModels
                : [];
            const loadingCount = (b.loadingModelCount != null) ? b.loadingModelCount : loadingArr.length;
            let loadingCell;
            if (loadingCount > 0 && loadingArr.length === 0) {
                // Только счётчик известен
                loadingCell = '<span class="badge" style="background:rgba(74,158,255,0.12);color:#4a9eff;border-color:rgba(74,158,255,0.3)" title="' +
                    _t('gguf.col_loading_count', { count: loadingCount }) + '"><i class="fas fa-spinner fa-spin"></i> ' + loadingCount + '</span>';
            } else if (loadingArr.length > 0) {
                const cells = loadingArr.slice(0, 2).map(function (lm) {
                    const startedAt = lm.loadingStartedAt ? new Date(lm.loadingStartedAt).getTime() : Date.now();
                    const elapsedMs = (lm.elapsedMs && lm.elapsedMs > 0) ? lm.elapsedMs : (Date.now() - startedAt);
                    const elapsedSec = Math.max(0, Math.floor(elapsedMs / 1000));
                    const elapsedLabel = elapsedSec < 60
                        ? elapsedSec + 's'
                        : Math.floor(elapsedSec / 60) + 'm ' + (elapsedSec % 60) + 's';
                    if (lm.state === 'error') {
                        return '<span class="badge badge-red" title="' + escapeHtml(lm.error || 'error') + '" style="margin-right:4px;">' +
                            '<i class="fas fa-times-circle"></i> ' + escapeHtml(lm.name) +
                        '</span>';
                    }
                    // R346: размер загружаемого файла — в подсказке бейджа.
                    // ЗАЧЕМ. «Загрузка 3 минуты» без размера не отличает нормальную
                    // загрузку 8-гигабайтной модели от зависшей на 300 МБ; размер
                    // уже приходит от воркера (loadingSizeBytes), читать его было некому.
                    var loadSizeMb = Number(lm.loadingSizeBytes || 0) / (1024 * 1024);
                    var loadSizeLabel = loadSizeMb > 0
                        ? (loadSizeMb >= 1024 ? (loadSizeMb / 1024).toFixed(1) + ' GB' : Math.round(loadSizeMb) + ' MB')
                        : '';
                    return '<span class="badge" style="background:rgba(74,158,255,0.12);color:#4a9eff;border-color:rgba(74,158,255,0.3);margin-right:4px;" title="' +
                        _t('gguf.loading_indicator') + ': ' + escapeHtml(lm.name) +
                        (loadSizeLabel ? ' · ' + escapeHtml(loadSizeLabel) : '') + '">' +
                        '<i class="fas fa-spinner fa-spin"></i> ' + escapeHtml(lm.name) + ' ' + elapsedLabel +
                    '</span>';
                }).join('');
                const more = loadingArr.length > 2 ? (' +' + (loadingArr.length - 2)) : '';
                loadingCell = cells + more;
            } else {
                loadingCell = '<span style="color:var(--text-secondary)">-</span>';
            }

            const agentActionsHtml = b.hasAgent ? `
                <div class="agent-actions">
                    <button class="btn btn-restart" data-admin-only onclick="ui.restartAgent('${safeId}')" title="${_t('agents.restart_hint')}">${_t('agents.restart')}</button>
                    <button class="btn btn-logs" onclick="ui.viewAgentLogs('${safeId}')" title="${_t('agents.view_logs_hint')}">${_t('agents.view_logs')}</button>
                    <button class="btn btn-config" onclick="ui.showAgentDetails('${safeId}')" title="${_t('agents.config_hint')}">${_t('agents.config')}</button>
                </div>
            ` : '';

            return `
                <tr class="be-main-row" data-expand="${rowId}" style="cursor:pointer;">
                    <td><div class="backend-id-cell"><span class="backend-id-name">${escapeHtml(b.id)}</span><div class="backend-id-badges">${getBackendTypeBadge(b)} <span class="be-expand-icon">▶</span></div></div></td>
                    <td>${escapeHtml(b.name || b.id)}</td>
                    <td>${escapeHtml(b.host)}</td>
                    <td>${portHtml}</td>
                    <td>${b.agentPort || 18032}</td>
                    <td>${b.weight || 1}</td>
                    <td>${b.maxConcurrentRequests || 10}</td>
                    <td>${maxModels}</td>
                    <td>${badge(b.hasAgent ? _t('common.yes') : _t('common.no'), b.hasAgent ? 'success' : 'warning')}</td>
                    <td>${escapeHtml(labels)}</td>
                    <td>${escapeHtml(lastContact)}</td>
                    <td>${statusBadge(b.status)}${b.disabled
                        ? ' <span class="badge badge-warning" title="' +
                          escapeHtml(b.disabledReason || _t('backends.excluded_hint')) + '">' +
                          escapeHtml(_t('backends.excluded')) + '</span>'
                        : ''}${donorBadgeHtml(b, backends)}</td>
                    <td class="col-right" style="min-width:140px;max-width:240px;">${loadingCell}</td>
                    <td>
                        <button class="action-btn edit" data-admin-only onclick="event.stopPropagation(); ui.editBackend('${safeId}')">${_t('common.edit')}</button>
                        <button class="action-btn delete" data-admin-only onclick="event.stopPropagation(); ui.confirmDeleteBackend('${safeId}')">${_t('common.delete')}</button>
                        <button class="action-btn model" data-admin-only onclick="event.stopPropagation(); ui.openModelManageModal('${safeId}')" title="${_t('models.manage_title')}">${_t('models.manage_action')}</button>
                        <button class="action-btn" data-admin-only onclick="event.stopPropagation(); ui.toggleBackendEnabled('${safeId}', ${b.disabled ? 'true' : 'false'})" title="${b.disabled ? _t('backends.use_on_hint') : _t('backends.use_off_hint')}">${b.disabled ? _t('backends.use_on') : _t('backends.use_off')}</button>
                    </td>
                </tr>
                <tr class="be-detail-row" id="${rowId}" style="display:none;">
                    <td colspan="14">
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
            // Round 17 (2026-07-10): для Ollama бэкенда берём из b.ollama.runningModels.
            // Для llama.cpp бэкенда — из b.llamaCpp.loadedModels (cppworker poller).
            // Раньше читался ТОЛЬКО ollama → models tab был пустой для bundled-режима.
            const ollamaModels = (b.ollama && Array.isArray(b.ollama.runningModels)) ? b.ollama.runningModels : [];
            ollamaModels.forEach(m => {
                allModels.push({ ...m, backend: b.id, backendStatus: b.status });
            });
            const isLlamaCpp = Utils.getBackendType(b) === 'llama_cpp';
            if (isLlamaCpp) {
                const cppLoaded = (b.llamaCpp && Array.isArray(b.llamaCpp.loadedModels)) ? b.llamaCpp.loadedModels : [];
                cppLoaded.forEach(function (lm) {
                    // Round 18: cppworker doesn't track per-model VRAM/RAM.
                    // Вычисляем estimatedVram/estimatedRam из file size + GPU layer split.
                    // Formula:
                    //   layerRatio = numGpuLayers / nLayers  (0..1)
                    //   modelWeightsVRAM ≈ size * layerRatio
                    //   modelWeightsRAM  ≈ size * (1 - layerRatio)
                    //   kvCache ≈ 2 × nLayers × nKvHeads × headDim × 2 bytes × contextSize
                    //     (split по layerRatio аналогично — KV cache для GPU-слоёв в VRAM)
                    //   overhead ≈ 10% runtime buffer
                    const split = computeMemorySplit(lm);
                    // Round 18b: fallback — парсим quantization из имени если пусто
                    // (cppworker не отдаёт; backend-парсер уже пытается, но это страховка).
                    var lmQuant = lm.quantization || '';
                    if (!lmQuant) {
                        lmQuant = parseQuantizationFromName(lm.name) ||
                                  parseQuantizationFromName(lm.path || '') || '';
                    }
                    allModels.push({
                        name: lm.name,
                        // cppworker returns size in bytes, modelsGrid expects bytes
                        size: lm.size || 0,
                        // Round 18+: estimated VRAM/RAM (cppworker reports 0 for these)
                        // modelsGrid делает (m.vramUsage || 0) / 1024 / 1024 → ожидает bytes.
                        vramUsage: split.vramBytes,
                        ramUsage: split.ramBytes,
                        // cppworker doesn't track expiresAt / digest
                        digest: '',
                        expiresAt: '',
                        // GGUF-specific
                        quantization: lmQuant,
                        ggufPath: lm.path || '',
                        // Backend identification
                        backend: b.id,
                        backendStatus: b.status,
                        backendType: 'llama_cpp',
                        // Context size, gpu layers, batch size (for tooltip)
                        contextLength: lm.contextLength || 0,
                        numGpuLayers: lm.numGpuLayers || 0,
                        batchSize: lm.batchSize || 0,
                        state: lm.state || 'loaded',
                        // Round 18+: architecture metadata (для tooltip)
                        architecture: lm.architecture || '',
                        nLayers: lm.nLayers || 0,
                        nKvHeads: lm.nKvHeads || 0,
                        nEmbd: lm.nEmbd || 0,
                        headDimK: lm.headDimK || 0,
                        headDimV: lm.headDimV || 0,
                        maxContext: lm.maxContext || 0,
                        // Round 18+: пометка что VRAM/RAM оценочные (не реальные)
                        vramEstimated: split.estimated,
                        ramEstimated: split.estimated
                    });
                });
            }
            // R-Image (2026-10-02): image-модели воркера. Источник -
            // backend.image.models из того же /api/v1/metrics (воркер напрямую
            // НЕ опрашиваем: страница «Изображения» уже работает с
            // /api/v1/image/models и её контракт не трогаем). У image-модели нет
            // digest/expiresAt и она не GGUF, зато есть vramEstimateMb.
            if (Utils.getBackendType(b) === 'image_cpp') {
                const imgBlock = Utils.getBackendImage(b) || {};
                Utils.getBackendImageModels(b).forEach(function (im) {
                    const imVramMb = Utils.imageVramEstimateMb(im);
                    allModels.push({
                        name: im.name,
                        size: Utils.imageSizeBytes(im),
                        // modelsGrid ожидает байты: (m.vramUsage || 0) / 1024 / 1024.
                        vramUsage: imVramMb * 1024 * 1024,
                        ramUsage: 0,
                        digest: '',
                        expiresAt: '',
                        quantization: '',
                        ggufPath: '',
                        // Backend identification
                        backend: b.id,
                        backendStatus: b.status,
                        backendType: 'image_cpp',
                        state: im.state || 'not_loaded',
                        family: im.family || '',
                        imageSizeBytes: Utils.imageSizeBytes(im),
                        imageVramMb: imVramMb,
                        activeQueries: im.activeQueries || 0,
                        // Модель, которую воркер держит в памяти прямо сейчас.
                        isCurrentImageModel: !!(imgBlock.currentModel && imgBlock.currentModel === im.name),
                        vramEstimated: true
                    });
                });
            }
        });

        // R91 (2026-10-09): одна модель одного бэкенда — одна карточка и один
        // счётчик. Без сведения дублей страница показывала ДВЕ карточки на одну
        // модель: из Ollama-проекции (только имя, прочерки, «⌛ Истёк») и из
        // llamaCpp.loadedModels (реальные данные). См. dedupModelCards.
        const dedupedModels = dedupModelCards(allModels);
        Utils.setText('modelsTotal', dedupedModels.length);
        Utils.setText('modelsLoaded', dedupedModels.filter(m => m.backendStatus === 'healthy').length);

        Utils.setHTML('backendLoadList', backendLoad(backends));
        Utils.setHTML('modelsGrid', modelsGrid(dedupedModels, backendMap));
        // Применяем текущую сортировку (если в UI активна) сразу после рендера
        if (window.ui && typeof window.ui.applyModelsSort === 'function') {
            try { window.ui.applyModelsSort(); } catch (e) { /* ignore */ }
        }
        // Session B: применяем текущий search query (если есть) + обновляем счётчик.
        var searchInput = document.getElementById('modelsSearch');
        if (window.ui && typeof window.ui.filterModels === 'function') {
            try {
                window.ui.filterModels(searchInput ? searchInput.value : '');
            } catch (e) { /* ignore */ }
        }
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
                <td>${escapeHtml(s.model || '-')}${protocolBadge(s.protocol)}</td>
                <td>${s.requestCount || 0}</td>
                <td>${Utils.formatDate(s.lastRequestAt)}</td>
                <td>${escapeHtml(s.clientIP || '-')}</td>
                <td>${clientIcon} ${escapeHtml(s.clientName || '-')}</td>
            </tr>
            `;
        }).join('');
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
            // Utils.formatDate вместо локальной проверки на falsy: нулевое время
            // Go ("0001-01-01T00:00:00Z") истинно и давало дату «01.01.1».
            const lastHb = Utils.formatDateFirst(a.lastHeartbeat, a.lastAgentContact);
            return '<tr>' +
                '<td><strong>' + escapeHtml(a.id || '-') + '</strong></td>' +
                '<td>' + escapeHtml(a.host || '-') + '</td>' +
                '<td>' + escapeHtml(String(a.agentPort || a.port || '-')) + '</td>' +
                '<td>' + badge(statusLabel(status), statusClass) + '</td>' +
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

    // ---- GGUF Models Page ----

    function ggufPage(data) {
        var container = document.getElementById('ggufContainer');
        if (!container) return;
        if (!data || !data.backends || !data.backends.length) {
            container.innerHTML = loading(_t('gguf.no_backends'));
            return;
        }

        // R211 (2026-10-21): НА СТРАНИЦЕ GGUF - ТОЛЬКО llama.cpp-БЭКЕНДЫ.
        //
        // Живая жалоба (скриншот): на странице «GGUF Models» выбираешь llama.cpp-бэкенд,
        // но в списке присутствует imageworker (и другие типы), и действия по модели
        // уходят на бэкенд, который к GGUF-моделям отношения не имеет - отсюда ошибки
        // «not a llama_cpp backend» и попытки грузить модель в image-воркер.
        var ggufBackends = data.backends.filter(function (b) {
            var t = (Utils.getBackendType ? Utils.getBackendType(b) : (b.backendType || b.type || ''));
            return String(t || '').toLowerCase() === 'llama_cpp';
        });
        if (!ggufBackends.length) {
            container.innerHTML = loading(_t('gguf.no_backends'));
            return;
        }

        Utils.setText('ggufTotalBackends', ggufBackends.length);

        // Build backend selector
        var selectorHtml = '<div class="gguf-backend-selector"><label>' + _t('gguf.select_backend') + ': </label><select id="ggufBackendSelect" onchange="Renderers.ggufFilterBackend()" style="margin-left:0.5rem;padding:0.3rem 0.5rem;border-radius:4px;border:1px solid var(--border);background:var(--bg);color:var(--text);">';
        selectorHtml += '<option value="">' + _t('gguf.all_backends') + '</option>';
        ggufBackends.forEach(function(b) {
            var selected = (window._ggufSelectedBackend === b.id) ? ' selected' : '';
            selectorHtml += '<option value="' + escapeHtml(b.id) + '"' + selected + '>' + escapeHtml(b.id) + ' (' + escapeHtml(b.status) + ')</option>';
        });
        selectorHtml += '</select></div>';

        // R66b (2026-09-22): счётчик бэкендов. Utils.setText('ggufTotalBackends', …)
        // вызывался, но элемента с таким id не существовало ни в index.html, ни в
        // генерируемой разметке → счётчик в шапке GGUF не отображался.
        var totalHtml = '<div class="gguf-total-backends-row">' +
            '<span data-i18n="gguf.total_backends">' + _t('gguf.total_backends') + '</span>: ' +
            '<span id="ggufTotalBackends">' + ggufBackends.length + '</span></div>';

        container.innerHTML = totalHtml + selectorHtml + '<div id="ggufBackendCards" style="display:grid;gap:1rem;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));"></div>';
        renderGgufCardsInto(ggufBackends);
    }

        return {
            backendsPage: backendsPage,
            modelsPage: modelsPage,
            sessionsPage: sessionsPage,
            agentsPage: agentsPage,
            ggufPage: ggufPage
        };
    };
})();
