/**
 * UI Renderers — all HTML generation in one place
 */
const Renderers = (function () {
    const { formatNumber, formatMB, getBackendMode, getBackendModeBadge, getBackendTypeBadge, getGPUStatus, getProgressClass, percent, escapeHtml } = Utils;
    // R389 (2026-11-02): escapeAttr — экранирование для АТРИБУТОВ (title/data-*).
    // Utils.escapeHtml работает через textContent/innerHTML и НЕ трогает кавычки:
    // title="' + escapeHtml(err) + '" рвался на первом же `"` внутри текста
    // ошибки (а в JSON-ошибках кавычки есть всегда) — поэтому наведение не
    // показывало содержимое, а колонка «уходила за край».
    // =====================================================================

    function escapeAttr(value) {
        return String(value == null ? '' : value)
            .replace(/&/g, '&amp;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;');
    }

    // Cache for prediction hysteresis to prevent flickering between warning/success

    // Stable backend order: new backends appended at the end, removed ones leave
    // gaps that are filled by shifting remaining backends. Ensures visual stability.
    const _backendOrder = new Map(); // id -> { index, addedAt }

    const _t = (k, p) => window.I18N ? window.I18N.t(k, p) : k;


    // === R83 (2026-09-25), N5: канонический ID модели для клиента ===
    //
    // ЗАЧЕМ. Оператор видит в карточке «Runtime C:65.5K», а в клиенте (Cline,
    // OpenWebUI) надо вписать имя модели. Живой случай: в клиенте стояло
    // "qwen3.8:latest", на диске лежит "Qwen3.8-27B-UD-Q4_K_M" — угадать это по
    // интерфейсу было невозможно, запрос уходил в никуда (500/404).
    //
    // Копирование сделано через data-атрибут + делегирование, а НЕ через
    // inline onclick с подставленным именем: имя модели приходит из бэкенда (и
    // потенциально управляется клиентом), поэтому вставлять его в HTML-атрибут
    // с JS-кодом нельзя — это инъекция. Значение читается из data-атрибута,
    // который экранирован для атрибутного контекста.
    window.copyModelId = function (name, btn) {
      if (!name) return;
      const mark = () => {
        if (!btn) return;
        const prev = btn.textContent;
        btn.textContent = '✓';
        setTimeout(() => { btn.textContent = prev; }, 1200);
      };
      try {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(name).then(mark, mark);
          return;
        }
      } catch (e) {
        // Клипборд может быть недоступен (не HTTPS, старый браузер) — ID и так
        // виден на экране, его можно выделить руками.
      }
      mark();
    };

    if (typeof document !== 'undefined' && document.addEventListener) {
      document.addEventListener('click', (ev) => {
        const target = ev.target;
        if (!target || typeof target.closest !== 'function') return;
        const btn = target.closest('[data-copy-model-id]');
        if (!btn) return;
        ev.preventDefault();
        window.copyModelId(btn.getAttribute('data-copy-model-id'), btn);
      });
    }

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

    // R239: статусы бэкенда приходят с сервера по-английски ("healthy",
    // "warming_up"), и бейдж печатал их как есть рядом с локализованным UI.
    // Переводим через тот же словарь, что и в monitor (monitor.backendStatus.*),
    // иначе показываем исходную строку - неизвестный статус лучше не скрывать.
    function statusLabel(status) {
        if (!status) return '';
        const key = 'monitor.backendStatus.' + String(status).toLowerCase();
        const translated = _t(key);
        return translated && translated !== key ? translated : status;
    }

    function statusBadge(status) {
        const type = String(status || '').toLowerCase() === 'healthy' ? 'success'
            : (String(status || '').toLowerCase() === 'unhealthy' ? 'danger' : 'warning');
        return badge(statusLabel(status), type);
    }

    // ==== R304 (2026-10-30): донор держит ТОЛЬКО память ====
    //
    // ЗАЧЕМ. Доктрина оператора: «если бэкенд становится донором, он уже не
    // может подымать свои копии моделей — он становится именно донором мастера
    // и может только делить память с мастером». Панель обязана называть роль и
    // мастера ТАМ ЖЕ, где стоит статус узла: иначе «донор» на странице
    // «Мониторинг» и кнопка «Загрузить» на странице «Модели» — противоречие,
    // которое читается как ошибка интерфейса, а не как нарушение роли.
    //
    // ЧТО БЫЛО НЕ ТАК. Пометки донора в списке не было вообще: роль жила только
    // в панели раскладки (js/monitor/layout-panel.js), а «Бэкенды»/«Дашборд»
    // показывали донора обычным здоровым узлом. Разбор роли — в Utils.donorMark
    // (один разбор на всю панель; там же fallback для старого балансера).
    function donorMarkOf(backend, backends) {
        return (typeof Utils.donorMark === 'function') ? Utils.donorMark(backend, backends) : null;
    }

    // donorBadgeHtml — бейджи роли донора рядом со статусом.
    //   badge-info    — «⛓ донор (мастер: X)»;
    //   badge-warning — «своя копия: N», если донор вопреки роли держит копию
    //                   (title перечисляет имена: N без имён — загадка).
    function donorBadgeHtml(backend, backends) {
        const dm = donorMarkOf(backend, backends);
        if (!dm) return '';
        const label = dm.master
            ? _t('backends.donor_badge', { master: dm.master })
            : _t('backends.donor_badge_no_master');
        let html = ' <span class="badge badge-info">⛓ ' + escapeHtml(label) + '</span>';
        if (dm.ownCopies.length > 0) {
            html += ' <span class="badge badge-warning" title="' +
                escapeHtml(dm.ownCopies.join(', ')) + '">' +
                escapeHtml(_t('backends.donor_own_copies', { count: dm.ownCopies.length })) + '</span>';
        }
        return html;
    }

    // donorDetailHtml — секция в раскрытой строке бэкенда: чей это донор и что
    // делать, если он держит свою копию (выгрузить или снять роль — решение
    // оператора, поэтому это предупреждение, а не автоматическая уборка).
    function donorDetailHtml(backend, backends) {
        const dm = donorMarkOf(backend, backends);
        if (!dm) return '';
        const mainLine = dm.master
            ? _t('backends.donor_detail', { master: dm.master })
            : _t('backends.donor_detail_no_master');
        let inner = '<div class="be-param-item" style="grid-column:1/-1;">' +
            '<span class="be-param-value">' + escapeHtml(mainLine) + '</span></div>';
        if (dm.ownCopies.length > 0) {
            inner += '<div class="be-param-item" style="grid-column:1/-1;">' +
                '<span class="be-param-value" style="color:var(--warning);">⚠ ' +
                escapeHtml(_t('backends.donor_violation', { names: dm.ownCopies.join(', ') })) +
                '</span></div>';
        }
        return '<div class="be-detail-section"><div class="be-detail-title">' +
            _t('backends.donor_section') + '</div><div class="be-params-grid">' + inner + '</div></div>';
    }

    function loading(text) {
        if (!text) text = _t('renderers.loading_data');
        return `<div class="loading">${escapeHtml(text)}</div>`;
    }

    function emptyRow(cols, text) {
        return `<tr><td colspan="${cols}" class="loading-cell">${escapeHtml(text)}</td></tr>`;
    }


    // ---- Общие данные страниц: подписи параметров загрузки (R643) ----

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

    // R643: рендер параметров загрузки — в renderers-params.js (фабрика).
    // Зависимости передаём ЯВНО: Utils и isZeroTimestamp (объявлен ниже,
    // но function-объявление поднимается — порядок в файле не важен).
    // =====================================================================

    var paramsRenderers = window.RenderersParams({
        Utils: Utils,
        isZeroTimestamp: isZeroTimestamp,
        escapeHtml: escapeHtml,
        formatNumber: formatNumber,
        formatMB: formatMB,
        percent: percent,
        _t: _t,
        OLLAMA_PARAMS: OLLAMA_PARAMS
    });
    var renderImageParams = paramsRenderers.renderImageParams;
    var formatAgeR348 = paramsRenderers.formatAgeR348;
    var formatLoadedAtR346 = paramsRenderers.formatLoadedAtR346;
    var renderOllamaParams = paramsRenderers.renderOllamaParams;

    // R650 (2026-11-20): очередь, журналы и прокси-логи вынесены в renderers-logs.js.
    // Список зависимостей посчитан scripts/webui_extract_deps.js — не «на глаз».
    var logRenderers = window.RenderersLogs({
        Utils: Utils,
        _t: _t,
        percent: percent,
        emptyRow: emptyRow,
        escapeAttr: escapeAttr,
        escapeHtml: escapeHtml,
        protocolBadge: protocolBadge
    });
    var queueView = logRenderers.queueView;
    var queuePage = logRenderers.queuePage;
    var logs = logRenderers.logs;
    var predictionAlerts = logRenderers.predictionAlerts;
    var proxyLogs = logRenderers.proxyLogs;
    var copyProxyLogs = logRenderers.copyProxyLogs;

    // R651 (2026-11-20): дашборд и GPU-кластер вынесены в renderers-cluster.js.
    // Список зависимостей посчитан scripts/webui_extract_deps.js — не «на глаз».
    var clusterRenderers = window.RenderersCluster({
        Utils: Utils,
        _t: _t,
        computeMemorySplit: computeMemorySplit,
        donorBadgeHtml: donorBadgeHtml,
        emptyRow: emptyRow,
        escapeHtml: escapeHtml,
        formatMB: formatMB,
        formatNumber: formatNumber,
        getBackendMode: getBackendMode,
        getBackendModeBadge: getBackendModeBadge,
        getBackendTypeBadge: getBackendTypeBadge,
        getGPUStatus: getGPUStatus,
        getProgressClass: getProgressClass,
        isZeroTimestamp: isZeroTimestamp,
        loading: loading,
        parseQuantizationFromName: parseQuantizationFromName,
        percent: percent,
        queueView: queueView,
        stableBackendOrder: stableBackendOrder,
        statusBadge: statusBadge
    });
    var dashboard = clusterRenderers.dashboard;
    var runtimeCluster = clusterRenderers.runtimeCluster;
    var gpuCluster = clusterRenderers.gpuCluster;

    // R649 (2026-11-20): пять страниц вынесены в renderers-pages.js (фабрика).
    // Список зависимостей посчитан scripts/webui_extract_deps.js — не «на глаз».
    var pagesRenderers = window.RenderersPages({
        Utils: Utils,
        _t: _t,
        backendLoad: backendLoad,
        badge: badge,
        computeMemorySplit: computeMemorySplit,
        dedupModelCards: dedupModelCards,
        donorBadgeHtml: donorBadgeHtml,
        donorDetailHtml: donorDetailHtml,
        emptyRow: emptyRow,
        escapeHtml: escapeHtml,
        formatUptime: formatUptime,
        getBackendTypeBadge: getBackendTypeBadge,
        getClientIcon: getClientIcon,
        loading: loading,
        logs: logs,
        modelsGrid: modelsGrid,
        parseQuantizationFromName: parseQuantizationFromName,
        protocolBadge: protocolBadge,
        renderGgufCardsInto: renderGgufCardsInto,
        renderOllamaParams: renderOllamaParams,
        stableBackendOrder: stableBackendOrder,
        statusBadge: statusBadge,
        statusLabel: statusLabel
    });
    var backendsPage = pagesRenderers.backendsPage;
    var modelsPage = pagesRenderers.modelsPage;
    var sessionsPage = pagesRenderers.sessionsPage;
    var agentsPage = pagesRenderers.agentsPage;
    var ggufPage = pagesRenderers.ggufPage;

    /**
     * Round 18: computeMemorySplit — оценка VRAM/RAM для cppworker моделей.
     * cppworker не сообщает actual per-model usage, поэтому вычисляем из
     * архитектуры + GPU layer split + KV cache.
     *
     * @param {Object} lm - loaded model из b.llamaCpp.loadedModels
     * @returns {Object} { vramBytes, ramBytes, estimated, layerRatio }
     *   - estimated=true если расчёт приблизительный (нет nLayers).
     */
    function computeMemorySplit(lm) {
        const sizeBytes = (lm && lm.size) ? Number(lm.size) : 0;
        const nLayers = (lm && lm.nLayers) ? Number(lm.nLayers) : 0;
        const numGpuLayers = (lm && lm.numGpuLayers) ? Number(lm.numGpuLayers) : 0;
        const nKvHeads = (lm && lm.nKvHeads) ? Number(lm.nKvHeads) : 0;
        // Head dimension: KV cache использует head_dim_k. Если нет — fallback на n_embd/n_heads.
        const headDimK = (lm && lm.headDimK) ? Number(lm.headDimK)
            : (lm && lm.nEmbd && lm.nHeads ? Math.floor(Number(lm.nEmbd) / Number(lm.nHeads)) : 0);
        const headDimV = (lm && lm.headDimV) ? Number(lm.headDimV) : headDimK;
        const contextSize = (lm && lm.contextLength) ? Number(lm.contextLength) : 0;

        // Если нет nLayers/size — нечего оценивать.
        if (!sizeBytes || !nLayers) {
            return { vramBytes: 0, ramBytes: 0, estimated: false, layerRatio: 0 };
        }

        // Layer ratio: доля слоёв в GPU.
        // numGpuLayers: -1 = all, 0 = none, N = N слоёв.
        let layerRatio = 0;
        if (numGpuLayers === -1 || numGpuLayers >= nLayers) {
            layerRatio = 1.0;
        } else if (numGpuLayers > 0) {
            layerRatio = numGpuLayers / nLayers;
        }

        // Model weights: распределяются по layerRatio.
        const weightsVRAM = sizeBytes * layerRatio;
        const weightsRAM = sizeBytes * (1 - layerRatio);

        // KV cache (fp16 по умолчанию, 2 байта на элемент).
        // Формула: 2 (K + V) × n_layers × n_kv_heads × head_dim × 2 (bytes) × context_size
        // Если headDimK/V или nKvHeads неизвестны — пропускаем (≈0).
        let kvCacheTotal = 0;
        if (nKvHeads > 0 && headDimK > 0 && contextSize > 0) {
            const headDim = Math.max(headDimK, headDimV);
            kvCacheTotal = 2 * nLayers * nKvHeads * headDim * 2 * contextSize;
        }
        const kvVRAM = kvCacheTotal * layerRatio;
        const kvRAM = kvCacheTotal * (1 - layerRatio);

        // Runtime overhead ~10% (activations, scratch buffer, etc).
        const overheadFactor = 1.1;

        const vramBytes = Math.round((weightsVRAM + kvVRAM) * overheadFactor);
        const ramBytes = Math.round((weightsRAM + kvRAM) * overheadFactor);

        return {
            vramBytes: vramBytes,
            ramBytes: ramBytes,
            estimated: true,
            layerRatio: layerRatio
        };
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

            // Round 17 (2026-07-10): declare isLlamaCpp — used to switch between Ollama
            // runningModels and cppworker loadedModels in the Models column.
            const backendType = Utils.getBackendType(b);
            const isLlamaCpp = backendType === 'llama_cpp';

            const totalVRAM = gpu.memoryTotal || 0;
            const usedVRAM = gpu.memoryUsed || 0;
            const totalRAM = sys.memoryTotal || 0;
            const usedRAM = sys.memoryUsed || 0;
            const vramPercent = percent(usedVRAM, totalVRAM);
            const ramPercent = percent(usedRAM, totalRAM);

            const ollamaModels = oll.runningModels || [];
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

            // Round 16 (2026-07-10): для cppworker бэкенда показываем llama.cpp loaded models
            // (имя + ctx + GPU layers), а не только ollama runningModels.
            const loadedCppModels = (b.llamaCpp && Array.isArray(b.llamaCpp.loadedModels)) ? b.llamaCpp.loadedModels : [];
            // R-Image Phase 5: модели image-воркера (backend.image.models) - без
            // этой ветки блок «Загрузка бэкендов» показывал у image-бэкенда
            // «Моделей 0», хотя SD-модель загружена.
            const imageLoadedModels = Utils.getBackendImageModels(b);
            const isImageBk = Utils.getBackendType(b) === 'image_cpp';
            const displayModels = (isImageBk && imageLoadedModels.length)
                ? imageLoadedModels
                : (isLlamaCpp && loadedCppModels.length ? loadedCppModels : ollamaModels);
            const modelsHtml = displayModels.slice(0, 4).map(function (m) {
                const name = (typeof m === 'string') ? m : (m.name || m);
                let extra = '';
                if (typeof m === 'object' && m !== null) {
                    if (isImageBk) {
                        const imgSize = Utils.imageSizeBytes(m);
                        const imgVram = Utils.imageVramEstimateMb(m);
                        extra += ' <span style="color:var(--text-secondary);font-size:0.85em">' + escapeHtml(Utils.imageStateLabel(m.state)) + '</span>';
                        if (imgSize > 0) extra += ' <span style="color:var(--text-secondary);font-size:0.85em">' + escapeHtml(Utils.formatMB(imgSize / 1024 / 1024)) + '</span>';
                        if (imgVram > 0) extra += ' <span style="color:var(--text-secondary);font-size:0.85em">VRAM ' + escapeHtml(Utils.formatMB(imgVram)) + '</span>';
                    }
                    if (m.contextLength) extra += ' <span style="color:var(--text-secondary);font-size:0.85em">C:' + formatNumber(m.contextLength) + '</span>';
                    if (m.numGpuLayers && m.numGpuLayers !== -1) extra += ' <span style="color:var(--text-secondary);font-size:0.85em">GPU:' + m.numGpuLayers + '</span>';
                    else if (m.numGpuLayers === -1) extra += ' <span style="color:var(--text-secondary);font-size:0.85em">GPU:all</span>';
                }
                return '<span class="badge" style="background:rgba(168,85,247,0.12);color:var(--purple-accent);border-color:rgba(168,85,247,0.2);margin:2px">' +
                    escapeHtml(name) + extra + '</span>';
            }).join(' ');

            return `
                <div class="backend-load-item">
                    <div class="backend-load-header">
                        <span class="backend-load-name"><strong>${escapeHtml(b.id)}</strong></span>
                        ${statusBadge(b.status)}
                    </div>
                    <div class="backend-load-stats">
                        <span class="backend-load-stat">${_t('renderers.models_label')} <strong>${displayModels.length}</strong></span>
                        <span class="backend-load-stat">${_t('renderers.active')}: <strong>${activeReq}/${maxReq}</strong></span>
                        <span class="backend-load-stat">${_t('renderers.free_label')} <strong>${freeSlots}</strong></span>
                    </div>
                    ${modelsHtml ? '<div class="backend-load-models" style="margin:6px 0;display:flex;flex-wrap:wrap;gap:2px">' + modelsHtml + '</div>' : ''}
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
        if (isZeroTimestamp(exp)) return '';
        var expDate = new Date(exp);
        var now = Date.now();
        var diff = expDate.getTime() - now;
        if (diff < 0) return '⌛' + _t('renderers.expired');
        var sec = Math.floor(diff / 1000);
        if (sec < 60) return '⌛' + sec + 's';
        if (sec < 3600) return '⌛' + Math.floor(sec / 60) + 'm';
        if (sec < 86400) return '⌛' + Math.floor(sec / 3600) + 'h';
        return '⌛' + exp.substring(0, 10);
    }

    /**
     * isZeroTimestamp — «срока нет» вместо «срок истёк».
     *
     * R91 (2026-10-09): агент и Ollama-совместимая проекция балансера отдают
     * нулевое время Go — `0001-01-01T00:00:00Z` — там, где срока жизни модели нет
     * вовсе (llama.cpp его не отслеживает; expiresAt никто не заполняет). Строка
     * truthy, `new Date(...)` парсится, разница отрицательная — и карточка писала
     * «⌛ Истёк». Оператор читал это как «модель выгружена», хотя она загружена и
     * обслуживает запросы. Ноль = «неизвестно», а не «истекло».
     */
    function isZeroTimestamp(exp) {
        if (!exp) return true;
        var d = new Date(exp);
        if (isNaN(d.getTime())) return true;
        return d.getUTCFullYear() <= 1;
    }

    /**
     * modelCardKey — ключ «одна модель одного бэкенда».
     *
     * Имя приводим как каталог моделей на балансере (см.
     * internal/balancer/model_catalog.go): регистр и расширение .gguf не должны
     * делать из одной модели две.
     */
    function modelCardKey(backendId, name) {
        return String(backendId || '') + '\u0000' +
            String(name || '').trim().toLowerCase().replace(/\.gguf$/, '');
    }

    /**
     * modelCardRichness — насколько запись «богаче» данными.
     *
     * Нужно при сведении дублей: у одной и той же модели может быть запись из
     * Ollama-совместимой проекции (только имя, нулевые size/vram) и из
     * llamaCpp.loadedModels (путь, размер, ctx, квантование). Оставляем вторую.
     */
    function modelCardRichness(m) {
        var score = 0;
        if (Number(m.size) > 0) score += 4;
        if (m.ggufPath) score += 2;
        if (m.contextLength) score += 1;
        if (m.backendType === 'llama_cpp' && m.architecture) score += 1;
        if (!isZeroTimestamp(m.expiresAt)) score += 1;
        return score;
    }

    /**
     * dedupModelCards — одна модель одного бэкенда = одна карточка.
     *
     * R91 (2026-10-09). Живой скриншот оператора: Qwen3-Instruct-2507-q4km
     * рисовался ДВУМЯ карточками одного бэкенда cppworker-gpu-bundled-agent —
     * с прочерками, «(сведения ещё не получены)» и «⌛ Истёк» и рядом с полными
     * данными (2.3 GB, GGUF path, RAM 2620/25044). Источники: ollama.runningModels
     * (проекция для /api/ps, только имя) и llamaCpp.loadedModels (реальные данные).
     * Балансер больше не отдаёт проекцию, когда есть loadedModels (cluster_state.go),
     * но WebUI обязан быть устойчив и к старому балансеру: образы WebUI и балансера
     * обновляются отдельно.
     */
    function dedupModelCards(models) {
        var byKey = {};
        var order = [];
        models.forEach(function (m) {
            var key = modelCardKey(m.backend, m.name);
            var prev = byKey[key];
            if (!prev) {
                byKey[key] = m;
                order.push(key);
                return;
            }
            if (modelCardRichness(m) > modelCardRichness(prev)) {
                byKey[key] = m;
            }
        });
        return order.map(function (k) { return byKey[k]; });
    }

    /**
     * Round 18b (2026-07-10): parseQuantizationFromName — extract GGUF quantization
     * from filename. cppworker не отдаёт quantization, поэтому парсим из имени.
     * Поддерживает Q2_K..Q8_0, Q*_K_S/M/L, IQ*_XXS/XS/S/M/NL, TQ*, F16/F32/BF16.
     */
    function parseQuantizationFromName(name) {
        if (!name || typeof name !== 'string') return '';
        // Убираем .gguf (если есть) — regex с $ не работает с суффиксом после квантизации.
        var base = name;
        if (base.toLowerCase().endsWith('.gguf')) {
            base = base.substring(0, base.length - 5);
        }
        // СЛИТНАЯ ЗАПИСЬ llama.cpp (q4km, q3ks, q4k): у неё нет разделителя перед
        // квантом, поэтому общий regex ниже её не ловит (он требует [-._] перед
        // группой). Проверяем отдельно и в самом конце имени — иначе «q4km» так и
        // оставалось прочерком в карточке модели (живой случай: Qwen3-…-q4km).
        var fused = base.match(/q([0-8])([km])([km])?$/i);
        if (fused) {
            return 'Q' + fused[1] + '_' + fused[2].toUpperCase() + (fused[3] ? '_' + fused[3].toUpperCase() : '');
        }
        // Ищем квантизацию в конце имени.
        // Разделитель: - или . (mistral-7B-v0.3.Q4_K_M.gguf) или в начале (Q2_K.gguf).
        // Паттерны: IQ1..IQ4, TQ1/TQ2, Q2..Q8, F16/F26/F32, BF16/FP16.
        var re = new RegExp(
            '(?:^|[-._])' +              // separator (start of string, -, ., _)
            '(IQ[1-4]_[A-Z]+' +          // IQ4_XS, IQ3_XXS, IQ1_S, IQ2_M, IQ4_NL
            '|TQ[12]_[0-9]' +            // TQ1_0, TQ2_0
            '|Q[0-8]_[0-9K]_[A-Z]' +     // Q4_K_M, Q5_K_S, Q3_K_L, Q2_K_S
            '|Q[0-8]_[A-Z]' +            // Q2_K, Q4_K, Q5_K, Q6_K
            '|Q[0-8][KM][KM]' +          // слитная запись llama.cpp: q4km, q3ks
            '|Q[0-8][KM]' +              // слитная запись: q4k, q6k
            '|Q[0-8]_[0-9]' +            // Q4_0, Q5_1, Q8_0
            '|F1[26]' +                  // F16, F26
            '|F32' +                     // F32
            '|BF16' +                    // BF16
            '|FP16' +                    // FP16
            ')$',
            'i'
        );
        var m = base.match(re);
        if (!m || !m[1]) return '';
        // Приводим слитную запись к канонической: q4km → Q4_K_M, q6k → Q6_K.
        var out = m[1].toUpperCase();
        var fused = out.match(/^Q([0-8])([KM]{1,2})$/);
        if (fused) {
            out = 'Q' + fused[1] + '_' + fused[2].split('').join('_');
        }
        return out;
    }

    function modelsGrid(allModels, backendMap) {
        if (!allModels.length) return loading(_t('models.no_models'));

        // R304: список бэкендов для поиска мастера — из уже собранного backendMap
        // (тот же набор записей, что пришёл в modelsPage). Второй источник не
        // заводим: master-по-роли — fallback внутри Utils.donorMark.
        const _backendList = Object.keys(backendMap || {}).map(function (k) { return backendMap[k]; });

        return allModels.map(m => {
            // Round 18 (2026-07-10): cppworker does NOT track per-model VRAM/RAM,
            // reports 0. Show "—" instead of "0 MB" to avoid misleading operators.
            // Same for size when file is missing/corrupt.
            const hasVram = (m.vramUsage || 0) > 0;
            const hasRam = (m.ramUsage || 0) > 0;
            const hasSize = (m.size || 0) > 0;
            const vramMB = hasVram ? Math.round(m.vramUsage / 1024 / 1024) : null;
            const ramMB = hasRam ? Math.round(m.ramUsage / 1024 / 1024) : null;
            const sizeGB = hasSize ? (m.size / 1024 / 1024 / 1024) : null;
            const ramGB = hasRam ? (m.ramUsage / 1024 / 1024 / 1024) : null;
            const fmtMB = function (v) { return v === null ? '—' : v + ' MB'; };
            const fmtGB = function (v, fixed) { return v === null ? '—' : v.toFixed(fixed || 1) + ' GB'; };
            var digestShort = (m.digest || m.Digest || '').substring(0, 12);
            var expDate = isZeroTimestamp(m.expiresAt || m.ExpiresAt) ? '' : (m.expiresAt || m.ExpiresAt || '');
            var expShort = fmtExpiresShort(expDate);

            const backend = backendMap[m.backend] || {};
            const mode = getBackendMode(backend);
            const isGPU = mode === 'gpu';
            const gpu = backend.gpu || {};
            const sys = backend.system || {};

            const totalVRAM = isGPU ? (gpu.memoryTotal || 1) : 0;
            const totalRAM = sys.memoryTotal || 1;
            // Round 18: handle null vramMB/ramMB gracefully (cppworker doesn't track them).
            const vramPercent = (hasVram && totalVRAM > 0) ? Math.min(percent(vramMB, totalVRAM), 100) : 0;
            const ramPercent = (hasRam && totalRAM > 0) ? Math.min(percent(ramMB, totalRAM), 100) : 0;
            const showVRAM = isGPU && totalVRAM > 0 && hasVram;
            const showRAM = totalRAM > 0 && hasRam;

            const safeBackend = escapeHtml(m.backend);
            const safeName = escapeHtml(m.name).replace(/'/g, "\\'");
            const backendType = Utils.getBackendType(backend) || (m.backendType || '');
            const sizeBytes = m.size || 0;
            // R-Image (2026-10-02): image-модель (источник - backend.image.models
            // из /api/v1/metrics).
            const isImageModel = (backendType === 'image_cpp');
            // session A (Q3 W4): bulk operations — checkbox для multi-select.
            // Хранится в window.bulkModels.selected (Set ключей `${backend}::${model}`).
            // Не блокирует клик по карточке (mousedown on .model-card-checkbox → stopPropagation).
            // Для image-моделей checkbox НЕ рисуем: bulk load/unload идёт
            // ollama-путём бэкенда и image-воркер его не понимает.
            const safeNameAttr = escapeHtml(m.name);
            // R304: у донора «Загрузить» недоступна — его VRAM отдана мастеру
            // через rpc-server, и своя копия отнимает ровно те гигабайты, ради
            // которых донора назначили. Кнопка остаётся на месте (не прячем:
            // оператор должен видеть, ЧТО здесь запрещено), но гаснет и
            // объясняет причину в title. «Выгрузить» НЕ блокируем: снять
            // нарушающую копию — законное действие, ради него сюда и приходят.
            const _donorMark = donorMarkOf(backend, _backendList);
            const _donorLoadBlocked = !!(_donorMark && !_donorMark.canLoad);
            const loadDisabledAttr = (m.backendStatus !== 'healthy' || _donorLoadBlocked) ? ' disabled' : '';
            const loadBlockedTitleAttr = _donorLoadBlocked
                ? ' title="' + escapeHtml(_t('models.donor_load_blocked')) + '"'
                : '';
            // R-Image (2026-10-02): действия карточки.
            // image-модель грузит image-воркер (POST /api/v1/image/models/...), а
            // /api/v1/cluster/models/info про неё не знает - ⓘ открыл бы модал с
            // ошибкой, а load/unload/delete ушли бы в чужой API. Единственное
            // действие - переход на страницу «Изображения»: там генерация,
            // список моделей воркера и профили, то есть рабочее управление.
            const actionsHtml = isImageModel
                ? `<button class="btn btn-load" onclick="window.Renderers.openImagePage('${safeBackend}')" title="${escapeHtml(_t('image.open_page'))}">🎨 ${escapeHtml(_t('image.open_page'))}</button>`
                : `<button class="btn btn-info" title="${escapeHtml(backendType === 'llama_cpp' ? _t('models.details.title_llama_cpp') : _t('models.details.title'))}" onclick="window.openModelDetailsModal('${safeName}', '${safeBackend}', '${backendType}')" aria-label="${escapeHtml(backendType === 'llama_cpp' ? _t('models.details.title_llama_cpp') : _t('models.details.title'))}">ⓘ</button>` +
                  (backendType === 'llama_cpp'
                    // Round 18e: для llama.cpp бэкендов кнопки load/unload/delete
                    // скрыты — ⓘ редиректит на GGUF Models tab с pre-selected
                    // backend, где есть полноценное управление (load options,
                    // profiles, file management, delete).
                    ? ''
                    : `<button class="btn btn-load" data-admin-only onclick="window.modelCardAction('load', '${safeBackend}', '${safeName}')"${loadDisabledAttr}${loadBlockedTitleAttr}>${_t('models.load')}</button>` +
                      `<button class="btn btn-unload" data-admin-only onclick="window.modelCardAction('unload', '${safeBackend}', '${safeName}')" ${m.backendStatus !== 'healthy' ? 'disabled' : ''}>${_t('models.unload')}</button>` +
                      `<button class="btn btn-delete" data-admin-only onclick="window.modelCardAction('delete', '${safeBackend}', '${safeName}')" ${m.backendStatus !== 'healthy' ? 'disabled' : ''}>${_t('models.delete')}</button>`);

            return `
                <div class="model-card" data-backend="${safeBackend}" data-model="${safeName}" data-model-name="${escapeHtml(m.name).toLowerCase()}" data-backend-type="${escapeHtml(backendType)}" data-size-bytes="${sizeBytes}" data-vram-mb="${vramMB === null ? 0 : vramMB}">
                    ${isImageModel ? '' : `<label class="model-card-checkbox" onclick="event.stopPropagation();" title="${escapeHtml(_t('models.bulk.select_this'))}">
                        <input type="checkbox" class="model-select-cb"
                               data-backend="${safeBackend}" data-model="${safeNameAttr}"
                               onchange="window.bulkModels.onSelectionChanged()">
                    </label>`}
                    <div class="model-card-header">
                        <span class="model-name">${escapeHtml(m.name)}</span>
                        ${(window.AutoTuneUI && m.isSubOptimal)
                            ? window.AutoTuneUI.renderModelAutoTuneBadge(m)
                            : ''}
                        ${m.isCurrentImageModel ? '<span class="badge badge-success">' + escapeHtml(_t('image.active_badge')) + '</span>' : ''}
                        ${getBackendTypeBadge(backend)} ${badge(m.backend, m.backendStatus === 'healthy' ? 'success' : 'danger')}
                    </div>
                    <div class="model-size">${sizeGB === null ? '—' : sizeGB.toFixed(1) + ' GB'}</div>
                    <div class="model-details">
                        ${modelDetail('VRAM', fmtMB(vramMB), m.vramEstimated ? 'Estimated from model size + GPU layer split' : '')}
                        ${modelDetail('RAM', fmtMB(ramMB), m.ramEstimated ? 'Estimated from model size + GPU layer split' : '')}
                        ${backendTypeDetails(m, backend)}
                        ${digestShort ? '<div class="model-detail"><div class="model-detail-label">' + _t('renderers.digest') + '</div><div class="model-detail-value"><code style="font-size:var(--font-size-xs);background:var(--bg-secondary);padding:1px 4px;border-radius:3px">' + escapeHtml(digestShort) + '</code></div></div>' : ''}
                        ${expShort ? '<div class="model-detail"><div class="model-detail-label">' + _t('renderers.expires') + '</div><div class="model-detail-value" title="' + escapeHtml(expDate) + '" style="color:var(--warning);font-size:var(--font-size-xs)">' + expShort + '</div></div>' : ''}
                        ${(ramGB !== null && ramGB > 0.5) ? '<div class="model-detail"><div class="model-detail-label">RAM</div><div class="model-detail-value" style="font-size:var(--font-size-xs);color:var(--text-secondary)">' + ramGB.toFixed(2) + ' GB</div></div>' : ''}
                    </div>
                    <div class="model-memory-section">
                        <div class="model-memory-title">${_t('models.memory_title')}</div>
                        ${showVRAM ? memoryBar('VRAM', vramMB, totalVRAM, vramPercent, 'vram') : ''}
                        ${showRAM ? memoryBar('RAM', ramMB, totalRAM, ramPercent, 'ram') : ''}
                    </div>
                    <div class="model-card-actions">
                        ${actionsHtml}
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
            // GGUF-специфичные поля. Round 17: добавлены context length и GPU layers
            // чтобы оператор видел runtime config прямо в карточке модели.
            //
            // ПРОЧЕРК ВМЕСТО ДАННЫХ (живой случай 2026-10-06): карточка показывала
            // «GGUF Path: -», «Quantization: -», «Runtime: -», хотя модель в списке
            // есть. Причина: путь/квантование приходят не из /api/models, а из
            // рантайм-среза воркера, и в момент перерегистрации бэкенда (или до
            // первого опроса) этих полей ещё нет. Различаем «данных пока нет» и
            // «движок не сообщил»: первое — подсказка, второе — прочерк. Имя
            // модели тоже несёт квант (…-q4km, …-Q4_K_M) — парсим его как фолбэк.
            var hasPath = !!(model.ggufPath || model.path);
            var ggufPath = hasPath ? (model.ggufPath || model.path)
                : (model.backendStatus && model.backendStatus !== 'healthy'
                    ? '— (бэкенд ' + model.backendStatus + ')'
                    : '— (сведения ещё не получены)');
            var rawQuant = model.quantization || model.ggufQuant || '';
            if (!rawQuant && typeof parseQuantizationFromName === 'function') {
                rawQuant = parseQuantizationFromName(model.name) || parseQuantizationFromName(ggufPath) || '';
            }
            var ggufQuant = rawQuant || (hasPath ? '—' : '— (данные не получены)');
            var ctx = model.contextLength ? 'C:' + formatNumber(model.contextLength) : '';
            var gpu = model.numGpuLayers === -1 ? 'GPU:all' :
                      (model.numGpuLayers ? 'GPU:' + model.numGpuLayers : '');
            var runtime = [ctx, gpu].filter(Boolean).join(' ');
            if (!runtime) runtime = '— (не загружена)';

            // R83 (2026-09-25): показываем границу VRAM рядом с фактическим n_ctx.
            //
            // ЗАЧЕМ. В карточке были только `C:<n_ctx>` и (на вкладке агента)
            // `gguf_max=262144`. Оператор читал 262144 как «можно 128k/256k» и
            // пробовал 65536/128000 — а на A10 24 GB для 27B Q4_K_M граница
            // exact_fit около 32K, выше неё стратегия уходит в partial_offload
            // (часть слоёв в RAM) и ответ перестаёт укладываться в таймаут клиента.
            // Отсюда «32768 работает, 65536 нет» без объяснения в интерфейсе.
            var feas = model.feasibleMaxContext || 0;
            var ggufMax = model.ggufMaxContext || 0;
            var feasDetails = '';
            if (feas > 0 || ggufMax > 0) {
                if (feas > 0) {
                    feasDetails += modelDetail('Feasible n_ctx', formatNumber(feas),
                        'Максимум n_ctx, который влезает в VRAM целиком (режим exact_fit)');
                }
                if (ggufMax > 0) {
                    feasDetails += modelDetail('GGUF max n_ctx', formatNumber(ggufMax),
                        'Обучающий контекст модели — жёсткий верх, а не рекомендация');
                }
                if (feas > 0 && model.contextLength > feas) {
                    feasDetails += '<div class="model-detail">' +
                        '<div class="model-detail-label">Режим</div>' +
                        '<div class="model-detail-value" style="color:var(--warning,#f0ad4e)">' +
                        'partial offload: n_ctx ' + escapeHtml(formatNumber(model.contextLength)) +
                        ' выше границы VRAM ' + escapeHtml(formatNumber(feas)) +
                        ' — часть слоёв в RAM, ответ медленнее' +
                        '</div></div>';
                }
            }

            // R83 (N5): точный ID, который нужно вписать в клиент.
            //
            // Живой случай: в Cline стояло "qwen3.8:latest", а модель на диске
            // называется "Qwen3.8-27B-UD-Q4_K_M" — по интерфейсу это было не
            // угадать, и запрос уходил в 500/404. Показываем каноническое имя
            // (без .gguf) и даём скопировать его одним кликом.
            const clientId = String(model.name || '').replace(/\.gguf$/i, '');
            const clientIdHtml = clientId
                ? '<div class="model-detail">' +
                      '<div class="model-detail-label">' + escapeHtml('Имя для клиента') + '</div>' +
                      '<div class="model-detail-value">' +
                          '<code style="font-size:var(--font-size-xs)">' + escapeHtml(clientId) + '</code> ' +
                          '<button type="button" data-copy-model-id="' + escapeHtml(clientId) + '" ' +
                              'title="Скопировать ID модели для настройки клиента (Cline/OpenWebUI)" ' +
                              'style="background:transparent;border:1px solid var(--border-color);' +
                              'border-radius:3px;cursor:pointer;font-size:var(--font-size-xs);padding:0 5px;' +
                              'color:var(--text-primary)">⧉</button>' +
                      '</div>' +
                  '</div>'
                : '';

            return modelDetail('GGUF Path', ggufPath) +
                modelDetail('Quantization', ggufQuant) +
                (runtime ? modelDetail('Runtime', runtime) : '') +
                clientIdHtml +
                feasDetails;
        }
        // R-Image (2026-10-02): image-модель (backend.image.models из
        // /api/v1/metrics). GGUF-полей у неё нет, зато есть семейство
        // (sd15/sdxl/flux), состояние воркера и оценка VRAM.
        if (bt === 'image_cpp') {
            var imgSize = Utils.imageSizeBytes(model);
            var imgVram = Utils.imageVramEstimateMb(model);
            return modelDetail(_t('image.col_family'), model.family || '-') +
                modelDetail(_t('image.col_state'), Utils.imageStateLabel(model.state)) +
                modelDetail(_t('image.col_vram'), imgVram > 0 ? Utils.formatMB(imgVram) : '-') +
                (imgSize > 0 ? modelDetail(_t('image.size'), Utils.formatMB(imgSize / 1024 / 1024)) : '') +
                (model.activeQueries ? modelDetail(_t('image.col_active'), String(model.activeQueries)) : '') +
                (model.isCurrentImageModel ? modelDetail(_t('image.current_model'), _t('image.active_badge')) : '');
        }
        // Ollama-специфичные поля
        return modelDetail('Family', model.family || '-') +
            modelDetail('Format', model.format || '-') +
            modelDetail('Params', model.parameterSize || '-') +
            modelDetail('Quant', model.quantization || '-');
    }

    function modelDetail(label, value, title) {
        return `
            <div class="model-detail">
                <div class="model-detail-label">${escapeHtml(label)}</div>
                <div class="model-detail-value"${title ? ' title="' + escapeHtml(title) + '" style="cursor:help;border-bottom:1px dotted var(--text-secondary)"' : ''}>${escapeHtml(String(value))}</div>
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

    /**
     * R-Image (2026-10-02): переход со страницы «Модели» на страницу
     * «Изображения» с предвыбранным image-бэкендом.
     *
     * Зачем именно переход, а не кнопки load/unload в карточке: image-модель
     * грузит image-воркер (POST /api/v1/image/models/...), у него свои профили и
     * таймауты, а /api/v1/cluster/models/info про image-модели не знает.
     * Страница «Изображения» уже умеет всё это - дублировать её логику в карточке
     * значило бы сломать обе.
     */
    function openImagePage(backendId) {
        var navLink = document.querySelector('[data-page="image"]');
        if (navLink) navLink.click();
        if (!backendId) return;
        // Ждём, пока app.js отрисует страницу, затем предвыбираем бэкенд.
        // ImagePage.refresh() сохраняет selectedBackendId, если бэкенд ещё в списке.
        setTimeout(function () {
            var page = window.ImagePage;
            if (!page) return;
            if (page._state) page._state.selectedBackendId = backendId;
            if (typeof page.refresh === 'function') {
                try { page.refresh(); } catch (e) { /* ignore */ }
            }
        }, 100);
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

    // R229 (2026-10-22): БЕЙДЖ ПРОТОКОЛА — каким путём пришёл клиент.
    //
    // ЖИВЫЕ ЖАЛОБЫ ОПЕРАТОРА: «Cline жалуется на инструменты», «Open WebUI получает
    // не тот n_ctx», «сессия висит» — почти все относятся к одному из двух путей,
    // а в сессиях/очереди пути были неразличимы: видно клиента, но не видно,
    // говорил он по OpenAI-совместимому API (/v1/...) или по родному Ollama
    // (/api/...). Бренд-имена не переводятся — это названия API.
    function protocolBadge(protocol) {
        const p = String(protocol || '').toLowerCase();
        if (p === 'openai') {
            return ' <span class="badge protocol-badge protocol-openai" title="OpenAI API (/v1/...)" ' +
                'style="background:#10a37f;color:#fff;font-size:var(--font-size-xs);padding:1px 5px;border-radius:3px;">OpenAI</span>';
        }
        if (p === 'ollama') {
            return ' <span class="badge protocol-badge protocol-ollama" title="Ollama API (/api/...)" ' +
                'style="background:#64748b;color:#fff;font-size:var(--font-size-xs);padding:1px 5px;border-radius:3px;">Ollama</span>';
        }
        // Пусто/неизвестно — бейджа нет: выдуманный протокол вводил бы в заблуждение.
        return '';
    }


    // ---- Queue Page ----


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
        html += '<div class="be-param-item"><span class="be-param-label">' + _t('agents.last_heartbeat') + '</span><span class="be-param-value">' + escapeHtml(Utils.formatDateFirst(agentInfo.lastHeartbeat, agentInfo.lastAgentContact)) + '</span></div>';
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
                var expDate = isZeroTimestamp(m.expiresAt || m.ExpiresAt) ? '' : (m.expiresAt || m.ExpiresAt || '');
                html += '<div class="be-param-item" style="grid-column: 1/-1;">' +
                    '<span class="be-param-label">' + escapeHtml(m.name) + '</span>' +
                    '<span class="be-param-value">' + sizeGB.toFixed(1) + ' GB | ' + escapeHtml(m.family || '-') + ' | ' + escapeHtml(m.parameterSize || '-') + ' | ' + escapeHtml(m.quantization || '-') +
                    (digestShort ? ' <code title="' + _t('renderers.digest') + ': ' + escapeHtml(digestShort) + '">' + escapeHtml(digestShort) + '</code>' : '') +
                    (expDate ? ' <span title="' + _t('renderers.expires') + ': ' + escapeHtml(expDate) + '" style="font-size:var(--font-size-xs);color:var(--warning)">⌛' + escapeHtml(expDate.substring(0, 10)) + '</span>' : '') +
                    (ramGB > 0.5 ? ' <span style="font-size:var(--font-size-xs);color:var(--text-secondary)">RAM: ' + ramGB.toFixed(2) + ' GB</span>' : '') +
                    '</span>' +
                '</div>';
            });
            html += '</div></div>';
        }


        html += '</div>';
        container.innerHTML = html;
    }


    function ggufFilterBackend() {
        var sel = document.getElementById('ggufBackendSelect');
        var selected = sel ? sel.value : '';
        window._ggufSelectedBackend = selected;
        // Re-fetch data and re-render
        if (window.Api && window.Api.fetchGgufBackends) {
            window.Api.fetchGgufBackends().then(function(data) {
                    // R211: фильтр «все» тоже не должен возвращать image/ollama.
                    var llamaOnly = data.backends.filter(function (b) {
                        var t = (Utils.getBackendType ? Utils.getBackendType(b) : (b.backendType || b.type || ''));
                        return String(t || '').toLowerCase() === 'llama_cpp';
                    });
                if (data && data.backends) {
                    var filtered = selected
                        ? llamaOnly.filter(function(b) { return b.id === selected; })
                        : llamaOnly;
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
                    statusBadge(b.status) +
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
        // R-Image (2026-10-02): раскрытая строка image-бэкенда и переход на
        // страницу «Изображения». Экспортируем и для юнит-тестов
        // (webui/js/modules/renderers-image.test.js).
        renderImageParams,
        openImagePage,
        // R88 (2026-10-08): карточки дашборда экспортируем для юнит-тестов
        // (webui/js/modules/renderers-image.test.js): подпись «прогноз нагрузки»
        // и прочерк вместо «0W» — проверяемые свойства разметки.
        gpuCluster,
        runtimeCluster,
        sessionsPage,
        queuePage,
        // R91 (2026-10-08): чистый маппинг статистики очереди — экспортируем для
        // юнит-теста (webui/js/modules/renderers-queue.test.js).
        queueView,
        // R229 (2026-10-22): бейдж протокола — экспортируем для юнит-теста.
        protocolBadge,
        // R91 (2026-10-09): сведение дублей моделей и распознавание нулевой
        // метки времени — экспортируем для юнит-теста
        // (webui/js/modules/renderers-models-dedup.test.js): страница моделей
        // рисовала одну модель двумя карточками одного бэкенда.
        dedupModelCards,
        modelCardKey,
        isZeroTimestamp,
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

// R59.12 (2026-09-03): expose to window. Same pattern as R59.3/5/6/7
// — `app-listeners.js:309` does `const Renderers = window.Renderers;`
// and relies on Renderers.proxyLogs being callable. Before this export,
// that line returned `undefined` and the proxy-logs tab stayed
// empty (R59.10 interaction smoke test doesn't cover the proxy tab
// yet, so this slipped through the regression net). Now Renderers
// is available as a window global for any module that destructures
// or wraps it.
if (typeof window !== 'undefined') {
    window.Renderers = Renderers;
}
